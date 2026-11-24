package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	maxHeldErrorBytes = 2048

	heldJobColumns = `
		id, request_id, status, pool, alt_pools, payload_json, reason, snapshot_summary,
		released_job_id, released_pool, released_at, cancelled_at,
		last_error, error_count, created_at, updated_at
	`
)

// HoldJob persists a held job idempotently by request_id.
//
// Idempotency contract:
//   - If a job already exists in jobs for request_id, returns ErrJobAlreadyExists (no hold created).
//   - If a held_jobs row already exists with status='held', returns (existing, false, nil).
//   - If a held_jobs row already exists with status='released', returns ErrJobAlreadyExists.
//   - If a held_jobs row already exists with status='cancelled', returns (existing, false, ErrConflict).
func (s *Store) HoldJob(ctx context.Context, p HoldJobParams) (*HeldJob, bool, error) {
	reqID := strings.TrimSpace(p.RequestID)
	if reqID == "" {
		reqID = strings.TrimSpace(p.Payload.RequestID)
	}
	if reqID == "" {
		return nil, false, fmt.Errorf("%w: request_id must not be empty", ErrInvalidInput)
	}
	if strings.TrimSpace(p.Payload.Title) == "" {
		return nil, false, fmt.Errorf("%w: title must not be empty", ErrInvalidInput)
	}

	payload := p.Payload
	payload.RequestID = reqID
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, false, fmt.Errorf("%w: encode payload: %v", ErrInvalidInput, err)
	}
	altPoolsJSON, err := encodeAltPools(p.AltPools)
	if err != nil {
		return nil, false, fmt.Errorf("%w: encode alt_pools: %v", ErrInvalidInput, err)
	}

	var (
		result  *HeldJob
		created bool
	)

	err = s.withImmediateTx(ctx, func(conn *sql.Conn) error {
		// Never hold if a real job already exists for this request_id.
		if _, err := queryJobByRequestID(ctx, conn, reqID); err == nil {
			return fmt.Errorf("%w: request_id=%s", ErrJobAlreadyExists, reqID)
		} else if !errors.Is(err, ErrNotFound) {
			return err
		}

		// Idempotency check against existing held_jobs row.
		existing, err := queryHeldJobByRequestID(ctx, conn, reqID)
		if err == nil {
			switch existing.Status {
			case HeldStatusHeld:
				result = existing
				created = false
				return nil
			case HeldStatusReleased:
				return fmt.Errorf("%w: request_id=%s already released", ErrJobAlreadyExists, reqID)
			default:
				result = existing
				return fmt.Errorf("%w: held job %s for request_id=%s is %s", ErrConflict, existing.ID, reqID, existing.Status)
			}
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}

		suffix, err := randomHex(8)
		if err != nil {
			return fmt.Errorf("generate held_id: %w", err)
		}
		heldID := "held-" + suffix
		nowStr := formatTime(time.Now().UTC())

		_, err = conn.ExecContext(ctx, `
			INSERT INTO held_jobs (
				id, request_id, status, pool, alt_pools, payload_json, reason, snapshot_summary,
				released_job_id, released_pool, released_at, cancelled_at,
				last_error, error_count, created_at, updated_at
			) VALUES (
				?, ?, 'held', ?, ?, ?, ?, ?,
				NULL, '', NULL, NULL,
				'', 0, ?, ?
			)
		`,
			heldID, reqID, strings.TrimSpace(p.Pool), altPoolsJSON, string(payloadBytes),
			strings.TrimSpace(p.Reason), strings.TrimSpace(p.SnapshotSummary),
			nowStr, nowStr,
		)
		if err != nil {
			if isUniqueConstraintError(err) {
				existing, rerr := queryHeldJobByRequestID(ctx, conn, reqID)
				if rerr == nil && existing.Status == HeldStatusHeld {
					result = existing
					created = false
					return nil
				}
			}
			return err
		}

		h, err := queryHeldJobByID(ctx, conn, heldID)
		if err != nil {
			return err
		}
		result = h
		created = true
		return nil
	})
	if err != nil {
		return result, false, err
	}
	return result, created, nil
}

// GetHeldJob returns a held_jobs row by ID, or ErrNotFound if absent.
func (s *Store) GetHeldJob(ctx context.Context, heldID string) (*HeldJob, error) {
	heldID = strings.TrimSpace(heldID)
	if heldID == "" {
		return nil, fmt.Errorf("%w: held_id must not be empty", ErrInvalidInput)
	}
	return queryHeldJobByID(ctx, s.readDB, heldID)
}

// GetHeldJobByRequestID returns a held_jobs row by request_id, or ErrNotFound if absent.
func (s *Store) GetHeldJobByRequestID(ctx context.Context, requestID string) (*HeldJob, error) {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return nil, fmt.Errorf("%w: request_id must not be empty", ErrInvalidInput)
	}
	return queryHeldJobByRequestID(ctx, s.readDB, requestID)
}

// ListHeldJobs returns held_jobs filtered by status, ordered oldest-first (created_at ASC, id ASC).
// Empty status defaults to HeldStatusHeld. Pass status="all" to include all statuses.
func (s *Store) ListHeldJobs(ctx context.Context, status HeldStatus, limit int) ([]*HeldJob, error) {
	page, err := s.ListHeldJobsPage(ctx, ListHeldJobsFilter{
		Status: status,
		Limit:  limit,
	})
	if err != nil {
		return nil, err
	}
	return page.Held, nil
}

// ListHeldJobsPage returns a cursor-paginated page of held_jobs ordered oldest-first (created_at ASC, id ASC).
// Cursor format is "<created_at RFC3339Nano>|<held_id>".
func (s *Store) ListHeldJobsPage(ctx context.Context, filter ListHeldJobsFilter) (*ListHeldJobsPage, error) {
	status := filter.Status
	if status == "" {
		status = HeldStatusHeld
	}
	if status != "all" && !status.Valid() {
		return nil, fmt.Errorf("%w: invalid held status %q", ErrInvalidInput, status)
	}
	limit := filter.Limit
	if limit <= 0 {
		limit = DefaultListLimit
	}
	if limit > MaxListLimit {
		limit = MaxListLimit
	}

	var (
		conditions []string
		args       []any
	)
	if status != "all" {
		conditions = append(conditions, "status = ?")
		args = append(args, string(status))
	}
	if cursor := strings.TrimSpace(filter.Cursor); cursor != "" {
		cursorTime, cursorID, err := parseCompositeTimeStringCursor(cursor)
		if err != nil {
			return nil, fmt.Errorf("%w: invalid cursor %q: %v", ErrInvalidInput, cursor, err)
		}
		conditions = append(conditions, "(created_at > ? OR (created_at = ? AND id > ?))")
		args = append(args, cursorTime, cursorTime, cursorID)
	}

	query := `SELECT ` + heldJobColumns + ` FROM held_jobs`
	if len(conditions) > 0 {
		query += ` WHERE ` + strings.Join(conditions, " AND ")
	}
	query += ` ORDER BY created_at ASC, id ASC LIMIT ?`
	args = append(args, limit+1)

	rows, err := s.readDB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*HeldJob
	for rows.Next() {
		h, err := scanHeldJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	page := &ListHeldJobsPage{}
	if len(out) > limit {
		page.HasMore = true
		out = out[:limit]
	}
	page.Held = out
	if page.HasMore && len(out) > 0 {
		last := out[len(out)-1]
		page.NextCursor = formatCompositeTimeStringCursor(last.CreatedAt, last.ID)
	}
	return page, nil
}

// CancelHeldJob transitions a held entry from status='held' to status='cancelled' without
// deleting the row so audit history and request_id uniqueness remain intact.
func (s *Store) CancelHeldJob(ctx context.Context, heldID string) (*HeldJob, error) {
	heldID = strings.TrimSpace(heldID)
	if heldID == "" {
		return nil, fmt.Errorf("%w: held_id must not be empty", ErrInvalidInput)
	}

	var out *HeldJob
	err := s.withImmediateTx(ctx, func(conn *sql.Conn) error {
		nowStr := formatTime(time.Now().UTC())
		res, err := conn.ExecContext(ctx, `
			UPDATE held_jobs
			SET status = 'cancelled',
				cancelled_at = ?,
				updated_at = ?
			WHERE id = ? AND status = 'held'
		`, nowStr, nowStr, heldID)
		if err != nil {
			return err
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if affected == 0 {
			return classifyHeldConflict(ctx, conn, heldID, "held")
		}
		h, err := queryHeldJobByID(ctx, conn, heldID)
		if err != nil {
			return err
		}
		out = h
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// MarkHeldReleased transitions a held entry from status='held' to status='released',
// recording the created job_id and selected pool and clearing any prior last_error.
func (s *Store) MarkHeldReleased(ctx context.Context, heldID string, jobID int64, selectedPool string) (*HeldJob, error) {
	heldID = strings.TrimSpace(heldID)
	if heldID == "" || jobID <= 0 {
		return nil, fmt.Errorf("%w: held_id and positive job_id are required", ErrInvalidInput)
	}

	var out *HeldJob
	err := s.withImmediateTx(ctx, func(conn *sql.Conn) error {
		nowStr := formatTime(time.Now().UTC())
		res, err := conn.ExecContext(ctx, `
			UPDATE held_jobs
			SET status = 'released',
				released_job_id = ?,
				released_pool = ?,
				released_at = ?,
				last_error = '',
				updated_at = ?
			WHERE id = ? AND status = 'held'
		`, jobID, strings.TrimSpace(selectedPool), nowStr, nowStr, heldID)
		if err != nil {
			return err
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if affected == 0 {
			return classifyHeldConflict(ctx, conn, heldID, "held")
		}
		h, err := queryHeldJobByID(ctx, conn, heldID)
		if err != nil {
			return err
		}
		out = h
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// RecordHeldReleaseError records a release failure on a held entry while keeping it held.
//
// Deduplication: we store only the single latest error string (capped at maxHeldErrorBytes)
// rather than appending a history log, so repeated periodic failures cannot bloat the DB.
// If errMsg matches the existing last_error verbatim, only updated_at is refreshed and
// isNew=false is returned so callers can suppress redundant log lines as well.
func (s *Store) RecordHeldReleaseError(ctx context.Context, heldID, errMsg string) (*HeldJob, bool, error) {
	heldID = strings.TrimSpace(heldID)
	if heldID == "" {
		return nil, false, fmt.Errorf("%w: held_id must not be empty", ErrInvalidInput)
	}
	errMsg = strings.TrimSpace(errMsg)
	if len(errMsg) > maxHeldErrorBytes {
		errMsg = errMsg[:maxHeldErrorBytes]
	}

	var (
		out   *HeldJob
		isNew bool
	)
	err := s.withImmediateTx(ctx, func(conn *sql.Conn) error {
		var (
			statusStr string
			prevErr   string
		)
		err := conn.QueryRowContext(ctx, `SELECT status, last_error FROM held_jobs WHERE id = ?`, heldID).Scan(&statusStr, &prevErr)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: held job %s", ErrNotFound, heldID)
		}
		if err != nil {
			return err
		}
		if HeldStatus(statusStr) != HeldStatusHeld {
			return fmt.Errorf("%w: held job %s is in status %s, expected held", ErrConflict, heldID, statusStr)
		}

		nowStr := formatTime(time.Now().UTC())
		if prevErr == errMsg {
			// Same error as last attempt: refresh updated_at only to avoid bloating records/logs.
			isNew = false
			if _, err := conn.ExecContext(ctx, `
				UPDATE held_jobs
				SET updated_at = ?
				WHERE id = ? AND status = 'held'
			`, nowStr, heldID); err != nil {
				return err
			}
		} else {
			isNew = true
			if _, err := conn.ExecContext(ctx, `
				UPDATE held_jobs
				SET last_error = ?,
					error_count = error_count + 1,
					updated_at = ?
				WHERE id = ? AND status = 'held'
			`, errMsg, nowStr, heldID); err != nil {
				return err
			}
		}

		h, err := queryHeldJobByID(ctx, conn, heldID)
		if err != nil {
			return err
		}
		out = h
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return out, isNew, nil
}

func queryHeldJobByID(ctx context.Context, q queryRower, heldID string) (*HeldJob, error) {
	row := q.QueryRowContext(ctx, `SELECT `+heldJobColumns+` FROM held_jobs WHERE id = ?`, heldID)
	h, err := scanHeldJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return h, err
}

func queryHeldJobByRequestID(ctx context.Context, q queryRower, requestID string) (*HeldJob, error) {
	row := q.QueryRowContext(ctx, `SELECT `+heldJobColumns+` FROM held_jobs WHERE request_id = ?`, requestID)
	h, err := scanHeldJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return h, err
}

func scanHeldJob(s rowScanner) (*HeldJob, error) {
	var (
		h             HeldJob
		statusStr     string
		altPoolsStr   string
		payloadStr    string
		releasedJobID sql.NullInt64
		releasedAt    sql.NullString
		cancelledAt   sql.NullString
		createdAt     string
		updatedAt     string
	)
	err := s.Scan(
		&h.ID, &h.RequestID, &statusStr, &h.Pool, &altPoolsStr, &payloadStr, &h.Reason, &h.SnapshotSummary,
		&releasedJobID, &h.ReleasedPool, &releasedAt, &cancelledAt,
		&h.LastError, &h.ErrorCount, &createdAt, &updatedAt,
	)
	if err != nil {
		return nil, err
	}
	h.Status = HeldStatus(statusStr)
	altPools, err := decodeAltPools(altPoolsStr)
	if err != nil {
		return nil, fmt.Errorf("decode held alt_pools: %w", err)
	}
	h.AltPools = altPools

	if strings.TrimSpace(payloadStr) != "" {
		if err := json.Unmarshal([]byte(payloadStr), &h.Payload); err != nil {
			return nil, fmt.Errorf("decode held payload_json: %w", err)
		}
	}
	if releasedJobID.Valid {
		v := releasedJobID.Int64
		h.ReleasedJobID = &v
	}
	if releasedAt.Valid {
		t, err := parseTime(releasedAt.String)
		if err != nil {
			return nil, fmt.Errorf("parse held released_at: %w", err)
		}
		h.ReleasedAt = &t
	}
	if cancelledAt.Valid {
		t, err := parseTime(cancelledAt.String)
		if err != nil {
			return nil, fmt.Errorf("parse held cancelled_at: %w", err)
		}
		h.CancelledAt = &t
	}
	ct, err := parseTime(createdAt)
	if err != nil {
		return nil, fmt.Errorf("parse held created_at: %w", err)
	}
	h.CreatedAt = ct
	ut, err := parseTime(updatedAt)
	if err != nil {
		return nil, fmt.Errorf("parse held updated_at: %w", err)
	}
	h.UpdatedAt = ut
	return &h, nil
}

func classifyHeldConflict(ctx context.Context, conn *sql.Conn, heldID, expected string) error {
	var statusStr string
	err := conn.QueryRowContext(ctx, `SELECT status FROM held_jobs WHERE id = ?`, heldID).Scan(&statusStr)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: held job %s", ErrNotFound, heldID)
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("%w: held job %s is in status %s, expected %s", ErrConflict, heldID, statusStr, expected)
}

func ensureHeldJobsColumns(ctx context.Context, db *sql.DB) error {
	existing, err := tableColumns(ctx, db, "held_jobs")
	if err != nil {
		return err
	}
	additions := []struct {
		name string
		ddl  string
	}{
		{"status", `ALTER TABLE held_jobs ADD COLUMN status TEXT NOT NULL DEFAULT 'held'`},
		{"snapshot_summary", `ALTER TABLE held_jobs ADD COLUMN snapshot_summary TEXT NOT NULL DEFAULT ''`},
		{"released_job_id", `ALTER TABLE held_jobs ADD COLUMN released_job_id INTEGER NULL`},
		{"released_pool", `ALTER TABLE held_jobs ADD COLUMN released_pool TEXT NOT NULL DEFAULT ''`},
		{"released_at", `ALTER TABLE held_jobs ADD COLUMN released_at TEXT NULL`},
		{"cancelled_at", `ALTER TABLE held_jobs ADD COLUMN cancelled_at TEXT NULL`},
		{"last_error", `ALTER TABLE held_jobs ADD COLUMN last_error TEXT NOT NULL DEFAULT ''`},
		{"error_count", `ALTER TABLE held_jobs ADD COLUMN error_count INTEGER NOT NULL DEFAULT 0`},
	}
	for _, add := range additions {
		if _, ok := existing[add.name]; ok {
			continue
		}
		if _, err := db.ExecContext(ctx, add.ddl); err != nil {
			return err
		}
	}
	_, err = db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_held_jobs_status_created ON held_jobs(status, created_at, id)`)
	return err
}

func ensureGateDecisionsSchema(ctx context.Context, db *sql.DB) error {
	cols, err := tableColumns(ctx, db, "gate_decisions")
	if err != nil {
		return err
	}
	jobCol, hasJob := cols["job_id"]
	_, hasHeldID := cols["held_id"]
	_, hasHeld := cols["held"]
	if hasJob && !jobCol.notNull && hasHeldID && hasHeld {
		_, err = db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_gate_decisions_held_id ON gate_decisions(held_id)`)
		return err
	}

	// Rebuild gate_decisions from Phase 3-1 schema so job_id is nullable (for enforce holds)
	// and held / held_id columns exist.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `
		CREATE TABLE gate_decisions_new (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			job_id INTEGER NULL REFERENCES jobs(id),
			held_id TEXT NOT NULL DEFAULT '',
			held INTEGER NOT NULL DEFAULT 0,
			mode TEXT NOT NULL DEFAULT 'dry-run',
			requested_pool TEXT NOT NULL DEFAULT '',
			alt_pools TEXT NOT NULL DEFAULT '[]',
			decision TEXT NOT NULL CHECK(decision IN ('would_pass','would_hold','unknown')),
			blocked_by TEXT NOT NULL DEFAULT '',
			selected_pool TEXT NOT NULL DEFAULT '',
			bypassed INTEGER NOT NULL DEFAULT 0,
			reason TEXT NOT NULL DEFAULT '',
			measured_at TEXT NULL,
			decided_at TEXT NOT NULL
		)
	`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO gate_decisions_new (
			id, job_id, held_id, held, mode, requested_pool, alt_pools, decision,
			blocked_by, selected_pool, bypassed, reason, measured_at, decided_at
		)
		SELECT
			id, job_id, '', 0, mode, requested_pool, alt_pools, decision,
			blocked_by, selected_pool, bypassed, reason, measured_at, decided_at
		FROM gate_decisions
	`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE gate_decisions`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `ALTER TABLE gate_decisions_new RENAME TO gate_decisions`); err != nil {
		return err
	}
	for _, idx := range []string{
		`CREATE INDEX IF NOT EXISTS idx_gate_decisions_id_desc ON gate_decisions(id DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_gate_decisions_job_id ON gate_decisions(job_id)`,
		`CREATE INDEX IF NOT EXISTS idx_gate_decisions_held_id ON gate_decisions(held_id)`,
	} {
		if _, err := tx.ExecContext(ctx, idx); err != nil {
			return err
		}
	}
	return tx.Commit()
}

type tableColInfo struct {
	notNull bool
}

func tableColumns(ctx context.Context, db *sql.DB, table string) (map[string]tableColInfo, error) {
	rows, err := db.QueryContext(ctx, fmt.Sprintf(`PRAGMA table_info(%s)`, table))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]tableColInfo)
	for rows.Next() {
		var (
			cid       int
			name      string
			colType   string
			notNull   int
			dfltValue sql.NullString
			pk        int
		)
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dfltValue, &pk); err != nil {
			return nil, err
		}
		out[name] = tableColInfo{notNull: notNull != 0}
	}
	return out, rows.Err()
}

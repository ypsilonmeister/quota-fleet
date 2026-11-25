package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const (
	defaultBusyTimeoutMS = 5000
	maxBusyRetries       = 25
	metaKeyInstanceID    = "instance_id"
)

// Options configures Store behavior on Open.
type Options struct {
	// BusyTimeoutMS sets PRAGMA busy_timeout in milliseconds. Defaults to 5000.
	BusyTimeoutMS int
	// InstanceID is recorded into signal_outbox.env when emitting signals.
	InstanceID string
	// ReadMaxOpenConns sets max open connections for the read pool. Defaults to 4.
	ReadMaxOpenConns int
}

// Store provides transactional, CAS-based persistence for quota-fleet over SQLite.
type Store struct {
	writeDB    *sql.DB
	readDB     *sql.DB
	sharedDB   bool
	instanceID string
	events     jobEventHub
	// afterJobCommit, set only by tests, runs between a job commit and its publication.
	afterJobCommit func()
}

// Open opens (or creates) the SQLite database at dbPath, enables WAL mode and
// busy_timeout, serializes writes on a single connection, and ensures the schema exists.
func Open(ctx context.Context, dbPath string, opts Options) (*Store, error) {
	dbPath = strings.TrimSpace(dbPath)
	if dbPath == "" {
		return nil, fmt.Errorf("%w: db path must not be empty", ErrInvalidInput)
	}
	busyMS := opts.BusyTimeoutMS
	if busyMS <= 0 {
		busyMS = defaultBusyTimeoutMS
	}
	readConns := opts.ReadMaxOpenConns
	if readConns <= 0 {
		readConns = 4
	}

	isMem := isMemoryPath(dbPath)
	resolvedPath := dbPath
	if !isMem {
		absPath, err := filepath.Abs(dbPath)
		if err != nil {
			return nil, fmt.Errorf("resolve db path %q: %w", dbPath, err)
		}
		resolvedPath = absPath
		dir := filepath.Dir(resolvedPath)
		if dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, fmt.Errorf("create db parent dir: %w", err)
			}
		}
	}

	dsn := buildDSN(resolvedPath, busyMS, isMem)

	writeDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open write sqlite db: %w", err)
	}
	// Serialize all writes through a single connection.
	writeDB.SetMaxOpenConns(1)
	writeDB.SetMaxIdleConns(1)
	writeDB.SetConnMaxLifetime(0)

	if err := initPragmas(ctx, writeDB, busyMS, isMem); err != nil {
		_ = writeDB.Close()
		return nil, err
	}

	if _, err := writeDB.ExecContext(ctx, schemaSQL); err != nil {
		_ = writeDB.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	if err := ensureWorktreeSpecColumn(ctx, writeDB); err != nil {
		_ = writeDB.Close()
		return nil, fmt.Errorf("migrate jobs.worktree_spec: %w", err)
	}
	if err := ensureCreatedByColumn(ctx, writeDB); err != nil {
		_ = writeDB.Close()
		return nil, fmt.Errorf("migrate jobs.created_by: %w", err)
	}
	if err := ensureHeldJobsColumns(ctx, writeDB); err != nil {
		_ = writeDB.Close()
		return nil, fmt.Errorf("migrate held_jobs: %w", err)
	}
	if err := ensureGateDecisionsSchema(ctx, writeDB); err != nil {
		_ = writeDB.Close()
		return nil, fmt.Errorf("migrate gate_decisions: %w", err)
	}

	if err := ensurePushTokenIDColumn(ctx, writeDB); err != nil {
		_ = writeDB.Close()
		return nil, fmt.Errorf("migrate push_subscriptions.token_id: %w", err)
	}

	var readDB *sql.DB
	shared := isMem
	if isMem {
		readDB = writeDB
	} else {
		readDB, err = sql.Open("sqlite", dsn)
		if err != nil {
			_ = writeDB.Close()
			return nil, fmt.Errorf("open read sqlite db: %w", err)
		}
		readDB.SetMaxOpenConns(readConns)
		readDB.SetMaxIdleConns(readConns)
		readDB.SetConnMaxLifetime(0)
		if err := readDB.PingContext(ctx); err != nil {
			_ = readDB.Close()
			_ = writeDB.Close()
			return nil, fmt.Errorf("ping read sqlite db: %w", err)
		}
	}

	return &Store{
		writeDB:    writeDB,
		readDB:     readDB,
		sharedDB:   shared,
		instanceID: opts.InstanceID,
	}, nil
}

func isMemoryPath(dbPath string) bool {
	return dbPath == ":memory:" || strings.HasPrefix(dbPath, "file::memory:") || strings.Contains(dbPath, "mode=memory")
}

func buildDSN(dbPath string, busyMS int, isMem bool) string {
	if isMem {
		return dbPath
	}
	slashPath := filepath.ToSlash(dbPath)
	if !strings.HasPrefix(slashPath, "/") {
		slashPath = "/" + slashPath
	}
	u := url.URL{
		Scheme: "file",
		Path:   slashPath,
	}
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", busyMS))
	q.Add("_pragma", "foreign_keys(ON)")
	q.Add("_pragma", "synchronous(NORMAL)")
	u.RawQuery = q.Encode()
	return u.String()
}

func initPragmas(ctx context.Context, db *sql.DB, busyMS int, isMem bool) error {
	pragmas := []string{
		fmt.Sprintf("PRAGMA busy_timeout = %d", busyMS),
		"PRAGMA foreign_keys = ON",
	}
	if !isMem {
		pragmas = append([]string{"PRAGMA journal_mode = WAL", "PRAGMA synchronous = NORMAL"}, pragmas...)
	}
	for _, p := range pragmas {
		if _, err := db.ExecContext(ctx, p); err != nil {
			return fmt.Errorf("exec %q: %w", p, err)
		}
	}
	return nil
}

// Close closes the underlying database connections.
func (s *Store) Close() error {
	s.CloseJobEvents()
	var errs []error
	if !s.sharedDB && s.readDB != nil {
		if err := s.readDB.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if s.writeDB != nil {
		if err := s.writeDB.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// JournalMode returns the current SQLite journal_mode (for diagnostics and tests).
func (s *Store) JournalMode(ctx context.Context) (string, error) {
	var mode string
	if err := s.readDB.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
		return "", err
	}
	return mode, nil
}

// SetMeta inserts or updates a key/value pair in the meta table.
func (s *Store) SetMeta(ctx context.Context, key, value string) error {
	if strings.TrimSpace(key) == "" {
		return fmt.Errorf("%w: meta key must not be empty", ErrInvalidInput)
	}
	now := formatTime(time.Now().UTC())
	return s.withImmediateTx(ctx, func(conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, `
			INSERT INTO meta (key, value, updated_at)
			VALUES (?, ?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at
		`, key, value, now)
		return err
	})
}

// GetMeta retrieves a value from the meta table.
func (s *Store) GetMeta(ctx context.Context, key string) (string, error) {
	var value string
	err := s.readDB.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	return value, nil
}

// VerifyInstanceID checks meta.instance_id against expected.
// Returns ErrNotFound if not initialized, or ErrInstanceIDMismatch if different.
func (s *Store) VerifyInstanceID(ctx context.Context, expected string) error {
	got, err := s.GetMeta(ctx, metaKeyInstanceID)
	if err != nil {
		return err
	}
	if got != expected {
		return fmt.Errorf("%w: db has %q, config has %q", ErrInstanceIDMismatch, got, expected)
	}
	return nil
}

// InitInstanceID sets meta.instance_id if absent, succeeds if identical, or fails with ErrInstanceIDMismatch.
func (s *Store) InitInstanceID(ctx context.Context, instanceID string) error {
	if strings.TrimSpace(instanceID) == "" {
		return fmt.Errorf("%w: instance_id must not be empty", ErrInvalidInput)
	}
	now := formatTime(time.Now().UTC())
	return s.withImmediateTx(ctx, func(conn *sql.Conn) error {
		var existing string
		err := conn.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, metaKeyInstanceID).Scan(&existing)
		if err == nil {
			if existing != instanceID {
				return fmt.Errorf("%w: db has %q, expected %q", ErrInstanceIDMismatch, existing, instanceID)
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		_, err = conn.ExecContext(ctx, `
			INSERT INTO meta (key, value, updated_at) VALUES (?, ?, ?)
		`, metaKeyInstanceID, instanceID, now)
		return err
	})
}

// CreateJob registers a new job idempotently by request_id.
// Returns (job, created, err), where created is false when an existing job matched request_id.
func (s *Store) CreateJob(ctx context.Context, p CreateJobParams) (*Job, bool, error) {
	if strings.TrimSpace(p.RequestID) == "" {
		return nil, false, fmt.Errorf("%w: request_id must not be empty", ErrInvalidInput)
	}
	if strings.TrimSpace(p.Title) == "" {
		return nil, false, fmt.Errorf("%w: title must not be empty", ErrInvalidInput)
	}

	altPoolsJSON, err := encodeAltPools(p.AltPools)
	if err != nil {
		return nil, false, fmt.Errorf("%w: encode alt_pools: %v", ErrInvalidInput, err)
	}
	worktreeSpecJSON, err := encodeWorktreeSpec(p.WorktreeSpec)
	if err != nil {
		return nil, false, fmt.Errorf("%w: encode worktree_spec: %v", ErrInvalidInput, err)
	}

	var (
		result  *Job
		created bool
	)

	err = s.withJobTx(ctx, func(conn *sql.Conn, changes *[]statusChange) error {
		// Idempotency check by request_id.
		existing, err := queryJobByRequestID(ctx, conn, p.RequestID)
		if err == nil {
			result = existing
			created = false
			return nil
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}

		// Determine initial status based on parent_id / Draft.
		var status Status
		if p.ParentID == nil {
			if p.Draft {
				status = StatusDraft
			} else {
				status = StatusQueued
			}
		} else {
			var parentStatusStr string
			err := conn.QueryRowContext(ctx, `SELECT status FROM jobs WHERE id = ?`, *p.ParentID).Scan(&parentStatusStr)
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: parent_id=%d", ErrParentNotFound, *p.ParentID)
			}
			if err != nil {
				return err
			}
			parentStatus := Status(parentStatusStr)
			switch parentStatus {
			case StatusFailed, StatusCancelled:
				return fmt.Errorf("%w: parent_id=%d has status %s", ErrParentTerminal, *p.ParentID, parentStatus)
			case StatusDone:
				status = StatusQueued
			default:
				status = StatusWaiting
			}
		}

		now := time.Now().UTC()
		nowStr := formatTime(now)

		var parentArg any
		if p.ParentID != nil {
			parentArg = *p.ParentID
		}

		res, err := conn.ExecContext(ctx, `
			INSERT INTO jobs (
				request_id, title, status, agent, host, workdir, worktree_spec, target_ref,
				instructions, acceptance_criteria, parent_id,
				claimed_by, claimed_at, attempts, result_text, last_error, delivered_at,
				created_at, updated_at, model, pool, alt_pools, session_id, created_by
			) VALUES (
				?, ?, ?, ?, ?, ?, ?, ?,
				?, ?, ?,
				'', NULL, 0, '', '', NULL,
				?, ?, ?, ?, ?, ?, ?
			)
		`,
			p.RequestID, p.Title, string(status), p.Agent, p.Host, p.Workdir, worktreeSpecJSON, p.TargetRef,
			p.Instructions, p.AcceptanceCriteria, parentArg,
			nowStr, nowStr, p.Model, p.Pool, altPoolsJSON, p.SessionID, strings.TrimSpace(p.CreatedBy),
		)
		if err != nil {
			if isUniqueConstraintError(err) {
				existing, rerr := queryJobByRequestID(ctx, conn, p.RequestID)
				if rerr == nil {
					result = existing
					created = false
					return nil
				}
			}
			return err
		}

		jobID, err := res.LastInsertId()
		if err != nil {
			return err
		}

		if status == StatusQueued {
			if err := s.insertOutboxTx(ctx, conn, jobID, "create", "queued", nowStr); err != nil {
				return err
			}
		}

		job, err := queryJobByID(ctx, conn, jobID)
		if err != nil {
			return err
		}
		result = job
		created = true
		*changes = append(*changes, statusChange{jobID: jobID, next: status})
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return result, created, nil
}

// Claim atomically checks that claimed count < maxClaimed and claims the oldest queued job
// inside a single BEGIN IMMEDIATE transaction. Returns (nil, false, nil) if at capacity or no queued jobs.
func (s *Store) Claim(ctx context.Context, maxClaimed int) (*Job, bool, error) {
	if maxClaimed <= 0 {
		return nil, false, nil
	}

	var claimed *Job

	err := s.withJobTx(ctx, func(conn *sql.Conn, changes *[]statusChange) error {
		var currentClaimed int
		if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE status = 'claimed'`).Scan(&currentClaimed); err != nil {
			return err
		}
		if currentClaimed >= maxClaimed {
			claimed = nil
			return nil
		}

		var candidateID int64
		err := conn.QueryRowContext(ctx, `
			SELECT id FROM jobs
			WHERE status = 'queued'
			ORDER BY created_at ASC, id ASC
			LIMIT 1
		`).Scan(&candidateID)
		if errors.Is(err, sql.ErrNoRows) {
			claimed = nil
			return nil
		}
		if err != nil {
			return err
		}

		token, err := randomHex(16)
		if err != nil {
			return fmt.Errorf("generate claim token: %w", err)
		}
		nowStr := formatTime(time.Now().UTC())

		res, err := conn.ExecContext(ctx, `
			UPDATE jobs
			SET status = 'claimed',
				claimed_by = ?,
				claimed_at = ?,
				attempts = attempts + 1,
				updated_at = ?
			WHERE id = ? AND status = 'queued'
		`, token, nowStr, nowStr, candidateID)
		if err != nil {
			return err
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if affected == 0 {
			claimed = nil
			return nil
		}

		job, err := queryJobByID(ctx, conn, candidateID)
		if err != nil {
			return err
		}
		claimed = job
		*changes = append(*changes, statusChange{jobID: candidateID, prev: StatusQueued, next: StatusClaimed})
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	if claimed == nil {
		return nil, false, nil
	}
	return claimed, true, nil
}

// ClaimByID atomically checks that jobID is queued and claimed count < maxClaimed,
// then claims jobID inside a single BEGIN IMMEDIATE transaction.
// Returns ErrNotFound if jobID does not exist, or (nil, false, nil) if at capacity
// or the job is no longer queued.
func (s *Store) ClaimByID(ctx context.Context, jobID int64, maxClaimed int) (*Job, bool, error) {
	var claimed *Job

	err := s.withJobTx(ctx, func(conn *sql.Conn, changes *[]statusChange) error {
		var statusStr string
		err := conn.QueryRowContext(ctx, `SELECT status FROM jobs WHERE id = ?`, jobID).Scan(&statusStr)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: job %d", ErrNotFound, jobID)
		}
		if err != nil {
			return err
		}
		if Status(statusStr) != StatusQueued {
			claimed = nil
			return nil
		}
		if maxClaimed <= 0 {
			claimed = nil
			return nil
		}

		var currentClaimed int
		if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE status = 'claimed'`).Scan(&currentClaimed); err != nil {
			return err
		}
		if currentClaimed >= maxClaimed {
			claimed = nil
			return nil
		}

		token, err := randomHex(16)
		if err != nil {
			return fmt.Errorf("generate claim token: %w", err)
		}
		nowStr := formatTime(time.Now().UTC())

		res, err := conn.ExecContext(ctx, `
			UPDATE jobs
			SET status = 'claimed',
				claimed_by = ?,
				claimed_at = ?,
				attempts = attempts + 1,
				updated_at = ?
			WHERE id = ? AND status = 'queued'
		`, token, nowStr, nowStr, jobID)
		if err != nil {
			return err
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if affected == 0 {
			claimed = nil
			return nil
		}

		job, err := queryJobByID(ctx, conn, jobID)
		if err != nil {
			return err
		}
		claimed = job
		*changes = append(*changes, statusChange{jobID: jobID, prev: StatusQueued, next: StatusClaimed})
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	if claimed == nil {
		return nil, false, nil
	}
	return claimed, true, nil
}

// Complete marks a claimed job as done (verifying claimToken) and promotes its direct
// waiting children to queued (recording signal_outbox rows) in the same transaction.
func (s *Store) Complete(ctx context.Context, jobID int64, claimToken, resultText string) (*CompleteResult, error) {
	var out CompleteResult

	err := s.withJobTx(ctx, func(conn *sql.Conn, changes *[]statusChange) error {
		nowStr := formatTime(time.Now().UTC())

		res, err := conn.ExecContext(ctx, `
			UPDATE jobs
			SET status = 'done',
				result_text = ?,
				updated_at = ?
			WHERE id = ? AND status = 'claimed' AND claimed_by = ?
		`, resultText, nowStr, jobID, claimToken)
		if err != nil {
			return err
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if affected == 0 {
			return classifyClaimedConflict(ctx, conn, jobID, claimToken)
		}

		if _, err := conn.ExecContext(ctx, `DELETE FROM leases WHERE job_id = ?`, jobID); err != nil {
			return err
		}

		*changes = append(*changes, statusChange{jobID: jobID, prev: StatusClaimed, next: StatusDone})

		// Promote direct waiting children to queued.
		childIDs, err := listDirectWaitingChildren(ctx, conn, jobID)
		if err != nil {
			return err
		}
		for _, cid := range childIDs {
			cres, err := conn.ExecContext(ctx, `
				UPDATE jobs
				SET status = 'queued',
					updated_at = ?
				WHERE id = ? AND status = 'waiting'
			`, nowStr, cid)
			if err != nil {
				return err
			}
			caffected, err := cres.RowsAffected()
			if err != nil {
				return err
			}
			if caffected > 0 {
				if err := s.insertOutboxTx(ctx, conn, cid, "promote", "queued", nowStr); err != nil {
					return err
				}
				out.PromotedChildIDs = append(out.PromotedChildIDs, cid)
				*changes = append(*changes, statusChange{jobID: cid, prev: StatusWaiting, next: StatusQueued})
			}
		}

		job, err := queryJobByID(ctx, conn, jobID)
		if err != nil {
			return err
		}
		out.Job = job
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// Fail marks a claimed job as failed (verifying claimToken) and recursively cancels
// all waiting descendants with the parent's failure reason in last_error.
func (s *Store) Fail(ctx context.Context, jobID int64, claimToken, reason string) (*FailResult, error) {
	return s.FailWithResult(ctx, jobID, claimToken, reason, "")
}

// FailWithResult marks a claimed job as failed (verifying claimToken), recording both
// last_error and result_text (for example, per-criterion evaluation details), and
// recursively cancels all waiting descendants.
func (s *Store) FailWithResult(ctx context.Context, jobID int64, claimToken, reason, resultText string) (*FailResult, error) {
	var out FailResult

	err := s.withJobTx(ctx, func(conn *sql.Conn, changes *[]statusChange) error {
		nowStr := formatTime(time.Now().UTC())

		res, err := conn.ExecContext(ctx, `
			UPDATE jobs
			SET status = 'failed',
				last_error = ?,
				result_text = ?,
				updated_at = ?
			WHERE id = ? AND status = 'claimed' AND claimed_by = ?
		`, reason, resultText, nowStr, jobID, claimToken)
		if err != nil {
			return err
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if affected == 0 {
			return classifyClaimedConflict(ctx, conn, jobID, claimToken)
		}

		if _, err := conn.ExecContext(ctx, `DELETE FROM leases WHERE job_id = ?`, jobID); err != nil {
			return err
		}

		childErr := formatCascadeReason(jobID, "failed", reason)
		cancelledIDs, err := cascadeCancelWaitingDescendants(ctx, conn, jobID, childErr, nowStr)
		if err != nil {
			return err
		}
		out.CancelledChildIDs = cancelledIDs
		*changes = append(*changes, statusChange{jobID: jobID, prev: StatusClaimed, next: StatusFailed})
		*changes = appendCascadeChanges(*changes, cancelledIDs)

		job, err := queryJobByID(ctx, conn, jobID)
		if err != nil {
			return err
		}
		out.Job = job
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// Cancel transitions a draft, queued, or waiting job to cancelled and recursively cancels
// any waiting descendants.
func (s *Store) Cancel(ctx context.Context, jobID int64, reason string) (*CancelResult, error) {
	var out CancelResult

	err := s.withJobTx(ctx, func(conn *sql.Conn, changes *[]statusChange) error {
		nowStr := formatTime(time.Now().UTC())

		prev, err := queryJobStatus(ctx, conn, jobID)
		if err != nil {
			return err
		}

		res, err := conn.ExecContext(ctx, `
			UPDATE jobs
			SET status = 'cancelled',
				last_error = ?,
				updated_at = ?
			WHERE id = ? AND status IN ('draft', 'queued', 'waiting')
		`, reason, nowStr, jobID)
		if err != nil {
			return err
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if affected == 0 {
			return classifyStatusConflict(ctx, conn, jobID, "draft|queued|waiting")
		}

		childErr := formatCascadeReason(jobID, "cancelled", reason)
		cancelledIDs, err := cascadeCancelWaitingDescendants(ctx, conn, jobID, childErr, nowStr)
		if err != nil {
			return err
		}
		out.CancelledChildIDs = cancelledIDs
		*changes = append(*changes, statusChange{jobID: jobID, prev: prev, next: StatusCancelled})
		*changes = appendCascadeChanges(*changes, cancelledIDs)

		job, err := queryJobByID(ctx, conn, jobID)
		if err != nil {
			return err
		}
		out.Job = job
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// CancelClaimed transitions a claimed job to cancelled (verifying claimToken) and
// recursively cancels any waiting descendants.
func (s *Store) CancelClaimed(ctx context.Context, jobID int64, claimToken, reason string) (*CancelResult, error) {
	var out CancelResult

	err := s.withJobTx(ctx, func(conn *sql.Conn, changes *[]statusChange) error {
		nowStr := formatTime(time.Now().UTC())

		res, err := conn.ExecContext(ctx, `
			UPDATE jobs
			SET status = 'cancelled',
				last_error = ?,
				updated_at = ?
			WHERE id = ? AND status = 'claimed' AND claimed_by = ?
		`, reason, nowStr, jobID, claimToken)
		if err != nil {
			return err
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if affected == 0 {
			return classifyClaimedConflict(ctx, conn, jobID, claimToken)
		}

		if _, err := conn.ExecContext(ctx, `DELETE FROM leases WHERE job_id = ?`, jobID); err != nil {
			return err
		}

		childErr := formatCascadeReason(jobID, "cancelled", reason)
		cancelledIDs, err := cascadeCancelWaitingDescendants(ctx, conn, jobID, childErr, nowStr)
		if err != nil {
			return err
		}
		out.CancelledChildIDs = cancelledIDs
		*changes = append(*changes, statusChange{jobID: jobID, prev: StatusClaimed, next: StatusCancelled})
		*changes = appendCascadeChanges(*changes, cancelledIDs)

		job, err := queryJobByID(ctx, conn, jobID)
		if err != nil {
			return err
		}
		out.Job = job
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// Requeue moves a failed or cancelled job back to queued, resets attempts to 0,
// and emits a signal_outbox row. Cancelled children are NOT revived.
func (s *Store) Requeue(ctx context.Context, jobID int64) (*Job, error) {
	var job *Job

	err := s.withJobTx(ctx, func(conn *sql.Conn, changes *[]statusChange) error {
		nowStr := formatTime(time.Now().UTC())

		prev, err := queryJobStatus(ctx, conn, jobID)
		if err != nil {
			return err
		}

		res, err := conn.ExecContext(ctx, `
			UPDATE jobs
			SET status = 'queued',
				attempts = 0,
				claimed_by = '',
				claimed_at = NULL,
				last_error = '',
				updated_at = ?
			WHERE id = ? AND status IN ('failed', 'cancelled')
		`, nowStr, jobID)
		if err != nil {
			return err
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if affected == 0 {
			return classifyStatusConflict(ctx, conn, jobID, "failed|cancelled")
		}

		if err := s.insertOutboxTx(ctx, conn, jobID, "requeue", "queued", nowStr); err != nil {
			return err
		}

		j, err := queryJobByID(ctx, conn, jobID)
		if err != nil {
			return err
		}
		job = j
		*changes = append(*changes, statusChange{jobID: jobID, prev: prev, next: StatusQueued})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return job, nil
}

// Reset moves a claimed job back to queued while preserving attempts,
// clears claim ownership/leases, and emits a signal_outbox row.
func (s *Store) Reset(ctx context.Context, jobID int64) (*Job, error) {
	var job *Job

	err := s.withJobTx(ctx, func(conn *sql.Conn, changes *[]statusChange) error {
		nowStr := formatTime(time.Now().UTC())

		res, err := conn.ExecContext(ctx, `
			UPDATE jobs
			SET status = 'queued',
				claimed_by = '',
				claimed_at = NULL,
				updated_at = ?
			WHERE id = ? AND status = 'claimed'
		`, nowStr, jobID)
		if err != nil {
			return err
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if affected == 0 {
			return classifyStatusConflict(ctx, conn, jobID, "claimed")
		}

		if _, err := conn.ExecContext(ctx, `DELETE FROM leases WHERE job_id = ?`, jobID); err != nil {
			return err
		}
		if err := s.insertOutboxTx(ctx, conn, jobID, "reset", "queued", nowStr); err != nil {
			return err
		}

		j, err := queryJobByID(ctx, conn, jobID)
		if err != nil {
			return err
		}
		job = j
		*changes = append(*changes, statusChange{jobID: jobID, prev: StatusClaimed, next: StatusQueued})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return job, nil
}

// RejectInvalid transitions a queued job directly to failed and cancels any waiting descendants.
func (s *Store) RejectInvalid(ctx context.Context, jobID int64, reason string) (*FailResult, error) {
	var out FailResult

	err := s.withJobTx(ctx, func(conn *sql.Conn, changes *[]statusChange) error {
		nowStr := formatTime(time.Now().UTC())

		res, err := conn.ExecContext(ctx, `
			UPDATE jobs
			SET status = 'failed',
				last_error = ?,
				updated_at = ?
			WHERE id = ? AND status = 'queued'
		`, reason, nowStr, jobID)
		if err != nil {
			return err
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if affected == 0 {
			return classifyStatusConflict(ctx, conn, jobID, "queued")
		}

		childErr := formatCascadeReason(jobID, "failed", reason)
		cancelledIDs, err := cascadeCancelWaitingDescendants(ctx, conn, jobID, childErr, nowStr)
		if err != nil {
			return err
		}
		out.CancelledChildIDs = cancelledIDs
		*changes = append(*changes, statusChange{jobID: jobID, prev: StatusQueued, next: StatusFailed})
		*changes = appendCascadeChanges(*changes, cancelledIDs)

		job, err := queryJobByID(ctx, conn, jobID)
		if err != nil {
			return err
		}
		out.Job = job
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// GetJob returns a job by ID, or ErrNotFound if it does not exist.
func (s *Store) GetJob(ctx context.Context, jobID int64) (*Job, error) {
	return queryJobByID(ctx, s.readDB, jobID)
}

// GetJobByRequestID returns a job by request_id, or ErrNotFound if it does not exist.
func (s *Store) GetJobByRequestID(ctx context.Context, requestID string) (*Job, error) {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return nil, fmt.Errorf("%w: request_id must not be empty", ErrInvalidInput)
	}
	return queryJobByRequestID(ctx, s.readDB, requestID)
}

// ListJobs returns a cursor-paginated list of jobs matching filter.Status and optional filters.
// Status is mandatory; omitting or passing an invalid status returns ErrInvalidInput.
func (s *Store) ListJobs(ctx context.Context, filter ListJobsFilter) (*ListJobsPage, error) {
	if !filter.Status.Valid() {
		return nil, fmt.Errorf("%w: valid status filter is required (got %q)", ErrInvalidInput, filter.Status)
	}
	limit := filter.Limit
	if limit <= 0 {
		limit = DefaultListLimit
	}
	if limit > MaxListLimit {
		limit = MaxListLimit
	}

	conditions := []string{"status = ?"}
	args := []any{string(filter.Status)}

	if agent := strings.TrimSpace(filter.Agent); agent != "" {
		conditions = append(conditions, "agent = ?")
		args = append(args, agent)
	}
	if host := strings.TrimSpace(filter.Host); host != "" {
		conditions = append(conditions, "host = ?")
		args = append(args, host)
	}
	if q := strings.TrimSpace(filter.Query); q != "" {
		conditions = append(conditions, `LOWER(title) LIKE ? ESCAPE '\'`)
		args = append(args, "%"+escapeLikePattern(strings.ToLower(q))+"%")
	}
	if filter.ParentID != nil {
		conditions = append(conditions, "parent_id = ?")
		args = append(args, *filter.ParentID)
	}

	fetchLimit := limit + 1
	orderClause := "ORDER BY id ASC"
	if filter.Descending {
		orderClause = "ORDER BY id DESC"
		if filter.Cursor > 0 {
			conditions = append(conditions, "id < ?")
			args = append(args, filter.Cursor)
		}
	} else if filter.Cursor > 0 {
		conditions = append(conditions, "id > ?")
		args = append(args, filter.Cursor)
	}

	query := `SELECT ` + jobColumns + ` FROM jobs WHERE ` + strings.Join(conditions, " AND ") + ` ` + orderClause + ` LIMIT ?`
	args = append(args, fetchLimit)

	rows, err := s.readDB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var jobs []*Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	page := &ListJobsPage{}
	if len(jobs) > limit {
		page.HasMore = true
		jobs = jobs[:limit]
	}
	page.Jobs = jobs
	if page.HasMore && len(jobs) > 0 {
		page.NextCursor = jobs[len(jobs)-1].ID
	}
	return page, nil
}

func escapeLikePattern(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// MarkDelivered records delivered_at on a done or failed job. It is idempotent for already-delivered jobs.
func (s *Store) MarkDelivered(ctx context.Context, jobID int64) (*Job, error) {
	var job *Job

	err := s.withImmediateTx(ctx, func(conn *sql.Conn) error {
		nowStr := formatTime(time.Now().UTC())

		res, err := conn.ExecContext(ctx, `
			UPDATE jobs
			SET delivered_at = COALESCE(delivered_at, ?),
				updated_at = CASE WHEN delivered_at IS NULL THEN ? ELSE updated_at END
			WHERE id = ? AND status IN ('done', 'failed')
		`, nowStr, nowStr, jobID)
		if err != nil {
			return err
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if affected == 0 {
			return classifyStatusConflict(ctx, conn, jobID, "done|failed")
		}

		j, err := queryJobByID(ctx, conn, jobID)
		if err != nil {
			return err
		}
		job = j
		return nil
	})
	if err != nil {
		return nil, err
	}
	return job, nil
}

// ListUndelivered returns done and failed jobs whose delivered_at is NULL, ordered by updated_at ASC, id ASC.
func (s *Store) ListUndelivered(ctx context.Context, limit int) ([]*Job, error) {
	return s.ListDeliveries(ctx, DeliveryStatusUndelivered, limit)
}

// ListDeliveries returns done and failed jobs filtered by delivery status.
// Empty status defaults to DeliveryStatusUndelivered.
func (s *Store) ListDeliveries(ctx context.Context, status DeliveryStatus, limit int) ([]*Job, error) {
	page, err := s.ListDeliveriesPage(ctx, ListDeliveriesFilter{
		Status: status,
		Limit:  limit,
	})
	if err != nil {
		return nil, err
	}
	return page.Jobs, nil
}

// ListDeliveriesPage returns a cursor-paginated page of done/failed jobs filtered by delivery status.
// Cursor format is "<timestamp RFC3339Nano>|<job_id>".
func (s *Store) ListDeliveriesPage(ctx context.Context, filter ListDeliveriesFilter) (*ListDeliveriesPage, error) {
	status := filter.Status
	if status == "" {
		status = DeliveryStatusUndelivered
	}
	if !status.Valid() {
		return nil, fmt.Errorf("%w: invalid delivery status %q", ErrInvalidInput, status)
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
		orderBy    string
		timeCol    string
		asc        bool
	)
	switch status {
	case DeliveryStatusUndelivered:
		conditions = append(conditions, "status IN ('done', 'failed')", "delivered_at IS NULL")
		orderBy = "ORDER BY updated_at ASC, id ASC"
		timeCol = "updated_at"
		asc = true
	case DeliveryStatusDelivered:
		conditions = append(conditions, "status IN ('done', 'failed')", "delivered_at IS NOT NULL")
		orderBy = "ORDER BY delivered_at DESC, id DESC"
		timeCol = "delivered_at"
		asc = false
	case DeliveryStatusAll:
		conditions = append(conditions, "status IN ('done', 'failed')")
		orderBy = "ORDER BY updated_at DESC, id DESC"
		timeCol = "updated_at"
		asc = false
	}

	if cursor := strings.TrimSpace(filter.Cursor); cursor != "" {
		cursorTime, cursorID, err := parseCompositeTimeIDCursor(cursor)
		if err != nil {
			return nil, fmt.Errorf("%w: invalid cursor %q: %v", ErrInvalidInput, cursor, err)
		}
		if asc {
			conditions = append(conditions, fmt.Sprintf("(%s > ? OR (%s = ? AND id > ?))", timeCol, timeCol))
		} else {
			conditions = append(conditions, fmt.Sprintf("(%s < ? OR (%s = ? AND id < ?))", timeCol, timeCol))
		}
		args = append(args, cursorTime, cursorTime, cursorID)
	}

	query := `SELECT ` + jobColumns + ` FROM jobs WHERE ` + strings.Join(conditions, " AND ") + ` ` + orderBy + ` LIMIT ?`
	args = append(args, limit+1)

	rows, err := s.readDB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var jobs []*Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	page := &ListDeliveriesPage{}
	if len(jobs) > limit {
		page.HasMore = true
		jobs = jobs[:limit]
	}
	page.Jobs = jobs
	if page.HasMore && len(jobs) > 0 {
		last := jobs[len(jobs)-1]
		cursorTS := last.UpdatedAt
		if status == DeliveryStatusDelivered && last.DeliveredAt != nil {
			cursorTS = *last.DeliveredAt
		}
		page.NextCursor = formatCompositeTimeIDCursor(cursorTS, last.ID)
	}
	return page, nil
}

// CountByStatus returns a map containing the count for every defined job status.
func (s *Store) CountByStatus(ctx context.Context) (map[Status]int64, error) {
	counts := make(map[Status]int64, len(AllStatuses))
	for _, st := range AllStatuses {
		counts[st] = 0
	}

	rows, err := s.readDB.QueryContext(ctx, `SELECT status, COUNT(*) FROM jobs GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			st string
			n  int64
		)
		if err := rows.Scan(&st, &n); err != nil {
			return nil, err
		}
		counts[Status(st)] = n
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return counts, nil
}

// ListPendingOutbox returns undispatched outbox events ordered by id ascending.
func (s *Store) ListPendingOutbox(ctx context.Context, limit int) ([]OutboxEvent, error) {
	if limit <= 0 {
		limit = DefaultListLimit
	}
	rows, err := s.readDB.QueryContext(ctx, `
		SELECT id, event_id, job_id, env, src, hop, event, created_at, dispatched_at
		FROM signal_outbox
		WHERE dispatched_at IS NULL
		ORDER BY id ASC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []OutboxEvent
	for rows.Next() {
		var (
			ev           OutboxEvent
			createdStr   string
			dispatchedNS sql.NullString
		)
		if err := rows.Scan(
			&ev.ID, &ev.EventID, &ev.JobID, &ev.Env, &ev.Src, &ev.Hop, &ev.Event, &createdStr, &dispatchedNS,
		); err != nil {
			return nil, err
		}
		t, err := parseTime(createdStr)
		if err != nil {
			return nil, err
		}
		ev.CreatedAt = t
		if dispatchedNS.Valid {
			dt, err := parseTime(dispatchedNS.String)
			if err != nil {
				return nil, err
			}
			ev.DispatchedAt = &dt
		}
		events = append(events, ev)
	}
	return events, rows.Err()
}

// MarkOutboxDispatched sets dispatched_at on an outbox row if it has not been dispatched yet.
func (s *Store) MarkOutboxDispatched(ctx context.Context, id int64) error {
	nowStr := formatTime(time.Now().UTC())
	return s.withImmediateTx(ctx, func(conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, `
			UPDATE signal_outbox
			SET dispatched_at = COALESCE(dispatched_at, ?)
			WHERE id = ?
		`, nowStr, id)
		return err
	})
}

// RecordReconcilerDedup atomically records dedupKey in reconciler_dedup.
// It returns isNew=true on the first sighting (caller should emit the report)
// and isNew=false on subsequent sightings (caller should suppress duplicate reports).
func (s *Store) RecordReconcilerDedup(ctx context.Context, dedupKey string) (bool, error) {
	if strings.TrimSpace(dedupKey) == "" {
		return false, fmt.Errorf("%w: dedup_key must not be empty", ErrInvalidInput)
	}
	var isNew bool
	nowStr := formatTime(time.Now().UTC())
	err := s.withImmediateTx(ctx, func(conn *sql.Conn) error {
		res, err := conn.ExecContext(ctx, `
			INSERT OR IGNORE INTO reconciler_dedup (dedup_key, first_seen_at, last_reported_at, count)
			VALUES (?, ?, ?, 1)
		`, dedupKey, nowStr, nowStr)
		if err != nil {
			return err
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if affected > 0 {
			isNew = true
			return nil
		}
		_, err = conn.ExecContext(ctx, `
			UPDATE reconciler_dedup
			SET last_reported_at = ?, count = count + 1
			WHERE dedup_key = ?
		`, nowStr, dedupKey)
		isNew = false
		return err
	})
	if err != nil {
		return false, err
	}
	return isNew, nil
}

// RecordGateDecision inserts a Quota Gate evaluation record into gate_decisions.
// Either JobID > 0 or HeldID != "" must be provided.
func (s *Store) RecordGateDecision(ctx context.Context, d GateDecision) (*GateDecision, error) {
	heldID := strings.TrimSpace(d.HeldID)
	if d.JobID < 0 || (d.JobID == 0 && heldID == "") {
		return nil, fmt.Errorf("%w: job_id must be > 0 or held_id must be set", ErrInvalidInput)
	}
	switch d.Decision {
	case "would_pass", "would_hold", "unknown":
	default:
		return nil, fmt.Errorf("%w: invalid gate decision %q", ErrInvalidInput, d.Decision)
	}

	mode := strings.TrimSpace(d.Mode)
	if mode == "" {
		mode = "dry-run"
	}
	decidedAt := d.DecidedAt.UTC()
	if decidedAt.IsZero() {
		decidedAt = time.Now().UTC()
	}
	altPoolsJSON, err := encodeAltPools(d.AltPools)
	if err != nil {
		return nil, fmt.Errorf("%w: encode alt_pools: %v", ErrInvalidInput, err)
	}

	var measuredArg any
	var measuredCopy *time.Time
	if d.MeasuredAt != nil && !d.MeasuredAt.IsZero() {
		t := d.MeasuredAt.UTC()
		measuredCopy = &t
		measuredArg = formatTime(t)
	}

	var jobIDArg any
	if d.JobID > 0 {
		jobIDArg = d.JobID
	}

	bypassedInt := 0
	if d.Bypassed {
		bypassedInt = 1
	}
	heldInt := 0
	if d.Held {
		heldInt = 1
	}

	var insertedID int64
	err = s.withImmediateTx(ctx, func(conn *sql.Conn) error {
		res, err := conn.ExecContext(ctx, `
			INSERT INTO gate_decisions (
				job_id, held_id, held, mode, requested_pool, alt_pools, decision,
				blocked_by, selected_pool, bypassed, reason, measured_at, decided_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`,
			jobIDArg, heldID, heldInt, mode, strings.TrimSpace(d.RequestedPool), altPoolsJSON, d.Decision,
			strings.TrimSpace(d.BlockedBy), strings.TrimSpace(d.SelectedPool), bypassedInt,
			strings.TrimSpace(d.Reason), measuredArg, formatTime(decidedAt),
		)
		if err != nil {
			return err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		insertedID = id
		return nil
	})
	if err != nil {
		return nil, err
	}

	out := d
	out.ID = insertedID
	out.HeldID = heldID
	out.Mode = mode
	out.RequestedPool = strings.TrimSpace(d.RequestedPool)
	out.BlockedBy = strings.TrimSpace(d.BlockedBy)
	out.SelectedPool = strings.TrimSpace(d.SelectedPool)
	out.Reason = strings.TrimSpace(d.Reason)
	out.MeasuredAt = measuredCopy
	out.DecidedAt = decidedAt
	if len(d.AltPools) > 0 {
		out.AltPools = append([]string(nil), d.AltPools...)
	}
	return &out, nil
}

// ListGateDecisions returns recorded gate decisions ordered newest-first (id DESC).
func (s *Store) ListGateDecisions(ctx context.Context, limit int) ([]*GateDecision, error) {
	return s.ListGateDecisionsFiltered(ctx, GateDecisionsFilter{Limit: limit})
}

// ListGateDecisionsFiltered returns recorded gate decisions matching optional job_id/held_id filters,
// ordered newest-first (id DESC).
func (s *Store) ListGateDecisionsFiltered(ctx context.Context, filter GateDecisionsFilter) ([]*GateDecision, error) {
	page, err := s.ListGateDecisionsPage(ctx, filter)
	if err != nil {
		return nil, err
	}
	return page.Decisions, nil
}

// ListGateDecisionsPage returns a cursor-paginated page of gate decisions matching optional
// job_id/held_id filters, ordered newest-first (id DESC). When filter.Cursor > 0, returns rows with id < cursor.
func (s *Store) ListGateDecisionsPage(ctx context.Context, filter GateDecisionsFilter) (*ListGateDecisionsPage, error) {
	limit := filter.Limit
	if limit <= 0 {
		limit = DefaultListLimit
	}
	if limit > MaxListLimit {
		limit = MaxListLimit
	}
	if filter.Cursor < 0 {
		return nil, fmt.Errorf("%w: invalid cursor %d", ErrInvalidInput, filter.Cursor)
	}

	var (
		conditions []string
		args       []any
	)
	if filter.JobID != nil {
		conditions = append(conditions, "job_id = ?")
		args = append(args, *filter.JobID)
	}
	if heldID := strings.TrimSpace(filter.HeldID); heldID != "" {
		conditions = append(conditions, "held_id = ?")
		args = append(args, heldID)
	}
	if filter.Cursor > 0 {
		conditions = append(conditions, "id < ?")
		args = append(args, filter.Cursor)
	}

	query := `
		SELECT
			id, job_id, held_id, held, mode, requested_pool, alt_pools, decision,
			blocked_by, selected_pool, bypassed, reason, measured_at, decided_at
		FROM gate_decisions
	`
	if len(conditions) > 0 {
		query += ` WHERE ` + strings.Join(conditions, " AND ")
	}
	query += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit+1)

	rows, err := s.readDB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var decisions []*GateDecision
	for rows.Next() {
		var (
			d           GateDecision
			jobIDNI     sql.NullInt64
			heldInt     int
			altPoolsStr string
			bypassedInt int
			measuredNS  sql.NullString
			decidedStr  string
		)
		if err := rows.Scan(
			&d.ID, &jobIDNI, &d.HeldID, &heldInt, &d.Mode, &d.RequestedPool, &altPoolsStr, &d.Decision,
			&d.BlockedBy, &d.SelectedPool, &bypassedInt, &d.Reason, &measuredNS, &decidedStr,
		); err != nil {
			return nil, err
		}
		if jobIDNI.Valid {
			d.JobID = jobIDNI.Int64
		}
		d.Held = heldInt != 0
		d.Bypassed = bypassedInt != 0
		altPools, err := decodeAltPools(altPoolsStr)
		if err != nil {
			return nil, fmt.Errorf("decode gate decision alt_pools: %w", err)
		}
		d.AltPools = altPools
		if measuredNS.Valid {
			mt, err := parseTime(measuredNS.String)
			if err != nil {
				return nil, fmt.Errorf("parse gate decision measured_at: %w", err)
			}
			d.MeasuredAt = &mt
		}
		dt, err := parseTime(decidedStr)
		if err != nil {
			return nil, fmt.Errorf("parse gate decision decided_at: %w", err)
		}
		d.DecidedAt = dt
		decisions = append(decisions, &d)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	page := &ListGateDecisionsPage{}
	if len(decisions) > limit {
		page.HasMore = true
		decisions = decisions[:limit]
	}
	page.Decisions = decisions
	if page.HasMore && len(decisions) > 0 {
		page.NextCursor = decisions[len(decisions)-1].ID
	}
	return page, nil
}

// --- internal helpers ---

type queryRower interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

type rowScanner interface {
	Scan(dest ...any) error
}

const jobColumns = `
	id, request_id, title, status, agent, host, workdir, worktree_spec, target_ref,
	instructions, acceptance_criteria, parent_id,
	claimed_by, claimed_at, attempts, result_text, last_error, delivered_at,
	created_at, updated_at, model, pool, alt_pools, session_id, created_by
`

func queryJobByID(ctx context.Context, q queryRower, jobID int64) (*Job, error) {
	row := q.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE id = ?`, jobID)
	j, err := scanJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return j, err
}

func queryJobByRequestID(ctx context.Context, q queryRower, requestID string) (*Job, error) {
	row := q.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE request_id = ?`, requestID)
	j, err := scanJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return j, err
}

func scanJob(s rowScanner) (*Job, error) {
	var (
		j               Job
		statusStr       string
		worktreeSpecStr string
		parentID        sql.NullInt64
		claimedAt       sql.NullString
		deliveredAt     sql.NullString
		createdAt       string
		updatedAt       string
		altPoolsStr     string
	)
	err := s.Scan(
		&j.ID, &j.RequestID, &j.Title, &statusStr,
		&j.Agent, &j.Host, &j.Workdir, &worktreeSpecStr, &j.TargetRef,
		&j.Instructions, &j.AcceptanceCriteria, &parentID,
		&j.ClaimedBy, &claimedAt, &j.Attempts, &j.ResultText, &j.LastError, &deliveredAt,
		&createdAt, &updatedAt, &j.Model, &j.Pool, &altPoolsStr, &j.SessionID, &j.CreatedBy,
	)
	if err != nil {
		return nil, err
	}
	j.Status = Status(statusStr)
	if parentID.Valid {
		v := parentID.Int64
		j.ParentID = &v
	}
	if claimedAt.Valid {
		t, err := parseTime(claimedAt.String)
		if err != nil {
			return nil, fmt.Errorf("parse claimed_at: %w", err)
		}
		j.ClaimedAt = &t
	}
	if deliveredAt.Valid {
		t, err := parseTime(deliveredAt.String)
		if err != nil {
			return nil, fmt.Errorf("parse delivered_at: %w", err)
		}
		j.DeliveredAt = &t
	}
	ct, err := parseTime(createdAt)
	if err != nil {
		return nil, fmt.Errorf("parse created_at: %w", err)
	}
	j.CreatedAt = ct
	ut, err := parseTime(updatedAt)
	if err != nil {
		return nil, fmt.Errorf("parse updated_at: %w", err)
	}
	j.UpdatedAt = ut

	altPools, err := decodeAltPools(altPoolsStr)
	if err != nil {
		return nil, fmt.Errorf("decode alt_pools: %w", err)
	}
	j.AltPools = altPools

	wtSpec, err := decodeWorktreeSpec(worktreeSpecStr)
	if err != nil {
		return nil, fmt.Errorf("decode worktree_spec: %w", err)
	}
	j.WorktreeSpec = wtSpec
	return &j, nil
}

func (s *Store) withImmediateTx(ctx context.Context, fn func(conn *sql.Conn) error) error {
	return s.withImmediateTxCommit(ctx, fn, nil)
}

// commitHook wraps COMMIT while the write connection is still held, so no other writer
// runs until it returns. It must call commitTx once and return an error only if that
// call failed.
type commitHook func(conn *sql.Conn, commitTx func() error) error

// withImmediateTxCommit is withImmediateTx with an optional commit hook.
func (s *Store) withImmediateTxCommit(ctx context.Context, fn func(conn *sql.Conn) error, hook commitHook) error {
	var lastErr error
	for attempt := 0; attempt < maxBusyRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}

		err := s.runImmediateTxOnce(ctx, fn, hook)
		if err == nil {
			return nil
		}
		if !isBusyError(err) {
			return err
		}
		lastErr = err

		backoff := time.Duration(5*(attempt+1)) * time.Millisecond
		if backoff > 100*time.Millisecond {
			backoff = 100 * time.Millisecond
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return fmt.Errorf("sqlite busy after %d retries: %w", maxBusyRetries, lastErr)
}

func (s *Store) runImmediateTxOnce(ctx context.Context, fn func(conn *sql.Conn) error, hook commitHook) error {
	conn, err := s.writeDB.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}

	if err := fn(conn); err != nil {
		_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		return err
	}

	commitTx := func() error {
		_, err := conn.ExecContext(ctx, "COMMIT")
		return err
	}
	if hook != nil {
		err = hook(conn, commitTx)
	} else {
		err = commitTx()
	}
	if err != nil {
		_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		return err
	}
	return nil
}

func (s *Store) insertOutboxTx(ctx context.Context, conn *sql.Conn, jobID int64, src, event, createdAt string) error {
	eventID, err := newUUIDv4()
	if err != nil {
		return fmt.Errorf("generate event_id: %w", err)
	}
	_, err = conn.ExecContext(ctx, `
		INSERT INTO signal_outbox (event_id, job_id, env, src, hop, event, created_at)
		VALUES (?, ?, ?, ?, 0, ?, ?)
	`, eventID, jobID, s.instanceID, src, event, createdAt)
	return err
}

func listDirectWaitingChildren(ctx context.Context, conn *sql.Conn, parentID int64) ([]int64, error) {
	rows, err := conn.QueryContext(ctx, `
		SELECT id FROM jobs
		WHERE parent_id = ? AND status = 'waiting'
		ORDER BY id ASC
	`, parentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// cascadeCancelWaitingDescendants performs a cycle-safe BFS over waiting descendants and cancels them.
func cascadeCancelWaitingDescendants(ctx context.Context, conn *sql.Conn, rootID int64, lastError, nowStr string) ([]int64, error) {
	visited := map[int64]bool{rootID: true}
	queue := []int64{rootID}
	var cancelled []int64

	for len(queue) > 0 {
		curr := queue[0]
		queue = queue[1:]

		children, err := listDirectWaitingChildren(ctx, conn, curr)
		if err != nil {
			return nil, err
		}
		for _, cid := range children {
			if visited[cid] {
				continue
			}
			visited[cid] = true

			res, err := conn.ExecContext(ctx, `
				UPDATE jobs
				SET status = 'cancelled',
					last_error = ?,
					updated_at = ?
				WHERE id = ? AND status = 'waiting'
			`, lastError, nowStr, cid)
			if err != nil {
				return nil, err
			}
			affected, err := res.RowsAffected()
			if err != nil {
				return nil, err
			}
			if affected > 0 {
				cancelled = append(cancelled, cid)
				queue = append(queue, cid)
			}
		}
	}
	return cancelled, nil
}

// queryJobStatus returns the job's status, or "" when it does not exist (the following
// guarded UPDATE then affects no rows and reports the error).
func queryJobStatus(ctx context.Context, conn *sql.Conn, jobID int64) (Status, error) {
	var status string
	err := conn.QueryRowContext(ctx, `SELECT status FROM jobs WHERE id = ?`, jobID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return Status(status), err
}

func appendCascadeChanges(changes []statusChange, cancelledIDs []int64) []statusChange {
	for _, id := range cancelledIDs {
		changes = append(changes, statusChange{jobID: id, prev: StatusWaiting, next: StatusCancelled})
	}
	return changes
}

func classifyClaimedConflict(ctx context.Context, conn *sql.Conn, jobID int64, claimToken string) error {
	var (
		statusStr string
		claimedBy string
	)
	err := conn.QueryRowContext(ctx, `SELECT status, claimed_by FROM jobs WHERE id = ?`, jobID).Scan(&statusStr, &claimedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: job %d", ErrNotFound, jobID)
	}
	if err != nil {
		return err
	}
	if Status(statusStr) != StatusClaimed {
		return fmt.Errorf("%w: job %d is in status %s, expected claimed", ErrConflict, jobID, statusStr)
	}
	if claimedBy != claimToken {
		return fmt.Errorf("%w: job %d", ErrTokenMismatch, jobID)
	}
	return fmt.Errorf("%w: job %d could not transition", ErrConflict, jobID)
}

func classifyStatusConflict(ctx context.Context, conn *sql.Conn, jobID int64, expected string) error {
	var statusStr string
	err := conn.QueryRowContext(ctx, `SELECT status FROM jobs WHERE id = ?`, jobID).Scan(&statusStr)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: job %d", ErrNotFound, jobID)
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("%w: job %d is in status %s, expected %s", ErrConflict, jobID, statusStr, expected)
}

func formatCascadeReason(parentID int64, verb, reason string) string {
	if strings.TrimSpace(reason) == "" {
		return fmt.Sprintf("parent job %d %s", parentID, verb)
	}
	return fmt.Sprintf("parent job %d %s: %s", parentID, verb, reason)
}

func encodeAltPools(pools []string) (string, error) {
	if len(pools) == 0 {
		return "[]", nil
	}
	b, err := json.Marshal(pools)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func decodeAltPools(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" || raw == "[]" {
		return nil, nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, err
	}
	return out, nil
}

func encodeWorktreeSpec(spec *WorktreeSpec) (string, error) {
	if spec == nil {
		return "", nil
	}
	if strings.TrimSpace(spec.RepoPath) == "" && strings.TrimSpace(spec.BaseRef) == "" && strings.TrimSpace(spec.Branch) == "" {
		return "", nil
	}
	b, err := json.Marshal(spec)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func decodeWorktreeSpec(raw string) (*WorktreeSpec, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var spec WorktreeSpec
	if err := json.Unmarshal([]byte(raw), &spec); err != nil {
		return nil, err
	}
	return &spec, nil
}

func ensureWorktreeSpecColumn(ctx context.Context, db *sql.DB) error {
	return ensureJobsColumn(ctx, db, "worktree_spec", `ALTER TABLE jobs ADD COLUMN worktree_spec TEXT NOT NULL DEFAULT ''`)
}

func ensureCreatedByColumn(ctx context.Context, db *sql.DB) error {
	return ensureJobsColumn(ctx, db, "created_by", `ALTER TABLE jobs ADD COLUMN created_by TEXT NOT NULL DEFAULT ''`)
}

func ensureJobsColumn(ctx context.Context, db *sql.DB, colName, alterSQL string) error {
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(jobs)`)
	if err != nil {
		return err
	}
	defer rows.Close()

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
			return err
		}
		if name == colName {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, alterSQL)
	return err
}

// ensurePushTokenIDColumn upgrades a table created without the token binding. Its rows
// get an empty token_id, so they receive nothing until they are registered again.
func ensurePushTokenIDColumn(ctx context.Context, db *sql.DB) error {
	var found int
	err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pragma_table_info('push_subscriptions') WHERE name = 'token_id'`).Scan(&found)
	if err != nil || found > 0 {
		return err
	}
	_, err = db.ExecContext(ctx, `ALTER TABLE push_subscriptions ADD COLUMN token_id TEXT NOT NULL DEFAULT ''`)
	return err
}

func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

func parseTime(s string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, s)
}

func formatCompositeTimeIDCursor(t time.Time, id int64) string {
	return formatTime(t) + "|" + strconv.FormatInt(id, 10)
}

func parseCompositeTimeIDCursor(cursor string) (string, int64, error) {
	tsRaw, idRaw, ok := strings.Cut(cursor, "|")
	if !ok || strings.Contains(idRaw, "|") {
		return "", 0, errors.New("expected '<RFC3339Nano>|<id>'")
	}
	t, err := parseTime(strings.TrimSpace(tsRaw))
	if err != nil {
		return "", 0, fmt.Errorf("invalid timestamp: %w", err)
	}
	id, err := strconv.ParseInt(strings.TrimSpace(idRaw), 10, 64)
	if err != nil || id <= 0 {
		return "", 0, errors.New("invalid positive integer id")
	}
	return formatTime(t), id, nil
}

func formatCompositeTimeStringCursor(t time.Time, id string) string {
	return formatTime(t) + "|" + id
}

func parseCompositeTimeStringCursor(cursor string) (string, string, error) {
	tsRaw, idRaw, ok := strings.Cut(cursor, "|")
	if !ok || strings.Contains(idRaw, "|") {
		return "", "", errors.New("expected '<RFC3339Nano>|<id>'")
	}
	t, err := parseTime(strings.TrimSpace(tsRaw))
	if err != nil {
		return "", "", fmt.Errorf("invalid timestamp: %w", err)
	}
	id := strings.TrimSpace(idRaw)
	if id == "" {
		return "", "", errors.New("empty id")
	}
	return formatTime(t), id, nil
}

func randomHex(nBytes int) (string, error) {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func newUUIDv4() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

func isBusyError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "SQLITE_BUSY") ||
		strings.Contains(msg, "SQLITE_LOCKED") ||
		strings.Contains(msg, "database is locked") ||
		strings.Contains(msg, "database table is locked")
}

func isUniqueConstraintError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") || strings.Contains(msg, "SQLITE_CONSTRAINT_UNIQUE")
}

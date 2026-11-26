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

// Chain run persistence (docs/22, Issue #26).
//
// Every state change of a run happens inside one BEGIN IMMEDIATE transaction that also
// writes the step history, the job or hold of a new step and the notification row of a
// stop. Writers are serialized (F17), so a check followed by an insert inside the
// transaction cannot be interleaved with another writer.

// ErrChainManaged is returned for generic operations a chain-managed job does not allow
// (POST /jobs/{id}/requeue, docs/22 §5.3).
var ErrChainManaged = errors.New("chain-managed job; register a new chain run")

const chainSchemaSQL = `
CREATE TABLE IF NOT EXISTS chain_runs (
    id TEXT PRIMARY KEY,
    request_id TEXT NOT NULL UNIQUE,
    owner TEXT NOT NULL,
    title TEXT NOT NULL,
    scope TEXT NOT NULL DEFAULT '',
    definition_json TEXT NOT NULL,
    definition_hash TEXT NOT NULL,
    definition_version INTEGER NOT NULL,
    status TEXT NOT NULL CHECK(status IN ('running','completed','stopped','cancelled')),
    current_seq INTEGER NOT NULL,
    revision INTEGER NOT NULL,
    round INTEGER NOT NULL,
    max_rounds INTEGER NOT NULL,
    max_steps INTEGER NOT NULL,
    stop_reason TEXT NOT NULL DEFAULT '',
    stop_detail TEXT NOT NULL DEFAULT '',
    next_check_at TEXT NULL,
    check_failures INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_chain_runs_status_created ON chain_runs(status, created_at, id);

CREATE TABLE IF NOT EXISTS chain_steps (
    run_id TEXT NOT NULL REFERENCES chain_runs(id),
    seq INTEGER NOT NULL,
    step TEXT NOT NULL CHECK(step IN ('implement','review','fix','publish')),
    round INTEGER NOT NULL,
    request_id TEXT NOT NULL UNIQUE,
    job_id INTEGER NULL REFERENCES jobs(id),
    held_id TEXT NULL REFERENCES held_jobs(id),
    verdict_nonce TEXT NOT NULL DEFAULT '',
    decided_from_seq INTEGER NULL,
    decided_branch TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    PRIMARY KEY (run_id, seq)
);

CREATE TABLE IF NOT EXISTS chain_artifacts (
    job_id INTEGER PRIMARY KEY REFERENCES jobs(id),
    run_id TEXT NOT NULL,
    seq INTEGER NOT NULL,
    status TEXT NOT NULL CHECK(status IN ('captured','missing','invalid','unavailable')),
    verdict TEXT NOT NULL DEFAULT '',
    reviewed_commit TEXT NOT NULL DEFAULT '',
    raw_sha256 TEXT NOT NULL DEFAULT '',
    reason TEXT NOT NULL DEFAULT '',
    captured_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS chain_notifications (
    run_id TEXT NOT NULL,
    revision INTEGER NOT NULL,
    kind TEXT NOT NULL CHECK(kind IN ('chain_stopped','chain_stalled','chain_completed')),
    payload_json TEXT NOT NULL,
    attempts INTEGER NOT NULL DEFAULT 0,
    last_error TEXT NOT NULL DEFAULT '',
    next_attempt_at TEXT NULL,
    sent_at TEXT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY (run_id, revision, kind)
);

CREATE INDEX IF NOT EXISTS idx_chain_notifications_pending
    ON chain_notifications(sent_at, next_attempt_at)
    WHERE sent_at IS NULL;
`

// MetaKeyChainDriverLastSweep is the meta key the Chain Driver writes after every sweep.
const MetaKeyChainDriverLastSweep = "chain_driver_last_sweep_at"

// ensureChainColumns adds the chain columns to jobs, creates the chain tables and checks
// that no request_id outside a chain run uses the reserved prefix (docs/22 §4.2).
func ensureChainColumns(ctx context.Context, db *sql.DB) error {
	if err := ensureJobsColumn(ctx, db, "chain_run_id", `ALTER TABLE jobs ADD COLUMN chain_run_id TEXT NULL`); err != nil {
		return err
	}
	if err := ensureJobsColumn(ctx, db, "chain_seq", `ALTER TABLE jobs ADD COLUMN chain_seq INTEGER NULL`); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, chainSchemaSQL); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_jobs_chain_run ON jobs(chain_run_id, chain_seq)`); err != nil {
		return err
	}
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM jobs WHERE substr(request_id, 1, 6) = 'chain:' AND chain_run_id IS NULL`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("%d existing jobs use the reserved request_id prefix %q; rename them before upgrading (docs/22 §4.2)", n, ReservedRequestIDPrefix)
	}
	return nil
}

// ensureHeldChainPrefixFree is the held_jobs half of the prefix check; it runs after the
// held_jobs columns exist.
func ensureHeldChainPrefixFree(ctx context.Context, db *sql.DB) error {
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM held_jobs WHERE substr(request_id, 1, 6) = 'chain:' AND chain_run_id IS NULL`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("%d existing held jobs use the reserved request_id prefix %q; rename them before upgrading (docs/22 §4.2)", n, ReservedRequestIDPrefix)
	}
	_, err := db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_held_jobs_chain_run ON held_jobs(chain_run_id, status)`)
	return err
}

const chainRunColumns = `
	id, request_id, owner, title, scope, definition_json, definition_hash, definition_version,
	status, current_seq, revision, round, max_rounds, max_steps, stop_reason, stop_detail,
	next_check_at, check_failures, created_at, updated_at
`

const chainStepColumns = `
	run_id, seq, step, round, request_id, job_id, held_id, verdict_nonce,
	decided_from_seq, decided_branch, created_at
`

func scanChainRun(s rowScanner) (*ChainRun, error) {
	var (
		r         ChainRun
		status    string
		nextCheck sql.NullString
		createdAt string
		updatedAt string
	)
	if err := s.Scan(&r.ID, &r.RequestID, &r.Owner, &r.Title, &r.Scope, &r.DefinitionJSON, &r.DefinitionHash,
		&r.DefinitionVersion, &status, &r.CurrentSeq, &r.Revision, &r.Round, &r.MaxRounds, &r.MaxSteps,
		&r.StopReason, &r.StopDetail, &nextCheck, &r.CheckFailures, &createdAt, &updatedAt); err != nil {
		return nil, err
	}
	r.Status = ChainRunStatus(status)
	if nextCheck.Valid {
		t, err := parseTime(nextCheck.String)
		if err != nil {
			return nil, fmt.Errorf("parse next_check_at: %w", err)
		}
		r.NextCheckAt = &t
	}
	var err error
	if r.CreatedAt, err = parseTime(createdAt); err != nil {
		return nil, fmt.Errorf("parse chain created_at: %w", err)
	}
	if r.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return nil, fmt.Errorf("parse chain updated_at: %w", err)
	}
	return &r, nil
}

func scanChainStep(s rowScanner) (*ChainStep, error) {
	var (
		st        ChainStep
		jobID     sql.NullInt64
		heldID    sql.NullString
		fromSeq   sql.NullInt64
		createdAt string
	)
	if err := s.Scan(&st.RunID, &st.Seq, &st.Step, &st.Round, &st.RequestID, &jobID, &heldID,
		&st.VerdictNonce, &fromSeq, &st.DecidedBranch, &createdAt); err != nil {
		return nil, err
	}
	if jobID.Valid {
		v := jobID.Int64
		st.JobID = &v
	}
	st.HeldID = heldID.String
	if fromSeq.Valid {
		v := int(fromSeq.Int64)
		st.DecidedFromSeq = &v
	}
	t, err := parseTime(createdAt)
	if err != nil {
		return nil, fmt.Errorf("parse chain step created_at: %w", err)
	}
	st.CreatedAt = t
	return &st, nil
}

func queryChainRun(ctx context.Context, q queryRower, runID string) (*ChainRun, error) {
	r, err := scanChainRun(q.QueryRowContext(ctx, `SELECT `+chainRunColumns+` FROM chain_runs WHERE id = ?`, runID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: chain run %s", ErrNotFound, runID)
	}
	return r, err
}

func queryChainRunByRequestID(ctx context.Context, q queryRower, requestID string) (*ChainRun, error) {
	r, err := scanChainRun(q.QueryRowContext(ctx, `SELECT `+chainRunColumns+` FROM chain_runs WHERE request_id = ?`, requestID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return r, err
}

func queryChainStep(ctx context.Context, q queryRower, runID string, seq int) (*ChainStep, error) {
	st, err := scanChainStep(q.QueryRowContext(ctx, `SELECT `+chainStepColumns+` FROM chain_steps WHERE run_id = ? AND seq = ?`, runID, seq))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return st, err
}

func queryChainArtifact(ctx context.Context, q queryRower, jobID int64) (*ChainArtifact, error) {
	var (
		a          ChainArtifact
		capturedAt string
	)
	err := q.QueryRowContext(ctx, `
		SELECT job_id, run_id, seq, status, verdict, reviewed_commit, raw_sha256, reason, captured_at
		FROM chain_artifacts WHERE job_id = ?
	`, jobID).Scan(&a.JobID, &a.RunID, &a.Seq, &a.Status, &a.Verdict, &a.ReviewedCommit, &a.RawSHA256, &a.Reason, &capturedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if a.CapturedAt, err = parseTime(capturedAt); err != nil {
		return nil, fmt.Errorf("parse captured_at: %w", err)
	}
	return &a, nil
}

// loadChainSnapshot reads the run, its current step and the step's entity through q (the
// read pool outside a transaction, the write connection inside one).
func loadChainSnapshot(ctx context.Context, q queryRower, runID string) (*ChainSnapshot, error) {
	run, err := queryChainRun(ctx, q, runID)
	if err != nil {
		return nil, err
	}
	snap := &ChainSnapshot{Run: run}
	step, err := queryChainStep(ctx, q, runID, run.CurrentSeq)
	if errors.Is(err, ErrNotFound) {
		return snap, nil
	}
	if err != nil {
		return nil, err
	}
	snap.Step = step
	if step.JobID != nil {
		job, err := queryJobByID(ctx, q, *step.JobID)
		switch {
		case errors.Is(err, ErrNotFound):
			snap.JobMissing = true
		case err != nil:
			return nil, err
		default:
			snap.Job = job
			art, err := queryChainArtifact(ctx, q, job.ID)
			switch {
			case errors.Is(err, ErrNotFound):
			case err != nil:
				return nil, err
			default:
				snap.Artifact = art
			}
		}
	} else if step.HeldID != "" {
		held, err := queryHeldJobByID(ctx, q, step.HeldID)
		switch {
		case errors.Is(err, ErrNotFound):
			snap.HeldMissing = true
		case err != nil:
			return nil, err
		default:
			snap.Held = held
		}
	}
	return snap, nil
}

// LoadChainSnapshot returns the branch function's input for one run.
func (s *Store) LoadChainSnapshot(ctx context.Context, runID string) (*ChainSnapshot, error) {
	return loadChainSnapshot(ctx, s.readDB, runID)
}

// GetChainRun returns a run by ID.
func (s *Store) GetChainRun(ctx context.Context, runID string) (*ChainRun, error) {
	return queryChainRun(ctx, s.readDB, strings.TrimSpace(runID))
}

// GetChainRunByRequestID returns a run by its registration request_id.
func (s *Store) GetChainRunByRequestID(ctx context.Context, requestID string) (*ChainRun, error) {
	return queryChainRunByRequestID(ctx, s.readDB, strings.TrimSpace(requestID))
}

// GetChainStep returns one step of a run.
func (s *Store) GetChainStep(ctx context.Context, runID string, seq int) (*ChainStep, error) {
	return queryChainStep(ctx, s.readDB, runID, seq)
}

// GetChainArtifact returns the captured verdict of a review job.
func (s *Store) GetChainArtifact(ctx context.Context, jobID int64) (*ChainArtifact, error) {
	return queryChainArtifact(ctx, s.readDB, jobID)
}

// ListChainSteps returns every step of a run in seq order.
func (s *Store) ListChainSteps(ctx context.Context, runID string) ([]*ChainStep, error) {
	rows, err := s.readDB.QueryContext(ctx, `SELECT `+chainStepColumns+` FROM chain_steps WHERE run_id = ? ORDER BY seq ASC`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ChainStep
	for rows.Next() {
		st, err := scanChainStep(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// ListChainNotifications returns the notification rows of a run (oldest first).
func (s *Store) ListChainNotifications(ctx context.Context, runID string) ([]*ChainNotification, error) {
	return s.queryChainNotifications(ctx, `WHERE run_id = ? ORDER BY revision ASC, kind ASC`, runID)
}

// ListChainRunsPage lists runs newest first; the cursor is "<created_at>|<id>".
func (s *Store) ListChainRunsPage(ctx context.Context, f ListChainRunsFilter) (*ListChainRunsPage, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = DefaultListLimit
	}
	if limit > MaxListLimit {
		limit = MaxListLimit
	}
	var (
		conds []string
		args  []any
	)
	if f.Status != "" {
		if !f.Status.Valid() {
			return nil, fmt.Errorf("%w: invalid chain status %q", ErrInvalidInput, f.Status)
		}
		conds = append(conds, "status = ?")
		args = append(args, string(f.Status))
	}
	if sc := strings.TrimSpace(f.Scope); sc != "" {
		conds = append(conds, "scope = ?")
		args = append(args, sc)
	}
	if c := strings.TrimSpace(f.Cursor); c != "" {
		ts, id, err := parseCompositeTimeStringCursor(c)
		if err != nil {
			return nil, fmt.Errorf("%w: invalid cursor %q: %v", ErrInvalidInput, c, err)
		}
		conds = append(conds, "(created_at < ? OR (created_at = ? AND id < ?))")
		args = append(args, ts, ts, id)
	}
	q := `SELECT ` + chainRunColumns + ` FROM chain_runs`
	if len(conds) > 0 {
		q += ` WHERE ` + strings.Join(conds, " AND ")
	}
	q += ` ORDER BY created_at DESC, id DESC LIMIT ?`
	args = append(args, limit+1)
	rows, err := s.readDB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var runs []*ChainRun
	for rows.Next() {
		r, err := scanChainRun(rows)
		if err != nil {
			return nil, err
		}
		runs = append(runs, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	page := &ListChainRunsPage{}
	if len(runs) > limit {
		page.HasMore = true
		runs = runs[:limit]
	}
	page.Runs = runs
	if page.HasMore {
		last := runs[len(runs)-1]
		page.NextCursor = formatCompositeTimeStringCursor(last.CreatedAt, last.ID)
	}
	return page, nil
}

// ListRunningChainRunsAfter returns up to limit running runs ordered by (created_at, id)
// strictly after the given key (empty key = from the start). The sweep walks every page.
func (s *Store) ListRunningChainRunsAfter(ctx context.Context, afterCreated time.Time, afterID string, limit int) ([]*ChainRun, error) {
	if limit <= 0 || limit > MaxListLimit {
		limit = MaxListLimit
	}
	q := `SELECT ` + chainRunColumns + ` FROM chain_runs WHERE status = 'running'`
	var args []any
	if afterID != "" {
		ts := formatTime(afterCreated)
		q += ` AND (created_at > ? OR (created_at = ? AND id > ?))`
		args = append(args, ts, ts, afterID)
	}
	q += ` ORDER BY created_at ASC, id ASC LIMIT ?`
	args = append(args, limit)
	rows, err := s.readDB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ChainRun
	for rows.Next() {
		r, err := scanChainRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CountChainRunsByStatus returns the number of runs per status.
func (s *Store) CountChainRunsByStatus(ctx context.Context) (map[ChainRunStatus]int64, error) {
	rows, err := s.readDB.QueryContext(ctx, `SELECT status, COUNT(*) FROM chain_runs GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[ChainRunStatus]int64{}
	for rows.Next() {
		var (
			st string
			n  int64
		)
		if err := rows.Scan(&st, &n); err != nil {
			return nil, err
		}
		out[ChainRunStatus(st)] = n
	}
	return out, rows.Err()
}

// --- writes ---

// insertChainStepTx writes the jobs or held_jobs row of a new step plus its chain_steps
// row and gate decision. The request_id chain:<run>:<seq> is checked against both tables
// first (F12): the two UNIQUE constraints are separate, so a single INSERT would not see
// a row in the other table.
func (s *Store) insertChainStepTx(
	ctx context.Context,
	conn *sql.Conn,
	changes *[]statusChange,
	run *ChainRun,
	seq int,
	stepName string,
	round int,
	parentJobID *int64,
	fromSeq *int,
	branch string,
	w ChainStepWrite,
	nowStr string,
) (jobID int64, heldID string, err error) {
	reqID := ChainStepRequestID(run.ID, seq)
	if _, err := queryJobByRequestID(ctx, conn, reqID); err == nil {
		return 0, "", fmt.Errorf("%w: a job already uses request_id %s", ErrConflict, reqID)
	} else if !errors.Is(err, ErrNotFound) {
		return 0, "", err
	}
	if _, err := queryHeldJobByRequestID(ctx, conn, reqID); err == nil {
		return 0, "", fmt.Errorf("%w: a held job already uses request_id %s", ErrConflict, reqID)
	} else if !errors.Is(err, ErrNotFound) {
		return 0, "", err
	}

	p := w.Params
	p.RequestID = reqID
	p.ParentID = parentJobID
	p.Draft = false
	if strings.TrimSpace(p.Title) == "" {
		return 0, "", fmt.Errorf("%w: step title must not be empty", ErrInvalidInput)
	}
	effort := strings.TrimSpace(p.Effort)
	if err := ValidateEffort(p.Agent, effort); err != nil {
		return 0, "", err
	}
	p.Effort = effort

	var (
		jobArg  any
		heldArg any
	)
	if w.Hold {
		altJSON, err := encodeAltPools(w.HoldAltPools)
		if err != nil {
			return 0, "", fmt.Errorf("%w: encode alt_pools: %v", ErrInvalidInput, err)
		}
		payloadBytes, err := json.Marshal(p)
		if err != nil {
			return 0, "", fmt.Errorf("%w: encode payload: %v", ErrInvalidInput, err)
		}
		heldID, err = insertHeldTx(ctx, conn, reqID, HoldJobParams{
			Pool:            w.HoldPool,
			Reason:          w.HoldReason,
			SnapshotSummary: w.HoldSnapshotSummary,
		}, altJSON, string(payloadBytes), run.ID, seq)
		if err != nil {
			return 0, "", err
		}
		heldArg = heldID
	} else {
		altJSON, err := encodeAltPools(p.AltPools)
		if err != nil {
			return 0, "", fmt.Errorf("%w: encode alt_pools: %v", ErrInvalidInput, err)
		}
		wtJSON, err := encodeWorktreeSpec(p.WorktreeSpec)
		if err != nil {
			return 0, "", fmt.Errorf("%w: encode worktree_spec: %v", ErrInvalidInput, err)
		}
		job, err := s.insertJobTx(ctx, conn, changes, p, altJSON, wtJSON, effort, run.ID, seq)
		if err != nil {
			return 0, "", err
		}
		jobID = job.ID
		jobArg = jobID
	}

	var fromArg any
	if fromSeq != nil {
		fromArg = *fromSeq
	}
	if _, err := conn.ExecContext(ctx, `
		INSERT INTO chain_steps (run_id, seq, step, round, request_id, job_id, held_id, verdict_nonce,
			decided_from_seq, decided_branch, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, run.ID, seq, stepName, round, reqID, jobArg, heldArg, w.VerdictNonce, fromArg, branch, nowStr); err != nil {
		return 0, "", err
	}

	if w.Gate != nil {
		d := *w.Gate
		d.JobID = jobID
		d.HeldID = heldID
		d.Held = w.Hold
		if err := insertGateDecisionTx(ctx, conn, d); err != nil {
			return 0, "", err
		}
	}
	return jobID, heldID, nil
}

// insertGateDecisionTx inserts a gate_decisions row inside an open transaction.
func insertGateDecisionTx(ctx context.Context, conn *sql.Conn, d GateDecision) error {
	switch d.Decision {
	case "would_pass", "would_hold", "unknown":
	default:
		return fmt.Errorf("%w: invalid gate decision %q", ErrInvalidInput, d.Decision)
	}
	mode := strings.TrimSpace(d.Mode)
	if mode == "" {
		mode = "dry-run"
	}
	decidedAt := d.DecidedAt.UTC()
	if decidedAt.IsZero() {
		decidedAt = time.Now().UTC()
	}
	altJSON, err := encodeAltPools(d.AltPools)
	if err != nil {
		return fmt.Errorf("%w: encode alt_pools: %v", ErrInvalidInput, err)
	}
	var measured any
	if d.MeasuredAt != nil && !d.MeasuredAt.IsZero() {
		measured = formatTime(*d.MeasuredAt)
	}
	var jobArg any
	if d.JobID > 0 {
		jobArg = d.JobID
	}
	_, err = conn.ExecContext(ctx, `
		INSERT INTO gate_decisions (
			job_id, held_id, held, mode, requested_pool, alt_pools, decision,
			blocked_by, selected_pool, bypassed, reason, measured_at, decided_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, jobArg, strings.TrimSpace(d.HeldID), boolInt(d.Held), mode, strings.TrimSpace(d.RequestedPool), altJSON, d.Decision,
		strings.TrimSpace(d.BlockedBy), strings.TrimSpace(d.SelectedPool), boolInt(d.Bypassed),
		strings.TrimSpace(d.Reason), measured, formatTime(decidedAt))
	return err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// CreateChainRun registers a run and its first step (seq 1) in one transaction
// (docs/22 §4.3, last row of the counterexample table). When request_id already exists
// with the same definition_hash and owner, the existing run is returned with
// created=false; otherwise ErrConflict.
func (s *Store) CreateChainRun(ctx context.Context, p CreateChainRunParams) (*ChainRun, bool, error) {
	reqID := strings.TrimSpace(p.RequestID)
	if reqID == "" {
		return nil, false, fmt.Errorf("%w: request_id must not be empty", ErrInvalidInput)
	}
	if strings.TrimSpace(p.Title) == "" {
		return nil, false, fmt.Errorf("%w: title must not be empty", ErrInvalidInput)
	}
	if p.MaxRounds <= 0 || p.MaxSteps <= 0 {
		return nil, false, fmt.Errorf("%w: max_rounds and max_steps must be positive", ErrInvalidInput)
	}
	runID := strings.TrimSpace(p.RunID)
	if runID == "" {
		id, err := newUUIDv4()
		if err != nil {
			return nil, false, err
		}
		runID = id
	}

	var (
		out     *ChainRun
		created bool
	)
	err := s.withJobTx(ctx, func(conn *sql.Conn, changes *[]statusChange) error {
		existing, err := queryChainRunByRequestID(ctx, conn, reqID)
		if err == nil {
			if existing.DefinitionHash != p.DefinitionHash || existing.Owner != p.Owner {
				out = existing
				return fmt.Errorf("%w: chain request_id %s already registered with a different definition or owner", ErrConflict, reqID)
			}
			out = existing
			created = false
			return nil
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}

		nowStr := formatTime(time.Now().UTC())
		run := &ChainRun{ID: runID}
		if _, err := conn.ExecContext(ctx, `
			INSERT INTO chain_runs (
				id, request_id, owner, title, scope, definition_json, definition_hash, definition_version,
				status, current_seq, revision, round, max_rounds, max_steps, stop_reason, stop_detail,
				next_check_at, check_failures, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'running', 1, 1, 0, ?, ?, '', '', NULL, 0, ?, ?)
		`, runID, reqID, p.Owner, strings.TrimSpace(p.Title), strings.TrimSpace(p.Scope), p.DefinitionJSON,
			p.DefinitionHash, p.DefinitionVersion, p.MaxRounds, p.MaxSteps, nowStr, nowStr); err != nil {
			return err
		}
		if _, _, err := s.insertChainStepTx(ctx, conn, changes, run, 1, p.FirstStep, 0, nil, nil, ChainBranchStart, p.First, nowStr); err != nil {
			return err
		}
		r, err := queryChainRun(ctx, conn, runID)
		if err != nil {
			return err
		}
		out = r
		created = true
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrConflict) && out != nil {
			return out, false, err
		}
		return nil, false, err
	}
	return out, created, nil
}

// chainStopPayload is the JSON body of chain notifications.
type chainStopPayload struct {
	RunID      string `json:"run_id"`
	Title      string `json:"title"`
	Scope      string `json:"scope,omitempty"`
	Status     string `json:"status"`
	StopReason string `json:"stop_reason,omitempty"`
	StopDetail string `json:"stop_detail,omitempty"`
	Seq        int    `json:"seq"`
	Step       string `json:"step,omitempty"`
	Round      int    `json:"round"`
	JobID      int64  `json:"job_id,omitempty"`
	HeldID     string `json:"held_id,omitempty"`
	Verdict    string `json:"verdict,omitempty"`
}

func buildChainPayload(snap *ChainSnapshot, status ChainRunStatus, reason, detail string) string {
	p := chainStopPayload{
		RunID:      snap.Run.ID,
		Title:      snap.Run.Title,
		Scope:      snap.Run.Scope,
		Status:     string(status),
		StopReason: reason,
		StopDetail: detail,
		Seq:        snap.Run.CurrentSeq,
		Round:      snap.Run.Round,
	}
	if snap.Step != nil {
		p.Step = snap.Step.Step
		if snap.Step.JobID != nil {
			p.JobID = *snap.Step.JobID
		}
		p.HeldID = snap.Step.HeldID
	}
	if snap.Artifact != nil {
		p.Verdict = snap.Artifact.Verdict
	}
	b, _ := json.Marshal(p)
	return string(b)
}

func insertChainNotificationTx(ctx context.Context, conn *sql.Conn, runID string, revision int, kind, payload, nowStr string) error {
	_, err := conn.ExecContext(ctx, `
		INSERT OR IGNORE INTO chain_notifications (run_id, revision, kind, payload_json, created_at)
		VALUES (?, ?, ?, ?, ?)
	`, runID, revision, kind, payload, nowStr)
	return err
}

// stopChainRunTx moves a running run (at revision rev) to stopped and records the stop
// notification in the same transaction (docs/22 §8). It returns false when the run moved
// on (another writer won).
func stopChainRunTx(ctx context.Context, conn *sql.Conn, snap *ChainSnapshot, reason, detail, nowStr string) (bool, error) {
	run := snap.Run
	res, err := conn.ExecContext(ctx, `
		UPDATE chain_runs
		SET status = 'stopped', stop_reason = ?, stop_detail = ?, revision = revision + 1,
			next_check_at = NULL, check_failures = 0, updated_at = ?
		WHERE id = ? AND status = 'running' AND revision = ?
	`, reason, truncateBytes(detail, maxHeldErrorBytes), nowStr, run.ID, run.Revision)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil || n == 0 {
		return false, err
	}
	payload := buildChainPayload(snap, ChainRunStopped, reason, detail)
	if err := insertChainNotificationTx(ctx, conn, run.ID, run.Revision+1, ChainNotifyStopped, payload, nowStr); err != nil {
		return false, err
	}
	return true, nil
}

func truncateBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// AdvanceChain applies one transition of a run in a single BEGIN IMMEDIATE transaction
// (docs/22 §4.3). It re-reads the snapshot, requires the fingerprint read before admission
// to be unchanged (revision, status, current_seq, the step's job and its terminal status,
// the captured verdict hash), re-evaluates the branch function and requires the same
// decision. If any check fails nothing is written and Applied is false.
func (s *Store) AdvanceChain(ctx context.Context, p AdvanceChainParams) (*AdvanceChainResult, error) {
	if p.Decide == nil {
		return nil, fmt.Errorf("%w: Decide must be set", ErrInvalidInput)
	}
	out := &AdvanceChainResult{}
	err := s.withJobTx(ctx, func(conn *sql.Conn, changes *[]statusChange) error {
		*out = AdvanceChainResult{}
		snap, err := loadChainSnapshot(ctx, conn, p.RunID)
		if err != nil {
			return err
		}
		out.Run = snap.Run
		if snap.Run.Status != ChainRunRunning {
			out.Skipped = "run is " + string(snap.Run.Status)
			return nil
		}
		if snap.Fingerprint() != p.Fingerprint {
			out.Skipped = "snapshot changed"
			return nil
		}
		again := p.Decide(*snap)
		if again.Key() != p.Planned.Key() {
			out.Skipped = "decision changed"
			return nil
		}
		decision := p.Planned
		if p.StopOverride != nil {
			decision = *p.StopOverride
		}
		run := snap.Run
		nowStr := formatTime(time.Now().UTC())

		switch decision.Action {
		case ChainActionStop:
			ok, err := stopChainRunTx(ctx, conn, snap, decision.StopReason, decision.StopDetail, nowStr)
			if err != nil {
				return err
			}
			if !ok {
				out.Skipped = "run moved on"
				return nil
			}
		case ChainActionComplete:
			res, err := conn.ExecContext(ctx, `
				UPDATE chain_runs SET status = 'completed', revision = revision + 1,
					next_check_at = NULL, check_failures = 0, updated_at = ?
				WHERE id = ? AND status = 'running' AND revision = ?
			`, nowStr, run.ID, run.Revision)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n == 0 {
				out.Skipped = "run moved on"
				return nil
			}
			if err := insertChainNotificationTx(ctx, conn, run.ID, run.Revision+1, ChainNotifyCompleted,
				buildChainPayload(snap, ChainRunCompleted, "", ""), nowStr); err != nil {
				return err
			}
		case ChainActionLink:
			if snap.Step == nil || decision.LinkJobID <= 0 {
				return fmt.Errorf("%w: link without a step or job", ErrInvalidInput)
			}
			if _, err := conn.ExecContext(ctx, `
				UPDATE chain_steps SET job_id = ? WHERE run_id = ? AND seq = ? AND job_id IS NULL
			`, decision.LinkJobID, run.ID, snap.Step.Seq); err != nil {
				return err
			}
			if _, err := conn.ExecContext(ctx, `
				UPDATE jobs SET chain_run_id = ?, chain_seq = ? WHERE id = ? AND chain_run_id IS NULL
			`, run.ID, snap.Step.Seq, decision.LinkJobID); err != nil {
				return err
			}
			res, err := conn.ExecContext(ctx, `
				UPDATE chain_runs SET revision = revision + 1, next_check_at = NULL, check_failures = 0, updated_at = ?
				WHERE id = ? AND status = 'running' AND revision = ?
			`, nowStr, run.ID, run.Revision)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n == 0 {
				return fmt.Errorf("%w: run %s moved on during link", ErrConflict, run.ID)
			}
		case ChainActionAdvance:
			if p.Next == nil || snap.Step == nil || snap.Job == nil {
				return fmt.Errorf("%w: advance needs the next step and a done current job", ErrInvalidInput)
			}
			nextSeq := run.CurrentSeq + 1
			if nextSeq > run.MaxSteps {
				// Second guard (docs/22 §3.2): the fixed shape never gets here.
				ok, err := stopChainRunTx(ctx, conn, snap, ChainStopStepBudgetExceeded,
					fmt.Sprintf("seq %d would exceed max_steps %d", nextSeq, run.MaxSteps), nowStr)
				if err != nil {
					return err
				}
				if !ok {
					out.Skipped = "run moved on"
				}
				out.Applied = ok
				out.Run, _ = queryChainRun(ctx, conn, run.ID)
				return nil
			}
			parent := snap.Job.ID
			from := snap.Step.Seq
			jobID, heldID, err := s.insertChainStepTx(ctx, conn, changes, run, nextSeq, decision.NextStep,
				decision.NextRound, &parent, &from, decision.Branch, *p.Next, nowStr)
			if err != nil {
				return err
			}
			res, err := conn.ExecContext(ctx, `
				UPDATE chain_runs SET current_seq = ?, round = ?, revision = revision + 1,
					next_check_at = NULL, check_failures = 0, updated_at = ?
				WHERE id = ? AND status = 'running' AND revision = ?
			`, nextSeq, decision.NextRound, nowStr, run.ID, run.Revision)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n == 0 {
				return fmt.Errorf("%w: run %s moved on during advance", ErrConflict, run.ID)
			}
			out.JobID = jobID
			out.HeldID = heldID
		default:
			out.Skipped = "nothing to apply for action " + string(decision.Action)
			return nil
		}
		out.Applied = true
		r, err := queryChainRun(ctx, conn, run.ID)
		if err != nil {
			return err
		}
		out.Run = r
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// RecordChainStall records one chain_stalled notification per revision (docs/22 §6 R1/R8).
// Nothing is written when the run is no longer running at that revision.
func (s *Store) RecordChainStall(ctx context.Context, runID string, revision int, detail string) (bool, error) {
	var inserted bool
	err := s.withImmediateTx(ctx, func(conn *sql.Conn) error {
		inserted = false
		snap, err := loadChainSnapshot(ctx, conn, runID)
		if err != nil {
			return err
		}
		if snap.Run.Status != ChainRunRunning || snap.Run.Revision != revision {
			return nil
		}
		nowStr := formatTime(time.Now().UTC())
		res, err := conn.ExecContext(ctx, `
			INSERT OR IGNORE INTO chain_notifications (run_id, revision, kind, payload_json, created_at)
			VALUES (?, ?, ?, ?, ?)
		`, runID, revision, ChainNotifyStalled, buildChainPayload(snap, ChainRunRunning, "stalled", detail), nowStr)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		inserted = n > 0
		return err
	})
	return inserted, err
}

// RecordChainCheckError defers the next check of a run after a transient error
// (docs/22 §6: exponential next_check_at).
func (s *Store) RecordChainCheckError(ctx context.Context, runID string, base, maxDelay time.Duration, now time.Time) error {
	return s.withImmediateTx(ctx, func(conn *sql.Conn) error {
		var failures int
		err := conn.QueryRowContext(ctx, `SELECT check_failures FROM chain_runs WHERE id = ? AND status = 'running'`, runID).Scan(&failures)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		failures++
		delay := base
		for i := 1; i < failures && delay < maxDelay; i++ {
			delay *= 2
		}
		if delay > maxDelay {
			delay = maxDelay
		}
		_, err = conn.ExecContext(ctx, `
			UPDATE chain_runs SET check_failures = ?, next_check_at = ? WHERE id = ? AND status = 'running'
		`, failures, formatTime(now.Add(delay)), runID)
		return err
	})
}

// CancelChainRun cancels a run (docs/22 §5.2) in one transaction: the run becomes
// cancelled (revision+1); a held current step is cancelled; a draft/queued/waiting job is
// cancelled with the usual cascade. A claimed job is left running and reported.
func (s *Store) CancelChainRun(ctx context.Context, runID, reason string) (*CancelChainRunResult, error) {
	out := &CancelChainRunResult{}
	err := s.withJobTx(ctx, func(conn *sql.Conn, changes *[]statusChange) error {
		*out = CancelChainRunResult{}
		snap, err := loadChainSnapshot(ctx, conn, runID)
		if err != nil {
			return err
		}
		run := snap.Run
		switch run.Status {
		case ChainRunCancelled:
			out.Run = run
			out.AlreadyCancelled = true
			return nil
		case ChainRunRunning:
		default:
			out.Run = run
			return fmt.Errorf("%w: chain run %s is %s", ErrConflict, run.ID, run.Status)
		}
		nowStr := formatTime(time.Now().UTC())
		if _, err := conn.ExecContext(ctx, `
			UPDATE chain_runs SET status = 'cancelled', stop_detail = ?, revision = revision + 1,
				next_check_at = NULL, updated_at = ?
			WHERE id = ? AND status = 'running' AND revision = ?
		`, truncateBytes(strings.TrimSpace(reason), maxHeldErrorBytes), nowStr, run.ID, run.Revision); err != nil {
			return err
		}
		if snap.Held != nil && snap.Held.Status == HeldStatusHeld {
			if _, err := conn.ExecContext(ctx, `
				UPDATE held_jobs SET status = 'cancelled', cancel_reason = 'chain cancelled', cancelled_at = ?, updated_at = ?
				WHERE id = ? AND status = 'held'
			`, nowStr, nowStr, snap.Held.ID); err != nil {
				return err
			}
			out.CancelledHeldID = snap.Held.ID
		}
		if snap.Job != nil {
			switch snap.Job.Status {
			case StatusDraft, StatusQueued, StatusWaiting:
				jobReason := "chain run cancelled"
				if r := strings.TrimSpace(reason); r != "" {
					jobReason += ": " + r
				}
				ids, err := cancelJobTx(ctx, conn, changes, snap.Job.ID, snap.Job.Status, jobReason, nowStr)
				if err != nil {
					return err
				}
				out.CancelledJobIDs = ids
			case StatusClaimed:
				id := snap.Job.ID
				out.RunningJobID = &id
			}
		}
		r, err := queryChainRun(ctx, conn, run.ID)
		if err != nil {
			return err
		}
		out.Run = r
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrConflict) && out.Run != nil {
			return out, err
		}
		return nil, err
	}
	return out, nil
}

// cancelJobTx performs the same writes as Cancel for one draft/queued/waiting job and
// returns the job ID followed by the cascaded descendants.
func cancelJobTx(ctx context.Context, conn *sql.Conn, changes *[]statusChange, jobID int64, prev Status, reason, nowStr string) ([]int64, error) {
	res, err := conn.ExecContext(ctx, `
		UPDATE jobs SET status = 'cancelled', last_error = ?, updated_at = ?
		WHERE id = ? AND status IN ('draft', 'queued', 'waiting')
	`, reason, nowStr, jobID)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, classifyStatusConflict(ctx, conn, jobID, "draft|queued|waiting")
	}
	cascaded, err := cascadeCancelWaitingDescendants(ctx, conn, jobID, formatCascadeReason(jobID, "cancelled", reason), nowStr)
	if err != nil {
		return nil, err
	}
	*changes = append(*changes, statusChange{jobID: jobID, prev: prev, next: StatusCancelled})
	*changes = appendCascadeChanges(*changes, cascaded)
	return append([]int64{jobID}, cascaded...), nil
}

// CancelOrphanChainHolds cancels holds still 'held' whose run is no longer running
// (docs/22 §6 T1). It returns the IDs it cancelled.
func (s *Store) CancelOrphanChainHolds(ctx context.Context) ([]string, error) {
	var pending int
	if err := s.readDB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM held_jobs h JOIN chain_runs r ON r.id = h.chain_run_id
		WHERE h.status = 'held' AND r.status != 'running'
	`).Scan(&pending); err != nil {
		return nil, err
	}
	if pending == 0 {
		return nil, nil // no write transaction on an idle sweep
	}
	var ids []string
	err := s.withImmediateTx(ctx, func(conn *sql.Conn) error {
		ids = ids[:0]
		rows, err := conn.QueryContext(ctx, `
			SELECT h.id, r.status FROM held_jobs h JOIN chain_runs r ON r.id = h.chain_run_id
			WHERE h.status = 'held' AND r.status != 'running'
			ORDER BY h.created_at ASC, h.id ASC LIMIT ?
		`, MaxListLimit)
		if err != nil {
			return err
		}
		type item struct{ id, status string }
		var items []item
		for rows.Next() {
			var it item
			if err := rows.Scan(&it.id, &it.status); err != nil {
				rows.Close()
				return err
			}
			items = append(items, it)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		nowStr := formatTime(time.Now().UTC())
		for _, it := range items {
			if _, err := conn.ExecContext(ctx, `
				UPDATE held_jobs SET status = 'cancelled', cancel_reason = ?, cancelled_at = ?, updated_at = ?
				WHERE id = ? AND status = 'held'
			`, "chain "+it.status, nowStr, nowStr, it.id); err != nil {
				return err
			}
			ids = append(ids, it.id)
		}
		return nil
	})
	return ids, err
}

// CompleteWithArtifact is Complete plus the review verdict captured by Shepherd, written in
// the same transaction (docs/22 §7.1 step 4). artifact may be nil (non-review jobs).
func (s *Store) CompleteWithArtifact(ctx context.Context, jobID int64, claimToken, resultText string, artifact *ChainArtifact) (*CompleteResult, error) {
	if artifact == nil {
		return s.Complete(ctx, jobID, claimToken, resultText)
	}
	return s.complete(ctx, jobID, claimToken, resultText, func(conn *sql.Conn) error {
		capturedAt := artifact.CapturedAt
		if capturedAt.IsZero() {
			capturedAt = time.Now().UTC()
		}
		_, err := conn.ExecContext(ctx, `
			INSERT INTO chain_artifacts (job_id, run_id, seq, status, verdict, reviewed_commit, raw_sha256, reason, captured_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, jobID, artifact.RunID, artifact.Seq, artifact.Status, artifact.Verdict, artifact.ReviewedCommit,
			artifact.RawSHA256, truncateBytes(artifact.Reason, maxHeldErrorBytes), formatTime(capturedAt))
		return err
	})
}

// --- notifications (docs/22 §8) ---

func (s *Store) queryChainNotifications(ctx context.Context, where string, args ...any) ([]*ChainNotification, error) {
	rows, err := s.readDB.QueryContext(ctx, `
		SELECT run_id, revision, kind, payload_json, attempts, last_error, next_attempt_at, sent_at, created_at
		FROM chain_notifications `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ChainNotification
	for rows.Next() {
		var (
			n         ChainNotification
			next      sql.NullString
			sent      sql.NullString
			createdAt string
		)
		if err := rows.Scan(&n.RunID, &n.Revision, &n.Kind, &n.PayloadJSON, &n.Attempts, &n.LastError, &next, &sent, &createdAt); err != nil {
			return nil, err
		}
		if next.Valid {
			t, err := parseTime(next.String)
			if err != nil {
				return nil, err
			}
			n.NextAttemptAt = &t
		}
		if sent.Valid {
			t, err := parseTime(sent.String)
			if err != nil {
				return nil, err
			}
			n.SentAt = &t
		}
		if n.CreatedAt, err = parseTime(createdAt); err != nil {
			return nil, err
		}
		out = append(out, &n)
	}
	return out, rows.Err()
}

// ListDueChainNotifications returns unsent notifications whose next attempt is due.
func (s *Store) ListDueChainNotifications(ctx context.Context, now time.Time, limit int) ([]*ChainNotification, error) {
	if limit <= 0 || limit > MaxListLimit {
		limit = MaxListLimit
	}
	return s.queryChainNotifications(ctx,
		`WHERE sent_at IS NULL AND (next_attempt_at IS NULL OR next_attempt_at <= ?) ORDER BY created_at ASC, run_id ASC, revision ASC LIMIT ?`,
		formatTime(now), limit)
}

// MarkChainNotificationSent commits sent_at after a successful send.
func (s *Store) MarkChainNotificationSent(ctx context.Context, runID string, revision int, kind string, now time.Time) error {
	return s.withImmediateTx(ctx, func(conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, `
			UPDATE chain_notifications SET sent_at = ?, attempts = attempts + 1, last_error = ''
			WHERE run_id = ? AND revision = ? AND kind = ? AND sent_at IS NULL
		`, formatTime(now), runID, revision, kind)
		return err
	})
}

// RecordChainNotificationFailure records a failed send and schedules the next attempt
// (exponential, capped by maxDelay).
func (s *Store) RecordChainNotificationFailure(ctx context.Context, runID string, revision int, kind, errMsg string, base, maxDelay time.Duration, now time.Time) error {
	return s.withImmediateTx(ctx, func(conn *sql.Conn) error {
		var attempts int
		err := conn.QueryRowContext(ctx, `
			SELECT attempts FROM chain_notifications WHERE run_id = ? AND revision = ? AND kind = ? AND sent_at IS NULL
		`, runID, revision, kind).Scan(&attempts)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		attempts++
		delay := base
		for i := 1; i < attempts && delay < maxDelay; i++ {
			delay *= 2
		}
		if delay > maxDelay {
			delay = maxDelay
		}
		_, err = conn.ExecContext(ctx, `
			UPDATE chain_notifications SET attempts = ?, last_error = ?, next_attempt_at = ?
			WHERE run_id = ? AND revision = ? AND kind = ? AND sent_at IS NULL
		`, attempts, truncateBytes(errMsg, maxHeldErrorBytes), formatTime(now.Add(delay)), runID, revision, kind)
		return err
	})
}

// ChainNotificationPendingStats returns the number of unsent notifications and the oldest.
func (s *Store) ChainNotificationPendingStats(ctx context.Context) (*ChainNotificationStats, error) {
	var (
		n      int64
		oldest sql.NullString
	)
	if err := s.readDB.QueryRowContext(ctx,
		`SELECT COUNT(*), MIN(created_at) FROM chain_notifications WHERE sent_at IS NULL`).Scan(&n, &oldest); err != nil {
		return nil, err
	}
	out := &ChainNotificationStats{Pending: n}
	if oldest.Valid {
		t, err := parseTime(oldest.String)
		if err != nil {
			return nil, err
		}
		out.OldestPending = &t
	}
	return out, nil
}

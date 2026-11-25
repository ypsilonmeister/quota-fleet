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

// Supersede (docs/20): every job registration that carries a supersede_key gets one row in
// supersede_registrations. A held entry and the job it is later released into share that
// row (same request_id), so seq is the order in which work was registered, not the order
// in which it was released.

// SupersedeRef identifies one registration: a job, or a held entry that has no job.
type SupersedeRef struct {
	JobID  int64
	HeldID string
}

// SupersedeInfo describes how one registration relates to others with the same key.
type SupersedeInfo struct {
	// Superseded lists the registrations this one cancelled.
	Superseded []SupersedeRef
	// CascadeCancelledJobIDs lists waiting descendants of those jobs, cancelled with them.
	CascadeCancelledJobIDs []int64
	// SupersededBy is set when a newer registration cancelled this one.
	SupersededBy *SupersedeRef
}

// KeyedReleaseResult is the outcome of ReleaseKeyedHeld.
type KeyedReleaseResult struct {
	Held    *HeldJob
	Job     *Job
	Created bool
	// SupersededReason is non-empty when the entry was cancelled instead of released.
	SupersededReason string
}

type supersedeRegistration struct {
	seq       int64
	key       string
	requestID string
}

func supersedeReason(key, newerRequestID string) string {
	return fmt.Sprintf("superseded: newer registration request_id=%s has supersede_key=%s", newerRequestID, key)
}

// registerSupersedeTx records the registration of requestID under key. A new registration
// cancels every older one that has not started: held entries, and queued or waiting jobs
// (with their waiting descendants). Claimed jobs are never touched.
//
// A request_id that is already registered (a held entry becoming a job) keeps its place in
// the order and cancels nothing; it is rejected when a newer registration exists.
//
// The returned set holds the IDs of the jobs cancelled by this call.
func registerSupersedeTx(
	ctx context.Context, conn *sql.Conn, changes *[]statusChange, key, requestID, nowStr string,
) (map[int64]bool, error) {
	existing, err := querySupersedeRegistration(ctx, conn, requestID)
	if err == nil {
		if existing.key != key {
			return nil, fmt.Errorf("%w: request_id=%s is registered with supersede_key=%s", ErrConflict, requestID, existing.key)
		}
		newer, err := queryNewerRegistration(ctx, conn, existing)
		if err == nil {
			return nil, fmt.Errorf("%w: request_id=%s is %s", ErrConflict, requestID, supersedeReason(key, newer.requestID))
		}
		if !errors.Is(err, ErrNotFound) {
			return nil, err
		}
		return nil, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, err
	}

	res, err := conn.ExecContext(ctx, `
		INSERT INTO supersede_registrations (supersede_key, request_id, created_at)
		VALUES (?, ?, ?)
	`, key, requestID, nowStr)
	if err != nil {
		return nil, err
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return supersedeOlderTx(ctx, conn, changes, supersedeRegistration{seq: seq, key: key, requestID: requestID}, nowStr)
}

func supersedeOlderTx(
	ctx context.Context, conn *sql.Conn, changes *[]statusChange, newer supersedeRegistration, nowStr string,
) (map[int64]bool, error) {
	type target struct {
		seq    int64
		heldID string
		jobID  sql.NullInt64
		status string // job status when jobID is set, held status otherwise
	}
	rows, err := conn.QueryContext(ctx, `
		SELECT r.seq, r.held_id, r.job_id, COALESCE(j.status, h.status, '')
		FROM supersede_registrations r
		LEFT JOIN jobs j ON j.id = r.job_id
		LEFT JOIN held_jobs h ON r.job_id IS NULL AND h.id = r.held_id
		WHERE r.supersede_key = ? AND r.seq < ?
			AND (j.status IN ('queued', 'waiting') OR h.status = 'held')
		ORDER BY r.seq ASC
	`, newer.key, newer.seq)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// Read everything first: the transaction has a single connection.
	var targets []target
	for rows.Next() {
		var tg target
		if err := rows.Scan(&tg.seq, &tg.heldID, &tg.jobID, &tg.status); err != nil {
			return nil, err
		}
		targets = append(targets, tg)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	reason := supersedeReason(newer.key, newer.requestID)
	cancelled := map[int64]bool{}
	var directIDs []int64

	// Cancel every same-key target before cascading, so that a same-key descendant is
	// recorded as superseded rather than as a child of a cancelled parent.
	for _, tg := range targets {
		if tg.jobID.Valid {
			if _, err := conn.ExecContext(ctx, `
				UPDATE jobs
				SET status = 'cancelled', last_error = ?, updated_at = ?
				WHERE id = ? AND status IN ('queued', 'waiting')
			`, reason, nowStr, tg.jobID.Int64); err != nil {
				return nil, err
			}
			cancelled[tg.jobID.Int64] = true
			directIDs = append(directIDs, tg.jobID.Int64)
			*changes = append(*changes, statusChange{jobID: tg.jobID.Int64, prev: Status(tg.status), next: StatusCancelled})
		} else if err := cancelHeldSupersededTx(ctx, conn, tg.heldID, reason, nowStr); err != nil {
			return nil, err
		}
		if _, err := conn.ExecContext(ctx,
			`UPDATE supersede_registrations SET superseded_by_seq = ? WHERE seq = ?`, newer.seq, tg.seq); err != nil {
			return nil, err
		}
	}

	var cascaded []int64
	for _, id := range directIDs {
		ids, err := cascadeCancelWaitingDescendants(ctx, conn, id, formatCascadeReason(id, "cancelled", reason), nowStr)
		if err != nil {
			return nil, err
		}
		cascaded = append(cascaded, ids...)
	}
	if len(cascaded) > 0 {
		for _, id := range cascaded {
			cancelled[id] = true
		}
		*changes = appendCascadeChanges(*changes, cascaded)
		encoded, err := json.Marshal(cascaded)
		if err != nil {
			return nil, err
		}
		if _, err := conn.ExecContext(ctx,
			`UPDATE supersede_registrations SET cascaded_job_ids = ? WHERE seq = ?`, string(encoded), newer.seq); err != nil {
			return nil, err
		}
	}
	return cancelled, nil
}

func cancelHeldSupersededTx(ctx context.Context, conn *sql.Conn, heldID, reason, nowStr string) error {
	_, err := conn.ExecContext(ctx, `
		UPDATE held_jobs
		SET status = 'cancelled', cancel_reason = ?, cancelled_at = ?, updated_at = ?
		WHERE id = ? AND status = 'held'
	`, reason, nowStr, nowStr, heldID)
	return err
}

// checkHoldParentTx rejects a keyed hold whose parent can never complete: accepting it
// would cancel older work in favor of an entry that can never be released.
func checkHoldParentTx(ctx context.Context, conn *sql.Conn, parentID int64, key string, cancelled map[int64]bool) error {
	status, err := queryJobStatus(ctx, conn, parentID)
	if err != nil {
		return err
	}
	switch status {
	case "":
		return fmt.Errorf("%w: parent_id=%d", ErrParentNotFound, parentID)
	case StatusFailed, StatusCancelled:
		return parentTerminalError(parentID, status, key, cancelled)
	}
	return nil
}

func parentTerminalError(parentID int64, status Status, key string, cancelled map[int64]bool) error {
	if cancelled[parentID] {
		return fmt.Errorf("%w: parent_id=%d would be cancelled by this registration's supersede_key=%s",
			ErrParentTerminal, parentID, key)
	}
	return fmt.Errorf("%w: parent_id=%d has status %s", ErrParentTerminal, parentID, status)
}

// ReleaseKeyedHeld releases a held entry that has a supersede_key. In one transaction it
// either cancels the entry as superseded, when a newer registration with the same key
// exists, or creates its job in selectedPool and marks it released.
func (s *Store) ReleaseKeyedHeld(ctx context.Context, heldID, selectedPool string) (*KeyedReleaseResult, error) {
	var out *KeyedReleaseResult

	err := s.withJobTx(ctx, func(conn *sql.Conn, changes *[]statusChange) error {
		out = nil
		held, err := queryHeldJobByID(ctx, conn, heldID)
		if err != nil {
			return err
		}
		if held.Status != HeldStatusHeld {
			return classifyHeldConflict(ctx, conn, heldID, "held")
		}
		reg, err := querySupersedeRegistration(ctx, conn, held.RequestID)
		if err != nil {
			return fmt.Errorf("supersede registration of held %s: %w", heldID, err)
		}

		newer, err := queryNewerRegistration(ctx, conn, reg)
		switch {
		case err == nil:
			reason := supersedeReason(reg.key, newer.requestID)
			nowStr := formatTime(time.Now().UTC())
			if err := cancelHeldSupersededTx(ctx, conn, heldID, reason, nowStr); err != nil {
				return err
			}
			if _, err := conn.ExecContext(ctx,
				`UPDATE supersede_registrations SET superseded_by_seq = ? WHERE seq = ?`, newer.seq, reg.seq); err != nil {
				return err
			}
			held, err = queryHeldJobByID(ctx, conn, heldID)
			if err != nil {
				return err
			}
			out = &KeyedReleaseResult{Held: held, SupersededReason: reason}
			return nil
		case !errors.Is(err, ErrNotFound):
			return err
		}

		params := held.Payload
		params.RequestID = held.RequestID
		params.Pool = selectedPool
		job, created, err := s.createJobTx(ctx, conn, changes, params)
		if err != nil {
			return err
		}
		released, err := markHeldReleasedTx(ctx, conn, heldID, job.ID, selectedPool)
		if err != nil {
			return err
		}
		out = &KeyedReleaseResult{Held: released, Job: job, Created: created}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// SupersedeInfoForJob returns the supersede relations of a job. It is empty for a job
// registered without a supersede_key.
func (s *Store) SupersedeInfoForJob(ctx context.Context, jobID int64) (*SupersedeInfo, error) {
	return s.supersedeInfo(ctx, `job_id = ?`, jobID)
}

// SupersedeInfoForHeld returns the supersede relations of a held entry.
func (s *Store) SupersedeInfoForHeld(ctx context.Context, heldID string) (*SupersedeInfo, error) {
	return s.supersedeInfo(ctx, `held_id = ?`, strings.TrimSpace(heldID))
}

func (s *Store) supersedeInfo(ctx context.Context, where string, arg any) (*SupersedeInfo, error) {
	var (
		seq      int64
		bySeq    sql.NullInt64
		cascaded string
	)
	err := s.readDB.QueryRowContext(ctx,
		`SELECT seq, superseded_by_seq, cascaded_job_ids FROM supersede_registrations WHERE `+where, arg,
	).Scan(&seq, &bySeq, &cascaded)
	if errors.Is(err, sql.ErrNoRows) {
		return &SupersedeInfo{}, nil
	}
	if err != nil {
		return nil, err
	}

	info := &SupersedeInfo{}
	if err := json.Unmarshal([]byte(cascaded), &info.CascadeCancelledJobIDs); err != nil {
		return nil, fmt.Errorf("decode cascaded_job_ids: %w", err)
	}
	if bySeq.Valid {
		var ref SupersedeRef
		var jobID sql.NullInt64
		if err := s.readDB.QueryRowContext(ctx,
			`SELECT job_id, held_id FROM supersede_registrations WHERE seq = ?`, bySeq.Int64,
		).Scan(&jobID, &ref.HeldID); err != nil {
			return nil, err
		}
		ref.JobID = jobID.Int64
		info.SupersededBy = &ref
	}

	rows, err := s.readDB.QueryContext(ctx,
		`SELECT job_id, held_id FROM supersede_registrations WHERE superseded_by_seq = ? ORDER BY seq ASC`, seq)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var ref SupersedeRef
		var jobID sql.NullInt64
		if err := rows.Scan(&jobID, &ref.HeldID); err != nil {
			return nil, err
		}
		ref.JobID = jobID.Int64
		info.Superseded = append(info.Superseded, ref)
	}
	return info, rows.Err()
}

func querySupersedeRegistration(ctx context.Context, q queryRower, requestID string) (supersedeRegistration, error) {
	return scanSupersedeRegistration(q.QueryRowContext(ctx, `
		SELECT seq, supersede_key, request_id FROM supersede_registrations WHERE request_id = ?
	`, requestID))
}

// queryNewerRegistration returns the latest registration with reg's key that came after
// reg, whatever state it is in now, or ErrNotFound.
func queryNewerRegistration(ctx context.Context, q queryRower, reg supersedeRegistration) (supersedeRegistration, error) {
	return scanSupersedeRegistration(q.QueryRowContext(ctx, `
		SELECT seq, supersede_key, request_id FROM supersede_registrations
		WHERE supersede_key = ? AND seq > ?
		ORDER BY seq DESC
		LIMIT 1
	`, reg.key, reg.seq))
}

func scanSupersedeRegistration(row rowScanner) (supersedeRegistration, error) {
	var reg supersedeRegistration
	err := row.Scan(&reg.seq, &reg.key, &reg.requestID)
	if errors.Is(err, sql.ErrNoRows) {
		return reg, ErrNotFound
	}
	return reg, err
}

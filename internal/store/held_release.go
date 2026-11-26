package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// CancelHeldJob transitions a held entry from status='held' to status='cancelled' without
// deleting the row so audit history and request_id uniqueness remain intact. The reason
// recorded in cancel_reason is "cancelled via gate API". When the hold is the current step
// of a running chain run, the run is stopped with step_cancelled (and its notification
// row written) in the same transaction (docs/22 §5.2).
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
				cancel_reason = 'cancelled via gate API',
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
		if h.ChainRunID != "" {
			snap, err := loadChainSnapshot(ctx, conn, h.ChainRunID)
			if err != nil && !errors.Is(err, ErrNotFound) {
				return err
			}
			if snap != nil && snap.Run.Status == ChainRunRunning && snap.Step != nil &&
				snap.Step.Seq == h.ChainSeq && snap.Step.HeldID == h.ID {
				if _, err := stopChainRunTx(ctx, conn, snap, ChainStopStepCancelled,
					fmt.Sprintf("held %s cancelled via gate API", h.ID), nowStr); err != nil {
					return err
				}
			}
		}
		out = h
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// isPermanentReleaseError reports whether a job insert failed for a reason a retry will
// not fix (docs/22 §5.1 step 5).
func isPermanentReleaseError(err error) bool {
	return errors.Is(err, ErrParentNotFound) || errors.Is(err, ErrParentTerminal) || errors.Is(err, ErrInvalidInput)
}

// ReleaseHeld releases a hold in one transaction (docs/22 §5.1): it re-checks that the
// hold is still 'held'; for a chain hold it checks that the run is running and still points
// at this hold (otherwise, if the run is terminal, the hold is cancelled with reason
// "chain <status>"); it creates the job (or reuses one already created under the same
// request_id), marks the hold released, links the chain step, bumps the run revision and
// records the gate decision. A permanent insert error is recorded on the hold and, for a
// chain hold, stops the run with release_failed in the same transaction. Any other error
// rolls everything back.
//
// When the hold is not 'held' (cancel won the race) nothing is written and ErrConflict is
// returned together with the current row.
func (s *Store) ReleaseHeld(ctx context.Context, p ReleaseHeldParams) (*ReleaseHeldOutcome, error) {
	heldID := strings.TrimSpace(p.HeldID)
	if heldID == "" {
		return nil, fmt.Errorf("%w: held_id must not be empty", ErrInvalidInput)
	}
	out := &ReleaseHeldOutcome{}
	err := s.withJobTx(ctx, func(conn *sql.Conn, changes *[]statusChange) error {
		*out = ReleaseHeldOutcome{}
		held, err := queryHeldJobByID(ctx, conn, heldID)
		if err != nil {
			return err
		}
		out.Held = held
		if held.Status != HeldStatusHeld {
			return fmt.Errorf("%w: held job %s is %s (expected held)", ErrConflict, held.ID, held.Status)
		}
		nowStr := formatTime(time.Now().UTC())

		var snap *ChainSnapshot
		if held.ChainRunID != "" {
			snap, err = loadChainSnapshot(ctx, conn, held.ChainRunID)
			if err != nil {
				return err
			}
			if snap.Run.Status != ChainRunRunning {
				if _, err := conn.ExecContext(ctx, `
					UPDATE held_jobs SET status = 'cancelled', cancel_reason = ?, cancelled_at = ?, updated_at = ?
					WHERE id = ? AND status = 'held'
				`, "chain "+string(snap.Run.Status), nowStr, nowStr, held.ID); err != nil {
					return err
				}
				out.CancelledForChain = true
				out.Held, err = queryHeldJobByID(ctx, conn, held.ID)
				return err
			}
			if snap.Run.CurrentSeq != held.ChainSeq || snap.Step == nil || snap.Step.HeldID != held.ID {
				return fmt.Errorf("%w: held job %s is not the current step of chain run %s", ErrConflict, held.ID, held.ChainRunID)
			}
		}

		params := held.Payload
		params.RequestID = held.RequestID
		if sel := strings.TrimSpace(p.SelectedPool); sel != "" {
			params.Pool = sel
		}

		job, err := queryJobByRequestID(ctx, conn, held.RequestID)
		createdJob := false
		switch {
		case err == nil:
			// A job from an interrupted pre-§5.1 release already exists: adopt it.
		case errors.Is(err, ErrNotFound):
			altJSON, aerr := encodeAltPools(params.AltPools)
			if aerr != nil {
				err = fmt.Errorf("%w: encode alt_pools: %v", ErrInvalidInput, aerr)
				break
			}
			wtJSON, werr := encodeWorktreeSpec(params.WorktreeSpec)
			if werr != nil {
				err = fmt.Errorf("%w: encode worktree_spec: %v", ErrInvalidInput, werr)
				break
			}
			effort := strings.TrimSpace(params.Effort)
			if verr := ValidateEffort(params.Agent, effort); verr != nil {
				err = verr
				break
			}
			job, err = s.insertJobTx(ctx, conn, changes, params, altJSON, wtJSON, effort, held.ChainRunID, held.ChainSeq)
			createdJob = err == nil
		}
		if err != nil {
			if !isPermanentReleaseError(err) {
				return err
			}
			// Permanent: record on the hold (and stop the run) and commit.
			msg := truncateBytes(strings.TrimSpace(err.Error()), maxHeldErrorBytes)
			// Same dedupe as RecordHeldReleaseError: an identical error only refreshes updated_at.
			if _, uerr := conn.ExecContext(ctx, `
				UPDATE held_jobs
				SET error_count = CASE WHEN last_error = ? THEN error_count ELSE error_count + 1 END,
					last_error = ?, updated_at = ?
				WHERE id = ? AND status = 'held'
			`, msg, msg, nowStr, held.ID); uerr != nil {
				return uerr
			}
			if snap != nil {
				if _, serr := stopChainRunTx(ctx, conn, snap, ChainStopReleaseFailed, msg, nowStr); serr != nil {
					return serr
				}
			}
			out.PermanentErr = err
			out.Held, err = queryHeldJobByID(ctx, conn, held.ID)
			return err
		}

		if _, err := conn.ExecContext(ctx, `
			UPDATE held_jobs SET status = 'released', released_job_id = ?, released_pool = ?, released_at = ?,
				last_error = '', updated_at = ?
			WHERE id = ? AND status = 'held'
		`, job.ID, strings.TrimSpace(p.SelectedPool), nowStr, nowStr, held.ID); err != nil {
			return err
		}
		if snap != nil {
			if _, err := conn.ExecContext(ctx, `
				UPDATE chain_steps SET job_id = ? WHERE run_id = ? AND seq = ?
			`, job.ID, snap.Run.ID, held.ChainSeq); err != nil {
				return err
			}
			res, err := conn.ExecContext(ctx, `
				UPDATE chain_runs SET revision = revision + 1, updated_at = ?
				WHERE id = ? AND status = 'running' AND revision = ?
			`, nowStr, snap.Run.ID, snap.Run.Revision)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n == 0 {
				return fmt.Errorf("%w: chain run %s moved on during release", ErrConflict, snap.Run.ID)
			}
		}
		if p.Gate != nil {
			d := *p.Gate
			d.JobID = job.ID
			d.HeldID = held.ID
			d.Held = false
			if err := insertGateDecisionTx(ctx, conn, d); err != nil {
				return err
			}
		}
		out.Job = job
		out.Created = createdJob
		out.Released = true
		out.Held, err = queryHeldJobByID(ctx, conn, held.ID)
		return err
	})
	if err != nil {
		if errors.Is(err, ErrConflict) && out.Held != nil {
			return out, err
		}
		return nil, err
	}
	return out, nil
}

package runtime

import (
	"strings"
	"testing"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/adapter"
	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

// TestChainResetAfterCancelDoesNotRequeue: a step job that was claimed when its run was
// cancelled (or stopped/completed) is never made claimable again by the Reconciler's
// automatic reset (docs/22 §5.2). The run-state check is in Reset's transaction.
func TestChainResetAfterCancelDoesNotRequeue(t *testing.T) {
	for _, terminal := range []string{"cancelled", "stopped", "completed"} {
		t.Run("reconciler/"+terminal, func(t *testing.T) {
			e := newChainEnv(t, config.GateModeOff)
			run := e.createRun("reset-"+terminal, 0, "")
			job := e.claimCurrent(run.ID)
			if terminal == "cancelled" {
				res, err := e.st.CancelChainRun(e.ctx, run.ID, "")
				if err != nil || res.RunningJobID == nil {
					t.Fatalf("cancel: %+v %v", res, err)
				}
			} else {
				e.exec(`UPDATE chain_runs SET status = ? WHERE id = ?`, terminal, run.ID)
			}
			notif := &recordingNotifier{}
			rec := NewReconciler(ReconcilerOptions{
				Store:             e.st,
				Adapter:           adapter.NewMock(adapter.MockOptions{Default: adapter.MockBehavior{ForceMissing: true}}),
				Notifier:          notif,
				StaleClaimedAfter: time.Minute,
				AttemptsCap:       3,
				Now:               func() time.Time { return time.Now().Add(time.Hour) },
				Logger:            discardLogger(),
			})
			if err := rec.ReconcileOnce(e.ctx); err != nil {
				t.Fatal(err)
			}
			j, err := e.st.GetJob(e.ctx, job.ID)
			if err != nil {
				t.Fatal(err)
			}
			if j.Status != store.StatusCancelled || !strings.Contains(j.LastError, terminal) {
				t.Fatalf("job after reset = %s %q, want cancelled", j.Status, j.LastError)
			}
			if c, ok, err := e.st.Claim(e.ctx, 10); err != nil || ok {
				t.Fatalf("job claimable again: %+v %v", c, err)
			}
			e.sweep()
			if n := len(e.steps(run.ID)); n != 1 {
				t.Fatalf("steps = %d", n)
			}
		})
	}

	t.Run("running run keeps the §5.3 reset", func(t *testing.T) {
		e := newChainEnv(t, config.GateModeOff)
		run := e.createRun("reset-running", 0, "")
		job := e.claimCurrent(run.ID)
		j, err := e.st.Reset(e.ctx, job.ID)
		if err != nil || j.Status != store.StatusQueued {
			t.Fatalf("reset: %+v %v", j, err)
		}
	})

	t.Run("publish step of a cancelled run", func(t *testing.T) {
		e := newChainEnv(t, config.GateModeOff)
		run := e.createRun("reset-publish", 0, "")
		e.finishCurrent(run.ID, "")
		e.sweep()
		e.finishCurrent(run.ID, "ok_to_merge")
		e.sweep()
		if s := e.snapshot(run.ID); s.Step.Step != store.ChainStepPublish {
			t.Fatalf("step = %s", s.Step.Step)
		}
		job := e.claimCurrent(run.ID)
		if _, err := e.st.CancelChainRun(e.ctx, run.ID, ""); err != nil {
			t.Fatal(err)
		}
		j, err := e.st.Reset(e.ctx, job.ID)
		if err != nil || j.Status != store.StatusCancelled {
			t.Fatalf("reset: %+v %v", j, err)
		}
	})
}

// TestChainOwnershipOfReleasedJobs: the R10 repair, the normal released state and
// CancelChainRun all use the same ownership check (ChainSnapshot.OwnershipProblem).
func TestChainOwnershipOfReleasedJobs(t *testing.T) {
	// r10 returns a run whose seq 2 hold was released but chain_steps.job_id is NULL.
	r10 := func(t *testing.T, e *chainEnv, reqID string) (*store.ChainRun, *store.HeldJob, int64) {
		t.Helper()
		run, held := heldRun(t, e, reqID)
		out, err := e.st.ReleaseHeld(e.ctx, store.ReleaseHeldParams{HeldID: held.ID, SelectedPool: "codex"})
		if err != nil || !out.Released {
			t.Fatalf("release: %+v %v", out, err)
		}
		e.exec(`UPDATE chain_steps SET job_id = NULL WHERE run_id = ? AND seq = 2`, run.ID)
		e.exec(`UPDATE jobs SET chain_run_id = NULL, chain_seq = NULL WHERE id = ?`, out.Job.ID) // pre-§5.1 state
		return e.run(run.ID), held, out.Job.ID
	}
	// otherDoneJob returns the done implement job of another run.
	otherDoneJob := func(e *chainEnv) *store.Job {
		e.snap.set(false)
		other := e.createRun("other", 0, "codex")
		return e.finishCurrent(other.ID, "")
	}
	stepJobID := func(e *chainEnv, runID string) *int64 {
		s := e.snapshot(runID)
		return s.Step.JobID
	}

	t.Run("R10 unowned job of the hold is linked", func(t *testing.T) {
		e := newChainEnv(t, config.GateModeEnforce)
		run, _, jobID := r10(t, e, "own-ok")
		e.sweep()
		mustStatus(t, e.run(run.ID), store.ChainRunRunning, "")
		if id := stepJobID(e, run.ID); id == nil || *id != jobID {
			t.Fatalf("not linked: %v", id)
		}
		if j, _ := e.st.GetJob(e.ctx, jobID); j.ChainRunID != run.ID || j.ChainSeq != 2 {
			t.Fatalf("job ownership: %q %d", j.ChainRunID, j.ChainSeq)
		}
	})

	t.Run("R10 pointing at another run's done job stops", func(t *testing.T) {
		e := newChainEnv(t, config.GateModeEnforce)
		run, held, _ := r10(t, e, "own-foreign")
		foreign := otherDoneJob(e)
		e.exec(`UPDATE held_jobs SET released_job_id = ? WHERE id = ?`, foreign.ID, held.ID)
		e.sweep()
		e.sweep()
		mustStatus(t, e.run(run.ID), store.ChainRunStopped, store.ChainStopInternalInconsistency)
		if id := stepJobID(e, run.ID); id != nil {
			t.Fatalf("foreign job linked: %d", *id)
		}
		if j, _ := e.st.GetJob(e.ctx, foreign.ID); j.ChainRunID == run.ID {
			t.Fatal("foreign job re-owned")
		}
		if n := len(e.steps(run.ID)); n != 2 {
			t.Fatalf("steps = %d (foreign result adopted)", n)
		}
	})

	t.Run("R10 pointing at a missing job stops instead of retrying", func(t *testing.T) {
		e := newChainEnv(t, config.GateModeEnforce)
		run, held, _ := r10(t, e, "own-missing")
		conn, err := e.raw().Conn(e.ctx)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = conn.ExecContext(e.ctx, `PRAGMA foreign_keys = OFF`)
		if _, err := conn.ExecContext(e.ctx, `UPDATE held_jobs SET released_job_id = 999999 WHERE id = ?`, held.ID); err != nil {
			t.Fatal(err)
		}
		conn.Close()
		e.sweep()
		got := e.run(run.ID)
		mustStatus(t, got, store.ChainRunStopped, store.ChainStopInternalInconsistency)
		if got.CheckFailures != 0 {
			t.Fatalf("treated as a retryable error: failures=%d", got.CheckFailures)
		}
	})

	t.Run("linked step whose hold points at another job stops", func(t *testing.T) {
		e := newChainEnv(t, config.GateModeEnforce)
		run, held := heldRun(t, e, "own-mismatch")
		if _, err := e.st.ReleaseHeld(e.ctx, store.ReleaseHeldParams{HeldID: held.ID, SelectedPool: "codex"}); err != nil {
			t.Fatal(err)
		}
		foreign := otherDoneJob(e)
		e.exec(`UPDATE held_jobs SET released_job_id = ? WHERE id = ?`, foreign.ID, held.ID)
		e.sweep()
		mustStatus(t, e.run(run.ID), store.ChainRunStopped, store.ChainStopInternalInconsistency)
	})

	t.Run("cancel before the R10 repair cancels the released queued job", func(t *testing.T) {
		e := newChainEnv(t, config.GateModeEnforce)
		run, _, jobID := r10(t, e, "own-cancel")
		res, err := e.st.CancelChainRun(e.ctx, run.ID, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(res.CancelledJobIDs) == 0 || res.CancelledJobIDs[0] != jobID || res.Inconsistency != "" {
			t.Fatalf("cancel result: %+v", res)
		}
		if j, _ := e.st.GetJob(e.ctx, jobID); j.Status != store.StatusCancelled {
			t.Fatalf("released job left %s", j.Status)
		}
	})

	t.Run("cancel with a foreign released job leaves that job alone", func(t *testing.T) {
		e := newChainEnv(t, config.GateModeEnforce)
		run, held, _ := r10(t, e, "own-cancel-foreign")
		e.snap.set(false)
		other := e.createRun("other-queued", 0, "codex")
		foreign := e.snapshot(other.ID).Job // queued implement job of another run
		e.exec(`UPDATE held_jobs SET released_job_id = ? WHERE id = ?`, foreign.ID, held.ID)
		res, err := e.st.CancelChainRun(e.ctx, run.ID, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(res.CancelledJobIDs) != 0 || res.Inconsistency == "" {
			t.Fatalf("cancel result: %+v", res)
		}
		if j, _ := e.st.GetJob(e.ctx, foreign.ID); j.Status != store.StatusQueued {
			t.Fatalf("foreign job %s", j.Status)
		}
		mustStatus(t, e.run(run.ID), store.ChainRunCancelled, "")
	})
}

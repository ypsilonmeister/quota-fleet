package runtime

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

// heldRun returns a run whose current step (review, seq 2) is held by the Gate.
func heldRun(t *testing.T, e *chainEnv, reqID string) (*store.ChainRun, *store.HeldJob) {
	t.Helper()
	e.snap.set(false)
	run := e.createRun(reqID, 0, "codex")
	e.finishCurrent(run.ID, "")
	e.snap.set(true)
	e.sweep()
	s := e.snapshot(run.ID)
	if s.Held == nil || s.Held.Status != store.HeldStatusHeld {
		t.Fatalf("expected a held step, got %+v", s.Step)
	}
	return e.run(run.ID), s.Held
}

func (e *chainEnv) gateChecker() *GateChecker {
	return NewGateChecker(GateCheckerOptions{
		Store: e.st, GateMode: config.GateModeEnforce, Pools: e.pools, Now: e.nowFn,
		Logger: discardLogger(), SnapshotLoader: e.snap.load,
	})
}

// TestChainReleaseHeldVersusCancel fixes docs/22 §5: release and cancel each commit as one
// transaction, so whichever commits first wins completely.
func TestChainReleaseHeldVersusCancel(t *testing.T) {
	t.Run("cancel first, then release does nothing", func(t *testing.T) {
		e := newChainEnv(t, config.GateModeEnforce)
		run, held := heldRun(t, e, "c-first")
		res, err := e.st.CancelChainRun(e.ctx, run.ID, "stop it")
		if err != nil || res.CancelledHeldID != held.ID {
			t.Fatalf("CancelChainRun: %+v %v", res, err)
		}
		e.snap.set(false)
		rr, err := e.gateChecker().ReleaseByID(e.ctx, held.ID)
		if !errors.Is(err, store.ErrConflict) || rr.Released {
			t.Fatalf("release after cancel: %+v %v", rr, err)
		}
		if _, err := e.st.GetJobByRequestID(e.ctx, held.RequestID); !isNotFound(err) {
			t.Fatalf("job created after cancel: %v", err)
		}
		h, _ := e.st.GetHeldJob(e.ctx, held.ID)
		if h.Status != store.HeldStatusCancelled || h.CancelReason != "chain cancelled" {
			t.Fatalf("hold: %s %q", h.Status, h.CancelReason)
		}
	})

	t.Run("release first, then cancel cancels the queued job", func(t *testing.T) {
		e := newChainEnv(t, config.GateModeEnforce)
		run, held := heldRun(t, e, "r-first")
		e.snap.set(false)
		rr, err := e.gateChecker().ReleaseByID(e.ctx, held.ID)
		if err != nil || !rr.Released || rr.Job.ChainRunID != run.ID || rr.Job.ChainSeq != 2 || rr.Job.Status != store.StatusQueued {
			t.Fatalf("release: %+v %v", rr, err)
		}
		if s := e.snapshot(run.ID); s.Step.JobID == nil || *s.Step.JobID != rr.Job.ID || s.Run.Revision != run.Revision+1 {
			t.Fatalf("step not linked in the release transaction: %+v rev=%d", s.Step, s.Run.Revision)
		}
		if n := e.countRows(`SELECT COUNT(*) FROM gate_decisions WHERE job_id = ? AND held_id = ?`, rr.Job.ID, held.ID); n != 1 {
			t.Fatalf("release gate decision rows = %d", n)
		}
		res, err := e.st.CancelChainRun(e.ctx, run.ID, "")
		if err != nil || len(res.CancelledJobIDs) == 0 || res.CancelledJobIDs[0] != rr.Job.ID {
			t.Fatalf("cancel: %+v %v", res, err)
		}
		j, _ := e.st.GetJob(e.ctx, rr.Job.ID)
		if j.Status != store.StatusCancelled {
			t.Fatalf("job status = %s", j.Status)
		}
	})

	t.Run("concurrent release and cancel never leave a job of a cancelled run", func(t *testing.T) {
		for i := 0; i < 25; i++ {
			e := newChainEnv(t, config.GateModeEnforce)
			run, held := heldRun(t, e, fmt.Sprintf("race-%d", i))
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				_, _ = e.st.ReleaseHeld(e.ctx, store.ReleaseHeldParams{HeldID: held.ID, SelectedPool: "codex"})
			}()
			go func() { defer wg.Done(); _, _ = e.st.CancelChainRun(e.ctx, run.ID, "") }()
			wg.Wait()
			mustStatus(t, e.run(run.ID), store.ChainRunCancelled, "")
			h, _ := e.st.GetHeldJob(e.ctx, held.ID)
			job, jerr := e.st.GetJobByRequestID(e.ctx, held.RequestID)
			switch h.Status {
			case store.HeldStatusCancelled:
				if jerr == nil {
					t.Fatalf("iteration %d: hold cancelled but job #%d exists", i, job.ID)
				}
			case store.HeldStatusReleased:
				if jerr != nil || job.Status != store.StatusCancelled {
					t.Fatalf("iteration %d: hold released, job=%v err=%v", i, job, jerr)
				}
			default:
				t.Fatalf("iteration %d: hold left %s", i, h.Status)
			}
		}
	})

	t.Run("cancel does not stop a claimed job and no step follows it", func(t *testing.T) {
		e := newChainEnv(t, config.GateModeOff)
		run := e.createRun("claimed", 0, "")
		job := e.claimCurrent(run.ID)
		res, err := e.st.CancelChainRun(e.ctx, run.ID, "")
		if err != nil || res.RunningJobID == nil || *res.RunningJobID != job.ID {
			t.Fatalf("cancel: %+v %v", res, err)
		}
		if j, _ := e.st.GetJob(e.ctx, job.ID); j.Status != store.StatusClaimed {
			t.Fatalf("claimed job touched: %s", j.Status)
		}
		if _, err := e.st.Complete(e.ctx, job.ID, job.ClaimedBy, "done"); err != nil {
			t.Fatal(err)
		}
		e.sweep()
		if n := len(e.steps(run.ID)); n != 1 {
			t.Fatalf("step created for a cancelled run: %d", n)
		}
		again, err := e.st.CancelChainRun(e.ctx, run.ID, "")
		if err != nil || !again.AlreadyCancelled {
			t.Fatalf("cancel is not idempotent: %+v %v", again, err)
		}
	})

	t.Run("DELETE /gate/held on a chain hold stops the run", func(t *testing.T) {
		e := newChainEnv(t, config.GateModeEnforce)
		run, held := heldRun(t, e, "del")
		h, err := e.st.CancelHeldJob(e.ctx, held.ID, " stale round ")
		if err != nil || h.CancelReason != "stale round" {
			t.Fatalf("CancelHeldJob: %+v %v", h, err)
		}
		got := e.run(run.ID)
		mustStatus(t, got, store.ChainRunStopped, store.ChainStopStepCancelled)
		if !strings.Contains(got.StopDetail, "cancelled via gate API: stale round") {
			t.Fatalf("stop detail %q", got.StopDetail)
		}
		if ns := e.notifications(run.ID); len(ns) != 1 || ns[0].Revision != got.Revision {
			t.Fatalf("notification: %+v", ns)
		}
	})

	t.Run("release of a hold whose run stopped cancels the hold", func(t *testing.T) {
		e := newChainEnv(t, config.GateModeEnforce)
		run, held := heldRun(t, e, "stopped")
		e.exec(`UPDATE chain_runs SET status = 'stopped', stop_reason = 'x' WHERE id = ?`, run.ID)
		e.snap.set(false)
		rr, err := e.gateChecker().ReleaseByID(e.ctx, held.ID)
		if !errors.Is(err, store.ErrConflict) || rr.Released || rr.Held.CancelReason != "chain stopped" {
			t.Fatalf("release: %+v %v", rr, err)
		}
		if _, err := e.st.GetJobByRequestID(e.ctx, held.RequestID); !isNotFound(err) {
			t.Fatal("job created for a stopped run")
		}
	})

	t.Run("permanent release error stops the run, quota wait does not", func(t *testing.T) {
		e := newChainEnv(t, config.GateModeEnforce)
		run, held := heldRun(t, e, "perm")
		// Not releasable (still exhausted): no error, run keeps waiting.
		if rr, err := e.gateChecker().ReleaseByID(e.ctx, held.ID); err != nil || rr.Released {
			t.Fatalf("exhausted release: %+v %v", rr, err)
		}
		e.sweep()
		mustStatus(t, e.run(run.ID), store.ChainRunRunning, "")
		if h, _ := e.st.GetHeldJob(e.ctx, held.ID); h.LastError != "" {
			t.Fatalf("quota wait recorded an error: %q", h.LastError)
		}
		// Permanent: the parent disappeared from the payload.
		e.exec(`UPDATE held_jobs SET payload_json = json_set(payload_json, '$.parent_id', 999999) WHERE id = ?`, held.ID)
		e.snap.set(false)
		if _, err := e.gateChecker().ReleaseByID(e.ctx, held.ID); !errors.Is(err, store.ErrParentNotFound) {
			t.Fatalf("permanent release err = %v", err)
		}
		got := e.run(run.ID)
		mustStatus(t, got, store.ChainRunStopped, store.ChainStopReleaseFailed)
		if ns := e.notifications(run.ID); len(ns) != 1 || ns[0].Revision != got.Revision {
			t.Fatalf("notification: %+v", ns)
		}
	})
}

// TestGateCheckerCheckOnceWalksAllPages: more than one page of holds are all judged even
// when the first page stays exhausted (docs/22 §5.1, F7').
func TestGateCheckerCheckOnceWalksAllPages(t *testing.T) {
	e := newChainEnv(t, config.GateModeEnforce)
	e.pools["blocked"] = config.PoolConfig{Provider: "x"}
	const n = 230
	for i := 0; i < n; i++ {
		pool := "codex"
		if i < store.MaxListLimit {
			pool = "blocked" // the whole first page stays held (no measurement for this pool)
		}
		if _, _, err := e.st.HoldJob(e.ctx, store.HoldJobParams{
			RequestID: fmt.Sprintf("page-%03d", i), Pool: pool,
			Payload: store.CreateJobParams{Title: "t", Pool: pool},
		}); err != nil {
			t.Fatal(err)
		}
	}
	e.snap.set(false)
	if err := e.gateChecker().CheckOnce(e.ctx); err != nil {
		t.Fatal(err)
	}
	released := e.countRows(`SELECT COUNT(*) FROM held_jobs WHERE status = 'released'`)
	if released != n-store.MaxListLimit {
		t.Fatalf("released %d holds, want %d (holds after the first page)", released, n-store.MaxListLimit)
	}
}

package runtime

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/chain"
	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

func TestChainHappyPath(t *testing.T) {
	e := newChainEnv(t, config.GateModeOff)
	run := e.createRun("happy", 0, "")
	if run.MaxRounds != 3 || run.MaxSteps != 7 || run.CurrentSeq != 1 {
		t.Fatalf("run defaults: %+v", run)
	}

	e.sweep() // implement still queued: wait
	if got := e.steps(run.ID); len(got) != 1 {
		t.Fatalf("steps after idle sweep = %d", len(got))
	}
	impl := e.finishCurrent(run.ID, "")
	e.sweep()
	s := e.snapshot(run.ID)
	if s.Step.Step != store.ChainStepReview || s.Step.Round != 1 || s.Step.VerdictNonce == "" || len(s.Step.VerdictNonce) != 32 {
		t.Fatalf("review step: %+v", s.Step)
	}
	if s.Job.ParentID == nil || *s.Job.ParentID != impl.ID || s.Job.Status != store.StatusQueued {
		t.Fatalf("review job parent/status: %+v", s.Job)
	}
	if s.Job.RequestID != store.ChainStepRequestID(run.ID, 2) || s.Job.ChainRunID != run.ID || s.Job.ChainSeq != 2 {
		t.Fatalf("review job chain fields: %+v", s.Job)
	}
	if !strings.Contains(s.Job.Instructions, chain.VerdictPath(run.ID, 2)) || !strings.Contains(s.Job.Instructions, s.Step.VerdictNonce) {
		t.Fatalf("review instructions not expanded: %q", s.Job.Instructions)
	}

	e.finishCurrent(run.ID, "ok_to_merge")
	e.sweep()
	s = e.snapshot(run.ID)
	if s.Step.Step != store.ChainStepPublish || s.Step.DecidedBranch != store.ChainBranchOKToMerge {
		t.Fatalf("publish step: %+v", s.Step)
	}
	if !strings.Contains(s.Job.Instructions, testCommit) {
		t.Fatalf("publish instructions lack reviewed_commit: %q", s.Job.Instructions)
	}
	e.finishCurrent(run.ID, "")
	e.sweep()
	mustStatus(t, e.run(run.ID), store.ChainRunCompleted, "")
	reps := e.notifier.Reports()
	if len(reps) != 1 || reps[0].Kind != store.ChainNotifyCompleted || !strings.HasPrefix(reps[0].EventID, "chain:"+run.ID+":") {
		t.Fatalf("reports: %+v", reps)
	}
	if ns := e.notifications(run.ID); len(ns) != 1 || ns[0].SentAt == nil {
		t.Fatalf("notification not marked sent: %+v", ns)
	}
	if raw, err := e.st.GetMeta(e.ctx, store.MetaKeyChainDriverLastSweep); err != nil || raw == "" {
		t.Fatalf("last sweep meta: %q %v", raw, err)
	}
}

// TestChainDoubleDriveCounterexamples fixes the counterexample table of docs/22 §4.3:
// a second Driver execution (or an operator) that commits between another execution's
// read and its commit leaves exactly one valid step per seq, and the loser writes nothing.
func TestChainDoubleDriveCounterexamples(t *testing.T) {
	t.Run("one execution holds, the other creates a job", func(t *testing.T) {
		for _, order := range []struct {
			name           string
			outer, inner   bool // exhausted? (true = hold)
			wantJob, wantH bool
		}{
			{"outer holds, inner creates", true, false, true, false},
			{"outer creates, inner holds", false, true, false, true},
		} {
			t.Run(order.name, func(t *testing.T) {
				e := newChainEnv(t, config.GateModeEnforce)
				e.snap.set(false)
				run := e.createRun("dd-"+strings.ReplaceAll(order.name, " ", ""), 0, "codex")
				e.finishCurrent(run.ID, "")
				revBefore := e.run(run.ID).Revision

				// Outer plans with the first snapshot; the hook runs a whole inner execution
				// (second snapshot) that commits first.
				e.snap.set(order.outer, order.inner)
				ran := false
				e.drv.beforeAdvance = func(id string) {
					if ran {
						return
					}
					ran = true
					if err := e.drv.ProcessRun(e.ctx, id); err != nil {
						t.Fatalf("inner ProcessRun: %v", err)
					}
				}
				if err := e.drv.ProcessRun(e.ctx, run.ID); err != nil {
					t.Fatalf("outer ProcessRun: %v", err)
				}
				e.drv.beforeAdvance = nil

				key := store.ChainStepRequestID(run.ID, 2)
				_, jerr := e.st.GetJobByRequestID(e.ctx, key)
				_, herr := e.st.GetHeldJobByRequestID(e.ctx, key)
				if (jerr == nil) != order.wantJob || (herr == nil) != order.wantH {
					t.Fatalf("seq 2: job=%v held=%v, want job=%v held=%v", jerr, herr, order.wantJob, order.wantH)
				}
				if n := e.countRows(`SELECT COUNT(*) FROM chain_steps WHERE run_id = ? AND seq = 2`, run.ID); n != 1 {
					t.Fatalf("chain_steps(seq 2) rows = %d", n)
				}
				if n := e.countRows(`SELECT COUNT(*) FROM jobs WHERE chain_run_id = ?`, run.ID); n != map[bool]int{true: 2, false: 1}[order.wantJob] {
					t.Fatalf("jobs of the run = %d", n)
				}
				if got := e.run(run.ID); got.Revision != revBefore+1 || got.CurrentSeq != 2 {
					t.Fatalf("run advanced %d times (rev %d→%d), seq %d", got.Revision-revBefore, revBefore, got.Revision, got.CurrentSeq)
				}
				if !order.wantJob {
					// The loser's job never existed, so the Launcher has nothing to claim.
					if j, ok, err := e.st.Claim(e.ctx, 1000); err != nil || ok {
						t.Fatalf("Claim found a job: %+v %v", j, err)
					}
				}
				// Restart: the state converges, no extra step appears.
				e.reopen()
				e.sweep()
				if n := e.countRows(`SELECT COUNT(*) FROM chain_steps WHERE run_id = ?`, run.ID); n != 2 {
					t.Fatalf("steps after restart = %d", n)
				}
			})
		}
	})

	t.Run("the verdict changes between read and commit", func(t *testing.T) {
		e := newChainEnv(t, config.GateModeOff)
		run := e.createRun("dd-verdict", 0, "")
		e.finishCurrent(run.ID, "")
		e.sweep()
		review := e.finishCurrent(run.ID, "ok_to_merge")
		e.drv.beforeAdvance = func(string) {
			e.exec(`UPDATE chain_artifacts SET verdict = 'changes_needed', raw_sha256 = 'other' WHERE job_id = ?`, review.ID)
		}
		if err := e.drv.ProcessRun(e.ctx, run.ID); err != nil {
			t.Fatal(err)
		}
		e.drv.beforeAdvance = nil
		if n := len(e.steps(run.ID)); n != 2 {
			t.Fatalf("a step was created from a stale verdict (steps=%d)", n)
		}
		e.sweep() // re-reads the DB value
		steps := e.steps(run.ID)
		if len(steps) != 3 || steps[2].Step != store.ChainStepFix {
			t.Fatalf("steps = %+v", steps)
		}
	})

	t.Run("a failure inside the transaction leaves neither pointer nor step", func(t *testing.T) {
		e := newChainEnv(t, config.GateModeOff)
		run := e.createRun("dd-atomic", 0, "")
		e.finishCurrent(run.ID, "")
		key := store.ChainStepRequestID(run.ID, 2)
		e.drv.beforeAdvance = func(string) {
			// Another writer put a held row under the same key (F12: separate UNIQUEs).
			e.exec(`INSERT INTO held_jobs (id, request_id, status, pool, payload_json, created_at, updated_at)
				VALUES ('held-x', ?, 'held', 'p', '{}', ?, ?)`, key, time.Now().UTC().Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano))
		}
		err := e.drv.ProcessRun(e.ctx, run.ID)
		e.drv.beforeAdvance = nil
		if !errors.Is(err, store.ErrConflict) {
			t.Fatalf("ProcessRun err = %v, want conflict", err)
		}
		got := e.run(run.ID)
		if got.CurrentSeq != 1 || len(e.steps(run.ID)) != 1 {
			t.Fatalf("pointer or step written: seq=%d steps=%d", got.CurrentSeq, len(e.steps(run.ID)))
		}
		if _, err := e.st.GetJobByRequestID(e.ctx, key); !isNotFound(err) {
			t.Fatalf("job row written: %v", err)
		}
		// Once the conflicting row is gone, the next sweep converges.
		e.exec(`DELETE FROM held_jobs WHERE id = 'held-x'`)
		e.exec(`UPDATE chain_runs SET next_check_at = NULL WHERE id = ?`, run.ID)
		e.sweep()
		if got := e.run(run.ID); got.CurrentSeq != 2 {
			t.Fatalf("did not converge: seq=%d", got.CurrentSeq)
		}
	})

	t.Run("cancel between read and commit", func(t *testing.T) {
		e := newChainEnv(t, config.GateModeOff)
		run := e.createRun("dd-cancel", 0, "")
		e.finishCurrent(run.ID, "")
		e.drv.beforeAdvance = func(id string) {
			if _, err := e.st.CancelChainRun(e.ctx, id, "operator"); err != nil {
				t.Fatalf("CancelChainRun: %v", err)
			}
		}
		if err := e.drv.ProcessRun(e.ctx, run.ID); err != nil {
			t.Fatal(err)
		}
		e.drv.beforeAdvance = nil
		mustStatus(t, e.run(run.ID), store.ChainRunCancelled, "")
		if n := len(e.steps(run.ID)); n != 1 {
			t.Fatalf("step created after cancel: %d", n)
		}
	})

	t.Run("initial registration is one transaction and idempotent", func(t *testing.T) {
		e := newChainEnv(t, config.GateModeOff)
		d := e.definition("init", 0, "")
		js, hash, _ := d.CanonicalJSON()
		bad := store.ChainStepWrite{Params: store.CreateJobParams{Title: "x", Agent: "codex", Effort: "ultra"}}
		_, _, err := e.st.CreateChainRun(e.ctx, store.CreateChainRunParams{
			RequestID: "init", Owner: "o", Title: "t", DefinitionJSON: js, DefinitionHash: hash,
			DefinitionVersion: 1, MaxRounds: 3, MaxSteps: 7, FirstStep: store.ChainStepImplement, First: bad,
		})
		if err == nil {
			t.Fatal("invalid first step accepted")
		}
		if _, err := e.st.GetChainRunByRequestID(e.ctx, "init"); !isNotFound(err) {
			t.Fatalf("run row left behind: %v", err)
		}
		if n := e.countRows(`SELECT COUNT(*) FROM jobs`); n != 0 {
			t.Fatalf("job left behind: %d", n)
		}
		run := e.createRun("init", 0, "")
		good := store.ChainStepWrite{Params: store.CreateJobParams{Title: "x", Agent: "codex"}}
		p := store.CreateChainRunParams{RequestID: "init", Owner: "owner", Title: "t", DefinitionJSON: run.DefinitionJSON,
			DefinitionHash: run.DefinitionHash, DefinitionVersion: 1, MaxRounds: 3, MaxSteps: 7, FirstStep: store.ChainStepImplement, First: good}
		again, created, err := e.st.CreateChainRun(e.ctx, p)
		if err != nil || created || again.ID != run.ID {
			t.Fatalf("idempotent create: created=%v err=%v id=%s", created, err, again.ID)
		}
		p.DefinitionHash = "different"
		if _, _, err := e.st.CreateChainRun(e.ctx, p); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("different definition: %v", err)
		}
		p.DefinitionHash = run.DefinitionHash
		p.Owner = "someone-else"
		if _, _, err := e.st.CreateChainRun(e.ctx, p); !errors.Is(err, store.ErrConflict) {
			t.Fatalf("different owner: %v", err)
		}
		if n := e.countRows(`SELECT COUNT(*) FROM jobs`); n != 1 {
			t.Fatalf("jobs = %d", n)
		}
	})

	t.Run("reserved chain: prefix and requeue of chain jobs", func(t *testing.T) {
		e := newChainEnv(t, config.GateModeOff)
		if _, _, err := e.st.CreateJob(e.ctx, store.CreateJobParams{RequestID: "chain:x:1", Title: "t"}); !errors.Is(err, store.ErrInvalidInput) {
			t.Fatalf("CreateJob chain: prefix: %v", err)
		}
		if _, _, err := e.st.HoldJob(e.ctx, store.HoldJobParams{RequestID: "chain:x:1", Pool: "p", Payload: store.CreateJobParams{Title: "t"}}); !errors.Is(err, store.ErrInvalidInput) {
			t.Fatalf("HoldJob chain: prefix: %v", err)
		}
		run := e.createRun("rq", 0, "")
		e.failCurrent(run.ID)
		s := e.snapshot(run.ID)
		if _, err := e.st.Requeue(e.ctx, s.Job.ID); !errors.Is(err, store.ErrConflict) || !errors.Is(err, store.ErrChainManaged) {
			t.Fatalf("Requeue chain job: %v", err)
		}
	})
}

// TestChainTerminationMaxRounds fixes docs/22 §3.3 / §13-5: a reviewer that always asks for
// changes ends in max_rounds_exceeded within 2 × max_rounds + 1 steps.
func TestChainTerminationMaxRounds(t *testing.T) {
	for _, tc := range []struct{ maxRounds, wantSteps int }{{1, 2}, {3, 6}, {10, 20}} {
		e := newChainEnv(t, config.GateModeOff)
		run := e.createRun("term", tc.maxRounds, "")
		for i := 0; i < 100; i++ {
			s := e.snapshot(run.ID)
			if s.Run.Status != store.ChainRunRunning {
				break
			}
			verdict := ""
			if s.Step.Step == store.ChainStepReview {
				verdict = "changes_needed"
			}
			e.finishCurrent(run.ID, verdict)
			e.sweep()
		}
		got := e.run(run.ID)
		mustStatus(t, got, store.ChainRunStopped, store.ChainStopMaxRoundsExceeded)
		steps := e.steps(run.ID)
		if len(steps) != tc.wantSteps || len(steps) > chain.MaxSteps(tc.maxRounds) {
			t.Fatalf("max_rounds=%d: %d steps (bound %d)", tc.maxRounds, len(steps), chain.MaxSteps(tc.maxRounds))
		}
		reviews, fixes := 0, 0
		for _, st := range steps {
			switch st.Step {
			case store.ChainStepReview:
				reviews++
			case store.ChainStepFix:
				fixes++
			}
		}
		if reviews != tc.maxRounds || fixes != tc.maxRounds-1 || got.Round != tc.maxRounds {
			t.Fatalf("max_rounds=%d: reviews=%d fixes=%d round=%d", tc.maxRounds, reviews, fixes, got.Round)
		}
	}
	// The second guard: a run whose budget is smaller than the fixed shape needs stops.
	e := newChainEnv(t, config.GateModeOff)
	run := e.createRun("budget", 3, "")
	e.exec(`UPDATE chain_runs SET max_steps = 1 WHERE id = ?`, run.ID)
	e.finishCurrent(run.ID, "")
	e.sweep()
	mustStatus(t, e.run(run.ID), store.ChainRunStopped, store.ChainStopStepBudgetExceeded)
}

// TestChainSweepRepairTable fixes docs/22 §6: every state R1–R12 and T1–T2 is waited on,
// advanced or stopped as listed.
func TestChainSweepRepairTable(t *testing.T) {
	type setup func(e *chainEnv, run *store.ChainRun)
	toReview := func(e *chainEnv, run *store.ChainRun) { e.finishCurrent(run.ID, ""); e.sweep() }
	cases := []struct {
		name       string
		gate       string
		prepare    setup
		wantStatus store.ChainRunStatus
		wantReason string
		check      func(t *testing.T, e *chainEnv, run *store.ChainRun)
	}{
		{"R1 queued job waits", config.GateModeOff, func(e *chainEnv, run *store.ChainRun) {}, store.ChainRunRunning, "",
			func(t *testing.T, e *chainEnv, run *store.ChainRun) {
				if len(e.steps(run.ID)) != 1 {
					t.Fatal("advanced while queued")
				}
			}},
		{"R1 claimed job waits", config.GateModeOff, func(e *chainEnv, run *store.ChainRun) { e.claimCurrent(run.ID) }, store.ChainRunRunning, "", nil},
		{"R2 implement done creates review", config.GateModeOff, func(e *chainEnv, run *store.ChainRun) { e.finishCurrent(run.ID, "") }, store.ChainRunRunning, "",
			func(t *testing.T, e *chainEnv, run *store.ChainRun) {
				if s := e.snapshot(run.ID); s.Step.Step != store.ChainStepReview {
					t.Fatalf("step = %s", s.Step.Step)
				}
			}},
		{"R2 fix done creates review", config.GateModeOff, func(e *chainEnv, run *store.ChainRun) {
			toReview(e, run)
			e.finishCurrent(run.ID, "changes_needed")
			e.sweep()
			e.finishCurrent(run.ID, "")
		}, store.ChainRunRunning, "", func(t *testing.T, e *chainEnv, run *store.ChainRun) {
			if s := e.snapshot(run.ID); s.Step.Step != store.ChainStepReview || s.Step.Round != 2 {
				t.Fatalf("step = %+v", s.Step)
			}
		}},
		{"R3 ok_to_merge creates publish", config.GateModeOff, func(e *chainEnv, run *store.ChainRun) {
			toReview(e, run)
			e.finishCurrent(run.ID, "ok_to_merge")
		}, store.ChainRunRunning, "", func(t *testing.T, e *chainEnv, run *store.ChainRun) {
			if s := e.snapshot(run.ID); s.Step.Step != store.ChainStepPublish {
				t.Fatalf("step = %s", s.Step.Step)
			}
		}},
		{"R3 changes_needed creates fix", config.GateModeOff, func(e *chainEnv, run *store.ChainRun) {
			toReview(e, run)
			e.finishCurrent(run.ID, "changes_needed")
		}, store.ChainRunRunning, "", func(t *testing.T, e *chainEnv, run *store.ChainRun) {
			s := e.snapshot(run.ID)
			if s.Step.Step != store.ChainStepFix || s.Run.Round != 2 || !strings.Contains(s.Job.Instructions, testCommit) {
				t.Fatalf("fix step: %+v round=%d", s.Step, s.Run.Round)
			}
		}},
		{"R4 review done without verdict (manual complete)", config.GateModeOff, func(e *chainEnv, run *store.ChainRun) {
			toReview(e, run)
			e.finishCurrent(run.ID, "")
		}, store.ChainRunStopped, store.ChainStopVerdictUnavailable, nil},
		{"R4 review done with invalid verdict", config.GateModeOff, func(e *chainEnv, run *store.ChainRun) {
			toReview(e, run)
			job := e.claimCurrent(run.ID)
			if _, err := e.st.CompleteWithArtifact(e.ctx, job.ID, job.ClaimedBy, "ok", &store.ChainArtifact{
				JobID: job.ID, RunID: run.ID, Seq: 2, Status: store.ChainArtifactInvalid, Reason: "nonce does not match"}); err != nil {
				t.Fatal(err)
			}
		}, store.ChainRunStopped, store.ChainStopVerdictUnavailable, nil},
		{"R5 publish done completes", config.GateModeOff, func(e *chainEnv, run *store.ChainRun) {
			toReview(e, run)
			e.finishCurrent(run.ID, "ok_to_merge")
			e.sweep()
			e.finishCurrent(run.ID, "")
		}, store.ChainRunCompleted, "", nil},
		{"R6 failed step stops", config.GateModeOff, func(e *chainEnv, run *store.ChainRun) { e.failCurrent(run.ID) }, store.ChainRunStopped, store.ChainStopStepFailed, nil},
		{"R7 cancelled job stops", config.GateModeOff, func(e *chainEnv, run *store.ChainRun) {
			s := e.snapshot(run.ID)
			if _, err := e.st.Cancel(e.ctx, s.Job.ID, "op"); err != nil {
				t.Fatal(err)
			}
		}, store.ChainRunStopped, store.ChainStopStepCancelled, nil},
		{"R8 held step waits", config.GateModeEnforce, func(e *chainEnv, run *store.ChainRun) {
			e.finishCurrent(run.ID, "")
			e.snap.set(true)
		}, store.ChainRunRunning, "", func(t *testing.T, e *chainEnv, run *store.ChainRun) {
			s := e.snapshot(run.ID)
			if s.Held == nil || s.Held.Status != store.HeldStatusHeld || s.Held.ChainRunID != run.ID || s.Held.ChainSeq != 2 {
				t.Fatalf("held step: %+v", s.Held)
			}
		}},
		{"R9 held with permanent error stops", config.GateModeEnforce, func(e *chainEnv, run *store.ChainRun) {
			e.finishCurrent(run.ID, "")
			e.snap.set(true)
			e.sweep()
			s := e.snapshot(run.ID)
			if _, _, err := e.st.RecordHeldReleaseError(e.ctx, s.Held.ID, "parent job not found"); err != nil {
				t.Fatal(err)
			}
		}, store.ChainRunStopped, store.ChainStopReleaseFailed, nil},
		{"R10 released hold without link is linked", config.GateModeEnforce, func(e *chainEnv, run *store.ChainRun) {
			e.finishCurrent(run.ID, "")
			e.snap.set(true)
			e.sweep()
			s := e.snapshot(run.ID)
			if _, err := e.st.ReleaseHeld(e.ctx, store.ReleaseHeldParams{HeldID: s.Held.ID, SelectedPool: "codex"}); err != nil {
				t.Fatal(err)
			}
			e.exec(`UPDATE chain_steps SET job_id = NULL WHERE run_id = ? AND seq = 2`, run.ID) // pre-§5.1 state
		}, store.ChainRunRunning, "", func(t *testing.T, e *chainEnv, run *store.ChainRun) {
			s := e.snapshot(run.ID)
			if s.Job == nil || s.Job.ChainSeq != 2 {
				t.Fatalf("not linked: %+v", s.Step)
			}
		}},
		{"R11 cancelled hold stops", config.GateModeEnforce, func(e *chainEnv, run *store.ChainRun) {
			e.finishCurrent(run.ID, "")
			e.snap.set(true)
			e.sweep()
			e.exec(`UPDATE held_jobs SET status = 'cancelled' WHERE chain_run_id = ?`, run.ID)
		}, store.ChainRunStopped, store.ChainStopStepCancelled, nil},
		{"R12 missing step stops", config.GateModeOff, func(e *chainEnv, run *store.ChainRun) {
			e.exec(`UPDATE chain_runs SET current_seq = 5 WHERE id = ?`, run.ID)
		}, store.ChainRunStopped, store.ChainStopInternalInconsistency, nil},
		{"R12 step without job or hold stops", config.GateModeOff, func(e *chainEnv, run *store.ChainRun) {
			e.exec(`UPDATE chain_steps SET job_id = NULL WHERE run_id = ?`, run.ID)
		}, store.ChainRunStopped, store.ChainStopInternalInconsistency, nil},
		{"R12 step pointing at a missing job stops", config.GateModeOff, func(e *chainEnv, run *store.ChainRun) {
			e.exec(`PRAGMA foreign_keys = OFF`)
			db := e.raw()
			conn, _ := db.Conn(e.ctx)
			defer conn.Close()
			_, _ = conn.ExecContext(e.ctx, `PRAGMA foreign_keys = OFF`)
			if _, err := conn.ExecContext(e.ctx, `UPDATE chain_steps SET job_id = 999999 WHERE run_id = ?`, run.ID); err != nil {
				t.Fatal(err)
			}
		}, store.ChainRunStopped, store.ChainStopInternalInconsistency, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newChainEnv(t, tc.gate)
			e.snap.set(false)
			run := e.createRun("rep", 0, "codex")
			tc.prepare(e, run)
			e.sweep()
			got := e.run(run.ID)
			mustStatus(t, got, tc.wantStatus, tc.wantReason)
			if tc.wantStatus == store.ChainRunStopped {
				ns := e.notifications(run.ID)
				if len(ns) != 1 || ns[0].Kind != store.ChainNotifyStopped || ns[0].Revision != got.Revision || ns[0].SentAt == nil {
					t.Fatalf("stop notification: %+v (run rev %d)", ns, got.Revision)
				}
			}
			if tc.check != nil {
				tc.check(t, e, run)
			}
		})
	}

	t.Run("R1 stall is reported once per revision and does not stop", func(t *testing.T) {
		e := newChainEnv(t, config.GateModeOff)
		run := e.createRun("stall", 0, "")
		e.now = time.Now().Add(25 * time.Hour) // step created_at uses the wall clock
		e.sweep()
		e.sweep()
		ns := e.notifications(run.ID)
		if len(ns) != 1 || ns[0].Kind != store.ChainNotifyStalled {
			t.Fatalf("stall notifications: %+v", ns)
		}
		mustStatus(t, e.run(run.ID), store.ChainRunRunning, "")
	})

	t.Run("T1 hold of a finished run is cancelled", func(t *testing.T) {
		e := newChainEnv(t, config.GateModeEnforce)
		e.snap.set(false)
		run := e.createRun("t1", 0, "codex")
		e.finishCurrent(run.ID, "")
		e.snap.set(true)
		e.sweep()
		e.exec(`UPDATE chain_runs SET status = 'stopped', stop_reason = 'step_failed' WHERE id = ?`, run.ID)
		e.sweep()
		s := e.snapshot(run.ID)
		if s.Held.Status != store.HeldStatusCancelled || s.Held.CancelReason != "chain stopped" {
			t.Fatalf("hold: %s %q", s.Held.Status, s.Held.CancelReason)
		}
	})

	t.Run("T2 notifications are retried and delivered at least once", func(t *testing.T) {
		e := newChainEnv(t, config.GateModeOff)
		run := e.createRun("t2", 0, "")
		e.failCurrent(run.ID)
		e.notifier.setFail(errors.New("webhook down"))
		e.sweep()
		mustStatus(t, e.run(run.ID), store.ChainRunStopped, store.ChainStopStepFailed)
		ns := e.notifications(run.ID)
		if len(ns) != 1 || ns[0].SentAt != nil || ns[0].Attempts != 1 || ns[0].LastError == "" || ns[0].NextAttemptAt == nil {
			t.Fatalf("failed send not recorded: %+v", ns[0])
		}
		if st, _ := e.st.ChainNotificationPendingStats(e.ctx); st.Pending != 1 {
			t.Fatalf("pending = %d", st.Pending)
		}
		e.notifier.setFail(nil)
		e.sweep() // not due yet
		if len(e.notifier.Reports()) != 0 {
			t.Fatal("sent before next_attempt_at")
		}
		e.now = e.now.Add(2 * time.Hour)
		// Stop after the send but before sent_at commits: the next sweep sends again.
		e.drv.afterSend = func(*store.ChainNotification) bool { return false }
		e.sweep()
		e.drv.afterSend = nil
		e.sweep()
		reps := e.notifier.Reports()
		if len(reps) != 2 || reps[0].EventID != reps[1].EventID || reps[0].EventID != ns[0].EventID() {
			t.Fatalf("reports: %+v", reps)
		}
		if ns := e.notifications(run.ID); ns[0].SentAt == nil {
			t.Fatal("sent_at not committed")
		}
		e.sweep()
		if len(e.notifier.Reports()) != 2 {
			t.Fatal("sent again after sent_at")
		}
	})
}

// TestChainSweepIsolatesBrokenRun: one run that cannot be read does not stop the others,
// and it is retried later (next_check_at).
func TestChainSweepIsolatesBrokenRun(t *testing.T) {
	e := newChainEnv(t, config.GateModeOff)
	broken := e.createRun("broken", 0, "")
	healthy := e.createRun("healthy", 0, "")
	e.finishCurrent(broken.ID, "")
	e.finishCurrent(healthy.ID, "")
	brokenJob := e.steps(broken.ID)[0].JobID
	e.exec(`UPDATE jobs SET created_at = 'not-a-time' WHERE id = ?`, *brokenJob)
	e.sweep()
	if got := e.run(healthy.ID); got.CurrentSeq != 2 {
		t.Fatalf("healthy run did not advance: seq=%d", got.CurrentSeq)
	}
	b := e.run(broken.ID)
	if b.NextCheckAt == nil || !b.NextCheckAt.After(e.now) || b.CheckFailures != 1 || b.Status != store.ChainRunRunning {
		t.Fatalf("broken run not deferred: %+v", b)
	}
}

// TestReconcilerReportsStaleChainDriver: GET /status and the Reconciler make a stopped
// Driver visible (docs/22 §6).
func TestReconcilerReportsStaleChainDriver(t *testing.T) {
	e := newChainEnv(t, config.GateModeOff)
	e.createRun("stale", 0, "")
	notif := &recordingNotifier{}
	rec := NewReconciler(ReconcilerOptions{Store: e.st, Notifier: notif, Now: e.nowFn, Logger: discardLogger(), ChainDriverStaleAfter: 3 * time.Second})
	_ = rec.ReconcileOnce(e.ctx)
	if reps := notif.Reports(); len(reps) != 1 || reps[0].Kind != "chain_driver_stale" {
		t.Fatalf("no stale report before the first sweep: %+v", reps)
	}
	e.sweep()
	_ = rec.ReconcileOnce(e.ctx)
	if len(notif.Reports()) != 1 {
		t.Fatal("reported although the driver just swept")
	}
	e.now = e.now.Add(time.Minute)
	_ = rec.ReconcileOnce(e.ctx)
	_ = rec.ReconcileOnce(e.ctx)
	if reps := notif.Reports(); len(reps) != 2 {
		t.Fatalf("stale driver reports = %d, want 2 (deduplicated per last sweep)", len(reps))
	}
}

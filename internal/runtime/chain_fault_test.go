package runtime

import (
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/chain"
	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

// chainDBState counts every table a chain transaction writes, plus the run's own row.
func (e *chainEnv) chainDBState(runID string) map[string]string {
	e.t.Helper()
	out := map[string]string{}
	for _, tbl := range []string{"chain_runs", "chain_steps", "jobs", "held_jobs", "signal_outbox", "gate_decisions", "chain_notifications", "chain_artifacts"} {
		out[tbl] = fmt.Sprint(e.countRows(`SELECT COUNT(*) FROM ` + tbl))
	}
	out["held_status"] = fmt.Sprint(e.countRows(`SELECT COUNT(*) FROM held_jobs WHERE status = 'held'`))
	if runID != "" {
		r := e.run(runID)
		out["run"] = fmt.Sprintf("%s seq=%d rev=%d round=%d", r.Status, r.CurrentSeq, r.Revision, r.Round)
		out["linked_steps"] = fmt.Sprint(e.countRows(`SELECT COUNT(*) FROM chain_steps WHERE run_id = ? AND job_id IS NOT NULL`, runID))
	}
	return out
}

// TestChainFaultAfterWritesRollsBackEverything injects a failure inside the transaction
// after rows were written (docs/22 §13): the job or hold, its Outbox row, the gate
// decision, the step history, the run pointer and the notification row all roll back
// together, and the next sweep converges.
func TestChainFaultAfterWritesRollsBackEverything(t *testing.T) {
	boom := errors.New("injected fault")
	failAt := func(e *chainEnv, stage string) *int {
		hits := new(int)
		e.st.SetChainFaultHookForTest(func(s string) error {
			if s == stage {
				*hits++
				return boom
			}
			return nil
		})
		e.t.Cleanup(func() { e.st.SetChainFaultHookForTest(nil) })
		return hits
	}
	retrySweep := func(e *chainEnv, runID string) {
		e.st.SetChainFaultHookForTest(nil)
		e.exec(`UPDATE chain_runs SET next_check_at = NULL WHERE id = ?`, runID)
		e.sweep()
	}

	for _, stage := range []string{store.ChainFaultAdvanceAfterStep, store.ChainFaultAdvanceAfterPointer} {
		for _, hold := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s hold=%v", stage, hold), func(t *testing.T) {
				e := newChainEnv(t, config.GateModeEnforce)
				e.snap.set(false)
				run := e.createRun("fault-adv", 0, "codex")
				e.finishCurrent(run.ID, "")
				e.snap.set(hold)
				before := e.chainDBState(run.ID)
				hits := failAt(e, stage)
				if err := e.drv.ProcessRun(e.ctx, run.ID); !errors.Is(err, boom) {
					t.Fatalf("ProcessRun err = %v", err)
				}
				if *hits != 1 {
					t.Fatalf("fault point reached %d times", *hits)
				}
				if after := e.chainDBState(run.ID); !reflect.DeepEqual(before, after) {
					t.Fatalf("partial write survived:\nbefore %v\nafter  %v", before, after)
				}
				retrySweep(e, run.ID)
				got := e.chainDBState(run.ID)
				if got["run"] == before["run"] || got["chain_steps"] == before["chain_steps"] || got["gate_decisions"] == before["gate_decisions"] {
					t.Fatalf("did not converge (or the fault point wrote no gate decision): %v", got)
				}
				if !hold && got["signal_outbox"] == before["signal_outbox"] {
					t.Fatalf("no Outbox row on the converged advance: %v", got)
				}
			})
		}
	}

	t.Run("stop: run status and notification roll back together", func(t *testing.T) {
		e := newChainEnv(t, config.GateModeOff)
		run := e.createRun("fault-stop", 0, "")
		e.failCurrent(run.ID)
		before := e.chainDBState(run.ID)
		failAt(e, store.ChainFaultStopAfterWrite)
		if err := e.drv.ProcessRun(e.ctx, run.ID); !errors.Is(err, boom) {
			t.Fatalf("ProcessRun err = %v", err)
		}
		if after := e.chainDBState(run.ID); !reflect.DeepEqual(before, after) {
			t.Fatalf("partial write survived:\nbefore %v\nafter  %v", before, after)
		}
		retrySweep(e, run.ID)
		mustStatus(t, e.run(run.ID), store.ChainRunStopped, store.ChainStopStepFailed)
		if ns := e.notifications(run.ID); len(ns) != 1 {
			t.Fatalf("notifications = %d", len(ns))
		}
	})

	t.Run("release: job, Outbox, hold, link, revision and gate decision roll back together", func(t *testing.T) {
		e := newChainEnv(t, config.GateModeEnforce)
		run, held := heldRun(t, e, "fault-rel")
		e.snap.set(false)
		before := e.chainDBState(run.ID)
		failAt(e, store.ChainFaultReleaseAfterLink)
		if _, err := e.gateChecker().ReleaseByID(e.ctx, held.ID); !errors.Is(err, boom) {
			t.Fatalf("release err = %v", err)
		}
		if after := e.chainDBState(run.ID); !reflect.DeepEqual(before, after) {
			t.Fatalf("partial write survived:\nbefore %v\nafter  %v", before, after)
		}
		e.st.SetChainFaultHookForTest(nil)
		rr, err := e.gateChecker().ReleaseByID(e.ctx, held.ID)
		if err != nil || !rr.Released {
			t.Fatalf("release after the fault: %+v %v", rr, err)
		}
		got := e.chainDBState(run.ID)
		if got["linked_steps"] == before["linked_steps"] || got["signal_outbox"] == before["signal_outbox"] || got["gate_decisions"] == before["gate_decisions"] {
			t.Fatalf("release did not write all rows: %v", got)
		}
	})

	t.Run("create: run, first job, Outbox, gate decision and step roll back together", func(t *testing.T) {
		e := newChainEnv(t, config.GateModeEnforce)
		e.snap.set(false)
		before := e.chainDBState("")
		d := e.definition("fault-create", 0, "codex")
		js, hash, _ := d.CanonicalJSON()
		runID, _ := chain.NewRunID()
		first, reject, err := chain.PlanStep(e.ctx, e.adm, d, chain.StepInput{
			Run: &store.ChainRun{ID: runID, Scope: d.Scope, Owner: "owner"}, Step: store.ChainStepImplement, Seq: 1,
		})
		if err != nil || reject != nil {
			t.Fatalf("PlanStep: %v %v", err, reject)
		}
		params := store.CreateChainRunParams{
			RequestID: d.RequestID, Owner: "owner", Title: d.Title, Scope: d.Scope,
			DefinitionJSON: js, DefinitionHash: hash, DefinitionVersion: chain.DefinitionVersion,
			MaxRounds: d.MaxRounds, MaxSteps: chain.MaxSteps(d.MaxRounds), RunID: runID,
			FirstStep: store.ChainStepImplement, First: first,
		}
		failAt(e, store.ChainFaultCreateAfterStep)
		if _, _, err := e.st.CreateChainRun(e.ctx, params); !errors.Is(err, boom) {
			t.Fatalf("CreateChainRun err = %v", err)
		}
		if after := e.chainDBState(""); !reflect.DeepEqual(before, after) {
			t.Fatalf("partial write survived:\nbefore %v\nafter  %v", before, after)
		}
		e.st.SetChainFaultHookForTest(nil)
		if _, created, err := e.st.CreateChainRun(e.ctx, params); err != nil || !created {
			t.Fatalf("CreateChainRun after the fault: %v %v", created, err)
		}
		got := e.chainDBState("")
		if got["gate_decisions"] == before["gate_decisions"] || got["signal_outbox"] == before["signal_outbox"] {
			t.Fatalf("create did not write gate decision and Outbox: %v", got)
		}
	})
}

// TestChainDoubleDriveTwoConnections runs two Drivers on two Store instances (two
// SQLite write connections to the same file, as two processes would) against the same
// run (docs/22 §13): exactly one step per seq is written, with one job and one Outbox row.
func TestChainDoubleDriveTwoConnections(t *testing.T) {
	second := func(e *chainEnv) *ChainDriver {
		st2, err := store.Open(e.ctx, filepath.Join(e.dir, "fleet.db"), store.Options{InstanceID: "chain-test-2"})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st2.Close() })
		return NewChainDriver(ChainDriverOptions{
			Store: st2, Admission: e.adm, Notifier: e.notifier, Interval: time.Second,
			StallAfter: 24 * time.Hour, Now: e.nowFn, Logger: discardLogger(),
		})
	}
	check := func(t *testing.T, e *chainEnv, run *store.ChainRun) {
		t.Helper()
		key := store.ChainStepRequestID(run.ID, 2)
		if n := e.countRows(`SELECT COUNT(*) FROM chain_steps WHERE run_id = ? AND seq = 2`, run.ID); n != 1 {
			t.Fatalf("seq 2 rows = %d", n)
		}
		if n := e.countRows(`SELECT COUNT(*) FROM chain_steps WHERE run_id = ?`, run.ID); n != 2 {
			t.Fatalf("steps = %d", n)
		}
		if n := e.countRows(`SELECT COUNT(*) FROM jobs WHERE request_id = ?`, key); n != 1 {
			t.Fatalf("jobs for seq 2 = %d", n)
		}
		if n := e.countRows(`SELECT COUNT(*) FROM signal_outbox o JOIN jobs j ON j.id = o.job_id WHERE j.request_id = ?`, key); n != 1 {
			t.Fatalf("Outbox rows for seq 2 = %d", n)
		}
		if got := e.run(run.ID); got.CurrentSeq != 2 || got.Revision != run.Revision+1 {
			t.Fatalf("run seq=%d rev=%d (before rev %d)", got.CurrentSeq, got.Revision, run.Revision)
		}
	}

	t.Run("second connection commits between the first one's read and commit", func(t *testing.T) {
		e := newChainEnv(t, config.GateModeOff)
		drv2 := second(e)
		run := e.createRun("two-conn", 0, "")
		e.finishCurrent(run.ID, "")
		run = e.run(run.ID)
		ran := false
		e.drv.beforeAdvance = func(id string) {
			if ran {
				return
			}
			ran = true
			if err := drv2.ProcessRun(e.ctx, id); err != nil {
				t.Errorf("second connection: %v", err)
			}
		}
		if err := e.drv.ProcessRun(e.ctx, run.ID); err != nil {
			t.Fatalf("first connection: %v", err)
		}
		e.drv.beforeAdvance = nil
		if !ran {
			t.Fatal("hook did not run")
		}
		check(t, e, run)
	})

	t.Run("free-running on two connections", func(t *testing.T) {
		for i := 0; i < 15; i++ {
			e := newChainEnv(t, config.GateModeOff)
			drv2 := second(e)
			run := e.createRun(fmt.Sprintf("two-conn-%d", i), 0, "")
			e.finishCurrent(run.ID, "")
			run = e.run(run.ID)
			var wg sync.WaitGroup
			errs := make([]error, 2)
			for k, d := range []*ChainDriver{e.drv, drv2} {
				wg.Add(1)
				go func(k int, d *ChainDriver) {
					defer wg.Done()
					errs[k] = d.ProcessRun(e.ctx, run.ID)
				}(k, d)
			}
			wg.Wait()
			for _, err := range errs {
				if err != nil && !errors.Is(err, store.ErrConflict) {
					t.Fatalf("iteration %d: %v", i, err)
				}
			}
			check(t, e, run)
		}
	})
}

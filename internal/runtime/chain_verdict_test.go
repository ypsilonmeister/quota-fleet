package runtime

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ypsilonmeister/quota-fleet/internal/adapter"
	"github.com/ypsilonmeister/quota-fleet/internal/chain"
	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/evaluator"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

// localFAAdapter is the mock adapter plus local file access to the job's workdir.
type localFAAdapter struct {
	*adapter.MockAdapter
	faErr error
}

func (a *localFAAdapter) FileAccessFor(job *store.Job) (adapter.FileAccess, error) {
	if a.faErr != nil {
		return nil, a.faErr
	}
	return adapter.NewLocalFileAccess(job.Workdir), nil
}

func (e *chainEnv) shepherd(ad adapter.Adapter) *Shepherd {
	return NewShepherd(ShepherdOptions{
		Store: e.st, Adapter: ad, Evaluator: evaluator.NewAlwaysPass(), Notifier: &recordingNotifier{},
		MaxEvalRetries: 2, Logger: discardLogger(),
	})
}

func writeVerdict(t *testing.T, workdir, runID string, seq int, body string) string {
	t.Helper()
	p := filepath.Join(workdir, filepath.FromSlash(chain.VerdictPath(runID, seq)))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func verdictBody(runID string, seq int, nonce, verdict string) string {
	b, _ := json.Marshal(map[string]any{"schema": chain.VerdictSchema, "run_id": runID, "seq": seq, "nonce": nonce,
		"verdict": verdict, "reviewed_commit": testCommit})
	return string(b)
}

// reviewAt drives a run to its review at the given round (seq 2 for round 1, seq 4 for round 2)
// and claims it. It returns the claimed job and its step.
func reviewAt(t *testing.T, e *chainEnv, run *store.ChainRun, round int) (*store.Job, *store.ChainStep) {
	t.Helper()
	e.finishCurrent(run.ID, "")
	e.sweep()
	for r := 1; r < round; r++ {
		e.finishCurrent(run.ID, "changes_needed")
		e.sweep()
		e.finishCurrent(run.ID, "")
		e.sweep()
	}
	job := e.claimCurrent(run.ID)
	st, err := e.st.GetChainStep(e.ctx, run.ID, job.ChainSeq)
	if err != nil || st.Step != store.ChainStepReview {
		t.Fatalf("not at a review step: %+v %v", st, err)
	}
	return job, st
}

func (e *chainEnv) finishWithShepherd(sh *Shepherd, job *store.Job) {
	sh.finishExitedJob(e.ctx, job, adapter.RunState{State: adapter.StateExited, ExitCode: 0})
}

// TestChainVerdictCapture fixes docs/22 §7 / §13-4: only the current review's own verdict
// file is captured, it is captured with the completion, and the Driver never reads files.
func TestChainVerdictCapture(t *testing.T) {
	cases := []struct {
		name   string
		round  int
		write  func(t *testing.T, e *chainEnv, run *store.ChainRun, st *store.ChainStep)
		status string
		stop   string
	}{
		{"valid verdict is captured", 1, func(t *testing.T, e *chainEnv, run *store.ChainRun, st *store.ChainStep) {
			writeVerdict(t, e.workdir, run.ID, st.Seq, verdictBody(run.ID, st.Seq, st.VerdictNonce, "ok_to_merge"))
		}, store.ChainArtifactCaptured, ""},
		{"only the previous round's file exists", 2, func(t *testing.T, e *chainEnv, run *store.ChainRun, st *store.ChainStep) {
			prev, _ := e.st.GetChainStep(e.ctx, run.ID, 2)
			writeVerdict(t, e.workdir, run.ID, 2, verdictBody(run.ID, 2, prev.VerdictNonce, "ok_to_merge"))
		}, store.ChainArtifactMissing, store.ChainStopVerdictUnavailable},
		{"previous round's file copied to this seq", 2, func(t *testing.T, e *chainEnv, run *store.ChainRun, st *store.ChainStep) {
			prev, _ := e.st.GetChainStep(e.ctx, run.ID, 2)
			writeVerdict(t, e.workdir, run.ID, st.Seq, verdictBody(run.ID, 2, prev.VerdictNonce, "ok_to_merge"))
		}, store.ChainArtifactInvalid, store.ChainStopVerdictUnavailable},
		{"copied with seq edited still has the old nonce", 2, func(t *testing.T, e *chainEnv, run *store.ChainRun, st *store.ChainStep) {
			prev, _ := e.st.GetChainStep(e.ctx, run.ID, 2)
			writeVerdict(t, e.workdir, run.ID, st.Seq, verdictBody(run.ID, st.Seq, prev.VerdictNonce, "ok_to_merge"))
		}, store.ChainArtifactInvalid, store.ChainStopVerdictUnavailable},
		{"half-written file", 1, func(t *testing.T, e *chainEnv, run *store.ChainRun, st *store.ChainStep) {
			body := verdictBody(run.ID, st.Seq, st.VerdictNonce, "ok_to_merge")
			writeVerdict(t, e.workdir, run.ID, st.Seq, body[:len(body)-10])
		}, store.ChainArtifactInvalid, store.ChainStopVerdictUnavailable},
		{"over 4 KiB", 1, func(t *testing.T, e *chainEnv, run *store.ChainRun, st *store.ChainStep) {
			writeVerdict(t, e.workdir, run.ID, st.Seq, verdictBody(run.ID, st.Seq, st.VerdictNonce, "ok_to_merge")+strings.Repeat(" ", 5000))
		}, store.ChainArtifactInvalid, store.ChainStopVerdictUnavailable},
		{"symbolic link", 1, func(t *testing.T, e *chainEnv, run *store.ChainRun, st *store.ChainStep) {
			outside := filepath.Join(t.TempDir(), "v.json")
			_ = os.WriteFile(outside, []byte(verdictBody(run.ID, st.Seq, st.VerdictNonce, "ok_to_merge")), 0o644)
			p := filepath.Join(e.workdir, filepath.FromSlash(chain.VerdictPath(run.ID, st.Seq)))
			_ = os.MkdirAll(filepath.Dir(p), 0o755)
			if err := os.Symlink(outside, p); err != nil {
				t.Skip("symlinks unsupported")
			}
		}, store.ChainArtifactInvalid, store.ChainStopVerdictUnavailable},
		{"symbolic link in a parent directory", 1, func(t *testing.T, e *chainEnv, run *store.ChainRun, st *store.ChainStep) {
			outside := t.TempDir()
			_ = os.MkdirAll(filepath.Join(outside, run.ID, "2"), 0o755)
			_ = os.WriteFile(filepath.Join(outside, run.ID, "2", "verdict.json"), []byte(verdictBody(run.ID, st.Seq, st.VerdictNonce, "ok_to_merge")), 0o644)
			if err := os.Symlink(outside, filepath.Join(e.workdir, ".qf-chain")); err != nil {
				t.Skip("symlinks unsupported")
			}
		}, store.ChainArtifactInvalid, store.ChainStopVerdictUnavailable},
		{"a directory instead of a file", 1, func(t *testing.T, e *chainEnv, run *store.ChainRun, st *store.ChainStep) {
			_ = os.MkdirAll(filepath.Join(e.workdir, filepath.FromSlash(chain.VerdictPath(run.ID, st.Seq))), 0o755)
		}, store.ChainArtifactInvalid, store.ChainStopVerdictUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newChainEnv(t, config.GateModeOff)
			run := e.createRun("v", 0, "")
			job, st := reviewAt(t, e, run, tc.round)
			tc.write(t, e, run, st)
			e.finishWithShepherd(e.shepherd(&localFAAdapter{MockAdapter: adapter.NewMock(adapter.MockOptions{})}), job)
			a, err := e.st.GetChainArtifact(e.ctx, job.ID)
			if err != nil {
				t.Fatalf("no artifact: %v", err)
			}
			if a.Status != tc.status {
				t.Fatalf("artifact %s (%s), want %s", a.Status, a.Reason, tc.status)
			}
			if j, _ := e.st.GetJob(e.ctx, job.ID); j.Status != store.StatusDone {
				t.Fatalf("job status %s", j.Status)
			}
			e.sweep()
			got := e.run(run.ID)
			if tc.stop == "" {
				if got.Status != store.ChainRunRunning || e.snapshot(run.ID).Step.Step != store.ChainStepPublish {
					t.Fatalf("did not advance to publish: %+v", got)
				}
				return
			}
			mustStatus(t, got, store.ChainRunStopped, tc.stop)
		})
	}

	t.Run("host unreachable is retried, then stored as unavailable", func(t *testing.T) {
		e := newChainEnv(t, config.GateModeOff)
		run := e.createRun("v", 0, "")
		job, st := reviewAt(t, e, run, 1)
		writeVerdict(t, e.workdir, run.ID, st.Seq, verdictBody(run.ID, st.Seq, st.VerdictNonce, "ok_to_merge"))
		ad := &localFAAdapter{MockAdapter: adapter.NewMock(adapter.MockOptions{}), faErr: errors.New("ssh: connect: no route to host")}
		sh := e.shepherd(ad)
		e.finishWithShepherd(sh, job)
		if j, _ := e.st.GetJob(e.ctx, job.ID); j.Status != store.StatusClaimed {
			t.Fatalf("completed on the first transient error: %s", j.Status)
		}
		e.finishWithShepherd(sh, job) // second attempt reaches MaxEvalRetries=2
		a, err := e.st.GetChainArtifact(e.ctx, job.ID)
		if err != nil || a.Status != store.ChainArtifactUnavailable {
			t.Fatalf("artifact: %+v %v", a, err)
		}
		e.sweep()
		mustStatus(t, e.run(run.ID), store.ChainRunStopped, store.ChainStopVerdictUnavailable)
	})

	t.Run("adapter without file access stores unavailable", func(t *testing.T) {
		e := newChainEnv(t, config.GateModeOff)
		run := e.createRun("v", 0, "")
		job, _ := reviewAt(t, e, run, 1)
		e.finishWithShepherd(e.shepherd(adapter.NewMock(adapter.MockOptions{})), job)
		if a, err := e.st.GetChainArtifact(e.ctx, job.ID); err != nil || a.Status != store.ChainArtifactUnavailable {
			t.Fatalf("artifact: %+v %v", a, err)
		}
	})

	t.Run("rewriting the file after done does not change the decision, even after restart", func(t *testing.T) {
		e := newChainEnv(t, config.GateModeOff)
		run := e.createRun("v", 0, "")
		job, st := reviewAt(t, e, run, 1)
		writeVerdict(t, e.workdir, run.ID, st.Seq, verdictBody(run.ID, st.Seq, st.VerdictNonce, "changes_needed"))
		e.finishWithShepherd(e.shepherd(&localFAAdapter{MockAdapter: adapter.NewMock(adapter.MockOptions{})}), job)
		before, _ := e.st.GetChainArtifact(e.ctx, job.ID)
		writeVerdict(t, e.workdir, run.ID, st.Seq, verdictBody(run.ID, st.Seq, st.VerdictNonce, "ok_to_merge"))
		e.reopen()
		e.sweep()
		after, _ := e.st.GetChainArtifact(e.ctx, job.ID)
		if after.Verdict != "changes_needed" || after.RawSHA256 != before.RawSHA256 {
			t.Fatalf("artifact changed: %+v", after)
		}
		if s := e.snapshot(run.ID); s.Step.Step != store.ChainStepFix {
			t.Fatalf("decision changed: next step %s", s.Step.Step)
		}
	})

	t.Run("non-review chain steps and plain jobs are not captured", func(t *testing.T) {
		e := newChainEnv(t, config.GateModeOff)
		run := e.createRun("v", 0, "")
		job := e.claimCurrent(run.ID) // implement
		e.finishWithShepherd(e.shepherd(&localFAAdapter{MockAdapter: adapter.NewMock(adapter.MockOptions{})}), job)
		if _, err := e.st.GetChainArtifact(e.ctx, job.ID); !isNotFound(err) {
			t.Fatalf("implement step captured: %v", err)
		}
	})
}

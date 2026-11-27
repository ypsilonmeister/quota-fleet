package runtime

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/adapter"
	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/evaluator"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

func newLocalLinuxSSHAdapter(agentCmd string) *adapter.SSHAdapter {
	cfg := &config.Config{
		Hosts: map[string]config.HostConfig{
			"local-linux": {
				Transport: "local",
				OS:        "linux",
				Agents: map[string]config.HostAgentOverride{
					"claude": {Command: agentCmd},
				},
			},
		},
		Agents: map[string]config.AgentConfig{
			"claude": {
				Launch:      "claude",
				PromptStyle: config.PromptStyleStdin,
			},
		},
	}
	return adapter.NewSSHAdapter(cfg, adapter.SSHAdapterOptions{
		Transport: adapter.NewLocalTransport(adapter.LocalTransportOptions{}),
	})
}

func writePerJobArtifacts(t *testing.T, workdir string, jobID int64, exitContent, logContent, pidContent string) string {
	t.Helper()
	runDir := filepath.Join(workdir, ".fleet", "runs", fmt.Sprintf("%d", jobID))
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatalf("mkdir runDir %q: %v", runDir, err)
	}
	if exitContent != "" {
		if err := os.WriteFile(filepath.Join(runDir, "agent.exit"), []byte(exitContent), 0o600); err != nil {
			t.Fatalf("write agent.exit: %v", err)
		}
	}
	if logContent != "" {
		if err := os.WriteFile(filepath.Join(runDir, "agent.log"), []byte(logContent), 0o600); err != nil {
			t.Fatalf("write agent.log: %v", err)
		}
	}
	if pidContent != "" {
		if err := os.WriteFile(filepath.Join(runDir, "agent.pid"), []byte(pidContent), 0o600); err != nil {
			t.Fatalf("write agent.pid: %v", err)
		}
	}
	return runDir
}

// Regression test (b): a claimed job whose per-job artifacts already show process exit
// (missed completion) is automatically recovered by Reconciler through evidence collection
// and acceptance evaluation to done/failed without re-running the agent, while missing or
// inconsistent artifacts still follow the Missing -> reset path.
func TestReconcilerRecoversMissedCompletionWithoutRelaunch(t *testing.T) {
	ctx := context.Background()

	t.Run("exit 0 with passing deliverables recovers to done without relaunch", func(t *testing.T) {
		st, _ := openTestStore(t, "recover-done")
		workdir := t.TempDir()
		if err := os.WriteFile(filepath.Join(workdir, "summary.md"), []byte("PASS_EVIDENCE_ISSUE_40\n"), 0o644); err != nil {
			t.Fatalf("write summary.md: %v", err)
		}

		// Point agent command at a script that MUST NEVER execute during recovery.
		sentinelPath := filepath.Join(workdir, "agent-was-relaunched")
		forbidCmd := filepath.Join(t.TempDir(), "forbid.sh")
		if err := os.WriteFile(forbidCmd, []byte("#!/bin/sh\ntouch "+sentinelPath+"\nexit 99\n"), 0o700); err != nil {
			t.Fatalf("write forbid.sh: %v", err)
		}
		sshAdapter := newLocalLinuxSSHAdapter(forbidCmd)

		job, _, err := st.CreateJob(ctx, store.CreateJobParams{
			RequestID:          "missed-done-1",
			Title:              "Missed completion passing job",
			Agent:              "claude",
			Host:               "local-linux",
			Workdir:            workdir,
			AcceptanceCriteria: "file_nonempty: summary.md\ncontains: summary.md :: PASS_EVIDENCE_ISSUE_40",
		})
		if err != nil {
			t.Fatalf("CreateJob: %v", err)
		}
		claimed, ok, err := st.Claim(ctx, 2)
		if err != nil || !ok || claimed.ID != job.ID {
			t.Fatalf("Claim: ok=%v err=%v", ok, err)
		}

		// Simulate missed completion: agent already finished and recorded exit=0 + log in per-job dir,
		// while DB still has status=claimed.
		writePerJobArtifacts(t, workdir, claimed.ID, "0\n", "worker completed cleanly\n", "999999\n")

		var logBuf bytes.Buffer
		recNotifier := &recordingNotifier{}
		eval := evaluator.NewTypedDecision(evaluator.TypedDecisionOptions{
			CommandTimeout: 5 * time.Second,
			Getenv:         func(string) string { return "" },
		})

		fakeNow := time.Now().UTC().Add(30 * time.Minute)
		reconciler := NewReconciler(ReconcilerOptions{
			Store:             st,
			Adapter:           sshAdapter,
			Evaluator:         eval,
			Notifier:          recNotifier,
			StaleClaimedAfter: 10 * time.Minute,
			AttemptsCap:       3,
			Now:               func() time.Time { return fakeNow },
			Logger:            log.New(&logBuf, "", 0),
		})

		if err := reconciler.ReconcileOnce(ctx); err != nil {
			t.Fatalf("ReconcileOnce: %v", err)
		}

		got, err := st.GetJob(ctx, claimed.ID)
		if err != nil {
			t.Fatalf("GetJob: %v", err)
		}
		if got.Status != store.StatusDone {
			t.Fatalf("status = %s, want done (last_error=%q, result=%q)", got.Status, got.LastError, got.ResultText)
		}
		if got.DeliveredAt == nil {
			t.Errorf("DeliveredAt = nil, want non-nil")
		}
		if got.Attempts != 1 {
			t.Errorf("Attempts = %d, want 1 (must not relaunch)", got.Attempts)
		}
		if _, err := os.Stat(sentinelPath); !os.IsNotExist(err) {
			t.Fatalf("agent command was unexpectedly re-executed during recovery")
		}
		for _, want := range []string{
			"overall: PASS",
			"typed file_nonempty: summary.md",
			"typed contains: matched in summary.md",
			"recovered_by: reconciler",
		} {
			if !strings.Contains(got.ResultText, want) {
				t.Errorf("ResultText missing %q:\n%s", want, got.ResultText)
			}
		}
		if !strings.Contains(logBuf.String(), "reconciler recovered exited claimed job") {
			t.Errorf("reconciler log missing recovery record:\n%s", logBuf.String())
		}
		reports := recNotifier.Reports()
		if len(reports) != 1 || reports[0].Kind != "stale_claimed_recovered" || reports[0].JobID != claimed.ID {
			t.Errorf("unexpected reconciler reports: %+v", reports)
		}
		if len(recNotifier.Deliveries()) != 1 || recNotifier.Deliveries()[0].Status != store.StatusDone {
			t.Errorf("unexpected deliveries: %+v", recNotifier.Deliveries())
		}
	})

	t.Run("non-zero exit recovers to failed with preserved log tail evidence", func(t *testing.T) {
		st, _ := openTestStore(t, "recover-nonzero")
		workdir := t.TempDir()
		sshAdapter := newLocalLinuxSSHAdapter("/bin/false")

		job, _, err := st.CreateJob(ctx, store.CreateJobParams{
			RequestID:          "missed-fail-exit",
			Title:              "Missed non-zero exit job",
			Agent:              "claude",
			Host:               "local-linux",
			Workdir:            workdir,
			AcceptanceCriteria: "always pass if zero",
		})
		if err != nil {
			t.Fatalf("CreateJob: %v", err)
		}
		claimed, ok, err := st.Claim(ctx, 2)
		if err != nil || !ok || claimed.ID != job.ID {
			t.Fatalf("Claim: ok=%v err=%v", ok, err)
		}

		writePerJobArtifacts(t, workdir, claimed.ID, "3\n", "fatal: preserved worker error output\n", "999998\n")

		recNotifier := &recordingNotifier{}
		fakeNow := time.Now().UTC().Add(30 * time.Minute)
		reconciler := NewReconciler(ReconcilerOptions{
			Store:             st,
			Adapter:           sshAdapter,
			Evaluator:         evaluator.NewAlwaysPass(),
			Notifier:          recNotifier,
			StaleClaimedAfter: 10 * time.Minute,
			AttemptsCap:       3,
			Now:               func() time.Time { return fakeNow },
			Logger:            discardLogger(),
		})

		if err := reconciler.ReconcileOnce(ctx); err != nil {
			t.Fatalf("ReconcileOnce: %v", err)
		}

		got, err := st.GetJob(ctx, claimed.ID)
		if err != nil {
			t.Fatalf("GetJob: %v", err)
		}
		if got.Status != store.StatusFailed {
			t.Fatalf("status = %s, want failed", got.Status)
		}
		if got.Attempts != 1 {
			t.Errorf("Attempts = %d, want 1", got.Attempts)
		}
		if !strings.Contains(got.LastError, "3") || !strings.Contains(got.LastError, "preserved worker error output") {
			t.Errorf("LastError missing exit code or log tail: %q", got.LastError)
		}
		if !strings.Contains(got.ResultText, "exit_code: 3") ||
			!strings.Contains(got.ResultText, "preserved worker error output") ||
			!strings.Contains(got.ResultText, "recovered_by: reconciler") {
			t.Errorf("ResultText unexpected:\n%s", got.ResultText)
		}
		if len(recNotifier.Reports()) != 1 || recNotifier.Reports()[0].Kind != "stale_claimed_recovered" {
			t.Errorf("unexpected reports: %+v", recNotifier.Reports())
		}
	})

	t.Run("exit 0 with failing acceptance criteria recovers to failed", func(t *testing.T) {
		st, _ := openTestStore(t, "recover-eval-fail")
		workdir := t.TempDir()
		sshAdapter := newLocalLinuxSSHAdapter("/bin/false")

		job, _, err := st.CreateJob(ctx, store.CreateJobParams{
			RequestID:          "missed-eval-fail",
			Title:              "Missed exit 0 missing deliverable",
			Agent:              "claude",
			Host:               "local-linux",
			Workdir:            workdir,
			AcceptanceCriteria: "file_nonempty: summary.md",
		})
		if err != nil {
			t.Fatalf("CreateJob: %v", err)
		}
		claimed, ok, err := st.Claim(ctx, 2)
		if err != nil || !ok || claimed.ID != job.ID {
			t.Fatalf("Claim: ok=%v err=%v", ok, err)
		}

		writePerJobArtifacts(t, workdir, claimed.ID, "0\n", "exited 0 without creating summary.md\n", "999997\n")

		eval := evaluator.NewTypedDecision(evaluator.TypedDecisionOptions{
			CommandTimeout: 5 * time.Second,
			Getenv:         func(string) string { return "" },
		})
		fakeNow := time.Now().UTC().Add(30 * time.Minute)
		reconciler := NewReconciler(ReconcilerOptions{
			Store:             st,
			Adapter:           sshAdapter,
			Evaluator:         eval,
			Notifier:          &recordingNotifier{},
			StaleClaimedAfter: 10 * time.Minute,
			AttemptsCap:       3,
			Now:               func() time.Time { return fakeNow },
			Logger:            discardLogger(),
		})

		if err := reconciler.ReconcileOnce(ctx); err != nil {
			t.Fatalf("ReconcileOnce: %v", err)
		}

		got, err := st.GetJob(ctx, claimed.ID)
		if err != nil {
			t.Fatalf("GetJob: %v", err)
		}
		if got.Status != store.StatusFailed {
			t.Fatalf("status = %s, want failed", got.Status)
		}
		if !strings.Contains(got.ResultText, "overall: FAIL") ||
			!strings.Contains(got.ResultText, "not found: summary.md") ||
			!strings.Contains(got.ResultText, "recovered_by: reconciler") {
			t.Errorf("ResultText unexpected:\n%s", got.ResultText)
		}
	})

	t.Run("missing or corrupt artifacts still follow Missing -> reset path even with sibling running in same workdir", func(t *testing.T) {
		st, _ := openTestStore(t, "recover-missing-reset")
		workdir := t.TempDir()
		sleeper := filepath.Join(t.TempDir(), "sleep.sh")
		if err := os.WriteFile(sleeper, []byte("#!/bin/sh\nsleep 30\n"), 0o700); err != nil {
			t.Fatalf("write sleep.sh: %v", err)
		}
		sshAdapter := newLocalLinuxSSHAdapter(sleeper)

		// Job 1: missing artifacts completely.
		jMissing, _, err := st.CreateJob(ctx, store.CreateJobParams{
			RequestID: "missing-no-art",
			Title:     "No artifacts",
			Agent:     "claude",
			Host:      "local-linux",
			Workdir:   workdir,
		})
		if err != nil {
			t.Fatalf("CreateJob jMissing: %v", err)
		}
		// Job 2: corrupt agent.exit.
		jCorrupt, _, err := st.CreateJob(ctx, store.CreateJobParams{
			RequestID: "missing-corrupt-exit",
			Title:     "Corrupt exit file",
			Agent:     "claude",
			Host:      "local-linux",
			Workdir:   workdir,
		})
		if err != nil {
			t.Fatalf("CreateJob jCorrupt: %v", err)
		}
		// Job 3: genuinely running in the SAME workdir.
		jRunning, _, err := st.CreateJob(ctx, store.CreateJobParams{
			RequestID: "sibling-running",
			Title:     "Running sibling",
			Agent:     "claude",
			Host:      "local-linux",
			Workdir:   workdir,
		})
		if err != nil {
			t.Fatalf("CreateJob jRunning: %v", err)
		}

		for i := 0; i < 3; i++ {
			if _, ok, err := st.Claim(ctx, 5); err != nil || !ok {
				t.Fatalf("Claim %d: ok=%v err=%v", i, ok, err)
			}
		}

		writePerJobArtifacts(t, workdir, jCorrupt.ID, "corrupt-exit\n", "partial\n", "")
		runningJob, _ := st.GetJob(ctx, jRunning.ID)
		info, err := sshAdapter.Launch(ctx, runningJob)
		if err != nil {
			t.Fatalf("Launch sibling running job: %v", err)
		}
		t.Cleanup(func() {
			_ = syscall.Kill(-info.PID, syscall.SIGKILL)
			_ = syscall.Kill(info.PID, syscall.SIGKILL)
		})

		fakeNow := time.Now().UTC().Add(30 * time.Minute)
		reconciler := NewReconciler(ReconcilerOptions{
			Store:             st,
			Adapter:           sshAdapter,
			Evaluator:         evaluator.NewAlwaysPass(),
			Notifier:          &recordingNotifier{},
			StaleClaimedAfter: 10 * time.Minute,
			AttemptsCap:       3,
			Now:               func() time.Time { return fakeNow },
			Logger:            discardLogger(),
		})

		if err := reconciler.ReconcileOnce(ctx); err != nil {
			t.Fatalf("ReconcileOnce: %v", err)
		}

		gotMissing, _ := st.GetJob(ctx, jMissing.ID)
		gotCorrupt, _ := st.GetJob(ctx, jCorrupt.ID)
		gotRunning, _ := st.GetJob(ctx, jRunning.ID)
		if gotMissing.Status != store.StatusQueued {
			t.Errorf("jMissing status = %s, want queued", gotMissing.Status)
		}
		if gotCorrupt.Status != store.StatusQueued {
			t.Errorf("jCorrupt status = %s, want queued", gotCorrupt.Status)
		}
		if gotRunning.Status != store.StatusClaimed {
			t.Errorf("jRunning status = %s, want claimed (still running)", gotRunning.Status)
		}
	})

	t.Run("shared Shepherd in-flight guard prevents duplicate evaluation between Shepherd and Reconciler", func(t *testing.T) {
		st, _ := openTestStore(t, "recover-inflight-guard")
		mockAdapter := adapter.NewMock(adapter.MockOptions{
			Default: adapter.MockBehavior{
				ExitAfterPolls: 1,
				ExitCode:       0,
			},
		})
		recNotifier := &recordingNotifier{}

		evalStarted := make(chan struct{})
		releaseEval := make(chan struct{})
		var evalMu sync.Mutex
		evalCalls := 0
		blockingEval := &scriptedEvaluator{
			handler: func(call int) (evaluator.Result, error) {
				evalMu.Lock()
				evalCalls++
				evalMu.Unlock()
				if call == 1 {
					close(evalStarted)
					<-releaseEval
				}
				return evaluator.Result{Passed: true}, nil
			},
		}

		shepherd := NewShepherd(ShepherdOptions{
			Store:     st,
			Adapter:   mockAdapter,
			Evaluator: blockingEval,
			Notifier:  recNotifier,
			Logger:    discardLogger(),
		})
		fakeNow := time.Now().UTC().Add(30 * time.Minute)
		reconciler := NewReconciler(ReconcilerOptions{
			Store:             st,
			Adapter:           mockAdapter,
			Shepherd:          shepherd,
			Notifier:          recNotifier,
			StaleClaimedAfter: 10 * time.Minute,
			AttemptsCap:       3,
			Now:               func() time.Time { return fakeNow },
			Logger:            discardLogger(),
		})

		job, _, err := st.CreateJob(ctx, store.CreateJobParams{
			RequestID:          "inflight-1",
			Title:              "Concurrent sweep guard",
			AcceptanceCriteria: "passes",
		})
		if err != nil {
			t.Fatalf("CreateJob: %v", err)
		}
		claimed, ok, err := st.Claim(ctx, 2)
		if err != nil || !ok || claimed.ID != job.ID {
			t.Fatalf("Claim: ok=%v err=%v", ok, err)
		}
		if _, err := mockAdapter.Launch(ctx, claimed); err != nil {
			t.Fatalf("Launch: %v", err)
		}
		mockAdapter.FinishJob(job.ID, 0)

		shepherdDone := make(chan error, 1)
		go func() {
			shepherdDone <- shepherd.SweepOnce(ctx)
		}()

		<-evalStarted
		if err := reconciler.ReconcileOnce(ctx); err != nil {
			t.Fatalf("ReconcileOnce while Shepherd finishing: %v", err)
		}
		close(releaseEval)

		if err := <-shepherdDone; err != nil {
			t.Fatalf("Shepherd SweepOnce: %v", err)
		}

		evalMu.Lock()
		gotCalls := evalCalls
		evalMu.Unlock()
		if gotCalls != 1 {
			t.Errorf("evaluator calls = %d, want 1 (Reconciler should skip in-flight job)", gotCalls)
		}
		got, _ := st.GetJob(ctx, job.ID)
		if got.Status != store.StatusDone {
			t.Fatalf("status = %s, want done", got.Status)
		}
		if strings.Contains(got.ResultText, "recovered_by: reconciler") {
			t.Errorf("normal Shepherd completion should not be tagged recovered_by: reconciler:\n%s", got.ResultText)
		}
	})
}

package runtime

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/adapter"
	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/evaluator"
	"github.com/ypsilonmeister/quota-fleet/internal/quota"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
	_ "modernc.org/sqlite"
)

func openTestStore(t *testing.T, instanceID string) (*store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "fleet.db")
	st, err := store.Open(context.Background(), dbPath, store.Options{InstanceID: instanceID})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.InitInstanceID(context.Background(), instanceID); err != nil {
		t.Fatalf("InitInstanceID: %v", err)
	}
	return st, dir
}

func discardLogger() *log.Logger {
	return log.New(io.Discard, "", 0)
}

func waitUntil(t *testing.T, timeout time.Duration, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for: %s", timeout, desc)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

type recordingNotifier struct {
	mu         sync.Mutex
	deliveries []*store.Job
	reports    []Report
}

func (n *recordingNotifier) Deliver(ctx context.Context, job *store.Job) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	cp := *job
	n.deliveries = append(n.deliveries, &cp)
	return nil
}

func (n *recordingNotifier) Report(ctx context.Context, report Report) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.reports = append(n.reports, report)
	return nil
}

func (n *recordingNotifier) Reports() []Report {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]Report, len(n.reports))
	copy(out, n.reports)
	return out
}

func (n *recordingNotifier) Deliveries() []*store.Job {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]*store.Job, len(n.deliveries))
	copy(out, n.deliveries)
	return out
}

func TestE2ESingleJobLifecycle(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	st, stateDir := openTestStore(t, "e2e-single")
	mockAdapter := adapter.NewMock(adapter.MockOptions{
		Default: adapter.MockBehavior{
			ExitAfterPolls: 2,
			ExitCode:       0,
		},
	})

	rt, err := New(Options{
		Store:             st,
		Adapter:           mockAdapter,
		Evaluator:         evaluator.NewAlwaysPass(),
		InstanceID:        "e2e-single",
		StateDir:          stateDir,
		MaxClaimed:        2,
		AttemptsCap:       3,
		DispatchInterval:  10 * time.Millisecond,
		ShepherdInterval:  10 * time.Millisecond,
		ReconcileInterval: 20 * time.Millisecond,
		StaleClaimedAfter: 5 * time.Second,
		Logger:            discardLogger(),
	})
	if err != nil {
		t.Fatalf("runtime.New: %v", err)
	}
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("runtime.Start: %v", err)
	}
	defer rt.Stop()

	job, created, err := st.CreateJob(ctx, store.CreateJobParams{
		RequestID:          "e2e-single-1",
		Title:              "Single E2E job",
		Agent:              "claude",
		Host:               "ubuntu",
		Workdir:            "/work/demo",
		AcceptanceCriteria: "build succeeds\ntests pass",
	})
	if err != nil || !created {
		t.Fatalf("CreateJob: created=%v err=%v", created, err)
	}

	waitUntil(t, 3*time.Second, "single job done and delivered", func() bool {
		j, err := st.GetJob(ctx, job.ID)
		return err == nil && j.Status == store.StatusDone && j.DeliveredAt != nil
	})

	got, err := st.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1", got.Attempts)
	}
	if !strings.Contains(got.ResultText, "overall: PASS") || !strings.Contains(got.ResultText, "[PASS] tests pass") {
		t.Errorf("unexpected ResultText:\n%s", got.ResultText)
	}
	if mockAdapter.PrepareCalls(job.ID) != 1 || mockAdapter.LaunchCalls(job.ID) != 1 || mockAdapter.CollectCalls(job.ID) != 1 {
		t.Errorf("adapter calls: prepare=%d launch=%d collect=%d",
			mockAdapter.PrepareCalls(job.ID), mockAdapter.LaunchCalls(job.ID), mockAdapter.CollectCalls(job.ID))
	}

	// Verify delivery file exists and is valid JSON.
	deliveryPath := filepath.Join(stateDir, "deliveries", fmt.Sprintf("job-%d.json", job.ID))
	data, err := os.ReadFile(deliveryPath)
	if err != nil {
		t.Fatalf("read delivery file %q: %v", deliveryPath, err)
	}
	var payload DeliveryPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("unmarshal delivery file: %v", err)
	}
	if payload.JobID != job.ID || payload.Status != "done" || payload.Title != "Single E2E job" {
		t.Errorf("unexpected delivery payload: %+v", payload)
	}

	// Verify signal.jsonl contains an 8-field record for this job.
	sigPath := filepath.Join(stateDir, "signal.jsonl")
	sigData, err := os.ReadFile(sigPath)
	if err != nil {
		t.Fatalf("read signal.jsonl: %v", err)
	}
	if !strings.Contains(string(sigData), fmt.Sprintf(`"job_id":%d`, job.ID)) {
		t.Errorf("signal.jsonl missing job_id=%d:\n%s", job.ID, string(sigData))
	}
}

func TestE2EChainParentToChildPromotion(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	st, stateDir := openTestStore(t, "e2e-chain")
	mockAdapter := adapter.NewMock(adapter.MockOptions{
		Default: adapter.MockBehavior{
			ExitAfterPolls: 1,
		},
	})

	rt, err := New(Options{
		Store:             st,
		Adapter:           mockAdapter,
		Evaluator:         evaluator.NewAlwaysPass(),
		InstanceID:        "e2e-chain",
		StateDir:          stateDir,
		MaxClaimed:        2,
		AttemptsCap:       3,
		DispatchInterval:  10 * time.Millisecond,
		ShepherdInterval:  10 * time.Millisecond,
		ReconcileInterval: 25 * time.Millisecond,
		StaleClaimedAfter: 5 * time.Second,
		Logger:            discardLogger(),
	})
	if err != nil {
		t.Fatalf("runtime.New: %v", err)
	}
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("runtime.Start: %v", err)
	}
	defer rt.Stop()

	parent, _, err := st.CreateJob(ctx, store.CreateJobParams{
		RequestID:          "chain-parent",
		Title:              "Parent implementation",
		AcceptanceCriteria: "implementation compiles",
	})
	if err != nil {
		t.Fatalf("CreateJob parent: %v", err)
	}

	parentID := parent.ID
	child, _, err := st.CreateJob(ctx, store.CreateJobParams{
		RequestID:          "chain-child",
		Title:              "Child review",
		ParentID:           &parentID,
		AcceptanceCriteria: "review completed",
	})
	if err != nil {
		t.Fatalf("CreateJob child: %v", err)
	}
	if child.Status != store.StatusWaiting && child.Status != store.StatusQueued {
		t.Fatalf("initial child status = %s, want waiting (or queued if parent already finished)", child.Status)
	}

	waitUntil(t, 3*time.Second, "parent and child both done and delivered", func() bool {
		p, err1 := st.GetJob(ctx, parent.ID)
		c, err2 := st.GetJob(ctx, child.ID)
		return err1 == nil && err2 == nil &&
			p.Status == store.StatusDone && p.DeliveredAt != nil &&
			c.Status == store.StatusDone && c.DeliveredAt != nil
	})

	if mockAdapter.LaunchCalls(parent.ID) != 1 || mockAdapter.LaunchCalls(child.ID) != 1 {
		t.Errorf("launch calls: parent=%d child=%d, want 1 each",
			mockAdapter.LaunchCalls(parent.ID), mockAdapter.LaunchCalls(child.ID))
	}
}

func TestE2EParentFailureCancelsChild(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	st, stateDir := openTestStore(t, "e2e-fail-cascade")
	mockAdapter := adapter.NewMock(adapter.MockOptions{
		Default: adapter.MockBehavior{
			ExitAfterPolls: 1,
		},
	})
	eval := evaluator.NewSelectiveFail(evaluator.SelectiveFailOptions{
		FailSubstrings: []string{"unit tests"},
		FailReason:     "test suite returned non-zero",
	})

	rt, err := New(Options{
		Store:             st,
		Adapter:           mockAdapter,
		Evaluator:         eval,
		InstanceID:        "e2e-fail-cascade",
		StateDir:          stateDir,
		MaxClaimed:        2,
		AttemptsCap:       3,
		DispatchInterval:  10 * time.Millisecond,
		ShepherdInterval:  10 * time.Millisecond,
		ReconcileInterval: 25 * time.Millisecond,
		StaleClaimedAfter: 5 * time.Second,
		Logger:            discardLogger(),
	})
	if err != nil {
		t.Fatalf("runtime.New: %v", err)
	}
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("runtime.Start: %v", err)
	}
	defer rt.Stop()

	parent, _, err := st.CreateJob(ctx, store.CreateJobParams{
		RequestID:          "fail-parent",
		Title:              "Failing parent",
		AcceptanceCriteria: "build succeeds\nunit tests pass",
	})
	if err != nil {
		t.Fatalf("CreateJob parent: %v", err)
	}
	parentID := parent.ID
	child, _, err := st.CreateJob(ctx, store.CreateJobParams{
		RequestID:          "fail-child",
		Title:              "Dependent child",
		ParentID:           &parentID,
		AcceptanceCriteria: "review passes",
	})
	if err != nil {
		t.Fatalf("CreateJob child: %v", err)
	}

	waitUntil(t, 3*time.Second, "parent failed+delivered and child cancelled", func() bool {
		p, err1 := st.GetJob(ctx, parent.ID)
		c, err2 := st.GetJob(ctx, child.ID)
		return err1 == nil && err2 == nil &&
			p.Status == store.StatusFailed && p.DeliveredAt != nil &&
			c.Status == store.StatusCancelled
	})

	gotParent, err := st.GetJob(ctx, parent.ID)
	if err != nil {
		t.Fatalf("GetJob parent: %v", err)
	}
	if !strings.Contains(gotParent.ResultText, "[PASS] build succeeds") ||
		!strings.Contains(gotParent.ResultText, "[FAIL] unit tests pass (test suite returned non-zero)") {
		t.Errorf("parent ResultText missing per-criterion details:\n%s", gotParent.ResultText)
	}
	if mockAdapter.LaunchCalls(child.ID) != 0 {
		t.Errorf("cancelled child should never be launched, got %d calls", mockAdapter.LaunchCalls(child.ID))
	}
}

func TestE2EMaxClaimedConcurrencyLimit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	st, stateDir := openTestStore(t, "e2e-max-claimed")
	mockAdapter := adapter.NewMock(adapter.MockOptions{
		Default: adapter.MockBehavior{
			StayRunning: true,
		},
	})

	rt, err := New(Options{
		Store:             st,
		Adapter:           mockAdapter,
		Evaluator:         evaluator.NewAlwaysPass(),
		InstanceID:        "e2e-max-claimed",
		StateDir:          stateDir,
		MaxClaimed:        1,
		AttemptsCap:       3,
		DispatchInterval:  10 * time.Millisecond,
		ShepherdInterval:  10 * time.Millisecond,
		ReconcileInterval: 30 * time.Millisecond,
		StaleClaimedAfter: 5 * time.Second,
		Logger:            discardLogger(),
	})
	if err != nil {
		t.Fatalf("runtime.New: %v", err)
	}
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("runtime.Start: %v", err)
	}
	defer rt.Stop()

	j1, _, err := st.CreateJob(ctx, store.CreateJobParams{
		RequestID:          "cap-1",
		Title:              "Job 1",
		AcceptanceCriteria: "done",
	})
	if err != nil {
		t.Fatalf("CreateJob 1: %v", err)
	}
	j2, _, err := st.CreateJob(ctx, store.CreateJobParams{
		RequestID:          "cap-2",
		Title:              "Job 2",
		AcceptanceCriteria: "done",
	})
	if err != nil {
		t.Fatalf("CreateJob 2: %v", err)
	}

	// Wait until job 1 is launched while job 2 remains queued.
	waitUntil(t, 3*time.Second, "job 1 claimed and launched", func() bool {
		g1, err := st.GetJob(ctx, j1.ID)
		return err == nil && g1.Status == store.StatusClaimed && mockAdapter.LaunchCalls(j1.ID) == 1
	})

	// Give dispatcher/launcher a couple ticks to confirm job 2 is NOT claimed while job 1 is active.
	time.Sleep(40 * time.Millisecond)

	counts, err := st.CountByStatus(ctx)
	if err != nil {
		t.Fatalf("CountByStatus: %v", err)
	}
	if counts[store.StatusClaimed] != 1 || counts[store.StatusQueued] != 1 {
		t.Fatalf("while job 1 running: claimed=%d queued=%d, want 1 and 1",
			counts[store.StatusClaimed], counts[store.StatusQueued])
	}
	if mockAdapter.MaxConcurrentRunning() > 1 {
		t.Fatalf("MaxConcurrentRunning = %d, want <= 1", mockAdapter.MaxConcurrentRunning())
	}

	// Finish job 1; Shepherd completes it and wakes Launcher so job 2 is claimed next.
	mockAdapter.FinishJob(j1.ID, 0)

	waitUntil(t, 3*time.Second, "job 1 done and job 2 claimed+launched", func() bool {
		g1, err1 := st.GetJob(ctx, j1.ID)
		g2, err2 := st.GetJob(ctx, j2.ID)
		return err1 == nil && err2 == nil &&
			g1.Status == store.StatusDone && g1.DeliveredAt != nil &&
			g2.Status == store.StatusClaimed && mockAdapter.LaunchCalls(j2.ID) == 1
	})

	// Finish job 2 and verify both finish without ever exceeding concurrency 1.
	mockAdapter.FinishJob(j2.ID, 0)

	waitUntil(t, 3*time.Second, "job 2 done and delivered", func() bool {
		g2, err := st.GetJob(ctx, j2.ID)
		return err == nil && g2.Status == store.StatusDone && g2.DeliveredAt != nil
	})

	if mockAdapter.MaxConcurrentRunning() != 1 {
		t.Errorf("MaxConcurrentRunning = %d, want 1", mockAdapter.MaxConcurrentRunning())
	}
}

func TestE2EReconcilerResetsMissingClaimedJob(t *testing.T) {
	ctx := context.Background()

	// Part 1: Direct Reconciler verification that a Missing claimed job is reset back to queued.
	st, _ := openTestStore(t, "e2e-reconcile-reset")
	mockAdapter := adapter.NewMock(adapter.MockOptions{
		Default: adapter.MockBehavior{
			ForceMissing: true,
		},
	})
	recNotifier := &recordingNotifier{}

	job, _, err := st.CreateJob(ctx, store.CreateJobParams{
		RequestID: "missing-1",
		Title:     "Missing worker job",
	})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	claimed, ok, err := st.Claim(ctx, 1)
	if err != nil || !ok || claimed.ID != job.ID {
		t.Fatalf("Claim: ok=%v err=%v", ok, err)
	}

	// Advance clock beyond StaleClaimedAfter.
	fakeNow := time.Now().UTC().Add(30 * time.Minute)
	reconciler := NewReconciler(ReconcilerOptions{
		Store:             st,
		Adapter:           mockAdapter,
		Notifier:          recNotifier,
		StaleClaimedAfter: 10 * time.Minute,
		AttemptsCap:       3,
		Now:               func() time.Time { return fakeNow },
		Logger:            discardLogger(),
	})

	if err := reconciler.ReconcileOnce(ctx); err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}

	afterReset, err := st.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("GetJob after reset: %v", err)
	}
	if afterReset.Status != store.StatusQueued {
		t.Fatalf("Status after reset = %s, want queued", afterReset.Status)
	}
	if afterReset.ClaimedBy != "" || afterReset.ClaimedAt != nil {
		t.Errorf("claim fields not cleared: claimed_by=%q claimed_at=%v", afterReset.ClaimedBy, afterReset.ClaimedAt)
	}
	if afterReset.Attempts != 1 {
		t.Errorf("Attempts after reset = %d, want 1 (reset must preserve attempts)", afterReset.Attempts)
	}

	// Part 2: Full Runtime self-recovery: job is launched, dropped from adapter (Missing),
	// Reconciler resets it to queued, Dispatcher+Launcher re-claim it (attempts=2), and it finishes.
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	st2, stateDir2 := openTestStore(t, "e2e-reconcile-recover")
	mockAdapter2 := adapter.NewMock(adapter.MockOptions{
		Default: adapter.MockBehavior{
			StayRunning: true,
		},
	})

	rt, err := New(Options{
		Store:             st2,
		Adapter:           mockAdapter2,
		Evaluator:         evaluator.NewAlwaysPass(),
		InstanceID:        "e2e-reconcile-recover",
		StateDir:          stateDir2,
		MaxClaimed:        1,
		AttemptsCap:       3,
		DispatchInterval:  10 * time.Millisecond,
		ShepherdInterval:  10 * time.Millisecond,
		ReconcileInterval: 10 * time.Millisecond,
		StaleClaimedAfter: 15 * time.Millisecond,
		Logger:            discardLogger(),
	})
	if err != nil {
		t.Fatalf("runtime.New: %v", err)
	}
	if err := rt.Start(runCtx); err != nil {
		t.Fatalf("runtime.Start: %v", err)
	}
	defer rt.Stop()

	j, _, err := st2.CreateJob(runCtx, store.CreateJobParams{
		RequestID:          "recover-1",
		Title:              "Vanished then recovered",
		AcceptanceCriteria: "succeeds on retry",
	})
	if err != nil {
		t.Fatalf("CreateJob recover: %v", err)
	}

	waitUntil(t, 3*time.Second, "first launch observed", func() bool {
		return mockAdapter2.LaunchCalls(j.ID) == 1
	})

	// Simulate worker disappearance; next launch should exit cleanly.
	mockAdapter2.SetJobBehavior(j.ID, adapter.MockBehavior{ExitAfterPolls: 1})
	mockAdapter2.DropJob(j.ID)

	waitUntil(t, 3*time.Second, "recovered job done on attempt 2", func() bool {
		got, err := st2.GetJob(runCtx, j.ID)
		return err == nil && got.Status == store.StatusDone && got.DeliveredAt != nil && got.Attempts == 2
	})
}

func TestReconcilerUndeliveredOrphanDedupAndIdleSkip(t *testing.T) {
	ctx := context.Background()
	st, stateDir := openTestStore(t, "rec-unit")
	dbPath := filepath.Join(stateDir, "fleet.db")
	mockAdapter := adapter.NewMock(adapter.MockOptions{})
	recNotifier := &recordingNotifier{}

	reconciler := NewReconciler(ReconcilerOptions{
		Store:             st,
		Adapter:           mockAdapter,
		Notifier:          recNotifier,
		StaleClaimedAfter: 10 * time.Minute,
		AttemptsCap:       3,
		Logger:            discardLogger(),
	})
	shepherd := NewShepherd(ShepherdOptions{
		Store:     st,
		Adapter:   mockAdapter,
		Evaluator: evaluator.NewAlwaysPass(),
		Notifier:  recNotifier,
		Logger:    discardLogger(),
	})

	// 1. Idle skip: with zero active jobs, Shepherd and Reconciler skip adapter inspection.
	if err := shepherd.SweepOnce(ctx); err != nil {
		t.Fatalf("idle SweepOnce: %v", err)
	}
	if err := reconciler.ReconcileOnce(ctx); err != nil {
		t.Fatalf("idle ReconcileOnce: %v", err)
	}
	if mockAdapter.TotalStatusCalls() != 0 {
		t.Fatalf("idle TotalStatusCalls = %d, want 0", mockAdapter.TotalStatusCalls())
	}

	// 2. Undelivered done job is collected and delivered by Reconciler.
	jDone, _, err := st.CreateJob(ctx, store.CreateJobParams{RequestID: "undel-1", Title: "Undelivered done"})
	if err != nil {
		t.Fatalf("CreateJob undel: %v", err)
	}
	cDone, ok, err := st.Claim(ctx, 1)
	if err != nil || !ok {
		t.Fatalf("Claim undel: ok=%v err=%v", ok, err)
	}
	if _, err := st.Complete(ctx, cDone.ID, cDone.ClaimedBy, "ok"); err != nil {
		t.Fatalf("Complete undel: %v", err)
	}

	if err := reconciler.ReconcileOnce(ctx); err != nil {
		t.Fatalf("ReconcileOnce undelivered: %v", err)
	}
	gotDone, err := st.GetJob(ctx, jDone.ID)
	if err != nil {
		t.Fatalf("GetJob undel: %v", err)
	}
	if gotDone.DeliveredAt == nil {
		t.Fatalf("expected Reconciler to mark done job delivered")
	}
	if len(recNotifier.Deliveries()) != 1 {
		t.Fatalf("Deliveries len = %d, want 1", len(recNotifier.Deliveries()))
	}

	// 3. Orphan waiting job (parent terminal, child stuck in waiting) is reported ONCE (deduped).
	parent, _, err := st.CreateJob(ctx, store.CreateJobParams{RequestID: "orp-p", Title: "Orphan parent"})
	if err != nil {
		t.Fatalf("CreateJob parent: %v", err)
	}
	pid := parent.ID
	child, _, err := st.CreateJob(ctx, store.CreateJobParams{
		RequestID: "orp-c",
		Title:     "Orphan child",
		ParentID:  &pid,
	})
	if err != nil {
		t.Fatalf("CreateJob child: %v", err)
	}
	// Cancel parent (which cascades child to cancelled), then force child back to waiting directly
	// in SQLite to simulate a stuck/corrupted orphan waiting row.
	if _, err := st.Cancel(ctx, parent.ID, "operator cancel"); err != nil {
		t.Fatalf("Cancel parent: %v", err)
	}
	rawDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("raw sql.Open: %v", err)
	}
	defer rawDB.Close()
	if _, err := rawDB.ExecContext(ctx, `UPDATE jobs SET status = 'waiting' WHERE id = ?`, child.ID); err != nil {
		t.Fatalf("force child waiting: %v", err)
	}

	if err := reconciler.ReconcileOnce(ctx); err != nil {
		t.Fatalf("ReconcileOnce orphan pass 1: %v", err)
	}
	if err := reconciler.ReconcileOnce(ctx); err != nil {
		t.Fatalf("ReconcileOnce orphan pass 2: %v", err)
	}

	reports := recNotifier.Reports()
	if len(reports) != 1 {
		t.Fatalf("Reports len = %d, want 1 (duplicate should be suppressed): %+v", len(reports), reports)
	}
	if reports[0].Kind != "orphan_waiting" || reports[0].JobID != child.ID {
		t.Errorf("unexpected report: %+v", reports[0])
	}
}

func TestDispatcherSignalFormatAndRotation(t *testing.T) {
	ctx := context.Background()
	st, stateDir := openTestStore(t, "disp-inst")
	sigPath := filepath.Join(stateDir, "signal.jsonl")
	notifyCh := make(chan struct{}, 1)

	// Tiny maxBytes so the second event triggers rotation to signal.jsonl.1.
	disp := NewDispatcher(DispatcherOptions{
		Store:          st,
		NotifyCh:       notifyCh,
		SignalPath:     sigPath,
		SignalMaxBytes: 180,
		InstanceID:     "disp-inst",
		Interval:       time.Second,
		Logger:         discardLogger(),
	})

	for i := 1; i <= 3; i++ {
		if _, _, err := st.CreateJob(ctx, store.CreateJobParams{
			RequestID: fmt.Sprintf("sig-%d", i),
			Title:     fmt.Sprintf("Signal job %d", i),
		}); err != nil {
			t.Fatalf("CreateJob %d: %v", i, err)
		}
	}

	n, err := disp.DispatchOnce(ctx)
	if err != nil {
		t.Fatalf("DispatchOnce: %v", err)
	}
	if n != 3 {
		t.Fatalf("dispatched = %d, want 3", n)
	}

	// Outbox must now be empty.
	pending, err := st.ListPendingOutbox(ctx, 10)
	if err != nil {
		t.Fatalf("ListPendingOutbox: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending outbox = %d, want 0", len(pending))
	}

	// Notification channel must have received a wakeup.
	select {
	case <-notifyCh:
	default:
		t.Fatalf("expected notification on notifyCh")
	}

	// Rotated file must exist.
	rotatedPath := sigPath + ".1"
	if _, err := os.Stat(rotatedPath); err != nil {
		t.Fatalf("expected rotated file %q to exist: %v", rotatedPath, err)
	}

	// Validate 8-field JSONL schema on both files.
	requiredKeys := []string{"v", "event_id", "job_id", "env", "src", "hop", "ts", "event"}
	for _, p := range []string{rotatedPath, sigPath} {
		f, err := os.Open(p)
		if err != nil {
			t.Fatalf("open %q: %v", p, err)
		}
		sc := bufio.NewScanner(f)
		lines := 0
		for sc.Scan() {
			lines++
			var raw map[string]any
			if err := json.Unmarshal(sc.Bytes(), &raw); err != nil {
				_ = f.Close()
				t.Fatalf("invalid JSON in %q: %v", p, err)
			}
			if len(raw) != 8 {
				_ = f.Close()
				t.Fatalf("expected exactly 8 fields in %q, got %d: %v", p, len(raw), raw)
			}
			for _, k := range requiredKeys {
				if _, ok := raw[k]; !ok {
					_ = f.Close()
					t.Fatalf("missing field %q in %q: %v", k, p, raw)
				}
			}
		}
		_ = f.Close()
		if lines == 0 {
			t.Errorf("expected at least 1 JSONL line in %q", p)
		}
	}
}

func TestLauncherPrepareAndLaunchFailureRecorded(t *testing.T) {
	ctx := context.Background()
	st, _ := openTestStore(t, "launcher-err")
	mockAdapter := adapter.NewMock(adapter.MockOptions{})
	notifyCh := make(chan struct{}, 1)

	j1, _, err := st.CreateJob(ctx, store.CreateJobParams{RequestID: "lerr-1", Title: "Prepare fail"})
	if err != nil {
		t.Fatalf("CreateJob 1: %v", err)
	}
	j2, _, err := st.CreateJob(ctx, store.CreateJobParams{RequestID: "lerr-2", Title: "Launch fail"})
	if err != nil {
		t.Fatalf("CreateJob 2: %v", err)
	}

	mockAdapter.SetJobBehavior(j1.ID, adapter.MockBehavior{PrepareErr: errors.New("workdir disk full")})
	mockAdapter.SetJobBehavior(j2.ID, adapter.MockBehavior{LaunchErr: errors.New("spawn failed")})

	launcher := NewLauncher(LauncherOptions{
		Store:      st,
		Adapter:    mockAdapter,
		NotifyCh:   notifyCh,
		MaxClaimed: 2,
		Logger:     discardLogger(),
	})

	launched := launcher.DrainOnce(ctx)
	if launched != 0 {
		t.Errorf("launched = %d, want 0", launched)
	}

	g1, err := st.GetJob(ctx, j1.ID)
	if err != nil {
		t.Fatalf("GetJob 1: %v", err)
	}
	if g1.Status != store.StatusFailed || !strings.Contains(g1.LastError, "workdir disk full") {
		t.Errorf("j1 = status:%s last_error:%q", g1.Status, g1.LastError)
	}

	g2, err := st.GetJob(ctx, j2.ID)
	if err != nil {
		t.Fatalf("GetJob 2: %v", err)
	}
	if g2.Status != store.StatusFailed || !strings.Contains(g2.LastError, "spawn failed") {
		t.Errorf("j2 = status:%s last_error:%q", g2.Status, g2.LastError)
	}
}

type spyEvaluator struct {
	mu       sync.Mutex
	calls    int
	lastEv   adapter.Evidence
	delegate evaluator.Evaluator
}

func (s *spyEvaluator) Evaluate(ctx context.Context, job *store.Job, ev adapter.Evidence) (evaluator.Result, error) {
	s.mu.Lock()
	s.calls++
	s.lastEv = ev
	s.mu.Unlock()
	if s.delegate != nil {
		return s.delegate.Evaluate(ctx, job, ev)
	}
	return evaluator.Result{Passed: true}, nil
}

func (s *spyEvaluator) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *spyEvaluator) LastEvidence() adapter.Evidence {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastEv
}

func TestShepherdNonZeroExitFailsWithoutEvaluator(t *testing.T) {
	ctx := context.Background()
	st, _ := openTestStore(t, "shepherd-exit-code")
	recNotifier := &recordingNotifier{}
	spyEval := &spyEvaluator{delegate: evaluator.NewAlwaysPass()}
	mockAdapter := adapter.NewMock(adapter.MockOptions{})

	shepherd := NewShepherd(ShepherdOptions{
		Store:     st,
		Adapter:   mockAdapter,
		Evaluator: spyEval,
		Notifier:  recNotifier,
		Logger:    discardLogger(),
	})

	// 1. Non-zero exit code (2): must fail directly WITHOUT calling Evaluator,
	// recording exit code and agent.log tail in both last_error and result_text.
	jFail, _, err := st.CreateJob(ctx, store.CreateJobParams{
		RequestID:          "nonzero-1",
		Title:              "Agy bad flag failure",
		AcceptanceCriteria: "always pass criterion",
	})
	if err != nil {
		t.Fatalf("CreateJob jFail: %v", err)
	}
	claimedFail, ok, err := st.Claim(ctx, 2)
	if err != nil || !ok || claimedFail.ID != jFail.ID {
		t.Fatalf("Claim jFail: ok=%v err=%v", ok, err)
	}

	logTail := "flag needs an argument: -print\nUsage of agy:\n  --print  Run a single prompt"
	mockAdapter.SetJobBehavior(jFail.ID, adapter.MockBehavior{
		ExitAfterPolls: 1,
		ExitCode:       2,
		Evidence: &adapter.Evidence{
			FinalReport: logTail,
			ExitCode:    2,
		},
	})
	if _, err := mockAdapter.Launch(ctx, claimedFail); err != nil {
		t.Fatalf("Launch jFail: %v", err)
	}

	if err := shepherd.SweepOnce(ctx); err != nil {
		t.Fatalf("SweepOnce (non-zero): %v", err)
	}

	if spyEval.Calls() != 0 {
		t.Fatalf("Evaluator.Evaluate calls = %d, want 0 for non-zero exit code", spyEval.Calls())
	}

	gotFail, err := st.GetJob(ctx, jFail.ID)
	if err != nil {
		t.Fatalf("GetJob jFail: %v", err)
	}
	if gotFail.Status != store.StatusFailed {
		t.Fatalf("jFail status = %s, want failed", gotFail.Status)
	}
	if gotFail.DeliveredAt == nil {
		t.Errorf("jFail DeliveredAt = nil, want non-nil")
	}
	if !strings.Contains(gotFail.LastError, "2") || !strings.Contains(gotFail.LastError, "flag needs an argument: -print") {
		t.Errorf("jFail LastError missing exit code or log tail: %q", gotFail.LastError)
	}
	if !strings.Contains(gotFail.ResultText, "exit_code: 2") || !strings.Contains(gotFail.ResultText, logTail) {
		t.Errorf("jFail ResultText missing exit code or log tail:\n%s", gotFail.ResultText)
	}

	// 2. Zero exit code (0): must call Evaluator with Evidence.ExitCode == 0 and transition to done.
	jOK, _, err := st.CreateJob(ctx, store.CreateJobParams{
		RequestID:          "zero-1",
		Title:              "Normal exit 0 job",
		AcceptanceCriteria: "passes cleanly",
	})
	if err != nil {
		t.Fatalf("CreateJob jOK: %v", err)
	}
	claimedOK, ok, err := st.Claim(ctx, 2)
	if err != nil || !ok || claimedOK.ID != jOK.ID {
		t.Fatalf("Claim jOK: ok=%v err=%v", ok, err)
	}
	mockAdapter.SetJobBehavior(jOK.ID, adapter.MockBehavior{
		ExitAfterPolls: 1,
		ExitCode:       0,
	})
	if _, err := mockAdapter.Launch(ctx, claimedOK); err != nil {
		t.Fatalf("Launch jOK: %v", err)
	}

	if err := shepherd.SweepOnce(ctx); err != nil {
		t.Fatalf("SweepOnce (zero): %v", err)
	}

	if spyEval.Calls() != 1 {
		t.Fatalf("Evaluator.Evaluate calls = %d, want 1 for exit code 0", spyEval.Calls())
	}
	if spyEval.LastEvidence().ExitCode != 0 {
		t.Errorf("Evaluator received ExitCode = %d, want 0", spyEval.LastEvidence().ExitCode)
	}
	gotOK, err := st.GetJob(ctx, jOK.ID)
	if err != nil {
		t.Fatalf("GetJob jOK: %v", err)
	}
	if gotOK.Status != store.StatusDone {
		t.Fatalf("jOK status = %s, want done", gotOK.Status)
	}
}

func TestE2ENonZeroExitFailsJobAndCancelsChild(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	st, stateDir := openTestStore(t, "e2e-nonzero-exit")
	logTail := "flag needs an argument: -print\nfatal error on stderr"
	mockAdapter := adapter.NewMock(adapter.MockOptions{
		Default: adapter.MockBehavior{
			ExitAfterPolls: 1,
			ExitCode:       2,
			Evidence: &adapter.Evidence{
				FinalReport: logTail,
				ExitCode:    2,
			},
		},
	})

	// Even with AlwaysPassEvaluator, a non-zero exit code must mark the job failed.
	rt, err := New(Options{
		Store:             st,
		Adapter:           mockAdapter,
		Evaluator:         evaluator.NewAlwaysPass(),
		InstanceID:        "e2e-nonzero-exit",
		StateDir:          stateDir,
		MaxClaimed:        2,
		AttemptsCap:       3,
		DispatchInterval:  10 * time.Millisecond,
		ShepherdInterval:  10 * time.Millisecond,
		ReconcileInterval: 25 * time.Millisecond,
		StaleClaimedAfter: 5 * time.Second,
		Logger:            discardLogger(),
	})
	if err != nil {
		t.Fatalf("runtime.New: %v", err)
	}
	if err := rt.Start(ctx); err != nil {
		t.Fatalf("runtime.Start: %v", err)
	}
	defer rt.Stop()

	parent, _, err := st.CreateJob(ctx, store.CreateJobParams{
		RequestID:          "e2e-nz-parent",
		Title:              "Agy job exiting with 2",
		Agent:              "agy",
		Host:               "ubuntu",
		Workdir:            "/work/demo",
		AcceptanceCriteria: "should not pass on non-zero exit",
	})
	if err != nil {
		t.Fatalf("CreateJob parent: %v", err)
	}
	parentID := parent.ID
	child, _, err := st.CreateJob(ctx, store.CreateJobParams{
		RequestID: "e2e-nz-child",
		Title:     "Child depending on failing agy job",
		ParentID:  &parentID,
	})
	if err != nil {
		t.Fatalf("CreateJob child: %v", err)
	}

	waitUntil(t, 3*time.Second, "parent failed+delivered and child cancelled", func() bool {
		p, err1 := st.GetJob(ctx, parent.ID)
		c, err2 := st.GetJob(ctx, child.ID)
		return err1 == nil && err2 == nil &&
			p.Status == store.StatusFailed && p.DeliveredAt != nil &&
			c.Status == store.StatusCancelled
	})

	gotParent, err := st.GetJob(ctx, parent.ID)
	if err != nil {
		t.Fatalf("GetJob parent: %v", err)
	}
	if !strings.Contains(gotParent.LastError, "2") || !strings.Contains(gotParent.LastError, "flag needs an argument: -print") {
		t.Errorf("parent LastError = %q, want exit code 2 and log tail", gotParent.LastError)
	}
	if !strings.Contains(gotParent.ResultText, "exit_code: 2") || !strings.Contains(gotParent.ResultText, logTail) {
		t.Errorf("parent ResultText:\n%s", gotParent.ResultText)
	}
	if mockAdapter.LaunchCalls(child.ID) != 0 {
		t.Errorf("child LaunchCalls = %d, want 0", mockAdapter.LaunchCalls(child.ID))
	}

	// Verify delivery payload records status="failed" and last_error.
	deliveryPath := filepath.Join(stateDir, "deliveries", fmt.Sprintf("job-%d.json", parent.ID))
	data, err := os.ReadFile(deliveryPath)
	if err != nil {
		t.Fatalf("read delivery file: %v", err)
	}
	var payload DeliveryPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("unmarshal delivery file: %v", err)
	}
	if payload.Status != "failed" || !strings.Contains(payload.LastError, "2") {
		t.Errorf("unexpected delivery payload: %+v", payload)
	}
}

type scriptedEvaluator struct {
	mu      sync.Mutex
	calls   int
	handler func(call int) (evaluator.Result, error)
}

func (e *scriptedEvaluator) Evaluate(ctx context.Context, job *store.Job, ev adapter.Evidence) (evaluator.Result, error) {
	e.mu.Lock()
	e.calls++
	n := e.calls
	e.mu.Unlock()
	return e.handler(n)
}

func (e *scriptedEvaluator) Calls() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls
}

func TestShepherdEvaluationErrorRetriesAndFailsAtLimit(t *testing.T) {
	ctx := context.Background()
	st, _ := openTestStore(t, "shepherd-eval-retries")
	recNotifier := &recordingNotifier{}
	mockAdapter := adapter.NewMock(adapter.MockOptions{})

	evalErr := &evaluator.EvaluationUnavailableError{
		Kind:   evaluator.UnavailableAPIError,
		Reason: "decisions API returned HTTP 503",
	}
	scripted := &scriptedEvaluator{
		handler: func(call int) (evaluator.Result, error) {
			return evaluator.Result{}, evalErr
		},
	}

	shepherd := NewShepherd(ShepherdOptions{
		Store:          st,
		Adapter:        mockAdapter,
		Evaluator:      scripted,
		Notifier:       recNotifier,
		MaxEvalRetries: 3,
		Logger:         discardLogger(),
	})

	parent, _, err := st.CreateJob(ctx, store.CreateJobParams{
		RequestID:          "eval-retry-parent",
		Title:              "Job with failing evaluator",
		AcceptanceCriteria: "freeform criterion",
	})
	if err != nil {
		t.Fatalf("CreateJob parent: %v", err)
	}
	pid := parent.ID
	child, _, err := st.CreateJob(ctx, store.CreateJobParams{
		RequestID: "eval-retry-child",
		Title:     "Dependent child",
		ParentID:  &pid,
	})
	if err != nil {
		t.Fatalf("CreateJob child: %v", err)
	}

	claimed, ok, err := st.Claim(ctx, 2)
	if err != nil || !ok || claimed.ID != parent.ID {
		t.Fatalf("Claim: ok=%v err=%v", ok, err)
	}
	mockAdapter.SetJobBehavior(parent.ID, adapter.MockBehavior{
		ExitAfterPolls: 1,
		ExitCode:       0,
	})
	if _, err := mockAdapter.Launch(ctx, claimed); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	// Attempt 1 and 2: job stays claimed, not delivered, child stays waiting.
	for attempt := 1; attempt <= 2; attempt++ {
		if err := shepherd.SweepOnce(ctx); err != nil {
			t.Fatalf("SweepOnce attempt %d: %v", attempt, err)
		}
		got, err := st.GetJob(ctx, parent.ID)
		if err != nil {
			t.Fatalf("GetJob attempt %d: %v", attempt, err)
		}
		if got.Status != store.StatusClaimed {
			t.Fatalf("attempt %d: status = %s, want claimed", attempt, got.Status)
		}
		if got.DeliveredAt != nil {
			t.Fatalf("attempt %d: DeliveredAt should still be nil", attempt)
		}
	}
	if scripted.Calls() != 2 {
		t.Fatalf("evaluator calls after 2 sweeps = %d, want 2", scripted.Calls())
	}
	if len(recNotifier.Deliveries()) != 0 {
		t.Fatalf("deliveries before limit = %d, want 0", len(recNotifier.Deliveries()))
	}

	// Attempt 3 (reaches MaxEvalRetries=3): job transitions to failed with error reason, delivered, child cancelled.
	if err := shepherd.SweepOnce(ctx); err != nil {
		t.Fatalf("SweepOnce attempt 3: %v", err)
	}
	gotParent, err := st.GetJob(ctx, parent.ID)
	if err != nil {
		t.Fatalf("GetJob final: %v", err)
	}
	if gotParent.Status != store.StatusFailed {
		t.Fatalf("final status = %s, want failed", gotParent.Status)
	}
	if gotParent.DeliveredAt == nil {
		t.Errorf("final DeliveredAt = nil, want non-nil")
	}
	if !strings.Contains(gotParent.LastError, "3/3") || !strings.Contains(gotParent.LastError, "HTTP 503") {
		t.Errorf("LastError missing retry count or eval error reason: %q", gotParent.LastError)
	}
	if !strings.Contains(gotParent.ResultText, "evaluation_error:") || !strings.Contains(gotParent.ResultText, "HTTP 503") {
		t.Errorf("ResultText missing evaluation error details:\n%s", gotParent.ResultText)
	}
	gotChild, err := st.GetJob(ctx, child.ID)
	if err != nil {
		t.Fatalf("GetJob child: %v", err)
	}
	if gotChild.Status != store.StatusCancelled {
		t.Errorf("child status = %s, want cancelled", gotChild.Status)
	}
	if len(recNotifier.Deliveries()) != 1 {
		t.Errorf("deliveries = %d, want 1", len(recNotifier.Deliveries()))
	}
}

func TestShepherdEvaluationRetryCounterResetsOnSuccess(t *testing.T) {
	ctx := context.Background()
	st, _ := openTestStore(t, "shepherd-eval-reset")
	recNotifier := &recordingNotifier{}
	mockAdapter := adapter.NewMock(adapter.MockOptions{})

	scripted := &scriptedEvaluator{
		handler: func(call int) (evaluator.Result, error) {
			// Calls 1 & 2 error; call 3 succeeds with PASS.
			// Calls 4 & 5 error (on requeued run); call 6 succeeds with FAIL (verifies counter reset on both PASS and FAIL).
			switch call {
			case 1, 2, 4, 5:
				return evaluator.Result{}, &evaluator.EvaluationUnavailableError{
					Kind:   evaluator.UnavailableTimeout,
					Reason: "temporary timeout",
				}
			case 3:
				return evaluator.Result{
					Passed: true,
					Criteria: []evaluator.CriterionResult{
						{Criterion: "check ok", Passed: true, Evidence: "decision1 noul p=0.90 (>= 0.50)"},
					},
				}, nil
			default:
				return evaluator.Result{
					Passed: false,
					Criteria: []evaluator.CriterionResult{
						{Criterion: "check ok", Passed: false, Evidence: "decision1 noul p=0.10 (< 0.50)"},
					},
				}, nil
			}
		},
	}

	shepherd := NewShepherd(ShepherdOptions{
		Store:          st,
		Adapter:        mockAdapter,
		Evaluator:      scripted,
		Notifier:       recNotifier,
		MaxEvalRetries: 3,
		Logger:         discardLogger(),
	})

	// Job 1: 2 errors then PASS on 3rd sweep -> Done, counter cleared.
	j1, _, err := st.CreateJob(ctx, store.CreateJobParams{
		RequestID:          "eval-reset-1",
		Title:              "Recovers on 3rd attempt",
		AcceptanceCriteria: "check ok",
	})
	if err != nil {
		t.Fatalf("CreateJob j1: %v", err)
	}
	c1, ok, err := st.Claim(ctx, 2)
	if err != nil || !ok {
		t.Fatalf("Claim j1: ok=%v err=%v", ok, err)
	}
	mockAdapter.SetJobBehavior(j1.ID, adapter.MockBehavior{ExitAfterPolls: 1, ExitCode: 0})
	if _, err := mockAdapter.Launch(ctx, c1); err != nil {
		t.Fatalf("Launch j1: %v", err)
	}

	for i := 1; i <= 2; i++ {
		if err := shepherd.SweepOnce(ctx); err != nil {
			t.Fatalf("j1 SweepOnce %d: %v", i, err)
		}
		g, _ := st.GetJob(ctx, j1.ID)
		if g.Status != store.StatusClaimed {
			t.Fatalf("j1 after sweep %d: status=%s, want claimed", i, g.Status)
		}
	}
	if err := shepherd.SweepOnce(ctx); err != nil {
		t.Fatalf("j1 SweepOnce 3: %v", err)
	}
	g1, err := st.GetJob(ctx, j1.ID)
	if err != nil {
		t.Fatalf("GetJob j1: %v", err)
	}
	if g1.Status != store.StatusDone {
		t.Fatalf("j1 status = %s, want done", g1.Status)
	}
	shepherd.evalMu.Lock()
	_, stillTracked := shepherd.evalFailures[j1.ID]
	shepherd.evalMu.Unlock()
	if stillTracked {
		t.Errorf("expected evalFailures entry for job #%d to be cleared after success", j1.ID)
	}

	// Job 2: 2 errors then FAIL verdict on 3rd sweep -> Failed with verdict (not eval error), counter cleared.
	j2, _, err := st.CreateJob(ctx, store.CreateJobParams{
		RequestID:          "eval-reset-2",
		Title:              "Fails cleanly on 3rd attempt",
		AcceptanceCriteria: "check ok",
	})
	if err != nil {
		t.Fatalf("CreateJob j2: %v", err)
	}
	c2, ok, err := st.Claim(ctx, 2)
	if err != nil || !ok {
		t.Fatalf("Claim j2: ok=%v err=%v", ok, err)
	}
	mockAdapter.SetJobBehavior(j2.ID, adapter.MockBehavior{ExitAfterPolls: 1, ExitCode: 0})
	if _, err := mockAdapter.Launch(ctx, c2); err != nil {
		t.Fatalf("Launch j2: %v", err)
	}

	for i := 1; i <= 3; i++ {
		if err := shepherd.SweepOnce(ctx); err != nil {
			t.Fatalf("j2 SweepOnce %d: %v", i, err)
		}
	}
	g2, err := st.GetJob(ctx, j2.ID)
	if err != nil {
		t.Fatalf("GetJob j2: %v", err)
	}
	if g2.Status != store.StatusFailed {
		t.Fatalf("j2 status = %s, want failed", g2.Status)
	}
	if !strings.Contains(g2.ResultText, "decision1 noul p=0.10 (< 0.50)") {
		t.Errorf("j2 ResultText should reflect evaluator verdict, got:\n%s", g2.ResultText)
	}
	shepherd.evalMu.Lock()
	_, stillTracked2 := shepherd.evalFailures[j2.ID]
	shepherd.evalMu.Unlock()
	if stillTracked2 {
		t.Errorf("expected evalFailures entry for job #%d to be cleared after fail verdict", j2.ID)
	}
}

func TestGateCheckerAutoReleaseSoonestResetAndFailureRetention(t *testing.T) {
	ctx := context.Background()
	st, _ := openTestStore(t, "gate-checker-test")

	measured := time.Date(2026, 11, 23, 1, 0, 0, 0, time.UTC)
	now := measured.Add(10 * time.Minute)
	resetSooner := measured.Add(12 * time.Hour)
	resetLater := measured.Add(72 * time.Hour)
	intPtr := func(v int) *int { return &v }

	var (
		snapMu sync.Mutex
		snap   = &quota.Snapshot{
			MeasuredAt: measured,
			Pools: map[string]quota.PoolSnapshot{
				"claude": {WeeklyRemainingPercent: 0, WeeklyResetAt: &resetLater, ShortRemainingPercent: intPtr(50), MeasuredAt: measured},
				"codex":  {WeeklyRemainingPercent: 40, WeeklyResetAt: &resetSooner, ShortRemainingPercent: intPtr(0), MeasuredAt: measured},
			},
		}
	)

	checker := NewGateChecker(GateCheckerOptions{
		Store:      st,
		GateMode:   config.GateModeEnforce,
		StaleAfter: 45 * time.Minute,
		Pools: map[string]config.PoolConfig{
			"claude": {Provider: "claude-usage"},
			"codex":  {Provider: "codex-usage"},
		},
		Now:    func() time.Time { return now },
		Logger: discardLogger(),
		SnapshotLoader: func() (*quota.Snapshot, error) {
			snapMu.Lock()
			defer snapMu.Unlock()
			return snap, nil
		},
	})

	// 1. Hold a normal job with pool=claude, alt_pools=[codex].
	hOK, _, err := st.HoldJob(ctx, store.HoldJobParams{
		RequestID: "gc-rel-ok",
		Pool:      "claude",
		AltPools:  []string{"codex"},
		Payload: store.CreateJobParams{
			RequestID:    "gc-rel-ok",
			Title:        "Recoverable Held Job",
			Agent:        "claude",
			Host:         "ubuntu",
			Workdir:      "/work/repo",
			Instructions: "do it",
			Pool:         "claude",
			AltPools:     []string{"codex"},
		},
		Reason: "exhausted:weekly",
	})
	if err != nil {
		t.Fatalf("HoldJob hOK: %v", err)
	}

	// 2. Hold a job referencing a missing parent so CreateJob fails on release.
	missingParent := int64(999999)
	hFail, _, err := st.HoldJob(ctx, store.HoldJobParams{
		RequestID: "gc-rel-fail",
		Pool:      "claude",
		Payload: store.CreateJobParams{
			RequestID: "gc-rel-fail",
			Title:     "Failing Release Job",
			ParentID:  &missingParent,
			Pool:      "claude",
		},
		Reason: "exhausted:weekly",
	})
	if err != nil {
		t.Fatalf("HoldJob hFail: %v", err)
	}

	// While pools are exhausted, CheckOnce releases nothing.
	if err := checker.CheckOnce(ctx); err != nil {
		t.Fatalf("CheckOnce (exhausted): %v", err)
	}
	gotOK, _ := st.GetHeldJob(ctx, hOK.ID)
	if gotOK.Status != store.HeldStatusHeld {
		t.Fatalf("hOK status while exhausted = %s, want held", gotOK.Status)
	}

	// Swap snapshot: BOTH claude (reset +72h) and codex (reset +12h) recover -> codex must be picked (soonest weekly reset).
	snapMu.Lock()
	snap = &quota.Snapshot{
		MeasuredAt: measured,
		Pools: map[string]quota.PoolSnapshot{
			"claude": {WeeklyRemainingPercent: 80, WeeklyResetAt: &resetLater, ShortRemainingPercent: intPtr(90), MeasuredAt: measured},
			"codex":  {WeeklyRemainingPercent: 40, WeeklyResetAt: &resetSooner, ShortRemainingPercent: intPtr(70), MeasuredAt: measured},
		},
	}
	snapMu.Unlock()

	if err := checker.CheckOnce(ctx); err != nil {
		t.Fatalf("CheckOnce (recovered): %v", err)
	}

	// Verify hOK was released to a job using codex (soonest weekly reset).
	gotOK, err = st.GetHeldJob(ctx, hOK.ID)
	if err != nil {
		t.Fatalf("GetHeldJob hOK: %v", err)
	}
	if gotOK.Status != store.HeldStatusReleased || gotOK.ReleasedJobID == nil || gotOK.ReleasedPool != "codex" {
		t.Fatalf("hOK after recovery = %+v, want released pool=codex", gotOK)
	}
	createdJob, err := st.GetJob(ctx, *gotOK.ReleasedJobID)
	if err != nil {
		t.Fatalf("GetJob released: %v", err)
	}
	if createdJob.RequestID != "gc-rel-ok" || createdJob.Pool != "codex" || createdJob.Title != "Recoverable Held Job" {
		t.Errorf("createdJob = %+v", createdJob)
	}

	// Verify hFail remained held with last_error recorded and error_count=1.
	gotFail, err := st.GetHeldJob(ctx, hFail.ID)
	if err != nil {
		t.Fatalf("GetHeldJob hFail: %v", err)
	}
	if gotFail.Status != store.HeldStatusHeld || !strings.Contains(gotFail.LastError, "parent") || gotFail.ErrorCount != 1 {
		t.Fatalf("hFail after 1st failure = %+v", gotFail)
	}

	// Running CheckOnce again with the SAME failure does not inflate error_count (deduped).
	if err := checker.CheckOnce(ctx); err != nil {
		t.Fatalf("CheckOnce (2nd failure): %v", err)
	}
	gotFail2, err := st.GetHeldJob(ctx, hFail.ID)
	if err != nil {
		t.Fatalf("GetHeldJob hFail 2: %v", err)
	}
	if gotFail2.Status != store.HeldStatusHeld || gotFail2.ErrorCount != 1 || gotFail2.LastError != gotFail.LastError {
		t.Errorf("hFail after 2nd identical failure = %+v, want error_count=1", gotFail2)
	}
}

package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/adapter"
	"github.com/ypsilonmeister/quota-fleet/internal/admission"
	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/evaluator"
	"github.com/ypsilonmeister/quota-fleet/internal/quota"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

type requeueRuntimeEnv struct {
	t        *testing.T
	ctx      context.Context
	cfg      *config.Config
	st       *store.Store
	mock     *adapter.MockAdapter
	notify   *recordingNotifier
	launch   chan struct{}
	shepherd *Shepherd

	mu        sync.Mutex
	measured  time.Time
	exhausted bool
	snapErr   error
}

func newRequeueRuntimeEnv(t *testing.T, format string) *requeueRuntimeEnv {
	t.Helper()
	yes := true
	cfg := &config.Config{
		Quota: config.QuotaConfig{
			GateMode:    config.GateModeEnforce,
			UsageFormat: format,
			Cooldown:    config.CooldownConfig{Enabled: &yes},
			Requeue:     config.RequeueConfig{Enabled: &yes},
		},
		Pools: map[string]config.PoolConfig{
			"codex": {Provider: "codex-usage", AppliesTo: []config.PoolTarget{{Agent: "codex"}}},
			"spare": {Unmeasured: true},
		},
	}
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "fleet.db"), store.Options{Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	e := &requeueRuntimeEnv{
		t: t, ctx: ctx, cfg: cfg, st: st,
		mock:   adapter.NewMock(adapter.MockOptions{}),
		notify: &recordingNotifier{},
		launch: make(chan struct{}, 1),
		// Measured before any 429 in the test, so it never counts as cooldown recovery.
		measured: time.Now().UTC().Add(-10 * time.Minute),
	}
	e.shepherd = NewShepherd(ShepherdOptions{
		Config: cfg, Store: st, Adapter: e.mock, Notifier: e.notify, LauncherNotify: e.launch, Logger: discardLogger(),
		Evaluator: &scriptedEvaluator{handler: func(int) (evaluator.Result, error) { return evaluator.Result{Passed: true}, nil }},
	})
	adm := admission.New(admission.Options{
		Store: st, GateMode: config.GateModeEnforce, Pools: cfg.Pools, UsageFormat: format,
		StaleAfter: 45 * time.Minute, LoadSnapshot: e.loadSnapshot,
	})
	st.SetRequeueGate(NewRequeueGate(adm, discardLogger()))
	return e
}

func (e *requeueRuntimeEnv) loadSnapshot() (*quota.Snapshot, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.snapErr != nil {
		return nil, e.snapErr
	}
	now := e.measured
	reset := now.Add(24 * time.Hour)
	short, weekly := 90, 80
	if e.exhausted {
		weekly = 0
	}
	return &quota.Snapshot{
		MeasuredAt: now,
		Pools: map[string]quota.PoolSnapshot{
			"codex": {WeeklyRemainingPercent: weekly, WeeklyResetAt: &reset, ShortRemainingPercent: &short, MeasuredAt: now},
		},
	}, nil
}

// setSnapshot publishes a new measurement (taken now) with the given health.
func (e *requeueRuntimeEnv) setSnapshot(exhausted bool, err error) {
	e.mu.Lock()
	e.measured = time.Now().UTC()
	e.exhausted, e.snapErr = exhausted, err
	e.mu.Unlock()
}

func (e *requeueRuntimeEnv) claim() *store.Job {
	e.t.Helper()
	j, ok, err := e.st.Claim(e.ctx, 4)
	if err != nil {
		e.t.Fatal(err)
	}
	if !ok {
		return nil
	}
	return j
}

// runRateLimited launches j on the mock adapter and lets the Shepherd settle a 429 exit.
func (e *requeueRuntimeEnv) runRateLimited(j *store.Job) *store.Job {
	e.t.Helper()
	e.mock.SetJobBehavior(j.ID, adapter.MockBehavior{Evidence: &adapter.Evidence{ErrLogTail: "usage_limit_reached"}})
	if _, err := e.mock.Launch(e.ctx, j); err != nil {
		e.t.Fatal(err)
	}
	return e.shepherd.finishExitedJobFrom(e.ctx, j, adapter.RunState{ExitCode: 1}, "shepherd")
}

func TestShepherdRequeuesRateLimitedJobWithoutDelivery(t *testing.T) {
	e := newRequeueRuntimeEnv(t, config.UsageFormatLegacy)
	parent, _, err := e.st.CreateJob(e.ctx, store.CreateJobParams{RequestID: "parent", Title: "parent", Agent: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	child, _, err := e.st.CreateJob(e.ctx, store.CreateJobParams{RequestID: "child", Title: "child", Agent: "codex", ParentID: &parent.ID})
	if err != nil {
		t.Fatal(err)
	}
	j := e.claim()
	if j == nil || j.ID != parent.ID {
		t.Fatalf("claim = %+v", j)
	}
	got := e.runRateLimited(j)
	if got == nil || got.Status != store.StatusQueued || got.RateLimitRetries != 1 || got.Attempts != 0 {
		t.Fatalf("finish = %+v", got)
	}
	if d := e.notify.Deliveries(); len(d) != 0 {
		t.Fatalf("requeued job was delivered: %+v", d)
	}
	kinds := map[string]bool{}
	for _, r := range e.notify.Reports() {
		kinds[r.Kind] = true
	}
	if !kinds["pool_cooldown"] || !kinds["job_rate_limit_requeued"] {
		t.Fatalf("reports = %+v", e.notify.Reports())
	}
	select {
	case <-e.launch:
	default:
		t.Fatal("launcher was not woken after the slot freed")
	}
	if c, err := e.st.GetJob(e.ctx, child.ID); err != nil || c.Status != store.StatusWaiting {
		t.Fatalf("child = %+v %v", c, err)
	}
	if again := e.claim(); again != nil {
		t.Fatalf("claimed %d during cooldown", again.ID)
	}
}

func TestShepherdRequeueRespectsOnRateLimitFail(t *testing.T) {
	e := newRequeueRuntimeEnv(t, config.UsageFormatLegacy)
	if _, _, err := e.st.CreateJob(e.ctx, store.CreateJobParams{RequestID: "a", Title: "a", Agent: "codex", OnRateLimit: store.OnRateLimitFail}); err != nil {
		t.Fatal(err)
	}
	got := e.runRateLimited(e.claim())
	if got == nil || got.Status != store.StatusFailed {
		t.Fatalf("finish = %+v", got)
	}
	if d := e.notify.Deliveries(); len(d) != 1 {
		t.Fatalf("failed job must be delivered once: %+v", d)
	}
}

func TestRequeueGateHoldsExhaustedPoolUntilReset(t *testing.T) {
	e := newRequeueRuntimeEnv(t, config.UsageFormatLegacy)
	job, _, err := e.st.CreateJob(e.ctx, store.CreateJobParams{RequestID: "a", Title: "a", Agent: "codex", Pool: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	e.runRateLimited(e.claim())
	// The cooldown ends early (manual clear), but the snapshot now says the pool is empty.
	if _, err := e.st.ClearCooldown(e.ctx, "codex"); err != nil {
		t.Fatal(err)
	}
	e.setSnapshot(true, nil)
	if got := e.claim(); got != nil {
		t.Fatalf("claimed %d on an exhausted pool", got.ID)
	}
	if got, err := e.st.GetJob(e.ctx, job.ID); err != nil || got.RateLimitRetries != 1 {
		t.Fatalf("retry budget consumed while held: %+v %v", got, err)
	}
	e.setSnapshot(false, nil)
	if got := e.claim(); got == nil || got.ID != job.ID || got.Attempts != 1 {
		t.Fatalf("claim after reset = %+v", got)
	}
}

func TestRequeueGateFailsClosedOnSnapshotErrorV1(t *testing.T) {
	e := newRequeueRuntimeEnv(t, config.UsageFormatV1)
	job, _, err := e.st.CreateJob(e.ctx, store.CreateJobParams{RequestID: "a", Title: "a", Agent: "codex", Pool: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	e.runRateLimited(e.claim())
	if _, err := e.st.ClearCooldown(e.ctx, "codex"); err != nil {
		t.Fatal(err)
	}
	e.setSnapshot(false, errors.New("usage file unreadable"))
	if got := e.claim(); got != nil {
		t.Fatalf("claimed %d despite snapshot error", got.ID)
	}
	if _, ok, err := e.st.ClaimByID(e.ctx, job.ID, 4); ok || err != nil {
		t.Fatalf("ClaimByID ok=%v err=%v", ok, err)
	}
}

func TestRequeueGateFailsOverToDeclaredAltPool(t *testing.T) {
	e := newRequeueRuntimeEnv(t, config.UsageFormatLegacy)
	job, _, err := e.st.CreateJob(e.ctx, store.CreateJobParams{RequestID: "a", Title: "a", Agent: "codex", Model: "gpt-5", Pool: "codex", AltPools: []string{"spare"}})
	if err != nil {
		t.Fatal(err)
	}
	e.runRateLimited(e.claim())
	// codex is cooling down; the declared unmeasured alt pool takes the job at once.
	got := e.claim()
	if got == nil || got.ID != job.ID {
		t.Fatalf("failover claim = %+v", got)
	}
	if got.Pool != "spare" || got.Agent != "codex" || got.Model != "gpt-5" {
		t.Fatalf("failover changed pool/agent/model: pool=%s agent=%s model=%s", got.Pool, got.Agent, got.Model)
	}
	if len(got.AltPools) != 1 || got.AltPools[0] != "codex" {
		t.Fatalf("original pool must stay a candidate: %v", got.AltPools)
	}
	decs, err := e.st.ListGateDecisions(e.ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range decs {
		if d.JobID == job.ID && d.SelectedPool == "spare" && strings.HasPrefix(d.Reason, "rate_limit_requeue_failover") {
			found = true
		}
	}
	if !found {
		t.Fatalf("failover decision missing: %+v", decs)
	}
}

func TestReconcilerReportsRequeueAndStall(t *testing.T) {
	e := newRequeueRuntimeEnv(t, config.UsageFormatLegacy)
	if _, _, err := e.st.CreateJob(e.ctx, store.CreateJobParams{RequestID: "a", Title: "a", Agent: "codex"}); err != nil {
		t.Fatal(err)
	}
	j := e.claim()
	e.mock.SetJobBehavior(j.ID, adapter.MockBehavior{ExitCode: 1, Evidence: &adapter.Evidence{ErrLogTail: "usage_limit_reached"}})
	if _, err := e.mock.Launch(e.ctx, j); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Add(time.Hour)
	r := NewReconciler(ReconcilerOptions{
		Store: e.st, Adapter: e.mock, Shepherd: e.shepherd, Notifier: e.notify,
		StaleClaimedAfter: time.Minute, Now: func() time.Time { return now }, Logger: discardLogger(),
		Evaluator: &scriptedEvaluator{handler: func(int) (evaluator.Result, error) { return evaluator.Result{Passed: true}, nil }},
	})
	if err := r.ReconcileOnce(e.ctx); err != nil {
		t.Fatal(err)
	}
	kinds := map[string]int{}
	for _, rep := range e.notify.Reports() {
		kinds[rep.Kind]++
	}
	if kinds["stale_claimed_requeued"] != 1 || kinds["stale_claimed_recovered"] != 0 {
		t.Fatalf("reports = %v", kinds)
	}
	now = now.Add(7 * time.Hour)
	if err := r.ReconcileOnce(e.ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.ReconcileOnce(e.ctx); err != nil {
		t.Fatal(err)
	}
	kinds = map[string]int{}
	for _, rep := range e.notify.Reports() {
		kinds[rep.Kind]++
	}
	if kinds["job_rate_limit_requeue_stalled"] != 1 {
		t.Fatalf("stall reports = %v", kinds)
	}
}

// newRuntimeForRequeue builds the Runtime the way the daemon does, so the Claim-time Gate
// comes from runtime.New and not from the test helper.
func (e *requeueRuntimeEnv) newRuntime() {
	e.t.Helper()
	e.st.SetRequeueGate(nil)
	if _, err := New(Options{
		Store: e.st, Adapter: e.mock, Evaluator: evaluator.NewAlwaysPass(), Config: e.cfg,
		StateDir: e.t.TempDir(), GateMode: config.GateModeEnforce, QuotaUsageFormat: e.cfg.Quota.UsageFormat,
		QuotaStaleAfter: 45 * time.Minute, Pools: e.cfg.Pools, SnapshotLoader: e.loadSnapshot,
		Logger: discardLogger(),
	}); err != nil {
		e.t.Fatal(err)
	}
}

// Review P1: a job's own on_rate_limit=requeue must not enable requeue when the operator
// did not opt in, because that path would run outside the configured requeue guard rails.
func TestOnRateLimitRequeueNeedsConfigOptIn(t *testing.T) {
	e := newRequeueRuntimeEnv(t, config.UsageFormatV1)
	no := false
	e.cfg.Quota.Requeue.Enabled = &no
	e.newRuntime()
	job, _, err := e.st.CreateJob(e.ctx, store.CreateJobParams{RequestID: "a", Title: "a", Agent: "codex", Pool: "codex", OnRateLimit: store.OnRateLimitRequeue})
	if err != nil {
		t.Fatal(err)
	}
	if got := e.runRateLimited(e.claim()); got.Status != store.StatusFailed || got.RateLimitRetries != 0 {
		t.Fatalf("job %d: status=%s retries=%d, want failed without requeue", job.ID, got.Status, got.RateLimitRetries)
	}
}

// Review P1: a job requeued while requeue was enabled stays gated after a restart with
// requeue switched off; runtime.New must install the Gate regardless of the requeue config.
func TestRuntimeNewGatesRequeuedJobsAfterRequeueDisabled(t *testing.T) {
	e := newRequeueRuntimeEnv(t, config.UsageFormatV1)
	job, _, err := e.st.CreateJob(e.ctx, store.CreateJobParams{RequestID: "a", Title: "a", Agent: "codex", Pool: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if got := e.runRateLimited(e.claim()); got.Status != store.StatusQueued {
		t.Fatalf("not requeued: %+v", got)
	}

	no := false
	e.cfg.Quota.Requeue.Enabled = &no // restart with requeue disabled
	e.newRuntime()
	if _, err := e.st.ClearCooldown(e.ctx, "codex"); err != nil {
		t.Fatal(err)
	}
	e.setSnapshot(false, errors.New("usage file unreadable"))
	if got := e.claim(); got != nil {
		t.Fatalf("Claim took %d despite a v1 snapshot error", got.ID)
	}
	if got, ok, err := e.st.ClaimByID(e.ctx, job.ID, 4); ok || err != nil {
		t.Fatalf("ClaimByID despite a v1 snapshot error: job=%+v ok=%v err=%v", got, ok, err)
	}

	// The Gate installed by runtime.New lets it through once the snapshot is healthy.
	e.setSnapshot(false, nil)
	if got, ok, err := e.st.ClaimByID(e.ctx, job.ID, 4); !ok || err != nil || got.ID != job.ID {
		t.Fatalf("ClaimByID after recovery: job=%+v ok=%v err=%v", got, ok, err)
	}
}

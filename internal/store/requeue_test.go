package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/config"
)

type requeueEnv struct {
	t   *testing.T
	ctx context.Context
	st  *Store
	cfg *config.Config
}

func newRequeueEnv(t *testing.T, requeue bool, maxRetries int) *requeueEnv {
	t.Helper()
	yes := true
	cfg := &config.Config{
		Quota: config.QuotaConfig{
			GateMode: config.GateModeEnforce,
			Cooldown: config.CooldownConfig{Enabled: &yes},
			Requeue:  config.RequeueConfig{Enabled: &requeue, MaxRetries: &maxRetries},
		},
		Pools: map[string]config.PoolConfig{
			"p": {AppliesTo: []config.PoolTarget{{Agent: "codex"}}},
			"q": {},
		},
	}
	st, err := Open(context.Background(), filepath.Join(t.TempDir(), "fleet.db"), Options{Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	// runtime.New always installs the Gate; these tests exercise the store mechanics
	// behind a Gate that lets every requeued job through.
	st.SetRequeueGate(allowAllGate)
	return &requeueEnv{t: t, ctx: context.Background(), st: st, cfg: cfg}
}

func allowAllGate(context.Context, *Job, string, bool) RequeueGateVerdict {
	return RequeueGateVerdict{Allow: true}
}

func (e *requeueEnv) add(p CreateJobParams) *Job {
	e.t.Helper()
	if p.Title == "" {
		p.Title = p.RequestID
	}
	if p.Agent == "" {
		p.Agent = "codex"
	}
	j, _, err := e.st.CreateJob(e.ctx, p)
	if err != nil {
		e.t.Fatal(err)
	}
	return j
}

func (e *requeueEnv) claim() *Job {
	e.t.Helper()
	j, ok, err := e.st.Claim(e.ctx, 8)
	if err != nil {
		e.t.Fatal(err)
	}
	if !ok {
		return nil
	}
	return j
}

func (e *requeueEnv) job(id int64) *Job {
	e.t.Helper()
	j, err := e.st.GetJob(e.ctx, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return j
}

// expire ends the pool cooldown and the retry_after wait, as if the backoff elapsed.
func (e *requeueEnv) expire() {
	e.t.Helper()
	past := formatTime(time.Now().Add(-time.Second))
	if _, err := e.st.writeDB.ExecContext(e.ctx, `UPDATE pool_cooldowns SET active_until=?`, past); err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.st.writeDB.ExecContext(e.ctx, `UPDATE jobs SET retry_after=? WHERE retry_after IS NOT NULL`, past); err != nil {
		e.t.Fatal(err)
	}
}

func (e *requeueEnv) hit(j *Job) *FailResult {
	e.t.Helper()
	out, err := e.st.RequeueOnRateLimit(e.ctx, j.ID, j.ClaimedBy, "agent exited with code 1: usage_limit_reached", "overall: FAIL", "codex:quota_limit")
	if err != nil {
		e.t.Fatal(err)
	}
	return out
}

func TestRequeueOnRateLimitKeepsChildrenAndAttempts(t *testing.T) {
	e := newRequeueEnv(t, true, 3)
	parent := e.add(CreateJobParams{RequestID: "parent", Pool: "p"})
	child := e.add(CreateJobParams{RequestID: "child", Pool: "p", ParentID: &parent.ID})
	if child.Status != StatusWaiting {
		t.Fatalf("child status %s", child.Status)
	}

	for retry := 1; retry <= 3; retry++ {
		c := e.claim()
		if c == nil || c.ID != parent.ID || c.Attempts != 1 {
			t.Fatalf("retry %d: claim=%+v", retry, c)
		}
		out := e.hit(c)
		if !out.Requeued || out.Job.Status != StatusQueued {
			t.Fatalf("retry %d: not requeued: %+v", retry, out)
		}
		j := e.job(parent.ID)
		if j.Attempts != 0 || j.RateLimitRetries != retry || j.ClaimedBy != "" || j.ClaimedAt != nil {
			t.Fatalf("retry %d: counters %+v", retry, j)
		}
		if !strings.Contains(j.LastError, "requeued (retry ") || !strings.Contains(j.LastError, "cooldown") {
			t.Fatalf("retry %d: last_error %q", retry, j.LastError)
		}
		cds, err := e.st.ListCooldowns(e.ctx)
		if err != nil || len(cds) != 1 {
			t.Fatalf("cooldowns %v %v", cds, err)
		}
		if j.RetryAfter == nil || !j.RetryAfter.Equal(cds[0].ActiveUntil) {
			t.Fatalf("retry_after %v does not mirror cooldown %v", j.RetryAfter, cds[0].ActiveUntil)
		}
		if cds[0].ConsecutiveHits != retry {
			t.Fatalf("cooldown hits %d want %d", cds[0].ConsecutiveHits, retry)
		}
		if got := e.job(child.ID); got.Status != StatusWaiting {
			t.Fatalf("retry %d: child became %s", retry, got.Status)
		}
		if again := e.claim(); again != nil {
			t.Fatalf("retry %d: claimed %d during cooldown", retry, again.ID)
		}
		e.expire()
	}

	// Fourth rate limit: budget spent → failed and the child is cancelled.
	c := e.claim()
	if c == nil || c.ID != parent.ID {
		t.Fatalf("final claim %+v", c)
	}
	out := e.hit(c)
	if out.Requeued || out.Job.Status != StatusFailed {
		t.Fatalf("exhausted: %+v", out)
	}
	if !strings.Contains(out.Job.LastError, "rate limit retries exhausted (3/3)") {
		t.Fatalf("last_error %q", out.Job.LastError)
	}
	if len(out.CancelledChildIDs) != 1 || out.CancelledChildIDs[0] != child.ID || e.job(child.ID).Status != StatusCancelled {
		t.Fatalf("child not cancelled: %+v", out.CancelledChildIDs)
	}
}

func TestRequeueOnRateLimitPolicyFallsBackToFail(t *testing.T) {
	t.Run("disabled", func(t *testing.T) {
		e := newRequeueEnv(t, false, 3)
		j := e.add(CreateJobParams{RequestID: "a", Pool: "p"})
		child := e.add(CreateJobParams{RequestID: "a-child", Pool: "p", ParentID: &j.ID})
		out := e.hit(e.claim())
		if out.Requeued || out.Job.Status != StatusFailed || e.job(child.ID).Status != StatusCancelled {
			t.Fatalf("disabled requeue must fail as before: %+v", out)
		}
		if out.Cooldown == nil {
			t.Fatal("cooldown must still be recorded")
		}
	})
	t.Run("job opts out", func(t *testing.T) {
		e := newRequeueEnv(t, true, 3)
		e.add(CreateJobParams{RequestID: "a", Pool: "p", OnRateLimit: OnRateLimitFail})
		if out := e.hit(e.claim()); out.Requeued || out.Job.Status != StatusFailed {
			t.Fatalf("on_rate_limit=fail must fail: %+v", out)
		}
	})
	t.Run("job requeue alone does not opt in", func(t *testing.T) {
		// The opt-in is the configuration; a job cannot enable requeue on its own.
		e := newRequeueEnv(t, false, 3)
		e.add(CreateJobParams{RequestID: "a", Pool: "p", OnRateLimit: OnRateLimitRequeue})
		if out := e.hit(e.claim()); out.Requeued || out.Job.Status != StatusFailed {
			t.Fatalf("on_rate_limit=requeue without quota.requeue.enabled must fail: %+v", out)
		}
	})
	t.Run("job requeue without cooldown", func(t *testing.T) {
		e := newRequeueEnv(t, true, 3)
		no := false
		e.cfg.Quota.Cooldown.Enabled = &no
		e.add(CreateJobParams{RequestID: "a", Pool: "p", OnRateLimit: OnRateLimitRequeue})
		if out := e.hit(e.claim()); out.Requeued {
			t.Fatalf("requeue without cooldown: %+v", out)
		}
	})
	t.Run("max retries zero", func(t *testing.T) {
		e := newRequeueEnv(t, true, 0)
		e.add(CreateJobParams{RequestID: "a", Pool: "p"})
		if out := e.hit(e.claim()); out.Requeued {
			t.Fatalf("max_retries 0 must disable requeue: %+v", out)
		}
	})
	t.Run("gate not enforce", func(t *testing.T) {
		e := newRequeueEnv(t, true, 3)
		e.cfg.Quota.GateMode = config.GateModeDryRun
		e.add(CreateJobParams{RequestID: "a", Pool: "p"})
		if out := e.hit(e.claim()); out.Requeued {
			t.Fatalf("dry-run must not requeue: %+v", out)
		}
	})
	t.Run("token mismatch", func(t *testing.T) {
		e := newRequeueEnv(t, true, 3)
		e.add(CreateJobParams{RequestID: "a", Pool: "p"})
		j := e.claim()
		_, err := e.st.RequeueOnRateLimit(e.ctx, j.ID, "wrong", "r", "r", "codex:quota_limit")
		if !errors.Is(err, ErrTokenMismatch) && !errors.Is(err, ErrConflict) {
			t.Fatalf("err = %v", err)
		}
		if e.job(j.ID).Status != StatusClaimed {
			t.Fatal("job changed despite token mismatch")
		}
	})
}

func TestOnRateLimitValidation(t *testing.T) {
	e := newRequeueEnv(t, true, 3)
	if _, _, err := e.st.CreateJob(e.ctx, CreateJobParams{RequestID: "bad", Title: "bad", OnRateLimit: "retry"}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("err = %v", err)
	}
	j := e.add(CreateJobParams{RequestID: "ok", OnRateLimit: " fail "})
	if j.OnRateLimit != OnRateLimitFail {
		t.Fatalf("on_rate_limit %q", j.OnRateLimit)
	}
}

// createChainJob registers a chain run whose first step job is returned.
func (e *requeueEnv) createChainJob(step string) *Job {
	e.t.Helper()
	run, _, err := e.st.CreateChainRun(e.ctx, CreateChainRunParams{
		RequestID: "run-" + step, Owner: "owner", Title: "chain", Scope: "o/r#1",
		DefinitionJSON: "{}", DefinitionHash: "h-" + step, DefinitionVersion: 1,
		MaxRounds: 1, MaxSteps: 3, FirstStep: ChainStepImplement,
		First: ChainStepWrite{Params: CreateJobParams{Title: "step", Agent: "codex", Pool: "p"}},
	})
	if err != nil {
		e.t.Fatal(err)
	}
	if step != ChainStepImplement {
		if _, err := e.st.writeDB.ExecContext(e.ctx, `UPDATE chain_steps SET step=? WHERE run_id=?`, step, run.ID); err != nil {
			e.t.Fatal(err)
		}
	}
	j := e.claim()
	if j == nil || j.ChainRunID != run.ID {
		e.t.Fatalf("chain job not claimed: %+v", j)
	}
	return j
}

func TestRequeueOnRateLimitChainSteps(t *testing.T) {
	t.Run("implement is requeued", func(t *testing.T) {
		e := newRequeueEnv(t, true, 3)
		if out := e.hit(e.createChainJob(ChainStepImplement)); !out.Requeued {
			t.Fatalf("implement step must be requeued: %+v", out)
		}
	})
	t.Run("publish fails by default", func(t *testing.T) {
		e := newRequeueEnv(t, true, 3)
		if out := e.hit(e.createChainJob(ChainStepPublish)); out.Requeued || out.Job.Status != StatusFailed {
			t.Fatalf("publish step must not be requeued by default: %+v", out)
		}
	})
	t.Run("publish requeued when the job asks", func(t *testing.T) {
		e := newRequeueEnv(t, true, 3)
		j := e.createChainJob(ChainStepPublish)
		if _, err := e.st.writeDB.ExecContext(e.ctx, `UPDATE jobs SET on_rate_limit='requeue' WHERE id=?`, j.ID); err != nil {
			t.Fatal(err)
		}
		if out := e.hit(j); !out.Requeued {
			t.Fatalf("explicit requeue on publish: %+v", out)
		}
	})
	t.Run("terminal run cancels", func(t *testing.T) {
		e := newRequeueEnv(t, true, 3)
		j := e.createChainJob(ChainStepImplement)
		if _, err := e.st.writeDB.ExecContext(e.ctx, `UPDATE chain_runs SET status='cancelled' WHERE id=?`, j.ChainRunID); err != nil {
			t.Fatal(err)
		}
		out := e.hit(j)
		if out.Requeued || out.Job.Status != StatusCancelled {
			t.Fatalf("job of a terminal run must be cancelled: %+v", out)
		}
		if cds, _ := e.st.ListCooldowns(e.ctx); len(cds) != 1 {
			t.Fatalf("cooldown must still be recorded: %v", cds)
		}
	})
}

func TestRequeuedJobDoesNotBlockOtherPools(t *testing.T) {
	e := newRequeueEnv(t, true, 3)
	first := e.add(CreateJobParams{RequestID: "first", Pool: "p"})
	e.hit(e.claim())
	other := e.add(CreateJobParams{RequestID: "other", Pool: "q", Agent: "claude"})
	got := e.claim()
	if got == nil || got.ID != other.ID {
		t.Fatalf("claim = %+v, want job %d (requeued job %d must not block)", got, other.ID, first.ID)
	}
}

func TestRetryAfterAloneBlocksClaim(t *testing.T) {
	// Even with the pool cooldown cleared out of band, retry_after holds the job.
	e := newRequeueEnv(t, true, 3)
	j := e.add(CreateJobParams{RequestID: "a", Pool: "p"})
	e.hit(e.claim())
	if _, err := e.st.writeDB.ExecContext(e.ctx, `DELETE FROM pool_cooldowns`); err != nil {
		t.Fatal(err)
	}
	if got := e.claim(); got != nil {
		t.Fatalf("claimed %d before retry_after", got.ID)
	}
	if _, err := e.st.writeDB.ExecContext(e.ctx, `UPDATE jobs SET retry_after=? WHERE id=?`, formatTime(time.Now().Add(-time.Millisecond)), j.ID); err != nil {
		t.Fatal(err)
	}
	if got := e.claim(); got == nil || got.ID != j.ID {
		t.Fatalf("claim after retry_after = %+v", got)
	}
}

func TestClearCooldownClearsRetryAfter(t *testing.T) {
	e := newRequeueEnv(t, true, 3)
	j := e.add(CreateJobParams{RequestID: "a", Pool: "p"})
	e.hit(e.claim())
	if e.job(j.ID).RetryAfter == nil {
		t.Fatal("retry_after not set")
	}
	if cleared, err := e.st.ClearCooldown(e.ctx, "p"); err != nil || !cleared {
		t.Fatalf("ClearCooldown %v %v", cleared, err)
	}
	if got := e.job(j.ID); got.RetryAfter != nil {
		t.Fatalf("retry_after kept after clear: %v", got.RetryAfter)
	}
	if got := e.claim(); got == nil || got.ID != j.ID {
		t.Fatalf("claim after clear = %+v", got)
	}
}

func TestManualRequeueResetsRateLimitRetries(t *testing.T) {
	e := newRequeueEnv(t, true, 1)
	j := e.add(CreateJobParams{RequestID: "a", Pool: "p"})
	e.hit(e.claim())
	e.expire()
	if out := e.hit(e.claim()); out.Job.Status != StatusFailed {
		t.Fatalf("expected exhausted failure: %+v", out)
	}
	got, err := e.st.Requeue(e.ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RateLimitRetries != 0 || got.RetryAfter != nil || got.Attempts != 0 {
		t.Fatalf("manual requeue did not reset counters: %+v", got)
	}
}

func TestRequeueGateVerdicts(t *testing.T) {
	t.Run("hold keeps the job queued", func(t *testing.T) {
		e := newRequeueEnv(t, true, 3)
		j := e.add(CreateJobParams{RequestID: "a", Pool: "p"})
		e.hit(e.claim())
		e.expire()
		calls := 0
		e.st.SetRequeueGate(func(ctx context.Context, job *Job, pool string, failover bool) RequeueGateVerdict {
			calls++
			if job.ID != j.ID || pool != "p" || !failover {
				t.Errorf("gate called with job=%d pool=%s failover=%v", job.ID, pool, failover)
			}
			return RequeueGateVerdict{}
		})
		if got := e.claim(); got != nil {
			t.Fatalf("claimed %d although the gate holds it", got.ID)
		}
		if _, ok, err := e.st.ClaimByID(e.ctx, j.ID, 8); ok || err != nil {
			t.Fatalf("ClaimByID ok=%v err=%v", ok, err)
		}
		if calls != 2 {
			t.Fatalf("gate calls = %d", calls)
		}
		e.st.SetRequeueGate(func(context.Context, *Job, string, bool) RequeueGateVerdict {
			return RequeueGateVerdict{Allow: true}
		})
		if got := e.claim(); got == nil || got.ID != j.ID || got.Pool != "p" {
			t.Fatalf("claim after gate pass = %+v", got)
		}
	})
	t.Run("pass still respects retry_after on the same pool", func(t *testing.T) {
		e := newRequeueEnv(t, true, 3)
		e.add(CreateJobParams{RequestID: "a", Pool: "p"})
		e.hit(e.claim())
		e.st.SetRequeueGate(func(context.Context, *Job, string, bool) RequeueGateVerdict {
			return RequeueGateVerdict{Allow: true}
		})
		if got := e.claim(); got != nil {
			t.Fatalf("claimed %d during retry_after", got.ID)
		}
	})
	t.Run("failover to declared alt pool", func(t *testing.T) {
		e := newRequeueEnv(t, true, 3)
		j := e.add(CreateJobParams{RequestID: "a", Pool: "p", AltPools: []string{"q"}})
		e.hit(e.claim())
		e.st.SetRequeueGate(func(ctx context.Context, job *Job, pool string, failover bool) RequeueGateVerdict {
			return RequeueGateVerdict{Allow: true, SelectedPool: "q", Gate: &GateDecision{
				Mode: config.GateModeEnforce, RequestedPool: "p", AltPools: []string{"q"},
				Decision: "would_pass", SelectedPool: "q", Reason: "failover",
			}}
		})
		got := e.claim()
		if got == nil || got.ID != j.ID {
			t.Fatalf("failover claim = %+v", got)
		}
		if got.Pool != "q" || len(got.AltPools) != 1 || got.AltPools[0] != "p" || got.Agent != "codex" {
			t.Fatalf("pool switch = pool %s alts %v agent %s", got.Pool, got.AltPools, got.Agent)
		}
		decs, err := e.st.ListGateDecisions(e.ctx, 10)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, d := range decs {
			if d.JobID == j.ID && d.SelectedPool == "q" && d.Reason == "failover" {
				found = true
			}
		}
		if !found {
			t.Fatalf("failover gate decision not recorded: %+v", decs)
		}
	})
	t.Run("failover disabled by pool config", func(t *testing.T) {
		e := newRequeueEnv(t, true, 3)
		no := false
		e.cfg.Pools["p"] = config.PoolConfig{AppliesTo: []config.PoolTarget{{Agent: "codex"}}, Requeue: config.RequeueConfig{FailoverAltPools: &no}}
		e.add(CreateJobParams{RequestID: "a", Pool: "p", AltPools: []string{"q"}})
		e.hit(e.claim())
		var sawFailover bool
		e.st.SetRequeueGate(func(ctx context.Context, job *Job, pool string, failover bool) RequeueGateVerdict {
			sawFailover = failover
			return RequeueGateVerdict{Allow: true}
		})
		if got := e.claim(); got != nil {
			t.Fatalf("claimed %d during retry_after", got.ID)
		}
		if sawFailover {
			t.Fatal("failover flag must follow pools.p.requeue.failover_alt_pools")
		}
	})
	t.Run("fresh jobs are not gated", func(t *testing.T) {
		e := newRequeueEnv(t, true, 3)
		j := e.add(CreateJobParams{RequestID: "a", Pool: "p"})
		e.st.SetRequeueGate(func(context.Context, *Job, string, bool) RequeueGateVerdict {
			t.Error("gate called for a job that was never requeued")
			return RequeueGateVerdict{}
		})
		if got := e.claim(); got == nil || got.ID != j.ID {
			t.Fatalf("claim = %+v", got)
		}
	})
}

func TestRequeuedJobWithoutGateFailsClosedInEnforce(t *testing.T) {
	e := newRequeueEnv(t, true, 3)
	j := e.add(CreateJobParams{RequestID: "a", Pool: "p"})
	e.hit(e.claim())
	e.expire()
	e.st.SetRequeueGate(nil)
	other := e.add(CreateJobParams{RequestID: "other", Pool: "q", Agent: "claude"})
	if got := e.claim(); got == nil || got.ID != other.ID {
		t.Fatalf("claim = %+v, want the fresh job %d (requeued job %d must be skipped, not block)", got, other.ID, j.ID)
	}
	if got, ok, err := e.st.ClaimByID(e.ctx, j.ID, 8); ok || err != nil {
		t.Fatalf("ClaimByID without gate: job=%+v ok=%v err=%v", got, ok, err)
	}
	if got := e.claim(); got != nil {
		t.Fatalf("claimed %d without a gate in enforce", got.ID)
	}

	// Outside enforce there is no Gate to wait for: retry_after and the cooldown decide.
	e.cfg.Quota.GateMode = config.GateModeDryRun
	if got := e.claim(); got == nil || got.ID != j.ID {
		t.Fatalf("dry-run claim = %+v", got)
	}
}

func TestFailoverClearsRetryAfterOfOldPool(t *testing.T) {
	e := newRequeueEnv(t, true, 3)
	j := e.add(CreateJobParams{RequestID: "a", Pool: "p", AltPools: []string{"q"}})
	e.hit(e.claim())
	if e.job(j.ID).RetryAfter == nil {
		t.Fatal("retry_after not set")
	}
	e.st.SetRequeueGate(func(ctx context.Context, job *Job, pool string, failover bool) RequeueGateVerdict {
		if pool == "q" {
			return RequeueGateVerdict{Allow: true}
		}
		return RequeueGateVerdict{Allow: true, SelectedPool: "q"}
	})
	got := e.claim()
	if got == nil || got.ID != j.ID || got.Pool != "q" {
		t.Fatalf("failover claim = %+v", got)
	}
	if got.RetryAfter != nil {
		t.Fatalf("retry_after of the old pool kept after failover: %v", got.RetryAfter)
	}
	// The run on q dies (e.g. worker lost) and is reset: q is healthy, so the job must be
	// claimable again at once instead of waiting for p's cooldown deadline.
	if _, err := e.st.Reset(e.ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	if again := e.claim(); again == nil || again.ID != j.ID || again.Pool != "q" {
		t.Fatalf("re-claim on q after reset = %+v", again)
	}
}

func TestStalledRateLimitRequeued(t *testing.T) {
	e := newRequeueEnv(t, true, 3)
	j := e.add(CreateJobParams{RequestID: "a", Pool: "p"})
	e.hit(e.claim())
	if n, err := e.st.CountRateLimitRequeued(e.ctx); err != nil || n != 1 {
		t.Fatalf("count = %d %v", n, err)
	}
	if got, err := e.st.ListStalledRateLimitRequeued(e.ctx, time.Now()); err != nil || len(got) != 0 {
		t.Fatalf("fresh requeue reported as stalled: %v %v", got, err)
	}
	got, err := e.st.ListStalledRateLimitRequeued(e.ctx, time.Now().Add(6*time.Hour+time.Minute))
	if err != nil || len(got) != 1 || got[0].ID != j.ID {
		t.Fatalf("stalled = %v %v", got, err)
	}
}

func TestRateLimitColumnsMigration(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "fleet.db")
	st, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.CreateJob(ctx, CreateJobParams{RequestID: "old", Title: "old"}); err != nil {
		t.Fatal(err)
	}
	for _, col := range []string{"rate_limit_retries", "retry_after", "on_rate_limit"} {
		if _, err := st.writeDB.ExecContext(ctx, `ALTER TABLE jobs DROP COLUMN `+col); err != nil {
			t.Fatal(err)
		}
	}
	_ = st.Close()
	st, err = Open(ctx, path, Options{})
	if err != nil {
		t.Fatalf("reopen (migration): %v", err)
	}
	defer st.Close()
	j, err := st.GetJobByRequestID(ctx, "old")
	if err != nil {
		t.Fatal(err)
	}
	if j.RateLimitRetries != 0 || j.RetryAfter != nil || j.OnRateLimit != "" {
		t.Fatalf("migrated defaults %+v", j)
	}
}

// TestStaleRequeueVerdictIsNotReused reproduces the review finding on 141a80e: a Gate
// pass evaluated for one queue generation of a job must not let a claim that started
// earlier take the job after it was claimed, rate-limited and requeued again in between.
func TestStaleRequeueVerdictIsNotReused(t *testing.T) {
	for _, byID := range []bool{false, true} {
		name := "Claim"
		if byID {
			name = "ClaimByID"
		}
		t.Run(name, func(t *testing.T) {
			e := newRequeueEnv(t, true, 3)
			j := e.add(CreateJobParams{RequestID: "a", Pool: "p"})
			e.hit(e.claim())
			e.expire()

			var nested, raced bool
			e.st.SetRequeueGate(func(ctx context.Context, job *Job, pool string, failover bool) RequeueGateVerdict {
				if nested {
					// The concurrent claim sees retry=1 and is allowed through.
					return RequeueGateVerdict{Allow: true}
				}
				if raced {
					// Any evaluation of the newer generation holds the job.
					return RequeueGateVerdict{}
				}
				if job.RateLimitRetries != 1 {
					t.Errorf("first evaluation saw retry=%d", job.RateLimitRetries)
				}
				// Before this retry=1 pass returns, another claim takes the job, it hits
				// the rate limit again (retry=2) and the cooldown is cleared manually.
				raced, nested = true, true
				other := e.claim()
				nested = false
				if other == nil || other.ID != j.ID {
					t.Errorf("concurrent claim = %+v", other)
					return RequeueGateVerdict{Allow: true}
				}
				e.hit(other)
				if cleared, err := e.st.ClearCooldown(ctx, "p"); err != nil || !cleared {
					t.Errorf("ClearCooldown cleared=%v err=%v", cleared, err)
				}
				return RequeueGateVerdict{Allow: true}
			})

			var got *Job
			var ok bool
			var err error
			if byID {
				got, ok, err = e.st.ClaimByID(e.ctx, j.ID, 8)
			} else {
				got, ok, err = e.st.Claim(e.ctx, 8)
			}
			if err != nil {
				t.Fatal(err)
			}
			if !raced {
				t.Fatal("gate was not evaluated")
			}
			if ok {
				t.Fatalf("claimed job %d (retry=%d) with a verdict evaluated for retry=1", got.ID, got.RateLimitRetries)
			}
			if cur := e.job(j.ID); cur.Status != StatusQueued || cur.RateLimitRetries != 2 {
				t.Fatalf("job after stale verdict: status=%s retry=%d", cur.Status, cur.RateLimitRetries)
			}
			// A fresh evaluation decides the new generation (here: hold).
			if again := e.claim(); again != nil {
				t.Fatalf("claimed %d although a fresh evaluation holds it", again.ID)
			}
		})
	}
}

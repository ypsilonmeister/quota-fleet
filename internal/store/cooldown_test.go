package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/quota"
)

func TestCooldownClaimDebounceAndHalfOpen(t *testing.T) {
	ctx := context.Background()
	yes := true
	cfg := &config.Config{
		Quota: config.QuotaConfig{GateMode: config.GateModeEnforce, Cooldown: config.CooldownConfig{Enabled: &yes}},
		Pools: map[string]config.PoolConfig{"p": {AppliesTo: []config.PoolTarget{{Agent: "codex"}}}, "q": {}},
	}
	st, err := Open(ctx, filepath.Join(t.TempDir(), "fleet.db"), Options{Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	add := func(pool, id string) *Job {
		t.Helper()
		j, _, err := st.CreateJob(ctx, CreateJobParams{RequestID: id, Title: id, Pool: pool, Agent: "codex"})
		if err != nil {
			t.Fatal(err)
		}
		return j
	}
	claim := func(want *Job) *Job {
		t.Helper()
		j, ok, err := st.Claim(ctx, 8)
		if err != nil || !ok || j.ID != want.ID {
			t.Fatalf("Claim=%+v, %t, %v, want %d", j, ok, err, want.ID)
		}
		return j
	}
	hit := func(j *Job, started bool) {
		t.Helper()
		out, err := st.FailWithCooldown(ctx, j.ID, j.ClaimedBy, "failed", "failed", "quota")
		if err != nil || (out.Cooldown != nil) != started || !strings.Contains(out.Job.LastError, "cooldown") {
			t.Fatalf("hit=%+v err=%v", out, err)
		}
	}
	state := func() quota.CooldownState {
		t.Helper()
		rows, err := st.ListCooldowns(ctx)
		if err != nil || len(rows) != 1 {
			t.Fatalf("cooldowns=%v err=%v", rows, err)
		}
		return rows[0]
	}
	expire := func() {
		t.Helper()
		if _, err := st.writeDB.ExecContext(ctx, "UPDATE pool_cooldowns SET active_until=? WHERE pool='p'", formatTime(time.Now().Add(-time.Second))); err != nil {
			t.Fatal(err)
		}
	}

	first := claim(add("p", "first"))
	parallel := claim(add("p", "parallel"))
	earlierSuccess := claim(add("p", "earlier-success"))
	hit(first, true)
	initial := state()
	hit(parallel, false)
	if got := state(); got.ConsecutiveHits != 1 || got.ActiveUntil != initial.ActiveUntil || *got.LastJobID != parallel.ID {
		t.Fatalf("debounce changed streak/deadline or lost diagnostics: %+v", got)
	}
	if _, err := st.Complete(ctx, earlierSuccess.ID, earlierSuccess.ClaimedBy, "ok"); err != nil {
		t.Fatal(err)
	}
	if !state().Active(time.Now()) {
		t.Fatal("older success cleared cooldown")
	}

	probe := add("", "inferred-pool")
	second := add("p", "second")
	other := add("q", "other")
	claim(other)
	if _, ok, err := st.ClaimByID(ctx, probe.ID, 8); err != nil || ok {
		t.Fatalf("active ClaimByID ok=%v err=%v", ok, err)
	}
	expire()
	running := claim(probe)
	if _, ok, err := st.Claim(ctx, 8); err != nil || ok {
		t.Fatalf("multiple probes ok=%v err=%v", ok, err)
	}
	hit(running, true)
	if got := state(); got.ConsecutiveHits != 2 || got.ActiveUntil.Sub(got.TriggeredAt) != 10*time.Minute {
		t.Fatalf("backoff=%+v", got)
	}
	expire()
	running = claim(second)
	if _, err := st.Complete(ctx, running.ID, running.ClaimedBy, "ok"); err != nil {
		t.Fatal(err)
	}
	if got := state(); got.ClearedReason != "success" || got.ConsecutiveHits != 0 {
		t.Fatalf("successful probe did not reset: %+v", got)
	}

	hit(claim(add("p", "new-hit")), true)
	trigger := state().TriggeredAt
	recoverAt := trigger.Add(time.Second)
	short := false
	opts := quota.EvalOptions{Now: recoverAt, Format: quota.FormatV1, Pools: map[string]config.PoolConfig{"p": {ShortWindow: &short}}}
	snap := &quota.Snapshot{GeneratedAt: recoverAt, Pools: map[string]quota.PoolSnapshot{
		"p": {MeasuredAt: trigger, WeeklyRemainingPercent: 50, ShortApplicable: &short},
	}}
	if err := st.LoadCooldowns(ctx, snap, &opts); err != nil {
		t.Fatal(err)
	}
	if !state().Active(recoverAt) {
		t.Fatal("equal timestamp cleared cooldown")
	}
	p := snap.Pools["p"]
	p.MeasuredAt = recoverAt
	snap.Pools["p"] = p
	if err := st.LoadCooldowns(ctx, snap, &opts); err != nil {
		t.Fatal(err)
	}
	if got := state(); got.ClearedReason != "snapshot_recovered" || len(opts.Cooldowns) != 0 {
		t.Fatalf("snapshot did not reset cooldown: %+v", got)
	}
}

func TestCooldownHalfOpenHeldRelease(t *testing.T) {
	ctx := context.Background()
	enabled := true
	initial := "1ns"
	cfg := &config.Config{
		Quota: config.QuotaConfig{GateMode: config.GateModeEnforce, Cooldown: config.CooldownConfig{Enabled: &enabled, Initial: &initial}},
		Pools: map[string]config.PoolConfig{"p": {Unmeasured: true}},
	}
	st, err := Open(ctx, filepath.Join(t.TempDir(), "fleet.db"), Options{Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, _, err := st.CreateJob(ctx, CreateJobParams{RequestID: "trigger", Title: "trigger", Pool: "p"}); err != nil {
		t.Fatal(err)
	}
	trigger, ok, err := st.Claim(ctx, 4)
	if err != nil || !ok {
		t.Fatalf("claim=%t error=%v", ok, err)
	}
	if _, err := st.FailWithCooldown(ctx, trigger.ID, trigger.ClaimedBy, "failed", "failed", "quota"); err != nil {
		t.Fatal(err)
	}
	hold := func(id string) string {
		t.Helper()
		h, _, err := st.HoldJob(ctx, HoldJobParams{RequestID: id, Pool: "p", Payload: CreateJobParams{RequestID: id, Title: id, Pool: "p"}})
		if err != nil {
			t.Fatal(err)
		}
		return h.ID
	}

	first, second := hold("first"), hold("second")
	out, err := st.ReleaseHeld(ctx, ReleaseHeldParams{HeldID: first, SelectedPool: "p"})
	if err != nil || out.Job == nil {
		t.Fatalf("release=%+v error=%v", out, err)
	}
	mustBlock := func() {
		t.Helper()
		if _, err := st.ReleaseHeld(ctx, ReleaseHeldParams{HeldID: second, SelectedPool: "p"}); !errors.Is(err, ErrCooldownBlocked) {
			t.Fatalf("second release error=%v", err)
		}
	}
	mustBlock()
	probe, ok, err := st.Claim(ctx, 4)
	if err != nil || !ok || probe.ID != out.Job.ID {
		t.Fatalf("claim=%+v %t error=%v", probe, ok, err)
	}
	mustBlock()
	if _, err := st.Complete(ctx, probe.ID, probe.ClaimedBy, "ok"); err != nil {
		t.Fatal(err)
	}
	if out, err := st.ReleaseHeld(ctx, ReleaseHeldParams{HeldID: second, SelectedPool: "p"}); err != nil || out.Job == nil {
		t.Fatalf("release after probe=%+v error=%v", out, err)
	}
}

func TestCooldownDryRunDoesNotBlockClaim(t *testing.T) {
	ctx := context.Background()
	enabled := true
	cfg := &config.Config{
		Quota: config.QuotaConfig{GateMode: config.GateModeDryRun, Cooldown: config.CooldownConfig{Enabled: &enabled}},
		Pools: map[string]config.PoolConfig{"p": {Unmeasured: true}},
	}
	dbPath := filepath.Join(t.TempDir(), "fleet.db")
	st, err := Open(ctx, dbPath, Options{Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { st.Close() }()
	for _, id := range []string{"first", "second"} {
		if _, _, err := st.CreateJob(ctx, CreateJobParams{RequestID: id, Title: id, Pool: "p"}); err != nil {
			t.Fatal(err)
		}
	}
	first, ok, err := st.Claim(ctx, 2)
	if err != nil || !ok {
		t.Fatalf("first claim=%t error=%v", ok, err)
	}
	if _, err := st.FailWithCooldown(ctx, first.ID, first.ClaimedBy, "failed", "failed", "quota"); err != nil {
		t.Fatal(err)
	}

	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, dbPath, Options{Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	st = reopened
	rows, err := st.ListCooldowns(ctx)
	if err != nil || len(rows) != 1 || !rows[0].Active(time.Now()) {
		t.Fatalf("persisted cooldown=%+v error=%v", rows, err)
	}
	var decisions int
	if err := st.readDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM gate_decisions WHERE mode='dry-run' AND decision='would_hold' AND blocked_by='cooldown'`).Scan(&decisions); err != nil || decisions != 1 {
		t.Fatalf("decisions=%d error=%v", decisions, err)
	}
	second, ok, err := st.Claim(ctx, 2)
	if err != nil || !ok || second.RequestID != "second" {
		t.Fatalf("dry-run claim=%+v %t error=%v", second, ok, err)
	}
}

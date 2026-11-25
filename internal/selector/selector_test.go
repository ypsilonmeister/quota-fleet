package selector

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/quota"
)

var now = time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

func pool(weekly, short int) quota.PoolSnapshot {
	s := short
	return quota.PoolSnapshot{WeeklyRemainingPercent: weekly, ShortRemainingPercent: &s, MeasuredAt: now}
}

func snap(pools map[string]quota.PoolSnapshot) *quota.Snapshot {
	return &quota.Snapshot{MeasuredAt: now, Pools: pools}
}

func input(role, weight string, s *quota.Snapshot) Input {
	return Input{Role: role, Weight: weight, Usage: s, Eval: quota.EvalOptions{Now: now, StaleAfter: 45 * time.Minute}}
}

func TestSelectFollowsTableWhenHealthy(t *testing.T) {
	s := snap(map[string]quota.PoolSnapshot{"codex": pool(50, 50), "claude": pool(50, 50), "agy-gemini": pool(50, 50)})
	cases := []struct {
		role, weight, agent, model string
	}{
		{"", "", "codex", "gpt-6.2-sol"},
		{RoleImplement, WeightHeavy, "claude", "claude-opus-5-5"},
		{RoleReview, WeightLight, "agy", "gemini-4-neon-medium"},
		{RoleFix, WeightStandard, "codex", "gpt-6.2-sol"},
		{RoleJudge, WeightHeavy, "codex", "gpt-6.2-sol"}, // docs/10: Gemini Decision is not a job; default candidate
		{RoleMisc, WeightLight, "agy", "gemini-4-neon-medium"},
	}
	for _, c := range cases {
		got := Select(input(c.role, c.weight, s))
		if got.Agent != c.agent || got.Model != c.model || got.Fallback {
			t.Errorf("%s/%s = %+v, want %s/%s", c.role, c.weight, got, c.agent, c.model)
		}
	}
}

func TestSelectAvoidsExhaustedPoolAndRecordsWhy(t *testing.T) {
	s := snap(map[string]quota.PoolSnapshot{"codex": pool(0, 0), "claude": pool(50, 50), "agy-gemini": pool(40, 40)})
	got := Select(input(RoleImplement, WeightStandard, s))
	if got.Agent != "agy" || got.Pool != quota.PoolAgyGemini {
		t.Fatalf("got %+v, want agy on agy-gemini (codex exhausted)", got)
	}
	if got.Fallback {
		t.Errorf("a healthy candidate exists; must not be a fallback")
	}
	if !strings.Contains(got.Reason, "codex: exhausted(weekly") || !strings.Contains(got.Reason, "excluded") {
		t.Errorf("reason does not record the excluded pool: %s", got.Reason)
	}
}

func TestSelectStaleFallsBackToDefaultOrderWithReason(t *testing.T) {
	old := pool(50, 50)
	old.MeasuredAt = now.Add(-3 * time.Hour)
	s := snap(map[string]quota.PoolSnapshot{"codex": old, "claude": old, "agy-gemini": old})
	s.MeasuredAt = old.MeasuredAt
	got := Select(input(RoleImplement, WeightStandard, s))
	if got.Agent != "codex" || got.Model != "gpt-6.2-sol" || !got.Fallback {
		t.Fatalf("got %+v, want deterministic default codex/gpt-6.2-sol as fallback", got)
	}
	if !strings.Contains(got.Reason, "FALLBACK") || !strings.Contains(got.Reason, "stale") {
		t.Errorf("fallback reason missing: %s", got.Reason)
	}
}

func TestSelectMissingUsageFallsBack(t *testing.T) {
	in := input(RoleReview, WeightStandard, nil)
	in.UsageErr = errors.New("no such file")
	got := Select(in)
	if !got.Fallback || got.Agent != "codex" || !strings.Contains(got.Reason, "usage_unavailable") {
		t.Fatalf("got %+v", got)
	}
}

func TestSelectNeverOverridesExplicitFields(t *testing.T) {
	s := snap(map[string]quota.PoolSnapshot{"codex": pool(0, 0), "claude": pool(50, 50), "agy-gemini": pool(50, 50)})

	// Explicit agent only: model/pool filled from candidates of that agent.
	in := input(RoleImplement, WeightStandard, s)
	in.Agent = "claude"
	got := Select(in)
	if got.Agent != "claude" || got.Model != "claude-sonnet-5-5" || got.Pool != "claude" {
		t.Errorf("explicit agent: got %+v", got)
	}

	// Explicit exhausted pool is kept even though it is exhausted (Gate decides).
	in = input(RoleImplement, WeightStandard, s)
	in.Agent, in.Model = "codex", "gpt-6.2-sol"
	got = Select(in)
	if got.Agent != "codex" || got.Model != "gpt-6.2-sol" || got.Pool != "codex" {
		t.Errorf("explicit exhausted: got %+v", got)
	}

	// Explicit values outside the table are kept verbatim.
	in = input(RoleImplement, WeightStandard, s)
	in.Agent, in.Model = "custom", "my-model"
	got = Select(in)
	if got.Agent != "custom" || got.Model != "my-model" || !strings.Contains(got.Reason, "no table candidate") {
		t.Errorf("out-of-table: got %+v", got)
	}
}

func TestSelectAllExhaustedKeepsFirstCandidateForGate(t *testing.T) {
	s := snap(map[string]quota.PoolSnapshot{"codex": pool(0, 0), "claude": pool(0, 0), "agy-gemini": pool(0, 0)})
	got := Select(input(RoleImplement, WeightStandard, s))
	if got.Agent != "codex" || !got.Fallback || !strings.Contains(got.Reason, "all candidate pools exhausted") {
		t.Fatalf("got %+v", got)
	}
}

func TestDefaults(t *testing.T) {
	d := Defaults("", "", "", nil)
	if d != DefaultCandidate {
		t.Errorf("Defaults() = %+v", d)
	}
	d = Defaults("claude", "", "", nil)
	if d.Agent != "claude" || d.Model != "" || d.Pool != "" {
		t.Errorf("explicit agent must not get a codex model/pool: %+v", d)
	}
}

func TestValidators(t *testing.T) {
	if !ValidRole("") || !ValidRole("review") || ValidRole("nope") || !ValidWeight("heavy") || ValidWeight("huge") {
		t.Error("validators misbehave")
	}
}

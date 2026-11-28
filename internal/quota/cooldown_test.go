package quota

import (
	"testing"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/config"
)

func TestCooldownDecisionMatrix(t *testing.T) {
	now := time.Now().UTC()
	trigger := now.Add(-time.Minute)
	c := CooldownState{Pool: "p", TriggeredAt: trigger, ActiveUntil: now.Add(time.Minute), ConsecutiveHits: 1}
	short := false
	for _, tc := range []struct {
		name                string
		unmeasured, cooling bool
		snapshotStatus      string
		percent             int
		measured            time.Time
		want                PoolHealthStatus
	}{
		{"missing", false, false, "", 0, time.Time{}, PoolUnknown},
		{"missing with cooldown", false, true, "", 0, time.Time{}, PoolUnknown},
		{"stale with cooldown", false, true, "ok", 25, now.Add(-time.Hour), PoolUnknown},
		{"collector error with cooldown", false, true, "error", 0, trigger, PoolUnknown},
		{"exhausted", false, false, "ok", 0, trigger, PoolExhausted},
		{"exhausted with cooldown", false, true, "ok", 0, now, PoolExhausted},
		{"healthy at trigger", false, true, "ok", 25, trigger, PoolCooldown},
		{"healthy before trigger", false, true, "ok", 25, trigger.Add(-time.Second), PoolCooldown},
		{"healthy after trigger", false, true, "ok", 25, now, PoolHealthy},
		{"healthy", false, false, "ok", 25, trigger, PoolHealthy},
		{"unmeasured cooling", true, true, "", 0, time.Time{}, PoolCooldown},
		{"unmeasured bypass", true, false, "", 0, time.Time{}, PoolUnmeasured},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := EvalOptions{Now: now, Format: FormatV1, CheckPoolConfig: true,
				Pools: map[string]config.PoolConfig{"p": {Unmeasured: tc.unmeasured, ShortWindow: &short}, "alt": {Unmeasured: true}}}
			if tc.cooling {
				opts.Cooldowns = map[string]CooldownState{"p": c}
			}
			var snap *Snapshot
			if tc.snapshotStatus != "" {
				snap = &Snapshot{GeneratedAt: now, Pools: map[string]PoolSnapshot{
					"p": {Status: tc.snapshotStatus, MeasuredAt: tc.measured, WeeklyRemainingPercent: tc.percent, ShortApplicable: &short},
				}}
			}
			ev := EvaluatePool("p", snap, opts)
			if ev.Status != tc.want {
				t.Fatalf("status=%s want %s (%+v)", ev.Status, tc.want, ev)
			}
			held := tc.want == PoolUnknown || tc.want == PoolExhausted || tc.want == PoolCooldown
			if gate := EvaluateGate("p", nil, snap, opts); ShouldHoldOnEnforce(gate) != held {
				t.Fatalf("hold=%v want %v (%+v)", ShouldHoldOnEnforce(gate), held, gate)
			}
			gate := EvaluateGate("p", []string{"alt"}, snap, opts)
			if tc.want == PoolUnknown && !ShouldHoldOnEnforce(gate) {
				t.Fatalf("v1 unknown allowed unmeasured alt: %+v", gate)
			}
			if !tc.unmeasured && (tc.want == PoolExhausted || tc.want == PoolCooldown) && gate.SelectedPool != "alt" {
				t.Fatalf("expected unmeasured detour: %+v", gate)
			}
		})
	}
	opts := EvalOptions{Now: now, Cooldowns: map[string]CooldownState{"p": c}}
	if gate := EvaluateGate("p", []string{"missing-alt"}, nil, opts); !ShouldHoldOnEnforce(gate) || gate.BlockedBy != "cooldown" {
		t.Fatalf("legacy missing snapshot with cooldown: %+v", gate)
	}
}

func TestCooldownUnmeasuredDetourFromFile(t *testing.T) {
	now := time.Now()
	opts := EvalOptions{Now: now, CheckPoolConfig: true,
		Pools:     map[string]config.PoolConfig{"p": {Unmeasured: true}, "alt": {Unmeasured: true}},
		Cooldowns: map[string]CooldownState{"p": {Pool: "p", ActiveUntil: now.Add(time.Minute)}},
	}
	for _, format := range []string{FormatV1, FormatLegacy} {
		t.Run(format, func(t *testing.T) {
			opts.Format = format
			for _, primary := range []string{"p", ""} {
				got, err := EvaluateFromFile("", primary, []string{"p", "alt"}, opts)
				if err != nil || got.Decision != DecisionWouldPass || got.SelectedPool != "alt" {
					t.Errorf("primary=%q: gate=%+v error=%v", primary, got, err)
				}
			}
		})
	}
}

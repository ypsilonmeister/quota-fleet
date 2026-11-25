package quota

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/config"
)

const sampleUsageFixture = `{
  "updatedAt": "2026-11-23T10:29:49+09:00",
  "updatedText": "Updated 11/23 10:29",
  "services": {
    "muse": {
      "remainingPercent": 4,
      "resetAt": "2026-11-23T20:07:00+09:00",
      "countdownText": "9h",
      "stale": false,
      "fetchedAt": "2026-11-23T10:29:49+09:00",
      "label": "MUSE"
    },
    "claude": {
      "remainingPercent": 83,
      "resetAt": "2026-11-26T20:00:00+09:00",
      "subText": "5h 98%",
      "subResetAt": "2026-11-23T14:30:00+09:00",
      "countdownText": "3d",
      "stale": false,
      "fetchedAt": "2026-11-23T10:29:49+09:00",
      "label": "CLAUDE"
    },
    "codex": {
      "remainingPercent": 38,
      "resetAt": "2026-11-24T14:51:23+09:00",
      "subText": "5h 70%",
      "subResetAt": "2026-11-23T12:36:25+09:00",
      "countdownText": "1d",
      "stale": false,
      "fetchedAt": "2026-11-23T10:29:49+09:00",
      "label": "CODEX"
    },
    "antigravity": {
      "remainingPercent": 65,
      "resetAt": "2026-11-28T18:00:17+09:00",
      "countdownText": "5d",
      "stale": false,
      "fetchedAt": "2026-11-23T10:29:49+09:00",
      "label": "ANTIGRAVITY",
      "subText": "5h 34%",
      "subResetAt": "2026-11-23T11:00:02+09:00",
      "altPercent": 48,
      "altResetAt": "2026-11-26T01:32:24+09:00",
      "altCountdownText": "2d",
      "altSubText": "5h 100%",
      "altSubResetAt": "2026-11-23T15:30:02+09:00"
    }
  },
  "flat": {
    "claudePercent": 83
  }
}`

func mustTime(t *testing.T, raw string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		t.Fatalf("parse time %q: %v", raw, err)
	}
	return parsed
}

func intPtr(v int) *int {
	return &v
}

func timePtr(v time.Time) *time.Time {
	return &v
}

func TestParseUsageJSONNormalizesAllFourPools(t *testing.T) {
	snap, err := ParseUsageJSON([]byte(sampleUsageFixture))
	if err != nil {
		t.Fatalf("ParseUsageJSON: %v", err)
	}

	wantMeasured := mustTime(t, "2026-11-23T10:29:49+09:00")
	if !snap.MeasuredAt.Equal(wantMeasured) {
		t.Fatalf("MeasuredAt = %v, want %v", snap.MeasuredAt, wantMeasured)
	}
	if len(snap.Pools) != 4 {
		t.Fatalf("len(Pools) = %d, want 4 (%v)", len(snap.Pools), snap.Pools)
	}

	// claude
	claude := snap.Pools[PoolClaude]
	if claude.WeeklyRemainingPercent != 83 {
		t.Errorf("claude.WeeklyRemainingPercent = %d, want 83", claude.WeeklyRemainingPercent)
	}
	if claude.ShortRemainingPercent == nil || *claude.ShortRemainingPercent != 98 {
		t.Errorf("claude.ShortRemainingPercent = %v, want 98", claude.ShortRemainingPercent)
	}
	if claude.WeeklyResetAt == nil || !claude.WeeklyResetAt.Equal(mustTime(t, "2026-11-26T20:00:00+09:00")) {
		t.Errorf("claude.WeeklyResetAt = %v", claude.WeeklyResetAt)
	}
	if claude.ShortResetAt == nil || !claude.ShortResetAt.Equal(mustTime(t, "2026-11-23T14:30:00+09:00")) {
		t.Errorf("claude.ShortResetAt = %v", claude.ShortResetAt)
	}
	if !claude.MeasuredAt.Equal(wantMeasured) {
		t.Errorf("claude.MeasuredAt = %v, want %v", claude.MeasuredAt, wantMeasured)
	}

	// codex
	codex := snap.Pools[PoolCodex]
	if codex.WeeklyRemainingPercent != 38 || codex.ShortRemainingPercent == nil || *codex.ShortRemainingPercent != 70 {
		t.Errorf("codex = %+v", codex)
	}

	// agy-gemini (antigravity primary)
	agyGem := snap.Pools[PoolAgyGemini]
	if agyGem.WeeklyRemainingPercent != 65 || agyGem.ShortRemainingPercent == nil || *agyGem.ShortRemainingPercent != 34 {
		t.Errorf("agy-gemini = %+v", agyGem)
	}
	if agyGem.WeeklyResetAt == nil || !agyGem.WeeklyResetAt.Equal(mustTime(t, "2026-11-28T18:00:17+09:00")) {
		t.Errorf("agy-gemini.WeeklyResetAt = %v", agyGem.WeeklyResetAt)
	}

	// agy-3p (antigravity alt)
	agy3p := snap.Pools[PoolAgy3P]
	if agy3p.WeeklyRemainingPercent != 48 {
		t.Errorf("agy-3p.WeeklyRemainingPercent = %d, want 48", agy3p.WeeklyRemainingPercent)
	}
	if agy3p.ShortRemainingPercent == nil || *agy3p.ShortRemainingPercent != 100 {
		t.Errorf("agy-3p.ShortRemainingPercent = %v, want 100", agy3p.ShortRemainingPercent)
	}
	if agy3p.WeeklyResetAt == nil || !agy3p.WeeklyResetAt.Equal(mustTime(t, "2026-11-26T01:32:24+09:00")) {
		t.Errorf("agy-3p.WeeklyResetAt = %v", agy3p.WeeklyResetAt)
	}
	if agy3p.ShortResetAt == nil || !agy3p.ShortResetAt.Equal(mustTime(t, "2026-11-23T15:30:02+09:00")) {
		t.Errorf("agy-3p.ShortResetAt = %v", agy3p.ShortResetAt)
	}
	if !agy3p.MeasuredAt.Equal(wantMeasured) {
		t.Errorf("agy-3p.MeasuredAt = %v, want %v", agy3p.MeasuredAt, wantMeasured)
	}
}

func TestParseUsageJSONSubTextFailureYieldsNull(t *testing.T) {
	fixture := `{
		"updatedAt": "2026-11-23T10:29:49+09:00",
		"services": {
			"claude": {
				"remainingPercent": 80,
				"resetAt": "2026-11-26T20:00:00+09:00",
				"subText": "resetting",
				"subResetAt": ""
			},
			"codex": {
				"remainingPercent": 50,
				"resetAt": "2026-11-25T20:00:00+09:00",
				"subText": "5h 0%",
				"subResetAt": "2026-11-23T12:00:00+09:00"
			},
			"antigravity": {
				"remainingPercent": 60,
				"resetAt": "2026-11-28T00:00:00+09:00",
				"subText": "",
				"altPercent": 40,
				"altResetAt": "2026-11-27T00:00:00+09:00",
				"altSubText": "15h 99%"
			}
		}
	}`

	snap, err := ParseUsageJSON([]byte(fixture))
	if err != nil {
		t.Fatalf("ParseUsageJSON: %v", err)
	}

	if snap.Pools[PoolClaude].ShortRemainingPercent != nil {
		t.Errorf("claude short = %v, want nil on unparseable subText", *snap.Pools[PoolClaude].ShortRemainingPercent)
	}
	if snap.Pools[PoolClaude].ShortResetAt != nil {
		t.Errorf("claude ShortResetAt = %v, want nil", snap.Pools[PoolClaude].ShortResetAt)
	}
	if snap.Pools[PoolCodex].ShortRemainingPercent == nil || *snap.Pools[PoolCodex].ShortRemainingPercent != 0 {
		t.Errorf("codex short = %v, want 0", snap.Pools[PoolCodex].ShortRemainingPercent)
	}
	if snap.Pools[PoolAgyGemini].ShortRemainingPercent != nil {
		t.Errorf("agy-gemini short = %v, want nil on empty subText", *snap.Pools[PoolAgyGemini].ShortRemainingPercent)
	}
	if snap.Pools[PoolAgy3P].ShortRemainingPercent != nil {
		t.Errorf("agy-3p short = %v, want nil on non-5h subText '15h 99%%'", *snap.Pools[PoolAgy3P].ShortRemainingPercent)
	}
}

func TestLoadUsageFileAndErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.json")
	if err := os.WriteFile(path, []byte(sampleUsageFixture), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	snap, err := LoadUsageFile(path)
	if err != nil {
		t.Fatalf("LoadUsageFile: %v", err)
	}
	if snap.Pools[PoolAgy3P].WeeklyRemainingPercent != 48 {
		t.Errorf("agy-3p weekly = %d, want 48", snap.Pools[PoolAgy3P].WeeklyRemainingPercent)
	}

	if _, err := ParseUsageJSON([]byte(`{"services":{}}`)); err == nil {
		t.Errorf("expected error when updatedAt is missing")
	}
	if _, err := LoadUsageFile(filepath.Join(dir, "missing.json")); err == nil {
		t.Errorf("expected error for missing file")
	}
}

func TestEvaluatePoolHealthRules(t *testing.T) {
	measured := mustTime(t, "2026-11-23T10:00:00+09:00")
	freshNow := measured.Add(20 * time.Minute)
	exactBoundNow := measured.Add(45 * time.Minute)
	staleNow := measured.Add(45*time.Minute + time.Second)
	resetAt := measured.Add(24 * time.Hour)

	makeSnap := func(ps PoolSnapshot) *Snapshot {
		ps.MeasuredAt = measured
		return &Snapshot{
			MeasuredAt: measured,
			Pools:      map[string]PoolSnapshot{"p": ps},
		}
	}

	t.Run("healthy when weekly > 0 and 5h > 0 within stale bound", func(t *testing.T) {
		snap := makeSnap(PoolSnapshot{
			WeeklyRemainingPercent: 10,
			WeeklyResetAt:          timePtr(resetAt),
			ShortRemainingPercent:  intPtr(1),
		})
		ev := EvaluatePool("p", snap, EvalOptions{Now: exactBoundNow, StaleAfter: 45 * time.Minute})
		if ev.Status != PoolHealthy || ev.BlockedBy != BlockedByNone || ev.Reason != ReasonNone {
			t.Fatalf("got %+v, want healthy", ev)
		}
	})

	t.Run("weekly 0 is exhausted blocked_by weekly", func(t *testing.T) {
		snap := makeSnap(PoolSnapshot{
			WeeklyRemainingPercent: 0,
			ShortRemainingPercent:  intPtr(90),
		})
		ev := EvaluatePool("p", snap, EvalOptions{Now: freshNow})
		if ev.Status != PoolExhausted || ev.BlockedBy != BlockedByWeekly {
			t.Fatalf("got %+v, want exhausted blocked_by=weekly", ev)
		}
	})

	t.Run("5h 0 is exhausted blocked_by 5h", func(t *testing.T) {
		snap := makeSnap(PoolSnapshot{
			WeeklyRemainingPercent: 55,
			ShortRemainingPercent:  intPtr(0),
		})
		ev := EvaluatePool("p", snap, EvalOptions{Now: freshNow})
		if ev.Status != PoolExhausted || ev.BlockedBy != BlockedByShort {
			t.Fatalf("got %+v, want exhausted blocked_by=5h", ev)
		}
	})

	t.Run("both weekly 0 and 5h 0 records weekly,5h", func(t *testing.T) {
		snap := makeSnap(PoolSnapshot{
			WeeklyRemainingPercent: 0,
			ShortRemainingPercent:  intPtr(0),
		})
		ev := EvaluatePool("p", snap, EvalOptions{Now: freshNow})
		if ev.Status != PoolExhausted || ev.BlockedBy != BlockedByWeeklyAnd5 {
			t.Fatalf("got %+v, want exhausted blocked_by=weekly,5h", ev)
		}
	})

	// PROVISIONAL behavior test (docs/06 §7-6):
	// When short_remaining_percent is nil and weekly > 0, the final spec policy is undecided.
	// Provisionally we classify the pool as unknown (neither healthy nor exhausted) with
	// reason="short_window_null" so dry-run logs distinguish it clearly.
	t.Run("provisional: short null with weekly > 0 yields unknown (short_window_null)", func(t *testing.T) {
		snap := makeSnap(PoolSnapshot{
			WeeklyRemainingPercent: 75,
			ShortRemainingPercent:  nil,
		})
		ev := EvaluatePool("p", snap, EvalOptions{Now: freshNow})
		if ev.Status != PoolUnknown || ev.Reason != ReasonShortWindowNull || ev.BlockedBy != BlockedByNone {
			t.Fatalf("got %+v, want unknown reason=%q", ev, ReasonShortWindowNull)
		}
	})

	t.Run("weekly 0 with short null is still definitively exhausted by weekly", func(t *testing.T) {
		snap := makeSnap(PoolSnapshot{
			WeeklyRemainingPercent: 0,
			ShortRemainingPercent:  nil,
		})
		ev := EvaluatePool("p", snap, EvalOptions{Now: freshNow})
		if ev.Status != PoolExhausted || ev.BlockedBy != BlockedByWeekly {
			t.Fatalf("got %+v, want exhausted blocked_by=weekly", ev)
		}
	})

	t.Run("stale snapshot older than 45m is not healthy (unknown reason=stale)", func(t *testing.T) {
		snap := makeSnap(PoolSnapshot{
			WeeklyRemainingPercent: 99,
			ShortRemainingPercent:  intPtr(99),
		})
		ev := EvaluatePool("p", snap, EvalOptions{Now: staleNow, StaleAfter: 45 * time.Minute})
		if ev.Status != PoolUnknown || ev.Reason != ReasonStale {
			t.Fatalf("got %+v, want unknown reason=stale", ev)
		}
	})

	t.Run("service-level stale flag is not healthy", func(t *testing.T) {
		snap := makeSnap(PoolSnapshot{
			WeeklyRemainingPercent: 99,
			ShortRemainingPercent:  intPtr(99),
			ServiceStale:           true,
		})
		ev := EvaluatePool("p", snap, EvalOptions{Now: freshNow})
		if ev.Status != PoolUnknown || ev.Reason != ReasonStale {
			t.Fatalf("got %+v, want unknown reason=stale", ev)
		}
	})

	t.Run("unmeasured pool bypasses snapshot check", func(t *testing.T) {
		ev := EvaluatePool("agy-argon", nil, EvalOptions{
			Now: staleNow,
			Pools: map[string]config.PoolConfig{
				"agy-argon": {Unmeasured: true},
			},
		})
		if ev.Status != PoolUnmeasured || !ev.Unmeasured || ev.Reason != ReasonUnmeasured {
			t.Fatalf("got %+v, want unmeasured", ev)
		}
	})
}

func TestEvaluateGateSelectionAndEdgeCases(t *testing.T) {
	snap, err := ParseUsageJSON([]byte(sampleUsageFixture))
	if err != nil {
		t.Fatalf("ParseUsageJSON: %v", err)
	}
	freshNow := mustTime(t, "2026-11-23T10:40:00+09:00")
	staleNow := mustTime(t, "2026-11-23T11:20:00+09:00")

	t.Run("selects healthy pool with soonest weekly_reset_at", func(t *testing.T) {
		// claude resets 11/26 20:00, codex resets 11/24 14:51, agy-3p resets 11/26 01:32, agy-gemini resets 11/28 18:00
		ev := EvaluateGate("claude", []string{"agy-gemini", "agy-3p", "codex"}, snap, EvalOptions{Now: freshNow})
		if ev.Decision != DecisionWouldPass || ev.SelectedPool != PoolCodex || ev.Bypassed {
			t.Fatalf("got %+v, want would_pass selected=codex", ev)
		}

		// Without codex, agy-3p (11/26 01:32) beats claude (11/26 20:00) and agy-gemini (11/28).
		ev2 := EvaluateGate("claude", []string{"agy-gemini", "agy-3p"}, snap, EvalOptions{Now: freshNow})
		if ev2.Decision != DecisionWouldPass || ev2.SelectedPool != PoolAgy3P {
			t.Fatalf("got %+v, want would_pass selected=agy-3p", ev2)
		}
	})

	t.Run("stale snapshot yields unknown and never passes measured pool", func(t *testing.T) {
		ev := EvaluateGate("claude", []string{"codex"}, snap, EvalOptions{Now: staleNow, StaleAfter: 45 * time.Minute})
		if ev.Decision != DecisionUnknown || ev.Reason != ReasonStale || ev.SelectedPool != "" {
			t.Fatalf("got %+v, want unknown reason=stale", ev)
		}
	})

	t.Run("unmeasured primary pool bypasses even when snapshot is stale or nil", func(t *testing.T) {
		ev := EvaluateGate("agy-argon", nil, nil, EvalOptions{
			Now: staleNow,
			Pools: map[string]config.PoolConfig{
				"agy-argon": {Unmeasured: true},
			},
		})
		if ev.Decision != DecisionWouldPass || !ev.Bypassed || ev.SelectedPool != "agy-argon" || ev.Reason != ReasonUnmeasured {
			t.Fatalf("got %+v, want bypassed would_pass selected=agy-argon", ev)
		}
	})

	t.Run("primary exhausted falls back to healthy alt pool", func(t *testing.T) {
		snapCopy := &Snapshot{
			MeasuredAt: snap.MeasuredAt,
			Pools: map[string]PoolSnapshot{
				PoolClaude: {
					WeeklyRemainingPercent: 0,
					WeeklyResetAt:          timePtr(mustTime(t, "2026-11-24T00:00:00+09:00")),
					ShortRemainingPercent:  intPtr(80),
					MeasuredAt:             snap.MeasuredAt,
				},
				PoolCodex: snap.Pools[PoolCodex],
			},
		}
		ev := EvaluateGate(PoolClaude, []string{PoolCodex}, snapCopy, EvalOptions{Now: freshNow})
		if ev.Decision != DecisionWouldPass || ev.SelectedPool != PoolCodex {
			t.Fatalf("got %+v, want would_pass selected=codex", ev)
		}
	})

	t.Run("all exhausted yields would_hold with primary blocked_by", func(t *testing.T) {
		snapCopy := &Snapshot{
			MeasuredAt: snap.MeasuredAt,
			Pools: map[string]PoolSnapshot{
				PoolClaude: {
					WeeklyRemainingPercent: 50,
					ShortRemainingPercent:  intPtr(0),
					MeasuredAt:             snap.MeasuredAt,
				},
				PoolCodex: {
					WeeklyRemainingPercent: 0,
					ShortRemainingPercent:  intPtr(50),
					MeasuredAt:             snap.MeasuredAt,
				},
			},
		}
		ev := EvaluateGate(PoolClaude, []string{PoolCodex}, snapCopy, EvalOptions{Now: freshNow})
		if ev.Decision != DecisionWouldHold || ev.BlockedBy != BlockedByShort || ev.SelectedPool != "" {
			t.Fatalf("got %+v, want would_hold blocked_by=5h", ev)
		}
	})
}

func TestResolvePoolByAppliesTo(t *testing.T) {
	pools := map[string]config.PoolConfig{
		"claude": {
			Provider:  "claude-usage",
			AppliesTo: []config.PoolTarget{{Agent: "claude"}},
		},
		"agy-gemini": {
			Provider:  "agy-usage",
			AppliesTo: []config.PoolTarget{{Agent: "agy", ModelFamily: "gemini"}},
		},
		"agy-3p": {
			Provider:  "agy-usage",
			AppliesTo: []config.PoolTarget{{Agent: "agy", ModelFamily: "claude-gpt"}},
		},
	}

	tests := []struct {
		agent string
		model string
		want  string
	}{
		{"claude", "", "claude"},
		{"claude", "claude-sonnet-4-5", "claude"},
		{"agy", "gemini-2.5-pro", "agy-gemini"},
		{"agy", "gemini-gpt-preview", "agy-gemini"},
		{"agy", "claude-sonnet-4-5", "agy-3p"},
		{"agy", "gpt-5", "agy-3p"},
		{"agy", "custom-gpt-model", ""},
		{"agy", "opus-4", ""},
		{"agy", "", ""},
		{"codex", "gpt-5", ""},
	}
	for _, tc := range tests {
		got := ResolvePoolByAppliesTo(pools, tc.agent, tc.model)
		if got != tc.want {
			t.Errorf("ResolvePoolByAppliesTo(%q, %q) = %q, want %q", tc.agent, tc.model, got, tc.want)
		}
	}
}

func TestShouldHoldOnEnforceAndSummary(t *testing.T) {
	measured := mustTime(t, "2026-11-23T10:00:00+09:00")
	freshNow := measured.Add(10 * time.Minute)
	staleNow := measured.Add(50 * time.Minute)
	resetAt := measured.Add(24 * time.Hour)

	t.Run("all exhausted holds", func(t *testing.T) {
		snap := &Snapshot{
			MeasuredAt: measured,
			Pools: map[string]PoolSnapshot{
				PoolClaude: {WeeklyRemainingPercent: 0, WeeklyResetAt: timePtr(resetAt), ShortRemainingPercent: intPtr(50), MeasuredAt: measured},
				PoolCodex:  {WeeklyRemainingPercent: 40, WeeklyResetAt: timePtr(resetAt), ShortRemainingPercent: intPtr(0), MeasuredAt: measured},
			},
		}
		ev := EvaluateGate(PoolClaude, []string{PoolCodex}, snap, EvalOptions{Now: freshNow})
		if !ShouldHoldOnEnforce(ev) {
			t.Fatalf("expected hold for all exhausted, got %+v", ev)
		}
		if got := FormatHoldReason(ev); got != "exhausted:weekly" {
			t.Errorf("FormatHoldReason = %q, want exhausted:weekly", got)
		}
		if sum := SummarizeEvaluation(ev); sum == "" {
			t.Errorf("SummarizeEvaluation returned empty string")
		}
	})

	t.Run("all stale holds", func(t *testing.T) {
		snap := &Snapshot{
			MeasuredAt: measured,
			Pools: map[string]PoolSnapshot{
				PoolClaude: {WeeklyRemainingPercent: 90, ShortRemainingPercent: intPtr(90), MeasuredAt: measured},
			},
		}
		ev := EvaluateGate(PoolClaude, nil, snap, EvalOptions{Now: staleNow, StaleAfter: 45 * time.Minute})
		if ev.Decision != DecisionUnknown || ev.Reason != ReasonStale {
			t.Fatalf("unexpected eval: %+v", ev)
		}
		if !ShouldHoldOnEnforce(ev) {
			t.Fatalf("expected hold for stale snapshot")
		}
		if got := FormatHoldReason(ev); got != ReasonStale {
			t.Errorf("FormatHoldReason = %q, want %q", got, ReasonStale)
		}
	})

	t.Run("mixed exhausted and stale holds", func(t *testing.T) {
		snap := &Snapshot{
			MeasuredAt: measured,
			Pools: map[string]PoolSnapshot{
				PoolClaude: {WeeklyRemainingPercent: 0, ShortRemainingPercent: intPtr(50), MeasuredAt: measured},
				PoolCodex:  {WeeklyRemainingPercent: 50, ShortRemainingPercent: intPtr(50), MeasuredAt: measured, ServiceStale: true},
			},
		}
		ev := EvaluateGate(PoolClaude, []string{PoolCodex}, snap, EvalOptions{Now: freshNow})
		if !ShouldHoldOnEnforce(ev) {
			t.Fatalf("expected hold for mixed exhausted + stale, got %+v", ev)
		}
	})

	t.Run("short_window_null only does NOT hold", func(t *testing.T) {
		snap := &Snapshot{
			MeasuredAt: measured,
			Pools: map[string]PoolSnapshot{
				PoolClaude: {WeeklyRemainingPercent: 80, ShortRemainingPercent: nil, MeasuredAt: measured},
			},
		}
		ev := EvaluateGate(PoolClaude, nil, snap, EvalOptions{Now: freshNow})
		if ev.Decision != DecisionUnknown || ev.Reason != ReasonShortWindowNull {
			t.Fatalf("unexpected eval: %+v", ev)
		}
		if ShouldHoldOnEnforce(ev) {
			t.Fatalf("provisional short_window_null must NOT hold")
		}
	})

	t.Run("unmeasured bypass does NOT hold", func(t *testing.T) {
		ev := EvaluateGate("agy-argon", nil, nil, EvalOptions{
			Now: staleNow,
			Pools: map[string]config.PoolConfig{
				"agy-argon": {Unmeasured: true},
			},
		})
		if ShouldHoldOnEnforce(ev) {
			t.Fatalf("unmeasured pool must NOT hold")
		}
	})
}

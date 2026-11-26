package quota

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/config"
)

func boolPtr(b bool) *bool { return &b }

func TestParseUsageJSONV1ValidAndNormalization(t *testing.T) {
	raw := `{
		"schema_version": 1,
		"generated_at": "2026-11-26T20:30:12Z",
		"collector": {
			"name": "quota-fleet-collector",
			"version": "1.0.0",
			"host": "collector-vm",
			"interval_seconds": 1800
		},
		"pools": {
			"demo-pool": {
				"status": "ok",
				"measured_at": "2026-11-26T20:30:10Z",
				"weekly": { "remaining_percent": 80, "reset_at": "2026-12-03T00:00:00Z" },
				"short":  { "remaining_percent": 60, "reset_at": "2026-11-27T01:30:00Z", "window": "5h" }
			},
			"short-null": {
				"measured_at": "2026-11-26T20:30:10Z",
				"weekly": { "remaining_percent": 50 },
				"short": null
			},
			"short-empty-obj": {
				"measured_at": "2026-11-26T20:30:10Z",
				"weekly": { "remaining_percent": 50 },
				"short": {}
			},
			"short-pct-null": {
				"measured_at": "2026-11-26T20:30:10Z",
				"weekly": { "remaining_percent": 50 },
				"short": { "remaining_percent": null, "window": "5h" }
			},
			"weekly-only": {
				"measured_at": "2026-11-26T20:30:11Z",
				"weekly": { "remaining_percent": 90, "reset_at": "2026-12-03T00:00:00Z", "reset_at_approx": true },
				"short": { "applicable": false }
			},
			"initial-err": {
				"status": "error",
				"error": "probe_timeout",
				"measured_at": null,
				"weekly": null
			}
		}
	}`

	snap, err := ParseUsageJSONWithFormat([]byte(raw), FormatV1)
	if err != nil {
		t.Fatalf("ParseUsageJSONWithFormat(v1): %v", err)
	}
	if snap.Format != FormatV1 || snap.SchemaVersion != 1 {
		t.Fatalf("snap metadata = format:%q sv:%d", snap.Format, snap.SchemaVersion)
	}
	if snap.Collector == nil || snap.Collector.Name != "quota-fleet-collector" || *snap.Collector.IntervalSeconds != 1800 {
		t.Fatalf("unexpected collector: %+v", snap.Collector)
	}

	demo := snap.Pools["demo-pool"]
	if demo.WeeklyRemainingPercent != 80 || demo.ShortRemainingPercent == nil || *demo.ShortRemainingPercent != 60 {
		t.Fatalf("demo-pool = %+v", demo)
	}
	for _, k := range []string{"short-null", "short-empty-obj", "short-pct-null"} {
		p := snap.Pools[k]
		if p.ShortRemainingPercent != nil {
			t.Errorf("%s ShortRemainingPercent = %v, want nil", k, *p.ShortRemainingPercent)
		}
	}
	if !snap.Pools["short-empty-obj"].ShortNormalized || !snap.Pools["short-pct-null"].ShortNormalized {
		t.Errorf("expected ShortNormalized=true for short:{} and short:{remaining_percent:null}")
	}
	if snap.Pools["short-null"].ShortNormalized {
		t.Errorf("canonical short:null should have ShortNormalized=false")
	}

	wo := snap.Pools["weekly-only"]
	if wo.ShortApplicable == nil || *wo.ShortApplicable || !wo.WeeklyResetAtApprox {
		t.Errorf("weekly-only = %+v", wo)
	}

	ie := snap.Pools["initial-err"]
	if ie.EffectiveStatus() != PoolEntryStatusError || ie.Error != "probe_timeout" || !ie.WeeklyAbsent || !ie.MeasuredAt.IsZero() {
		t.Errorf("initial-err = %+v", ie)
	}
}

func TestParseUsageJSONStrictRejectionsAndNoFallback(t *testing.T) {
	v1Valid := `{"schema_version":1,"generated_at":"2026-11-26T20:30:12Z","pools":{"p":{"measured_at":"2026-11-26T20:30:10Z","weekly":{"remaining_percent":80},"short":{"remaining_percent":50}}}}`
	legacyValid := `{"updatedAt":"2026-11-26T20:30:12Z","services":{"claude":{"remainingPercent":80,"subText":"5h 50%"}}}`

	// v1 parser rejects legacy file (no fallback).
	if _, err := ParseUsageJSONWithFormat([]byte(legacyValid), FormatV1); err == nil || ParseErrorCode(err) != ReasonFormatMismatch {
		t.Errorf("v1 on legacy = %v (code=%s), want format_mismatch", err, ParseErrorCode(err))
	}

	// Legacy parser rejects valid v1 AND invalid schema_version (no fallback, Decision 3).
	for _, raw := range []string{
		v1Valid,
		`{"schema_version":null,"updatedAt":"2026-11-26T20:30:12Z","services":{}}`,
		`{"schema_version":"1","updatedAt":"2026-11-26T20:30:12Z","services":{}}`,
		`{"schema_version":2,"updatedAt":"2026-11-26T20:30:12Z","services":{}}`,
	} {
		if _, err := ParseUsageJSONWithFormat([]byte(raw), FormatLegacy); err == nil || ParseErrorCode(err) != ReasonFormatMismatch {
			t.Errorf("legacy on %s = %v (code=%s), want format_mismatch", raw, err, ParseErrorCode(err))
		}
	}

	// Item A: Legacy without schema_version preserves json.Unmarshal (does NOT reject duplicate keys at runtime).
	legacyDup := `{"updatedAt":"2026-11-26T20:30:12Z","services":{"claude":{"remainingPercent":80,"remainingPercent":0,"subText":"5h 50%"}}}`
	if snap, err := ParseUsageJSONWithFormat([]byte(legacyDup), FormatLegacy); err != nil {
		t.Errorf("legacy duplicate key should still parse in legacy runtime mode (Item A), got err=%v", err)
	} else if snap.Pools["claude"].WeeklyRemainingPercent != 0 {
		t.Errorf("legacy dup remainingPercent = %d, want 0", snap.Pools["claude"].WeeklyRemainingPercent)
	}

	// Schema v1 strict rejections.
	badV1 := []struct {
		name     string
		raw      string
		wantCode string
	}{
		{"duplicate pool key", `{"schema_version":1,"generated_at":"2026-11-26T20:30:12Z","pools":{"a":{"measured_at":"2026-11-26T20:30:10Z","weekly":{"remaining_percent":80}},"a":{"measured_at":"2026-11-26T20:30:10Z","weekly":{"remaining_percent":10}}}}`, "duplicate_key"},
		{"schema_version null", `{"schema_version":null,"generated_at":"2026-11-26T20:30:12Z","pools":{}}`, "schema_violation"},
		{"schema_version float", `{"schema_version":1.0,"generated_at":"2026-11-26T20:30:12Z","pools":{}}`, "schema_violation"},
		{"schema_version string", `{"schema_version":"1","generated_at":"2026-11-26T20:30:12Z","pools":{}}`, "schema_violation"},
		{"schema_version 2", `{"schema_version":2,"generated_at":"2026-11-26T20:30:12Z","pools":{}}`, "schema_violation"},
		{"weekly missing remaining_percent", `{"schema_version":1,"generated_at":"2026-11-26T20:30:12Z","pools":{"a":{"measured_at":"2026-11-26T20:30:10Z","weekly":{}}}}`, "schema_violation"},
		{"weekly null remaining_percent", `{"schema_version":1,"generated_at":"2026-11-26T20:30:12Z","pools":{"a":{"measured_at":"2026-11-26T20:30:10Z","weekly":{"remaining_percent":null}}}}`, "schema_violation"},
		{"weekly float remaining_percent", `{"schema_version":1,"generated_at":"2026-11-26T20:30:12Z","pools":{"a":{"measured_at":"2026-11-26T20:30:10Z","weekly":{"remaining_percent":12.5}}}}`, "schema_violation"},
		{"short remaining_percent > 100", `{"schema_version":1,"generated_at":"2026-11-26T20:30:12Z","pools":{"a":{"measured_at":"2026-11-26T20:30:10Z","weekly":{"remaining_percent":80},"short":{"remaining_percent":101}}}}`, "schema_violation"},
		{"short remaining_percent < 0", `{"schema_version":1,"generated_at":"2026-11-26T20:30:12Z","pools":{"a":{"measured_at":"2026-11-26T20:30:10Z","weekly":{"remaining_percent":80},"short":{"remaining_percent":-1}}}}`, "schema_violation"},
		{"short remaining_percent float", `{"schema_version":1,"generated_at":"2026-11-26T20:30:12Z","pools":{"a":{"measured_at":"2026-11-26T20:30:10Z","weekly":{"remaining_percent":80},"short":{"remaining_percent":12.5}}}}`, "schema_violation"},
		{"short remaining_percent string", `{"schema_version":1,"generated_at":"2026-11-26T20:30:12Z","pools":{"a":{"measured_at":"2026-11-26T20:30:10Z","weekly":{"remaining_percent":80},"short":{"remaining_percent":"80"}}}}`, "schema_violation"},
		{"short remaining_percent bool", `{"schema_version":1,"generated_at":"2026-11-26T20:30:12Z","pools":{"a":{"measured_at":"2026-11-26T20:30:10Z","weekly":{"remaining_percent":80},"short":{"remaining_percent":true}}}}`, "schema_violation"},
		{"short applicable false with remaining_percent", `{"schema_version":1,"generated_at":"2026-11-26T20:30:12Z","pools":{"a":{"measured_at":"2026-11-26T20:30:10Z","weekly":{"remaining_percent":80},"short":{"applicable":false,"remaining_percent":50}}}}`, "schema_violation"},
		{"short applicable false with null window", `{"schema_version":1,"generated_at":"2026-11-26T20:30:12Z","pools":{"a":{"measured_at":"2026-11-26T20:30:10Z","weekly":{"remaining_percent":80},"short":{"applicable":false,"window":null}}}}`, "schema_violation"},
		{"short non-object scalar", `{"schema_version":1,"generated_at":"2026-11-26T20:30:12Z","pools":{"a":{"measured_at":"2026-11-26T20:30:10Z","weekly":{"remaining_percent":80},"short":100}}}`, "schema_violation"},
		{"ok status null measured_at", `{"schema_version":1,"generated_at":"2026-11-26T20:30:12Z","pools":{"a":{"status":"ok","measured_at":null,"weekly":{"remaining_percent":80}}}}`, "schema_violation"},
		{"ok status null weekly", `{"schema_version":1,"generated_at":"2026-11-26T20:30:12Z","pools":{"a":{"status":"ok","measured_at":"2026-11-26T20:30:10Z","weekly":null}}}`, "schema_violation"},
		{"whitespace pool key", `{"schema_version":1,"generated_at":"2026-11-26T20:30:12Z","pools":{" a":{"measured_at":"2026-11-26T20:30:10Z","weekly":{"remaining_percent":80}}}}`, "schema_violation"},
	}
	for _, tc := range badV1 {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseUsageJSONWithFormat([]byte(tc.raw), FormatV1); err == nil {
				t.Fatalf("expected error for %s, got nil", tc.name)
			} else if got := ParseErrorCode(err); got != tc.wantCode {
				t.Errorf("error code = %q, want %q (err=%v)", got, tc.wantCode, err)
			}
		})
	}
}

func TestEvaluateGateV1RulesPrioritiesAndItemsABD(t *testing.T) {
	now := time.Date(2026, 11, 26, 20, 35, 0, 0, time.UTC)
	genAt := now.Add(-2 * time.Minute)
	poolsCfg := map[string]config.PoolConfig{
		"demo-pool":       {},
		"demo-alt":        {},
		"weekly-only":     {ShortWindow: boolPtr(false)},
		"demo-unmeasured": {Unmeasured: true},
	}

	opts := EvalOptions{
		Now:             now,
		StaleAfter:      45 * time.Minute,
		Pools:           poolsCfg,
		Format:          FormatV1,
		CheckPoolConfig: true,
	}

	t.Run("generated_at future skew blocks entire file including healthy sibling", func(t *testing.T) {
		snap := &Snapshot{
			Format:      FormatV1,
			GeneratedAt: now.Add(6 * time.Minute),
			Pools: map[string]PoolSnapshot{
				"demo-pool": {WeeklyRemainingPercent: 80, ShortRemainingPercent: intPtr(60), MeasuredAt: now},
				"demo-alt":  {WeeklyRemainingPercent: 90, ShortRemainingPercent: intPtr(90), MeasuredAt: now},
			},
		}
		ev := EvaluateGate("demo-alt", nil, snap, opts)
		if ev.Decision != DecisionUnknown || ev.Reason != ReasonSnapshotError || !strings.Contains(ev.Detail, ReasonTimestampFuture) || !ShouldHoldOnEnforce(ev) {
			t.Fatalf("unexpected eval: %+v", ev)
		}
	})

	t.Run("per-pool measured_at anomaly is localized and 4a timestamp_future beats 4b timestamp_inconsistent (Item D)", func(t *testing.T) {
		snap := &Snapshot{
			Format:      FormatV1,
			GeneratedAt: now,
			Pools: map[string]PoolSnapshot{
				// measured_at = now+6m violates BOTH > now+5m (4a) AND > generated_at+60s (4b).
				"demo-pool": {WeeklyRemainingPercent: 80, ShortRemainingPercent: intPtr(60), MeasuredAt: now.Add(6 * time.Minute)},
				"demo-alt":  {WeeklyRemainingPercent: 90, ShortRemainingPercent: intPtr(90), MeasuredAt: now.Add(-1 * time.Minute)},
			},
		}
		evSingle := EvaluateGate("demo-pool", nil, snap, opts)
		if evSingle.Decision != DecisionUnknown || evSingle.Reason != ReasonTimestampFuture || !ShouldHoldOnEnforce(evSingle) {
			t.Fatalf("expected localized timestamp_future (4a > 4b), got: %+v", evSingle)
		}
		// Healthy sibling pool B is unaffected.
		evAlt := EvaluateGate("demo-pool", []string{"demo-alt"}, snap, opts)
		if evAlt.Decision != DecisionWouldPass || evAlt.SelectedPool != "demo-alt" {
			t.Fatalf("healthy sibling should pass, got: %+v", evAlt)
		}
	})

	t.Run("multi-candidate unknown aggregation picks lowest rank (collector_error rank 5 over pool_stale rank 6)", func(t *testing.T) {
		snap := &Snapshot{
			Format:      FormatV1,
			GeneratedAt: genAt,
			Pools: map[string]PoolSnapshot{
				"demo-pool": {Status: PoolEntryStatusOK, WeeklyRemainingPercent: 80, ShortRemainingPercent: intPtr(60), MeasuredAt: now.Add(-60 * time.Minute)},       // rank 6 pool_stale
				"demo-alt":  {Status: PoolEntryStatusError, Error: "probe_timeout", WeeklyRemainingPercent: 80, ShortRemainingPercent: intPtr(60), MeasuredAt: genAt}, // rank 5 collector_error
			},
		}
		ev := EvaluateGate("demo-pool", []string{"demo-alt"}, snap, opts)
		if ev.Decision != DecisionUnknown || ev.Reason != ReasonCollectorError || ev.PoolError != "probe_timeout" || !ShouldHoldOnEnforce(ev) {
			t.Fatalf("expected primary reason collector_error (rank 5 < rank 6), got: %+v", ev)
		}
	})

	t.Run("short window missing: weekly>0 holds or picks healthy alt; weekly==0 is exhausted and allows unmeasured detour", func(t *testing.T) {
		snapPos := &Snapshot{
			Format:      FormatV1,
			GeneratedAt: genAt,
			Pools: map[string]PoolSnapshot{
				"demo-pool": {WeeklyRemainingPercent: 80, ShortRemainingPercent: nil, MeasuredAt: genAt},
				"demo-alt":  {WeeklyRemainingPercent: 70, ShortRemainingPercent: intPtr(50), MeasuredAt: genAt},
			},
		}
		evHold := EvaluateGate("demo-pool", nil, snapPos, opts)
		if evHold.Decision != DecisionUnknown || evHold.Reason != ReasonShortWindowMissing || !ShouldHoldOnEnforce(evHold) {
			t.Fatalf("weekly>0 + short missing single: %+v", evHold)
		}
		evAlt := EvaluateGate("demo-pool", []string{"demo-alt"}, snapPos, opts)
		if evAlt.Decision != DecisionWouldPass || evAlt.SelectedPool != "demo-alt" {
			t.Fatalf("weekly>0 + short missing with healthy alt: %+v", evAlt)
		}

		snapZero := &Snapshot{
			Format:      FormatV1,
			GeneratedAt: genAt,
			Pools: map[string]PoolSnapshot{
				"demo-pool": {WeeklyRemainingPercent: 0, ShortRemainingPercent: nil, MeasuredAt: genAt},
			},
		}
		evZero := EvaluateGate("demo-pool", nil, snapZero, opts)
		if evZero.Decision != DecisionWouldHold || evZero.BlockedBy != BlockedByWeekly {
			t.Fatalf("weekly==0 + short missing should be exhausted:weekly, got %+v", evZero)
		}
		evZeroDetour := EvaluateGate("demo-pool", []string{"demo-unmeasured"}, snapZero, opts)
		if evZeroDetour.Decision != DecisionWouldPass || evZeroDetour.SelectedPool != "demo-unmeasured" || !evZeroDetour.Bypassed {
			t.Fatalf("weekly==0 + short missing + unmeasured alt should detour, got %+v", evZeroDetour)
		}
	})

	t.Run("Decision 6: exhausted + short_window_missing + unmeasured alt holds without detour; all exhausted detours", func(t *testing.T) {
		snapMixed := &Snapshot{
			Format:      FormatV1,
			GeneratedAt: genAt,
			Pools: map[string]PoolSnapshot{
				"demo-pool": {WeeklyRemainingPercent: 0, ShortRemainingPercent: intPtr(50), MeasuredAt: genAt}, // exhausted
				"demo-alt":  {WeeklyRemainingPercent: 80, ShortRemainingPercent: nil, MeasuredAt: genAt},       // short_window_missing
			},
		}
		ev := EvaluateGate("demo-pool", []string{"demo-alt", "demo-unmeasured"}, snapMixed, opts)
		if ev.Decision != DecisionUnknown || ev.Reason != ReasonShortWindowMissing || ev.Bypassed || !ShouldHoldOnEnforce(ev) {
			t.Fatalf("mixed exhausted + unknown + unmeasured must NOT detour, got %+v", ev)
		}

		snapAllExh := &Snapshot{
			Format:      FormatV1,
			GeneratedAt: genAt,
			Pools: map[string]PoolSnapshot{
				"demo-pool": {WeeklyRemainingPercent: 0, ShortRemainingPercent: intPtr(50), MeasuredAt: genAt},
				"demo-alt":  {WeeklyRemainingPercent: 50, ShortRemainingPercent: intPtr(0), MeasuredAt: genAt},
			},
		}
		evDetour := EvaluateGate("demo-pool", []string{"demo-alt", "demo-unmeasured"}, snapAllExh, opts)
		if evDetour.Decision != DecisionWouldPass || evDetour.SelectedPool != "demo-unmeasured" || !evDetour.Bypassed {
			t.Fatalf("all measured exhausted should detour to demo-unmeasured, got %+v", evDetour)
		}
	})

	t.Run("short_window authority in fleet.yaml", func(t *testing.T) {
		snap := &Snapshot{
			Format:      FormatV1,
			GeneratedAt: genAt,
			Pools: map[string]PoolSnapshot{
				"demo-pool":   {WeeklyRemainingPercent: 80, ShortApplicable: boolPtr(false), MeasuredAt: genAt},
				"weekly-only": {WeeklyRemainingPercent: 80, ShortApplicable: boolPtr(false), MeasuredAt: genAt},
			},
		}
		evMismatch := EvaluateGate("demo-pool", nil, snap, opts)
		if evMismatch.Decision != DecisionUnknown || evMismatch.Reason != ReasonShortWindowMismatch || !ShouldHoldOnEnforce(evMismatch) {
			t.Fatalf("short_window_mismatch expected, got %+v", evMismatch)
		}
		evExempt := EvaluateGate("weekly-only", nil, snap, opts)
		if evExempt.Decision != DecisionWouldPass || evExempt.SelectedPool != "weekly-only" {
			t.Fatalf("weekly-only should pass, got %+v", evExempt)
		}
	})

	t.Run("Step 1 undefined pool runs before bypass and snapshot error (Item B)", func(t *testing.T) {
		// Unmeasured primary + undefined alt -> pool_not_configured (NOT bypass!).
		ev1 := EvaluateGate("demo-unmeasured", []string{"typo-pool"}, nil, opts)
		if ev1.Decision != DecisionUnknown || ev1.Reason != ReasonPoolNotConfigured || ev1.Bypassed {
			t.Fatalf("unmeasured + undefined alt should yield pool_not_configured, got %+v", ev1)
		}
		// Measured primary + undefined alt + nil (corrupt/missing) snapshot -> pool_not_configured (NOT snapshot_error!).
		ev2 := EvaluateGate("demo-pool", []string{"typo-pool"}, nil, opts)
		if ev2.Decision != DecisionUnknown || ev2.Reason != ReasonPoolNotConfigured {
			t.Fatalf("measured + undefined alt + nil snap should yield pool_not_configured, got %+v", ev2)
		}
		// Zero measured candidates + all defined unmeasured alts -> bypass pass (§4.2 ②-b).
		ev3 := EvaluateGate("", []string{"demo-unmeasured"}, nil, opts)
		if ev3.Decision != DecisionWouldPass || ev3.SelectedPool != "demo-unmeasured" || !ev3.Bypassed {
			t.Fatalf("empty primary + unmeasured alt only should bypass, got %+v", ev3)
		}
	})

	t.Run("v1 weekly reset ordering ignores past and approx resets (§3.5)", func(t *testing.T) {
		past := now.Add(-1 * time.Hour)
		soonApprox := now.Add(2 * time.Hour)
		realFuture := now.Add(24 * time.Hour)
		snap := &Snapshot{
			Format:      FormatV1,
			GeneratedAt: genAt,
			Pools: map[string]PoolSnapshot{
				"demo-pool":   {WeeklyRemainingPercent: 80, ShortRemainingPercent: intPtr(80), WeeklyResetAt: &past, MeasuredAt: genAt},
				"demo-alt":    {WeeklyRemainingPercent: 80, ShortRemainingPercent: intPtr(80), WeeklyResetAt: &realFuture, MeasuredAt: genAt},
				"weekly-only": {WeeklyRemainingPercent: 80, ShortRemainingPercent: intPtr(80), WeeklyResetAt: &soonApprox, WeeklyResetAtApprox: true, MeasuredAt: genAt},
			},
		}
		evV1 := EvaluateGate("demo-pool", []string{"weekly-only", "demo-alt"}, snap, opts)
		if evV1.SelectedPool != "demo-alt" {
			t.Fatalf("v1 should prefer realFuture demo-alt over past and approx, got %q", evV1.SelectedPool)
		}
		// Legacy keeps old resetBefore (past wins) and CompareLegacyWithV1 surfaces the diff.
		legacyOpts := opts
		legacyOpts.Format = FormatLegacy
		legacyOpts.CheckPoolConfig = false
		evLeg := EvaluateGate("demo-pool", []string{"demo-alt"}, snap, legacyOpts)
		if evLeg.SelectedPool != "demo-pool" {
			t.Fatalf("legacy should keep past resetBefore selecting demo-pool, got %q", evLeg.SelectedPool)
		}
		diffs := CompareLegacyWithV1("demo-pool", []string{"demo-alt"}, snap, legacyOpts, evLeg)
		if len(diffs) == 0 || !strings.Contains(strings.Join(diffs, ";"), "selected_pool_diff") {
			t.Errorf("expected selected_pool_diff in legacy diffs, got %v", diffs)
		}
	})

	t.Run("HeldRelease config-change rules (Item B & §4.2.C)", func(t *testing.T) {
		relOpts := opts
		relOpts.HeldRelease = true
		snap := &Snapshot{
			Format:      FormatV1,
			GeneratedAt: genAt,
			Pools: map[string]PoolSnapshot{
				"demo-pool": {WeeklyRemainingPercent: 80, ShortRemainingPercent: intPtr(80), MeasuredAt: genAt},
			},
		}
		// Primary changed to unmeasured + alt deleted -> bypass release to primary with UndefinedPools recorded!
		evUnm := EvaluateGate("demo-unmeasured", []string{"deleted-alt"}, snap, relOpts)
		if evUnm.Decision != DecisionWouldPass || evUnm.SelectedPool != "demo-unmeasured" || !evUnm.Bypassed || len(evUnm.UndefinedPools) != 1 {
			t.Fatalf("held primary unmeasured + deleted alt should bypass with warning list, got %+v", evUnm)
		}
		// All candidates deleted -> pool_not_configured.
		evAllDel := EvaluateGate("deleted-a", []string{"deleted-b"}, snap, relOpts)
		if evAllDel.Decision != DecisionUnknown || evAllDel.Reason != ReasonPoolNotConfigured {
			t.Fatalf("all deleted held candidates should yield pool_not_configured, got %+v", evAllDel)
		}
		// Partial deletion with healthy remaining candidate -> release to healthy candidate.
		evPartOK := EvaluateGate("deleted-a", []string{"demo-pool"}, snap, relOpts)
		if evPartOK.Decision != DecisionWouldPass || evPartOK.SelectedPool != "demo-pool" || len(evPartOK.UndefinedPools) != 1 {
			t.Fatalf("partial deletion with healthy remaining should release, got %+v", evPartOK)
		}
		// Partial deletion with exhausted remaining + unmeasured alt -> deleted unknown blocks detour!
		snapExh := &Snapshot{
			Format:      FormatV1,
			GeneratedAt: genAt,
			Pools: map[string]PoolSnapshot{
				"demo-pool": {WeeklyRemainingPercent: 0, ShortRemainingPercent: intPtr(80), MeasuredAt: genAt},
			},
		}
		evPartBlock := EvaluateGate("deleted-a", []string{"demo-pool", "demo-unmeasured"}, snapExh, relOpts)
		if evPartBlock.Decision != DecisionUnknown || evPartBlock.Reason != ReasonPoolNotConfigured || evPartBlock.Bypassed {
			t.Fatalf("partial deletion with no healthy measured must not detour to unmeasured, got %+v", evPartBlock)
		}
	})
}

func TestValidateUsageFileModesAndItemsCD(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 26, 21, 0, 0, 0, time.UTC)

	writeFile := func(name, content string) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatalf("WriteFile %s: %v", name, err)
		}
		return p
	}

	cfg := &config.Config{
		InstanceID: "val-test",
		Agents: map[string]config.AgentConfig{
			"claude": {Launch: "claude -p"},
			"codex":  {Launch: "codex exec"},
		},
		Pools: map[string]config.PoolConfig{
			"demo-pool": {AppliesTo: []config.PoolTarget{{Agent: "claude"}}},
			"demo-alt":  {},
		},
		Quota: config.QuotaConfig{
			UsageFormat: config.UsageFormatV1,
			StaleAfter:  "45m",
		},
	}

	// 1. Partial failure (status=error + healthy sibling): normal=exit 2 (even --ignore-stale), --check-deploy=exit 0 (degraded).
	partialPath := writeFile("partial.json", `{
		"schema_version": 1,
		"generated_at": "2026-11-26T20:55:00Z",
		"pools": {
			"demo-pool": {"status":"error","error":"probe_timeout","measured_at":"2026-11-26T20:50:00Z","weekly":{"remaining_percent":80},"short":{"remaining_percent":60}},
			"demo-alt":  {"status":"ok","measured_at":"2026-11-26T20:54:50Z","weekly":{"remaining_percent":90},"short":{"remaining_percent":80}}
		}
	}`)
	rNorm := ValidateUsageFile(ValidateOptions{File: partialPath, Config: cfg, Format: FormatV1, Now: now})
	if rNorm.ExitCode != 2 || !rNorm.Degraded || !rNorm.Deployable {
		t.Errorf("partial normal = exit %d degraded=%t deployable=%t, want 2/true/true", rNorm.ExitCode, rNorm.Degraded, rNorm.Deployable)
	}
	rIgn := ValidateUsageFile(ValidateOptions{File: partialPath, Config: cfg, Format: FormatV1, IgnoreStale: true, Now: now})
	if rIgn.ExitCode != 2 {
		t.Errorf("partial --ignore-stale = exit %d, want 2", rIgn.ExitCode)
	}
	rDeploy := ValidateUsageFile(ValidateOptions{File: partialPath, Config: cfg, Format: FormatV1, CheckDeploy: true, Now: now})
	if rDeploy.ExitCode != 0 || !rDeploy.Degraded || !rDeploy.Deployable {
		t.Errorf("partial --check-deploy = exit %d degraded=%t, want 0/true", rDeploy.ExitCode, rDeploy.Degraded)
	}

	// 2. Item D: snapshot_stale + per-pool timestamp_inconsistent -> exit 1 in normal, --ignore-stale, AND --check-deploy!
	staleAndInconsistent := writeFile("stale_inconsistent.json", `{
		"schema_version": 1,
		"generated_at": "2026-11-26T19:00:00Z",
		"pools": {
			"demo-pool": {"status":"ok","measured_at":"2026-11-26T19:05:00Z","weekly":{"remaining_percent":80},"short":{"remaining_percent":60}},
			"demo-alt":  {"status":"ok","measured_at":"2026-11-26T18:59:50Z","weekly":{"remaining_percent":80},"short":{"remaining_percent":60}}
		}
	}`)
	for _, tc := range []struct {
		name string
		opts ValidateOptions
	}{
		{"normal", ValidateOptions{File: staleAndInconsistent, Format: FormatV1, Now: now}},
		{"ignore-stale", ValidateOptions{File: staleAndInconsistent, Format: FormatV1, IgnoreStale: true, Now: now}},
		{"check-deploy", ValidateOptions{File: staleAndInconsistent, Format: FormatV1, CheckDeploy: true, Now: now}},
	} {
		r := ValidateUsageFile(tc.opts)
		if r.ExitCode != 1 {
			t.Errorf("Item D (%s): exit=%d, want 1; diags=%+v", tc.name, r.ExitCode, r.Diagnostics)
		}
	}

	// 3. snapshot_stale + missing configured pool -> exit 1 (exit 1 > exit 2).
	staleMissingPool := writeFile("stale_missing.json", `{
		"schema_version": 1,
		"generated_at": "2026-11-26T19:00:00Z",
		"pools": {
			"demo-pool": {"status":"ok","measured_at":"2026-11-26T18:59:50Z","weekly":{"remaining_percent":80},"short":{"remaining_percent":60}}
		}
	}`)
	if r := ValidateUsageFile(ValidateOptions{File: staleMissingPool, Config: cfg, Format: FormatV1, Now: now}); r.ExitCode != 1 {
		t.Errorf("stale + pool_not_found: exit=%d, want 1", r.ExitCode)
	}

	// 4. Weekly 0 + short missing -> exit 0 (info only). Without --config, applicable=false -> exit 0 (info unverified).
	weeklyZeroPath := writeFile("weekly_zero.json", `{
		"schema_version": 1,
		"generated_at": "2026-11-26T20:55:00Z",
		"pools": {
			"demo-pool": {"status":"ok","measured_at":"2026-11-26T20:54:50Z","weekly":{"remaining_percent":0},"short":{}},
			"demo-alt":  {"status":"ok","measured_at":"2026-11-26T20:54:50Z","weekly":{"remaining_percent":80},"short":{"applicable":false}}
		}
	}`)
	if r := ValidateUsageFile(ValidateOptions{File: weeklyZeroPath, Format: FormatV1, Now: now}); r.ExitCode != 0 {
		t.Errorf("weekly 0 + applicable=false without --config: exit=%d, want 0; diags=%+v", r.ExitCode, r.Diagnostics)
	}
	if r := ValidateUsageFile(ValidateOptions{File: weeklyZeroPath, Config: cfg, Format: FormatV1, Now: now}); r.ExitCode != 1 {
		t.Errorf("applicable=false with --config (short_window=true): exit=%d, want 1 (short_window_mismatch)", r.ExitCode)
	}

	// 5. Item C: --reject-older-than (older rejected, equal+same SHA256 ok, equal+diff SHA256 conflict, future-clock existing skipped, stale/error existing still protects).
	existingPath := writeFile("existing.json", `{
		"schema_version": 1,
		"generated_at": "2026-11-26T20:55:00Z",
		"pools": {
			"demo-pool": {"status":"ok","measured_at":"2026-11-26T20:54:50Z","weekly":{"remaining_percent":80},"short":{"remaining_percent":60}}
		}
	}`)
	olderPath := writeFile("older.json", `{
		"schema_version": 1,
		"generated_at": "2026-11-26T20:50:00Z",
		"pools": {
			"demo-pool": {"status":"ok","measured_at":"2026-11-26T20:49:50Z","weekly":{"remaining_percent":80},"short":{"remaining_percent":60}}
		}
	}`)
	sameTimeDiffContent := writeFile("same_time_diff.json", `{
		"schema_version": 1,
		"generated_at": "2026-11-26T20:55:00Z",
		"pools": {
			"demo-pool": {"status":"ok","measured_at":"2026-11-26T20:54:50Z","weekly":{"remaining_percent":10},"short":null}
		}
	}`)
	futureExisting := writeFile("future_existing.json", `{
		"schema_version": 1,
		"generated_at": "2026-11-26T22:00:00Z",
		"pools": {
			"demo-pool": {"status":"ok","measured_at":"2026-11-26T20:54:50Z","weekly":{"remaining_percent":80},"short":{"remaining_percent":60}}
		}
	}`)
	staleExisting := writeFile("stale_existing.json", `{
		"schema_version": 1,
		"generated_at": "2026-11-26T19:50:00Z",
		"pools": {
			"demo-pool": {"status":"error","error":"probe_timeout","measured_at":"2026-11-26T19:49:00Z","weekly":{"remaining_percent":80},"short":{"remaining_percent":60}}
		}
	}`)
	evenOlderPath := writeFile("even_older.json", `{
		"schema_version": 1,
		"generated_at": "2026-11-26T19:40:00Z",
		"pools": {
			"demo-pool": {"status":"ok","measured_at":"2026-11-26T19:39:50Z","weekly":{"remaining_percent":80},"short":{"remaining_percent":60}}
		}
	}`)

	if r := ValidateUsageFile(ValidateOptions{File: olderPath, Format: FormatV1, CheckDeploy: true, RejectOlderThan: existingPath, Now: now}); r.ExitCode != 1 {
		t.Errorf("older generation: exit=%d, want 1", r.ExitCode)
	}
	if r := ValidateUsageFile(ValidateOptions{File: existingPath, Format: FormatV1, CheckDeploy: true, RejectOlderThan: existingPath, Now: now}); r.ExitCode != 0 {
		t.Errorf("equal timestamp + identical SHA256: exit=%d, want 0", r.ExitCode)
	}
	if r := ValidateUsageFile(ValidateOptions{File: sameTimeDiffContent, Format: FormatV1, CheckDeploy: true, RejectOlderThan: existingPath, Now: now}); r.ExitCode != 1 {
		t.Errorf("Item C equal timestamp + different content: exit=%d, want 1 (generation_conflict); diags=%+v", r.ExitCode, r.Diagnostics)
	}
	if r := ValidateUsageFile(ValidateOptions{File: existingPath, Format: FormatV1, CheckDeploy: true, RejectOlderThan: futureExisting, Now: now}); r.ExitCode != 0 {
		t.Errorf("Item C clock recovery over future-skewed existing: exit=%d, want 0; diags=%+v", r.ExitCode, r.Diagnostics)
	}
	if r := ValidateUsageFile(ValidateOptions{File: evenOlderPath, Format: FormatV1, CheckDeploy: true, RejectOlderThan: staleExisting, Now: now}); r.ExitCode != 1 {
		t.Errorf("Item C stale/error existing must still reject older generation: exit=%d, want 1", r.ExitCode)
	}
}

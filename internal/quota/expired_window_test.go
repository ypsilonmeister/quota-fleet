package quota

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/config"
)

// Issue #42: a window whose reset_at has passed keeps reporting its old remaining
// percentage until the window is next used, so it must not count as exhaustion.

var expiredWindowNow = time.Date(2026, 11, 28, 6, 6, 0, 0, time.UTC)

const (
	pastReset   = "2026-11-28T04:49:59Z"
	futureReset = "2026-11-28T11:10:00Z"
)

type testWindow struct {
	pct    int
	reset  string // empty means no reset_at
	approx bool   // Schema v1 only
}

// expiredWindowSnapshot builds a fresh single-pool ("claude") snapshot through the real
// parser of the given format. A nil short window is a missing short value.
func expiredWindowSnapshot(t *testing.T, format string, weekly testWindow, short *testWindow) *Snapshot {
	t.Helper()
	var raw string
	if format == FormatV1 {
		window := func(w testWindow) string {
			reset := "null"
			if w.reset != "" {
				reset = strconv.Quote(w.reset)
			}
			return fmt.Sprintf(`{"remaining_percent":%d,"reset_at":%s,"reset_at_approx":%t}`, w.pct, reset, w.approx)
		}
		shortJSON := "null"
		if short != nil {
			shortJSON = window(*short)
		}
		raw = fmt.Sprintf(`{"schema_version":1,"generated_at":"2026-11-28T06:05:00Z","pools":{"claude":{"status":"ok","measured_at":"2026-11-28T06:04:50Z","weekly":%s,"short":%s}}}`,
			window(weekly), shortJSON)
	} else {
		subText, subReset := "", ""
		if short != nil {
			subText, subReset = fmt.Sprintf("5h %d%%", short.pct), short.reset
		}
		raw = fmt.Sprintf(`{"updatedAt":"2026-11-28T06:05:00Z","services":{"claude":{"remainingPercent":%d,"resetAt":%q,"subText":%q,"subResetAt":%q}}}`,
			weekly.pct, weekly.reset, subText, subReset)
	}
	snap, err := ParseUsageJSONWithFormat([]byte(raw), format)
	if err != nil {
		t.Fatalf("parse %s snapshot: %v\n%s", format, err, raw)
	}
	return snap
}

func TestGateExpiredWindowIsNotExhaustion(t *testing.T) {
	ok := testWindow{pct: 80, reset: futureReset}
	zeroPast := testWindow{pct: 0, reset: pastReset}
	zeroFuture := testWindow{pct: 0, reset: futureReset}
	zeroUnknown := testWindow{pct: 0}
	zeroAtNow := testWindow{pct: 0, reset: expiredWindowNow.Format(time.RFC3339)}
	zeroSentinel := testWindow{pct: 0, reset: "0001-01-01T00:00:00Z"}
	// Same past instant written with a non-UTC offset.
	zeroPastJST := testWindow{pct: 0, reset: "2026-11-28T13:49:59+09:00"}
	// A future instant whose wall-clock text looks earlier than now (UTC).
	zeroFutureWest := testWindow{pct: 0, reset: "2026-11-28T05:00:00-09:00"}

	cases := []struct {
		name      string
		weekly    testWindow
		short     testWindow
		blockedBy string // empty means healthy
	}{
		{"short 0% reset past", ok, zeroPast, ""},
		{"weekly 0% reset past", zeroPast, ok, ""},
		{"both 0% reset past", zeroPast, zeroPast, ""},
		{"short 0% reset past with offset", ok, zeroPastJST, ""},
		{"short 0% reset future", ok, zeroFuture, BlockedByShort},
		{"weekly 0% reset future", zeroFuture, ok, BlockedByWeekly},
		{"both 0% reset future", zeroFuture, zeroFuture, BlockedByWeeklyAnd5},
		{"short 0% reset future with offset", ok, zeroFutureWest, BlockedByShort},
		{"weekly expired, short still exhausted", zeroPast, zeroFuture, BlockedByShort},
		{"short expired, weekly still exhausted", zeroFuture, zeroPast, BlockedByWeekly},
		{"short 0% reset unknown", ok, zeroUnknown, BlockedByShort},
		{"weekly 0% reset unknown", zeroUnknown, ok, BlockedByWeekly},
		{"short 0% reset equals now", ok, zeroAtNow, BlockedByShort},
		{"weekly 0% reset equals now", zeroAtNow, ok, BlockedByWeekly},
		{"short 0% reset zero time", ok, zeroSentinel, BlockedByShort},
		{"weekly 0% reset zero time", zeroSentinel, ok, BlockedByWeekly},
	}

	for _, format := range []string{FormatLegacy, FormatV1} {
		for _, tc := range cases {
			t.Run(format+"/"+tc.name, func(t *testing.T) {
				short := tc.short
				snap := expiredWindowSnapshot(t, format, tc.weekly, &short)
				eval := EvaluateGate("claude", nil, snap, EvalOptions{Now: expiredWindowNow, Format: format})
				if len(eval.Pools) != 1 {
					t.Fatalf("pools = %+v", eval.Pools)
				}
				pool := eval.Pools[0]
				if tc.blockedBy == "" {
					if pool.Status != PoolHealthy || pool.BlockedBy != "" {
						t.Errorf("pool = %s blocked_by=%q, want healthy", pool.Status, pool.BlockedBy)
					}
					if eval.Decision != DecisionWouldPass || eval.SelectedPool != "claude" || ShouldHoldOnEnforce(eval) {
						t.Errorf("gate = %s selected=%q hold=%t, want pass without hold", eval.Decision, eval.SelectedPool, ShouldHoldOnEnforce(eval))
					}
				} else {
					if pool.Status != PoolExhausted || pool.BlockedBy != tc.blockedBy {
						t.Errorf("pool = %s blocked_by=%q, want exhausted %q", pool.Status, pool.BlockedBy, tc.blockedBy)
					}
					if eval.Decision != DecisionWouldHold || eval.BlockedBy != tc.blockedBy || !ShouldHoldOnEnforce(eval) {
						t.Errorf("gate = %s blocked_by=%q hold=%t, want hold %q", eval.Decision, eval.BlockedBy, ShouldHoldOnEnforce(eval), tc.blockedBy)
					}
				}
				// The measured values are interpreted, never rewritten.
				if pool.Snapshot.WeeklyRemainingPercent != tc.weekly.pct || pool.Snapshot.ShortRemainingPercent == nil ||
					*pool.Snapshot.ShortRemainingPercent != tc.short.pct {
					t.Errorf("remaining percentages were rewritten: %+v", pool.Snapshot)
				}
			})
		}
	}
}

// reset_at_approx does not change the expiry rule.
func TestGateExpiredWindowApproxReset(t *testing.T) {
	short := testWindow{pct: 0, reset: pastReset, approx: true}
	snap := expiredWindowSnapshot(t, FormatV1, testWindow{pct: 0, reset: pastReset, approx: true}, &short)
	if ev := EvaluatePool("claude", snap, EvalOptions{Now: expiredWindowNow, Format: FormatV1}); ev.Status != PoolHealthy {
		t.Errorf("approx past reset = %s blocked_by=%q, want healthy", ev.Status, ev.BlockedBy)
	}
}

// An expired weekly window only stops being an exhaustion reason: without a short value
// the pool is still held, now because the short window is missing.
func TestGateExpiredWeeklyWithMissingShortHolds(t *testing.T) {
	for _, format := range []string{FormatLegacy, FormatV1} {
		snap := expiredWindowSnapshot(t, format, testWindow{pct: 0, reset: pastReset}, nil)
		eval := EvaluateGate("claude", nil, snap, EvalOptions{Now: expiredWindowNow, Format: format})
		if eval.Decision != DecisionUnknown || eval.Reason != ReasonShortWindowMissing || eval.BlockedBy != "" || !ShouldHoldOnEnforce(eval) {
			t.Errorf("%s: gate = %s reason=%q blocked_by=%q hold=%t, want unknown %q hold",
				format, eval.Decision, eval.Reason, eval.BlockedBy, ShouldHoldOnEnforce(eval), ReasonShortWindowMissing)
		}
	}
}

// Legacy keeps passing a missing short value (short_window_null) when the weekly window
// has quota left, and the new hold combines with other candidates like any held pool.
func TestGateLegacyMissingShortCandidates(t *testing.T) {
	const (
		expired   = `{"remainingPercent":0,"resetAt":"` + pastReset + `"}`
		remaining = `{"remainingPercent":80,"resetAt":"` + pastReset + `"}`
		exhausted = `{"remainingPercent":0,"resetAt":"` + futureReset + `","subText":"5h 80%"}`
		healthy   = `{"remainingPercent":80,"resetAt":"` + futureReset + `","subText":"5h 80%"}`
	)
	for _, tc := range []struct {
		name         string
		claude       string
		codex        string // empty means claude is the only candidate
		wantHold     bool
		wantSelected string
	}{
		{"weekly remaining, short missing", remaining, "", false, ""},
		{"expired, short missing", expired, "", true, ""},
		{"expired, alt exhausted", expired, exhausted, true, ""},
		{"exhausted, alt expired", exhausted, expired, true, ""},
		{"expired, alt healthy", expired, healthy, false, "codex"},
		{"expired, alt short_window_null", expired, remaining, false, ""},
		{"short_window_null, alt expired", remaining, expired, false, ""},
	} {
		services := `"claude":` + tc.claude
		var alts []string
		if tc.codex != "" {
			services += `,"codex":` + tc.codex
			alts = []string{"codex"}
		}
		snap, err := ParseUsageJSON([]byte(`{"updatedAt":"2026-11-28T06:05:00Z","services":{` + services + `}}`))
		if err != nil {
			t.Fatalf("%s: parse: %v", tc.name, err)
		}
		eval := EvaluateGate("claude", alts, snap, EvalOptions{Now: expiredWindowNow, Format: FormatLegacy})
		if ShouldHoldOnEnforce(eval) != tc.wantHold || eval.SelectedPool != tc.wantSelected {
			t.Errorf("%s: gate = %s reason=%q selected=%q hold=%t, want selected=%q hold=%t",
				tc.name, eval.Decision, eval.Reason, eval.SelectedPool, ShouldHoldOnEnforce(eval), tc.wantSelected, tc.wantHold)
		}
	}
}

// The Legacy parser passes negative percentages through. They are not valid observations,
// so a passed reset time does not clear them.
func TestGateExpiredWindowKeepsNegativeRemainingExhausted(t *testing.T) {
	ok := testWindow{pct: 80, reset: futureReset}
	negative := testWindow{pct: -1, reset: pastReset}
	for _, short := range []*testWindow{&ok, nil} {
		snap := expiredWindowSnapshot(t, FormatLegacy, negative, short)
		eval := EvaluateGate("claude", nil, snap, EvalOptions{Now: expiredWindowNow, Format: FormatLegacy})
		if eval.Decision != DecisionWouldHold || eval.BlockedBy != BlockedByWeekly || !ShouldHoldOnEnforce(eval) {
			t.Errorf("short=%v: gate = %s blocked_by=%q, want would_hold weekly", short, eval.Decision, eval.BlockedBy)
		}
	}

	// The text parser cannot produce a negative short value; the predicate treats it alike.
	snap := expiredWindowSnapshot(t, FormatLegacy, ok, &testWindow{pct: 0, reset: pastReset})
	*snap.Pools["claude"].ShortRemainingPercent = -1
	ev := EvaluatePool("claude", snap, EvalOptions{Now: expiredWindowNow, Format: FormatLegacy})
	if ev.Status != PoolExhausted || ev.BlockedBy != BlockedByShort {
		t.Errorf("negative short = %s blocked_by=%q, want exhausted 5h", ev.Status, ev.BlockedBy)
	}
}

// Expired windows do not override the checks that reject an untrustworthy snapshot.
func TestGateExpiredWindowKeepsFailClosed(t *testing.T) {
	zero := testWindow{pct: 0, reset: pastReset}
	for _, format := range []string{FormatLegacy, FormatV1} {
		snap := expiredWindowSnapshot(t, format, zero, &zero)
		eval := EvaluateGate("claude", nil, snap, EvalOptions{Now: expiredWindowNow.Add(2 * time.Hour), Format: format})
		if eval.Decision != DecisionUnknown || eval.Reason != ReasonStale || !ShouldHoldOnEnforce(eval) {
			t.Errorf("%s stale: gate = %s reason=%q, want unknown stale hold", format, eval.Decision, eval.Reason)
		}
	}

	snap := expiredWindowSnapshot(t, FormatV1, zero, &zero)
	ps := snap.Pools["claude"]
	ps.Status = PoolEntryStatusError
	snap.Pools["claude"] = ps
	eval := EvaluateGate("claude", nil, snap, EvalOptions{Now: expiredWindowNow, Format: FormatV1})
	if eval.Decision != DecisionUnknown || eval.Reason != ReasonCollectorError || !ShouldHoldOnEnforce(eval) {
		t.Errorf("collector error: gate = %s reason=%q, want unknown collector_error hold", eval.Decision, eval.Reason)
	}

	for format, raw := range map[string]string{
		FormatV1:     `{"schema_version":1,"generated_at":"2026-11-28T06:05:00Z","pools":{"claude":{"measured_at":"2026-11-28T06:04:50Z","weekly":{"remaining_percent":80},"short":{"remaining_percent":0,"reset_at":"13:49"}}}}`,
		FormatLegacy: `{"updatedAt":"2026-11-28T06:05:00Z","services":{"claude":{"remainingPercent":80,"subText":"5h 0%","subResetAt":"13:49"}}}`,
	} {
		if _, err := ParseUsageJSONWithFormat([]byte(raw), format); err == nil {
			t.Errorf("%s: unparseable reset_at must still be a parse error", format)
		}
	}
}

// usage validate must not call an expired weekly 0% "definitively exhausted".
func TestValidateExpiredWeeklyWithMissingShort(t *testing.T) {
	files := map[string]func(reset string) string{
		FormatV1: func(reset string) string {
			return fmt.Sprintf(`{"schema_version":1,"generated_at":"2026-11-28T06:05:00Z","pools":{"claude":{"measured_at":"2026-11-28T06:04:50Z","weekly":{"remaining_percent":0,"reset_at":%q},"short":null}}}`, reset)
		},
		FormatLegacy: func(reset string) string {
			return fmt.Sprintf(`{"updatedAt":"2026-11-28T06:05:00Z","services":{"claude":{"remainingPercent":0,"resetAt":%q}}}`, reset)
		},
	}
	cfg := &config.Config{Pools: map[string]config.PoolConfig{"claude": {}}}
	for format, build := range files {
		for _, withConfig := range []*config.Config{nil, cfg} {
			for _, tc := range []struct {
				reset      string
				wantExit   int
				wantHealth PoolHealthStatus
				wantCode   string
			}{
				{pastReset, 2, PoolUnknown, ReasonShortWindowMissing},
				{futureReset, 0, PoolExhausted, "short_window_missing_while_weekly_exhausted"},
			} {
				path := filepath.Join(t.TempDir(), "usage.json")
				if err := os.WriteFile(path, []byte(build(tc.reset)), 0o600); err != nil {
					t.Fatalf("WriteFile: %v", err)
				}
				res := ValidateUsageFile(ValidateOptions{File: path, Config: withConfig, Format: format, Now: expiredWindowNow})
				hasCode := false
				for _, d := range res.Diagnostics {
					hasCode = hasCode || d.Code == tc.wantCode
				}
				// Both formats hold either way, so there is no enforce difference to report.
				for _, diff := range res.LegacyDiffs {
					if strings.Contains(diff, "passes in legacy enforce") {
						t.Errorf("%s reset=%s: wrong legacy diff %q", format, tc.reset, diff)
					}
				}
				for _, d := range res.Diagnostics {
					if strings.Contains(d.Message, "legacy enforce passes") {
						t.Errorf("%s reset=%s: wrong diagnostic %q", format, tc.reset, d.Message)
					}
				}
				if res.ExitCode != tc.wantExit || len(res.Pools) != 1 || res.Pools[0].Health != string(tc.wantHealth) || !hasCode {
					t.Errorf("%s config=%t reset=%s: exit=%d pools=%+v diagnostics=%+v, want exit %d health %s code %s",
						format, withConfig != nil, tc.reset, res.ExitCode, res.Pools, res.Diagnostics, tc.wantExit, tc.wantHealth, tc.wantCode)
				}
			}
		}
	}
}

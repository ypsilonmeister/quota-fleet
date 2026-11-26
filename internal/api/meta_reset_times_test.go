package api

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/quota"
)

func TestMetaPoolSummariesResetTimesLegacy(t *testing.T) {
	usagePath := filepath.Join(t.TempDir(), "usage-legacy.json")
	legacyJSON := `{
		"updatedAt": "2026-11-27T08:00:00+09:00",
		"services": {
			"claude": {
				"remainingPercent": 83,
				"resetAt": "2026-12-03T20:00:00+09:00",
				"subText": "5h 98%",
				"subResetAt": "2026-11-27T12:30:00+09:00"
			},
			"codex": {
				"remainingPercent": 38,
				"resetAt": "2026-12-01T14:51:23+09:00",
				"subText": "5h 70%",
				"subResetAt": "2026-11-27T10:36:25+09:00"
			},
			"antigravity": {
				"remainingPercent": 65,
				"resetAt": "2026-12-04T18:00:17+09:00",
				"subText": "5h 34%",
				"subResetAt": "2026-11-27T09:00:02+09:00",
				"altPercent": 48,
				"altResetAt": "2026-12-02T01:32:24+09:00",
				"altSubText": "5h 100%",
				"altSubResetAt": ""
			}
		}
	}`
	if err := os.WriteFile(usagePath, []byte(legacyJSON), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	now, _ := time.Parse(time.RFC3339, "2026-11-27T08:10:00+09:00")
	_, _, ts := newTestServer(t, Options{
		InstanceID:       "meta-reset-legacy",
		GateMode:         config.GateModeEnforce,
		QuotaUsageFile:   usagePath,
		QuotaUsageFormat: config.UsageFormatLegacy,
		QuotaStaleAfter:  45 * time.Minute,
		Pools: map[string]config.PoolConfig{
			"claude":     {Provider: "claude-usage"},
			"codex":      {Provider: "codex-usage"},
			"agy-gemini": {Provider: "agy-usage"},
			"agy-3p":     {Provider: "agy-usage"},
			"agy-argon":  {Unmeasured: true},
		},
		Now: func() time.Time { return now },
	})

	resp := doJSON(t, http.MethodGet, ts.URL+"/meta", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /meta status = %d, want 200", resp.StatusCode)
	}
	rawBytes, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("read /meta: %v", err)
	}

	var meta MetaResponse
	if err := json.Unmarshal(rawBytes, &meta); err != nil {
		t.Fatalf("unmarshal /meta: %v", err)
	}
	byPool := make(map[string]PoolSummaryDTO, len(meta.PoolSummaries))
	for _, ps := range meta.PoolSummaries {
		byPool[ps.Pool] = ps
	}

	assertTimeStr := func(pool, field string, got *time.Time, want string) {
		t.Helper()
		if want == "" {
			if got != nil {
				t.Errorf("%s.%s = %v, want nil", pool, field, got)
			}
			return
		}
		if got == nil {
			t.Fatalf("%s.%s = nil, want %s", pool, field, want)
		}
		if s := got.Format(time.RFC3339); s != want {
			t.Errorf("%s.%s = %q, want verbatim %q", pool, field, s, want)
		}
	}

	assertTimeStr("claude", "weekly_reset_at", byPool["claude"].WeeklyResetAt, "2026-12-03T20:00:00+09:00")
	assertTimeStr("claude", "short_reset_at", byPool["claude"].ShortResetAt, "2026-11-27T12:30:00+09:00")
	assertTimeStr("codex", "weekly_reset_at", byPool["codex"].WeeklyResetAt, "2026-12-01T14:51:23+09:00")
	assertTimeStr("codex", "short_reset_at", byPool["codex"].ShortResetAt, "2026-11-27T10:36:25+09:00")
	assertTimeStr("agy-gemini", "weekly_reset_at", byPool["agy-gemini"].WeeklyResetAt, "2026-12-04T18:00:17+09:00")
	assertTimeStr("agy-gemini", "short_reset_at", byPool["agy-gemini"].ShortResetAt, "2026-11-27T09:00:02+09:00")
	assertTimeStr("agy-3p", "weekly_reset_at", byPool["agy-3p"].WeeklyResetAt, "2026-12-02T01:32:24+09:00")
	assertTimeStr("agy-3p", "short_reset_at", byPool["agy-3p"].ShortResetAt, "")
	assertTimeStr("agy-argon", "weekly_reset_at", byPool["agy-argon"].WeeklyResetAt, "")
	assertTimeStr("agy-argon", "short_reset_at", byPool["agy-argon"].ShortResetAt, "")
}

func TestMetaPoolSummariesResetTimesSchemaV1(t *testing.T) {
	usagePath := filepath.Join(t.TempDir(), "usage-v1.json")
	v1JSON := `{
		"schema_version": 1,
		"generated_at": "2026-11-26T20:30:12Z",
		"pools": {
			"claude": {
				"status": "ok",
				"measured_at": "2026-11-26T20:29:48Z",
				"weekly": { "remaining_percent": 100, "reset_at": "2026-12-03T11:00:00Z" },
				"short": { "remaining_percent": 100, "reset_at": null, "window": "5h" }
			},
			"codex": {
				"status": "error",
				"error": "probe_auth_expired",
				"measured_at": "2026-11-26T19:59:44Z",
				"weekly": { "remaining_percent": 62, "reset_at": "2026-12-01T05:54:42Z" },
				"short": { "remaining_percent": 63, "reset_at": "2026-11-26T22:54:25Z", "window": "5h" }
			},
			"agy-gemini": {
				"status": "ok",
				"measured_at": "2026-11-26T20:30:10Z",
				"weekly": { "remaining_percent": 94, "reset_at": "2026-12-03T09:29:03Z", "reset_at_approx": false },
				"short": { "remaining_percent": 79, "reset_at": "2026-11-26T23:29:03Z", "reset_at_approx": true, "window": "1h" }
			},
			"agy-3p": {
				"status": "ok",
				"measured_at": "2026-11-26T20:30:10Z",
				"weekly": { "remaining_percent": 0, "reset_at": "2026-12-02T08:24:03Z", "reset_at_approx": false },
				"short": null
			},
			"weekly-only-plan": {
				"status": "ok",
				"measured_at": "2026-11-26T20:30:11Z",
				"weekly": { "remaining_percent": 80, "reset_at": "2026-12-03T00:00:00Z", "reset_at_approx": true },
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
	if err := os.WriteFile(usagePath, []byte(v1JSON), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	falseVal := false
	now := time.Date(2026, 11, 26, 20, 35, 0, 0, time.UTC)
	_, _, ts := newTestServer(t, Options{
		InstanceID:       "meta-reset-v1",
		GateMode:         config.GateModeEnforce,
		QuotaUsageFile:   usagePath,
		QuotaUsageFormat: config.UsageFormatV1,
		QuotaStaleAfter:  45 * time.Minute,
		Pools: map[string]config.PoolConfig{
			"claude":           {},
			"codex":            {},
			"agy-gemini":       {},
			"agy-3p":           {},
			"weekly-only-plan": {ShortWindow: &falseVal},
			"initial-err":      {},
			"missing-pool":     {},
			"agy-argon":        {Unmeasured: true},
		},
		Now: func() time.Time { return now },
	})

	resp := doJSON(t, http.MethodGet, ts.URL+"/meta", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /meta status = %d, want 200", resp.StatusCode)
	}
	meta := decodeResp[MetaResponse](t, resp)

	byPool := make(map[string]PoolSummaryDTO, len(meta.PoolSummaries))
	for _, ps := range meta.PoolSummaries {
		byPool[ps.Pool] = ps
	}

	assertTimeStr := func(pool, field string, got *time.Time, want string) {
		t.Helper()
		if want == "" {
			if got != nil {
				t.Errorf("%s.%s = %v, want nil", pool, field, got)
			}
			return
		}
		if got == nil {
			t.Fatalf("%s.%s = nil, want %s", pool, field, want)
		}
		if s := got.Format(time.RFC3339); s != want {
			t.Errorf("%s.%s = %q, want verbatim %q", pool, field, s, want)
		}
	}

	assertTimeStr("claude", "weekly_reset_at", byPool["claude"].WeeklyResetAt, "2026-12-03T11:00:00Z")
	assertTimeStr("claude", "short_reset_at", byPool["claude"].ShortResetAt, "")
	if byPool["claude"].ShortWindow != "5h" {
		t.Errorf("claude.short_window = %q, want 5h", byPool["claude"].ShortWindow)
	}

	assertTimeStr("codex", "weekly_reset_at", byPool["codex"].WeeklyResetAt, "2026-12-01T05:54:42Z")
	assertTimeStr("codex", "short_reset_at", byPool["codex"].ShortResetAt, "2026-11-26T22:54:25Z")
	if byPool["codex"].Status != string(quota.PoolUnknown) || byPool["codex"].Reason != quota.ReasonCollectorError {
		t.Errorf("codex status/reason = %s/%s, want unknown/collector_error", byPool["codex"].Status, byPool["codex"].Reason)
	}

	assertTimeStr("agy-gemini", "weekly_reset_at", byPool["agy-gemini"].WeeklyResetAt, "2026-12-03T09:29:03Z")
	assertTimeStr("agy-gemini", "short_reset_at", byPool["agy-gemini"].ShortResetAt, "2026-11-26T23:29:03Z")
	if !byPool["agy-gemini"].ShortResetAtApprox || byPool["agy-gemini"].ShortWindow != "1h" {
		t.Errorf("agy-gemini short meta = approx:%v window:%q", byPool["agy-gemini"].ShortResetAtApprox, byPool["agy-gemini"].ShortWindow)
	}

	assertTimeStr("agy-3p", "weekly_reset_at", byPool["agy-3p"].WeeklyResetAt, "2026-12-02T08:24:03Z")
	assertTimeStr("agy-3p", "short_reset_at", byPool["agy-3p"].ShortResetAt, "")

	wo := byPool["weekly-only-plan"]
	assertTimeStr("weekly-only-plan", "weekly_reset_at", wo.WeeklyResetAt, "2026-12-03T00:00:00Z")
	assertTimeStr("weekly-only-plan", "short_reset_at", wo.ShortResetAt, "")
	if !wo.WeeklyResetAtApprox {
		t.Errorf("weekly-only-plan.weekly_reset_at_approx = false, want true")
	}
	if wo.ShortApplicable == nil || *wo.ShortApplicable {
		t.Errorf("weekly-only-plan.short_applicable = %v, want false", wo.ShortApplicable)
	}

	ie := byPool["initial-err"]
	if ie.WeeklyRemainingPercent != nil {
		t.Errorf("initial-err.weekly_remaining_percent = %v, want nil when weekly is null", *ie.WeeklyRemainingPercent)
	}
	assertTimeStr("initial-err", "weekly_reset_at", ie.WeeklyResetAt, "")
	assertTimeStr("initial-err", "short_reset_at", ie.ShortResetAt, "")

	assertTimeStr("missing-pool", "weekly_reset_at", byPool["missing-pool"].WeeklyResetAt, "")
	assertTimeStr("missing-pool", "short_reset_at", byPool["missing-pool"].ShortResetAt, "")
	assertTimeStr("agy-argon", "weekly_reset_at", byPool["agy-argon"].WeeklyResetAt, "")
	assertTimeStr("agy-argon", "short_reset_at", byPool["agy-argon"].ShortResetAt, "")
}

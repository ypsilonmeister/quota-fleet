package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/api"
	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/runtime"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

// Issue #42 reproduction: collected at 15:06 JST, claude 5h 0% with a reset time of
// 13:49:59 JST that has already passed. The job must not be held, a job held while the
// window was still running must be released after its reset, and /meta must show healthy.
func TestExpiredShortWindowPassesGateReleasesHeldAndMetaHealthy(t *testing.T) {
	jst := time.FixedZone("JST", 9*60*60)
	reset := time.Date(2026, 11, 28, 13, 49, 59, 0, jst)

	files := map[string]func(measured time.Time) string{
		config.UsageFormatLegacy: func(measured time.Time) string {
			return fmt.Sprintf(`{"updatedAt":%q,"services":{"claude":{"remainingPercent":60,"resetAt":"2026-12-03T20:00:00+09:00","subText":"5h 0%%","subResetAt":%q}}}`,
				measured.Format(time.RFC3339), reset.Format(time.RFC3339))
		},
		config.UsageFormatV1: func(measured time.Time) string {
			at := measured.Format(time.RFC3339)
			return fmt.Sprintf(`{"schema_version":1,"generated_at":%q,"pools":{"claude":{"status":"ok","measured_at":%q,"weekly":{"remaining_percent":60,"reset_at":"2026-12-03T20:00:00+09:00"},"short":{"remaining_percent":0,"reset_at":%q,"window":"5h"}}}}`,
				at, at, reset.Format(time.RFC3339))
		},
	}

	for format, build := range files {
		t.Run(format, func(t *testing.T) {
			now := time.Date(2026, 11, 28, 13, 40, 0, 0, jst) // before the reset
			dir := t.TempDir()
			usagePath := filepath.Join(dir, "usage.json")
			writeUsage := func() {
				t.Helper()
				if err := os.WriteFile(usagePath, []byte(build(now.Add(-time.Minute))), 0o600); err != nil {
					t.Fatalf("WriteFile: %v", err)
				}
			}
			writeUsage()

			st, err := store.Open(context.Background(), filepath.Join(dir, "fleet.db"), store.Options{InstanceID: "expwin"})
			if err != nil {
				t.Fatalf("store.Open: %v", err)
			}
			defer st.Close()

			pools := map[string]config.PoolConfig{"claude": {}}
			clock := func() time.Time { return now }
			checker := runtime.NewGateChecker(runtime.GateCheckerOptions{
				Store:       st,
				GateMode:    config.GateModeEnforce,
				UsageFile:   usagePath,
				UsageFormat: format,
				StaleAfter:  45 * time.Minute,
				Pools:       pools,
				Now:         clock,
			})
			srv := api.NewServer(st, api.Options{
				InstanceID:       "expwin",
				GateMode:         config.GateModeEnforce,
				QuotaUsageFile:   usagePath,
				QuotaUsageFormat: format,
				QuotaStaleAfter:  45 * time.Minute,
				Pools:            pools,
				Now:              clock,
				GateReleaser:     testGateReleaser{checker: checker},
			})
			ts := httptest.NewServer(srv.Handler())
			defer ts.Close()

			postJob := func(requestID string) (int, string) {
				t.Helper()
				resp, err := http.Post(ts.URL+"/jobs", "application/json",
					strings.NewReader(fmt.Sprintf(`{"request_id":%q,"title":"t","pool":"claude"}`, requestID)))
				if err != nil {
					t.Fatalf("POST /jobs: %v", err)
				}
				defer resp.Body.Close()
				var body struct {
					HeldID string `json:"held_id"`
				}
				if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
					t.Fatalf("decode POST /jobs: %v", err)
				}
				return resp.StatusCode, body.HeldID
			}
			claudeSummary := func() api.PoolSummaryDTO {
				t.Helper()
				resp, err := http.Get(ts.URL + "/meta")
				if err != nil {
					t.Fatalf("GET /meta: %v", err)
				}
				defer resp.Body.Close()
				var meta api.MetaResponse
				if err := json.NewDecoder(resp.Body).Decode(&meta); err != nil {
					t.Fatalf("decode /meta: %v", err)
				}
				if len(meta.PoolSummaries) != 1 {
					t.Fatalf("pool_summaries = %+v", meta.PoolSummaries)
				}
				return meta.PoolSummaries[0]
			}

			// Before the reset the window is genuinely exhausted.
			if ps := claudeSummary(); ps.Status != "exhausted" || ps.BlockedBy != "5h" {
				t.Fatalf("before reset: /meta = %s blocked_by=%q, want exhausted 5h", ps.Status, ps.BlockedBy)
			}
			status, heldID := postJob("before-reset")
			if status != http.StatusAccepted || heldID == "" {
				t.Fatalf("before reset: POST status = %d held_id=%q, want 202 held", status, heldID)
			}

			// 15:06: a fresh collection still returns the expired window.
			now = time.Date(2026, 11, 28, 15, 6, 0, 0, jst)
			writeUsage()

			ps := claudeSummary()
			if ps.Status != "healthy" || ps.BlockedBy != "" {
				t.Errorf("after reset: /meta = %s blocked_by=%q, want healthy", ps.Status, ps.BlockedBy)
			}
			if ps.ShortRemainingPercent == nil || *ps.ShortRemainingPercent != 0 ||
				ps.ShortResetAt == nil || !ps.ShortResetAt.Equal(reset) {
				t.Errorf("after reset: /meta must report the measured values unchanged: %+v", ps)
			}
			if status, _ := postJob("after-reset"); status != http.StatusCreated {
				t.Errorf("after reset: POST status = %d, want 201 created", status)
			}

			if err := checker.CheckOnce(context.Background()); err != nil {
				t.Fatalf("CheckOnce: %v", err)
			}
			held, err := st.GetHeldJob(context.Background(), heldID)
			if err != nil {
				t.Fatalf("GetHeldJob: %v", err)
			}
			if held.Status != store.HeldStatusReleased {
				t.Errorf("held job status = %q (reason=%q), want released", held.Status, held.Reason)
			}
		})
	}
}

// A weekly 0% past its reset with no short value must still be held at registration, and
// released once the short window is measured.
func TestExpiredWeeklyWithMissingShortHoldsUntilShortMeasured(t *testing.T) {
	now := time.Date(2026, 11, 28, 6, 6, 0, 0, time.UTC)
	dir := t.TempDir()
	usagePath := filepath.Join(dir, "usage.json")
	writeUsage := func(subText string) {
		t.Helper()
		raw := fmt.Sprintf(`{"updatedAt":"2026-11-28T06:05:00Z","services":{"claude":{"remainingPercent":0,"resetAt":"2026-11-28T04:49:59Z","subText":%q}}}`, subText)
		if err := os.WriteFile(usagePath, []byte(raw), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}
	writeUsage("")

	st, err := store.Open(context.Background(), filepath.Join(dir, "fleet.db"), store.Options{InstanceID: "expwin"})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	pools := map[string]config.PoolConfig{"claude": {}}
	clock := func() time.Time { return now }
	checker := runtime.NewGateChecker(runtime.GateCheckerOptions{
		Store:       st,
		GateMode:    config.GateModeEnforce,
		UsageFile:   usagePath,
		UsageFormat: config.UsageFormatLegacy,
		StaleAfter:  45 * time.Minute,
		Pools:       pools,
		Now:         clock,
	})
	srv := api.NewServer(st, api.Options{
		InstanceID:       "expwin",
		GateMode:         config.GateModeEnforce,
		QuotaUsageFile:   usagePath,
		QuotaUsageFormat: config.UsageFormatLegacy,
		QuotaStaleAfter:  45 * time.Minute,
		Pools:            pools,
		Now:              clock,
		GateReleaser:     testGateReleaser{checker: checker},
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/jobs", "application/json",
		strings.NewReader(`{"request_id":"missing-short","title":"t","pool":"claude"}`))
	if err != nil {
		t.Fatalf("POST /jobs: %v", err)
	}
	defer resp.Body.Close()
	var body struct {
		HeldID string `json:"held_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode POST /jobs: %v", err)
	}
	if resp.StatusCode != http.StatusAccepted || body.HeldID == "" {
		t.Fatalf("POST status = %d held_id=%q, want 202 held", resp.StatusCode, body.HeldID)
	}

	checkHeld := func(want store.HeldStatus) {
		t.Helper()
		if err := checker.CheckOnce(context.Background()); err != nil {
			t.Fatalf("CheckOnce: %v", err)
		}
		held, err := st.GetHeldJob(context.Background(), body.HeldID)
		if err != nil {
			t.Fatalf("GetHeldJob: %v", err)
		}
		if held.Status != want {
			t.Errorf("held job status = %q (reason=%q), want %q", held.Status, held.Reason, want)
		}
	}
	checkHeld(store.HeldStatusHeld)
	writeUsage("5h 80%")
	checkHeld(store.HeldStatusReleased)
}

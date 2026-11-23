package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/api"
	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/quota"
	"github.com/ypsilonmeister/quota-fleet/internal/runtime"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

func TestGateReleaserAdapterWithRuntimeGateChecker(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "fleet.db"), store.Options{InstanceID: "serve-rel-test"})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	short0 := 0
	short60 := 60
	resetAt := now.Add(24 * time.Hour)
	recovered := false

	pools := map[string]config.PoolConfig{
		"claude": {Provider: "anthropic"},
	}
	loader := func() (*quota.Snapshot, error) {
		if recovered {
			return &quota.Snapshot{
				MeasuredAt: now,
				Pools: map[string]quota.PoolSnapshot{
					"claude": {
						WeeklyRemainingPercent: 40,
						WeeklyResetAt:          &resetAt,
						ShortRemainingPercent:  &short60,
						MeasuredAt:             now,
					},
				},
			}, nil
		}
		return &quota.Snapshot{
			MeasuredAt: now,
			Pools: map[string]quota.PoolSnapshot{
				"claude": {
					WeeklyRemainingPercent: 0,
					WeeklyResetAt:          &resetAt,
					ShortRemainingPercent:  &short0,
					MeasuredAt:             now,
				},
			},
		}, nil
	}

	checker := runtime.NewGateChecker(runtime.GateCheckerOptions{
		Store:          st,
		GateMode:       config.GateModeEnforce,
		Pools:          pools,
		Now:            func() time.Time { return now },
		Logger:         log.New(io.Discard, "", 0),
		SnapshotLoader: loader,
	})

	held, _, err := st.HoldJob(ctx, store.HoldJobParams{
		RequestID: "serve-rel-1",
		Pool:      "claude",
		Payload: store.CreateJobParams{
			RequestID: "serve-rel-1",
			Title:     "Real GateChecker release",
			Pool:      "claude",
		},
		Reason: "exhausted:weekly",
	})
	if err != nil {
		t.Fatalf("HoldJob: %v", err)
	}

	srv := api.NewServer(st, api.Options{
		InstanceID:     "serve-rel-test",
		GateMode:       config.GateModeEnforce,
		EvaluatorMode:  config.EvaluatorModeAlwaysPass,
		Pools:          pools,
		Hosts:          sortedMapKeys(map[string]config.HostConfig{"ubuntu": {KeyFile: "/home/user/.ssh/id_ed25519"}}),
		Agents:         sortedMapKeys(map[string]config.AgentConfig{"claude": {}}),
		GateReleaser:   gateReleaserAdapter{checker: checker},
		Logger:         log.New(io.Discard, "", 0),
		Now:            func() time.Time { return now },
		SnapshotLoader: loader,
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Still exhausted -> 409.
	resp, err := http.Post(ts.URL+"/gate/held/"+held.ID+"/release", "application/json", nil)
	if err != nil {
		t.Fatalf("POST release (exhausted): %v", err)
	}
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("exhausted release status = %d, want 409", resp.StatusCode)
	}
	var res1 api.ReleaseHeldResponse
	if err := json.NewDecoder(resp.Body).Decode(&res1); err != nil {
		t.Fatalf("decode res1: %v", err)
	}
	_ = resp.Body.Close()
	if res1.Released || res1.Reason == "" {
		t.Fatalf("unexpected res1: %+v", res1)
	}

	// Recovered -> 200 with created job_id.
	recovered = true
	resp, err = http.Post(ts.URL+"/gate/held/"+held.ID+"/release", "application/json", nil)
	if err != nil {
		t.Fatalf("POST release (recovered): %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("recovered release status = %d, want 200", resp.StatusCode)
	}
	var res2 api.ReleaseHeldResponse
	if err := json.NewDecoder(resp.Body).Decode(&res2); err != nil {
		t.Fatalf("decode res2: %v", err)
	}
	_ = resp.Body.Close()
	if !res2.Released || res2.JobID <= 0 {
		t.Fatalf("unexpected res2: %+v", res2)
	}
}

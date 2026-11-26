package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/admission"
	"github.com/ypsilonmeister/quota-fleet/internal/api"
	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/quota"
	"github.com/ypsilonmeister/quota-fleet/internal/runtime"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

type testGateReleaser struct {
	checker *runtime.GateChecker
}

func (r testGateReleaser) ReleaseHeld(ctx context.Context, heldID string) (*api.GateReleaseResult, error) {
	res, err := r.checker.ReleaseByID(ctx, heldID)
	if res == nil {
		return nil, err
	}
	out := &api.GateReleaseResult{
		Released:     res.Released,
		SelectedPool: res.SelectedPool,
		Reason:       res.Reason,
	}
	if res.Job != nil {
		out.JobID = res.Job.ID
	}
	return out, err
}

func TestUsageV1EnforceDryRunUndefinedAndChainDriver(t *testing.T) {
	now := time.Date(2026, 11, 26, 21, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	usagePath := filepath.Join(dir, "usage-v1.json")

	writeV1 := func(body string) {
		t.Helper()
		if err := os.WriteFile(usagePath, []byte(body), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
	}

	pools := map[string]config.PoolConfig{
		"demo-pool":       {AppliesTo: []config.PoolTarget{{Agent: "claude"}}},
		"demo-alt":        {},
		"demo-unmeasured": {Unmeasured: true},
	}

	// Start with corrupt JSON to also verify Item B (undefined pool rejects 400 / records pool_not_configured BEFORE file read!).
	writeV1("{not valid json")

	st, err := store.Open(context.Background(), filepath.Join(dir, "enforce.db"), store.Options{InstanceID: "v1-test"})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	var logBuf bytes.Buffer
	logger := log.New(&logBuf, "", 0)
	checker := runtime.NewGateChecker(runtime.GateCheckerOptions{
		Store:       st,
		GateMode:    config.GateModeEnforce,
		UsageFile:   usagePath,
		UsageFormat: config.UsageFormatV1,
		StaleAfter:  45 * time.Minute,
		Pools:       pools,
		Now:         func() time.Time { return now },
		Logger:      logger,
	})
	srv := api.NewServer(st, api.Options{
		InstanceID:       "v1-test",
		GateMode:         config.GateModeEnforce,
		QuotaUsageFile:   usagePath,
		QuotaUsageFormat: config.UsageFormatV1,
		QuotaStaleAfter:  45 * time.Minute,
		Pools:            pools,
		Now:              func() time.Time { return now },
		GateReleaser:     testGateReleaser{checker: checker},
		Logger:           logger,
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	postJob := func(body string) (int, string) {
		t.Helper()
		resp, err := http.Post(ts.URL+"/jobs", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("POST /jobs: %v", err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	// 1. Item B: undefined pool in enforce returns HTTP 400 even when usage file is corrupt (and even with unmeasured primary).
	for _, tc := range []struct {
		name string
		body string
	}{
		{"undefined primary", `{"request_id":"u1","title":"t","pool":"nope"}`},
		{"measured primary + undefined alt + corrupt file", `{"request_id":"u2","title":"t","pool":"demo-pool","alt_pools":["nope"]}`},
		{"unmeasured primary + undefined alt", `{"request_id":"u3","title":"t","pool":"demo-unmeasured","alt_pools":["nope"]}`},
	} {
		status, body := postJob(tc.body)
		if status != http.StatusBadRequest || !strings.Contains(body, "unknown_pool") {
			t.Errorf("%s: status=%d body=%q, want 400 with unknown_pool", tc.name, status, body)
		}
	}

	// 2. Corrupt file with valid measured pool -> fail-closed Hold (202 Accepted) in v1 enforce.
	status, body := postJob(`{"request_id":"corrupt-1","title":"Corrupt Hold","pool":"demo-pool"}`)
	if status != http.StatusAccepted || !strings.Contains(body, "snapshot_error") {
		t.Fatalf("corrupt file v1 enforce: status=%d body=%q, want 202 with snapshot_error", status, body)
	}
	var holdResp struct {
		HeldID string `json:"held_id"`
	}
	_ = json.Unmarshal([]byte(body), &holdResp)

	// 3. Write valid v1 with demo-pool short missing + demo-alt exhausted + demo-unmeasured alt -> Hold (no detour!).
	writeV1(`{
		"schema_version": 1,
		"generated_at": "2026-11-26T20:55:00Z",
		"pools": {
			"demo-pool": {"status":"ok","measured_at":"2026-11-26T20:54:50Z","weekly":{"remaining_percent":80},"short":{}},
			"demo-alt":  {"status":"ok","measured_at":"2026-11-26T20:54:50Z","weekly":{"remaining_percent":0},"short":{"remaining_percent":50}}
		}
	}`)
	status, body = postJob(`{"request_id":"mix-1","title":"Mixed No Detour","pool":"demo-pool","alt_pools":["demo-alt","demo-unmeasured"]}`)
	if status != http.StatusAccepted || !strings.Contains(body, "short_window_missing") {
		t.Fatalf("mixed exhausted+missing+unmeasured: status=%d body=%q, want 202 short_window_missing", status, body)
	}

	// 3b. Unsanitized collector errors (multiline raw upstream text and single-line dummy credential)
	// -> Hold with collector_error and sanitized summary/log/decision (no raw leak).
	for _, tc := range []struct {
		reqID  string
		rawErr string
		secret string
	}{
		{"raw-err-1", `probe_timeout\nREVIEW_TEST_RAW_UPSTREAM_LINE`, "REVIEW_TEST_RAW_UPSTREAM_LINE"},
		{"raw-err-cred-1", "sk-proj-DummySecretToken1234567890", "sk-proj-DummySecretToken1234567890"},
	} {
		logBuf.Reset()
		writeV1(`{
		"schema_version": 1,
		"generated_at": "2026-11-26T20:56:00Z",
		"pools": {
			"demo-pool": {"status":"error","error":"` + tc.rawErr + `","measured_at":"2026-11-26T20:55:50Z","weekly":{"remaining_percent":80},"short":{"remaining_percent":60}}
		}
	}`)
		status, body = postJob(`{"request_id":"` + tc.reqID + `","title":"Raw Collector Error","pool":"demo-pool"}`)
		if status != http.StatusAccepted || !strings.Contains(body, quota.ReasonCollectorError) {
			t.Fatalf("unsanitized collector error (%s) hold: status=%d body=%q, want 202 collector_error", tc.reqID, status, body)
		}
		if strings.Contains(body, tc.secret) {
			t.Errorf("HTTP hold response (%s) leaked raw collector error: %q", tc.reqID, body)
		}
		var rawErrHoldResp struct {
			HeldID string `json:"held_id"`
		}
		_ = json.Unmarshal([]byte(body), &rawErrHoldResp)
		heldRawErr, err := st.GetHeldJob(context.Background(), rawErrHoldResp.HeldID)
		if err != nil {
			t.Fatalf("GetHeldJob (%s): %v", tc.reqID, err)
		}
		if strings.Contains(heldRawErr.Reason, tc.secret) || strings.Contains(heldRawErr.Reason, "\n") || !strings.Contains(heldRawErr.Reason, quota.RedactedCollectorError) {
			t.Errorf("held_jobs.reason (%s) not sanitized: %q", tc.reqID, heldRawErr.Reason)
		}
		if strings.Contains(heldRawErr.SnapshotSummary, tc.secret) || !strings.Contains(heldRawErr.SnapshotSummary, quota.RedactedCollectorError) {
			t.Errorf("held_jobs.snapshot_summary (%s) not sanitized: %q", tc.reqID, heldRawErr.SnapshotSummary)
		}
		if strings.Contains(logBuf.String(), tc.secret) || !strings.Contains(logBuf.String(), "pool_error="+quota.RedactedCollectorError) {
			t.Errorf("operational log (%s) leaked raw collector error or missed sanitized summary:\n%s", tc.reqID, logBuf.String())
		}
		rawDecs, err := st.ListGateDecisions(context.Background(), 1)
		if err != nil || len(rawDecs) == 0 || strings.Contains(rawDecs[0].Reason, tc.secret) || strings.Contains(rawDecs[0].Reason, "\n") || !strings.Contains(rawDecs[0].Reason, quota.RedactedCollectorError) {
			t.Errorf("gate_decisions.reason (%s) not sanitized: %+v (err=%v)", tc.reqID, rawDecs, err)
		}
	}

	// 4. Write recovered healthy v1 -> periodic release releases held job!
	writeV1(`{
		"schema_version": 1,
		"generated_at": "2026-11-26T20:58:00Z",
		"pools": {
			"demo-pool": {"status":"ok","measured_at":"2026-11-26T20:57:50Z","weekly":{"remaining_percent":80},"short":{"remaining_percent":60}},
			"demo-alt":  {"status":"ok","measured_at":"2026-11-26T20:57:50Z","weekly":{"remaining_percent":90},"short":{"remaining_percent":70}}
		}
	}`)
	if err := checker.CheckOnce(context.Background()); err != nil {
		t.Fatalf("CheckOnce: %v", err)
	}
	h, err := st.GetHeldJob(context.Background(), holdResp.HeldID)
	if err != nil {
		t.Fatalf("GetHeldJob: %v", err)
	}
	if h.Status != store.HeldStatusReleased || h.ReleasedPool != "demo-pool" {
		t.Fatalf("held job after recovery = status:%s pool:%s, want released/demo-pool", h.Status, h.ReleasedPool)
	}

	// 5. Dry-run with undefined pool + corrupt file (Item B) -> 201 Created + gate_decision records pool_not_configured!
	writeV1("{corrupt again")
	stDry, err := store.Open(context.Background(), filepath.Join(dir, "dry.db"), store.Options{InstanceID: "v1-dry"})
	if err != nil {
		t.Fatalf("store.Open dry: %v", err)
	}
	defer stDry.Close()
	srvDry := api.NewServer(stDry, api.Options{
		InstanceID:       "v1-dry",
		GateMode:         config.GateModeDryRun,
		QuotaUsageFile:   usagePath,
		QuotaUsageFormat: config.UsageFormatV1,
		QuotaStaleAfter:  45 * time.Minute,
		Pools:            pools,
		Now:              func() time.Time { return now },
		Logger:           logger,
	})
	tsDry := httptest.NewServer(srvDry.Handler())
	defer tsDry.Close()

	resp, err := http.Post(tsDry.URL+"/jobs", "application/json", strings.NewReader(`{"request_id":"dry-u1","title":"Dry Undefined","pool":"demo-pool","alt_pools":["nope"]}`))
	if err != nil {
		t.Fatalf("POST dry: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("dry-run undefined pool status=%d, want 201", resp.StatusCode)
	}
	decs, err := stDry.ListGateDecisions(context.Background(), 10)
	if err != nil || len(decs) != 1 || decs[0].Decision != string(quota.DecisionUnknown) || !strings.Contains(decs[0].Reason, quota.ReasonPoolNotConfigured) {
		t.Fatalf("dry-run gate decisions = %+v (err=%v), want unknown/pool_not_configured", decs, err)
	}

	// 6. Chain Driver path: undefined pool on review step stops chain with ChainStopAdmissionRejected!
	writeV1(`{
		"schema_version": 1,
		"generated_at": "2026-11-26T20:58:00Z",
		"pools": {
			"demo-pool": {"status":"ok","measured_at":"2026-11-26T20:57:50Z","weekly":{"remaining_percent":80},"short":{"remaining_percent":60}}
		}
	}`)
	stChain, err := store.Open(context.Background(), filepath.Join(dir, "chain.db"), store.Options{InstanceID: "v1-chain"})
	if err != nil {
		t.Fatalf("store.Open chain: %v", err)
	}
	defer stChain.Close()
	admChain := admission.New(admission.Options{
		GateMode:    config.GateModeEnforce,
		UsageFile:   usagePath,
		UsageFormat: config.UsageFormatV1,
		Pools:       pools,
		Now:         func() time.Time { return now },
		Logger:      logger,
	})
	srvChain := api.NewServer(stChain, api.Options{
		InstanceID:       "v1-chain",
		GateMode:         config.GateModeEnforce,
		QuotaUsageFile:   usagePath,
		QuotaUsageFormat: config.UsageFormatV1,
		Pools:            pools,
		Now:              func() time.Time { return now },
		Logger:           logger,
	})
	tsChain := httptest.NewServer(srvChain.Handler())
	defer tsChain.Close()

	// Chain with undefined pool in implement step -> HTTP 400 immediately.
	chainBadImpl := `{
		"request_id": "ch-bad-1",
		"title": "Bad Impl Pool",
		"workspace": {"workdir": "/tmp/w"},
		"steps": {
			"implement": {"title": "impl", "agent": "claude", "pool": "does-not-exist", "instructions": "do"},
			"review":    {"title": "rev",  "agent": "codex",  "pool": "demo-pool", "instructions": "write {{verdict.path}} nonce {{verdict.nonce}}"},
			"fix":       {"title": "fix",  "agent": "claude", "pool": "demo-pool", "instructions": "fix {{verdict.reviewed_commit}}"},
			"publish":   {"title": "pub",  "agent": "claude", "pool": "demo-pool", "instructions": "publish {{verdict.reviewed_commit}}"}
		}
	}`
	cResp, err := http.Post(tsChain.URL+"/chains", "application/json", strings.NewReader(chainBadImpl))
	if err != nil {
		t.Fatalf("POST /chains bad impl: %v", err)
	}
	cBody, _ := io.ReadAll(cResp.Body)
	cResp.Body.Close()
	if cResp.StatusCode != http.StatusBadRequest || !strings.Contains(string(cBody), "unknown_pool") {
		t.Fatalf("POST /chains bad impl: status=%d body=%q, want 400 unknown_pool", cResp.StatusCode, string(cBody))
	}

	// Chain with valid implement pool but undefined review pool -> implement runs, then ChainDriver stops with admission_rejected!
	chainBadRev := `{
		"request_id": "ch-bad-2",
		"title": "Bad Review Pool",
		"workspace": {"workdir": "/tmp/w"},
		"steps": {
			"implement": {"title": "impl", "agent": "claude", "pool": "demo-pool", "instructions": "do"},
			"review":    {"title": "rev",  "agent": "codex",  "pool": "does-not-exist", "instructions": "write {{verdict.path}} nonce {{verdict.nonce}}"},
			"fix":       {"title": "fix",  "agent": "claude", "pool": "demo-pool", "instructions": "fix {{verdict.reviewed_commit}}"},
			"publish":   {"title": "pub",  "agent": "claude", "pool": "demo-pool", "instructions": "publish {{verdict.reviewed_commit}}"}
		}
	}`
	cResp2, err := http.Post(tsChain.URL+"/chains", "application/json", strings.NewReader(chainBadRev))
	if err != nil {
		t.Fatalf("POST /chains bad rev: %v", err)
	}
	var createdChain struct {
		Run struct {
			ID string `json:"id"`
		} `json:"run"`
		Steps []struct {
			JobID *int64 `json:"job_id"`
		} `json:"steps"`
	}
	_ = json.NewDecoder(cResp2.Body).Decode(&createdChain)
	cResp2.Body.Close()
	if cResp2.StatusCode != http.StatusCreated || createdChain.Run.ID == "" || len(createdChain.Steps) == 0 || createdChain.Steps[0].JobID == nil {
		t.Fatalf("POST /chains bad rev unexpected: status=%d %+v", cResp2.StatusCode, createdChain)
	}
	// Complete implement job so ChainDriver advances to review step.
	ctx := context.Background()
	implJobID := *createdChain.Steps[0].JobID
	claimed, ok, err := stChain.ClaimByID(ctx, implJobID, 3)
	if err != nil || !ok {
		t.Fatalf("ClaimByID: ok=%t err=%v", ok, err)
	}
	if _, err := stChain.Complete(ctx, implJobID, claimed.ClaimedBy, "ok"); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	driver := runtime.NewChainDriver(runtime.ChainDriverOptions{
		Store:     stChain,
		Admission: admChain,
		Now:       func() time.Time { return now },
		Logger:    logger,
	})
	if err := driver.ProcessRun(ctx, createdChain.Run.ID); err != nil {
		t.Fatalf("ProcessRun: %v", err)
	}
	runSnap, err := stChain.LoadChainSnapshot(ctx, createdChain.Run.ID)
	if err != nil {
		t.Fatalf("LoadChainSnapshot: %v", err)
	}
	if runSnap.Run.Status != store.ChainRunStopped || runSnap.Run.StopReason != store.ChainStopAdmissionRejected || !strings.Contains(runSnap.Run.StopDetail, "unknown_pool") {
		t.Fatalf("chain run after undefined review pool = status:%s reason:%s detail:%q", runSnap.Run.Status, runSnap.Run.StopReason, runSnap.Run.StopDetail)
	}

	// 7. Selector (apply) filling a pool not defined in fleet.yaml is rejected at Gate Step 1 with HTTP 400!
	stSel, err := store.Open(ctx, filepath.Join(dir, "sel.db"), store.Options{InstanceID: "v1-sel"})
	if err != nil {
		t.Fatalf("store.Open sel: %v", err)
	}
	defer stSel.Close()
	srvSel := api.NewServer(stSel, api.Options{
		InstanceID:       "v1-sel",
		GateMode:         config.GateModeEnforce,
		SelectorMode:     config.SelectorModeApply,
		QuotaUsageFile:   usagePath,
		QuotaUsageFormat: config.UsageFormatV1,
		// Only demo-pool is configured; Selector's table will fill "codex" which is NOT in fleet.yaml!
		Pools:  map[string]config.PoolConfig{"demo-pool": {}},
		Now:    func() time.Time { return now },
		Logger: logger,
	})
	tsSel := httptest.NewServer(srvSel.Handler())
	defer tsSel.Close()
	selResp, err := http.Post(tsSel.URL+"/jobs", "application/json", strings.NewReader(`{"request_id":"sel-undef","title":"Selector Undef Pool","role":"implement","weight":"standard"}`))
	if err != nil {
		t.Fatalf("POST sel: %v", err)
	}
	selBody, _ := io.ReadAll(selResp.Body)
	selResp.Body.Close()
	if selResp.StatusCode != http.StatusBadRequest || !strings.Contains(string(selBody), "unknown_pool") {
		t.Fatalf("Selector-filled undefined pool: status=%d body=%q, want 400 unknown_pool", selResp.StatusCode, string(selBody))
	}
}

func TestHeldJobReleaseConfigChangesAndLegacyCompatItemA(t *testing.T) {
	now := time.Date(2026, 11, 26, 21, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	ctx := context.Background()

	// Part 1 (Item B): Held job in v1 after config changes — periodic & manual release match identically,
	// AND neither path reads the usage snapshot file when every held job is skip-eligible.
	stV1, err := store.Open(ctx, filepath.Join(dir, "held-v1.db"), store.Options{InstanceID: "held-v1"})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer stV1.Close()

	hAllDel, _, err := stV1.HoldJob(ctx, store.HoldJobParams{
		RequestID: "h-all-del",
		Pool:      "removed-a",
		AltPools:  []string{"removed-b"},
		Payload:   store.CreateJobParams{RequestID: "h-all-del", Title: "All Deleted", Pool: "removed-a"},
		Reason:    "exhausted:weekly",
	})
	if err != nil {
		t.Fatalf("HoldJob all-del: %v", err)
	}
	hUnmPrimaryManual, _, err := stV1.HoldJob(ctx, store.HoldJobParams{
		RequestID: "h-unm-primary-manual",
		Pool:      "now-unmeasured",
		AltPools:  []string{"removed-alt"},
		Payload:   store.CreateJobParams{RequestID: "h-unm-primary-manual", Title: "Primary Now Unmeasured (Manual)", Pool: "now-unmeasured"},
		Reason:    "exhausted:weekly",
	})
	if err != nil {
		t.Fatalf("HoldJob unm-primary-manual: %v", err)
	}
	hUnmPrimaryPeriodic, _, err := stV1.HoldJob(ctx, store.HoldJobParams{
		RequestID: "h-unm-primary-periodic",
		Pool:      "now-unmeasured",
		AltPools:  []string{"removed-alt"},
		Payload:   store.CreateJobParams{RequestID: "h-unm-primary-periodic", Title: "Primary Now Unmeasured (Periodic)", Pool: "now-unmeasured"},
		Reason:    "exhausted:weekly",
	})
	if err != nil {
		t.Fatalf("HoldJob unm-primary-periodic: %v", err)
	}

	newPools := map[string]config.PoolConfig{
		"now-unmeasured": {Unmeasured: true},
		"demo-pool":      {},
	}
	usagePath := filepath.Join(dir, "v1.json")
	_ = os.WriteFile(usagePath, []byte(`{"schema_version":1,"generated_at":"2026-11-26T20:59:00Z","pools":{"demo-pool":{"status":"ok","measured_at":"2026-11-26T20:58:50Z","weekly":{"remaining_percent":80},"short":{"remaining_percent":60}}}}`), 0o600)

	v1LoadCalls := 0
	checkerV1 := runtime.NewGateChecker(runtime.GateCheckerOptions{
		Store:       stV1,
		GateMode:    config.GateModeEnforce,
		UsageFile:   usagePath,
		UsageFormat: config.UsageFormatV1,
		Pools:       newPools,
		Now:         func() time.Time { return now },
		Logger:      log.New(io.Discard, "", 0),
		SnapshotLoader: func() (*quota.Snapshot, error) {
			v1LoadCalls++
			return quota.LoadUsageFileWithFormat(usagePath, config.UsageFormatV1)
		},
	})

	// Manual release of all-deleted -> refused with pool_not_configured, ZERO snapshot reads.
	relAllDel, err := checkerV1.ReleaseByID(ctx, hAllDel.ID)
	if err != nil || relAllDel.Released || !strings.Contains(relAllDel.Reason, quota.ReasonPoolNotConfigured) {
		t.Fatalf("manual release all-deleted = %+v err=%v, want not released pool_not_configured", relAllDel, err)
	}
	if v1LoadCalls != 0 {
		t.Fatalf("manual release all-deleted loadCalls=%d, want 0", v1LoadCalls)
	}

	// Manual release of primary-now-unmeasured + deleted-alt -> releases to now-unmeasured, ZERO snapshot reads (Item B).
	relUnmManual, err := checkerV1.ReleaseByID(ctx, hUnmPrimaryManual.ID)
	if err != nil || !relUnmManual.Released || relUnmManual.SelectedPool != "now-unmeasured" {
		t.Fatalf("manual release primary-now-unmeasured + deleted-alt = %+v err=%v, want released/now-unmeasured", relUnmManual, err)
	}
	if v1LoadCalls != 0 {
		t.Fatalf("manual release primary-now-unmeasured loadCalls=%d, want 0", v1LoadCalls)
	}

	// Periodic release with ONLY skip-eligible held jobs (all-deleted + primary-now-unmeasured):
	// all-deleted stays held, primary-now-unmeasured releases to now-unmeasured, ZERO snapshot reads!
	if err := checkerV1.CheckOnce(ctx); err != nil {
		t.Fatalf("CheckOnce skip-only: %v", err)
	}
	if v1LoadCalls != 0 {
		t.Fatalf("periodic CheckOnce with only skip-eligible held jobs read snapshot %d times, want 0", v1LoadCalls)
	}
	gotAllDel, _ := stV1.GetHeldJob(ctx, hAllDel.ID)
	if gotAllDel.Status != store.HeldStatusHeld {
		t.Errorf("all-deleted held status = %s, want held", gotAllDel.Status)
	}
	gotUnm, _ := stV1.GetHeldJob(ctx, hUnmPrimaryPeriodic.ID)
	if gotUnm.Status != store.HeldStatusReleased || gotUnm.ReleasedPool != "now-unmeasured" {
		t.Errorf("primary-now-unmeasured + deleted-alt (periodic) = status:%s pool:%s, want released/now-unmeasured", gotUnm.Status, gotUnm.ReleasedPool)
	}

	// Now add two measured held jobs alongside the remaining all-deleted held job -> CheckOnce defers load until first measured held job and reads ONCE!
	hPartialMeasured1, _, err := stV1.HoldJob(ctx, store.HoldJobParams{
		RequestID: "h-part-m1", Pool: "removed-a", AltPools: []string{"demo-pool"},
		Payload: store.CreateJobParams{RequestID: "h-part-m1", Title: "Partial Measured 1", Pool: "removed-a"}, Reason: "exhausted:weekly",
	})
	if err != nil {
		t.Fatalf("HoldJob h-part-m1: %v", err)
	}
	hPartialMeasured2, _, err := stV1.HoldJob(ctx, store.HoldJobParams{
		RequestID: "h-part-m2", Pool: "demo-pool",
		Payload: store.CreateJobParams{RequestID: "h-part-m2", Title: "Measured 2", Pool: "demo-pool"}, Reason: "exhausted:weekly",
	})
	if err != nil {
		t.Fatalf("HoldJob h-part-m2: %v", err)
	}
	if err := checkerV1.CheckOnce(ctx); err != nil {
		t.Fatalf("CheckOnce with measured held jobs: %v", err)
	}
	if v1LoadCalls != 1 {
		t.Fatalf("periodic CheckOnce with 1 skip-eligible + 2 measured held jobs loadCalls=%d, want 1", v1LoadCalls)
	}
	if got, _ := stV1.GetHeldJob(ctx, hPartialMeasured1.ID); got.Status != store.HeldStatusReleased || got.ReleasedPool != "demo-pool" {
		t.Errorf("partial deleted + healthy measured = status:%s pool:%s, want released/demo-pool", got.Status, got.ReleasedPool)
	}
	if got, _ := stV1.GetHeldJob(ctx, hPartialMeasured2.ID); got.Status != store.HeldStatusReleased || got.ReleasedPool != "demo-pool" {
		t.Errorf("measured 2 = status:%s pool:%s, want released/demo-pool", got.Status, got.ReleasedPool)
	}
	if got, _ := stV1.GetHeldJob(ctx, hAllDel.ID); got.Status != store.HeldStatusHeld {
		t.Errorf("all-deleted still held status=%s, want held", got.Status)
	}

	// Part 2 (Item A): Legacy mode with missing/format_mismatch usage file:
	//   - With unmeasured alt U: registration selects U AND manual & periodic held release both release to U!
	//   - Without unmeasured alt: registration passes (legacy compat) and records snapshot_error in gate_decisions,
	//     while manual & periodic held release both defer!
	stLeg, err := store.Open(ctx, filepath.Join(dir, "legacy.db"), store.Options{InstanceID: "legacy-a"})
	if err != nil {
		t.Fatalf("store.Open legacy: %v", err)
	}
	defer stLeg.Close()

	// Put a v1 file into a legacy-configured server -> format_mismatch (no fallback!).
	v1Misplaced := filepath.Join(dir, "misplaced-v1.json")
	_ = os.WriteFile(v1Misplaced, []byte(`{"schema_version":1,"generated_at":"2026-11-26T20:59:00Z","pools":{"claude":{"measured_at":"2026-11-26T20:58:00Z","weekly":{"remaining_percent":0},"short":{"remaining_percent":0}}}}`), 0o600)

	legPools := map[string]config.PoolConfig{
		"claude":    {},
		"agy-argon": {Unmeasured: true},
	}
	var legLog bytes.Buffer
	legChecker := runtime.NewGateChecker(runtime.GateCheckerOptions{
		Store:       stLeg,
		GateMode:    config.GateModeEnforce,
		UsageFile:   v1Misplaced,
		UsageFormat: config.UsageFormatLegacy,
		Pools:       legPools,
		Now:         func() time.Time { return now },
		Logger:      log.New(&legLog, "", 0),
	})
	legSrv := api.NewServer(stLeg, api.Options{
		InstanceID:       "legacy-a",
		GateMode:         config.GateModeEnforce,
		QuotaUsageFile:   v1Misplaced,
		QuotaUsageFormat: config.UsageFormatLegacy,
		Pools:            legPools,
		Now:              func() time.Time { return now },
		GateReleaser:     testGateReleaser{checker: legChecker},
		Logger:           log.New(&legLog, "", 0),
	})
	tsLeg := httptest.NewServer(legSrv.Handler())
	defer tsLeg.Close()

	// Registration with unmeasured alt U on format_mismatch -> selects U (Item A)!
	respU, err := http.Post(tsLeg.URL+"/jobs", "application/json", strings.NewReader(`{"request_id":"leg-u","title":"Legacy Alt U","pool":"claude","alt_pools":["agy-argon"]}`))
	if err != nil {
		t.Fatalf("POST legacy alt U: %v", err)
	}
	var jobU struct {
		Job struct {
			ID   int64  `json:"id"`
			Pool string `json:"pool"`
		} `json:"job"`
	}
	_ = json.NewDecoder(respU.Body).Decode(&jobU)
	respU.Body.Close()
	if respU.StatusCode != http.StatusCreated || jobU.Job.Pool != "agy-argon" {
		t.Fatalf("Item A legacy alt U on error: status=%d pool=%q, want 201 agy-argon", respU.StatusCode, jobU.Job.Pool)
	}

	// Registration without unmeasured alt on format_mismatch -> passes (legacy compat), records unknown/snapshot_error in gate_decisions!
	respNoU, err := http.Post(tsLeg.URL+"/jobs", "application/json", strings.NewReader(`{"request_id":"leg-nou","title":"Legacy No U","pool":"claude"}`))
	if err != nil {
		t.Fatalf("POST legacy no U: %v", err)
	}
	var jobNoU struct {
		Job struct {
			ID int64 `json:"id"`
		} `json:"job"`
	}
	_ = json.NewDecoder(respNoU.Body).Decode(&jobNoU)
	respNoU.Body.Close()
	if respNoU.StatusCode != http.StatusCreated {
		t.Fatalf("legacy no U on format_mismatch: status=%d, want 201", respNoU.StatusCode)
	}
	legDecs, err := stLeg.ListGateDecisions(ctx, 10)
	if err != nil || len(legDecs) < 2 {
		t.Fatalf("legacy gate_decisions = %+v (err=%v), want >= 2", legDecs, err)
	}
	var foundNoUSnapErr, foundUBypass bool
	for _, d := range legDecs {
		if d.JobID == jobNoU.Job.ID && d.Decision == string(quota.DecisionUnknown) &&
			strings.Contains(d.Reason, quota.ReasonSnapshotError) && strings.Contains(d.Reason, quota.ReasonFormatMismatch) {
			foundNoUSnapErr = true
		}
		if d.JobID == jobU.Job.ID && d.Decision == string(quota.DecisionWouldPass) && d.SelectedPool == "agy-argon" && d.Bypassed {
			foundUBypass = true
		}
	}
	if !foundNoUSnapErr {
		t.Errorf("legacy no-U registration missing unknown/snapshot_error(format_mismatch) gate_decision record: %+v", legDecs)
	}
	if !foundUBypass {
		t.Errorf("legacy with-U registration missing bypassed agy-argon gate_decision record: %+v", legDecs)
	}

	// Manual held release in legacy on format_mismatch: with U -> releases to U; without U -> deferred!
	hLegManualU, _, _ := stLeg.HoldJob(ctx, store.HoldJobParams{
		RequestID: "h-leg-manual-u", Pool: "claude", AltPools: []string{"agy-argon"},
		Payload: store.CreateJobParams{RequestID: "h-leg-manual-u", Title: "Held Leg Manual U", Pool: "claude"}, Reason: "exhausted:weekly",
	})
	hLegManualNoU, _, _ := stLeg.HoldJob(ctx, store.HoldJobParams{
		RequestID: "h-leg-manual-nou", Pool: "claude",
		Payload: store.CreateJobParams{RequestID: "h-leg-manual-nou", Title: "Held Leg Manual No U", Pool: "claude"}, Reason: "exhausted:weekly",
	})
	relWithU, err := legChecker.ReleaseByID(ctx, hLegManualU.ID)
	if err != nil || !relWithU.Released || relWithU.SelectedPool != "agy-argon" {
		t.Fatalf("Item A legacy manual held release with U = %+v err=%v, want released to agy-argon", relWithU, err)
	}
	relNoU, err := legChecker.ReleaseByID(ctx, hLegManualNoU.ID)
	if err != nil || relNoU.Released || !strings.Contains(relNoU.Reason, quota.ReasonSnapshotError) {
		t.Fatalf("legacy manual held release without U = %+v err=%v, want deferred snapshot_error", relNoU, err)
	}

	// Periodic CheckOnce in legacy on format_mismatch: with U -> releases to U; without U -> remains held!
	hLegPeriodicU, _, _ := stLeg.HoldJob(ctx, store.HoldJobParams{
		RequestID: "h-leg-periodic-u", Pool: "claude", AltPools: []string{"agy-argon"},
		Payload: store.CreateJobParams{RequestID: "h-leg-periodic-u", Title: "Held Leg Periodic U", Pool: "claude"}, Reason: "exhausted:weekly",
	})
	if err := legChecker.CheckOnce(ctx); err != nil {
		t.Fatalf("Item A legacy CheckOnce: %v", err)
	}
	gotLegPeriodicU, _ := stLeg.GetHeldJob(ctx, hLegPeriodicU.ID)
	if gotLegPeriodicU.Status != store.HeldStatusReleased || gotLegPeriodicU.ReleasedPool != "agy-argon" {
		t.Fatalf("Item A legacy periodic release with U = status:%s pool:%s, want released/agy-argon", gotLegPeriodicU.Status, gotLegPeriodicU.ReleasedPool)
	}
	gotLegPeriodicNoU, _ := stLeg.GetHeldJob(ctx, hLegManualNoU.ID)
	if gotLegPeriodicNoU.Status != store.HeldStatusHeld {
		t.Fatalf("Item A legacy periodic release without U = status:%s, want held", gotLegPeriodicNoU.Status)
	}
	if !strings.Contains(legLog.String(), "format_mismatch") {
		t.Errorf("expected format_mismatch warning in legacy log, got:\n%s", legLog.String())
	}
	_ = fmt.Sprint(hLegManualU.ID)
}

func TestLegacyPeriodicReleaseUnmeasuredPrimaryPreservesMeasuredAtAndWarning(t *testing.T) {
	now := time.Date(2026, 11, 26, 21, 0, 0, 0, time.UTC)
	snapTime := time.Date(2026, 11, 26, 20, 55, 0, 0, time.UTC)
	dir := t.TempDir()
	ctx := context.Background()

	// Case 1: Healthy Legacy snapshot + held job whose primary pool became unmeasured: true.
	// Periodic CheckOnce must still load the snapshot and record gate_decisions.measured_at.
	stOK, err := store.Open(ctx, filepath.Join(dir, "legacy-ok.db"), store.Options{InstanceID: "legacy-unm-ok"})
	if err != nil {
		t.Fatalf("store.Open legacy-ok: %v", err)
	}
	defer stOK.Close()

	legacyOKPath := filepath.Join(dir, "legacy-ok.json")
	legacyOKJSON := `{"updatedAt":"` + snapTime.Format(time.RFC3339) + `","services":{"claude":{"fiveHour":{"remainingPercent":0},"sevenDay":{"remainingPercent":0}},"codex":{"fiveHour":{"remainingPercent":0},"sevenDay":{"remainingPercent":0}}}}`
	if err := os.WriteFile(legacyOKPath, []byte(legacyOKJSON), 0o600); err != nil {
		t.Fatalf("WriteFile legacy-ok: %v", err)
	}

	// Hold a measured job (codex, exhausted) alongside a job whose primary pool (claude) became unmeasured: true,
	// confirming both single-sweep snapshot reuse and measured_at preservation on the unmeasured-primary release.
	hExhausted, _, err := stOK.HoldJob(ctx, store.HoldJobParams{
		RequestID: "leg-exh-1",
		Pool:      "codex",
		Payload:   store.CreateJobParams{RequestID: "leg-exh-1", Title: "Legacy Exhausted", Pool: "codex"},
		Reason:    "exhausted:weekly",
	})
	if err != nil {
		t.Fatalf("HoldJob leg-exh-1: %v", err)
	}
	hUnmOK, _, err := stOK.HoldJob(ctx, store.HoldJobParams{
		RequestID: "leg-unm-ok",
		Pool:      "claude",
		Payload:   store.CreateJobParams{RequestID: "leg-unm-ok", Title: "Legacy Primary Unmeasured OK", Pool: "claude"},
		Reason:    "exhausted:weekly",
	})
	if err != nil {
		t.Fatalf("HoldJob leg-unm-ok: %v", err)
	}

	okLoadCalls := 0
	var okLog bytes.Buffer
	checkerOK := runtime.NewGateChecker(runtime.GateCheckerOptions{
		Store:       stOK,
		GateMode:    config.GateModeEnforce,
		UsageFile:   legacyOKPath,
		UsageFormat: config.UsageFormatLegacy,
		Pools: map[string]config.PoolConfig{
			"claude": {Unmeasured: true},
			"codex":  {},
		},
		Now:    func() time.Time { return now },
		Logger: log.New(&okLog, "", 0),
		SnapshotLoader: func() (*quota.Snapshot, error) {
			okLoadCalls++
			return quota.LoadUsageFileWithFormat(legacyOKPath, config.UsageFormatLegacy)
		},
	})

	if err := checkerOK.CheckOnce(ctx); err != nil {
		t.Fatalf("CheckOnce legacy ok: %v", err)
	}
	if okLoadCalls != 1 {
		t.Fatalf("legacy CheckOnce loadCalls=%d, want 1", okLoadCalls)
	}
	gotExh, err := stOK.GetHeldJob(ctx, hExhausted.ID)
	if err != nil || gotExh.Status != store.HeldStatusHeld {
		t.Fatalf("exhausted held job = %+v (err=%v), want still held", gotExh, err)
	}
	gotUnmOK, err := stOK.GetHeldJob(ctx, hUnmOK.ID)
	if err != nil || gotUnmOK.Status != store.HeldStatusReleased || gotUnmOK.ReleasedPool != "claude" || gotUnmOK.ReleasedJobID == nil {
		t.Fatalf("legacy unmeasured-primary held job = %+v (err=%v), want released/claude", gotUnmOK, err)
	}
	decsOK, err := stOK.ListGateDecisions(ctx, 10)
	if err != nil {
		t.Fatalf("ListGateDecisions legacy ok: %v", err)
	}
	var foundMeasured bool
	for _, d := range decsOK {
		if d.HeldID == hUnmOK.ID && d.SelectedPool == "claude" && d.Bypassed {
			if d.MeasuredAt == nil || !d.MeasuredAt.Equal(snapTime) {
				t.Fatalf("legacy periodic release gate_decisions.measured_at = %v, want %s", d.MeasuredAt, snapTime.Format(time.RFC3339))
			}
			foundMeasured = true
		}
	}
	if !foundMeasured {
		t.Fatalf("legacy periodic release missing gate_decision for held %s: %+v", hUnmOK.ID, decsOK)
	}

	// Case 2: Broken Legacy snapshot + ONLY a held job whose primary pool became unmeasured: true.
	// Periodic CheckOnce must still load the snapshot, release to claude, and emit the bypass WARNING.
	stErr, err := store.Open(ctx, filepath.Join(dir, "legacy-err.db"), store.Options{InstanceID: "legacy-unm-err"})
	if err != nil {
		t.Fatalf("store.Open legacy-err: %v", err)
	}
	defer stErr.Close()

	legacyBrokenPath := filepath.Join(dir, "legacy-broken.json")
	if err := os.WriteFile(legacyBrokenPath, []byte(`{corrupt-json`), 0o600); err != nil {
		t.Fatalf("WriteFile legacy-broken: %v", err)
	}

	hUnmErr, _, err := stErr.HoldJob(ctx, store.HoldJobParams{
		RequestID: "leg-unm-err",
		Pool:      "claude",
		Payload:   store.CreateJobParams{RequestID: "leg-unm-err", Title: "Legacy Primary Unmeasured SnapErr", Pool: "claude"},
		Reason:    "exhausted:weekly",
	})
	if err != nil {
		t.Fatalf("HoldJob leg-unm-err: %v", err)
	}

	errLoadCalls := 0
	var errLog bytes.Buffer
	checkerErr := runtime.NewGateChecker(runtime.GateCheckerOptions{
		Store:       stErr,
		GateMode:    config.GateModeEnforce,
		UsageFile:   legacyBrokenPath,
		UsageFormat: config.UsageFormatLegacy,
		Pools: map[string]config.PoolConfig{
			"claude": {Unmeasured: true},
		},
		Now:    func() time.Time { return now },
		Logger: log.New(&errLog, "", 0),
		SnapshotLoader: func() (*quota.Snapshot, error) {
			errLoadCalls++
			return quota.LoadUsageFileWithFormat(legacyBrokenPath, config.UsageFormatLegacy)
		},
	})

	if err := checkerErr.CheckOnce(ctx); err != nil {
		t.Fatalf("CheckOnce legacy err: %v", err)
	}
	if errLoadCalls != 1 {
		t.Fatalf("legacy CheckOnce (broken snapshot) loadCalls=%d, want 1", errLoadCalls)
	}
	gotUnmErr, err := stErr.GetHeldJob(ctx, hUnmErr.ID)
	if err != nil || gotUnmErr.Status != store.HeldStatusReleased || gotUnmErr.ReleasedPool != "claude" {
		t.Fatalf("legacy unmeasured-primary held job on broken snapshot = %+v (err=%v), want released/claude", gotUnmErr, err)
	}
	wantWarn := "WARNING: gate release for held " + hUnmErr.ID + " bypassed to claude despite usage snapshot error:"
	if !strings.Contains(errLog.String(), wantWarn) {
		t.Errorf("legacy periodic release missing bypass snapshot-error warning %q, got log:\n%s", wantWarn, errLog.String())
	}
}

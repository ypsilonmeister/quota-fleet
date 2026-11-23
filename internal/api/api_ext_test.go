package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/quota"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

type fakeGateReleaser struct {
	fn func(ctx context.Context, heldID string) (*GateReleaseResult, error)
}

func (f fakeGateReleaser) ReleaseHeld(ctx context.Context, heldID string) (*GateReleaseResult, error) {
	return f.fn(ctx, heldID)
}

func TestReleaseHeldJobContract(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	shortZero := 0
	shortFifty := 50
	resetAt := now.Add(24 * time.Hour)

	healthy := false
	var stRef *store.Store
	releaser := fakeGateReleaser{
		fn: func(ctx context.Context, heldID string) (*GateReleaseResult, error) {
			held, err := stRef.GetHeldJob(ctx, heldID)
			if err != nil {
				return nil, err
			}
			if held.Status != store.HeldStatusHeld {
				return &GateReleaseResult{
					Released: false,
					Reason:   fmt.Sprintf("held job %s is %s (expected held)", held.ID, held.Status),
				}, fmt.Errorf("%w: held job %s is %s", store.ErrConflict, held.ID, held.Status)
			}
			if !healthy {
				return &GateReleaseResult{
					Released: false,
					Reason:   "all candidate pools exhausted (blocked_by=weekly)",
				}, nil
			}
			params := held.Payload
			params.RequestID = held.RequestID
			params.Pool = held.Pool
			job, _, err := stRef.CreateJob(ctx, params)
			if err != nil {
				return nil, err
			}
			if _, err := stRef.MarkHeldReleased(ctx, held.ID, job.ID, held.Pool); err != nil {
				return nil, err
			}
			return &GateReleaseResult{
				Released:     true,
				JobID:        job.ID,
				SelectedPool: held.Pool,
			}, nil
		},
	}

	_, st, ts := newTestServer(t, Options{
		InstanceID: "test-release",
		GateMode:   config.GateModeEnforce,
		Pools: map[string]config.PoolConfig{
			"claude": {Provider: "anthropic"},
		},
		Now: func() time.Time { return now },
		SnapshotLoader: func() (*quota.Snapshot, error) {
			if healthy {
				return &quota.Snapshot{
					MeasuredAt: now,
					Pools: map[string]quota.PoolSnapshot{
						"claude": {
							WeeklyRemainingPercent: 50,
							WeeklyResetAt:          &resetAt,
							ShortRemainingPercent:  &shortFifty,
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
						ShortRemainingPercent:  &shortZero,
						MeasuredAt:             now,
					},
				},
			}, nil
		},
		GateReleaser: releaser,
	})
	stRef = st

	// Hold a job while exhausted -> 202.
	resp := doJSON(t, http.MethodPost, ts.URL+"/jobs", "", CreateJobRequest{
		RequestID: "req-rel-1",
		Title:     "Held task 1",
		Pool:      "claude",
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST /jobs (exhausted) status = %d, want 202", resp.StatusCode)
	}
	heldResp := decodeResp[HoldJobResponse](t, resp)
	heldID := heldResp.HeldID

	// 1. Not found -> 404.
	resp = doJSON(t, http.MethodPost, ts.URL+"/gate/held/held-does-not-exist/release", "", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("release missing held status = %d, want 404", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// 2. Still exhausted -> 409 {"released": false, "reason": ...} (even if caller tries force=true).
	resp = doJSON(t, http.MethodPost, ts.URL+"/gate/held/"+heldID+"/release?force=true", "", map[string]any{"force": true})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("release while exhausted status = %d, want 409", resp.StatusCode)
	}
	notRel := decodeResp[ReleaseHeldResponse](t, resp)
	if notRel.Released || notRel.JobID != 0 || notRel.Reason == "" {
		t.Fatalf("unexpected not-released response: %+v", notRel)
	}

	// 3. Quota recovers -> 200 {"released": true, "job_id": <id>}.
	healthy = true
	resp = doJSON(t, http.MethodPost, ts.URL+"/gate/held/"+heldID+"/release", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("release after recovery status = %d, want 200", resp.StatusCode)
	}
	okRel := decodeResp[ReleaseHeldResponse](t, resp)
	if !okRel.Released || okRel.JobID <= 0 {
		t.Fatalf("unexpected released response: %+v", okRel)
	}
	if _, err := st.GetJob(ctx, okRel.JobID); err != nil {
		t.Fatalf("released job #%d not found in store: %v", okRel.JobID, err)
	}

	// 4a. Already released -> 409.
	resp = doJSON(t, http.MethodPost, ts.URL+"/gate/held/"+heldID+"/release", "", nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("release already-released status = %d, want 409", resp.StatusCode)
	}
	dupRel := decodeResp[ReleaseHeldResponse](t, resp)
	if dupRel.Released || dupRel.Reason == "" {
		t.Fatalf("unexpected already-released response: %+v", dupRel)
	}

	// 4b. Already cancelled -> 409.
	held2, _, err := st.HoldJob(ctx, store.HoldJobParams{
		RequestID: "req-rel-2",
		Pool:      "claude",
		Payload: store.CreateJobParams{
			RequestID: "req-rel-2",
			Title:     "Held task 2",
			Pool:      "claude",
		},
		Reason: "exhausted:weekly",
	})
	if err != nil {
		t.Fatalf("HoldJob: %v", err)
	}
	if _, err := st.CancelHeldJob(ctx, held2.ID); err != nil {
		t.Fatalf("CancelHeldJob: %v", err)
	}
	resp = doJSON(t, http.MethodPost, ts.URL+"/gate/held/"+held2.ID+"/release", "", nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("release already-cancelled status = %d, want 409", resp.StatusCode)
	}
	cancelRel := decodeResp[ReleaseHeldResponse](t, resp)
	if cancelRel.Released || cancelRel.Reason == "" {
		t.Fatalf("unexpected already-cancelled response: %+v", cancelRel)
	}
}

func TestReleaseHeldJobWithoutReleaserReturns503(t *testing.T) {
	_, _, ts := newTestServer(t, Options{InstanceID: "test-no-releaser"})
	resp := doJSON(t, http.MethodPost, ts.URL+"/gate/held/held-123/release", "", nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

func TestListJobsFilters(t *testing.T) {
	_, st, ts := newTestServer(t, Options{InstanceID: "test-job-filters"})
	ctx := context.Background()

	parent, _, err := st.CreateJob(ctx, store.CreateJobParams{
		RequestID: "flt-parent",
		Title:     "Parent build pipeline",
		Agent:     "claude",
		Host:      "ubuntu",
	})
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}
	_, _, err = st.CreateJob(ctx, store.CreateJobParams{
		RequestID: "flt-q1",
		Title:     "Fix 100% CPU_spike in API",
		Agent:     "codex",
		Host:      "ubuntu",
	})
	if err != nil {
		t.Fatalf("create q1: %v", err)
	}
	_, _, err = st.CreateJob(ctx, store.CreateJobParams{
		RequestID: "flt-q2",
		Title:     "Write UI docs",
		Agent:     "claude",
		Host:      "home-win",
	})
	if err != nil {
		t.Fatalf("create q2: %v", err)
	}
	child, _, err := st.CreateJob(ctx, store.CreateJobParams{
		RequestID: "flt-child",
		Title:     "Child follow-up task",
		Agent:     "agy",
		Host:      "ubuntu",
		ParentID:  &parent.ID,
	})
	if err != nil {
		t.Fatalf("create child: %v", err)
	}

	// status is still mandatory -> 400.
	resp := doJSON(t, http.MethodGet, ts.URL+"/jobs?agent=claude", "", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("GET /jobs without status = %d, want 400", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// Filter by agent.
	resp = doJSON(t, http.MethodGet, ts.URL+"/jobs?status=queued&agent=codex", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("filter agent status = %d, want 200", resp.StatusCode)
	}
	list := decodeResp[ListJobsResponse](t, resp)
	if len(list.Jobs) != 1 || list.Jobs[0].Agent != "codex" {
		t.Fatalf("agent filter got %+v", list.Jobs)
	}

	// Filter by host.
	resp = doJSON(t, http.MethodGet, ts.URL+"/jobs?status=queued&host=home-win", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("filter host status = %d, want 200", resp.StatusCode)
	}
	list = decodeResp[ListJobsResponse](t, resp)
	if len(list.Jobs) != 1 || list.Jobs[0].Host != "home-win" {
		t.Fatalf("host filter got %+v", list.Jobs)
	}

	// Filter by q (case-insensitive substring with LIKE special chars % and _).
	qParam := url.QueryEscape("100% cpu_spike")
	resp = doJSON(t, http.MethodGet, ts.URL+"/jobs?status=queued&q="+qParam, "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("filter q status = %d, want 200", resp.StatusCode)
	}
	list = decodeResp[ListJobsResponse](t, resp)
	if len(list.Jobs) != 1 || !strings.Contains(list.Jobs[0].Title, "100% CPU_spike") {
		t.Fatalf("q filter got %+v", list.Jobs)
	}

	// Combined filters (agent + host + q).
	resp = doJSON(t, http.MethodGet, ts.URL+"/jobs?status=queued&agent=claude&host=ubuntu&q=pipeline", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("combined filter status = %d, want 200", resp.StatusCode)
	}
	list = decodeResp[ListJobsResponse](t, resp)
	if len(list.Jobs) != 1 || list.Jobs[0].ID != parent.ID {
		t.Fatalf("combined filter got %+v, want parent %d", list.Jobs, parent.ID)
	}

	// Filter by parent_id (child is waiting).
	resp = doJSON(t, http.MethodGet, fmt.Sprintf("%s/jobs?status=waiting&parent_id=%d", ts.URL, parent.ID), "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("parent_id filter status = %d, want 200", resp.StatusCode)
	}
	list = decodeResp[ListJobsResponse](t, resp)
	if len(list.Jobs) != 1 || list.Jobs[0].ID != child.ID {
		t.Fatalf("parent_id filter got %+v, want child %d", list.Jobs, child.ID)
	}

	// Invalid parent_id -> 400.
	resp = doJSON(t, http.MethodGet, ts.URL+"/jobs?status=queued&parent_id=abc", "", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid parent_id status = %d, want 400", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

func TestDeliveriesStatusAndLimit(t *testing.T) {
	_, st, ts := newTestServer(t, Options{InstanceID: "test-deliveries-ext", MaxClaimed: 5})
	ctx := context.Background()

	// Create 3 terminal jobs: j1 done+delivered, j2 failed+undelivered, j3 done+undelivered.
	mkTerminal := func(reqID, title string, fail bool, deliver bool) int64 {
		j, _, err := st.CreateJob(ctx, store.CreateJobParams{RequestID: reqID, Title: title})
		if err != nil {
			t.Fatalf("CreateJob %s: %v", reqID, err)
		}
		claimed, ok, err := st.ClaimByID(ctx, j.ID, 5)
		if err != nil || !ok {
			t.Fatalf("ClaimByID %d: %v, ok=%v", j.ID, err, ok)
		}
		if fail {
			if _, err := st.Fail(ctx, j.ID, claimed.ClaimedBy, "boom"); err != nil {
				t.Fatalf("Fail %d: %v", j.ID, err)
			}
		} else {
			if _, err := st.Complete(ctx, j.ID, claimed.ClaimedBy, "ok"); err != nil {
				t.Fatalf("Complete %d: %v", j.ID, err)
			}
		}
		if deliver {
			if _, err := st.MarkDelivered(ctx, j.ID); err != nil {
				t.Fatalf("MarkDelivered %d: %v", j.ID, err)
			}
		}
		return j.ID
	}

	j1 := mkTerminal("del-1", "Delivered job", false, true)
	j2 := mkTerminal("del-2", "Undelivered failed", true, false)
	j3 := mkTerminal("del-3", "Undelivered done", false, false)

	// Default (omitted status) == undelivered.
	resp := doJSON(t, http.MethodGet, ts.URL+"/deliveries", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /deliveries status = %d, want 200", resp.StatusCode)
	}
	defResp := decodeResp[DeliveriesResponse](t, resp)
	if len(defResp.Jobs) != 2 || defResp.Jobs[0].ID != j2 || defResp.Jobs[1].ID != j3 {
		t.Fatalf("default deliveries = %+v, want [%d, %d]", defResp.Jobs, j2, j3)
	}

	// status=delivered
	resp = doJSON(t, http.MethodGet, ts.URL+"/deliveries?status=delivered", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /deliveries?status=delivered = %d, want 200", resp.StatusCode)
	}
	delResp := decodeResp[DeliveriesResponse](t, resp)
	if len(delResp.Jobs) != 1 || delResp.Jobs[0].ID != j1 {
		t.Fatalf("delivered = %+v, want [%d]", delResp.Jobs, j1)
	}

	// status=all with limit=2
	resp = doJSON(t, http.MethodGet, ts.URL+"/deliveries?status=all&limit=2", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /deliveries?status=all&limit=2 = %d, want 200", resp.StatusCode)
	}
	allResp := decodeResp[DeliveriesResponse](t, resp)
	if len(allResp.Jobs) != 2 {
		t.Fatalf("all limit=2 len = %d, want 2", len(allResp.Jobs))
	}

	// Invalid status -> 400.
	resp = doJSON(t, http.MethodGet, ts.URL+"/deliveries?status=invalid", "", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid delivery status = %d, want 400", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

func TestListGateDecisionsFilters(t *testing.T) {
	_, st, ts := newTestServer(t, Options{InstanceID: "test-decisions-ext"})
	ctx := context.Background()

	j1, _, err := st.CreateJob(ctx, store.CreateJobParams{RequestID: "dec-j1", Title: "Job 1"})
	if err != nil {
		t.Fatalf("create j1: %v", err)
	}
	j2, _, err := st.CreateJob(ctx, store.CreateJobParams{RequestID: "dec-j2", Title: "Job 2"})
	if err != nil {
		t.Fatalf("create j2: %v", err)
	}

	d1, err := st.RecordGateDecision(ctx, store.GateDecision{
		JobID:         j1.ID,
		Mode:          config.GateModeDryRun,
		RequestedPool: "claude",
		Decision:      string(quota.DecisionWouldPass),
		SelectedPool:  "claude",
	})
	if err != nil {
		t.Fatalf("record d1: %v", err)
	}
	d2, err := st.RecordGateDecision(ctx, store.GateDecision{
		HeldID:        "held-abc",
		Held:          true,
		Mode:          config.GateModeEnforce,
		RequestedPool: "claude",
		Decision:      string(quota.DecisionWouldHold),
		BlockedBy:     "weekly",
	})
	if err != nil {
		t.Fatalf("record d2: %v", err)
	}
	d3, err := st.RecordGateDecision(ctx, store.GateDecision{
		JobID:         j2.ID,
		HeldID:        "held-abc",
		Held:          false,
		Mode:          config.GateModeEnforce,
		RequestedPool: "claude",
		Decision:      string(quota.DecisionWouldPass),
		SelectedPool:  "codex",
	})
	if err != nil {
		t.Fatalf("record d3: %v", err)
	}

	// All decisions (newest-first).
	resp := doJSON(t, http.MethodGet, ts.URL+"/gate/decisions", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /gate/decisions status = %d, want 200", resp.StatusCode)
	}
	all := decodeResp[ListGateDecisionsResponse](t, resp)
	if len(all.Decisions) != 3 || all.Decisions[0].ID != d3.ID || all.Decisions[2].ID != d1.ID {
		t.Fatalf("all decisions = %+v", all.Decisions)
	}

	// Filter by job_id.
	resp = doJSON(t, http.MethodGet, fmt.Sprintf("%s/gate/decisions?job_id=%d", ts.URL, j1.ID), "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("filter job_id status = %d, want 200", resp.StatusCode)
	}
	byJob := decodeResp[ListGateDecisionsResponse](t, resp)
	if len(byJob.Decisions) != 1 || byJob.Decisions[0].ID != d1.ID {
		t.Fatalf("byJob = %+v, want [%d]", byJob.Decisions, d1.ID)
	}

	// Filter by held_id.
	resp = doJSON(t, http.MethodGet, ts.URL+"/gate/decisions?held_id=held-abc", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("filter held_id status = %d, want 200", resp.StatusCode)
	}
	byHeld := decodeResp[ListGateDecisionsResponse](t, resp)
	if len(byHeld.Decisions) != 2 || byHeld.Decisions[0].ID != d3.ID || byHeld.Decisions[1].ID != d2.ID {
		t.Fatalf("byHeld = %+v, want [%d, %d]", byHeld.Decisions, d3.ID, d2.ID)
	}

	// Filter by held_id + limit=1.
	resp = doJSON(t, http.MethodGet, ts.URL+"/gate/decisions?held_id=held-abc&limit=1", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("filter held_id+limit status = %d, want 200", resp.StatusCode)
	}
	limited := decodeResp[ListGateDecisionsResponse](t, resp)
	if len(limited.Decisions) != 1 || limited.Decisions[0].ID != d3.ID {
		t.Fatalf("limited = %+v, want [%d]", limited.Decisions, d3.ID)
	}

	// Invalid job_id -> 400.
	resp = doJSON(t, http.MethodGet, ts.URL+"/gate/decisions?job_id=0", "", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid job_id status = %d, want 400", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// Invalid limit -> 400.
	resp = doJSON(t, http.MethodGet, ts.URL+"/gate/decisions?limit=-5", "", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid limit status = %d, want 400", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

func TestMetaEndpointAndNoSecretLeakage(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "SUPER-SECRET-GEMINI-KEY-99999")
	apiToken := "SUPER-SECRET-API-TOKEN-12345"
	usagePath := "/secret/path/to/usage.json"

	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	short80 := 80
	short0 := 0
	reset1 := now.Add(48 * time.Hour)
	reset2 := now.Add(12 * time.Hour)

	_, _, ts := newTestServer(t, Options{
		InstanceID:     "test-meta",
		Token:          apiToken,
		GateMode:       config.GateModeEnforce,
		EvaluatorMode:  config.EvaluatorModeTypedDecision,
		QuotaUsageFile: usagePath,
		Hosts:          []string{"ubuntu", "home-win", "ubuntu"},
		Agents:         []string{"codex", "claude", "agy"},
		Pools: map[string]config.PoolConfig{
			"claude": {Provider: "anthropic"},
			"codex":  {Provider: "openai"},
			"local":  {Provider: "local", Unmeasured: true},
		},
		Now: func() time.Time { return now },
		SnapshotLoader: func() (*quota.Snapshot, error) {
			return &quota.Snapshot{
				MeasuredAt: now,
				Pools: map[string]quota.PoolSnapshot{
					"claude": {
						WeeklyRemainingPercent: 75,
						WeeklyResetAt:          &reset1,
						ShortRemainingPercent:  &short80,
						MeasuredAt:             now,
					},
					"codex": {
						WeeklyRemainingPercent: 0,
						WeeklyResetAt:          &reset2,
						ShortRemainingPercent:  &short0,
						MeasuredAt:             now,
					},
				},
			}, nil
		},
	})

	// Auth middleware still applies to /meta.
	unauth := doJSON(t, http.MethodGet, ts.URL+"/meta", "", nil)
	if unauth.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated /meta status = %d, want 401", unauth.StatusCode)
	}
	_ = unauth.Body.Close()

	resp := doJSON(t, http.MethodGet, ts.URL+"/meta", apiToken, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /meta status = %d, want 200", resp.StatusCode)
	}
	rawBytes, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("read /meta body: %v", err)
	}
	raw := string(rawBytes)

	// Verify no secrets or sensitive file paths appear in response.
	for _, forbidden := range []string{
		apiToken,
		"SUPER-SECRET-GEMINI-KEY-99999",
		usagePath,
		"id_ed25519",
		"key_file",
		"api_key",
	} {
		if strings.Contains(raw, forbidden) {
			t.Errorf("/meta response leaked forbidden string %q: %s", forbidden, raw)
		}
	}

	var meta MetaResponse
	if err := json.Unmarshal(rawBytes, &meta); err != nil {
		t.Fatalf("unmarshal /meta: %v", err)
	}

	if got, want := strings.Join(meta.Hosts, ","), "home-win,ubuntu"; got != want {
		t.Errorf("hosts = %q, want %q", got, want)
	}
	if got, want := strings.Join(meta.Agents, ","), "agy,claude,codex"; got != want {
		t.Errorf("agents = %q, want %q", got, want)
	}
	if got, want := strings.Join(meta.Pools, ","), "claude,codex,local"; got != want {
		t.Errorf("pools = %q, want %q", got, want)
	}
	if meta.GateMode != config.GateModeEnforce {
		t.Errorf("gate_mode = %q, want %q", meta.GateMode, config.GateModeEnforce)
	}
	if meta.EvaluatorMode != config.EvaluatorModeTypedDecision {
		t.Errorf("evaluator_mode = %q, want %q", meta.EvaluatorMode, config.EvaluatorModeTypedDecision)
	}
	if len(meta.PoolSummaries) != 3 {
		t.Fatalf("pool_summaries len = %d, want 3", len(meta.PoolSummaries))
	}

	byPool := make(map[string]PoolSummaryDTO, len(meta.PoolSummaries))
	for _, ps := range meta.PoolSummaries {
		byPool[ps.Pool] = ps
	}
	if byPool["claude"].Status != string(quota.PoolHealthy) || byPool["claude"].WeeklyRemainingPercent == nil || *byPool["claude"].WeeklyRemainingPercent != 75 {
		t.Errorf("unexpected claude summary: %+v", byPool["claude"])
	}
	if byPool["codex"].Status != string(quota.PoolExhausted) || byPool["codex"].BlockedBy != quota.BlockedByWeeklyAnd5 {
		t.Errorf("unexpected codex summary: %+v", byPool["codex"])
	}
	if byPool["local"].Status != string(quota.PoolUnmeasured) || !byPool["local"].Unmeasured {
		t.Errorf("unexpected local summary: %+v", byPool["local"])
	}
}

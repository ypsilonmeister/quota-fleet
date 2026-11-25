package api

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/quota"
)

func selectorServer(t *testing.T, mode string, snap *quota.Snapshot, now time.Time) string {
	t.Helper()
	pools := map[string]config.PoolConfig{
		"claude":     {AppliesTo: []config.PoolTarget{{Agent: "claude"}}},
		"codex":      {AppliesTo: []config.PoolTarget{{Agent: "codex"}}},
		"agy-gemini": {AppliesTo: []config.PoolTarget{{Agent: "agy", ModelFamily: "gemini"}}},
	}
	_, _, ts := newTestServer(t, Options{
		InstanceID:      "selector-inst",
		SelectorMode:    mode,
		QuotaStaleAfter: 45 * time.Minute,
		Pools:           pools,
		Now:             func() time.Time { return now },
		SnapshotLoader:  func() (*quota.Snapshot, error) { return snap, nil },
	})
	return ts.URL
}

func healthySnap(now time.Time, exhausted ...string) *quota.Snapshot {
	short := 50
	mk := func(w int) quota.PoolSnapshot {
		s := short
		if w == 0 {
			s = 0
		}
		return quota.PoolSnapshot{WeeklyRemainingPercent: w, ShortRemainingPercent: &s, MeasuredAt: now}
	}
	snap := &quota.Snapshot{MeasuredAt: now, Pools: map[string]quota.PoolSnapshot{
		"claude": mk(50), "codex": mk(50), "agy-gemini": mk(50),
	}}
	for _, p := range exhausted {
		snap.Pools[p] = mk(0)
	}
	return snap
}

func postJob(t *testing.T, url string, req CreateJobRequest) (int, *JobDTO, string) {
	t.Helper()
	resp := doJSON(t, http.MethodPost, url+"/jobs", "", req)
	status := resp.StatusCode
	if status != http.StatusCreated {
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return status, nil, string(b)
	}
	out := decodeResp[CreateJobResponse](t, resp)
	return status, out.Job, ""
}

func getJob(t *testing.T, url string, id int64) *JobDTO {
	t.Helper()
	resp := doJSON(t, http.MethodGet, fmt.Sprintf("%s/jobs/%d", url, id), "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET job status %d", resp.StatusCode)
	}
	return decodeResp[*JobDTO](t, resp)
}

func TestSelectorOffKeepsLegacyBehavior(t *testing.T) {
	now := time.Now()
	url := selectorServer(t, "", healthySnap(now), now)
	status, job, _ := postJob(t, url, CreateJobRequest{RequestID: "off-1", Title: "t", Role: "review", Weight: "heavy"})
	if status != http.StatusCreated {
		t.Fatalf("status = %d", status)
	}
	if job.Agent != "" || job.Model != "" || job.Pool != "" || job.SelectionMode != "" || job.SelectedAgent != "" || job.SelectionReason != "" {
		t.Errorf("off mode must not fill or record anything: %+v", job)
	}
}

func TestSelectorRecordRecordsButExecutesDefaults(t *testing.T) {
	now := time.Now()
	url := selectorServer(t, config.SelectorModeRecord, healthySnap(now), now)
	status, job, _ := postJob(t, url, CreateJobRequest{RequestID: "rec-1", Title: "t", Role: "implement", Weight: "heavy"})
	if status != http.StatusCreated {
		t.Fatalf("status = %d", status)
	}
	// Selection (heavy implement -> claude opus) is recorded...
	if job.SelectionMode != "recorded" || job.SelectedAgent != "claude" || job.SelectedModel != "claude-opus-5-5" || job.SelectedPool != "claude" || job.SelectionReason == "" {
		t.Errorf("selection not recorded: %+v", job)
	}
	// ...but execution uses the docs/10 default.
	if job.Agent != "codex" || job.Model != "gpt-6.2-sol" || job.Pool != "codex" {
		t.Errorf("record mode must execute defaults, got %s/%s/%s", job.Agent, job.Model, job.Pool)
	}
	got := getJob(t, url, job.ID)
	if got.SelectionMode != "recorded" || got.SelectedModel != "claude-opus-5-5" || got.SelectionReason != job.SelectionReason {
		t.Errorf("GET /jobs/{id} lost selection record: %+v", got)
	}
}

func TestSelectorApplyExecutesSelection(t *testing.T) {
	now := time.Now()
	url := selectorServer(t, config.SelectorModeApply, healthySnap(now), now)
	status, job, _ := postJob(t, url, CreateJobRequest{RequestID: "app-1", Title: "t", Role: "implement", Weight: "heavy"})
	if status != http.StatusCreated {
		t.Fatalf("status = %d", status)
	}
	if job.SelectionMode != "applied" || job.Agent != "claude" || job.Model != "claude-opus-5-5" || job.Pool != "claude" {
		t.Errorf("apply must execute the selection: %+v", job)
	}
	if job.SelectedAgent != job.Agent || job.SelectedModel != job.Model || job.SelectedPool != job.Pool {
		t.Errorf("record and execution disagree: %+v", job)
	}
}

func TestSelectorApplyAvoidsExhaustedPool(t *testing.T) {
	now := time.Now()
	url := selectorServer(t, config.SelectorModeApply, healthySnap(now, "codex"), now)
	_, job, _ := postJob(t, url, CreateJobRequest{RequestID: "app-ex", Title: "t", Role: "implement", Weight: "standard"})
	if job.Agent != "agy" || job.Pool != "agy-gemini" {
		t.Errorf("exhausted codex must be avoided: %s/%s/%s", job.Agent, job.Model, job.Pool)
	}
	if !strings.Contains(job.SelectionReason, "codex: exhausted") {
		t.Errorf("reason must record the excluded pool: %s", job.SelectionReason)
	}
}

func TestSelectorApplyStaleFallsBackAndSaysSo(t *testing.T) {
	now := time.Now()
	stale := healthySnap(now.Add(-5 * time.Hour))
	url := selectorServer(t, config.SelectorModeApply, stale, now)
	_, job, _ := postJob(t, url, CreateJobRequest{RequestID: "app-stale", Title: "t", Role: "implement", Weight: "standard"})
	if job.Agent != "codex" || job.Model != "gpt-6.2-sol" {
		t.Errorf("stale must fall back to the deterministic default: %+v", job)
	}
	if !strings.Contains(job.SelectionReason, "FALLBACK") || !strings.Contains(job.SelectionReason, "stale") {
		t.Errorf("fallback reason not recorded: %s", job.SelectionReason)
	}
}

func TestSelectorNeverOverridesExplicit(t *testing.T) {
	now := time.Now()
	for _, mode := range []string{config.SelectorModeRecord, config.SelectorModeApply} {
		t.Run(mode, func(t *testing.T) {
			url := selectorServer(t, mode, healthySnap(now), now)

			// Fully explicit: untouched, recorded as explicit.
			_, job, _ := postJob(t, url, CreateJobRequest{RequestID: "x-1", Title: "t", Agent: "agy", Model: "gemini-4-neon-medium", Role: "implement", Weight: "heavy"})
			if job.Agent != "agy" || job.Model != "gemini-4-neon-medium" || job.Pool != "agy-gemini" || job.SelectionMode != "explicit" {
				t.Errorf("explicit overridden: %+v", job)
			}

			// Explicit pool + agent, model missing: agent and pool survive.
			_, job, _ = postJob(t, url, CreateJobRequest{RequestID: "x-2", Title: "t", Agent: "claude", Pool: "claude", Role: "implement", Weight: "standard"})
			if job.Agent != "claude" || job.Pool != "claude" {
				t.Errorf("explicit agent/pool overridden: %+v", job)
			}
			if mode == config.SelectorModeApply && job.Model != "claude-sonnet-5-5" {
				t.Errorf("apply should complete only the model for claude: %+v", job)
			}

			// Explicit model only: kept verbatim.
			_, job, _ = postJob(t, url, CreateJobRequest{RequestID: "x-3", Title: "t", Model: "my-own-model"})
			if job.Model != "my-own-model" {
				t.Errorf("explicit model overridden: %+v", job)
			}
		})
	}
}

func TestSelectorRejectsUnknownRoleWeight(t *testing.T) {
	now := time.Now()
	url := selectorServer(t, config.SelectorModeApply, healthySnap(now), now)
	for _, req := range []CreateJobRequest{
		{RequestID: "bad-r", Title: "t", Role: "wizard"},
		{RequestID: "bad-w", Title: "t", Weight: "gigantic"},
	} {
		if status, _, msg := postJob(t, url, req); status != http.StatusBadRequest {
			t.Errorf("%+v: status %d (%s), want 400", req, status, msg)
		}
	}
}

func TestMetaExposesSelectorMode(t *testing.T) {
	now := time.Now()
	url := selectorServer(t, config.SelectorModeRecord, healthySnap(now), now)
	resp := doJSON(t, http.MethodGet, url+"/meta", "", nil)
	meta := decodeResp[MetaResponse](t, resp)
	if meta.SelectorMode != "record" {
		t.Errorf("selector_mode = %q", meta.SelectorMode)
	}
	url = selectorServer(t, "", healthySnap(now), now)
	meta = decodeResp[MetaResponse](t, doJSON(t, http.MethodGet, url+"/meta", "", nil))
	if meta.SelectorMode != "off" {
		t.Errorf("default selector_mode = %q, want off", meta.SelectorMode)
	}
}

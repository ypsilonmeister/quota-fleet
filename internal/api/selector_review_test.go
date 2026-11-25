package api

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/quota"
)

func selectorGateServer(t *testing.T, selMode, gateMode string, snap *quota.Snapshot, now time.Time, pools map[string]config.PoolConfig) string {
	t.Helper()
	if pools == nil {
		pools = map[string]config.PoolConfig{
			"claude":     {AppliesTo: []config.PoolTarget{{Agent: "claude"}}},
			"codex":      {AppliesTo: []config.PoolTarget{{Agent: "codex"}}},
			"agy-gemini": {AppliesTo: []config.PoolTarget{{Agent: "agy", ModelFamily: "gemini"}}},
		}
	}
	_, _, ts := newTestServer(t, Options{
		InstanceID: "sel-gate", SelectorMode: selMode, GateMode: gateMode,
		QuotaStaleAfter: 45 * time.Minute, Pools: pools,
		Now:            func() time.Time { return now },
		SnapshotLoader: func() (*quota.Snapshot, error) { return snap, nil },
	})
	return ts.URL
}

// Review item 1: the Gate evaluates the pool the job will run on.
func TestSelectorGateHoldsWhenAllApplyCandidatesExhausted(t *testing.T) {
	now := time.Now()
	url := selectorGateServer(t, config.SelectorModeApply, config.GateModeEnforce, healthySnap(now, "codex", "claude", "agy-gemini"), now, nil)
	status, job, body := postJob(t, url, CreateJobRequest{RequestID: "gate-check", Title: "t"})
	if status != http.StatusAccepted {
		t.Fatalf("status = %d (job=%+v body=%s), want 202 hold", status, job, body)
	}
}

func TestSelectorGateHoldsRecordDefaultPoolExhausted(t *testing.T) {
	now := time.Now()
	// Suggested pool (claude) healthy, executed default pool (codex) exhausted.
	url := selectorGateServer(t, config.SelectorModeRecord, config.GateModeEnforce, healthySnap(now, "codex"), now, nil)
	status, job, body := postJob(t, url, CreateJobRequest{RequestID: "rec-gate", Title: "t", Role: "implement", Weight: "heavy"})
	if status != http.StatusAccepted {
		t.Fatalf("status = %d (job=%+v body=%s), want 202 hold", status, job, body)
	}
}

// Review item 2: partial explicit input is completed with compatible values.
func TestSelectorApplyPartialExplicit(t *testing.T) {
	now := time.Now()
	url := selectorServer(t, config.SelectorModeApply, healthySnap(now), now)
	cases := []struct {
		name               string
		req                CreateJobRequest
		agent, model, pool string
	}{
		{"agent only (outside role row)", CreateJobRequest{Agent: "claude", Role: "review", Weight: "light"}, "claude", "claude-sonnet-5-5", "claude"},
		{"agent only", CreateJobRequest{Agent: "codex"}, "codex", "gpt-6.2-sol", "codex"},
		{"agent+model known pair, pool resolved", CreateJobRequest{Agent: "agy", Model: "gemini-4-neon-medium"}, "agy", "gemini-4-neon-medium", "agy-gemini"},
		{"model only", CreateJobRequest{Model: "claude-opus-5-5"}, "claude", "claude-opus-5-5", "claude"},
		{"pool only", CreateJobRequest{Pool: "agy-gemini"}, "agy", "gemini-4-neon-medium", "agy-gemini"},
	}
	for i, c := range cases {
		c.req.RequestID = "partial-" + strconv.Itoa(i)
		c.req.Title = "t"
		status, job, body := postJob(t, url, c.req)
		if status != http.StatusCreated {
			t.Fatalf("%s: status %d %s", c.name, status, body)
		}
		if job.Agent != c.agent || job.Model != c.model || job.Pool != c.pool {
			t.Errorf("%s: executed %s/%s/%s, want %s/%s/%s", c.name, job.Agent, job.Model, job.Pool, c.agent, c.model, c.pool)
		}
		if job.SelectedAgent != job.Agent || job.SelectedModel != job.Model || job.SelectedPool != job.Pool {
			t.Errorf("%s: record %s/%s/%s differs from execution", c.name, job.SelectedAgent, job.SelectedModel, job.SelectedPool)
		}
	}
}

// Review item 3: off accepts and ignores role/weight (docs/19 contract).
func TestSelectorOffIgnoresInvalidHints(t *testing.T) {
	now := time.Now()
	url := selectorServer(t, "", healthySnap(now), now)
	status, _, body := postJob(t, url, CreateJobRequest{RequestID: "off-wiz", Title: "t", Role: "wizard", Weight: "huge"})
	if status != http.StatusCreated {
		t.Fatalf("off must ignore hints: %d %s", status, body)
	}
	url = selectorServer(t, config.SelectorModeRecord, healthySnap(now), now)
	if status, _, _ := postJob(t, url, CreateJobRequest{RequestID: "rec-wiz", Title: "t", Role: "wizard"}); status != http.StatusBadRequest {
		t.Errorf("record must reject invalid role, got %d", status)
	}
}

// Review item 4: fix follows the parent's implementer.
func TestSelectorApplyFixFollowsParent(t *testing.T) {
	now := time.Now()
	url := selectorServer(t, config.SelectorModeApply, healthySnap(now), now)
	_, parent, _ := postJob(t, url, CreateJobRequest{RequestID: "parent", Title: "t", Agent: "codex", Model: "gpt-6.2-sol"})
	status, job, body := postJob(t, url, CreateJobRequest{RequestID: "fix", Title: "t", ParentID: &parent.ID, Role: "fix", Weight: "heavy"})
	if status != http.StatusCreated {
		t.Fatalf("status %d %s", status, body)
	}
	if job.Agent != "codex" || job.Model != "gpt-6.2-sol" {
		t.Errorf("fix must follow parent codex/gpt-6.2-sol, got %s/%s", job.Agent, job.Model)
	}
	if !strings.Contains(job.SelectionReason, "parent job") {
		t.Errorf("reason lacks parent note: %s", job.SelectionReason)
	}
}

// Review item 5: reasons distinguish missing / unmeasured and do not call the chosen pool excluded.
func TestSelectorReasonMissingVsUnmeasured(t *testing.T) {
	now := time.Now()
	_, _, ts := newTestServer(t, Options{
		InstanceID: "sel-missing", SelectorMode: config.SelectorModeApply, QuotaStaleAfter: 45 * time.Minute,
		Pools:          map[string]config.PoolConfig{"agy-gemini": {AppliesTo: []config.PoolTarget{{Agent: "agy", ModelFamily: "gemini"}}}},
		Now:            func() time.Time { return now },
		SnapshotLoader: func() (*quota.Snapshot, error) { return nil, nil },
	})
	_, job, _ := postJob(t, ts.URL, CreateJobRequest{RequestID: "miss", Title: "t", Role: "review", Weight: "light"})
	if job.Pool != "agy-gemini" || !strings.Contains(job.SelectionReason, "usage missing") || strings.Contains(job.SelectionReason, "agy-gemini: unknown(snapshot_error) excluded") {
		t.Errorf("missing-usage reason wrong: pool=%s %s", job.Pool, job.SelectionReason)
	}

	url := selectorGateServer(t, config.SelectorModeApply, "", healthySnap(now), now, map[string]config.PoolConfig{
		"agy-gemini": {Unmeasured: true, AppliesTo: []config.PoolTarget{{Agent: "agy", ModelFamily: "gemini"}}},
	})
	_, job, _ = postJob(t, url, CreateJobRequest{RequestID: "unm", Title: "t", Role: "review", Weight: "light"})
	if !strings.Contains(job.SelectionReason, "unmeasured") || strings.Contains(job.SelectionReason, "usage stale or missing") {
		t.Errorf("unmeasured reason wrong: %s", job.SelectionReason)
	}
}

package api

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/quota"
)

// Round 4, item 4: the switch proposal widens beyond the requested role x weight row.
func TestSelectorFixSwitchProposalWidensBeyondRow(t *testing.T) {
	now := time.Now()
	for _, mode := range []string{config.SelectorModeRecord, config.SelectorModeApply} {
		url := selectorServer(t, mode, healthySnap(now, "codex", "agy-gemini"), now)
		_, parent, _ := postJob(t, url, CreateJobRequest{RequestID: "p", Title: "t", Agent: "codex", Model: "gpt-6.2-sol"})
		status, job, body := postJob(t, url, CreateJobRequest{RequestID: "f", Title: "t", ParentID: &parent.ID, Role: "fix", Weight: "light"})
		if status != http.StatusCreated {
			t.Fatalf("%s: %d %s", mode, status, body)
		}
		if !strings.Contains(job.SelectionReason, "proposing a switch to another line claude/claude-sonnet-5-5") {
			t.Errorf("%s: no Claude switch proposal: %s", mode, job.SelectionReason)
		}
		// Proposal only: the followed parent stays the selection.
		if job.SelectedAgent != "codex" || job.SelectedModel != "gpt-6.2-sol" {
			t.Errorf("%s: selection changed to %s/%s", mode, job.SelectedAgent, job.SelectedModel)
		}
	}
}

// Round 4, item 5: abstain leaves selected_* empty and says what runs from where.
func TestSelectorAbstainPartialExplicitLeavesSelectionEmpty(t *testing.T) {
	now := time.Now()
	for _, mode := range []string{config.SelectorModeRecord, config.SelectorModeApply} {
		url := selectorGateServer(t, mode, config.GateModeEnforce, healthySnap(now), now, map[string]config.PoolConfig{
			"agy-gemini": {Unmeasured: true, AppliesTo: []config.PoolTarget{{Agent: "agy", ModelFamily: "gemini"}}},
		})
		status, job, body := postJob(t, url, CreateJobRequest{RequestID: "abs", Title: "t", Role: "review", Weight: "light", Pool: "agy-gemini"})
		if status != http.StatusCreated {
			t.Fatalf("%s: %d %s", mode, status, body)
		}
		if job.Agent != "codex" || job.Model != "gpt-6.2-sol" || job.Pool != "agy-gemini" {
			t.Errorf("%s: executed %s/%s/%s", mode, job.Agent, job.Model, job.Pool)
		}
		if job.SelectionMode != "abstained" || job.SelectedAgent != "" || job.SelectedModel != "" || job.SelectedPool != "" {
			t.Errorf("%s: mode=%s selected=%q/%q/%q, want abstained and all empty", mode, job.SelectionMode, job.SelectedAgent, job.SelectedModel, job.SelectedPool)
		}
		for _, want := range []string{"全候補が未測定のため自動選択を見送った", "agent=codex(default)", "model=gpt-6.2-sol(default)", "pool=agy-gemini(explicit)"} {
			if !strings.Contains(job.SelectionReason, want) {
				t.Errorf("%s: reason lacks %q: %s", mode, want, job.SelectionReason)
			}
		}
		if strings.Contains(job.SelectionReason, "excluded") {
			t.Errorf("%s: executed pool recorded as excluded: %s", mode, job.SelectionReason)
		}
	}
}

// Round 4, item 5: a fix that would follow its parent also records nothing when abstaining.
func TestSelectorAbstainFixDoesNotRecordParent(t *testing.T) {
	now := time.Now()
	for _, mode := range []string{config.SelectorModeRecord, config.SelectorModeApply} {
		url := selectorGateServer(t, mode, "", healthySnap(now), now, map[string]config.PoolConfig{
			"claude": {Unmeasured: true, AppliesTo: []config.PoolTarget{{Agent: "claude"}}},
		})
		_, parent, _ := postJob(t, url, CreateJobRequest{RequestID: "p", Title: "t", Agent: "claude", Model: "claude-sonnet-5-5"})
		_, job, body := postJob(t, url, CreateJobRequest{RequestID: "f", Title: "t", ParentID: &parent.ID, Role: "fix"})
		if job == nil {
			t.Fatalf("%s: %s", mode, body)
		}
		if job.SelectionMode != "abstained" || job.SelectedAgent != "" || job.SelectedModel != "" || job.SelectedPool != "" {
			t.Errorf("%s: mode=%s selected=%q/%q/%q", mode, job.SelectionMode, job.SelectedAgent, job.SelectedModel, job.SelectedPool)
		}
		if job.Agent != "codex" || strings.Contains(job.SelectionReason, "fix follows parent") || !strings.Contains(job.SelectionReason, "not followed") {
			t.Errorf("%s: agent=%s reason=%s", mode, job.Agent, job.SelectionReason)
		}
	}
}

// The switch proposal is chosen by model line, not merely by a different pool.
func TestSelectorFixSwitchProposalSkipsSameLine(t *testing.T) {
	now := time.Now()
	snap := healthySnap(now)
	short := 0
	snap.Pools["agy-3p"] = quota.PoolSnapshot{WeeklyRemainingPercent: 0, ShortRemainingPercent: &short, MeasuredAt: now}
	_, _, ts := newTestServer(t, Options{
		InstanceID: "sel-line", SelectorMode: config.SelectorModeApply, QuotaStaleAfter: 45 * time.Minute,
		Pools: map[string]config.PoolConfig{
			"claude": {AppliesTo: []config.PoolTarget{{Agent: "claude"}}},
			"codex":  {AppliesTo: []config.PoolTarget{{Agent: "codex"}}},
			"agy-3p": {AppliesTo: []config.PoolTarget{{Agent: "agy", ModelFamily: "claude"}}},
		},
		Now:            func() time.Time { return now },
		SnapshotLoader: func() (*quota.Snapshot, error) { return snap, nil },
	})
	snap.Pools["agy-gemini"] = snap.Pools["agy-3p"]
	_, parent, _ := postJob(t, ts.URL, CreateJobRequest{RequestID: "p", Title: "t", Agent: "agy", Model: "claude-sonnet-5-5-medium", Pool: "agy-3p"})
	_, job, body := postJob(t, ts.URL, CreateJobRequest{RequestID: "f", Title: "t", ParentID: &parent.ID, Role: "fix", Weight: "heavy"})
	if job == nil {
		t.Fatal(body)
	}
	// heavy row starts with claude opus (same Claude line as the parent) -> skipped for codex astra.
	if !strings.Contains(job.SelectionReason, "proposing a switch to another line codex/gpt-6.2-astra") {
		t.Errorf("reason = %s", job.SelectionReason)
	}
}

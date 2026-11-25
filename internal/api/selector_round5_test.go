package api

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/quota"
)

// Round 5, item 4: the switch proposal judges the pool the parent job actually uses.
func TestSelectorFixSwitchProposalUsesActualParentPool(t *testing.T) {
	now := time.Now()
	for _, mode := range []string{config.SelectorModeRecord, config.SelectorModeApply} {
		t.Run(mode, func(t *testing.T) {
			url := selectorServer(t, mode, healthySnap(now, "codex-secondary"), now)
			_, parent, body := postJob(t, url, CreateJobRequest{RequestID: "p", Title: "t", Agent: "codex", Model: "gpt-6.2-sol", Pool: "codex-secondary"})
			if parent == nil || parent.Pool != "codex-secondary" {
				t.Fatalf("parent=%+v body=%s", parent, body)
			}
			status, fix, body := postJob(t, url, CreateJobRequest{RequestID: "f", Title: "t", ParentID: &parent.ID, Role: "fix", Weight: "light"})
			if status != http.StatusCreated {
				t.Fatalf("%d %s", status, body)
			}
			want := "parent pool codex-secondary is exhausted and pool agy-gemini is healthy: proposing a switch to another line agy/gemini-4-neon-medium"
			if !strings.Contains(fix.SelectionReason, want) {
				t.Errorf("reason lacks %q: %s", want, fix.SelectionReason)
			}
			// Proposal only: the followed parent stays the selection.
			if fix.SelectedAgent != "codex" || fix.SelectedModel != "gpt-6.2-sol" {
				t.Errorf("selection changed to %s/%s", fix.SelectedAgent, fix.SelectedModel)
			}
		})
	}
}

// Round 5, item 4: an exhausted table pool does not matter while the parent's pool is healthy.
func TestSelectorFixNoSwitchProposalWhileParentPoolHealthy(t *testing.T) {
	now := time.Now()
	for _, mode := range []string{config.SelectorModeRecord, config.SelectorModeApply} {
		t.Run(mode, func(t *testing.T) {
			snap := healthySnap(now, "codex")
			snap.Pools["codex-secondary"] = snap.Pools["claude"]
			url := selectorServer(t, mode, snap, now)
			_, parent, body := postJob(t, url, CreateJobRequest{RequestID: "p", Title: "t", Agent: "codex", Model: "gpt-6.2-sol", Pool: "codex-secondary"})
			if parent == nil {
				t.Fatal(body)
			}
			_, fix, body := postJob(t, url, CreateJobRequest{RequestID: "f", Title: "t", ParentID: &parent.ID, Role: "fix", Weight: "light"})
			if fix == nil {
				t.Fatal(body)
			}
			if strings.Contains(fix.SelectionReason, "proposing a switch") {
				t.Errorf("unexpected switch proposal: %s", fix.SelectionReason)
			}
		})
	}
}

// A parent stored without a pool is still judged by the pool of its agent/model.
func TestSelectorFixSwitchProposalParentWithoutPool(t *testing.T) {
	now := time.Now()
	snap := healthySnap(now, "codex")
	_, _, ts := newTestServer(t, Options{
		InstanceID: "sel-nopool", SelectorMode: config.SelectorModeRecord, QuotaStaleAfter: 45 * time.Minute,
		Now:            func() time.Time { return now },
		SnapshotLoader: func() (*quota.Snapshot, error) { return snap, nil },
	})
	_, parent, body := postJob(t, ts.URL, CreateJobRequest{RequestID: "p", Title: "t", Agent: "codex", Model: "gpt-6.2-sol"})
	if parent == nil || parent.Pool != "" {
		t.Fatalf("parent=%+v body=%s", parent, body)
	}
	_, fix, body := postJob(t, ts.URL, CreateJobRequest{RequestID: "f", Title: "t", ParentID: &parent.ID, Role: "fix", Weight: "light"})
	if fix == nil {
		t.Fatal(body)
	}
	if !strings.Contains(fix.SelectionReason, "parent pool codex is exhausted") {
		t.Errorf("reason = %s", fix.SelectionReason)
	}
}

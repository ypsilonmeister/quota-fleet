package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/quota"
	"github.com/ypsilonmeister/quota-fleet/internal/runtime"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

// A keyed registration filled in by the Selector (docs/19) keeps both records through a
// Gate hold and its release (docs/20).
func TestSelectorRecordSurvivesKeyedHoldAndRelease(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	pools := map[string]config.PoolConfig{
		"claude": {AppliesTo: []config.PoolTarget{{Agent: "claude"}}},
		"codex":  {AppliesTo: []config.PoolTarget{{Agent: "codex"}}},
	}
	// Record mode executes on the default pool (codex), which starts exhausted.
	snap := healthySnap(now, "codex")
	load := func() (*quota.Snapshot, error) { return snap, nil }
	_, st, ts := newTestServer(t, Options{
		InstanceID: "selector-supersede", SelectorMode: config.SelectorModeRecord,
		GateMode: config.GateModeEnforce, Pools: pools, Now: func() time.Time { return now },
		SnapshotLoader: load,
	})
	hold := func(requestID string) string {
		t.Helper()
		resp := doJSON(t, http.MethodPost, ts.URL+"/jobs", "", CreateJobRequest{
			RequestID: requestID, Title: "publish", Role: "implement", Weight: "heavy",
			SupersedeScope: "pr1", SupersedeKey: "publish",
		})
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("hold %s = %d, want 202", requestID, resp.StatusCode)
		}
		return decodeResp[HoldJobResponse](t, resp).HeldID
	}
	old := hold("r1")
	current := hold("r2")
	if h, err := st.GetHeldJob(ctx, old); err != nil || h.Status != store.HeldStatusCancelled {
		t.Fatalf("old held = %+v, %v", h, err)
	}

	snap = healthySnap(now)
	checker := runtime.NewGateChecker(runtime.GateCheckerOptions{
		Store: st, GateMode: config.GateModeEnforce, Pools: pools,
		Now: func() time.Time { return now }, SnapshotLoader: load,
	})
	res, err := checker.ReleaseByID(ctx, current)
	if err != nil || res.Job == nil {
		t.Fatalf("release = %+v, %v", res, err)
	}
	job := getJob(t, ts.URL, res.Job.ID)
	if job.SupersedeScope != "pr1" || job.SupersedeKey != "publish" ||
		len(job.SupersededHeldIDs) != 1 || job.SupersededHeldIDs[0] != old {
		t.Errorf("supersede record = %q/%q %+v", job.SupersedeScope, job.SupersedeKey, job.SupersedeDTO)
	}
	if job.SelectionMode != "recorded" || job.SelectedModel != "claude-opus-5-5" || job.SelectedPool != "claude" ||
		job.SelectionReason == "" || job.Agent != "codex" || job.Pool != "codex" {
		t.Errorf("selection record = %+v", job)
	}
}

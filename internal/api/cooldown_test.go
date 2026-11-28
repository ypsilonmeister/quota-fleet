package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

func TestCooldownVisibilityAndClearScope(t *testing.T) {
	ctx := context.Background()
	enabled := true
	cfg := &config.Config{
		Pools: map[string]config.PoolConfig{"p": {Unmeasured: true}},
		Quota: config.QuotaConfig{GateMode: config.GateModeEnforce, Cooldown: config.CooldownConfig{Enabled: &enabled}},
	}
	_, st, ts := newTestServer(t, Options{
		GateMode: config.GateModeEnforce, QuotaUsageFormat: config.UsageFormatV1,
		Pools: cfg.Pools, Config: cfg, Token: "operator",
		Tokens: []config.APITokenConfig{{Token: "viewer", Scopes: []string{config.ScopeRead}}},
	})
	if _, _, err := st.CreateJob(ctx, store.CreateJobParams{RequestID: "first", Title: "first", Pool: "p"}); err != nil {
		t.Fatal(err)
	}
	j, ok, err := st.Claim(ctx, 1)
	if err != nil || !ok {
		t.Fatalf("claim=%t error=%v", ok, err)
	}
	if _, err := st.FailWithCooldown(ctx, j.ID, j.ClaimedBy, "failed", "failed", "quota"); err != nil {
		t.Fatal(err)
	}
	meta := decodeResp[MetaResponse](t, doJSON(t, http.MethodGet, ts.URL+"/meta", "viewer", nil))
	if !meta.CooldownEnabled || len(meta.PoolSummaries) != 1 || meta.PoolSummaries[0].Status != "cooldown" ||
		meta.PoolSummaries[0].Cooldown == nil || !meta.PoolSummaries[0].Cooldown.Active {
		t.Fatalf("meta=%+v", meta)
	}
	status := decodeResp[StatusResponse](t, doJSON(t, http.MethodGet, ts.URL+"/status", "viewer", nil))
	if len(status.Cooldowns) != 1 || status.Cooldowns[0].Pool != "p" {
		t.Fatalf("status=%+v", status)
	}
	resp := doJSON(t, http.MethodPost, ts.URL+"/jobs", "operator", map[string]string{"request_id": "next", "title": "next", "pool": "p"})
	if resp.StatusCode != http.StatusAccepted {
		resp.Body.Close()
		t.Fatalf("POST /jobs=%d", resp.StatusCode)
	}
	held := decodeResp[HoldJobResponse](t, resp)
	if held.Held == nil || !strings.Contains(held.Held.Reason, "cooldown") || !strings.Contains(held.Held.SnapshotSummary, "cooldown_until") {
		t.Fatalf("held=%+v", held)
	}

	for _, tc := range []struct {
		token string
		want  int
	}{{"viewer", http.StatusForbidden}, {"operator", http.StatusNoContent}} {
		resp = doJSON(t, http.MethodDelete, ts.URL+"/gate/cooldowns/p", tc.token, nil)
		resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Fatalf("DELETE by %s=%d want %d", tc.token, resp.StatusCode, tc.want)
		}
	}
	status = decodeResp[StatusResponse](t, doJSON(t, http.MethodGet, ts.URL+"/status", "viewer", nil))
	if len(status.Cooldowns) != 0 {
		t.Fatalf("status after clear=%+v", status)
	}
	rows, err := st.ListCooldowns(ctx)
	if err != nil || len(rows) != 1 || rows[0].ClearedReason != "manual" {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
}

package api

import (
	"context"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/quota"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

func TestMetaAgentsUnionAndModelsCatalog(t *testing.T) {
	cfg := &config.Config{
		InstanceID: "meta-models-test",
		Hosts: map[string]config.HostConfig{
			"ubuntu": {
				Agents: map[string]config.HostAgentOverride{
					"claude": {},
					"agy":    {},
				},
			},
			"home-win": {
				Agents: map[string]config.HostAgentOverride{
					"codex": {},
				},
			},
		},
		Agents: map[string]config.AgentConfig{
			// Note: "claude" is only under hosts.ubuntu.agents (no top-level entry).
			// "top-only" is only at top-level.
			"agy": {},
			"codex": {
				Models: []config.ModelConfig{
					{ID: "gpt-6.2-sol", DisplayName: "GPT-6.2-Sol"},
				},
			},
			"top-only": {},
		},
		Pools: map[string]config.PoolConfig{
			"claude": {
				Provider:  "claude-usage",
				AppliesTo: []config.PoolTarget{{Agent: "claude"}},
			},
			"agy-gemini": {
				Provider:  "agy-usage",
				AppliesTo: []config.PoolTarget{{Agent: "agy", ModelFamily: "gemini"}},
			},
			"agy-3p": {
				Provider:  "agy-usage",
				AppliesTo: []config.PoolTarget{{Agent: "agy", ModelFamily: "claude-gpt"}},
			},
		},
	}

	_, st, ts := newTestServer(t, Options{
		InstanceID: "meta-models-test",
		Pools:      cfg.Pools,
		Config:     cfg,
	})

	// Seed probed agy models in DB for ubuntu.
	err := st.UpsertAgentModels(context.Background(), store.AgentModelCatalog{
		Host:   "ubuntu",
		Agent:  "agy",
		Source: "agy_cli",
		Models: []store.AgentModelItem{
			{ID: "gemini-4-neon-medium", DisplayName: "Gemini 4 Neon (Medium)", Family: quota.ModelFamilyGemini},
			{ID: "claude-opus-5-5-high", DisplayName: "Claude Opus 5.5 (High)", Family: quota.ModelFamilyClaude},
		},
		CheckedAt: time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("UpsertAgentModels: %v", err)
	}

	resp := doJSON(t, http.MethodGet, ts.URL+"/meta", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /meta status = %d, want 200", resp.StatusCode)
	}
	meta := decodeResp[MetaResponse](t, resp)

	// Agents must be union of all host agents ∪ top-level agents.
	wantAgents := []string{"agy", "claude", "codex", "top-only"}
	if !reflect.DeepEqual(meta.Agents, wantAgents) {
		t.Errorf("meta.Agents = %v, want %v", meta.Agents, wantAgents)
	}
	wantHosts := []string{"home-win", "ubuntu"}
	if !reflect.DeepEqual(meta.Hosts, wantHosts) {
		t.Errorf("meta.Hosts = %v, want %v", meta.Hosts, wantHosts)
	}
	if !reflect.DeepEqual(meta.HostAgents["ubuntu"], []string{"agy", "claude"}) {
		t.Errorf("meta.HostAgents[ubuntu] = %v, want [agy claude]", meta.HostAgents["ubuntu"])
	}
	if !reflect.DeepEqual(meta.HostAgents["home-win"], []string{"codex"}) {
		t.Errorf("meta.HostAgents[home-win] = %v, want [codex]", meta.HostAgents["home-win"])
	}

	// DB-probed agy models must be returned under ubuntu/agy.
	ubuntuAgy := meta.Models["ubuntu"]["agy"]
	if len(ubuntuAgy) != 2 || ubuntuAgy[0].ID != "gemini-4-neon-medium" || ubuntuAgy[1].ID != "claude-opus-5-5-high" {
		t.Errorf("meta.Models[ubuntu][agy] = %+v", ubuntuAgy)
	}
	// Config catalog fallback for home-win/codex must also be present.
	winCodex := meta.Models["home-win"]["codex"]
	if len(winCodex) != 1 || winCodex[0].ID != "gpt-6.2-sol" {
		t.Errorf("meta.Models[home-win][codex] = %+v", winCodex)
	}
	// Pool rules must be exposed for UI pool preview.
	if len(meta.PoolRules["agy-gemini"]) != 1 || meta.PoolRules["agy-gemini"][0].ModelFamily != "gemini" {
		t.Errorf("meta.PoolRules[agy-gemini] = %+v", meta.PoolRules["agy-gemini"])
	}
}

func TestMetaClaudeCatalogDeletionClearsStaleDBSnapshot(t *testing.T) {
	// Simulate serve restart after deleting all entries from agents.claude.models in fleet.yaml,
	// while an old claude snapshot still exists in agent_models DB.
	cfg := &config.Config{
		InstanceID: "claude-delete-test",
		Hosts: map[string]config.HostConfig{
			"ubuntu": {
				Agents: map[string]config.HostAgentOverride{
					"claude": {},
				},
			},
		},
		Agents: map[string]config.AgentConfig{
			// agents.claude.models intentionally empty (all deleted).
			"claude": {},
		},
	}

	_, st, ts := newTestServer(t, Options{
		InstanceID: "claude-delete-test",
		Config:     cfg,
	})

	// Seed a stale claude snapshot in DB from an earlier run.
	err := st.UpsertAgentModels(context.Background(), store.AgentModelCatalog{
		Host:   "ubuntu",
		Agent:  "claude",
		Source: "config",
		Models: []store.AgentModelItem{
			{ID: "claude-old-model", DisplayName: "Claude Old Model", Family: quota.ModelFamilyClaude},
		},
		CheckedAt: time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("UpsertAgentModels: %v", err)
	}

	resp := doJSON(t, http.MethodGet, ts.URL+"/meta", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /meta status = %d, want 200", resp.StatusCode)
	}
	meta := decodeResp[MetaResponse](t, resp)

	if got := meta.Models["ubuntu"]["claude"]; len(got) != 0 {
		t.Errorf("meta.Models[ubuntu][claude] = %+v, want empty (deleted from config)", got)
	}
}

func TestCreateJobAutoDerivesPoolFromAppliesTo(t *testing.T) {
	pools := map[string]config.PoolConfig{
		"claude": {
			Provider:  "claude-usage",
			AppliesTo: []config.PoolTarget{{Agent: "claude"}},
		},
		"codex": {
			Provider:  "codex-usage",
			AppliesTo: []config.PoolTarget{{Agent: "codex"}},
		},
		"agy-gemini": {
			Provider:  "agy-usage",
			AppliesTo: []config.PoolTarget{{Agent: "agy", ModelFamily: "gemini"}},
		},
		"agy-3p": {
			Provider:  "agy-usage",
			AppliesTo: []config.PoolTarget{{Agent: "agy", ModelFamily: "claude-gpt"}},
		},
	}

	_, _, ts := newTestServer(t, Options{
		InstanceID: "pool-derive-test",
		GateMode:   config.GateModeOff,
		Pools:      pools,
	})

	cases := []struct {
		name     string
		agent    string
		model    string
		reqPool  string
		wantPool string
	}{
		{name: "claude auto", agent: "claude", model: "claude-sonnet-4-6", reqPool: "", wantPool: "claude"},
		{name: "claude auto empty model", agent: "claude", model: "", reqPool: "", wantPool: "claude"},
		{name: "codex auto", agent: "codex", model: "gpt-6.2-sol", reqPool: "", wantPool: "codex"},
		{name: "agy gemini auto", agent: "agy", model: "gemini-4-neon-medium", reqPool: "", wantPool: "agy-gemini"},
		{name: "agy gemini-gpt-preview uses canonical gemini family", agent: "agy", model: "gemini-gpt-preview", reqPool: "", wantPool: "agy-gemini"},
		{name: "agy claude auto", agent: "agy", model: "claude-opus-5-5-high", reqPool: "", wantPool: "agy-3p"},
		{name: "agy gpt auto", agent: "agy", model: "gpt-oss-120b-medium", reqPool: "", wantPool: "agy-3p"},
		{name: "agy custom-gpt-model uses canonical unknown family", agent: "agy", model: "custom-gpt-model", reqPool: "", wantPool: ""},
		{name: "explicit pool wins", agent: "agy", model: "gemini-4-neon-medium", reqPool: "agy-3p", wantPool: "agy-3p"},
		{name: "unmatched model leaves pool empty", agent: "agy", model: "unknown-model-x", reqPool: "", wantPool: ""},
		{name: "agy empty model leaves pool empty", agent: "agy", model: "", reqPool: "", wantPool: ""},
		{name: "unknown agent leaves pool empty", agent: "other-agent", model: "foo", reqPool: "", wantPool: ""},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := doJSON(t, http.MethodPost, ts.URL+"/jobs", "", CreateJobRequest{
				RequestID: "derive-req-" + tc.name + "-" + string(rune('a'+i)),
				Title:     "Test " + tc.name,
				Agent:     tc.agent,
				Host:      "ubuntu",
				Model:     tc.model,
				Pool:      tc.reqPool,
			})
			if resp.StatusCode != http.StatusCreated {
				t.Fatalf("POST /jobs status = %d, want 201", resp.StatusCode)
			}
			got := decodeResp[CreateJobResponse](t, resp)
			if got.Job.Pool != tc.wantPool {
				t.Errorf("job.Pool = %q, want %q", got.Job.Pool, tc.wantPool)
			}
		})
	}
}

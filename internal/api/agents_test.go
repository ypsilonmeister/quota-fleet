package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/agentver"
	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

type stubAgentChecker struct {
	fn func(ctx context.Context) (*agentver.CheckResult, error)
}

func (s stubAgentChecker) CheckOnce(ctx context.Context) (*agentver.CheckResult, error) {
	return s.fn(ctx)
}

func TestAgentsAPIEndpointsAndSinceFilter(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, ":memory:", store.Options{InstanceID: "test"})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	cfg := &config.Config{
		InstanceID: "test",
		Hosts: map[string]config.HostConfig{
			"ubuntu":   {},
			"home-win": {Agents: map[string]config.HostAgentOverride{"codex": {Pin: "0.169.0"}}},
		},
		Agents: map[string]config.AgentConfig{
			"claude": {},
			"codex":  {},
		},
	}

	t0 := time.Date(2026, 11, 24, 6, 0, 0, 0, time.UTC)
	t1 := t0.Add(10 * time.Minute)
	t2 := t0.Add(20 * time.Minute)

	_ = st.UpsertAgentVersion(ctx, store.AgentVersion{
		Host: "ubuntu", Agent: "claude", VersionRaw: "2.1.76 (Claude Code)", Version: "2.1.76",
		Status: store.AgentVersionStatusOK, CheckedAt: t0,
	})
	_ = st.UpsertAgentVersion(ctx, store.AgentVersion{
		Host: "home-win", Agent: "claude", VersionRaw: "2.1.70 (Claude Code)", Version: "2.1.70",
		Status: store.AgentVersionStatusOK, CheckedAt: t0,
	})
	_ = st.UpsertAgentVersion(ctx, store.AgentVersion{
		Host: "ubuntu", Agent: "codex", VersionRaw: "0.171.0", Version: "0.171.0",
		Status: store.AgentVersionStatusOK, CheckedAt: t0,
	})
	_ = st.UpsertAgentVersion(ctx, store.AgentVersion{
		Host: "home-win", Agent: "codex", VersionRaw: "0.169.0", Version: "0.169.0",
		Status: store.AgentVersionStatusOK, CheckedAt: t0,
	})

	_, _ = st.RecordAgentVersionEvent(ctx, store.AgentVersionEvent{
		EventType: store.AgentEventDriftStarted, Agent: "claude", Host: "home-win",
		NewVersion: "2.1.76", Detail: "drift started", CreatedAt: t1,
	})
	_, _ = st.RecordAgentVersionEvent(ctx, store.AgentVersionEvent{
		EventType: store.AgentEventVersionChanged, Agent: "claude", Host: "home-win",
		OldVersion: "2.1.70", NewVersion: "2.1.76", Detail: "upgraded", CreatedAt: t2,
	})

	checkCalled := false
	srv := NewServer(st, Options{
		InstanceID: "test",
		Token:      "secret-token",
		Config:     cfg,
		Hosts:      []string{"ubuntu", "home-win"},
		Agents:     []string{"claude", "codex"},
		AgentChecker: stubAgentChecker{fn: func(ctx context.Context) (*agentver.CheckResult, error) {
			checkCalled = true
			rows, err := st.ListAgentVersions(ctx)
			if err != nil {
				return nil, err
			}
			return &agentver.CheckResult{
				Matrix: agentver.BuildMatrix(cfg, rows),
			}, nil
		}},
	})

	// 1. Auth required on /agents.
	req := httptest.NewRequest(http.MethodGet, "/agents", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET /agents without token status = %d, want 401", rec.Code)
	}

	// 2. GET /agents returns matrix with claude drift and codex pinned.
	req = httptest.NewRequest(http.MethodGet, "/agents", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /agents status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var agentsResp AgentsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &agentsResp); err != nil {
		t.Fatalf("decode GET /agents: %v", err)
	}
	if !agentsResp.HasDrift {
		t.Errorf("expected HasDrift=true")
	}
	if len(agentsResp.Hosts) != 2 || len(agentsResp.Agents) != 2 {
		t.Errorf("unexpected hosts/agents: %v / %v", agentsResp.Hosts, agentsResp.Agents)
	}

	// 3. GET /agents/events?since=t1 returns only the t2 event.
	req = httptest.NewRequest(http.MethodGet, "/agents/events?since="+url.QueryEscape(t1.Format(time.RFC3339Nano)), nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /agents/events status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var eventsResp AgentEventsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &eventsResp); err != nil {
		t.Fatalf("decode GET /agents/events: %v", err)
	}
	if len(eventsResp.Events) != 1 || eventsResp.Events[0].EventType != string(store.AgentEventVersionChanged) {
		t.Errorf("expected 1 event after t1, got %+v", eventsResp.Events)
	}

	// 4. POST /agents/check invokes checker and returns 200.
	req = httptest.NewRequest(http.MethodPost, "/agents/check", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /agents/check status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if !checkCalled {
		t.Errorf("expected AgentChecker.CheckOnce to be called")
	}
}

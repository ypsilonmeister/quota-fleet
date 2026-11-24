package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/ypsilonmeister/quota-fleet/internal/agentver"
	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/quota"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

type tokenTestGateReleaser struct{}

func (tokenTestGateReleaser) ReleaseHeld(_ context.Context, heldID string) (*GateReleaseResult, error) {
	return &GateReleaseResult{Released: false, Reason: "stub:" + heldID}, nil
}

type tokenTestAgentChecker struct{}

func (tokenTestAgentChecker) CheckOnce(_ context.Context) (*agentver.CheckResult, error) {
	return &agentver.CheckResult{}, nil
}

func TestAPITokenScopeEndpointMatrix(t *testing.T) {
	_, st, ts := newTestServer(t, Options{
		InstanceID: "inst-scopes",
		MaxClaimed: 5,
		Token:      "tok-legacy-secret",
		Tokens: []config.APITokenConfig{
			{Name: "coordinator", Token: "tok-coord-secret", Scopes: []string{"read", "register", "operate", "worker", "agents"}},
			{Name: "watch", Token: "tok-watch-secret", Scopes: []string{"read"}},
			{Name: "registrar", Token: "tok-reg-secret", Scopes: []string{"register"}},
			{Name: "operator", Token: "tok-op-secret", Scopes: []string{"operate"}},
			{Name: "worker-bot", Token: "tok-worker-secret", Scopes: []string{"worker"}},
			{Name: "agents-bot", Token: "tok-agents-secret", Scopes: []string{"agents"}},
		},
		GateReleaser: tokenTestGateReleaser{},
		AgentChecker: tokenTestAgentChecker{},
	})

	// Seed a job directly in store so job endpoints have a valid target ID.
	job, _, err := st.CreateJob(context.Background(), store.CreateJobParams{
		RequestID: "req-matrix-seed",
		Title:     "Matrix Seed Job",
	})
	if err != nil {
		t.Fatalf("seed job: %v", err)
	}
	jobID := job.ID

	type endpointCase struct {
		name          string
		method        string
		path          string
		body          any
		requiredScope string
	}

	endpoints := []endpointCase{
		// read
		{name: "GET /jobs", method: http.MethodGet, path: "/jobs?status=queued", requiredScope: config.ScopeRead},
		{name: "GET /jobs/{id}", method: http.MethodGet, path: fmt.Sprintf("/jobs/%d", jobID), requiredScope: config.ScopeRead},
		{name: "GET /status", method: http.MethodGet, path: "/status", requiredScope: config.ScopeRead},
		{name: "GET /meta", method: http.MethodGet, path: "/meta", requiredScope: config.ScopeRead},
		{name: "GET /deliveries", method: http.MethodGet, path: "/deliveries", requiredScope: config.ScopeRead},
		{name: "GET /gate/held", method: http.MethodGet, path: "/gate/held", requiredScope: config.ScopeRead},
		{name: "GET /gate/decisions", method: http.MethodGet, path: "/gate/decisions", requiredScope: config.ScopeRead},
		{name: "GET /agents", method: http.MethodGet, path: "/agents", requiredScope: config.ScopeRead},
		{name: "GET /agents/events", method: http.MethodGet, path: "/agents/events", requiredScope: config.ScopeRead},

		// register
		{name: "POST /jobs", method: http.MethodPost, path: "/jobs", body: CreateJobRequest{RequestID: "req-matrix-post", Title: "Matrix Post"}, requiredScope: config.ScopeRegister},

		// operate
		{name: "POST /jobs/{id}/cancel", method: http.MethodPost, path: fmt.Sprintf("/jobs/%d/cancel", jobID+999), body: ReasonRequest{Reason: "test"}, requiredScope: config.ScopeOperate},
		{name: "POST /jobs/{id}/requeue", method: http.MethodPost, path: fmt.Sprintf("/jobs/%d/requeue", jobID+999), requiredScope: config.ScopeOperate},
		{name: "POST /jobs/{id}/reset", method: http.MethodPost, path: fmt.Sprintf("/jobs/%d/reset", jobID+999), requiredScope: config.ScopeOperate},
		{name: "POST /jobs/{id}/deliver", method: http.MethodPost, path: fmt.Sprintf("/jobs/%d/deliver", jobID+999), requiredScope: config.ScopeOperate},
		{name: "POST /gate/held/{id}/release", method: http.MethodPost, path: "/gate/held/held-missing/release", requiredScope: config.ScopeOperate},
		{name: "DELETE /gate/held/{id}", method: http.MethodDelete, path: "/gate/held/held-missing", requiredScope: config.ScopeOperate},
		{name: "POST /agents/check", method: http.MethodPost, path: "/agents/check", requiredScope: config.ScopeOperate},

		// worker
		{name: "POST /jobs/{id}/claim", method: http.MethodPost, path: fmt.Sprintf("/jobs/%d/claim", jobID+999), requiredScope: config.ScopeWorker},
		{name: "POST /jobs/{id}/complete", method: http.MethodPost, path: fmt.Sprintf("/jobs/%d/complete", jobID+999), body: CompleteRequest{Token: "x"}, requiredScope: config.ScopeWorker},
		{name: "POST /jobs/{id}/fail", method: http.MethodPost, path: fmt.Sprintf("/jobs/%d/fail", jobID+999), body: FailRequest{Token: "x"}, requiredScope: config.ScopeWorker},
		{name: "POST /jobs/{id}/cancel-claimed", method: http.MethodPost, path: fmt.Sprintf("/jobs/%d/cancel-claimed", jobID+999), body: FailRequest{Token: "x"}, requiredScope: config.ScopeWorker},
		{name: "POST /jobs/{id}/reject-invalid", method: http.MethodPost, path: fmt.Sprintf("/jobs/%d/reject-invalid", jobID+999), body: ReasonRequest{Reason: "invalid"}, requiredScope: config.ScopeWorker},
	}

	type tokenCase struct {
		name   string
		token  string
		scopes map[string]bool
	}

	tokens := []tokenCase{
		{name: "legacy", token: "tok-legacy-secret", scopes: map[string]bool{"read": true, "register": true, "operate": true, "worker": true, "agents": true}},
		{name: "coordinator", token: "tok-coord-secret", scopes: map[string]bool{"read": true, "register": true, "operate": true, "worker": true, "agents": true}},
		{name: "watch", token: "tok-watch-secret", scopes: map[string]bool{"read": true}},
		{name: "registrar", token: "tok-reg-secret", scopes: map[string]bool{"register": true}},
		{name: "operator", token: "tok-op-secret", scopes: map[string]bool{"operate": true}},
		{name: "worker-bot", token: "tok-worker-secret", scopes: map[string]bool{"worker": true}},
		{name: "agents-bot", token: "tok-agents-secret", scopes: map[string]bool{"agents": true}},
	}

	for _, ep := range endpoints {
		// Unauthenticated -> 401
		t.Run(ep.name+"/no_token", func(t *testing.T) {
			resp := doJSON(t, ep.method, ts.URL+ep.path, "", ep.body)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", resp.StatusCode)
			}
		})

		// Wrong token -> 401
		t.Run(ep.name+"/wrong_token", func(t *testing.T) {
			resp := doJSON(t, ep.method, ts.URL+ep.path, "wrong-secret", ep.body)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", resp.StatusCode)
			}
		})

		for _, tc := range tokens {
			t.Run(ep.name+"/"+tc.name, func(t *testing.T) {
				resp := doJSON(t, ep.method, ts.URL+ep.path, tc.token, ep.body)
				rawBody, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()

				allowed := tc.scopes[ep.requiredScope]
				if allowed {
					if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
						t.Fatalf("expected %s to be allowed for %s (required=%s), got %d: %s",
							tc.name, ep.name, ep.requiredScope, resp.StatusCode, string(rawBody))
					}
					return
				}

				if resp.StatusCode != http.StatusForbidden {
					t.Fatalf("status = %d, want 403 (body=%s)", resp.StatusCode, string(rawBody))
				}
				var forb ForbiddenResponse
				if err := json.Unmarshal(rawBody, &forb); err != nil {
					t.Fatalf("unmarshal 403 body %q: %v", string(rawBody), err)
				}
				if forb.RequiredScope != ep.requiredScope {
					t.Errorf("required_scope = %q, want %q", forb.RequiredScope, ep.requiredScope)
				}
				if !strings.Contains(forb.Error, ep.requiredScope) {
					t.Errorf("error = %q, expected to mention missing scope %q", forb.Error, ep.requiredScope)
				}
				// Must not leak token name or secret value in 403 response.
				if strings.Contains(string(rawBody), tc.token) || strings.Contains(string(rawBody), tc.name) {
					t.Errorf("403 body leaked token info: %s", string(rawBody))
				}
			})
		}
	}

	// UI static assets remain accessible without authentication.
	resp, err := http.Get(ts.URL + "/ui/")
	if err != nil {
		t.Fatalf("GET /ui/: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /ui/ status = %d, want 200", resp.StatusCode)
	}
}

func TestLegacySingleTokenBackwardCompatibility(t *testing.T) {
	_, _, ts := newTestServer(t, Options{
		InstanceID:   "inst-legacy",
		MaxClaimed:   3,
		Token:        "only-legacy-token",
		GateReleaser: tokenTestGateReleaser{},
		AgentChecker: tokenTestAgentChecker{},
	})

	// Missing token -> 401
	resp := doJSON(t, http.MethodGet, ts.URL+"/status", "", nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("GET /status without token = %d, want 401", resp.StatusCode)
	}

	// GET /meta returns all scopes for legacy token.
	resp = doJSON(t, http.MethodGet, ts.URL+"/meta", "only-legacy-token", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /meta status = %d, want 200", resp.StatusCode)
	}
	meta := decodeResp[MetaResponse](t, resp)
	if !reflect.DeepEqual(meta.Scopes, config.AllAPIScopes) {
		t.Fatalf("GET /meta scopes = %v, want %v", meta.Scopes, config.AllAPIScopes)
	}

	// POST /jobs (register) -> 201 and created_by = "legacy"
	resp = doJSON(t, http.MethodPost, ts.URL+"/jobs", "only-legacy-token", CreateJobRequest{
		RequestID: "req-legacy-1",
		Title:     "Legacy Created Job",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /jobs status = %d, want 201", resp.StatusCode)
	}
	created := decodeResp[CreateJobResponse](t, resp)
	if created.Job.CreatedBy != config.LegacyTokenName {
		t.Fatalf("created.Job.CreatedBy = %q, want %q", created.Job.CreatedBy, config.LegacyTokenName)
	}

	// POST /jobs/{id}/claim (worker) -> 200
	resp = doJSON(t, http.MethodPost, fmt.Sprintf("%s/jobs/%d/claim", ts.URL, created.Job.ID), "only-legacy-token", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /jobs/{id}/claim status = %d, want 200", resp.StatusCode)
	}
	claimResp := decodeResp[ClaimResponse](t, resp)
	if !claimResp.Claimed {
		t.Fatalf("expected job to be claimed")
	}

	// POST /jobs/{id}/reset (operate) -> 200
	resp = doJSON(t, http.MethodPost, fmt.Sprintf("%s/jobs/%d/reset", ts.URL, created.Job.ID), "only-legacy-token", nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /jobs/{id}/reset status = %d, want 200", resp.StatusCode)
	}

	// POST /agents/check (operate) -> 200
	resp = doJSON(t, http.MethodPost, ts.URL+"/agents/check", "only-legacy-token", nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /agents/check status = %d, want 200", resp.StatusCode)
	}
}

func TestCreatedByAuditAndMetaScopes(t *testing.T) {
	_, _, ts := newTestServer(t, Options{
		InstanceID: "inst-audit",
		Tokens: []config.APITokenConfig{
			{Name: "coordinator", Token: "tok-coord", Scopes: []string{"read", "register", "operate", "worker", "agents"}},
			{Name: "watch", Token: "tok-watch", Scopes: []string{"read"}},
			{Name: "mcp-register", Token: "tok-mcp", Scopes: []string{"read", "register"}},
		},
	})

	// 1. /meta returns the exact scopes of each authenticated token.
	for _, tc := range []struct {
		name   string
		token  string
		scopes []string
	}{
		{name: "watch", token: "tok-watch", scopes: []string{"read"}},
		{name: "mcp-register", token: "tok-mcp", scopes: []string{"read", "register"}},
		{name: "coordinator", token: "tok-coord", scopes: []string{"read", "register", "operate", "worker", "agents"}},
	} {
		resp := doJSON(t, http.MethodGet, ts.URL+"/meta", tc.token, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /meta (%s) status = %d, want 200", tc.name, resp.StatusCode)
		}
		meta := decodeResp[MetaResponse](t, resp)
		if !reflect.DeepEqual(meta.Scopes, tc.scopes) {
			t.Errorf("GET /meta (%s) scopes = %v, want %v", tc.name, meta.Scopes, tc.scopes)
		}
	}

	// 2. POST /jobs records created_by = token name (and ignores any client-supplied created_by field).
	rawReq := []byte(`{"request_id":"req-audit-1","title":"Audit Test Job","created_by":"spoofed-actor"}`)
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/jobs", bytes.NewReader(rawReq))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer tok-mcp")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /jobs: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /jobs status = %d, want 201", resp.StatusCode)
	}
	created := decodeResp[CreateJobResponse](t, resp)
	if created.Job.CreatedBy != "mcp-register" {
		t.Fatalf("POST /jobs created_by = %q, want %q", created.Job.CreatedBy, "mcp-register")
	}

	// 3. GET /jobs/{id} returns created_by.
	resp = doJSON(t, http.MethodGet, fmt.Sprintf("%s/jobs/%d", ts.URL, created.Job.ID), "tok-watch", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /jobs/{id} status = %d, want 200", resp.StatusCode)
	}
	got := decodeResp[JobDTO](t, resp)
	if got.CreatedBy != "mcp-register" {
		t.Errorf("GET /jobs/{id} created_by = %q, want %q", got.CreatedBy, "mcp-register")
	}

	// 4. GET /jobs?status=queued returns created_by.
	resp = doJSON(t, http.MethodGet, ts.URL+"/jobs?status=queued", "tok-watch", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /jobs status = %d, want 200", resp.StatusCode)
	}
	list := decodeResp[ListJobsResponse](t, resp)
	if len(list.Jobs) != 1 || list.Jobs[0].CreatedBy != "mcp-register" {
		t.Errorf("GET /jobs created_by = %+v, want mcp-register", list.Jobs)
	}
}

func TestCreatedByPreservedWhenHeldByGate(t *testing.T) {
	_, _, ts := newTestServer(t, Options{
		InstanceID: "inst-gate-audit",
		GateMode:   config.GateModeEnforce,
		Pools: map[string]config.PoolConfig{
			"claude": {Provider: "claude"},
		},
		Tokens: []config.APITokenConfig{
			{Name: "mcp-register", Token: "tok-mcp", Scopes: []string{"read", "register"}},
		},
		SnapshotLoader: func() (*quota.Snapshot, error) {
			return &quota.Snapshot{
				Pools: map[string]quota.PoolSnapshot{
					"claude": {WeeklyRemainingPercent: 0},
				},
			}, nil
		},
	})

	resp := doJSON(t, http.MethodPost, ts.URL+"/jobs", "tok-mcp", CreateJobRequest{
		RequestID: "req-held-audit-1",
		Title:     "Held Audit Job",
		Pool:      "claude",
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST /jobs (held) status = %d, want 202", resp.StatusCode)
	}
	heldResp := decodeResp[HoldJobResponse](t, resp)
	if heldResp.Held == nil || heldResp.Held.Payload.CreatedBy != "mcp-register" {
		t.Fatalf("held payload created_by = %+v, want mcp-register", heldResp.Held)
	}
}

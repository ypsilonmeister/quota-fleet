package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/config"
)

// docs/21: effort is stored, returned, validated, and never silently dropped.
func TestCreateJobEffort(t *testing.T) {
	now := time.Now()
	url := selectorGateServer(t, config.SelectorModeOff, config.GateModeOff, healthySnap(now), now, nil)

	status, job, body := postJob(t, url, CreateJobRequest{RequestID: "e-codex", Title: "t", Agent: "codex", Effort: "xhigh"})
	if status != http.StatusCreated || job.Effort != "xhigh" {
		t.Fatalf("codex+xhigh: status=%d job=%+v body=%s", status, job, body)
	}
	if got := getJob(t, url, job.ID); got.Effort != "xhigh" {
		t.Fatalf("GET /jobs/{id} effort = %q, want xhigh", got.Effort)
	}

	status, job, body = postJob(t, url, CreateJobRequest{RequestID: "e-claude", Title: "t", Agent: "claude", Effort: "high"})
	if status != http.StatusCreated || job.Effort != "high" {
		t.Fatalf("claude+high: status=%d job=%+v body=%s", status, job, body)
	}

	// Unset effort: field omitted from the response (legacy shape).
	resp := doJSON(t, http.MethodPost, url+"/jobs", "", CreateJobRequest{RequestID: "e-none", Title: "t", Agent: "codex"})
	var raw struct {
		Job map[string]any `json:"job"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	resp.Body.Close()
	if _, ok := raw.Job["effort"]; ok || resp.StatusCode != http.StatusCreated {
		t.Fatalf("unset effort: status=%d job=%v, want 201 without effort key", resp.StatusCode, raw.Job)
	}

	for _, c := range []struct {
		name string
		req  CreateJobRequest
		want string
	}{
		{"invalid value", CreateJobRequest{RequestID: "bad-1", Title: "t", Agent: "codex", Effort: "max"}, "not supported"},
		{"agy", CreateJobRequest{RequestID: "bad-2", Title: "t", Agent: "agy", Model: "gemini-4-neon-medium", Effort: "high"}, "agy"},
		{"no agent", CreateJobRequest{RequestID: "bad-3", Title: "t", Effort: "high"}, "requires an agent"},
		{"other agent", CreateJobRequest{RequestID: "bad-4", Title: "t", Agent: "custom", Effort: "low"}, "not supported for agent"},
	} {
		status, _, body := postJob(t, url, c.req)
		if status != http.StatusBadRequest || !strings.Contains(body, c.want) {
			t.Errorf("%s: status=%d body=%s, want 400 containing %q", c.name, status, body, c.want)
		}
	}
}

// The Gate keeps effort in the held payload, and a rejected effort is 400 before any hold.
func TestCreateJobEffortGateHold(t *testing.T) {
	now := time.Now()
	url := selectorGateServer(t, config.SelectorModeOff, config.GateModeEnforce, healthySnap(now, "codex", "agy-gemini"), now, nil)

	resp := doJSON(t, http.MethodPost, url+"/jobs", "", CreateJobRequest{RequestID: "held-e", Title: "t", Agent: "codex", Pool: "codex", Effort: "medium"})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	hold := decodeResp[HoldJobResponse](t, resp)
	if hold.Held == nil || hold.Held.Payload.Effort != "medium" {
		t.Fatalf("held payload = %+v, want effort medium", hold.Held)
	}

	status, _, body := postJob(t, url, CreateJobRequest{RequestID: "held-agy", Title: "t", Agent: "agy", Pool: "agy-gemini", Effort: "high"})
	if status != http.StatusBadRequest {
		t.Fatalf("agy+effort under enforce: status=%d body=%s, want 400", status, body)
	}
}

// Selector apply fills the agent; effort is checked against the filled agent and kept as sent.
func TestCreateJobEffortWithSelector(t *testing.T) {
	now := time.Now()
	url := selectorGateServer(t, config.SelectorModeRecord, config.GateModeOff, healthySnap(now), now, nil)
	status, job, body := postJob(t, url, CreateJobRequest{RequestID: "sel-e", Title: "t", Role: "implement", Weight: "standard", Effort: "high"})
	if status != http.StatusCreated {
		t.Fatalf("status=%d body=%s", status, body)
	}
	if job.Agent != "codex" || job.Effort != "high" {
		t.Fatalf("job agent=%q effort=%q, want codex/high (record-mode default agent)", job.Agent, job.Effort)
	}
}

// Review item 1: registration rejects an effort the job's host could not pass to the agent CLI
// (the host launches the agent through a different executable), and accepts the agent's own CLI.
func TestCreateJobEffortHostLaunchCommand(t *testing.T) {
	cfg := &config.Config{
		Hosts: map[string]config.HostConfig{
			"plain":   {OS: "linux"},
			"pathed":  {OS: "linux", Agents: map[string]config.HostAgentOverride{"claude": {Command: "/home/u/.local/bin/claude"}}},
			"wrapped": {OS: "linux", Agents: map[string]config.HostAgentOverride{"claude": {Command: "/opt/bin/run-agent.sh"}}},
		},
	}
	_, _, ts := newTestServer(t, Options{InstanceID: "effort-host", Config: cfg, Hosts: []string{"plain", "pathed", "wrapped"}, Agents: []string{"claude", "codex"}})

	for _, c := range []struct {
		id, host string
		effort   string
		want     int
	}{
		{"h-plain", "plain", "high", http.StatusCreated},
		{"h-pathed", "pathed", "high", http.StatusCreated},
		{"h-wrapped", "wrapped", "high", http.StatusBadRequest},
		{"h-wrapped-none", "wrapped", "", http.StatusCreated},
	} {
		status, _, body := postJob(t, ts.URL, CreateJobRequest{RequestID: c.id, Title: "t", Agent: "claude", Host: c.host, Effort: c.effort})
		if status != c.want {
			t.Errorf("%s: status=%d body=%s, want %d", c.id, status, body, c.want)
		}
		if c.want == http.StatusBadRequest && !strings.Contains(body, "not the claude CLI") {
			t.Errorf("%s: body=%s, want reason", c.id, body)
		}
	}
}

// Review item 4: where effort appears in each response shape (docs/21 §2).
func TestCreateJobEffortResponsePaths(t *testing.T) {
	now := time.Now()
	url := selectorGateServer(t, config.SelectorModeOff, config.GateModeOff, healthySnap(now), now, nil)
	resp := doJSON(t, http.MethodPost, url+"/jobs", "", CreateJobRequest{RequestID: "paths", Title: "t", Agent: "codex", Effort: "low"})
	var created struct {
		Job map[string]any `json:"job"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("decode POST: %v", err)
	}
	resp.Body.Close()
	if created.Job["effort"] != "low" {
		t.Fatalf("POST job.effort = %v", created.Job["effort"])
	}
	id := created.Job["id"]

	resp = doJSON(t, http.MethodGet, url+"/jobs?status=queued", "", nil)
	var list struct {
		Jobs []map[string]any `json:"jobs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	resp.Body.Close()
	if len(list.Jobs) != 1 || list.Jobs[0]["effort"] != "low" {
		t.Fatalf("GET /jobs jobs[].effort = %v", list.Jobs)
	}

	resp = doJSON(t, http.MethodGet, fmt.Sprintf("%s/jobs/%v", url, id), "", nil)
	var detail map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&detail); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	resp.Body.Close()
	if detail["effort"] != "low" {
		t.Fatalf("GET /jobs/{id} top-level effort = %v", detail["effort"])
	}
}

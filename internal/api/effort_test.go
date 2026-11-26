package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/adapter"
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

// fakeHostAccess runs probe commands locally (the real probe command through sh) for the hosts in
// up and fails to reach any other host.
type fakeHostAccess struct {
	up map[string]bool
	// dir is the directory host commands run in ("" = the test's working directory), like the
	// SSH login directory of a real host, which is not the job's workdir.
	dir string
}

func (f fakeHostAccess) FileAccessForHost(host string) (adapter.FileAccess, error) {
	if !f.up[host] {
		return nil, errors.New("ssh: connect to host " + host + ": connection refused")
	}
	return adapter.NewLocalFileAccess(f.dir), nil
}

// writeFakeCLI writes an executable that prints help to stdout and appends a line to calls on
// each run.
func writeFakeCLI(t *testing.T, path, help, calls string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\necho run >> '" + calls + "'\ncat <<'HELP'\n" + help + "\nHELP\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

// Review round 2, item 1: a CLI with the agent's own name that does not take the effort option
// (an older release, e.g. /opt/legacy/bin/claude) must be rejected with 400 at registration. The
// job's host is asked for the CLI's help, which has to list --effort (claude) or -c/--config
// (codex).
func TestCreateJobEffortHostCLIProbe(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake CLIs are shell scripts")
	}
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	legacyClaude := filepath.Join(dir, "opt/legacy/bin/claude")
	currentClaude := filepath.Join(dir, "opt/current/bin/claude")
	legacyCodex := filepath.Join(dir, "opt/legacy/bin/codex")
	currentCodex := filepath.Join(dir, "opt/current/bin/codex")
	writeFakeCLI(t, legacyClaude, "Usage: claude [options]\n  -p, --print  Print response\n  --model <model>  Model", calls)
	writeFakeCLI(t, currentClaude, "Usage: claude [options]\n  -p, --print  Print response\n  --effort <level>  Effort level for the current session (low, medium, high, xhigh, max)", calls)
	writeFakeCLI(t, legacyCodex, "Usage: codex [OPTIONS]\n  -m, --model <MODEL>", calls)
	writeFakeCLI(t, currentCodex, "Usage: codex [OPTIONS]\n  -c, --config <key=value>\n          Override a configuration value", calls)

	host := func(agent, command string) config.HostConfig {
		return config.HostConfig{OS: "linux", Agents: map[string]config.HostAgentOverride{agent: {Command: command}}}
	}
	cfg := &config.Config{Hosts: map[string]config.HostConfig{
		"legacy":        host("claude", legacyClaude),
		"current":       host("claude", currentClaude),
		"missing":       host("claude", filepath.Join(dir, "nowhere/bin/claude")),
		"codex-legacy":  host("codex", legacyCodex),
		"codex-current": host("codex", currentCodex),
		"down":          host("claude", currentClaude),
		"wrapped":       host("claude", filepath.Join(dir, "opt/bin/run-agent.sh")),
	}}
	up := map[string]bool{"legacy": true, "current": true, "missing": true, "codex-legacy": true, "codex-current": true, "wrapped": true}
	names := make([]string, 0, len(cfg.Hosts))
	for h := range cfg.Hosts {
		names = append(names, h)
	}
	_, _, ts := newTestServer(t, Options{InstanceID: "effort-probe", Config: cfg, Hosts: names, Agents: []string{"claude", "codex"}, HostAccess: fakeHostAccess{up: up}})

	runs := func() int {
		b, _ := os.ReadFile(calls)
		return strings.Count(string(b), "run")
	}
	for _, c := range []struct {
		id, agent, host, effort string
		want                    int
		reason                  string
		probes                  int // fake CLI runs caused by this request
	}{
		{"p-legacy", "claude", "legacy", "high", http.StatusBadRequest, "does not list --effort", 1},
		{"p-legacy-again", "claude", "legacy", "low", http.StatusBadRequest, "does not list --effort", 0}, // settled result reused
		{"p-legacy-none", "claude", "legacy", "", http.StatusCreated, "", 0},
		{"p-current", "claude", "current", "high", http.StatusCreated, "", 1},
		{"p-current-again", "claude", "current", "medium", http.StatusCreated, "", 0},
		{"p-missing", "claude", "missing", "high", http.StatusBadRequest, "was not found", 0},
		{"p-codex-legacy", "codex", "codex-legacy", "low", http.StatusBadRequest, "does not list -c/--config", 1},
		{"p-codex-current", "codex", "codex-current", "low", http.StatusCreated, "", 1},
		{"p-down", "claude", "down", "high", http.StatusServiceUnavailable, "cannot confirm effort support", 0},
		{"p-wrapped", "claude", "wrapped", "high", http.StatusBadRequest, "not the claude CLI", 0},
	} {
		before := runs()
		status, _, body := postJob(t, ts.URL, CreateJobRequest{RequestID: c.id, Title: "t", Agent: c.agent, Host: c.host, Effort: c.effort})
		if status != c.want {
			t.Errorf("%s: status=%d body=%s, want %d", c.id, status, body, c.want)
		}
		if c.reason != "" && !strings.Contains(body, c.reason) {
			t.Errorf("%s: body=%s, want %q", c.id, body, c.reason)
		}
		if got := runs() - before; got != c.probes {
			t.Errorf("%s: CLI ran %d times, want %d", c.id, got, c.probes)
		}
	}
}

// Review round 3, item 1: a relative command path (./claude, bin/claude) is resolved by the
// launcher from the job's workdir (run.sh does `cd <workdir>` first), while the registration probe
// runs in the host's login directory, so the probe could judge a different file. Such a command
// is rejected at registration when the job has an effort, before any probe runs, in both
// directions the reviewer reproduced:
//   - the probe's directory has a CLI with --effort but the workdir's ./claude does not (was 201);
//   - the probe's directory has no ./claude but the workdir's has --effort (was 400 "was not
//     found", a wrong reason).
func TestCreateJobEffortRelativeCommandRejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake CLIs are shell scripts")
	}
	const supported = "Usage: claude [options]\n  --effort <level>  Effort level"
	const legacy = "Usage: claude [options]\n  --model <model>  Model"
	for _, c := range []struct {
		name                    string
		probeDirCLI, workdirCLI string // help text of ./claude in each directory ("" = absent)
	}{
		{"probe-dir-supported-workdir-legacy", supported, legacy},
		{"probe-dir-missing-workdir-supported", "", supported},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			calls := filepath.Join(dir, "calls")
			probeDir := filepath.Join(dir, "home")
			workdir := filepath.Join(dir, "work")
			for _, d := range []string{probeDir, workdir} {
				if err := os.MkdirAll(d, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if c.probeDirCLI != "" {
				writeFakeCLI(t, filepath.Join(probeDir, "claude"), c.probeDirCLI, calls)
			}
			writeFakeCLI(t, filepath.Join(workdir, "claude"), c.workdirCLI, calls)

			cfg := &config.Config{Hosts: map[string]config.HostConfig{
				"rel":    {OS: "linux", Agents: map[string]config.HostAgentOverride{"claude": {Command: "./claude"}}},
				"relsub": {OS: "linux", Agents: map[string]config.HostAgentOverride{"claude": {Command: "bin/claude"}}},
			}}
			_, _, ts := newTestServer(t, Options{InstanceID: "effort-rel", Config: cfg, Hosts: []string{"rel", "relsub"}, Agents: []string{"claude"},
				HostAccess: fakeHostAccess{up: map[string]bool{"rel": true, "relsub": true}, dir: probeDir}})

			for _, host := range []string{"rel", "relsub"} {
				status, _, body := postJob(t, ts.URL, CreateJobRequest{RequestID: c.name + host, Title: "t", Agent: "claude", Host: host, Workdir: workdir, Effort: "high"})
				if status != http.StatusBadRequest || !strings.Contains(body, "relative path") {
					t.Errorf("%s: status=%d body=%s, want 400 naming the relative path", host, status, body)
				}
			}
			if b, _ := os.ReadFile(calls); len(b) != 0 {
				t.Errorf("a CLI was probed for a relative command: %q", b)
			}
			// Without an effort nothing is passed to the CLI, so the command is accepted as before.
			if status, _, body := postJob(t, ts.URL, CreateJobRequest{RequestID: c.name + "-none", Title: "t", Agent: "claude", Host: "rel", Workdir: workdir}); status != http.StatusCreated {
				t.Errorf("no effort: status=%d body=%s, want 201", status, body)
			}
		})
	}
}

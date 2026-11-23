package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

func TestCLIConfigCheckAndStatus(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	cfgPath := filepath.Join(dir, "fleet.yaml")

	cfgYAML := `
instance_id: "cli-test-inst"
state_dir: "` + filepath.ToSlash(stateDir) + `"
limits:
  max_claimed: 2
  attempts_cap: 3
hosts:
  local:
    transport: local
    os: linux
agents:
  claude:
    launch: "claude -p"
pools:
  claude:
    provider: claude-usage
`
	if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// config check succeeds.
	var out, errBuf bytes.Buffer
	code := run([]string{"config", "check", "--config", cfgPath}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("config check exit=%d stderr=%q", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "config OK") || !strings.Contains(out.String(), "cli-test-inst") {
		t.Errorf("unexpected config check output: %q", out.String())
	}

	// Seed store with jobs and verify status output.
	dbPath := filepath.Join(stateDir, "fleet.db")
	st, err := store.Open(context.Background(), dbPath, store.Options{InstanceID: "cli-test-inst"})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if err := st.InitInstanceID(context.Background(), "cli-test-inst"); err != nil {
		t.Fatalf("InitInstanceID: %v", err)
	}
	if _, _, err := st.CreateJob(context.Background(), store.CreateJobParams{
		RequestID: "cli-1",
		Title:     "Queued job",
	}); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if _, _, err := st.CreateJob(context.Background(), store.CreateJobParams{
		RequestID: "cli-2",
		Title:     "Draft job",
		Draft:     true,
	}); err != nil {
		t.Fatalf("CreateJob draft: %v", err)
	}
	_ = st.Close()

	out.Reset()
	errBuf.Reset()
	code = run([]string{"status", "--config", cfgPath}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("status exit=%d stderr=%q", code, errBuf.String())
	}
	got := out.String()
	for _, want := range []string{"draft      1", "queued     1", "total      2"} {
		if !strings.Contains(got, want) {
			t.Errorf("status output missing %q:\n%s", want, got)
		}
	}

	// Invalid config fails with non-zero exit code.
	badCfgPath := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(badCfgPath, []byte("instance_id: \"\"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile bad: %v", err)
	}
	out.Reset()
	errBuf.Reset()
	if code := run([]string{"config", "check", "--config", badCfgPath}, &out, &errBuf); code == 0 {
		t.Errorf("expected non-zero exit for invalid config")
	}
}

func TestCLIJobCreateListGetCancelRequeue(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "cli-jobs.db")

	var out, errBuf bytes.Buffer

	// 1. job create
	code := run([]string{
		"job", "create",
		"--db", dbPath,
		"--title", "Phase 1-2 CLI Test",
		"--agent", "claude",
		"--host", "ubuntu",
		"--workdir", "/work/repo",
		"--instructions", "run checks",
		"--acceptance", "tests pass",
		"--request-id", "cli-req-1",
		"--target-ref", "impl/phase1-core",
		"--model", "claude-opus",
		"--pool", "claude",
	}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("job create exit=%d stderr=%q", code, errBuf.String())
	}
	createOut := out.String()
	for _, want := range []string{"#1", "queued", "Phase 1-2 CLI Test"} {
		if !strings.Contains(createOut, want) {
			t.Errorf("job create output missing %q:\n%s", want, createOut)
		}
	}

	// 2. job list --status queued
	out.Reset()
	errBuf.Reset()
	code = run([]string{"job", "list", "--db", dbPath, "--status", "queued", "--limit", "10"}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("job list exit=%d stderr=%q", code, errBuf.String())
	}
	listOut := out.String()
	for _, want := range []string{"#1", "queued", "Phase 1-2 CLI Test"} {
		if !strings.Contains(listOut, want) {
			t.Errorf("job list output missing %q:\n%s", want, listOut)
		}
	}

	// 3. job get 1
	out.Reset()
	errBuf.Reset()
	code = run([]string{"job", "get", "1", "--db", dbPath}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("job get exit=%d stderr=%q", code, errBuf.String())
	}
	getOut := out.String()
	for _, want := range []string{"id:          1", "status:      queued", "title:       Phase 1-2 CLI Test", "agent:       claude", "pool:        claude"} {
		if !strings.Contains(getOut, want) {
			t.Errorf("job get output missing %q:\n%s", want, getOut)
		}
	}

	// 4. job cancel 1 -> job requeue 1
	out.Reset()
	errBuf.Reset()
	code = run([]string{"job", "cancel", "1", "--db", dbPath, "--reason", "operator pause"}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("job cancel exit=%d stderr=%q", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "cancelled") {
		t.Errorf("job cancel output = %q, want cancelled", out.String())
	}

	out.Reset()
	errBuf.Reset()
	code = run([]string{"job", "requeue", "--db", dbPath, "1"}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("job requeue exit=%d stderr=%q", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "queued") {
		t.Errorf("job requeue output = %q, want queued", out.String())
	}
}

func TestServeRunsAPIAndRuntimeWithGracefulShutdown(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	cfgPath := filepath.Join(dir, "fleet.yaml")

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	cfgYAML := fmt.Sprintf(`
instance_id: "serve-e2e-inst"
state_dir: "%s"
api:
  listen: "%s"
runtime:
  adapter: "mock"
  dispatch_interval: "15ms"
  shepherd_interval: "15ms"
  reconcile_interval: "30ms"
  stale_claimed_after: "5s"
limits:
  max_claimed: 2
  attempts_cap: 3
evaluator:
  mode: "always-pass"
`, filepath.ToSlash(stateDir), addr)
	if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	var out, errBuf bytes.Buffer
	doneCh := make(chan int, 1)
	go func() {
		doneCh <- runServeContext(ctx, []string{"--config", cfgPath}, &out, &errBuf)
	}()

	baseURL := "http://" + addr
	// Wait for HTTP API to come up.
	deadline := time.Now().Add(3 * time.Second)
	for {
		resp, err := http.Get(baseURL + "/status")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("serve did not become ready on %s; stderr=%s", addr, errBuf.String())
		}
		time.Sleep(15 * time.Millisecond)
	}

	// Create a job via HTTP API and verify the resident runtime completes and delivers it.
	reqBody := `{"request_id":"serve-req-1","title":"Serve E2E Job","acceptance_criteria":"build ok"}`
	resp, err := http.Post(baseURL+"/jobs", "application/json", strings.NewReader(reqBody))
	if err != nil {
		cancel()
		t.Fatalf("POST /jobs: %v", err)
	}
	var createResp struct {
		Job struct {
			ID int64 `json:"id"`
		} `json:"job"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&createResp); err != nil {
		_ = resp.Body.Close()
		cancel()
		t.Fatalf("decode create response: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated || createResp.Job.ID == 0 {
		cancel()
		t.Fatalf("POST /jobs status=%d id=%d", resp.StatusCode, createResp.Job.ID)
	}

	deadline = time.Now().Add(3 * time.Second)
	for {
		gresp, err := http.Get(fmt.Sprintf("%s/jobs/%d", baseURL, createResp.Job.ID))
		if err == nil {
			var jobResp struct {
				Status      string  `json:"status"`
				DeliveredAt *string `json:"delivered_at"`
			}
			_ = json.NewDecoder(gresp.Body).Decode(&jobResp)
			_ = gresp.Body.Close()
			if jobResp.Status == "done" && jobResp.DeliveredAt != nil {
				break
			}
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("job #%d did not complete and deliver in time", createResp.Job.ID)
		}
		time.Sleep(15 * time.Millisecond)
	}

	// Trigger graceful shutdown and ensure serve exits cleanly with code 0.
	cancel()
	select {
	case code := <-doneCh:
		if code != 0 {
			t.Fatalf("serve exit code = %d, want 0; stderr=%s", code, errBuf.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("serve did not shut down gracefully within timeout")
	}
}

func TestServeWiringWithSSHAdapter(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	cfgPath := filepath.Join(dir, "fleet.yaml")

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	cfgYAML := fmt.Sprintf(`
instance_id: "serve-ssh-wiring-inst"
state_dir: "%s"
api:
  listen: "%s"
runtime:
  adapter: "ssh"
  dispatch_interval: "25ms"
  shepherd_interval: "25ms"
  reconcile_interval: "50ms"
limits:
  max_claimed: 1
  attempts_cap: 3
hosts:
  ubuntu:
    transport: ssh
    ssh: worker@ubuntu.example.ts.net
    os: linux
    key_file: ~/.ssh/id_ed25519
    workdir_root: ~/agent-work
agents:
  claude:
    launch: "claude -p --model {model}"
`, filepath.ToSlash(stateDir), addr)
	if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	var out, errBuf bytes.Buffer
	doneCh := make(chan int, 1)
	go func() {
		doneCh <- runServeContext(ctx, []string{"--config", cfgPath}, &out, &errBuf)
	}()

	client := &http.Client{
		Transport: &http.Transport{DisableKeepAlives: true},
	}
	baseURL := "http://" + addr
	deadline := time.Now().Add(3 * time.Second)
	for {
		resp, err := client.Get(baseURL + "/status")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("serve(ssh) did not become ready on %s; stderr=%s", addr, errBuf.String())
		}
		time.Sleep(15 * time.Millisecond)
	}

	cancel()
	select {
	case code := <-doneCh:
		if code != 0 {
			t.Fatalf("serve(ssh) exit code = %d, want 0; stderr=%s", code, errBuf.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("serve(ssh) did not shut down gracefully within timeout")
	}
	if !strings.Contains(out.String(), "adapter=ssh") {
		t.Errorf("expected stdout to log adapter=ssh, got %q", out.String())
	}
	if !strings.Contains(out.String(), "evaluator=typed+decision") {
		t.Errorf("expected stdout to log default evaluator=typed+decision, got %q", out.String())
	}
}

func TestServeDefaultTypedDecisionEvaluatorWithTypedCriteria(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	workdir := filepath.Join(dir, "work")
	cfgPath := filepath.Join(dir, "fleet.yaml")

	if err := os.MkdirAll(workdir, 0o755); err != nil {
		t.Fatalf("MkdirAll workdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workdir, "artifact.txt"), []byte("ok-token\n"), 0o644); err != nil {
		t.Fatalf("WriteFile artifact.txt: %v", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	// No evaluator section -> defaults to typed+decision.
	// Use a dedicated unset env var to verify typed-only criteria do not require an API key.
	cfgYAML := fmt.Sprintf(`
instance_id: "serve-typed-inst"
state_dir: "%s"
api:
  listen: "%s"
runtime:
  adapter: "mock"
  dispatch_interval: "15ms"
  shepherd_interval: "15ms"
  reconcile_interval: "30ms"
  stale_claimed_after: "5s"
limits:
  max_claimed: 1
  attempts_cap: 3
evaluator:
  api_key_env: "QUOTA_FLEET_UNSET_TEST_KEY"
`, filepath.ToSlash(stateDir), addr)
	if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	var out, errBuf bytes.Buffer
	doneCh := make(chan int, 1)
	go func() {
		doneCh <- runServeContext(ctx, []string{"--config", cfgPath}, &out, &errBuf)
	}()

	baseURL := "http://" + addr
	deadline := time.Now().Add(3 * time.Second)
	for {
		resp, err := http.Get(baseURL + "/status")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("serve did not become ready on %s; stderr=%s", addr, errBuf.String())
		}
		time.Sleep(15 * time.Millisecond)
	}

	payload, _ := json.Marshal(map[string]any{
		"request_id":          "serve-typed-1",
		"title":               "Typed Criteria Job",
		"workdir":             workdir,
		"acceptance_criteria": "file_exists: artifact.txt\ncontains: artifact.txt :: ok-token",
	})
	resp, err := http.Post(baseURL+"/jobs", "application/json", bytes.NewReader(payload))
	if err != nil {
		cancel()
		t.Fatalf("POST /jobs: %v", err)
	}
	var createResp struct {
		Job struct {
			ID int64 `json:"id"`
		} `json:"job"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&createResp)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated || createResp.Job.ID == 0 {
		cancel()
		t.Fatalf("POST /jobs status=%d id=%d", resp.StatusCode, createResp.Job.ID)
	}

	var resultText string
	deadline = time.Now().Add(3 * time.Second)
	for {
		gresp, err := http.Get(fmt.Sprintf("%s/jobs/%d", baseURL, createResp.Job.ID))
		if err == nil {
			var jobResp struct {
				Status      string  `json:"status"`
				ResultText  string  `json:"result_text"`
				DeliveredAt *string `json:"delivered_at"`
			}
			_ = json.NewDecoder(gresp.Body).Decode(&jobResp)
			_ = gresp.Body.Close()
			if jobResp.Status == "done" && jobResp.DeliveredAt != nil {
				resultText = jobResp.ResultText
				break
			}
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("typed job #%d did not complete in time; stderr=%s", createResp.Job.ID, errBuf.String())
		}
		time.Sleep(15 * time.Millisecond)
	}

	if !strings.Contains(resultText, "typed file_exists") || !strings.Contains(resultText, "typed contains") {
		t.Errorf("unexpected ResultText:\n%s", resultText)
	}

	cancel()
	select {
	case code := <-doneCh:
		if code != 0 {
			t.Fatalf("serve exit code = %d, want 0; stderr=%s", code, errBuf.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("serve did not shut down gracefully within timeout")
	}
}

func TestCLIGateCheckAndDecisions(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	cfgPath := filepath.Join(dir, "fleet.yaml")
	usagePath := filepath.Join(dir, "usage.json")

	now := time.Now().UTC().Truncate(time.Second)
	usageJSON := fmt.Sprintf(`{
		"updatedAt": "%s",
		"services": {
			"claude": {
				"remainingPercent": 80,
				"resetAt": "%s",
				"subText": "5h 90%%",
				"subResetAt": "%s"
			},
			"codex": {
				"remainingPercent": 40,
				"resetAt": "%s",
				"subText": "5h 70%%",
				"subResetAt": "%s"
			}
		}
	}`,
		now.Add(-5*time.Minute).Format(time.RFC3339),
		now.Add(72*time.Hour).Format(time.RFC3339),
		now.Add(3*time.Hour).Format(time.RFC3339),
		now.Add(24*time.Hour).Format(time.RFC3339),
		now.Add(2*time.Hour).Format(time.RFC3339),
	)
	if err := os.WriteFile(usagePath, []byte(usageJSON), 0o600); err != nil {
		t.Fatalf("WriteFile usage.json: %v", err)
	}

	cfgYAML := fmt.Sprintf(`
instance_id: "cli-gate-inst"
state_dir: "%s"
limits:
  max_claimed: 2
  attempts_cap: 3
pools:
  claude:
    provider: claude-usage
  codex:
    provider: codex-usage
  agy-argon:
    unmeasured: true
quota:
  usage_file: "%s"
  stale_after: 45m
  gate_mode: dry-run
`, filepath.ToSlash(stateDir), filepath.ToSlash(usagePath))
	if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0o600); err != nil {
		t.Fatalf("WriteFile fleet.yaml: %v", err)
	}

	// 1. gate check with alt-pools selects codex (soonest weekly reset).
	var out, errBuf bytes.Buffer
	code := run([]string{"gate", "check", "--config", cfgPath, "--pool", "claude", "--alt-pools", "codex"}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("gate check exit=%d stderr=%q", code, errBuf.String())
	}
	checkOut := out.String()
	for _, want := range []string{"decision:      would_pass", "selected_pool: codex"} {
		if !strings.Contains(checkOut, want) {
			t.Errorf("gate check output missing %q:\n%s", want, checkOut)
		}
	}

	// 2. gate check for unmeasured pool reports bypass.
	out.Reset()
	errBuf.Reset()
	code = run([]string{"gate", "check", "--config", cfgPath, "--pool", "agy-argon"}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("gate check unmeasured exit=%d stderr=%q", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "bypassed:      true") || !strings.Contains(out.String(), "reason:        unmeasured") {
		t.Errorf("unexpected unmeasured gate check output:\n%s", out.String())
	}

	// 3. Seed two gate decisions and verify `gate decisions --limit` shows newest first.
	dbPath := filepath.Join(stateDir, "fleet.db")
	st, err := store.Open(context.Background(), dbPath, store.Options{InstanceID: "cli-gate-inst"})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if err := st.InitInstanceID(context.Background(), "cli-gate-inst"); err != nil {
		t.Fatalf("InitInstanceID: %v", err)
	}
	j1, _, err := st.CreateJob(context.Background(), store.CreateJobParams{RequestID: "cg-1", Title: "Job 1"})
	if err != nil {
		t.Fatalf("CreateJob 1: %v", err)
	}
	j2, _, err := st.CreateJob(context.Background(), store.CreateJobParams{RequestID: "cg-2", Title: "Job 2"})
	if err != nil {
		t.Fatalf("CreateJob 2: %v", err)
	}
	measured := now.Add(-5 * time.Minute)
	if _, err := st.RecordGateDecision(context.Background(), store.GateDecision{
		JobID:         j1.ID,
		RequestedPool: "claude",
		Decision:      "would_pass",
		SelectedPool:  "claude",
		MeasuredAt:    &measured,
		DecidedAt:     now.Add(-2 * time.Minute),
	}); err != nil {
		t.Fatalf("RecordGateDecision 1: %v", err)
	}
	if _, err := st.RecordGateDecision(context.Background(), store.GateDecision{
		JobID:         j2.ID,
		RequestedPool: "codex",
		Decision:      "would_hold",
		BlockedBy:     "5h",
		MeasuredAt:    &measured,
		DecidedAt:     now.Add(-1 * time.Minute),
	}); err != nil {
		t.Fatalf("RecordGateDecision 2: %v", err)
	}
	_ = st.Close()

	out.Reset()
	errBuf.Reset()
	code = run([]string{"gate", "decisions", "--config", cfgPath, "--limit", "10"}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("gate decisions exit=%d stderr=%q", code, errBuf.String())
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("gate decisions lines = %d, want 2:\n%s", len(lines), out.String())
	}
	if !strings.Contains(lines[0], "job=#2") || !strings.Contains(lines[0], "would_hold") || !strings.Contains(lines[0], "blocked_by=5h") {
		t.Errorf("first (newest) line = %q", lines[0])
	}
	if !strings.Contains(lines[1], "job=#1") || !strings.Contains(lines[1], "would_pass") {
		t.Errorf("second line = %q", lines[1])
	}

	// Limit=1 returns only the newest decision.
	out.Reset()
	errBuf.Reset()
	code = run([]string{"gate", "decisions", "--config", cfgPath, "--limit", "1"}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("gate decisions limit=1 exit=%d stderr=%q", code, errBuf.String())
	}
	lines1 := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines1) != 1 || !strings.Contains(lines1[0], "job=#2") {
		t.Errorf("limit=1 output = %q, want only job=#2", out.String())
	}
}

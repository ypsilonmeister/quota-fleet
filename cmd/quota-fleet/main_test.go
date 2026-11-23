package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

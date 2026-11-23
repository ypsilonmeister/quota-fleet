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

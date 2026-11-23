package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ypsilonmeister/quota-fleet/internal/agentver"
	"github.com/ypsilonmeister/quota-fleet/internal/config"
)

func TestAgentsCLICheckAndMatrixAndBanner(t *testing.T) {
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}

	// Create two small fake agent scripts on local host.
	claudeBin := filepath.Join(binDir, "fake-claude")
	if err := os.WriteFile(claudeBin, []byte("#!/bin/sh\necho '2.1.76 (Claude Code)'\n"), 0o755); err != nil {
		t.Fatalf("write fake-claude: %v", err)
	}
	codexBin := filepath.Join(binDir, "fake-codex")
	if err := os.WriteFile(codexBin, []byte("#!/bin/sh\necho 'codex-cli 0.169.0'\n"), 0o755); err != nil {
		t.Fatalf("write fake-codex: %v", err)
	}

	cfgPath := filepath.Join(dir, "fleet.yaml")
	dbPath := filepath.Join(dir, "fleet.db")
	cfgText := "instance_id: cli-test\n" +
		"state_dir: " + dir + "\n" +
		"agents_check_interval: \"0\"\n" +
		"runtime:\n" +
		"  adapter: ssh\n" +
		"hosts:\n" +
		"  local-a:\n" +
		"    transport: local\n" +
		"    agents:\n" +
		"      claude:\n" +
		"        command: " + claudeBin + "\n" +
		"      codex:\n" +
		"        command: " + codexBin + "\n" +
		"        pin: \"0.169.0\"\n" +
		"      missing-agent:\n" +
		"        command: definitely-does-not-exist-cmd-xyz\n" +
		"agents:\n" +
		"  claude: {}\n" +
		"  codex: {}\n" +
		"  missing-agent: {}\n"
	if err := os.WriteFile(cfgPath, []byte(cfgText), 0o600); err != nil {
		t.Fatalf("write fleet.yaml: %v", err)
	}

	// 1. Run `quota-fleet agents check`.
	var stdout, stderr bytes.Buffer
	code := run([]string{"agents", "check", "--config", cfgPath, "--db", dbPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("agents check failed (code=%d): stderr=%s", code, stderr.String())
	}
	outStr := stdout.String()
	for _, want := range []string{"local-a", "2.1.76", "0.169.0 [pinned]", "not_found"} {
		if !strings.Contains(outStr, want) {
			t.Errorf("agents check output missing %q:\n%s", want, outStr)
		}
	}

	// 2. Run `quota-fleet agents --json` and verify structured matrix.
	stdout.Reset()
	stderr.Reset()
	code = run([]string{"agents", "--config", cfgPath, "--db", dbPath, "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("agents --json failed (code=%d): stderr=%s", code, stderr.String())
	}
	var m agentver.Matrix
	if err := json.Unmarshal(stdout.Bytes(), &m); err != nil {
		t.Fatalf("decode agents --json: %v", err)
	}
	if m.HasDrift {
		t.Errorf("expected HasDrift=false")
	}

	// 3. Verify serve banner explicitly notes disabled agents_check when interval is 0.
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	var banner bytes.Buffer
	printServeBanner(&banner, nil, cfg, "127.0.0.1:8737", dbPath, filepath.Join(dir, "deliveries"))
	if !strings.Contains(banner.String(), "agents_check: disabled (agents_check_interval=0)") {
		t.Errorf("serve banner missing disabled agents_check notice:\n%s", banner.String())
	}
}

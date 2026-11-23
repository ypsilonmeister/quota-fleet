package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseAndValidateFullDesignExample(t *testing.T) {
	yamlData := `
instance_id: "home-ubuntu-01"
state_dir: /var/lib/quota-fleet

limits:
  max_claimed: 3
  attempts_cap: 3

hosts:
  ubuntu:
    transport: ssh
    ssh: user@ubuntu.example.ts.net
    os: linux
    proxy_command: null
    key_file: ~/.ssh/id_ed25519
    workdir_root: ~/agent-work
    agents:
      agy:
        command: ~/.local/bin/agy
  penguin:
    transport: ssh
    ssh: user@penguin.example.ts.net
    os: linux
    workdir_root: ~/agent-work
    agents:
      codex:
        command: codex
        env:
          PATH: "~/.config/nvm/versions/node/v24.18.1/bin:$PATH"
  home-win:
    transport: ssh
    ssh: user@home-win.example.ts.net
    os: windows
    workdir_root: 'C:\Users\user\agent-work'
    owner_fixup: true
  local-node:
    transport: local
    os: linux
    workdir_root: /tmp/work

agents:
  claude:
    preflight: "claude --version"
    launch: "claude -p --model {model}"
    resume: "claude -p --resume {session_id}"
    session_log_glob: "~/.claude/projects/*/*.jsonl"
    sandbox_flag: "--permission-mode bypassPermissions"
  codex:
    launch: "codex exec"
    sandbox_flag: "--dangerously-bypass-approvals-and-sandbox"
  agy:
    launch: "agy --print --model {model}"

pools:
  claude:
    provider: claude-usage
  codex:
    provider: codex-usage
  agy-gemini:
    provider: agy-usage
    applies_to: [{ agent: agy, model_family: gemini }]
  agy-3p:
    provider: agy-usage
    applies_to: [{ agent: agy, model_family: claude-gpt }]
  agy-argon:
    unmeasured: true

# Unknown sections and keys must be ignored leniently.
quota:
  stale_after: 45m
  burndown:
    window_before_reset: 24h
    min_weekly_remaining_pct: 40
notifier:
  webhook_url: null
  file_dir: /var/lib/quota-fleet/deliveries
shepherd:
  reconciler_claimed_mode: report-only
future_unknown_key:
  nested: true
`
	cfg, err := Parse([]byte(yamlData))
	if err != nil {
		t.Fatalf("Parse() unexpected error: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() unexpected error: %v", err)
	}

	if cfg.InstanceID != "home-ubuntu-01" {
		t.Errorf("InstanceID = %q, want %q", cfg.InstanceID, "home-ubuntu-01")
	}
	if cfg.StateDir != "/var/lib/quota-fleet" {
		t.Errorf("StateDir = %q, want %q", cfg.StateDir, "/var/lib/quota-fleet")
	}
	if cfg.Limits.MaxClaimed != 3 || cfg.Limits.AttemptsCap != 3 {
		t.Errorf("Limits = %+v, want {3 3}", cfg.Limits)
	}
	if got := cfg.Hosts["ubuntu"].Agents["agy"].Command; got != "~/.local/bin/agy" {
		t.Errorf("ubuntu agy command = %q, want %q", got, "~/.local/bin/agy")
	}
	if got := cfg.Hosts["penguin"].Agents["codex"].Env["PATH"]; !strings.Contains(got, "nvm") {
		t.Errorf("penguin codex PATH = %q, want nvm path", got)
	}
	if !cfg.Hosts["home-win"].OwnerFixup {
		t.Errorf("home-win owner_fixup = false, want true")
	}
	if cfg.Agents["claude"].Resume != "claude -p --resume {session_id}" {
		t.Errorf("claude resume = %q", cfg.Agents["claude"].Resume)
	}
	if !cfg.Pools["agy-argon"].Unmeasured {
		t.Errorf("agy-argon unmeasured = false, want true")
	}
	if got := cfg.Pools["agy-gemini"].AppliesTo; len(got) != 1 || got[0].Agent != "agy" || got[0].ModelFamily != "gemini" {
		t.Errorf("agy-gemini applies_to = %+v", got)
	}
	if got := cfg.DBPath(); got != filepath.Join("/var/lib/quota-fleet", "fleet.db") {
		t.Errorf("DBPath() = %q", got)
	}
}

func TestValidateErrors(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name: "empty instance_id",
			yaml: `
instance_id: ""
limits:
  max_claimed: 2
  attempts_cap: 3
`,
			wantErr: "instance_id",
		},
		{
			name: "whitespace instance_id",
			yaml: `
instance_id: "   "
limits:
  max_claimed: 2
  attempts_cap: 3
`,
			wantErr: "instance_id",
		},
		{
			name: "zero max_claimed",
			yaml: `
instance_id: "inst-1"
limits:
  max_claimed: 0
  attempts_cap: 3
`,
			wantErr: "max_claimed",
		},
		{
			name: "negative attempts_cap",
			yaml: `
instance_id: "inst-1"
limits:
  max_claimed: 2
  attempts_cap: -1
`,
			wantErr: "attempts_cap",
		},
		{
			name: "invalid transport",
			yaml: `
instance_id: "inst-1"
limits:
  max_claimed: 1
  attempts_cap: 1
hosts:
  bad:
    transport: telnet
`,
			wantErr: "transport",
		},
		{
			name: "invalid os",
			yaml: `
instance_id: "inst-1"
limits:
  max_claimed: 1
  attempts_cap: 1
hosts:
  bad:
    os: plan9
`,
			wantErr: "os",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Parse([]byte(tc.yaml))
			if err != nil {
				t.Fatalf("Parse() unexpected error: %v", err)
			}
			err = cfg.Validate()
			if err == nil {
				t.Fatalf("Validate() expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("Validate() error = %q, want substring %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestLoadFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fleet.yaml")
	content := `
instance_id: "test-inst"
state_dir: "/tmp/state"
limits:
  max_claimed: 1
  attempts_cap: 2
unknown_top_level: 123
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	if cfg.InstanceID != "test-inst" {
		t.Errorf("InstanceID = %q, want %q", cfg.InstanceID, "test-inst")
	}

	if _, err := Load(filepath.Join(dir, "nonexistent.yaml")); err == nil {
		t.Errorf("Load(nonexistent) expected error, got nil")
	}
}

func TestAPIConfigDefaultsAndFields(t *testing.T) {
	// Default listen address when api section is omitted.
	cfg, err := Parse([]byte(`
instance_id: "inst-1"
limits:
  max_claimed: 2
  attempts_cap: 3
`))
	if err != nil {
		t.Fatalf("Parse default api: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate default api: %v", err)
	}
	if got := cfg.API.ListenAddr(); got != DefaultAPIListen {
		t.Errorf("ListenAddr() = %q, want %q", got, DefaultAPIListen)
	}
	if cfg.API.Token != "" || cfg.API.RequireAuth {
		t.Errorf("unexpected default API auth: token=%q require_auth=%v", cfg.API.Token, cfg.API.RequireAuth)
	}

	// Explicit api section.
	cfg2, err := Parse([]byte(`
instance_id: "inst-2"
api:
  listen: "0.0.0.0:9000"
  token: "secret-token-123"
  require_auth: true
limits:
  max_claimed: 2
  attempts_cap: 3
`))
	if err != nil {
		t.Fatalf("Parse explicit api: %v", err)
	}
	if err := cfg2.Validate(); err != nil {
		t.Fatalf("Validate explicit api: %v", err)
	}
	if got := cfg2.API.ListenAddr(); got != "0.0.0.0:9000" {
		t.Errorf("ListenAddr() = %q, want 0.0.0.0:9000", got)
	}
	if cfg2.API.Token != "secret-token-123" {
		t.Errorf("Token = %q, want secret-token-123", cfg2.API.Token)
	}
	if !cfg2.API.RequireAuth {
		t.Errorf("RequireAuth = false, want true")
	}
}

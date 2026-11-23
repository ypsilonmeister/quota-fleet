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
    owner_fixup_user: 'HOME-WIN\user'
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
    prompt_style: argument

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
	if got := cfg.Hosts["home-win"].OwnerFixupUser; got != `HOME-WIN\user` {
		t.Errorf("home-win owner_fixup_user = %q, want %q", got, `HOME-WIN\user`)
	}
	if cfg.Agents["claude"].Resume != "claude -p --resume {session_id}" {
		t.Errorf("claude resume = %q", cfg.Agents["claude"].Resume)
	}
	if cfg.Agents["agy"].PromptStyle != PromptStyleArgument {
		t.Errorf("agy prompt_style = %q, want %q", cfg.Agents["agy"].PromptStyle, PromptStyleArgument)
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
		{
			name: "invalid agent prompt_style",
			yaml: `
instance_id: "inst-1"
limits:
  max_claimed: 1
  attempts_cap: 1
agents:
  agy:
    launch: "agy --print"
    prompt_style: env
`,
			wantErr: "prompt_style",
		},
		{
			name: "owner_fixup_user on linux host",
			yaml: `
instance_id: "inst-1"
limits:
  max_claimed: 1
  attempts_cap: 1
hosts:
  ubuntu:
    transport: ssh
    os: linux
    owner_fixup_user: "HOST\\worker"
`,
			wantErr: "owner_fixup_user",
		},
		{
			name: "owner_fixup_user with disallowed shell characters",
			yaml: `
instance_id: "inst-1"
limits:
  max_claimed: 1
  attempts_cap: 1
hosts:
  win:
    transport: ssh
    os: windows
    owner_fixup: true
    owner_fixup_user: "HOST\\worker; whoami"
`,
			wantErr: "owner_fixup_user",
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

func TestRuntimeConfigDefaultsAndValidation(t *testing.T) {
	cfg, err := Parse([]byte(`
instance_id: "inst-rt"
state_dir: "/var/lib/qf"
limits:
  max_claimed: 2
  attempts_cap: 3
`))
	if err != nil {
		t.Fatalf("Parse default runtime: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate default runtime: %v", err)
	}
	if got := cfg.Runtime.EffectiveAdapter(); got != DefaultRuntimeAdapter {
		t.Errorf("EffectiveAdapter() = %q, want %q", got, DefaultRuntimeAdapter)
	}
	if got := cfg.Runtime.EffectiveDispatchInterval(); got != DefaultDispatchInterval {
		t.Errorf("EffectiveDispatchInterval() = %v, want %v", got, DefaultDispatchInterval)
	}
	if got := cfg.Runtime.EffectiveShepherdInterval(); got != DefaultShepherdInterval {
		t.Errorf("EffectiveShepherdInterval() = %v, want %v", got, DefaultShepherdInterval)
	}
	if got := cfg.Runtime.EffectiveReconcileInterval(); got != DefaultReconcileInterval {
		t.Errorf("EffectiveReconcileInterval() = %v, want %v", got, DefaultReconcileInterval)
	}
	if got := cfg.Runtime.EffectiveStaleClaimedAfter(); got != DefaultStaleClaimedAfter {
		t.Errorf("EffectiveStaleClaimedAfter() = %v, want %v", got, DefaultStaleClaimedAfter)
	}
	if got := cfg.Runtime.EffectiveSignalMaxBytes(); got != DefaultSignalMaxBytes {
		t.Errorf("EffectiveSignalMaxBytes() = %d, want %d", got, DefaultSignalMaxBytes)
	}
	if got := cfg.Runtime.EffectiveDeliveriesDir(cfg.StateDir); got != filepath.Join("/var/lib/qf", "deliveries") {
		t.Errorf("EffectiveDeliveriesDir() = %q", got)
	}

	// Explicit valid runtime section.
	cfg2, err := Parse([]byte(`
instance_id: "inst-rt-2"
state_dir: "/var/lib/qf"
limits:
  max_claimed: 2
  attempts_cap: 3
runtime:
  adapter: "mock"
  dispatch_interval: "200ms"
  shepherd_interval: "1s"
  reconcile_interval: "5s"
  stale_claimed_after: "2m"
  signal_max_bytes: 4096
  deliveries_dir: "out/deliveries"
`))
	if err != nil {
		t.Fatalf("Parse explicit runtime: %v", err)
	}
	if err := cfg2.Validate(); err != nil {
		t.Fatalf("Validate explicit runtime: %v", err)
	}
	if got := cfg2.Runtime.EffectiveDeliveriesDir(cfg2.StateDir); got != filepath.Join("/var/lib/qf", "out/deliveries") {
		t.Errorf("EffectiveDeliveriesDir(relative) = %q", got)
	}
	if got := cfg2.Runtime.EffectiveSignalMaxBytes(); got != 4096 {
		t.Errorf("EffectiveSignalMaxBytes() = %d, want 4096", got)
	}

	// Explicit valid runtime section with ssh adapter and host SSH options.
	cfgSSH, err := Parse([]byte(`
instance_id: "inst-rt-ssh"
state_dir: "/var/lib/qf"
limits:
  max_claimed: 2
  attempts_cap: 3
runtime:
  adapter: "ssh"
hosts:
  ubuntu:
    transport: ssh
    ssh: user@ubuntu.example.ts.net
    os: linux
    key_file: ~/.ssh/id_ed25519
    proxy_command: "nc -X connect -x 127.0.0.1:8080 %h %p"
    known_hosts_file: ~/.ssh/known_hosts
    strict_host_key_checking: accept-new
`))
	if err != nil {
		t.Fatalf("Parse ssh runtime: %v", err)
	}
	if err := cfgSSH.Validate(); err != nil {
		t.Fatalf("Validate ssh runtime: %v", err)
	}
	if got := cfgSSH.Runtime.EffectiveAdapter(); got != RuntimeAdapterSSH {
		t.Errorf("EffectiveAdapter() = %q, want %q", got, RuntimeAdapterSSH)
	}
	if cfgSSH.Hosts["ubuntu"].ProxyCommand == nil || !strings.Contains(*cfgSSH.Hosts["ubuntu"].ProxyCommand, "127.0.0.1:8080") {
		t.Errorf("ProxyCommand = %v, want non-nil proxy command", cfgSSH.Hosts["ubuntu"].ProxyCommand)
	}
	if cfgSSH.Hosts["ubuntu"].KnownHostsFile != "~/.ssh/known_hosts" {
		t.Errorf("KnownHostsFile = %q", cfgSSH.Hosts["ubuntu"].KnownHostsFile)
	}
	if cfgSSH.Hosts["ubuntu"].StrictHostKeyChecking != "accept-new" {
		t.Errorf("StrictHostKeyChecking = %q", cfgSSH.Hosts["ubuntu"].StrictHostKeyChecking)
	}

	// Invalid runtime configs.
	invalidCases := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name: "unsupported adapter",
			yaml: `
instance_id: "inst"
limits: { max_claimed: 1, attempts_cap: 1 }
runtime:
  adapter: "unknown-adapter"
`,
			wantErr: "runtime.adapter",
		},
		{
			name: "invalid strict_host_key_checking",
			yaml: `
instance_id: "inst"
limits: { max_claimed: 1, attempts_cap: 1 }
hosts:
  bad:
    transport: ssh
    strict_host_key_checking: maybe
`,
			wantErr: "strict_host_key_checking",
		},
		{
			name: "bad duration",
			yaml: `
instance_id: "inst"
limits: { max_claimed: 1, attempts_cap: 1 }
runtime:
  shepherd_interval: "notaduration"
`,
			wantErr: "shepherd_interval",
		},
		{
			name: "non-positive duration",
			yaml: `
instance_id: "inst"
limits: { max_claimed: 1, attempts_cap: 1 }
runtime:
  stale_claimed_after: "0s"
`,
			wantErr: "stale_claimed_after",
		},
		{
			name: "negative signal_max_bytes",
			yaml: `
instance_id: "inst"
limits: { max_claimed: 1, attempts_cap: 1 }
runtime:
  signal_max_bytes: -10
`,
			wantErr: "signal_max_bytes",
		},
	}
	for _, tc := range invalidCases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Parse([]byte(tc.yaml))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			err = c.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate() err = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}

func TestQuotaConfigDefaultsAndValidation(t *testing.T) {
	cfg, err := Parse([]byte(`
instance_id: "inst-q-default"
limits: { max_claimed: 1, attempts_cap: 1 }
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if got := cfg.Quota.EffectiveGateMode(); got != GateModeOff {
		t.Errorf("EffectiveGateMode() = %q, want %q", got, GateModeOff)
	}
	if got := cfg.Quota.EffectiveStaleAfter(); got != DefaultQuotaStaleAfter {
		t.Errorf("EffectiveStaleAfter() = %v, want %v", got, DefaultQuotaStaleAfter)
	}
	if got := cfg.Quota.EffectiveGateCheckInterval(); got != DefaultQuotaGateCheckInterval {
		t.Errorf("EffectiveGateCheckInterval() = %v, want %v", got, DefaultQuotaGateCheckInterval)
	}

	cfgDry, err := Parse([]byte(`
instance_id: "inst-q-dry"
limits: { max_claimed: 1, attempts_cap: 1 }
quota:
  usage_file: ~/workspace/muse-widget/usage.json
  stale_after: 30m
  gate_mode: dry-run
  gate_check_interval: 2m
`))
	if err != nil {
		t.Fatalf("Parse dry-run: %v", err)
	}
	if err := cfgDry.Validate(); err != nil {
		t.Fatalf("Validate dry-run: %v", err)
	}
	if cfgDry.Quota.UsageFile != "~/workspace/muse-widget/usage.json" {
		t.Errorf("UsageFile = %q", cfgDry.Quota.UsageFile)
	}
	if cfgDry.Quota.EffectiveGateMode() != GateModeDryRun {
		t.Errorf("EffectiveGateMode() = %q, want dry-run", cfgDry.Quota.EffectiveGateMode())
	}
	if cfgDry.Quota.EffectiveStaleAfter().Minutes() != 30 {
		t.Errorf("EffectiveStaleAfter() = %v, want 30m", cfgDry.Quota.EffectiveStaleAfter())
	}
	if cfgDry.Quota.EffectiveGateCheckInterval().Minutes() != 2 {
		t.Errorf("EffectiveGateCheckInterval() = %v, want 2m", cfgDry.Quota.EffectiveGateCheckInterval())
	}

	cfgEnforce, err := Parse([]byte(`
instance_id: "inst-q-enforce"
limits: { max_claimed: 1, attempts_cap: 1 }
quota:
  usage_file: ~/workspace/muse-widget/usage.json
  stale_after: 45m
  gate_mode: enforce
  gate_check_interval: 30s
`))
	if err != nil {
		t.Fatalf("Parse enforce: %v", err)
	}
	if err := cfgEnforce.Validate(); err != nil {
		t.Fatalf("Validate enforce: %v", err)
	}
	if cfgEnforce.Quota.EffectiveGateMode() != GateModeEnforce {
		t.Errorf("EffectiveGateMode() = %q, want enforce", cfgEnforce.Quota.EffectiveGateMode())
	}
	if cfgEnforce.Quota.EffectiveGateCheckInterval().Seconds() != 30 {
		t.Errorf("EffectiveGateCheckInterval() = %v, want 30s", cfgEnforce.Quota.EffectiveGateCheckInterval())
	}

	invalidCases := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name: "unsupported gate_mode",
			yaml: `
instance_id: "inst"
limits: { max_claimed: 1, attempts_cap: 1 }
quota:
  gate_mode: strict
`,
			wantErr: "quota.gate_mode",
		},
		{
			name: "invalid stale_after duration",
			yaml: `
instance_id: "inst"
limits: { max_claimed: 1, attempts_cap: 1 }
quota:
  stale_after: "bad"
`,
			wantErr: "quota.stale_after",
		},
		{
			name: "zero stale_after",
			yaml: `
instance_id: "inst"
limits: { max_claimed: 1, attempts_cap: 1 }
quota:
  stale_after: "0s"
`,
			wantErr: "quota.stale_after",
		},
		{
			name: "invalid gate_check_interval duration",
			yaml: `
instance_id: "inst"
limits: { max_claimed: 1, attempts_cap: 1 }
quota:
  gate_check_interval: "nope"
`,
			wantErr: "quota.gate_check_interval",
		},
		{
			name: "zero gate_check_interval",
			yaml: `
instance_id: "inst"
limits: { max_claimed: 1, attempts_cap: 1 }
quota:
  gate_check_interval: "0s"
`,
			wantErr: "quota.gate_check_interval",
		},
	}
	for _, tc := range invalidCases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Parse([]byte(tc.yaml))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			err = c.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate() err = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}

func TestEvaluatorConfigDefaultsAndValidation(t *testing.T) {
	// Default values when evaluator section is omitted.
	cfg, err := Parse([]byte(`
instance_id: "inst-eval-default"
limits: { max_claimed: 1, attempts_cap: 1 }
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if got := cfg.Evaluator.EffectiveMode(); got != EvaluatorModeTypedDecision {
		t.Errorf("EffectiveMode() = %q, want %q", got, EvaluatorModeTypedDecision)
	}
	if got := cfg.Evaluator.EffectiveAPIKeyEnv(); got != DefaultEvaluatorAPIKeyEnv {
		t.Errorf("EffectiveAPIKeyEnv() = %q, want %q", got, DefaultEvaluatorAPIKeyEnv)
	}
	if got := cfg.Evaluator.EffectiveDecisionThreshold(); got != DefaultEvaluatorDecisionThreshold {
		t.Errorf("EffectiveDecisionThreshold() = %v, want %v", got, DefaultEvaluatorDecisionThreshold)
	}
	if got := cfg.Evaluator.EffectiveMaxEvalRetries(); got != DefaultEvaluatorMaxEvalRetries {
		t.Errorf("EffectiveMaxEvalRetries() = %d, want %d", got, DefaultEvaluatorMaxEvalRetries)
	}

	// Explicit valid evaluator section.
	cfgExplicit, err := Parse([]byte(`
instance_id: "inst-eval-explicit"
limits: { max_claimed: 1, attempts_cap: 1 }
evaluator:
  mode: always-pass
  api_key_env: CUSTOM_GEMINI_KEY
  decision_threshold: 0.75
  max_eval_retries: 5
`))
	if err != nil {
		t.Fatalf("Parse explicit: %v", err)
	}
	if err := cfgExplicit.Validate(); err != nil {
		t.Fatalf("Validate explicit: %v", err)
	}
	if got := cfgExplicit.Evaluator.EffectiveMode(); got != EvaluatorModeAlwaysPass {
		t.Errorf("EffectiveMode() = %q, want %q", got, EvaluatorModeAlwaysPass)
	}
	if got := cfgExplicit.Evaluator.EffectiveAPIKeyEnv(); got != "CUSTOM_GEMINI_KEY" {
		t.Errorf("EffectiveAPIKeyEnv() = %q, want CUSTOM_GEMINI_KEY", got)
	}
	if got := cfgExplicit.Evaluator.EffectiveDecisionThreshold(); got != 0.75 {
		t.Errorf("EffectiveDecisionThreshold() = %v, want 0.75", got)
	}
	if got := cfgExplicit.Evaluator.EffectiveMaxEvalRetries(); got != 5 {
		t.Errorf("EffectiveMaxEvalRetries() = %d, want 5", got)
	}

	// Boundary decision_threshold values (0.0 and 1.0) are valid.
	for _, th := range []string{"0.0", "1.0"} {
		c, err := Parse([]byte("instance_id: inst\nlimits: { max_claimed: 1, attempts_cap: 1 }\nevaluator:\n  decision_threshold: " + th + "\n"))
		if err != nil {
			t.Fatalf("Parse threshold %s: %v", th, err)
		}
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate threshold %s: %v", th, err)
		}
	}

	// Invalid evaluator configs.
	invalidCases := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name: "unsupported evaluator mode",
			yaml: `
instance_id: "inst"
limits: { max_claimed: 1, attempts_cap: 1 }
evaluator:
  mode: llm-only
`,
			wantErr: "evaluator.mode",
		},
		{
			name: "negative decision_threshold",
			yaml: `
instance_id: "inst"
limits: { max_claimed: 1, attempts_cap: 1 }
evaluator:
  decision_threshold: -0.1
`,
			wantErr: "evaluator.decision_threshold",
		},
		{
			name: "decision_threshold > 1",
			yaml: `
instance_id: "inst"
limits: { max_claimed: 1, attempts_cap: 1 }
evaluator:
  decision_threshold: 1.5
`,
			wantErr: "evaluator.decision_threshold",
		},
		{
			name: "zero max_eval_retries",
			yaml: `
instance_id: "inst"
limits: { max_claimed: 1, attempts_cap: 1 }
evaluator:
  max_eval_retries: 0
`,
			wantErr: "evaluator.max_eval_retries",
		},
		{
			name: "negative max_eval_retries",
			yaml: `
instance_id: "inst"
limits: { max_claimed: 1, attempts_cap: 1 }
evaluator:
  max_eval_retries: -2
`,
			wantErr: "evaluator.max_eval_retries",
		},
	}
	for _, tc := range invalidCases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Parse([]byte(tc.yaml))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			err = c.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate() err = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}

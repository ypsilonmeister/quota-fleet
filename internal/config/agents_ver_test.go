package config

import (
	"testing"
	"time"
)

func TestAgentsVersionConfigParsingAndValidation(t *testing.T) {
	yamlData := []byte(`
instance_id: test-fleet
agents_check_interval: "0"
hosts:
  home-win:
    transport: ssh
    ssh: home-win
    os: windows
    agents:
      codex:
        pin: "0.169.0"
        manual_update: true
        version_args: ["-V"]
agents:
  agy:
    version_args: "--version"
    manual_update: true
  claude:
    version_args: ["--version"]
`)
	cfg, err := Parse(yamlData)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate failed: %v", err)
	}
	if got := cfg.EffectiveAgentsCheckInterval(); got != 0 {
		t.Errorf("EffectiveAgentsCheckInterval = %v, want 0 (disabled)", got)
	}
	if got := cfg.Hosts["home-win"].Agents["codex"].Pin; got != "0.169.0" {
		t.Errorf("pin = %q, want 0.169.0", got)
	}
	if !cfg.Hosts["home-win"].Agents["codex"].ManualUpdate {
		t.Errorf("expected home-win/codex manual_update=true")
	}
	if args := cfg.Agents["agy"].VersionArgs; len(args) != 1 || args[0] != "--version" {
		t.Errorf("agy version_args = %v, want [--version]", args)
	}
	if args := cfg.Hosts["home-win"].Agents["codex"].VersionArgs; len(args) != 1 || args[0] != "-V" {
		t.Errorf("codex version_args = %v, want [-V]", args)
	}

	// Default interval is 6h when omitted.
	defaultCfg, err := Parse([]byte("instance_id: def\n"))
	if err != nil {
		t.Fatalf("Parse default: %v", err)
	}
	if got := defaultCfg.EffectiveAgentsCheckInterval(); got != 6*time.Hour {
		t.Errorf("default EffectiveAgentsCheckInterval = %v, want 6h", got)
	}

	// Negative interval fails validation.
	badCfg, err := Parse([]byte("instance_id: bad\nruntime:\n  agents_check_interval: -1m\n"))
	if err != nil {
		t.Fatalf("Parse bad: %v", err)
	}
	if err := badCfg.Validate(); err == nil {
		t.Errorf("expected negative agents_check_interval to fail Validate()")
	}
}

package config

import (
	"reflect"
	"testing"
	"time"
)

func TestModelsConfigParsingAndAllAgentsUnion(t *testing.T) {
	yamlData := []byte(`
instance_id: test-models
models_check_interval: "12h"
hosts:
  ubuntu:
    transport: local
    agents:
      claude: {}
      agy: {}
      codex: {}
  home-win:
    transport: ssh
    ssh: home-win
    os: windows
    agents:
      codex: {}
agents:
  agy:
    launch: "agy --model {model} --print"
  claude:
    models:
      - claude-opus-4-6
      - id: claude-sonnet-4-6
        display_name: "Claude Sonnet 4.6"
  custom-top-only:
    launch: "custom"
`)
	cfg, err := Parse(yamlData)
	if err != nil {
		t.Fatalf("Parse failed: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate failed: %v", err)
	}
	if got := cfg.EffectiveModelsCheckInterval(); got != 12*time.Hour {
		t.Errorf("EffectiveModelsCheckInterval = %v, want 12h", got)
	}

	wantAgents := []string{"agy", "claude", "codex", "custom-top-only"}
	if got := cfg.AllAgents(); !reflect.DeepEqual(got, wantAgents) {
		t.Errorf("AllAgents() = %v, want %v", got, wantAgents)
	}

	hm := cfg.HostAgentsMap()
	if !reflect.DeepEqual(hm["ubuntu"], []string{"agy", "claude", "codex"}) {
		t.Errorf("HostAgentsMap[ubuntu] = %v", hm["ubuntu"])
	}
	if !reflect.DeepEqual(hm["home-win"], []string{"codex"}) {
		t.Errorf("HostAgentsMap[home-win] = %v", hm["home-win"])
	}

	wantClaudeModels := []ModelConfig{
		{ID: "claude-opus-4-6", DisplayName: ""},
		{ID: "claude-sonnet-4-6", DisplayName: "Claude Sonnet 4.6"},
	}
	if got := cfg.Agents["claude"].Models; !reflect.DeepEqual(got, wantClaudeModels) {
		t.Errorf("Agents[claude].Models = %+v, want %+v", got, wantClaudeModels)
	}

	// Default models_check_interval is 24h.
	defCfg, err := Parse([]byte("instance_id: def\n"))
	if err != nil {
		t.Fatalf("Parse default: %v", err)
	}
	if got := defCfg.EffectiveModelsCheckInterval(); got != 24*time.Hour {
		t.Errorf("default EffectiveModelsCheckInterval = %v, want 24h", got)
	}

	// "0" disables background model probing; runtime overrides top-level.
	zeroCfg, err := Parse([]byte("instance_id: z\nmodels_check_interval: 12h\nruntime:\n  models_check_interval: \"0\"\n"))
	if err != nil {
		t.Fatalf("Parse zero: %v", err)
	}
	if err := zeroCfg.Validate(); err != nil {
		t.Fatalf("Validate zero: %v", err)
	}
	if got := zeroCfg.EffectiveModelsCheckInterval(); got != 0 {
		t.Errorf("zero EffectiveModelsCheckInterval = %v, want 0", got)
	}

	// Negative models_check_interval fails validation.
	badCfg, err := Parse([]byte("instance_id: bad\nmodels_check_interval: -5m\n"))
	if err != nil {
		t.Fatalf("Parse bad: %v", err)
	}
	if err := badCfg.Validate(); err == nil {
		t.Errorf("expected negative models_check_interval to fail Validate()")
	}
}

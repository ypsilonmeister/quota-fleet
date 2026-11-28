package config

import (
	"strings"
	"testing"
	"time"
)

func TestRequeueConfigDefaultsAndOverrides(t *testing.T) {
	cfg, err := Parse([]byte(`
quota:
  gate_mode: enforce
  cooldown:
    enabled: true
  requeue:
    enabled: true
pools:
  codex: {}
  agy:
    unmeasured: true
    requeue:
      max_retries: 6
      failover_alt_pools: false
  off:
    requeue:
      enabled: false
`))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.validateRequeue(); err != nil {
		t.Fatal(err)
	}
	if p := cfg.RequeueFor("codex"); !p.Enabled || p.MaxRetries != 3 || p.StallAfter != 6*time.Hour || !p.FailoverAltPools {
		t.Fatalf("codex policy = %+v", p)
	}
	if p := cfg.RequeueFor("agy"); !p.Enabled || p.MaxRetries != 6 || p.FailoverAltPools {
		t.Fatalf("agy policy = %+v", p)
	}
	if !cfg.RequeueActive("codex") || cfg.RequeueActive("off") || cfg.RequeueActive("undefined") {
		t.Fatal("RequeueActive mismatch")
	}
	if w := cfg.RequeueWarnings(); len(w) != 0 {
		t.Fatalf("unexpected warnings %v", w)
	}
}

func TestRequeueDefaultOff(t *testing.T) {
	cfg, err := Parse([]byte(`
quota:
  gate_mode: enforce
  cooldown: {enabled: true}
pools:
  codex: {}
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RequeueFor("codex").Enabled || cfg.RequeueActive("codex") || cfg.HasRequeue() {
		t.Fatal("requeue must be opt-in")
	}
	var nilCfg *Config
	if nilCfg.RequeueActive("codex") || nilCfg.HasRequeue() {
		t.Fatal("nil config must not requeue")
	}
}

func TestRequeueInactiveWithoutEnforceOrCooldown(t *testing.T) {
	cfg, err := Parse([]byte(`
quota:
  gate_mode: dry-run
  cooldown: {enabled: true}
  requeue: {enabled: true}
pools:
  codex: {}
  nocool:
    cooldown: {enabled: false}
`))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.validateRequeue(); err != nil {
		t.Fatalf("inherited requeue without cooldown must stay valid (inert): %v", err)
	}
	if cfg.RequeueActive("codex") {
		t.Fatal("requeue must not be active outside gate_mode enforce")
	}
	if w := cfg.RequeueWarnings(); len(w) != 1 || !strings.Contains(w[0], "dry-run") {
		t.Fatalf("warnings = %v", w)
	}
	cfg.Quota.GateMode = GateModeEnforce
	if cfg.RequeueActive("nocool") {
		t.Fatal("requeue must not be active without the pool cooldown")
	}
}

func TestRequeueConfigValidation(t *testing.T) {
	for _, bad := range []string{
		"quota: {requeue: {max_retries: -1}}",
		"quota: {requeue: {stall_after: nonsense}}",
		"pools: {p: {requeue: {max_retries: -2}}}",
		"pools: {p: {requeue: {enabled: true}}}", // cooldown disabled
		"quota: {cooldown: {enabled: true}}\npools: {p: {cooldown: {enabled: false}, requeue: {enabled: true}}}",
	} {
		cfg, err := Parse([]byte(bad))
		if err == nil {
			err = cfg.validateRequeue()
		}
		if err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	cfg, err := Parse([]byte("quota: {cooldown: {enabled: true}}\npools: {p: {requeue: {enabled: true, max_retries: 0}}}"))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.validateRequeue(); err != nil {
		t.Fatalf("max_retries 0 disables requeue and must be accepted: %v", err)
	}
	if cfg.RequeueActive("p") {
		t.Fatal("max_retries 0 must disable requeue")
	}
}

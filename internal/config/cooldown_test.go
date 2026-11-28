package config

import (
	"testing"
	"time"
)

func TestCooldownConfig(t *testing.T) {
	cfg, err := Parse([]byte(`
quota:
  cooldown:
    enabled: true
    streak_reset_after: 0
pools:
  primary:
    cooldown:
      initial: 10m
      max: 2h
  other:
    cooldown:
      enabled: false
`))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.validateCooldown(); err != nil {
		t.Fatal(err)
	}
	p := cfg.CooldownFor("primary")
	if !p.Enabled || p.Initial != 10*time.Minute || p.Max != 2*time.Hour || p.Multiplier != 2 || p.StreakResetAfter != 0 {
		t.Fatalf("wrong effective policy: %+v", p)
	}
	if cfg.CooldownFor("other").Enabled || (&Config{}).CooldownFor("other").Enabled {
		t.Fatal("expected opt out")
	}
	for _, bad := range []string{
		"quota: {cooldown: {initial: 0s}}",
		"quota: {cooldown: {initial: 2h}}",
		"quota: {cooldown: {multiplier: .inf}}",
		"quota: {cooldown: {streak_reset_after: -1s}}",
		"pools: {primary: {cooldown: {max: 3m}}}",
		"agents: {custom: {rate_limit_patterns: ['[']}}",
		"agents: {custom: {rate_limit_exit_codes: [0]}}",
	} {
		cfg, err := Parse([]byte(bad))
		if err == nil {
			err = cfg.validateCooldown()
		}
		if err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

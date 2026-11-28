package config

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"time"
)

type CooldownConfig struct {
	Enabled          *bool    `yaml:"enabled"`
	Initial          *string  `yaml:"initial"`
	Multiplier       *float64 `yaml:"multiplier"`
	Max              *string  `yaml:"max"`
	StreakResetAfter *string  `yaml:"streak_reset_after"`
}

type CooldownPolicy struct {
	Enabled                        bool
	Initial, Max, StreakResetAfter time.Duration
	Multiplier                     float64
}

func (c CooldownConfig) apply(p CooldownPolicy) CooldownPolicy {
	if c.Enabled != nil {
		p.Enabled = *c.Enabled
	}
	if c.Initial != nil {
		p.Initial = cooldownDuration(*c.Initial)
	}
	if c.Max != nil {
		p.Max = cooldownDuration(*c.Max)
	}
	if c.Multiplier != nil {
		p.Multiplier = *c.Multiplier
	}
	if c.StreakResetAfter != nil {
		p.StreakResetAfter = cooldownDuration(*c.StreakResetAfter)
	}
	return p
}

func cooldownDuration(raw string) time.Duration {
	d, err := time.ParseDuration(raw)
	if err != nil {
		return -1
	}
	return d
}

func (c *Config) CooldownFor(pool string) CooldownPolicy {
	p := c.Quota.Cooldown.apply(CooldownPolicy{Initial: 5 * time.Minute, Max: time.Hour, Multiplier: 2, StreakResetAfter: 2 * time.Hour})
	return c.Pools[pool].Cooldown.apply(p)
}

func (c *Config) HasCooldown() bool {
	for pool := range c.Pools {
		if c.CooldownFor(pool).Enabled {
			return true
		}
	}
	return false
}

func (p CooldownPolicy) validate() error {
	if p.Initial <= 0 || p.Max < p.Initial || p.StreakResetAfter < 0 ||
		p.Multiplier < 1 || math.IsInf(p.Multiplier, 0) || math.IsNaN(p.Multiplier) {
		return errors.New("require initial > 0, max >= initial, finite multiplier >= 1 and streak_reset_after >= 0")
	}
	return nil
}

func (c *Config) validateCooldown() error {
	var errs []error
	if err := c.CooldownFor("").validate(); err != nil {
		errs = append(errs, fmt.Errorf("quota.cooldown: %w", err))
	}
	for name := range c.Pools {
		if err := c.CooldownFor(name).validate(); err != nil {
			errs = append(errs, fmt.Errorf("pools.%s.cooldown: %w", name, err))
		}
	}
	for name, agent := range c.Agents {
		for _, pattern := range agent.RateLimitPatterns {
			if _, err := regexp.Compile(pattern); err != nil || pattern == "" {
				errs = append(errs, fmt.Errorf("agents.%s.rate_limit_patterns: invalid pattern %q", name, pattern))
			}
		}
		for _, code := range agent.RateLimitExitCodes {
			if code <= 0 {
				errs = append(errs, fmt.Errorf("agents.%s.rate_limit_exit_codes: expected positive exit code, got %d", name, code))
			}
		}
	}
	return errors.Join(errs...)
}

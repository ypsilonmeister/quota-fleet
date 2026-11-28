package config

import (
	"errors"
	"fmt"
	"sort"
	"time"
)

// DefaultRequeueMaxRetries is the default number of automatic rate-limit requeues per job
// (Issue #56 decision 2).
const DefaultRequeueMaxRetries = 3

// DefaultRequeueStallAfter is the default age after which a rate-limit requeued job that is
// still queued is reported by the Reconciler (Issue #56 decision 2; report only).
const DefaultRequeueStallAfter = 6 * time.Hour

// RequeueConfig configures automatic requeue of jobs that failed on a provider rate limit
// (Issue #56). It lives under quota.requeue and pools.<name>.requeue; pool values override
// the global ones key by key.
type RequeueConfig struct {
	Enabled          *bool   `yaml:"enabled"`
	MaxRetries       *int    `yaml:"max_retries"`
	StallAfter       *string `yaml:"stall_after"`
	FailoverAltPools *bool   `yaml:"failover_alt_pools"`
}

// RequeuePolicy is the effective requeue configuration of one pool.
type RequeuePolicy struct {
	// Enabled is the opt-in switch (default false).
	Enabled bool
	// MaxRetries bounds the automatic requeues per job (default 3). 0 disables requeue.
	MaxRetries int
	// StallAfter is the Reconciler report threshold for a requeued job still queued (default 6h).
	StallAfter time.Duration
	// FailoverAltPools lets a requeued job move to a healthy declared alt pool (default true).
	FailoverAltPools bool
}

func (c RequeueConfig) apply(p RequeuePolicy) RequeuePolicy {
	if c.Enabled != nil {
		p.Enabled = *c.Enabled
	}
	if c.MaxRetries != nil {
		p.MaxRetries = *c.MaxRetries
	}
	if c.StallAfter != nil {
		p.StallAfter = cooldownDuration(*c.StallAfter)
	}
	if c.FailoverAltPools != nil {
		p.FailoverAltPools = *c.FailoverAltPools
	}
	return p
}

// RequeueFor returns the effective requeue policy for pool. An empty or undefined pool
// gets the global quota.requeue values.
func (c *Config) RequeueFor(pool string) RequeuePolicy {
	p := c.Quota.Requeue.apply(RequeuePolicy{
		MaxRetries:       DefaultRequeueMaxRetries,
		StallAfter:       DefaultRequeueStallAfter,
		FailoverAltPools: true,
	})
	return c.Pools[pool].Requeue.apply(p)
}

// RequeueActive reports whether a rate-limited job on pool is requeued instead of failed:
// requeue opted in with max_retries > 0, the pool's cooldown enabled, and gate_mode enforce.
func (c *Config) RequeueActive(pool string) bool {
	if c == nil {
		return false
	}
	if _, ok := c.Pools[pool]; !ok {
		return false
	}
	p := c.RequeueFor(pool)
	return p.Enabled && p.MaxRetries > 0 && c.CooldownFor(pool).Enabled &&
		c.Quota.EffectiveGateMode() == GateModeEnforce
}

// HasRequeue reports whether any configured pool opted in to rate-limit requeue.
func (c *Config) HasRequeue() bool {
	if c == nil {
		return false
	}
	for pool := range c.Pools {
		if c.RequeueFor(pool).Enabled {
			return true
		}
	}
	return false
}

// RequeueWarnings lists configuration that is valid but leaves requeue inert
// (gate_mode is not enforce), for a startup log line.
func (c *Config) RequeueWarnings() []string {
	if c == nil || !c.HasRequeue() || c.Quota.EffectiveGateMode() == GateModeEnforce {
		return nil
	}
	return []string{fmt.Sprintf("quota.requeue is enabled but quota.gate_mode is %q; rate-limited jobs are not requeued unless gate_mode is enforce", c.Quota.EffectiveGateMode())}
}

func (c *Config) validateRequeue() error {
	var errs []error
	check := func(prefix string, p RequeuePolicy) {
		if p.MaxRetries < 0 {
			errs = append(errs, fmt.Errorf("%s.max_retries must be >= 0 (got %d)", prefix, p.MaxRetries))
		}
		if p.StallAfter < 0 {
			errs = append(errs, fmt.Errorf("%s.stall_after must be a non-negative duration", prefix))
		}
	}
	check("quota.requeue", c.RequeueFor(""))
	names := make([]string, 0, len(c.Pools))
	for name := range c.Pools {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		p := c.RequeueFor(name)
		check("pools."+name+".requeue", p)
		// Requeue without a backoff would relaunch into the same 429 at once. A pool that only
		// inherits quota.requeue.enabled without a cooldown stays inert (RequeueActive is false).
		if e := c.Pools[name].Requeue.Enabled; e != nil && *e && p.MaxRetries > 0 && !c.CooldownFor(name).Enabled {
			errs = append(errs, fmt.Errorf("pools.%s.requeue.enabled requires pools.%s.cooldown.enabled (or quota.cooldown.enabled)", name, name))
		}
	}
	return errors.Join(errs...)
}

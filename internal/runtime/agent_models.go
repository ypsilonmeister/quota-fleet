package runtime

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/adapter"
	"github.com/ypsilonmeister/quota-fleet/internal/agentmodels"
	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

// AgentModelProberOptions configures the periodic host × agent model catalog prober.
type AgentModelProberOptions struct {
	Store        *store.Store
	Config       *config.Config
	Provider     adapter.HostFileAccessProvider
	Interval     time.Duration
	ProbeTimeout time.Duration
	Now          func() time.Time
	Logger       *log.Logger
}

// AgentModelProber runs periodic model catalog probes (default 24h) and persists
// results to agent_models.
type AgentModelProber struct {
	prober   *agentmodels.Prober
	interval time.Duration
	logger   *log.Logger
	mu       sync.Mutex
}

// NewAgentModelProber constructs an AgentModelProber.
// When Store or Config is nil, it returns a disabled prober.
func NewAgentModelProber(opts AgentModelProberOptions) *AgentModelProber {
	logger := opts.Logger
	if logger == nil {
		logger = log.Default()
	}
	if opts.Store == nil || opts.Config == nil {
		return &AgentModelProber{
			interval: opts.Interval,
			logger:   logger,
		}
	}

	prober, err := agentmodels.NewProber(agentmodels.ProberOptions{
		Store:        opts.Store,
		Config:       opts.Config,
		Provider:     opts.Provider,
		ProbeTimeout: opts.ProbeTimeout,
		Now:          opts.Now,
		Logger:       logger,
	})
	if err != nil {
		logger.Printf("agent model prober init failed: %v", err)
		return &AgentModelProber{
			interval: opts.Interval,
			logger:   logger,
		}
	}

	return &AgentModelProber{
		prober:   prober,
		interval: opts.Interval,
		logger:   logger,
	}
}

// Enabled reports whether periodic background model probing is active.
func (p *AgentModelProber) Enabled() bool {
	return p != nil && p.prober != nil && p.interval > 0
}

// CanProbe reports whether on-demand ProbeOnce calls can execute.
func (p *AgentModelProber) CanProbe() bool {
	return p != nil && p.prober != nil
}

// Run performs an initial probe on startup and then repeats every Interval until ctx is cancelled.
func (p *AgentModelProber) Run(ctx context.Context) error {
	if !p.Enabled() {
		return nil
	}
	if err := p.ProbeOnce(ctx); err != nil && ctx.Err() == nil {
		p.logger.Printf("agent model prober initial probe error: %v", err)
	}

	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := p.ProbeOnce(ctx); err != nil && ctx.Err() == nil {
				p.logger.Printf("agent model prober error: %v", err)
			}
		}
	}
}

// ProbeOnce runs one full host × agent model catalog probe, serialized against concurrent runs.
func (p *AgentModelProber) ProbeOnce(ctx context.Context) error {
	if !p.CanProbe() {
		return fmt.Errorf("agent model prober is not available")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.prober.Probe(ctx)
}

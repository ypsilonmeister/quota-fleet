package runtime

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/adapter"
	"github.com/ypsilonmeister/quota-fleet/internal/agentver"
	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

// AgentVersionCheckerOptions configures the periodic and on-demand agent version checker.
type AgentVersionCheckerOptions struct {
	Store        *store.Store
	Config       *config.Config
	Provider     adapter.HostFileAccessProvider
	Notifier     Notifier
	Interval     time.Duration
	ProbeTimeout time.Duration
	Now          func() time.Time
	Logger       *log.Logger
}

// AgentVersionChecker runs periodic agent version probes and serves manual check triggers.
type AgentVersionChecker struct {
	checker  *agentver.Checker
	interval time.Duration
	logger   *log.Logger
	mu       sync.Mutex
}

// NewAgentVersionChecker constructs an AgentVersionChecker.
// When Config or Provider is nil, it returns a disabled checker whose CheckOnce returns an error.
func NewAgentVersionChecker(opts AgentVersionCheckerOptions) *AgentVersionChecker {
	logger := opts.Logger
	if logger == nil {
		logger = log.Default()
	}
	if opts.Store == nil || opts.Config == nil || opts.Provider == nil {
		return &AgentVersionChecker{
			interval: opts.Interval,
			logger:   logger,
		}
	}

	var onEvent agentver.EventHook
	if opts.Notifier != nil {
		notifier := opts.Notifier
		onEvent = func(ctx context.Context, ev *store.AgentVersionEvent) {
			if ev == nil {
				return
			}
			rep := Report{
				DedupKey:  fmt.Sprintf("agent-version:%s:%s:%s:%d", ev.EventType, ev.Agent, ev.Host, ev.ID),
				Kind:      "agent_" + string(ev.EventType),
				Message:   formatAgentEventNotification(ev),
				Timestamp: ev.CreatedAt,
			}
			if err := notifier.Report(ctx, rep); err != nil {
				logger.Printf("agent version notifier error (event #%d): %v", ev.ID, err)
			}
		}
	}

	checker, err := agentver.NewChecker(agentver.CheckerOptions{
		Store:        opts.Store,
		Config:       opts.Config,
		Provider:     opts.Provider,
		ProbeTimeout: opts.ProbeTimeout,
		Now:          opts.Now,
		OnEvent:      onEvent,
		Logger:       logger,
	})
	if err != nil {
		logger.Printf("agent version checker init failed: %v", err)
		return &AgentVersionChecker{
			interval: opts.Interval,
			logger:   logger,
		}
	}

	return &AgentVersionChecker{
		checker:  checker,
		interval: opts.Interval,
		logger:   logger,
	}
}

// Enabled reports whether periodic background version checking is active.
func (c *AgentVersionChecker) Enabled() bool {
	return c != nil && c.checker != nil && c.interval > 0
}

// CanCheck reports whether on-demand CheckOnce calls can execute probes.
func (c *AgentVersionChecker) CanCheck() bool {
	return c != nil && c.checker != nil
}

// Run periodically executes CheckOnce until ctx is cancelled.
func (c *AgentVersionChecker) Run(ctx context.Context) error {
	if !c.Enabled() {
		return nil
	}
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if _, err := c.CheckOnce(ctx); err != nil && ctx.Err() == nil {
				c.logger.Printf("agent version checker error: %v", err)
			}
		}
	}
}

// CheckOnce runs one full probe + drift detection pass, serialized against concurrent runs.
func (c *AgentVersionChecker) CheckOnce(ctx context.Context) (*agentver.CheckResult, error) {
	if !c.CanCheck() {
		return nil, fmt.Errorf("agent version checker is not available (adapter does not support host command execution)")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.checker.Check(ctx)
}

func formatAgentEventNotification(ev *store.AgentVersionEvent) string {
	if ev == nil {
		return ""
	}
	msg := ev.Detail
	if msg == "" {
		msg = fmt.Sprintf("[%s] agent=%s host=%s old=%s new=%s", ev.EventType, ev.Agent, ev.Host, ev.OldVersion, ev.NewVersion)
	}
	if ev.ManualUpdate && !containsManualNotice(msg) {
		msg += " (自動更新不可・手作業が必要)"
	}
	return msg
}

func containsManualNotice(s string) bool {
	return len(s) > 0 && (containsSubstr(s, "manual_update") || containsSubstr(s, "手作業"))
}

func containsSubstr(s, sub string) bool {
	if len(sub) == 0 || len(s) < len(sub) {
		return false
	}
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

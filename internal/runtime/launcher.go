package runtime

import (
	"context"
	"fmt"
	"log"

	"github.com/ypsilonmeister/quota-fleet/internal/adapter"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

// LauncherOptions configures a Launcher.
type LauncherOptions struct {
	Store      *store.Store
	Adapter    adapter.Adapter
	NotifyCh   <-chan struct{}
	MaxClaimed int
	Logger     *log.Logger
}

// Launcher waits for wakeup notifications, claims the oldest queued jobs up to MaxClaimed,
// prepares their workdir, and launches them through Adapter. It never polls or patrols.
type Launcher struct {
	store      *store.Store
	adapter    adapter.Adapter
	notifyCh   <-chan struct{}
	maxClaimed int
	logger     *log.Logger
}

// NewLauncher constructs a Launcher.
func NewLauncher(opts LauncherOptions) *Launcher {
	maxClaimed := opts.MaxClaimed
	if maxClaimed <= 0 {
		maxClaimed = 1
	}
	logger := opts.Logger
	if logger == nil {
		logger = log.Default()
	}
	return &Launcher{
		store:      opts.Store,
		adapter:    opts.Adapter,
		notifyCh:   opts.NotifyCh,
		maxClaimed: maxClaimed,
		logger:     logger,
	}
}

// Run blocks waiting on wakeup notifications until ctx is cancelled.
func (l *Launcher) Run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case _, ok := <-l.notifyCh:
			if !ok {
				return nil
			}
			l.DrainOnce(ctx)
		}
	}
}

// DrainOnce claims and launches queued jobs until either MaxClaimed is reached,
// no queued jobs remain, or ctx is cancelled. Returns the number of jobs launched.
func (l *Launcher) DrainOnce(ctx context.Context) int {
	launched := 0
	for {
		if ctx.Err() != nil {
			return launched
		}
		// Claim atomically persists the claim token in Store BEFORE launching.
		job, ok, err := l.store.Claim(ctx, l.maxClaimed)
		if err != nil {
			if ctx.Err() == nil {
				l.logger.Printf("launcher claim error: %v", err)
			}
			return launched
		}
		if !ok || job == nil {
			return launched
		}

		if err := l.adapter.PrepareWorkdir(ctx, job); err != nil {
			if ctx.Err() != nil {
				return launched
			}
			reason := fmt.Sprintf("prepare workdir failed: %v", err)
			if _, ferr := l.store.Fail(ctx, job.ID, job.ClaimedBy, reason); ferr != nil {
				l.logger.Printf("launcher fail job #%d after prepare error: %v", job.ID, ferr)
			} else {
				l.logger.Printf("launcher failed job #%d: %s", job.ID, reason)
			}
			continue
		}

		info, err := l.adapter.Launch(ctx, job)
		if err != nil {
			if ctx.Err() != nil {
				return launched
			}
			reason := fmt.Sprintf("launch failed: %v", err)
			if _, ferr := l.store.Fail(ctx, job.ID, job.ClaimedBy, reason); ferr != nil {
				l.logger.Printf("launcher fail job #%d after launch error: %v", job.ID, ferr)
			} else {
				l.logger.Printf("launcher failed job #%d: %s", job.ID, reason)
			}
			continue
		}

		launched++
		l.logger.Printf("launcher started job #%d (%q) pid=%d", job.ID, job.Title, info.PID)
	}
}

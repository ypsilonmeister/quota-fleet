package runtime

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/adapter"
	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/evaluator"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

// ShepherdOptions configures a Shepherd.
type ShepherdOptions struct {
	Store          *store.Store
	Adapter        adapter.Adapter
	Evaluator      evaluator.Evaluator
	Notifier       Notifier
	LauncherNotify chan<- struct{}
	Interval       time.Duration
	Logger         *log.Logger
}

// Shepherd periodically inspects claimed jobs, evaluates exited runs, transitions them
// to done or failed, delivers results via Notifier, and signals Launcher when slots open.
type Shepherd struct {
	store          *store.Store
	adapter        adapter.Adapter
	evaluator      evaluator.Evaluator
	notifier       Notifier
	launcherNotify chan<- struct{}
	interval       time.Duration
	logger         *log.Logger
}

// NewShepherd creates a new Shepherd.
func NewShepherd(opts ShepherdOptions) *Shepherd {
	interval := opts.Interval
	if interval <= 0 {
		interval = config.DefaultShepherdInterval
	}
	logger := opts.Logger
	if logger == nil {
		logger = log.Default()
	}
	return &Shepherd{
		store:          opts.Store,
		adapter:        opts.Adapter,
		evaluator:      opts.Evaluator,
		notifier:       opts.Notifier,
		launcherNotify: opts.LauncherNotify,
		interval:       interval,
		logger:         logger,
	}
}

// Run periodically executes SweepOnce until ctx is cancelled.
func (s *Shepherd) Run(ctx context.Context) error {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := s.SweepOnce(ctx); err != nil && ctx.Err() == nil {
				s.logger.Printf("shepherd sweep error: %v", err)
			}
		}
	}
}

// SweepOnce inspects active claimed jobs. When active Store counts are all zero,
// detailed inspection is skipped immediately (design §1.1).
func (s *Shepherd) SweepOnce(ctx context.Context) error {
	counts, hasUndel, err := checkActiveWork(ctx, s.store)
	if err != nil {
		return err
	}
	if !hasAnyActive(counts, hasUndel) {
		return nil
	}
	if counts[store.StatusClaimed] == 0 {
		return nil
	}

	claimedJobs, err := listAllByStatus(ctx, s.store, store.StatusClaimed)
	if err != nil {
		return fmt.Errorf("list claimed jobs: %w", err)
	}

	for _, job := range claimedJobs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.handleClaimedJob(ctx, job)
	}
	return nil
}

func (s *Shepherd) handleClaimedJob(ctx context.Context, job *store.Job) {
	state, err := s.adapter.Status(ctx, job)
	if err != nil {
		if ctx.Err() == nil {
			s.logger.Printf("shepherd status error for job #%d: %v", job.ID, err)
		}
		return
	}

	switch state.State {
	case adapter.StateRunning:
		return
	case adapter.StateMissing:
		// Missing claimed jobs are handled by Reconciler.
		return
	case adapter.StateExited:
		s.finishExitedJob(ctx, job, state)
	default:
		s.logger.Printf("shepherd unexpected state %q for job #%d", state.State, job.ID)
	}
}

func (s *Shepherd) finishExitedJob(ctx context.Context, job *store.Job, state adapter.RunState) {
	ev, err := s.adapter.CollectEvidence(ctx, job)
	if err != nil {
		if ctx.Err() == nil {
			s.logger.Printf("shepherd collect evidence error for job #%d: %v", job.ID, err)
		}
		return
	}
	ev.ExitCode = state.ExitCode

	evalRes, err := s.evaluator.Evaluate(ctx, job, ev)
	if err != nil {
		if ctx.Err() == nil {
			s.logger.Printf("shepherd evaluate error for job #%d: %v", job.ID, err)
		}
		return
	}

	resultText := evalRes.FormatText()

	var terminalJob *store.Job
	if evalRes.Passed {
		res, err := s.store.Complete(ctx, job.ID, job.ClaimedBy, resultText)
		if err != nil {
			if errors.Is(err, store.ErrConflict) || errors.Is(err, store.ErrTokenMismatch) {
				// Lost ownership; stop before delivery per docs/02-job-model.md.
				return
			}
			s.logger.Printf("shepherd complete job #%d error: %v", job.ID, err)
			return
		}
		terminalJob = res.Job
		s.logger.Printf("shepherd completed job #%d (promoted=%v)", job.ID, res.PromotedChildIDs)
	} else {
		reason := evalRes.FailSummary()
		res, err := s.store.FailWithResult(ctx, job.ID, job.ClaimedBy, reason, resultText)
		if err != nil {
			if errors.Is(err, store.ErrConflict) || errors.Is(err, store.ErrTokenMismatch) {
				return
			}
			s.logger.Printf("shepherd fail job #%d error: %v", job.ID, err)
			return
		}
		terminalJob = res.Job
		s.logger.Printf("shepherd failed job #%d (cancelled_children=%v)", job.ID, res.CancelledChildIDs)
	}

	// Free slot became available; wake Launcher for any already-queued jobs.
	s.notifyLauncher()

	// Deliver terminal job and mark delivered_at (Reconciler recovers if this fails).
	if err := deliverAndMark(ctx, s.store, s.notifier, terminalJob); err != nil && ctx.Err() == nil {
		s.logger.Printf("shepherd delivery error for job #%d: %v", terminalJob.ID, err)
	}
}

func (s *Shepherd) notifyLauncher() {
	if s.launcherNotify == nil {
		return
	}
	select {
	case s.launcherNotify <- struct{}{}:
	default:
	}
}

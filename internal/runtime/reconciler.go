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

// ReconcilerOptions configures a Reconciler.
type ReconcilerOptions struct {
	Store             *store.Store
	Adapter           adapter.Adapter
	Shepherd          *Shepherd
	Evaluator         evaluator.Evaluator
	LauncherNotify    chan<- struct{}
	MaxEvalRetries    int
	Notifier          Notifier
	Interval          time.Duration
	StaleClaimedAfter time.Duration
	AttemptsCap       int
	Now               func() time.Time
	Logger            *log.Logger
	// ChainDriverStaleAfter reports a Chain Driver whose last sweep is older than this
	// (3 × chains.sweep_interval, docs/22 §6). Zero disables the check.
	ChainDriverStaleAfter time.Duration
}

// Reconciler periodically checks for:
//
//	(a) undelivered done/failed jobs and delivers them via Notifier,
//	(b) claimed jobs older than StaleClaimedAfter:
//	    - if Adapter.Status is Exited, recovers completion without re-running the agent
//	      (collecting evidence, running acceptance evaluation, committing done/failed),
//	    - if Adapter.Status is Missing, resets them to queued (or cancels if chain ended),
//	(c) waiting jobs whose parent is already in a terminal state (reporting once per dedup key).
type Reconciler struct {
	store             *store.Store
	adapter           adapter.Adapter
	shepherd          *Shepherd
	notifier          Notifier
	interval          time.Duration
	staleClaimedAfter time.Duration
	attemptsCap       int
	now               func() time.Time
	logger            *log.Logger
	chainStaleAfter   time.Duration
}

// NewReconciler constructs a Reconciler.
func NewReconciler(opts ReconcilerOptions) *Reconciler {
	interval := opts.Interval
	if interval <= 0 {
		interval = config.DefaultReconcileInterval
	}
	staleAfter := opts.StaleClaimedAfter
	if staleAfter <= 0 {
		staleAfter = config.DefaultStaleClaimedAfter
	}
	nowFn := opts.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	logger := opts.Logger
	if logger == nil {
		logger = log.Default()
	}
	shepherd := opts.Shepherd
	if shepherd == nil && opts.Evaluator != nil {
		shepherd = NewShepherd(ShepherdOptions{
			Store:          opts.Store,
			Adapter:        opts.Adapter,
			Evaluator:      opts.Evaluator,
			Notifier:       opts.Notifier,
			LauncherNotify: opts.LauncherNotify,
			MaxEvalRetries: opts.MaxEvalRetries,
			Logger:         logger,
		})
	}
	return &Reconciler{
		store:             opts.Store,
		adapter:           opts.Adapter,
		shepherd:          shepherd,
		notifier:          opts.Notifier,
		interval:          interval,
		staleClaimedAfter: staleAfter,
		attemptsCap:       opts.AttemptsCap,
		now:               nowFn,
		logger:            logger,
		chainStaleAfter:   opts.ChainDriverStaleAfter,
	}
}

// Run periodically executes ReconcileOnce until ctx is cancelled.
func (r *Reconciler) Run(ctx context.Context) error {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := r.ReconcileOnce(ctx); err != nil && ctx.Err() == nil {
				r.logger.Printf("reconciler error: %v", err)
			}
		}
	}
}

// ReconcileOnce performs one reconciliation pass. When all active Store counts
// (claimed, queued, waiting, undelivered done/failed) are zero, detailed checks are skipped.
func (r *Reconciler) ReconcileOnce(ctx context.Context) error {
	r.checkChainDriver(ctx)

	counts, hasUndel, err := checkActiveWork(ctx, r.store)
	if err != nil {
		return err
	}
	if !hasAnyActive(counts, hasUndel) {
		return nil
	}

	if hasUndel {
		if err := r.reconcileUndelivered(ctx); err != nil {
			return err
		}
	}
	if counts[store.StatusClaimed] > 0 {
		if err := r.reconcileStaleClaimed(ctx); err != nil {
			return err
		}
	}
	if counts[store.StatusWaiting] > 0 {
		if err := r.reconcileOrphanWaiting(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (r *Reconciler) reconcileUndelivered(ctx context.Context) error {
	jobs, err := r.store.ListUndelivered(ctx, store.MaxListLimit)
	if err != nil {
		return fmt.Errorf("list undelivered: %w", err)
	}
	for _, job := range jobs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := deliverAndMark(ctx, r.store, r.notifier, job); err != nil && ctx.Err() == nil {
			r.logger.Printf("reconciler deliver job #%d error: %v", job.ID, err)
		}
	}
	return nil
}

func (r *Reconciler) reconcileStaleClaimed(ctx context.Context) error {
	claimedJobs, err := listAllByStatus(ctx, r.store, store.StatusClaimed)
	if err != nil {
		return fmt.Errorf("list claimed jobs: %w", err)
	}
	now := r.now().UTC()

	for _, job := range claimedJobs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if job.ClaimedAt == nil {
			continue
		}
		if now.Sub(*job.ClaimedAt) < r.staleClaimedAfter {
			continue
		}

		state, err := r.adapter.Status(ctx, job)
		if err != nil {
			if ctx.Err() == nil {
				r.logger.Printf("reconciler status check error for job #%d: %v", job.ID, err)
			}
			continue
		}
		if state.State == adapter.StateExited {
			if r.shepherd == nil {
				r.logger.Printf("reconciler observed exited claimed job #%d (%s) but no evaluator/shepherd is configured; skipping recovery", job.ID, state.Detail)
				continue
			}
			terminalJob := r.shepherd.finishExitedJobFrom(ctx, job, state, "reconciler")
			if terminalJob == nil {
				continue
			}
			r.logger.Printf("reconciler recovered exited claimed job #%d -> %s (exit_code=%d, exit_code_unknown=%v, detail=%s)",
				job.ID, terminalJob.Status, state.ExitCode, state.ExitCodeUnknown, state.Detail)
			dedupKey := fmt.Sprintf("stale-claimed-recovered:%d:%s", job.ID, job.ClaimedBy)
			r.reportOnce(ctx, Report{
				DedupKey:  dedupKey,
				Kind:      "stale_claimed_recovered",
				JobID:     job.ID,
				Message:   fmt.Sprintf("job #%d (%q) remained claimed after exit (%s); recovered without relaunch to %s", job.ID, job.Title, state.Detail, terminalJob.Status),
				Timestamp: now,
			})
			continue
		}
		if state.State != adapter.StateMissing {
			continue
		}

		if r.attemptsCap > 0 && job.Attempts >= r.attemptsCap {
			dedupKey := fmt.Sprintf("stale-claimed-attempts-cap:%d:%d", job.ID, job.Attempts)
			r.reportOnce(ctx, Report{
				DedupKey:  dedupKey,
				Kind:      "stale_claimed_attempts_cap",
				JobID:     job.ID,
				Message:   fmt.Sprintf("job #%d (%q) missing on worker and reached attempts_cap (%d/%d); skipping auto-reset", job.ID, job.Title, job.Attempts, r.attemptsCap),
				Timestamp: now,
			})
			continue
		}

		reset, err := r.store.Reset(ctx, job.ID)
		if err != nil {
			if errors.Is(err, store.ErrConflict) {
				continue
			}
			r.logger.Printf("reconciler reset job #%d error: %v", job.ID, err)
			continue
		}
		if reset != nil && reset.Status == store.StatusCancelled {
			// A step of a chain run that is no longer running: Reset cancelled it instead.
			r.logger.Printf("reconciler cancelled missing claimed job #%d: %s", job.ID, reset.LastError)
			r.reportOnce(ctx, Report{
				DedupKey:  fmt.Sprintf("stale-claimed-chain-cancelled:%d", job.ID),
				Kind:      "stale_claimed_cancelled",
				JobID:     job.ID,
				Message:   fmt.Sprintf("job #%d (%q) missing on worker after %s; %s", job.ID, job.Title, r.staleClaimedAfter, reset.LastError),
				Timestamp: now,
			})
			continue
		}
		r.logger.Printf("reconciler reset missing claimed job #%d back to queued (attempts=%d)", job.ID, job.Attempts)
		dedupKey := fmt.Sprintf("stale-claimed-reset:%d:%d", job.ID, job.Attempts)
		r.reportOnce(ctx, Report{
			DedupKey:  dedupKey,
			Kind:      "stale_claimed_reset",
			JobID:     job.ID,
			Message:   fmt.Sprintf("job #%d (%q) missing on worker after %s; reset to queued (attempts=%d)", job.ID, job.Title, r.staleClaimedAfter, job.Attempts),
			Timestamp: now,
		})
	}
	return nil
}

func (r *Reconciler) reconcileOrphanWaiting(ctx context.Context) error {
	waitingJobs, err := listAllByStatus(ctx, r.store, store.StatusWaiting)
	if err != nil {
		return fmt.Errorf("list waiting jobs: %w", err)
	}
	now := r.now().UTC()

	for _, job := range waitingJobs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if job.ParentID == nil {
			continue
		}
		parent, err := r.store.GetJob(ctx, *job.ParentID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				dedupKey := fmt.Sprintf("orphan-waiting:%d:parent-%d-missing", job.ID, *job.ParentID)
				r.reportOnce(ctx, Report{
					DedupKey:  dedupKey,
					Kind:      "orphan_waiting",
					JobID:     job.ID,
					ParentID:  job.ParentID,
					Message:   fmt.Sprintf("waiting job #%d (%q) references missing parent #%d", job.ID, job.Title, *job.ParentID),
					Timestamp: now,
				})
				continue
			}
			r.logger.Printf("reconciler get parent #%d for waiting job #%d error: %v", *job.ParentID, job.ID, err)
			continue
		}

		if parent.Status == store.StatusDone || parent.Status == store.StatusFailed || parent.Status == store.StatusCancelled {
			dedupKey := fmt.Sprintf("orphan-waiting:%d:parent-%d-%s", job.ID, parent.ID, parent.Status)
			r.reportOnce(ctx, Report{
				DedupKey:  dedupKey,
				Kind:      "orphan_waiting",
				JobID:     job.ID,
				ParentID:  job.ParentID,
				Message:   fmt.Sprintf("waiting job #%d (%q) has terminal parent #%d in status %s", job.ID, job.Title, parent.ID, parent.Status),
				Timestamp: now,
			})
		}
	}
	return nil
}

func (r *Reconciler) reportOnce(ctx context.Context, rep Report) {
	isNew, err := r.store.RecordReconcilerDedup(ctx, rep.DedupKey)
	if err != nil {
		if ctx.Err() == nil {
			r.logger.Printf("reconciler dedup record error for %q: %v", rep.DedupKey, err)
		}
		return
	}
	if !isNew {
		return
	}
	if r.notifier != nil {
		if err := r.notifier.Report(ctx, rep); err != nil && ctx.Err() == nil {
			r.logger.Printf("reconciler report error for %q: %v", rep.DedupKey, err)
		}
	}
}

// checkChainDriver reports, once per last-sweep value, a Chain Driver whose last sweep is
// older than chainStaleAfter while chain runs are running (docs/22 §6).
func (r *Reconciler) checkChainDriver(ctx context.Context) {
	if r.chainStaleAfter <= 0 {
		return
	}
	counts, err := r.store.CountChainRunsByStatus(ctx)
	if err != nil || counts[store.ChainRunRunning] == 0 {
		return
	}
	now := r.now().UTC()
	last := "never"
	raw, err := r.store.GetMeta(ctx, store.MetaKeyChainDriverLastSweep)
	if err == nil {
		t, perr := time.Parse(time.RFC3339Nano, raw)
		if perr == nil && now.Sub(t) <= r.chainStaleAfter {
			return
		}
		last = raw
	} else if !errors.Is(err, store.ErrNotFound) {
		return
	}
	r.reportOnce(ctx, Report{
		DedupKey:  "chain-driver-stale:" + last,
		Kind:      "chain_driver_stale",
		Message:   fmt.Sprintf("chain driver has not swept since %s (threshold %s) while %d chain runs are running", last, r.chainStaleAfter, counts[store.ChainRunRunning]),
		Timestamp: now,
	})
}

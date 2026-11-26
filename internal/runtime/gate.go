package runtime

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/quota"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

// GateCheckerOptions configures the resident Quota Gate release loop and manual release helper.
type GateCheckerOptions struct {
	Store      *store.Store
	GateMode   string
	UsageFile  string
	StaleAfter time.Duration
	Pools      map[string]config.PoolConfig
	Interval   time.Duration
	Now        func() time.Time
	Logger     *log.Logger

	// SnapshotLoader optionally overrides loading usage snapshots from UsageFile (useful in tests).
	SnapshotLoader func() (*quota.Snapshot, error)
}

// ReleaseResult describes the outcome of evaluating and attempting to release a single held job.
type ReleaseResult struct {
	Held         *store.HeldJob
	Job          *store.Job
	Created      bool
	Released     bool
	SelectedPool string
	Evaluation   quota.GateEvaluation
	Reason       string
}

// GateChecker periodically re-evaluates held_jobs when gate_mode=enforce and releases any
// held job whose candidate pools (pool + alt_pools) have a healthy pool, selecting the one
// with the soonest weekly reset.
type GateChecker struct {
	store          *store.Store
	gateMode       string
	usageFile      string
	staleAfter     time.Duration
	pools          map[string]config.PoolConfig
	interval       time.Duration
	now            func() time.Time
	logger         *log.Logger
	snapshotLoader func() (*quota.Snapshot, error)
}

// NewGateChecker constructs a GateChecker.
func NewGateChecker(opts GateCheckerOptions) *GateChecker {
	mode := strings.TrimSpace(opts.GateMode)
	if mode == "" {
		mode = config.GateModeOff
	}
	staleAfter := opts.StaleAfter
	if staleAfter <= 0 {
		staleAfter = config.DefaultQuotaStaleAfter
	}
	interval := opts.Interval
	if interval <= 0 {
		interval = config.DefaultQuotaGateCheckInterval
	}
	nowFn := opts.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	logger := opts.Logger
	if logger == nil {
		logger = log.Default()
	}
	return &GateChecker{
		store:          opts.Store,
		gateMode:       mode,
		usageFile:      strings.TrimSpace(opts.UsageFile),
		staleAfter:     staleAfter,
		pools:          opts.Pools,
		interval:       interval,
		now:            nowFn,
		logger:         logger,
		snapshotLoader: opts.SnapshotLoader,
	}
}

// Enabled reports whether the periodic Gate release loop should run (only in enforce mode).
func (g *GateChecker) Enabled() bool {
	return g != nil && g.gateMode == config.GateModeEnforce
}

// Run periodically executes CheckOnce until ctx is cancelled.
func (g *GateChecker) Run(ctx context.Context) error {
	if !g.Enabled() {
		return nil
	}
	ticker := time.NewTicker(g.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := g.CheckOnce(ctx); err != nil && ctx.Err() == nil {
				g.logger.Printf("gate checker error: %v", err)
			}
		}
	}
}

// CheckOnce scans active held_jobs and releases any whose candidate pools are now healthy.
// When no held jobs exist, it returns immediately without reading the usage snapshot file.
func (g *GateChecker) CheckOnce(ctx context.Context) error {
	if !g.Enabled() {
		return nil
	}
	// docs/22 §5.1 (F7'): walk every page so an exhausted hold at the head of the list
	// cannot hide the holds behind it. The snapshot is loaded once, lazily.
	var (
		snap     *quota.Snapshot
		snapErr  error
		snapRead bool
		cursor   string
	)
	for {
		page, err := g.store.ListHeldJobsPage(ctx, store.ListHeldJobsFilter{
			Status: store.HeldStatusHeld,
			Cursor: cursor,
			Limit:  store.MaxListLimit,
		})
		if err != nil {
			return fmt.Errorf("list held jobs: %w", err)
		}
		if len(page.Held) > 0 && !snapRead {
			snap, snapErr = g.loadSnapshot()
			snapRead = true
		}
		for _, held := range page.Held {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			_, _ = g.tryReleaseHeld(ctx, held, snap, snapErr)
		}
		if !page.HasMore || page.NextCursor == "" {
			return nil
		}
		cursor = page.NextCursor
	}
}

// ReleaseByID manually re-evaluates and attempts to release a single held job by ID.
// If the candidate pools are not healthy, it returns Released=false with a descriptive Reason
// and leaves the held entry untouched. If job creation fails, last_error is recorded on the
// held entry and the error is returned.
func (g *GateChecker) ReleaseByID(ctx context.Context, heldID string) (*ReleaseResult, error) {
	held, err := g.store.GetHeldJob(ctx, heldID)
	if err != nil {
		return nil, err
	}
	if held.Status != store.HeldStatusHeld {
		return &ReleaseResult{
			Held:     held,
			Released: false,
			Reason:   fmt.Sprintf("held job %s is %s (expected held)", held.ID, held.Status),
		}, fmt.Errorf("%w: held job %s is %s", store.ErrConflict, held.ID, held.Status)
	}

	var (
		snap    *quota.Snapshot
		snapErr error
	)
	if !g.isUnmeasured(held.Pool) {
		snap, snapErr = g.loadSnapshot()
	}
	return g.tryReleaseHeld(ctx, held, snap, snapErr)
}

func (g *GateChecker) tryReleaseHeld(
	ctx context.Context,
	held *store.HeldJob,
	snap *quota.Snapshot,
	snapErr error,
) (*ReleaseResult, error) {
	eval := quota.EvaluateGate(held.Pool, held.AltPools, snap, quota.EvalOptions{
		Now:        g.now(),
		StaleAfter: g.staleAfter,
		Pools:      g.pools,
	})
	if snapErr != nil && !eval.Bypassed {
		eval.Decision = quota.DecisionUnknown
		eval.Reason = fmt.Sprintf("%s: %v", quota.ReasonSnapshotError, snapErr)
	}

	if eval.Decision != quota.DecisionWouldPass || eval.SelectedPool == "" {
		return &ReleaseResult{
			Held:       held,
			Released:   false,
			Evaluation: eval,
			Reason:     formatNotReleasableReason(eval),
		}, nil
	}

	// docs/22 §5.1: create the job, mark the hold released, link a chain step and record
	// the gate decision in one transaction. A cancel that committed first makes this a
	// no-op (ErrConflict); a hold whose chain run is over is cancelled instead.
	out, err := g.store.ReleaseHeld(ctx, store.ReleaseHeldParams{
		HeldID:       held.ID,
		SelectedPool: eval.SelectedPool,
		Gate: &store.GateDecision{
			Mode:          config.GateModeEnforce,
			RequestedPool: eval.RequestedPool,
			AltPools:      eval.AltPools,
			Decision:      string(eval.Decision),
			BlockedBy:     eval.BlockedBy,
			SelectedPool:  eval.SelectedPool,
			Bypassed:      eval.Bypassed,
			Reason:        eval.Reason,
			MeasuredAt:    eval.MeasuredAt,
			DecidedAt:     eval.DecidedAt,
		},
	})
	if err != nil {
		res := &ReleaseResult{
			Held:         held,
			Released:     false,
			SelectedPool: eval.SelectedPool,
			Evaluation:   eval,
			Reason:       err.Error(),
		}
		if out != nil && out.Held != nil {
			res.Held = out.Held
		}
		if errors.Is(err, store.ErrConflict) || errors.Is(err, store.ErrNotFound) {
			return res, err
		}
		// Transient (DB) error: nothing was written. Outside a chain keep recording it on
		// the hold as before; a chain hold's last_error means "permanent" (R9), so only log.
		if held.ChainRunID == "" {
			updatedHeld, isNew, recErr := g.store.RecordHeldReleaseError(ctx, held.ID, err.Error())
			if recErr != nil {
				g.logger.Printf("WARNING: failed to record release error on held %s: %v", held.ID, recErr)
			} else {
				res.Held = updatedHeld
				if isNew {
					g.logger.Printf("WARNING: gate release failed for held %s (selected_pool=%s): %v", held.ID, eval.SelectedPool, err)
				}
			}
		} else {
			g.logger.Printf("WARNING: gate release of chain held %s failed (will retry): %v", held.ID, err)
		}
		return res, err
	}
	if out.CancelledForChain {
		return &ReleaseResult{
			Held:         out.Held,
			Released:     false,
			SelectedPool: eval.SelectedPool,
			Evaluation:   eval,
			Reason:       fmt.Sprintf("held job %s was cancelled because its chain run is no longer running (%s)", held.ID, out.Held.CancelReason),
		}, fmt.Errorf("%w: held job %s belongs to a chain run that is no longer running", store.ErrConflict, held.ID)
	}
	if out.PermanentErr != nil {
		g.logger.Printf("WARNING: gate release failed permanently for held %s (selected_pool=%s): %v", held.ID, eval.SelectedPool, out.PermanentErr)
		return &ReleaseResult{
			Held:         out.Held,
			Released:     false,
			SelectedPool: eval.SelectedPool,
			Evaluation:   eval,
			Reason:       out.PermanentErr.Error(),
		}, out.PermanentErr
	}
	return &ReleaseResult{
		Held:         out.Held,
		Job:          out.Job,
		Created:      out.Created,
		Released:     true,
		SelectedPool: eval.SelectedPool,
		Evaluation:   eval,
	}, nil
}

func (g *GateChecker) loadSnapshot() (*quota.Snapshot, error) {
	if g.snapshotLoader != nil {
		return g.snapshotLoader()
	}
	return quota.LoadUsageFile(g.usageFile)
}

func (g *GateChecker) isUnmeasured(pool string) bool {
	if pool == "" || g.pools == nil {
		return false
	}
	cfg, ok := g.pools[strings.TrimSpace(pool)]
	return ok && cfg.Unmeasured
}

func formatNotReleasableReason(eval quota.GateEvaluation) string {
	switch eval.Decision {
	case quota.DecisionWouldHold:
		if eval.BlockedBy != "" {
			return fmt.Sprintf("all candidate pools exhausted (blocked_by=%s)", eval.BlockedBy)
		}
		return "all candidate pools exhausted"
	case quota.DecisionUnknown:
		if eval.Reason != "" {
			return fmt.Sprintf("pool status unknown (%s)", eval.Reason)
		}
		return "pool status unknown"
	default:
		return fmt.Sprintf("not releasable (decision=%s)", eval.Decision)
	}
}

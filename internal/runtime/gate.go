package runtime

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/quota"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

// GateCheckerOptions configures the resident Quota Gate release loop and manual release helper.
type GateCheckerOptions struct {
	LauncherNotify chan<- struct{}
	Store          *store.Store
	GateMode       string
	UsageFile      string
	UsageFormat    string
	Pace           config.PaceConfig
	StaleAfter     time.Duration
	Pools          map[string]config.PoolConfig
	Interval       time.Duration
	Now            func() time.Time
	Logger         *log.Logger

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
	launcherNotify chan<- struct{}
	store          *store.Store
	gateMode       string
	usageFile      string
	usageFormat    string
	pace           config.PaceConfig
	staleAfter     time.Duration
	pools          map[string]config.PoolConfig
	interval       time.Duration
	now            func() time.Time
	logger         *log.Logger
	snapshotLoader func() (*quota.Snapshot, error)

	warnMu   sync.Mutex
	lastWarn map[string]string
}

// NewGateChecker constructs a GateChecker.
func NewGateChecker(opts GateCheckerOptions) *GateChecker {
	mode := strings.TrimSpace(opts.GateMode)
	if mode == "" {
		mode = config.GateModeOff
	}
	usageFormat := strings.TrimSpace(opts.UsageFormat)
	if usageFormat == "" {
		usageFormat = config.UsageFormatLegacy
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
		launcherNotify: opts.LauncherNotify,
		store:          opts.Store,
		gateMode:       mode,
		usageFile:      strings.TrimSpace(opts.UsageFile),
		usageFormat:    usageFormat,
		pace:           opts.Pace,
		staleAfter:     staleAfter,
		pools:          opts.Pools,
		interval:       interval,
		now:            nowFn,
		logger:         logger,
		snapshotLoader: opts.SnapshotLoader,
		lastWarn:       make(map[string]string),
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
// With cooldown enabled it also checks for snapshot recovery and wakes the launcher.
func (g *GateChecker) CheckOnce(ctx context.Context) error {
	if !g.Enabled() {
		return nil
	}
	var (
		snap         *quota.Snapshot
		snapErr      error
		snapRead     bool
		cursor       string
		paceLoad     map[string]int
		paceLoadRead bool
		seenIDs      = make(map[string]struct{})
	)
	if g.store.CooldownsEnabled() {
		if g.usageFile != "" || g.snapshotLoader != nil {
			snap, snapErr = g.loadSnapshot()
			snapRead = true
		}
		opts := quota.EvalOptions{Now: g.now(), Pools: g.pools, StaleAfter: g.staleAfter, Format: g.usageFormat}
		if snapErr != nil {
			snap = nil
		}
		if err := g.store.LoadCooldowns(ctx, snap, &opts); err != nil {
			return err
		}
		defer g.NotifyLauncher()
	}

	for {
		page, err := g.store.ListHeldJobsPage(ctx, store.ListHeldJobsFilter{
			Status: store.HeldStatusHeld,
			Cursor: cursor,
			Limit:  store.MaxListLimit,
		})
		if err != nil {
			return fmt.Errorf("list held jobs: %w", err)
		}
		for _, held := range page.Held {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			seenIDs[held.ID] = struct{}{}
			var (
				heldSnap    *quota.Snapshot
				heldSnapErr error
			)
			// Periodic snapshot skip is scoped to Schema v1 (§4.2.C); Legacy periodic sweeps
			// always load the snapshot once when any held job exists so unmeasured-primary
			// releases keep recording gate_decisions.measured_at and snapshot-error bypass warnings.
			skipSnap := g.usageFormat == config.UsageFormatV1 && g.shouldSkipSnapshotForHeld(held)
			if !skipSnap {
				if !snapRead {
					snap, snapErr = g.loadSnapshot()
					snapRead = true
				}
				heldSnap, heldSnapErr = snap, snapErr
			}
			if !paceLoadRead && g.pace.ShadowEnabled() && heldSnap != nil && heldSnapErr == nil {
				paceLoad = g.readPaceLoad(ctx)
				paceLoadRead = true
			}
			res, _ := g.tryReleaseHeld(ctx, held, heldSnap, heldSnapErr, true, paceLoad)
			if res != nil && res.Released {
				g.clearWarn(held.ID)
			}
		}
		if !page.HasMore || page.NextCursor == "" {
			g.pruneWarns(seenIDs)
			return nil
		}
		cursor = page.NextCursor
	}
}

// ReleaseByID manually re-evaluates and attempts to release a single held job by ID.
// It uses the exact same tryReleaseHeld logic as periodic CheckOnce (§4.2.C).
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
	if !g.shouldSkipSnapshotForHeld(held) {
		snap, snapErr = g.loadSnapshot()
	}
	var paceLoad map[string]int
	if g.pace.ShadowEnabled() && snap != nil && snapErr == nil {
		paceLoad = g.readPaceLoad(ctx)
	}
	res, relErr := g.tryReleaseHeld(ctx, held, snap, snapErr, false, paceLoad)
	if res != nil && res.Released {
		g.clearWarn(held.ID)
	}
	return res, relErr
}

func (g *GateChecker) shouldSkipSnapshotForHeld(held *store.HeldJob) bool {
	if held == nil {
		return true
	}
	primary := strings.TrimSpace(held.Pool)
	if g.store.CooldownsEnabled() && g.isUnmeasured(primary) {
		if _, enabled := g.store.CooldownPolicy(primary); enabled {
			for _, alt := range held.AltPools {
				if p, ok := g.pools[strings.TrimSpace(alt)]; ok && !p.Unmeasured {
					return false
				}
			}
		}
	}
	if g.usageFormat != config.UsageFormatV1 {
		return g.isUnmeasured(primary)
	}
	// v1 (§4.2.C): skip file read if empty candidates, primary defined & unmeasured,
	// primary empty & all alts defined & unmeasured, or all stored candidates deleted from config.
	if primary == "" && len(held.AltPools) == 0 {
		return true
	}
	if primary != "" && g.isUnmeasured(primary) {
		return true
	}
	if primary == "" {
		allUnmeasured := true
		anyAlt := false
		for _, a := range held.AltPools {
			p := strings.TrimSpace(a)
			if p == "" {
				continue
			}
			anyAlt = true
			if !g.isUnmeasured(p) {
				allUnmeasured = false
				break
			}
		}
		if anyAlt && allUnmeasured {
			return true
		}
	}
	// Check if all stored candidates were deleted from g.pools.
	anyDefined := false
	if primary != "" {
		if _, ok := g.pools[primary]; ok {
			anyDefined = true
		}
	}
	if !anyDefined {
		for _, a := range held.AltPools {
			if p := strings.TrimSpace(a); p != "" {
				if _, ok := g.pools[p]; ok {
					anyDefined = true
					break
				}
			}
		}
	}
	return !anyDefined
}

func (g *GateChecker) readPaceLoad(ctx context.Context) map[string]int {
	load, err := g.store.ActivePoolLoads(ctx)
	if err != nil {
		g.logger.Printf("gate release pace shadow load: %v", err)
	}
	return load
}

func (g *GateChecker) tryReleaseHeld(
	ctx context.Context,
	held *store.HeldJob,
	snap *quota.Snapshot,
	snapErr error,
	periodic bool,
	paceLoad map[string]int,
) (*ReleaseResult, error) {
	opts := quota.EvalOptions{
		Now:             g.now(),
		StaleAfter:      g.staleAfter,
		Pools:           g.pools,
		Format:          g.usageFormat,
		CheckPoolConfig: g.usageFormat == config.UsageFormatV1,
		HeldRelease:     g.usageFormat == config.UsageFormatV1,
	}
	if snapErr != nil {
		snap = nil
	}
	if err := g.store.LoadCooldowns(ctx, snap, &opts); err != nil {
		return nil, err
	}

	eval := quota.EvaluateGate(held.Pool, held.AltPools, snap, opts)
	if snapErr != nil && eval.BlockedBy != "cooldown" {
		if !eval.Bypassed {
			if g.usageFormat == config.UsageFormatV1 {
				if eval.Reason != quota.ReasonPoolNotConfigured && eval.Reason != quota.ReasonNoCandidatePools {
					eval.Decision = quota.DecisionUnknown
					eval.Reason = quota.ReasonSnapshotError
					eval.Detail = snapErr.Error()
				}
			} else {
				eval.Decision = quota.DecisionUnknown
				eval.Reason = fmt.Sprintf("%s: %v", quota.ReasonSnapshotError, snapErr)
			}
		} else if g.logger != nil {
			// Legacy compatibility (Item A): unmeasured alt still releases, while logging snapshot error.
			g.logHeldWarnOnce(held.ID, periodic, fmt.Sprintf("bypass_snap_err:%v", snapErr),
				fmt.Sprintf("WARNING: gate release for held %s bypassed to %s despite usage snapshot error: %v", held.ID, eval.SelectedPool, snapErr))
		}
	}

	if eval.Decision != quota.DecisionWouldPass || eval.SelectedPool == "" {
		reason := formatNotReleasableReason(eval, g.staleAfter)
		if eval.Decision == quota.DecisionUnknown {
			g.logHeldWarnOnce(held.ID, periodic, reason,
				fmt.Sprintf("WARNING: gate release deferred for held %s (format=%s pool=%s alt=%v): %s",
					held.ID, g.usageFormat, held.Pool, held.AltPools, reason))
		}
		return &ReleaseResult{
			Held:       held,
			Released:   false,
			Evaluation: eval,
			Reason:     reason,
		}, nil
	}

	if len(eval.UndefinedPools) > 0 && g.logger != nil {
		g.logger.Printf("WARNING: gate release for held %s selected %s while removed pool(s) %v are no longer in fleet.yaml",
			held.ID, eval.SelectedPool, eval.UndefinedPools)
	}

	var shadowPool, comparison string
	if paceLoad != nil && !eval.Bypassed {
		ranking := quota.RankHealthy(eval.Pools, opts, g.pace, paceLoad)
		if ranking.Index >= 0 {
			shadowPool = eval.Pools[ranking.Index].Pool
			comparison = ranking.Describe(eval.SelectedPool, shadowPool)
			if eval.Reason == "" {
				eval.Reason = comparison
			} else {
				eval.Reason = quota.FormatDecisionReason(eval) + "; " + comparison
				eval.Detail = ""
			}
		}
	}
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
			Reason:        quota.FormatDecisionReason(eval),
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
		if errors.Is(err, store.ErrCooldownBlocked) {
			return res, nil
		}
		if errors.Is(err, store.ErrConflict) || errors.Is(err, store.ErrNotFound) {
			return res, err
		}
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
	if out.Created && shadowPool != "" {
		paceLoad[shadowPool]++
		if shadowPool != eval.SelectedPool {
			g.logger.Printf("INFO: gate release held=%s pool=%s alt=%v: %s", held.ID, eval.RequestedPool, eval.AltPools, comparison)
		}
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

func (g *GateChecker) logHeldWarnOnce(heldID string, periodic bool, key, msg string) {
	if g.logger == nil {
		return
	}
	if !periodic {
		g.logger.Print(msg)
		return
	}
	g.warnMu.Lock()
	prev := g.lastWarn[heldID]
	if prev == key {
		g.warnMu.Unlock()
		return
	}
	g.lastWarn[heldID] = key
	g.warnMu.Unlock()
	g.logger.Print(msg)
}

func (g *GateChecker) clearWarn(heldID string) {
	g.warnMu.Lock()
	delete(g.lastWarn, heldID)
	g.warnMu.Unlock()
}

func (g *GateChecker) pruneWarns(active map[string]struct{}) {
	g.warnMu.Lock()
	for id := range g.lastWarn {
		if _, ok := active[id]; !ok {
			delete(g.lastWarn, id)
		}
	}
	g.warnMu.Unlock()
}

func (g *GateChecker) loadSnapshot() (*quota.Snapshot, error) {
	if g.snapshotLoader != nil {
		return g.snapshotLoader()
	}
	return quota.LoadUsageFileWithFormat(g.usageFile, g.usageFormat)
}

func (g *GateChecker) isUnmeasured(pool string) bool {
	if pool == "" || g.pools == nil {
		return false
	}
	cfg, ok := g.pools[strings.TrimSpace(pool)]
	return ok && cfg.Unmeasured
}

func formatNotReleasableReason(eval quota.GateEvaluation, staleAfter time.Duration) string {
	switch eval.Decision {
	case quota.DecisionWouldHold:
		if eval.BlockedBy == "cooldown" {
			for _, p := range eval.Pools {
				if p.Status == quota.PoolCooldown {
					return fmt.Sprintf("pool %s is in cooldown until %s (remaining %s); clear cooldown to force release", p.Pool, p.Cooldown.ActiveUntil.Format(time.RFC3339Nano), p.Cooldown.ActiveUntil.Sub(eval.DecidedAt).Round(time.Second))
				}
			}
		}
		if eval.BlockedBy != "" {
			return fmt.Sprintf("all candidate pools exhausted (blocked_by=%s)", eval.BlockedBy)
		}
		return "all candidate pools exhausted"
	case quota.DecisionUnknown:
		if eval.Reason == quota.ReasonStale && eval.AgeSeconds != nil {
			return fmt.Sprintf("pool status unknown (%s: %s)", eval.Reason,
				quota.FormatStaleUnarrivedMessage(time.Duration(*eval.AgeSeconds)*time.Second, staleAfter))
		}
		if r := quota.FormatDecisionReason(eval); r != "" {
			return fmt.Sprintf("pool status unknown (%s)", r)
		}
		return "pool status unknown"
	default:
		return fmt.Sprintf("not releasable (decision=%s)", eval.Decision)
	}
}

func (g *GateChecker) NotifyLauncher() {
	select {
	case g.launcherNotify <- struct{}{}:
	default:
	}
}

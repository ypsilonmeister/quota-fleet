package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/admission"
	"github.com/ypsilonmeister/quota-fleet/internal/chain"
	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

const (
	defaultChainStallAfter     = 24 * time.Hour
	maxChainRetryDelay         = 30 * time.Minute
	maxChainNotificationDelay  = time.Hour
	chainSweepPageSize         = store.MaxListLimit
	defaultChainNotifyBaseWait = 30 * time.Second
)

// ChainDriverOptions configures a ChainDriver.
type ChainDriverOptions struct {
	Store          *store.Store
	Admission      *admission.Admission
	Notifier       Notifier
	LauncherNotify chan<- struct{}
	// Interval is chains.sweep_interval (default: the Shepherd interval).
	Interval time.Duration
	// StallAfter is chains.stall_after (default 24h). Stalls are reported, not stopped.
	StallAfter time.Duration
	Now        func() time.Time
	Logger     *log.Logger
}

// ChainDriver advances running chain runs from a periodic sweep (docs/22 §6). It is
// deterministic code: it reads DB rows only (never worker files), decides with
// chain.Decide, and writes each transition with Store.AdvanceChain in one transaction.
type ChainDriver struct {
	store          *store.Store
	adm            *admission.Admission
	notifier       Notifier
	launcherNotify chan<- struct{}
	interval       time.Duration
	stallAfter     time.Duration
	now            func() time.Time
	logger         *log.Logger

	// Test hooks.
	beforeAdvance func(runID string)
	afterSend     func(n *store.ChainNotification) bool
}

// NewChainDriver constructs a ChainDriver.
func NewChainDriver(opts ChainDriverOptions) *ChainDriver {
	interval := opts.Interval
	if interval <= 0 {
		interval = config.DefaultShepherdInterval
	}
	stall := opts.StallAfter
	if stall <= 0 {
		stall = defaultChainStallAfter
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	logger := opts.Logger
	if logger == nil {
		logger = log.Default()
	}
	return &ChainDriver{
		store:          opts.Store,
		adm:            opts.Admission,
		notifier:       opts.Notifier,
		launcherNotify: opts.LauncherNotify,
		interval:       interval,
		stallAfter:     stall,
		now:            now,
		logger:         logger,
	}
}

// Interval returns the sweep interval.
func (d *ChainDriver) Interval() time.Duration { return d.interval }

// Run sweeps once immediately, then every Interval until ctx is cancelled.
func (d *ChainDriver) Run(ctx context.Context) error {
	if err := d.SweepOnce(ctx); err != nil && ctx.Err() == nil {
		d.logger.Printf("chain driver sweep error: %v", err)
	}
	ticker := time.NewTicker(d.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := d.SweepOnce(ctx); err != nil && ctx.Err() == nil {
				d.logger.Printf("chain driver sweep error: %v", err)
			}
		}
	}
}

// SweepOnce walks every running run (keyset pages of 200), then cancels holds of finished
// runs (T1), sends due notifications (T2) and records the sweep time. One run's error is
// logged and deferred with next_check_at; it never stops the other runs.
func (d *ChainDriver) SweepOnce(ctx context.Context) error {
	now := d.now()
	var (
		afterCreated time.Time
		afterID      string
	)
	for {
		runs, err := d.store.ListRunningChainRunsAfter(ctx, afterCreated, afterID, chainSweepPageSize)
		if err != nil {
			return fmt.Errorf("list running chain runs: %w", err)
		}
		for _, run := range runs {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if run.NextCheckAt != nil && run.NextCheckAt.After(now) {
				continue
			}
			if err := d.ProcessRun(ctx, run.ID); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				d.logger.Printf("chain driver: run %s: %v (retrying later)", run.ID, err)
				if rerr := d.store.RecordChainCheckError(ctx, run.ID, d.interval, maxChainRetryDelay, now); rerr != nil {
					d.logger.Printf("chain driver: record retry for run %s: %v", run.ID, rerr)
				}
			}
		}
		if len(runs) < chainSweepPageSize {
			break
		}
		last := runs[len(runs)-1]
		afterCreated, afterID = last.CreatedAt, last.ID
	}

	// T1: holds left 'held' by a run that is no longer running.
	for {
		ids, err := d.store.CancelOrphanChainHolds(ctx)
		if err != nil {
			d.logger.Printf("chain driver: cancel orphan chain holds: %v", err)
			break
		}
		if len(ids) > 0 {
			d.logger.Printf("chain driver: cancelled holds of finished chain runs: %v", ids)
		}
		if len(ids) < store.MaxListLimit {
			break
		}
	}

	// T2: notifications.
	d.sendNotifications(ctx)

	return d.store.SetMeta(ctx, store.MetaKeyChainDriverLastSweep, d.now().UTC().Format(time.RFC3339Nano))
}

// ProcessRun reads one run, decides, plans admission when a step is to be created and
// commits the transition with AdvanceChain. A lost race (another sweep or an operator
// moved the run) commits nothing and is not an error.
func (d *ChainDriver) ProcessRun(ctx context.Context, runID string) error {
	snap, err := d.store.LoadChainSnapshot(ctx, runID)
	if err != nil {
		return err
	}
	if snap.Run.Status != store.ChainRunRunning {
		return nil
	}
	now := d.now()
	decide := func(s store.ChainSnapshot) store.ChainDecision { return chain.Decide(s, now, d.stallAfter) }
	planned := decide(*snap)

	params := store.AdvanceChainParams{
		RunID:       runID,
		Fingerprint: snap.Fingerprint(),
		Planned:     planned,
		Decide:      decide,
	}
	switch planned.Action {
	case store.ChainActionWait:
		if planned.Stalled {
			detail := fmt.Sprintf("step %d (%s) has not finished for more than %s", snap.Step.Seq, snap.Step.Step, d.stallAfter)
			if ok, err := d.store.RecordChainStall(ctx, runID, snap.Run.Revision, detail); err != nil {
				return err
			} else if ok {
				d.logger.Printf("chain driver: run %s stalled at seq %d", runID, snap.Step.Seq)
			}
		}
		return nil
	case store.ChainActionAdvance:
		def, err := chain.DecodeStored(snap.Run.DefinitionJSON)
		if err != nil {
			stop := store.ChainDecision{Action: store.ChainActionStop, StopReason: store.ChainStopInternalInconsistency, StopDetail: err.Error()}
			params.StopOverride = &stop
			break
		}
		in := chain.StepInput{
			Run:       snap.Run,
			Step:      planned.NextStep,
			Seq:       snap.Run.CurrentSeq + 1,
			Round:     planned.NextRound,
			PrevJobID: snap.Job.ID,
		}
		if snap.Artifact != nil && (planned.NextStep == store.ChainStepFix || planned.NextStep == store.ChainStepPublish) {
			in.ReviewedCommit = snap.Artifact.ReviewedCommit
		}
		w, reject, err := chain.PlanStep(ctx, d.adm, def, in)
		if err != nil {
			return err
		}
		if reject != nil {
			stop := store.ChainDecision{Action: store.ChainActionStop, StopReason: store.ChainStopAdmissionRejected,
				StopDetail: fmt.Sprintf("%s step: %v", planned.NextStep, reject)}
			params.StopOverride = &stop
		} else {
			params.Next = &w
		}
	}

	if d.beforeAdvance != nil {
		d.beforeAdvance(runID)
	}
	res, err := d.store.AdvanceChain(ctx, params)
	if err != nil {
		return err
	}
	if !res.Applied {
		return nil
	}
	switch {
	case res.JobID > 0:
		d.logger.Printf("chain driver: run %s seq %d → %s job #%d", runID, res.Run.CurrentSeq, planned.NextStep, res.JobID)
		d.notifyLauncher()
	case res.HeldID != "":
		d.logger.Printf("chain driver: run %s seq %d → %s held %s", runID, res.Run.CurrentSeq, planned.NextStep, res.HeldID)
	default:
		d.logger.Printf("chain driver: run %s is %s %s", runID, res.Run.Status, res.Run.StopReason)
	}
	return nil
}

func (d *ChainDriver) notifyLauncher() {
	if d.launcherNotify == nil {
		return
	}
	select {
	case d.launcherNotify <- struct{}{}:
	default:
	}
}

// sendNotifications delivers due chain notifications and commits sent_at only after a
// successful send (at-least-once; receivers drop duplicates by event_id).
func (d *ChainDriver) sendNotifications(ctx context.Context) {
	if d.notifier == nil {
		return
	}
	due, err := d.store.ListDueChainNotifications(ctx, d.now(), store.MaxListLimit)
	if err != nil {
		d.logger.Printf("chain driver: list chain notifications: %v", err)
		return
	}
	for _, n := range due {
		if ctx.Err() != nil {
			return
		}
		rep := Report{
			DedupKey:   n.EventID(),
			EventID:    n.EventID(),
			Kind:       n.Kind,
			ChainRunID: n.RunID,
			Message:    chainNotificationMessage(n),
			Payload:    json.RawMessage(n.PayloadJSON),
			Timestamp:  n.CreatedAt,
		}
		if err := reportChecked(ctx, d.notifier, rep); err != nil {
			d.logger.Printf("chain driver: send %s: %v", n.EventID(), err)
			base := d.interval
			if base < defaultChainNotifyBaseWait {
				base = defaultChainNotifyBaseWait
			}
			if rerr := d.store.RecordChainNotificationFailure(ctx, n.RunID, n.Revision, n.Kind, err.Error(), base, maxChainNotificationDelay, d.now()); rerr != nil {
				d.logger.Printf("chain driver: record send failure %s: %v", n.EventID(), rerr)
			}
			continue
		}
		if d.afterSend != nil && !d.afterSend(n) {
			continue // simulated stop between the send and the sent_at commit
		}
		if err := d.store.MarkChainNotificationSent(ctx, n.RunID, n.Revision, n.Kind, d.now()); err != nil {
			d.logger.Printf("chain driver: mark %s sent: %v", n.EventID(), err)
		}
	}
}

func chainNotificationMessage(n *store.ChainNotification) string {
	var p struct {
		Title      string `json:"title"`
		StopReason string `json:"stop_reason"`
		StopDetail string `json:"stop_detail"`
		Seq        int    `json:"seq"`
		Step       string `json:"step"`
	}
	_ = json.Unmarshal([]byte(n.PayloadJSON), &p)
	switch n.Kind {
	case store.ChainNotifyStopped:
		return fmt.Sprintf("chain run %s (%q) stopped at seq %d (%s): %s — %s", n.RunID, p.Title, p.Seq, p.Step, p.StopReason, p.StopDetail)
	case store.ChainNotifyStalled:
		return fmt.Sprintf("chain run %s (%q) is stalled at seq %d (%s): %s", n.RunID, p.Title, p.Seq, p.Step, p.StopDetail)
	case store.ChainNotifyCompleted:
		return fmt.Sprintf("chain run %s (%q) completed", n.RunID, p.Title)
	}
	return fmt.Sprintf("chain run %s: %s", n.RunID, n.Kind)
}

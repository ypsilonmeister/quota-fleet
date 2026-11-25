package runtime

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"sync"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/adapter"
	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/evaluator"
	"github.com/ypsilonmeister/quota-fleet/internal/quota"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

// Options configures the resident Runtime bundle (Dispatcher, Launcher, Shepherd, Reconciler, GateChecker, AgentChecker).
type Options struct {
	Store               *store.Store
	Adapter             adapter.Adapter
	Evaluator           evaluator.Evaluator
	Notifier            Notifier
	Config              *config.Config
	InstanceID          string
	StateDir            string
	DeliveriesDir       string
	MaxClaimed          int
	AttemptsCap         int
	MaxEvalRetries      int
	DispatchInterval    time.Duration
	ShepherdInterval    time.Duration
	ReconcileInterval   time.Duration
	StaleClaimedAfter   time.Duration
	SignalMaxBytes      int64
	GateMode            string
	QuotaUsageFile      string
	QuotaStaleAfter     time.Duration
	GateCheckInterval   time.Duration
	AgentsCheckInterval time.Duration
	ModelsCheckInterval time.Duration
	Pools               map[string]config.PoolConfig
	SnapshotLoader      func() (*quota.Snapshot, error)
	Now                 func() time.Time
	Logger              *log.Logger
}

// Runtime bundles and coordinates the lifecycle of all resident components.
type Runtime struct {
	Dispatcher   *Dispatcher
	Launcher     *Launcher
	Shepherd     *Shepherd
	Reconciler   *Reconciler
	GateChecker  *GateChecker
	AgentChecker *AgentVersionChecker
	ModelProber  *AgentModelProber

	notifyCh chan struct{}
	logger   *log.Logger

	mu      sync.Mutex
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	started bool
}

// New constructs a Runtime from opts, wiring Dispatcher, Launcher, Shepherd, and Reconciler together.
func New(opts Options) (*Runtime, error) {
	if opts.Store == nil {
		return nil, errors.New("runtime: Store must not be nil")
	}
	if opts.Adapter == nil {
		return nil, errors.New("runtime: Adapter must not be nil")
	}
	eval := opts.Evaluator
	if eval == nil {
		return nil, errors.New("runtime: Evaluator must not be nil")
	}
	logger := opts.Logger
	if logger == nil {
		logger = log.Default()
	}

	deliveriesDir := opts.DeliveriesDir
	if deliveriesDir == "" {
		if opts.StateDir != "" {
			deliveriesDir = filepath.Join(opts.StateDir, "deliveries")
		} else {
			deliveriesDir = "deliveries"
		}
	}
	notifier := opts.Notifier
	if notifier == nil {
		notifier = NewFileNotifier(deliveriesDir, opts.Store)
	}

	signalPath := ""
	if opts.StateDir != "" {
		signalPath = filepath.Join(opts.StateDir, "signal.jsonl")
	}

	maxClaimed := opts.MaxClaimed
	if maxClaimed <= 0 {
		maxClaimed = 1
	}
	attemptsCap := opts.AttemptsCap
	if attemptsCap <= 0 {
		attemptsCap = 3
	}

	dispatchInterval := opts.DispatchInterval
	if dispatchInterval <= 0 {
		dispatchInterval = config.DefaultDispatchInterval
	}
	shepherdInterval := opts.ShepherdInterval
	if shepherdInterval <= 0 {
		shepherdInterval = config.DefaultShepherdInterval
	}
	reconcileInterval := opts.ReconcileInterval
	if reconcileInterval <= 0 {
		reconcileInterval = config.DefaultReconcileInterval
	}
	staleAfter := opts.StaleClaimedAfter
	if staleAfter <= 0 {
		staleAfter = config.DefaultStaleClaimedAfter
	}
	signalMaxBytes := opts.SignalMaxBytes
	if signalMaxBytes <= 0 {
		signalMaxBytes = config.DefaultSignalMaxBytes
	}

	notifyCh := make(chan struct{}, 1)

	dispatcher := NewDispatcher(DispatcherOptions{
		Store:          opts.Store,
		NotifyCh:       notifyCh,
		SignalPath:     signalPath,
		SignalMaxBytes: signalMaxBytes,
		InstanceID:     opts.InstanceID,
		Interval:       dispatchInterval,
		Logger:         logger,
	})
	launcher := NewLauncher(LauncherOptions{
		Store:      opts.Store,
		Adapter:    opts.Adapter,
		NotifyCh:   notifyCh,
		MaxClaimed: maxClaimed,
		Logger:     logger,
	})
	shepherd := NewShepherd(ShepherdOptions{
		Store:          opts.Store,
		Adapter:        opts.Adapter,
		Evaluator:      eval,
		Notifier:       notifier,
		LauncherNotify: notifyCh,
		Interval:       shepherdInterval,
		MaxEvalRetries: opts.MaxEvalRetries,
		Logger:         logger,
	})
	reconciler := NewReconciler(ReconcilerOptions{
		Store:             opts.Store,
		Adapter:           opts.Adapter,
		Notifier:          notifier,
		Interval:          reconcileInterval,
		StaleClaimedAfter: staleAfter,
		AttemptsCap:       attemptsCap,
		Now:               opts.Now,
		Logger:            logger,
	})
	gateChecker := NewGateChecker(GateCheckerOptions{
		Store:          opts.Store,
		GateMode:       opts.GateMode,
		UsageFile:      opts.QuotaUsageFile,
		StaleAfter:     opts.QuotaStaleAfter,
		Pools:          opts.Pools,
		Interval:       opts.GateCheckInterval,
		Now:            opts.Now,
		Logger:         logger,
		SnapshotLoader: opts.SnapshotLoader,
	})

	var hostFAProvider adapter.HostFileAccessProvider
	if p, ok := opts.Adapter.(adapter.HostFileAccessProvider); ok {
		hostFAProvider = p
	}
	agentChecker := NewAgentVersionChecker(AgentVersionCheckerOptions{
		Store:    opts.Store,
		Config:   opts.Config,
		Provider: hostFAProvider,
		Notifier: notifier,
		Interval: opts.AgentsCheckInterval,
		Now:      opts.Now,
		Logger:   logger,
	})
	modelProber := NewAgentModelProber(AgentModelProberOptions{
		Store:    opts.Store,
		Config:   opts.Config,
		Provider: hostFAProvider,
		Interval: opts.ModelsCheckInterval,
		Now:      opts.Now,
		Logger:   logger,
	})

	return &Runtime{
		Dispatcher:   dispatcher,
		Launcher:     launcher,
		Shepherd:     shepherd,
		Reconciler:   reconciler,
		GateChecker:  gateChecker,
		AgentChecker: agentChecker,
		ModelProber:  modelProber,
		notifyCh:     notifyCh,
		logger:       logger,
	}, nil
}

// Start launches Dispatcher, Launcher, Shepherd, Reconciler, and (when enforce) GateChecker in background goroutines.
// Call Stop to gracefully shut down and wait for all goroutines to exit.
func (r *Runtime) Start(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.started {
		return errors.New("runtime already started")
	}

	runCtx, cancel := context.WithCancel(ctx)
	r.cancel = cancel
	r.started = true

	components := []struct {
		name string
		fn   func(context.Context) error
	}{
		{"dispatcher", r.Dispatcher.Run},
		{"launcher", r.Launcher.Run},
		{"shepherd", r.Shepherd.Run},
		{"reconciler", r.Reconciler.Run},
	}
	if r.GateChecker != nil && r.GateChecker.Enabled() {
		components = append(components, struct {
			name string
			fn   func(context.Context) error
		}{"gate", r.GateChecker.Run})
	}
	if r.AgentChecker != nil && r.AgentChecker.Enabled() {
		components = append(components, struct {
			name string
			fn   func(context.Context) error
		}{"agent-versions", r.AgentChecker.Run})
	}
	if r.ModelProber != nil && r.ModelProber.Enabled() {
		components = append(components, struct {
			name string
			fn   func(context.Context) error
		}{"agent-models", r.ModelProber.Run})
	}

	for _, comp := range components {
		c := comp
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			if err := c.fn(runCtx); err != nil && runCtx.Err() == nil {
				r.logger.Printf("runtime component %s exited with error: %v", c.name, err)
			}
		}()
	}

	return nil
}

// Stop cancels all resident goroutines and waits until every component has exited.
func (r *Runtime) Stop() {
	r.mu.Lock()
	cancel := r.cancel
	started := r.started
	r.mu.Unlock()

	if !started {
		return
	}
	if cancel != nil {
		cancel()
	}
	r.wg.Wait()

	r.mu.Lock()
	r.started = false
	r.cancel = nil
	r.mu.Unlock()
}

// Run starts all resident components, blocks until ctx is cancelled, and waits
// for a clean graceful shutdown before returning.
func (r *Runtime) Run(ctx context.Context) error {
	if err := r.Start(ctx); err != nil {
		return err
	}
	<-ctx.Done()
	r.Stop()
	return nil
}

// --- shared package helpers ---

func checkActiveWork(ctx context.Context, st *store.Store) (map[store.Status]int64, bool, error) {
	counts, err := st.CountByStatus(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("count jobs by status: %w", err)
	}
	undel, err := st.ListUndelivered(ctx, 1)
	if err != nil {
		return nil, false, fmt.Errorf("probe undelivered jobs: %w", err)
	}
	return counts, len(undel) > 0, nil
}

func hasAnyActive(counts map[store.Status]int64, hasUndel bool) bool {
	if hasUndel {
		return true
	}
	return counts[store.StatusClaimed] > 0 ||
		counts[store.StatusQueued] > 0 ||
		counts[store.StatusWaiting] > 0
}

func listAllByStatus(ctx context.Context, st *store.Store, status store.Status) ([]*store.Job, error) {
	var all []*store.Job
	var cursor int64
	for {
		page, err := st.ListJobs(ctx, store.ListJobsFilter{
			Status: status,
			Cursor: cursor,
			Limit:  store.MaxListLimit,
		})
		if err != nil {
			return nil, err
		}
		all = append(all, page.Jobs...)
		if !page.HasMore {
			break
		}
		cursor = page.NextCursor
	}
	return all, nil
}

func deliverAndMark(ctx context.Context, st *store.Store, notifier Notifier, job *store.Job) error {
	if job == nil {
		return nil
	}
	if notifier != nil {
		if err := notifier.Deliver(ctx, job); err != nil {
			return err
		}
	}
	// Idempotent fallback in case a custom Notifier does not call MarkDelivered itself.
	if _, err := st.MarkDelivered(ctx, job.ID); err != nil {
		return err
	}
	return nil
}

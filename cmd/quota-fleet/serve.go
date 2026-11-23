package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/adapter"
	"github.com/ypsilonmeister/quota-fleet/internal/api"
	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/evaluator"
	"github.com/ypsilonmeister/quota-fleet/internal/runtime"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

func runServe(args []string, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runServeContext(ctx, args, stdout, stderr)
}

func runServeContext(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		cfgPath string
		dbPath  string
		listen  string
	)
	fs.StringVar(&cfgPath, "config", resolveDefaultConfigPath(), "path to fleet.yaml")
	fs.StringVar(&cfgPath, "c", resolveDefaultConfigPath(), "path to fleet.yaml (shorthand)")
	fs.StringVar(&dbPath, "db", "", "override SQLite database path")
	fs.StringVar(&listen, "listen", "", "override HTTP listen address (default from api.listen or 127.0.0.1:8737)")
	if err := fs.Parse(args); err != nil {
		return 1
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "serve failed: %v\n", err)
		return 1
	}
	if dbPath == "" {
		dbPath = cfg.DBPath()
	}
	if listen == "" {
		listen = cfg.API.ListenAddr()
	}

	stateDir := cfg.StateDir
	if stateDir == "" {
		stateDir = filepath.Dir(dbPath)
	}
	deliveriesDir := cfg.Runtime.EffectiveDeliveriesDir(stateDir)

	var execAdapter adapter.Adapter
	switch cfg.Runtime.EffectiveAdapter() {
	case config.DefaultRuntimeAdapter:
		execAdapter = adapter.NewMock(adapter.MockOptions{})
	case config.RuntimeAdapterSSH:
		execAdapter = adapter.NewSSHAdapter(cfg, adapter.SSHAdapterOptions{
			Transport: adapter.NewRoutingTransport(
				adapter.NewSSHTransport(adapter.SSHTransportOptions{}),
				adapter.NewLocalTransport(adapter.LocalTransportOptions{}),
			),
		})
	default:
		fmt.Fprintf(stderr, "serve failed: unsupported runtime.adapter %q\n", cfg.Runtime.Adapter)
		return 1
	}

	st, err := store.Open(ctx, dbPath, store.Options{InstanceID: cfg.InstanceID})
	if err != nil {
		fmt.Fprintf(stderr, "open store %q: %v\n", dbPath, err)
		return 1
	}
	defer st.Close()

	if err := st.InitInstanceID(ctx, cfg.InstanceID); err != nil {
		fmt.Fprintf(stderr, "instance_id check failed: %v\n", err)
		return 1
	}

	var jobEvaluator evaluator.Evaluator
	switch cfg.Evaluator.EffectiveMode() {
	case config.EvaluatorModeTypedDecision:
		jobEvaluator = evaluator.NewTypedDecision(evaluator.TypedDecisionOptions{
			APIKeyEnv:         cfg.Evaluator.EffectiveAPIKeyEnv(),
			DecisionThreshold: cfg.Evaluator.EffectiveDecisionThreshold(),
			ThresholdSet:      cfg.Evaluator.DecisionThreshold != nil,
		})
	case config.EvaluatorModeAlwaysPass:
		jobEvaluator = evaluator.NewAlwaysPass()
	default:
		fmt.Fprintf(stderr, "serve failed: unsupported evaluator.mode %q\n", cfg.Evaluator.Mode)
		return 1
	}

	logger := log.New(stderr, "quota-fleet: ", log.LstdFlags)

	rt, err := runtime.New(runtime.Options{
		Store:             st,
		Adapter:           execAdapter,
		Evaluator:         jobEvaluator,
		InstanceID:        cfg.InstanceID,
		StateDir:          stateDir,
		DeliveriesDir:     deliveriesDir,
		MaxClaimed:        cfg.Limits.MaxClaimed,
		AttemptsCap:       cfg.Limits.AttemptsCap,
		MaxEvalRetries:    cfg.Evaluator.EffectiveMaxEvalRetries(),
		DispatchInterval:  cfg.Runtime.EffectiveDispatchInterval(),
		ShepherdInterval:  cfg.Runtime.EffectiveShepherdInterval(),
		ReconcileInterval: cfg.Runtime.EffectiveReconcileInterval(),
		StaleClaimedAfter: cfg.Runtime.EffectiveStaleClaimedAfter(),
		SignalMaxBytes:    cfg.Runtime.EffectiveSignalMaxBytes(),
		Logger:            logger,
	})
	if err != nil {
		fmt.Fprintf(stderr, "init runtime failed: %v\n", err)
		return 1
	}

	if err := rt.Start(ctx); err != nil {
		fmt.Fprintf(stderr, "start runtime failed: %v\n", err)
		return 1
	}
	defer rt.Stop()

	apiSrv := api.NewServer(st, api.Options{
		InstanceID:      cfg.InstanceID,
		MaxClaimed:      cfg.Limits.MaxClaimed,
		Token:           cfg.API.Token,
		RequireAuth:     cfg.API.RequireAuth,
		Logger:          logger,
		GateMode:        cfg.Quota.EffectiveGateMode(),
		QuotaUsageFile:  cfg.Quota.UsageFile,
		QuotaStaleAfter: cfg.Quota.EffectiveStaleAfter(),
		Pools:           cfg.Pools,
	})

	httpSrv := &http.Server{
		Addr:              listen,
		Handler:           apiSrv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		fmt.Fprintf(stdout, "quota-fleet serve listening on %s (instance_id=%s, db=%s, adapter=%s, evaluator=%s, gate_mode=%s)\n",
			listen, cfg.InstanceID, dbPath, cfg.Runtime.EffectiveAdapter(), cfg.Evaluator.EffectiveMode(), cfg.Quota.EffectiveGateMode())
		errCh <- httpSrv.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutCtx)
		rt.Stop()
		return 0
	case err := <-errCh:
		rt.Stop()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(stderr, "serve error: %v\n", err)
			return 1
		}
		return 0
	}
}

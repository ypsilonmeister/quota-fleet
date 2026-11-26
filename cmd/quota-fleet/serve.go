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
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/adapter"
	"github.com/ypsilonmeister/quota-fleet/internal/api"
	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/evaluator"
	"github.com/ypsilonmeister/quota-fleet/internal/mcpserver"
	"github.com/ypsilonmeister/quota-fleet/internal/push"
	"github.com/ypsilonmeister/quota-fleet/internal/runtime"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

// webPushTransport is replaced by tests to reach a fake push service.
var webPushTransport http.RoundTripper

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

	// One Decision 1 client serves both acceptance evaluation and the review gate (docs/18).
	decisionClient := evaluator.NewTypedDecision(evaluator.TypedDecisionOptions{
		APIKeyEnv:         cfg.Evaluator.EffectiveAPIKeyEnv(),
		APIKey:            cfg.Evaluator.APIKey,
		DecisionThreshold: cfg.Evaluator.EffectiveDecisionThreshold(),
		ThresholdSet:      cfg.Evaluator.DecisionThreshold != nil,
	})

	var jobEvaluator evaluator.Evaluator
	switch cfg.Evaluator.EffectiveMode() {
	case config.EvaluatorModeTypedDecision:
		jobEvaluator = decisionClient
	case config.EvaluatorModeAlwaysPass:
		jobEvaluator = evaluator.NewAlwaysPass()
	default:
		fmt.Fprintf(stderr, "serve failed: unsupported evaluator.mode %q\n", cfg.Evaluator.Mode)
		return 1
	}

	logger := log.New(stderr, "quota-fleet: ", log.LstdFlags)

	fileNotifier := runtime.NewFileNotifier(deliveriesDir, st)
	var jobNotifier runtime.Notifier = fileNotifier
	if webhookURL := cfg.Notifier.EffectiveWebhookURL(); webhookURL != "" {
		webhookNotifier := runtime.NewWebhookNotifier(runtime.WebhookNotifierOptions{
			URL:        webhookURL,
			Timeout:    cfg.Notifier.EffectiveWebhookTimeout(),
			Retries:    cfg.Notifier.EffectiveWebhookRetries(),
			RetriesSet: cfg.Notifier.WebhookRetries != nil,
			Logger:     logger,
		})
		jobNotifier = runtime.NewMultiNotifier(fileNotifier, webhookNotifier)
	}

	var webPushPublicKey string
	if cfg.API.WebPush {
		keyPath := filepath.Join(stateDir, push.KeyFileName)
		keys, created, err := push.LoadOrCreateVAPIDKeys(keyPath)
		if err != nil {
			fmt.Fprintf(stderr, "serve failed: web push: %v\n", err)
			return 1
		}
		if created {
			logger.Printf("web push: generated a VAPID key pair in %s", keyPath)
		}
		webPushPublicKey = keys.PublicKey
		// Started before the runtime so that no transition is missed. Its deferred Close
		// runs after rt.Stop and before st.Close.
		sender := push.Start(st, push.Options{
			Keys:         keys,
			Subject:      cfg.API.EffectiveWebPushSubject(),
			ReadTokenIDs: webPushReadTokenIDs(cfg.API),
			Logger:       logger,
			Transport:    webPushTransport,
		})
		defer sender.Close()
	}

	rt, err := runtime.New(runtime.Options{
		Store:               st,
		Adapter:             execAdapter,
		Evaluator:           jobEvaluator,
		Notifier:            jobNotifier,
		Config:              cfg,
		InstanceID:          cfg.InstanceID,
		StateDir:            stateDir,
		DeliveriesDir:       deliveriesDir,
		MaxClaimed:          cfg.Limits.MaxClaimed,
		AttemptsCap:         cfg.Limits.AttemptsCap,
		MaxEvalRetries:      cfg.Evaluator.EffectiveMaxEvalRetries(),
		DispatchInterval:    cfg.Runtime.EffectiveDispatchInterval(),
		ShepherdInterval:    cfg.Runtime.EffectiveShepherdInterval(),
		ReconcileInterval:   cfg.Runtime.EffectiveReconcileInterval(),
		StaleClaimedAfter:   cfg.Runtime.EffectiveStaleClaimedAfter(),
		SignalMaxBytes:      cfg.Runtime.EffectiveSignalMaxBytes(),
		GateMode:            cfg.Quota.EffectiveGateMode(),
		QuotaUsageFile:      cfg.Quota.UsageFile,
		QuotaStaleAfter:     cfg.Quota.EffectiveStaleAfter(),
		GateCheckInterval:   cfg.Quota.EffectiveGateCheckInterval(),
		AgentsCheckInterval: cfg.EffectiveAgentsCheckInterval(),
		ModelsCheckInterval: cfg.EffectiveModelsCheckInterval(),
		Pools:               cfg.Pools,
		Logger:              logger,
		SelectorMode:        cfg.Selector.EffectiveMode(),
		ChainSweepInterval:  cfg.EffectiveChainSweepInterval(),
		ChainStallAfter:     cfg.Chains.EffectiveStallAfter(),
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

	var mcpHandler func(http.Handler) http.Handler
	if cfg.API.MCPHTTP {
		mcpHandler = func(backend http.Handler) http.Handler {
			return mcpserver.NewHTTPHandler(backend, cfg.API.MCPAllowedOriginHosts)
		}
	}
	// docs/21 §3: with a real adapter, POST /jobs asks the job's host whether its CLI takes the effort.
	var hostAccess adapter.HostFileAccessProvider
	if p, ok := execAdapter.(adapter.HostFileAccessProvider); ok {
		hostAccess = p
	}
	apiSrv := api.NewServer(st, api.Options{
		ReviewGateEnabled: cfg.ReviewGate.Enabled,
		ReviewJudge:       decisionClient,

		MCPHandler:       mcpHandler,
		JobEvents:        cfg.API.JobEvents,
		WebPushPublicKey: webPushPublicKey,
		InstanceID:       cfg.InstanceID,
		MaxClaimed:       cfg.Limits.MaxClaimed,
		Token:            cfg.API.Token,
		Tokens:           cfg.API.Tokens,
		RequireAuth:      cfg.API.RequireAuth,
		Logger:           logger,
		GateMode:         cfg.Quota.EffectiveGateMode(),
		SelectorMode:     cfg.Selector.EffectiveMode(),
		EvaluatorMode:    cfg.Evaluator.EffectiveMode(),
		QuotaUsageFile:   cfg.Quota.UsageFile,
		QuotaStaleAfter:  cfg.Quota.EffectiveStaleAfter(),
		Pools:            cfg.Pools,
		Hosts:            sortedMapKeys(cfg.Hosts),
		Agents:           cfg.AllAgents(),
		Config:           cfg,
		GateReleaser:     gateReleaserAdapter{checker: rt.GateChecker},
		Chains: api.ChainOptions{
			MaxRoundsCap:     cfg.Chains.EffectiveMaxRoundsCap(),
			DriverStaleAfter: 3 * cfg.EffectiveChainSweepInterval(),
		},
		AgentChecker: rt.AgentChecker,
		HostAccess:   hostAccess,
	})

	httpSrv := &http.Server{
		Addr:              listen,
		Handler:           apiSrv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	// Event streams never end on their own; close them so Shutdown does not wait for them.
	httpSrv.RegisterOnShutdown(st.CloseJobEvents)

	errCh := make(chan error, 1)
	go func() {
		printServeBanner(stdout, logger, cfg, listen, dbPath, deliveriesDir)
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

// webPushReadTokenIDs identifies the configured tokens whose subscriptions may receive
// job notifications: those the API accepts with the read scope.
func webPushReadTokenIDs(cfg config.APIConfig) []string {
	var ids []string
	if cfg.Token != "" {
		ids = append(ids, push.TokenID(cfg.Token))
	}
	for _, t := range cfg.Tokens {
		if t.Token != "" && slices.Contains(t.Scopes, config.ScopeRead) {
			ids = append(ids, push.TokenID(t.Token))
		}
	}
	return ids
}

func formatPlannedUIURL(listen string) string {
	addr := strings.TrimSpace(listen)
	if addr == "" {
		addr = config.DefaultAPIListen
	}
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	return "http://" + addr + "/ui/"
}

func printServeBanner(w io.Writer, logger *log.Logger, cfg *config.Config, listen, dbPath, deliveriesDir string) {
	adapterName := cfg.Runtime.EffectiveAdapter()
	gateMode := cfg.Quota.EffectiveGateMode()
	evalMode := cfg.Evaluator.EffectiveMode()
	apiKeyEnv := cfg.Evaluator.EffectiveAPIKeyEnv()

	notifierSummary := fmt.Sprintf("file (%s)", deliveriesDir)
	if cfg.Notifier.EffectiveWebhookURL() != "" {
		notifierSummary = fmt.Sprintf("file (%s) + webhook", deliveriesDir)
	}

	apiAuthSummary := "disabled (no token)"
	switch {
	case len(cfg.API.Tokens) > 0 && strings.TrimSpace(cfg.API.Token) != "":
		apiAuthSummary = fmt.Sprintf("enabled (%d named tokens + legacy token)", len(cfg.API.Tokens))
	case len(cfg.API.Tokens) > 0:
		apiAuthSummary = fmt.Sprintf("enabled (%d named tokens)", len(cfg.API.Tokens))
	case strings.TrimSpace(cfg.API.Token) != "":
		apiAuthSummary = "enabled (bearer token)"
	case cfg.API.RequireAuth:
		apiAuthSummary = "enabled (require_auth=true, token unset)"
	}

	agentsCheckInterval := cfg.EffectiveAgentsCheckInterval()
	var agentsCheckSummary string
	switch {
	case agentsCheckInterval <= 0:
		agentsCheckSummary = "disabled (agents_check_interval=0)"
	case adapterName == config.DefaultRuntimeAdapter:
		agentsCheckSummary = fmt.Sprintf("disabled (adapter=%s)", adapterName)
	default:
		agentsCheckSummary = fmt.Sprintf("every %s", agentsCheckInterval)
	}

	modelsCheckInterval := cfg.EffectiveModelsCheckInterval()
	var modelsCheckSummary string
	if modelsCheckInterval <= 0 {
		modelsCheckSummary = "disabled (models_check_interval=0)"
	} else {
		modelsCheckSummary = fmt.Sprintf("every %s", modelsCheckInterval)
	}

	fmt.Fprintf(w, "quota-fleet serve listening on %s (instance_id=%s, db=%s, adapter=%s, evaluator=%s, gate_mode=%s)\n",
		listen, cfg.InstanceID, dbPath, adapterName, evalMode, gateMode)
	fmt.Fprintf(w, "  adapter:   %s\n", adapterName)
	fmt.Fprintf(w, "  gate_mode: %s\n", gateMode)
	fmt.Fprintf(w, "  evaluator: %s\n", evalMode)
	reviewGateSummary := "disabled"
	if cfg.ReviewGate.Enabled {
		reviewGateSummary = "enabled"
	}
	fmt.Fprintf(w, "  review_gate: %s\n", reviewGateSummary)
	fmt.Fprintf(w, "  notifier:  %s\n", notifierSummary)
	fmt.Fprintf(w, "  api_auth:  %s\n", apiAuthSummary)
	fmt.Fprintf(w, "  agents_check: %s\n", agentsCheckSummary)
	fmt.Fprintf(w, "  models_check: %s\n", modelsCheckSummary)
	fmt.Fprintf(w, "  ui_url:    %s\n", formatPlannedUIURL(listen))
	if cfg.API.WebPush {
		fmt.Fprintf(w, "  web_push:  enabled\n")
		if !cfg.API.HasAuthTokens() {
			fmt.Fprintf(w, "  WARNING:   api.web_push needs api.token or api.tokens; /push requests are rejected without one\n")
		}
	}

	keyConfigured := cfg.Evaluator.ResolveAPIKey(nil) != ""
	warn := func(warnMsg string) {
		fmt.Fprintf(w, "  WARNING:   %s\n", warnMsg)
		if logger != nil {
			logger.Printf("WARNING: %s", warnMsg)
		}
	}
	if evalMode == config.EvaluatorModeTypedDecision && !keyConfigured {
		warn(fmt.Sprintf(
			"%s is not set and evaluator.api_key is empty while evaluator.mode is %q; free-form acceptance criteria will fail evaluation (set %s or evaluator.api_key, or set evaluator.mode to %q)",
			apiKeyEnv, evalMode, apiKeyEnv, config.EvaluatorModeAlwaysPass,
		))
	}
	if cfg.ReviewGate.Enabled && !keyConfigured {
		warn(fmt.Sprintf(
			"review_gate.enabled is true but no Decision API key is configured; POST /decision/review-verdict will answer 503 (set %s or evaluator.api_key)",
			apiKeyEnv,
		))
	}
}

type gateReleaserAdapter struct {
	checker *runtime.GateChecker
}

func (a gateReleaserAdapter) ReleaseHeld(ctx context.Context, heldID string) (*api.GateReleaseResult, error) {
	if a.checker == nil {
		return nil, errors.New("gate checker is not initialized")
	}
	res, err := a.checker.ReleaseByID(ctx, heldID)
	if res == nil {
		return nil, err
	}
	out := &api.GateReleaseResult{
		Released:     res.Released,
		SelectedPool: res.SelectedPool,
		Reason:       res.Reason,
	}
	if res.Job != nil {
		out.JobID = res.Job.ID
	}
	return out, err
}

func sortedMapKeys[V any](m map[string]V) []string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		if trimmed := strings.TrimSpace(k); trimmed != "" {
			keys = append(keys, trimmed)
		}
	}
	sort.Strings(keys)
	return keys
}

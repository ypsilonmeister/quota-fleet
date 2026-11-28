package api

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/adapter"
	"github.com/ypsilonmeister/quota-fleet/internal/admission"
	"github.com/ypsilonmeister/quota-fleet/internal/agentmodels"
	"github.com/ypsilonmeister/quota-fleet/internal/agentver"
	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/push"
	"github.com/ypsilonmeister/quota-fleet/internal/quota"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
	"github.com/ypsilonmeister/quota-fleet/internal/ui"
)

const (
	defaultMaxClaimed = 3
	maxBodyBytes      = 1 << 20 // 1 MiB
)

// GateReleaseResult represents the outcome of an immediate re-evaluation release attempt for a held job.
type GateReleaseResult struct {
	Released     bool
	JobID        int64
	SelectedPool string
	Reason       string
}

// GateReleaser decouples the HTTP API server from the concrete Quota Gate checker implementation.
type GateReleaser interface {
	ReleaseHeld(ctx context.Context, heldID string) (*GateReleaseResult, error)
}

// AgentChecker decouples the HTTP API server from the concrete agent version prober.
type AgentChecker interface {
	CheckOnce(ctx context.Context) (*agentver.CheckResult, error)
}

// Options configures the HTTP API Server.
type Options struct {
	InstanceID  string
	MaxClaimed  int
	Token       string
	Tokens      []config.APITokenConfig
	RequireAuth bool
	Logger      *log.Logger

	// Quota Gate integration settings (off | dry-run | enforce).
	GateMode string
	// SelectorMode is off | record | apply (docs/19). Empty means off.
	SelectorMode     string
	EvaluatorMode    string
	QuotaUsageFile   string
	QuotaUsageFormat string
	QuotaStaleAfter  time.Duration
	Pools            map[string]config.PoolConfig
	// Non-sensitive configured names exposed by GET /meta.
	Hosts  []string
	Agents []string
	// Config provides host/agent pin and manual_update policies for GET /agents.
	Config *config.Config
	// MCPHandler, when non-nil, is mounted at /mcp behind bearer authentication (docs/13).
	// Unlike the JSON API, /mcp never allows anonymous access: with no tokens configured it
	// answers 401 even when RequireAuth is false.
	// It receives the authenticated API handler so tool calls re-enter the normal API path
	// (scope enforcement and created_by recording) with the caller's own token.
	MCPHandler func(backend http.Handler) http.Handler
	// JobEvents registers GET /events, the job status SSE stream (docs/14).
	JobEvents bool
	// WebPushPublicKey, when non-empty, registers the /push endpoints and is the VAPID
	// public key they hand out (docs/16). The private key is never given to the server.
	WebPushPublicKey string
	// EventsHeartbeat overrides the SSE heartbeat interval in tests when positive.
	EventsHeartbeat time.Duration
	// GateReleaser handles POST /gate/held/{id}/release via immediate quota re-evaluation.
	GateReleaser GateReleaser
	// AgentChecker handles POST /agents/check via immediate host×agent version probing.
	AgentChecker AgentChecker
	// HostAccess runs commands on configured hosts. When set, POST /jobs with an effort asks the
	// job's host whether its agent CLI takes the effort option (docs/21 §3). Without it (mock
	// adapter, tests) only the configuration is checked.
	HostAccess adapter.HostFileAccessProvider
	// EffortProbeTimeout bounds that host probe; zero means defaultEffortProbeTimeout.
	EffortProbeTimeout time.Duration
	// ReviewGateEnabled turns on POST /decision/review-verdict (docs/18). When false the
	// endpoint answers 503.
	ReviewGateEnabled bool
	// ReviewJudge makes the judgments for that endpoint. Without a judge that has an API
	// key the endpoint answers 503.
	ReviewJudge ReviewJudge
	// Now overrides the evaluation clock in tests when non-nil.
	Now func() time.Time
	// SnapshotLoader optionally overrides loading usage snapshots from QuotaUsageFile in tests.
	SnapshotLoader func() (*quota.Snapshot, error)
	// Chains configures the chain run endpoints (docs/22).
	Chains ChainOptions
}

// ChainOptions configures the chain run endpoints and their GET /status section.
type ChainOptions struct {
	// MaxRoundsCap rejects max_rounds above it (chains.max_rounds_cap, default 10).
	MaxRoundsCap int
	// DriverStaleAfter marks the Chain Driver stale in GET /status when its last sweep is
	// older (3 × chains.sweep_interval). Zero disables the flag.
	DriverStaleAfter time.Duration
}

type apiToken struct {
	name     string
	token    string
	scopes   []string
	scopeSet map[string]struct{}
}

type authContextKey struct{}

type authPrincipal struct {
	name string
	// tokenID is push.TokenID of the presented token, empty for anonymous access.
	tokenID  string
	scopes   []string
	scopeSet map[string]struct{}
}

func (p authPrincipal) hasScope(scope string) bool {
	_, ok := p.scopeSet[scope]
	return ok
}

func authPrincipalFromContext(ctx context.Context) (authPrincipal, bool) {
	p, ok := ctx.Value(authContextKey{}).(authPrincipal)
	return p, ok
}

func normalizeScopes(scopes []string) ([]string, map[string]struct{}) {
	rawSet := make(map[string]struct{}, len(scopes))
	for _, sc := range scopes {
		if config.ValidAPIScope(sc) {
			rawSet[sc] = struct{}{}
		}
	}
	out := make([]string, 0, len(rawSet))
	for _, sc := range config.AllAPIScopes {
		if _, ok := rawSet[sc]; ok {
			out = append(out, sc)
		}
	}
	return out, rawSet
}

// Server exposes the quota-fleet Job Store over an HTTP JSON API.
type Server struct {
	// Review verdict gate (docs/18).
	reviewGateEnabled bool
	reviewJudge       ReviewJudge

	store            *store.Store
	instanceID       string
	maxClaimed       int
	tokens           []apiToken
	requireAuth      bool
	logger           *log.Logger
	gateMode         string
	selectorMode     string
	evaluatorMode    string
	quotaUsageFile   string
	quotaUsageFormat string
	quotaStaleAfter  time.Duration
	pools            map[string]config.PoolConfig
	hosts            []string
	agents           []string
	cfg              *config.Config
	gateReleaser     GateReleaser
	agentChecker     AgentChecker
	effortProbe      *effortProber
	nowFn            func() time.Time
	snapshotLoader   func() (*quota.Snapshot, error)
	eventsHeartbeat  time.Duration
	webPushPublicKey string
	handler          http.Handler
	adm              *admission.Admission
	chains           ChainOptions
}

// NewServer constructs an HTTP API server backed by st.
func NewServer(st *store.Store, opts Options) *Server {
	maxClaimed := opts.MaxClaimed
	if maxClaimed <= 0 {
		maxClaimed = defaultMaxClaimed
	}
	logger := opts.Logger
	if logger == nil {
		logger = log.Default()
	}
	selectorMode := strings.TrimSpace(opts.SelectorMode)
	if selectorMode == "" {
		selectorMode = config.SelectorModeOff
	}
	gateMode := strings.TrimSpace(opts.GateMode)
	if gateMode == "" {
		gateMode = config.GateModeOff
	}
	evaluatorMode := strings.TrimSpace(opts.EvaluatorMode)
	if evaluatorMode == "" {
		evaluatorMode = config.DefaultEvaluatorMode
	}
	staleAfter := opts.QuotaStaleAfter
	if staleAfter <= 0 {
		staleAfter = config.DefaultQuotaStaleAfter
	}
	nowFn := opts.Now
	if nowFn == nil {
		nowFn = time.Now
	}

	hostNames := append([]string(nil), opts.Hosts...)
	agentNames := append([]string(nil), opts.Agents...)
	if opts.Config != nil {
		for h := range opts.Config.Hosts {
			hostNames = append(hostNames, h)
		}
		agentNames = append(agentNames, opts.Config.AllAgents()...)
	}

	tokens := make([]apiToken, 0, len(opts.Tokens)+1)
	if opts.Token != "" {
		allScopes, allScopeSet := normalizeScopes(config.AllAPIScopes)
		tokens = append(tokens, apiToken{
			name:     config.LegacyTokenName,
			token:    opts.Token,
			scopes:   allScopes,
			scopeSet: allScopeSet,
		})
	}
	for _, t := range opts.Tokens {
		if t.Token == "" {
			continue
		}
		sc, scSet := normalizeScopes(t.Scopes)
		tokens = append(tokens, apiToken{
			name:     strings.TrimSpace(t.Name),
			token:    t.Token,
			scopes:   sc,
			scopeSet: scSet,
		})
	}

	eventsHeartbeat := opts.EventsHeartbeat
	if eventsHeartbeat <= 0 {
		eventsHeartbeat = defaultEventsHeartbeat
	}

	usageFormat := strings.TrimSpace(opts.QuotaUsageFormat)
	if usageFormat == "" {
		usageFormat = config.UsageFormatLegacy
	}

	s := &Server{
		reviewGateEnabled: opts.ReviewGateEnabled,
		reviewJudge:       opts.ReviewJudge,

		store:            st,
		instanceID:       opts.InstanceID,
		maxClaimed:       maxClaimed,
		tokens:           tokens,
		requireAuth:      opts.RequireAuth,
		logger:           logger,
		gateMode:         gateMode,
		selectorMode:     selectorMode,
		evaluatorMode:    evaluatorMode,
		quotaUsageFile:   strings.TrimSpace(opts.QuotaUsageFile),
		quotaUsageFormat: usageFormat,
		quotaStaleAfter:  staleAfter,
		pools:            opts.Pools,
		hosts:            normalizeSortedNames(hostNames),
		agents:           normalizeSortedNames(agentNames),
		cfg:              opts.Config,
		gateReleaser:     opts.GateReleaser,
		agentChecker:     opts.AgentChecker,
		effortProbe:      newEffortProber(opts.HostAccess, opts.EffortProbeTimeout, nowFn),
		nowFn:            nowFn,
		snapshotLoader:   opts.SnapshotLoader,
		eventsHeartbeat:  eventsHeartbeat,
		webPushPublicKey: opts.WebPushPublicKey,
		chains:           opts.Chains,
	}
	s.adm = admission.New(admission.Options{
		Store:        s.store,
		GateMode:     gateMode,
		SelectorMode: selectorMode,
		Pools:        opts.Pools,
		StaleAfter:   staleAfter,
		UsageFile:    strings.TrimSpace(opts.QuotaUsageFile),
		UsageFormat:  usageFormat,
		Now:          nowFn,
		LoadSnapshot: opts.SnapshotLoader,
		GetJob:       st.GetJob,
		Logger:       logger,
	})

	if len(s.tokens) == 0 && !s.requireAuth {
		s.logger.Printf("WARNING: api.token and api.tokens are empty; HTTP API is running without authentication (intended for localhost only)")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /jobs", s.requireScope(config.ScopeRegister, s.handleCreateJob))
	mux.HandleFunc("GET /jobs", s.requireScope(config.ScopeRead, s.handleListJobs))
	mux.HandleFunc("GET /jobs/{id}", s.requireScope(config.ScopeRead, s.handleGetJob))
	mux.HandleFunc("POST /jobs/{id}/claim", s.requireScope(config.ScopeWorker, s.handleClaimJob))
	mux.HandleFunc("POST /jobs/{id}/complete", s.requireScope(config.ScopeWorker, s.handleCompleteJob))
	mux.HandleFunc("POST /jobs/{id}/fail", s.requireScope(config.ScopeWorker, s.handleFailJob))
	mux.HandleFunc("POST /jobs/{id}/cancel", s.requireScope(config.ScopeOperate, s.handleCancelJob))
	mux.HandleFunc("POST /jobs/{id}/cancel-claimed", s.requireScope(config.ScopeWorker, s.handleCancelClaimedJob))
	mux.HandleFunc("POST /jobs/{id}/requeue", s.requireScope(config.ScopeOperate, s.handleRequeueJob))
	mux.HandleFunc("POST /jobs/{id}/reset", s.requireScope(config.ScopeOperate, s.handleResetJob))
	mux.HandleFunc("POST /jobs/{id}/reject-invalid", s.requireScope(config.ScopeWorker, s.handleRejectInvalidJob))
	mux.HandleFunc("POST /jobs/{id}/deliver", s.requireScope(config.ScopeOperate, s.handleDeliverJob))
	mux.HandleFunc("POST /chains", s.requireScope(config.ScopeRegister, s.handleCreateChain))
	mux.HandleFunc("GET /chains", s.requireScope(config.ScopeRead, s.handleListChains))
	mux.HandleFunc("GET /chains/{id}", s.requireScope(config.ScopeRead, s.handleGetChain))
	// operate, or the token that registered the run (checked in the handler, docs/22 §9).
	mux.HandleFunc("POST /chains/{id}/cancel", s.handleCancelChain)
	mux.HandleFunc("GET /status", s.requireScope(config.ScopeRead, s.handleStatus))
	mux.HandleFunc("GET /meta", s.requireScope(config.ScopeRead, s.handleMeta))
	mux.HandleFunc("GET /deliveries", s.requireScope(config.ScopeRead, s.handleDeliveries))
	mux.HandleFunc("DELETE /gate/cooldowns/{pool}", s.requireScope(config.ScopeOperate, s.handleClearCooldown))
	mux.HandleFunc("GET /gate/held", s.requireScope(config.ScopeRead, s.handleListHeldJobs))
	mux.HandleFunc("POST /gate/held/{id}/release", s.requireScope(config.ScopeOperate, s.handleReleaseHeldJob))
	mux.HandleFunc("DELETE /gate/held/{id}", s.requireScope(config.ScopeOperate, s.handleCancelHeldJob))
	mux.HandleFunc("GET /gate/decisions", s.requireScope(config.ScopeRead, s.handleListGateDecisions))
	mux.HandleFunc("GET /agents", s.requireScope(config.ScopeRead, s.handleGetAgents))
	mux.HandleFunc("GET /agents/events", s.requireScope(config.ScopeRead, s.handleListAgentEvents))
	mux.HandleFunc("POST /agents/check", s.requireScope(config.ScopeOperate, s.handleCheckAgents))
	// docs/18: a verdict can drive a merge, so like /events this route has no anonymous
	// fallback even when require_auth is false.
	mux.Handle("POST /decision/review-verdict",
		s.requireTokenAuth(s.requireScope(config.ScopeOperate, s.handleReviewVerdict)))
	apiHandler := s.authMiddleware(mux)
	uiHandler := ui.NewHandler()

	rootMux := http.NewServeMux()
	// UI shell and static assets contain no job or config data and are served without auth.
	rootMux.Handle("/ui", uiHandler)
	rootMux.Handle("/ui/", uiHandler)
	// All JSON API endpoints remain protected by authMiddleware.
	if opts.MCPHandler != nil {
		mcpHandler := s.requireTokenAuth(opts.MCPHandler(apiHandler))
		rootMux.Handle("/mcp", mcpHandler)
		rootMux.Handle("/mcp/", mcpHandler)
	}
	if opts.JobEvents {
		// docs/14: /events always needs a bearer token. Unlike the JSON API it has no
		// anonymous fallback.
		rootMux.Handle("GET /events", s.requireTokenAuth(s.requireScope(config.ScopeRead, s.handleJobEvents)))
	}
	if s.webPushPublicKey != "" {
		// docs/16: like /events, no anonymous access. A subscriber receives job results.
		handle := func(pattern, scope string, handler http.HandlerFunc) {
			rootMux.Handle(pattern, s.requireTokenAuth(s.requireScope(scope, handler)))
		}
		handle("GET /push/vapid-public-key", config.ScopeRead, s.handleVAPIDPublicKey)
		// Registering also needs read: the token must be able to see what is pushed.
		handle("POST /push/subscriptions", config.ScopeOperate,
			s.requireScope(config.ScopeRead, s.handleCreatePushSubscription))
		handle("DELETE /push/subscriptions", config.ScopeOperate, s.handleDeletePushSubscription)
	}
	rootMux.Handle("/", apiHandler)

	s.handler = rootMux
	return s
}

// Handler returns the http.Handler for the API server (including auth middleware).
func (s *Server) Handler() http.Handler {
	return s.handler
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

func (s *Server) authMiddleware(next http.Handler) http.Handler {
	allScopes, allScopeSet := normalizeScopes(config.AllAPIScopes)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(s.tokens) == 0 {
			if s.requireAuth {
				writeError(w, http.StatusUnauthorized, "unauthorized: server api token is not configured")
				return
			}
			ctx := context.WithValue(r.Context(), authContextKey{}, authPrincipal{
				name:     "",
				scopes:   allScopes,
				scopeSet: allScopeSet,
			})
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}

		authHeader := r.Header.Get("Authorization")
		scheme, presented, ok := strings.Cut(authHeader, " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") {
			writeError(w, http.StatusUnauthorized, "unauthorized: missing or invalid bearer token")
			return
		}

		presentedBytes := []byte(presented)
		var matched *apiToken
		for i := range s.tokens {
			if subtle.ConstantTimeCompare(presentedBytes, []byte(s.tokens[i].token)) == 1 && matched == nil {
				matched = &s.tokens[i]
			}
		}
		if matched == nil {
			writeError(w, http.StatusUnauthorized, "unauthorized: invalid bearer token")
			return
		}

		ctx := context.WithValue(r.Context(), authContextKey{}, authPrincipal{
			name:     matched.name,
			tokenID:  push.TokenID(matched.token),
			scopes:   matched.scopes,
			scopeSet: matched.scopeSet,
		})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// requireTokenAuth is authMiddleware without the anonymous fallback: when no tokens are
// configured every request is rejected.
func (s *Server) requireTokenAuth(next http.Handler) http.Handler {
	if len(s.tokens) > 0 {
		return s.authMiddleware(next)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusUnauthorized, "unauthorized: server api token is not configured")
	})
}

func (s *Server) requireScope(scope string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := authPrincipalFromContext(r.Context())
		if !ok || !p.hasScope(scope) {
			writeJSON(w, http.StatusForbidden, ForbiddenResponse{
				Error:         fmt.Sprintf("forbidden: missing required scope %q", scope),
				RequiredScope: scope,
			})
			return
		}
		next(w, r)
	}
}

func (s *Server) handleCreateJob(w http.ResponseWriter, r *http.Request) {
	var req CreateJobRequest
	if err := decodeJSON(w, r, &req, false); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if store.IsReservedRequestID(req.RequestID) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("%v: request_id prefix %q is reserved for chain runs (docs/22)", store.ErrInvalidInput, store.ReservedRequestIDPrefix))
		return
	}

	createdBy := ""
	if p, ok := authPrincipalFromContext(r.Context()); ok {
		createdBy = p.name
	}
	admReq := admission.Request{
		Params: store.CreateJobParams{
			RequestID:          req.RequestID,
			Title:              req.Title,
			Draft:              req.Draft,
			Agent:              req.Agent,
			Host:               req.Host,
			Workdir:            req.Workdir,
			WorktreeSpec:       req.WorktreeSpec,
			TargetRef:          req.TargetRef,
			Instructions:       req.Instructions,
			AcceptanceCriteria: req.AcceptanceCriteria,
			ParentID:           req.ParentID,
			Model:              req.Model,
			Pool:               req.Pool,
			AltPools:           req.AltPools,
			SessionID:          req.SessionID,
			CreatedBy:          createdBy,
			Effort:             strings.TrimSpace(req.Effort),
		},
		Role:   req.Role,
		Weight: req.Weight,
	}
	if err := s.adm.ValidateRequest(admReq); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.adm.ValidateExplicitPools(req.Pool, req.AltPools); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	params := admReq.Params
	params.Pool = s.adm.ResolvePool(req.Agent, req.Model, req.Pool)

	// One usage snapshot per request, shared by the Selector and the Gate.
	usage := s.adm.NewUsage()
	s.adm.Select(r.Context(), &params, admReq, usage)

	// docs/21: effort is checked against the agent the job will actually run on (after the
	// Selector filled it), before the Gate may hold the payload. Never silently dropped.
	if err := store.ValidateEffort(params.Agent, params.Effort); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if status, err := s.checkEffortLaunch(r.Context(), params); err != nil {
		writeError(w, status, err.Error())
		return
	}

	if s.gateMode == config.GateModeEnforce {
		s.handleCreateJobEnforce(w, r, req, params, usage)
		return
	}

	job, created, err := s.store.CreateJob(r.Context(), params)
	if err != nil {
		s.writeCreateJobError(w, err)
		return
	}

	if created && s.gateMode == config.GateModeDryRun {
		s.recordDryRunGateDecision(r.Context(), job, params, req.AltPools, usage)
	}

	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, CreateJobResponse{
		Job:     toJobDTO(job),
		Created: created,
	})
}

func (s *Server) handleCreateJobEnforce(w http.ResponseWriter, r *http.Request, req CreateJobRequest, params store.CreateJobParams, usage *admission.Usage) {
	ctx := r.Context()

	reqID := strings.TrimSpace(req.RequestID)
	if reqID == "" {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("%v: request_id must not be empty", store.ErrInvalidInput))
		return
	}
	if strings.TrimSpace(req.Title) == "" {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("%v: title must not be empty", store.ErrInvalidInput))
		return
	}

	// Idempotency #1: if a job already exists for request_id, return it immediately without holding.
	if existingJob, err := s.store.GetJobByRequestID(ctx, reqID); err == nil {
		writeJSON(w, http.StatusOK, CreateJobResponse{
			Job:     toJobDTO(existingJob),
			Created: false,
		})
		return
	} else if !errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Idempotency #2: if a held_jobs entry already exists for request_id, return it without duplicate holds.
	if existingHeld, err := s.store.GetHeldJobByRequestID(ctx, reqID); err == nil {
		switch existingHeld.Status {
		case store.HeldStatusHeld:
			writeJSON(w, http.StatusAccepted, HoldJobResponse{
				HeldID:  existingHeld.ID,
				Held:    toHeldJobDTO(existingHeld, s.nowFn()),
				Created: false,
			})
			return
		case store.HeldStatusReleased:
			if existingHeld.ReleasedJobID != nil {
				if job, jerr := s.store.GetJob(ctx, *existingHeld.ReleasedJobID); jerr == nil {
					writeJSON(w, http.StatusOK, CreateJobResponse{
						Job:     toJobDTO(job),
						Created: false,
					})
					return
				}
			}
			writeJSON(w, http.StatusConflict, HeldConflictResponse{
				Error:         fmt.Sprintf("%v: held job %s for request_id=%s was already released", store.ErrConflict, existingHeld.ID, reqID),
				CurrentStatus: string(existingHeld.Status),
				Held:          toHeldJobDTO(existingHeld, s.nowFn()),
			})
			return
		default:
			writeJSON(w, http.StatusConflict, HeldConflictResponse{
				Error:         fmt.Sprintf("%v: held job %s for request_id=%s is %s", store.ErrConflict, existingHeld.ID, reqID, existingHeld.Status),
				CurrentStatus: string(existingHeld.Status),
				Held:          toHeldJobDTO(existingHeld, s.nowFn()),
			})
			return
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Gate input is the pool the job will actually run on (after the Selector).
	dec := s.adm.Gate(params, req.AltPools, usage)
	if dec.Kind == admission.KindReject {
		writeError(w, http.StatusBadRequest, dec.Reject.Error())
		return
	}
	if dec.SnapshotErr != nil {
		s.logger.Printf("WARNING: quota gate enforce evaluation for request_id=%s failed to load snapshot: %v", reqID, dec.SnapshotErr)
	}

	if dec.Kind == admission.KindHold {
		held, heldCreated, err := s.store.HoldJob(ctx, store.HoldJobParams{
			RequestID:       reqID,
			Pool:            dec.HoldPool,
			AltPools:        dec.HoldAltPools,
			Payload:         params,
			Reason:          dec.HoldReason,
			SnapshotSummary: dec.HoldSummary,
		})
		if err != nil {
			switch {
			case errors.Is(err, store.ErrJobAlreadyExists):
				if existingJob, jerr := s.store.GetJobByRequestID(ctx, reqID); jerr == nil {
					writeJSON(w, http.StatusOK, CreateJobResponse{
						Job:     toJobDTO(existingJob),
						Created: false,
					})
					return
				}
				writeError(w, http.StatusConflict, err.Error())
			case errors.Is(err, store.ErrConflict):
				resp := HeldConflictResponse{Error: err.Error()}
				if held != nil {
					resp.CurrentStatus = string(held.Status)
					resp.Held = toHeldJobDTO(held, s.nowFn())
				}
				writeJSON(w, http.StatusConflict, resp)
			case errors.Is(err, store.ErrInvalidInput):
				writeError(w, http.StatusBadRequest, err.Error())
			default:
				writeError(w, http.StatusInternalServerError, err.Error())
			}
			return
		}

		if heldCreated && dec.Gate != nil {
			rec := *dec.Gate
			rec.HeldID = held.ID
			if _, derr := s.store.RecordGateDecision(ctx, rec); derr != nil {
				s.logger.Printf("WARNING: failed to record enforce hold gate decision for held %s: %v", held.ID, derr)
			}
		}

		writeJSON(w, http.StatusAccepted, HoldJobResponse{
			HeldID:  held.ID,
			Held:    toHeldJobDTO(held, s.nowFn()),
			Created: heldCreated,
		})
		return
	}

	// Not held (healthy pool selected, unmeasured bypass, or provisional unknown only).
	// When a healthy/bypassed pool is selected, Gate already assigned it to the job.
	job, created, err := s.store.CreateJob(ctx, dec.Params)
	if err != nil {
		s.writeCreateJobError(w, err)
		return
	}

	if created && dec.Gate != nil {
		rec := *dec.Gate
		rec.JobID = job.ID
		if _, derr := s.store.RecordGateDecision(ctx, rec); derr != nil {
			s.logger.Printf("WARNING: failed to record enforce pass gate decision for job #%d: %v", job.ID, derr)
		}
	}

	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, CreateJobResponse{
		Job:     toJobDTO(job),
		Created: created,
	})
}

func (s *Server) writeCreateJobError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrInvalidInput),
		errors.Is(err, store.ErrParentNotFound),
		errors.Is(err, store.ErrParentTerminal):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}

func (s *Server) recordDryRunGateDecision(ctx context.Context, job *store.Job, params store.CreateJobParams, altPools []string, usage *admission.Usage) {
	dec := s.adm.Gate(params, altPools, usage)
	if dec.Gate == nil {
		return
	}
	if dec.SnapshotErr != nil {
		s.logger.Printf("WARNING: quota gate dry-run evaluation for job #%d failed to load snapshot: %v", job.ID, dec.SnapshotErr)
	}
	rec := *dec.Gate
	rec.JobID = job.ID
	if _, err := s.store.RecordGateDecision(ctx, rec); err != nil {
		s.logger.Printf("WARNING: failed to record dry-run gate decision for job #%d: %v", job.ID, err)
	}
}

func (s *Server) handleListHeldJobs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	status := store.HeldStatusHeld
	if v := strings.TrimSpace(q.Get("status")); v != "" {
		st := store.HeldStatus(v)
		if st != "all" && !st.Valid() {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid held status %q", v))
			return
		}
		status = st
	}

	var limit int
	if v := strings.TrimSpace(q.Get("limit")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid limit %q", v))
			return
		}
		limit = n
	}

	page, err := s.store.ListHeldJobsPage(r.Context(), store.ListHeldJobsFilter{
		Status: status,
		Cursor: strings.TrimSpace(q.Get("cursor")),
		Limit:  limit,
	})
	if err != nil {
		if errors.Is(err, store.ErrInvalidInput) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, ListHeldJobsResponse{
		Held:       toHeldJobDTOs(page.Held, s.nowFn()),
		NextCursor: page.NextCursor,
		HasMore:    page.HasMore,
	})
}

func (s *Server) handleCancelHeldJob(w http.ResponseWriter, r *http.Request) {
	heldID := strings.TrimSpace(r.PathValue("id"))
	if heldID == "" {
		writeError(w, http.StatusBadRequest, "held id is required")
		return
	}

	var req ReasonRequest
	if err := decodeOptionalJSONObject(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	held, err := s.store.CancelHeldJob(r.Context(), heldID, req.effectiveReason())
	if err != nil {
		switch {
		case errors.Is(err, store.ErrInvalidInput):
			writeError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, store.ErrNotFound):
			writeError(w, http.StatusNotFound, err.Error())
		case errors.Is(err, store.ErrConflict):
			resp := HeldConflictResponse{Error: err.Error()}
			if cur, gerr := s.store.GetHeldJob(r.Context(), heldID); gerr == nil && cur != nil {
				resp.CurrentStatus = string(cur.Status)
				resp.Held = toHeldJobDTO(cur, s.nowFn())
			}
			writeJSON(w, http.StatusConflict, resp)
		default:
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}

	writeJSON(w, http.StatusOK, HeldJobActionResponse{
		Held: toHeldJobDTO(held, s.nowFn()),
	})
}

func (s *Server) handleReleaseHeldJob(w http.ResponseWriter, r *http.Request) {
	heldID := strings.TrimSpace(r.PathValue("id"))
	if heldID == "" {
		writeError(w, http.StatusBadRequest, "held id is required")
		return
	}
	if s.gateReleaser == nil {
		writeError(w, http.StatusServiceUnavailable, "gate releaser is not configured")
		return
	}

	res, err := s.gateReleaser.ReleaseHeld(r.Context(), heldID)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrInvalidInput):
			writeError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, store.ErrNotFound):
			writeError(w, http.StatusNotFound, err.Error())
		case errors.Is(err, store.ErrConflict):
			reason := err.Error()
			if res != nil && strings.TrimSpace(res.Reason) != "" {
				reason = res.Reason
			}
			writeJSON(w, http.StatusConflict, ReleaseHeldResponse{
				Released: false,
				Reason:   reason,
			})
		default:
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}

	if res == nil || !res.Released {
		reason := "not releasable"
		if res != nil && strings.TrimSpace(res.Reason) != "" {
			reason = res.Reason
		}
		writeJSON(w, http.StatusConflict, ReleaseHeldResponse{
			Released: false,
			Reason:   reason,
		})
		return
	}

	writeJSON(w, http.StatusOK, ReleaseHeldResponse{
		Released: true,
		JobID:    res.JobID,
	})
}

func (s *Server) handleListGateDecisions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var filter store.GateDecisionsFilter

	if v := strings.TrimSpace(q.Get("job_id")); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid job_id %q", v))
			return
		}
		filter.JobID = &n
	}
	if v := strings.TrimSpace(q.Get("held_id")); v != "" {
		filter.HeldID = v
	}
	if v := strings.TrimSpace(q.Get("cursor")); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid cursor %q", v))
			return
		}
		filter.Cursor = n
	}
	if v := strings.TrimSpace(q.Get("limit")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid limit %q", v))
			return
		}
		filter.Limit = n
	}

	page, err := s.store.ListGateDecisionsPage(r.Context(), filter)
	if err != nil {
		if errors.Is(err, store.ErrInvalidInput) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, ListGateDecisionsResponse{
		Decisions:  toGateDecisionDTOs(page.Decisions),
		NextCursor: page.NextCursor,
		HasMore:    page.HasMore,
	})
}

func (s *Server) handleListJobs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	statusParam := strings.TrimSpace(q.Get("status"))
	if statusParam == "" {
		writeError(w, http.StatusBadRequest, "query parameter status is required")
		return
	}
	status := store.Status(statusParam)
	if !status.Valid() {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid status %q", statusParam))
		return
	}

	var (
		cursor   int64
		limit    int
		desc     bool
		parentID *int64
	)
	if v := strings.TrimSpace(q.Get("cursor")); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid cursor %q", v))
			return
		}
		cursor = n
	}
	if v := strings.TrimSpace(q.Get("limit")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid limit %q", v))
			return
		}
		limit = n
	}
	if v := strings.TrimSpace(q.Get("desc")); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid desc %q", v))
			return
		}
		desc = b
	}
	if v := strings.TrimSpace(q.Get("parent_id")); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid parent_id %q", v))
			return
		}
		parentID = &n
	}

	page, err := s.store.ListJobs(r.Context(), store.ListJobsFilter{
		Status:     status,
		Agent:      strings.TrimSpace(q.Get("agent")),
		Host:       strings.TrimSpace(q.Get("host")),
		Query:      strings.TrimSpace(q.Get("q")),
		ParentID:   parentID,
		Cursor:     cursor,
		Limit:      limit,
		Descending: desc,
	})
	if err != nil {
		if errors.Is(err, store.ErrInvalidInput) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, ListJobsResponse{
		Jobs:       toJobDTOs(page.Jobs),
		NextCursor: page.NextCursor,
		HasMore:    page.HasMore,
	})
}

func (s *Server) handleGetJob(w http.ResponseWriter, r *http.Request) {
	jobID, ok := parseJobID(w, r)
	if !ok {
		return
	}

	job, err := s.store.GetJob(r.Context(), jobID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, toJobDTO(job))
}

func (s *Server) handleClaimJob(w http.ResponseWriter, r *http.Request) {
	jobID, ok := parseJobID(w, r)
	if !ok {
		return
	}

	job, claimed, err := s.store.ClaimByID(r.Context(), jobID, s.maxClaimed)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !claimed {
		writeJSON(w, http.StatusOK, ClaimResponse{Claimed: false})
		return
	}
	writeJSON(w, http.StatusOK, ClaimResponse{
		Claimed: true,
		Token:   job.ClaimedBy,
		Job:     toJobDTO(job),
	})
}

func (s *Server) handleCompleteJob(w http.ResponseWriter, r *http.Request) {
	jobID, ok := parseJobID(w, r)
	if !ok {
		return
	}

	var req CompleteRequest
	if err := decodeJSON(w, r, &req, false); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	res, err := s.store.Complete(r.Context(), jobID, req.effectiveToken(), req.ResultText)
	if err != nil {
		s.writeTransitionError(w, r, jobID, err)
		return
	}

	promoted := res.PromotedChildIDs
	if promoted == nil {
		promoted = []int64{}
	}
	writeJSON(w, http.StatusOK, CompleteResponse{
		Job:              toJobDTO(res.Job),
		PromotedChildIDs: promoted,
	})
}

func (s *Server) handleFailJob(w http.ResponseWriter, r *http.Request) {
	jobID, ok := parseJobID(w, r)
	if !ok {
		return
	}

	var req FailRequest
	if err := decodeJSON(w, r, &req, false); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	res, err := s.store.Fail(r.Context(), jobID, req.effectiveToken(), req.effectiveReason())
	if err != nil {
		s.writeTransitionError(w, r, jobID, err)
		return
	}

	cancelled := res.CancelledChildIDs
	if cancelled == nil {
		cancelled = []int64{}
	}
	writeJSON(w, http.StatusOK, CascadeResponse{
		Job:               toJobDTO(res.Job),
		CancelledChildIDs: cancelled,
	})
}

func (s *Server) handleCancelJob(w http.ResponseWriter, r *http.Request) {
	jobID, ok := parseJobID(w, r)
	if !ok {
		return
	}

	var req ReasonRequest
	if err := decodeJSON(w, r, &req, true); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	res, err := s.store.Cancel(r.Context(), jobID, req.effectiveReason())
	if err != nil {
		s.writeTransitionError(w, r, jobID, err)
		return
	}

	cancelled := res.CancelledChildIDs
	if cancelled == nil {
		cancelled = []int64{}
	}
	writeJSON(w, http.StatusOK, CascadeResponse{
		Job:               toJobDTO(res.Job),
		CancelledChildIDs: cancelled,
	})
}

func (s *Server) handleCancelClaimedJob(w http.ResponseWriter, r *http.Request) {
	jobID, ok := parseJobID(w, r)
	if !ok {
		return
	}

	var req FailRequest
	if err := decodeJSON(w, r, &req, false); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	res, err := s.store.CancelClaimed(r.Context(), jobID, req.effectiveToken(), req.effectiveReason())
	if err != nil {
		s.writeTransitionError(w, r, jobID, err)
		return
	}

	cancelled := res.CancelledChildIDs
	if cancelled == nil {
		cancelled = []int64{}
	}
	writeJSON(w, http.StatusOK, CascadeResponse{
		Job:               toJobDTO(res.Job),
		CancelledChildIDs: cancelled,
	})
}

func (s *Server) handleRequeueJob(w http.ResponseWriter, r *http.Request) {
	jobID, ok := parseJobID(w, r)
	if !ok {
		return
	}

	job, err := s.store.Requeue(r.Context(), jobID)
	if err != nil {
		s.writeTransitionError(w, r, jobID, err)
		return
	}

	writeJSON(w, http.StatusOK, JobActionResponse{Job: toJobDTO(job)})
}

func (s *Server) handleResetJob(w http.ResponseWriter, r *http.Request) {
	jobID, ok := parseJobID(w, r)
	if !ok {
		return
	}

	job, err := s.store.Reset(r.Context(), jobID)
	if err != nil {
		s.writeTransitionError(w, r, jobID, err)
		return
	}

	writeJSON(w, http.StatusOK, JobActionResponse{Job: toJobDTO(job)})
}

func (s *Server) handleRejectInvalidJob(w http.ResponseWriter, r *http.Request) {
	jobID, ok := parseJobID(w, r)
	if !ok {
		return
	}

	var req ReasonRequest
	if err := decodeJSON(w, r, &req, true); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	res, err := s.store.RejectInvalid(r.Context(), jobID, req.effectiveReason())
	if err != nil {
		s.writeTransitionError(w, r, jobID, err)
		return
	}

	cancelled := res.CancelledChildIDs
	if cancelled == nil {
		cancelled = []int64{}
	}
	writeJSON(w, http.StatusOK, CascadeResponse{
		Job:               toJobDTO(res.Job),
		CancelledChildIDs: cancelled,
	})
}

func (s *Server) handleDeliverJob(w http.ResponseWriter, r *http.Request) {
	jobID, ok := parseJobID(w, r)
	if !ok {
		return
	}

	job, err := s.store.MarkDelivered(r.Context(), jobID)
	if err != nil {
		s.writeTransitionError(w, r, jobID, err)
		return
	}

	writeJSON(w, http.StatusOK, JobActionResponse{Job: toJobDTO(job)})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	counts, err := s.store.CountByStatus(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	out := make(map[string]int64, len(store.AllStatuses))
	var total int64
	for _, st := range store.AllStatuses {
		n := counts[st]
		out[string(st)] = n
		total += n
	}

	instanceID := s.instanceID
	if instanceID == "" {
		if id, err := s.store.GetMeta(r.Context(), "instance_id"); err == nil {
			instanceID = id
		}
	}

	var cooldowns []CooldownSummaryDTO
	if s.store.CooldownsEnabled() {
		opts, err := s.cooldownEvaluation(r.Context(), s.loadSnapshotForMeta())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		for _, c := range opts.Cooldowns {
			if c.Active(opts.Now) {
				item := cooldownSummary(c, opts.Now)
				item.Pool = c.Pool
				cooldowns = append(cooldowns, item)
			}
		}
		sort.Slice(cooldowns, func(i, j int) bool { return cooldowns[i].Pool < cooldowns[j].Pool })
	}
	writeJSON(w, http.StatusOK, StatusResponse{
		Cooldowns:  cooldowns,
		InstanceID: instanceID,
		Counts:     out,
		Total:      total,
		Chains:     s.chainStatus(r.Context()),
	})
}

func (s *Server) handleMeta(w http.ResponseWriter, r *http.Request) {
	poolNames := make([]string, 0, len(s.pools))
	for name := range s.pools {
		if trimmed := strings.TrimSpace(name); trimmed != "" {
			poolNames = append(poolNames, trimmed)
		}
	}
	sort.Strings(poolNames)

	var snap *quota.Snapshot
	if len(poolNames) > 0 {
		snap = s.loadSnapshotForMeta()
	}

	evalOpts, err := s.cooldownEvaluation(r.Context(), snap)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	summaries := make([]PoolSummaryDTO, 0, len(poolNames))
	for _, name := range poolNames {
		ev := quota.EvaluatePool(name, snap, evalOpts)
		summary := PoolSummaryDTO{
			Pool:       name,
			Status:     string(ev.Status),
			Unmeasured: ev.Unmeasured,
			BlockedBy:  ev.BlockedBy,
			Reason:     ev.Reason,
		}
		if c := ev.Cooldown; c != nil {
			summary.Cooldown = &CooldownDTO{CooldownSummaryDTO: cooldownSummary(*c, evalOpts.Now),
				Active: true, TriggeredAt: c.TriggeredAt, LastAgent: c.LastAgent, LastPattern: c.LastPattern}
		}

		if ev.Snapshot != nil {
			if !ev.Snapshot.WeeklyAbsent {
				w := ev.Snapshot.WeeklyRemainingPercent
				summary.WeeklyRemainingPercent = &w
			}
			summary.WeeklyResetAt = ev.Snapshot.WeeklyResetAt
			summary.WeeklyResetAtApprox = ev.Snapshot.WeeklyResetAtApprox
			summary.ShortApplicable = ev.Snapshot.ShortApplicable
			summary.ShortRemainingPercent = ev.Snapshot.ShortRemainingPercent
			summary.ShortResetAt = ev.Snapshot.ShortResetAt
			summary.ShortResetAtApprox = ev.Snapshot.ShortResetAtApprox
			summary.ShortWindow = ev.Snapshot.ShortWindow
			if !ev.Snapshot.MeasuredAt.IsZero() {
				mt := ev.Snapshot.MeasuredAt
				summary.MeasuredAt = &mt
			}
			summary.ServiceStale = ev.Snapshot.ServiceStale
		}
		summaries = append(summaries, summary)
	}

	hosts := s.hosts
	if hosts == nil {
		hosts = []string{}
	}
	agents := s.agents
	if agents == nil {
		agents = []string{}
	}

	var hostAgents map[string][]string
	if s.cfg != nil {
		hostAgents = s.cfg.HostAgentsMap()
	}

	models := s.buildMetaModels(r.Context())

	var poolRules map[string][]PoolAppliesToDTO
	if len(s.pools) > 0 {
		poolRules = make(map[string][]PoolAppliesToDTO, len(s.pools))
		for _, name := range poolNames {
			cfg := s.pools[name]
			if len(cfg.AppliesTo) == 0 {
				continue
			}
			rules := make([]PoolAppliesToDTO, 0, len(cfg.AppliesTo))
			for _, t := range cfg.AppliesTo {
				rules = append(rules, PoolAppliesToDTO{
					Agent:       strings.TrimSpace(t.Agent),
					ModelFamily: strings.TrimSpace(t.ModelFamily),
				})
			}
			poolRules[name] = rules
		}
		if len(poolRules) == 0 {
			poolRules = nil
		}
	}

	scopes := []string{}
	if p, ok := authPrincipalFromContext(r.Context()); ok && len(p.scopes) > 0 {
		scopes = append([]string(nil), p.scopes...)
	}

	writeJSON(w, http.StatusOK, MetaResponse{
		CooldownEnabled: s.cfg != nil && s.cfg.HasCooldown(),
		Hosts:           hosts,
		Agents:          agents,
		HostAgents:      hostAgents,
		Models:          models,
		Pools:           poolNames,
		PoolRules:       poolRules,
		GateMode:        s.gateMode,
		SelectorMode:    s.selectorMode,
		EvaluatorMode:   s.evaluatorMode,
		ReviewGate:      s.reviewGateMeta(),
		PoolSummaries:   summaries,
		Scopes:          scopes,
	})
}

func (s *Server) buildMetaModels(ctx context.Context) map[string]map[string][]ModelDTO {
	out := make(map[string]map[string][]ModelDTO)

	setModels := func(host, agent string, items []store.AgentModelItem) {
		if host == "" || agent == "" || len(items) == 0 {
			return
		}
		if out[host] == nil {
			out[host] = make(map[string][]ModelDTO)
		}
		dtos := make([]ModelDTO, 0, len(items))
		for _, it := range items {
			dtos = append(dtos, ModelDTO{
				ID:          it.ID,
				DisplayName: it.DisplayName,
				Family:      it.Family,
			})
		}
		out[host][agent] = dtos
	}

	if s.store != nil {
		if rows, err := s.store.ListAgentModels(ctx); err == nil {
			for _, row := range rows {
				if row == nil || len(row.Models) == 0 {
					continue
				}
				// claude's catalog is governed strictly by the current config (docs/11 §3);
				// ignore any stale DB snapshot when config is available so deletions take effect immediately.
				if s.cfg != nil && strings.EqualFold(strings.TrimSpace(row.Agent), "claude") {
					continue
				}
				setModels(row.Host, row.Agent, row.Models)
			}
		} else {
			s.logger.Printf("WARNING: failed to load agent_models for /meta: %v", err)
		}
	}

	// Fill in static config catalogs (e.g. claude in fleet.yaml) for any host×agent
	// not yet populated in agent_models.
	if s.cfg != nil {
		allAgents := s.cfg.AllAgents()
		for _, h := range s.hosts {
			hc := s.cfg.Hosts[h]
			var hostAgents []string
			if len(hc.Agents) > 0 {
				for a := range hc.Agents {
					if name := strings.TrimSpace(a); name != "" {
						hostAgents = append(hostAgents, name)
					}
				}
			} else {
				hostAgents = allAgents
			}
			for _, a := range hostAgents {
				if out[h] != nil && len(out[h][a]) > 0 {
					continue
				}
				if cfgItems := agentmodels.CatalogFromConfig(s.cfg, a); len(cfgItems) > 0 {
					setModels(h, a, cfgItems)
				}
			}
		}
	}

	return out
}

func (s *Server) loadSnapshotForMeta() *quota.Snapshot {
	if s.snapshotLoader != nil {
		snap, err := s.snapshotLoader()
		if err != nil {
			return nil
		}
		return snap
	}
	if s.quotaUsageFile == "" {
		return nil
	}
	snap, err := quota.LoadUsageFileWithFormat(s.quotaUsageFile, s.quotaUsageFormat)
	if err != nil {
		return nil
	}
	return snap
}

func normalizeSortedNames(names []string) []string {
	if len(names) == 0 {
		return []string{}
	}
	seen := make(map[string]struct{}, len(names))
	out := make([]string, 0, len(names))
	for _, n := range names {
		trimmed := strings.TrimSpace(n)
		if trimmed == "" {
			continue
		}
		if _, ok := seen[trimmed]; ok {
			continue
		}
		seen[trimmed] = struct{}{}
		out = append(out, trimmed)
	}
	sort.Strings(out)
	return out
}

func (s *Server) handleDeliveries(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	status := store.DeliveryStatusUndelivered
	if v := strings.TrimSpace(q.Get("status")); v != "" {
		st := store.DeliveryStatus(v)
		if !st.Valid() {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid delivery status %q", v))
			return
		}
		status = st
	}

	var limit int
	if v := strings.TrimSpace(q.Get("limit")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid limit %q", v))
			return
		}
		limit = n
	}

	page, err := s.store.ListDeliveriesPage(r.Context(), store.ListDeliveriesFilter{
		Status: status,
		Cursor: strings.TrimSpace(q.Get("cursor")),
		Limit:  limit,
	})
	if err != nil {
		if errors.Is(err, store.ErrInvalidInput) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, DeliveriesResponse{
		Jobs:       toJobDTOs(page.Jobs),
		NextCursor: page.NextCursor,
		HasMore:    page.HasMore,
	})
}

func (s *Server) handleGetAgents(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.ListAgentVersions(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	matrix := agentver.BuildMatrix(s.cfg, rows)
	writeJSON(w, http.StatusOK, AgentsResponse{Matrix: matrix})
}

func (s *Server) handleListAgentEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var filter store.AgentVersionEventsFilter

	if v := strings.TrimSpace(q.Get("since")); v != "" {
		t, err := time.Parse(time.RFC3339Nano, v)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid since timestamp %q (expected RFC3339)", v))
			return
		}
		t = t.UTC()
		filter.Since = &t
	}
	if v := strings.TrimSpace(q.Get("agent")); v != "" {
		filter.Agent = v
	}
	if v := strings.TrimSpace(q.Get("host")); v != "" {
		filter.Host = v
	}
	if v := strings.TrimSpace(q.Get("type")); v != "" {
		et := store.AgentVersionEventType(v)
		if !et.Valid() {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid event type %q", v))
			return
		}
		filter.EventType = et
	}
	if v := strings.TrimSpace(q.Get("limit")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid limit %q", v))
			return
		}
		filter.Limit = n
	}

	events, err := s.store.ListAgentVersionEvents(r.Context(), filter)
	if err != nil {
		if errors.Is(err, store.ErrInvalidInput) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, AgentEventsResponse{
		Events: toAgentVersionEventDTOs(events),
	})
}

func (s *Server) handleCheckAgents(w http.ResponseWriter, r *http.Request) {
	if s.agentChecker == nil {
		writeError(w, http.StatusServiceUnavailable, "agent version checker is not configured")
		return
	}
	res, err := s.agentChecker.CheckOnce(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, AgentCheckResponse{
		Matrix: res.Matrix,
		Events: toAgentVersionEventDTOs(res.Events),
	})
}

func (s *Server) writeTransitionError(w http.ResponseWriter, r *http.Request, jobID int64, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, store.ErrConflict), errors.Is(err, store.ErrTokenMismatch):
		resp := ConflictResponse{
			Error: err.Error(),
		}
		if cur, gerr := s.store.GetJob(r.Context(), jobID); gerr == nil && cur != nil {
			resp.CurrentStatus = string(cur.Status)
			resp.Job = toJobDTO(cur)
		}
		writeJSON(w, http.StatusConflict, resp)
	case errors.Is(err, store.ErrInvalidInput):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}

func parseJobID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	raw := r.PathValue("id")
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid job id %q", raw))
		return 0, false
	}
	return id, true
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any, allowEmpty bool) error {
	if r.Body == nil {
		if allowEmpty {
			return nil
		}
		return errors.New("request body is required")
	}
	limited := http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(limited)
	err := dec.Decode(dst)
	if err != nil {
		if errors.Is(err, io.EOF) && allowEmpty {
			return nil
		}
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	return nil
}

// decodeOptionalJSONObject accepts an empty body, leaving dst unchanged, or a body that
// is exactly one JSON object. Unlike decodeJSON it rejects null and trailing data.
func decodeOptionalJSONObject(w http.ResponseWriter, r *http.Request, dst any) error {
	if r.Body == nil {
		return nil
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	if len(body) == 0 {
		return nil
	}
	if !bytes.HasPrefix(bytes.TrimLeft(body, " \t\r\n"), []byte("{")) {
		return errors.New("invalid JSON body: want a JSON object")
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, ErrorResponse{Error: message})
}

func cooldownSummary(c quota.CooldownState, now time.Time) CooldownSummaryDTO {
	return CooldownSummaryDTO{Until: c.ActiveUntil, RemainingSeconds: int64(c.ActiveUntil.Sub(now).Seconds()),
		ConsecutiveHits: c.ConsecutiveHits, LastJobID: c.LastJobID}
}

func (s *Server) cooldownEvaluation(ctx context.Context, snap *quota.Snapshot) (quota.EvalOptions, error) {
	opts := quota.EvalOptions{Now: s.nowFn(), StaleAfter: s.quotaStaleAfter, Pools: s.pools, Format: s.quotaUsageFormat}
	err := s.store.LoadCooldowns(ctx, snap, &opts)
	return opts, err
}

func (s *Server) handleClearCooldown(w http.ResponseWriter, r *http.Request) {
	pool := r.PathValue("pool")
	if _, ok := s.pools[pool]; !ok {
		writeError(w, http.StatusNotFound, "pool is not configured")
		return
	}
	cleared, err := s.store.ClearCooldown(r.Context(), pool)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if notifier, ok := s.gateReleaser.(interface{ NotifyLauncher() }); cleared && ok {
		notifier.NotifyLauncher()
	}
	w.WriteHeader(http.StatusNoContent)
}

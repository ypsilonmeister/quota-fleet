package api

import (
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

	"github.com/ypsilonmeister/quota-fleet/internal/agentver"
	"github.com/ypsilonmeister/quota-fleet/internal/config"
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
	GateMode        string
	EvaluatorMode   string
	QuotaUsageFile  string
	QuotaStaleAfter time.Duration
	Pools           map[string]config.PoolConfig
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
	// GateReleaser handles POST /gate/held/{id}/release via immediate quota re-evaluation.
	GateReleaser GateReleaser
	// AgentChecker handles POST /agents/check via immediate host×agent version probing.
	AgentChecker AgentChecker
	// Now overrides the evaluation clock in tests when non-nil.
	Now func() time.Time
	// SnapshotLoader optionally overrides loading usage snapshots from QuotaUsageFile in tests.
	SnapshotLoader func() (*quota.Snapshot, error)
}

type apiToken struct {
	name     string
	token    string
	scopes   []string
	scopeSet map[string]struct{}
}

type authContextKey struct{}

type authPrincipal struct {
	name     string
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
	store           *store.Store
	instanceID      string
	maxClaimed      int
	tokens          []apiToken
	requireAuth     bool
	logger          *log.Logger
	gateMode        string
	evaluatorMode   string
	quotaUsageFile  string
	quotaStaleAfter time.Duration
	pools           map[string]config.PoolConfig
	hosts           []string
	agents          []string
	cfg             *config.Config
	gateReleaser    GateReleaser
	agentChecker    AgentChecker
	nowFn           func() time.Time
	snapshotLoader  func() (*quota.Snapshot, error)
	handler         http.Handler
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

	s := &Server{
		store:           st,
		instanceID:      opts.InstanceID,
		maxClaimed:      maxClaimed,
		tokens:          tokens,
		requireAuth:     opts.RequireAuth,
		logger:          logger,
		gateMode:        gateMode,
		evaluatorMode:   evaluatorMode,
		quotaUsageFile:  strings.TrimSpace(opts.QuotaUsageFile),
		quotaStaleAfter: staleAfter,
		pools:           opts.Pools,
		hosts:           normalizeSortedNames(opts.Hosts),
		agents:          normalizeSortedNames(opts.Agents),
		cfg:             opts.Config,
		gateReleaser:    opts.GateReleaser,
		agentChecker:    opts.AgentChecker,
		nowFn:           nowFn,
		snapshotLoader:  opts.SnapshotLoader,
	}

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
	mux.HandleFunc("GET /status", s.requireScope(config.ScopeRead, s.handleStatus))
	mux.HandleFunc("GET /meta", s.requireScope(config.ScopeRead, s.handleMeta))
	mux.HandleFunc("GET /deliveries", s.requireScope(config.ScopeRead, s.handleDeliveries))
	mux.HandleFunc("GET /gate/held", s.requireScope(config.ScopeRead, s.handleListHeldJobs))
	mux.HandleFunc("POST /gate/held/{id}/release", s.requireScope(config.ScopeOperate, s.handleReleaseHeldJob))
	mux.HandleFunc("DELETE /gate/held/{id}", s.requireScope(config.ScopeOperate, s.handleCancelHeldJob))
	mux.HandleFunc("GET /gate/decisions", s.requireScope(config.ScopeRead, s.handleListGateDecisions))
	mux.HandleFunc("GET /agents", s.requireScope(config.ScopeRead, s.handleGetAgents))
	mux.HandleFunc("GET /agents/events", s.requireScope(config.ScopeRead, s.handleListAgentEvents))
	mux.HandleFunc("POST /agents/check", s.requireScope(config.ScopeOperate, s.handleCheckAgents))

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

	createdBy := ""
	if p, ok := authPrincipalFromContext(r.Context()); ok {
		createdBy = p.name
	}

	params := store.CreateJobParams{
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
	}

	if s.gateMode == config.GateModeEnforce {
		s.handleCreateJobEnforce(w, r, req, params)
		return
	}

	job, created, err := s.store.CreateJob(r.Context(), params)
	if err != nil {
		s.writeCreateJobError(w, err)
		return
	}

	if created && s.gateMode == config.GateModeDryRun {
		s.recordDryRunGateDecision(r.Context(), job, req)
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

func (s *Server) handleCreateJobEnforce(w http.ResponseWriter, r *http.Request, req CreateJobRequest, params store.CreateJobParams) {
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
				Held:    toHeldJobDTO(existingHeld),
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
				Held:          toHeldJobDTO(existingHeld),
			})
			return
		default:
			writeJSON(w, http.StatusConflict, HeldConflictResponse{
				Error:         fmt.Sprintf("%v: held job %s for request_id=%s is %s", store.ErrConflict, existingHeld.ID, reqID, existingHeld.Status),
				CurrentStatus: string(existingHeld.Status),
				Held:          toHeldJobDTO(existingHeld),
			})
			return
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	reqPool := s.resolveRequestPool(req)
	if reqPool == "" && len(req.AltPools) == 0 {
		job, created, err := s.store.CreateJob(ctx, params)
		if err != nil {
			s.writeCreateJobError(w, err)
			return
		}
		status := http.StatusOK
		if created {
			status = http.StatusCreated
		}
		writeJSON(w, status, CreateJobResponse{
			Job:     toJobDTO(job),
			Created: created,
		})
		return
	}

	eval, evalErr := s.evaluateGate(reqPool, req.AltPools)
	if evalErr != nil {
		s.logger.Printf("WARNING: quota gate enforce evaluation for request_id=%s failed to load snapshot: %v", reqID, evalErr)
	}

	if quota.ShouldHoldOnEnforce(eval) {
		held, heldCreated, err := s.store.HoldJob(ctx, store.HoldJobParams{
			RequestID:       reqID,
			Pool:            reqPool,
			AltPools:        eval.AltPools,
			Payload:         params,
			Reason:          quota.FormatHoldReason(eval),
			SnapshotSummary: quota.SummarizeEvaluation(eval),
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
					resp.Held = toHeldJobDTO(held)
				}
				writeJSON(w, http.StatusConflict, resp)
			case errors.Is(err, store.ErrInvalidInput):
				writeError(w, http.StatusBadRequest, err.Error())
			default:
				writeError(w, http.StatusInternalServerError, err.Error())
			}
			return
		}

		if heldCreated {
			if _, derr := s.store.RecordGateDecision(ctx, store.GateDecision{
				HeldID:        held.ID,
				Held:          true,
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
			}); derr != nil {
				s.logger.Printf("WARNING: failed to record enforce hold gate decision for held %s: %v", held.ID, derr)
			}
		}

		writeJSON(w, http.StatusAccepted, HoldJobResponse{
			HeldID:  held.ID,
			Held:    toHeldJobDTO(held),
			Created: heldCreated,
		})
		return
	}

	// Not held (healthy pool selected, unmeasured bypass, or provisional unknown only).
	// When a healthy/bypassed pool is selected, assign it to the created job.
	if eval.Decision == quota.DecisionWouldPass && eval.SelectedPool != "" {
		params.Pool = eval.SelectedPool
	}

	job, created, err := s.store.CreateJob(ctx, params)
	if err != nil {
		s.writeCreateJobError(w, err)
		return
	}

	if created {
		if _, derr := s.store.RecordGateDecision(ctx, store.GateDecision{
			JobID:         job.ID,
			Held:          false,
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
		}); derr != nil {
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

func (s *Server) resolveRequestPool(req CreateJobRequest) string {
	reqPool := strings.TrimSpace(req.Pool)
	if reqPool == "" {
		reqPool = quota.ResolvePoolByAppliesTo(s.pools, req.Agent, req.Model)
	}
	return reqPool
}

func (s *Server) evaluateGate(primaryPool string, altPools []string) (quota.GateEvaluation, error) {
	opts := quota.EvalOptions{
		Now:        s.nowFn(),
		StaleAfter: s.quotaStaleAfter,
		Pools:      s.pools,
	}
	primaryPool = strings.TrimSpace(primaryPool)
	if primaryPool != "" {
		if cfg, ok := s.pools[primaryPool]; ok && cfg.Unmeasured {
			return quota.EvaluateGate(primaryPool, altPools, nil, opts), nil
		}
	}
	if s.snapshotLoader != nil {
		snap, err := s.snapshotLoader()
		if err != nil {
			eval := quota.EvaluateGate(primaryPool, altPools, nil, opts)
			if !eval.Bypassed {
				eval.Decision = quota.DecisionUnknown
				eval.Reason = fmt.Sprintf("%s: %v", quota.ReasonSnapshotError, err)
			}
			return eval, err
		}
		return quota.EvaluateGate(primaryPool, altPools, snap, opts), nil
	}
	return quota.EvaluateFromFile(s.quotaUsageFile, primaryPool, altPools, opts)
}

func (s *Server) recordDryRunGateDecision(ctx context.Context, job *store.Job, req CreateJobRequest) {
	reqPool := s.resolveRequestPool(req)
	if reqPool == "" && len(req.AltPools) == 0 {
		return
	}

	eval, evalErr := s.evaluateGate(reqPool, req.AltPools)
	if evalErr != nil {
		s.logger.Printf("WARNING: quota gate dry-run evaluation for job #%d failed to load snapshot: %v", job.ID, evalErr)
	}

	_, err := s.store.RecordGateDecision(ctx, store.GateDecision{
		JobID:         job.ID,
		Mode:          config.GateModeDryRun,
		RequestedPool: eval.RequestedPool,
		AltPools:      eval.AltPools,
		Decision:      string(eval.Decision),
		BlockedBy:     eval.BlockedBy,
		SelectedPool:  eval.SelectedPool,
		Bypassed:      eval.Bypassed,
		Reason:        eval.Reason,
		MeasuredAt:    eval.MeasuredAt,
		DecidedAt:     eval.DecidedAt,
	})
	if err != nil {
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
		Held:       toHeldJobDTOs(page.Held),
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

	held, err := s.store.CancelHeldJob(r.Context(), heldID)
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
				resp.Held = toHeldJobDTO(cur)
			}
			writeJSON(w, http.StatusConflict, resp)
		default:
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}

	writeJSON(w, http.StatusOK, HeldJobActionResponse{
		Held: toHeldJobDTO(held),
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

	writeJSON(w, http.StatusOK, StatusResponse{
		InstanceID: instanceID,
		Counts:     out,
		Total:      total,
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

	evalOpts := quota.EvalOptions{
		Now:        s.nowFn(),
		StaleAfter: s.quotaStaleAfter,
		Pools:      s.pools,
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
		if ev.Snapshot != nil {
			w := ev.Snapshot.WeeklyRemainingPercent
			summary.WeeklyRemainingPercent = &w
			summary.WeeklyResetAt = ev.Snapshot.WeeklyResetAt
			summary.ShortRemainingPercent = ev.Snapshot.ShortRemainingPercent
			summary.ShortResetAt = ev.Snapshot.ShortResetAt
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

	scopes := []string{}
	if p, ok := authPrincipalFromContext(r.Context()); ok && len(p.scopes) > 0 {
		scopes = append([]string(nil), p.scopes...)
	}

	writeJSON(w, http.StatusOK, MetaResponse{
		Hosts:         hosts,
		Agents:        agents,
		Pools:         poolNames,
		GateMode:      s.gateMode,
		EvaluatorMode: s.evaluatorMode,
		PoolSummaries: summaries,
		Scopes:        scopes,
	})
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
	snap, err := quota.LoadUsageFile(s.quotaUsageFile)
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

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, ErrorResponse{Error: message})
}

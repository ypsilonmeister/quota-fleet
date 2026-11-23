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
	"strconv"
	"strings"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/quota"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

const (
	defaultMaxClaimed = 3
	maxBodyBytes      = 1 << 20 // 1 MiB
)

// Options configures the HTTP API Server.
type Options struct {
	InstanceID  string
	MaxClaimed  int
	Token       string
	RequireAuth bool
	Logger      *log.Logger

	// Quota Gate integration settings (off | dry-run | enforce).
	GateMode        string
	QuotaUsageFile  string
	QuotaStaleAfter time.Duration
	Pools           map[string]config.PoolConfig
	// Now overrides the evaluation clock in tests when non-nil.
	Now func() time.Time
	// SnapshotLoader optionally overrides loading usage snapshots from QuotaUsageFile in tests.
	SnapshotLoader func() (*quota.Snapshot, error)
}

// Server exposes the quota-fleet Job Store over an HTTP JSON API.
type Server struct {
	store           *store.Store
	instanceID      string
	maxClaimed      int
	token           string
	requireAuth     bool
	logger          *log.Logger
	gateMode        string
	quotaUsageFile  string
	quotaStaleAfter time.Duration
	pools           map[string]config.PoolConfig
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
	staleAfter := opts.QuotaStaleAfter
	if staleAfter <= 0 {
		staleAfter = config.DefaultQuotaStaleAfter
	}
	nowFn := opts.Now
	if nowFn == nil {
		nowFn = time.Now
	}

	s := &Server{
		store:           st,
		instanceID:      opts.InstanceID,
		maxClaimed:      maxClaimed,
		token:           opts.Token,
		requireAuth:     opts.RequireAuth,
		logger:          logger,
		gateMode:        gateMode,
		quotaUsageFile:  strings.TrimSpace(opts.QuotaUsageFile),
		quotaStaleAfter: staleAfter,
		pools:           opts.Pools,
		nowFn:           nowFn,
		snapshotLoader:  opts.SnapshotLoader,
	}

	if s.token == "" && !s.requireAuth {
		s.logger.Printf("WARNING: api.token is empty; HTTP API is running without authentication (intended for localhost only)")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /jobs", s.handleCreateJob)
	mux.HandleFunc("GET /jobs", s.handleListJobs)
	mux.HandleFunc("GET /jobs/{id}", s.handleGetJob)
	mux.HandleFunc("POST /jobs/{id}/claim", s.handleClaimJob)
	mux.HandleFunc("POST /jobs/{id}/complete", s.handleCompleteJob)
	mux.HandleFunc("POST /jobs/{id}/fail", s.handleFailJob)
	mux.HandleFunc("POST /jobs/{id}/cancel", s.handleCancelJob)
	mux.HandleFunc("POST /jobs/{id}/cancel-claimed", s.handleCancelClaimedJob)
	mux.HandleFunc("POST /jobs/{id}/requeue", s.handleRequeueJob)
	mux.HandleFunc("POST /jobs/{id}/reset", s.handleResetJob)
	mux.HandleFunc("POST /jobs/{id}/reject-invalid", s.handleRejectInvalidJob)
	mux.HandleFunc("POST /jobs/{id}/deliver", s.handleDeliverJob)
	mux.HandleFunc("GET /status", s.handleStatus)
	mux.HandleFunc("GET /deliveries", s.handleDeliveries)
	mux.HandleFunc("GET /gate/held", s.handleListHeldJobs)
	mux.HandleFunc("DELETE /gate/held/{id}", s.handleCancelHeldJob)

	s.handler = s.authMiddleware(mux)
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
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.token == "" {
			if s.requireAuth {
				writeError(w, http.StatusUnauthorized, "unauthorized: server api token is not configured")
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		authHeader := r.Header.Get("Authorization")
		scheme, token, ok := strings.Cut(authHeader, " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") {
			writeError(w, http.StatusUnauthorized, "unauthorized: missing or invalid bearer token")
			return
		}
		if subtle.ConstantTimeCompare([]byte(token), []byte(s.token)) != 1 {
			writeError(w, http.StatusUnauthorized, "unauthorized: invalid bearer token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleCreateJob(w http.ResponseWriter, r *http.Request) {
	var req CreateJobRequest
	if err := decodeJSON(w, r, &req, false); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
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

	items, err := s.store.ListHeldJobs(r.Context(), status, limit)
	if err != nil {
		if errors.Is(err, store.ErrInvalidInput) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, ListHeldJobsResponse{
		Held: toHeldJobDTOs(items),
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
		cursor int64
		limit  int
		desc   bool
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

	page, err := s.store.ListJobs(r.Context(), store.ListJobsFilter{
		Status:     status,
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

func (s *Server) handleDeliveries(w http.ResponseWriter, r *http.Request) {
	var limit int
	if v := strings.TrimSpace(r.URL.Query().Get("limit")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid limit %q", v))
			return
		}
		limit = n
	}

	jobs, err := s.store.ListUndelivered(r.Context(), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, DeliveriesResponse{
		Jobs: toJobDTOs(jobs),
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

package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/chain"
	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

// Chain run endpoints (docs/22 §9).

// ChainRunDTO is a chain run.
type ChainRunDTO struct {
	ID                string     `json:"id"`
	RequestID         string     `json:"request_id"`
	Owner             string     `json:"owner"`
	Title             string     `json:"title"`
	Scope             string     `json:"scope,omitempty"`
	DefinitionHash    string     `json:"definition_hash"`
	DefinitionVersion int        `json:"definition_version"`
	Status            string     `json:"status"`
	CurrentSeq        int        `json:"current_seq"`
	Revision          int        `json:"revision"`
	Round             int        `json:"round"`
	MaxRounds         int        `json:"max_rounds"`
	MaxSteps          int        `json:"max_steps"`
	StopReason        string     `json:"stop_reason,omitempty"`
	StopDetail        string     `json:"stop_detail,omitempty"`
	NextCheckAt       *time.Time `json:"next_check_at,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

func toChainRunDTO(r *store.ChainRun) *ChainRunDTO {
	if r == nil {
		return nil
	}
	return &ChainRunDTO{
		ID: r.ID, RequestID: r.RequestID, Owner: r.Owner, Title: r.Title, Scope: r.Scope,
		DefinitionHash: r.DefinitionHash, DefinitionVersion: r.DefinitionVersion,
		Status: string(r.Status), CurrentSeq: r.CurrentSeq, Revision: r.Revision, Round: r.Round,
		MaxRounds: r.MaxRounds, MaxSteps: r.MaxSteps, StopReason: r.StopReason, StopDetail: r.StopDetail,
		NextCheckAt: r.NextCheckAt, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}

// ChainVerdictDTO is a captured review verdict.
type ChainVerdictDTO struct {
	Status         string    `json:"status"`
	Verdict        string    `json:"verdict,omitempty"`
	ReviewedCommit string    `json:"reviewed_commit,omitempty"`
	RawSHA256      string    `json:"raw_sha256,omitempty"`
	Reason         string    `json:"reason,omitempty"`
	CapturedAt     time.Time `json:"captured_at"`
}

// ChainStepDTO is one step of a run with the state of its job or hold.
type ChainStepDTO struct {
	Seq            int              `json:"seq"`
	Step           string           `json:"step"`
	Round          int              `json:"round"`
	RequestID      string           `json:"request_id"`
	JobID          *int64           `json:"job_id,omitempty"`
	JobStatus      string           `json:"job_status,omitempty"`
	HeldID         string           `json:"held_id,omitempty"`
	HeldStatus     string           `json:"held_status,omitempty"`
	DecidedFromSeq *int             `json:"decided_from_seq,omitempty"`
	DecidedBranch  string           `json:"decided_branch,omitempty"`
	Verdict        *ChainVerdictDTO `json:"verdict,omitempty"`
	CreatedAt      time.Time        `json:"created_at"`
}

// ChainNotificationDTO is the delivery state of one chain notification.
type ChainNotificationDTO struct {
	EventID       string     `json:"event_id"`
	Kind          string     `json:"kind"`
	Revision      int        `json:"revision"`
	Attempts      int        `json:"attempts"`
	LastError     string     `json:"last_error,omitempty"`
	NextAttemptAt *time.Time `json:"next_attempt_at,omitempty"`
	SentAt        *time.Time `json:"sent_at,omitempty"`
}

// ChainDetailResponse is returned by GET /chains/{id} and POST /chains.
type ChainDetailResponse struct {
	Run           *ChainRunDTO           `json:"run"`
	Steps         []ChainStepDTO         `json:"steps"`
	Notifications []ChainNotificationDTO `json:"notifications"`
	Created       *bool                  `json:"created,omitempty"`
}

// ListChainsResponse is returned by GET /chains.
type ListChainsResponse struct {
	Runs       []*ChainRunDTO `json:"runs"`
	NextCursor string         `json:"next_cursor,omitempty"`
	HasMore    bool           `json:"has_more"`
}

// CancelChainResponse is returned by POST /chains/{id}/cancel.
type CancelChainResponse struct {
	Run              *ChainRunDTO `json:"run"`
	AlreadyCancelled bool         `json:"already_cancelled"`
	CancelledJobIDs  []int64      `json:"cancelled_job_ids"`
	CancelledHeldID  string       `json:"cancelled_held_id,omitempty"`
	RunningJobID     *int64       `json:"running_job_id,omitempty"`
	Warning          string       `json:"warning,omitempty"`
}

// ChainStatusDTO is the chains section of GET /status.
type ChainStatusDTO struct {
	Counts                    map[string]int64 `json:"counts"`
	DriverLastSweepAt         *time.Time       `json:"driver_last_sweep_at,omitempty"`
	DriverStale               bool             `json:"driver_stale"`
	PendingNotifications      int64            `json:"pending_notifications"`
	OldestPendingNotification *time.Time       `json:"oldest_pending_notification_at,omitempty"`
}

func (s *Server) chainStatus(ctx context.Context) *ChainStatusDTO {
	out := &ChainStatusDTO{Counts: map[string]int64{}}
	for _, st := range []store.ChainRunStatus{store.ChainRunRunning, store.ChainRunCompleted, store.ChainRunStopped, store.ChainRunCancelled} {
		out.Counts[string(st)] = 0
	}
	if counts, err := s.store.CountChainRunsByStatus(ctx); err == nil {
		for k, v := range counts {
			out.Counts[string(k)] = v
		}
	}
	if raw, err := s.store.GetMeta(ctx, store.MetaKeyChainDriverLastSweep); err == nil {
		if t, perr := time.Parse(time.RFC3339Nano, raw); perr == nil {
			out.DriverLastSweepAt = &t
			if s.chains.DriverStaleAfter > 0 && s.nowFn().Sub(t) > s.chains.DriverStaleAfter {
				out.DriverStale = true
			}
		}
	} else if s.chains.DriverStaleAfter > 0 && out.Counts[string(store.ChainRunRunning)] > 0 {
		out.DriverStale = true
	}
	if st, err := s.store.ChainNotificationPendingStats(ctx); err == nil {
		out.PendingNotifications = st.Pending
		out.OldestPendingNotification = st.OldestPending
	}
	return out
}

func (s *Server) chainMaxRoundsCap() int {
	if s.chains.MaxRoundsCap > 0 {
		return s.chains.MaxRoundsCap
	}
	return chain.DefaultMaxRoundsCap
}

func (s *Server) handleCreateChain(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, chain.MaxDefinitionBytes+1))
	if err != nil || len(body) > chain.MaxDefinitionBytes {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("%v: definition exceeds %d bytes or could not be read", chain.ErrInvalidDefinition, chain.MaxDefinitionBytes))
		return
	}
	def, err := chain.ParseDefinition(body, s.chainMaxRoundsCap())
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	defJSON, defHash, err := def.CanonicalJSON()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	owner := ""
	if p, ok := authPrincipalFromContext(ctx); ok {
		owner = p.name
	}

	if existing, err := s.store.GetChainRunByRequestID(ctx, def.RequestID); err == nil {
		if existing.DefinitionHash != defHash || existing.Owner != owner {
			writeError(w, http.StatusConflict, fmt.Sprintf("%v: chain request_id %s already registered with a different definition or owner", store.ErrConflict, def.RequestID))
			return
		}
		s.writeChainDetail(w, r, existing, http.StatusOK, boolPtr(false))
		return
	} else if !errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	runID, err := chain.NewRunID()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	stub := &store.ChainRun{ID: runID, Scope: def.Scope, Owner: owner}
	first, reject, err := chain.PlanStep(ctx, s.adm, def, chain.StepInput{Run: stub, Step: store.ChainStepImplement, Seq: 1, Round: 0})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if reject != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("admission rejected the implement step: %v", reject))
		return
	}
	run, created, err := s.store.CreateChainRun(ctx, store.CreateChainRunParams{
		RequestID:         def.RequestID,
		Owner:             owner,
		Title:             def.Title,
		Scope:             def.Scope,
		DefinitionJSON:    defJSON,
		DefinitionHash:    defHash,
		DefinitionVersion: chain.DefinitionVersion,
		MaxRounds:         def.MaxRounds,
		MaxSteps:          chain.MaxSteps(def.MaxRounds),
		RunID:             runID,
		FirstStep:         store.ChainStepImplement,
		First:             first,
	})
	if err != nil {
		switch {
		case errors.Is(err, store.ErrConflict):
			writeError(w, http.StatusConflict, err.Error())
		case errors.Is(err, store.ErrInvalidInput):
			writeError(w, http.StatusBadRequest, err.Error())
		default:
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	s.writeChainDetail(w, r, run, status, boolPtr(created))
}

func boolPtr(b bool) *bool { return &b }

func (s *Server) handleListChains(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.ListChainRunsFilter{
		Status: store.ChainRunStatus(strings.TrimSpace(q.Get("status"))),
		Scope:  q.Get("scope"),
		Cursor: q.Get("cursor"),
	}
	if v := strings.TrimSpace(q.Get("limit")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid limit %q", v))
			return
		}
		f.Limit = n
	}
	page, err := s.store.ListChainRunsPage(r.Context(), f)
	if err != nil {
		if errors.Is(err, store.ErrInvalidInput) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := ListChainsResponse{Runs: []*ChainRunDTO{}, NextCursor: page.NextCursor, HasMore: page.HasMore}
	for _, run := range page.Runs {
		out.Runs = append(out.Runs, toChainRunDTO(run))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGetChain(w http.ResponseWriter, r *http.Request) {
	run, err := s.store.GetChainRun(r.Context(), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.writeChainDetail(w, r, run, http.StatusOK, nil)
}

func (s *Server) writeChainDetail(w http.ResponseWriter, r *http.Request, run *store.ChainRun, status int, created *bool) {
	ctx := r.Context()
	steps, err := s.store.ListChainSteps(ctx, run.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := ChainDetailResponse{Run: toChainRunDTO(run), Steps: []ChainStepDTO{}, Notifications: []ChainNotificationDTO{}, Created: created}
	for _, st := range steps {
		dto := ChainStepDTO{
			Seq: st.Seq, Step: st.Step, Round: st.Round, RequestID: st.RequestID, JobID: st.JobID,
			HeldID: st.HeldID, DecidedFromSeq: st.DecidedFromSeq, DecidedBranch: st.DecidedBranch, CreatedAt: st.CreatedAt,
		}
		if st.JobID != nil {
			if job, err := s.store.GetJob(ctx, *st.JobID); err == nil {
				dto.JobStatus = string(job.Status)
			}
			if a, err := s.store.GetChainArtifact(ctx, *st.JobID); err == nil {
				dto.Verdict = &ChainVerdictDTO{Status: a.Status, Verdict: a.Verdict, ReviewedCommit: a.ReviewedCommit,
					RawSHA256: a.RawSHA256, Reason: a.Reason, CapturedAt: a.CapturedAt}
			}
		}
		if st.HeldID != "" {
			if h, err := s.store.GetHeldJob(ctx, st.HeldID); err == nil {
				dto.HeldStatus = string(h.Status)
			}
		}
		out.Steps = append(out.Steps, dto)
	}
	if ns, err := s.store.ListChainNotifications(ctx, run.ID); err == nil {
		for _, n := range ns {
			out.Notifications = append(out.Notifications, ChainNotificationDTO{
				EventID: n.EventID(), Kind: n.Kind, Revision: n.Revision, Attempts: n.Attempts,
				LastError: n.LastError, NextAttemptAt: n.NextAttemptAt, SentAt: n.SentAt,
			})
		}
	}
	writeJSON(w, status, out)
}

func (s *Server) handleCancelChain(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p, ok := authPrincipalFromContext(ctx)
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	run, err := s.store.GetChainRun(ctx, r.PathValue("id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// docs/22 §9 / §15-4 (decided 2026-11-26): operate, or the token that registered the run.
	if !p.hasScope(config.ScopeOperate) && (run.Owner == "" || p.name != run.Owner) {
		writeJSON(w, http.StatusForbidden, ForbiddenResponse{
			Error:         "forbidden: cancelling a chain run needs the operate scope or the token that registered it",
			RequiredScope: config.ScopeOperate,
		})
		return
	}
	var req ReasonRequest
	if err := decodeJSON(w, r, &req, true); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	res, err := s.store.CancelChainRun(ctx, run.ID, req.effectiveReason())
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			resp := map[string]any{"error": err.Error()}
			if res != nil {
				resp["run"] = toChainRunDTO(res.Run)
			}
			writeJSON(w, http.StatusConflict, resp)
			return
		}
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := CancelChainResponse{
		Run:              toChainRunDTO(res.Run),
		AlreadyCancelled: res.AlreadyCancelled,
		CancelledJobIDs:  res.CancelledJobIDs,
		CancelledHeldID:  res.CancelledHeldID,
		RunningJobID:     res.RunningJobID,
	}
	if out.CancelledJobIDs == nil {
		out.CancelledJobIDs = []int64{}
	}
	if res.RunningJobID != nil {
		out.Warning = fmt.Sprintf("job #%d is already running and was not stopped; its external side effects may remain. The run will not create further steps.", *res.RunningJobID)
	}
	writeJSON(w, http.StatusOK, out)
}



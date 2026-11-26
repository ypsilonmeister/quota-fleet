package api

import (
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/agentver"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

// JobDTO is the JSON representation of a job returned by the HTTP API.
type JobDTO struct {
	ID                 int64      `json:"id"`
	RequestID          string     `json:"request_id"`
	Title              string     `json:"title"`
	Status             string     `json:"status"`
	Agent              string     `json:"agent"`
	Host               string     `json:"host"`
	Workdir            string              `json:"workdir"`
	WorktreeSpec       *store.WorktreeSpec `json:"worktree_spec,omitempty"`
	TargetRef          string              `json:"target_ref"`
	Instructions       string     `json:"instructions"`
	AcceptanceCriteria string     `json:"acceptance_criteria"`
	ParentID           *int64     `json:"parent_id,omitempty"`
	ClaimedBy          string     `json:"claimed_by,omitempty"`
	ClaimedAt          *time.Time `json:"claimed_at,omitempty"`
	Attempts           int        `json:"attempts"`
	ResultText         string     `json:"result_text,omitempty"`
	LastError          string     `json:"last_error,omitempty"`
	DeliveredAt        *time.Time `json:"delivered_at,omitempty"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
	Model              string     `json:"model,omitempty"`
	Pool               string     `json:"pool,omitempty"`
	AltPools           []string   `json:"alt_pools,omitempty"`
	SessionID          string     `json:"session_id,omitempty"`
	CreatedBy          string     `json:"created_by,omitempty"`

	SelectedAgent   string `json:"selected_agent,omitempty"`
	SelectedModel   string `json:"selected_model,omitempty"`
	SelectedPool    string `json:"selected_pool,omitempty"`
	SelectionReason string `json:"selection_reason,omitempty"`
	SelectionMode   string `json:"selection_mode,omitempty"`

	// Effort is the per-job reasoning effort (docs/21); omitted when unset (agent default).
	Effort string `json:"effort,omitempty"`

	// ChainRunID / ChainSeq identify the chain run step this job belongs to (docs/22).
	ChainRunID string `json:"chain_run_id,omitempty"`
	ChainSeq   int    `json:"chain_seq,omitempty"`
}

func toJobDTO(j *store.Job) *JobDTO {
	if j == nil {
		return nil
	}
	dto := &JobDTO{
		ID:                 j.ID,
		RequestID:          j.RequestID,
		Title:              j.Title,
		Status:             string(j.Status),
		Agent:              j.Agent,
		Host:               j.Host,
		Workdir:            j.Workdir,
		WorktreeSpec:       cloneWorktreeSpec(j.WorktreeSpec),
		TargetRef:          j.TargetRef,
		Instructions:       j.Instructions,
		AcceptanceCriteria: j.AcceptanceCriteria,
		ParentID:           j.ParentID,
		ClaimedBy:          j.ClaimedBy,
		ClaimedAt:          j.ClaimedAt,
		Attempts:           j.Attempts,
		ResultText:         j.ResultText,
		LastError:          j.LastError,
		DeliveredAt:        j.DeliveredAt,
		CreatedAt:          j.CreatedAt,
		UpdatedAt:          j.UpdatedAt,
		Model:              j.Model,
		Pool:               j.Pool,
		SessionID:          j.SessionID,
		CreatedBy:          j.CreatedBy,
	}
	if len(j.AltPools) > 0 {
		dto.AltPools = append([]string(nil), j.AltPools...)
	}
	dto.SelectedAgent = j.SelectedAgent
	dto.SelectedModel = j.SelectedModel
	dto.SelectedPool = j.SelectedPool
	dto.SelectionReason = j.SelectionReason
	dto.SelectionMode = j.SelectionMode
	dto.Effort = j.Effort
	dto.ChainRunID = j.ChainRunID
	dto.ChainSeq = j.ChainSeq
	return dto
}

func toJobDTOs(jobs []*store.Job) []*JobDTO {
	out := make([]*JobDTO, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, toJobDTO(j))
	}
	return out
}

func cloneWorktreeSpec(spec *store.WorktreeSpec) *store.WorktreeSpec {
	if spec == nil {
		return nil
	}
	cp := *spec
	return &cp
}

// CreateJobRequest is the JSON request body for POST /jobs.
type CreateJobRequest struct {
	RequestID          string   `json:"request_id"`
	Title              string   `json:"title"`
	Draft              bool     `json:"draft"`
	Agent              string   `json:"agent"`
	Host               string   `json:"host"`
	Workdir            string              `json:"workdir"`
	WorktreeSpec       *store.WorktreeSpec `json:"worktree_spec,omitempty"`
	TargetRef          string              `json:"target_ref"`
	Instructions       string   `json:"instructions"`
	AcceptanceCriteria string   `json:"acceptance_criteria"`
	ParentID           *int64   `json:"parent_id"`
	Model              string   `json:"model"`
	Pool               string   `json:"pool"`
	AltPools           []string `json:"alt_pools"`
	SessionID          string   `json:"session_id"`
	// Selector hints (docs/19). Optional.
	Role   string `json:"role"`
	Weight string `json:"weight"`
	// Effort is the optional per-job reasoning effort (docs/21): low|medium|high|xhigh.
	Effort string `json:"effort"`
}

// CreateJobResponse is returned by POST /jobs.
type CreateJobResponse struct {
	Job     *JobDTO `json:"job"`
	Created bool    `json:"created"`
}

// ListJobsResponse is returned by GET /jobs.
type ListJobsResponse struct {
	Jobs       []*JobDTO `json:"jobs"`
	NextCursor int64     `json:"next_cursor,omitempty"`
	HasMore    bool      `json:"has_more"`
}

// ClaimResponse is returned by POST /jobs/{id}/claim.
type ClaimResponse struct {
	Claimed bool    `json:"claimed"`
	Token   string  `json:"token,omitempty"`
	Job     *JobDTO `json:"job,omitempty"`
}

// CompleteRequest is the JSON body for POST /jobs/{id}/complete.
type CompleteRequest struct {
	Token      string `json:"token"`
	ClaimToken string `json:"claim_token"`
	ResultText string `json:"result_text"`
}

func (r CompleteRequest) effectiveToken() string {
	if r.Token != "" {
		return r.Token
	}
	return r.ClaimToken
}

// CompleteResponse is returned by POST /jobs/{id}/complete.
type CompleteResponse struct {
	Job              *JobDTO `json:"job"`
	PromotedChildIDs []int64 `json:"promoted_child_ids"`
}

// FailRequest is the JSON body for POST /jobs/{id}/fail and POST /jobs/{id}/cancel-claimed.
type FailRequest struct {
	Token      string `json:"token"`
	ClaimToken string `json:"claim_token"`
	LastError  string `json:"last_error"`
	Reason     string `json:"reason"`
}

func (r FailRequest) effectiveToken() string {
	if r.Token != "" {
		return r.Token
	}
	return r.ClaimToken
}

func (r FailRequest) effectiveReason() string {
	if r.LastError != "" {
		return r.LastError
	}
	return r.Reason
}

// ReasonRequest is the JSON body for POST /jobs/{id}/cancel and POST /jobs/{id}/reject-invalid.
type ReasonRequest struct {
	LastError string `json:"last_error"`
	Reason    string `json:"reason"`
}

func (r ReasonRequest) effectiveReason() string {
	if r.LastError != "" {
		return r.LastError
	}
	return r.Reason
}

// CascadeResponse is returned by fail, cancel, cancel-claimed, and reject-invalid.
type CascadeResponse struct {
	Job               *JobDTO `json:"job"`
	CancelledChildIDs []int64 `json:"cancelled_child_ids"`
}

// JobActionResponse is returned by requeue, reset, and deliver.
type JobActionResponse struct {
	Job *JobDTO `json:"job"`
}

// StatusResponse is returned by GET /status.
type StatusResponse struct {
	InstanceID string           `json:"instance_id"`
	Counts     map[string]int64 `json:"counts"`
	Total      int64            `json:"total"`
	// Chains summarizes chain runs and the Chain Driver (docs/22 §6, §8).
	Chains *ChainStatusDTO `json:"chains,omitempty"`
}

// DeliveriesResponse is returned by GET /deliveries.
type DeliveriesResponse struct {
	Jobs       []*JobDTO `json:"jobs"`
	NextCursor string    `json:"next_cursor,omitempty"`
	HasMore    bool      `json:"has_more"`
}

// ErrorResponse is the uniform JSON error envelope.
type ErrorResponse struct {
	Error string `json:"error"`
}

// ForbiddenResponse is returned on 403 Forbidden when an authenticated token lacks a required scope.
type ForbiddenResponse struct {
	Error         string `json:"error"`
	RequiredScope string `json:"required_scope"`
}

// ConflictResponse is returned on 409 Conflict with the job's current state.
type ConflictResponse struct {
	Error         string  `json:"error"`
	CurrentStatus string  `json:"current_status,omitempty"`
	Job           *JobDTO `json:"job,omitempty"`
}

// HeldJobDTO is the JSON representation of a held_jobs entry.
type HeldJobDTO struct {
	ID              string                `json:"id"`
	RequestID       string                `json:"request_id"`
	Status          string                `json:"status"`
	Pool            string                `json:"pool"`
	AltPools        []string              `json:"alt_pools,omitempty"`
	Payload         store.CreateJobParams `json:"payload"`
	Reason          string                `json:"reason,omitempty"`
	SnapshotSummary string                `json:"snapshot_summary,omitempty"`
	ReleasedJobID   *int64                `json:"released_job_id,omitempty"`
	ReleasedPool    string                `json:"released_pool,omitempty"`
	ReleasedAt      *time.Time            `json:"released_at,omitempty"`
	CancelledAt     *time.Time            `json:"cancelled_at,omitempty"`
	LastError       string                `json:"last_error,omitempty"`
	ErrorCount      int                   `json:"error_count,omitempty"`
	CreatedAt       time.Time             `json:"created_at"`
	UpdatedAt       time.Time             `json:"updated_at"`
	CancelReason    string                `json:"cancel_reason,omitempty"`
	ChainRunID      string                `json:"chain_run_id,omitempty"`
	ChainSeq        int                   `json:"chain_seq,omitempty"`
}

func toHeldJobDTO(h *store.HeldJob) *HeldJobDTO {
	if h == nil {
		return nil
	}
	dto := &HeldJobDTO{
		ID:              h.ID,
		RequestID:       h.RequestID,
		Status:          string(h.Status),
		Pool:            h.Pool,
		Payload:         h.Payload,
		Reason:          h.Reason,
		SnapshotSummary: h.SnapshotSummary,
		ReleasedJobID:   h.ReleasedJobID,
		ReleasedPool:    h.ReleasedPool,
		ReleasedAt:      h.ReleasedAt,
		CancelledAt:     h.CancelledAt,
		LastError:       h.LastError,
		ErrorCount:      h.ErrorCount,
		CreatedAt:       h.CreatedAt,
		UpdatedAt:       h.UpdatedAt,
	}
	if len(h.AltPools) > 0 {
		dto.AltPools = append([]string(nil), h.AltPools...)
	}
	dto.CancelReason = h.CancelReason
	dto.ChainRunID = h.ChainRunID
	dto.ChainSeq = h.ChainSeq
	return dto
}

func toHeldJobDTOs(items []*store.HeldJob) []*HeldJobDTO {
	out := make([]*HeldJobDTO, 0, len(items))
	for _, h := range items {
		out = append(out, toHeldJobDTO(h))
	}
	return out
}

// HoldJobResponse is returned with 202 Accepted by POST /jobs when Quota Gate holds a job.
type HoldJobResponse struct {
	HeldID  string      `json:"held_id"`
	Held    *HeldJobDTO `json:"held"`
	Created bool        `json:"created"`
}

// ListHeldJobsResponse is returned by GET /gate/held.
type ListHeldJobsResponse struct {
	Held       []*HeldJobDTO `json:"held"`
	NextCursor string        `json:"next_cursor,omitempty"`
	HasMore    bool          `json:"has_more"`
}

// HeldJobActionResponse is returned by DELETE /gate/held/{id}.
type HeldJobActionResponse struct {
	Held *HeldJobDTO `json:"held"`
}

// HeldConflictResponse is returned on 409 Conflict for held_jobs transitions.
type HeldConflictResponse struct {
	Error         string      `json:"error"`
	CurrentStatus string      `json:"current_status,omitempty"`
	Held          *HeldJobDTO `json:"held,omitempty"`
}

// ReleaseHeldResponse is returned by POST /gate/held/{id}/release.
type ReleaseHeldResponse struct {
	Released bool   `json:"released"`
	JobID    int64  `json:"job_id,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// GateDecisionDTO is the JSON representation of a gate_decisions entry.
type GateDecisionDTO struct {
	ID            int64      `json:"id"`
	JobID         int64      `json:"job_id,omitempty"`
	HeldID        string     `json:"held_id,omitempty"`
	Held          bool       `json:"held"`
	Mode          string     `json:"mode"`
	RequestedPool string     `json:"requested_pool,omitempty"`
	AltPools      []string   `json:"alt_pools,omitempty"`
	Decision      string     `json:"decision"`
	BlockedBy     string     `json:"blocked_by,omitempty"`
	SelectedPool  string     `json:"selected_pool,omitempty"`
	Bypassed      bool       `json:"bypassed"`
	Reason        string     `json:"reason,omitempty"`
	MeasuredAt    *time.Time `json:"measured_at,omitempty"`
	DecidedAt     time.Time  `json:"decided_at"`
}

func toGateDecisionDTO(d *store.GateDecision) *GateDecisionDTO {
	if d == nil {
		return nil
	}
	dto := &GateDecisionDTO{
		ID:            d.ID,
		JobID:         d.JobID,
		HeldID:        d.HeldID,
		Held:          d.Held,
		Mode:          d.Mode,
		RequestedPool: d.RequestedPool,
		Decision:      d.Decision,
		BlockedBy:     d.BlockedBy,
		SelectedPool:  d.SelectedPool,
		Bypassed:      d.Bypassed,
		Reason:        d.Reason,
		MeasuredAt:    d.MeasuredAt,
		DecidedAt:     d.DecidedAt,
	}
	if len(d.AltPools) > 0 {
		dto.AltPools = append([]string(nil), d.AltPools...)
	}
	return dto
}

func toGateDecisionDTOs(items []*store.GateDecision) []*GateDecisionDTO {
	out := make([]*GateDecisionDTO, 0, len(items))
	for _, d := range items {
		out = append(out, toGateDecisionDTO(d))
	}
	return out
}

// ListGateDecisionsResponse is returned by GET /gate/decisions.
type ListGateDecisionsResponse struct {
	Decisions  []*GateDecisionDTO `json:"decisions"`
	NextCursor int64              `json:"next_cursor,omitempty"`
	HasMore    bool               `json:"has_more"`
}

// PoolSummaryDTO describes the current quota summary for a configured pool.
type PoolSummaryDTO struct {
	Pool                   string     `json:"pool"`
	Status                 string     `json:"status"`
	Unmeasured             bool       `json:"unmeasured,omitempty"`
	BlockedBy              string     `json:"blocked_by,omitempty"`
	Reason                 string     `json:"reason,omitempty"`
	WeeklyRemainingPercent *int       `json:"weekly_remaining_percent,omitempty"`
	WeeklyResetAt          *time.Time `json:"weekly_reset_at,omitempty"`
	ShortRemainingPercent  *int       `json:"short_remaining_percent,omitempty"`
	ShortResetAt           *time.Time `json:"short_reset_at,omitempty"`
	MeasuredAt             *time.Time `json:"measured_at,omitempty"`
	ServiceStale           bool       `json:"service_stale,omitempty"`
}

// ModelDTO describes a selectable model exposed by GET /meta.
type ModelDTO struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name,omitempty"`
	Family      string `json:"family,omitempty"`
}

// PoolAppliesToDTO describes a pool's applies_to matching rule for UI pool preview.
type PoolAppliesToDTO struct {
	Agent       string `json:"agent"`
	ModelFamily string `json:"model_family,omitempty"`
}

// MetaResponse is returned by GET /meta with non-sensitive form candidates and runtime modes.
type MetaResponse struct {
	Hosts         []string                         `json:"hosts"`
	Agents        []string                         `json:"agents"`
	HostAgents    map[string][]string              `json:"host_agents,omitempty"`
	Models        map[string]map[string][]ModelDTO `json:"models"`
	Pools         []string                         `json:"pools"`
	PoolRules     map[string][]PoolAppliesToDTO    `json:"pool_rules,omitempty"`
	GateMode      string                           `json:"gate_mode"`
	SelectorMode  string                           `json:"selector_mode"`
	EvaluatorMode string                           `json:"evaluator_mode"`
	ReviewGate    ReviewGateDTO                    `json:"review_gate"`
	PoolSummaries []PoolSummaryDTO                 `json:"pool_summaries"`
	Scopes        []string                         `json:"scopes"`
}

// ReviewGateDTO is the review verdict gate state exposed by GET /meta (docs/18). It says
// only whether an API key is configured, never anything about its value.
type ReviewGateDTO struct {
	Enabled       bool   `json:"enabled"`
	Model         string `json:"model"`
	KeyConfigured bool   `json:"key_configured"`
}

// ReviewVerdictRequest is the body of POST /decision/review-verdict.
type ReviewVerdictRequest struct {
	Text        string `json:"text"`
	JobID       *int64 `json:"job_id,omitempty"`
	ReviewJobID *int64 `json:"review_job_id,omitempty"`
}

// ReviewVerdictResponse is returned by POST /decision/review-verdict.
type ReviewVerdictResponse struct {
	Verdict string  `json:"verdict"`
	Score   float64 `json:"score"`
	Reason  string  `json:"reason"`
}

// AgentVersionEventDTO is the JSON representation of an agent_version_events entry.
type AgentVersionEventDTO struct {
	ID           int64     `json:"id"`
	EventType    string    `json:"event_type"`
	Agent        string    `json:"agent"`
	Host         string    `json:"host,omitempty"`
	OldVersion   string    `json:"old_version,omitempty"`
	NewVersion   string    `json:"new_version,omitempty"`
	Detail       string    `json:"detail,omitempty"`
	ManualUpdate bool      `json:"manual_update,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}

func toAgentVersionEventDTO(ev *store.AgentVersionEvent) *AgentVersionEventDTO {
	if ev == nil {
		return nil
	}
	return &AgentVersionEventDTO{
		ID:           ev.ID,
		EventType:    string(ev.EventType),
		Agent:        ev.Agent,
		Host:         ev.Host,
		OldVersion:   ev.OldVersion,
		NewVersion:   ev.NewVersion,
		Detail:       ev.Detail,
		ManualUpdate: ev.ManualUpdate,
		CreatedAt:    ev.CreatedAt,
	}
}

func toAgentVersionEventDTOs(items []*store.AgentVersionEvent) []*AgentVersionEventDTO {
	out := make([]*AgentVersionEventDTO, 0, len(items))
	for _, ev := range items {
		out = append(out, toAgentVersionEventDTO(ev))
	}
	return out
}

// JobStatusEventDTO is the data of a "job_status" server-sent event on GET /events.
type JobStatusEventDTO struct {
	JobID  int64  `json:"job_id"`
	Title  string `json:"title"`
	Status string `json:"status"`
	// PrevStatus is empty for a newly created job.
	PrevStatus string    `json:"prev_status"`
	Agent      string    `json:"agent"`
	Host       string    `json:"host"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// VAPIDPublicKeyResponse is returned by GET /push/vapid-public-key.
type VAPIDPublicKeyResponse struct {
	PublicKey string `json:"public_key"`
}

// PushSubscriptionRequest is the body of POST /push/subscriptions, in the shape of the
// browser's PushSubscription.toJSON(). Other members are ignored.
type PushSubscriptionRequest struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
}

// PushSubscriptionResponse is returned by POST /push/subscriptions.
type PushSubscriptionResponse struct {
	// Created is false when the endpoint was already registered.
	Created bool `json:"created"`
}

// DeletePushSubscriptionRequest is the body of DELETE /push/subscriptions.
type DeletePushSubscriptionRequest struct {
	Endpoint string `json:"endpoint"`
}

// DeletePushSubscriptionResponse is returned by DELETE /push/subscriptions.
type DeletePushSubscriptionResponse struct {
	// Deleted is false when the endpoint was not registered.
	Deleted bool `json:"deleted"`
}

// AgentsResponse is returned by GET /agents with the latest version matrix and drift summaries.
type AgentsResponse struct {
	agentver.Matrix
}

// AgentEventsResponse is returned by GET /agents/events.
type AgentEventsResponse struct {
	Events []*AgentVersionEventDTO `json:"events"`
}

// AgentCheckResponse is returned by POST /agents/check with the updated matrix and newly recorded events.
type AgentCheckResponse struct {
	agentver.Matrix
	Events []*AgentVersionEventDTO `json:"events"`
}

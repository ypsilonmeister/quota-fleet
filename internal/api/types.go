package api

import (
	"time"

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
	Workdir            string     `json:"workdir"`
	TargetRef          string     `json:"target_ref"`
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
	}
	if len(j.AltPools) > 0 {
		dto.AltPools = append([]string(nil), j.AltPools...)
	}
	return dto
}

func toJobDTOs(jobs []*store.Job) []*JobDTO {
	out := make([]*JobDTO, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, toJobDTO(j))
	}
	return out
}

// CreateJobRequest is the JSON request body for POST /jobs.
type CreateJobRequest struct {
	RequestID          string   `json:"request_id"`
	Title              string   `json:"title"`
	Draft              bool     `json:"draft"`
	Agent              string   `json:"agent"`
	Host               string   `json:"host"`
	Workdir            string   `json:"workdir"`
	TargetRef          string   `json:"target_ref"`
	Instructions       string   `json:"instructions"`
	AcceptanceCriteria string   `json:"acceptance_criteria"`
	ParentID           *int64   `json:"parent_id"`
	Model              string   `json:"model"`
	Pool               string   `json:"pool"`
	AltPools           []string `json:"alt_pools"`
	SessionID          string   `json:"session_id"`
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
}

// DeliveriesResponse is returned by GET /deliveries.
type DeliveriesResponse struct {
	Jobs []*JobDTO `json:"jobs"`
}

// ErrorResponse is the uniform JSON error envelope.
type ErrorResponse struct {
	Error string `json:"error"`
}

// ConflictResponse is returned on 409 Conflict with the job's current state.
type ConflictResponse struct {
	Error         string  `json:"error"`
	CurrentStatus string  `json:"current_status,omitempty"`
	Job           *JobDTO `json:"job,omitempty"`
}

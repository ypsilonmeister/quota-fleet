package store

import (
	"errors"
	"time"
)

// Status represents one of the 7 job lifecycle states.
type Status string

const (
	StatusDraft     Status = "draft"
	StatusQueued    Status = "queued"
	StatusClaimed   Status = "claimed"
	StatusWaiting   Status = "waiting"
	StatusDone      Status = "done"
	StatusFailed    Status = "failed"
	StatusCancelled Status = "cancelled"
)

// AllStatuses lists all 7 valid job statuses in canonical order.
var AllStatuses = []Status{
	StatusDraft,
	StatusQueued,
	StatusClaimed,
	StatusWaiting,
	StatusDone,
	StatusFailed,
	StatusCancelled,
}

// Valid reports whether s is one of the 7 defined job statuses.
func (s Status) Valid() bool {
	switch s {
	case StatusDraft, StatusQueued, StatusClaimed, StatusWaiting, StatusDone, StatusFailed, StatusCancelled:
		return true
	default:
		return false
	}
}

const (
	DefaultListLimit = 50
	MaxListLimit     = 200
)

// Sentinel errors returned by Store methods.
var (
	ErrNotFound           = errors.New("not found")
	ErrInvalidInput       = errors.New("invalid input")
	ErrParentNotFound     = errors.New("parent job not found")
	ErrParentTerminal     = errors.New("parent job is in terminal failed/cancelled state")
	ErrConflict           = errors.New("status conflict")
	ErrTokenMismatch      = errors.New("claim token mismatch")
	ErrInstanceIDMismatch = errors.New("instance_id mismatch")
	ErrJobAlreadyExists   = errors.New("job already exists for request_id")
)

// WorktreeSpec defines optional git worktree creation parameters for a job.
type WorktreeSpec struct {
	RepoPath string `json:"repo_path"`
	BaseRef  string `json:"base_ref,omitempty"`
	Branch   string `json:"branch,omitempty"`
}

// Job represents a single row in the jobs table.
type Job struct {
	ID                 int64
	RequestID          string
	Title              string
	Status             Status
	Agent              string
	Host               string
	Workdir            string
	WorktreeSpec       *WorktreeSpec
	TargetRef          string
	Instructions       string
	AcceptanceCriteria string
	ParentID           *int64
	ClaimedBy          string
	ClaimedAt          *time.Time
	Attempts           int
	ResultText         string
	LastError          string
	DeliveredAt        *time.Time
	CreatedAt          time.Time
	UpdatedAt          time.Time
	Model              string
	Pool               string
	AltPools           []string
	SessionID          string
	CreatedBy          string
	SupersedeKey       string

	// Selector record (docs/19). Empty when the Selector did not run (mode off).
	SelectedAgent   string
	SelectedModel   string
	SelectedPool    string
	SelectionReason string
	SelectionMode   string // explicit | recorded | applied
}

// CreateJobParams holds input fields for CreateJob.
type CreateJobParams struct {
	RequestID          string        `json:"request_id"`
	Title              string        `json:"title"`
	Draft              bool          `json:"draft,omitempty"`
	Agent              string        `json:"agent,omitempty"`
	Host               string        `json:"host,omitempty"`
	Workdir            string        `json:"workdir,omitempty"`
	WorktreeSpec       *WorktreeSpec `json:"worktree_spec,omitempty"`
	TargetRef          string        `json:"target_ref,omitempty"`
	Instructions       string        `json:"instructions,omitempty"`
	AcceptanceCriteria string        `json:"acceptance_criteria,omitempty"`
	ParentID           *int64        `json:"parent_id,omitempty"`
	Model              string        `json:"model,omitempty"`
	Pool               string        `json:"pool,omitempty"`
	AltPools           []string      `json:"alt_pools,omitempty"`
	SessionID          string        `json:"session_id,omitempty"`
	CreatedBy          string        `json:"created_by,omitempty"`
	// SupersedeKey, when non-empty, cancels older unstarted registrations with the same
	// key (docs/20).
	SupersedeKey string `json:"supersede_key,omitempty"`
	// SupersedeOwnOnly limits that to registrations with the same CreatedBy, for a caller
	// that may register jobs but not cancel them. It is recorded with the registration,
	// not in a held payload.
	SupersedeOwnOnly bool `json:"-"`

	SelectedAgent   string `json:"selected_agent,omitempty"`
	SelectedModel   string `json:"selected_model,omitempty"`
	SelectedPool    string `json:"selected_pool,omitempty"`
	SelectionReason string `json:"selection_reason,omitempty"`
	SelectionMode   string `json:"selection_mode,omitempty"`
}

// CompleteResult holds the outcome of Complete.
type CompleteResult struct {
	Job              *Job
	PromotedChildIDs []int64
}

// FailResult holds the outcome of Fail or RejectInvalid.
type FailResult struct {
	Job               *Job
	CancelledChildIDs []int64
}

// CancelResult holds the outcome of Cancel or CancelClaimed.
type CancelResult struct {
	Job               *Job
	CancelledChildIDs []int64
}

// ListJobsFilter specifies filtering and cursor pagination for ListJobs.
// Status is mandatory.
type ListJobsFilter struct {
	Status     Status
	Agent      string
	Host       string
	Query      string
	ParentID   *int64
	Cursor     int64
	Limit      int
	Descending bool
}

// DeliveryStatus selects which completed/failed jobs are returned by ListDeliveries.
type DeliveryStatus string

const (
	DeliveryStatusUndelivered DeliveryStatus = "undelivered"
	DeliveryStatusDelivered   DeliveryStatus = "delivered"
	DeliveryStatusAll         DeliveryStatus = "all"
)

// Valid reports whether s is a supported DeliveryStatus value.
func (s DeliveryStatus) Valid() bool {
	switch s {
	case DeliveryStatusUndelivered, DeliveryStatusDelivered, DeliveryStatusAll:
		return true
	default:
		return false
	}
}

// GateDecisionsFilter specifies optional filters and cursor pagination for ListGateDecisionsPage.
type GateDecisionsFilter struct {
	JobID  *int64
	HeldID string
	Cursor int64
	Limit  int
}

// ListGateDecisionsPage holds one page of gate_decisions results.
type ListGateDecisionsPage struct {
	Decisions  []*GateDecision
	NextCursor int64
	HasMore    bool
}

// ListDeliveriesFilter specifies filtering and cursor pagination for ListDeliveriesPage.
type ListDeliveriesFilter struct {
	Status DeliveryStatus
	Cursor string
	Limit  int
}

// ListDeliveriesPage holds one page of delivery results.
type ListDeliveriesPage struct {
	Jobs       []*Job
	NextCursor string
	HasMore    bool
}

// ListHeldJobsFilter specifies filtering and cursor pagination for ListHeldJobsPage.
type ListHeldJobsFilter struct {
	Status HeldStatus
	Cursor string
	Limit  int
}

// ListHeldJobsPage holds one page of held_jobs results.
type ListHeldJobsPage struct {
	Held       []*HeldJob
	NextCursor string
	HasMore    bool
}

// ListJobsPage holds one page of ListJobs results.
type ListJobsPage struct {
	Jobs       []*Job
	NextCursor int64
	HasMore    bool
}

// OutboxEvent represents a row in signal_outbox.
type OutboxEvent struct {
	ID           int64
	EventID      string
	JobID        int64
	Env          string
	Src          string
	Hop          int
	Event        string
	CreatedAt    time.Time
	DispatchedAt *time.Time
}

// GateDecision represents a row in gate_decisions recording a Quota Gate verdict.
type GateDecision struct {
	ID            int64
	JobID         int64
	HeldID        string
	Held          bool
	Mode          string
	RequestedPool string
	AltPools      []string
	Decision      string
	BlockedBy     string
	SelectedPool  string
	Bypassed      bool
	Reason        string
	MeasuredAt    *time.Time
	DecidedAt     time.Time
}

// HeldStatus represents the lifecycle state of a held_jobs entry.
type HeldStatus string

const (
	HeldStatusHeld      HeldStatus = "held"
	HeldStatusReleased  HeldStatus = "released"
	HeldStatusCancelled HeldStatus = "cancelled"
)

// Valid reports whether s is a valid HeldStatus.
func (s HeldStatus) Valid() bool {
	switch s {
	case HeldStatusHeld, HeldStatusReleased, HeldStatusCancelled:
		return true
	default:
		return false
	}
}

// HeldJob represents a row in held_jobs persisted by Quota Gate in enforce mode.
type HeldJob struct {
	ID              string
	RequestID       string
	Status          HeldStatus
	Pool            string
	AltPools        []string
	Payload         CreateJobParams
	Reason          string
	SnapshotSummary string
	ReleasedJobID   *int64
	ReleasedPool    string
	ReleasedAt      *time.Time
	CancelledAt     *time.Time
	LastError       string
	ErrorCount      int
	CreatedAt       time.Time
	UpdatedAt       time.Time
	// CancelReason is set when the system cancelled the entry (superseded, docs/20).
	CancelReason string
}

// HoldJobParams holds input fields for HoldJob.
type HoldJobParams struct {
	RequestID       string
	Pool            string
	AltPools        []string
	Payload         CreateJobParams
	Reason          string
	SnapshotSummary string
}

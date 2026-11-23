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
)

// Job represents a single row in the jobs table.
type Job struct {
	ID                 int64
	RequestID          string
	Title              string
	Status             Status
	Agent              string
	Host               string
	Workdir            string
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
}

// CreateJobParams holds input fields for CreateJob.
type CreateJobParams struct {
	RequestID          string
	Title              string
	Draft              bool
	Agent              string
	Host               string
	Workdir            string
	TargetRef          string
	Instructions       string
	AcceptanceCriteria string
	ParentID           *int64
	Model              string
	Pool               string
	AltPools           []string
	SessionID          string
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
	Cursor     int64
	Limit      int
	Descending bool
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

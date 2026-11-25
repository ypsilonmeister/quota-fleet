package adapter

import (
	"context"

	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

// Adapter abstracts host/agent execution operations (workdir setup, process launch,
// liveness inspection, and evidence collection). Implementations must not perform
// job store transitions directly.
type Adapter interface {
	PrepareWorkdir(ctx context.Context, job *store.Job) error
	Launch(ctx context.Context, job *store.Job) (LaunchInfo, error)
	Status(ctx context.Context, job *store.Job) (RunState, error)
	CollectEvidence(ctx context.Context, job *store.Job) (Evidence, error)
}

// LaunchInfo carries generic process/session metadata returned by Launch.
type LaunchInfo struct {
	PID       int               `json:"pid"`
	SessionID string            `json:"session_id,omitempty"`
	Meta      map[string]string `json:"meta,omitempty"`
}

// StateKind classifies the liveness of a launched job run.
type StateKind string

const (
	StateRunning StateKind = "running"
	StateExited  StateKind = "exited"
	StateMissing StateKind = "missing"
)

// RunState represents the observed runtime state of a job on its worker host.
type RunState struct {
	State           StateKind `json:"state"`
	ExitCode        int       `json:"exit_code"`
	ExitCodeUnknown bool      `json:"exit_code_unknown,omitempty"`
	Detail          string    `json:"detail,omitempty"`
}

// Evidence holds generic artifacts collected after a job exits for Evaluator inspection.
type Evidence struct {
	HasDiff         bool              `json:"has_diff"`
	DiffSummary     string            `json:"diff_summary,omitempty"`
	Artifacts       map[string]bool   `json:"artifacts,omitempty"`
	CommandOutputs  map[string]string `json:"command_outputs,omitempty"`
	FinalReport     string            `json:"final_report,omitempty"`
	ErrLogTail      string            `json:"err_log_tail,omitempty"`
	ExitCode        int               `json:"exit_code"`
	ExitCodeUnknown bool              `json:"exit_code_unknown,omitempty"`
	Notes           []string          `json:"notes,omitempty"`
}

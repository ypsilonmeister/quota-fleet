package store

import (
	"fmt"
	"strings"
	"time"
)

// Chain runs (docs/22, Issue #26): a chain run drives the fixed step sequence
// implement → review → (publish | fix → review). The Store persists runs, their step
// history, captured review verdicts and stop notifications; the branching itself is a pure
// function supplied by the caller (package chain) so that it is evaluated twice with the
// same code: once before admission and once inside the commit transaction.

// ChainRunStatus is the lifecycle state of a chain run.
type ChainRunStatus string

const (
	ChainRunRunning   ChainRunStatus = "running"
	ChainRunCompleted ChainRunStatus = "completed"
	ChainRunStopped   ChainRunStatus = "stopped"
	ChainRunCancelled ChainRunStatus = "cancelled"
)

// Valid reports whether s is a defined chain run status.
func (s ChainRunStatus) Valid() bool {
	switch s {
	case ChainRunRunning, ChainRunCompleted, ChainRunStopped, ChainRunCancelled:
		return true
	}
	return false
}

// Chain step names (C1 fixed shape).
const (
	ChainStepImplement = "implement"
	ChainStepReview    = "review"
	ChainStepFix       = "fix"
	ChainStepPublish   = "publish"
)

// Branch names recorded in chain_steps.decided_branch.
const (
	ChainBranchStart         = "start"
	ChainBranchDone          = "done"
	ChainBranchOKToMerge     = "ok_to_merge"
	ChainBranchChangesNeeded = "changes_needed"
)

// Stop reasons (docs/22 §8).
const (
	ChainStopVerdictUnavailable    = "verdict_unavailable"
	ChainStopStepFailed            = "step_failed"
	ChainStopStepCancelled         = "step_cancelled"
	ChainStopMaxRoundsExceeded     = "max_rounds_exceeded"
	ChainStopStepBudgetExceeded    = "step_budget_exceeded"
	ChainStopReleaseFailed         = "release_failed"
	ChainStopAdmissionRejected     = "admission_rejected"
	ChainStopInternalInconsistency = "internal_inconsistency"
)

// Chain notification kinds.
const (
	ChainNotifyStopped   = "chain_stopped"
	ChainNotifyStalled   = "chain_stalled"
	ChainNotifyCompleted = "chain_completed"
)

// Verdict artifact statuses (chain_artifacts.status).
const (
	ChainArtifactCaptured    = "captured"
	ChainArtifactMissing     = "missing"
	ChainArtifactInvalid     = "invalid"
	ChainArtifactUnavailable = "unavailable"
)

// ReservedRequestIDPrefix marks request_ids owned by chain runs. Jobs and holds registered
// through POST /jobs, the CLI or MCP may not use it (docs/22 §4.2).
const ReservedRequestIDPrefix = "chain:"

// IsReservedRequestID reports whether requestID uses the chain-reserved prefix.
func IsReservedRequestID(requestID string) bool {
	return strings.HasPrefix(strings.TrimSpace(requestID), ReservedRequestIDPrefix)
}

// ChainStepRequestID returns the request_id of step seq in run runID: chain:<run_id>:<seq>.
// It carries neither the branch nor the round, so one seq can never hold two steps.
func ChainStepRequestID(runID string, seq int) string {
	return fmt.Sprintf("%s%s:%d", ReservedRequestIDPrefix, runID, seq)
}

// ChainRun is a row of chain_runs.
type ChainRun struct {
	ID                string
	RequestID         string
	Owner             string
	Title             string
	Scope             string
	DefinitionJSON    string
	DefinitionHash    string
	DefinitionVersion int
	Status            ChainRunStatus
	CurrentSeq        int
	Revision          int
	Round             int
	MaxRounds         int
	MaxSteps          int
	StopReason        string
	StopDetail        string
	NextCheckAt       *time.Time
	CheckFailures     int
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// ChainStep is a row of chain_steps.
type ChainStep struct {
	RunID          string
	Seq            int
	Step           string
	Round          int
	RequestID      string
	JobID          *int64
	HeldID         string
	VerdictNonce   string
	DecidedFromSeq *int
	DecidedBranch  string
	CreatedAt      time.Time
}

// ChainArtifact is a row of chain_artifacts: the review verdict captured by Shepherd in
// the same transaction that completed the review job (docs/22 §7).
type ChainArtifact struct {
	JobID          int64
	RunID          string
	Seq            int
	Status         string
	Verdict        string
	ReviewedCommit string
	RawSHA256      string
	Reason         string
	CapturedAt     time.Time
}

// ChainNotification is a row of chain_notifications (docs/22 §8).
type ChainNotification struct {
	RunID         string
	Revision      int
	Kind          string
	PayloadJSON   string
	Attempts      int
	LastError     string
	NextAttemptAt *time.Time
	SentAt        *time.Time
	CreatedAt     time.Time
}

// EventID is the stable delivery key chain:<run_id>:<revision>:<kind>. Receivers drop
// duplicates by it (delivery is at-least-once).
func (n ChainNotification) EventID() string {
	return fmt.Sprintf("%s%s:%d:%s", ReservedRequestIDPrefix, n.RunID, n.Revision, n.Kind)
}

// ChainSnapshot is everything the branch function may look at for one run: the run row,
// the step at current_seq and the entity that step points to.
type ChainSnapshot struct {
	Run  *ChainRun
	Step *ChainStep // nil when chain_steps(current_seq) does not exist
	Job  *Job       // the step's job (nil when job_id is NULL or the row is missing)
	// JobMissing is true when job_id is set but no jobs row exists.
	JobMissing bool
	Held       *HeldJob // the step's hold (nil when held_id is NULL or the row is missing)
	// HeldMissing is true when held_id is set but no held_jobs row exists.
	HeldMissing bool
	// Artifact is the captured verdict of the step's job (review steps only).
	Artifact *ChainArtifact
	// ReleasedJob is the job a released hold points to while chain_steps.job_id is still
	// NULL (R10). ReleasedJobMissing is true when released_job_id names no jobs row.
	ReleasedJob        *Job
	ReleasedJobMissing bool
}

// OwnershipProblem returns a non-empty description when the step's job or hold does not
// belong to this run's current step (docs/22 §6 R10/R12). The branch function stops the
// run with internal_inconsistency on it, and CancelChainRun never touches a job that
// fails it. Rules:
//   - a step job must carry chain_run_id = run and chain_seq = step seq;
//   - a step with both job_id and held_id must point at a released hold whose
//     released_job_id is that job;
//   - a released hold of a step without job_id must point at an existing job created
//     from that hold (same request_id) that is unowned or owned by this step;
//   - the step's hold itself must carry chain_run_id = run and chain_seq = step seq
//     (HeldOwnershipProblem).
func (s ChainSnapshot) OwnershipProblem() string {
	if s.Run == nil || s.Step == nil {
		return ""
	}
	if problem := s.HeldOwnershipProblem(); problem != "" {
		return problem
	}
	st := s.Step
	owned := func(j *Job, allowUnowned bool) bool {
		if j.ChainRunID == "" {
			return allowUnowned
		}
		return j.ChainRunID == s.Run.ID && j.ChainSeq == st.Seq
	}
	if st.JobID != nil {
		if s.Job != nil && !owned(s.Job, false) {
			return fmt.Sprintf("step %d job #%d belongs to chain run %q seq %d", st.Seq, s.Job.ID, s.Job.ChainRunID, s.Job.ChainSeq)
		}
		if st.HeldID != "" {
			switch {
			case s.HeldMissing || s.Held == nil:
				return fmt.Sprintf("step %d references missing held job %s", st.Seq, st.HeldID)
			case s.Held.Status != HeldStatusReleased:
				return fmt.Sprintf("step %d has job #%d but held %s is %s", st.Seq, *st.JobID, s.Held.ID, s.Held.Status)
			case s.Held.ReleasedJobID == nil || *s.Held.ReleasedJobID != *st.JobID:
				released := int64(0)
				if s.Held.ReleasedJobID != nil {
					released = *s.Held.ReleasedJobID
				}
				return fmt.Sprintf("step %d job #%d differs from held %s released job #%d", st.Seq, *st.JobID, s.Held.ID, released)
			}
		}
		return ""
	}
	if s.Held != nil && s.Held.Status == HeldStatusReleased && s.Held.ReleasedJobID != nil {
		id := *s.Held.ReleasedJobID
		switch {
		case s.ReleasedJobMissing || s.ReleasedJob == nil:
			return fmt.Sprintf("held %s was released to missing job #%d", s.Held.ID, id)
		case s.ReleasedJob.RequestID != s.Held.RequestID:
			return fmt.Sprintf("held %s was released to job #%d with another request_id", s.Held.ID, id)
		case !owned(s.ReleasedJob, true):
			return fmt.Sprintf("held %s was released to job #%d of chain run %q seq %d", s.Held.ID, id, s.ReleasedJob.ChainRunID, s.ReleasedJob.ChainSeq)
		}
	}
	return ""
}

// HeldOwnershipProblem returns a non-empty description when the step's hold row exists
// but belongs to another run or seq (held_jobs.chain_run_id / chain_seq). CancelChainRun
// never updates such a hold.
func (s ChainSnapshot) HeldOwnershipProblem() string {
	if s.Run == nil || s.Step == nil || s.Held == nil {
		return ""
	}
	if s.Held.ChainRunID != s.Run.ID || s.Held.ChainSeq != s.Step.Seq {
		return fmt.Sprintf("step %d held %s belongs to chain run %q seq %d", s.Step.Seq, s.Held.ID, s.Held.ChainRunID, s.Held.ChainSeq)
	}
	return ""
}

// Fingerprint summarises every input of the branch function. AdvanceChain compares the
// fingerprint read before admission with the one read inside the commit transaction.
func (s ChainSnapshot) Fingerprint() string {
	var b strings.Builder
	if s.Run != nil {
		fmt.Fprintf(&b, "run=%s status=%s rev=%d seq=%d round=%d;", s.Run.ID, s.Run.Status, s.Run.Revision, s.Run.CurrentSeq, s.Run.Round)
	}
	if s.Step != nil {
		jobID := int64(0)
		if s.Step.JobID != nil {
			jobID = *s.Step.JobID
		}
		fmt.Fprintf(&b, "step=%d:%s:%d job=%d held=%s;", s.Step.Seq, s.Step.Step, s.Step.Round, jobID, s.Step.HeldID)
	} else {
		b.WriteString("step=none;")
	}
	if s.Job != nil {
		fmt.Fprintf(&b, "jobstatus=%s;", s.Job.Status)
	}
	if s.JobMissing {
		b.WriteString("jobmissing;")
	}
	if s.Held != nil {
		released := int64(0)
		if s.Held.ReleasedJobID != nil {
			released = *s.Held.ReleasedJobID
		}
		fmt.Fprintf(&b, "held=%s err=%t released=%d;", s.Held.Status, s.Held.LastError != "", released)
	}
	if s.HeldMissing {
		b.WriteString("heldmissing;")
	}
	if s.ReleasedJob != nil {
		fmt.Fprintf(&b, "releasedjob=%d:%s:%s:%d:%s;", s.ReleasedJob.ID, s.ReleasedJob.Status, s.ReleasedJob.ChainRunID, s.ReleasedJob.ChainSeq, s.ReleasedJob.RequestID)
	}
	if s.ReleasedJobMissing {
		b.WriteString("releasedjobmissing;")
	}
	if s.Artifact != nil {
		fmt.Fprintf(&b, "artifact=%s:%s:%s:%d;", s.Artifact.Status, s.Artifact.RawSHA256, s.Artifact.RunID, s.Artifact.Seq)
	}
	return b.String()
}

// ChainAction is what the branch function decided for a run.
type ChainAction string

const (
	ChainActionWait     ChainAction = "wait"
	ChainActionAdvance  ChainAction = "advance"
	ChainActionComplete ChainAction = "complete"
	ChainActionStop     ChainAction = "stop"
	// ChainActionLink records a released hold's job on chain_steps (R10).
	ChainActionLink ChainAction = "link"
)

// ChainDecision is the output of the branch function.
type ChainDecision struct {
	Action     ChainAction
	NextStep   string
	NextRound  int
	Branch     string
	StopReason string
	StopDetail string
	LinkJobID  int64
	// Stalled is set with ChainActionWait when the current step exceeded stall_after.
	Stalled bool
}

// Key identifies the decision for the in-transaction re-check (time-dependent fields such
// as Stalled are excluded).
func (d ChainDecision) Key() string {
	return fmt.Sprintf("%s|%s|%d|%s|%s|%d", d.Action, d.NextStep, d.NextRound, d.Branch, d.StopReason, d.LinkJobID)
}

// ChainStepWrite describes the jobs or held_jobs row to create for a new step.
type ChainStepWrite struct {
	// Params is the job payload after admission. RequestID, ParentID and Draft are set by
	// the Store.
	Params CreateJobParams
	// Hold stores the payload in held_jobs instead of creating a job.
	Hold                bool
	HoldPool            string
	HoldAltPools        []string
	HoldReason          string
	HoldSnapshotSummary string
	// Gate, when non-nil, is recorded in gate_decisions in the same transaction.
	Gate *GateDecision
	// VerdictNonce is stored on review steps (docs/22 §7).
	VerdictNonce string
}

// AdvanceChainParams drives one transition of a run (docs/22 §4.3).
type AdvanceChainParams struct {
	RunID string
	// Fingerprint is ChainSnapshot.Fingerprint() of the snapshot the decision was made on.
	Fingerprint string
	// Planned is the decision made outside the transaction.
	Planned ChainDecision
	// Decide is the same branch function; it is evaluated again inside the transaction.
	Decide func(ChainSnapshot) ChainDecision
	// Next is required when Planned.Action is advance.
	Next *ChainStepWrite
	// StopOverride turns an advance into a stop (admission rejected) after the re-check.
	StopOverride *ChainDecision
}

// AdvanceChainResult reports what AdvanceChain wrote.
type AdvanceChainResult struct {
	Applied bool
	// Skipped explains why nothing was written (stale snapshot, decision changed, ...).
	Skipped string
	Run     *ChainRun
	JobID   int64
	HeldID  string
}

// CreateChainRunParams registers a run and its first step in one transaction.
type CreateChainRunParams struct {
	RequestID         string
	Owner             string
	Title             string
	Scope             string
	DefinitionJSON    string
	DefinitionHash    string
	DefinitionVersion int
	MaxRounds         int
	MaxSteps          int
	// RunID, when set, is used as the run ID (the first step's payload may embed it).
	RunID     string
	FirstStep string
	First     ChainStepWrite
}

// ListChainRunsFilter filters GET /chains.
type ListChainRunsFilter struct {
	Status ChainRunStatus // empty = all
	Scope  string
	Cursor string
	Limit  int
}

// ListChainRunsPage is one page of chain runs ordered newest first.
type ListChainRunsPage struct {
	Runs       []*ChainRun
	NextCursor string
	HasMore    bool
}

// CancelChainRunResult reports POST /chains/{id}/cancel.
type CancelChainRunResult struct {
	Run              *ChainRun
	AlreadyCancelled bool
	// RunningJobID is set when the current step's job was claimed and was left running.
	RunningJobID      *int64
	CancelledJobIDs   []int64
	CancelledHeldID   string
	CancelledByChange bool
	// LinkedJobID is set when the current step had a released hold whose job was not yet
	// linked (R10): the cancel recorded that job on the step and the job's ownership.
	LinkedJobID *int64
	// Inconsistency is set when the step's job or hold failed OwnershipProblem; neither
	// that job nor a hold of another run was touched.
	Inconsistency string
}

// ReleaseHeldParams releases a hold in one transaction (docs/22 §5.1).
type ReleaseHeldParams struct {
	HeldID       string
	SelectedPool string
	Gate         *GateDecision
}

// ReleaseHeldOutcome reports ReleaseHeld.
type ReleaseHeldOutcome struct {
	Held    *HeldJob
	Job     *Job
	Created bool
	// Released is true when the hold now points at a job.
	Released bool
	// CancelledForChain is true when the hold was cancelled because its run is no longer
	// running.
	CancelledForChain bool
	// PermanentErr is set when the job could not be created for a reason a retry will not
	// fix (parent missing/terminal, invalid payload). It was recorded on the hold.
	PermanentErr error
}

// ChainNotificationStats summarises unsent chain notifications for GET /status.
type ChainNotificationStats struct {
	Pending       int64
	OldestPending *time.Time
}

package chain

import (
	"fmt"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

// Decide is the branch function (docs/22 §6, rows R1–R12). It looks only at the snapshot
// (DB rows), never at worker files. The Driver calls it outside the transaction to plan
// admission, and AdvanceChain calls it again inside the transaction; for equal inputs it
// returns an equal Key().
//
// now and stallAfter only set Stalled on wait decisions (excluded from Key).
func Decide(s store.ChainSnapshot, now time.Time, stallAfter time.Duration) store.ChainDecision {
	run := s.Run
	if run == nil || run.Status != store.ChainRunRunning {
		return store.ChainDecision{Action: store.ChainActionWait}
	}
	stop := func(reason, detail string) store.ChainDecision {
		return store.ChainDecision{Action: store.ChainActionStop, StopReason: reason, StopDetail: detail}
	}
	wait := func() store.ChainDecision {
		d := store.ChainDecision{Action: store.ChainActionWait}
		if stallAfter > 0 && s.Step != nil && now.Sub(s.Step.CreatedAt) > stallAfter {
			d.Stalled = true
		}
		return d
	}

	// R12: missing step or entity.
	if s.Step == nil {
		return stop(store.ChainStopInternalInconsistency, fmt.Sprintf("chain_steps(seq=%d) does not exist", run.CurrentSeq))
	}
	st := s.Step
	if st.JobID == nil && st.HeldID == "" {
		return stop(store.ChainStopInternalInconsistency, fmt.Sprintf("step %d has neither job_id nor held_id", st.Seq))
	}
	if s.JobMissing {
		return stop(store.ChainStopInternalInconsistency, fmt.Sprintf("step %d references missing job #%d", st.Seq, *st.JobID))
	}

	if st.JobID != nil {
		job := s.Job
		if job == nil {
			return stop(store.ChainStopInternalInconsistency, fmt.Sprintf("step %d job could not be read", st.Seq))
		}
		switch job.Status {
		case store.StatusQueued, store.StatusClaimed, store.StatusWaiting, store.StatusDraft:
			return wait() // R1
		case store.StatusFailed:
			return stop(store.ChainStopStepFailed, fmt.Sprintf("%s job #%d failed: %s", st.Step, job.ID, truncate(job.LastError, 300))) // R6
		case store.StatusCancelled:
			return stop(store.ChainStopStepCancelled, fmt.Sprintf("%s job #%d was cancelled", st.Step, job.ID)) // R7
		case store.StatusDone:
		default:
			return stop(store.ChainStopInternalInconsistency, fmt.Sprintf("job #%d has unknown status %q", job.ID, job.Status))
		}
		return decideDone(s, stop)
	}

	// Current step is a hold.
	if s.HeldMissing || s.Held == nil {
		return stop(store.ChainStopInternalInconsistency, fmt.Sprintf("step %d references missing held job %s", st.Seq, st.HeldID))
	}
	h := s.Held
	switch h.Status {
	case store.HeldStatusHeld:
		if h.LastError != "" {
			return stop(store.ChainStopReleaseFailed, fmt.Sprintf("held %s could not be released: %s", h.ID, truncate(h.LastError, 300))) // R9
		}
		return wait() // R8
	case store.HeldStatusReleased:
		if h.ReleasedJobID == nil {
			return stop(store.ChainStopInternalInconsistency, fmt.Sprintf("held %s is released without a job", h.ID))
		}
		return store.ChainDecision{Action: store.ChainActionLink, LinkJobID: *h.ReleasedJobID} // R10
	case store.HeldStatusCancelled:
		return stop(store.ChainStopStepCancelled, fmt.Sprintf("held %s was cancelled (%s)", h.ID, h.CancelReason)) // R11
	}
	return stop(store.ChainStopInternalInconsistency, fmt.Sprintf("held %s has unknown status %q", h.ID, h.Status))
}

func decideDone(s store.ChainSnapshot, stop func(string, string) store.ChainDecision) store.ChainDecision {
	run, st := s.Run, s.Step
	advance := func(next string, round int, branch string) store.ChainDecision {
		if run.CurrentSeq+1 > run.MaxSteps {
			return stop(store.ChainStopStepBudgetExceeded, fmt.Sprintf("seq %d would exceed max_steps %d", run.CurrentSeq+1, run.MaxSteps))
		}
		return store.ChainDecision{Action: store.ChainActionAdvance, NextStep: next, NextRound: round, Branch: branch}
	}
	switch st.Step {
	case store.ChainStepImplement:
		return advance(store.ChainStepReview, 1, store.ChainBranchDone) // R2
	case store.ChainStepFix:
		return advance(store.ChainStepReview, run.Round, store.ChainBranchDone) // R2
	case store.ChainStepPublish:
		return store.ChainDecision{Action: store.ChainActionComplete} // R5
	case store.ChainStepReview:
		a := s.Artifact
		if a == nil {
			return stop(store.ChainStopVerdictUnavailable, "no verdict was captured for review job") // R4
		}
		if a.Status != store.ChainArtifactCaptured {
			return stop(store.ChainStopVerdictUnavailable, fmt.Sprintf("verdict %s: %s", a.Status, truncate(a.Reason, 300))) // R4
		}
		if a.RunID != run.ID || a.Seq != st.Seq {
			return stop(store.ChainStopVerdictUnavailable, "captured verdict belongs to another step") // R4
		}
		switch a.Verdict {
		case store.ChainBranchOKToMerge:
			return advance(store.ChainStepPublish, run.Round, store.ChainBranchOKToMerge) // R3
		case store.ChainBranchChangesNeeded:
			if run.Round+1 > run.MaxRounds {
				return stop(store.ChainStopMaxRoundsExceeded, fmt.Sprintf("round %d reached max_rounds %d", run.Round, run.MaxRounds)) // R3
			}
			return advance(store.ChainStepFix, run.Round+1, store.ChainBranchChangesNeeded) // R3
		}
		return stop(store.ChainStopVerdictUnavailable, fmt.Sprintf("unknown verdict %q", a.Verdict)) // R4
	}
	return stop(store.ChainStopInternalInconsistency, fmt.Sprintf("unknown step %q", st.Step))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

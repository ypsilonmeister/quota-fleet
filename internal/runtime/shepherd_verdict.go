package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/adapter"
	"github.com/ypsilonmeister/quota-fleet/internal/chain"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

// captureVerdict reads and validates the verdict of a chain review job (docs/22 §7.1).
// It returns (nil, nil) for jobs that are not chain review steps. A non-nil error is
// transient (host unreachable, DB busy) and is retried like an evaluation error; every
// other outcome is an artifact row (captured, missing, invalid or unavailable).
func (s *Shepherd) captureVerdict(ctx context.Context, job *store.Job) (*store.ChainArtifact, error) {
	if job.ChainRunID == "" {
		return nil, nil
	}
	step, err := s.store.GetChainStep(ctx, job.ChainRunID, job.ChainSeq)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read chain step: %w", err)
	}
	if step.Step != store.ChainStepReview {
		return nil, nil
	}
	path := chain.VerdictPath(job.ChainRunID, job.ChainSeq)

	provider, ok := s.adapter.(adapter.FileAccessProvider)
	if !ok {
		return s.verdictArtifact(job, store.ChainArtifactUnavailable, "the adapter provides no file access to read the verdict"), nil
	}
	fa, err := provider.FileAccessFor(job)
	if err != nil {
		return nil, fmt.Errorf("resolve host file access: %w", err)
	}
	nf, ok := fa.(adapter.NoFollowStatter)
	if !ok {
		return s.verdictArtifact(job, store.ChainArtifactUnavailable, "the host file access cannot check for symbolic links"), nil
	}
	st, err := nf.StatNoFollow(ctx, path)
	if err != nil {
		if errors.Is(err, adapter.ErrNotRelative) || errors.Is(err, adapter.ErrDeniedPath) {
			return s.verdictArtifact(job, store.ChainArtifactInvalid, err.Error()), nil
		}
		return nil, fmt.Errorf("inspect %s: %w", path, err)
	}
	switch {
	case !st.Exists:
		return s.verdictArtifact(job, store.ChainArtifactMissing, path+" does not exist"), nil
	case st.Symlink:
		return s.verdictArtifact(job, store.ChainArtifactInvalid, path+" is or passes through a symbolic link"), nil
	case !st.IsRegular:
		return s.verdictArtifact(job, store.ChainArtifactInvalid, path+" is not a regular file"), nil
	case st.Size > chain.MaxVerdictBytes:
		return s.verdictArtifact(job, store.ChainArtifactInvalid, fmt.Sprintf("%s is %d bytes (max %d)", path, st.Size, chain.MaxVerdictBytes)), nil
	}
	data, err := fa.ReadFile(ctx, path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s.verdictArtifact(job, store.ChainArtifactMissing, path+" does not exist"), nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	a := chain.ParseVerdict(data, job.ChainRunID, job.ChainSeq, step.VerdictNonce)
	a.JobID = job.ID
	a.CapturedAt = time.Now().UTC()
	return &a, nil
}

func (s *Shepherd) verdictArtifact(job *store.Job, status, reason string) *store.ChainArtifact {
	return &store.ChainArtifact{
		JobID:      job.ID,
		RunID:      job.ChainRunID,
		Seq:        job.ChainSeq,
		Status:     status,
		Reason:     reason,
		CapturedAt: time.Now().UTC(),
	}
}

func (s *Shepherd) recordCaptureFailure(jobID int64, claimedBy string) int {
	s.evalMu.Lock()
	defer s.evalMu.Unlock()
	if s.captureFailures == nil {
		s.captureFailures = make(map[int64]evalFailureState)
	}
	st := s.captureFailures[jobID]
	if st.claimedBy != claimedBy {
		st = evalFailureState{claimedBy: claimedBy}
	}
	st.count++
	s.captureFailures[jobID] = st
	return st.count
}

func (s *Shepherd) clearCaptureFailure(jobID int64) {
	s.evalMu.Lock()
	defer s.evalMu.Unlock()
	delete(s.captureFailures, jobID)
}

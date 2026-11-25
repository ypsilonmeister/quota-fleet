package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ypsilonmeister/quota-fleet/internal/evaluator"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

const decisionLogTimeout = 10 * time.Second

// ReviewJudge is the Decision 1 client behind the review verdict gate (docs/18).
// Implementations must keep the API key out of returned values and errors.
type ReviewJudge interface {
	ModelName() string
	HasAPIKey() bool
	JudgeReview(ctx context.Context, text string) (evaluator.ReviewJudgment, error)
}

func (s *Server) reviewKeyConfigured() bool {
	return s.reviewJudge != nil && s.reviewJudge.HasAPIKey()
}

func (s *Server) reviewGateMeta() ReviewGateDTO {
	model := evaluator.DefaultDecisionModel
	if s.reviewJudge != nil {
		model = s.reviewJudge.ModelName()
	}
	return ReviewGateDTO{
		Enabled:       s.reviewGateEnabled,
		Model:         model,
		KeyConfigured: s.reviewKeyConfigured(),
	}
}

// handleReviewVerdict serves POST /decision/review-verdict. Every path that cannot produce
// and record a judgment answers with an error status and no verdict.
func (s *Server) handleReviewVerdict(w http.ResponseWriter, r *http.Request) {
	if !s.reviewGateEnabled {
		writeError(w, http.StatusServiceUnavailable, "review gate is disabled (review_gate.enabled is false)")
		return
	}
	if !s.reviewKeyConfigured() {
		writeError(w, http.StatusServiceUnavailable, "review gate is unavailable: Decision API key is not configured")
		return
	}

	var req ReviewVerdictRequest
	if err := decodeJSON(w, r, &req, false); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(req.Text) == "" {
		writeError(w, http.StatusBadRequest, "text is required")
		return
	}
	if utf8.RuneCountInString(req.Text) > evaluator.MaxReviewTextRunes {
		writeError(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("text exceeds %d characters", evaluator.MaxReviewTextRunes))
		return
	}
	if (req.JobID != nil && *req.JobID <= 0) || (req.ReviewJobID != nil && *req.ReviewJobID <= 0) {
		writeError(w, http.StatusBadRequest, "job_id and review_job_id must be positive integers when set")
		return
	}

	judgment, err := s.reviewJudge.JudgeReview(r.Context(), req.Text)
	if err != nil {
		s.writeReviewJudgeError(w, err)
		return
	}
	if judgment.Verdict != evaluator.VerdictOKToMerge && judgment.Verdict != evaluator.VerdictChangesNeeded {
		s.logger.Printf("review verdict failed: judge returned unknown verdict %q", judgment.Verdict)
		writeError(w, http.StatusBadGateway, "review verdict unavailable: invalid_response")
		return
	}

	// A judgment that was made is recorded even if the client has gone away.
	logCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), decisionLogTimeout)
	defer cancel()
	if _, err := s.store.InsertDecisionLog(logCtx, store.DecisionLogEntry{
		JobID:       req.JobID,
		ReviewJobID: req.ReviewJobID,
		Verdict:     judgment.Verdict,
		Score:       judgment.Score,
		Model:       s.reviewJudge.ModelName(),
	}); err != nil {
		s.logger.Printf("review verdict failed: %v", err)
		writeError(w, http.StatusInternalServerError, "review verdict unavailable: could not record the decision")
		return
	}

	writeJSON(w, http.StatusOK, ReviewVerdictResponse{
		Verdict: judgment.Verdict,
		Score:   judgment.Score,
		Reason:  judgment.Reason,
	})
}

// writeReviewJudgeError reports only the failure kind to the client. Details, which may
// quote the upstream response, go to the server log.
func (s *Server) writeReviewJudgeError(w http.ResponseWriter, err error) {
	s.logger.Printf("review verdict failed: %v", err)
	status := http.StatusBadGateway
	kind := evaluator.UnavailableAPIError
	var unavailable *evaluator.EvaluationUnavailableError
	if errors.As(err, &unavailable) {
		kind = unavailable.Kind
		switch kind {
		case evaluator.UnavailableMissingAPIKey:
			status = http.StatusServiceUnavailable
		case evaluator.UnavailableTimeout:
			status = http.StatusGatewayTimeout
		}
	}
	writeError(w, status, "review verdict unavailable: "+string(kind))
}

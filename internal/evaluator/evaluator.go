package evaluator

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ypsilonmeister/quota-fleet/internal/adapter"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

// ErrEvaluationUnavailable is the sentinel error matched by EvaluationUnavailableError
// when evaluation cannot be performed (e.g., missing API key or Decisions API failure).
var ErrEvaluationUnavailable = errors.New("evaluator: evaluation unavailable")

// EvaluationUnavailableKind classifies why evaluation could not be completed.
type EvaluationUnavailableKind string

const (
	UnavailableMissingAPIKey EvaluationUnavailableKind = "missing_api_key"
	UnavailableAPIError      EvaluationUnavailableKind = "api_error"
	UnavailableInvalidResp   EvaluationUnavailableKind = "invalid_response"
	UnavailableTimeout       EvaluationUnavailableKind = "timeout"
)

// EvaluationUnavailableError indicates that the evaluator could not produce a verdict
// and did not silently fall back to pass or fail.
type EvaluationUnavailableError struct {
	Kind   EvaluationUnavailableKind
	Reason string
	Err    error
}

// Error implements the error interface.
func (e *EvaluationUnavailableError) Error() string {
	if e == nil {
		return ErrEvaluationUnavailable.Error()
	}
	var parts []string
	if e.Kind != "" {
		parts = append(parts, string(e.Kind))
	}
	if strings.TrimSpace(e.Reason) != "" {
		parts = append(parts, strings.TrimSpace(e.Reason))
	}
	if e.Err != nil {
		parts = append(parts, e.Err.Error())
	}
	if len(parts) == 0 {
		return ErrEvaluationUnavailable.Error()
	}
	return fmt.Sprintf("evaluation unavailable (%s)", strings.Join(parts, ": "))
}

// Unwrap returns the underlying error, if any.
func (e *EvaluationUnavailableError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// Is allows matching via errors.Is(err, ErrEvaluationUnavailable).
func (e *EvaluationUnavailableError) Is(target error) bool {
	return target == ErrEvaluationUnavailable
}

// Evaluator assesses a completed job's acceptance criteria against collected Evidence.
// Implementations must not perform job store transitions directly.
type Evaluator interface {
	Evaluate(ctx context.Context, job *store.Job, ev adapter.Evidence) (Result, error)
}

// FileAccessEvaluator is an optional extension of Evaluator that accepts a host-scoped
// FileAccess for evaluating typed checks and reading workdir artifacts on the job's host.
type FileAccessEvaluator interface {
	Evaluator
	EvaluateWithFileAccess(ctx context.Context, job *store.Job, ev adapter.Evidence, fa FileAccess) (Result, error)
}

// CriterionResult holds the outcome for a single acceptance criterion.
type CriterionResult struct {
	Criterion string `json:"criterion"`
	Passed    bool   `json:"passed"`
	Evidence  string `json:"evidence"`
}

// Result holds the per-criterion results and overall pass/fail verdict.
type Result struct {
	Passed   bool              `json:"passed"`
	Criteria []CriterionResult `json:"criteria"`
}

// ParseCriteria splits a job's AcceptanceCriteria string (one criterion per line)
// into trimmed non-empty criteria strings.
func ParseCriteria(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	lines := strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n")
	var out []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		out = append(out, trimmed)
	}
	return out
}

// FormatText renders the evaluation outcome as a human-readable multi-line report
// suitable for storing in job.result_text.
func (r Result) FormatText() string {
	var b strings.Builder
	verdict := "FAIL"
	if r.Passed {
		verdict = "PASS"
	}
	fmt.Fprintf(&b, "overall: %s", verdict)
	for _, c := range r.Criteria {
		tag := "FAIL"
		if c.Passed {
			tag = "PASS"
		}
		if strings.TrimSpace(c.Evidence) != "" {
			fmt.Fprintf(&b, "\n- [%s] %s (%s)", tag, c.Criterion, c.Evidence)
		} else {
			fmt.Fprintf(&b, "\n- [%s] %s", tag, c.Criterion)
		}
	}
	return b.String()
}

// FailSummary returns a concise single-line summary of failed criteria for last_error.
func (r Result) FailSummary() string {
	if r.Passed {
		return ""
	}
	var failed []string
	for _, c := range r.Criteria {
		if !c.Passed {
			failed = append(failed, c.Criterion)
		}
	}
	if len(failed) == 0 {
		return "evaluation failed"
	}
	return fmt.Sprintf("acceptance criteria failed (%d): %s", len(failed), strings.Join(failed, "; "))
}

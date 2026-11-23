package evaluator

import (
	"context"
	"fmt"
	"strings"

	"github.com/ypsilonmeister/quota-fleet/internal/adapter"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

// Evaluator assesses a completed job's acceptance criteria against collected Evidence.
// Implementations must not perform job store transitions directly.
type Evaluator interface {
	Evaluate(ctx context.Context, job *store.Job, ev adapter.Evidence) (Result, error)
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

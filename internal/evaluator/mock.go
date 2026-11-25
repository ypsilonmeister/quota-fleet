package evaluator

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ypsilonmeister/quota-fleet/internal/adapter"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

// ErrNilJob is returned when a nil *store.Job is passed to Evaluate.
var ErrNilJob = errors.New("evaluator: job must not be nil")

// AlwaysPassEvaluator is a mock Evaluator that marks every acceptance criterion as PASS.
type AlwaysPassEvaluator struct {
	EvidenceText string
}

// NewAlwaysPass returns an AlwaysPassEvaluator with a default evidence note.
func NewAlwaysPass() *AlwaysPassEvaluator {
	return &AlwaysPassEvaluator{
		EvidenceText: "mock evaluator: passed",
	}
}

// Evaluate marks all parsed acceptance criteria as PASS and returns overall Passed=true.
func (e *AlwaysPassEvaluator) Evaluate(ctx context.Context, job *store.Job, ev adapter.Evidence) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if job == nil {
		return Result{}, ErrNilJob
	}
	note := e.EvidenceText
	if note == "" {
		note = "mock evaluator: passed"
	}
	if ev.FinalReport != "" {
		note = fmt.Sprintf("%s; %s", note, ev.FinalReport)
	}

	criteria := ParseCriteria(job.AcceptanceCriteria)
	results := make([]CriterionResult, 0, len(criteria))
	for _, c := range criteria {
		results = append(results, CriterionResult{
			Criterion: c,
			Passed:    true,
			Evidence:  note,
		})
	}
	return Result{
		Passed:   true,
		Criteria: results,
	}, nil
}

// SelectiveFailEvaluator is a mock Evaluator that fails criteria matching any configured
// 0-based index or substring, while passing all other criteria.
type SelectiveFailEvaluator struct {
	FailIndexes    map[int]bool
	FailSubstrings []string
	FailReason     string
	PassReason     string
}

// SelectiveFailOptions configures a SelectiveFailEvaluator.
type SelectiveFailOptions struct {
	FailIndexes    []int
	FailSubstrings []string
	FailReason     string
	PassReason     string
}

// NewSelectiveFail constructs a SelectiveFailEvaluator.
func NewSelectiveFail(opts SelectiveFailOptions) *SelectiveFailEvaluator {
	idxMap := make(map[int]bool, len(opts.FailIndexes))
	for _, idx := range opts.FailIndexes {
		idxMap[idx] = true
	}
	failReason := opts.FailReason
	if failReason == "" {
		failReason = "mock evaluator: forced fail"
	}
	passReason := opts.PassReason
	if passReason == "" {
		passReason = "mock evaluator: passed"
	}
	var subs []string
	for _, s := range opts.FailSubstrings {
		if s != "" {
			subs = append(subs, s)
		}
	}
	return &SelectiveFailEvaluator{
		FailIndexes:    idxMap,
		FailSubstrings: subs,
		FailReason:     failReason,
		PassReason:     passReason,
	}
}

// Evaluate evaluates each acceptance criterion and marks matching indexes or substrings as FAIL.
func (e *SelectiveFailEvaluator) Evaluate(ctx context.Context, job *store.Job, ev adapter.Evidence) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if job == nil {
		return Result{}, ErrNilJob
	}

	criteria := ParseCriteria(job.AcceptanceCriteria)
	results := make([]CriterionResult, 0, len(criteria))
	overall := true

	for i, c := range criteria {
		failed := e.FailIndexes[i]
		if !failed {
			for _, sub := range e.FailSubstrings {
				if strings.Contains(c, sub) {
					failed = true
					break
				}
			}
		}
		if failed {
			overall = false
			results = append(results, CriterionResult{
				Criterion: c,
				Passed:    false,
				Evidence:  e.FailReason,
			})
		} else {
			results = append(results, CriterionResult{
				Criterion: c,
				Passed:    true,
				Evidence:  e.PassReason,
			})
		}
	}

	return Result{
		Passed:   overall,
		Criteria: results,
	}, nil
}

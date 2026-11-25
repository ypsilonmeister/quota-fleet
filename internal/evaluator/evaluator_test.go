package evaluator

import (
	"context"
	"strings"
	"testing"

	"github.com/ypsilonmeister/quota-fleet/internal/adapter"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

func TestAlwaysPassEvaluator(t *testing.T) {
	ev := NewAlwaysPass()
	job := &store.Job{
		ID:                 1,
		Title:              "build & test",
		AcceptanceCriteria: "go build succeeds\n\n  go test passes  \n",
	}

	res, err := ev.Evaluate(context.Background(), job, adapter.Evidence{HasDiff: true, FinalReport: "all green"})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !res.Passed {
		t.Fatalf("Passed = false, want true")
	}
	if len(res.Criteria) != 2 {
		t.Fatalf("Criteria len = %d, want 2", len(res.Criteria))
	}
	if res.Criteria[0].Criterion != "go build succeeds" || !res.Criteria[0].Passed {
		t.Errorf("Criteria[0] = %+v", res.Criteria[0])
	}
	if res.Criteria[1].Criterion != "go test passes" || !res.Criteria[1].Passed {
		t.Errorf("Criteria[1] = %+v", res.Criteria[1])
	}
	text := res.FormatText()
	if !strings.Contains(text, "overall: PASS") || !strings.Contains(text, "[PASS] go test passes") {
		t.Errorf("unexpected FormatText:\n%s", text)
	}
}

func TestSelectiveFailEvaluatorByIndexAndSubstring(t *testing.T) {
	job := &store.Job{
		ID:                 2,
		Title:              "selective fail",
		AcceptanceCriteria: "build succeeds\nunit tests pass\nno lint errors",
	}

	// Fail by index 1.
	byIdx := NewSelectiveFail(SelectiveFailOptions{
		FailIndexes: []int{1},
		FailReason:  "tests failed",
	})
	res, err := byIdx.Evaluate(context.Background(), job, adapter.Evidence{})
	if err != nil {
		t.Fatalf("Evaluate by index: %v", err)
	}
	if res.Passed {
		t.Fatalf("Passed = true, want false")
	}
	if len(res.Criteria) != 3 {
		t.Fatalf("Criteria len = %d, want 3", len(res.Criteria))
	}
	if !res.Criteria[0].Passed || res.Criteria[1].Passed || !res.Criteria[2].Passed {
		t.Errorf("unexpected criteria outcomes: %+v", res.Criteria)
	}
	if !strings.Contains(res.FormatText(), "[FAIL] unit tests pass (tests failed)") {
		t.Errorf("unexpected FormatText:\n%s", res.FormatText())
	}
	if !strings.Contains(res.FailSummary(), "unit tests pass") {
		t.Errorf("unexpected FailSummary: %q", res.FailSummary())
	}

	// Fail by substring.
	bySub := NewSelectiveFail(SelectiveFailOptions{
		FailSubstrings: []string{"lint"},
	})
	res2, err := bySub.Evaluate(context.Background(), job, adapter.Evidence{})
	if err != nil {
		t.Fatalf("Evaluate by substring: %v", err)
	}
	if res2.Passed {
		t.Fatalf("Passed = true, want false")
	}
	if !res2.Criteria[0].Passed || !res2.Criteria[1].Passed || res2.Criteria[2].Passed {
		t.Errorf("unexpected substring criteria outcomes: %+v", res2.Criteria)
	}
}

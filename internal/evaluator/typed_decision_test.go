package evaluator

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/adapter"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

func TestTypedChecksAllKindsPassAndFail(t *testing.T) {
	wd := t.TempDir()
	if err := os.MkdirAll(filepath.Join(wd, "sub"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wd, "hello.txt"), []byte("hello quota-fleet world\n"), 0o644); err != nil {
		t.Fatalf("WriteFile hello.txt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wd, "empty.txt"), nil, 0o644); err != nil {
		t.Fatalf("WriteFile empty.txt: %v", err)
	}

	var apiCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiCalls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	// No API key set, verifying typed-only checks never require Decision 1 or an API key.
	eval := NewTypedDecision(TypedDecisionOptions{
		Endpoint:       srv.URL,
		CommandTimeout: 200 * time.Millisecond,
		Getenv:         func(string) string { return "" },
	})

	t.Run("all four typed kinds pass", func(t *testing.T) {
		job := &store.Job{
			ID:      1,
			Title:   "typed pass",
			Workdir: wd,
			AcceptanceCriteria: strings.Join([]string{
				"file_exists: hello.txt",
				"file_exists: sub",
				"file_nonempty: hello.txt",
				"contains: hello.txt :: quota-fleet",
				"command: test -f hello.txt && grep -q quota-fleet hello.txt",
			}, "\n"),
		}
		res, err := eval.Evaluate(context.Background(), job, adapter.Evidence{})
		if err != nil {
			t.Fatalf("Evaluate unexpected error: %v", err)
		}
		if !res.Passed {
			t.Fatalf("Passed = false, want true:\n%s", res.FormatText())
		}
		if len(res.Criteria) != 5 {
			t.Fatalf("Criteria len = %d, want 5", len(res.Criteria))
		}
		for i, c := range res.Criteria {
			if !c.Passed {
				t.Errorf("Criteria[%d] passed=false: %+v", i, c)
			}
			if !strings.HasPrefix(c.Evidence, "typed ") {
				t.Errorf("Criteria[%d] evidence %q should start with 'typed '", i, c.Evidence)
			}
		}
		if apiCalls.Load() != 0 {
			t.Errorf("Decision 1 API calls = %d, want 0", apiCalls.Load())
		}
	})

	t.Run("file_exists fails on missing file", func(t *testing.T) {
		job := &store.Job{
			ID:                 2,
			Workdir:            wd,
			AcceptanceCriteria: "file_exists: missing.txt",
		}
		res, err := eval.Evaluate(context.Background(), job, adapter.Evidence{})
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if res.Passed || len(res.Criteria) != 1 || res.Criteria[0].Passed {
			t.Fatalf("expected FAIL, got %+v", res)
		}
		if !strings.Contains(res.Criteria[0].Evidence, "typed file_exists") {
			t.Errorf("unexpected evidence: %q", res.Criteria[0].Evidence)
		}
	})

	t.Run("file_nonempty fails on empty file, dir, and missing file", func(t *testing.T) {
		job := &store.Job{
			ID:      3,
			Workdir: wd,
			AcceptanceCriteria: strings.Join([]string{
				"file_nonempty: empty.txt",
				"file_nonempty: sub",
				"file_nonempty: missing.txt",
			}, "\n"),
		}
		res, err := eval.Evaluate(context.Background(), job, adapter.Evidence{})
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if res.Passed {
			t.Fatalf("Passed = true, want false")
		}
		for i, c := range res.Criteria {
			if c.Passed {
				t.Errorf("Criteria[%d] unexpectedly passed: %+v", i, c)
			}
			if !strings.Contains(c.Evidence, "typed file_nonempty") {
				t.Errorf("Criteria[%d] unexpected evidence: %q", i, c.Evidence)
			}
		}
	})

	t.Run("contains fails on mismatch, missing file, and invalid syntax", func(t *testing.T) {
		job := &store.Job{
			ID:      4,
			Workdir: wd,
			AcceptanceCriteria: strings.Join([]string{
				"contains: hello.txt :: not-present",
				"contains: missing.txt :: hello",
				"contains: hello.txt without separator",
			}, "\n"),
		}
		res, err := eval.Evaluate(context.Background(), job, adapter.Evidence{})
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if res.Passed {
			t.Fatalf("Passed = true, want false")
		}
		for i, c := range res.Criteria {
			if c.Passed {
				t.Errorf("Criteria[%d] unexpectedly passed: %+v", i, c)
			}
			if !strings.Contains(c.Evidence, "typed contains") {
				t.Errorf("Criteria[%d] unexpected evidence: %q", i, c.Evidence)
			}
		}
	})

	t.Run("command fails on non-zero exit and timeout without evaluation error", func(t *testing.T) {
		job := &store.Job{
			ID:      5,
			Workdir: wd,
			AcceptanceCriteria: strings.Join([]string{
				"command: echo bad >&2; exit 7",
				"command: sleep 5",
			}, "\n"),
		}
		res, err := eval.Evaluate(context.Background(), job, adapter.Evidence{})
		if err != nil {
			t.Fatalf("expected FAIL result rather than evaluation error, got err=%v", err)
		}
		if res.Passed || len(res.Criteria) != 2 {
			t.Fatalf("expected 2 failed criteria, got %+v", res)
		}
		if res.Criteria[0].Passed || !strings.Contains(res.Criteria[0].Evidence, "exit 7") {
			t.Errorf("Criteria[0] = %+v, want exit 7 failure", res.Criteria[0])
		}
		if res.Criteria[1].Passed || !strings.Contains(res.Criteria[1].Evidence, "timed out") {
			t.Errorf("Criteria[1] = %+v, want timeout failure", res.Criteria[1])
		}
	})

	if apiCalls.Load() != 0 {
		t.Errorf("total Decision 1 API calls during typed checks = %d, want 0", apiCalls.Load())
	}
}

func TestDecision1FreeformAndMixedEvaluation(t *testing.T) {
	wd := t.TempDir()
	// Initialize a git repo with a modified file to verify git status/diff inclusion in shared input.
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", wd}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, string(out))
		}
	}
	runGit("init")
	runGit("config", "user.email", "test@example.com")
	runGit("config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(wd, "README.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatalf("WriteFile README.txt: %v", err)
	}
	runGit("add", "README.txt")
	runGit("commit", "-m", "init")
	if err := os.WriteFile(filepath.Join(wd, "README.txt"), []byte("v1\nv2\n"), 0o644); err != nil {
		t.Fatalf("Update README.txt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wd, "summary.md"), []byte("Summary: implemented feature X"), 0o644); err != nil {
		t.Fatalf("WriteFile summary.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wd, "result.txt"), []byte("RESULT: OK"), 0o644); err != nil {
		t.Fatalf("WriteFile result.txt: %v", err)
	}

	var (
		apiCalls       atomic.Int32
		capturedAPIKey string
		capturedReq    decisionsRequest
		probsByKey     map[string]float64
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiCalls.Add(1)
		capturedAPIKey = r.Header.Get("x-goog-api-key")
		var req decisionsRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		capturedReq = req

		answers := make(map[string]map[string]any, len(req.Questions))
		for k := range req.Questions {
			p := 0.85
			if v, ok := probsByKey[k]; ok {
				p = v
			}
			answers[k] = map[string]any{"noul": p, "confidence": 0.91}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"answers": answers,
			"usage":   map[string]any{"total_input_tokens": 321},
		})
	}))
	defer srv.Close()

	eval := NewTypedDecision(TypedDecisionOptions{
		Endpoint:          srv.URL,
		APIKeyEnv:         "TEST_GEMINI_KEY",
		DecisionThreshold: 0.5,
		Getenv: func(k string) string {
			if k == "TEST_GEMINI_KEY" {
				return "fake-key-for-test"
			}
			return ""
		},
	})

	t.Run("freeform only batched in single call passes above threshold", func(t *testing.T) {
		apiCalls.Store(0)
		probsByKey = map[string]float64{
			"criterion_1": 0.87,
			"criterion_2": 0.50, // exact threshold boundary passes
		}
		job := &store.Job{
			ID:                 10,
			Title:              "Implement feature X",
			Instructions:       "Add feature X and document it",
			Workdir:            wd,
			AcceptanceCriteria: "Feature X is clearly summarized\nNo regressions reported in final log",
		}
		ev := adapter.Evidence{
			HasDiff:     true,
			DiffSummary: "1 file changed, 1 insertion(+)",
			FinalReport: "All checks green; feature X complete.",
		}
		res, err := eval.Evaluate(context.Background(), job, ev)
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if !res.Passed {
			t.Fatalf("Passed = false, want true:\n%s", res.FormatText())
		}
		if apiCalls.Load() != 1 {
			t.Fatalf("API calls = %d, want 1 (batched)", apiCalls.Load())
		}
		if capturedAPIKey != "fake-key-for-test" {
			t.Errorf("x-goog-api-key = %q, want fake-key-for-test", capturedAPIKey)
		}
		if capturedReq.Model != DefaultDecisionModel {
			t.Errorf("model = %q, want %q", capturedReq.Model, DefaultDecisionModel)
		}
		if len(capturedReq.Input) != 1 {
			t.Fatalf("input parts = %d, want 1", len(capturedReq.Input))
		}
		shared := capturedReq.Input[0].Text
		for _, want := range []string{
			"Implement feature X",
			"Add feature X and document it",
			"All checks green; feature X complete.",
			"1 file changed, 1 insertion(+)",
			"Summary: implemented feature X",
			"RESULT: OK",
			"README.txt",
		} {
			if !strings.Contains(shared, want) {
				t.Errorf("shared input missing %q:\n%s", want, shared)
			}
		}
		if len(capturedReq.Questions) != 2 {
			t.Fatalf("questions len = %d, want 2", len(capturedReq.Questions))
		}
		if capturedReq.Questions["criterion_1"].Type != "noul" ||
			!strings.Contains(capturedReq.Questions["criterion_1"].Instructions, "Feature X is clearly summarized") {
			t.Errorf("unexpected question criterion_1: %+v", capturedReq.Questions["criterion_1"])
		}

		text := res.FormatText()
		if !strings.Contains(text, "[PASS] Feature X is clearly summarized (decision1 noul p=0.87 (>= 0.50))") {
			t.Errorf("FormatText missing expected decision1 evidence:\n%s", text)
		}
		if !strings.Contains(text, "[PASS] No regressions reported in final log (decision1 noul p=0.50 (>= 0.50))") {
			t.Errorf("FormatText missing boundary decision1 evidence:\n%s", text)
		}
	})

	t.Run("freeform below threshold fails overall", func(t *testing.T) {
		apiCalls.Store(0)
		probsByKey = map[string]float64{
			"criterion_1": 0.90,
			"criterion_2": 0.49,
		}
		job := &store.Job{
			ID:                 11,
			Title:              "Partial work",
			Workdir:            wd,
			AcceptanceCriteria: "First condition ok\nSecond condition ok",
		}
		res, err := eval.Evaluate(context.Background(), job, adapter.Evidence{})
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if res.Passed {
			t.Fatalf("Passed = true, want false")
		}
		if !res.Criteria[0].Passed || res.Criteria[1].Passed {
			t.Errorf("unexpected criteria outcomes: %+v", res.Criteria)
		}
		if !strings.Contains(res.Criteria[1].Evidence, "decision1 noul p=0.49 (< 0.50)") {
			t.Errorf("unexpected fail evidence: %q", res.Criteria[1].Evidence)
		}
	})

	t.Run("mixed typed pass + freeform pass preserves original order", func(t *testing.T) {
		apiCalls.Store(0)
		probsByKey = map[string]float64{
			"criterion_2": 0.92,
		}
		job := &store.Job{
			ID:      12,
			Title:   "Mixed job",
			Workdir: wd,
			AcceptanceCriteria: strings.Join([]string{
				"file_exists: summary.md",
				"Summary explains the change clearly",
				"contains: result.txt :: RESULT: OK",
			}, "\n"),
		}
		res, err := eval.Evaluate(context.Background(), job, adapter.Evidence{})
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if !res.Passed || len(res.Criteria) != 3 {
			t.Fatalf("expected 3 passed criteria, got %+v", res)
		}
		if apiCalls.Load() != 1 {
			t.Fatalf("API calls = %d, want 1", apiCalls.Load())
		}
		if len(capturedReq.Questions) != 1 {
			t.Fatalf("expected only 1 freeform question sent to Decision 1, got %d", len(capturedReq.Questions))
		}
		if !strings.HasPrefix(res.Criteria[0].Evidence, "typed file_exists") {
			t.Errorf("Criteria[0] = %+v", res.Criteria[0])
		}
		if !strings.HasPrefix(res.Criteria[1].Evidence, "decision1 noul p=0.92") {
			t.Errorf("Criteria[1] = %+v", res.Criteria[1])
		}
		if !strings.HasPrefix(res.Criteria[2].Evidence, "typed contains") {
			t.Errorf("Criteria[2] = %+v", res.Criteria[2])
		}
	})

	t.Run("mixed typed fail short-circuits without calling Decision 1", func(t *testing.T) {
		apiCalls.Store(0)
		// Even with missing API key, a failing typed check returns FAIL without evaluation error.
		evalNoKey := NewTypedDecision(TypedDecisionOptions{
			Endpoint: srv.URL,
			Getenv:   func(string) string { return "" },
		})
		job := &store.Job{
			ID:      13,
			Title:   "Mixed fail job",
			Workdir: wd,
			AcceptanceCriteria: strings.Join([]string{
				"file_exists: does-not-exist.txt",
				"Summary looks reasonable",
			}, "\n"),
		}
		res, err := evalNoKey.Evaluate(context.Background(), job, adapter.Evidence{})
		if err != nil {
			t.Fatalf("expected deterministic FAIL without error, got err=%v", err)
		}
		if res.Passed {
			t.Fatalf("Passed = true, want false")
		}
		if apiCalls.Load() != 0 {
			t.Fatalf("Decision 1 should not be called when typed check fails; calls = %d", apiCalls.Load())
		}
	})
}

func TestDecision1EvaluationUnavailableErrors(t *testing.T) {
	freeformJob := &store.Job{
		ID:                 20,
		Title:              "Freeform job",
		AcceptanceCriteria: "Implementation looks idiomatic",
	}

	t.Run("missing API key returns ErrEvaluationUnavailable", func(t *testing.T) {
		eval := NewTypedDecision(TypedDecisionOptions{
			APIKeyEnv: "MISSING_KEY_ENV",
			Getenv:    func(string) string { return "" },
		})
		_, err := eval.Evaluate(context.Background(), freeformJob, adapter.Evidence{})
		if err == nil {
			t.Fatalf("expected error, got nil")
		}
		if !errors.Is(err, ErrEvaluationUnavailable) {
			t.Fatalf("errors.Is(err, ErrEvaluationUnavailable) = false; err=%v", err)
		}
		var unavail *EvaluationUnavailableError
		if !errors.As(err, &unavail) || unavail.Kind != UnavailableMissingAPIKey {
			t.Fatalf("expected UnavailableMissingAPIKey, got %#v", err)
		}
		if !strings.Contains(err.Error(), "MISSING_KEY_ENV") {
			t.Errorf("error should mention env var name, got %q", err.Error())
		}
	})

	t.Run("HTTP 500 API error returns ErrEvaluationUnavailable", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"backend failure"}`))
		}))
		defer srv.Close()

		eval := NewTypedDecision(TypedDecisionOptions{
			Endpoint: srv.URL,
			Getenv:   func(string) string { return "test-key" },
		})
		_, err := eval.Evaluate(context.Background(), freeformJob, adapter.Evidence{})
		if !errors.Is(err, ErrEvaluationUnavailable) {
			t.Fatalf("expected ErrEvaluationUnavailable, got %v", err)
		}
		var unavail *EvaluationUnavailableError
		if !errors.As(err, &unavail) || unavail.Kind != UnavailableAPIError {
			t.Fatalf("expected UnavailableAPIError, got %#v", err)
		}
		if !strings.Contains(err.Error(), "500") {
			t.Errorf("error should mention HTTP 500, got %q", err.Error())
		}
	})

	t.Run("API timeout returns ErrEvaluationUnavailable", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(300 * time.Millisecond)
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		eval := NewTypedDecision(TypedDecisionOptions{
			Endpoint:   srv.URL,
			APITimeout: 50 * time.Millisecond,
			Getenv:     func(string) string { return "test-key" },
		})
		_, err := eval.Evaluate(context.Background(), freeformJob, adapter.Evidence{})
		if !errors.Is(err, ErrEvaluationUnavailable) {
			t.Fatalf("expected ErrEvaluationUnavailable, got %v", err)
		}
		var unavail *EvaluationUnavailableError
		if !errors.As(err, &unavail) || unavail.Kind != UnavailableTimeout {
			t.Fatalf("expected UnavailableTimeout, got %#v", err)
		}
	})

	invalidResponses := []struct {
		name string
		body string
	}{
		{name: "malformed JSON", body: `{not json`},
		{name: "missing answers field", body: `{}`},
		{name: "missing question key", body: `{"answers":{"other":{"noul":0.9}}}`},
		{name: "missing noul field", body: `{"answers":{"criterion_1":{"confidence":0.9}}}`},
		{name: "out-of-range noul > 1", body: `{"answers":{"criterion_1":{"noul":1.5}}}`},
		{name: "out-of-range noul < 0", body: `{"answers":{"criterion_1":{"noul":-0.2}}}`},
	}

	for _, tc := range invalidResponses {
		t.Run("invalid response: "+tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			eval := NewTypedDecision(TypedDecisionOptions{
				Endpoint: srv.URL,
				Getenv:   func(string) string { return "test-key" },
			})
			_, err := eval.Evaluate(context.Background(), freeformJob, adapter.Evidence{})
			if !errors.Is(err, ErrEvaluationUnavailable) {
				t.Fatalf("expected ErrEvaluationUnavailable, got %v", err)
			}
			var unavail *EvaluationUnavailableError
			if !errors.As(err, &unavail) || unavail.Kind != UnavailableInvalidResp {
				t.Fatalf("expected UnavailableInvalidResp, got %#v", err)
			}
		})
	}
}

func TestSharedInputTruncation(t *testing.T) {
	huge := strings.Repeat("a", 10000)
	job := &store.Job{
		Title:        strings.Repeat("t", 1000),
		Instructions: huge,
	}
	ev := adapter.Evidence{
		FinalReport: strings.Repeat("b", 10000) + "TAIL_MARKER",
		DiffSummary: huge,
	}
	out := buildDecisionSharedInput(context.Background(), job, ev)
	if len([]rune(out)) > maxSharedInputRunes+32 {
		t.Errorf("shared input rune length = %d, exceeded cap %d", len([]rune(out)), maxSharedInputRunes)
	}
	if !strings.Contains(out, "TAIL_MARKER") {
		t.Errorf("expected FinalReport tail marker to be preserved after truncation")
	}
	if !strings.Contains(out, "...(truncated)") {
		t.Errorf("expected truncation marker in oversized shared input")
	}
}

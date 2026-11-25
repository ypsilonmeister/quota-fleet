package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/evaluator"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

const (
	reviewTestKey  = "FAKE-DECISION-KEY-do-not-leak-0123456789"
	reviewTestPath = "/decision/review-verdict"
)

type fakeReviewJudge struct {
	hasKey   bool
	judgment evaluator.ReviewJudgment
	err      error
	calls    atomic.Int32
	lastText atomic.Value
}

func (f *fakeReviewJudge) ModelName() string { return "fake-decision-model" }
func (f *fakeReviewJudge) HasAPIKey() bool   { return f.hasKey }

func (f *fakeReviewJudge) JudgeReview(_ context.Context, text string) (evaluator.ReviewJudgment, error) {
	f.calls.Add(1)
	f.lastText.Store(text)
	return f.judgment, f.err
}

func reviewTestOptions(judge ReviewJudge, enabled bool) Options {
	return Options{
		InstanceID: "inst-review",
		Tokens: []config.APITokenConfig{
			{Name: "operator", Token: "tok-operate", Scopes: []string{config.ScopeOperate}},
			{Name: "watch", Token: "tok-read", Scopes: []string{config.ScopeRead}},
		},
		ReviewGateEnabled: enabled,
		ReviewJudge:       judge,
	}
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

func TestReviewVerdictReturnsVerdictAndRecordsDecision(t *testing.T) {
	judge := &fakeReviewJudge{hasKey: true, judgment: evaluator.ReviewJudgment{
		Verdict: evaluator.VerdictOKToMerge, Score: 0.91, Reason: "stub reason",
	}}
	_, st, ts := newTestServer(t, reviewTestOptions(judge, true))
	jobID, reviewJobID := int64(41), int64(42)

	resp := doJSON(t, http.MethodPost, ts.URL+reviewTestPath, "tok-operate", ReviewVerdictRequest{
		Text: "verdict: OK TO MERGE", JobID: &jobID, ReviewJobID: &reviewJobID,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, readBody(t, resp))
	}
	got := decodeResp[map[string]any](t, resp)
	want := map[string]any{"verdict": "ok_to_merge", "score": 0.91, "reason": "stub reason"}
	if len(got) != len(want) || got["verdict"] != want["verdict"] || got["score"] != want["score"] || got["reason"] != want["reason"] {
		t.Errorf("response = %v, want %v", got, want)
	}
	if text := judge.lastText.Load(); text != "verdict: OK TO MERGE" {
		t.Errorf("judge received %q", text)
	}

	// A second judgment without references is recorded with NULL ids.
	judge.judgment = evaluator.ReviewJudgment{Verdict: evaluator.VerdictChangesNeeded, Score: 0.1, Reason: "no"}
	resp = doJSON(t, http.MethodPost, ts.URL+reviewTestPath, "tok-operate", ReviewVerdictRequest{Text: "x"})
	if body := readBody(t, resp); resp.StatusCode != http.StatusOK || !strings.Contains(body, `"changes_needed"`) {
		t.Fatalf("status = %d body = %s, want 200 changes_needed", resp.StatusCode, body)
	}

	entries, err := st.ListDecisionLog(context.Background(), 0)
	if err != nil {
		t.Fatalf("ListDecisionLog: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("decision_log entries = %d, want 2", len(entries))
	}
	second, first := entries[0], entries[1]
	if first.Verdict != evaluator.VerdictOKToMerge || first.Score != 0.91 || first.Model != "fake-decision-model" || first.CreatedAt.IsZero() {
		t.Errorf("first entry = %+v", first)
	}
	if first.JobID == nil || *first.JobID != jobID || first.ReviewJobID == nil || *first.ReviewJobID != reviewJobID {
		t.Errorf("first entry ids = %v / %v, want %d / %d", first.JobID, first.ReviewJobID, jobID, reviewJobID)
	}
	if second.Verdict != evaluator.VerdictChangesNeeded || second.JobID != nil || second.ReviewJobID != nil {
		t.Errorf("second entry = %+v", second)
	}
}

func TestReviewVerdictFailsClosedWhenDisabledOrKeyMissing(t *testing.T) {
	okJudgment := evaluator.ReviewJudgment{Verdict: evaluator.VerdictOKToMerge, Score: 1}
	tests := []struct {
		name    string
		judge   *fakeReviewJudge
		enabled bool
		wantMsg string
	}{
		{"disabled", &fakeReviewJudge{hasKey: true, judgment: okJudgment}, false, "review_gate.enabled"},
		{"disabled without key", &fakeReviewJudge{judgment: okJudgment}, false, "review_gate.enabled"},
		{"key not configured", &fakeReviewJudge{judgment: okJudgment}, true, "API key is not configured"},
		{"no judge", nil, true, "API key is not configured"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var judge ReviewJudge
			if tc.judge != nil {
				judge = tc.judge
			}
			_, st, ts := newTestServer(t, reviewTestOptions(judge, tc.enabled))
			resp := doJSON(t, http.MethodPost, ts.URL+reviewTestPath, "tok-operate", ReviewVerdictRequest{Text: "verdict: OK TO MERGE"})
			body := readBody(t, resp)
			if resp.StatusCode != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503: %s", resp.StatusCode, body)
			}
			if !strings.Contains(body, tc.wantMsg) || strings.Contains(body, `"verdict"`) {
				t.Errorf("body = %s, want error mentioning %q and no verdict", body, tc.wantMsg)
			}
			if tc.judge != nil && tc.judge.calls.Load() != 0 {
				t.Errorf("judge was called %d times", tc.judge.calls.Load())
			}
			assertNoDecisionLog(t, st)
		})
	}
}

func TestReviewVerdictRequiresOperateScope(t *testing.T) {
	// The scope check comes first, whether or not the gate could answer.
	for _, enabled := range []bool{true, false} {
		judge := &fakeReviewJudge{hasKey: true, judgment: evaluator.ReviewJudgment{Verdict: evaluator.VerdictOKToMerge, Score: 1}}
		_, st, ts := newTestServer(t, reviewTestOptions(judge, enabled))
		resp := doJSON(t, http.MethodPost, ts.URL+reviewTestPath, "tok-read", ReviewVerdictRequest{Text: "x"})
		body := readBody(t, resp)
		if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, `"required_scope":"operate"`) {
			t.Fatalf("enabled=%v: status = %d body = %s, want 403 requiring operate", enabled, resp.StatusCode, body)
		}
		if judge.calls.Load() != 0 {
			t.Errorf("enabled=%v: judge was called", enabled)
		}
		assertNoDecisionLog(t, st)
	}
}

func TestReviewVerdictRejectsBadRequests(t *testing.T) {
	judge := &fakeReviewJudge{hasKey: true, judgment: evaluator.ReviewJudgment{Verdict: evaluator.VerdictOKToMerge, Score: 1}}
	_, st, ts := newTestServer(t, reviewTestOptions(judge, true))
	zero := int64(0)
	tests := []struct {
		name string
		body any
		want int
	}{
		{"no body", nil, http.StatusBadRequest},
		{"blank text", ReviewVerdictRequest{Text: " \n\t"}, http.StatusBadRequest},
		{"zero job_id", ReviewVerdictRequest{Text: "x", JobID: &zero}, http.StatusBadRequest},
		{"zero review_job_id", ReviewVerdictRequest{Text: "x", ReviewJobID: &zero}, http.StatusBadRequest},
		// Too long must be refused rather than cut: the cut part could hold an unresolved finding.
		{"too long", ReviewVerdictRequest{Text: strings.Repeat("あ", evaluator.MaxReviewTextRunes+1)}, http.StatusRequestEntityTooLarge},
	}
	for _, tc := range tests {
		resp := doJSON(t, http.MethodPost, ts.URL+reviewTestPath, "tok-operate", tc.body)
		if body := readBody(t, resp); resp.StatusCode != tc.want {
			t.Errorf("%s: status = %d, want %d: %s", tc.name, resp.StatusCode, tc.want, body)
		}
	}
	if judge.calls.Load() != 0 {
		t.Errorf("judge was called %d times for rejected requests", judge.calls.Load())
	}
	assertNoDecisionLog(t, st)

	resp := doJSON(t, http.MethodPost, ts.URL+reviewTestPath, "tok-operate",
		ReviewVerdictRequest{Text: strings.Repeat("あ", evaluator.MaxReviewTextRunes)})
	if body := readBody(t, resp); resp.StatusCode != http.StatusOK {
		t.Errorf("text at the limit: status = %d, want 200: %s", resp.StatusCode, body)
	}
}

func TestReviewVerdictJudgeFailures(t *testing.T) {
	unavailable := func(kind evaluator.EvaluationUnavailableKind) error {
		return &evaluator.EvaluationUnavailableError{Kind: kind, Reason: "upstream detail"}
	}
	tests := []struct {
		name     string
		judgment evaluator.ReviewJudgment
		err      error
		want     int
	}{
		{"api error", evaluator.ReviewJudgment{}, unavailable(evaluator.UnavailableAPIError), http.StatusBadGateway},
		{"invalid response", evaluator.ReviewJudgment{}, unavailable(evaluator.UnavailableInvalidResp), http.StatusBadGateway},
		{"timeout", evaluator.ReviewJudgment{}, unavailable(evaluator.UnavailableTimeout), http.StatusGatewayTimeout},
		{"key removed", evaluator.ReviewJudgment{}, unavailable(evaluator.UnavailableMissingAPIKey), http.StatusServiceUnavailable},
		{"other error", evaluator.ReviewJudgment{}, errors.New("upstream detail"), http.StatusBadGateway},
		{"unknown verdict", evaluator.ReviewJudgment{Verdict: "maybe"}, nil, http.StatusBadGateway},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			judge := &fakeReviewJudge{hasKey: true, judgment: tc.judgment, err: tc.err}
			_, st, ts := newTestServer(t, reviewTestOptions(judge, true))
			resp := doJSON(t, http.MethodPost, ts.URL+reviewTestPath, "tok-operate", ReviewVerdictRequest{Text: "x"})
			body := readBody(t, resp)
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d: %s", resp.StatusCode, tc.want, body)
			}
			if strings.Contains(body, `"verdict"`) || strings.Contains(body, "upstream detail") {
				t.Errorf("body = %s, want no verdict and no upstream detail", body)
			}
			assertNoDecisionLog(t, st)
		})
	}
}

func TestReviewVerdictFailsWhenDecisionCannotBeRecorded(t *testing.T) {
	judge := &fakeReviewJudge{hasKey: true, judgment: evaluator.ReviewJudgment{Verdict: evaluator.VerdictOKToMerge, Score: 1}}
	_, st, ts := newTestServer(t, reviewTestOptions(judge, true))
	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	resp := doJSON(t, http.MethodPost, ts.URL+reviewTestPath, "tok-operate", ReviewVerdictRequest{Text: "x"})
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusInternalServerError || strings.Contains(body, `"verdict"`) {
		t.Fatalf("status = %d body = %s, want 500 without verdict", resp.StatusCode, body)
	}
}

func TestMetaReportsReviewGate(t *testing.T) {
	for _, tc := range []struct{ enabled, hasKey bool }{{false, false}, {false, true}, {true, false}, {true, true}} {
		_, _, ts := newTestServer(t, reviewTestOptions(&fakeReviewJudge{hasKey: tc.hasKey}, tc.enabled))
		meta := decodeResp[MetaResponse](t, doJSON(t, http.MethodGet, ts.URL+"/meta", "tok-read", nil))
		want := ReviewGateDTO{Enabled: tc.enabled, Model: "fake-decision-model", KeyConfigured: tc.hasKey}
		if meta.ReviewGate != want {
			t.Errorf("review_gate = %+v, want %+v", meta.ReviewGate, want)
		}
	}

	_, _, ts := newTestServer(t, Options{Token: "tok"})
	meta := decodeResp[MetaResponse](t, doJSON(t, http.MethodGet, ts.URL+"/meta", "tok", nil))
	if want := (ReviewGateDTO{Model: evaluator.DefaultDecisionModel}); meta.ReviewGate != want {
		t.Errorf("default review_gate = %+v, want %+v", meta.ReviewGate, want)
	}
}

// Uses the real Decision client against a stub endpoint that echoes the key back, which is
// the way a key could reach a response or the log.
func TestReviewVerdictNeverExposesAPIKey(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		upstream string
		want     int
	}{
		{"success", http.StatusOK,
			`{"answers":{"verdict":{"noul":0.9},"unresolved":{"noul":0.9},"reviewer_reliability":{"noul":0.9}},"echo":"` + reviewTestKey + `"}`,
			http.StatusOK},
		{"error body", http.StatusBadRequest, "bad key " + reviewTestKey, http.StatusBadGateway},
		// The key straddles the point where the logged snippet is cut.
		{"error body cut inside the key", http.StatusBadRequest, strings.Repeat("a", 500) + reviewTestKey, http.StatusBadGateway},
		{"invalid JSON", http.StatusOK, reviewTestKey, http.StatusBadGateway},
	}
	for _, tc := range tests {
		for _, source := range []string{"api_key", "env"} {
			t.Run(tc.name+"/"+source, func(t *testing.T) {
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if got := r.Header.Get("x-goog-api-key"); got != reviewTestKey {
						t.Errorf("upstream received an unexpected key (length %d)", len(got))
					}
					w.WriteHeader(tc.status)
					_, _ = io.WriteString(w, tc.upstream)
				}))
				defer upstream.Close()

				opts := evaluator.TypedDecisionOptions{
					Endpoint: upstream.URL,
					Getenv:   func(string) string { return "" },
				}
				if source == "api_key" {
					opts.APIKey = reviewTestKey
				} else {
					opts.Getenv = func(string) string { return reviewTestKey }
				}
				var logs bytes.Buffer
				apiOpts := reviewTestOptions(evaluator.NewTypedDecision(opts), true)
				apiOpts.Logger = log.New(&logs, "", 0)
				_, st, ts := newTestServer(t, apiOpts)

				metaBody := readBody(t, doJSON(t, http.MethodGet, ts.URL+"/meta", "tok-read", nil))
				if !strings.Contains(metaBody, `"key_configured":true`) {
					t.Errorf("/meta = %s, want key_configured true", metaBody)
				}
				resp := doJSON(t, http.MethodPost, ts.URL+reviewTestPath, "tok-operate", ReviewVerdictRequest{Text: "x"})
				body := readBody(t, resp)
				if resp.StatusCode != tc.want {
					t.Fatalf("status = %d, want %d: %s", resp.StatusCode, tc.want, body)
				}
				entries, err := st.ListDecisionLog(context.Background(), 0)
				if err != nil {
					t.Fatalf("ListDecisionLog: %v", err)
				}

				// A prefix, so that a partly cut key is caught too.
				marker := reviewTestKey[:12]
				for what, content := range map[string]string{"/meta": metaBody, "response": body, "log": logs.String()} {
					if strings.Contains(content, marker) {
						t.Errorf("%s exposes the API key: %s", what, content)
					}
				}
				for _, e := range entries {
					if strings.Contains(e.Model+e.Verdict, marker) {
						t.Errorf("decision_log exposes the API key: %+v", e)
					}
				}
				if tc.want != http.StatusOK && !strings.Contains(logs.String(), "review verdict failed") {
					t.Errorf("failure was not logged: %q", logs.String())
				}
			})
		}
	}
}

func assertNoDecisionLog(t *testing.T, st *store.Store) {
	t.Helper()
	entries, err := st.ListDecisionLog(context.Background(), 0)
	if err != nil {
		t.Fatalf("ListDecisionLog: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("decision_log = %+v, want empty", entries)
	}
}

package evaluator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ypsilonmeister/quota-fleet/internal/adapter"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

func stubDecisions(t *testing.T, answers string, capture *decisionsRequest, key *string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if key != nil {
			*key = r.Header.Get("x-goog-api-key")
		}
		if capture != nil {
			if err := json.NewDecoder(r.Body).Decode(capture); err != nil {
				t.Errorf("decode request: %v", err)
			}
		}
		_, _ = io.WriteString(w, `{"answers":`+answers+`}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func reviewAnswers(verdict, unresolved, reliability float64) string {
	return fmt.Sprintf(`{"verdict":{"noul":%v},"unresolved":{"noul":%v},"reviewer_reliability":{"noul":%v}}`,
		verdict, unresolved, reliability)
}

func TestJudgeReviewVerdictRule(t *testing.T) {
	tests := []struct {
		name                             string
		threshold                        float64
		verdict, unresolved, reliability float64
		wantVerdict                      string
		wantScore                        float64
	}{
		{"both pass", 0.5, 0.9, 0.8, 0.9, VerdictOKToMerge, 0.8},
		{"both exactly at threshold", 0.5, 0.5, 0.5, 0.9, VerdictOKToMerge, 0.5},
		{"verdict fails", 0.5, 0.49, 0.9, 0.9, VerdictChangesNeeded, 0.49},
		{"unresolved remains", 0.5, 0.9, 0.2, 0.9, VerdictChangesNeeded, 0.2},
		{"reliability is informational", 0.5, 0.9, 0.9, 0.01, VerdictOKToMerge, 0.9},
		{"stricter threshold applies", 0.95, 0.9, 0.9, 0.9, VerdictChangesNeeded, 0.9},
		{"looser threshold is floored", 0.1, 0.3, 0.3, 0.9, VerdictChangesNeeded, 0.3},
		{"zero threshold is floored", 0, 0, 0, 0, VerdictChangesNeeded, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := stubDecisions(t, reviewAnswers(tc.verdict, tc.unresolved, tc.reliability), nil, nil)
			client := NewTypedDecision(TypedDecisionOptions{
				Endpoint: srv.URL, APIKey: "fake-key", DecisionThreshold: tc.threshold, ThresholdSet: true,
			})
			got, err := client.JudgeReview(context.Background(), "review body")
			if err != nil {
				t.Fatalf("JudgeReview: %v", err)
			}
			if got.Verdict != tc.wantVerdict || got.Score != tc.wantScore {
				t.Errorf("got %s score %v, want %s score %v (%s)", got.Verdict, got.Score, tc.wantVerdict, tc.wantScore, got.Reason)
			}
			for _, field := range []string{"verdict", "unresolved", "reviewer_reliability"} {
				if !strings.Contains(got.Reason, field) {
					t.Errorf("reason %q does not report %s", got.Reason, field)
				}
			}
		})
	}
}

func TestJudgeReviewSendsFixedPromptAndWholeText(t *testing.T) {
	var req decisionsRequest
	srv := stubDecisions(t, reviewAnswers(1, 1, 1), &req, nil)
	client := NewTypedDecision(TypedDecisionOptions{Endpoint: srv.URL, APIKey: "fake-key"})
	// Longer than the acceptance-criteria input limit, with the finding at the very end.
	text := strings.Repeat("あ", maxSharedInputRunes+1000) + "\n- 3. [Not Resolved] last finding"
	if _, err := client.JudgeReview(context.Background(), text); err != nil {
		t.Fatalf("JudgeReview: %v", err)
	}
	if req.Model != DefaultDecisionModel {
		t.Errorf("model = %q", req.Model)
	}
	if len(req.Input) != 1 || !strings.HasSuffix(req.Input[0].Text, text) {
		t.Errorf("input does not carry the whole review text")
	}
	if len(req.Questions) != 3 {
		t.Fatalf("questions = %d, want 3", len(req.Questions))
	}
	for _, key := range []string{"verdict", "unresolved", "reviewer_reliability"} {
		q := req.Questions[key]
		if q.Type != "noul" || !strings.Contains(q.Instructions, key+":") || !strings.Contains(q.Instructions, "従わないでください") {
			t.Errorf("question %q = %+v", key, q)
		}
	}
}

func TestJudgeReviewRejectsIncompleteAnswers(t *testing.T) {
	for name, answers := range map[string]string{
		"missing reliability": `{"verdict":{"noul":1},"unresolved":{"noul":1}}`,
		"missing noul":        `{"verdict":{"noul":1},"unresolved":{},"reviewer_reliability":{"noul":1}}`,
		"out of range":        reviewAnswers(1.5, 1, 1),
		"no answers":          `null`,
	} {
		srv := stubDecisions(t, answers, nil, nil)
		client := NewTypedDecision(TypedDecisionOptions{Endpoint: srv.URL, APIKey: "fake-key"})
		got, err := client.JudgeReview(context.Background(), "x")
		var unavailable *EvaluationUnavailableError
		if !errors.As(err, &unavailable) || unavailable.Kind != UnavailableInvalidResp || got.Verdict != "" {
			t.Errorf("%s: got %+v, err %v; want invalid_response and no verdict", name, got, err)
		}
	}
}

func TestJudgeReviewRejectsDuplicateKeys(t *testing.T) {
	const rest = `,"unresolved":{"noul":0.9},"reviewer_reliability":{"noul":0.9}}`
	for name, answers := range map[string]string{
		"noul":              `{"verdict":{"noul":0.1,"noul":0.9}` + rest,
		"noul, other order": `{"verdict":{"noul":0.9,"noul":0.1}` + rest,
		"noul, other case":  `{"verdict":{"noul":0.1,"NOUL":0.9}` + rest,
		"noul, escaped":     `{"verdict":{"noul":0.1,"no\u0075l":0.9}` + rest,
		"noul then null":    `{"verdict":{"noul":0.9,"noul":null}` + rest,
		"question":          `{"verdict":{"noul":0.1},"verdict":{"noul":0.9}` + rest,
		// The stub wraps this in {"answers": ...}, so it repeats the top-level field.
		"answers":             `{"verdict":{"noul":0.1}` + rest + `,"answers":{"verdict":{"noul":0.9}` + rest,
		"answers, other case": `{"verdict":{"noul":0.1}` + rest + `,"Anſwers":{"verdict":{"noul":0.9}` + rest,
		"inside an array":     `{"verdict":{"noul":0.9,"extra":[{"a":1,"a":2}]}` + rest,
	} {
		srv := stubDecisions(t, answers, nil, nil)
		client := NewTypedDecision(TypedDecisionOptions{Endpoint: srv.URL, APIKey: "fake-key"})
		got, err := client.JudgeReview(context.Background(), "x")
		var unavailable *EvaluationUnavailableError
		if !errors.As(err, &unavailable) || unavailable.Kind != UnavailableInvalidResp || got.Verdict != "" {
			t.Errorf("%s: got %+v, err %v; want invalid_response and no verdict", name, got, err)
		}
	}
}

// The same names in different objects are not duplicates, and fields the client ignores
// may hold anything that is valid JSON.
func TestJudgeReviewAcceptsRepeatedNamesInSeparateObjects(t *testing.T) {
	answers := `{"verdict":{"noul":0.9,"extra":[{"a":1},{"a":2},1e999]},` +
		`"unresolved":{"noul":0.9,"extra":{"noul":0}},"reviewer_reliability":{"noul":0.9}}`
	srv := stubDecisions(t, answers, nil, nil)
	client := NewTypedDecision(TypedDecisionOptions{Endpoint: srv.URL, APIKey: "fake-key"})
	got, err := client.JudgeReview(context.Background(), "x")
	if err != nil || got.Verdict != VerdictOKToMerge {
		t.Fatalf("got %+v, err %v; want ok_to_merge", got, err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type failingReader struct{ err error }

func (r failingReader) Read([]byte) (int, error) { return 0, r.err }

type timeoutError string

func (e timeoutError) Error() string   { return string(e) }
func (e timeoutError) Timeout() bool   { return true }
func (e timeoutError) Temporary() bool { return true }

func TestDecisionTransportErrorsRedactAPIKey(t *testing.T) {
	const key = "FAKE-DECISION-KEY-do-not-leak-0123456789"
	sendFails := func(err error) roundTripFunc {
		return func(*http.Request) (*http.Response, error) { return nil, err }
	}
	readFails := func(err error) roundTripFunc {
		return func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(failingReader{err})}, nil
		}
	}
	msg := "upstream said " + key
	for _, tc := range []struct {
		name      string
		transport roundTripFunc
		want      EvaluationUnavailableKind
	}{
		{"send", sendFails(errors.New(msg)), UnavailableAPIError},
		{"send timeout", sendFails(timeoutError(msg)), UnavailableTimeout},
		{"read", readFails(errors.New(msg)), UnavailableAPIError},
		{"read timeout", readFails(timeoutError(msg)), UnavailableTimeout},
	} {
		client := NewTypedDecision(TypedDecisionOptions{APIKey: key, HTTPClient: &http.Client{Transport: tc.transport}})
		_, judgeErr := client.JudgeReview(context.Background(), "x")
		// Acceptance evaluation stores the same error text in the job result.
		_, evalErr := client.Evaluate(context.Background(),
			&store.Job{Title: "t", AcceptanceCriteria: "the work is complete"}, adapter.Evidence{})
		for path, err := range map[string]error{"JudgeReview": judgeErr, "Evaluate": evalErr} {
			var unavailable *EvaluationUnavailableError
			if !errors.As(err, &unavailable) || unavailable.Kind != tc.want {
				t.Errorf("%s/%s: err = %v, want kind %s", tc.name, path, err, tc.want)
				continue
			}
			// Check the wrapped error too, not only the formatted message.
			full := err.Error()
			if inner := errors.Unwrap(err); inner != nil {
				full += " " + inner.Error()
			}
			if strings.Contains(full, key) || !strings.Contains(full, "upstream said [REDACTED]") {
				t.Errorf("%s/%s: err = %q, want the detail with the key redacted", tc.name, path, full)
			}
		}
	}
}

func TestJudgeReviewWithoutKeyDoesNotCallAPI(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("API must not be called without a key")
	}))
	defer srv.Close()
	client := NewTypedDecision(TypedDecisionOptions{Endpoint: srv.URL, Getenv: func(string) string { return "  " }})
	if client.HasAPIKey() {
		t.Error("HasAPIKey = true without a key")
	}
	_, err := client.JudgeReview(context.Background(), "x")
	var unavailable *EvaluationUnavailableError
	if !errors.As(err, &unavailable) || unavailable.Kind != UnavailableMissingAPIKey {
		t.Fatalf("err = %v, want missing_api_key", err)
	}
}

func TestDecisionClientAPIKeyPrecedence(t *testing.T) {
	env := func(name string) string {
		if name == "MY_KEY_ENV" {
			return "from-env"
		}
		return ""
	}
	for _, tc := range []struct{ apiKey, want string }{
		{"from-config", "from-config"},
		{"", "from-env"},
		{"   ", "from-env"},
	} {
		var sent string
		srv := stubDecisions(t, `{"criterion_1":{"noul":1}}`, nil, &sent)
		client := NewTypedDecision(TypedDecisionOptions{
			Endpoint: srv.URL, APIKey: tc.apiKey, APIKeyEnv: "MY_KEY_ENV", Getenv: env,
		})
		// Acceptance evaluation and the review gate share the same resolution.
		res, err := client.Evaluate(context.Background(),
			&store.Job{Title: "t", AcceptanceCriteria: "the work is complete"}, adapter.Evidence{})
		if err != nil || !res.Passed {
			t.Fatalf("api_key %q: Evaluate = %+v, %v", tc.apiKey, res, err)
		}
		if sent != tc.want {
			t.Errorf("api_key %q: sent key %q, want %q", tc.apiKey, sent, tc.want)
		}
	}
}

package evaluator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/adapter"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

const (
	// DefaultDecisionsEndpoint is the Gemini Decisions API endpoint.
	DefaultDecisionsEndpoint = "https://generativelanguage.googleapis.com/v1beta/decisions"
	// DefaultDecisionModel is the model identifier for Gemini Decision 1.
	DefaultDecisionModel = "gemini-decision-1"
	// DefaultAPIKeyEnv is the default environment variable containing the Gemini API key.
	DefaultAPIKeyEnv = "GEMINI_API_KEY"
	// DefaultDecisionThreshold is the default minimum noul probability for PASS.
	DefaultDecisionThreshold = 0.5
	// DefaultAPITimeout is the default timeout for a Decisions API request.
	DefaultAPITimeout = 60 * time.Second

	gitEvidenceTimeout = 5 * time.Second

	// Per-section rune limits for building the Decision 1 shared input.
	maxTitleRunes         = 500
	maxInstructionsRunes  = 4000
	maxFinalReportRunes   = 6000
	maxDiffSummaryRunes   = 4000
	maxArtifactFileRunes  = 4000
	maxGitOutputRunes     = 2000
	maxCommandOutputRunes = 1000
	maxSharedInputRunes   = 24000

	maxAPIErrorBodyBytes = 512
	maxAPIResponseBytes  = 4 << 20 // 4 MiB
)

// TypedDecisionOptions configures a TypedDecisionEvaluator.
type TypedDecisionOptions struct {
	// Endpoint overrides the Decisions API URL (defaults to DefaultDecisionsEndpoint).
	Endpoint string
	// Model overrides the Decisions model name (defaults to DefaultDecisionModel).
	Model string
	// APIKeyEnv is the environment variable name holding the Gemini API key (defaults to GEMINI_API_KEY).
	APIKeyEnv string
	// DecisionThreshold is the minimum noul probability required for PASS (defaults to 0.5).
	// Use ThresholdSet or leave 0 for the default 0.5.
	DecisionThreshold float64
	// ThresholdSet allows explicitly configuring a 0.0 threshold when true.
	ThresholdSet bool
	// CommandTimeout overrides the per-command timeout for `command:` criteria (defaults to 60s).
	CommandTimeout time.Duration
	// APITimeout overrides the HTTP request timeout for Decisions API calls (defaults to 60s).
	APITimeout time.Duration
	// HTTPClient overrides the HTTP client used to call the Decisions API.
	HTTPClient *http.Client
	// Getenv overrides os.Getenv for reading the API key (useful in tests).
	Getenv func(string) string
}

// TypedDecisionEvaluator evaluates acceptance criteria using deterministic typed checks
// (`file_exists:`, `file_nonempty:`, `contains:`, `command:`) first and Gemini Decision 1
// for any remaining free-form criteria.
type TypedDecisionEvaluator struct {
	// Endpoint is the Decisions API URL; can be overridden directly or via TypedDecisionOptions.
	Endpoint          string
	Model             string
	APIKeyEnv         string
	DecisionThreshold float64
	CommandTimeout    time.Duration
	APITimeout        time.Duration
	HTTPClient        *http.Client
	Getenv            func(string) string
}

// NewTypedDecision constructs a TypedDecisionEvaluator with sensible defaults.
func NewTypedDecision(opts TypedDecisionOptions) *TypedDecisionEvaluator {
	endpoint := strings.TrimSpace(opts.Endpoint)
	if endpoint == "" {
		endpoint = DefaultDecisionsEndpoint
	}
	model := strings.TrimSpace(opts.Model)
	if model == "" {
		model = DefaultDecisionModel
	}
	apiKeyEnv := strings.TrimSpace(opts.APIKeyEnv)
	if apiKeyEnv == "" {
		apiKeyEnv = DefaultAPIKeyEnv
	}
	threshold := opts.DecisionThreshold
	if !opts.ThresholdSet && threshold == 0 {
		threshold = DefaultDecisionThreshold
	}
	cmdTimeout := opts.CommandTimeout
	if cmdTimeout <= 0 {
		cmdTimeout = DefaultCommandTimeout
	}
	apiTimeout := opts.APITimeout
	if apiTimeout <= 0 {
		apiTimeout = DefaultAPITimeout
	}
	getenv := opts.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	return &TypedDecisionEvaluator{
		Endpoint:          endpoint,
		Model:             model,
		APIKeyEnv:         apiKeyEnv,
		DecisionThreshold: threshold,
		CommandTimeout:    cmdTimeout,
		APITimeout:        apiTimeout,
		HTTPClient:        opts.HTTPClient,
		Getenv:            getenv,
	}
}

type indexedTyped struct {
	idx int
	tc  typedCriterion
}

type indexedFreeform struct {
	idx  int
	text string
}

// Evaluate checks all typed criteria deterministically first. If any typed criterion fails,
// it returns overall FAIL immediately without calling Decision 1. If all typed criteria pass
// and no free-form criteria exist, it returns PASS without calling Decision 1 or requiring an API key.
// Otherwise, it batches all free-form criteria into a single Decisions API noul request.
func (e *TypedDecisionEvaluator) Evaluate(ctx context.Context, job *store.Job, ev adapter.Evidence) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if job == nil {
		return Result{}, ErrNilJob
	}

	criteria := ParseCriteria(job.AcceptanceCriteria)
	if len(criteria) == 0 {
		return Result{Passed: true}, nil
	}

	var (
		typedItems    []indexedTyped
		freeformItems []indexedFreeform
	)
	for i, line := range criteria {
		if tc, ok := parseTypedCriterion(line); ok {
			typedItems = append(typedItems, indexedTyped{idx: i, tc: tc})
		} else {
			freeformItems = append(freeformItems, indexedFreeform{idx: i, text: line})
		}
	}

	results := make([]CriterionResult, len(criteria))
	typedResults := make([]CriterionResult, 0, len(typedItems))
	allTypedPassed := true

	for _, item := range typedItems {
		cr, err := evaluateTypedCriterion(ctx, item.tc, job.Workdir, e.CommandTimeout)
		if err != nil {
			return Result{}, err
		}
		results[item.idx] = cr
		typedResults = append(typedResults, cr)
		if !cr.Passed {
			allTypedPassed = false
		}
	}

	// Short-circuit: if any typed check failed, fail immediately without calling Decision 1.
	if !allTypedPassed {
		return Result{
			Passed:   false,
			Criteria: typedResults,
		}, nil
	}

	// If no free-form criteria remain, complete without calling Decision 1 or requiring an API key.
	if len(freeformItems) == 0 {
		return Result{
			Passed:   true,
			Criteria: results,
		}, nil
	}

	// Free-form criteria require Gemini Decision 1.
	decisionResults, err := e.evaluateFreeform(ctx, job, ev, freeformItems)
	if err != nil {
		return Result{}, err
	}

	overall := true
	for i, item := range freeformItems {
		cr := decisionResults[i]
		results[item.idx] = cr
		if !cr.Passed {
			overall = false
		}
	}

	return Result{
		Passed:   overall,
		Criteria: results,
	}, nil
}

type decisionsRequest struct {
	Model     string                      `json:"model"`
	Input     []decisionsInputPart        `json:"input"`
	Questions map[string]decisionQuestion `json:"questions"`
}

type decisionsInputPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type decisionQuestion struct {
	Type         string `json:"type"`
	Instructions string `json:"instructions"`
}

type decisionsResponse struct {
	Answers map[string]decisionAnswer `json:"answers"`
	Usage   *decisionsUsage           `json:"usage,omitempty"`
}

type decisionAnswer struct {
	Noul       *float64 `json:"noul"`
	Confidence *float64 `json:"confidence"`
}

type decisionsUsage struct {
	TotalInputTokens int `json:"total_input_tokens"`
}

func (e *TypedDecisionEvaluator) evaluateFreeform(
	ctx context.Context,
	job *store.Job,
	ev adapter.Evidence,
	items []indexedFreeform,
) ([]CriterionResult, error) {
	envName := strings.TrimSpace(e.APIKeyEnv)
	if envName == "" {
		envName = DefaultAPIKeyEnv
	}
	getenv := e.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	apiKey := strings.TrimSpace(getenv(envName))
	if apiKey == "" {
		return nil, &EvaluationUnavailableError{
			Kind:   UnavailableMissingAPIKey,
			Reason: fmt.Sprintf("API key environment variable %s is not set", envName),
		}
	}

	sharedInput := buildDecisionSharedInput(ctx, job, ev)
	questions := make(map[string]decisionQuestion, len(items))
	keys := make([]string, len(items))
	for i, item := range items {
		key := fmt.Sprintf("criterion_%d", item.idx+1)
		keys[i] = key
		questions[key] = decisionQuestion{
			Type: "noul",
			Instructions: fmt.Sprintf(
				"Based on the shared job context and execution evidence, is the following acceptance criterion satisfied?\n\nCriterion: %s",
				item.text,
			),
		}
	}

	model := strings.TrimSpace(e.Model)
	if model == "" {
		model = DefaultDecisionModel
	}
	reqPayload := decisionsRequest{
		Model: model,
		Input: []decisionsInputPart{
			{Type: "text", Text: sharedInput},
		},
		Questions: questions,
	}

	bodyBytes, err := json.Marshal(reqPayload)
	if err != nil {
		return nil, &EvaluationUnavailableError{
			Kind:   UnavailableAPIError,
			Reason: "marshal decisions request",
			Err:    err,
		}
	}

	endpoint := strings.TrimSpace(e.Endpoint)
	if endpoint == "" {
		endpoint = DefaultDecisionsEndpoint
	}
	apiTimeout := e.APITimeout
	if apiTimeout <= 0 {
		apiTimeout = DefaultAPITimeout
	}

	reqCtx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, &EvaluationUnavailableError{
			Kind:   UnavailableAPIError,
			Reason: "build decisions request",
			Err:    err,
		}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", apiKey)

	client := e.effectiveHTTPClient(apiTimeout)
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if isTimeoutErr(reqCtx, err) {
			return nil, &EvaluationUnavailableError{
				Kind:   UnavailableTimeout,
				Reason: fmt.Sprintf("decisions API request timed out after %s", apiTimeout),
				Err:    err,
			}
		}
		return nil, &EvaluationUnavailableError{
			Kind:   UnavailableAPIError,
			Reason: "decisions API request failed",
			Err:    err,
		}
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxAPIResponseBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if isTimeoutErr(reqCtx, err) {
			return nil, &EvaluationUnavailableError{
				Kind:   UnavailableTimeout,
				Reason: "reading decisions API response timed out",
				Err:    err,
			}
		}
		return nil, &EvaluationUnavailableError{
			Kind:   UnavailableAPIError,
			Reason: "read decisions API response",
			Err:    err,
		}
	}
	if len(respBody) > maxAPIResponseBytes {
		return nil, &EvaluationUnavailableError{
			Kind:   UnavailableInvalidResp,
			Reason: fmt.Sprintf("decisions API response exceeded %d bytes", maxAPIResponseBytes),
		}
	}

	if resp.StatusCode != http.StatusOK {
		snippet := sanitizeOneLineSnippet(string(respBody), maxAPIErrorBodyBytes)
		reason := fmt.Sprintf("decisions API returned HTTP %d", resp.StatusCode)
		if snippet != "" {
			reason = fmt.Sprintf("%s: %s", reason, snippet)
		}
		return nil, &EvaluationUnavailableError{
			Kind:   UnavailableAPIError,
			Reason: reason,
		}
	}

	var parsed decisionsResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, &EvaluationUnavailableError{
			Kind:   UnavailableInvalidResp,
			Reason: "invalid JSON from decisions API",
			Err:    err,
		}
	}
	if parsed.Answers == nil {
		return nil, &EvaluationUnavailableError{
			Kind:   UnavailableInvalidResp,
			Reason: "decisions API response missing answers object",
		}
	}

	threshold := e.DecisionThreshold
	out := make([]CriterionResult, len(items))
	for i, item := range items {
		key := keys[i]
		ans, ok := parsed.Answers[key]
		if !ok {
			return nil, &EvaluationUnavailableError{
				Kind:   UnavailableInvalidResp,
				Reason: fmt.Sprintf("decisions API response missing answer for %q", key),
			}
		}
		if ans.Noul == nil {
			return nil, &EvaluationUnavailableError{
				Kind:   UnavailableInvalidResp,
				Reason: fmt.Sprintf("decisions API answer for %q missing noul probability", key),
			}
		}
		prob := *ans.Noul
		if math.IsNaN(prob) || math.IsInf(prob, 0) || prob < 0 || prob > 1 {
			return nil, &EvaluationUnavailableError{
				Kind:   UnavailableInvalidResp,
				Reason: fmt.Sprintf("decisions API answer for %q has invalid noul probability %v", key, prob),
			}
		}

		passed := prob >= threshold
		op := "<"
		if passed {
			op = ">="
		}
		out[i] = CriterionResult{
			Criterion: item.text,
			Passed:    passed,
			Evidence:  fmt.Sprintf("decision1 noul p=%.2f (%s %.2f)", prob, op, threshold),
		}
	}
	return out, nil
}

func (e *TypedDecisionEvaluator) effectiveHTTPClient(timeout time.Duration) *http.Client {
	if e.HTTPClient != nil {
		return e.HTTPClient
	}
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("redirect refused")
		},
	}
}

func isTimeoutErr(reqCtx context.Context, err error) bool {
	if errors.Is(reqCtx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func buildDecisionSharedInput(ctx context.Context, job *store.Job, ev adapter.Evidence) string {
	var b strings.Builder

	appendSection := func(heading, content string) {
		content = strings.TrimSpace(content)
		if content == "" {
			return
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "## %s\n%s", heading, content)
	}

	appendSection("Job Title", truncateHeadRunes(job.Title, maxTitleRunes))
	appendSection("Job Instructions", truncateHeadRunes(job.Instructions, maxInstructionsRunes))
	appendSection("Agent Final Report (agent.log tail)", truncateTailRunes(ev.FinalReport, maxFinalReportRunes))
	appendSection("Diff Summary", truncateHeadRunes(ev.DiffSummary, maxDiffSummaryRunes))

	if len(ev.Artifacts) > 0 {
		var present []string
		for name, exists := range ev.Artifacts {
			if exists {
				present = append(present, name)
			}
		}
		sort.Strings(present)
		if len(present) > 0 {
			appendSection("Observed Artifacts", strings.Join(present, ", "))
		}
	}

	if len(ev.CommandOutputs) > 0 {
		names := make([]string, 0, len(ev.CommandOutputs))
		for k := range ev.CommandOutputs {
			names = append(names, k)
		}
		sort.Strings(names)
		var cb strings.Builder
		for _, k := range names {
			val := truncateHeadRunes(strings.TrimSpace(ev.CommandOutputs[k]), maxCommandOutputRunes)
			if val == "" {
				continue
			}
			if cb.Len() > 0 {
				cb.WriteString("\n\n")
			}
			fmt.Fprintf(&cb, "[%s]\n%s", k, val)
		}
		appendSection("Command Outputs", cb.String())
	}

	wd := strings.TrimSpace(job.Workdir)
	if wd != "" {
		if summary := readOptionalArtifact(filepath.Join(wd, "summary.md"), maxArtifactFileRunes); summary != "" {
			appendSection("Artifact: summary.md", summary)
		}
		if resultTxt := readOptionalArtifact(filepath.Join(wd, "result.txt"), maxArtifactFileRunes); resultTxt != "" {
			appendSection("Artifact: result.txt", resultTxt)
		}
		if gitStatus := runOptionalGit(ctx, wd, "status", "--short"); gitStatus != "" {
			appendSection("Git Status (git status --short)", truncateHeadRunes(gitStatus, maxGitOutputRunes))
		}
		if gitDiffStat := runOptionalGit(ctx, wd, "diff", "--stat"); gitDiffStat != "" {
			appendSection("Git Diff Stat (git diff --stat)", truncateHeadRunes(gitDiffStat, maxGitOutputRunes))
		}
	}

	return truncateHeadRunes(b.String(), maxSharedInputRunes)
}

func readOptionalArtifact(path string, maxRunes int) string {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() || info.Size() == 0 {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	// Read enough bytes for maxRunes UTF-8 characters (up to 4 bytes per rune) plus a small buffer.
	limit := int64(maxRunes*4 + 256)
	data, err := io.ReadAll(io.LimitReader(f, limit))
	if err != nil {
		return ""
	}
	return truncateHeadRunes(strings.TrimSpace(string(data)), maxRunes)
}

func runOptionalGit(ctx context.Context, workdir string, args ...string) string {
	info, err := os.Stat(workdir)
	if err != nil || !info.IsDir() {
		return ""
	}
	gitCtx, cancel := context.WithTimeout(ctx, gitEvidenceTimeout)
	defer cancel()

	fullArgs := append([]string{"-C", workdir}, args...)
	cmd := exec.CommandContext(gitCtx, "git", fullArgs...)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func truncateHeadRunes(s string, maxRunes int) string {
	s = strings.TrimSpace(s)
	if maxRunes <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return strings.TrimSpace(string(runes[:maxRunes])) + "\n...(truncated)"
}

func truncateTailRunes(s string, maxRunes int) string {
	s = strings.TrimSpace(s)
	if maxRunes <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return "...(truncated)\n" + strings.TrimSpace(string(runes[len(runes)-maxRunes:]))
}

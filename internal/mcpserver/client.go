package mcpserver

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const (
	defaultHTTPTimeout      = 30 * time.Second
	defaultMaxResponseBytes = 512 << 20 // 512 MiB (accommodates max-limit list pages)
	maxErrorResponseBytes   = 64 << 10  // 64 KiB cap for non-2xx error bodies
	maxErrorMessageRunes    = 256
)

var (
	authHeaderRe  = regexp.MustCompile(`(?i)\bauthorization\s*[:=]\s*[^\r\n,;"]+`)
	bearerTokenRe = regexp.MustCompile(`(?i)\bbearer\s+\S+`)
	basicTokenRe  = regexp.MustCompile(`(?i)\bbasic\s+[A-Za-z0-9+/=]{4,}`)
	safeStatusRe  = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
)

// Client is a thin HTTP client for the quota-fleet JSON API.
type Client struct {
	baseURL          string
	token            string
	httpClient       *http.Client
	maxResponseBytes int64
}

// APIError represents a non-success HTTP response from the quota-fleet API.
type APIError struct {
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("quota-fleet API error (HTTP %d): %s", e.StatusCode, e.Message)
	}
	return fmt.Sprintf("quota-fleet API error (HTTP %d)", e.StatusCode)
}

// NewClient constructs an HTTP API client for quota-fleet.
func NewClient(baseURL, token string, httpClient *http.Client) (*Client, error) {
	trimmed := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if trimmed == "" {
		return nil, errors.New("QUOTA_FLEET_API_URL must not be empty")
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return nil, fmt.Errorf("invalid QUOTA_FLEET_API_URL %q: %w", trimmed, err)
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, fmt.Errorf("invalid QUOTA_FLEET_API_URL %q: must include http(s) scheme and host", trimmed)
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultHTTPTimeout}
	}
	return &Client{
		baseURL:          trimmed,
		token:            strings.TrimSpace(token),
		httpClient:       httpClient,
		maxResponseBytes: defaultMaxResponseBytes,
	}, nil
}

func (c *Client) doJSON(ctx context.Context, method, path string, query url.Values, body any) (int, []byte, error) {
	endpoint := c.baseURL + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}

	var reqBody io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, fmt.Errorf("marshal request body: %w", err)
		}
		reqBody = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, method, endpoint, reqBody)
	if err != nil {
		return 0, nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("request %s %s failed: %s", method, path, c.sanitizeErrorText(err.Error()))
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBytes, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorResponseBytes))
		if err != nil {
			return resp.StatusCode, nil, fmt.Errorf("read response from %s %s: %s", method, path, c.sanitizeErrorText(err.Error()))
		}
		return resp.StatusCode, respBytes, nil
	}

	limit := c.maxResponseBytes
	if limit <= 0 {
		limit = defaultMaxResponseBytes
	}
	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("read response from %s %s: %s", method, path, c.sanitizeErrorText(err.Error()))
	}
	if int64(len(respBytes)) > limit {
		return resp.StatusCode, nil, fmt.Errorf("response from %s %s exceeded %d bytes", method, path, limit)
	}
	return resp.StatusCode, respBytes, nil
}

func (c *Client) parseAPIError(status int, body []byte) error {
	// Never echo upstream response bodies for authentication/authorization failures.
	switch status {
	case http.StatusUnauthorized:
		return &APIError{StatusCode: status, Message: "unauthorized"}
	case http.StatusForbidden:
		return &APIError{StatusCode: status, Message: "forbidden"}
	}

	var envelope struct {
		Error         string `json:"error"`
		Reason        string `json:"reason"`
		CurrentStatus string `json:"current_status"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil {
		msg := strings.TrimSpace(envelope.Error)
		if msg == "" {
			msg = strings.TrimSpace(envelope.Reason)
		}
		msg = c.sanitizeErrorText(msg)
		if msg != "" {
			cs := strings.TrimSpace(envelope.CurrentStatus)
			if cs != "" && safeStatusRe.MatchString(cs) && (c.token == "" || !strings.Contains(cs, c.token)) && !strings.Contains(msg, cs) {
				msg = fmt.Sprintf("%s (current_status=%s)", msg, cs)
			}
			return &APIError{StatusCode: status, Message: msg}
		}
	}

	// Do not echo non-JSON (plain text / HTML) upstream or proxy bodies.
	return &APIError{StatusCode: status, Message: safeStatusMessage(status)}
}

func safeStatusMessage(status int) string {
	if text := strings.TrimSpace(http.StatusText(status)); text != "" {
		return text
	}
	return "unexpected error"
}

func (c *Client) sanitizeErrorText(s string) string {
	if s == "" {
		return ""
	}
	if c.token != "" {
		s = strings.ReplaceAll(s, c.token, "[REDACTED]")
	}
	s = authHeaderRe.ReplaceAllString(s, "Authorization: [REDACTED]")
	s = bearerTokenRe.ReplaceAllString(s, "Bearer [REDACTED]")
	s = basicTokenRe.ReplaceAllString(s, "Basic [REDACTED]")

	var b strings.Builder
	b.Grow(len(s))
	lastSpace := false
	for _, r := range s {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			if !lastSpace {
				b.WriteByte(' ')
				lastSpace = true
			}
			continue
		}
		b.WriteRune(r)
		lastSpace = false
	}
	out := strings.TrimSpace(b.String())
	runes := []rune(out)
	if len(runes) > maxErrorMessageRunes {
		out = string(runes[:maxErrorMessageRunes]) + "..."
	}
	return out
}

func decodeObject(body []byte) (map[string]any, error) {
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode JSON response: %w", err)
	}
	return out, nil
}

// WorktreeSpec defines optional git worktree creation parameters for a job.
type WorktreeSpec struct {
	RepoPath string `json:"repo_path" jsonschema:"Path to the base git repository on the worker host"`
	BaseRef  string `json:"base_ref,omitempty" jsonschema:"Base git ref to branch from"`
	Branch   string `json:"branch,omitempty" jsonschema:"Target branch name for the worktree"`
}

// CreateJobInput is the input schema for the create_job MCP tool.
type CreateJobInput struct {
	Title              string        `json:"title" jsonschema:"Job title (required)"`
	RequestID          string        `json:"request_id,omitempty" jsonschema:"Idempotency key; auto-generated if omitted"`
	Draft              bool          `json:"draft,omitempty" jsonschema:"Create job in draft status instead of queued"`
	Agent              string        `json:"agent,omitempty" jsonschema:"Agent name (e.g. claude, codex, agy)"`
	Host               string        `json:"host,omitempty" jsonschema:"Target worker host name"`
	Workdir            string        `json:"workdir,omitempty" jsonschema:"Working directory on the worker host"`
	WorktreeSpec       *WorktreeSpec `json:"worktree_spec,omitempty" jsonschema:"Optional git worktree specification"`
	TargetRef          string        `json:"target_ref,omitempty" jsonschema:"Target git ref or branch"`
	Instructions       string        `json:"instructions,omitempty" jsonschema:"Task instructions for the worker agent"`
	AcceptanceCriteria string        `json:"acceptance_criteria,omitempty" jsonschema:"Acceptance criteria evaluated upon completion"`
	ParentID           *int64        `json:"parent_id,omitempty" jsonschema:"Parent job ID for chaining"`
	Model              string        `json:"model,omitempty" jsonschema:"Model override"`
	Pool               string        `json:"pool,omitempty" jsonschema:"Primary quota pool name"`
	AltPools           []string      `json:"alt_pools,omitempty" jsonschema:"Alternative quota pool names"`
	SessionID          string        `json:"session_id,omitempty" jsonschema:"Session identifier"`
}

// CreateJobOutput is the structured result returned by create_job.
// HTTP 202 (Quota Gate hold) is returned as a normal result with Held=true, HeldID, and Reason.
type CreateJobOutput struct {
	Held            bool           `json:"held"`
	Created         bool           `json:"created"`
	Job             map[string]any `json:"job,omitempty"`
	HeldID          string         `json:"held_id,omitempty"`
	Reason          string         `json:"reason,omitempty"`
	SnapshotSummary string         `json:"snapshot_summary,omitempty"`
	HeldJob         map[string]any `json:"held_job,omitempty"`
}

func (c *Client) CreateJob(ctx context.Context, in CreateJobInput) (*CreateJobOutput, error) {
	if strings.TrimSpace(in.Title) == "" {
		return nil, errors.New("title is required")
	}
	if strings.TrimSpace(in.RequestID) == "" {
		reqID, err := generateMCPRequestID()
		if err != nil {
			return nil, fmt.Errorf("generate request_id: %w", err)
		}
		in.RequestID = reqID
	}

	status, body, err := c.doJSON(ctx, http.MethodPost, "/jobs", nil, in)
	if err != nil {
		return nil, err
	}

	switch status {
	case http.StatusOK, http.StatusCreated:
		var resp struct {
			Job     map[string]any `json:"job"`
			Created bool           `json:"created"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			return nil, fmt.Errorf("decode create job response: %w", err)
		}
		return &CreateJobOutput{
			Held:    false,
			Created: resp.Created,
			Job:     resp.Job,
		}, nil

	case http.StatusAccepted:
		var resp struct {
			HeldID  string         `json:"held_id"`
			Held    map[string]any `json:"held"`
			Created bool           `json:"created"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			return nil, fmt.Errorf("decode held job response: %w", err)
		}
		var reason, snapshotSummary string
		if resp.Held != nil {
			if r, ok := resp.Held["reason"].(string); ok {
				reason = r
			}
			if s, ok := resp.Held["snapshot_summary"].(string); ok {
				snapshotSummary = s
			}
		}
		return &CreateJobOutput{
			Held:            true,
			Created:         resp.Created,
			HeldID:          resp.HeldID,
			Reason:          reason,
			SnapshotSummary: snapshotSummary,
			HeldJob:         resp.Held,
		}, nil

	default:
		return nil, c.parseAPIError(status, body)
	}
}

// ListJobsInput is the input schema for the list_jobs MCP tool.
type ListJobsInput struct {
	Status   string `json:"status" jsonschema:"Job status filter (required): draft, queued, claimed, waiting, done, failed, or cancelled"`
	Agent    string `json:"agent,omitempty" jsonschema:"Filter by agent name"`
	Host     string `json:"host,omitempty" jsonschema:"Filter by worker host name"`
	Query    string `json:"q,omitempty" jsonschema:"Substring filter on job title"`
	ParentID *int64 `json:"parent_id,omitempty" jsonschema:"Filter by parent job ID"`
	Cursor   *int64 `json:"cursor,omitempty" jsonschema:"Pagination cursor (job ID)"`
	Limit    *int   `json:"limit,omitempty" jsonschema:"Maximum number of jobs to return"`
	Desc     *bool  `json:"desc,omitempty" jsonschema:"Order descending by job ID"`
}

func (c *Client) ListJobs(ctx context.Context, in ListJobsInput) (map[string]any, error) {
	st := strings.TrimSpace(in.Status)
	if st == "" {
		return nil, errors.New("status is required")
	}
	q := url.Values{}
	q.Set("status", st)
	if v := strings.TrimSpace(in.Agent); v != "" {
		q.Set("agent", v)
	}
	if v := strings.TrimSpace(in.Host); v != "" {
		q.Set("host", v)
	}
	if v := strings.TrimSpace(in.Query); v != "" {
		q.Set("q", v)
	}
	if in.ParentID != nil {
		q.Set("parent_id", strconv.FormatInt(*in.ParentID, 10))
	}
	if in.Cursor != nil {
		q.Set("cursor", strconv.FormatInt(*in.Cursor, 10))
	}
	if in.Limit != nil {
		q.Set("limit", strconv.Itoa(*in.Limit))
	}
	if in.Desc != nil {
		q.Set("desc", strconv.FormatBool(*in.Desc))
	}

	status, body, err := c.doJSON(ctx, http.MethodGet, "/jobs", q, nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, c.parseAPIError(status, body)
	}
	return decodeObject(body)
}

// JobIDInput is the input schema for job-ID-only tools (get_job, requeue_job, mark_delivered).
type JobIDInput struct {
	ID int64 `json:"id" jsonschema:"Job ID (positive integer)"`
}

func (c *Client) GetJob(ctx context.Context, id int64) (map[string]any, error) {
	if id <= 0 {
		return nil, fmt.Errorf("invalid job id %d: must be positive", id)
	}
	status, body, err := c.doJSON(ctx, http.MethodGet, fmt.Sprintf("/jobs/%d", id), nil, nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, c.parseAPIError(status, body)
	}
	return decodeObject(body)
}

// CancelJobInput is the input schema for the cancel_job MCP tool.
type CancelJobInput struct {
	ID     int64  `json:"id" jsonschema:"Job ID to cancel (must be in draft, queued, or waiting status)"`
	Reason string `json:"reason,omitempty" jsonschema:"Optional cancellation reason"`
}

func (c *Client) CancelJob(ctx context.Context, in CancelJobInput) (map[string]any, error) {
	if in.ID <= 0 {
		return nil, fmt.Errorf("invalid job id %d: must be positive", in.ID)
	}
	var payload any
	if r := strings.TrimSpace(in.Reason); r != "" {
		payload = map[string]string{"reason": r}
	}
	status, body, err := c.doJSON(ctx, http.MethodPost, fmt.Sprintf("/jobs/%d/cancel", in.ID), nil, payload)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, c.parseAPIError(status, body)
	}
	return decodeObject(body)
}

func (c *Client) RequeueJob(ctx context.Context, id int64) (map[string]any, error) {
	if id <= 0 {
		return nil, fmt.Errorf("invalid job id %d: must be positive", id)
	}
	status, body, err := c.doJSON(ctx, http.MethodPost, fmt.Sprintf("/jobs/%d/requeue", id), nil, nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, c.parseAPIError(status, body)
	}
	return decodeObject(body)
}

func (c *Client) MarkDelivered(ctx context.Context, id int64) (map[string]any, error) {
	if id <= 0 {
		return nil, fmt.Errorf("invalid job id %d: must be positive", id)
	}
	status, body, err := c.doJSON(ctx, http.MethodPost, fmt.Sprintf("/jobs/%d/deliver", id), nil, nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, c.parseAPIError(status, body)
	}
	return decodeObject(body)
}

// ListHeldInput is the input schema for the list_held MCP tool.
type ListHeldInput struct {
	Status string `json:"status,omitempty" jsonschema:"Held job status filter: held, released, cancelled, or all (default: held)"`
	Limit  *int   `json:"limit,omitempty" jsonschema:"Maximum number of held jobs to return"`
}

func (c *Client) ListHeld(ctx context.Context, in ListHeldInput) (map[string]any, error) {
	q := url.Values{}
	if v := strings.TrimSpace(in.Status); v != "" {
		q.Set("status", v)
	}
	if in.Limit != nil {
		q.Set("limit", strconv.Itoa(*in.Limit))
	}
	status, body, err := c.doJSON(ctx, http.MethodGet, "/gate/held", q, nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, c.parseAPIError(status, body)
	}
	return decodeObject(body)
}

// HeldIDInput is the input schema for held-ID-only tools (release_held, cancel_held).
type HeldIDInput struct {
	ID string `json:"id" jsonschema:"Held job ID"`
}

// ReleaseHeldOutput is the structured result returned by release_held.
type ReleaseHeldOutput struct {
	Released bool   `json:"released"`
	JobID    int64  `json:"job_id,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

func (c *Client) ReleaseHeld(ctx context.Context, id string) (*ReleaseHeldOutput, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, errors.New("held job id is required")
	}
	path := "/gate/held/" + url.PathEscape(id) + "/release"
	status, body, err := c.doJSON(ctx, http.MethodPost, path, nil, nil)
	if err != nil {
		return nil, err
	}
	switch status {
	case http.StatusOK:
		var out ReleaseHeldOutput
		if err := json.Unmarshal(body, &out); err != nil {
			return nil, fmt.Errorf("decode release held response: %w", err)
		}
		return &out, nil
	case http.StatusConflict:
		var out ReleaseHeldOutput
		if err := json.Unmarshal(body, &out); err == nil && !out.Released && out.Reason != "" {
			return &out, nil
		}
		return nil, c.parseAPIError(status, body)
	default:
		return nil, c.parseAPIError(status, body)
	}
}

func (c *Client) CancelHeld(ctx context.Context, id string) (map[string]any, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, errors.New("held job id is required")
	}
	path := "/gate/held/" + url.PathEscape(id)
	status, body, err := c.doJSON(ctx, http.MethodDelete, path, nil, nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, c.parseAPIError(status, body)
	}
	return decodeObject(body)
}

// GateDecisionsInput is the input schema for the gate_decisions MCP tool.
type GateDecisionsInput struct {
	JobID  *int64 `json:"job_id,omitempty" jsonschema:"Filter by job ID"`
	HeldID string `json:"held_id,omitempty" jsonschema:"Filter by held job ID"`
	Limit  *int   `json:"limit,omitempty" jsonschema:"Maximum number of decisions to return"`
}

func (c *Client) GateDecisions(ctx context.Context, in GateDecisionsInput) (map[string]any, error) {
	q := url.Values{}
	if in.JobID != nil {
		q.Set("job_id", strconv.FormatInt(*in.JobID, 10))
	}
	if v := strings.TrimSpace(in.HeldID); v != "" {
		q.Set("held_id", v)
	}
	if in.Limit != nil {
		q.Set("limit", strconv.Itoa(*in.Limit))
	}
	status, body, err := c.doJSON(ctx, http.MethodGet, "/gate/decisions", q, nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, c.parseAPIError(status, body)
	}
	return decodeObject(body)
}

// ListDeliveriesInput is the input schema for the list_deliveries MCP tool.
type ListDeliveriesInput struct {
	Status string `json:"status,omitempty" jsonschema:"Delivery status filter: undelivered, delivered, or all (default: undelivered)"`
	Limit  *int   `json:"limit,omitempty" jsonschema:"Maximum number of deliveries to return"`
}

func (c *Client) ListDeliveries(ctx context.Context, in ListDeliveriesInput) (map[string]any, error) {
	q := url.Values{}
	if v := strings.TrimSpace(in.Status); v != "" {
		q.Set("status", v)
	}
	if in.Limit != nil {
		q.Set("limit", strconv.Itoa(*in.Limit))
	}
	status, body, err := c.doJSON(ctx, http.MethodGet, "/deliveries", q, nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, c.parseAPIError(status, body)
	}
	return decodeObject(body)
}

// FleetStatusInput is the empty input schema for the fleet_status MCP tool.
type FleetStatusInput struct{}

func (c *Client) FleetStatus(ctx context.Context) (map[string]any, error) {
	status, body, err := c.doJSON(ctx, http.MethodGet, "/status", nil, nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, c.parseAPIError(status, body)
	}
	return decodeObject(body)
}

func generateMCPRequestID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "mcp-" + hex.EncodeToString(b[:]), nil
}

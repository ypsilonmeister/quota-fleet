package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

const (
	// DefaultWebhookInitialBackoff is the initial exponential backoff delay before the first webhook retry.
	DefaultWebhookInitialBackoff = 200 * time.Millisecond

	maxWebhookBackoff        = 30 * time.Second
	maxWebhookErrorBodyBytes = 256
)

// Report describes an operational anomaly or recovery action emitted by Reconciler.
type Report struct {
	DedupKey  string    `json:"dedup_key"`
	Kind      string    `json:"kind"`
	JobID     int64     `json:"job_id"`
	ParentID  *int64    `json:"parent_id,omitempty"`
	Message   string    `json:"message"`
	Timestamp time.Time `json:"timestamp"`
}

// Notifier delivers terminal job results and reconciler reports to an external sink.
type Notifier interface {
	Deliver(ctx context.Context, job *store.Job) error
	Report(ctx context.Context, report Report) error
}

// DeliveryPayload is the JSON schema emitted per job by FileNotifier and WebhookNotifier.
type DeliveryPayload struct {
	JobID       int64      `json:"job_id"`
	RequestID   string     `json:"request_id"`
	Title       string     `json:"title"`
	Status      string     `json:"status"`
	Agent       string     `json:"agent,omitempty"`
	Host        string     `json:"host,omitempty"`
	Workdir     string     `json:"workdir,omitempty"`
	TargetRef   string     `json:"target_ref,omitempty"`
	ParentID    *int64     `json:"parent_id,omitempty"`
	Attempts    int        `json:"attempts"`
	ResultText  string     `json:"result_text,omitempty"`
	LastError   string     `json:"last_error,omitempty"`
	DeliveredAt time.Time  `json:"delivered_at"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

func buildDeliveryPayload(job *store.Job, deliveredAt time.Time) DeliveryPayload {
	return DeliveryPayload{
		JobID:       job.ID,
		RequestID:   job.RequestID,
		Title:       job.Title,
		Status:      string(job.Status),
		Agent:       job.Agent,
		Host:        job.Host,
		Workdir:     job.Workdir,
		TargetRef:   job.TargetRef,
		ParentID:    job.ParentID,
		Attempts:    job.Attempts,
		ResultText:  job.ResultText,
		LastError:   job.LastError,
		DeliveredAt: deliveredAt,
		CreatedAt:   job.CreatedAt,
		UpdatedAt:   job.UpdatedAt,
	}
}

// FileNotifier writes one JSON file per delivered job into a deliveries directory
// and records delivered_at via Store.MarkDelivered. Reports are appended to reports.jsonl.
type FileNotifier struct {
	dir   string
	store *store.Store
	mu    sync.Mutex
}

// NewFileNotifier creates a FileNotifier targeting dir. If st is non-nil, Deliver
// also calls st.MarkDelivered after writing the delivery file.
func NewFileNotifier(dir string, st *store.Store) *FileNotifier {
	return &FileNotifier{
		dir:   dir,
		store: st,
	}
}

// Deliver atomically writes <dir>/job-<id>.json and marks the job delivered in Store.
func (n *FileNotifier) Deliver(ctx context.Context, job *store.Job) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if job == nil {
		return errors.New("notifier: job must not be nil")
	}
	if err := os.MkdirAll(n.dir, 0o755); err != nil {
		return fmt.Errorf("create deliveries dir %q: %w", n.dir, err)
	}

	payload := buildDeliveryPayload(job, time.Now().UTC())
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal delivery payload for job %d: %w", job.ID, err)
	}
	data = append(data, '\n')

	targetPath := filepath.Join(n.dir, fmt.Sprintf("job-%d.json", job.ID))
	tmpFile, err := os.CreateTemp(n.dir, fmt.Sprintf(".job-%d-*.tmp", job.ID))
	if err != nil {
		return fmt.Errorf("create temp delivery file: %w", err)
	}
	tmpPath := tmpFile.Name()
	if _, err := tmpFile.Write(data); err != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("write temp delivery file: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close temp delivery file: %w", err)
	}
	if err := os.Rename(tmpPath, targetPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename delivery file %q: %w", targetPath, err)
	}

	if n.store != nil {
		if _, err := n.store.MarkDelivered(ctx, job.ID); err != nil {
			return fmt.Errorf("mark job %d delivered: %w", job.ID, err)
		}
	}
	return nil
}

// Report appends a reconciler report record as a single JSON line to <dir>/reports.jsonl.
func (n *FileNotifier) Report(ctx context.Context, report Report) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if report.Timestamp.IsZero() {
		report.Timestamp = time.Now().UTC()
	}
	if err := os.MkdirAll(n.dir, 0o755); err != nil {
		return fmt.Errorf("create deliveries dir %q: %w", n.dir, err)
	}
	line, err := json.Marshal(report)
	if err != nil {
		return fmt.Errorf("marshal report: %w", err)
	}
	line = append(line, '\n')

	n.mu.Lock()
	defer n.mu.Unlock()
	path := filepath.Join(n.dir, "reports.jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open reports file %q: %w", path, err)
	}
	_, werr := f.Write(line)
	cerr := f.Close()
	if werr != nil {
		return fmt.Errorf("write reports file %q: %w", path, werr)
	}
	return cerr
}

// WebhookNotifierOptions configures a WebhookNotifier.
type WebhookNotifierOptions struct {
	// URL is the target HTTP/HTTPS endpoint. Empty disables webhook delivery.
	URL string
	// Timeout is the per-attempt HTTP request timeout (defaults to config.DefaultWebhookTimeout).
	Timeout time.Duration
	// Retries is the number of retries after the initial attempt (defaults to config.DefaultWebhookRetries).
	// Set RetriesSet to true to explicitly allow 0 retries (1 attempt total).
	Retries    int
	RetriesSet bool
	// InitialBackoff is the initial exponential backoff delay before the first retry
	// (defaults to DefaultWebhookInitialBackoff).
	InitialBackoff time.Duration
	// HTTPClient overrides the HTTP client used for POST requests.
	HTTPClient *http.Client
	// Logger receives error logs when webhook delivery fails (defaults to log.Default()).
	Logger *log.Logger
}

// WebhookNotifier POSTs job deliveries and reconciler reports as JSON to a configured URL.
// Delivery failures are retried with exponential backoff, logged on exhaustion, and never fail
// the Deliver or Report call itself (both return nil).
type WebhookNotifier struct {
	url            string
	timeout        time.Duration
	retries        int
	initialBackoff time.Duration
	client         *http.Client
	logger         *log.Logger
}

// NewWebhookNotifier constructs a WebhookNotifier with sensible defaults.
func NewWebhookNotifier(opts WebhookNotifierOptions) *WebhookNotifier {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = config.DefaultWebhookTimeout
	}
	retries := opts.Retries
	if (!opts.RetriesSet && retries == 0) || retries < 0 {
		retries = config.DefaultWebhookRetries
	}
	initialBackoff := opts.InitialBackoff
	if initialBackoff <= 0 {
		initialBackoff = DefaultWebhookInitialBackoff
	}
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{}
	}
	logger := opts.Logger
	if logger == nil {
		logger = log.Default()
	}
	return &WebhookNotifier{
		url:            strings.TrimSpace(opts.URL),
		timeout:        timeout,
		retries:        retries,
		initialBackoff: initialBackoff,
		client:         client,
		logger:         logger,
	}
}

// Deliver POSTs the job's DeliveryPayload JSON to the configured webhook URL.
// If webhook_url is empty, Deliver is a no-op. Failures are logged and never returned.
func (n *WebhookNotifier) Deliver(ctx context.Context, job *store.Job) error {
	if n == nil || n.url == "" {
		return nil
	}
	if job == nil {
		n.logger.Printf("webhook delivery skipped: job is nil")
		return nil
	}

	payload := buildDeliveryPayload(job, time.Now().UTC())
	data, err := json.Marshal(payload)
	if err != nil {
		n.logger.Printf("webhook delivery failed (marshal job #%d): %v", job.ID, err)
		return nil
	}
	n.postWithRetry(ctx, fmt.Sprintf("job #%d", job.ID), data)
	return nil
}

// Report POSTs the reconciler Report JSON to the configured webhook URL.
// If webhook_url is empty, Report is a no-op. Failures are logged and never returned.
func (n *WebhookNotifier) Report(ctx context.Context, report Report) error {
	if n == nil || n.url == "" {
		return nil
	}
	if report.Timestamp.IsZero() {
		report.Timestamp = time.Now().UTC()
	}

	data, err := json.Marshal(report)
	if err != nil {
		n.logger.Printf("webhook report failed (marshal %q): %v", report.DedupKey, err)
		return nil
	}
	n.postWithRetry(ctx, fmt.Sprintf("report %q (job #%d)", report.DedupKey, report.JobID), data)
	return nil
}

func (n *WebhookNotifier) postWithRetry(ctx context.Context, targetDesc string, body []byte) {
	maxAttempts := 1 + n.retries
	backoff := n.initialBackoff
	var lastErr error
	attemptsRun := 0

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if ctx.Err() != nil {
			lastErr = ctx.Err()
			break
		}
		attemptsRun = attempt
		err := n.postOnce(ctx, body)
		if err == nil {
			return
		}
		lastErr = err

		if attempt < maxAttempts {
			if ctx.Err() != nil {
				lastErr = ctx.Err()
				break
			}
			timer := time.NewTimer(backoff)
			select {
			case <-ctx.Done():
				timer.Stop()
				lastErr = ctx.Err()
				n.logger.Printf("webhook delivery failed (%s) to %s after %d/%d attempts: %v",
					targetDesc, redactWebhookURL(n.url), attemptsRun, maxAttempts, lastErr)
				return
			case <-timer.C:
			}
			if backoff < maxWebhookBackoff {
				backoff *= 2
				if backoff > maxWebhookBackoff {
					backoff = maxWebhookBackoff
				}
			}
		}
	}

	n.logger.Printf("webhook delivery failed (%s) to %s after %d/%d attempts: %v",
		targetDesc, redactWebhookURL(n.url), attemptsRun, maxAttempts, lastErr)
}

func (n *WebhookNotifier) postOnce(ctx context.Context, body []byte) error {
	attemptCtx, cancel := context.WithTimeout(ctx, n.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, n.url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := n.client.Do(req)
	if err != nil {
		return err
	}
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, maxWebhookErrorBodyBytes))
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()

	if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
		return nil
	}
	msg := strings.TrimSpace(string(snippet))
	if msg != "" {
		return fmt.Errorf("unexpected HTTP status %d: %s", resp.StatusCode, msg)
	}
	return fmt.Errorf("unexpected HTTP status %d", resp.StatusCode)
}

func redactWebhookURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<invalid-url>"
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

// MultiNotifier dispatches Deliver and Report calls sequentially to multiple Notifiers.
// Placing FileNotifier before WebhookNotifier ensures local durable recording happens first
// and its errors propagate, while WebhookNotifier logs its own failures and returns nil.
type MultiNotifier struct {
	notifiers []Notifier
}

// NewMultiNotifier creates a MultiNotifier invoking non-nil notifiers in order.
func NewMultiNotifier(notifiers ...Notifier) *MultiNotifier {
	filtered := make([]Notifier, 0, len(notifiers))
	for _, n := range notifiers {
		if n != nil {
			filtered = append(filtered, n)
		}
	}
	return &MultiNotifier{notifiers: filtered}
}

// Deliver invokes Deliver on each notifier in order, stopping on the first error.
func (m *MultiNotifier) Deliver(ctx context.Context, job *store.Job) error {
	for _, n := range m.notifiers {
		if err := n.Deliver(ctx, job); err != nil {
			return err
		}
	}
	return nil
}

// Report invokes Report on each notifier in order, stopping on the first error.
func (m *MultiNotifier) Report(ctx context.Context, report Report) error {
	for _, n := range m.notifiers {
		if err := n.Report(ctx, report); err != nil {
			return err
		}
	}
	return nil
}

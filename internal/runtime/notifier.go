package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/store"
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

// DeliveryPayload is the JSON schema written per job by FileNotifier.
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

	now := time.Now().UTC()
	payload := DeliveryPayload{
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
		DeliveredAt: now,
		CreatedAt:   job.CreatedAt,
		UpdatedAt:   job.UpdatedAt,
	}
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

package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

const outboxBatchSize = 50

// SignalRecord is the 8-field audit JSONL format defined in docs/02-job-model.md.
type SignalRecord struct {
	V       int    `json:"v"`
	EventID string `json:"event_id"`
	JobID   int64  `json:"job_id"`
	Env     string `json:"env"`
	Src     string `json:"src"`
	Hop     int    `json:"hop"`
	Ts      string `json:"ts"`
	Event   string `json:"event"`
}

// DispatcherOptions configures a Dispatcher.
type DispatcherOptions struct {
	Store          *store.Store
	NotifyCh       chan<- struct{}
	SignalPath     string
	SignalMaxBytes int64
	InstanceID     string
	Interval       time.Duration
	Logger         *log.Logger
}

// Dispatcher polls pending signal_outbox rows, appends them to the audit signal.jsonl
// (rotating when exceeding SignalMaxBytes), notifies Launcher, and marks rows dispatched.
type Dispatcher struct {
	store      *store.Store
	notifyCh   chan<- struct{}
	signalPath string
	maxBytes   int64
	instanceID string
	interval   time.Duration
	logger     *log.Logger
	fileMu     sync.Mutex
}

// NewDispatcher creates a new Dispatcher.
func NewDispatcher(opts DispatcherOptions) *Dispatcher {
	interval := opts.Interval
	if interval <= 0 {
		interval = config.DefaultDispatchInterval
	}
	maxBytes := opts.SignalMaxBytes
	if maxBytes <= 0 {
		maxBytes = config.DefaultSignalMaxBytes
	}
	logger := opts.Logger
	if logger == nil {
		logger = log.Default()
	}
	return &Dispatcher{
		store:      opts.Store,
		notifyCh:   opts.NotifyCh,
		signalPath: opts.SignalPath,
		maxBytes:   maxBytes,
		instanceID: opts.InstanceID,
		interval:   interval,
		logger:     logger,
	}
}

// Run polls signal_outbox on the configured interval until ctx is cancelled.
func (d *Dispatcher) Run(ctx context.Context) error {
	if _, err := d.DispatchOnce(ctx); err != nil && ctx.Err() == nil {
		d.logger.Printf("dispatcher error: %v", err)
	}

	ticker := time.NewTicker(d.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if _, err := d.DispatchOnce(ctx); err != nil && ctx.Err() == nil {
				d.logger.Printf("dispatcher error: %v", err)
			}
		}
	}
}

// DispatchOnce drains currently pending outbox rows, appends each to signal.jsonl,
// marks it dispatched, and notifies the Launcher if any events were processed.
func (d *Dispatcher) DispatchOnce(ctx context.Context) (int, error) {
	total := 0
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		events, err := d.store.ListPendingOutbox(ctx, outboxBatchSize)
		if err != nil {
			return total, fmt.Errorf("list pending outbox: %w", err)
		}
		if len(events) == 0 {
			break
		}

		for _, ev := range events {
			if err := ctx.Err(); err != nil {
				return total, err
			}
			if d.signalPath != "" {
				if err := d.appendSignal(ev); err != nil {
					return total, fmt.Errorf("append signal %s: %w", ev.EventID, err)
				}
			}
			if err := d.store.MarkOutboxDispatched(ctx, ev.ID); err != nil {
				return total, fmt.Errorf("mark outbox %d dispatched: %w", ev.ID, err)
			}
			total++
		}

		d.notifyLauncher()

		if len(events) < outboxBatchSize {
			break
		}
	}
	return total, nil
}

func (d *Dispatcher) appendSignal(ev store.OutboxEvent) error {
	env := ev.Env
	if env == "" {
		env = d.instanceID
	}
	rec := SignalRecord{
		V:       1,
		EventID: ev.EventID,
		JobID:   ev.JobID,
		Env:     env,
		Src:     ev.Src,
		Hop:     ev.Hop,
		Ts:      ev.CreatedAt.UTC().Format(time.RFC3339Nano),
		Event:   ev.Event,
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	line = append(line, '\n')

	d.fileMu.Lock()
	defer d.fileMu.Unlock()

	dir := filepath.Dir(d.signalPath)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create signal dir %q: %w", dir, err)
		}
	}

	if d.maxBytes > 0 {
		if fi, err := os.Stat(d.signalPath); err == nil {
			if fi.Size() > 0 && fi.Size()+int64(len(line)) > d.maxBytes {
				rotated := d.signalPath + ".1"
				if err := os.Rename(d.signalPath, rotated); err != nil {
					return fmt.Errorf("rotate signal log %q -> %q: %w", d.signalPath, rotated, err)
				}
			}
		}
	}

	f, err := os.OpenFile(d.signalPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open signal log %q: %w", d.signalPath, err)
	}
	_, werr := f.Write(line)
	cerr := f.Close()
	if werr != nil {
		return fmt.Errorf("write signal log %q: %w", d.signalPath, werr)
	}
	return cerr
}

func (d *Dispatcher) notifyLauncher() {
	if d.notifyCh == nil {
		return
	}
	select {
	case d.notifyCh <- struct{}{}:
	default:
	}
}

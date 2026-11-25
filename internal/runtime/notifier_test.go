package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

func sampleDeliveredJob() *store.Job {
	now := time.Now().UTC().Truncate(time.Second)
	parentID := int64(42)
	return &store.Job{
		ID:         7,
		RequestID:  "req-webhook-7",
		Title:      "Deliver Webhook Test",
		Status:     store.StatusDone,
		Agent:      "claude",
		Host:       "ubuntu",
		Workdir:    "/work/repo",
		TargetRef:  "impl/phase1-core",
		ParentID:   &parentID,
		Attempts:   1,
		ResultText: "overall: PASS",
		CreatedAt:  now.Add(-2 * time.Minute),
		UpdatedAt:  now,
	}
}

func TestWebhookNotifierDeliverAndReportSuccess(t *testing.T) {
	var (
		gotMethod      string
		gotContentType string
		gotBody        []byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotContentType = r.Header.Get("Content-Type")
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(r.Body)
		gotBody = buf.Bytes()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	var logBuf bytes.Buffer
	notifier := NewWebhookNotifier(WebhookNotifierOptions{
		URL:            srv.URL,
		Timeout:        2 * time.Second,
		InitialBackoff: 5 * time.Millisecond,
		HTTPClient:     srv.Client(),
		Logger:         log.New(&logBuf, "", 0),
	})

	job := sampleDeliveredJob()
	if err := notifier.Deliver(context.Background(), job); err != nil {
		t.Fatalf("Deliver() error = %v, want nil", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("HTTP method = %q, want POST", gotMethod)
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotContentType)
	}

	var payload DeliveryPayload
	if err := json.Unmarshal(gotBody, &payload); err != nil {
		t.Fatalf("unmarshal DeliveryPayload: %v (body=%s)", err, string(gotBody))
	}
	if payload.JobID != job.ID || payload.RequestID != job.RequestID || payload.Title != job.Title {
		t.Errorf("payload header mismatch: got %+v", payload)
	}
	if payload.Status != string(store.StatusDone) || payload.ResultText != job.ResultText {
		t.Errorf("payload status/result mismatch: got %+v", payload)
	}
	if payload.ParentID == nil || *payload.ParentID != *job.ParentID {
		t.Errorf("payload parent_id = %v, want %d", payload.ParentID, *job.ParentID)
	}
	if payload.DeliveredAt.IsZero() {
		t.Errorf("payload delivered_at is zero")
	}
	if logBuf.Len() != 0 {
		t.Errorf("unexpected log output on success: %q", logBuf.String())
	}

	// Verify Report POSTs valid Report JSON.
	rep := Report{
		DedupKey: "orphan-waiting:7:parent-42-failed",
		Kind:     "orphan_waiting",
		JobID:    7,
		Message:  "parent failed",
	}
	if err := notifier.Report(context.Background(), rep); err != nil {
		t.Fatalf("Report() error = %v, want nil", err)
	}
	if gotContentType != "application/json" {
		t.Errorf("Report Content-Type = %q, want application/json", gotContentType)
	}
	var gotRep Report
	if err := json.Unmarshal(gotBody, &gotRep); err != nil {
		t.Fatalf("unmarshal Report: %v", err)
	}
	if gotRep.DedupKey != rep.DedupKey || gotRep.Kind != rep.Kind || gotRep.JobID != rep.JobID || gotRep.Message != rep.Message {
		t.Errorf("decoded report = %+v, want %+v", gotRep, rep)
	}
	if gotRep.Timestamp.IsZero() {
		t.Errorf("decoded report timestamp is zero")
	}
}

func TestWebhookNotifierRetryUntilSuccess(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n < 3 {
			http.Error(w, "temporary failure", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	var logBuf bytes.Buffer
	notifier := NewWebhookNotifier(WebhookNotifierOptions{
		URL:            srv.URL,
		Timeout:        1 * time.Second,
		Retries:        2,
		RetriesSet:     true,
		InitialBackoff: 5 * time.Millisecond,
		HTTPClient:     srv.Client(),
		Logger:         log.New(&logBuf, "", 0),
	})

	if err := notifier.Deliver(context.Background(), sampleDeliveredJob()); err != nil {
		t.Fatalf("Deliver() error = %v, want nil", err)
	}
	if got := attempts.Load(); got != 3 {
		t.Errorf("attempts = %d, want 3", got)
	}
	if logBuf.Len() != 0 {
		t.Errorf("unexpected error log after eventual success: %q", logBuf.String())
	}
}

func TestWebhookNotifierAlwaysFailingStopsAtRetriesAndLogs(t *testing.T) {
	tests := []struct {
		name         string
		retries      int
		retriesSet   bool
		wantAttempts int32
	}{
		{name: "default retries (2)", retries: 0, retriesSet: false, wantAttempts: 3},
		{name: "explicit 0 retries", retries: 0, retriesSet: true, wantAttempts: 1},
		{name: "explicit 3 retries", retries: 3, retriesSet: true, wantAttempts: 4},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var attempts atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts.Add(1)
				http.Error(w, "upstream down", http.StatusInternalServerError)
			}))
			defer srv.Close()

			var logBuf bytes.Buffer
			notifier := NewWebhookNotifier(WebhookNotifierOptions{
				URL:            srv.URL,
				Timeout:        1 * time.Second,
				Retries:        tc.retries,
				RetriesSet:     tc.retriesSet,
				InitialBackoff: 5 * time.Millisecond,
				HTTPClient:     srv.Client(),
				Logger:         log.New(&logBuf, "", 0),
			})

			if err := notifier.Deliver(context.Background(), sampleDeliveredJob()); err != nil {
				t.Fatalf("Deliver() error = %v, want nil", err)
			}
			if got := attempts.Load(); got != tc.wantAttempts {
				t.Errorf("attempts = %d, want %d", got, tc.wantAttempts)
			}
			logged := logBuf.String()
			if !strings.Contains(logged, "webhook delivery failed") || !strings.Contains(logged, "500") {
				t.Errorf("expected failure log mentioning webhook delivery failed and 500, got %q", logged)
			}
		})
	}
}

func TestWebhookNotifierTimeoutAbortsAttemptAndReturnsNil(t *testing.T) {
	var attempts atomic.Int32
	unblock := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		select {
		case <-unblock:
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer func() {
		close(unblock)
		srv.Close()
	}()

	var logBuf bytes.Buffer
	notifier := NewWebhookNotifier(WebhookNotifierOptions{
		URL:            srv.URL,
		Timeout:        40 * time.Millisecond,
		Retries:        1,
		RetriesSet:     true,
		InitialBackoff: 5 * time.Millisecond,
		HTTPClient:     srv.Client(),
		Logger:         log.New(&logBuf, "", 0),
	})

	start := time.Now()
	if err := notifier.Deliver(context.Background(), sampleDeliveredJob()); err != nil {
		t.Fatalf("Deliver() error = %v, want nil", err)
	}
	elapsed := time.Since(start)

	if got := attempts.Load(); got != 2 {
		t.Errorf("attempts = %d, want 2 (1 initial + 1 retry)", got)
	}
	if elapsed > 1*time.Second {
		t.Errorf("Deliver() took %v, expected prompt abort on 40ms per-attempt timeout", elapsed)
	}
	if !strings.Contains(logBuf.String(), "webhook delivery failed") {
		t.Errorf("expected failure log on timeout, got %q", logBuf.String())
	}
}

func TestWebhookNotifierDisabledWhenURLEmpty(t *testing.T) {
	var logBuf bytes.Buffer
	notifier := NewWebhookNotifier(WebhookNotifierOptions{
		URL:    "",
		Logger: log.New(&logBuf, "", 0),
	})

	if err := notifier.Deliver(context.Background(), sampleDeliveredJob()); err != nil {
		t.Fatalf("Deliver() with empty URL error = %v, want nil", err)
	}
	if err := notifier.Report(context.Background(), Report{DedupKey: "k", Kind: "test", JobID: 1}); err != nil {
		t.Fatalf("Report() with empty URL error = %v, want nil", err)
	}
	if logBuf.Len() != 0 {
		t.Errorf("expected no log output when disabled, got %q", logBuf.String())
	}
}

func TestMultiNotifierFileFirstAndWebhookIsolation(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	deliveriesDir := filepath.Join(dir, "deliveries")
	dbPath := filepath.Join(dir, "fleet.db")

	st, err := store.Open(ctx, dbPath, store.Options{InstanceID: "multi-test"})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	created, _, err := st.CreateJob(ctx, store.CreateJobParams{
		RequestID: "multi-job-1",
		Title:     "MultiNotifier Job",
	})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	claimed, ok, err := st.Claim(ctx, 2)
	if err != nil || !ok {
		t.Fatalf("Claim: ok=%v err=%v", ok, err)
	}
	compRes, err := st.Complete(ctx, claimed.ID, claimed.ClaimedBy, "all good")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	// Case 1: Webhook always fails, yet FileNotifier succeeds -> job-<id>.json created, MarkDelivered called, Deliver returns nil.
	var webhookAttempts atomic.Int32
	failingSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		webhookAttempts.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer failingSrv.Close()

	var logBuf bytes.Buffer
	fileNotifier := NewFileNotifier(deliveriesDir, st)
	webhookNotifier := NewWebhookNotifier(WebhookNotifierOptions{
		URL:            failingSrv.URL,
		Timeout:        500 * time.Millisecond,
		Retries:        1,
		RetriesSet:     true,
		InitialBackoff: 5 * time.Millisecond,
		HTTPClient:     failingSrv.Client(),
		Logger:         log.New(&logBuf, "", 0),
	})
	multi := NewMultiNotifier(fileNotifier, webhookNotifier)

	if err := multi.Deliver(ctx, compRes.Job); err != nil {
		t.Fatalf("MultiNotifier.Deliver() error = %v, want nil", err)
	}
	if got := webhookAttempts.Load(); got != 2 {
		t.Errorf("webhookAttempts = %d, want 2", got)
	}
	if !strings.Contains(logBuf.String(), "webhook delivery failed") {
		t.Errorf("expected webhook failure log, got %q", logBuf.String())
	}

	deliveryFile := filepath.Join(deliveriesDir, fmt.Sprintf("job-%d.json", created.ID))
	raw, err := os.ReadFile(deliveryFile)
	if err != nil {
		t.Fatalf("expected delivery file %s: %v", deliveryFile, err)
	}
	var filePayload DeliveryPayload
	if err := json.Unmarshal(raw, &filePayload); err != nil {
		t.Fatalf("unmarshal delivery file: %v", err)
	}
	if filePayload.JobID != created.ID || filePayload.Title != "MultiNotifier Job" {
		t.Errorf("unexpected file payload: %+v", filePayload)
	}

	gotJob, err := st.GetJob(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if gotJob.DeliveredAt == nil {
		t.Errorf("job.DeliveredAt is nil, want non-nil (MarkDelivered must have run)")
	}

	// Report through MultiNotifier: reports.jsonl written even when webhook fails.
	if err := multi.Report(ctx, Report{
		DedupKey: "multi-rep-1",
		Kind:     "test_report",
		JobID:    created.ID,
		Message:  "check reports.jsonl",
	}); err != nil {
		t.Fatalf("MultiNotifier.Report() error = %v, want nil", err)
	}
	repData, err := os.ReadFile(filepath.Join(deliveriesDir, "reports.jsonl"))
	if err != nil || !strings.Contains(string(repData), "multi-rep-1") {
		t.Errorf("expected reports.jsonl to contain multi-rep-1; err=%v data=%q", err, string(repData))
	}

	// Case 2: Disabled WebhookNotifier (empty URL) inside MultiNotifier still writes file and marks delivered.
	created2, _, err := st.CreateJob(ctx, store.CreateJobParams{
		RequestID: "multi-job-2",
		Title:     "MultiNotifier Disabled Webhook Job",
	})
	if err != nil {
		t.Fatalf("CreateJob 2: %v", err)
	}
	claimed2, ok, err := st.Claim(ctx, 2)
	if err != nil || !ok {
		t.Fatalf("Claim 2: ok=%v err=%v", ok, err)
	}
	compRes2, err := st.Complete(ctx, claimed2.ID, claimed2.ClaimedBy, "ok")
	if err != nil {
		t.Fatalf("Complete 2: %v", err)
	}
	disabledMulti := NewMultiNotifier(fileNotifier, NewWebhookNotifier(WebhookNotifierOptions{URL: ""}))
	if err := disabledMulti.Deliver(ctx, compRes2.Job); err != nil {
		t.Fatalf("disabled MultiNotifier.Deliver() error = %v, want nil", err)
	}
	if _, err := os.Stat(filepath.Join(deliveriesDir, fmt.Sprintf("job-%d.json", created2.ID))); err != nil {
		t.Errorf("expected delivery file for job #%d: %v", created2.ID, err)
	}
	gotJob2, err := st.GetJob(ctx, created2.ID)
	if err != nil || gotJob2.DeliveredAt == nil {
		t.Errorf("job #%d DeliveredAt = %v (err=%v), want non-nil", created2.ID, gotJob2.DeliveredAt, err)
	}

	// Case 3: FileNotifier error propagates and prevents WebhookNotifier from running.
	blockingFile := filepath.Join(dir, "not-a-directory")
	if err := os.WriteFile(blockingFile, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile blocker: %v", err)
	}
	var shouldNotBeCalled atomic.Int32
	okSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		shouldNotBeCalled.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer okSrv.Close()

	badMulti := NewMultiNotifier(
		NewFileNotifier(filepath.Join(blockingFile, "sub"), st),
		NewWebhookNotifier(WebhookNotifierOptions{URL: okSrv.URL, HTTPClient: okSrv.Client()}),
	)
	if err := badMulti.Deliver(ctx, compRes.Job); err == nil {
		t.Fatalf("expected error when FileNotifier fails, got nil")
	}
	if got := shouldNotBeCalled.Load(); got != 0 {
		t.Errorf("webhook was called %d times after FileNotifier failed, want 0", got)
	}
}

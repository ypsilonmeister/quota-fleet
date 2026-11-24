package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

func eventsTestOptions() Options {
	return Options{
		InstanceID: "inst-sse",
		MaxClaimed: 5,
		JobEvents:  true,
		Tokens: []config.APITokenConfig{
			{Name: "watch", Token: "tok-read", Scopes: []string{config.ScopeRead}},
			{Name: "registrar", Token: "tok-register", Scopes: []string{config.ScopeRegister}},
			{Name: "worker", Token: "tok-worker", Scopes: []string{config.ScopeWorker}},
		},
	}
}

// openEvents connects to GET /events and returns a scanner positioned after the initial
// comment frame, so the subscription is known to be active.
func openEvents(t *testing.T, baseURL, token string) (*http.Response, *bufio.Scanner) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	// Registered after the test server's cleanup, so it runs first and unblocks Close.
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/events", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /events: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /events status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	sc := bufio.NewScanner(resp.Body)
	if got := readFrame(t, sc); got != ": connected" {
		t.Fatalf("first frame = %q, want %q", got, ": connected")
	}
	return resp, sc
}

// readFrame returns the lines of the next blank-line terminated SSE frame.
func readFrame(t *testing.T, sc *bufio.Scanner) string {
	t.Helper()
	var lines []string
	for sc.Scan() {
		if sc.Text() == "" {
			return strings.Join(lines, "\n")
		}
		lines = append(lines, sc.Text())
	}
	t.Fatalf("event stream ended mid-frame: lines=%q err=%v", lines, sc.Err())
	return ""
}

func readJobEvent(t *testing.T, sc *bufio.Scanner) map[string]any {
	t.Helper()
	frame := readFrame(t, sc)
	data, ok := strings.CutPrefix(frame, "event: job_status\ndata: ")
	if !ok {
		t.Fatalf("unexpected frame %q", frame)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(data), &out); err != nil {
		t.Fatalf("decode event %q: %v", data, err)
	}
	return out
}

func TestJobEventsStreamDeliversTransitions(t *testing.T) {
	_, st, ts := newTestServer(t, eventsTestOptions())
	ctx := context.Background()
	job, _, err := st.CreateJob(ctx, store.CreateJobParams{
		RequestID: "sse-1", Title: "SSE job", Agent: "claude", Host: "ubuntu",
	})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}

	// The job created above is in the past: it must not be replayed.
	_, sc := openEvents(t, ts.URL, "tok-read")

	resp := doJSON(t, http.MethodPost, fmt.Sprintf("%s/jobs/%d/claim", ts.URL, job.ID), "tok-worker", nil)
	claim := decodeResp[ClaimResponse](t, resp)
	if !claim.Claimed {
		t.Fatalf("claim = %+v", claim)
	}
	ev := readJobEvent(t, sc)
	if ev["status"] != "claimed" || ev["prev_status"] != "queued" {
		t.Fatalf("claim event = %v", ev)
	}

	resp = doJSON(t, http.MethodPost, fmt.Sprintf("%s/jobs/%d/complete", ts.URL, job.ID), "tok-worker",
		CompleteRequest{Token: claim.Token, ResultText: "done"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("complete status = %d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	ev = readJobEvent(t, sc)
	want := map[string]any{
		"job_id": float64(job.ID), "title": "SSE job", "status": "done", "prev_status": "claimed",
		"agent": "claude", "host": "ubuntu",
	}
	for k, v := range want {
		if ev[k] != v {
			t.Errorf("event[%q] = %v, want %v", k, ev[k], v)
		}
	}
	if ts, _ := ev["updated_at"].(string); ts == "" {
		t.Errorf("event updated_at missing: %v", ev)
	} else if _, err := time.Parse(time.RFC3339Nano, ts); err != nil {
		t.Errorf("event updated_at %q: %v", ts, err)
	}
	if len(ev) != len(want)+1 {
		t.Errorf("event has unexpected fields: %v", ev)
	}
}

func TestJobEventsAuth(t *testing.T) {
	_, _, ts := newTestServer(t, eventsTestOptions())

	for _, tc := range []struct {
		name  string
		path  string
		token string
		want  int
	}{
		{name: "no token", path: "/events", want: http.StatusUnauthorized},
		{name: "wrong token", path: "/events", token: "nope", want: http.StatusUnauthorized},
		{name: "no read scope", path: "/events", token: "tok-register", want: http.StatusForbidden},
		{name: "token query parameter", path: "/events?token=tok-read", want: http.StatusUnauthorized},
		{name: "access_token query parameter", path: "/events?access_token=tok-read", want: http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := doJSON(t, http.MethodGet, ts.URL+tc.path, tc.token, nil)
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
}

func TestJobEventsDisabledByDefault404(t *testing.T) {
	opts := eventsTestOptions()
	opts.JobEvents = false
	_, _, ts := newTestServer(t, opts)

	resp := doJSON(t, http.MethodGet, ts.URL+"/events", "tok-read", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /events with job_events off = %d, want 404", resp.StatusCode)
	}
}

func TestJobEventsHeartbeat(t *testing.T) {
	opts := eventsTestOptions()
	srv, _, _ := newTestServer(t, opts)
	if srv.eventsHeartbeat != 25*time.Second {
		t.Fatalf("default heartbeat = %v, want 25s", srv.eventsHeartbeat)
	}

	opts.EventsHeartbeat = 10 * time.Millisecond
	_, _, ts := newTestServer(t, opts)
	_, sc := openEvents(t, ts.URL, "tok-read")
	for range 3 {
		if got := readFrame(t, sc); got != ": heartbeat" {
			t.Fatalf("frame = %q, want heartbeat", got)
		}
	}
}

func TestJobEventsStreamEndsWhenEventsClose(t *testing.T) {
	_, st, ts := newTestServer(t, eventsTestOptions())
	_, sc := openEvents(t, ts.URL, "tok-read")

	st.CloseJobEvents()
	if sc.Scan() {
		t.Fatalf("unexpected line after close: %q", sc.Text())
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("stream did not end cleanly: %v", err)
	}
}

// A client that never reads its stream must not delay writers.
func TestJobEventsStalledClientDoesNotBlockWriters(t *testing.T) {
	_, st, ts := newTestServer(t, eventsTestOptions())
	openEvents(t, ts.URL, "tok-read")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// Large titles fill the socket buffers, so the handler stalls and the hub overflows.
	title := strings.Repeat("x", 64<<10)
	for i := range 600 {
		if _, _, err := st.CreateJob(ctx, store.CreateJobParams{
			RequestID: fmt.Sprintf("stall-%d", i), Title: title,
		}); err != nil {
			t.Fatalf("CreateJob %d blocked or failed: %v", i, err)
		}
	}
}

package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// With api.job_events on, serve streams transitions made by the resident runtime, and
// open streams do not hold up shutdown.
func TestServeJobEventsStreamAndShutdown(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "fleet.yaml")

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	cfgYAML := fmt.Sprintf(`
instance_id: "serve-events-inst"
state_dir: "%s"
api:
  listen: "%s"
  job_events: true
runtime:
  adapter: "mock"
  dispatch_interval: "15ms"
  shepherd_interval: "15ms"
  reconcile_interval: "30ms"
  stale_claimed_after: "5s"
evaluator:
  mode: "always-pass"
`, filepath.ToSlash(filepath.Join(dir, "state")), addr)
	if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out, errBuf bytes.Buffer
	doneCh := make(chan int, 1)
	go func() {
		doneCh <- runServeContext(ctx, []string{"--config", cfgPath}, &out, &errBuf)
	}()

	reqCtx, reqCancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer reqCancel()
	var resp *http.Response
	deadline := time.Now().Add(5 * time.Second)
	for {
		req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, "http://"+addr+"/events", nil)
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		if resp, err = http.DefaultClient.Do(req); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("serve did not become ready on %s: %v", addr, err)
		}
		time.Sleep(15 * time.Millisecond)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /events status = %d, want 200", resp.StatusCode)
	}
	sc := bufio.NewScanner(resp.Body)
	if !sc.Scan() || sc.Text() != ": connected" {
		t.Fatalf("first line = %q, err = %v", sc.Text(), sc.Err())
	}

	created, err := http.Post("http://"+addr+"/jobs", "application/json",
		strings.NewReader(`{"request_id":"serve-events-1","title":"Events E2E","acceptance_criteria":"build ok"}`))
	if err != nil {
		t.Fatalf("POST /jobs: %v", err)
	}
	_ = created.Body.Close()

	// queued -> claimed -> done is driven by the resident runtime.
	sawDone := false
	for !sawDone && sc.Scan() {
		line := sc.Text()
		sawDone = strings.HasPrefix(line, "data: ") &&
			strings.Contains(line, `"status":"done"`) && strings.Contains(line, `"prev_status":"claimed"`)
	}
	if !sawDone {
		t.Fatalf("no done event received: %v", sc.Err())
	}

	// A spare connection that never sent a request also delays Shutdown for 5s, which
	// would mask what is measured here.
	http.DefaultClient.CloseIdleConnections()
	start := time.Now()
	cancel()
	select {
	case code := <-doneCh:
		if code != 0 {
			t.Fatalf("serve exit = %d, stderr = %s", code, errBuf.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not shut down")
	}
	// Without closing the streams, Shutdown would wait out its 5s timeout.
	if took := time.Since(start); took >= 4*time.Second {
		t.Fatalf("shutdown took %v with an open event stream", took)
	}
	for sc.Scan() {
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("stream did not end cleanly: %v", err)
	}
}

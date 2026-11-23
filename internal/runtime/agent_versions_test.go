package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/adapter"
	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

type stubHostFA struct {
	out map[string]adapter.ExecResult
}

func (s *stubHostFA) Stat(ctx context.Context, p string) (adapter.FileStat, error) {
	return adapter.FileStat{}, nil
}
func (s *stubHostFA) ReadFile(ctx context.Context, p string) ([]byte, error) {
	return nil, os.ErrNotExist
}
func (s *stubHostFA) RunCommand(ctx context.Context, cmd string) (adapter.ExecResult, error) {
	for k, v := range s.out {
		if strings.Contains(cmd, k) {
			return v, nil
		}
	}
	return adapter.ExecResult{ExitCode: 127, Output: "command not found"}, nil
}

type stubProvider struct {
	hosts map[string]*stubHostFA
}

func (p *stubProvider) FileAccessForHost(host string) (adapter.FileAccess, error) {
	return p.hosts[host], nil
}

func TestAgentVersionCheckerForwardsEventsToNotifier(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, ":memory:", store.Options{InstanceID: "test"})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	var (
		mu       sync.Mutex
		received []Report
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var rep Report
		if err := json.NewDecoder(r.Body).Decode(&rep); err == nil {
			mu.Lock()
			received = append(received, rep)
			mu.Unlock()
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	dir := t.TempDir()
	notifier := NewMultiNotifier(
		NewFileNotifier(dir, st),
		NewWebhookNotifier(WebhookNotifierOptions{URL: srv.URL, Retries: 0, RetriesSet: true}),
	)

	cfg := &config.Config{
		InstanceID: "test",
		Hosts: map[string]config.HostConfig{
			"h1": {},
			"h2": {},
		},
		Agents: map[string]config.AgentConfig{
			"claude": {},
		},
	}

	provider := &stubProvider{
		hosts: map[string]*stubHostFA{
			"h1": {out: map[string]adapter.ExecResult{"claude": {ExitCode: 0, Output: "2.1.76\n"}}},
			"h2": {out: map[string]adapter.ExecResult{"claude": {ExitCode: 0, Output: "2.1.70\n"}}},
		},
	}

	checker := NewAgentVersionChecker(AgentVersionCheckerOptions{
		Store:    st,
		Config:   cfg,
		Provider: provider,
		Notifier: notifier,
		Interval: 0, // disabled periodic, still supports CheckOnce
	})
	if checker.Enabled() {
		t.Fatalf("expected Enabled()=false when Interval=0")
	}

	res, err := checker.CheckOnce(ctx)
	if err != nil {
		t.Fatalf("CheckOnce: %v", err)
	}
	if !res.Matrix.HasDrift {
		t.Fatalf("expected drift between h1 and h2")
	}

	mu.Lock()
	gotWebhook := len(received)
	mu.Unlock()
	if gotWebhook == 0 {
		t.Fatalf("expected webhook notifier to receive drift_started report")
	}

	data, err := os.ReadFile(filepath.Join(dir, "reports.jsonl"))
	if err != nil {
		t.Fatalf("read reports.jsonl: %v", err)
	}
	if !strings.Contains(string(data), "agent_drift_started") {
		t.Errorf("reports.jsonl missing agent_drift_started: %s", string(data))
	}

	// Verify Enabled()=true when Interval > 0.
	periodic := NewAgentVersionChecker(AgentVersionCheckerOptions{
		Store:    st,
		Config:   cfg,
		Provider: provider,
		Interval: time.Hour,
	})
	if !periodic.Enabled() {
		t.Fatalf("expected Enabled()=true when Interval > 0")
	}
}

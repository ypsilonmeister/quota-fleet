package mcpserver

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/ypsilonmeister/quota-fleet/internal/api"
	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

type bearerRT struct {
	token string
	base  http.RoundTripper
}

func (b bearerRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if b.token != "" {
		r.Header.Set("Authorization", "Bearer "+b.token)
	}
	return b.base.RoundTrip(r)
}

func newMCPHTTPFixture(t *testing.T, withMCP bool) (*httptest.Server, *store.Store) {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "fleet.db"), store.Options{InstanceID: "inst-mcp-http"})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	opts := api.Options{
		InstanceID: "inst-mcp-http",
		MaxClaimed: 5,
		Logger:     log.New(io.Discard, "", 0),
		Tokens: []config.APITokenConfig{
			{Name: "watch", Token: "tok-watch", Scopes: []string{"read"}},
			{Name: "registrar", Token: "tok-registrar", Scopes: []string{"read", "register"}},
			{Name: "other-registrar", Token: "tok-other", Scopes: []string{"read", "register"}},
		},
	}
	if withMCP {
		opts.MCPHandler = NewHTTPHandler
	}
	ts := httptest.NewServer(api.NewServer(st, opts).Handler())
	t.Cleanup(ts.Close)
	return ts, st
}

func connectHTTP(t *testing.T, ts *httptest.Server, token string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "http-test", Version: "0.0.1"}, nil)
	sess, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:   ts.URL + "/mcp",
		HTTPClient: &http.Client{Transport: bearerRT{token: token, base: http.DefaultTransport}},
	}, nil)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

func TestMCPHTTPInitializeAndListTools(t *testing.T) {
	ts, _ := newMCPHTTPFixture(t, true)
	sess := connectHTTP(t, ts, "tok-watch")
	res, err := sess.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	want := []string{"create_job", "list_jobs", "get_job", "cancel_job", "requeue_job", "mark_delivered",
		"list_held", "release_held", "cancel_held", "gate_decisions", "list_deliveries", "fleet_status"}
	if len(res.Tools) != len(want) {
		t.Fatalf("got %d tools, want %d", len(res.Tools), len(want))
	}
	got := map[string]bool{}
	for _, tl := range res.Tools {
		got[tl.Name] = true
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("missing tool %q", w)
		}
	}
}

func TestMCPHTTPUnauthenticated401(t *testing.T) {
	ts, _ := newMCPHTTPFixture(t, true)
	for _, auth := range []string{"", "Bearer wrong"} {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("auth=%q status=%d, want 401", auth, resp.StatusCode)
		}
	}
}

func TestMCPHTTPDisabledReturns404(t *testing.T) {
	ts, _ := newMCPHTTPFixture(t, false)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/mcp", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer tok-watch")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", resp.StatusCode)
	}
}

func TestMCPHTTPReadOnlyTokenCannotCreateJob(t *testing.T) {
	ts, st := newMCPHTTPFixture(t, true)
	sess := connectHTTP(t, ts, "tok-watch")
	res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "create_job",
		Arguments: map[string]any{"title": "should fail", "request_id": "req-watch"},
	})
	if err == nil && (res == nil || !res.IsError) {
		t.Fatalf("create_job with read-only token succeeded: %+v", res)
	}
	if res != nil && len(res.Content) > 0 {
		if tc, ok := res.Content[0].(*mcp.TextContent); ok && !strings.Contains(tc.Text, "scope") && !strings.Contains(tc.Text, "forbidden") {
			t.Errorf("error does not mention scope/forbidden: %q", tc.Text)
		}
	}
	if jobs := queuedJobs(t, st); len(jobs) != 0 {
		t.Fatalf("job was created despite scope failure: %d", len(jobs))
	}
}

func TestMCPHTTPCreatedByIsPerRequestTokenName(t *testing.T) {
	ts, st := newMCPHTTPFixture(t, true)
	for _, c := range []struct{ token, name string }{{"tok-registrar", "registrar"}, {"tok-other", "other-registrar"}} {
		sess := connectHTTP(t, ts, c.token)
		res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      "create_job",
			Arguments: map[string]any{"title": "job by " + c.name, "request_id": "req-" + c.name},
		})
		if err != nil || res.IsError {
			t.Fatalf("create_job(%s): err=%v res=%+v", c.name, err, res)
		}
	}
	jobs := queuedJobs(t, st)
	if len(jobs) != 2 {
		t.Fatalf("got %d jobs, want 2", len(jobs))
	}
	for _, j := range jobs {
		want := strings.TrimPrefix(j.Title, "job by ")
		if j.CreatedBy != want {
			t.Errorf("job %q created_by=%q, want %q", j.Title, j.CreatedBy, want)
		}
	}
}

func queuedJobs(t *testing.T, st *store.Store) []*store.Job {
	t.Helper()
	page, err := st.ListJobs(context.Background(), store.ListJobsFilter{Status: store.StatusQueued, Limit: 50})
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	return page.Jobs
}

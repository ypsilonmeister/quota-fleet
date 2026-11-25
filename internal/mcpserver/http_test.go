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
		opts.MCPHandler = func(backend http.Handler) http.Handler {
			return NewHTTPHandler(backend, []string{"fleet.example.ts.net"})
		}
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
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !res.IsError || len(res.Content) != 1 {
		t.Fatalf("want a tool error with one content item, got %+v", res)
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content type = %T, want *mcp.TextContent", res.Content[0])
	}
	for _, want := range []string{"HTTP 403", `missing required scope "register"`} {
		if !strings.Contains(tc.Text, want) {
			t.Errorf("tool error %q does not contain %q", tc.Text, want)
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

// With no tokens the JSON API may run anonymously, but /mcp must not (docs/13 §2).
func TestMCPHTTPNoTokensConfigured401(t *testing.T) {
	for _, requireAuth := range []bool{false, true} {
		st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "fleet.db"), store.Options{InstanceID: "inst-mcp-http"})
		if err != nil {
			t.Fatalf("store.Open: %v", err)
		}
		t.Cleanup(func() { _ = st.Close() })
		ts := httptest.NewServer(api.NewServer(st, api.Options{
			InstanceID:  "inst-mcp-http",
			RequireAuth: requireAuth,
			Logger:      log.New(io.Discard, "", 0),
			MCPHandler: func(backend http.Handler) http.Handler {
				return NewHTTPHandler(backend, nil)
			},
		}).Handler())
		t.Cleanup(ts.Close)

		const createJob = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"create_job","arguments":{"title":"anon","request_id":"req-anon"}}}`
		for _, path := range []string{"/mcp", "/mcp/"} {
			for _, auth := range []string{"", "Bearer anything"} {
				if got := postMCP(t, ts, path, createJob, http.Header{"Authorization": {auth}}); got != http.StatusUnauthorized {
					t.Errorf("require_auth=%v path=%s auth=%q status=%d, want 401", requireAuth, path, auth, got)
				}
			}
		}
		if jobs := queuedJobs(t, st); len(jobs) != 0 {
			t.Errorf("require_auth=%v: unauthenticated MCP call created %d jobs", requireAuth, len(jobs))
		}

		// The JSON API keeps its existing behaviour.
		resp, err := http.Get(ts.URL + "/status")
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		want := http.StatusOK
		if requireAuth {
			want = http.StatusUnauthorized
		}
		if resp.StatusCode != want {
			t.Errorf("require_auth=%v GET /status status=%d, want %d", requireAuth, resp.StatusCode, want)
		}
	}
}

func TestMCPHTTPOriginValidation(t *testing.T) {
	ts, _ := newMCPHTTPFixture(t, true)
	cases := []struct {
		origins []string
		want    int
	}{
		{nil, http.StatusOK},
		{[]string{"http://localhost"}, http.StatusOK},
		{[]string{"http://localhost:3000"}, http.StatusOK},
		{[]string{"http://127.0.0.1:8737"}, http.StatusOK},
		{[]string{"http://[::1]:8737"}, http.StatusOK},
		{[]string{"https://fleet.example.ts.net"}, http.StatusOK},
		{[]string{"http://Fleet.Example.TS.net:8737"}, http.StatusOK},
		{[]string{"http://evil.example"}, http.StatusForbidden},
		{[]string{"https://other.example.ts.net"}, http.StatusForbidden},
		{[]string{"https://fleet.example.ts.net.evil.example"}, http.StatusForbidden},
		{[]string{"http://localhost.evil.example"}, http.StatusForbidden},
		{[]string{"https://fleet.example.ts.net@evil.example"}, http.StatusForbidden},
		{[]string{"https://evil.example@localhost"}, http.StatusForbidden},
		{[]string{"http://localhost/path"}, http.StatusForbidden},
		{[]string{"file://localhost"}, http.StatusForbidden},
		{[]string{"null"}, http.StatusForbidden},
		{[]string{""}, http.StatusForbidden},
		{[]string{"http://localhost", "http://evil.example"}, http.StatusForbidden},
	}
	for _, c := range cases {
		h := http.Header{"Authorization": {"Bearer tok-watch"}}
		if c.origins != nil {
			h["Origin"] = c.origins
		}
		const listTools = `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`
		if got := postMCP(t, ts, "/mcp", listTools, h); got != c.want {
			t.Errorf("origins=%q status=%d, want %d", c.origins, got, c.want)
		}
	}
}

// A DNS rebound page sends an Origin that matches Host; it must still be rejected.
func TestMCPHTTPOriginMatchingHostIsStillRejected(t *testing.T) {
	h := NewHTTPHandler(http.NotFoundHandler(), nil)
	req := httptest.NewRequest(http.MethodPost, "http://evil.example/mcp", strings.NewReader("{}"))
	req.Header.Set("Origin", "http://evil.example")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status=%d, want 403", rec.Code)
	}
}

func postMCP(t *testing.T, ts *httptest.Server, path, body string, header http.Header) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header = header.Clone()
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

func queuedJobs(t *testing.T, st *store.Store) []*store.Job {
	t.Helper()
	page, err := st.ListJobs(context.Background(), store.ListJobsFilter{Status: store.StatusQueued, Limit: 50})
	if err != nil {
		t.Fatalf("ListJobs: %v", err)
	}
	return page.Jobs
}

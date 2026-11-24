package mcpserver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type recordedRequest struct {
	Method string
	Path   string
	Query  string
	Auth   string
	Body   []byte
}

func connectTestClient(t *testing.T, opts Options) (*mcp.ClientSession, func()) {
	t.Helper()
	srv, err := NewServer(opts)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	ct, st := mcp.NewInMemoryTransports()

	serverDone := make(chan error, 1)
	go func() {
		serverDone <- srv.Run(ctx, st)
	}()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.0.1"}, nil)
	session, err := client.Connect(ctx, ct, nil)
	if err != nil {
		cancel()
		t.Fatalf("client.Connect: %v", err)
	}

	cleanup := func() {
		_ = session.Close()
		cancel()
		<-serverDone
	}
	return session, cleanup
}

func decodeToolResultJSON(t *testing.T, res *mcp.CallToolResult) map[string]any {
	t.Helper()
	if res == nil {
		t.Fatalf("CallToolResult is nil")
	}
	if len(res.Content) == 0 {
		t.Fatalf("CallToolResult.Content is empty")
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("expected *mcp.TextContent, got %T", res.Content[0])
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(tc.Text), &out); err != nil {
		t.Fatalf("json.Unmarshal tool result text %q: %v", tc.Text, err)
	}
	return out
}

func TestToolsListSmoke(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer ts.Close()

	session, cleanup := connectTestClient(t, Options{
		APIURL:     ts.URL,
		Token:      "smoke-token",
		HTTPClient: ts.Client(),
	})
	defer cleanup()

	listRes, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	wantTools := []string{
		"cancel_held",
		"cancel_job",
		"create_job",
		"fleet_status",
		"gate_decisions",
		"get_job",
		"list_deliveries",
		"list_held",
		"list_jobs",
		"mark_delivered",
		"release_held",
		"requeue_job",
	}

	gotTools := make([]string, 0, len(listRes.Tools))
	byName := make(map[string]*mcp.Tool, len(listRes.Tools))
	for _, tool := range listRes.Tools {
		gotTools = append(gotTools, tool.Name)
		byName[tool.Name] = tool
		if strings.TrimSpace(tool.Description) == "" {
			t.Errorf("tool %q has empty description", tool.Name)
		}
		if tool.InputSchema == nil {
			t.Errorf("tool %q has nil inputSchema", tool.Name)
		}
	}
	sort.Strings(gotTools)

	if !reflect.DeepEqual(gotTools, wantTools) {
		t.Fatalf("tools/list mismatch:\n got: %v\nwant: %v", gotTools, wantTools)
	}

	// Ensure §2-3 excluded runtime operations are never exposed.
	for _, forbidden := range []string{
		"claim", "claim_job",
		"complete", "complete_job",
		"fail", "fail_job",
		"cancel_claimed", "cancel_running",
		"reset", "reset_job",
		"reject_invalid",
	} {
		if _, exists := byName[forbidden]; exists {
			t.Errorf("forbidden tool %q must not be exposed by MCP server", forbidden)
		}
	}
}

func TestAllToolsHTTPMapping(t *testing.T) {
	const testToken = "secret-mcp-bearer-token"

	var (
		mu   sync.Mutex
		reqs []recordedRequest
	)
	record := func(r *http.Request) []byte {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		reqs = append(reqs, recordedRequest{
			Method: r.Method,
			Path:   r.URL.Path,
			Query:  r.URL.RawQuery,
			Auth:   r.Header.Get("Authorization"),
			Body:   body,
		})
		mu.Unlock()
		return body
	}
	lastReq := func() recordedRequest {
		mu.Lock()
		defer mu.Unlock()
		if len(reqs) == 0 {
			t.Fatalf("no HTTP requests recorded")
		}
		return reqs[len(reqs)-1]
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /jobs", func(w http.ResponseWriter, r *http.Request) {
		body := record(r)
		var payload map[string]any
		_ = json.Unmarshal(body, &payload)
		w.Header().Set("Content-Type", "application/json")
		if payload["title"] == "Held Job" {
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{
				"held_id": "held-001",
				"created": true,
				"held": {
					"id": "held-001",
					"request_id": "req-hold-1",
					"status": "held",
					"pool": "claude",
					"reason": "pool claude exhausted (5h=0%)",
					"snapshot_summary": "claude: blocked(5h)"
				}
			}`))
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"created": true,
			"job": map[string]any{
				"id":         101,
				"request_id": payload["request_id"],
				"title":      payload["title"],
				"status":     "queued",
			},
		})
	})

	mux.HandleFunc("GET /jobs", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jobs":[{"id":101,"title":"Build MCP","status":"queued"}],"next_cursor":101,"has_more":false}`))
	})

	mux.HandleFunc("GET /jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":101,"title":"Build MCP","status":"done","instructions":"implement mcp","result_text":"all green"}`))
	})

	mux.HandleFunc("POST /jobs/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		w.Header().Set("Content-Type", "application/json")
		if r.PathValue("id") == "999" {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":"status conflict: cannot cancel claimed job","current_status":"claimed"}`))
			return
		}
		_, _ = w.Write([]byte(`{"job":{"id":101,"status":"cancelled"},"cancelled_child_ids":[102]}`))
	})

	mux.HandleFunc("POST /jobs/{id}/requeue", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"job":{"id":101,"status":"queued"}}`))
	})

	mux.HandleFunc("POST /jobs/{id}/deliver", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"job":{"id":101,"status":"done","delivered_at":"2026-11-24T01:00:00Z"}}`))
	})

	mux.HandleFunc("GET /gate/held", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"held":[{"id":"held-001","status":"held","pool":"claude","reason":"exhausted"}]}`))
	})

	mux.HandleFunc("POST /gate/held/{id}/release", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		w.Header().Set("Content-Type", "application/json")
		switch r.PathValue("id") {
		case "held-001":
			_, _ = w.Write([]byte(`{"released":true,"job_id":205}`))
		case "held-blocked":
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"released":false,"reason":"still exhausted (weekly=0%)"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"held job not found"}`))
		}
	})

	mux.HandleFunc("DELETE /gate/held/{id}", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"held":{"id":"held-001","status":"cancelled"}}`))
	})

	mux.HandleFunc("GET /gate/decisions", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"decisions":[{"id":7,"job_id":101,"decision":"would_pass","selected_pool":"claude"}]}`))
	})

	mux.HandleFunc("GET /deliveries", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jobs":[{"id":101,"status":"done"}]}`))
	})

	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"instance_id":"fleet-test","counts":{"queued":2,"done":5},"total":7}`))
	})

	ts := httptest.NewServer(mux)
	defer ts.Close()

	session, cleanup := connectTestClient(t, Options{
		APIURL:     ts.URL + "/",
		Token:      testToken,
		HTTPClient: ts.Client(),
	})
	defer cleanup()

	ctx := context.Background()

	// 1a. create_job -> 201 Created (explicit fields + worktree_spec)
	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "create_job",
		Arguments: map[string]any{
			"title":               "Build MCP",
			"request_id":          "req-1",
			"agent":               "claude",
			"host":                "ubuntu",
			"workdir":             "/work/repo",
			"target_ref":          "impl/phase1-core",
			"instructions":        "implement mcp",
			"acceptance_criteria": "go test passes",
			"parent_id":           10,
			"model":               "claude-opus",
			"pool":                "claude",
			"alt_pools":           []string{"codex"},
			"worktree_spec": map[string]any{
				"repo_path": "/repos/qf",
				"base_ref":  "impl/phase1-core",
				"branch":    "feat/mcp-server",
			},
		},
	})
	if err != nil || res.IsError {
		t.Fatalf("create_job failed: err=%v res=%+v", err, res)
	}
	got := decodeToolResultJSON(t, res)
	if got["held"] != false || got["created"] != true {
		t.Errorf("create_job result = %+v, want held=false created=true", got)
	}
	lr := lastReq()
	if lr.Method != http.MethodPost || lr.Path != "/jobs" || lr.Auth != "Bearer "+testToken {
		t.Errorf("create_job HTTP req = %+v", lr)
	}
	var sentBody map[string]any
	if err := json.Unmarshal(lr.Body, &sentBody); err != nil {
		t.Fatalf("unmarshal create_job body: %v", err)
	}
	if sentBody["request_id"] != "req-1" || sentBody["title"] != "Build MCP" {
		t.Errorf("unexpected create_job body: %+v", sentBody)
	}

	// 1b. create_job without request_id auto-generates one
	res, err = session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "create_job",
		Arguments: map[string]any{"title": "Auto Request ID Job"},
	})
	if err != nil || res.IsError {
		t.Fatalf("create_job auto request_id failed: err=%v res=%+v", err, res)
	}
	lr = lastReq()
	sentBody = nil
	_ = json.Unmarshal(lr.Body, &sentBody)
	if rid, _ := sentBody["request_id"].(string); !strings.HasPrefix(rid, "mcp-") {
		t.Errorf("expected auto-generated mcp-* request_id, got %q", rid)
	}

	// 1c. create_job -> 202 Accepted (Gate hold) is a normal non-error result with held_id and reason
	res, err = session.CallTool(ctx, &mcp.CallToolParams{
		Name: "create_job",
		Arguments: map[string]any{
			"title":      "Held Job",
			"request_id": "req-hold-1",
			"pool":       "claude",
		},
	})
	if err != nil {
		t.Fatalf("create_job 202 protocol err: %v", err)
	}
	if res.IsError {
		t.Fatalf("create_job 202 must NOT be an error result: %+v", res)
	}
	got = decodeToolResultJSON(t, res)
	if got["held"] != true || got["held_id"] != "held-001" || got["reason"] != "pool claude exhausted (5h=0%)" {
		t.Errorf("create_job 202 result = %+v, want held=true held_id=held-001 and reason", got)
	}

	// 2. list_jobs
	res, err = session.CallTool(ctx, &mcp.CallToolParams{
		Name: "list_jobs",
		Arguments: map[string]any{
			"status":    "queued",
			"agent":     "claude",
			"host":      "ubuntu",
			"q":         "MCP",
			"parent_id": 10,
			"cursor":    5,
			"limit":     20,
			"desc":      true,
		},
	})
	if err != nil || res.IsError {
		t.Fatalf("list_jobs failed: err=%v res=%+v", err, res)
	}
	lr = lastReq()
	if lr.Method != http.MethodGet || lr.Path != "/jobs" || lr.Auth != "Bearer "+testToken {
		t.Errorf("list_jobs HTTP req = %+v", lr)
	}
	for _, wantParam := range []string{"status=queued", "agent=claude", "host=ubuntu", "q=MCP", "parent_id=10", "cursor=5", "limit=20", "desc=true"} {
		if !strings.Contains(lr.Query, wantParam) {
			t.Errorf("list_jobs query %q missing %q", lr.Query, wantParam)
		}
	}

	// 3. get_job
	res, err = session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "get_job",
		Arguments: map[string]any{"id": 101},
	})
	if err != nil || res.IsError {
		t.Fatalf("get_job failed: err=%v res=%+v", err, res)
	}
	lr = lastReq()
	if lr.Method != http.MethodGet || lr.Path != "/jobs/101" {
		t.Errorf("get_job HTTP req = %+v", lr)
	}
	got = decodeToolResultJSON(t, res)
	if got["instructions"] != "implement mcp" || got["result_text"] != "all green" {
		t.Errorf("get_job result = %+v", got)
	}

	// 4a. cancel_job (draft/queued/waiting)
	res, err = session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "cancel_job",
		Arguments: map[string]any{"id": 101, "reason": "no longer needed"},
	})
	if err != nil || res.IsError {
		t.Fatalf("cancel_job failed: err=%v res=%+v", err, res)
	}
	lr = lastReq()
	if lr.Method != http.MethodPost || lr.Path != "/jobs/101/cancel" {
		t.Errorf("cancel_job HTTP req = %+v", lr)
	}
	if !strings.Contains(string(lr.Body), `"reason":"no longer needed"`) {
		t.Errorf("cancel_job body = %q", string(lr.Body))
	}

	// 4b. cancel_job on claimed job -> 409 Conflict surfaced as tool error
	res, err = session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "cancel_job",
		Arguments: map[string]any{"id": 999},
	})
	if err != nil {
		t.Fatalf("cancel_job 409 unexpected protocol err: %v", err)
	}
	if !res.IsError {
		t.Errorf("cancel_job 409 expected IsError=true, got %+v", res)
	}

	// 5. requeue_job
	res, err = session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "requeue_job",
		Arguments: map[string]any{"id": 101},
	})
	if err != nil || res.IsError {
		t.Fatalf("requeue_job failed: err=%v res=%+v", err, res)
	}
	lr = lastReq()
	if lr.Method != http.MethodPost || lr.Path != "/jobs/101/requeue" {
		t.Errorf("requeue_job HTTP req = %+v", lr)
	}

	// 6. mark_delivered
	res, err = session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "mark_delivered",
		Arguments: map[string]any{"id": 101},
	})
	if err != nil || res.IsError {
		t.Fatalf("mark_delivered failed: err=%v res=%+v", err, res)
	}
	lr = lastReq()
	if lr.Method != http.MethodPost || lr.Path != "/jobs/101/deliver" {
		t.Errorf("mark_delivered HTTP req = %+v", lr)
	}

	// 7. list_held
	res, err = session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "list_held",
		Arguments: map[string]any{"status": "all", "limit": 15},
	})
	if err != nil || res.IsError {
		t.Fatalf("list_held failed: err=%v res=%+v", err, res)
	}
	lr = lastReq()
	if lr.Method != http.MethodGet || lr.Path != "/gate/held" || !strings.Contains(lr.Query, "status=all") || !strings.Contains(lr.Query, "limit=15") {
		t.Errorf("list_held HTTP req = %+v", lr)
	}

	// 8a. release_held -> 200 released
	res, err = session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "release_held",
		Arguments: map[string]any{"id": "held-001"},
	})
	if err != nil || res.IsError {
		t.Fatalf("release_held 200 failed: err=%v res=%+v", err, res)
	}
	lr = lastReq()
	if lr.Method != http.MethodPost || lr.Path != "/gate/held/held-001/release" {
		t.Errorf("release_held HTTP req = %+v", lr)
	}
	got = decodeToolResultJSON(t, res)
	if got["released"] != true || got["job_id"] != float64(205) {
		t.Errorf("release_held 200 result = %+v", got)
	}

	// 8b. release_held -> 409 still blocked returns structured {released: false, reason}
	res, err = session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "release_held",
		Arguments: map[string]any{"id": "held-blocked"},
	})
	if err != nil || res.IsError {
		t.Fatalf("release_held 409 should return normal structured result: err=%v res=%+v", err, res)
	}
	got = decodeToolResultJSON(t, res)
	if got["released"] != false || got["reason"] != "still exhausted (weekly=0%)" {
		t.Errorf("release_held 409 result = %+v", got)
	}

	// 8c. release_held -> 404 returns tool error
	res, err = session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "release_held",
		Arguments: map[string]any{"id": "held-missing"},
	})
	if err != nil {
		t.Fatalf("release_held 404 protocol err: %v", err)
	}
	if !res.IsError {
		t.Errorf("release_held 404 expected IsError=true")
	}

	// 9. cancel_held
	res, err = session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "cancel_held",
		Arguments: map[string]any{"id": "held-001"},
	})
	if err != nil || res.IsError {
		t.Fatalf("cancel_held failed: err=%v res=%+v", err, res)
	}
	lr = lastReq()
	if lr.Method != http.MethodDelete || lr.Path != "/gate/held/held-001" {
		t.Errorf("cancel_held HTTP req = %+v", lr)
	}

	// 10. gate_decisions
	res, err = session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "gate_decisions",
		Arguments: map[string]any{"job_id": 101, "held_id": "held-001", "limit": 5},
	})
	if err != nil || res.IsError {
		t.Fatalf("gate_decisions failed: err=%v res=%+v", err, res)
	}
	lr = lastReq()
	if lr.Method != http.MethodGet || lr.Path != "/gate/decisions" {
		t.Errorf("gate_decisions HTTP req = %+v", lr)
	}
	for _, wantParam := range []string{"job_id=101", "held_id=held-001", "limit=5"} {
		if !strings.Contains(lr.Query, wantParam) {
			t.Errorf("gate_decisions query %q missing %q", lr.Query, wantParam)
		}
	}

	// 11. list_deliveries
	res, err = session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "list_deliveries",
		Arguments: map[string]any{"status": "delivered", "limit": 10},
	})
	if err != nil || res.IsError {
		t.Fatalf("list_deliveries failed: err=%v res=%+v", err, res)
	}
	lr = lastReq()
	if lr.Method != http.MethodGet || lr.Path != "/deliveries" || !strings.Contains(lr.Query, "status=delivered") || !strings.Contains(lr.Query, "limit=10") {
		t.Errorf("list_deliveries HTTP req = %+v", lr)
	}

	// 12. fleet_status
	res, err = session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "fleet_status",
		Arguments: map[string]any{},
	})
	if err != nil || res.IsError {
		t.Fatalf("fleet_status failed: err=%v res=%+v", err, res)
	}
	lr = lastReq()
	if lr.Method != http.MethodGet || lr.Path != "/status" {
		t.Errorf("fleet_status HTTP req = %+v", lr)
	}
	got = decodeToolResultJSON(t, res)
	if got["instance_id"] != "fleet-test" || got["total"] != float64(7) {
		t.Errorf("fleet_status result = %+v", got)
	}
}

func TestAPIErrorHandlingAndNoTokenLeak(t *testing.T) {
	const secret = "super-secret-token-xyz"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized: invalid bearer token"}`))
	}))
	defer ts.Close()

	session, cleanup := connectTestClient(t, Options{
		APIURL:     ts.URL,
		Token:      secret,
		HTTPClient: ts.Client(),
	})
	defer cleanup()

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "fleet_status",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("unexpected protocol error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError=true on HTTP 401")
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", res.Content[0])
	}
	if !strings.Contains(tc.Text, "401") || !strings.Contains(tc.Text, "unauthorized") {
		t.Errorf("unexpected error text: %q", tc.Text)
	}
	if strings.Contains(tc.Text, secret) {
		t.Errorf("error text leaked bearer token")
	}
}

func TestNewServerValidation(t *testing.T) {
	for _, bad := range []string{"", "ftp://example.com", "not-a-url", "http://"} {
		if _, err := NewServer(Options{APIURL: bad}); err == nil {
			t.Errorf("NewServer(APIURL=%q) unexpectedly succeeded", bad)
		}
	}
}

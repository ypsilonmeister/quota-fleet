package mcpserver

import (
	"bytes"
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

	// Verify every recorded HTTP request across all 12 tools sent the expected Bearer token.
	mu.Lock()
	recorded := append([]recordedRequest(nil), reqs...)
	mu.Unlock()
	if len(recorded) < 12 {
		t.Fatalf("expected at least 12 recorded HTTP requests, got %d", len(recorded))
	}
	for i, req := range recorded {
		if req.Auth != "Bearer "+testToken {
			t.Errorf("request[%d] (%s %s) Authorization = %q, want %q", i, req.Method, req.Path, req.Auth, "Bearer "+testToken)
		}
	}
}

func TestAPIErrorHandlingAndNoTokenLeak(t *testing.T) {
	const secret = "super-secret-token-xyz"
	const otherSecret = "other-leaked-bearer-999"

	cases := []struct {
		name           string
		status         int
		contentType    string
		body           string
		wantSubstr     []string
		forbidSubstr   []string
	}{
		{
			name:         "401 json echoing exact token",
			status:       http.StatusUnauthorized,
			contentType:  "application/json",
			body:         `{"error":"unauthorized: invalid bearer token ` + secret + `"}`,
			wantSubstr:   []string{"401", "unauthorized"},
			forbidSubstr: []string{secret, "invalid bearer token"},
		},
		{
			name:         "403 json echoing authorization header",
			status:       http.StatusForbidden,
			contentType:  "application/json",
			body:         `{"error":"forbidden: Authorization: Bearer ` + secret + `"}`,
			wantSubstr:   []string{"403", "forbidden"},
			forbidSubstr: []string{secret, "Authorization:"},
		},
		{
			name:         "500 json echoing bearer token in error field",
			status:       http.StatusInternalServerError,
			contentType:  "application/json",
			body:         `{"error":"proxy rejected Authorization: Bearer ` + secret + ` and Bearer ` + otherSecret + `"}`,
			wantSubstr:   []string{"500"},
			forbidSubstr: []string{secret, otherSecret},
		},
		{
			name:         "409 json echoing token in reason and current_status",
			status:       http.StatusConflict,
			contentType:  "application/json",
			body:         `{"reason":"conflict for token ` + secret + `","current_status":"Bearer ` + secret + `"}`,
			wantSubstr:   []string{"409"},
			forbidSubstr: []string{secret},
		},
		{
			name:         "502 plain text proxy error echoing authorization header",
			status:       http.StatusBadGateway,
			contentType:  "text/html",
			body:         `<html><body>502 Bad Gateway: Authorization: Bearer ` + secret + `</body></html>`,
			wantSubstr:   []string{"502"},
			forbidSubstr: []string{secret, "<html>", "Authorization:"},
		},
		{
			name:         "400 normal api json error preserves safe message",
			status:       http.StatusBadRequest,
			contentType:  "application/json",
			body:         `{"error":"invalid status \"bogus\""}`,
			wantSubstr:   []string{"400", "invalid status"},
			forbidSubstr: []string{secret},
		},
		{
			name:         "409 normal api json error preserves safe current_status",
			status:       http.StatusConflict,
			contentType:  "application/json",
			body:         `{"error":"status conflict: job is not cancellable","current_status":"claimed"}`,
			wantSubstr:   []string{"409", "job is not cancellable", "current_status=claimed"},
			forbidSubstr: []string{secret},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.contentType != "" {
					w.Header().Set("Content-Type", tc.contentType)
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
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
				t.Fatalf("expected IsError=true on HTTP %d", tc.status)
			}
			textContent, ok := res.Content[0].(*mcp.TextContent)
			if !ok {
				t.Fatalf("expected TextContent, got %T", res.Content[0])
			}
			for _, want := range tc.wantSubstr {
				if !strings.Contains(strings.ToLower(textContent.Text), strings.ToLower(want)) {
					t.Errorf("error text %q missing %q", textContent.Text, want)
				}
			}
			for _, forbid := range tc.forbidSubstr {
				if strings.Contains(textContent.Text, forbid) {
					t.Errorf("error text %q leaked forbidden substring %q", textContent.Text, forbid)
				}
			}
		})
	}
}

func TestLargeListResponsesExceeding4MiB(t *testing.T) {
	// Build a legitimate list payload larger than 4 MiB (e.g., 5 jobs x 1 MiB result_text).
	largeText := strings.Repeat("x", 1<<20)
	jobsPayload, err := json.Marshal(map[string]any{
		"jobs": []map[string]any{
			{"id": 1, "title": "job-1", "status": "done", "result_text": largeText},
			{"id": 2, "title": "job-2", "status": "done", "result_text": largeText},
			{"id": 3, "title": "job-3", "status": "done", "result_text": largeText},
			{"id": 4, "title": "job-4", "status": "done", "result_text": largeText},
			{"id": 5, "title": "job-5", "status": "done", "result_text": largeText},
		},
		"next_cursor": 5,
		"has_more":    true,
	})
	if err != nil {
		t.Fatalf("marshal large payload: %v", err)
	}
	if len(jobsPayload) <= 4<<20 {
		t.Fatalf("expected payload > 4 MiB, got %d bytes", len(jobsPayload))
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(jobsPayload)
	}))
	defer ts.Close()

	session, cleanup := connectTestClient(t, Options{
		APIURL:     ts.URL,
		Token:      "large-page-token",
		HTTPClient: ts.Client(),
	})
	defer cleanup()

	ctx := context.Background()

	// list_jobs with > 4 MiB response must succeed and preserve pagination metadata.
	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "list_jobs",
		Arguments: map[string]any{"status": "done", "limit": 5},
	})
	if err != nil || res.IsError {
		t.Fatalf("list_jobs > 4 MiB failed: err=%v res=%+v", err, res)
	}
	got := decodeToolResultJSON(t, res)
	if got["has_more"] != true || got["next_cursor"] != float64(5) {
		t.Errorf("list_jobs pagination mismatch: has_more=%v next_cursor=%v", got["has_more"], got["next_cursor"])
	}
	jobs, ok := got["jobs"].([]any)
	if !ok || len(jobs) != 5 {
		t.Errorf("list_jobs count = %d, want 5", len(jobs))
	}

	// list_deliveries with > 4 MiB response must also succeed.
	res, err = session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "list_deliveries",
		Arguments: map[string]any{"status": "undelivered", "limit": 5},
	})
	if err != nil || res.IsError {
		t.Fatalf("list_deliveries > 4 MiB failed: err=%v res=%+v", err, res)
	}
	got = decodeToolResultJSON(t, res)
	jobs, ok = got["jobs"].([]any)
	if !ok || len(jobs) != 5 {
		t.Errorf("list_deliveries count = %d, want 5", len(jobs))
	}
}

func TestResponseSizeBoundary(t *testing.T) {
	const boundary int64 = 1024
	const secret = "boundary-secret-token"

	// Construct JSON payloads of exact length == boundary and length == boundary + 1.
	makePayload := func(targetLen int) []byte {
		const prefix = `{"data":"`
		const suffix = `"}`
		padLen := targetLen - len(prefix) - len(suffix)
		if padLen < 0 {
			t.Fatalf("targetLen %d too small", targetLen)
		}
		return []byte(prefix + strings.Repeat("a", padLen) + suffix)
	}

	exactPayload := makePayload(int(boundary))
	overPayload := makePayload(int(boundary) + 1)
	oversizedError := strings.Repeat("e", maxErrorResponseBytes+1024) + " Bearer " + secret

	var mode string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch mode {
		case "exact":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(exactPayload)
		case "over":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(overPayload)
		case "huge_error":
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(oversizedError))
		}
	}))
	defer ts.Close()

	client, err := NewClient(ts.URL, secret, ts.Client())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	client.maxResponseBytes = boundary

	ctx := context.Background()

	// Exact boundary (len == limit) must succeed.
	mode = "exact"
	got, err := client.ListJobs(ctx, ListJobsInput{Status: "done"})
	if err != nil {
		t.Fatalf("ListJobs at exact boundary (%d bytes) failed: %v", boundary, err)
	}
	if _, ok := got["data"].(string); !ok {
		t.Errorf("ListJobs at exact boundary missing data field: %+v", got)
	}

	// One byte over boundary (len == limit + 1) must fail.
	mode = "over"
	_, err = client.ListJobs(ctx, ListJobsInput{Status: "done"})
	if err == nil {
		t.Fatalf("ListJobs at boundary+1 (%d bytes) unexpectedly succeeded", boundary+1)
	}
	if !strings.Contains(err.Error(), "exceeded") {
		t.Errorf("expected size-exceeded error, got: %v", err)
	}

	// Non-2xx response larger than maxErrorResponseBytes must still return HTTP status safely without token leak.
	mode = "huge_error"
	_, err = client.ListJobs(ctx, ListJobsInput{Status: "done"})
	if err == nil {
		t.Fatalf("ListJobs on huge 502 error unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "502") {
		t.Errorf("expected HTTP 502 in error, got: %v", err)
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "exceeded") {
		t.Errorf("unexpected error content for huge 502: %v", err)
	}
}

func TestReleaseHeld409SanitizesReason(t *testing.T) {
	const (
		secret      = "release-held-secret-token-xyz"
		otherBearer = "other-leaked-bearer-999"
		basicCreds  = "dXNlcjpwYXNz"
	)

	cases := []struct {
		name         string
		body         string
		wantIsError  bool
		wantReason   string
		wantSubstr   []string
		forbidSubstr []string
	}{
		{
			name:         "409 structured result redacts client token and auth headers in reason",
			body:         `{"released":false,"reason":"upstream rejected token ` + secret + ` via Authorization: Bearer ` + secret + ` and Bearer ` + otherBearer + ` and Basic ` + basicCreds + `"}`,
			wantIsError:  false,
			wantSubstr:   []string{"[REDACTED]"},
			forbidSubstr: []string{secret, otherBearer, basicCreds},
		},
		{
			name:         "409 structured result preserves normal safe reason",
			body:         `{"released":false,"reason":"primary pool still below threshold"}`,
			wantIsError:  false,
			wantReason:   "primary pool still below threshold",
			forbidSubstr: []string{secret},
		},
		{
			name:         "409 with whitespace-only reason falls back to API error",
			body:         `{"released":false,"reason":"   \t\n  "}`,
			wantIsError:  true,
			wantSubstr:   []string{"409"},
			forbidSubstr: []string{secret},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/gate/held/held-409/release" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer ts.Close()

			session, cleanup := connectTestClient(t, Options{
				APIURL:     ts.URL,
				Token:      secret,
				HTTPClient: ts.Client(),
			})
			defer cleanup()

			res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
				Name:      "release_held",
				Arguments: map[string]any{"id": "held-409"},
			})
			if err != nil {
				t.Fatalf("unexpected protocol error: %v", err)
			}
			if res.IsError != tc.wantIsError {
				t.Fatalf("IsError = %v, want %v (res=%+v)", res.IsError, tc.wantIsError, res)
			}

			tcText, ok := res.Content[0].(*mcp.TextContent)
			if !ok {
				t.Fatalf("expected *mcp.TextContent, got %T", res.Content[0])
			}
			structuredRaw, err := json.Marshal(res.StructuredContent)
			if err != nil {
				t.Fatalf("marshal StructuredContent: %v", err)
			}
			allOutput := tcText.Text + "\n" + string(structuredRaw)

			for _, forbid := range tc.forbidSubstr {
				if strings.Contains(allOutput, forbid) {
					t.Errorf("release_held output leaked forbidden substring %q: %s", forbid, allOutput)
				}
			}
			for _, want := range tc.wantSubstr {
				if !strings.Contains(allOutput, want) {
					t.Errorf("release_held output missing %q: %s", want, allOutput)
				}
			}

			if !tc.wantIsError {
				got := decodeToolResultJSON(t, res)
				if got["released"] != false {
					t.Errorf("released = %v, want false", got["released"])
				}
				reason, ok := got["reason"].(string)
				if !ok || reason == "" {
					t.Errorf("expected non-empty reason in structured result, got %+v", got)
				}
				if tc.wantReason != "" && reason != tc.wantReason {
					t.Errorf("reason = %q, want %q", reason, tc.wantReason)
				}
			}
		})
	}
}

func TestMaxListPageHTMLEscapeSizingAndStreaming(t *testing.T) {
	// API constants from internal/api/server.go (maxBodyBytes = 1 MiB) and
	// internal/store/types.go (MaxListLimit = 200), kept local so internal/mcpserver
	// stays decoupled from internal/store and internal/api.
	const (
		apiMaxBodyBytes = 1 << 20 // 1 MiB
		apiMaxListLimit = 200
	)

	// 1. Reproduce the review's 500 KiB '<' example: Go's json.NewEncoder (escapeHTML=true)
	// expands each '<' to '\u003c' (6x), so 200 items exceed 512 MiB.
	reviewInstructions := strings.Repeat("<", 500<<10)
	var reviewBuf bytes.Buffer
	if err := json.NewEncoder(&reviewBuf).Encode(reviewInstructions); err != nil {
		t.Fatalf("encode 500 KiB instructions: %v", err)
	}
	encodedReviewField := int64(reviewBuf.Len() - 1) // exclude trailing newline from Encode
	if encodedReviewField != 3_072_002 {
		t.Fatalf("encoded 500 KiB '<' field = %d bytes, want 3072002", encodedReviewField)
	}
	if encodedReviewField*apiMaxListLimit <= 512<<20 {
		t.Fatalf("expected 200x 500 KiB '<' to exceed 512 MiB, got %d", encodedReviewField*apiMaxListLimit)
	}

	// 2. Measure the worst-case single job accepted by the API:
	// up to 3 independent 1 MiB request bodies (create instructions, complete result_text,
	// fail/cancel last_error), each filled with HTML-escaped '<' characters.
	maxFieldChars := apiMaxBodyBytes - 64 // leave room for JSON request envelope within 1 MiB
	worstField := strings.Repeat("<", maxFieldChars)
	worstJob := map[string]any{
		"id":           123456,
		"request_id":   "req-worst-case-0001",
		"title":        "worst-case-job",
		"status":       "done",
		"agent":        "claude",
		"host":         "worker-01",
		"instructions": worstField,
		"result_text":  worstField,
		"last_error":   worstField,
		"created_at":   "2026-11-24T00:00:00Z",
		"updated_at":   "2026-11-24T00:00:00Z",
	}
	var jobBuf bytes.Buffer
	if err := json.NewEncoder(&jobBuf).Encode(worstJob); err != nil {
		t.Fatalf("encode worst-case job: %v", err)
	}
	singleJobBytes := int64(jobBuf.Len() - 1)
	if singleJobBytes >= defaultMaxSingleResponseBytes {
		t.Fatalf("worst-case single job (%d bytes) >= defaultMaxSingleResponseBytes (%d)", singleJobBytes, defaultMaxSingleResponseBytes)
	}
	// 200 worst-case jobs + commas + {"jobs":[...],"next_cursor":123456,"has_more":true} envelope.
	maxPageBytes := singleJobBytes*apiMaxListLimit + int64(apiMaxListLimit) + 128
	if maxPageBytes >= defaultMaxListResponseBytes {
		t.Fatalf("worst-case 200-job page (%d bytes) >= defaultMaxListResponseBytes (%d)", maxPageBytes, defaultMaxListResponseBytes)
	}
	if defaultMaxSingleResponseBytes >= defaultMaxListResponseBytes {
		t.Fatalf("expected single response limit (%d) < list response limit (%d)", defaultMaxSingleResponseBytes, defaultMaxListResponseBytes)
	}

	// 3. Stream a real 200-item HTML-escaped list_jobs response through Client.ListJobs
	// (200 x 8 KiB '<' -> ~48 KiB JSON/job, ~9.6 MiB HTTP response) and verify streaming
	// decode unescapes '<' and preserves pagination metadata.
	const itemChars = 8 << 10
	itemInstructions := strings.Repeat("<", itemChars)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		enc := json.NewEncoder(w)
		jobs := make([]map[string]any, apiMaxListLimit)
		for i := range jobs {
			jobs[i] = map[string]any{
				"id":           i + 1,
				"title":        "html-escaped-job",
				"status":       "queued",
				"instructions": itemInstructions,
			}
		}
		_ = enc.Encode(map[string]any{
			"jobs":        jobs,
			"next_cursor": apiMaxListLimit,
			"has_more":    true,
		})
	}))
	defer ts.Close()

	client, err := NewClient(ts.URL, "max-page-token", ts.Client())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	limit := apiMaxListLimit
	got, err := client.ListJobs(context.Background(), ListJobsInput{
		Status: "queued",
		Limit:  &limit,
	})
	if err != nil {
		t.Fatalf("Client.ListJobs 200-item HTML-escaped page failed: %v", err)
	}
	if got["has_more"] != true || got["next_cursor"] != float64(apiMaxListLimit) {
		t.Fatalf("pagination mismatch: has_more=%v next_cursor=%v", got["has_more"], got["next_cursor"])
	}
	jobs, ok := got["jobs"].([]any)
	if !ok || len(jobs) != apiMaxListLimit {
		t.Fatalf("jobs count = %d, want %d", len(jobs), apiMaxListLimit)
	}
	first, ok := jobs[0].(map[string]any)
	if !ok || first["instructions"] != itemInstructions {
		t.Fatalf("first job instructions mismatch (len=%d)", len(first["instructions"].(string)))
	}

	// 4. Also exercise the full MCP CallTool path with a 200-item HTML-escaped page
	// sized within the MCP JSON-RPC transport frame limit.
	mcpInstructions := strings.Repeat("<", 1<<10) // 1 KiB '<' -> 6 KiB JSON/job (~1.2 MiB HTTP response)
	tsMCP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		enc := json.NewEncoder(w)
		jobs := make([]map[string]any, apiMaxListLimit)
		for i := range jobs {
			jobs[i] = map[string]any{
				"id":           i + 1,
				"title":        "html-escaped-job",
				"status":       "queued",
				"instructions": mcpInstructions,
			}
		}
		_ = enc.Encode(map[string]any{
			"jobs":        jobs,
			"next_cursor": apiMaxListLimit,
			"has_more":    true,
		})
	}))
	defer tsMCP.Close()

	session, cleanup := connectTestClient(t, Options{
		APIURL:     tsMCP.URL,
		Token:      "max-page-token",
		HTTPClient: tsMCP.Client(),
	})
	defer cleanup()

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "list_jobs",
		Arguments: map[string]any{"status": "queued", "limit": apiMaxListLimit},
	})
	if err != nil || res.IsError {
		t.Fatalf("MCP list_jobs 200-item HTML-escaped page failed: err=%v res=%+v", err, res)
	}
	gotMCP := decodeToolResultJSON(t, res)
	if gotMCP["has_more"] != true || gotMCP["next_cursor"] != float64(apiMaxListLimit) {
		t.Fatalf("MCP pagination mismatch: has_more=%v next_cursor=%v", gotMCP["has_more"], gotMCP["next_cursor"])
	}
	mcpJobs, ok := gotMCP["jobs"].([]any)
	if !ok || len(mcpJobs) != apiMaxListLimit {
		t.Fatalf("MCP jobs count = %d, want %d", len(mcpJobs), apiMaxListLimit)
	}
}

func TestNewServerValidation(t *testing.T) {
	for _, bad := range []string{"", "ftp://example.com", "not-a-url", "http://"} {
		if _, err := NewServer(Options{APIURL: bad}); err == nil {
			t.Errorf("NewServer(APIURL=%q) unexpectedly succeeded", bad)
		}
	}
}

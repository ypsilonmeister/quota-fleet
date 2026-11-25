package mcpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/ypsilonmeister/quota-fleet/internal/api"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
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
			"supersede_key":       "publish-1",
			"supersede_scope":     "pr-1",
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
	if sentBody["supersede_key"] != "publish-1" || sentBody["supersede_scope"] != "pr-1" {
		t.Errorf("create_job did not send supersede_scope and supersede_key: %+v", sentBody)
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
		Arguments: map[string]any{"status": "all", "cursor": "2026-11-24T01:00:00Z|held-000", "limit": 15},
	})
	if err != nil || res.IsError {
		t.Fatalf("list_held failed: err=%v res=%+v", err, res)
	}
	lr = lastReq()
	if lr.Method != http.MethodGet || lr.Path != "/gate/held" || !strings.Contains(lr.Query, "status=all") || !strings.Contains(lr.Query, "limit=15") || !strings.Contains(lr.Query, "cursor=") {
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
		Arguments: map[string]any{"job_id": 101, "held_id": "held-001", "cursor": 50, "limit": 5},
	})
	if err != nil || res.IsError {
		t.Fatalf("gate_decisions failed: err=%v res=%+v", err, res)
	}
	lr = lastReq()
	if lr.Method != http.MethodGet || lr.Path != "/gate/decisions" {
		t.Errorf("gate_decisions HTTP req = %+v", lr)
	}
	for _, wantParam := range []string{"job_id=101", "held_id=held-001", "cursor=50", "limit=5"} {
		if !strings.Contains(lr.Query, wantParam) {
			t.Errorf("gate_decisions query %q missing %q", lr.Query, wantParam)
		}
	}

	// 11. list_deliveries
	res, err = session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "list_deliveries",
		Arguments: map[string]any{"status": "delivered", "cursor": "2026-11-24T01:00:00Z|100", "limit": 10},
	})
	if err != nil || res.IsError {
		t.Fatalf("list_deliveries failed: err=%v res=%+v", err, res)
	}
	lr = lastReq()
	if lr.Method != http.MethodGet || lr.Path != "/deliveries" || !strings.Contains(lr.Query, "status=delivered") || !strings.Contains(lr.Query, "limit=10") || !strings.Contains(lr.Query, "cursor=") {
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

	deliveriesPayload, err := json.Marshal(map[string]any{
		"jobs": []map[string]any{
			{"id": 1, "title": "job-1", "status": "done", "result_text": largeText, "updated_at": "2026-11-24T00:00:01Z"},
			{"id": 2, "title": "job-2", "status": "done", "result_text": largeText, "updated_at": "2026-11-24T00:00:02Z"},
			{"id": 3, "title": "job-3", "status": "done", "result_text": largeText, "updated_at": "2026-11-24T00:00:03Z"},
			{"id": 4, "title": "job-4", "status": "done", "result_text": largeText, "updated_at": "2026-11-24T00:00:04Z"},
			{"id": 5, "title": "job-5", "status": "done", "result_text": largeText, "updated_at": "2026-11-24T00:00:05Z"},
		},
		"next_cursor": "2026-11-24T00:00:05Z|5",
		"has_more":    true,
	})
	if err != nil {
		t.Fatalf("marshal deliveries payload: %v", err)
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if r.URL.Path == "/deliveries" {
			_, _ = w.Write(deliveriesPayload)
			return
		}
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

	// Construct JSON list payloads of exact length == boundary and length == boundary + 1.
	makePayload := func(targetLen int) []byte {
		const prefix = `{"jobs":[{"id":1,"title":"`
		const suffix = `"}],"has_more":false}`
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
	if len(got.Jobs) != 1 {
		t.Errorf("ListJobs at exact boundary jobs count = %d, want 1", len(got.Jobs))
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

func TestDecodeJSONRejectsTrailingData(t *testing.T) {
	// Build two ~400-byte items so a 2000-byte MCP frame budget keeps item 1 and truncates item 2.
	truncItem1 := `{"id":1,"title":"` + strings.Repeat("a", 300) + `","status":"done"}`
	truncItem2 := `{"id":2,"title":"` + strings.Repeat("b", 300) + `","status":"done"}`
	const tightMCPBudget int64 = 2000

	cases := []struct {
		name           string
		isList         bool
		body           string
		maxBytes       int64
		maxMCPBytes    int64
		wantErr        bool
		wantTruncated  bool
		errSubstr      string
	}{
		{
			name:    "single object with trailing whitespace succeeds",
			isList:  false,
			body:    "{\"id\":1,\"status\":\"done\"}   \t\r\n  ",
			wantErr: false,
		},
		{
			name:    "list object with trailing whitespace succeeds",
			isList:  true,
			body:    "{\"jobs\":[{\"id\":1}],\"has_more\":false}  \n\t ",
			wantErr: false,
		},
		{
			name:          "truncated list with trailing whitespace succeeds",
			isList:        true,
			body:          fmt.Sprintf(`{"jobs":[%s,%s],"has_more":false}   `+"\t\r\n ", truncItem1, truncItem2),
			maxMCPBytes:   tightMCPBudget,
			wantErr:       false,
			wantTruncated: true,
		},
		{
			name:        "truncated list followed by second JSON object fails",
			isList:      true,
			body:        fmt.Sprintf(`{"jobs":[%s,%s],"has_more":false}{"extra":true}`, truncItem1, truncItem2),
			maxMCPBytes: tightMCPBudget,
			wantErr:     true,
		},
		{
			name:        "truncated list followed by trailing garbage fails",
			isList:      true,
			body:        fmt.Sprintf(`{"jobs":[%s,%s],"has_more":false} trailing-junk`, truncItem1, truncItem2),
			maxMCPBytes: tightMCPBudget,
			wantErr:     true,
		},
		{
			name:        "truncated list with malformed skipped array item fails",
			isList:      true,
			body:        fmt.Sprintf(`{"jobs":[%s,%s,{"id":BROKEN}],"has_more":false}`, truncItem1, truncItem2),
			maxMCPBytes: tightMCPBudget,
			wantErr:     true,
		},
		{
			name:        "truncated list with missing closing array and object fails",
			isList:      true,
			body:        fmt.Sprintf(`{"jobs":[%s,%s`, truncItem1, truncItem2),
			maxMCPBytes: tightMCPBudget,
			wantErr:     true,
		},
		{
			name:        "truncated list with malformed trailing envelope field fails",
			isList:      true,
			body:        fmt.Sprintf(`{"jobs":[%s,%s],"has_more":notabool}`, truncItem1, truncItem2),
			maxMCPBytes: tightMCPBudget,
			wantErr:     true,
		},
		{
			name:    "single object followed by second JSON object fails",
			isList:  false,
			body:    `{"id":1}{"id":2}`,
			wantErr: true,
		},
		{
			name:    "single object followed by whitespace and second JSON value fails",
			isList:  false,
			body:    `{"id":1}   123`,
			wantErr: true,
		},
		{
			name:    "single object followed by null fails",
			isList:  false,
			body:    `{"id":1} null`,
			wantErr: true,
		},
		{
			name:    "single object followed by invalid trailing garbage fails",
			isList:  false,
			body:    `{"id":1} garbage`,
			wantErr: true,
		},
		{
			name:    "list object followed by second JSON object fails",
			isList:  true,
			body:    `{"jobs":[{"id":1}],"has_more":false}{"extra":true}`,
			wantErr: true,
		},
		{
			name:    "list object followed by invalid trailing garbage fails",
			isList:  true,
			body:    `{"jobs":[{"id":1}],"has_more":false} trailing-junk`,
			wantErr: true,
		},
		{
			name:      "trailing whitespace pushing over maxResponseBytes fails with exceeded",
			isList:    false,
			body:      `{"id":1}  `, // 10 bytes
			maxBytes:  9,
			wantErr:   true,
			errSubstr: "exceeded",
		},
		{
			name:     "trailing whitespace at exact maxResponseBytes succeeds",
			isList:   false,
			body:     `{"id":1}  `, // 10 bytes
			maxBytes: 10,
			wantErr:  false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer ts.Close()

			client, err := NewClient(ts.URL, "trailing-test-token", ts.Client())
			if err != nil {
				t.Fatalf("NewClient: %v", err)
			}
			if tc.maxBytes > 0 {
				client.maxResponseBytes = tc.maxBytes
			}
			if tc.maxMCPBytes > 0 {
				client.maxMCPFrameBytes = tc.maxMCPBytes
			}

			ctx := context.Background()
			var listRes *ListJobsResult
			if tc.isList {
				listRes, err = client.ListJobs(ctx, ListJobsInput{Status: "done"})
			} else {
				_, err = client.GetJob(ctx, 1)
			}

			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for body %q, got nil", tc.body)
				}
				if tc.errSubstr != "" && !strings.Contains(err.Error(), tc.errSubstr) {
					t.Fatalf("error %q missing substring %q", err.Error(), tc.errSubstr)
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error for body %q: %v", tc.body, err)
				}
				if tc.wantTruncated {
					if listRes == nil || !listRes.Truncated || !listRes.HasMore || listRes.NextCursor != 1 || len(listRes.Jobs) != 1 {
						t.Fatalf("expected truncated 1-job result with next_cursor=1, got %+v", listRes)
					}
				}
			}
		})
	}
}

func TestListJobsByteBudgetPaginationAndContinuation(t *testing.T) {
	// Verify:
	// 1) A single worst-case job with 3 x 1 MiB '<' fields (~18 MiB JSON, > 36 MiB raw MCP wire)
	//    does NOT block list_jobs (even with limit=1): list_jobs projects long text fields to
	//    <= defaultMaxListTextBytes, marks fields_truncated, and advances next_cursor.
	// 2) Multi-page cursor continuation collects all jobs cleanly without stalling.
	// 3) Because this ~18 MiB JSON job exceeds the default 16 MiB MCP transport frame limit
	//    (~39 MiB wire with dual output), full untruncated retrieval uses the HTTP API (Client.GetJob)
	//    on the exact same job data.
	const totalJobs = 5
	hugeHTMLText := strings.Repeat("<", 1<<20) // 1 MiB '<' -> 6 MiB HTML-escaped JSON per field (~18 MiB/job -> >36 MiB raw wire)

	makeGiantJob := func(id int) map[string]any {
		return map[string]any{
			"id":           id,
			"title":        fmt.Sprintf("job-%d", id),
			"status":       "done",
			"instructions": hugeHTMLText,
			"result_text":  hugeHTMLText,
			"last_error":   hugeHTMLText,
		}
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/jobs":
			cursor, _ := strconv.Atoi(r.URL.Query().Get("cursor"))
			limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
			if limit <= 0 {
				limit = 50
			}
			jobs := make([]map[string]any, 0, totalJobs)
			for id := cursor + 1; id <= totalJobs && len(jobs) < limit; id++ {
				jobs = append(jobs, makeGiantJob(id))
			}
			hasMore := len(jobs) > 0 && jobs[len(jobs)-1]["id"].(int) < totalJobs
			var nextCursor int
			if hasMore {
				nextCursor = jobs[len(jobs)-1]["id"].(int)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jobs":        jobs,
				"next_cursor": nextCursor,
				"has_more":    hasMore,
			})
		case "/jobs/1":
			_ = json.NewEncoder(w).Encode(makeGiantJob(1))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer ts.Close()

	session, cleanup := connectTestClient(t, Options{
		APIURL:     ts.URL,
		Token:      "budget-pagination-token",
		HTTPClient: ts.Client(),
	})
	defer cleanup()

	ctx := context.Background()
	var (
		collectedIDs []int
		cursor       int64
	)

	// Paginate with limit=1 across 5 giant jobs (~18 MiB JSON each).
	for page := 1; page <= totalJobs+1; page++ {
		args := map[string]any{"status": "done", "limit": 1}
		if cursor > 0 {
			args["cursor"] = cursor
		}
		res, err := session.CallTool(ctx, &mcp.CallToolParams{
			Name:      "list_jobs",
			Arguments: args,
		})
		if err != nil || res.IsError {
			t.Fatalf("list_jobs limit=1 page %d (cursor=%d) failed: err=%v res=%+v", page, cursor, err, res)
		}
		got := decodeToolResultJSON(t, res)
		jobs, ok := got["jobs"].([]any)
		if !ok || len(jobs) != 1 {
			t.Fatalf("page %d expected 1 job, got %+v", page, got)
		}
		item := jobs[0].(map[string]any)
		collectedIDs = append(collectedIDs, int(item["id"].(float64)))

		// Verify long fields were projected and fields_truncated was recorded.
		for _, field := range []string{"instructions", "result_text", "last_error"} {
			v, _ := item[field].(string)
			if len(v) > defaultMaxListTextBytes {
				t.Errorf("page %d %s len=%d > defaultMaxListTextBytes=%d", page, field, len(v), defaultMaxListTextBytes)
			}
			if !strings.HasSuffix(v, "...") {
				t.Errorf("page %d %s expected '...' suffix after projection", page, field)
			}
		}
		ftRaw, ok := item["fields_truncated"].([]any)
		if !ok || len(ftRaw) != 3 {
			t.Errorf("page %d fields_truncated = %v, want 3 entries", page, item["fields_truncated"])
		}

		hasMore, _ := got["has_more"].(bool)
		if !hasMore {
			break
		}
		nextCursorFloat, ok := got["next_cursor"].(float64)
		if !ok || int64(nextCursorFloat) <= cursor {
			t.Fatalf("page %d did not advance next_cursor: got %v (prev=%d)", page, got["next_cursor"], cursor)
		}
		cursor = int64(nextCursorFloat)
	}

	wantIDs := []int{1, 2, 3, 4, 5}
	if !reflect.DeepEqual(collectedIDs, wantIDs) {
		t.Fatalf("collected job IDs = %v, want %v", collectedIDs, wantIDs)
	}

	// Verify direct HTTP Client.GetJob retrieves the exact same 3 x 1 MiB '<' full text (< 32 MiB HTTP limit).
	httpClient, err := NewClient(ts.URL, "budget-pagination-token", ts.Client())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	full, err := httpClient.GetJob(ctx, 1)
	if err != nil {
		t.Fatalf("HTTP Client.GetJob(1) failed: %v", err)
	}
	for _, field := range []string{"instructions", "result_text", "last_error"} {
		if gotStr, _ := full[field].(string); gotStr != hugeHTMLText {
			t.Fatalf("HTTP Client.GetJob(1) %s len = %d, want exact %d", field, len(gotStr), len(hugeHTMLText))
		}
	}
}

func TestListByteBudgetBoundary(t *testing.T) {
	item1 := []byte(`{"id":1,"title":"item-1","status":"done","result_text":"` + strings.Repeat("b", 2000) + `"}`)
	item2 := []byte(`{"id":2,"title":"item-2","status":"done","result_text":"` + strings.Repeat("c", 2000) + `"}`)

	// Compute the exact wire size of the complete 2-item typed envelope.
	twoItemEnv, err := json.Marshal(&ListJobsResult{
		Jobs:       []json.RawMessage{item1, item2},
		NextCursor: 2,
		HasMore:    false,
	})
	if err != nil {
		t.Fatalf("marshal twoItemEnv: %v", err)
	}
	exactTwoItemWire := mcpWireFrameBytes(twoItemEnv)

	// Include a huge unknown envelope field ("extra_huge_blob") to verify that non-item
	// unknown fields are discarded and cannot blow the output-wide MCP frame budget.
	hugeUnknownBlob := strings.Repeat("Z", 200<<10)
	payload := fmt.Sprintf(`{"jobs":[%s,%s],"next_cursor":2,"has_more":false,"extra_huge_blob":%q}`, item1, item2, hugeUnknownBlob)

	var mode string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		switch mode {
		case "oversized_cursor":
			_, _ = fmt.Fprintf(w, `{"held":[{"id":"h1","created_at":"2026-11-24T00:00:00Z"}],"next_cursor":%q,"has_more":true}`, strings.Repeat("c", maxListCursorStringBytes+1))
		default:
			_, _ = w.Write([]byte(payload))
		}
	}))
	defer ts.Close()

	client, err := NewClient(ts.URL, "boundary-budget-token", ts.Client())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	ctx := context.Background()

	// 1. Comfortable budget: 2 items returned, unknown huge envelope field discarded, whole frame <= budget.
	client.maxMCPFrameBytes = exactTwoItemWire + 1024
	got, err := client.ListJobs(ctx, ListJobsInput{Status: "done"})
	if err != nil {
		t.Fatalf("ListJobs with comfortable budget failed: %v", err)
	}
	if got.Truncated {
		t.Errorf("expected no truncation, got truncated=true")
	}
	if len(got.Jobs) != 2 {
		t.Errorf("jobs count = %d, want 2", len(got.Jobs))
	}
	gotRaw, _ := json.Marshal(got)
	if strings.Contains(string(gotRaw), "extra_huge_blob") || strings.Contains(string(gotRaw), "ZZZZ") {
		t.Errorf("unknown envelope field leaked into typed output")
	}
	if wire := mcpWireFrameBytes(gotRaw); wire > client.maxMCPFrameBytes {
		t.Errorf("actual whole-output wire=%d > budget=%d", wire, client.maxMCPFrameBytes)
	}

	// 2. Exact boundary minus 1 byte: must truncate to 1 item with has_more=true, next_cursor=1,
	//    and the entire serialized output must fit within exactTwoItemWire-1.
	client.maxMCPFrameBytes = exactTwoItemWire - 1
	got, err = client.ListJobs(ctx, ListJobsInput{Status: "done"})
	if err != nil {
		t.Fatalf("ListJobs at exactTwoItemWire-1 failed: %v", err)
	}
	if !got.Truncated || !got.HasMore || got.NextCursor != 1 || len(got.Jobs) != 1 {
		t.Errorf("expected truncated=true has_more=true next_cursor=1 len=1, got %+v", got)
	}
	gotRaw, _ = json.Marshal(got)
	if wire := mcpWireFrameBytes(gotRaw); wire > client.maxMCPFrameBytes {
		t.Errorf("truncated whole-output wire=%d > budget=%d", wire, client.maxMCPFrameBytes)
	}

	// 3. Tight budget smaller than item1 full projection, but large enough for minimal stub:
	//    must still succeed using minimal stub projection so pagination can advance!
	client.maxMCPFrameBytes = 1500
	got, err = client.ListJobs(ctx, ListJobsInput{Status: "done"})
	if err != nil {
		t.Fatalf("ListJobs at minimal-stub budget failed: %v", err)
	}
	if !got.Truncated || !got.HasMore || got.NextCursor != 1 || len(got.Jobs) != 1 {
		t.Errorf("expected stub-projected truncated result with next_cursor=1, got %+v", got)
	}
	gotRaw, _ = json.Marshal(got)
	if wire := mcpWireFrameBytes(gotRaw); wire > client.maxMCPFrameBytes {
		t.Errorf("minimal-stub whole-output wire=%d > budget=%d", wire, client.maxMCPFrameBytes)
	}
	if !strings.Contains(string(got.Jobs[0]), `"fields_truncated":["*"]`) {
		t.Errorf("expected minimal stub fields_truncated=[\"*\"], got %s", string(got.Jobs[0]))
	}

	// 4. Impossibly tiny budget (< JSON-RPC wrapper + empty envelope): must fail cleanly.
	client.maxMCPFrameBytes = 256
	_, err = client.ListJobs(ctx, ListJobsInput{Status: "done"})
	if err == nil {
		t.Fatalf("expected error for impossibly tiny budget, got nil")
	}
	if !strings.Contains(err.Error(), "exceeds MCP frame budget") {
		t.Errorf("unexpected error for tiny budget: %v", err)
	}

	// 5. Oversized upstream string next_cursor must be rejected.
	mode = "oversized_cursor"
	client.maxMCPFrameBytes = defaultMaxMCPFrameBytes
	_, err = client.ListHeld(ctx, ListHeldInput{})
	if err == nil || !strings.Contains(err.Error(), "next_cursor length") {
		t.Errorf("expected oversized next_cursor rejection, got: %v", err)
	}
}

func TestAllFourListToolsCursorContinuationEndToEnd(t *testing.T) {
	// End-to-end test against a REAL SQLite store + REAL HTTP API server + REAL MCP server.
	// Populates jobs, held jobs, gate decisions, and deliveries (undelivered & delivered),
	// forces MCP frame budget truncation on every list tool, and verifies full cursor
	// continuation collects 100% of items without duplicates or stalls.
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "fleet.db")
	st, err := store.Open(ctx, dbPath, store.Options{InstanceID: "mcp-e2e-test"})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	longBody := strings.Repeat("X", 6000) // > defaultMaxListTextBytes (4096)

	// 1. Seed 5 done jobs (3 undelivered, 2 delivered).
	var wantJobIDs, wantUndeliveredIDs, wantDeliveredIDs []int64
	for i := 1; i <= 5; i++ {
		job, _, err := st.CreateJob(ctx, store.CreateJobParams{
			RequestID:    fmt.Sprintf("req-job-%d", i),
			Title:        fmt.Sprintf("job-%d", i),
			Agent:        "claude",
			Host:         "ubuntu",
			Instructions: longBody,
		})
		if err != nil {
			t.Fatalf("CreateJob %d: %v", i, err)
		}
		claimed, ok, err := st.ClaimByID(ctx, job.ID, 10)
		if err != nil || !ok {
			t.Fatalf("ClaimByID %d: ok=%v err=%v", job.ID, ok, err)
		}
		if _, err := st.Complete(ctx, job.ID, claimed.ClaimedBy, longBody); err != nil {
			t.Fatalf("Complete %d: %v", job.ID, err)
		}
		wantJobIDs = append(wantJobIDs, job.ID)
		if i <= 2 {
			if _, err := st.MarkDelivered(ctx, job.ID); err != nil {
				t.Fatalf("MarkDelivered %d: %v", job.ID, err)
			}
			wantDeliveredIDs = append(wantDeliveredIDs, job.ID)
		} else {
			wantUndeliveredIDs = append(wantUndeliveredIDs, job.ID)
		}
		time.Sleep(2 * time.Millisecond)
	}

	// 2. Seed 4 held jobs with large payload instructions.
	var wantHeldIDs []string
	for i := 1; i <= 4; i++ {
		held, _, err := st.HoldJob(ctx, store.HoldJobParams{
			RequestID: fmt.Sprintf("req-held-%d", i),
			Pool:      "claude",
			Payload: store.CreateJobParams{
				RequestID:    fmt.Sprintf("req-held-%d", i),
				Title:        fmt.Sprintf("held-%d", i),
				Instructions: longBody,
			},
			Reason:          longBody,
			SnapshotSummary: "claude: exhausted",
		})
		if err != nil {
			t.Fatalf("HoldJob %d: %v", i, err)
		}
		wantHeldIDs = append(wantHeldIDs, held.ID)
		time.Sleep(2 * time.Millisecond)
	}

	// 3. Seed 4 gate decisions with large reason text.
	var wantDecisionIDs []int64
	for i := 1; i <= 4; i++ {
		dec, err := st.RecordGateDecision(ctx, store.GateDecision{
			JobID:         wantJobIDs[0],
			Mode:          "enforce",
			RequestedPool: "claude",
			Decision:      "would_hold",
			Reason:        longBody,
			DecidedAt:     time.Now().UTC(),
		})
		if err != nil {
			t.Fatalf("RecordGateDecision %d: %v", i, err)
		}
		wantDecisionIDs = append(wantDecisionIDs, dec.ID)
	}
	// Decisions are returned newest-first (id DESC).
	for i, j := 0, len(wantDecisionIDs)-1; i < j; i, j = i+1, j-1 {
		wantDecisionIDs[i], wantDecisionIDs[j] = wantDecisionIDs[j], wantDecisionIDs[i]
	}
	// Delivered jobs are returned newest-first (delivered_at DESC, id DESC).
	for i, j := 0, len(wantDeliveredIDs)-1; i < j; i, j = i+1, j-1 {
		wantDeliveredIDs[i], wantDeliveredIDs[j] = wantDeliveredIDs[j], wantDeliveredIDs[i]
	}

	const apiToken = "e2e-cursor-token"
	apiSrv := api.NewServer(st, api.Options{Token: apiToken})
	ts := httptest.NewServer(apiSrv)
	defer ts.Close()

	// Create an MCP client with a tight frame budget (~24 KiB) so that even after
	// 4 KiB field projection (~10-18 KiB wire per item), each list tool truncates to 1-2 items
	// per page and must advance via next_cursor!
	client, err := NewClient(ts.URL, apiToken, ts.Client())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	client.maxMCPFrameBytes = 24 << 10 // 24 KiB

	// A) list_jobs continuation
	{
		var gotIDs []int64
		var cursor *int64
		truncatedPages := 0
		for page := 1; page <= 10; page++ {
			lim := 50
			res, err := client.ListJobs(ctx, ListJobsInput{Status: "done", Cursor: cursor, Limit: &lim})
			if err != nil {
				t.Fatalf("ListJobs page %d: %v", page, err)
			}
			if res.Truncated {
				truncatedPages++
			}
			for _, raw := range res.Jobs {
				id, ok := extractInt64ID(raw)
				if !ok {
					t.Fatalf("missing id in job %s", string(raw))
				}
				gotIDs = append(gotIDs, id)
			}
			if !res.HasMore {
				break
			}
			if res.NextCursor <= 0 {
				t.Fatalf("ListJobs page %d has_more=true without next_cursor", page)
			}
			nc := res.NextCursor
			cursor = &nc
		}
		if truncatedPages == 0 {
			t.Errorf("ListJobs expected at least 1 budget-truncated page")
		}
		if !reflect.DeepEqual(gotIDs, wantJobIDs) {
			t.Errorf("ListJobs collected = %v, want %v", gotIDs, wantJobIDs)
		}
	}

	// B) list_held continuation
	{
		var gotIDs []string
		var cursor string
		truncatedPages := 0
		for page := 1; page <= 10; page++ {
			lim := 50
			res, err := client.ListHeld(ctx, ListHeldInput{Status: "held", Cursor: cursor, Limit: &lim})
			if err != nil {
				t.Fatalf("ListHeld page %d: %v", page, err)
			}
			if res.Truncated {
				truncatedPages++
			}
			for _, raw := range res.Held {
				var item struct {
					ID string `json:"id"`
				}
				if err := json.Unmarshal(raw, &item); err != nil || item.ID == "" {
					t.Fatalf("invalid held item %s", string(raw))
				}
				gotIDs = append(gotIDs, item.ID)
			}
			if !res.HasMore {
				break
			}
			if res.NextCursor == "" {
				t.Fatalf("ListHeld page %d has_more=true without next_cursor", page)
			}
			cursor = res.NextCursor
		}
		if truncatedPages == 0 {
			t.Errorf("ListHeld expected at least 1 budget-truncated page")
		}
		if !reflect.DeepEqual(gotIDs, wantHeldIDs) {
			t.Errorf("ListHeld collected = %v, want %v", gotIDs, wantHeldIDs)
		}
	}

	// C) gate_decisions continuation (tighter budget since decisions have 1 long field)
	{
		client.maxMCPFrameBytes = 12 << 10
		var gotIDs []int64
		var cursor *int64
		truncatedPages := 0
		for page := 1; page <= 10; page++ {
			lim := 50
			res, err := client.GateDecisions(ctx, GateDecisionsInput{Cursor: cursor, Limit: &lim})
			if err != nil {
				t.Fatalf("GateDecisions page %d: %v", page, err)
			}
			if res.Truncated {
				truncatedPages++
			}
			for _, raw := range res.Decisions {
				id, ok := extractInt64ID(raw)
				if !ok {
					t.Fatalf("missing id in decision %s", string(raw))
				}
				gotIDs = append(gotIDs, id)
			}
			if !res.HasMore {
				break
			}
			if res.NextCursor <= 0 {
				t.Fatalf("GateDecisions page %d has_more=true without next_cursor", page)
			}
			nc := res.NextCursor
			cursor = &nc
		}
		if truncatedPages == 0 {
			t.Errorf("GateDecisions expected at least 1 budget-truncated page")
		}
		if !reflect.DeepEqual(gotIDs, wantDecisionIDs) {
			t.Errorf("GateDecisions collected = %v, want %v", gotIDs, wantDecisionIDs)
		}
	}

	// D) list_deliveries continuation (both undelivered and delivered)
	client.maxMCPFrameBytes = 24 << 10
	for _, tc := range []struct {
		status  string
		wantIDs []int64
	}{
		{status: "undelivered", wantIDs: wantUndeliveredIDs},
		{status: "delivered", wantIDs: wantDeliveredIDs},
	} {
		var gotIDs []int64
		var cursor string
		truncatedPages := 0
		for page := 1; page <= 10; page++ {
			lim := 50
			res, err := client.ListDeliveries(ctx, ListDeliveriesInput{Status: tc.status, Cursor: cursor, Limit: &lim})
			if err != nil {
				t.Fatalf("ListDeliveries(%s) page %d: %v", tc.status, page, err)
			}
			if res.Truncated {
				truncatedPages++
			}
			for _, raw := range res.Jobs {
				id, ok := extractInt64ID(raw)
				if !ok {
					t.Fatalf("missing id in delivery %s", string(raw))
				}
				gotIDs = append(gotIDs, id)
			}
			if !res.HasMore {
				break
			}
			if res.NextCursor == "" {
				t.Fatalf("ListDeliveries(%s) page %d has_more=true without next_cursor", tc.status, page)
			}
			cursor = res.NextCursor
		}
		if truncatedPages == 0 {
			t.Errorf("ListDeliveries(%s) expected at least 1 budget-truncated page", tc.status)
		}
		if !reflect.DeepEqual(gotIDs, tc.wantIDs) {
			t.Errorf("ListDeliveries(%s) collected = %v, want %v", tc.status, gotIDs, tc.wantIDs)
		}
	}

	// E) Invalid cursor returns 400 error on held / deliveries.
	if _, err := client.ListHeld(ctx, ListHeldInput{Cursor: "not-a-valid-cursor"}); err == nil {
		t.Errorf("ListHeld with invalid cursor unexpectedly succeeded")
	}
	if _, err := client.ListDeliveries(ctx, ListDeliveriesInput{Cursor: "not-a-valid-cursor"}); err == nil {
		t.Errorf("ListDeliveries with invalid cursor unexpectedly succeeded")
	}
}

type maxLineTrackingWriter struct {
	io.WriteCloser
	mu      sync.Mutex
	curLine int
	maxLine int
}

func (w *maxLineTrackingWriter) Write(p []byte) (int, error) {
	n, err := w.WriteCloser.Write(p)
	w.mu.Lock()
	for _, b := range p[:n] {
		w.curLine++
		if b == '\n' {
			if w.curLine > w.maxLine {
				w.maxLine = w.curLine
			}
			w.curLine = 0
		}
	}
	if w.curLine > w.maxLine {
		w.maxLine = w.curLine
	}
	w.mu.Unlock()
	return n, err
}

func (w *maxLineTrackingWriter) MaxLine() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.maxLine
}

func TestMaxMCPListPageWireLimitAndPeakMemory(t *testing.T) {
	// Stream 120 HTML-escaped jobs (4 text fields x 8 KiB '<' each -> ~192 KiB JSON/job,
	// ~23 MiB total upstream < 64 MiB HTTP cap; after 4 KiB field projection -> ~96 KiB JSON/job
	// -> ~200 KiB MCP wire/job -> ~24 MiB unbudgeted wire > 14 MiB MCP budget) through a real
	// MCP IOTransport with default 16 MiB DefaultMaxLineLength.
	// Verify:
	// 1) All 120 upstream items are validated to EOF while only budgeted items are kept.
	// 2) The actual JSON-RPC wire frame is > 12 MiB, <= 14 MiB, and < 16 MiB SDK limit.
	// 3) Live heap stays tightly bounded even while validating all remaining items to EOF.
	const (
		totalItems = 120
		fieldChars = 8 << 10 // 8 KiB '<' -> projected to 4 KiB -> ~24 KiB HTML-escaped JSON/field
	)
	fieldText := strings.Repeat("<", fieldChars)
	var streamedItems atomic.Int64

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, `{"jobs":[`)
		enc := json.NewEncoder(w)
		for i := 1; i <= totalItems; i++ {
			if i > 1 {
				if _, err := io.WriteString(w, ","); err != nil {
					return
				}
			}
			streamedItems.Add(1)
			if err := enc.Encode(map[string]any{
				"id":                  i,
				"title":               "html-escaped-job",
				"status":              "queued",
				"instructions":        fieldText,
				"acceptance_criteria": fieldText,
				"result_text":         fieldText,
				"last_error":          fieldText,
			}); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		_, _ = fmt.Fprintf(w, `],"next_cursor":%d,"has_more":false}`, totalItems)
	}))
	defer ts.Close()

	srv, err := NewServer(Options{
		APIURL:     ts.URL,
		Token:      "max-page-token",
		HTTPClient: ts.Client(),
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	c2sR, c2sW := io.Pipe()
	s2cR, s2cW := io.Pipe()
	tracker := &maxLineTrackingWriter{WriteCloser: s2cW}

	ctx, cancel := context.WithCancel(context.Background())
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- srv.Run(ctx, &mcp.IOTransport{
			Reader:        c2sR,
			Writer:        tracker,
			MaxLineLength: 0, // default 16 MiB mcp.DefaultMaxLineLength
		})
	}()

	client := mcp.NewClient(&mcp.Implementation{Name: "io-test-client", Version: "0.0.1"}, nil)
	session, err := client.Connect(ctx, &mcp.IOTransport{
		Reader:        s2cR,
		Writer:        c2sW,
		MaxLineLength: 0, // default 16 MiB mcp.DefaultMaxLineLength
	}, nil)
	if err != nil {
		cancel()
		t.Fatalf("client.Connect over IOTransport: %v", err)
	}
	defer func() {
		_ = session.Close()
		_ = c2sW.Close()
		_ = s2cR.Close()
		cancel()
		<-serverDone
	}()

	// Phase 1: Measure decodeBudgetedList directly against a synthetic 120-item (~23 MiB)
	// streaming JSON reader validating all items to EOF while keeping live heap bounded.
	{
		pr, pw := io.Pipe()
		go func() {
			defer pw.Close()
			if _, err := io.WriteString(pw, `{"jobs":[`); err != nil {
				return
			}
			enc := json.NewEncoder(pw)
			for i := 1; i <= totalItems; i++ {
				if i > 1 {
					if _, err := io.WriteString(pw, ","); err != nil {
						return
					}
				}
				if err := enc.Encode(map[string]any{
					"id":                  i,
					"title":               "html-escaped-job",
					"status":              "queued",
					"instructions":        fieldText,
					"acceptance_criteria": fieldText,
					"result_text":         fieldText,
					"last_error":          fieldText,
				}); err != nil {
					return
				}
			}
			_, _ = fmt.Fprintf(pw, `],"next_cursor":%d,"has_more":false}`, totalItems)
		}()

		runtime.GC()
		var decBefore, decAfter runtime.MemStats
		runtime.ReadMemStats(&decBefore)

		rawDecoded, err := decodeBudgetedList(pr, "jobs", listCursorInt64ID, defaultMaxMCPFrameBytes, defaultMaxListTextBytes)
		_ = pr.Close()
		if err != nil {
			t.Fatalf("decodeBudgetedList direct stream failed: %v", err)
		}
		decoded := rawDecoded.(*ListJobsResult)
		runtime.GC()
		runtime.ReadMemStats(&decAfter)
		decLiveHeap := int64(decAfter.HeapAlloc) - int64(decBefore.HeapAlloc)

		if len(decoded.Jobs) == 0 || len(decoded.Jobs) >= totalItems {
			t.Fatalf("direct decode expected truncated non-empty page (<%d), got %d", totalItems, len(decoded.Jobs))
		}
		if !decoded.Truncated || !decoded.HasMore {
			t.Fatalf("direct decode expected truncated=true has_more=true, got %v / %v", decoded.Truncated, decoded.HasMore)
		}
		const maxLiveHeap = 32 << 20
		if decLiveHeap > maxLiveHeap {
			t.Errorf("direct decode liveHeapDelta = %d bytes (> %d bytes)", decLiveHeap, maxLiveHeap)
		}
		t.Logf("direct stream decode (%d items validated to EOF): returned=%d jobs, live_heap_delta=%.2f MiB",
			totalItems, len(decoded.Jobs), float64(decLiveHeap)/(1<<20))
	}

	// Phase 2: Full end-to-end MCP CallTool over default 16 MiB IOTransport.
	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "list_jobs",
		Arguments: map[string]any{"status": "queued", "limit": totalItems},
	})
	if err != nil || res.IsError {
		t.Fatalf("MCP list_jobs max-page over default 16 MiB IOTransport failed: err=%v res=%+v", err, res)
	}

	wireBytes := int64(tracker.MaxLine())

	got := decodeToolResultJSON(t, res)
	jobs, ok := got["jobs"].([]any)
	if !ok || len(jobs) == 0 || len(jobs) >= totalItems {
		t.Fatalf("expected truncated non-empty page (<%d), got %d jobs", totalItems, len(jobs))
	}
	if got["truncated"] != true || got["has_more"] != true {
		t.Fatalf("expected truncated=true has_more=true, got truncated=%v has_more=%v", got["truncated"], got["has_more"])
	}
	if got["next_cursor"] != float64(len(jobs)) {
		t.Fatalf("next_cursor = %v, want %d", got["next_cursor"], len(jobs))
	}
	if streamedItems.Load() != totalItems {
		t.Errorf("streamedItems = %d, want %d (full stream validated to EOF)", streamedItems.Load(), totalItems)
	}
	first := jobs[0].(map[string]any)
	if len(first["instructions"].(string)) > defaultMaxListTextBytes {
		t.Fatalf("first job instructions len=%d > defaultMaxListTextBytes=%d", len(first["instructions"].(string)), defaultMaxListTextBytes)
	}

	if wireBytes <= 12<<20 {
		t.Errorf("measured wire frame = %d bytes, expected > 12 MiB to exercise max page", wireBytes)
	}
	if wireBytes > defaultMaxMCPFrameBytes {
		t.Errorf("measured wire frame = %d bytes > defaultMaxMCPFrameBytes (%d)", wireBytes, defaultMaxMCPFrameBytes)
	}
	if wireBytes >= mcp.DefaultMaxLineLength {
		t.Errorf("measured wire frame = %d bytes >= mcp.DefaultMaxLineLength (%d)", wireBytes, mcp.DefaultMaxLineLength)
	}

	t.Logf("max MCP list page verified: returned=%d/%d jobs (upstream validated=%d), wire_frame=%d bytes (%.2f MiB / 16.00 MiB SDK limit)",
		len(jobs), totalItems, streamedItems.Load(),
		wireBytes, float64(wireBytes)/(1<<20))
}

func connectIOTransportClient(t *testing.T, opts Options) (*mcp.ClientSession, *maxLineTrackingWriter, func()) {
	t.Helper()
	srv, err := NewServer(opts)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	c2sR, c2sW := io.Pipe()
	s2cR, s2cW := io.Pipe()
	tracker := &maxLineTrackingWriter{WriteCloser: s2cW}

	ctx, cancel := context.WithCancel(context.Background())
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- srv.Run(ctx, &mcp.IOTransport{
			Reader:        c2sR,
			Writer:        tracker,
			MaxLineLength: 0, // default 16 MiB mcp.DefaultMaxLineLength
		})
	}()

	client := mcp.NewClient(&mcp.Implementation{Name: "io-test-client", Version: "0.0.1"}, nil)
	session, err := client.Connect(ctx, &mcp.IOTransport{
		Reader:        s2cR,
		Writer:        c2sW,
		MaxLineLength: 0, // default 16 MiB mcp.DefaultMaxLineLength
	}, nil)
	if err != nil {
		cancel()
		t.Fatalf("client.Connect over IOTransport: %v", err)
	}

	cleanup := func() {
		_ = session.Close()
		_ = c2sW.Close()
		_ = s2cR.Close()
		cancel()
		<-serverDone
	}
	return session, tracker, cleanup
}

func TestListJobsExceedingHTTPResponseLimitContinuation(t *testing.T) {
	// End-to-end test against a REAL SQLite store + REAL HTTP API + REAL MCP server
	// over default 16 MiB IOTransport, crossing the REAL 64 MiB HTTP list response cap:
	// 15 queued jobs x 800 KiB '<' (~4.8 MiB JSON each -> ~72 MiB total > 64 MiB).
	// Verify that list_jobs(status="queued", limit=15) and omitted-limit filtered queries
	// automatically retry with smaller page sizes (preserving cursor & all filters),
	// validate every accepted HTTP page to EOF, report truncated=true + next_cursor,
	// and collect all 15 jobs with zero duplicates.
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "fleet-http-cap.db")
	st, err := store.Open(ctx, dbPath, store.Options{InstanceID: "mcp-http-cap-test"})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	const totalJobs = 15
	largeHTMLInstructions := strings.Repeat("<", 800<<10) // 800 KiB '<' -> ~4.8 MiB JSON/job
	wantAscIDs := make([]int64, 0, totalJobs)
	for i := 1; i <= totalJobs; i++ {
		job, _, err := st.CreateJob(ctx, store.CreateJobParams{
			RequestID:    fmt.Sprintf("http-cap-req-%d", i),
			Title:        fmt.Sprintf("http-cap-job-%d", i),
			Agent:        "claude",
			Host:         "ubuntu",
			Instructions: largeHTMLInstructions,
		})
		if err != nil {
			t.Fatalf("CreateJob %d: %v", i, err)
		}
		wantAscIDs = append(wantAscIDs, job.ID)
	}

	const apiToken = "http-cap-test-token"
	apiSrv := api.NewServer(st, api.Options{Token: apiToken})
	ts := httptest.NewServer(apiSrv)
	defer ts.Close()

	session, _, cleanup := connectIOTransportClient(t, Options{
		APIURL:     ts.URL,
		Token:      apiToken,
		HTTPClient: ts.Client(),
	})
	defer cleanup()

	// Case 1: Explicit limit=15 (exact 72 MiB > 64 MiB HTTP limit scenario).
	{
		var gotIDs []int64
		var cursor int64
		truncatedPages := 0
		for page := 1; page <= totalJobs; page++ {
			args := map[string]any{"status": "queued", "limit": totalJobs}
			if cursor > 0 {
				args["cursor"] = cursor
			}
			res, err := session.CallTool(ctx, &mcp.CallToolParams{
				Name:      "list_jobs",
				Arguments: args,
			})
			if err != nil || res.IsError {
				t.Fatalf("list_jobs(limit=15) page %d (cursor=%d) failed: err=%v res=%+v", page, cursor, err, res)
			}
			got := decodeToolResultJSON(t, res)
			jobs, ok := got["jobs"].([]any)
			if !ok || len(jobs) == 0 {
				t.Fatalf("page %d expected non-empty jobs, got %+v", page, got)
			}
			if page == 1 {
				if got["truncated"] != true || got["has_more"] != true {
					t.Fatalf("page 1 expected truncated=true has_more=true after HTTP-limit shrink, got truncated=%v has_more=%v", got["truncated"], got["has_more"])
				}
				reason, _ := got["truncated_reason"].(string)
				if !strings.Contains(reason, "HTTP") {
					t.Errorf("page 1 truncated_reason = %q, want HTTP limit explanation", reason)
				}
			}
			if got["truncated"] == true {
				truncatedPages++
			}
			for _, raw := range jobs {
				item := raw.(map[string]any)
				gotIDs = append(gotIDs, int64(item["id"].(float64)))
				instr, _ := item["instructions"].(string)
				if len(instr) > defaultMaxListTextBytes {
					t.Errorf("page %d instructions len=%d > %d", page, len(instr), defaultMaxListTextBytes)
				}
			}
			hasMore, _ := got["has_more"].(bool)
			if !hasMore {
				break
			}
			nc, ok := got["next_cursor"].(float64)
			if !ok || int64(nc) <= cursor {
				t.Fatalf("page %d next_cursor did not advance: got %v (prev=%d)", page, got["next_cursor"], cursor)
			}
			cursor = int64(nc)
		}
		if truncatedPages == 0 {
			t.Errorf("expected at least 1 HTTP-limit truncated page")
		}
		if !reflect.DeepEqual(gotIDs, wantAscIDs) {
			t.Fatalf("list_jobs(limit=15) collected = %v, want %v", gotIDs, wantAscIDs)
		}
	}

	// Case 2: Omitted limit + filters (agent, host, desc=true) preserved across HTTP-limit retries.
	{
		wantDescIDs := make([]int64, len(wantAscIDs))
		for i := range wantAscIDs {
			wantDescIDs[i] = wantAscIDs[len(wantAscIDs)-1-i]
		}
		var gotIDs []int64
		var cursor int64
		for page := 1; page <= totalJobs; page++ {
			args := map[string]any{
				"status": "queued",
				"agent":  "claude",
				"host":   "ubuntu",
				"desc":   true,
			}
			if cursor > 0 {
				args["cursor"] = cursor
			}
			res, err := session.CallTool(ctx, &mcp.CallToolParams{
				Name:      "list_jobs",
				Arguments: args,
			})
			if err != nil || res.IsError {
				t.Fatalf("list_jobs(omitted limit, desc=true) page %d failed: err=%v res=%+v", page, err, res)
			}
			got := decodeToolResultJSON(t, res)
			jobs, ok := got["jobs"].([]any)
			if !ok || len(jobs) == 0 {
				t.Fatalf("desc page %d expected non-empty jobs, got %+v", page, got)
			}
			for _, raw := range jobs {
				item := raw.(map[string]any)
				gotIDs = append(gotIDs, int64(item["id"].(float64)))
			}
			hasMore, _ := got["has_more"].(bool)
			if !hasMore {
				break
			}
			nc, ok := got["next_cursor"].(float64)
			if !ok || (cursor > 0 && int64(nc) >= cursor) {
				t.Fatalf("desc page %d next_cursor did not advance descending: got %v (prev=%d)", page, got["next_cursor"], cursor)
			}
			cursor = int64(nc)
		}
		if !reflect.DeepEqual(gotIDs, wantDescIDs) {
			t.Fatalf("list_jobs(desc=true) collected = %v, want %v", gotIDs, wantDescIDs)
		}
	}
}

func TestFullTextRecoveryContractOverDefaultIOTransport(t *testing.T) {
	// End-to-end round-trip verification of the full-text recovery contract using the EXACT SAME
	// stored SQLite records served by the real HTTP API over the official SDK's default
	// 16 MiB IOTransport (MaxLineLength=0):
	//
	// A) Jobs (list_jobs & list_deliveries -> MCP get_job / direct HTTP GET /jobs/{id}):
	//    1) Plain UTF-8 done job within guaranteed MCP get_job range:
	//       instructions, acceptance_criteria, result_text projected to <=4 KiB with exact
	//       fields_truncated; MCP get_job recovers all 3 fields byte-for-byte.
	//    2) Plain UTF-8 failed job within guaranteed MCP get_job range:
	//       instructions, acceptance_criteria, result_text, last_error projected to <=4 KiB with
	//       exact fields_truncated; MCP get_job recovers all 4 fields byte-for-byte.
	//    3) HTML-escaped ('<'/'>'/'&') failed job within guaranteed MCP get_job range (~10.4 MiB wire):
	//       all 4 fields projected with exact fields_truncated; MCP get_job recovers all 4 fields.
	//    4) HTML-escaped ('<'/'>'/'&') failed job exceeding default 16 MiB MCP frame limit (~20.3 MiB wire):
	//       list_jobs & list_deliveries still succeed with 4 KiB projection + exact fields_truncated;
	//       MCP get_job fails at the 16 MiB transport limit; direct HTTP GET /jobs/{id} (raw HTTP
	//       and MCP Client.GetJob) recovers all 4 fields byte-for-byte.
	//
	// B) Held jobs (list_held -> direct HTTP GET /gate/held, and released payload -> MCP get_job):
	//    1) Active held job with >4 KiB payload.instructions, payload.acceptance_criteria, reason,
	//       snapshot_summary, last_error: list_held projects all 5 with exact fields_truncated;
	//       direct HTTP GET /gate/held recovers all 5 fields byte-for-byte.
	//    2) Released held job: release clears held last_error; list_held(status=released) projects
	//       payload.instructions, payload.acceptance_criteria, reason, snapshot_summary with exact
	//       fields_truncated; MCP get_job(id=released_job_id) recovers carried-over payload fields
	//       (instructions, acceptance_criteria) while direct HTTP GET /gate/held recovers held-only
	//       fields (reason, snapshot_summary).
	//
	// C) Gate decisions (gate_decisions -> direct HTTP GET /gate/decisions):
	//    1) Decision with >4 KiB reason: gate_decisions projects reason with exact
	//       fields_truncated=["reason"]; direct HTTP GET /gate/decisions recovers reason byte-for-byte.
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "fleet-recovery.db")
	st, err := store.Open(ctx, dbPath, store.Options{InstanceID: "mcp-recovery-test"})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	seedDoneJob := func(reqID, title, instr, ac, result string) int64 {
		t.Helper()
		job, _, err := st.CreateJob(ctx, store.CreateJobParams{
			RequestID:          reqID,
			Title:              title,
			Agent:              "claude",
			Host:               "ubuntu",
			Instructions:       instr,
			AcceptanceCriteria: ac,
		})
		if err != nil {
			t.Fatalf("CreateJob(%s): %v", reqID, err)
		}
		claimed, ok, err := st.ClaimByID(ctx, job.ID, 10)
		if err != nil || !ok {
			t.Fatalf("ClaimByID(%d): ok=%v err=%v", job.ID, ok, err)
		}
		if _, err := st.Complete(ctx, job.ID, claimed.ClaimedBy, result); err != nil {
			t.Fatalf("Complete(%d): %v", job.ID, err)
		}
		return job.ID
	}

	seedFailedJob := func(reqID, title, instr, ac, result, lastErr string) int64 {
		t.Helper()
		job, _, err := st.CreateJob(ctx, store.CreateJobParams{
			RequestID:          reqID,
			Title:              title,
			Agent:              "claude",
			Host:               "ubuntu",
			Instructions:       instr,
			AcceptanceCriteria: ac,
		})
		if err != nil {
			t.Fatalf("CreateJob(%s): %v", reqID, err)
		}
		claimed, ok, err := st.ClaimByID(ctx, job.ID, 10)
		if err != nil || !ok {
			t.Fatalf("ClaimByID(%d): ok=%v err=%v", job.ID, ok, err)
		}
		if _, err := st.FailWithResult(ctx, job.ID, claimed.ClaimedBy, lastErr, result); err != nil {
			t.Fatalf("FailWithResult(%d): %v", job.ID, err)
		}
		return job.ID
	}

	// Job 1: done plain UTF-8 (3 x 512 KiB = 1.5 MiB plain -> ~3 MiB wire < 16 MiB).
	plainDoneInstr := strings.Repeat("A", 512<<10)
	plainDoneAC := strings.Repeat("C", 512<<10)
	plainDoneResult := strings.Repeat("B", 512<<10)
	plainDoneID := seedDoneJob("rec-plain-done", "plain-done-within-mcp", plainDoneInstr, plainDoneAC, plainDoneResult)

	// Job 2: failed plain UTF-8 (4 x 256 KiB = 1 MiB plain -> ~2 MiB wire < 16 MiB).
	plainFailInstr := strings.Repeat("D", 256<<10)
	plainFailAC := strings.Repeat("E", 256<<10)
	plainFailResult := strings.Repeat("F", 256<<10)
	plainFailErr := strings.Repeat("G", 256<<10)
	plainFailID := seedFailedJob("rec-plain-failed", "plain-failed-within-mcp", plainFailInstr, plainFailAC, plainFailResult, plainFailErr)

	// Job 3: failed HTML-escaped within 16 MiB MCP limit (4 x 200 KiB = 800 KiB -> ~10.4 MiB wire < 16 MiB).
	htmlWithinInstr := strings.Repeat("<", 200<<10)
	htmlWithinAC := strings.Repeat(">", 200<<10)
	htmlWithinResult := strings.Repeat("&", 200<<10)
	htmlWithinErr := strings.Repeat("<", 200<<10)
	htmlWithinID := seedFailedJob("rec-html-within", "html-within-mcp", htmlWithinInstr, htmlWithinAC, htmlWithinResult, htmlWithinErr)

	// Job 4: failed HTML-escaped exceeding 16 MiB MCP limit (4 x 400 KiB = 1.6 MiB -> ~20.3 MiB wire > 16 MiB).
	htmlOverInstr := strings.Repeat("<", 400<<10)
	htmlOverAC := strings.Repeat(">", 400<<10)
	htmlOverResult := strings.Repeat("&", 400<<10)
	htmlOverErr := strings.Repeat("<", 400<<10)
	htmlOverID := seedFailedJob("rec-html-over", "html-over-mcp", htmlOverInstr, htmlOverAC, htmlOverResult, htmlOverErr)

	// Held job 1 (active): all 5 long text fields > 4 KiB.
	heldInstr := "held-instr-" + strings.Repeat("H", 8<<10)
	heldAC := "held-ac-" + strings.Repeat("I", 8<<10)
	heldReason := "held-reason-" + strings.Repeat("J", 8<<10)
	heldSnapshot := "held-snapshot-" + strings.Repeat("K", 8<<10)
	heldLastErr := "held-last-error-" + strings.Repeat("L", 8<<10)
	heldActive, _, err := st.HoldJob(ctx, store.HoldJobParams{
		RequestID: "rec-held-active",
		Pool:      "claude-main",
		Payload: store.CreateJobParams{
			RequestID:          "rec-held-active",
			Title:              "held-active-long-fields",
			Agent:              "claude",
			Host:               "ubuntu",
			Instructions:       heldInstr,
			AcceptanceCriteria: heldAC,
		},
		Reason:          heldReason,
		SnapshotSummary: heldSnapshot,
	})
	if err != nil {
		t.Fatalf("HoldJob(active): %v", err)
	}
	if _, _, err := st.RecordHeldReleaseError(ctx, heldActive.ID, "initial release error"); err != nil {
		t.Fatalf("RecordHeldReleaseError(active): %v", err)
	}
	// Populate held_jobs.last_error with >4 KiB directly in SQLite so >4 KiB truncation of
	// held last_error is exercised end-to-end on the real SQLite store + HTTP API.
	rawDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open(sqlite): %v", err)
	}
	if _, err := rawDB.ExecContext(ctx, `UPDATE held_jobs SET last_error = ? WHERE id = ?`, heldLastErr, heldActive.ID); err != nil {
		rawDB.Close()
		t.Fatalf("UPDATE held_jobs.last_error: %v", err)
	}
	if err := rawDB.Close(); err != nil {
		t.Fatalf("rawDB.Close: %v", err)
	}

	// Held job 2 (released): payload fields transfer to created job, while held reason &
	// snapshot_summary stay on held_jobs and held last_error is cleared on release.
	relInstr := "rel-instr-" + strings.Repeat("M", 8<<10)
	relAC := "rel-ac-" + strings.Repeat("N", 8<<10)
	relReason := "rel-reason-" + strings.Repeat("O", 8<<10)
	relSnapshot := "rel-snapshot-" + strings.Repeat("P", 8<<10)
	heldReleased, _, err := st.HoldJob(ctx, store.HoldJobParams{
		RequestID: "rec-held-released",
		Pool:      "claude-main",
		Payload: store.CreateJobParams{
			RequestID:          "rec-held-released",
			Title:              "held-released-long-fields",
			Agent:              "claude",
			Host:               "ubuntu",
			Instructions:       relInstr,
			AcceptanceCriteria: relAC,
		},
		Reason:          relReason,
		SnapshotSummary: relSnapshot,
	})
	if err != nil {
		t.Fatalf("HoldJob(released): %v", err)
	}
	if _, _, err := st.RecordHeldReleaseError(ctx, heldReleased.ID, "transient error cleared on release"); err != nil {
		t.Fatalf("RecordHeldReleaseError(released): %v", err)
	}
	relParams := heldReleased.Payload
	relParams.RequestID = heldReleased.RequestID
	relParams.Pool = "claude-main"
	releasedJob, _, err := st.CreateJob(ctx, relParams)
	if err != nil {
		t.Fatalf("CreateJob(released): %v", err)
	}
	if _, err := st.MarkHeldReleased(ctx, heldReleased.ID, releasedJob.ID, "claude-main"); err != nil {
		t.Fatalf("MarkHeldReleased: %v", err)
	}

	// Gate decision with >4 KiB reason.
	decisionReason := "decision-reason-" + strings.Repeat("Q", 8<<10)
	decisionRec, err := st.RecordGateDecision(ctx, store.GateDecision{
		HeldID:        heldActive.ID,
		Held:          true,
		Mode:          "enforce",
		RequestedPool: "claude-main",
		Decision:      "would_hold",
		BlockedBy:     "weekly_remaining",
		Reason:        decisionReason,
	})
	if err != nil {
		t.Fatalf("RecordGateDecision: %v", err)
	}

	const apiToken = "recovery-contract-token"
	apiSrv := api.NewServer(st, api.Options{Token: apiToken})
	ts := httptest.NewServer(apiSrv)
	defer ts.Close()

	// Helper for direct raw HTTP API GET requests (bypassing MCP Client limits).
	directHTTPGetJSON := func(t *testing.T, path string) map[string]any {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+path, nil)
		if err != nil {
			t.Fatalf("NewRequest(%s): %v", path, err)
		}
		req.Header.Set("Authorization", "Bearer "+apiToken)
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatalf("HTTP GET %s: %v", path, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("HTTP GET %s status=%d body=%s", path, resp.StatusCode, string(body))
		}
		var out map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("decode HTTP GET %s: %v", path, err)
		}
		return out
	}

	toStringSlice := func(v any) []string {
		arr, ok := v.([]any)
		if !ok {
			return nil
		}
		out := make([]string, len(arr))
		for i, e := range arr {
			out[i], _ = e.(string)
		}
		return out
	}

	session, tracker, cleanup := connectIOTransportClient(t, Options{
		APIURL:     ts.URL,
		Token:      apiToken,
		HTTPClient: ts.Client(),
	})
	defer cleanup()

	// Verify tool descriptions explicitly document the recovery contract per tool/field.
	toolsRes, err := session.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	descByName := make(map[string]string, len(toolsRes.Tools))
	for _, tl := range toolsRes.Tools {
		descByName[tl.Name] = tl.Description
	}
	for _, tc := range []struct {
		tool   string
		substr string
	}{
		{"list_jobs", "GET /jobs/{id}"},
		{"list_jobs", "16 MiB"},
		{"list_jobs", "32 MiB"},
		{"get_job", "16 MiB"},
		{"get_job", "32 MiB"},
		{"get_job", "GET /jobs/{id}"},
		{"list_deliveries", "GET /jobs/{id}"},
		{"list_deliveries", "16 MiB"},
		{"list_deliveries", "32 MiB"},
		{"list_held", "GET /gate/held"},
		{"list_held", "reason, snapshot_summary, last_error"},
		{"list_held", "only returns fields carried over from payload"},
		{"list_held", "not held-record-specific fields"},
		{"gate_decisions", "GET /gate/decisions"},
	} {
		if !strings.Contains(descByName[tc.tool], tc.substr) {
			t.Errorf("tool %q description missing %q: %s", tc.tool, tc.substr, descByName[tc.tool])
		}
	}

	// Verify list_jobs (done & failed) and list_deliveries (undelivered) project all long fields
	// to <= 4 KiB and record the EXACT fields_truncated paths for every seeded job.
	wantJobFieldsTruncated := map[int64][]string{
		plainDoneID:  {"acceptance_criteria", "instructions", "result_text"},
		plainFailID:  {"acceptance_criteria", "instructions", "last_error", "result_text"},
		htmlWithinID: {"acceptance_criteria", "instructions", "last_error", "result_text"},
		htmlOverID:   {"acceptance_criteria", "instructions", "last_error", "result_text"},
	}
	checkProjectedJob := func(t *testing.T, source string, item map[string]any) {
		t.Helper()
		id := int64(item["id"].(float64))
		wantFT, ok := wantJobFieldsTruncated[id]
		if !ok {
			t.Fatalf("%s returned unexpected job id=%d", source, id)
		}
		for _, field := range wantFT {
			s, _ := item[field].(string)
			if len(s) == 0 || len(s) > defaultMaxListTextBytes {
				t.Errorf("%s id=%d field %q len=%d, want 1..%d", source, id, field, len(s), defaultMaxListTextBytes)
			}
		}
		gotFT := toStringSlice(item["fields_truncated"])
		if !reflect.DeepEqual(gotFT, wantFT) {
			t.Errorf("%s id=%d fields_truncated = %v, want %v", source, id, gotFT, wantFT)
		}
	}

	for _, status := range []string{"done", "failed"} {
		res, err := session.CallTool(ctx, &mcp.CallToolParams{
			Name:      "list_jobs",
			Arguments: map[string]any{"status": status, "limit": 10},
		})
		if err != nil || res.IsError {
			t.Fatalf("list_jobs(status=%s) failed: err=%v res=%+v", status, err, res)
		}
		got := decodeToolResultJSON(t, res)
		jobs := got["jobs"].([]any)
		wantCount := 1
		if status == "failed" {
			wantCount = 3
		}
		if len(jobs) != wantCount {
			t.Fatalf("list_jobs(status=%s) returned %d jobs, want %d", status, len(jobs), wantCount)
		}
		for _, raw := range jobs {
			checkProjectedJob(t, "list_jobs/"+status, raw.(map[string]any))
		}
	}

	{
		res, err := session.CallTool(ctx, &mcp.CallToolParams{
			Name:      "list_deliveries",
			Arguments: map[string]any{"status": "undelivered", "limit": 10},
		})
		if err != nil || res.IsError {
			t.Fatalf("list_deliveries failed: err=%v res=%+v", err, res)
		}
		got := decodeToolResultJSON(t, res)
		jobs := got["jobs"].([]any)
		if len(jobs) != 4 {
			t.Fatalf("list_deliveries returned %d jobs, want 4", len(jobs))
		}
		for _, raw := range jobs {
			checkProjectedJob(t, "list_deliveries", raw.(map[string]any))
		}
	}

	// A1-A3: Guaranteed full-text recovery via MCP get_job over default 16 MiB IOTransport
	// on the exact same stored jobs (verifying instructions, acceptance_criteria, result_text, last_error).
	for _, tc := range []struct {
		name        string
		id          int64
		wantInstr   string
		wantAC      string
		wantResult  string
		wantLastErr string
	}{
		{"plain_done", plainDoneID, plainDoneInstr, plainDoneAC, plainDoneResult, ""},
		{"plain_failed", plainFailID, plainFailInstr, plainFailAC, plainFailResult, plainFailErr},
		{"html_within_16MiB", htmlWithinID, htmlWithinInstr, htmlWithinAC, htmlWithinResult, htmlWithinErr},
	} {
		t.Run("get_job_"+tc.name, func(t *testing.T) {
			res, err := session.CallTool(ctx, &mcp.CallToolParams{
				Name:      "get_job",
				Arguments: map[string]any{"id": tc.id},
			})
			if err != nil || res.IsError {
				t.Fatalf("MCP get_job(%d) failed: err=%v res=%+v", tc.id, err, res)
			}
			full := decodeToolResultJSON(t, res)
			if got, _ := full["instructions"].(string); got != tc.wantInstr {
				t.Fatalf("get_job(%d) instructions len=%d, want exact %d", tc.id, len(got), len(tc.wantInstr))
			}
			if got, _ := full["acceptance_criteria"].(string); got != tc.wantAC {
				t.Fatalf("get_job(%d) acceptance_criteria len=%d, want exact %d", tc.id, len(got), len(tc.wantAC))
			}
			if got, _ := full["result_text"].(string); got != tc.wantResult {
				t.Fatalf("get_job(%d) result_text len=%d, want exact %d", tc.id, len(got), len(tc.wantResult))
			}
			if got, _ := full["last_error"].(string); got != tc.wantLastErr {
				t.Fatalf("get_job(%d) last_error len=%d, want exact %d", tc.id, len(got), len(tc.wantLastErr))
			}
		})
	}
	t.Logf("guaranteed MCP get_job round-trip max wire line: %d bytes (%.2f MiB / 16.00 MiB SDK limit)",
		tracker.MaxLine(), float64(tracker.MaxLine())/(1<<20))

	// A4: Job exceeding default 16 MiB MCP transport frame limit (~20.3 MiB wire):
	// Use a dedicated IOTransport session so the frame-limit disconnect does not affect earlier checks.
	overSession, _, overCleanup := connectIOTransportClient(t, Options{
		APIURL:     ts.URL,
		Token:      apiToken,
		HTTPClient: ts.Client(),
	})
	defer overCleanup()

	_, err = overSession.CallTool(ctx, &mcp.CallToolParams{
		Name:      "get_job",
		Arguments: map[string]any{"id": htmlOverID},
	})
	if err == nil {
		t.Fatalf("expected MCP get_job(%d) with 4x400KiB HTML-escaped fields to exceed default 16 MiB IOTransport frame limit, but it succeeded", htmlOverID)
	}
	t.Logf("confirmed MCP get_job(%d) above 16 MiB frame limit fails as documented: %v", htmlOverID, err)

	// Direct raw HTTP API GET /jobs/{id} (and MCP Client.GetJob) recover all 4 fields verbatim.
	rawOver := directHTTPGetJSON(t, fmt.Sprintf("/jobs/%d", htmlOverID))
	for field, want := range map[string]string{
		"instructions":        htmlOverInstr,
		"acceptance_criteria": htmlOverAC,
		"result_text":         htmlOverResult,
		"last_error":          htmlOverErr,
	} {
		if got, _ := rawOver[field].(string); got != want {
			t.Fatalf("direct HTTP GET /jobs/%d %s len=%d, want exact %d", htmlOverID, field, len(got), len(want))
		}
	}
	httpClient, err := NewClient(ts.URL, apiToken, ts.Client())
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	fullOver, err := httpClient.GetJob(ctx, htmlOverID)
	if err != nil {
		t.Fatalf("HTTP Client.GetJob(%d) failed: %v", htmlOverID, err)
	}
	for field, want := range map[string]string{
		"instructions":        htmlOverInstr,
		"acceptance_criteria": htmlOverAC,
		"result_text":         htmlOverResult,
		"last_error":          htmlOverErr,
	} {
		if got, _ := fullOver[field].(string); got != want {
			t.Fatalf("HTTP Client.GetJob(%d) %s len=%d, want exact %d", htmlOverID, field, len(got), len(want))
		}
	}

	// B1: list_held (active) projects all 5 long text fields with exact fields_truncated, and
	// direct HTTP GET /gate/held recovers the EXACT SAME record's 5 fields byte-for-byte.
	t.Run("list_held_active_http_recovery", func(t *testing.T) {
		res, err := session.CallTool(ctx, &mcp.CallToolParams{
			Name:      "list_held",
			Arguments: map[string]any{"status": "held", "limit": 10},
		})
		if err != nil || res.IsError {
			t.Fatalf("MCP list_held(held) failed: err=%v res=%+v", err, res)
		}
		got := decodeToolResultJSON(t, res)
		items := got["held"].([]any)
		if len(items) != 1 {
			t.Fatalf("list_held(held) returned %d items, want 1", len(items))
		}
		item := items[0].(map[string]any)
		if item["id"] != heldActive.ID {
			t.Fatalf("list_held(held) id=%v, want %s", item["id"], heldActive.ID)
		}
		wantFT := []string{
			"last_error",
			"payload.acceptance_criteria",
			"payload.instructions",
			"reason",
			"snapshot_summary",
		}
		if gotFT := toStringSlice(item["fields_truncated"]); !reflect.DeepEqual(gotFT, wantFT) {
			t.Fatalf("list_held(held) fields_truncated = %v, want %v", gotFT, wantFT)
		}
		payload := item["payload"].(map[string]any)
		for name, val := range map[string]string{
			"payload.instructions":        payload["instructions"].(string),
			"payload.acceptance_criteria": payload["acceptance_criteria"].(string),
			"reason":                      item["reason"].(string),
			"snapshot_summary":            item["snapshot_summary"].(string),
			"last_error":                  item["last_error"].(string),
		} {
			if len(val) == 0 || len(val) > defaultMaxListTextBytes {
				t.Errorf("list_held(held) %s len=%d, want 1..%d", name, len(val), defaultMaxListTextBytes)
			}
		}

		// Direct HTTP GET /gate/held?status=held recovers all 5 fields verbatim.
		httpHeldResp := directHTTPGetJSON(t, "/gate/held?status=held")
		httpItems := httpHeldResp["held"].([]any)
		if len(httpItems) != 1 {
			t.Fatalf("HTTP GET /gate/held?status=held returned %d items, want 1", len(httpItems))
		}
		httpItem := httpItems[0].(map[string]any)
		if httpItem["id"] != heldActive.ID {
			t.Fatalf("HTTP GET /gate/held id=%v, want %s", httpItem["id"], heldActive.ID)
		}
		httpPayload := httpItem["payload"].(map[string]any)
		if got, _ := httpPayload["instructions"].(string); got != heldInstr {
			t.Fatalf("HTTP GET /gate/held payload.instructions len=%d, want exact %d", len(got), len(heldInstr))
		}
		if got, _ := httpPayload["acceptance_criteria"].(string); got != heldAC {
			t.Fatalf("HTTP GET /gate/held payload.acceptance_criteria len=%d, want exact %d", len(got), len(heldAC))
		}
		if got, _ := httpItem["reason"].(string); got != heldReason {
			t.Fatalf("HTTP GET /gate/held reason len=%d, want exact %d", len(got), len(heldReason))
		}
		if got, _ := httpItem["snapshot_summary"].(string); got != heldSnapshot {
			t.Fatalf("HTTP GET /gate/held snapshot_summary len=%d, want exact %d", len(got), len(heldSnapshot))
		}
		if got, _ := httpItem["last_error"].(string); got != heldLastErr {
			t.Fatalf("HTTP GET /gate/held last_error len=%d, want exact %d", len(got), len(heldLastErr))
		}
	})

	// B2: list_held (released) -> MCP get_job(id=released_job_id) recovers carried-over payload
	// fields only, while held-specific fields (reason, snapshot_summary) are recovered via
	// direct HTTP GET /gate/held?status=released (and held last_error was cleared on release).
	t.Run("list_held_released_payload_vs_held_fields", func(t *testing.T) {
		res, err := session.CallTool(ctx, &mcp.CallToolParams{
			Name:      "list_held",
			Arguments: map[string]any{"status": "released", "limit": 10},
		})
		if err != nil || res.IsError {
			t.Fatalf("MCP list_held(released) failed: err=%v res=%+v", err, res)
		}
		got := decodeToolResultJSON(t, res)
		items := got["held"].([]any)
		if len(items) != 1 {
			t.Fatalf("list_held(released) returned %d items, want 1", len(items))
		}
		item := items[0].(map[string]any)
		if item["id"] != heldReleased.ID {
			t.Fatalf("list_held(released) id=%v, want %s", item["id"], heldReleased.ID)
		}
		wantFT := []string{
			"payload.acceptance_criteria",
			"payload.instructions",
			"reason",
			"snapshot_summary",
		}
		if gotFT := toStringSlice(item["fields_truncated"]); !reflect.DeepEqual(gotFT, wantFT) {
			t.Fatalf("list_held(released) fields_truncated = %v, want %v", gotFT, wantFT)
		}
		if le, _ := item["last_error"].(string); le != "" {
			t.Fatalf("list_held(released) last_error = %q, want empty (cleared on release)", le)
		}
		relJobID := int64(item["released_job_id"].(float64))
		if relJobID != releasedJob.ID {
			t.Fatalf("list_held(released) released_job_id = %d, want %d", relJobID, releasedJob.ID)
		}

		// MCP get_job(id=released_job_id) recovers carried-over payload fields, not held-specific fields.
		jobRes, err := session.CallTool(ctx, &mcp.CallToolParams{
			Name:      "get_job",
			Arguments: map[string]any{"id": relJobID},
		})
		if err != nil || jobRes.IsError {
			t.Fatalf("MCP get_job(%d) failed: err=%v res=%+v", relJobID, err, jobRes)
		}
		jobFull := decodeToolResultJSON(t, jobRes)
		if got, _ := jobFull["instructions"].(string); got != relInstr {
			t.Fatalf("get_job(released_job_id=%d) instructions len=%d, want exact %d", relJobID, len(got), len(relInstr))
		}
		if got, _ := jobFull["acceptance_criteria"].(string); got != relAC {
			t.Fatalf("get_job(released_job_id=%d) acceptance_criteria len=%d, want exact %d", relJobID, len(got), len(relAC))
		}
		if _, hasReason := jobFull["reason"]; hasReason {
			t.Errorf("get_job(released_job_id=%d) unexpectedly has held field 'reason'", relJobID)
		}
		if _, hasSnap := jobFull["snapshot_summary"]; hasSnap {
			t.Errorf("get_job(released_job_id=%d) unexpectedly has held field 'snapshot_summary'", relJobID)
		}

		// Held-specific fields (reason, snapshot_summary) are recovered via direct HTTP GET /gate/held.
		httpRelResp := directHTTPGetJSON(t, "/gate/held?status=released")
		httpItems := httpRelResp["held"].([]any)
		if len(httpItems) != 1 {
			t.Fatalf("HTTP GET /gate/held?status=released returned %d items, want 1", len(httpItems))
		}
		httpItem := httpItems[0].(map[string]any)
		if got, _ := httpItem["reason"].(string); got != relReason {
			t.Fatalf("HTTP GET /gate/held?status=released reason len=%d, want exact %d", len(got), len(relReason))
		}
		if got, _ := httpItem["snapshot_summary"].(string); got != relSnapshot {
			t.Fatalf("HTTP GET /gate/held?status=released snapshot_summary len=%d, want exact %d", len(got), len(relSnapshot))
		}
	})

	// C1: gate_decisions projects >4 KiB reason with exact fields_truncated=["reason"], and
	// direct HTTP GET /gate/decisions recovers the EXACT SAME decision's reason byte-for-byte.
	t.Run("gate_decisions_http_recovery", func(t *testing.T) {
		res, err := session.CallTool(ctx, &mcp.CallToolParams{
			Name:      "gate_decisions",
			Arguments: map[string]any{"held_id": heldActive.ID, "limit": 10},
		})
		if err != nil || res.IsError {
			t.Fatalf("MCP gate_decisions failed: err=%v res=%+v", err, res)
		}
		got := decodeToolResultJSON(t, res)
		decisions := got["decisions"].([]any)
		if len(decisions) != 1 {
			t.Fatalf("gate_decisions returned %d items, want 1", len(decisions))
		}
		dec := decisions[0].(map[string]any)
		if int64(dec["id"].(float64)) != decisionRec.ID {
			t.Fatalf("gate_decisions id=%v, want %d", dec["id"], decisionRec.ID)
		}
		if r, _ := dec["reason"].(string); len(r) == 0 || len(r) > defaultMaxListTextBytes {
			t.Errorf("gate_decisions reason len=%d, want 1..%d", len(r), defaultMaxListTextBytes)
		}
		wantFT := []string{"reason"}
		if gotFT := toStringSlice(dec["fields_truncated"]); !reflect.DeepEqual(gotFT, wantFT) {
			t.Fatalf("gate_decisions fields_truncated = %v, want %v", gotFT, wantFT)
		}

		// Direct HTTP GET /gate/decisions?held_id=... recovers reason verbatim.
		httpDecResp := directHTTPGetJSON(t, "/gate/decisions?held_id="+heldActive.ID)
		httpDecisions := httpDecResp["decisions"].([]any)
		if len(httpDecisions) != 1 {
			t.Fatalf("HTTP GET /gate/decisions returned %d items, want 1", len(httpDecisions))
		}
		httpDec := httpDecisions[0].(map[string]any)
		if int64(httpDec["id"].(float64)) != decisionRec.ID {
			t.Fatalf("HTTP GET /gate/decisions id=%v, want %d", httpDec["id"], decisionRec.ID)
		}
		if got, _ := httpDec["reason"].(string); got != decisionReason {
			t.Fatalf("HTTP GET /gate/decisions reason len=%d, want exact %d", len(got), len(decisionReason))
		}
	})
}

func TestNewServerValidation(t *testing.T) {
	for _, bad := range []string{"", "ftp://example.com", "not-a-url", "http://"} {
		if _, err := NewServer(Options{APIURL: bad}); err == nil {
			t.Errorf("NewServer(APIURL=%q) unexpectedly succeeded", bad)
		}
	}
}

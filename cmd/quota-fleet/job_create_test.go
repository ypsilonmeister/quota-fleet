package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ypsilonmeister/quota-fleet/internal/api"
)

func TestCLIJobCreateAPI(t *testing.T) {
	parent := int64(42)
	want := api.CreateJobRequest{
		RequestID: "api-req", Title: "CLI API job", Draft: true,
		Agent: "codex", Host: "local", Workdir: "/work/repo", TargetRef: "feat/test",
		Instructions: "run checks", AcceptanceCriteria: "tests pass", ParentID: &parent,
		Model: "gpt-6.2-sol", Pool: "codex",
	}
	for _, tc := range []struct {
		name       string
		status     int
		response   string
		wantOutput string
		configURL  bool
		extraFlags []string
		envToken   string
		wantToken  string
	}{
		{"created", http.StatusCreated, `{"job":{"id":9007199254740993,"status":"draft","title":"CLI API job","request_id":"api-req"},"created":true}`, "created job #9007199254740993 [draft] CLI API job", false, nil, "env-token", "env-token"},
		{"existing", http.StatusOK, `{"job":{"id":42,"status":"draft","title":"CLI API job","request_id":"api-req"},"created":false}`, "existing job #42 [draft] CLI API job", false, nil, "env-token", "env-token"},
		{"held", http.StatusAccepted, `{"held_id":"hold-42","held":{"request_id":"api-req","reason":"exhausted:weekly"},"created":true}`, "job held by Quota Gate\n  held_id: hold-42\n  reason: exhausted:weekly", true, nil, "", "cfg-token"},
		{"held-existing", http.StatusAccepted, `{"held_id":"hold-42","held":{"request_id":"api-req","reason":"exhausted:weekly"},"created":false}`, "job already held by Quota Gate\n  held_id: hold-42", true, nil, "env-token", "env-token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/jobs" {
					t.Errorf("got %s %s, want POST /jobs", r.Method, r.URL.Path)
				}
				if r.Header.Get("Content-Type") != "application/json" || r.Header.Get("Authorization") != "Bearer "+tc.wantToken {
					t.Error("unexpected request headers")
				}
				var got api.CreateJobRequest
				if err := json.NewDecoder(r.Body).Decode(&got); err != nil || !reflect.DeepEqual(got, want) {
					t.Errorf("request = %+v, decode error = %v, want %+v", got, err, want)
				}
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.response)
			}))
			defer srv.Close()

			t.Setenv("QUOTA_FLEET_TOKEN", tc.envToken)
			cfgAddr := "127.0.0.1:1"
			if tc.configURL {
				cfgAddr = strings.TrimPrefix(srv.URL, "http://")
			}
			cfg := filepath.Join(t.TempDir(), "fleet.yaml")
			body := fmt.Sprintf("instance_id: cli-api-test\napi:\n  listen: %s\n  token: cfg-token\n", cfgAddr)
			if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			args := []string{"job", "create", "--config", cfg, "--request-id", "api-req",
				"--title", "CLI API job", "--draft", "--agent", "codex", "--host", "local",
				"--workdir", "/work/repo", "--target-ref", "feat/test", "--instructions", "run checks",
				"--acceptance", "tests pass", "--parent", "42", "--model", "gpt-6.2-sol", "--pool", "codex"}
			switch tc.name {
			case "created":
				t.Setenv("QUOTA_FLEET_API_URL", "http://127.0.0.1:1")
				args = append(args, "--url", srv.URL+"/")
			case "existing":
				t.Setenv("QUOTA_FLEET_API_URL", srv.URL)
			default:
				t.Setenv("QUOTA_FLEET_API_URL", "")
			}
			args = append(args, tc.extraFlags...)
			var out, stderr bytes.Buffer
			if code := run(args, &out, &stderr); code != 0 {
				t.Fatalf("exit = %d, stderr = %q", code, stderr.String())
			}
			if !strings.Contains(out.String(), tc.wantOutput) || !strings.Contains(out.String(), "request_id: api-req") {
				t.Fatalf("unexpected output %q", out.String())
			}
		})
	}
}

func TestCLIJobCreateFlagConflicts(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"db without direct", []string{"--db", filepath.Join(t.TempDir(), "x.db")}, "--db requires --direct"},
		{"url with direct", []string{"--direct", "--db", filepath.Join(t.TempDir(), "x.db"), "--url", "http://127.0.0.1:1"}, "--url cannot be combined with --direct"},
		{"token flag removed", []string{"--token", "secret"}, "flag provided but not defined: -token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, stderr bytes.Buffer
			args := append([]string{"job", "create", "--title", "conflict"}, tc.args...)
			if code := run(args, &out, &stderr); code == 0 {
				t.Fatalf("expected failure, stdout = %q", out.String())
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Fatalf("stderr = %q, want %q", stderr.String(), tc.want)
			}
		})
	}
}

func TestCLIJobCreateUnreachableAPI(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()

	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	cfg := filepath.Join(dir, "fleet.yaml")
	body := fmt.Sprintf("instance_id: cli-unreachable-test\nstate_dir: %q\n", stateDir)
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	if code := run([]string{"job", "create", "--config", cfg, "--url", srv.URL, "--title", "Unreachable API"}, &out, &stderr); code == 0 {
		t.Fatalf("expected failure, stdout = %q", out.String())
	}
	if !strings.Contains(stderr.String(), "could not reach API (POST /jobs)") || out.Len() != 0 {
		t.Fatalf("stdout = %q, stderr = %q", out.String(), stderr.String())
	}
	if _, err := os.Stat(filepath.Join(stateDir, "fleet.db")); !os.IsNotExist(err) {
		t.Fatalf("expected no local database, stat error = %v", err)
	}
}

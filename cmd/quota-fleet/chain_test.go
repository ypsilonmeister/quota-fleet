package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestChainCreateSendsYAMLAsJSON(t *testing.T) {
	var got map[string]any
	var auth, path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, auth = r.Method+" "+r.URL.Path, r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"run":{"id":"r1"}}`))
	}))
	defer srv.Close()
	t.Setenv("QUOTA_FLEET_TOKEN", "tok")
	file := filepath.Join(t.TempDir(), "c.yaml")
	_ = os.WriteFile(file, []byte("request_id: qf-26-chain-1\nmax_rounds: 2\nsteps:\n  review: {title: r, agent: claude}\n"), 0o644)
	var out, errb bytes.Buffer
	if code := run([]string{"chain", "create", "-f", file, "--url", srv.URL}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if path != "POST /chains" || auth != "Bearer tok" || got["request_id"] != "qf-26-chain-1" || got["max_rounds"] != float64(2) {
		t.Fatalf("request: %s %q %+v", path, auth, got)
	}
	if !strings.Contains(out.String(), `"id": "r1"`) {
		t.Fatalf("output: %s", out.String())
	}
	if code := run([]string{"chain", "cancel", "--url", srv.URL}, &out, &errb); code == 0 {
		t.Fatal("cancel without id succeeded")
	}
}

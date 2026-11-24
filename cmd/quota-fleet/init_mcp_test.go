package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/ypsilonmeister/quota-fleet/internal/config"
)

func TestCLIInitCreatesMinimalSecureConfigAndGuardsOverwrite(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "fleet.yaml")

	var out, errBuf bytes.Buffer
	code := run([]string{"init", "--config", cfgPath}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("init exit=%d stderr=%q", code, errBuf.String())
	}

	info, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatalf("Stat(%q): %v", cfgPath, err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("fleet.yaml perm = %#o, want 0600", perm)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load(generated fleet.yaml): %v", err)
	}
	if !strings.HasPrefix(cfg.InstanceID, "fleet-") || len(cfg.InstanceID) < len("fleet-")+16 {
		t.Errorf("unexpected instance_id = %q", cfg.InstanceID)
	}
	if len(cfg.API.Token) < 64 {
		t.Errorf("expected strong random api.token (len >= 64), got len=%d", len(cfg.API.Token))
	}
	if !cfg.API.RequireAuth {
		t.Errorf("api.require_auth = false, want true")
	}
	if cfg.Runtime.EffectiveAdapter() != config.RuntimeAdapterSSH {
		t.Errorf("runtime.adapter = %q, want %q", cfg.Runtime.EffectiveAdapter(), config.RuntimeAdapterSSH)
	}
	if cfg.Limits.MaxClaimed != config.DefaultMaxClaimed || cfg.Limits.AttemptsCap != config.DefaultAttemptsCap {
		t.Errorf("limits = %+v, want {%d %d}", cfg.Limits, config.DefaultMaxClaimed, config.DefaultAttemptsCap)
	}
	localHost, ok := cfg.Hosts["local"]
	if !ok {
		t.Fatalf("expected hosts.local to be defined")
	}
	if localHost.Transport != "local" || localHost.OS != "linux" {
		t.Errorf("hosts.local = %+v, want transport=local os=linux", localHost)
	}
	for _, agentName := range []string{"claude", "codex", "agy"} {
		a, ok := cfg.Agents[agentName]
		if !ok {
			t.Errorf("expected agents.%s to be defined", agentName)
			continue
		}
		if strings.TrimSpace(a.Launch) == "" {
			t.Errorf("agents.%s.launch must not be empty", agentName)
		}
	}

	// Token must never be printed in plaintext to stdout or stderr.
	if strings.Contains(out.String(), cfg.API.Token) {
		t.Errorf("stdout leaked plaintext api.token")
	}
	if strings.Contains(errBuf.String(), cfg.API.Token) {
		t.Errorf("stderr leaked plaintext api.token")
	}
	if !strings.Contains(out.String(), "not printed") {
		t.Errorf("expected stdout to note token was written to file and not printed, got %q", out.String())
	}

	// Overwrite guard: running init again on the same path must fail and preserve original content.
	origBytes, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("ReadFile original: %v", err)
	}
	out.Reset()
	errBuf.Reset()
	code = run([]string{"init", "--config", cfgPath}, &out, &errBuf)
	if code == 0 {
		t.Fatalf("second init unexpectedly succeeded; expected overwrite guard error")
	}
	if !strings.Contains(errBuf.String(), "already exists") {
		t.Errorf("expected overwrite guard error message, got stderr=%q", errBuf.String())
	}
	afterBytes, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("ReadFile after second init: %v", err)
	}
	if !bytes.Equal(origBytes, afterBytes) {
		t.Errorf("existing fleet.yaml was modified despite overwrite guard")
	}

	// Uniqueness: another init in a different file generates different instance_id and token.
	cfgPath2 := filepath.Join(dir, "fleet2.yaml")
	out.Reset()
	errBuf.Reset()
	if code := run([]string{"init", "--config", cfgPath2}, &out, &errBuf); code != 0 {
		t.Fatalf("second path init exit=%d stderr=%q", code, errBuf.String())
	}
	cfg2, err := config.Load(cfgPath2)
	if err != nil {
		t.Fatalf("config.Load(fleet2.yaml): %v", err)
	}
	if cfg2.InstanceID == cfg.InstanceID {
		t.Errorf("expected distinct random instance_id across init calls")
	}
	if cfg2.API.Token == cfg.API.Token {
		t.Errorf("expected distinct random api.token across init calls")
	}
}

func TestCLIMCPConfigSnippetsAndNoTokenLeak(t *testing.T) {
	const secretToken = "qf-super-secret-token-should-never-appear"
	t.Setenv("QUOTA_FLEET_TOKEN", secretToken)
	t.Setenv("QUOTA_FLEET_API_URL", "http://127.0.0.1:9999")

	// Default format ("all") includes both Claude Code CLI and generic JSON snippets.
	var out, errBuf bytes.Buffer
	code := run([]string{"mcp", "config"}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("mcp config exit=%d stderr=%q", code, errBuf.String())
	}
	allOut := out.String()
	for _, want := range []string{
		"claude mcp add quota-fleet",
		`QUOTA_FLEET_API_URL="http://127.0.0.1:9999"`,
		`QUOTA_FLEET_TOKEN="${QUOTA_FLEET_TOKEN}"`,
		`"mcpServers"`,
		`"quota-fleet"`,
		`"QUOTA_FLEET_TOKEN": "${QUOTA_FLEET_TOKEN}"`,
	} {
		if !strings.Contains(allOut, want) {
			t.Errorf("mcp config output missing %q:\n%s", want, allOut)
		}
	}
	if strings.Contains(allOut, secretToken) || strings.Contains(errBuf.String(), secretToken) {
		t.Fatalf("mcp config leaked QUOTA_FLEET_TOKEN value")
	}

	// JSON-only format must be valid JSON referencing ${QUOTA_FLEET_TOKEN}.
	out.Reset()
	errBuf.Reset()
	code = run([]string{"mcp", "config", "--format", "json", "--url", "http://100.64.0.1:8737", "--binary", "/usr/local/bin/quota-fleet"}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("mcp config --format json exit=%d stderr=%q", code, errBuf.String())
	}
	var parsed mcpClientConfigJSON
	if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
		t.Fatalf("json.Unmarshal(mcp config --format json): %v\noutput:\n%s", err, out.String())
	}
	entry, ok := parsed.MCPServers["quota-fleet"]
	if !ok {
		t.Fatalf("missing mcpServers.quota-fleet entry")
	}
	if entry.Command != "/usr/local/bin/quota-fleet" {
		t.Errorf("command = %q, want /usr/local/bin/quota-fleet", entry.Command)
	}
	if len(entry.Args) != 1 || entry.Args[0] != "mcp" {
		t.Errorf("args = %v, want [mcp]", entry.Args)
	}
	if entry.Env["QUOTA_FLEET_API_URL"] != "http://100.64.0.1:8737" {
		t.Errorf("QUOTA_FLEET_API_URL = %q", entry.Env["QUOTA_FLEET_API_URL"])
	}
	if entry.Env["QUOTA_FLEET_TOKEN"] != "${QUOTA_FLEET_TOKEN}" {
		t.Errorf("QUOTA_FLEET_TOKEN = %q, want ${QUOTA_FLEET_TOKEN}", entry.Env["QUOTA_FLEET_TOKEN"])
	}
	if strings.Contains(out.String(), secretToken) {
		t.Fatalf("json output leaked QUOTA_FLEET_TOKEN value")
	}

	// Claude Code format only.
	out.Reset()
	errBuf.Reset()
	code = run([]string{"mcp", "config", "--format", "claude-code"}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("mcp config --format claude-code exit=%d stderr=%q", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "claude mcp add quota-fleet") || strings.Contains(out.String(), `"mcpServers"`) {
		t.Errorf("unexpected claude-code output:\n%s", out.String())
	}

	// Invalid format fails.
	out.Reset()
	errBuf.Reset()
	if code := run([]string{"mcp", "config", "--format", "yaml"}, &out, &errBuf); code == 0 {
		t.Errorf("expected non-zero exit for invalid --format")
	}
}

func TestCLIMCPServeContextToolsListAndStatus(t *testing.T) {
	const testToken = "cli-mcp-test-token"
	var gotAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && r.URL.Path == "/status" {
			_, _ = w.Write([]byte(`{"instance_id":"cli-mcp-inst","counts":{"queued":1},"total":1}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	t.Setenv("QUOTA_FLEET_API_URL", ts.URL)
	t.Setenv("QUOTA_FLEET_TOKEN", testToken)

	ct, st := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(context.Background())
	var errBuf bytes.Buffer
	doneCh := make(chan int, 1)
	go func() {
		doneCh <- runMCPServeContext(ctx, st, &errBuf)
	}()

	client := mcp.NewClient(&mcp.Implementation{Name: "cli-test-client", Version: "0.0.1"}, nil)
	session, err := client.Connect(ctx, ct, nil)
	if err != nil {
		cancel()
		t.Fatalf("client.Connect: %v (stderr=%q)", err, errBuf.String())
	}

	listRes, err := session.ListTools(ctx, nil)
	if err != nil {
		_ = session.Close()
		cancel()
		t.Fatalf("ListTools: %v", err)
	}
	if len(listRes.Tools) != 12 {
		t.Errorf("ListTools count = %d, want 12", len(listRes.Tools))
	}

	callRes, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "fleet_status",
		Arguments: map[string]any{},
	})
	if err != nil || callRes.IsError {
		t.Fatalf("fleet_status failed: err=%v res=%+v", err, callRes)
	}
	if gotAuth != "Bearer "+testToken {
		t.Errorf("Authorization header = %q, want %q", gotAuth, "Bearer "+testToken)
	}

	_ = session.Close()
	cancel()
	select {
	case code := <-doneCh:
		if code != 0 {
			t.Fatalf("runMCPServeContext exit=%d stderr=%q", code, errBuf.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("runMCPServeContext did not exit after cancel")
	}
}

func TestServeBannerAndMissingGeminiAPIKeyWarning(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "")

	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	cfgPath := filepath.Join(dir, "fleet.yaml")

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	// Intentionally omit limits to also verify serve starts with default limits (3/3).
	cfgYAML := fmt.Sprintf(`
instance_id: "serve-banner-inst"
state_dir: "%s"
api:
  listen: "%s"
  token: "test-bearer-token"
  require_auth: true
runtime:
  adapter: "ssh"
quota:
  gate_mode: "dry-run"
evaluator:
  mode: "typed+decision"
`, filepath.ToSlash(stateDir), addr)
	if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	var out, errBuf bytes.Buffer
	doneCh := make(chan int, 1)
	go func() {
		doneCh <- runServeContext(ctx, []string{"--config", cfgPath}, &out, &errBuf)
	}()

	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	deadline := time.Now().Add(3 * time.Second)
	for {
		req, _ := http.NewRequest(http.MethodGet, "http://"+addr+"/status", nil)
		req.Header.Set("Authorization", "Bearer test-bearer-token")
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("serve did not become ready on %s; stderr=%s", addr, errBuf.String())
		}
		time.Sleep(15 * time.Millisecond)
	}

	cancel()
	select {
	case code := <-doneCh:
		if code != 0 {
			t.Fatalf("serve exit code = %d, want 0; stderr=%s", code, errBuf.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("serve did not shut down within timeout")
	}

	banner := out.String()
	for _, want := range []string{
		"adapter:   ssh",
		"gate_mode: dry-run",
		"evaluator: typed+decision",
		"notifier:  file",
		"api_auth:  enabled (bearer token)",
		fmt.Sprintf("ui_url:    http://%s/ui/", addr),
		"WARNING:",
		"GEMINI_API_KEY",
		"free-form acceptance criteria",
	} {
		if !strings.Contains(banner, want) {
			t.Errorf("serve banner missing %q:\n%s", want, banner)
		}
	}
	if strings.Contains(banner, "test-bearer-token") {
		t.Errorf("serve banner must not leak api.token")
	}

	// When GEMINI_API_KEY is set, warning must not appear.
	t.Setenv("GEMINI_API_KEY", "dummy-key-for-banner-test")
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	var bannerWithKey bytes.Buffer
	printServeBanner(&bannerWithKey, nil, cfg, addr, cfg.DBPath(), cfg.Runtime.EffectiveDeliveriesDir(cfg.StateDir))
	if strings.Contains(bannerWithKey.String(), "WARNING:") {
		t.Errorf("unexpected warning when GEMINI_API_KEY is set:\n%s", bannerWithKey.String())
	}
}

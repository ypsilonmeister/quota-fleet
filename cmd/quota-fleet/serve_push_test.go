package main

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

// startPushServe runs serve until the returned stop function is called.
func startPushServe(t *testing.T, dir string, webPush bool) (base string, stop func() string) {
	t.Helper()
	return startPushServeWithAuth(t, dir, webPush, `  token: "serve-push-token"`)
}

// startPushServeWithAuth is startPushServe with the token lines of the api section.
func startPushServeWithAuth(t *testing.T, dir string, webPush bool, auth string) (base string, stop func() string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	cfgPath := filepath.Join(dir, "fleet.yaml")
	cfgYAML := fmt.Sprintf(`
instance_id: "serve-push-inst"
state_dir: "%s"
api:
  listen: "%s"
  web_push: %v
%s
runtime:
  adapter: "mock"
  dispatch_interval: "15ms"
  shepherd_interval: "15ms"
  reconcile_interval: "30ms"
  stale_claimed_after: "5s"
evaluator:
  mode: "always-pass"
`, filepath.ToSlash(filepath.Join(dir, "state")), addr, webPush, auth)
	if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	var out, errBuf bytes.Buffer
	doneCh := make(chan int, 1)
	go func() {
		doneCh <- runServeContext(ctx, []string{"--config", cfgPath}, &out, &errBuf)
	}()
	base = "http://" + addr
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := http.Get(base + "/ui/")
		if err == nil {
			_ = resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("serve did not become ready on %s: %v", addr, err)
		}
		time.Sleep(15 * time.Millisecond)
	}
	return base, func() string {
		http.DefaultClient.CloseIdleConnections()
		cancel()
		select {
		case code := <-doneCh:
			if code != 0 {
				t.Fatalf("serve exit = %d, stderr = %s", code, errBuf.String())
			}
		case <-time.After(10 * time.Second):
			t.Fatal("serve did not shut down")
		}
		return out.String() + errBuf.String()
	}
}

func serveCall(t *testing.T, method, url, body string) (int, string) {
	t.Helper()
	return serveCallAs(t, "serve-push-token", method, url, body)
}

func serveCallAs(t *testing.T, token, method, url, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(data)
}

// With api.web_push on, serve keeps one VAPID key pair across restarts and pushes the
// transitions made by the resident runtime.
func TestServeWebPushKeysPersistAndPush(t *testing.T) {
	pushed := make(chan string, 8)
	service := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pushed <- r.Header.Get("Authorization")
		w.WriteHeader(http.StatusGone)
	}))
	defer service.Close()
	webPushTransport = service.Client().Transport
	defer func() { webPushTransport = nil }()

	dir := t.TempDir()
	keyPath := filepath.Join(dir, "state", "vapid.json")

	base, stop := startPushServe(t, dir, true)
	code, first := serveCall(t, "GET", base+"/push/vapid-public-key", "")
	if code != http.StatusOK {
		t.Fatalf("public key = %d %s", code, first)
	}
	info, err := os.Stat(keyPath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v %v", info, err)
	}
	private, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sub := fmt.Sprintf(`{"endpoint":%q,"keys":{"p256dh":%q,"auth":%q}}`, service.URL+"/s",
		base64.RawURLEncoding.EncodeToString(private.PublicKey().Bytes()),
		base64.RawURLEncoding.EncodeToString(make([]byte, 16)))
	if code, body := serveCall(t, "POST", base+"/push/subscriptions", sub); code != http.StatusCreated {
		t.Fatalf("register = %d %s", code, body)
	}
	output := stop()
	if !strings.Contains(output, "web_push:  enabled") {
		t.Errorf("banner lacks web_push:\n%s", output)
	}

	base, stop = startPushServe(t, dir, true)
	if _, again := serveCall(t, "GET", base+"/push/vapid-public-key", ""); again != first {
		t.Errorf("public key changed across restart: %s -> %s", first, again)
	}
	var key struct {
		PublicKey string `json:"public_key"`
	}
	if err := json.Unmarshal([]byte(first), &key); err != nil {
		t.Fatal(err)
	}
	// queued -> claimed -> done is driven by the resident runtime.
	if code, body := serveCall(t, "POST", base+"/jobs",
		`{"request_id":"serve-push-1","title":"Push E2E","acceptance_criteria":"build ok"}`); code != http.StatusCreated {
		t.Fatalf("create job = %d %s", code, body)
	}
	select {
	case authorization := <-pushed:
		if !strings.HasSuffix(authorization, ", k="+key.PublicKey) {
			t.Errorf("Authorization = %q", authorization)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no push for the finished job")
	}
	// The 410 answer removes the subscription. Close of the sender, during stop, waits
	// for that.
	output = stop()
	if !strings.Contains(output, "expired (HTTP 410); deleted") {
		t.Errorf("log lacks the expiry:\n%s", output)
	}
	st, err := store.Open(context.Background(), filepath.Join(dir, "state", "fleet.db"), store.Options{})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()
	if subs, err := st.ListPushSubscriptions(context.Background()); err != nil || len(subs) != 0 {
		t.Errorf("subscriptions after 410 = %+v, %v", subs, err)
	}
}

// A subscription stops receiving as soon as the token that registered it is removed from
// the configuration or loses the read scope (docs/16).
func TestServeWebPushStopsForRevokedTokens(t *testing.T) {
	pushed := make(chan string, 16)
	service := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pushed <- r.URL.Path
		w.WriteHeader(http.StatusCreated)
	}))
	defer service.Close()
	webPushTransport = service.Client().Transport
	defer func() { webPushTransport = nil }()

	tokens := func(entries ...string) string {
		out := "  tokens:"
		for _, e := range entries {
			name, scopes, _ := strings.Cut(e, ":")
			out += fmt.Sprintf("\n    - name: %q\n      token: %q\n      scopes: [%s]", name, "tok-"+name, scopes)
		}
		return out
	}
	dir := t.TempDir()
	base, stop := startPushServeWithAuth(t, dir, true,
		tokens("removed:read, operate", "downgraded:read, operate", "kept:read, operate, register"))
	for _, name := range []string{"removed", "downgraded", "kept"} {
		private, err := ecdh.P256().GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		sub := fmt.Sprintf(`{"endpoint":%q,"keys":{"p256dh":%q,"auth":%q}}`, service.URL+"/"+name,
			base64.RawURLEncoding.EncodeToString(private.PublicKey().Bytes()),
			base64.RawURLEncoding.EncodeToString(make([]byte, 16)))
		if code, body := serveCallAs(t, "tok-"+name, "POST", base+"/push/subscriptions", sub); code != http.StatusCreated {
			t.Fatalf("register %s = %d %s", name, code, body)
		}
	}
	stop()

	base, stop = startPushServeWithAuth(t, dir, true,
		tokens("downgraded:operate", "kept:read, operate, register"))
	if code, body := serveCallAs(t, "tok-kept", "POST", base+"/jobs",
		`{"request_id":"revoke-1","title":"Revoke","acceptance_criteria":"build ok"}`); code != http.StatusCreated {
		t.Fatalf("create job = %d %s", code, body)
	}
	select {
	case path := <-pushed:
		pushed <- path
	case <-time.After(10 * time.Second):
		t.Fatal("no push for the finished job")
	}
	// Stopping waits for the delivery to every subscription of the job.
	stop()
	var got []string
	for len(pushed) > 0 {
		got = append(got, <-pushed)
	}
	if len(got) != 1 || got[0] != "/kept" {
		t.Errorf("pushed to %v, want only /kept", got)
	}
}

func TestServeWebPushOffByDefault(t *testing.T) {
	dir := t.TempDir()
	base, stop := startPushServe(t, dir, false)
	if code, body := serveCall(t, "GET", base+"/push/vapid-public-key", ""); code != http.StatusNotFound {
		t.Errorf("public key with web_push off = %d %s, want 404", code, body)
	}
	stop()
	if _, err := os.Stat(filepath.Join(dir, "state", "vapid.json")); !os.IsNotExist(err) {
		t.Errorf("key file exists with web_push off: %v", err)
	}
}

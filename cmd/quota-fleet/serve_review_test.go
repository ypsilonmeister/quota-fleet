package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// serve wires evaluator.api_key and review_gate into the API without printing the key.
// The review endpoint is only called with the gate disabled, so no Decisions API request
// is made.
func TestServeReviewGateWiringDoesNotExposeAPIKey(t *testing.T) {
	const key = "FAKE-YAML-KEY-do-not-leak-0123456789"
	for _, tc := range []struct {
		name        string
		extra       string
		wantMeta    string
		wantWarning bool
	}{
		{"enabled with api_key", "evaluator:\n  api_key: " + key + "\nreview_gate:\n  enabled: true\n",
			`"review_gate":{"enabled":true,"model":"gemini-decision-1","key_configured":true}`, false},
		{"disabled with api_key", "evaluator:\n  api_key: " + key + "\n",
			`"review_gate":{"enabled":false,"model":"gemini-decision-1","key_configured":true}`, false},
		{"enabled without key", "review_gate:\n  enabled: true\n",
			`"review_gate":{"enabled":true,"model":"gemini-decision-1","key_configured":false}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GEMINI_API_KEY", "")
			l, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatalf("listen: %v", err)
			}
			addr := l.Addr().String()
			_ = l.Close()

			dir := t.TempDir()
			cfgPath := filepath.Join(dir, "fleet.yaml")
			cfg := fmt.Sprintf("instance_id: review-wiring\nstate_dir: %q\napi:\n  token: tok\n  listen: %q\n%s", dir, addr, tc.extra)
			if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var out, errBuf bytes.Buffer
			done := make(chan int, 1)
			go func() { done <- runServeContext(ctx, []string{"--config", cfgPath}, &out, &errBuf) }()

			client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
			call := func(method, path, body string) (int, string, error) {
				req, err := http.NewRequest(method, "http://"+addr+path, strings.NewReader(body))
				if err != nil {
					return 0, "", err
				}
				req.Header.Set("Authorization", "Bearer tok")
				resp, err := client.Do(req)
				if err != nil {
					return 0, "", err
				}
				defer resp.Body.Close()
				b, err := io.ReadAll(resp.Body)
				return resp.StatusCode, string(b), err
			}

			var meta string
			for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
				status, body, err := call(http.MethodGet, "/meta", "")
				if err == nil && status == http.StatusOK {
					meta = body
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("serve did not answer /meta: status=%d err=%v", status, err)
				}
			}
			if !strings.Contains(meta, tc.wantMeta) {
				t.Errorf("/meta = %s, want %s", meta, tc.wantMeta)
			}
			gateEnabled := strings.Contains(tc.wantMeta, `"enabled":true`)
			var verdictBody string
			if !gateEnabled || tc.wantWarning {
				status, body, err := call(http.MethodPost, "/decision/review-verdict", `{"text":"x"}`)
				if err != nil || status != http.StatusServiceUnavailable {
					t.Errorf("review-verdict status = %d err = %v body = %s, want 503", status, err, body)
				}
				verdictBody = body
			}

			cancel()
			select {
			case code := <-done:
				if code != 0 {
					t.Fatalf("serve exit code = %d; stderr=%s", code, errBuf.String())
				}
			case <-time.After(10 * time.Second):
				t.Fatalf("serve did not shut down")
			}
			output := out.String() + errBuf.String()
			if got := strings.Contains(output, "no Decision API key is configured"); got != tc.wantWarning {
				t.Errorf("review gate warning present = %v, want %v:\n%s", got, tc.wantWarning, output)
			}
			if got := strings.Contains(output, "free-form acceptance criteria"); got != tc.wantWarning {
				t.Errorf("evaluator key warning present = %v, want %v:\n%s", got, tc.wantWarning, output)
			}
			for what, content := range map[string]string{"/meta": meta, "review-verdict": verdictBody, "serve output": output} {
				if strings.Contains(content, key[:12]) {
					t.Errorf("%s exposes the API key: %s", what, content)
				}
			}
		})
	}
}

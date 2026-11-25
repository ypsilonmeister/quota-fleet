package api_test

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ypsilonmeister/quota-fleet/internal/api"
	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/push"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

const testVAPIDPublicKey = "test-vapid-public-key"

func newPushAPI(t *testing.T, publicKey string, withTokens bool) (*httptest.Server, *store.Store) {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "fleet.db"), store.Options{InstanceID: "push"})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	opts := api.Options{InstanceID: "push", WebPushPublicKey: publicKey}
	if withTokens {
		for _, scope := range config.AllAPIScopes {
			opts.Tokens = append(opts.Tokens, config.APITokenConfig{Name: scope, Token: "tok-" + scope, Scopes: []string{scope}})
		}
		opts.Tokens = append(opts.Tokens, config.APITokenConfig{Name: "subscriber", Token: "tok-subscriber",
			Scopes: []string{config.ScopeRead, config.ScopeOperate}})
		opts.Token = "tok-all"
	}
	ts := httptest.NewServer(api.NewServer(st, opts).Handler())
	t.Cleanup(ts.Close)
	return ts, st
}

func pushCall(t *testing.T, ts *httptest.Server, method, path, token, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("User-Agent", "test-browser/1.0")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, strings.TrimSpace(string(data))
}

func subscriptionJSON(t *testing.T, endpoint string) string {
	t.Helper()
	private, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	// The shape of PushSubscription.toJSON().
	data, err := json.Marshal(map[string]any{
		"endpoint":       endpoint,
		"expirationTime": nil,
		"keys": map[string]string{
			"p256dh": base64.RawURLEncoding.EncodeToString(private.PublicKey().Bytes()),
			"auth":   base64.RawURLEncoding.EncodeToString(make([]byte, 16)),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestPushSubscribeAndUnsubscribe(t *testing.T) {
	ts, st := newPushAPI(t, testVAPIDPublicKey, true)
	code, body := pushCall(t, ts, "GET", "/push/vapid-public-key", "tok-read", "")
	if code != http.StatusOK || body != `{"public_key":"`+testVAPIDPublicKey+`"}` {
		t.Fatalf("public key = %d %s", code, body)
	}

	const endpoint = "https://push.example/s/1"
	sub := subscriptionJSON(t, endpoint)
	if code, body = pushCall(t, ts, "POST", "/push/subscriptions", "tok-subscriber", sub); code != http.StatusCreated || body != `{"created":true}` {
		t.Fatalf("register = %d %s", code, body)
	}
	if code, body = pushCall(t, ts, "POST", "/push/subscriptions", "tok-subscriber", sub); code != http.StatusOK || body != `{"created":false}` {
		t.Fatalf("register again = %d %s", code, body)
	}
	stored, err := st.ListPushSubscriptions(context.Background())
	if err != nil || len(stored) != 1 || stored[0].Endpoint != endpoint || stored[0].UserAgent != "test-browser/1.0" ||
		stored[0].TokenID != push.TokenID("tok-subscriber") {
		t.Fatalf("stored = %+v, %v", stored, err)
	}
	// Registering again with another token moves the subscription to that token.
	if code, body = pushCall(t, ts, "POST", "/push/subscriptions", "tok-all", sub); code != http.StatusOK {
		t.Fatalf("register with another token = %d %s", code, body)
	}
	if stored, err = st.ListPushSubscriptions(context.Background()); err != nil || len(stored) != 1 ||
		stored[0].TokenID != push.TokenID("tok-all") {
		t.Fatalf("stored = %+v, %v", stored, err)
	}

	del := `{"endpoint":"` + endpoint + `"}`
	if code, body = pushCall(t, ts, "DELETE", "/push/subscriptions", "tok-operate", del); code != http.StatusOK || body != `{"deleted":true}` {
		t.Fatalf("delete = %d %s", code, body)
	}
	if code, body = pushCall(t, ts, "DELETE", "/push/subscriptions", "tok-operate", del); code != http.StatusOK || body != `{"deleted":false}` {
		t.Fatalf("delete again = %d %s", code, body)
	}
	if stored, err = st.ListPushSubscriptions(context.Background()); err != nil || len(stored) != 0 {
		t.Fatalf("stored after delete = %+v, %v", stored, err)
	}

	for _, bad := range []string{"", "not json", "{}", `{"endpoint":"https://push.example/s"}`,
		subscriptionJSON(t, "http://push.example/s")} {
		if code, body = pushCall(t, ts, "POST", "/push/subscriptions", "tok-subscriber", bad); code != http.StatusBadRequest {
			t.Errorf("register %q = %d %s, want 400", bad, code, body)
		}
	}
	for _, bad := range []string{"", "{}", `{"endpoint":""}`} {
		if code, body = pushCall(t, ts, "DELETE", "/push/subscriptions", "tok-operate", bad); code != http.StatusBadRequest {
			t.Errorf("delete %q = %d %s, want 400", bad, code, body)
		}
	}
}

func TestPushScopes(t *testing.T) {
	ts, st := newPushAPI(t, testVAPIDPublicKey, true)
	sub := subscriptionJSON(t, "https://push.example/s/1")
	for _, route := range []struct{ method, path, scope, body string }{
		{"GET", "/push/vapid-public-key", config.ScopeRead, ""},
		{"POST", "/push/subscriptions", config.ScopeOperate, sub},
		{"DELETE", "/push/subscriptions", config.ScopeOperate, `{"endpoint":"https://push.example/s/1"}`},
	} {
		for _, token := range []string{"", "wrong"} {
			if code, _ := pushCall(t, ts, route.method, route.path, token, route.body); code != http.StatusUnauthorized {
				t.Errorf("%s %s token %q = %d, want 401", route.method, route.path, token, code)
			}
		}
		if route.method == "POST" {
			// Registering needs read as well as operate.
			code, body := pushCall(t, ts, route.method, route.path, "tok-operate", route.body)
			if code != http.StatusForbidden || !strings.Contains(body, `"required_scope":"read"`) {
				t.Errorf("register with operate only = %d %s, want 403", code, body)
			}
		}
		for _, scope := range config.AllAPIScopes {
			if scope == route.scope {
				continue
			}
			code, body := pushCall(t, ts, route.method, route.path, "tok-"+scope, route.body)
			if code != http.StatusForbidden || !strings.Contains(body, `"required_scope":"`+route.scope+`"`) {
				t.Errorf("%s %s with %s = %d %s, want 403", route.method, route.path, scope, code, body)
			}
		}
		if code, body := pushCall(t, ts, route.method, route.path, "tok-all", route.body); code >= 300 {
			t.Errorf("%s %s with the legacy token = %d %s", route.method, route.path, code, body)
		}
	}
	if stored, err := st.ListPushSubscriptions(context.Background()); err != nil || len(stored) != 0 {
		t.Errorf("stored = %+v, %v", stored, err)
	}
}

func TestPushNeedsTokenAndIsOffByDefault(t *testing.T) {
	routes := [][2]string{{"GET", "/push/vapid-public-key"}, {"POST", "/push/subscriptions"}, {"DELETE", "/push/subscriptions"}}
	// Without configured tokens the JSON API is open, but /push is not.
	open, st := newPushAPI(t, testVAPIDPublicKey, false)
	for _, r := range routes {
		code, _ := pushCall(t, open, r[0], r[1], "", subscriptionJSON(t, "https://push.example/anon"))
		if code != http.StatusUnauthorized {
			t.Errorf("anonymous %s %s = %d, want 401", r[0], r[1], code)
		}
	}
	if stored, err := st.ListPushSubscriptions(context.Background()); err != nil || len(stored) != 0 {
		t.Errorf("anonymous registration stored %+v, %v", stored, err)
	}
	off, _ := newPushAPI(t, "", true)
	for _, r := range routes {
		// While disabled these are unknown paths, so authentication comes first.
		for _, token := range []string{"", "wrong"} {
			if code, _ := pushCall(t, off, r[0], r[1], token, "{}"); code != http.StatusUnauthorized {
				t.Errorf("disabled %s %s token %q = %d, want 401", r[0], r[1], token, code)
			}
		}
		if code, _ := pushCall(t, off, r[0], r[1], "tok-all", "{}"); code != http.StatusNotFound {
			t.Errorf("disabled %s %s = %d, want 404", r[0], r[1], code)
		}
	}
	// A server that is open because it has no tokens answers 404 to anyone.
	offOpen, _ := newPushAPI(t, "", false)
	for _, r := range routes {
		if code, _ := pushCall(t, offOpen, r[0], r[1], "", "{}"); code != http.StatusNotFound {
			t.Errorf("disabled, no tokens: %s %s = %d, want 404", r[0], r[1], code)
		}
	}
}

// The registration is on its way before the removal, and the server reads it afterwards.
func TestPushLateRegistrationDoesNotRestoreDeletedSubscription(t *testing.T) {
	for name, registeredBefore := range map[string]bool{"registering again": true, "first registration": false} {
		t.Run(name, func(t *testing.T) {
			ts, st := newPushAPI(t, testVAPIDPublicKey, true)
			const endpoint = "https://push.example/s/late"
			sub := subscriptionJSON(t, endpoint)
			if registeredBefore {
				if code, body := pushCall(t, ts, "POST", "/push/subscriptions", "tok-subscriber", sub); code != http.StatusCreated {
					t.Fatalf("register = %d %s", code, body)
				}
			}

			reader, writer := io.Pipe()
			defer writer.Close()
			req, err := http.NewRequest("POST", ts.URL+"/push/subscriptions", reader)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer tok-subscriber")
			type answer struct {
				code int
				err  error
			}
			late := make(chan answer, 1)
			go func() {
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					late <- answer{err: err}
					return
				}
				resp.Body.Close()
				late <- answer{code: resp.StatusCode}
			}()
			if _, err := io.WriteString(writer, sub[:1]); err != nil {
				t.Fatalf("send the start of the registration: %v", err)
			}

			code, body := pushCall(t, ts, "DELETE", "/push/subscriptions", "tok-subscriber", `{"endpoint":"`+endpoint+`"}`)
			if code != http.StatusOK {
				t.Fatalf("delete = %d %s", code, body)
			}
			if _, err := io.WriteString(writer, sub[1:]); err != nil {
				t.Fatalf("send the rest of the registration: %v", err)
			}
			writer.Close()

			if got := <-late; got.err != nil || got.code != http.StatusGone {
				t.Errorf("late registration = %d, %v, want 410", got.code, got.err)
			}
			if stored, err := st.ListPushSubscriptions(context.Background()); err != nil || len(stored) != 0 {
				t.Errorf("stored = %+v, %v, want none", stored, err)
			}
		})
	}
}

package push

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

// browser holds the receiving side of one subscription.
type browser struct {
	private *ecdh.PrivateKey
	secret  []byte
}

type pushRequest struct {
	path   string
	method string
	header http.Header
	body   []byte
}

// pushService is a fake push service. status maps a path to the response code.
type pushService struct {
	*httptest.Server
	requests chan pushRequest
	mu       sync.Mutex
	status   map[string]int
}

func newPushService(t *testing.T) *pushService {
	t.Helper()
	p := &pushService{requests: make(chan pushRequest, 64), status: map[string]int{}}
	p.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		p.mu.Lock()
		code := p.status[r.URL.Path]
		p.mu.Unlock()
		p.requests <- pushRequest{path: r.URL.Path, method: r.Method, header: r.Header.Clone(), body: body}
		if code == 0 {
			code = http.StatusCreated
		}
		if code >= 300 && code < 400 {
			w.Header().Set("Location", "/redirected")
		}
		w.WriteHeader(code)
	}))
	t.Cleanup(p.Close)
	return p
}

func (p *pushService) answer(path string, code int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.status[path] = code
}

func (p *pushService) next(t *testing.T) pushRequest {
	t.Helper()
	select {
	case r := <-p.requests:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("no push request")
		return pushRequest{}
	}
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// readerToken is the API token the fixture's subscriptions are registered with.
const readerToken = "reader-token"

type fixture struct {
	st     *store.Store
	keys   VAPIDKeys
	logs   *syncBuffer
	sender *Sender
}

func newFixture(t *testing.T, opts Options) *fixture {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(context.Background(), filepath.Join(dir, "fleet.db"), store.Options{InstanceID: "push"})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	keys, _, err := LoadOrCreateVAPIDKeys(filepath.Join(dir, KeyFileName))
	if err != nil {
		t.Fatalf("keys: %v", err)
	}
	logs := &syncBuffer{}
	opts.Keys = keys
	opts.Logger = log.New(logs, "", 0)
	if opts.Subject == "" {
		opts.Subject = "mailto:ops@fleet.example"
	}
	if opts.ReadTokenIDs == nil {
		opts.ReadTokenIDs = []string{TokenID(readerToken)}
	}
	sender := Start(st, opts)
	t.Cleanup(sender.Close)
	return &fixture{st: st, keys: keys, logs: logs, sender: sender}
}

func (f *fixture) subscribe(t *testing.T, endpoint string) browser {
	t.Helper()
	private, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	secret := make([]byte, 16)
	_, _ = rand.Read(secret)
	sub, err := NewSubscription(endpoint,
		base64.RawURLEncoding.EncodeToString(private.PublicKey().Bytes()),
		base64.RawURLEncoding.EncodeToString(secret), "test")
	if err != nil {
		t.Fatalf("NewSubscription: %v", err)
	}
	sub.TokenID = TokenID(readerToken)
	if _, err := f.st.UpsertPushSubscription(context.Background(), sub); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	return browser{private: private, secret: secret}
}

func (f *fixture) claim(t *testing.T, requestID, title string) (int64, string) {
	t.Helper()
	ctx := context.Background()
	job, _, err := f.st.CreateJob(ctx, store.CreateJobParams{RequestID: requestID, Title: title})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	claimed, ok, err := f.st.ClaimByID(ctx, job.ID, 100)
	if err != nil || !ok {
		t.Fatalf("ClaimByID: %v %v", ok, err)
	}
	return job.ID, claimed.ClaimedBy
}

func (f *fixture) endpoints(t *testing.T) []string {
	t.Helper()
	subs, err := f.st.ListPushSubscriptions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, s := range subs {
		out = append(out, s.Endpoint)
	}
	return out
}

// decrypt is the receiving half of RFC 8291 (aes128gcm, one record).
func (b browser) decrypt(t *testing.T, body []byte) Payload {
	t.Helper()
	if len(body) < 21 {
		t.Fatalf("body is %d bytes", len(body))
	}
	salt, recordSize, keyLen := body[:16], binary.BigEndian.Uint32(body[16:20]), int(body[20])
	senderKey := body[21 : 21+keyLen]
	ciphertext := body[21+keyLen:]
	if len(body) > int(recordSize) || len(body) > 4096 {
		t.Fatalf("message of %d bytes exceeds the record size %d", len(body), recordSize)
	}
	public, err := ecdh.P256().NewPublicKey(senderKey)
	if err != nil {
		t.Fatal(err)
	}
	shared, err := b.private.ECDH(public)
	if err != nil {
		t.Fatal(err)
	}
	derive := func(secret, salt []byte, info string, n int) []byte {
		out, err := hkdf.Key(sha256.New, secret, salt, info, n)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	ikm := derive(shared, b.secret, "WebPush: info\x00"+string(b.private.PublicKey().Bytes())+string(senderKey), 32)
	block, err := aes.NewCipher(derive(ikm, salt, "Content-Encoding: aes128gcm\x00", 16))
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := gcm.Open(nil, derive(ikm, salt, "Content-Encoding: nonce\x00", 12), ciphertext, nil)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	plain = bytes.TrimRight(plain, "\x00")
	if len(plain) == 0 || plain[len(plain)-1] != 2 {
		t.Fatal("missing record delimiter")
	}
	var members map[string]any
	if err := json.Unmarshal(plain[:len(plain)-1], &members); err != nil {
		t.Fatalf("payload: %v", err)
	}
	for _, name := range []string{"title", "body", "job_id", "url", "status"} {
		if _, ok := members[name]; !ok {
			t.Errorf("payload has no %q", name)
		}
	}
	var payload Payload
	if err := json.Unmarshal(plain[:len(plain)-1], &payload); err != nil {
		t.Fatal(err)
	}
	return payload
}

// assertVAPID checks the Authorization header against the server's public key.
func assertVAPID(t *testing.T, header string, keys VAPIDKeys, audience, subject string) {
	t.Helper()
	rest, ok := strings.CutPrefix(header, "vapid t=")
	token, key, found := strings.Cut(rest, ", k=")
	if !ok || !found || key != keys.PublicKey {
		t.Fatalf("Authorization = %q", header)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d parts", len(parts))
	}
	public, _ := base64.RawURLEncoding.DecodeString(keys.PublicKey)
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(signature) != 64 {
		t.Fatalf("signature: %d bytes, %v", len(signature), err)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	ecdsaKey := &ecdsa.PublicKey{Curve: elliptic.P256(),
		X: new(big.Int).SetBytes(public[1:33]), Y: new(big.Int).SetBytes(public[33:])}
	if !ecdsa.Verify(ecdsaKey, digest[:], new(big.Int).SetBytes(signature[:32]), new(big.Int).SetBytes(signature[32:])) {
		t.Error("VAPID signature does not verify")
	}
	claimsJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims struct {
		Aud, Sub string
		Exp      int64
	}
	if err := json.Unmarshal(claimsJSON, &claims); err != nil {
		t.Fatal(err)
	}
	if claims.Aud != audience || claims.Sub != subject {
		t.Errorf("claims = %+v, want aud %q sub %q", claims, audience, subject)
	}
	if left := time.Until(time.Unix(claims.Exp, 0)); left <= 0 || left > 24*time.Hour {
		t.Errorf("token expires in %v", left)
	}
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestSenderPushesDoneAndFailed(t *testing.T) {
	service := newPushService(t)
	for _, subject := range []string{"mailto:ops@fleet.example", "https://fleet.example/contact"} {
		f := newFixture(t, Options{Transport: service.Client().Transport, Subject: subject})
		ctx := context.Background()
		receivers := map[string]browser{
			"/a": f.subscribe(t, service.URL+"/a"),
			"/b": f.subscribe(t, service.URL+"/b"),
		}
		receive := func() map[string]Payload {
			got := map[string]Payload{}
			for range receivers {
				r := service.next(t)
				if r.method != http.MethodPost || r.header.Get("Content-Encoding") != "aes128gcm" || r.header.Get("TTL") != "86400" {
					t.Errorf("request = %s %v", r.method, r.header)
				}
				assertVAPID(t, r.header.Get("Authorization"), f.keys, service.URL, subject)
				got[r.path] = receivers[r.path].decrypt(t, r.body)
			}
			return got
		}

		id, token := f.claim(t, "done-1", "Build the thing")
		if _, err := f.st.Complete(ctx, id, token, "SECRET-result"); err != nil {
			t.Fatalf("Complete: %v", err)
		}
		want := Payload{Title: "ジョブ #1 完了: Build the thing", Body: "状態: done（完了）",
			JobID: id, URL: "/ui/jobs/1", Status: "done"}
		if got := receive(); got["/a"] != want || got["/b"] != want {
			t.Errorf("done payloads = %+v, want %+v", got, want)
		}

		id, token = f.claim(t, "failed-1", "Break the thing")
		if _, err := f.st.FailWithResult(ctx, id, token, "SECRET-error", "SECRET-output"); err != nil {
			t.Fatalf("Fail: %v", err)
		}
		want = Payload{Title: "ジョブ #2 失敗: Break the thing", Body: "状態: failed（失敗）",
			JobID: id, URL: "/ui/jobs/2", Status: "failed"}
		if got := receive(); got["/a"] != want {
			t.Errorf("failed payload = %+v, want %+v", got["/a"], want)
		}
	}
}

func TestSenderSendsOnlyForReadTokens(t *testing.T) {
	for name, allowed := range map[string][]string{
		"current tokens": {TokenID(readerToken)},
		"no tokens":      {},
		// An empty ID must not admit the subscriptions that belong to no token.
		"empty id": {""},
	} {
		t.Run(name, func(t *testing.T) {
			service := newPushService(t)
			f := newFixture(t, Options{Transport: service.Client().Transport, ReadTokenIDs: allowed})
			ctx := context.Background()
			f.subscribe(t, service.URL+"/reader")
			for path, tokenID := range map[string]string{"/revoked": TokenID("revoked-token"), "/unbound": ""} {
				f.subscribe(t, service.URL+path)
				subs, err := f.st.ListPushSubscriptions(ctx)
				if err != nil {
					t.Fatal(err)
				}
				for _, s := range subs {
					if s.Endpoint == service.URL+path {
						s.TokenID = tokenID
						if _, err := f.st.UpsertPushSubscription(ctx, s); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			id, token := f.claim(t, "j", "job")
			if _, err := f.st.Complete(ctx, id, token, "ok"); err != nil {
				t.Fatal(err)
			}
			f.sender.Close()
			var got []string
			for len(service.requests) > 0 {
				got = append(got, (<-service.requests).path)
			}
			want := ""
			if name == "current tokens" {
				want = "/reader"
			}
			if strings.Join(got, ",") != want {
				t.Errorf("pushed to %v, want %q", got, want)
			}
			// Excluded subscriptions are kept.
			if n := len(f.endpoints(t)); n != 3 {
				t.Errorf("%d subscriptions left, want 3", n)
			}
		})
	}
}

func TestSenderReportsExcludedSubscriptionsAtStart(t *testing.T) {
	f := newFixture(t, Options{})
	f.subscribe(t, "https://push.example/secret-capability")
	logs := &syncBuffer{}
	Start(f.st, Options{Keys: f.keys, Logger: log.New(logs, "", 0)}).Close()
	if got := logs.String(); !strings.Contains(got, "1 subscription(s) receive nothing") || strings.Contains(got, "secret-capability") {
		t.Errorf("log = %q", got)
	}
}

func TestSenderIgnoresOtherTransitions(t *testing.T) {
	service := newPushService(t)
	f := newFixture(t, Options{Transport: service.Client().Transport})
	ctx := context.Background()
	receiver := f.subscribe(t, service.URL+"/a")

	// Creating, claiming, requeueing and cancelling push nothing.
	id, token := f.claim(t, "reset", "reset")
	if _, err := f.st.Reset(ctx, id); err != nil {
		t.Fatalf("Reset: %v (%s)", err, token)
	}
	if _, err := f.st.Cancel(ctx, id, "not needed"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	// A failing parent cancels its child; only the parent is pushed.
	parent, token := f.claim(t, "parent", "parent")
	if _, _, err := f.st.CreateJob(ctx, store.CreateJobParams{RequestID: "child", Title: "child", ParentID: &parent}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.Fail(ctx, parent, token, "boom"); err != nil {
		t.Fatalf("Fail: %v", err)
	}
	if got := receiver.decrypt(t, service.next(t).body); got.JobID != parent {
		t.Errorf("first push is for job %d, want %d", got.JobID, parent)
	}
	// Events are processed in order, so anything pushed wrongly would be here by now.
	f.sender.Close()
	if n := len(service.requests); n != 0 {
		t.Errorf("%d unexpected push requests", n)
	}
}

func TestSenderDeletesGoneSubscriptions(t *testing.T) {
	service := newPushService(t)
	f := newFixture(t, Options{Transport: service.Client().Transport})
	codes := map[string]int{"/ok": 201, "/gone": 410, "/missing": 404, "/error": 500,
		"/busy": 429, "/denied": 403, "/moved": 307}
	for path, code := range codes {
		service.answer(path, code)
		f.subscribe(t, service.URL+path)
	}
	id, token := f.claim(t, "j", "job")
	if _, err := f.st.Complete(context.Background(), id, token, "ok"); err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for range codes {
		seen[service.next(t).path]++
	}
	f.sender.Close()
	if len(seen) != len(codes) || len(service.requests) != 0 {
		t.Errorf("requests = %v and %d more, want one per subscription and no redirect", seen, len(service.requests))
	}
	got := map[string]bool{}
	for _, e := range f.endpoints(t) {
		got[strings.TrimPrefix(e, service.URL)] = true
	}
	for path := range codes {
		wantKept := path != "/gone" && path != "/missing"
		if got[path] != wantKept {
			t.Errorf("%s kept = %v, want %v", path, got[path], wantKept)
		}
	}
	logs := f.logs.String()
	for _, path := range []string{"/error", "/busy", "/denied", "/ok", "/gone"} {
		if strings.Contains(logs, service.URL+path) || strings.Contains(logs, path) {
			t.Errorf("log reveals the endpoint %s:\n%s", path, logs)
		}
	}
	for _, code := range []string{"HTTP 500", "HTTP 429", "HTTP 403", "HTTP 307", "HTTP 410", "HTTP 404"} {
		if !strings.Contains(logs, code) {
			t.Errorf("log lacks %q:\n%s", code, logs)
		}
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSenderFailureIsLoggedWithoutEndpoint(t *testing.T) {
	f := newFixture(t, Options{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("connection refused")
	})})
	f.subscribe(t, "https://push.example/secret-capability")
	id, token := f.claim(t, "j", "job")
	if _, err := f.st.Complete(context.Background(), id, token, "ok"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "a log line", func() bool { return strings.Contains(f.logs.String(), "connection refused") })
	if strings.Contains(f.logs.String(), "secret-capability") {
		t.Errorf("log reveals the endpoint:\n%s", f.logs.String())
	}
	if len(f.endpoints(t)) != 1 {
		t.Errorf("a network failure deleted the subscription")
	}
}

func TestSenderNeverBlocksTransitions(t *testing.T) {
	started := make(chan struct{}, 1024)
	f := newFixture(t, Options{
		ShutdownGrace: 50 * time.Millisecond,
		SendTimeout:   time.Minute,
		// A push service that never answers.
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			started <- struct{}{}
			<-r.Context().Done()
			return nil, r.Context().Err()
		}),
	})
	f.subscribe(t, "https://push.example/hang")
	ctx := context.Background()
	start := time.Now()
	// More finished jobs than the queue and the event buffer hold together.
	for i := range queueSize + 300 {
		id, token := f.claim(t, "j"+string(rune('a'+i%26))+strings.Repeat("x", i/26), "job")
		if _, err := f.st.Complete(ctx, id, token, "ok"); err != nil {
			t.Fatalf("Complete %d: %v", i, err)
		}
	}
	if took := time.Since(start); took > 30*time.Second {
		t.Fatalf("transitions took %v with a hanging push service", took)
	}
	<-started
	eventually(t, "the full queue warning", func() bool { return strings.Contains(f.logs.String(), "queue is full") })
	closing := time.Now()
	f.sender.Close()
	f.sender.Close()
	if took := time.Since(closing); took > 5*time.Second {
		t.Errorf("Close took %v", took)
	}
}

func TestSenderClosePushesPendingAndSurvivesEventShutdown(t *testing.T) {
	service := newPushService(t)
	f := newFixture(t, Options{Transport: service.Client().Transport})
	f.subscribe(t, service.URL+"/a")
	id, token := f.claim(t, "j", "job")
	if _, err := f.st.Complete(context.Background(), id, token, "ok"); err != nil {
		t.Fatal(err)
	}
	// What serve does on shutdown: close the event streams, then the sender.
	f.st.CloseJobEvents()
	f.sender.Close()
	if n := len(service.requests); n != 1 {
		t.Errorf("%d push requests, want the pending one", n)
	}
	if strings.Contains(f.logs.String(), "fell behind") {
		t.Errorf("a shutdown was taken for falling behind:\n%s", f.logs.String())
	}
}

// A subscription may be removed or replaced while it waits for a free request slot; what
// is stored when the slot frees decides whether, and with which keys, it is sent.
func TestSenderRechecksSubscriptionsWaitingForSlot(t *testing.T) {
	for _, change := range []string{"deleted", "moved to a token without read", "new keys"} {
		t.Run(change, func(t *testing.T) {
			requests := make(chan pushRequest, concurrency+1)
			release := make(chan struct{})
			// A push service that holds every answer until released.
			f := newFixture(t, Options{SendTimeout: time.Minute, Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					return nil, err
				}
				requests <- pushRequest{path: r.URL.Path, body: body}
				select {
				case <-release:
					return &http.Response{StatusCode: http.StatusCreated, Body: http.NoBody}, nil
				case <-r.Context().Done():
					return nil, r.Context().Err()
				}
			})})
			open := sync.OnceFunc(func() { close(release) })
			// Runs before the fixture closes the sender.
			t.Cleanup(open)
			ctx := context.Background()
			waiting := map[string]bool{}
			for i := range concurrency + 1 {
				path := fmt.Sprintf("/%02d", i)
				f.subscribe(t, "https://push.example"+path)
				waiting[path] = true
			}
			id, token := f.claim(t, "j", "job")
			if _, err := f.st.Complete(ctx, id, token, "ok"); err != nil {
				t.Fatal(err)
			}
			// Every slot is now taken, and one subscription has had no request.
			for range concurrency {
				select {
				case r := <-requests:
					delete(waiting, r.path)
				case <-time.After(5 * time.Second):
					t.Fatal("no push request")
				}
			}
			if len(waiting) != 1 || len(requests) != 0 {
				t.Fatalf("waiting = %v with %d more requests, want one subscription waiting", waiting, len(requests))
			}
			var path string
			for p := range waiting {
				path = p
			}
			endpoint := "https://push.example" + path

			var renewed browser
			switch change {
			case "deleted":
				if deleted, err := f.st.DeletePushSubscription(ctx, endpoint); err != nil || !deleted {
					t.Fatalf("delete = %v, %v", deleted, err)
				}
			case "moved to a token without read":
				subs, err := f.st.ListPushSubscriptions(ctx)
				if err != nil {
					t.Fatal(err)
				}
				for _, sub := range subs {
					if sub.Endpoint == endpoint {
						sub.TokenID = TokenID("operate-only-token")
						if _, err := f.st.UpsertPushSubscription(ctx, sub); err != nil {
							t.Fatal(err)
						}
					}
				}
			case "new keys":
				renewed = f.subscribe(t, endpoint)
			}
			open()
			f.sender.Close()

			if change != "new keys" {
				if len(requests) != 0 {
					t.Errorf("new request to %s after the subscription was %s", (<-requests).path, change)
				}
				return
			}
			if len(requests) != 1 {
				t.Fatalf("%d requests after release, want 1", len(requests))
			}
			if r := <-requests; r.path != path || renewed.decrypt(t, r.body).JobID != id {
				t.Errorf("request to %s, want job %d to %s", r.path, id, path)
			}
		})
	}
}

func TestPayloadIsBounded(t *testing.T) {
	long := strings.Repeat("<&>長い題名\x00\n", 2000)
	p := newPayload(store.JobEvent{JobID: 9223372036854775807, Status: store.StatusDone, Title: long})
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	// webpush-go rejects more than 3993 bytes.
	if len(data) > 3000 {
		t.Errorf("payload is %d bytes", len(data))
	}
	if !strings.HasSuffix(p.Title, "…") {
		t.Errorf("title is not cut: %q", p.Title)
	}
	empty := newPayload(store.JobEvent{JobID: 1, Status: store.StatusFailed, Title: " \n "})
	if empty.Title != "ジョブ #1 失敗: (無題)" || empty.Body != "状態: failed（失敗）" {
		t.Errorf("empty payload = %+v", empty)
	}
	if got := excerpt("abcde", 5); got != "abcde" {
		t.Errorf("excerpt at the limit = %q", got)
	}
}

func TestNewSubscriptionValidation(t *testing.T) {
	private, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	public := base64.RawURLEncoding.EncodeToString(private.PublicKey().Bytes())
	secret := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0xfb}, 16))
	const endpoint = "https://push.example/s/abc"

	padded := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0xfb}, 16))
	sub, err := NewSubscription(endpoint, public+"=", padded, strings.Repeat("あ", 600))
	if err != nil {
		t.Fatalf("valid subscription: %v", err)
	}
	if sub.P256dh != public || sub.Auth != secret || utf8.RuneCountInString(sub.UserAgent) != maxUserAgentRunes {
		t.Errorf("not normalized: %+v", sub)
	}

	offCurve := make([]byte, 65)
	offCurve[0] = 4
	for name, in := range map[string][3]string{
		"empty endpoint": {"", public, secret},
		"http":           {"http://push.example/s", public, secret},
		"relative":       {"/s/abc", public, secret},
		"no host":        {"https:///s", public, secret},
		"userinfo":       {"https://u:p@push.example/s", public, secret},
		"too long":       {endpoint + strings.Repeat("a", maxEndpointBytes), public, secret},
		"no p256dh":      {endpoint, "", secret},
		"bad p256dh":     {endpoint, "!!!", secret},
		"off curve":      {endpoint, base64.RawURLEncoding.EncodeToString(offCurve), secret},
		"short auth":     {endpoint, public, base64.RawURLEncoding.EncodeToString([]byte("short"))},
		"no auth":        {endpoint, public, ""},
	} {
		_, err := NewSubscription(in[0], in[1], in[2], "")
		if !errors.Is(err, store.ErrInvalidInput) {
			t.Errorf("%s: err = %v, want ErrInvalidInput", name, err)
		}
	}
}

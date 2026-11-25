package push

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode"

	webpush "github.com/SherClockHolmes/webpush-go"

	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

const (
	// ttlSeconds is how long a push service keeps a message for an unreachable device.
	ttlSeconds = 24 * 60 * 60
	// queueSize bounds the finished jobs waiting to be sent.
	queueSize = 256
	// concurrency bounds the simultaneous requests for one job.
	concurrency = 8

	defaultSendTimeout   = 10 * time.Second
	defaultShutdownGrace = 2 * time.Second

	maxTitleRunes = 100
)

// Payload is the JSON message the service worker receives.
type Payload struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	JobID int64  `json:"job_id"`
	URL   string `json:"url"`
	// Status gives the notification the same tag as the one an open tab shows, so that
	// the two replace each other.
	Status string `json:"status"`
}

// Options configures a Sender.
type Options struct {
	Keys VAPIDKeys
	// Subject is the VAPID contact: an https: or mailto: URL.
	Subject string
	// ReadTokenIDs lists the TokenID of every API token that may currently read jobs.
	// Only subscriptions registered with one of them receive messages; an empty list
	// means that none does.
	ReadTokenIDs []string
	Logger       *log.Logger
	// Transport overrides the HTTP transport in tests when non-nil.
	Transport http.RoundTripper
	// SendTimeout bounds one request to a push service when positive.
	SendTimeout time.Duration
	// ShutdownGrace is how long Close lets pending messages finish when positive.
	ShutdownGrace time.Duration
}

// Sender pushes a message to every subscription when a job becomes done or failed. It
// works in its own goroutines: job transitions never wait for it, and failures are only
// logged. Delivery is attempted once; nothing is persisted or retried.
type Sender struct {
	store  *store.Store
	keys   VAPIDKeys
	logger *log.Logger
	client *http.Client
	// subscriber is the subject in the form webpush-go expects: it adds "mailto:" to
	// anything that is not an https: URL.
	subscriber  string
	readTokens  map[string]bool
	sendTimeout time.Duration
	grace       time.Duration

	queue chan store.JobEvent
	done  chan struct{}
	// ctx is cancelled when Close gives up on pending messages.
	ctx    context.Context
	cancel context.CancelFunc

	mu          sync.Mutex
	closing     bool
	unsubscribe func()
}

// Start subscribes to st before returning, so every transition committed afterwards is
// sent. The caller must Close the Sender before closing st.
func Start(st *store.Store, opts Options) *Sender {
	logger := opts.Logger
	if logger == nil {
		logger = log.Default()
	}
	sendTimeout := opts.SendTimeout
	if sendTimeout <= 0 {
		sendTimeout = defaultSendTimeout
	}
	grace := opts.ShutdownGrace
	if grace <= 0 {
		grace = defaultShutdownGrace
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Sender{
		store:  st,
		keys:   opts.Keys,
		logger: logger,
		client: &http.Client{
			Transport: opts.Transport,
			// A push service answers directly; a redirect is never followed.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		subscriber:  strings.TrimPrefix(opts.Subject, "mailto:"),
		readTokens:  map[string]bool{},
		sendTimeout: sendTimeout,
		grace:       grace,
		queue:       make(chan store.JobEvent, queueSize),
		done:        make(chan struct{}),
		ctx:         ctx,
		cancel:      cancel,
	}
	for _, id := range opts.ReadTokenIDs {
		if id != "" {
			s.readTokens[id] = true
		}
	}
	s.logUnauthorized()
	events, unsubscribe := st.SubscribeJobEvents()
	s.unsubscribe = unsubscribe
	go s.collect(events)
	go s.run()
	return s
}

// Close stops listening, waits up to the shutdown grace for pending messages and then
// abandons the rest. It is safe to call more than once.
func (s *Sender) Close() {
	s.mu.Lock()
	s.closing = true
	s.unsubscribe()
	s.mu.Unlock()

	timer := time.NewTimer(s.grace)
	defer timer.Stop()
	select {
	case <-s.done:
	case <-timer.C:
		s.cancel()
		<-s.done
	}
	s.cancel()
}

// collect only moves events into the queue, so that the store never finds this
// subscriber too slow while a push service is.
func (s *Sender) collect(events <-chan store.JobEvent) {
	defer close(s.queue)
	for events != nil {
		for ev := range events {
			if ev.Status != store.StatusDone && ev.Status != store.StatusFailed {
				continue
			}
			select {
			case s.queue <- ev:
			default:
				s.logger.Printf("WARNING: web push queue is full; no push for job %d (%s)", ev.JobID, ev.Status)
			}
		}
		events = s.resubscribe()
	}
}

// resubscribe returns nil when the channel closed because of a shutdown.
func (s *Sender) resubscribe() <-chan store.JobEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing || s.store.JobEventsClosed() {
		return nil
	}
	s.logger.Printf("WARNING: web push fell behind the job events; some pushes may be missing")
	events, unsubscribe := s.store.SubscribeJobEvents()
	s.unsubscribe = unsubscribe
	return events
}

func (s *Sender) run() {
	defer close(s.done)
	for ev := range s.queue {
		if s.ctx.Err() != nil {
			s.logger.Printf("WARNING: shutting down; no web push for job %d (%s)", ev.JobID, ev.Status)
			continue
		}
		s.deliver(ev)
	}
}

// recipients returns the subscriptions whose token may still read jobs. The others are
// kept: they receive again if the token regains access or the browser registers anew.
func (s *Sender) recipients(ctx context.Context) (allowed []store.PushSubscription, excluded int, err error) {
	subs, err := s.store.ListPushSubscriptions(ctx)
	if err != nil {
		return nil, 0, err
	}
	for _, sub := range subs {
		if s.readTokens[sub.TokenID] {
			allowed = append(allowed, sub)
		}
	}
	return allowed, len(subs) - len(allowed), nil
}

// logUnauthorized tells the operator once, at startup, about subscriptions that no
// longer receive anything. The tokens only change with a restart.
func (s *Sender) logUnauthorized() {
	_, excluded, err := s.recipients(s.ctx)
	if err != nil {
		s.logger.Printf("WARNING: web push: list subscriptions: %v", err)
		return
	}
	if excluded > 0 {
		s.logger.Printf("web push: %d subscription(s) receive nothing: no configured token with the read scope registered them", excluded)
	}
}

func (s *Sender) deliver(ev store.JobEvent) {
	subs, _, err := s.recipients(s.ctx)
	if err != nil {
		s.logger.Printf("WARNING: web push for job %d: list subscriptions: %v", ev.JobID, err)
		return
	}
	if len(subs) == 0 {
		return
	}
	message, err := json.Marshal(newPayload(ev))
	if err != nil {
		s.logger.Printf("WARNING: web push for job %d: encode payload: %v", ev.JobID, err)
		return
	}

	var wg sync.WaitGroup
	slots := make(chan struct{}, concurrency)
	for _, sub := range subs {
		slots <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			s.send(ev.JobID, sub.Endpoint, message)
		}()
	}
	wg.Wait()
}

// newPayload takes nothing from the job's result or error: a notification may appear on
// a lock screen, and those texts can hold secrets. The job page shows them.
func newPayload(ev store.JobEvent) Payload {
	state, detail := "完了", "done（完了）"
	if ev.Status == store.StatusFailed {
		state, detail = "失敗", "failed（失敗）"
	}
	title := excerpt(ev.Title, maxTitleRunes)
	if title == "" {
		title = "(無題)"
	}
	return Payload{
		Title:  fmt.Sprintf("ジョブ #%d %s: %s", ev.JobID, state, title),
		Body:   "状態: " + detail,
		JobID:  ev.JobID,
		URL:    fmt.Sprintf("/ui/jobs/%d", ev.JobID),
		Status: string(ev.Status),
	}
}

// excerpt joins the words of text with single spaces and cuts the result to limit runes,
// which keeps the message within the size push services accept.
func excerpt(text string, limit int) string {
	out := make([]rune, 0, limit+1)
	space := false
	for _, r := range text {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			space = len(out) > 0
			continue
		}
		if space {
			out = append(out, ' ')
			space = false
		}
		out = append(out, r)
		if len(out) > limit {
			return string(out[:limit-1]) + "…"
		}
	}
	return string(out)
}

// send runs once a request slot is free. It reads the subscription again, because it may
// have been removed or registered anew while it waited. A removal after that read does
// not stop this message (docs/16).
func (s *Sender) send(jobID int64, endpoint string, message []byte) {
	// An endpoint is a capability URL, so only its host is logged.
	host := "invalid endpoint"
	if u, err := url.Parse(endpoint); err == nil {
		host = u.Host
	}
	sub, found, err := s.store.GetPushSubscription(s.ctx, endpoint)
	if err != nil {
		// Not sent: it may have been removed.
		s.logger.Printf("WARNING: web push for job %d to %s: read subscription: %v", jobID, host, err)
		return
	}
	if !found || !s.readTokens[sub.TokenID] {
		return
	}
	ctx, cancel := context.WithTimeout(s.ctx, s.sendTimeout)
	defer cancel()

	// webpush-go appends its padding to the slice it is given, so each request gets a copy.
	resp, err := webpush.SendNotificationWithContext(ctx, bytes.Clone(message), &webpush.Subscription{
		Endpoint: sub.Endpoint,
		Keys:     webpush.Keys{P256dh: sub.P256dh, Auth: sub.Auth},
	}, &webpush.Options{
		HTTPClient:      s.client,
		Subscriber:      s.subscriber,
		TTL:             ttlSeconds,
		VAPIDPublicKey:  s.keys.PublicKey,
		VAPIDPrivateKey: s.keys.PrivateKey,
	})
	if err != nil {
		// url.Error repeats the endpoint.
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		s.logger.Printf("WARNING: web push for job %d to %s failed: %v", jobID, host, err)
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))

	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		// Not s.ctx: a shutdown must not keep a dead subscription.
		if _, err := s.store.DeleteExpiredPushSubscription(context.Background(), sub); err != nil {
			s.logger.Printf("WARNING: delete expired push subscription on %s: %v", host, err)
			return
		}
		s.logger.Printf("web push subscription on %s expired (HTTP %d); deleted", host, resp.StatusCode)
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		s.logger.Printf("WARNING: web push for job %d to %s: HTTP %d", jobID, host, resp.StatusCode)
	}
}

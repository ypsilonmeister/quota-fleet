package api

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/quota"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

// The visibility tests decode into maps so that they pin the JSON field names and
// which fields are omitted.

func listHeldByID(t *testing.T, baseURL string) map[string]map[string]any {
	t.Helper()
	resp := doJSON(t, http.MethodGet, baseURL+"/gate/held?status=all", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /gate/held status = %d, want 200", resp.StatusCode)
	}
	body := decodeResp[struct {
		Held []map[string]any `json:"held"`
	}](t, resp)
	out := make(map[string]map[string]any, len(body.Held))
	for _, h := range body.Held {
		id, _ := h["id"].(string)
		out[id] = h
	}
	return out
}

// heldObject sends a request and returns the "held" object of its response.
func heldObject(t *testing.T, method, url string, body any, wantStatus int) map[string]any {
	t.Helper()
	resp := doJSON(t, method, url, "", body)
	if resp.StatusCode != wantStatus {
		t.Fatalf("%s %s status = %d, want %d", method, url, resp.StatusCode, wantStatus)
	}
	out := decodeResp[struct {
		Held map[string]any `json:"held"`
	}](t, resp)
	if out.Held == nil {
		t.Fatalf("%s %s: response has no held object", method, url)
	}
	return out.Held
}

func assertFields(t *testing.T, label string, got map[string]any, want map[string]any) {
	t.Helper()
	for key, w := range want {
		g, ok := got[key]
		if w == nil {
			if ok {
				t.Errorf("%s: %s = %v, want the field omitted", label, key, g)
			}
			continue
		}
		if !ok || g != w {
			t.Errorf("%s: %s = %v (present=%t), want %v", label, key, g, ok, w)
		}
	}
}

func holdForTest(t *testing.T, st *store.Store, requestID string, parentID *int64) *store.HeldJob {
	t.Helper()
	h, _, err := st.HoldJob(context.Background(), store.HoldJobParams{
		RequestID: requestID,
		Pool:      "claude",
		Payload:   store.CreateJobParams{RequestID: requestID, Title: requestID, ParentID: parentID},
		Reason:    "exhausted:weekly",
	})
	if err != nil {
		t.Fatalf("HoldJob %s: %v", requestID, err)
	}
	return h
}

// newHeldClockServer is newTestServer plus a way to rewrite stored held columns, so
// that tests can pin held times and durations without depending on the wall clock.
// time.Time values are stored in the store's text format.
func newHeldClockServer(t *testing.T, opts Options) (*store.Store, *httptest.Server, func(heldID, column string, value any)) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "fleet.db")
	st, err := store.Open(context.Background(), dbPath, store.Options{InstanceID: opts.InstanceID})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ts := httptest.NewServer(NewServer(st, opts).Handler())
	t.Cleanup(ts.Close)

	set := func(heldID, column string, value any) {
		t.Helper()
		if at, ok := value.(time.Time); ok {
			value = at.UTC().Format(time.RFC3339Nano)
		}
		res, err := db.Exec(`UPDATE held_jobs SET `+column+` = ? WHERE id = ?`, value, heldID)
		if err != nil {
			t.Errorf("set %s of %s: %v", column, heldID, err)
			return
		}
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			t.Errorf("set %s of %s: affected %d rows (err=%v), want 1", column, heldID, n, err)
		}
	}
	return st, ts, set
}

func TestHeldResponsesShowHeldTimeAndElapsed(t *testing.T) {
	var now time.Time
	st, ts, setHeld := newHeldClockServer(t, Options{
		InstanceID: "test-held-age",
		Now:        func() time.Time { return now },
	})
	waiting := holdForTest(t, st, "req-age-waiting", nil)
	cancelled := holdForTest(t, st, "req-age-cancelled", nil)
	released := holdForTest(t, st, "req-age-released", nil)
	ctx := context.Background()
	if _, err := st.CancelHeldJob(ctx, cancelled.ID, ""); err != nil {
		t.Fatalf("CancelHeldJob: %v", err)
	}
	job, _, err := st.CreateJob(ctx, released.Payload)
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if _, err := st.MarkHeldReleased(ctx, released.ID, job.ID, "claude"); err != nil {
		t.Fatalf("MarkHeldReleased: %v", err)
	}

	heldAt := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	for _, h := range []*store.HeldJob{waiting, cancelled, released} {
		setHeld(h.ID, "created_at", heldAt)
	}
	setHeld(cancelled.ID, "cancelled_at", heldAt.Add(20*time.Minute))
	setHeld(released.ID, "released_at", heldAt.Add(10*time.Minute))

	now = heldAt.Add(90*time.Minute + 500*time.Millisecond)
	items := listHeldByID(t, ts.URL)

	// Entries that are no longer held stop counting when they leave the held state.
	for _, tc := range []struct {
		held    *store.HeldJob
		seconds float64
	}{{waiting, 5400}, {cancelled, 1200}, {released, 600}} {
		assertFields(t, tc.held.RequestID, items[tc.held.ID], map[string]any{
			"held_at":      heldAt.Format(time.RFC3339Nano),
			"created_at":   heldAt.Format(time.RFC3339Nano),
			"held_seconds": tc.seconds,
		})
	}
}

func TestHeldSeconds(t *testing.T) {
	held := time.Date(2026, 11, 26, 1, 0, 0, 0, time.UTC)
	releasedAt := held.Add(10 * time.Minute)
	cancelledAt := held.Add(20 * time.Minute)
	now := held.Add(time.Hour)

	for _, tc := range []struct {
		name string
		job  store.HeldJob
		now  time.Time
		want int64
	}{
		{"held counts up to now", store.HeldJob{CreatedAt: held}, now, 3600},
		{"released stops at released_at", store.HeldJob{CreatedAt: held, ReleasedAt: &releasedAt}, now, 600},
		{"cancelled stops at cancelled_at", store.HeldJob{CreatedAt: held, CancelledAt: &cancelledAt}, now, 1200},
		{"clock behind held time is not negative", store.HeldJob{CreatedAt: held}, held.Add(-time.Minute), 0},
	} {
		if got := heldSeconds(&tc.job, tc.now); got != tc.want {
			t.Errorf("%s: heldSeconds = %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestHeldResponsesShowParentAndItsCurrentStatus(t *testing.T) {
	ctx := context.Background()
	_, st, ts := newTestServer(t, Options{InstanceID: "test-held-parent"})

	parent, _, err := st.CreateJob(ctx, store.CreateJobParams{RequestID: "req-parent", Title: "Parent"})
	if err != nil {
		t.Fatalf("CreateJob parent: %v", err)
	}
	child := holdForTest(t, st, "req-child", &parent.ID)
	orphanID := parent.ID + 1000
	orphan := holdForTest(t, st, "req-orphan", &orphanID)
	root := holdForTest(t, st, "req-root", nil)

	items := listHeldByID(t, ts.URL)
	assertFields(t, "child", items[child.ID], map[string]any{
		"parent_id":     float64(parent.ID),
		"parent_status": "queued",
	})
	assertFields(t, "orphan", items[orphan.ID], map[string]any{
		"parent_id":     float64(orphanID),
		"parent_status": "not_found",
	})
	assertFields(t, "root", items[root.ID], map[string]any{
		"parent_id":     nil,
		"parent_status": nil,
	})

	// The status is the parent's state at read time, not at hold time.
	if _, err := st.Cancel(ctx, parent.ID, "superseded by a newer round"); err != nil {
		t.Fatalf("Cancel parent: %v", err)
	}
	assertFields(t, "child after parent cancel", listHeldByID(t, ts.URL)[child.ID], map[string]any{
		"parent_status": "cancelled",
		"status":        "held",
	})
}

func TestHeldCancelReasonOverHTTP(t *testing.T) {
	_, st, ts := newTestServer(t, Options{InstanceID: "test-held-reason"})
	withReason := holdForTest(t, st, "req-with-reason", nil)
	noReason := holdForTest(t, st, "req-no-reason", nil)
	waiting := holdForTest(t, st, "req-still-held", nil)
	url := ts.URL + "/gate/held/" + withReason.ID

	// A malformed body is rejected and leaves the entry held.
	resp := doJSON(t, http.MethodDelete, url, "", "not an object")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("DELETE with malformed body status = %d, want 400", resp.StatusCode)
	}
	_ = resp.Body.Close()
	assertFields(t, "after 400", listHeldByID(t, ts.URL)[withReason.ID], map[string]any{
		"status":        "held",
		"cancel_reason": nil,
	})

	const reason = "stale round; replaced by round 3"
	got := heldObject(t, http.MethodDelete, url, map[string]any{"reason": reason}, http.StatusOK)
	assertFields(t, "DELETE with reason", got, map[string]any{"status": "cancelled", "cancel_reason": reason})

	// A second cancel conflicts and does not overwrite the stored reason.
	got = heldObject(t, http.MethodDelete, url, map[string]any{"reason": "another"}, http.StatusConflict)
	assertFields(t, "second DELETE", got, map[string]any{"cancel_reason": reason})

	// Cancelling without a body keeps working and records no reason.
	got = heldObject(t, http.MethodDelete, ts.URL+"/gate/held/"+noReason.ID, nil, http.StatusOK)
	assertFields(t, "DELETE without body", got, map[string]any{"status": "cancelled", "cancel_reason": nil})

	items := listHeldByID(t, ts.URL)
	assertFields(t, "list with reason", items[withReason.ID], map[string]any{"cancel_reason": reason})
	assertFields(t, "list without reason", items[noReason.ID], map[string]any{"cancel_reason": nil})
	assertFields(t, "list still held", items[waiting.ID], map[string]any{"cancel_reason": nil})
}

// A cancel body is optional, but one that is sent must be exactly one JSON object.
func TestHeldCancelBodyMustBeOneJSONObject(t *testing.T) {
	_, st, ts := newTestServer(t, Options{InstanceID: "test-held-cancel-body"})
	send := func(t *testing.T, id, body string) int {
		t.Helper()
		req, err := http.NewRequest(http.MethodDelete, ts.URL+"/gate/held/"+id, strings.NewReader(body))
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("DELETE: %v", err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	for _, tc := range []struct{ name, body string }{
		{"null", `null`},
		{"string", `"reason"`},
		{"array", `[{"reason":"test"}]`},
		{"number", `1`},
		{"whitespace only", " \n"},
		{"truncated object", `{"reason":"test"`},
		{"trailing text", `{"reason":"test"} trailing`},
		{"second object", `{"reason":"test"}{"reason":"again"}`},
		{"trailing brace", `{"reason":"test"}}`},
		{"wrong reason type", `{"reason":1}`},
		{"too large", `{"reason":"test"}` + strings.Repeat(" ", maxBodyBytes)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			held := holdForTest(t, st, "req-bad-body-"+tc.name, nil)
			if got := send(t, held.ID, tc.body); got != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", got)
			}
			assertFields(t, "after rejected body", listHeldByID(t, ts.URL)[held.ID], map[string]any{
				"status":        "held",
				"cancel_reason": nil,
				"cancelled_at":  nil,
			})
		})
	}

	for _, tc := range []struct {
		name, body string
		reason     any
	}{
		{"empty", "", nil},
		{"empty object", `{}`, nil},
		{"surrounding whitespace", " \r\n\t{\"reason\":\"stale\"}\n ", "stale"},
		{"last_error", `{"last_error":"stale"}`, "stale"},
	} {
		t.Run("accepts "+tc.name, func(t *testing.T) {
			h := holdForTest(t, st, "req-good-body-"+tc.name, nil)
			if got := send(t, h.ID, tc.body); got != http.StatusOK {
				t.Fatalf("status = %d, want 200", got)
			}
			assertFields(t, "after accepted body", listHeldByID(t, ts.URL)[h.ID], map[string]any{
				"status":        "cancelled",
				"cancel_reason": tc.reason,
			})
		})
	}
}

// POST /jobs returns the same held representation: 202 while held, 409 once cancelled.
func TestCreateJobHeldRepresentationShowsVisibilityFields(t *testing.T) {
	ctx := context.Background()
	// Ahead of the wall clock, so a new hold has a non-zero age.
	now := time.Now().UTC().Add(time.Hour)
	zero := 0
	resetAt := now.Add(48 * time.Hour)
	// onEvaluate runs once, between the handler's idempotency checks and its HoldJob call.
	var onEvaluate func()
	st, ts, setHeld := newHeldClockServer(t, Options{
		InstanceID: "test-held-post",
		GateMode:   config.GateModeEnforce,
		Pools:      map[string]config.PoolConfig{"claude": {Provider: "anthropic"}},
		Now:        func() time.Time { return now },
		SnapshotLoader: func() (*quota.Snapshot, error) {
			if f := onEvaluate; f != nil {
				onEvaluate = nil
				f()
			}
			return &quota.Snapshot{
				MeasuredAt: now,
				Pools: map[string]quota.PoolSnapshot{
					"claude": {
						WeeklyRemainingPercent: 0,
						WeeklyResetAt:          &resetAt,
						ShortRemainingPercent:  &zero,
						MeasuredAt:             now,
					},
				},
			}, nil
		},
	})
	parent, _, err := st.CreateJob(ctx, store.CreateJobParams{RequestID: "req-post-parent", Title: "Parent"})
	if err != nil {
		t.Fatalf("CreateJob parent: %v", err)
	}
	req := CreateJobRequest{RequestID: "req-post-child", Title: "Child", Pool: "claude", ParentID: &parent.ID}

	// assertAge pins the held time and the numeric duration of one held representation.
	assertAge := func(label string, got map[string]any, heldAt time.Time, seconds int64) {
		t.Helper()
		assertFields(t, label, got, map[string]any{
			"held_at":      heldAt.Format(time.RFC3339Nano),
			"held_seconds": float64(seconds),
		})
	}
	getHeld := func() *store.HeldJob {
		t.Helper()
		h, err := st.GetHeldJobByRequestID(ctx, req.RequestID)
		if err != nil {
			t.Fatalf("GetHeldJobByRequestID: %v", err)
		}
		return h
	}
	parentFields := map[string]any{"parent_id": float64(parent.ID), "parent_status": "queued"}

	// First submission creates the hold.
	got := heldObject(t, http.MethodPost, ts.URL+"/jobs", req, http.StatusAccepted)
	held := getHeld()
	assertAge("new hold", got, held.CreatedAt, int64(now.Sub(held.CreatedAt)/time.Second))
	assertFields(t, "new hold", got, parentFields)
	assertFields(t, "new hold", got, map[string]any{"cancel_reason": nil})
	url := ts.URL + "/gate/held/" + held.ID

	// The second returns the existing one.
	heldAt := now.Add(-(3*time.Hour + 500*time.Millisecond))
	setHeld(held.ID, "created_at", heldAt)
	got = heldObject(t, http.MethodPost, ts.URL+"/jobs", req, http.StatusAccepted)
	assertAge("existing hold", got, heldAt, 3*60*60)
	assertFields(t, "existing hold", got, parentFields)
	assertFields(t, "existing hold", got, map[string]any{"cancel_reason": nil})

	reason := strings.Repeat("x", 10)
	got = heldObject(t, http.MethodDelete, url, map[string]any{"reason": reason}, http.StatusOK)
	assertAge("DELETE", got, heldAt, int64(getHeld().CancelledAt.Sub(heldAt)/time.Second))

	// Once cancelled the duration stops, however late the response is built.
	setHeld(held.ID, "cancelled_at", heldAt.Add(20*time.Minute))
	now = now.Add(time.Minute)
	got = heldObject(t, http.MethodDelete, url, nil, http.StatusConflict)
	assertAge("second DELETE", got, heldAt, 20*60)
	assertFields(t, "second DELETE", got, parentFields)

	got = heldObject(t, http.MethodPost, ts.URL+"/jobs", req, http.StatusConflict)
	assertAge("resubmit after cancel", got, heldAt, 20*60)
	assertFields(t, "resubmit after cancel", got, parentFields)
	assertFields(t, "resubmit after cancel", got, map[string]any{
		"status":        "cancelled",
		"cancel_reason": reason,
	})

	// Cancelling a released entry conflicts with a duration that stopped at release.
	released := holdForTest(t, st, "req-post-released", nil)
	job, _, err := st.CreateJob(ctx, released.Payload)
	if err != nil {
		t.Fatalf("CreateJob released: %v", err)
	}
	if _, err := st.MarkHeldReleased(ctx, released.ID, job.ID, "claude"); err != nil {
		t.Fatalf("MarkHeldReleased: %v", err)
	}
	setHeld(released.ID, "created_at", heldAt)
	setHeld(released.ID, "released_at", heldAt.Add(10*time.Minute))
	got = heldObject(t, http.MethodDelete, ts.URL+"/gate/held/"+released.ID, nil, http.StatusConflict)
	assertAge("DELETE released", got, heldAt, 10*60)

	// Resubmitting a released entry whose job cannot be found conflicts. The release
	// flow leaves a job behind, so the row is marked released directly.
	orphan := holdForTest(t, st, "req-post-orphan", &parent.ID)
	setHeld(orphan.ID, "status", "released")
	setHeld(orphan.ID, "created_at", heldAt)
	setHeld(orphan.ID, "released_at", heldAt.Add(15*time.Minute))
	orphanReq := req
	orphanReq.RequestID = orphan.RequestID
	got = heldObject(t, http.MethodPost, ts.URL+"/jobs", orphanReq, http.StatusConflict)
	assertAge("resubmit released without job", got, heldAt, 15*60)
	assertFields(t, "resubmit released without job", got, parentFields)

	// Another caller holds and cancels the same request while this one is being
	// evaluated, so the conflict comes from HoldJob instead of the earlier lookup.
	raceReq := req
	raceReq.RequestID = "req-post-race"
	onEvaluate = func() {
		h, _, err := st.HoldJob(ctx, store.HoldJobParams{
			RequestID: raceReq.RequestID,
			Pool:      "claude",
			Payload:   store.CreateJobParams{RequestID: raceReq.RequestID, Title: "Child", ParentID: &parent.ID},
		})
		if err != nil {
			t.Errorf("HoldJob during evaluation: %v", err)
			return
		}
		if _, err := st.CancelHeldJob(ctx, h.ID, "raced"); err != nil {
			t.Errorf("CancelHeldJob during evaluation: %v", err)
			return
		}
		setHeld(h.ID, "created_at", heldAt)
		setHeld(h.ID, "cancelled_at", heldAt.Add(25*time.Minute))
	}
	got = heldObject(t, http.MethodPost, ts.URL+"/jobs", raceReq, http.StatusConflict)
	assertAge("cancelled during evaluation", got, heldAt, 25*60)
	assertFields(t, "cancelled during evaluation", got, parentFields)
	assertFields(t, "cancelled during evaluation", got, map[string]any{"cancel_reason": "raced"})
}

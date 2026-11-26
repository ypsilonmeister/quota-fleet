package api

import (
	"context"
	"net/http"
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

func TestHeldResponsesShowHeldTimeAndElapsed(t *testing.T) {
	var now time.Time
	_, st, ts := newTestServer(t, Options{
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

	now = waiting.CreatedAt.Add(90*time.Minute + 500*time.Millisecond)
	items := listHeldByID(t, ts.URL)

	got := items[waiting.ID]
	assertFields(t, "waiting", got, map[string]any{
		"held_at":      waiting.CreatedAt.Format(time.RFC3339Nano),
		"created_at":   waiting.CreatedAt.Format(time.RFC3339Nano),
		"held_seconds": float64(5400),
	})
	// Entries that are no longer held stop counting when they leave the held state.
	for _, h := range []*store.HeldJob{cancelled, released} {
		assertFields(t, h.RequestID, items[h.ID], map[string]any{
			"held_at":      h.CreatedAt.Format(time.RFC3339Nano),
			"held_seconds": float64(0),
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

// POST /jobs returns the same held representation: 202 while held, 409 once cancelled.
func TestCreateJobHeldRepresentationShowsVisibilityFields(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	zero := 0
	resetAt := now.Add(24 * time.Hour)
	_, st, ts := newTestServer(t, Options{
		InstanceID: "test-held-post",
		GateMode:   config.GateModeEnforce,
		Pools:      map[string]config.PoolConfig{"claude": {Provider: "anthropic"}},
		Now:        func() time.Time { return now },
		SnapshotLoader: func() (*quota.Snapshot, error) {
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

	// First submission creates the hold; the second returns the existing one.
	for _, label := range []string{"new hold", "existing hold"} {
		got := heldObject(t, http.MethodPost, ts.URL+"/jobs", req, http.StatusAccepted)
		assertFields(t, label, got, map[string]any{
			"parent_id":     float64(parent.ID),
			"parent_status": "queued",
			"held_at":       got["created_at"],
			"cancel_reason": nil,
		})
		if _, ok := got["held_seconds"]; !ok {
			t.Errorf("%s: held_seconds is missing", label)
		}
	}

	held, err := st.GetHeldJobByRequestID(ctx, req.RequestID)
	if err != nil {
		t.Fatalf("GetHeldJobByRequestID: %v", err)
	}
	reason := strings.Repeat("x", 10)
	heldObject(t, http.MethodDelete, ts.URL+"/gate/held/"+held.ID, map[string]any{"reason": reason}, http.StatusOK)

	got := heldObject(t, http.MethodPost, ts.URL+"/jobs", req, http.StatusConflict)
	assertFields(t, "resubmit after cancel", got, map[string]any{
		"status":        "cancelled",
		"cancel_reason": reason,
		"parent_id":     float64(parent.ID),
		"parent_status": "queued",
	})
}

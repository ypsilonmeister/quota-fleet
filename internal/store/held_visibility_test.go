package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func holdVisibilityJob(t *testing.T, st *Store, requestID string, parentID *int64) *HeldJob {
	t.Helper()
	h, _, err := st.HoldJob(context.Background(), HoldJobParams{
		RequestID: requestID,
		Pool:      "claude",
		Payload:   CreateJobParams{RequestID: requestID, Title: requestID, ParentID: parentID},
	})
	if err != nil {
		t.Fatalf("HoldJob %s: %v", requestID, err)
	}
	return h
}

func TestHeldJobParentStatusIsReadFromJobs(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "parent.db"), Options{InstanceID: "held-parent"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	parent, _, err := st.CreateJob(ctx, CreateJobParams{RequestID: "p", Title: "Parent"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	missingID := parent.ID + 1000
	child := holdVisibilityJob(t, st, "child", &parent.ID)
	orphan := holdVisibilityJob(t, st, "orphan", &missingID)
	root := holdVisibilityJob(t, st, "root", nil)

	if child.ParentStatus != StatusQueued || orphan.ParentStatus != "" || root.ParentStatus != "" {
		t.Fatalf("parent status on hold = %q, %q, %q", child.ParentStatus, orphan.ParentStatus, root.ParentStatus)
	}

	if _, err := st.Cancel(ctx, parent.ID, ""); err != nil {
		t.Fatalf("Cancel parent: %v", err)
	}
	byID, err := st.GetHeldJob(ctx, child.ID)
	if err != nil {
		t.Fatalf("GetHeldJob: %v", err)
	}
	byRequest, err := st.GetHeldJobByRequestID(ctx, child.RequestID)
	if err != nil {
		t.Fatalf("GetHeldJobByRequestID: %v", err)
	}
	list, err := st.ListHeldJobs(ctx, HeldStatusHeld, 10)
	if err != nil || len(list) != 3 {
		t.Fatalf("ListHeldJobs = (%d, %v), want 3", len(list), err)
	}
	for _, h := range []*HeldJob{byID, byRequest, list[0]} {
		if h.ID != child.ID || h.ParentStatus != StatusCancelled {
			t.Errorf("held %s parent status = %q, want %s %q", h.ID, h.ParentStatus, child.ID, StatusCancelled)
		}
	}
}

func TestCancelHeldJobStoresBoundedReason(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "reason.db"), Options{InstanceID: "held-reason"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	short := holdVisibilityJob(t, st, "short", nil)
	cancelled, err := st.CancelHeldJob(ctx, short.ID, "  stale round\n")
	if err != nil {
		t.Fatalf("CancelHeldJob: %v", err)
	}
	if cancelled.CancelReason != "stale round" {
		t.Errorf("CancelReason = %q, want trimmed text", cancelled.CancelReason)
	}

	// A multi-byte rune straddles the byte cap.
	long := holdVisibilityJob(t, st, "long", nil)
	cancelled, err = st.CancelHeldJob(ctx, long.ID, strings.Repeat("理", maxHeldErrorBytes))
	if err != nil {
		t.Fatalf("CancelHeldJob long: %v", err)
	}
	want := strings.Repeat("理", maxHeldErrorBytes/len("理"))
	if cancelled.CancelReason != want || !utf8.ValidString(cancelled.CancelReason) {
		t.Errorf("long CancelReason has %d bytes, want %d bytes of whole runes", len(cancelled.CancelReason), len(want))
	}
}

func TestHeldJobsCancelReasonColumnMigration(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "migrate.db")

	st1, err := Open(ctx, dbPath, Options{InstanceID: "held-reason-mig"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	old := holdVisibilityJob(t, st1, "cancelled-before-upgrade", nil)
	pending := holdVisibilityJob(t, st1, "held-before-upgrade", nil)
	if _, err := st1.CancelHeldJob(ctx, old.ID, ""); err != nil {
		t.Fatalf("CancelHeldJob: %v", err)
	}
	// Simulate a database written before cancel_reason existed.
	if _, err := st1.writeDB.ExecContext(ctx, `ALTER TABLE held_jobs DROP COLUMN cancel_reason`); err != nil {
		t.Fatalf("drop cancel_reason: %v", err)
	}
	_ = st1.Close()

	st2, err := Open(ctx, dbPath, Options{InstanceID: "held-reason-mig"})
	if err != nil {
		t.Fatalf("Open migrated: %v", err)
	}
	defer st2.Close()

	got, err := st2.GetHeldJob(ctx, old.ID)
	if err != nil {
		t.Fatalf("GetHeldJob: %v", err)
	}
	if got.Status != HeldStatusCancelled || got.CancelReason != "" {
		t.Errorf("migrated entry = status %q reason %q, want cancelled with no reason", got.Status, got.CancelReason)
	}
	got, err = st2.CancelHeldJob(ctx, pending.ID, "after upgrade")
	if err != nil {
		t.Fatalf("CancelHeldJob after migration: %v", err)
	}
	if got.CancelReason != "after upgrade" {
		t.Errorf("CancelReason = %q", got.CancelReason)
	}
}

package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "fleet.db")
	st, err := Open(context.Background(), dbPath, Options{
		BusyTimeoutMS: 5000,
		InstanceID:    "test-instance",
	})
	if err != nil {
		t.Fatalf("Open(%q): %v", dbPath, err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("Close(): %v", err)
		}
	})
	return st
}

func TestOpenWALSchemaAndMeta(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	mode, err := st.JournalMode(ctx)
	if err != nil {
		t.Fatalf("JournalMode: %v", err)
	}
	if strings.ToLower(mode) != "wal" {
		t.Errorf("journal_mode = %q, want wal", mode)
	}

	// Verify all required tables exist.
	tables := []string{
		"jobs",
		"meta",
		"signal_outbox",
		"leases",
		"held_jobs",
		"burndown_history",
		"reconciler_dedup",
	}
	for _, tbl := range tables {
		var name string
		err := st.readDB.QueryRowContext(ctx,
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, tbl,
		).Scan(&name)
		if err != nil {
			t.Errorf("table %q missing: %v", tbl, err)
		}
	}

	// Verify title immutability trigger.
	job, _, err := st.CreateJob(ctx, CreateJobParams{
		RequestID: "req-imm",
		Title:     "Immutable Title",
	})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if _, err := st.writeDB.ExecContext(ctx, `UPDATE jobs SET title = 'Changed' WHERE id = ?`, job.ID); err == nil {
		t.Errorf("updating job title succeeded, want trigger error")
	}

	// Verify instance_id init and mismatch protection.
	if err := st.VerifyInstanceID(ctx, "inst-a"); !errors.Is(err, ErrNotFound) {
		t.Errorf("VerifyInstanceID before init = %v, want ErrNotFound", err)
	}
	if err := st.InitInstanceID(ctx, "inst-a"); err != nil {
		t.Fatalf("InitInstanceID: %v", err)
	}
	if err := st.VerifyInstanceID(ctx, "inst-a"); err != nil {
		t.Errorf("VerifyInstanceID matching = %v, want nil", err)
	}
	if err := st.VerifyInstanceID(ctx, "inst-b"); !errors.Is(err, ErrInstanceIDMismatch) {
		t.Errorf("VerifyInstanceID mismatch = %v, want ErrInstanceIDMismatch", err)
	}
	if err := st.InitInstanceID(ctx, "inst-b"); !errors.Is(err, ErrInstanceIDMismatch) {
		t.Errorf("InitInstanceID mismatch = %v, want ErrInstanceIDMismatch", err)
	}
}

func TestCreateJobValidationAndIdempotency(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	// Empty title rejected.
	if _, _, err := st.CreateJob(ctx, CreateJobParams{RequestID: "r1", Title: ""}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("empty title err = %v, want ErrInvalidInput", err)
	}
	if _, _, err := st.CreateJob(ctx, CreateJobParams{RequestID: "r2", Title: "   "}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("whitespace title err = %v, want ErrInvalidInput", err)
	}
	// Empty request_id rejected.
	if _, _, err := st.CreateJob(ctx, CreateJobParams{RequestID: "", Title: "valid"}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("empty request_id err = %v, want ErrInvalidInput", err)
	}

	// Normal queued creation with all metadata fields.
	j1, created, err := st.CreateJob(ctx, CreateJobParams{
		RequestID:          "req-001",
		Title:              "Implement core store",
		Agent:              "claude",
		Host:               "ubuntu",
		Workdir:            "/work/repo",
		TargetRef:          "impl/phase1",
		Instructions:       "Write code",
		AcceptanceCriteria: "go test ./...",
		Model:              "claude-opus",
		Pool:               "claude",
		AltPools:           []string{"codex", "agy-3p"},
		SessionID:          "sess-1",
	})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if !created {
		t.Errorf("first CreateJob created = false, want true")
	}
	if j1.Status != StatusQueued {
		t.Errorf("status = %s, want queued", j1.Status)
	}
	if len(j1.AltPools) != 2 || j1.AltPools[0] != "codex" || j1.AltPools[1] != "agy-3p" {
		t.Errorf("AltPools = %v, want [codex agy-3p]", j1.AltPools)
	}
	if j1.Model != "claude-opus" || j1.Pool != "claude" || j1.SessionID != "sess-1" {
		t.Errorf("unexpected extra fields: model=%q pool=%q session=%q", j1.Model, j1.Pool, j1.SessionID)
	}

	// Idempotent re-creation with the same request_id returns existing job unchanged.
	j1Dup, createdDup, err := st.CreateJob(ctx, CreateJobParams{
		RequestID: "req-001",
		Title:     "Different title should not overwrite",
	})
	if err != nil {
		t.Fatalf("CreateJob duplicate: %v", err)
	}
	if createdDup {
		t.Errorf("duplicate CreateJob created = true, want false")
	}
	if j1Dup.ID != j1.ID || j1Dup.Title != j1.Title {
		t.Errorf("duplicate job = %+v, want ID=%d Title=%q", j1Dup, j1.ID, j1.Title)
	}

	// Outbox must have exactly 1 event for req-001 (not 2).
	events, err := st.ListPendingOutbox(ctx, 10)
	if err != nil {
		t.Fatalf("ListPendingOutbox: %v", err)
	}
	if len(events) != 1 || events[0].JobID != j1.ID || events[0].Env != "test-instance" {
		t.Errorf("outbox = %+v, want 1 event for job %d", events, j1.ID)
	}

	// Draft job does not emit outbox event.
	jd, createdDraft, err := st.CreateJob(ctx, CreateJobParams{
		RequestID: "req-draft",
		Title:     "Draft job",
		Draft:     true,
	})
	if err != nil {
		t.Fatalf("CreateJob draft: %v", err)
	}
	if !createdDraft || jd.Status != StatusDraft {
		t.Errorf("draft job created=%v status=%s, want true/draft", createdDraft, jd.Status)
	}
	events, err = st.ListPendingOutbox(ctx, 10)
	if err != nil {
		t.Fatalf("ListPendingOutbox after draft: %v", err)
	}
	if len(events) != 1 {
		t.Errorf("outbox len after draft = %d, want 1", len(events))
	}
}

func TestCreateJobParentRules(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	// Missing parent rejected.
	missingID := int64(99999)
	if _, _, err := st.CreateJob(ctx, CreateJobParams{
		RequestID: "child-orphan",
		Title:     "Orphan",
		ParentID:  &missingID,
	}); !errors.Is(err, ErrParentNotFound) {
		t.Errorf("orphan child err = %v, want ErrParentNotFound", err)
	}

	// Parent in queued -> child enters waiting (no outbox).
	parentQueued, _, err := st.CreateJob(ctx, CreateJobParams{
		RequestID: "parent-q",
		Title:     "Parent Queued",
	})
	if err != nil {
		t.Fatalf("create parentQueued: %v", err)
	}
	childWait, _, err := st.CreateJob(ctx, CreateJobParams{
		RequestID: "child-w",
		Title:     "Child Waiting",
		ParentID:  &parentQueued.ID,
	})
	if err != nil {
		t.Fatalf("create childWait: %v", err)
	}
	if childWait.Status != StatusWaiting {
		t.Errorf("childWait status = %s, want waiting", childWait.Status)
	}

	// Complete parentQueued -> new child created after parent is done enters queued directly.
	claimed, ok, err := st.Claim(ctx, 3)
	if err != nil || !ok {
		t.Fatalf("Claim: ok=%v err=%v", ok, err)
	}
	if _, err := st.Complete(ctx, claimed.ID, claimed.ClaimedBy, "done"); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	childAfterDone, _, err := st.CreateJob(ctx, CreateJobParams{
		RequestID: "child-after-done",
		Title:     "Child After Done",
		ParentID:  &parentQueued.ID,
	})
	if err != nil {
		t.Fatalf("create childAfterDone: %v", err)
	}
	if childAfterDone.Status != StatusQueued {
		t.Errorf("childAfterDone status = %s, want queued", childAfterDone.Status)
	}

	// Parent failed -> rejected.
	parentFail, _, err := st.CreateJob(ctx, CreateJobParams{
		RequestID: "parent-fail",
		Title:     "Parent Fail",
	})
	if err != nil {
		t.Fatalf("create parentFail: %v", err)
	}
	if _, err := st.RejectInvalid(ctx, parentFail.ID, "bad payload"); err != nil {
		t.Fatalf("RejectInvalid: %v", err)
	}
	if _, _, err := st.CreateJob(ctx, CreateJobParams{
		RequestID: "child-of-failed",
		Title:     "Child of Failed",
		ParentID:  &parentFail.ID,
	}); !errors.Is(err, ErrParentTerminal) {
		t.Errorf("child of failed err = %v, want ErrParentTerminal", err)
	}

	// Parent cancelled -> rejected.
	parentCancel, _, err := st.CreateJob(ctx, CreateJobParams{
		RequestID: "parent-cancel",
		Title:     "Parent Cancel",
	})
	if err != nil {
		t.Fatalf("create parentCancel: %v", err)
	}
	if _, err := st.Cancel(ctx, parentCancel.ID, "not needed"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if _, _, err := st.CreateJob(ctx, CreateJobParams{
		RequestID: "child-of-cancelled",
		Title:     "Child of Cancelled",
		ParentID:  &parentCancel.ID,
	}); !errors.Is(err, ErrParentTerminal) {
		t.Errorf("child of cancelled err = %v, want ErrParentTerminal", err)
	}
}

func TestClaimFIFOAndLimits(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	// Empty queue returns (nil, false, nil).
	j, ok, err := st.Claim(ctx, 2)
	if err != nil || ok || j != nil {
		t.Fatalf("Claim empty = (%v, %v, %v), want (nil, false, nil)", j, ok, err)
	}

	var ids []int64
	for i := 1; i <= 3; i++ {
		job, _, err := st.CreateJob(ctx, CreateJobParams{
			RequestID: fmt.Sprintf("fifo-%d", i),
			Title:     fmt.Sprintf("FIFO job %d", i),
		})
		if err != nil {
			t.Fatalf("CreateJob %d: %v", i, err)
		}
		ids = append(ids, job.ID)
	}

	c1, ok, err := st.Claim(ctx, 2)
	if err != nil || !ok {
		t.Fatalf("Claim 1: ok=%v err=%v", ok, err)
	}
	if c1.ID != ids[0] || c1.Attempts != 1 || c1.ClaimedBy == "" || c1.ClaimedAt == nil {
		t.Errorf("c1 = %+v, want ID=%d attempts=1 non-empty token", c1, ids[0])
	}

	c2, ok, err := st.Claim(ctx, 2)
	if err != nil || !ok {
		t.Fatalf("Claim 2: ok=%v err=%v", ok, err)
	}
	if c2.ID != ids[1] || c2.Attempts != 1 || c2.ClaimedBy == "" || c2.ClaimedBy == c1.ClaimedBy {
		t.Errorf("c2 = %+v, want ID=%d distinct token", c2, ids[1])
	}

	// Capacity reached (2 == maxClaimed): must return (nil, false, nil).
	c3, ok, err := st.Claim(ctx, 2)
	if err != nil || ok || c3 != nil {
		t.Fatalf("Claim at cap = (%v, %v, %v), want (nil, false, nil)", c3, ok, err)
	}

	// Complete c1 frees a slot so ids[2] can be claimed.
	if _, err := st.Complete(ctx, c1.ID, c1.ClaimedBy, "ok"); err != nil {
		t.Fatalf("Complete c1: %v", err)
	}
	c3, ok, err = st.Claim(ctx, 2)
	if err != nil || !ok || c3.ID != ids[2] {
		t.Fatalf("Claim after complete = (%v, %v, %v), want ID=%d", c3, ok, err, ids[2])
	}
}

func TestTransitionsNormalAndErrors(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	// Not found checks.
	if _, err := st.GetJob(ctx, 404); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetJob(404) = %v, want ErrNotFound", err)
	}
	if _, err := st.Complete(ctx, 404, "tok", "res"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Complete(404) = %v, want ErrNotFound", err)
	}
	if _, err := st.Fail(ctx, 404, "tok", "err"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Fail(404) = %v, want ErrNotFound", err)
	}
	if _, err := st.Cancel(ctx, 404, "reason"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Cancel(404) = %v, want ErrNotFound", err)
	}
	if _, err := st.CancelClaimed(ctx, 404, "tok", "reason"); !errors.Is(err, ErrNotFound) {
		t.Errorf("CancelClaimed(404) = %v, want ErrNotFound", err)
	}
	if _, err := st.Requeue(ctx, 404); !errors.Is(err, ErrNotFound) {
		t.Errorf("Requeue(404) = %v, want ErrNotFound", err)
	}
	if _, err := st.Reset(ctx, 404); !errors.Is(err, ErrNotFound) {
		t.Errorf("Reset(404) = %v, want ErrNotFound", err)
	}
	if _, err := st.RejectInvalid(ctx, 404, "bad"); !errors.Is(err, ErrNotFound) {
		t.Errorf("RejectInvalid(404) = %v, want ErrNotFound", err)
	}
	if _, err := st.MarkDelivered(ctx, 404); !errors.Is(err, ErrNotFound) {
		t.Errorf("MarkDelivered(404) = %v, want ErrNotFound", err)
	}

	// Create a job and test queued-state error paths.
	j, _, err := st.CreateJob(ctx, CreateJobParams{RequestID: "tr-1", Title: "Transition 1"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if _, err := st.Complete(ctx, j.ID, "any", "res"); !errors.Is(err, ErrConflict) {
		t.Errorf("Complete(queued) = %v, want ErrConflict", err)
	}
	if _, err := st.Fail(ctx, j.ID, "any", "err"); !errors.Is(err, ErrConflict) {
		t.Errorf("Fail(queued) = %v, want ErrConflict", err)
	}
	if _, err := st.CancelClaimed(ctx, j.ID, "any", "err"); !errors.Is(err, ErrConflict) {
		t.Errorf("CancelClaimed(queued) = %v, want ErrConflict", err)
	}
	if _, err := st.Reset(ctx, j.ID); !errors.Is(err, ErrConflict) {
		t.Errorf("Reset(queued) = %v, want ErrConflict", err)
	}
	if _, err := st.Requeue(ctx, j.ID); !errors.Is(err, ErrConflict) {
		t.Errorf("Requeue(queued) = %v, want ErrConflict", err)
	}
	if _, err := st.MarkDelivered(ctx, j.ID); !errors.Is(err, ErrConflict) {
		t.Errorf("MarkDelivered(queued) = %v, want ErrConflict", err)
	}

	// Claim and test token mismatch & claimed-state conflicts.
	claimed, ok, err := st.Claim(ctx, 5)
	if err != nil || !ok {
		t.Fatalf("Claim: ok=%v err=%v", ok, err)
	}
	if _, err := st.Complete(ctx, claimed.ID, "wrong-token", "res"); !errors.Is(err, ErrTokenMismatch) {
		t.Errorf("Complete wrong token = %v, want ErrTokenMismatch", err)
	}
	if _, err := st.Fail(ctx, claimed.ID, "wrong-token", "err"); !errors.Is(err, ErrTokenMismatch) {
		t.Errorf("Fail wrong token = %v, want ErrTokenMismatch", err)
	}
	if _, err := st.CancelClaimed(ctx, claimed.ID, "wrong-token", "err"); !errors.Is(err, ErrTokenMismatch) {
		t.Errorf("CancelClaimed wrong token = %v, want ErrTokenMismatch", err)
	}
	if _, err := st.Cancel(ctx, claimed.ID, "cannot cancel claimed"); !errors.Is(err, ErrConflict) {
		t.Errorf("Cancel(claimed) = %v, want ErrConflict", err)
	}
	if _, err := st.RejectInvalid(ctx, claimed.ID, "cannot reject claimed"); !errors.Is(err, ErrConflict) {
		t.Errorf("RejectInvalid(claimed) = %v, want ErrConflict", err)
	}

	// Reset keeps attempts=1 and puts back to queued (emitting outbox).
	beforeOutbox, _ := st.ListPendingOutbox(ctx, 100)
	resetJob, err := st.Reset(ctx, claimed.ID)
	if err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if resetJob.Status != StatusQueued || resetJob.Attempts != 1 || resetJob.ClaimedBy != "" {
		t.Errorf("after Reset: status=%s attempts=%d claimed_by=%q", resetJob.Status, resetJob.Attempts, resetJob.ClaimedBy)
	}
	afterOutbox, _ := st.ListPendingOutbox(ctx, 100)
	if len(afterOutbox) != len(beforeOutbox)+1 {
		t.Errorf("outbox did not grow on Reset: before=%d after=%d", len(beforeOutbox), len(afterOutbox))
	}

	// Re-claim increments attempts to 2.
	claimed2, ok, err := st.Claim(ctx, 5)
	if err != nil || !ok || claimed2.Attempts != 2 {
		t.Fatalf("re-Claim: job=%+v ok=%v err=%v", claimed2, ok, err)
	}

	// Fail with matching token -> failed.
	failRes, err := st.Fail(ctx, claimed2.ID, claimed2.ClaimedBy, "boom")
	if err != nil {
		t.Fatalf("Fail: %v", err)
	}
	if failRes.Job.Status != StatusFailed || failRes.Job.LastError != "boom" {
		t.Errorf("after Fail: status=%s last_error=%q", failRes.Job.Status, failRes.Job.LastError)
	}

	// Requeue resets attempts to 0 and emits outbox.
	beforeOutbox, _ = st.ListPendingOutbox(ctx, 100)
	requeued, err := st.Requeue(ctx, claimed2.ID)
	if err != nil {
		t.Fatalf("Requeue: %v", err)
	}
	if requeued.Status != StatusQueued || requeued.Attempts != 0 || requeued.LastError != "" {
		t.Errorf("after Requeue: status=%s attempts=%d last_error=%q", requeued.Status, requeued.Attempts, requeued.LastError)
	}
	afterOutbox, _ = st.ListPendingOutbox(ctx, 100)
	if len(afterOutbox) != len(beforeOutbox)+1 {
		t.Errorf("outbox did not grow on Requeue: before=%d after=%d", len(beforeOutbox), len(afterOutbox))
	}

	// Claim -> Complete -> MarkDelivered (idempotent).
	claimed3, ok, err := st.Claim(ctx, 5)
	if err != nil || !ok || claimed3.Attempts != 1 {
		t.Fatalf("Claim after requeue: job=%+v ok=%v err=%v", claimed3, ok, err)
	}
	compRes, err := st.Complete(ctx, claimed3.ID, claimed3.ClaimedBy, "all good")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if compRes.Job.Status != StatusDone || compRes.Job.ResultText != "all good" {
		t.Errorf("after Complete: status=%s result=%q", compRes.Job.Status, compRes.Job.ResultText)
	}
	del1, err := st.MarkDelivered(ctx, claimed3.ID)
	if err != nil || del1.DeliveredAt == nil {
		t.Fatalf("MarkDelivered 1: job=%+v err=%v", del1, err)
	}
	del2, err := st.MarkDelivered(ctx, claimed3.ID)
	if err != nil || del2.DeliveredAt == nil || !del2.DeliveredAt.Equal(*del1.DeliveredAt) {
		t.Errorf("MarkDelivered 2 not idempotent: first=%v second=%v err=%v", del1.DeliveredAt, del2.DeliveredAt, err)
	}

	// Draft cancel & RejectInvalid.
	jd, _, err := st.CreateJob(ctx, CreateJobParams{RequestID: "tr-draft", Title: "Draft", Draft: true})
	if err != nil {
		t.Fatalf("CreateJob draft: %v", err)
	}
	cancelRes, err := st.Cancel(ctx, jd.ID, "abandoned")
	if err != nil || cancelRes.Job.Status != StatusCancelled {
		t.Fatalf("Cancel(draft): res=%+v err=%v", cancelRes, err)
	}
	// Requeue from cancelled works too.
	reqFromCancel, err := st.Requeue(ctx, jd.ID)
	if err != nil || reqFromCancel.Status != StatusQueued {
		t.Fatalf("Requeue(cancelled): job=%+v err=%v", reqFromCancel, err)
	}
	// RejectInvalid from queued -> failed.
	rejRes, err := st.RejectInvalid(ctx, jd.ID, "invalid target_ref")
	if err != nil || rejRes.Job.Status != StatusFailed || rejRes.Job.LastError != "invalid target_ref" {
		t.Fatalf("RejectInvalid: res=%+v err=%v", rejRes, err)
	}
}

func TestParentCompletePromotesDirectChildrenOnly(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	// Chain: A -> B -> C, plus A -> B2
	a, _, err := st.CreateJob(ctx, CreateJobParams{RequestID: "p-a", Title: "A"})
	if err != nil {
		t.Fatalf("create A: %v", err)
	}
	b, _, err := st.CreateJob(ctx, CreateJobParams{RequestID: "p-b", Title: "B", ParentID: &a.ID})
	if err != nil {
		t.Fatalf("create B: %v", err)
	}
	b2, _, err := st.CreateJob(ctx, CreateJobParams{RequestID: "p-b2", Title: "B2", ParentID: &a.ID})
	if err != nil {
		t.Fatalf("create B2: %v", err)
	}
	c, _, err := st.CreateJob(ctx, CreateJobParams{RequestID: "p-c", Title: "C", ParentID: &b.ID})
	if err != nil {
		t.Fatalf("create C: %v", err)
	}

	if b.Status != StatusWaiting || b2.Status != StatusWaiting || c.Status != StatusWaiting {
		t.Fatalf("initial statuses: B=%s B2=%s C=%s, want all waiting", b.Status, b2.Status, c.Status)
	}

	claimedA, ok, err := st.Claim(ctx, 5)
	if err != nil || !ok || claimedA.ID != a.ID {
		t.Fatalf("Claim A: job=%+v ok=%v err=%v", claimedA, ok, err)
	}

	outboxBefore, _ := st.ListPendingOutbox(ctx, 100)
	compA, err := st.Complete(ctx, claimedA.ID, claimedA.ClaimedBy, "A done")
	if err != nil {
		t.Fatalf("Complete A: %v", err)
	}
	if len(compA.PromotedChildIDs) != 2 || compA.PromotedChildIDs[0] != b.ID || compA.PromotedChildIDs[1] != b2.ID {
		t.Errorf("PromotedChildIDs = %v, want [%d %d]", compA.PromotedChildIDs, b.ID, b2.ID)
	}
	outboxAfter, _ := st.ListPendingOutbox(ctx, 100)
	if len(outboxAfter) != len(outboxBefore)+2 {
		t.Errorf("outbox delta = %d, want 2", len(outboxAfter)-len(outboxBefore))
	}

	gotB, _ := st.GetJob(ctx, b.ID)
	gotB2, _ := st.GetJob(ctx, b2.ID)
	gotC, _ := st.GetJob(ctx, c.ID)
	if gotB.Status != StatusQueued || gotB2.Status != StatusQueued {
		t.Errorf("B=%s B2=%s, want queued", gotB.Status, gotB2.Status)
	}
	if gotC.Status != StatusWaiting {
		t.Errorf("grandchild C=%s, want waiting until B completes", gotC.Status)
	}

	// Complete B -> promotes C.
	claimedB, ok, err := st.Claim(ctx, 5)
	if err != nil || !ok || claimedB.ID != b.ID {
		t.Fatalf("Claim B: job=%+v ok=%v err=%v", claimedB, ok, err)
	}
	compB, err := st.Complete(ctx, claimedB.ID, claimedB.ClaimedBy, "B done")
	if err != nil {
		t.Fatalf("Complete B: %v", err)
	}
	if len(compB.PromotedChildIDs) != 1 || compB.PromotedChildIDs[0] != c.ID {
		t.Errorf("PromotedChildIDs after B = %v, want [%d]", compB.PromotedChildIDs, c.ID)
	}
	gotC, _ = st.GetJob(ctx, c.ID)
	if gotC.Status != StatusQueued {
		t.Errorf("C=%s, want queued", gotC.Status)
	}
}

func TestParentFailCascadesDescendantsAndRequeueDoesNotRevive(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	// Tree: A -> B -> C, A -> B2 -> D
	a, _, _ := st.CreateJob(ctx, CreateJobParams{RequestID: "f-a", Title: "A"})
	b, _, _ := st.CreateJob(ctx, CreateJobParams{RequestID: "f-b", Title: "B", ParentID: &a.ID})
	c, _, _ := st.CreateJob(ctx, CreateJobParams{RequestID: "f-c", Title: "C", ParentID: &b.ID})
	b2, _, _ := st.CreateJob(ctx, CreateJobParams{RequestID: "f-b2", Title: "B2", ParentID: &a.ID})
	d, _, _ := st.CreateJob(ctx, CreateJobParams{RequestID: "f-d", Title: "D", ParentID: &b2.ID})

	claimedA, ok, err := st.Claim(ctx, 5)
	if err != nil || !ok || claimedA.ID != a.ID {
		t.Fatalf("Claim A: job=%+v ok=%v err=%v", claimedA, ok, err)
	}

	failRes, err := st.Fail(ctx, claimedA.ID, claimedA.ClaimedBy, "unit tests failed")
	if err != nil {
		t.Fatalf("Fail A: %v", err)
	}
	if len(failRes.CancelledChildIDs) != 4 {
		t.Errorf("CancelledChildIDs = %v, want 4 descendants", failRes.CancelledChildIDs)
	}

	for _, id := range []int64{b.ID, c.ID, b2.ID, d.ID} {
		got, err := st.GetJob(ctx, id)
		if err != nil {
			t.Fatalf("GetJob(%d): %v", id, err)
		}
		if got.Status != StatusCancelled {
			t.Errorf("descendant %d status = %s, want cancelled", id, got.Status)
		}
		if !strings.Contains(got.LastError, "unit tests failed") {
			t.Errorf("descendant %d last_error = %q, want parent reason", id, got.LastError)
		}
	}

	// Requeue A must NOT revive cancelled children.
	requeuedA, err := st.Requeue(ctx, a.ID)
	if err != nil {
		t.Fatalf("Requeue A: %v", err)
	}
	if requeuedA.Status != StatusQueued || requeuedA.Attempts != 0 {
		t.Errorf("requeued A = status:%s attempts:%d, want queued/0", requeuedA.Status, requeuedA.Attempts)
	}
	for _, id := range []int64{b.ID, c.ID, b2.ID, d.ID} {
		got, _ := st.GetJob(ctx, id)
		if got.Status != StatusCancelled {
			t.Errorf("descendant %d after parent Requeue = %s, want cancelled", id, got.Status)
		}
	}
}

func TestCancelAndCancelClaimedCascade(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	// Cancel queued parent cascades to waiting child and grandchild.
	a, _, _ := st.CreateJob(ctx, CreateJobParams{RequestID: "c-a", Title: "A"})
	b, _, _ := st.CreateJob(ctx, CreateJobParams{RequestID: "c-b", Title: "B", ParentID: &a.ID})
	c, _, _ := st.CreateJob(ctx, CreateJobParams{RequestID: "c-c", Title: "C", ParentID: &b.ID})

	res, err := st.Cancel(ctx, a.ID, "superseded")
	if err != nil {
		t.Fatalf("Cancel A: %v", err)
	}
	if len(res.CancelledChildIDs) != 2 {
		t.Errorf("CancelledChildIDs = %v, want 2", res.CancelledChildIDs)
	}
	for _, id := range []int64{b.ID, c.ID} {
		got, _ := st.GetJob(ctx, id)
		if got.Status != StatusCancelled || !strings.Contains(got.LastError, "superseded") {
			t.Errorf("job %d = status:%s err:%q", id, got.Status, got.LastError)
		}
	}

	// CancelClaimed also cascades to waiting children.
	x, _, _ := st.CreateJob(ctx, CreateJobParams{RequestID: "cc-x", Title: "X"})
	y, _, _ := st.CreateJob(ctx, CreateJobParams{RequestID: "cc-y", Title: "Y", ParentID: &x.ID})
	claimedX, ok, err := st.Claim(ctx, 5)
	if err != nil || !ok || claimedX.ID != x.ID {
		t.Fatalf("Claim X: ok=%v err=%v", ok, err)
	}
	ccRes, err := st.CancelClaimed(ctx, claimedX.ID, claimedX.ClaimedBy, "aborted by operator")
	if err != nil {
		t.Fatalf("CancelClaimed X: %v", err)
	}
	if ccRes.Job.Status != StatusCancelled || len(ccRes.CancelledChildIDs) != 1 || ccRes.CancelledChildIDs[0] != y.ID {
		t.Errorf("CancelClaimed result = %+v", ccRes)
	}
	gotY, _ := st.GetJob(ctx, y.ID)
	if gotY.Status != StatusCancelled {
		t.Errorf("Y status = %s, want cancelled", gotY.Status)
	}
}

func TestConcurrentClaimRespectsMaxClaimed(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	const (
		totalJobs  = 30
		maxClaimed = 3
		workers    = 20
	)

	for i := 0; i < totalJobs; i++ {
		if _, _, err := st.CreateJob(ctx, CreateJobParams{
			RequestID: fmt.Sprintf("conc-%02d", i),
			Title:     fmt.Sprintf("Concurrent job %02d", i),
		}); err != nil {
			t.Fatalf("CreateJob %d: %v", i, err)
		}
	}

	// Phase 1: Burst of concurrent Claims must never exceed maxClaimed.
	start := make(chan struct{})
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		claimed []*Job
		errs    []error
	)

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			job, ok, err := st.Claim(ctx, maxClaimed)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			if ok && job != nil {
				claimed = append(claimed, job)
			}
		}()
	}
	close(start)
	wg.Wait()

	if len(errs) > 0 {
		t.Fatalf("concurrent Claim errors: %v", errs)
	}
	if len(claimed) != maxClaimed {
		t.Fatalf("claimed count = %d, want exactly %d", len(claimed), maxClaimed)
	}

	seenIDs := make(map[int64]bool)
	seenTokens := make(map[string]bool)
	for _, j := range claimed {
		if seenIDs[j.ID] {
			t.Errorf("duplicate job ID claimed: %d", j.ID)
		}
		seenIDs[j.ID] = true
		if seenTokens[j.ClaimedBy] {
			t.Errorf("duplicate claim token issued: %q", j.ClaimedBy)
		}
		seenTokens[j.ClaimedBy] = true
	}

	counts, err := st.CountByStatus(ctx)
	if err != nil {
		t.Fatalf("CountByStatus: %v", err)
	}
	if counts[StatusClaimed] != maxClaimed {
		t.Errorf("DB claimed count = %d, want %d", counts[StatusClaimed], maxClaimed)
	}

	// Complete the initial batch, then run concurrent claim+complete workers until drained.
	for _, j := range claimed {
		if _, err := st.Complete(ctx, j.ID, j.ClaimedBy, "done"); err != nil {
			t.Fatalf("Complete %d: %v", j.ID, err)
		}
	}

	var (
		wg2       sync.WaitGroup
		mu2       sync.Mutex
		completed = len(claimed)
		errs2     []error
	)
	for w := 0; w < 10; w++ {
		wg2.Add(1)
		go func() {
			defer wg2.Done()
			for {
				job, ok, err := st.Claim(ctx, maxClaimed)
				if err != nil {
					mu2.Lock()
					errs2 = append(errs2, err)
					mu2.Unlock()
					return
				}
				if !ok {
					// Check if any queued remain; if none, exit.
					c, cerr := st.CountByStatus(ctx)
					if cerr != nil {
						mu2.Lock()
						errs2 = append(errs2, cerr)
						mu2.Unlock()
						return
					}
					if c[StatusClaimed] > maxClaimed {
						mu2.Lock()
						errs2 = append(errs2, fmt.Errorf("claimed exceeded cap: %d > %d", c[StatusClaimed], maxClaimed))
						mu2.Unlock()
						return
					}
					if c[StatusQueued] == 0 {
						return
					}
					continue
				}
				// Verify claimed count never exceeds maxClaimed while holding a claim.
				c, cerr := st.CountByStatus(ctx)
				if cerr != nil {
					mu2.Lock()
					errs2 = append(errs2, cerr)
					mu2.Unlock()
					return
				}
				if c[StatusClaimed] > maxClaimed {
					mu2.Lock()
					errs2 = append(errs2, fmt.Errorf("claimed exceeded cap while active: %d > %d", c[StatusClaimed], maxClaimed))
					mu2.Unlock()
					return
				}
				if _, err := st.Complete(ctx, job.ID, job.ClaimedBy, "ok"); err != nil {
					mu2.Lock()
					errs2 = append(errs2, err)
					mu2.Unlock()
					return
				}
				mu2.Lock()
				completed++
				mu2.Unlock()
			}
		}()
	}
	wg2.Wait()

	if len(errs2) > 0 {
		t.Fatalf("drain errors: %v", errs2)
	}
	if completed != totalJobs {
		t.Errorf("completed = %d, want %d", completed, totalJobs)
	}
	finalCounts, err := st.CountByStatus(ctx)
	if err != nil {
		t.Fatalf("final CountByStatus: %v", err)
	}
	if finalCounts[StatusDone] != totalJobs || finalCounts[StatusQueued] != 0 || finalCounts[StatusClaimed] != 0 {
		t.Errorf("finalCounts = %+v", finalCounts)
	}
}

func TestListJobsPaginationAndCountByStatus(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	// Status filter is mandatory.
	if _, err := st.ListJobs(ctx, ListJobsFilter{Limit: 10}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("ListJobs without status err = %v, want ErrInvalidInput", err)
	}
	if _, err := st.ListJobs(ctx, ListJobsFilter{Status: Status("bogus"), Limit: 10}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("ListJobs invalid status err = %v, want ErrInvalidInput", err)
	}

	// Create 7 queued jobs and 2 draft jobs.
	var queuedIDs []int64
	for i := 1; i <= 7; i++ {
		j, _, err := st.CreateJob(ctx, CreateJobParams{
			RequestID: fmt.Sprintf("page-q-%d", i),
			Title:     fmt.Sprintf("Queued %d", i),
		})
		if err != nil {
			t.Fatalf("CreateJob queued %d: %v", i, err)
		}
		queuedIDs = append(queuedIDs, j.ID)
	}
	for i := 1; i <= 2; i++ {
		if _, _, err := st.CreateJob(ctx, CreateJobParams{
			RequestID: fmt.Sprintf("page-d-%d", i),
			Title:     fmt.Sprintf("Draft %d", i),
			Draft:     true,
		}); err != nil {
			t.Fatalf("CreateJob draft %d: %v", i, err)
		}
	}

	// Ascending pagination with limit=3: expects 3 + 3 + 1.
	var gotAsc []int64
	var cursor int64
	pages := 0
	for {
		page, err := st.ListJobs(ctx, ListJobsFilter{
			Status: StatusQueued,
			Cursor: cursor,
			Limit:  3,
		})
		if err != nil {
			t.Fatalf("ListJobs asc page %d: %v", pages, err)
		}
		pages++
		for _, j := range page.Jobs {
			if j.Status != StatusQueued {
				t.Errorf("unexpected status %s in queued listing", j.Status)
			}
			gotAsc = append(gotAsc, j.ID)
		}
		if !page.HasMore {
			if page.NextCursor != 0 {
				t.Errorf("last page NextCursor = %d, want 0", page.NextCursor)
			}
			break
		}
		if page.NextCursor == 0 {
			t.Fatalf("page %d HasMore=true but NextCursor=0", pages)
		}
		cursor = page.NextCursor
	}
	if pages != 3 {
		t.Errorf("asc pages = %d, want 3", pages)
	}
	if len(gotAsc) != len(queuedIDs) {
		t.Fatalf("gotAsc len = %d, want %d", len(gotAsc), len(queuedIDs))
	}
	for i := range queuedIDs {
		if gotAsc[i] != queuedIDs[i] {
			t.Errorf("gotAsc[%d] = %d, want %d", i, gotAsc[i], queuedIDs[i])
		}
	}

	// Descending pagination with limit=4: expects 4 + 3 in reverse order.
	var gotDesc []int64
	cursor = 0
	pages = 0
	for {
		page, err := st.ListJobs(ctx, ListJobsFilter{
			Status:     StatusQueued,
			Cursor:     cursor,
			Limit:      4,
			Descending: true,
		})
		if err != nil {
			t.Fatalf("ListJobs desc page %d: %v", pages, err)
		}
		pages++
		for _, j := range page.Jobs {
			gotDesc = append(gotDesc, j.ID)
		}
		if !page.HasMore {
			break
		}
		cursor = page.NextCursor
	}
	if pages != 2 {
		t.Errorf("desc pages = %d, want 2", pages)
	}
	if len(gotDesc) != len(queuedIDs) {
		t.Fatalf("gotDesc len = %d, want %d", len(gotDesc), len(queuedIDs))
	}
	for i := range queuedIDs {
		want := queuedIDs[len(queuedIDs)-1-i]
		if gotDesc[i] != want {
			t.Errorf("gotDesc[%d] = %d, want %d", i, gotDesc[i], want)
		}
	}

	// CountByStatus checks all 7 keys are present with accurate totals.
	counts, err := st.CountByStatus(ctx)
	if err != nil {
		t.Fatalf("CountByStatus: %v", err)
	}
	if len(counts) != len(AllStatuses) {
		t.Errorf("CountByStatus keys = %d, want %d", len(counts), len(AllStatuses))
	}
	if counts[StatusQueued] != 7 || counts[StatusDraft] != 2 || counts[StatusDone] != 0 {
		t.Errorf("counts = %+v, want queued=7 draft=2 done=0", counts)
	}
}

func TestClaimByIDAndUndelivered(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	// ClaimByID on missing job returns ErrNotFound.
	if _, _, err := st.ClaimByID(ctx, 9999, 2); !errors.Is(err, ErrNotFound) {
		t.Errorf("ClaimByID(9999) err = %v, want ErrNotFound", err)
	}

	j1, _, err := st.CreateJob(ctx, CreateJobParams{RequestID: "cid-1", Title: "Job 1"})
	if err != nil {
		t.Fatalf("CreateJob 1: %v", err)
	}
	j2, _, err := st.CreateJob(ctx, CreateJobParams{RequestID: "cid-2", Title: "Job 2"})
	if err != nil {
		t.Fatalf("CreateJob 2: %v", err)
	}

	// Claim j2 explicitly (not j1) with maxClaimed=1.
	c2, ok, err := st.ClaimByID(ctx, j2.ID, 1)
	if err != nil || !ok || c2.ID != j2.ID || c2.ClaimedBy == "" {
		t.Fatalf("ClaimByID(j2) = (%+v, %v, %v)", c2, ok, err)
	}

	// Claiming j1 now hits maxClaimed=1 -> (nil, false, nil).
	c1, ok, err := st.ClaimByID(ctx, j1.ID, 1)
	if err != nil || ok || c1 != nil {
		t.Fatalf("ClaimByID(j1) at cap = (%+v, %v, %v), want (nil, false, nil)", c1, ok, err)
	}

	// Claiming already-claimed j2 returns (nil, false, nil).
	c2Again, ok, err := st.ClaimByID(ctx, j2.ID, 5)
	if err != nil || ok || c2Again != nil {
		t.Fatalf("ClaimByID(j2 again) = (%+v, %v, %v), want (nil, false, nil)", c2Again, ok, err)
	}

	// Fail j2, claim & complete j1 -> both appear in ListUndelivered.
	if _, err := st.Fail(ctx, c2.ID, c2.ClaimedBy, "failed reason"); err != nil {
		t.Fatalf("Fail j2: %v", err)
	}
	c1, ok, err = st.ClaimByID(ctx, j1.ID, 1)
	if err != nil || !ok {
		t.Fatalf("ClaimByID(j1) after fail = (%+v, %v, %v)", c1, ok, err)
	}
	if _, err := st.Complete(ctx, c1.ID, c1.ClaimedBy, "ok"); err != nil {
		t.Fatalf("Complete j1: %v", err)
	}

	undel, err := st.ListUndelivered(ctx, 10)
	if err != nil {
		t.Fatalf("ListUndelivered: %v", err)
	}
	if len(undel) != 2 {
		t.Fatalf("ListUndelivered len = %d, want 2", len(undel))
	}

	// MarkDelivered works on failed job too and removes it from ListUndelivered.
	delFailed, err := st.MarkDelivered(ctx, j2.ID)
	if err != nil || delFailed.DeliveredAt == nil {
		t.Fatalf("MarkDelivered(failed) = (%+v, %v)", delFailed, err)
	}
	undel, err = st.ListUndelivered(ctx, 10)
	if err != nil {
		t.Fatalf("ListUndelivered after mark failed: %v", err)
	}
	if len(undel) != 1 || undel[0].ID != j1.ID {
		t.Fatalf("ListUndelivered = %+v, want only j1 (%d)", undel, j1.ID)
	}
}

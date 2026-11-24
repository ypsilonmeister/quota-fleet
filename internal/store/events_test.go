package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func openEventsStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(context.Background(), filepath.Join(t.TempDir(), "fleet.db"), Options{InstanceID: "inst-events"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// drain returns the events already buffered. Publishing happens before a Store method
// returns, so no waiting is needed.
func drain(events <-chan JobEvent) []string {
	var out []string
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return append(out, "closed")
			}
			out = append(out, fmt.Sprintf("%d:%s>%s", ev.JobID, ev.PrevStatus, ev.Status))
		default:
			return out
		}
	}
}

func TestJobEventsCoverEveryTransition(t *testing.T) {
	ctx := context.Background()
	st := openEventsStore(t)
	events, cancel := st.SubscribeJobEvents()
	defer cancel()

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	expect := func(step string, want ...string) {
		t.Helper()
		if got := drain(events); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("%s events = %v, want %v", step, got, want)
		}
	}
	create := func(reqID string, parent *int64, draft bool) *Job {
		t.Helper()
		job, _, err := st.CreateJob(ctx, CreateJobParams{
			RequestID: reqID, Title: "title " + reqID, Agent: "claude", Host: "ubuntu",
			ParentID: parent, Draft: draft,
		})
		must(err)
		return job
	}
	claim := func(id int64) *Job {
		t.Helper()
		job, ok, err := st.ClaimByID(ctx, id, 10)
		must(err)
		if !ok {
			t.Fatalf("ClaimByID(%d) did not claim", id)
		}
		return job
	}

	// create, claim, complete (with child promotion)
	a := create("a", nil, false)
	b := create("b", &a.ID, false)
	expect("create", "1:>queued", "2:>waiting")
	create("a", nil, false)
	expect("idempotent create")

	got, ok, err := st.Claim(ctx, 10)
	must(err)
	if !ok || got.ID != a.ID {
		t.Fatalf("Claim = %+v, %v", got, ok)
	}
	expect("claim", "1:queued>claimed")
	_, err = st.Complete(ctx, a.ID, got.ClaimedBy, "ok")
	must(err)
	expect("complete", "1:claimed>done", "2:waiting>queued")

	// reset, fail (with cascade), requeue
	c := create("c", &b.ID, false)
	d := create("d", &c.ID, false)
	drain(events)
	claim(b.ID)
	_, err = st.Reset(ctx, b.ID)
	must(err)
	expect("claim+reset", "2:queued>claimed", "2:claimed>queued")
	cb := claim(b.ID)
	drain(events)
	_, err = st.Fail(ctx, b.ID, cb.ClaimedBy, "boom")
	must(err)
	expect("fail", "2:claimed>failed", "3:waiting>cancelled", "4:waiting>cancelled")
	_, err = st.Requeue(ctx, b.ID)
	must(err)
	_, err = st.Requeue(ctx, d.ID)
	must(err)
	expect("requeue", "2:failed>queued", "4:cancelled>queued")

	// cancel from draft and queued, cancel-claimed, reject-invalid
	e := create("e", nil, true)
	f := create("f", &e.ID, false)
	drain(events)
	_, err = st.Cancel(ctx, e.ID, "no")
	must(err)
	expect("cancel", fmt.Sprintf("%d:draft>cancelled", e.ID), fmt.Sprintf("%d:waiting>cancelled", f.ID))
	cd := claim(d.ID)
	drain(events)
	_, err = st.CancelClaimed(ctx, d.ID, cd.ClaimedBy, "stop")
	must(err)
	expect("cancel-claimed", "4:claimed>cancelled")
	_, err = st.RejectInvalid(ctx, b.ID, "bad")
	must(err)
	expect("reject-invalid", "2:queued>failed")

	// Failed transitions and delivery bookkeeping publish nothing.
	if _, err := st.Cancel(ctx, a.ID, "late"); err == nil {
		t.Fatal("Cancel of done job succeeded")
	}
	if _, err := st.Complete(ctx, a.ID, "wrong", ""); err == nil {
		t.Fatal("Complete of done job succeeded")
	}
	_, err = st.MarkDelivered(ctx, a.ID)
	must(err)
	expect("no-op writes")
}

func TestJobEventPayload(t *testing.T) {
	ctx := context.Background()
	st := openEventsStore(t)
	events, cancel := st.SubscribeJobEvents()
	defer cancel()

	job, _, err := st.CreateJob(ctx, CreateJobParams{RequestID: "p", Title: "Payload", Agent: "codex", Host: "home"})
	if err != nil {
		t.Fatal(err)
	}
	ev := <-events
	want := JobEvent{JobID: job.ID, Title: "Payload", Status: StatusQueued, Agent: "codex", Host: "home", UpdatedAt: job.UpdatedAt}
	if ev != want {
		t.Fatalf("event = %+v, want %+v", ev, want)
	}
}

func TestJobEventsSlowSubscriberIsDroppedWithoutBlockingWriters(t *testing.T) {
	ctx, stop := context.WithTimeout(context.Background(), 30*time.Second)
	defer stop()
	st := openEventsStore(t)

	slow, cancelSlow := st.SubscribeJobEvents() // never read
	defer cancelSlow()
	fast, cancelFast := st.SubscribeJobEvents()
	defer cancelFast()

	const writes = jobEventBuffer + 20
	for i := range writes {
		if _, _, err := st.CreateJob(ctx, CreateJobParams{RequestID: fmt.Sprintf("slow-%d", i), Title: "t"}); err != nil {
			t.Fatalf("CreateJob %d blocked or failed: %v", i, err)
		}
		if got := drain(fast); len(got) != 1 {
			t.Fatalf("write %d: fast subscriber got %v, want 1 event", i, got)
		}
	}

	buffered := 0
	for range slow { // terminates only if the hub closed the channel
		buffered++
	}
	if buffered != jobEventBuffer {
		t.Fatalf("slow subscriber buffered %d events, want %d", buffered, jobEventBuffer)
	}
}

// A transition committed before a subscription is in the past, even while it has not been
// published yet.
func TestJobEventsSubscribeAfterCommitBeforePublish(t *testing.T) {
	ctx := context.Background()
	st := openEventsStore(t)
	job, _, err := st.CreateJob(ctx, CreateJobParams{RequestID: "boundary", Title: "t"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	claimed, ok, err := st.ClaimByID(ctx, job.ID, 1)
	if err != nil || !ok {
		t.Fatalf("ClaimByID: ok=%v err=%v", ok, err)
	}

	early, cancelEarly := st.SubscribeJobEvents()
	defer cancelEarly()
	var late <-chan JobEvent
	var cancelLate func()
	st.afterJobCommit = func() {
		if late == nil {
			late, cancelLate = st.SubscribeJobEvents()
		}
	}
	if _, err := st.Complete(ctx, job.ID, claimed.ClaimedBy, "ok"); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if late == nil {
		t.Fatal("no subscription between commit and publication")
	}
	defer cancelLate()

	if got, want := drain(early), []string{fmt.Sprintf("%d:claimed>done", job.ID)}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("earlier subscriber got %v, want %v", got, want)
	}
	if got := drain(late); len(got) != 0 {
		t.Fatalf("subscriber added after the commit got %v, want nothing", got)
	}

	next, _, err := st.CreateJob(ctx, CreateJobParams{RequestID: "boundary-next", Title: "t"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	want := []string{fmt.Sprintf("%d:>queued", next.ID)}
	for name, events := range map[string]<-chan JobEvent{"early": early, "late": late} {
		if got := drain(events); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("%s subscriber got %v, want %v", name, got, want)
		}
	}
}

// A subscription requested while a commit is running starts after it, so it cannot fall
// between the commit and the choice of recipients.
func TestJobEventsSubscribeWaitsForCommit(t *testing.T) {
	var hub jobEventHub
	subscribed := make(chan struct{})
	recipients, err := hub.commit(func() error {
		go func() {
			hub.subscribe()
			close(subscribed)
		}()
		select {
		case <-subscribed:
			t.Error("subscribe returned during a commit")
		case <-time.After(50 * time.Millisecond):
		}
		return nil
	})
	if err != nil || len(recipients) != 0 {
		t.Fatalf("commit = %d recipients, %v; want none", len(recipients), err)
	}
	select {
	case <-subscribed:
	case <-time.After(5 * time.Second):
		t.Fatal("subscribe still blocked after the commit")
	}
}

func TestJobEventsCancelAndClose(t *testing.T) {
	st := openEventsStore(t)

	events, cancel := st.SubscribeJobEvents()
	cancel()
	cancel() // idempotent
	if _, ok := <-events; ok {
		t.Fatal("channel open after cancel")
	}

	events, cancel = st.SubscribeJobEvents()
	defer cancel()
	st.CloseJobEvents()
	if _, ok := <-events; ok {
		t.Fatal("channel open after CloseJobEvents")
	}
	late, lateCancel := st.SubscribeJobEvents()
	defer lateCancel()
	if _, ok := <-late; ok {
		t.Fatal("subscription accepted after CloseJobEvents")
	}
	if _, _, err := st.CreateJob(context.Background(), CreateJobParams{RequestID: "after", Title: "t"}); err != nil {
		t.Fatalf("write after CloseJobEvents: %v", err)
	}
}

package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

func keyedParams(requestID, key string, parent *int64) CreateJobParams {
	return CreateJobParams{RequestID: requestID, Title: "publish", SupersedeKey: key, ParentID: parent}
}

func mustCreate(t *testing.T, st *Store, p CreateJobParams) *Job {
	t.Helper()
	job, _, err := st.CreateJob(context.Background(), p)
	if err != nil {
		t.Fatalf("CreateJob(%s): %v", p.RequestID, err)
	}
	return job
}

func mustHold(t *testing.T, st *Store, p CreateJobParams) *HeldJob {
	t.Helper()
	held, _, err := st.HoldJob(context.Background(), HoldJobParams{RequestID: p.RequestID, Pool: "claude", Payload: p})
	if err != nil {
		t.Fatalf("HoldJob(%s): %v", p.RequestID, err)
	}
	return held
}

func mustJob(t *testing.T, st *Store, id int64) *Job {
	t.Helper()
	job, err := st.GetJob(context.Background(), id)
	if err != nil {
		t.Fatalf("GetJob(%d): %v", id, err)
	}
	return job
}

func mustHeld(t *testing.T, st *Store, id string) *HeldJob {
	t.Helper()
	held, err := st.GetHeldJob(context.Background(), id)
	if err != nil {
		t.Fatalf("GetHeldJob(%s): %v", id, err)
	}
	return held
}

func mustJobInfo(t *testing.T, st *Store, id int64) *SupersedeInfo {
	t.Helper()
	info, err := st.SupersedeInfoForJob(context.Background(), id)
	if err != nil {
		t.Fatalf("SupersedeInfoForJob(%d): %v", id, err)
	}
	return info
}

func mustClaim(t *testing.T, st *Store, id int64) *Job {
	t.Helper()
	job, ok, err := st.ClaimByID(context.Background(), id, 10)
	if err != nil || !ok {
		t.Fatalf("ClaimByID(%d) = (%v, %v)", id, ok, err)
	}
	return job
}

// reviveHeld puts a superseded entry back to 'held'. Registration never leaves an older
// entry held, so this is how the release-time check is exercised on its own.
func reviveHeld(t *testing.T, st *Store, heldID string) {
	t.Helper()
	if _, err := st.writeDB.Exec(
		`UPDATE held_jobs SET status = 'held', cancel_reason = '', cancelled_at = NULL WHERE id = ?`, heldID); err != nil {
		t.Fatalf("revive held: %v", err)
	}
}

// Both ways of registering (a job, or a hold) must cancel every unstarted state.
func TestSupersedeCancelsUnstartedRegistrations(t *testing.T) {
	for _, newerIsHeld := range []bool{false, true} {
		for _, oldState := range []string{"held", "queued", "waiting"} {
			t.Run(fmt.Sprintf("old_%s_newer_held_%v", oldState, newerIsHeld), func(t *testing.T) {
				st := openTestStore(t)
				var (
					oldJobID  int64
					oldHeldID string
				)
				switch oldState {
				case "held":
					oldHeldID = mustHold(t, st, keyedParams("old", "k", nil)).ID
				case "queued":
					oldJobID = mustCreate(t, st, keyedParams("old", "k", nil)).ID
				case "waiting":
					parent := mustCreate(t, st, CreateJobParams{RequestID: "parent", Title: "parent"})
					job := mustCreate(t, st, keyedParams("old", "k", &parent.ID))
					if job.Status != StatusWaiting {
						t.Fatalf("old status = %s, want waiting", job.Status)
					}
					oldJobID = job.ID
				}

				var info *SupersedeInfo
				if newerIsHeld {
					held := mustHold(t, st, keyedParams("new", "k", nil))
					var err error
					if info, err = st.SupersedeInfoForHeld(context.Background(), held.ID); err != nil {
						t.Fatal(err)
					}
					if got := mustHeld(t, st, held.ID); got.Status != HeldStatusHeld {
						t.Fatalf("new held status = %s", got.Status)
					}
				} else {
					job := mustCreate(t, st, keyedParams("new", "k", nil))
					if job.Status != StatusQueued || job.SupersedeKey != "k" {
						t.Fatalf("new job = %+v", job)
					}
					info = mustJobInfo(t, st, job.ID)
				}

				want := SupersedeRef{JobID: oldJobID, HeldID: oldHeldID}
				if !slices.Equal(info.Superseded, []SupersedeRef{want}) {
					t.Fatalf("superseded = %+v, want [%+v]", info.Superseded, want)
				}
				if oldState == "held" {
					old := mustHeld(t, st, oldHeldID)
					if old.Status != HeldStatusCancelled || old.CancelledAt == nil ||
						!strings.HasPrefix(old.CancelReason, "superseded:") {
						t.Fatalf("old held = %+v", old)
					}
				} else {
					old := mustJob(t, st, oldJobID)
					if old.Status != StatusCancelled || !strings.HasPrefix(old.LastError, "superseded:") {
						t.Fatalf("old job status=%s last_error=%q", old.Status, old.LastError)
					}
					if by := mustJobInfo(t, st, oldJobID).SupersededBy; by == nil {
						t.Fatal("old job has no superseded_by")
					}
				}
			})
		}
	}
}

func TestSupersedeLeavesStartedAndDraftJobsAlone(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	done := mustCreate(t, st, keyedParams("done", "k", nil))
	done = mustClaim(t, st, done.ID)
	if _, err := st.Complete(ctx, done.ID, done.ClaimedBy, "ok"); err != nil {
		t.Fatal(err)
	}
	failed := mustCreate(t, st, keyedParams("failed", "k", nil))
	failed = mustClaim(t, st, failed.ID)
	if _, err := st.Fail(ctx, failed.ID, failed.ClaimedBy, "boom"); err != nil {
		t.Fatal(err)
	}
	draft := mustCreate(t, st, CreateJobParams{RequestID: "draft", Title: "draft", Draft: true})

	claimed := mustCreate(t, st, keyedParams("claimed", "k", nil))
	claimed = mustClaim(t, st, claimed.ID)
	// A child waiting on the claimed job is unstarted work with another key: it stays.
	child := mustCreate(t, st, CreateJobParams{RequestID: "child", Title: "child", ParentID: &claimed.ID})

	queued := mustCreate(t, st, keyedParams("queued", "k", nil))
	newest := mustCreate(t, st, keyedParams("newest", "k", nil))

	for _, c := range []struct {
		id   int64
		want Status
	}{
		{done.ID, StatusDone}, {failed.ID, StatusFailed}, {draft.ID, StatusDraft},
		{claimed.ID, StatusClaimed}, {child.ID, StatusWaiting}, {queued.ID, StatusCancelled},
		{newest.ID, StatusQueued},
	} {
		if got := mustJob(t, st, c.id); got.Status != c.want {
			t.Errorf("job %s status = %s, want %s", got.RequestID, got.Status, c.want)
		}
	}
	if got := mustJobInfo(t, st, newest.ID).Superseded; !slices.Equal(got, []SupersedeRef{{JobID: queued.ID}}) {
		t.Errorf("superseded = %+v, want only the queued job", got)
	}
	// The claimed job is untouched: its token still completes it and promotes its child.
	res, err := st.Complete(ctx, claimed.ID, claimed.ClaimedBy, "ok")
	if err != nil {
		t.Fatalf("complete claimed job: %v", err)
	}
	if !slices.Equal(res.PromotedChildIDs, []int64{child.ID}) {
		t.Errorf("promoted = %v", res.PromotedChildIDs)
	}
}

func TestSupersedeKeyScopeAndUnkeyedBehavior(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	plain1 := mustCreate(t, st, CreateJobParams{RequestID: "plain1", Title: "publish"})
	plainHeld := mustHold(t, st, CreateJobParams{RequestID: "plain-held", Title: "publish"})
	other := mustCreate(t, st, keyedParams("other", "other-key", nil))
	upper := mustCreate(t, st, keyedParams("upper", "K", nil))
	keyed := mustCreate(t, st, keyedParams("keyed", "k", nil))
	blank := mustCreate(t, st, keyedParams("blank", "   ", nil))
	plain2 := mustCreate(t, st, CreateJobParams{RequestID: "plain2", Title: "publish"})
	// Surrounding space is not part of a key.
	spaced := mustCreate(t, st, keyedParams("spaced", " k ", nil))

	for _, id := range []int64{plain1.ID, plain2.ID, other.ID, upper.ID, blank.ID, spaced.ID} {
		if got := mustJob(t, st, id); got.Status != StatusQueued || got.LastError != "" {
			t.Errorf("job %s = %s %q, want untouched queued", got.RequestID, got.Status, got.LastError)
		}
	}
	if got := mustJob(t, st, keyed.ID); got.Status != StatusCancelled {
		t.Errorf("keyed status = %s, want cancelled", got.Status)
	}
	if blank.SupersedeKey != "" || spaced.SupersedeKey != "k" {
		t.Errorf("keys = %q, %q", blank.SupersedeKey, spaced.SupersedeKey)
	}
	if got := mustHeld(t, st, plainHeld.ID); got.Status != HeldStatusHeld || got.CancelReason != "" {
		t.Errorf("unkeyed held = %+v", got)
	}

	// Unkeyed holds keep accepting a parent that does not exist, as before.
	missing := int64(999999)
	mustHold(t, st, CreateJobParams{RequestID: "plain-orphan", Title: "publish", ParentID: &missing})

	var n int
	if err := st.readDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM supersede_registrations WHERE request_id LIKE 'plain%' OR request_id = 'blank'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("unkeyed registrations recorded = %d, want 0", n)
	}
}

func TestSupersedeReplayDoesNotCancelNewerWork(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	old := mustCreate(t, st, keyedParams("old", "k", nil))
	oldHeld := mustHold(t, st, keyedParams("old-held", "k", nil))
	newest := mustCreate(t, st, keyedParams("new", "k", nil))

	// Replaying an old request_id returns the old job and changes nothing.
	again, created, err := st.CreateJob(ctx, keyedParams("old", "k", nil))
	if err != nil || created || again.ID != old.ID || again.Status != StatusCancelled {
		t.Fatalf("replay = (%+v, %v, %v)", again, created, err)
	}
	if _, _, err := st.HoldJob(ctx, HoldJobParams{RequestID: "old-held", Payload: keyedParams("old-held", "k", nil)}); !errors.Is(err, ErrConflict) {
		t.Fatalf("replay superseded hold err = %v, want ErrConflict", err)
	}
	// Creating a job directly for the superseded held request (gate switched off, say)
	// must not bring the old work back.
	if _, _, err := st.CreateJob(ctx, keyedParams("old-held", "k", nil)); !errors.Is(err, ErrConflict) {
		t.Fatalf("direct create of superseded hold err = %v, want ErrConflict", err)
	}
	if got := mustJob(t, st, newest.ID); got.Status != StatusQueued {
		t.Fatalf("newest status = %s", got.Status)
	}
	if got := mustHeld(t, st, oldHeld.ID); got.Status != HeldStatusCancelled {
		t.Fatalf("old held status = %s", got.Status)
	}
}

// Only registrations with the key change: unkeyed and other-key children of a superseded
// job keep waiting on it.
func TestSupersedeDoesNotCascadeAndRejectsOwnAncestor(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	old := mustCreate(t, st, keyedParams("old", "group-a", nil))
	plain := mustCreate(t, st, CreateJobParams{RequestID: "plain", Title: "child", ParentID: &old.ID})
	other := mustCreate(t, st, keyedParams("other", "group-b", &old.ID))
	grandchild := mustCreate(t, st, CreateJobParams{RequestID: "grandchild", Title: "grandchild", ParentID: &plain.ID})

	// A registration that depends on the work it would cancel is rejected as a whole.
	for _, parent := range []int64{old.ID, plain.ID, other.ID, grandchild.ID} {
		_, _, err := st.CreateJob(ctx, keyedParams("self", "group-a", &parent))
		if !errors.Is(err, ErrParentTerminal) || !strings.Contains(err.Error(), "supersede_key=group-a") {
			t.Fatalf("create under parent %d err = %v", parent, err)
		}
		_, _, err = st.HoldJob(ctx, HoldJobParams{RequestID: "self-held", Payload: keyedParams("self-held", "group-a", &parent)})
		if !errors.Is(err, ErrParentTerminal) || !strings.Contains(err.Error(), "supersede_key=group-a") {
			t.Fatalf("hold under parent %d err = %v", parent, err)
		}
	}
	missing := int64(999999)
	if _, _, err := st.HoldJob(ctx, HoldJobParams{RequestID: "orphan", Payload: keyedParams("orphan", "group-a", &missing)}); !errors.Is(err, ErrParentNotFound) {
		t.Fatalf("keyed hold with missing parent err = %v", err)
	}
	if _, err := st.GetHeldJobByRequestID(ctx, "self-held"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rejected hold was stored: %v", err)
	}
	if got := mustJob(t, st, old.ID); got.Status != StatusQueued {
		t.Fatalf("old was changed by a rejected registration: %s", got.Status)
	}

	events, cancel := st.SubscribeJobEvents()
	defer cancel()
	newer := mustCreate(t, st, keyedParams("new", "group-a", nil))

	if got := mustJobInfo(t, st, newer.ID).Superseded; !slices.Equal(got, []SupersedeRef{{JobID: old.ID}}) {
		t.Fatalf("superseded = %+v", got)
	}
	for _, id := range []int64{plain.ID, other.ID, grandchild.ID} {
		got := mustJob(t, st, id)
		if got.Status != StatusWaiting || got.LastError != "" {
			t.Errorf("descendant %s = %s %q, want untouched waiting", got.RequestID, got.Status, got.LastError)
		}
	}
	var seen []string
	for range 2 {
		ev := <-events
		seen = append(seen, fmt.Sprintf("%d:%s>%s", ev.JobID, ev.PrevStatus, ev.Status))
	}
	want := []string{
		fmt.Sprintf("%d:queued>cancelled", old.ID),
		fmt.Sprintf("%d:>queued", newer.ID),
	}
	if !slices.Equal(seen, want) || len(events) != 0 {
		t.Errorf("events = %v (+%d), want %v", seen, len(events), want)
	}

	// A keyed registration under a child that is left waiting is rejected too: it could
	// never run, so it must not replace anything.
	current := mustCreate(t, st, keyedParams("current", "group-c", nil))
	for _, parent := range []int64{plain.ID, grandchild.ID} {
		if _, _, err := st.CreateJob(ctx, keyedParams("stranded", "group-c", &parent)); !errors.Is(err, ErrParentTerminal) {
			t.Fatalf("create under stranded parent %d err = %v", parent, err)
		}
		_, _, err := st.HoldJob(ctx, HoldJobParams{RequestID: "stranded-held", Payload: keyedParams("stranded-held", "group-c", &parent)})
		if !errors.Is(err, ErrParentTerminal) {
			t.Fatalf("hold under stranded parent %d err = %v", parent, err)
		}
	}
	if got := mustJob(t, st, current.ID); got.Status != StatusQueued {
		t.Fatalf("current = %s after rejected registrations", got.Status)
	}

	// A child is superseded by a registration with its own key.
	otherNewer := mustCreate(t, st, keyedParams("other-new", "group-b", nil))
	if got := mustJobInfo(t, st, otherNewer.ID).Superseded; !slices.Equal(got, []SupersedeRef{{JobID: other.ID}}) {
		t.Errorf("group-b superseded = %+v", got)
	}

	// The dependency survives: requeueing and completing the parent releases the child.
	if _, err := st.Requeue(ctx, old.ID); err != nil {
		t.Fatal(err)
	}
	claimed := mustClaim(t, st, old.ID)
	res, err := st.Complete(ctx, old.ID, claimed.ClaimedBy, "ok")
	if err != nil || !slices.Equal(res.PromotedChildIDs, []int64{plain.ID}) {
		t.Fatalf("complete = %+v, %v; want %d promoted", res, err, plain.ID)
	}
}

// Independent chains whose keys name their target do not touch each other, either when
// registering or when releasing.
func TestSupersedeTargetQualifiedKeysKeepChainsApart(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	parentA := mustCreate(t, st, CreateJobParams{RequestID: "pa", Title: "review A"})
	parentB := mustCreate(t, st, CreateJobParams{RequestID: "pb", Title: "review B"})
	heldA := mustHold(t, st, keyedParams("a1", "pr1:publish", &parentA.ID))
	heldB := mustHold(t, st, keyedParams("b1", "pr2:publish", &parentB.ID))

	res, err := st.ReleaseKeyedHeld(ctx, heldA.ID, "claude")
	if err != nil || res.Job == nil {
		t.Fatalf("release A = %+v, %v", res, err)
	}
	if got := mustHeld(t, st, heldB.ID); got.Status != HeldStatusHeld {
		t.Fatalf("B held = %s", got.Status)
	}
	mustCreate(t, st, keyedParams("b2", "pr2:publish", &parentB.ID))
	if got := mustJob(t, st, res.Job.ID); got.Status != StatusWaiting {
		t.Fatalf("A job = %s after a registration for B", got.Status)
	}
}

// A replay that drops the key must not bring superseded work back.
func TestSupersedeReplayWithoutKey(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	unkeyed := func(requestID string) CreateJobParams {
		return CreateJobParams{RequestID: requestID, Title: "publish"}
	}

	old := mustHold(t, st, keyedParams("old", "k", nil))
	live := mustHold(t, st, keyedParams("live", "k", nil))

	if job, _, err := st.CreateJob(ctx, unkeyed("old")); !errors.Is(err, ErrConflict) {
		t.Fatalf("unkeyed replay of superseded hold = %+v, %v; want ErrConflict", job, err)
	}
	if _, err := st.GetJobByRequestID(ctx, "old"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a job was created for the superseded entry: %v", err)
	}
	if got := mustHeld(t, st, old.ID); got.Status != HeldStatusCancelled {
		t.Fatalf("old held = %s", got.Status)
	}

	// A hold that is not superseded keeps its key and place when it becomes a job this way.
	job := mustCreate(t, st, unkeyed("live"))
	if job.SupersedeKey != "k" || job.Status != StatusQueued {
		t.Fatalf("job from live hold = %+v", job)
	}
	res, err := st.ReleaseKeyedHeld(ctx, live.ID, "claude")
	if err != nil || res.Created || res.Job.ID != job.ID {
		t.Fatalf("release = %+v, %v", res, err)
	}
	newer := mustCreate(t, st, keyedParams("newer", "k", nil))
	if got := mustJobInfo(t, st, newer.ID).Superseded; len(got) != 1 || got[0].JobID != job.ID {
		t.Fatalf("superseded = %+v", got)
	}
	// A request_id that was never registered with a key stays unkeyed.
	if plain := mustCreate(t, st, unkeyed("fresh")); plain.SupersedeKey != "" || plain.Status != StatusQueued {
		t.Fatalf("fresh unkeyed job = %+v", plain)
	}
}

// A registration limited to its owner leaves everyone else's work alone, when registering,
// releasing and replaying.
func TestSupersedeOwnOnly(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	owned := func(requestID, owner string, ownOnly bool) CreateJobParams {
		p := keyedParams(requestID, "k", nil)
		p.CreatedBy, p.SupersedeOwnOnly = owner, ownOnly
		return p
	}

	cliJob := mustCreate(t, st, owned("cli", "", false))
	// A newer full registration would cancel it, so the limited ones come after.
	otherHeld := mustHold(t, st, owned("other-held", "other", true))
	if got := mustJob(t, st, cliJob.ID); got.Status != StatusQueued {
		t.Fatalf("cli job = %s after a limited registration", got.Status)
	}
	mine := mustCreate(t, st, owned("mine-1", "me", true))
	mineHeld := mustHold(t, st, owned("mine-2", "me", true))
	// An unnamed limited registration owns nothing, not even other unnamed work.
	mustCreate(t, st, owned("nameless", "", true))

	if got := mustJob(t, st, cliJob.ID); got.Status != StatusQueued {
		t.Errorf("cli job = %s", got.Status)
	}
	if got := mustJob(t, st, mine.ID); got.Status != StatusCancelled {
		t.Errorf("own older job = %s, want cancelled", got.Status)
	}
	if got := mustHeld(t, st, otherHeld.ID); got.Status != HeldStatusHeld {
		t.Errorf("other's held = %s", got.Status)
	}
	// Newer registrations by others do not block its release or replay.
	res, err := st.ReleaseKeyedHeld(ctx, otherHeld.ID, "claude")
	if err != nil || res.Job == nil || res.SupersededReason != "" {
		t.Fatalf("release other's held = %+v, %v", res, err)
	}
	if _, created, err := st.CreateJob(ctx, owned("other-held", "other", true)); err != nil || created {
		t.Errorf("replay = %v, %v", created, err)
	}

	// Its own newer registration still does.
	reviveHeld(t, st, mineHeld.ID)
	mine3 := mustHold(t, st, owned("mine-3", "me", true))
	reviveHeld(t, st, mineHeld.ID)
	res, err = st.ReleaseKeyedHeld(ctx, mineHeld.ID, "claude")
	if err != nil || res.Job != nil || !strings.Contains(res.SupersededReason, "request_id=mine-3") {
		t.Fatalf("release own superseded = %+v, %v", res, err)
	}

	full := mustCreate(t, st, owned("full", "boss", false))
	if got := mustHeld(t, st, mine3.ID); got.Status != HeldStatusCancelled {
		t.Errorf("held after full registration = %s", got.Status)
	}
	if got := mustJob(t, st, cliJob.ID); got.Status != StatusCancelled {
		t.Errorf("cli job after full registration = %s", got.Status)
	}
	if got := mustJob(t, st, full.ID); got.Status != StatusQueued {
		t.Errorf("full = %s", got.Status)
	}
}

func TestSupersedeRejectsKeyedDraft(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	old := mustCreate(t, st, keyedParams("old", "k", nil))
	oldHeld := mustHold(t, st, keyedParams("old-held", "j", nil))

	for _, key := range []string{"k", "j"} {
		p := keyedParams("draft-"+key, key, nil)
		p.Draft = true
		if _, _, err := st.CreateJob(ctx, p); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("keyed draft err = %v", err)
		}
		if _, _, err := st.HoldJob(ctx, HoldJobParams{RequestID: p.RequestID, Payload: p}); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("keyed draft hold err = %v", err)
		}
		if _, err := st.GetHeldJobByRequestID(ctx, p.RequestID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("rejected draft was held: %v", err)
		}
	}
	// Dropping the key from a keyed hold does not make a draft of it acceptable.
	if _, _, err := st.CreateJob(ctx, CreateJobParams{RequestID: "old-held", Title: "publish", Draft: true}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("unkeyed draft replay err = %v", err)
	}
	if got := mustJob(t, st, old.ID); got.Status != StatusQueued {
		t.Fatalf("old job = %s", got.Status)
	}
	if got := mustHeld(t, st, oldHeld.ID); got.Status != HeldStatusHeld {
		t.Fatalf("old held = %s", got.Status)
	}
	// With a parent the draft flag has no effect: the job is runnable, so it may supersede.
	parent := mustCreate(t, st, CreateJobParams{RequestID: "parent", Title: "parent"})
	p := keyedParams("child", "k", &parent.ID)
	p.Draft = true
	if job := mustCreate(t, st, p); job.Status != StatusWaiting {
		t.Fatalf("status = %s", job.Status)
	}
	if got := mustJob(t, st, old.ID); got.Status != StatusCancelled {
		t.Fatalf("old job = %s", got.Status)
	}
}

func TestReleaseKeyedHeldChecksForNewerRegistration(t *testing.T) {
	ctx := context.Background()
	// Whatever has happened to the newer registration, the older one must not run.
	for _, newerState := range []string{"held", "queued", "claimed", "done", "cancelled"} {
		t.Run("newer_"+newerState, func(t *testing.T) {
			st := openTestStore(t)
			old := mustHold(t, st, keyedParams("old", "k", nil))

			var want SupersedeRef
			if newerState == "held" {
				want.HeldID = mustHold(t, st, keyedParams("new", "k", nil)).ID
			} else {
				job := mustCreate(t, st, keyedParams("new", "k", nil))
				want.JobID = job.ID
				switch newerState {
				case "claimed":
					mustClaim(t, st, job.ID)
				case "done":
					claimed := mustClaim(t, st, job.ID)
					if _, err := st.Complete(ctx, job.ID, claimed.ClaimedBy, "ok"); err != nil {
						t.Fatal(err)
					}
				case "cancelled":
					if _, err := st.Cancel(ctx, job.ID, "manual"); err != nil {
						t.Fatal(err)
					}
				}
			}
			reviveHeld(t, st, old.ID)

			res, err := st.ReleaseKeyedHeld(ctx, old.ID, "claude")
			if err != nil {
				t.Fatalf("ReleaseKeyedHeld: %v", err)
			}
			if res.Job != nil || res.SupersededReason == "" || res.Held.Status != HeldStatusCancelled ||
				res.Held.CancelReason != res.SupersededReason {
				t.Fatalf("result = %+v held = %+v", res, res.Held)
			}
			if _, err := st.GetJobByRequestID(ctx, "old"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("a job was created for the superseded entry: %v", err)
			}
			info, err := st.SupersedeInfoForHeld(ctx, old.ID)
			if err != nil || info.SupersededBy == nil || *info.SupersededBy != want {
				t.Fatalf("superseded_by = %+v, %v; want %+v", info.SupersededBy, err, want)
			}
		})
	}
}

func TestReleaseKeyedHeldCreatesJobAndKeepsRegistrationOrder(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	first := mustHold(t, st, keyedParams("first", "k", nil))
	second := mustHold(t, st, keyedParams("second", "k", nil))

	if _, err := st.ReleaseKeyedHeld(ctx, first.ID, "claude"); !errors.Is(err, ErrConflict) {
		t.Fatalf("release cancelled entry err = %v, want ErrConflict", err)
	}
	res, err := st.ReleaseKeyedHeld(ctx, second.ID, "codex")
	if err != nil {
		t.Fatalf("ReleaseKeyedHeld: %v", err)
	}
	if !res.Created || res.SupersededReason != "" || res.Job.Status != StatusQueued || res.Job.Pool != "codex" ||
		res.Job.SupersedeKey != "k" || res.Held.Status != HeldStatusReleased || *res.Held.ReleasedJobID != res.Job.ID {
		t.Fatalf("result = %+v job = %+v held = %+v", res, res.Job, res.Held)
	}
	if got := mustJobInfo(t, st, res.Job.ID).Superseded; !slices.Equal(got, []SupersedeRef{{HeldID: first.ID}}) {
		t.Fatalf("released job superseded = %+v", got)
	}

	// A later registration cancels the released, still queued job and reports it by job ID.
	third := mustCreate(t, st, keyedParams("third", "k", nil))
	if got := mustJob(t, st, res.Job.ID); got.Status != StatusCancelled {
		t.Fatalf("released job status = %s", got.Status)
	}
	if got := mustJobInfo(t, st, third.ID).Superseded; len(got) != 1 || got[0].JobID != res.Job.ID {
		t.Fatalf("third superseded = %+v", got)
	}
	// The released entry keeps its history.
	if got := mustHeld(t, st, second.ID); got.Status != HeldStatusReleased {
		t.Fatalf("second held status = %s", got.Status)
	}

	// An operator's requeue revives the job; it is no longer reported as superseded,
	// and the next registration supersedes it again.
	if _, err := st.Requeue(ctx, res.Job.ID); err != nil {
		t.Fatal(err)
	}
	if by := mustJobInfo(t, st, res.Job.ID).SupersededBy; by != nil {
		t.Fatalf("requeued job superseded_by = %+v", by)
	}
	fourth := mustCreate(t, st, keyedParams("fourth", "k", nil))
	if got := mustJobInfo(t, st, fourth.ID).Superseded; len(got) != 2 {
		t.Fatalf("fourth superseded = %+v, want the requeued job and the third", got)
	}
}

func TestSupersedeConcurrentRegistrationsLeaveOne(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	const n = 12
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p := keyedParams(fmt.Sprintf("c%d", i), "k", nil)
			var err error
			if i%2 == 0 {
				_, _, err = st.CreateJob(ctx, p)
			} else {
				_, _, err = st.HoldJob(ctx, HoldJobParams{RequestID: p.RequestID, Payload: p})
			}
			if err != nil {
				t.Errorf("register %d: %v", i, err)
			}
		}()
	}
	wg.Wait()

	var live int
	if err := st.readDB.QueryRowContext(ctx, `
		SELECT (SELECT COUNT(*) FROM jobs WHERE status IN ('queued', 'waiting'))
			+ (SELECT COUNT(*) FROM held_jobs WHERE status = 'held')
	`).Scan(&live); err != nil {
		t.Fatal(err)
	}
	if live != 1 {
		t.Fatalf("unstarted registrations = %d, want 1", live)
	}
}

func TestOpenMigratesDatabaseWithoutSupersedeSchema(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "fleet.db")
	st, err := Open(ctx, dbPath, Options{})
	if err != nil {
		t.Fatal(err)
	}
	job := mustCreate(t, st, CreateJobParams{RequestID: "before", Title: "before"})
	held := mustHold(t, st, CreateJobParams{RequestID: "before-held", Title: "before"})
	for _, ddl := range []string{
		`DROP TABLE supersede_registrations`,
		`ALTER TABLE jobs DROP COLUMN supersede_key`,
		`ALTER TABLE held_jobs DROP COLUMN cancel_reason`,
	} {
		if _, err := st.writeDB.ExecContext(ctx, ddl); err != nil {
			t.Fatalf("%s: %v", ddl, err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	st, err = Open(ctx, dbPath, Options{})
	if err != nil {
		t.Fatalf("reopen old database: %v", err)
	}
	defer st.Close()
	if got := mustJob(t, st, job.ID); got.SupersedeKey != "" || got.Status != StatusQueued {
		t.Fatalf("migrated job = %+v", got)
	}
	if got := mustHeld(t, st, held.ID); got.CancelReason != "" || got.Status != HeldStatusHeld {
		t.Fatalf("migrated held = %+v", got)
	}
	mustCreate(t, st, keyedParams("after", "k", nil))
}

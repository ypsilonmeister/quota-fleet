package runtime

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/quota"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

// docs/20: both release paths check again for a newer registration with the same key.
func TestGateCheckerCancelsSupersededHeldInsteadOfReleasing(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name, scope string
		manual      bool
	}{
		{"periodic", "", false}, {"manual", "", true},
		{"periodic_scoped", "pr1", false}, {"manual_scoped", "pr1", true},
	} {
		manual := c.manual
		t.Run(c.name, func(t *testing.T) {
			st, dir := openTestStore(t, "gate-supersede")
			now := time.Date(2026, 11, 25, 1, 0, 0, 0, time.UTC)
			reset := now.Add(24 * time.Hour)
			short := 50
			checker := NewGateChecker(GateCheckerOptions{
				Store:    st,
				GateMode: config.GateModeEnforce,
				Pools:    map[string]config.PoolConfig{"claude": {Provider: "claude-usage"}},
				Now:      func() time.Time { return now },
				Logger:   discardLogger(),
				SnapshotLoader: func() (*quota.Snapshot, error) {
					return &quota.Snapshot{MeasuredAt: now, Pools: map[string]quota.PoolSnapshot{
						"claude": {WeeklyRemainingPercent: 50, WeeklyResetAt: &reset, ShortRemainingPercent: &short, MeasuredAt: now},
					}}, nil
				},
			})

			hold := func(requestID string) *store.HeldJob {
				held, _, err := st.HoldJob(ctx, store.HoldJobParams{
					RequestID: requestID,
					Pool:      "claude",
					Payload: store.CreateJobParams{
						RequestID: requestID, Title: "publish", Pool: "claude", SupersedeScope: c.scope, SupersedeKey: "publish",
					},
				})
				if err != nil {
					t.Fatalf("HoldJob(%s): %v", requestID, err)
				}
				return held
			}
			old := hold("round-1")
			newer := hold("round-2")

			// Registration already cancelled the old entry. Put it back to test the
			// release-time check on its own.
			db, err := sql.Open("sqlite", filepath.Join(dir, "fleet.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err := db.ExecContext(ctx,
				`UPDATE held_jobs SET status = 'held', cancel_reason = '', cancelled_at = NULL WHERE id = ?`, old.ID); err != nil {
				t.Fatal(err)
			}

			if manual {
				res, err := checker.ReleaseByID(ctx, old.ID)
				if err != nil || res.Released || res.Job != nil || !strings.HasPrefix(res.Reason, "superseded:") ||
					res.Held.Status != store.HeldStatusCancelled {
					t.Fatalf("ReleaseByID = (%+v, %v)", res, err)
				}
				if _, err := checker.ReleaseByID(ctx, old.ID); !errors.Is(err, store.ErrConflict) {
					t.Fatalf("second release err = %v, want ErrConflict", err)
				}
				if res, err = checker.ReleaseByID(ctx, newer.ID); err != nil || !res.Released {
					t.Fatalf("release newer = (%+v, %v)", res, err)
				}
			} else if err := checker.CheckOnce(ctx); err != nil {
				t.Fatalf("CheckOnce: %v", err)
			}

			gotOld, err := st.GetHeldJob(ctx, old.ID)
			if err != nil || gotOld.Status != store.HeldStatusCancelled || gotOld.LastError != "" ||
				!strings.HasPrefix(gotOld.CancelReason, "superseded:") {
				t.Fatalf("old held = %+v, %v", gotOld, err)
			}
			if _, err := st.GetJobByRequestID(ctx, "round-1"); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("superseded entry produced a job: %v", err)
			}
			gotNewer, err := st.GetHeldJob(ctx, newer.ID)
			if err != nil || gotNewer.Status != store.HeldStatusReleased {
				t.Fatalf("newer held = %+v, %v", gotNewer, err)
			}
			job, err := st.GetJob(ctx, *gotNewer.ReleasedJobID)
			if err != nil || job.Status != store.StatusQueued || job.SupersedeKey != "publish" {
				t.Fatalf("newer job = %+v, %v", job, err)
			}
			decisions, err := st.ListGateDecisionsFiltered(ctx, store.GateDecisionsFilter{HeldID: newer.ID})
			if err != nil || len(decisions) != 1 || decisions[0].JobID != job.ID {
				t.Fatalf("release decisions = %+v, %v", decisions, err)
			}
		})
	}
}

// A keyed entry whose parent fails stays held with the error recorded, like any other.
func TestGateCheckerKeyedReleaseFailureStaysHeld(t *testing.T) {
	ctx := context.Background()
	st, _ := openTestStore(t, "gate-supersede-fail")
	parent, _, err := st.CreateJob(ctx, store.CreateJobParams{RequestID: "parent", Title: "parent"})
	if err != nil {
		t.Fatal(err)
	}
	held, _, err := st.HoldJob(ctx, store.HoldJobParams{
		RequestID: "child",
		Pool:      "local",
		Payload: store.CreateJobParams{
			RequestID: "child", Title: "publish", Pool: "local", SupersedeScope: "pr1", SupersedeKey: "publish", ParentID: &parent.ID,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Cancel(ctx, parent.ID, "manual"); err != nil {
		t.Fatal(err)
	}
	checker := NewGateChecker(GateCheckerOptions{
		Store:    st,
		GateMode: config.GateModeEnforce,
		Pools:    map[string]config.PoolConfig{"local": {Provider: "local", Unmeasured: true}},
		Logger:   discardLogger(),
	})
	res, err := checker.ReleaseByID(ctx, held.ID)
	if !errors.Is(err, store.ErrParentTerminal) || res.Released {
		t.Fatalf("ReleaseByID = (%+v, %v)", res, err)
	}
	got, err := st.GetHeldJob(ctx, held.ID)
	if err != nil || got.Status != store.HeldStatusHeld || got.ErrorCount != 1 || got.LastError == "" {
		t.Fatalf("held = %+v, %v", got, err)
	}
}

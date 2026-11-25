package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestDecisionLogIsAddedToExistingDatabase(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "fleet.db")
	st, err := Open(ctx, path, Options{InstanceID: "x"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	job, _, err := st.CreateJob(ctx, CreateJobParams{RequestID: "r1", Title: "kept"})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	// Make the database look like one created before decision_log existed.
	if _, err := st.writeDB.ExecContext(ctx, `DROP TABLE decision_log`); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	for round := 1; round <= 2; round++ {
		st, err = Open(ctx, path, Options{InstanceID: "x"})
		if err != nil {
			t.Fatalf("reopen %d: %v", round, err)
		}
		if _, err := st.InsertDecisionLog(ctx, DecisionLogEntry{
			JobID: &job.ID, Verdict: "changes_needed", Score: 0.25, Model: "m",
		}); err != nil {
			t.Fatalf("insert %d: %v", round, err)
		}
		entries, err := st.ListDecisionLog(ctx, 0)
		if err != nil {
			t.Fatalf("list %d: %v", round, err)
		}
		if len(entries) != round {
			t.Fatalf("entries after round %d = %d", round, len(entries))
		}
		if _, err := st.GetJob(ctx, job.ID); err != nil {
			t.Fatalf("existing job lost: %v", err)
		}
		if err := st.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}
}

func TestInsertDecisionLogRejectsUnknownVerdict(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "fleet.db"), Options{InstanceID: "x"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	for _, e := range []DecisionLogEntry{
		{Verdict: "maybe", Model: "m"},
		{Verdict: "", Model: "m"},
		{Verdict: "ok_to_merge", Model: ""},
	} {
		if _, err := st.InsertDecisionLog(ctx, e); err == nil {
			t.Errorf("InsertDecisionLog(%+v) succeeded", e)
		}
	}
}

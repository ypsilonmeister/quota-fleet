package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestValidateEffort(t *testing.T) {
	cases := []struct {
		agent, effort string
		ok            bool
	}{
		{"codex", "", true},
		{"claude", "", true},
		{"agy", "", true},
		{"", "", true},
		{"custom", "", true},
		{"codex", "low", true},
		{"codex", "medium", true},
		{"codex", "high", true},
		{"codex", "xhigh", true},
		{"claude", "low", true},
		{"claude", "xhigh", true},
		{" codex ", " high ", true},
		{"codex", "max", false},
		{"codex", "HIGH", false},
		{"codex", "minimal", false},
		{"codex", "bogus", false},
		{"claude", "none", false},
		{"agy", "high", false},
		{"agy", "low", false},
		{"", "high", false},
		{"custom", "high", false},
	}
	for _, c := range cases {
		err := ValidateEffort(c.agent, c.effort)
		if c.ok && err != nil {
			t.Errorf("ValidateEffort(%q,%q) = %v, want nil", c.agent, c.effort, err)
		}
		if !c.ok {
			if err == nil {
				t.Errorf("ValidateEffort(%q,%q) = nil, want error", c.agent, c.effort)
			} else if !errors.Is(err, ErrInvalidInput) {
				t.Errorf("ValidateEffort(%q,%q) = %v, want ErrInvalidInput", c.agent, c.effort, err)
			}
		}
	}
}

func TestCreateJobStoresEffort(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	job, created, err := st.CreateJob(ctx, CreateJobParams{RequestID: "e1", Title: "t", Agent: "codex", Effort: "xhigh"})
	if err != nil || !created {
		t.Fatalf("CreateJob = (%v, %v)", created, err)
	}
	if job.Effort != "xhigh" {
		t.Fatalf("job.Effort = %q, want xhigh", job.Effort)
	}
	got, err := st.GetJob(ctx, job.ID)
	if err != nil || got.Effort != "xhigh" {
		t.Fatalf("GetJob effort = %q (%v), want xhigh", got.Effort, err)
	}

	plain, _, err := st.CreateJob(ctx, CreateJobParams{RequestID: "e2", Title: "t", Agent: "codex"})
	if err != nil || plain.Effort != "" {
		t.Fatalf("unset effort = %q (%v), want empty", plain.Effort, err)
	}

	for _, p := range []CreateJobParams{
		{RequestID: "e3", Title: "t", Agent: "agy", Model: "gemini-4-neon-medium", Effort: "high"},
		{RequestID: "e4", Title: "t", Agent: "codex", Effort: "max"},
	} {
		if _, _, err := st.CreateJob(ctx, p); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("CreateJob(%+v) err = %v, want ErrInvalidInput", p, err)
		}
	}
}

func TestHoldJobKeepsEffortInPayload(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	held, created, err := st.HoldJob(ctx, HoldJobParams{
		RequestID: "h1",
		Pool:      "codex",
		Payload:   CreateJobParams{Title: "t", Agent: "codex", Effort: "high"},
		Reason:    "test",
	})
	if err != nil || !created {
		t.Fatalf("HoldJob = (%v, %v)", created, err)
	}
	reloaded, err := st.GetHeldJob(ctx, held.ID)
	if err != nil {
		t.Fatalf("GetHeldJob: %v", err)
	}
	if reloaded.Payload.Effort != "high" {
		t.Fatalf("held payload effort = %q, want high", reloaded.Payload.Effort)
	}

	// Release path: the runtime re-creates the job from the held payload.
	params := reloaded.Payload
	params.RequestID = reloaded.RequestID
	job, _, err := st.CreateJob(ctx, params)
	if err != nil || job.Effort != "high" {
		t.Fatalf("released job effort = %q (%v), want high", job.Effort, err)
	}

	if _, _, err := st.HoldJob(ctx, HoldJobParams{
		RequestID: "h2",
		Pool:      "agy",
		Payload:   CreateJobParams{Title: "t", Agent: "agy", Effort: "high"},
	}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("HoldJob(agy+effort) err = %v, want ErrInvalidInput", err)
	}
}

func TestEffortColumnMigratesExistingDB(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "fleet.db")
	st, err := Open(context.Background(), dbPath, Options{InstanceID: "m"})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := st.writeDB.Exec(`ALTER TABLE jobs DROP COLUMN effort`); err != nil {
		t.Skipf("sqlite cannot drop column here: %v", err)
	}
	if _, err := st.writeDB.Exec(`INSERT INTO jobs (request_id, title, status, created_at, updated_at) VALUES ('old', 'old', 'queued', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}
	_ = st.Close()

	st2, err := Open(context.Background(), dbPath, Options{InstanceID: "m"})
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	defer st2.Close()
	j, err := st2.GetJobByRequestID(context.Background(), "old")
	if err != nil || j.Effort != "" {
		t.Fatalf("legacy job effort = %q (%v), want empty", j.Effort, err)
	}
}

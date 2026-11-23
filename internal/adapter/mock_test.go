package adapter

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

func TestMockAdapterLifecycleAndErrors(t *testing.T) {
	ctx := context.Background()
	m := NewMock(MockOptions{
		Default: MockBehavior{
			ExitAfterPolls: 2,
			ExitCode:       0,
		},
	})

	job := &store.Job{ID: 1, Title: "test job", Agent: "claude", Host: "ubuntu"}

	// Before launch, Status returns Missing.
	st, err := m.Status(ctx, job)
	if err != nil || st.State != StateMissing {
		t.Fatalf("Status before launch = (%+v, %v), want Missing", st, err)
	}

	if err := m.PrepareWorkdir(ctx, job); err != nil {
		t.Fatalf("PrepareWorkdir: %v", err)
	}
	info, err := m.Launch(ctx, job)
	if err != nil || info.PID <= 0 || info.SessionID == "" {
		t.Fatalf("Launch = (%+v, %v)", info, err)
	}

	// Poll 1: Running.
	st, err = m.Status(ctx, job)
	if err != nil || st.State != StateRunning {
		t.Fatalf("Status poll 1 = (%+v, %v), want Running", st, err)
	}
	// Poll 2: Exited.
	st, err = m.Status(ctx, job)
	if err != nil || st.State != StateExited || st.ExitCode != 0 {
		t.Fatalf("Status poll 2 = (%+v, %v), want Exited(0)", st, err)
	}

	ev, err := m.CollectEvidence(ctx, job)
	if err != nil || !ev.HasDiff || ev.ExitCode != 0 {
		t.Fatalf("CollectEvidence = (%+v, %v)", ev, err)
	}

	// Failure modes via per-job overrides.
	prepErr := errors.New("prep boom")
	launchErr := errors.New("launch boom")
	job2 := &store.Job{ID: 2, Title: "prep fail"}
	m.SetJobBehavior(job2.ID, MockBehavior{PrepareErr: prepErr})
	if err := m.PrepareWorkdir(ctx, job2); !errors.Is(err, prepErr) {
		t.Fatalf("PrepareWorkdir err = %v, want %v", err, prepErr)
	}

	job3 := &store.Job{ID: 3, Title: "launch fail"}
	m.SetJobBehavior(job3.ID, MockBehavior{LaunchErr: launchErr})
	if _, err := m.Launch(ctx, job3); !errors.Is(err, launchErr) {
		t.Fatalf("Launch err = %v, want %v", err, launchErr)
	}

	job4 := &store.Job{ID: 4, Title: "forced missing"}
	m.SetJobBehavior(job4.ID, MockBehavior{ForceMissing: true})
	if _, err := m.Launch(ctx, job4); err != nil {
		t.Fatalf("Launch job4: %v", err)
	}
	if st, err := m.Status(ctx, job4); err != nil || st.State != StateMissing {
		t.Fatalf("Status job4 = (%+v, %v), want Missing", st, err)
	}
}

func TestMockAdapterConcurrentSafety(t *testing.T) {
	ctx := context.Background()
	m := NewMock(MockOptions{
		Default: MockBehavior{ExitAfterPolls: 3},
	})

	var wg sync.WaitGroup
	for i := 1; i <= 16; i++ {
		id := int64(i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			job := &store.Job{ID: id, Title: "concurrent"}
			_ = m.PrepareWorkdir(ctx, job)
			_, _ = m.Launch(ctx, job)
			for p := 0; p < 4; p++ {
				_, _ = m.Status(ctx, job)
			}
			_, _ = m.CollectEvidence(ctx, job)
		}()
	}
	wg.Wait()

	if m.ActiveRunning() != 0 {
		t.Errorf("ActiveRunning = %d, want 0 after all jobs exited", m.ActiveRunning())
	}
	if m.TotalStatusCalls() != 16*4 {
		t.Errorf("TotalStatusCalls = %d, want %d", m.TotalStatusCalls(), 16*4)
	}
}

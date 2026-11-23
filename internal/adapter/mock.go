package adapter

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

// ErrNilJob is returned when a nil *store.Job is passed to an adapter method.
var ErrNilJob = errors.New("adapter: job must not be nil")

// MockBehavior configures how MockAdapter responds for a job (either as the default
// or overridden for a specific job ID).
type MockBehavior struct {
	// PrepareErr, if non-nil, is returned by PrepareWorkdir.
	PrepareErr error
	// LaunchErr, if non-nil, is returned by Launch.
	LaunchErr error
	// StatusErr, if non-nil, is returned by Status.
	StatusErr error
	// CollectErr, if non-nil, is returned by CollectEvidence.
	CollectErr error
	// ForceMissing makes Status always return StateMissing even if launched.
	ForceMissing bool
	// StayRunning keeps Status returning StateRunning until FinishJob or DropJob is called.
	StayRunning bool
	// ExitAfterPolls controls after how many Status calls a running job transitions
	// to StateExited (when StayRunning is false). Values <= 0 default to 1.
	ExitAfterPolls int
	// ExitCode is the exit code reported once the job transitions to StateExited.
	ExitCode int
	// Evidence, if non-nil, overrides the default Evidence returned by CollectEvidence.
	Evidence *Evidence
}

// MockOptions configures a new MockAdapter.
type MockOptions struct {
	Default MockBehavior
}

type mockRun struct {
	pid         int
	sessionID   string
	state       StateKind
	exitCode    int
	statusPolls int
}

// MockAdapter is a thread-safe in-memory Adapter implementation for testing and Phase 1 serve.
type MockAdapter struct {
	mu            sync.Mutex
	defaultBeh    MockBehavior
	jobBehaviors  map[int64]MockBehavior
	runs          map[int64]*mockRun
	nextPID       int
	prepareCalls  map[int64]int
	launchCalls   map[int64]int
	statusCalls   map[int64]int
	collectCalls  map[int64]int
	totalStatus   int
	activeRunning int
	maxRunning    int
}

// NewMock constructs a MockAdapter with the provided options.
func NewMock(opts MockOptions) *MockAdapter {
	return &MockAdapter{
		defaultBeh:   opts.Default,
		jobBehaviors: make(map[int64]MockBehavior),
		runs:         make(map[int64]*mockRun),
		nextPID:      1000,
		prepareCalls: make(map[int64]int),
		launchCalls:  make(map[int64]int),
		statusCalls:  make(map[int64]int),
		collectCalls: make(map[int64]int),
	}
}

// SetDefaultBehavior replaces the default behavior for jobs without an explicit override.
func (m *MockAdapter) SetDefaultBehavior(beh MockBehavior) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.defaultBeh = beh
}

// SetJobBehavior configures a specific behavior override for jobID.
func (m *MockAdapter) SetJobBehavior(jobID int64, beh MockBehavior) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.jobBehaviors[jobID] = beh
}

// ClearJobBehavior removes any behavior override for jobID.
func (m *MockAdapter) ClearJobBehavior(jobID int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.jobBehaviors, jobID)
}

// FinishJob transitions a currently tracked job directly to StateExited with exitCode.
func (m *MockAdapter) FinishJob(jobID int64, exitCode int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	run, ok := m.runs[jobID]
	if !ok {
		m.runs[jobID] = &mockRun{
			pid:      m.allocPIDLocked(),
			state:    StateExited,
			exitCode: exitCode,
		}
		return
	}
	if run.state == StateRunning {
		m.activeRunning--
	}
	run.state = StateExited
	run.exitCode = exitCode
}

// DropJob removes the in-memory run record for jobID so subsequent Status calls return StateMissing.
func (m *MockAdapter) DropJob(jobID int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if run, ok := m.runs[jobID]; ok {
		if run.state == StateRunning {
			m.activeRunning--
		}
		delete(m.runs, jobID)
	}
}

// PrepareWorkdir records the call and returns the configured PrepareErr if any.
func (m *MockAdapter) PrepareWorkdir(ctx context.Context, job *store.Job) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if job == nil {
		return ErrNilJob
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.prepareCalls[job.ID]++
	beh := m.behaviorLocked(job.ID)
	return beh.PrepareErr
}

// Launch marks the job as running in memory and returns synthetic LaunchInfo.
func (m *MockAdapter) Launch(ctx context.Context, job *store.Job) (LaunchInfo, error) {
	if err := ctx.Err(); err != nil {
		return LaunchInfo{}, err
	}
	if job == nil {
		return LaunchInfo{}, ErrNilJob
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.launchCalls[job.ID]++
	beh := m.behaviorLocked(job.ID)
	if beh.LaunchErr != nil {
		return LaunchInfo{}, beh.LaunchErr
	}

	if prev, ok := m.runs[job.ID]; ok && prev.state == StateRunning {
		m.activeRunning--
	}

	pid := m.allocPIDLocked()
	sessionID := fmt.Sprintf("mock-sess-%d-%d", job.ID, pid)
	m.runs[job.ID] = &mockRun{
		pid:       pid,
		sessionID: sessionID,
		state:     StateRunning,
		exitCode:  beh.ExitCode,
	}
	m.activeRunning++
	if m.activeRunning > m.maxRunning {
		m.maxRunning = m.activeRunning
	}

	return LaunchInfo{
		PID:       pid,
		SessionID: sessionID,
		Meta: map[string]string{
			"adapter": "mock",
			"agent":   job.Agent,
			"host":    job.Host,
		},
	}, nil
}

// Status reports Running, Exited, or Missing for job according to its configured MockBehavior.
func (m *MockAdapter) Status(ctx context.Context, job *store.Job) (RunState, error) {
	if err := ctx.Err(); err != nil {
		return RunState{}, err
	}
	if job == nil {
		return RunState{}, ErrNilJob
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.statusCalls[job.ID]++
	m.totalStatus++

	beh := m.behaviorLocked(job.ID)
	if beh.StatusErr != nil {
		return RunState{}, beh.StatusErr
	}
	if beh.ForceMissing {
		return RunState{State: StateMissing, Detail: "mock forced missing"}, nil
	}

	run, ok := m.runs[job.ID]
	if !ok {
		return RunState{State: StateMissing, Detail: "no active mock run"}, nil
	}
	if run.state == StateExited {
		return RunState{State: StateExited, ExitCode: run.exitCode}, nil
	}

	run.statusPolls++
	if !beh.StayRunning {
		threshold := beh.ExitAfterPolls
		if threshold <= 0 {
			threshold = 1
		}
		if run.statusPolls >= threshold {
			run.state = StateExited
			run.exitCode = beh.ExitCode
			m.activeRunning--
			return RunState{State: StateExited, ExitCode: run.exitCode}, nil
		}
	}

	return RunState{State: StateRunning}, nil
}

// CollectEvidence returns configured or default mock Evidence for job.
func (m *MockAdapter) CollectEvidence(ctx context.Context, job *store.Job) (Evidence, error) {
	if err := ctx.Err(); err != nil {
		return Evidence{}, err
	}
	if job == nil {
		return Evidence{}, ErrNilJob
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.collectCalls[job.ID]++

	beh := m.behaviorLocked(job.ID)
	if beh.CollectErr != nil {
		return Evidence{}, beh.CollectErr
	}
	if beh.Evidence != nil {
		return cloneEvidence(*beh.Evidence), nil
	}

	exitCode := beh.ExitCode
	if run, ok := m.runs[job.ID]; ok {
		exitCode = run.exitCode
	}
	return Evidence{
		HasDiff:     true,
		DiffSummary: fmt.Sprintf("mock diff for job #%d (%s)", job.ID, job.Title),
		Artifacts: map[string]bool{
			"result.txt": true,
		},
		FinalReport: fmt.Sprintf("mock run completed for job #%d", job.ID),
		ExitCode:    exitCode,
	}, nil
}

// PrepareCalls returns how many times PrepareWorkdir was called for jobID.
func (m *MockAdapter) PrepareCalls(jobID int64) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.prepareCalls[jobID]
}

// LaunchCalls returns how many times Launch was called for jobID.
func (m *MockAdapter) LaunchCalls(jobID int64) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.launchCalls[jobID]
}

// StatusCalls returns how many times Status was called for jobID.
func (m *MockAdapter) StatusCalls(jobID int64) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.statusCalls[jobID]
}

// TotalStatusCalls returns the total number of Status calls across all jobs.
func (m *MockAdapter) TotalStatusCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.totalStatus
}

// CollectCalls returns how many times CollectEvidence was called for jobID.
func (m *MockAdapter) CollectCalls(jobID int64) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.collectCalls[jobID]
}

// MaxConcurrentRunning returns the peak number of concurrently running mock jobs observed.
func (m *MockAdapter) MaxConcurrentRunning() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.maxRunning
}

// ActiveRunning returns the current number of running mock jobs.
func (m *MockAdapter) ActiveRunning() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.activeRunning
}

func (m *MockAdapter) behaviorLocked(jobID int64) MockBehavior {
	if beh, ok := m.jobBehaviors[jobID]; ok {
		return beh
	}
	return m.defaultBeh
}

func (m *MockAdapter) allocPIDLocked() int {
	m.nextPID++
	return m.nextPID
}

func cloneEvidence(ev Evidence) Evidence {
	out := ev
	if ev.Artifacts != nil {
		out.Artifacts = make(map[string]bool, len(ev.Artifacts))
		for k, v := range ev.Artifacts {
			out.Artifacts[k] = v
		}
	}
	if ev.CommandOutputs != nil {
		out.CommandOutputs = make(map[string]string, len(ev.CommandOutputs))
		for k, v := range ev.CommandOutputs {
			out.CommandOutputs[k] = v
		}
	}
	return out
}

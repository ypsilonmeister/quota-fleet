package adapter

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

func waitForStatus(t *testing.T, ctx context.Context, a *SSHAdapter, job *store.Job, want StateKind, timeout time.Duration) RunState {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last RunState
	var lastErr error
	for {
		st, err := a.Status(ctx, job)
		last, lastErr = st, err
		if err == nil && st.State == want {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for job #%d state=%s (last=(%+v, err=%v))", job.ID, want, last, lastErr)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Regression test (a) on Linux: two sequential jobs share the exact same workdir;
// launching and completing job 2 must leave job 1's prompt, run script, pid, exit,
// and log intact so job 1's status and evidence remain accurately collectible, while
// deliverables (e.g. summary.md) continue to be written in workdir itself.
func TestPerJobRunArtifactsSequentialSameWorkdirLinux(t *testing.T) {
	ctx := context.Background()
	sharedWorkdir := t.TempDir()
	binDir := t.TempDir()
	fakeAgent := filepath.Join(binDir, "fake-agent.sh")

	// The fake agent reads prompt text from stdin, verifies CWD is workdir,
	// writes summary.md in CWD (workdir), prints its prompt marker to stdout,
	// and exits with the numeric code specified on the first line ("CODE:<n>").
	script := `#!/bin/sh
set -eu
prompt="$(cat)"
printf 'cwd=%s\n' "$(pwd)"
printf 'prompt=%s\n' "$prompt"
printf 'deliverable from %s\n' "$prompt" > summary.md
code="$(printf '%s\n' "$prompt" | sed -n 's/^CODE://p' | head -n 1)"
exit "${code:-0}"
`
	if err := os.WriteFile(fakeAgent, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake-agent.sh: %v", err)
	}

	cfg := &config.Config{
		Hosts: map[string]config.HostConfig{
			"local-linux": {
				Transport: "local",
				OS:        "linux",
				Agents: map[string]config.HostAgentOverride{
					"claude": {Command: fakeAgent},
				},
			},
		},
		Agents: map[string]config.AgentConfig{
			"claude": {
				Launch:      "claude",
				PromptStyle: config.PromptStyleStdin,
			},
		},
	}

	a := NewSSHAdapter(cfg, SSHAdapterOptions{
		Transport: NewLocalTransport(LocalTransportOptions{}),
	})

	job1 := &store.Job{
		ID:           101,
		Title:        "First job in shared workdir",
		Agent:        "claude",
		Host:         "local-linux",
		Workdir:      sharedWorkdir,
		Instructions: "CODE:0\nJOB_1_UNIQUE_MARKER",
	}
	job2 := &store.Job{
		ID:           102,
		Title:        "Second job in shared workdir",
		Agent:        "claude",
		Host:         "local-linux",
		Workdir:      sharedWorkdir,
		Instructions: "CODE:7\nJOB_2_UNIQUE_MARKER",
	}

	// Launch job 1 and wait until it exits cleanly.
	info1, err := a.Launch(ctx, job1)
	if err != nil {
		t.Fatalf("Launch job1: %v", err)
	}
	wantRunDir1 := filepath.Join(sharedWorkdir, ".fleet", "runs", "101")
	if info1.Meta["run_dir"] != wantRunDir1 {
		t.Errorf("job1 run_dir = %q, want %q", info1.Meta["run_dir"], wantRunDir1)
	}
	st1Before := waitForStatus(t, ctx, a, job1, StateExited, 5*time.Second)
	if st1Before.ExitCode != 0 {
		t.Fatalf("job1 exit code before job2 = %d, want 0", st1Before.ExitCode)
	}

	// Launch job 2 in the SAME workdir and wait until it exits with code 7.
	info2, err := a.Launch(ctx, job2)
	if err != nil {
		t.Fatalf("Launch job2: %v", err)
	}
	wantRunDir2 := filepath.Join(sharedWorkdir, ".fleet", "runs", "102")
	if info2.Meta["run_dir"] != wantRunDir2 {
		t.Errorf("job2 run_dir = %q, want %q", info2.Meta["run_dir"], wantRunDir2)
	}
	st2 := waitForStatus(t, ctx, a, job2, StateExited, 5*time.Second)
	if st2.ExitCode != 7 {
		t.Fatalf("job2 exit code = %d, want 7", st2.ExitCode)
	}

	// Verify workdir root does NOT contain per-job control files, while summary.md IS at workdir root.
	for _, ctrl := range []string{promptFileName, runScriptName, pidFileName, exitFileName, logFileName} {
		if _, err := os.Stat(filepath.Join(sharedWorkdir, ctrl)); !os.IsNotExist(err) {
			t.Errorf("unexpected control file %q in workdir root (err=%v)", ctrl, err)
		}
	}
	if _, err := os.Stat(filepath.Join(sharedWorkdir, "summary.md")); err != nil {
		t.Errorf("expected deliverable summary.md in workdir root: %v", err)
	}

	// After job 2 has run, job 1's Status and CollectEvidence must still be intact and isolated.
	st1After, err := a.Status(ctx, job1)
	if err != nil {
		t.Fatalf("Status job1 after job2: %v", err)
	}
	if st1After.State != StateExited || st1After.ExitCode != 0 {
		t.Fatalf("job1 status after job2 = %+v, want StateExited(0)", st1After)
	}

	ev1, err := a.CollectEvidence(ctx, job1)
	if err != nil {
		t.Fatalf("CollectEvidence job1: %v", err)
	}
	if ev1.ExitCode != 0 {
		t.Errorf("ev1.ExitCode = %d, want 0", ev1.ExitCode)
	}
	if !strings.Contains(ev1.FinalReport, "JOB_1_UNIQUE_MARKER") {
		t.Errorf("ev1.FinalReport missing job 1 marker:\n%s", ev1.FinalReport)
	}
	if strings.Contains(ev1.FinalReport, "JOB_2_UNIQUE_MARKER") {
		t.Errorf("ev1.FinalReport polluted with job 2 output:\n%s", ev1.FinalReport)
	}
	if !strings.Contains(ev1.FinalReport, "cwd="+sharedWorkdir) {
		t.Errorf("job 1 did not execute with CWD=workdir (%s):\n%s", sharedWorkdir, ev1.FinalReport)
	}
	if !ev1.Artifacts["agent.log"] || !ev1.Artifacts["agent.exit"] || !ev1.Artifacts["summary.md"] {
		t.Errorf("ev1.Artifacts = %+v, want agent.log, agent.exit, summary.md true", ev1.Artifacts)
	}

	ev2, err := a.CollectEvidence(ctx, job2)
	if err != nil {
		t.Fatalf("CollectEvidence job2: %v", err)
	}
	if ev2.ExitCode != 7 {
		t.Errorf("ev2.ExitCode = %d, want 7", ev2.ExitCode)
	}
	if !strings.Contains(ev2.FinalReport, "JOB_2_UNIQUE_MARKER") || strings.Contains(ev2.FinalReport, "JOB_1_UNIQUE_MARKER") {
		t.Errorf("ev2.FinalReport unexpected:\n%s", ev2.FinalReport)
	}
}

// Regression test (c) on Linux: while another job's process is actively running in the same
// workdir (and even if a stale root agent.pid holds a live PID), Status for another job in
// that workdir must inspect only its own run directory and never falsely report Running.
func TestStatusDoesNotMisidentifyOtherJobRunningInSameWorkdirLinux(t *testing.T) {
	ctx := context.Background()
	sharedWorkdir := t.TempDir()
	binDir := t.TempDir()
	sleeper := filepath.Join(binDir, "sleep-agent.sh")
	if err := os.WriteFile(sleeper, []byte("#!/bin/sh\nsleep 30\n"), 0o700); err != nil {
		t.Fatalf("write sleep-agent.sh: %v", err)
	}

	cfg := &config.Config{
		Hosts: map[string]config.HostConfig{
			"local-linux": {
				Transport: "local",
				OS:        "linux",
				Agents: map[string]config.HostAgentOverride{
					"claude": {Command: sleeper},
				},
			},
		},
		Agents: map[string]config.AgentConfig{
			"claude": {
				Launch:      "claude",
				PromptStyle: config.PromptStyleStdin,
			},
		},
	}
	a := NewSSHAdapter(cfg, SSHAdapterOptions{
		Transport: NewLocalTransport(LocalTransportOptions{}),
	})

	runningJob := &store.Job{
		ID:           302,
		Title:        "Active sibling job",
		Agent:        "claude",
		Host:         "local-linux",
		Workdir:      sharedWorkdir,
		Instructions: "stay alive",
	}
	info, err := a.Launch(ctx, runningJob)
	if err != nil {
		t.Fatalf("Launch runningJob: %v", err)
	}
	t.Cleanup(func() {
		// Kill entire detached process group created by setsid.
		_ = syscall.Kill(-info.PID, syscall.SIGKILL)
		_ = syscall.Kill(info.PID, syscall.SIGKILL)
	})

	// Confirm the sibling job itself IS running.
	stRunning, err := a.Status(ctx, runningJob)
	if err != nil || stRunning.State != StateRunning {
		t.Fatalf("runningJob Status = (%+v, %v), want StateRunning", stRunning, err)
	}

	// Plant a legacy root agent.pid containing the LIVE PID to ensure Status ignores workdir root.
	if err := os.WriteFile(filepath.Join(sharedWorkdir, pidFileName), []byte(fmt.Sprintf("%d\n", info.PID)), 0o600); err != nil {
		t.Fatalf("write legacy root agent.pid: %v", err)
	}

	// 1. Target job with no run directory -> Missing (NOT Running).
	noFilesJob := &store.Job{ID: 301, Host: "local-linux", Workdir: sharedWorkdir}
	stNoFiles, err := a.Status(ctx, noFilesJob)
	if err != nil || stNoFiles.State != StateMissing {
		t.Fatalf("noFilesJob Status = (%+v, %v), want StateMissing", stNoFiles, err)
	}

	// 2. Target job whose own agent.exit is recorded -> Exited (NOT Running).
	exitedJob := &store.Job{ID: 303, Host: "local-linux", Workdir: sharedWorkdir}
	runDir303 := filepath.Join(sharedWorkdir, ".fleet", "runs", "303")
	if err := os.MkdirAll(runDir303, 0o700); err != nil {
		t.Fatalf("mkdir runDir303: %v", err)
	}
	if err := os.WriteFile(filepath.Join(runDir303, exitFileName), []byte("0\n"), 0o600); err != nil {
		t.Fatalf("write job303 agent.exit: %v", err)
	}
	stExited, err := a.Status(ctx, exitedJob)
	if err != nil || stExited.State != StateExited || stExited.ExitCode != 0 {
		t.Fatalf("exitedJob Status = (%+v, %v), want StateExited(0)", stExited, err)
	}

	// 3. Target job with corrupt agent.exit -> Missing (routes to reset, NOT Running or error).
	corruptJob := &store.Job{ID: 304, Host: "local-linux", Workdir: sharedWorkdir}
	runDir304 := filepath.Join(sharedWorkdir, ".fleet", "runs", "304")
	if err := os.MkdirAll(runDir304, 0o700); err != nil {
		t.Fatalf("mkdir runDir304: %v", err)
	}
	if err := os.WriteFile(filepath.Join(runDir304, exitFileName), []byte("corrupt\n"), 0o600); err != nil {
		t.Fatalf("write job304 agent.exit: %v", err)
	}
	stCorrupt, err := a.Status(ctx, corruptJob)
	if err != nil || stCorrupt.State != StateMissing {
		t.Fatalf("corruptJob Status = (%+v, %v), want StateMissing", stCorrupt, err)
	}

	// 4. Target job with invalid PID ("0") -> Missing (must NOT pass kill -0 0!).
	badPIDJob := &store.Job{ID: 305, Host: "local-linux", Workdir: sharedWorkdir}
	runDir305 := filepath.Join(sharedWorkdir, ".fleet", "runs", "305")
	if err := os.MkdirAll(runDir305, 0o700); err != nil {
		t.Fatalf("mkdir runDir305: %v", err)
	}
	if err := os.WriteFile(filepath.Join(runDir305, pidFileName), []byte("0\n"), 0o600); err != nil {
		t.Fatalf("write job305 agent.pid: %v", err)
	}
	stBadPID, err := a.Status(ctx, badPIDJob)
	if err != nil || stBadPID.State != StateMissing {
		t.Fatalf("badPIDJob Status = (%+v, %v), want StateMissing", stBadPID, err)
	}
}

func TestLinuxExitAndPIDFileWhitespaceValidation(t *testing.T) {
	ctx := context.Background()
	workdir := t.TempDir()

	cfg := &config.Config{
		Hosts: map[string]config.HostConfig{
			"local-linux": {
				Transport: "local",
				OS:        "linux",
			},
		},
		Agents: map[string]config.AgentConfig{
			"claude": {
				Launch:      "claude",
				PromptStyle: config.PromptStyleStdin,
			},
		},
	}
	a := NewSSHAdapter(cfg, SSHAdapterOptions{
		Transport: NewLocalTransport(LocalTransportOptions{}),
	})

	validExitCases := []struct {
		name     string
		jobID    int64
		content  string
		wantCode int
	}{
		{name: "bare zero with newline", jobID: 501, content: "0\n", wantCode: 0},
		{name: "leading and trailing spaces tabs CRLF", jobID: 502, content: " \t 0 \t\r\n", wantCode: 0},
		{name: "leading and trailing newlines around single code", jobID: 503, content: "\n  7\n\n", wantCode: 7},
		{name: "EXIT prefix with surrounding whitespace", jobID: 504, content: "  EXIT:13 \r\n", wantCode: 13},
	}
	for _, tc := range validExitCases {
		t.Run("valid_exit/"+tc.name, func(t *testing.T) {
			runDir := filepath.Join(workdir, ".fleet", "runs", fmt.Sprintf("%d", tc.jobID))
			if err := os.MkdirAll(runDir, 0o700); err != nil {
				t.Fatalf("mkdir %q: %v", runDir, err)
			}
			if err := os.WriteFile(filepath.Join(runDir, exitFileName), []byte(tc.content), 0o600); err != nil {
				t.Fatalf("write agent.exit: %v", err)
			}
			job := &store.Job{ID: tc.jobID, Host: "local-linux", Workdir: workdir}
			st, err := a.Status(ctx, job)
			if err != nil {
				t.Fatalf("Status: %v", err)
			}
			if st.State != StateExited || st.ExitCode != tc.wantCode {
				t.Fatalf("Status = %+v, want StateExited(%d)", st, tc.wantCode)
			}
			ev, err := a.CollectEvidence(ctx, job)
			if err != nil {
				t.Fatalf("CollectEvidence: %v", err)
			}
			if ev.ExitCode != tc.wantCode || ev.ExitCodeUnknown {
				t.Fatalf("CollectEvidence exit = (code=%d, unknown=%v), want (%d, false)", ev.ExitCode, ev.ExitCodeUnknown, tc.wantCode)
			}
		})
	}

	invalidExitCases := []struct {
		name    string
		jobID   int64
		content string
	}{
		{name: "multiline duplicate zeros", jobID: 511, content: "0\n0\n"},
		{name: "space separated values", jobID: 512, content: "0 0\n"},
		{name: "multiline distinct values", jobID: 513, content: "0\n1\n"},
		{name: "CRLF multiline duplicate zeros", jobID: 514, content: "0\r\n0\r\n"},
		{name: "EXIT prefix followed by second line", jobID: 515, content: "EXIT:0\n0\n"},
		{name: "whitespace only", jobID: 516, content: " \t\n"},
		{name: "empty file", jobID: 517, content: ""},
	}
	for _, tc := range invalidExitCases {
		t.Run("invalid_exit/"+tc.name, func(t *testing.T) {
			runDir := filepath.Join(workdir, ".fleet", "runs", fmt.Sprintf("%d", tc.jobID))
			if err := os.MkdirAll(runDir, 0o700); err != nil {
				t.Fatalf("mkdir %q: %v", runDir, err)
			}
			if err := os.WriteFile(filepath.Join(runDir, exitFileName), []byte(tc.content), 0o600); err != nil {
				t.Fatalf("write agent.exit: %v", err)
			}
			job := &store.Job{ID: tc.jobID, Host: "local-linux", Workdir: workdir}
			st, err := a.Status(ctx, job)
			if err != nil {
				t.Fatalf("Status: %v", err)
			}
			if st.State != StateMissing || !strings.Contains(st.Detail, "invalid_exit_file") {
				t.Fatalf("Status(%q) = %+v, want StateMissing(invalid_exit_file)", tc.content, st)
			}
			ev, err := a.CollectEvidence(ctx, job)
			if err != nil {
				t.Fatalf("CollectEvidence: %v", err)
			}
			if strings.TrimSpace(tc.content) != "" && !ev.ExitCodeUnknown {
				t.Fatalf("CollectEvidence(%q) ExitCodeUnknown = false (ExitCode=%d), want true", tc.content, ev.ExitCode)
			}
		})
	}

	// Multiline / space-separated agent.pid must not concatenate digits into a different PID.
	myPID := fmt.Sprintf("%d", os.Getpid())
	if len(myPID) >= 2 {
		splitPID := myPID[:1] + "\n" + myPID[1:] + "\n"
		runDir := filepath.Join(workdir, ".fleet", "runs", "520")
		if err := os.MkdirAll(runDir, 0o700); err != nil {
			t.Fatalf("mkdir %q: %v", runDir, err)
		}
		if err := os.WriteFile(filepath.Join(runDir, pidFileName), []byte(splitPID), 0o600); err != nil {
			t.Fatalf("write split agent.pid: %v", err)
		}
		st, err := a.Status(ctx, &store.Job{ID: 520, Host: "local-linux", Workdir: workdir})
		if err != nil {
			t.Fatalf("Status split pid: %v", err)
		}
		if st.State != StateMissing || !strings.Contains(st.Detail, "invalid_pid") {
			t.Fatalf("Status split pid %q = %+v, want StateMissing(invalid_pid)", splitPID, st)
		}
	}
}

func TestLinuxUnreadableExitAndPIDFilesReturnMissing(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses 000 file read permissions")
	}

	ctx := context.Background()
	workdir := t.TempDir()
	cfg := &config.Config{
		Hosts: map[string]config.HostConfig{
			"local-linux": {
				Transport: "local",
				OS:        "linux",
			},
		},
		Agents: map[string]config.AgentConfig{
			"claude": {
				Launch:      "claude",
				PromptStyle: config.PromptStyleStdin,
			},
		},
	}
	a := NewSSHAdapter(cfg, SSHAdapterOptions{
		Transport: NewLocalTransport(LocalTransportOptions{}),
	})

	t.Run("unreadable agent.exit returns Missing instead of error", func(t *testing.T) {
		runDir := filepath.Join(workdir, ".fleet", "runs", "531")
		if err := os.MkdirAll(runDir, 0o700); err != nil {
			t.Fatalf("mkdir %q: %v", runDir, err)
		}
		exitPath := filepath.Join(runDir, exitFileName)
		if err := os.WriteFile(exitPath, []byte("0\n"), 0o600); err != nil {
			t.Fatalf("write agent.exit: %v", err)
		}
		if err := os.Chmod(exitPath, 0o000); err != nil {
			t.Fatalf("chmod 000 agent.exit: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(exitPath, 0o600) })

		st, err := a.Status(ctx, &store.Job{ID: 531, Host: "local-linux", Workdir: workdir})
		if err != nil {
			t.Fatalf("Status unreadable agent.exit returned error: %v", err)
		}
		if st.State != StateMissing || !strings.Contains(st.Detail, "unreadable_exit_file") {
			t.Fatalf("Status unreadable agent.exit = %+v, want StateMissing(unreadable_exit_file)", st)
		}
	})

	t.Run("unreadable agent.pid returns Missing instead of error", func(t *testing.T) {
		runDir := filepath.Join(workdir, ".fleet", "runs", "532")
		if err := os.MkdirAll(runDir, 0o700); err != nil {
			t.Fatalf("mkdir %q: %v", runDir, err)
		}
		pidPath := filepath.Join(runDir, pidFileName)
		if err := os.WriteFile(pidPath, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o600); err != nil {
			t.Fatalf("write agent.pid: %v", err)
		}
		if err := os.Chmod(pidPath, 0o000); err != nil {
			t.Fatalf("chmod 000 agent.pid: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(pidPath, 0o600) })

		st, err := a.Status(ctx, &store.Job{ID: 532, Host: "local-linux", Workdir: workdir})
		if err != nil {
			t.Fatalf("Status unreadable agent.pid returned error: %v", err)
		}
		if st.State != StateMissing || !strings.Contains(st.Detail, "unreadable_pid_file") {
			t.Fatalf("Status unreadable agent.pid = %+v, want StateMissing(unreadable_pid_file)", st)
		}
	})
}

// Regression tests (a) and (c) on Windows: sequential jobs sharing one workdir stage,
// probe, and collect evidence strictly under <workdir>\.fleet\runs\<job_id>\ without
// overwriting or misidentifying sibling jobs.
func TestPerJobRunArtifactsAndStatusIsolationWindows(t *testing.T) {
	ctx := context.Background()
	cfg := testWindowsFleetConfig()
	sharedWorkdir := `C:\Users\worker\agent-work\shared`

	job1 := &store.Job{
		ID:           401,
		Title:        "Win job 1",
		Agent:        "codex",
		Host:         "home-win",
		Workdir:      sharedWorkdir,
		Instructions: "job 1 instructions",
	}
	job2 := &store.Job{
		ID:           402,
		Title:        "Win job 2",
		Agent:        "codex",
		Host:         "home-win",
		Workdir:      sharedWorkdir,
		Instructions: "job 2 instructions",
	}

	fake := NewFakeTransport()
	fake.SetHandler(func(_ context.Context, _ config.HostConfig, cmd string) (CommandResult, error) {
		switch {
		case strings.Contains(cmd, "Win32_Process"):
			if strings.Contains(cmd, `\.fleet\runs\401\agent-run.cmd`) {
				return CommandResult{Stdout: "4001\r\n"}, nil
			}
			if strings.Contains(cmd, `\.fleet\runs\402\agent-run.cmd`) {
				return CommandResult{Stdout: "4002\r\n"}, nil
			}
			return CommandResult{ExitCode: 1, Stderr: "unexpected WMI cmd: " + cmd}, nil
		case strings.Contains(cmd, "Get-Process -Id"):
			// Job 402 is still running; job 401 has already exited with code 0; job 403 has no files.
			if strings.Contains(cmd, `\.fleet\runs\402\agent.exit`) {
				return CommandResult{Stdout: "RUNNING:4002\r\n"}, nil
			}
			if strings.Contains(cmd, `\.fleet\runs\401\agent.exit`) {
				return CommandResult{Stdout: "EXITED:0\r\n"}, nil
			}
			if strings.Contains(cmd, `\.fleet\runs\403\agent.exit`) {
				return CommandResult{Stdout: "MISSING:no_files\r\n"}, nil
			}
			return CommandResult{ExitCode: 1, Stderr: "unexpected status probe: " + cmd}, nil
		case strings.Contains(cmd, "status --porcelain"):
			return CommandResult{Stdout: "?? .fleet/\r\n?? summary.md\r\n"}, nil
		case strings.Contains(cmd, "diff --stat"):
			return CommandResult{Stdout: ""}, nil
		case strings.Contains(cmd, "Write-Output '1:"):
			return CommandResult{Stdout: "1:agent.log\r\n1:agent.err.log\r\n1:agent.exit\r\n1:summary.md\r\n0:result.txt\r\n"}, nil
		case strings.Contains(cmd, `\.fleet\runs\401\agent.log' -Encoding UTF8 -Tail`):
			return CommandResult{Stdout: "WIN_JOB_1_LOG\r\nEXIT:0\r\n"}, nil
		case strings.Contains(cmd, `\.fleet\runs\401\agent.err.log' -Encoding UTF8 -Tail`):
			return CommandResult{Stdout: "WIN_JOB_1_ERR\r\n"}, nil
		case strings.Contains(cmd, `\.fleet\runs\401\agent.exit`):
			return CommandResult{Stdout: "0\r\n"}, nil
		default:
			return CommandResult{ExitCode: 0}, nil
		}
	})

	a := NewSSHAdapter(cfg, SSHAdapterOptions{Transport: fake})

	// Launch job 1 then job 2 in the exact same workdir.
	info1, err := a.Launch(ctx, job1)
	if err != nil {
		t.Fatalf("Launch job1: %v", err)
	}
	info2, err := a.Launch(ctx, job2)
	if err != nil {
		t.Fatalf("Launch job2: %v", err)
	}
	if info1.Meta["run_dir"] != `C:\Users\worker\agent-work\shared\.fleet\runs\401` ||
		info2.Meta["run_dir"] != `C:\Users\worker\agent-work\shared\.fleet\runs\402` {
		t.Fatalf("unexpected run_dirs: job1=%q job2=%q", info1.Meta["run_dir"], info2.Meta["run_dir"])
	}

	// While job 2 is Running in sharedWorkdir, job 1 is Exited and job 3 is Missing (not misidentified as Running).
	st2, err := a.Status(ctx, job2)
	if err != nil || st2.State != StateRunning {
		t.Fatalf("job2 Status = (%+v, %v), want StateRunning", st2, err)
	}
	st1, err := a.Status(ctx, job1)
	if err != nil || st1.State != StateExited || st1.ExitCode != 0 {
		t.Fatalf("job1 Status = (%+v, %v), want StateExited(0)", st1, err)
	}
	job3 := &store.Job{ID: 403, Host: "home-win", Workdir: sharedWorkdir}
	st3, err := a.Status(ctx, job3)
	if err != nil || st3.State != StateMissing {
		t.Fatalf("job3 Status = (%+v, %v), want StateMissing", st3, err)
	}

	// CollectEvidence for job 1 retrieves job 1's own logs/exit even after job 2 launched in sharedWorkdir.
	ev1, err := a.CollectEvidence(ctx, job1)
	if err != nil {
		t.Fatalf("CollectEvidence job1: %v", err)
	}
	if ev1.ExitCode != 0 || !strings.Contains(ev1.FinalReport, "WIN_JOB_1_LOG") || !strings.Contains(ev1.ErrLogTail, "WIN_JOB_1_ERR") {
		t.Errorf("unexpected job1 evidence: %+v", ev1)
	}
	if strings.Contains(ev1.DiffSummary, ".fleet") {
		t.Errorf("DiffSummary should filter .fleet/: %q", ev1.DiffSummary)
	}
}

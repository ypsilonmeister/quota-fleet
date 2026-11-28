package adapter

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

// Issue #56 §5: before a rate-limit re-run, the previous agent.log is kept as
// agent.retry-<n>.log and the verdict file of this review step is removed, while the rest
// of the workdir (partial work, other steps' verdicts) is left alone.
func TestLaunchRateLimitRetryKeepsLogAndDropsVerdictLinux(t *testing.T) {
	ctx := context.Background()
	workdir := t.TempDir()
	fakeAgent := filepath.Join(t.TempDir(), "fake-agent.sh")
	if err := os.WriteFile(fakeAgent, []byte("#!/bin/sh\ncat >/dev/null\necho NEW_RUN_LOG\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Hosts: map[string]config.HostConfig{
			"local-linux": {Transport: "local", OS: "linux", Agents: map[string]config.HostAgentOverride{"claude": {Command: fakeAgent}}},
		},
		Agents: map[string]config.AgentConfig{"claude": {Launch: "claude", PromptStyle: config.PromptStyleStdin}},
	}
	a := NewSSHAdapter(cfg, SSHAdapterOptions{Transport: NewLocalTransport(LocalTransportOptions{})})

	const runID = "0f8fad5b-d9cb-469f-a165-70867728950e"
	write := func(rel, body string) string {
		t.Helper()
		p := filepath.Join(workdir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	verdict := write(".qf-chain/"+runID+"/2/verdict.json", `{"verdict":"approved"}`)
	otherVerdict := write(".qf-chain/"+runID+"/4/verdict.json", `{"verdict":"changes_needed"}`)
	partial := write("summary.md", "partial work")
	write(".fleet/runs/301/agent.log", "OLD_RUN_429_LOG\n")

	job := &store.Job{ID: 301, Title: "review", Agent: "claude", Host: "local-linux", Workdir: workdir,
		Instructions: "review", ChainRunID: runID, ChainSeq: 2, RateLimitRetries: 1}
	if _, err := a.Launch(ctx, job); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	waitForStatus(t, ctx, a, job, StateExited, 5*time.Second)

	if _, err := os.Stat(verdict); !os.IsNotExist(err) {
		t.Fatalf("verdict of the re-run step still present (err=%v)", err)
	}
	for _, keep := range []string{otherVerdict, partial} {
		if _, err := os.Stat(keep); err != nil {
			t.Fatalf("%s must be kept: %v", keep, err)
		}
	}
	runDir := filepath.Join(workdir, ".fleet", "runs", "301")
	if b, err := os.ReadFile(filepath.Join(runDir, "agent.retry-1.log")); err != nil || !strings.Contains(string(b), "OLD_RUN_429_LOG") {
		t.Fatalf("previous log not kept: %q %v", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(runDir, "agent.log")); err != nil || strings.Contains(string(b), "OLD_RUN_429_LOG") || !strings.Contains(string(b), "NEW_RUN_LOG") {
		t.Fatalf("agent.log of the re-run = %q %v", b, err)
	}

	// A first run neither moves the log nor touches verdicts.
	first := &store.Job{ID: 302, Title: "first", Agent: "claude", Host: "local-linux", Workdir: workdir,
		Instructions: "x", ChainRunID: runID, ChainSeq: 4}
	if _, err := a.Launch(ctx, first); err != nil {
		t.Fatalf("Launch first: %v", err)
	}
	waitForStatus(t, ctx, a, first, StateExited, 5*time.Second)
	if _, err := os.Stat(otherVerdict); err != nil {
		t.Fatalf("first run removed a verdict: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workdir, ".fleet", "runs", "302", "agent.retry-0.log")); !os.IsNotExist(err) {
		t.Fatalf("first run archived a log (err=%v)", err)
	}
}

func TestLaunchRateLimitRetryWindowsStaging(t *testing.T) {
	ctx := context.Background()
	fake := NewFakeTransport()
	fake.SetHandler(func(_ context.Context, _ config.HostConfig, cmd string) (CommandResult, error) {
		if strings.Contains(cmd, "Win32_Process") {
			return CommandResult{Stdout: "5001\r\n"}, nil
		}
		return CommandResult{}, nil
	})
	a := NewSSHAdapter(testWindowsFleetConfig(), SSHAdapterOptions{Transport: fake})
	job := &store.Job{ID: 501, Title: "review", Agent: "codex", Host: "home-win", Workdir: `C:\w\shared`,
		Instructions: "review", ChainRunID: "0f8fad5b-d9cb-469f-a165-70867728950e", ChainSeq: 2, RateLimitRetries: 2}
	if _, err := a.Launch(ctx, job); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	var stage string
	for _, c := range fake.Calls() {
		if strings.Contains(c.Command, "WriteAllBytes") {
			stage = c.Command
		}
	}
	for _, want := range []string{
		`try { Move-Item -Force -ErrorAction Stop -LiteralPath 'C:\w\shared\.fleet\runs\501\agent.log' -Destination 'C:\w\shared\.fleet\runs\501\agent.retry-2.log' } catch {`,
		`try { Move-Item -Force -ErrorAction Stop -LiteralPath 'C:\w\shared\.fleet\runs\501\agent.err.log' -Destination 'C:\w\shared\.fleet\runs\501\agent.err.retry-2.log' } catch {`,
		`if (Test-Path -LiteralPath 'C:\w\shared\.qf-chain\0f8fad5b-d9cb-469f-a165-70867728950e\2\verdict.json') { try { Remove-Item -Force -ErrorAction Stop -LiteralPath 'C:\w\shared\.qf-chain\0f8fad5b-d9cb-469f-a165-70867728950e\2\verdict.json' } catch {`,
		`exit 1`,
	} {
		if !strings.Contains(stage, want) {
			t.Errorf("staging command lacks %q:\n%s", want, stage)
		}
	}
	// A failed removal of an existing verdict must not be ignored (review of 141a80e).
	if i := strings.Index(stage, "New-Item"); i < 0 || strings.Contains(stage[:i], "SilentlyContinue") {
		t.Errorf("retry prep must not ignore errors:\n%s", stage[:i])
	}
	if i, j := strings.Index(stage, "verdict.json"), strings.Index(stage, "WriteAllBytes"); i < 0 || i > j {
		t.Errorf("stale verdict must be removed before the new run is staged:\n%s", stage)
	}
	if i, j := strings.Index(stage, "Move-Item"), strings.Index(stage, "WriteAllBytes"); i < 0 || i > j {
		t.Errorf("logs must be moved before the new run is staged:\n%s", stage)
	}
}

// TestLaunchRateLimitRetryWindowsPrepFailureStopsLaunch: when the remote staging exits
// non-zero because an existing verdict could not be removed (or a log could not be kept),
// Launch returns an error and never starts the agent.
func TestLaunchRateLimitRetryWindowsPrepFailureStopsLaunch(t *testing.T) {
	ctx := context.Background()
	fake := NewFakeTransport()
	fake.SetHandler(func(_ context.Context, _ config.HostConfig, cmd string) (CommandResult, error) {
		if strings.Contains(cmd, "Win32_Process") {
			return CommandResult{Stdout: "5001\r\n"}, nil
		}
		if strings.Contains(cmd, "WriteAllBytes") {
			return CommandResult{ExitCode: 1, Stderr: "rate-limit retry: remove stale verdict failed: Access to the path is denied."}, nil
		}
		return CommandResult{}, nil
	})
	a := NewSSHAdapter(testWindowsFleetConfig(), SSHAdapterOptions{Transport: fake})
	job := &store.Job{ID: 502, Title: "review", Agent: "codex", Host: "home-win", Workdir: `C:\w\shared`,
		Instructions: "review", ChainRunID: "0f8fad5b-d9cb-469f-a165-70867728950e", ChainSeq: 2, RateLimitRetries: 1}
	_, err := a.Launch(ctx, job)
	if err == nil || !strings.Contains(err.Error(), "remove stale verdict failed") {
		t.Fatalf("Launch err = %v, want the staging failure", err)
	}
	for _, c := range fake.Calls() {
		if strings.Contains(c.Command, "Win32_Process") {
			t.Fatalf("agent launched after a failed retry prep: %s", c.Command)
		}
	}
}

func TestWindowsRetryPrepSkipsMissingFiles(t *testing.T) {
	job := &store.Job{ID: 1, ChainRunID: "0f8fad5b-d9cb-469f-a165-70867728950e", ChainSeq: 2, RateLimitRetries: 1}
	stmts, err := windowsRetryPrepStatements(`C:\w`, `C:\w\.fleet\runs\1`, job)
	if err != nil {
		t.Fatal(err)
	}
	if len(stmts) != 4 {
		t.Fatalf("statements = %d: %q", len(stmts), stmts)
	}
	// Every fallible action only runs when its file exists, so a missing file proceeds.
	for _, st := range stmts {
		if !strings.HasPrefix(st, "if (Test-Path -LiteralPath ") {
			t.Errorf("statement is not guarded by Test-Path: %s", st)
		}
		if !strings.Contains(st, "exit 1") {
			t.Errorf("statement does not stop on failure: %s", st)
		}
	}
}

func TestRetryVerdictPathRejectsUnexpectedRunID(t *testing.T) {
	job := &store.Job{ID: 1, ChainRunID: "../../etc", ChainSeq: 1, RateLimitRetries: 1}
	if _, err := retryVerdictPath(job); err == nil {
		t.Fatal("expected an error for a run id outside the UUID alphabet")
	}
}

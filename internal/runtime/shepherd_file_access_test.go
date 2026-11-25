package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/adapter"
	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/evaluator"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

func TestShepherdTypedEvaluationUsesRemoteHostFileAccess(t *testing.T) {
	ctx := context.Background()
	st, _ := openTestStore(t, "shepherd-remote-fa")
	recNotifier := &recordingNotifier{}

	cfg := &config.Config{
		Hosts: map[string]config.HostConfig{
			"home-win": {
				Transport:   "ssh",
				SSH:         "yuya@home-win",
				OS:          "windows",
				WorkdirRoot: `C:\Users\yuya\work`,
			},
		},
	}

	fake := adapter.NewFakeTransport()
	fake.SetHandler(func(_ context.Context, _ config.HostConfig, cmd string) (adapter.CommandResult, error) {
		switch {
		// Windows Status probe -> exited with code 0.
		case strings.Contains(cmd, "EXITED:"):
			return adapter.CommandResult{Stdout: "EXITED:0\r\n"}, nil
		// Windows CollectEvidence: git status
		case strings.Contains(cmd, "status --porcelain"):
			return adapter.CommandResult{Stdout: "", ExitCode: 0}, nil
		// Windows CollectEvidence: git diff --stat
		case strings.Contains(cmd, "diff --stat"):
			return adapter.CommandResult{Stdout: "", ExitCode: 0}, nil
		// Windows CollectEvidence: artifact checks
		case strings.Contains(cmd, "'1:agent.log'"):
			return adapter.CommandResult{Stdout: "1:agent.log\r\n1:agent.exit\r\n"}, nil
		// Windows CollectEvidence: agent.log / agent.err.log tails
		case strings.Contains(cmd, "Get-Content -LiteralPath"):
			return adapter.CommandResult{Stdout: "done\r\n"}, nil
		// Windows CollectEvidence: agent.exit
		case strings.Contains(cmd, "agent.exit") && strings.Contains(cmd, "ReadAllText"):
			return adapter.CommandResult{Stdout: "0\r\n"}, nil
		// Remote Windows FileAccess Stat
		case strings.Contains(cmd, "PSIsContainer") && strings.Contains(cmd, "FILE:"):
			if strings.Contains(cmd, `'C:\Users\yuya\work\job-1\deliverable.txt'`) {
				return adapter.CommandResult{Stdout: "FILE:14\r\n"}, nil
			}
			return adapter.CommandResult{Stdout: "MISSING\r\n"}, nil
		// Remote Windows FileAccess ReadFile
		case strings.Contains(cmd, "ReadAllText") && strings.Contains(cmd, `'C:\Users\yuya\work\job-1\deliverable.txt'`):
			return adapter.CommandResult{Stdout: "DONE_ON_WIN11\n"}, nil
		// Remote Windows FileAccess RunCommand
		case strings.Contains(cmd, "Invoke-Expression"):
			if strings.Contains(cmd, "'verify.cmd'") {
				return adapter.CommandResult{Stdout: "verified\r\n", ExitCode: 0}, nil
			}
			return adapter.CommandResult{ExitCode: 1}, nil
		default:
			return adapter.CommandResult{ExitCode: 0}, nil
		}
	})

	sshAdapter := adapter.NewSSHAdapter(cfg, adapter.SSHAdapterOptions{Transport: fake})
	eval := evaluator.NewTypedDecision(evaluator.TypedDecisionOptions{
		CommandTimeout: 5 * time.Second,
		Getenv:         func(string) string { return "" },
	})

	shepherd := NewShepherd(ShepherdOptions{
		Store:     st,
		Adapter:   sshAdapter,
		Evaluator: eval,
		Notifier:  recNotifier,
		Logger:    discardLogger(),
	})

	// Workdir is a Windows path that does not exist on the Linux serve host.
	job, _, err := st.CreateJob(ctx, store.CreateJobParams{
		RequestID: "shepherd-win-typed-1",
		Title:     "Remote Windows typed check",
		Agent:     "codex",
		Host:      "home-win",
		Workdir:   `C:\Users\yuya\work\job-1`,
		AcceptanceCriteria: strings.Join([]string{
			"file_exists: deliverable.txt",
			"contains: deliverable.txt :: DONE_ON_WIN11",
			"command: verify.cmd",
		}, "\n"),
	})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	claimed, ok, err := st.Claim(ctx, 2)
	if err != nil || !ok || claimed.ID != job.ID {
		t.Fatalf("Claim: ok=%v err=%v", ok, err)
	}

	if err := shepherd.SweepOnce(ctx); err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}

	got, err := st.GetJob(ctx, claimed.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.Status != store.StatusDone {
		t.Fatalf("Status = %s, want done (last_error=%q, result=%q)", got.Status, got.LastError, got.ResultText)
	}
	if !strings.Contains(got.ResultText, "typed file_exists: found file deliverable.txt (14B)") {
		t.Errorf("ResultText missing file_exists evidence:\n%s", got.ResultText)
	}
	if !strings.Contains(got.ResultText, "typed contains: matched in deliverable.txt") {
		t.Errorf("ResultText missing contains evidence:\n%s", got.ResultText)
	}
	if !strings.Contains(got.ResultText, "typed command: exit 0") {
		t.Errorf("ResultText missing command evidence:\n%s", got.ResultText)
	}
}

func TestShepherdTypedEvaluationUsesLocalFileAccessForLocalHost(t *testing.T) {
	ctx := context.Background()
	st, _ := openTestStore(t, "shepherd-local-fa")
	recNotifier := &recordingNotifier{}

	localWorkdir := t.TempDir()
	if err := os.WriteFile(filepath.Join(localWorkdir, "local.txt"), []byte("local-ok\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg := &config.Config{
		Hosts: map[string]config.HostConfig{
			"ubuntu": {
				Transport: "local",
				OS:        "linux",
			},
		},
	}

	fake := adapter.NewFakeTransport()
	fake.SetHandler(func(_ context.Context, _ config.HostConfig, cmd string) (adapter.CommandResult, error) {
		switch {
		case strings.Contains(cmd, "EXITED:"):
			return adapter.CommandResult{Stdout: "EXITED:0\n"}, nil
		case strings.Contains(cmd, "status --porcelain"):
			return adapter.CommandResult{Stdout: "", ExitCode: 0}, nil
		case strings.Contains(cmd, "printf '1:"):
			return adapter.CommandResult{Stdout: "1:agent.log\n"}, nil
		case strings.Contains(cmd, "tail -n"):
			return adapter.CommandResult{Stdout: "ok\n"}, nil
		case strings.Contains(cmd, "agent.exit"):
			return adapter.CommandResult{Stdout: "0\n"}, nil
		default:
			// Must NOT be called for FileAccess on a local host!
			return adapter.CommandResult{ExitCode: 99, Stderr: "unexpected transport call during local FileAccess: " + cmd}, nil
		}
	})

	sshAdapter := adapter.NewSSHAdapter(cfg, adapter.SSHAdapterOptions{Transport: fake})
	eval := evaluator.NewTypedDecision(evaluator.TypedDecisionOptions{
		CommandTimeout: 5 * time.Second,
		Getenv:         func(string) string { return "" },
	})

	shepherd := NewShepherd(ShepherdOptions{
		Store:     st,
		Adapter:   sshAdapter,
		Evaluator: eval,
		Notifier:  recNotifier,
		Logger:    discardLogger(),
	})

	job, _, err := st.CreateJob(ctx, store.CreateJobParams{
		RequestID: "shepherd-local-typed-1",
		Title:     "Local host typed check",
		Agent:     "claude",
		Host:      "ubuntu",
		Workdir:   localWorkdir,
		AcceptanceCriteria: strings.Join([]string{
			"file_exists: local.txt",
			"contains: local.txt :: local-ok",
			"command: test -f local.txt",
		}, "\n"),
	})
	if err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	claimed, ok, err := st.Claim(ctx, 2)
	if err != nil || !ok || claimed.ID != job.ID {
		t.Fatalf("Claim: ok=%v err=%v", ok, err)
	}

	if err := shepherd.SweepOnce(ctx); err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}

	got, err := st.GetJob(ctx, claimed.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got.Status != store.StatusDone {
		t.Fatalf("Status = %s, want done (last_error=%q, result=%q)", got.Status, got.LastError, got.ResultText)
	}
	for _, call := range fake.Calls() {
		if strings.Contains(call.Command, "local.txt") {
			t.Errorf("local host typed check should use LocalFileAccess, not Transport; saw cmd: %s", call.Command)
		}
	}
}

package evaluator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/adapter"
	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

func TestTypedEvaluationRemoteLinuxViaFakeTransport(t *testing.T) {
	cfg := &config.Config{
		Hosts: map[string]config.HostConfig{
			"linux-worker": {
				Transport: "ssh",
				SSH:       "hatch@linux-worker",
				OS:        "linux",
			},
		},
	}

	fake := adapter.NewFakeTransport()
	fake.SetHandler(func(_ context.Context, _ config.HostConfig, cmd string) (adapter.CommandResult, error) {
		switch {
		case strings.Contains(cmd, "printf 'DIR\\n'"):
			switch {
			case strings.Contains(cmd, `'/nonexistent-remote-linux/job-10/report.txt'`):
				return adapter.CommandResult{Stdout: "FILE:25\n"}, nil
			case strings.Contains(cmd, `'/nonexistent-remote-linux/job-10/empty.txt'`):
				return adapter.CommandResult{Stdout: "FILE:0\n"}, nil
			case strings.Contains(cmd, `'/nonexistent-remote-linux/job-10/artifacts'`):
				return adapter.CommandResult{Stdout: "DIR\n"}, nil
			default:
				return adapter.CommandResult{Stdout: "MISSING\n"}, nil
			}
		case strings.Contains(cmd, `cat -- "$p"`):
			if strings.Contains(cmd, `'/nonexistent-remote-linux/job-10/report.txt'`) {
				return adapter.CommandResult{Stdout: "status: ALL_GREEN\n"}, nil
			}
			return adapter.CommandResult{ExitCode: 44}, nil
		case strings.Contains(cmd, "sh -c 'true'"):
			return adapter.CommandResult{ExitCode: 0}, nil
		case strings.Contains(cmd, "sh -c 'false'"):
			return adapter.CommandResult{Stderr: "check failed\n", ExitCode: 2}, nil
		default:
			return adapter.CommandResult{ExitCode: 1, Stderr: "unexpected cmd: " + cmd}, nil
		}
	})

	sshAdapter := adapter.NewSSHAdapter(cfg, adapter.SSHAdapterOptions{Transport: fake})
	eval := NewTypedDecision(TypedDecisionOptions{
		CommandTimeout: 5 * time.Second,
		Getenv:         func(string) string { return "" },
	})

	// Path does not exist on serve's local filesystem, yet remote typed checks must pass via FakeTransport.
	t.Run("all typed checks pass on nonexistent local path via Linux FakeTransport", func(t *testing.T) {
		job := &store.Job{
			ID:      10,
			Host:    "linux-worker",
			Workdir: "/nonexistent-remote-linux/job-10",
			AcceptanceCriteria: strings.Join([]string{
				"file_exists: report.txt",
				"file_exists: artifacts",
				"file_nonempty: report.txt",
				"contains: report.txt :: ALL_GREEN",
				"command: true",
			}, "\n"),
		}
		fa, err := sshAdapter.FileAccessFor(job)
		if err != nil {
			t.Fatalf("FileAccessFor: %v", err)
		}
		res, err := eval.EvaluateWithFileAccess(context.Background(), job, adapter.Evidence{}, fa)
		if err != nil {
			t.Fatalf("EvaluateWithFileAccess: %v", err)
		}
		if !res.Passed {
			t.Fatalf("Passed = false, want true:\n%s", res.FormatText())
		}
		if res.Criteria[0].Evidence != "typed file_exists: found file report.txt (25B)" {
			t.Errorf("Criteria[0].Evidence = %q", res.Criteria[0].Evidence)
		}
		if res.Criteria[1].Evidence != "typed file_exists: found dir artifacts" {
			t.Errorf("Criteria[1].Evidence = %q", res.Criteria[1].Evidence)
		}
		if res.Criteria[2].Evidence != "typed file_nonempty: report.txt (25B)" {
			t.Errorf("Criteria[2].Evidence = %q", res.Criteria[2].Evidence)
		}
		if res.Criteria[3].Evidence != "typed contains: matched in report.txt" {
			t.Errorf("Criteria[3].Evidence = %q", res.Criteria[3].Evidence)
		}
		if res.Criteria[4].Evidence != "typed command: exit 0" {
			t.Errorf("Criteria[4].Evidence = %q", res.Criteria[4].Evidence)
		}
	})

	t.Run("missing file, contains mismatch, empty file, and failing command fail cleanly", func(t *testing.T) {
		job := &store.Job{
			ID:      11,
			Host:    "linux-worker",
			Workdir: "/nonexistent-remote-linux/job-10",
			AcceptanceCriteria: strings.Join([]string{
				"file_exists: missing.txt",
				"file_nonempty: empty.txt",
				"contains: report.txt :: NOT_THERE",
				"command: false",
			}, "\n"),
		}
		fa, err := sshAdapter.FileAccessFor(job)
		if err != nil {
			t.Fatalf("FileAccessFor: %v", err)
		}
		res, err := eval.EvaluateWithFileAccess(context.Background(), job, adapter.Evidence{}, fa)
		if err != nil {
			t.Fatalf("EvaluateWithFileAccess: %v", err)
		}
		if res.Passed {
			t.Fatalf("Passed = true, want false")
		}
		if len(res.Criteria) != 4 {
			t.Fatalf("len(Criteria) = %d, want 4", len(res.Criteria))
		}
		if res.Criteria[0].Evidence != "typed file_exists: not found: missing.txt" {
			t.Errorf("Criteria[0].Evidence = %q", res.Criteria[0].Evidence)
		}
		if res.Criteria[1].Evidence != "typed file_nonempty: empty.txt is empty (0B)" {
			t.Errorf("Criteria[1].Evidence = %q", res.Criteria[1].Evidence)
		}
		if res.Criteria[2].Evidence != "typed contains: text not found in report.txt" {
			t.Errorf("Criteria[2].Evidence = %q", res.Criteria[2].Evidence)
		}
		if res.Criteria[3].Evidence != "typed command: exit 2 (check failed)" {
			t.Errorf("Criteria[3].Evidence = %q", res.Criteria[3].Evidence)
		}
	})
}

func TestTypedEvaluationRemoteWindowsViaFakeTransport(t *testing.T) {
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
		case strings.Contains(cmd, "Get-Item -LiteralPath") && strings.Contains(cmd, "PSIsContainer") && strings.Contains(cmd, "FILE:"):
			if strings.Contains(cmd, `'C:\Users\yuya\work\job-20\out\result.txt'`) {
				return adapter.CommandResult{Stdout: "FILE:19\r\n"}, nil
			}
			return adapter.CommandResult{Stdout: "MISSING\r\n"}, nil
		case strings.Contains(cmd, "ReadAllText"):
			if strings.Contains(cmd, `'C:\Users\yuya\work\job-20\out\result.txt'`) {
				return adapter.CommandResult{Stdout: "WINDOWS_OUTPUT_OK\r\n"}, nil
			}
			return adapter.CommandResult{ExitCode: 44}, nil
		case strings.Contains(cmd, "Invoke-Expression"):
			if strings.Contains(cmd, "'go test ./...'") {
				return adapter.CommandResult{Stdout: "ok\r\n", ExitCode: 0}, nil
			}
			return adapter.CommandResult{Stderr: "FAIL pkg\r\n", ExitCode: 1}, nil
		default:
			return adapter.CommandResult{ExitCode: 1, Stderr: "unexpected windows cmd: " + cmd}, nil
		}
	})

	sshAdapter := adapter.NewSSHAdapter(cfg, adapter.SSHAdapterOptions{Transport: fake})
	eval := NewTypedDecision(TypedDecisionOptions{
		CommandTimeout: 5 * time.Second,
		Getenv:         func(string) string { return "" },
	})

	t.Run("Windows remote pass checks use backslash join and PowerShell", func(t *testing.T) {
		job := &store.Job{
			ID:      20,
			Host:    "home-win",
			Workdir: `C:\Users\yuya\work\job-20`,
			AcceptanceCriteria: strings.Join([]string{
				"file_exists: out/result.txt",
				"file_nonempty: out/result.txt",
				"contains: out/result.txt :: WINDOWS_OUTPUT_OK",
				"command: go test ./...",
			}, "\n"),
		}
		fa, err := sshAdapter.FileAccessFor(job)
		if err != nil {
			t.Fatalf("FileAccessFor: %v", err)
		}
		res, err := eval.EvaluateWithFileAccess(context.Background(), job, adapter.Evidence{}, fa)
		if err != nil {
			t.Fatalf("EvaluateWithFileAccess: %v", err)
		}
		if !res.Passed {
			t.Fatalf("Passed = false, want true:\n%s", res.FormatText())
		}
		if res.Criteria[0].Evidence != "typed file_exists: found file out/result.txt (19B)" {
			t.Errorf("Criteria[0].Evidence = %q", res.Criteria[0].Evidence)
		}
		if res.Criteria[2].Evidence != "typed contains: matched in out/result.txt" {
			t.Errorf("Criteria[2].Evidence = %q", res.Criteria[2].Evidence)
		}
		if res.Criteria[3].Evidence != "typed command: exit 0" {
			t.Errorf("Criteria[3].Evidence = %q", res.Criteria[3].Evidence)
		}
	})

	t.Run("Windows remote fail checks for missing file, mismatch, and failing command", func(t *testing.T) {
		job := &store.Job{
			ID:      21,
			Host:    "home-win",
			Workdir: `C:\Users\yuya\work\job-20`,
			AcceptanceCriteria: strings.Join([]string{
				"file_exists: out/missing.txt",
				"contains: out/result.txt :: WRONG_MARKER",
				"command: go test ./broken",
			}, "\n"),
		}
		fa, err := sshAdapter.FileAccessFor(job)
		if err != nil {
			t.Fatalf("FileAccessFor: %v", err)
		}
		res, err := eval.EvaluateWithFileAccess(context.Background(), job, adapter.Evidence{}, fa)
		if err != nil {
			t.Fatalf("EvaluateWithFileAccess: %v", err)
		}
		if res.Passed {
			t.Fatalf("Passed = true, want false")
		}
		if res.Criteria[0].Evidence != "typed file_exists: not found: out/missing.txt" {
			t.Errorf("Criteria[0].Evidence = %q", res.Criteria[0].Evidence)
		}
		if res.Criteria[1].Evidence != "typed contains: text not found in out/result.txt" {
			t.Errorf("Criteria[1].Evidence = %q", res.Criteria[1].Evidence)
		}
		if res.Criteria[2].Evidence != "typed command: exit 1 (FAIL pkg)" {
			t.Errorf("Criteria[2].Evidence = %q", res.Criteria[2].Evidence)
		}
	})
}

// TestRemoteTypedEvaluationNeverConsultsServeLocalFS proves that remote typed evaluation
// never falls back to or reads from the serve host's local filesystem, even when a file
// exists locally at the exact same path.
func TestRemoteTypedEvaluationNeverConsultsServeLocalFS(t *testing.T) {
	localDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(localDir, "only-on-serve.txt"), []byte("serve-secret"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg := &config.Config{
		Hosts: map[string]config.HostConfig{
			"remote-box": {
				Transport: "ssh",
				SSH:       "hatch@remote-box",
				OS:        "linux",
			},
		},
	}

	fake := adapter.NewFakeTransport()
	fake.SetHandler(func(_ context.Context, _ config.HostConfig, cmd string) (adapter.CommandResult, error) {
		switch {
		case strings.Contains(cmd, "printf 'DIR\\n'"):
			// Remote says only-on-serve.txt is MISSING, while remote-only.txt EXISTS.
			if strings.Contains(cmd, "remote-only.txt") {
				return adapter.CommandResult{Stdout: "FILE:11\n"}, nil
			}
			return adapter.CommandResult{Stdout: "MISSING\n"}, nil
		case strings.Contains(cmd, `cat -- "$p"`):
			if strings.Contains(cmd, "remote-only.txt") {
				return adapter.CommandResult{Stdout: "remote-data"}, nil
			}
			return adapter.CommandResult{ExitCode: 44}, nil
		default:
			return adapter.CommandResult{ExitCode: 1}, nil
		}
	})

	sshAdapter := adapter.NewSSHAdapter(cfg, adapter.SSHAdapterOptions{Transport: fake})
	eval := NewTypedDecision(TypedDecisionOptions{Getenv: func(string) string { return "" }})

	// Point job.Workdir to localDir where only-on-serve.txt ACTUALLY exists locally and remote-only.txt does NOT.
	job := &store.Job{
		ID:      30,
		Host:    "remote-box",
		Workdir: localDir,
		AcceptanceCriteria: strings.Join([]string{
			"file_exists: only-on-serve.txt",
			"file_exists: remote-only.txt",
			"contains: only-on-serve.txt :: serve-secret",
			"contains: remote-only.txt :: remote-data",
		}, "\n"),
	}
	fa, err := sshAdapter.FileAccessFor(job)
	if err != nil {
		t.Fatalf("FileAccessFor: %v", err)
	}

	res, err := eval.EvaluateWithFileAccess(context.Background(), job, adapter.Evidence{}, fa)
	if err != nil {
		t.Fatalf("EvaluateWithFileAccess: %v", err)
	}
	if res.Passed {
		t.Fatalf("Passed = true, want false (only-on-serve.txt must not be seen)")
	}
	if len(res.Criteria) != 4 {
		t.Fatalf("len(Criteria) = %d, want 4", len(res.Criteria))
	}
	if res.Criteria[0].Passed || res.Criteria[0].Evidence != "typed file_exists: not found: only-on-serve.txt" {
		t.Errorf("Criteria[0] = %+v, want not found (must ignore local fs)", res.Criteria[0])
	}
	if !res.Criteria[1].Passed || res.Criteria[1].Evidence != "typed file_exists: found file remote-only.txt (11B)" {
		t.Errorf("Criteria[1] = %+v, want found via FakeTransport", res.Criteria[1])
	}
	if res.Criteria[2].Passed || res.Criteria[2].Evidence != "typed contains: not found: only-on-serve.txt" {
		t.Errorf("Criteria[2] = %+v, want not found (must ignore local fs)", res.Criteria[2])
	}
	if !res.Criteria[3].Passed || res.Criteria[3].Evidence != "typed contains: matched in remote-only.txt" {
		t.Errorf("Criteria[3] = %+v, want matched via FakeTransport", res.Criteria[3])
	}
	if len(fake.Calls()) != 4 {
		t.Errorf("FakeTransport calls = %d, want 4", len(fake.Calls()))
	}
}

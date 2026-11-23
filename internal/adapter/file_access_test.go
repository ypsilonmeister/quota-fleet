package adapter

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

func TestSSHAdapterFileAccessRouting(t *testing.T) {
	cfg := &config.Config{
		Hosts: map[string]config.HostConfig{
			"ubuntu-local": {
				Transport:   "local",
				OS:          "linux",
				WorkdirRoot: "/tmp/local-work",
			},
			"remote-linux": {
				Transport:   "ssh",
				SSH:         "hatch@linux-box",
				OS:          "linux",
				WorkdirRoot: "/home/hatch/work",
			},
			"home-win": {
				Transport:   "ssh",
				SSH:         "yuya@home-win",
				OS:          "windows",
				WorkdirRoot: `C:\Users\yuya\work`,
				DenyPaths:   []string{`H:\a`},
			},
		},
	}

	fake := NewFakeTransport()
	ad := NewSSHAdapter(cfg, SSHAdapterOptions{Transport: fake})

	t.Run("local transport host selects LocalFileAccess", func(t *testing.T) {
		fa, err := ad.FileAccessFor(&store.Job{ID: 1, Host: "ubuntu-local", Workdir: "/tmp/local-work/job-1"})
		if err != nil {
			t.Fatalf("FileAccessFor: %v", err)
		}
		if _, ok := fa.(*LocalFileAccess); !ok {
			t.Fatalf("got %T, want *LocalFileAccess", fa)
		}
	})

	t.Run("remote linux host selects linuxFileAccess", func(t *testing.T) {
		fa, err := ad.FileAccessFor(&store.Job{ID: 2, Host: "remote-linux", Workdir: "/home/hatch/work/job-2"})
		if err != nil {
			t.Fatalf("FileAccessFor: %v", err)
		}
		if _, ok := fa.(*linuxFileAccess); !ok {
			t.Fatalf("got %T, want *linuxFileAccess", fa)
		}
	})

	t.Run("remote windows host selects windowsFileAccess and resolves default workdir", func(t *testing.T) {
		fa, err := ad.FileAccessFor(&store.Job{ID: 3, Host: "home-win"})
		if err != nil {
			t.Fatalf("FileAccessFor: %v", err)
		}
		wfa, ok := fa.(*windowsFileAccess)
		if !ok {
			t.Fatalf("got %T, want *windowsFileAccess", fa)
		}
		if wfa.workdir != `C:\Users\yuya\work\job-3` {
			t.Errorf("workdir = %q, want %q", wfa.workdir, `C:\Users\yuya\work\job-3`)
		}
	})

	t.Run("deny_paths blocks forbidden workdir and file path", func(t *testing.T) {
		if _, err := ad.FileAccessFor(&store.Job{ID: 4, Host: "home-win", Workdir: `H:\a\secret`}); !errors.Is(err, ErrDeniedPath) {
			t.Fatalf("expected ErrDeniedPath for denied workdir, got %v", err)
		}
		fa, err := ad.FileAccessFor(&store.Job{ID: 5, Host: "home-win", Workdir: `C:\Users\yuya\work\job-5`})
		if err != nil {
			t.Fatalf("FileAccessFor: %v", err)
		}
		if _, err := fa.Stat(context.Background(), `H:\a\secret.txt`); !errors.Is(err, ErrDeniedPath) {
			t.Fatalf("expected ErrDeniedPath for denied file path, got %v", err)
		}
	})
}

func TestLinuxRemoteFileAccess(t *testing.T) {
	host := config.HostConfig{
		Transport: "ssh",
		SSH:       "hatch@linux-box",
		OS:        "linux",
	}
	fake := NewFakeTransport()
	fa, err := NewRemoteFileAccess(fake, host, "/remote/my project")
	if err != nil {
		t.Fatalf("NewRemoteFileAccess: %v", err)
	}

	t.Run("Stat file, dir, missing, and special characters quoting", func(t *testing.T) {
		fake.Reset()
		fake.SetHandler(func(_ context.Context, _ config.HostConfig, cmd string) (CommandResult, error) {
			switch {
			case strings.Contains(cmd, `'/remote/my project/sub/it'\''s ok.txt'`):
				return CommandResult{Stdout: "FILE:42\n"}, nil
			case strings.Contains(cmd, `'/remote/my project/out dir'`):
				return CommandResult{Stdout: "DIR\n"}, nil
			default:
				return CommandResult{Stdout: "MISSING\n"}, nil
			}
		})

		st, err := fa.Stat(context.Background(), "sub/it's ok.txt")
		if err != nil {
			t.Fatalf("Stat file: %v", err)
		}
		if !st.Exists || st.IsDir || st.Size != 42 {
			t.Errorf("Stat file = %+v, want Exists=true IsDir=false Size=42", st)
		}

		stDir, err := fa.Stat(context.Background(), "out dir")
		if err != nil {
			t.Fatalf("Stat dir: %v", err)
		}
		if !stDir.Exists || !stDir.IsDir {
			t.Errorf("Stat dir = %+v, want Exists=true IsDir=true", stDir)
		}

		stMissing, err := fa.Stat(context.Background(), "missing.txt")
		if err != nil {
			t.Fatalf("Stat missing: %v", err)
		}
		if stMissing.Exists {
			t.Errorf("Stat missing = %+v, want Exists=false", stMissing)
		}
	})

	t.Run("ReadFile existing, missing, and directory", func(t *testing.T) {
		fake.Reset()
		fake.SetHandler(func(_ context.Context, _ config.HostConfig, cmd string) (CommandResult, error) {
			switch {
			case strings.Contains(cmd, `'/remote/my project/notes $1.txt'`):
				return CommandResult{Stdout: "hello remote linux\n", ExitCode: 0}, nil
			case strings.Contains(cmd, `'/remote/my project/sub'`):
				return CommandResult{Stderr: "is a directory\n", ExitCode: remoteExitIsDir}, nil
			default:
				return CommandResult{ExitCode: remoteExitNotFound}, nil
			}
		})

		data, err := fa.ReadFile(context.Background(), "notes $1.txt")
		if err != nil {
			t.Fatalf("ReadFile existing: %v", err)
		}
		if string(data) != "hello remote linux\n" {
			t.Errorf("ReadFile content = %q, want %q", string(data), "hello remote linux\n")
		}

		_, err = fa.ReadFile(context.Background(), "nope.txt")
		if !errors.Is(err, os.ErrNotExist) {
			t.Errorf("ReadFile missing err = %v, want os.ErrNotExist", err)
		}

		_, err = fa.ReadFile(context.Background(), "sub")
		if err == nil || errors.Is(err, os.ErrNotExist) {
			t.Errorf("ReadFile dir err = %v, want non-not-exist error", err)
		}
	})

	t.Run("RunCommand quotes workdir and command via sh -c", func(t *testing.T) {
		fake.Reset()
		fake.SetHandler(func(_ context.Context, _ config.HostConfig, cmd string) (CommandResult, error) {
			if strings.Contains(cmd, "exit 7") {
				return CommandResult{Stdout: "out line\n", Stderr: "err line\n", ExitCode: 7}, nil
			}
			return CommandResult{Stdout: "ok\n", ExitCode: 0}, nil
		})

		res, err := fa.RunCommand(context.Background(), "echo 'hi' && test -f a.txt")
		if err != nil || res.ExitCode != 0 {
			t.Fatalf("RunCommand pass = (%+v, %v)", res, err)
		}
		calls := fake.Calls()
		if len(calls) != 1 {
			t.Fatalf("calls = %d, want 1", len(calls))
		}
		wantCmd := `cd '/remote/my project' && sh -c 'echo '\''hi'\'' && test -f a.txt'`
		if calls[0].Command != wantCmd {
			t.Errorf("sent command =\n  %s\nwant:\n  %s", calls[0].Command, wantCmd)
		}

		failRes, err := fa.RunCommand(context.Background(), "echo bad; exit 7")
		if err != nil {
			t.Fatalf("RunCommand non-zero err = %v, want nil", err)
		}
		if failRes.ExitCode != 7 || !strings.Contains(failRes.Output, "out line") || !strings.Contains(failRes.Output, "err line") {
			t.Errorf("RunCommand fail = %+v", failRes)
		}
	})
}

func TestWindowsRemoteFileAccess(t *testing.T) {
	host := config.HostConfig{
		Transport: "ssh",
		SSH:       "yuya@home-win",
		OS:        "windows",
	}
	fake := NewFakeTransport()
	fa, err := NewRemoteFileAccess(fake, host, `C:\Users\yuya\my work`)
	if err != nil {
		t.Fatalf("NewRemoteFileAccess: %v", err)
	}

	t.Run("Stat uses PowerShell with Windows backslash path joining", func(t *testing.T) {
		fake.Reset()
		fake.SetHandler(func(_ context.Context, _ config.HostConfig, cmd string) (CommandResult, error) {
			switch {
			case strings.Contains(cmd, `'C:\Users\yuya\my work\sub\it''s report.txt'`):
				return CommandResult{Stdout: "FILE:128\r\n"}, nil
			case strings.Contains(cmd, `'C:\Users\yuya\my work\artifacts'`):
				return CommandResult{Stdout: "DIR\r\n"}, nil
			default:
				return CommandResult{Stdout: "MISSING\r\n"}, nil
			}
		})

		// Pass forward-slash relative path; Windows implementation must join with backslashes.
		st, err := fa.Stat(context.Background(), "sub/it's report.txt")
		if err != nil {
			t.Fatalf("Stat windows file: %v", err)
		}
		if !st.Exists || st.IsDir || st.Size != 128 {
			t.Errorf("Stat windows file = %+v, want Exists=true IsDir=false Size=128", st)
		}
		calls := fake.Calls()
		if len(calls) != 1 || !strings.HasPrefix(calls[0].Command, "powershell -NoProfile -NonInteractive -Command ") {
			t.Fatalf("expected powershell invocation, got %+v", calls)
		}
		if !strings.Contains(calls[0].Command, "Test-Path -LiteralPath") || !strings.Contains(calls[0].Command, "Get-Item -LiteralPath") {
			t.Errorf("expected Test-Path and Get-Item in command: %s", calls[0].Command)
		}

		stDir, err := fa.Stat(context.Background(), "artifacts")
		if err != nil || !stDir.Exists || !stDir.IsDir {
			t.Errorf("Stat windows dir = (%+v, %v)", stDir, err)
		}

		stMissing, err := fa.Stat(context.Background(), "missing.txt")
		if err != nil || stMissing.Exists {
			t.Errorf("Stat windows missing = (%+v, %v)", stMissing, err)
		}
	})

	t.Run("ReadFile existing and missing via PowerShell", func(t *testing.T) {
		fake.Reset()
		fake.SetHandler(func(_ context.Context, _ config.HostConfig, cmd string) (CommandResult, error) {
			if strings.Contains(cmd, `'C:\Users\yuya\my work\out\summary.md'`) {
				return CommandResult{Stdout: "Windows result OK\r\n", ExitCode: 0}, nil
			}
			return CommandResult{ExitCode: remoteExitNotFound}, nil
		})

		data, err := fa.ReadFile(context.Background(), "out/summary.md")
		if err != nil {
			t.Fatalf("ReadFile windows: %v", err)
		}
		if string(data) != "Windows result OK\r\n" {
			t.Errorf("ReadFile content = %q", string(data))
		}

		_, err = fa.ReadFile(context.Background(), "missing.md")
		if !errors.Is(err, os.ErrNotExist) {
			t.Errorf("ReadFile missing err = %v, want os.ErrNotExist", err)
		}
	})

	t.Run("RunCommand sets location and safely encodes double quotes", func(t *testing.T) {
		fake.Reset()
		fake.SetHandler(func(_ context.Context, _ config.HostConfig, cmd string) (CommandResult, error) {
			if strings.Contains(cmd, "exit 3") {
				return CommandResult{Stderr: "boom\r\n", ExitCode: 3}, nil
			}
			return CommandResult{Stdout: "hello world\r\n", ExitCode: 0}, nil
		})

		res, err := fa.RunCommand(context.Background(), `echo "hello world"`)
		if err != nil || res.ExitCode != 0 {
			t.Fatalf("RunCommand windows pass = (%+v, %v)", res, err)
		}
		calls := fake.Calls()
		if len(calls) != 1 {
			t.Fatalf("calls = %d, want 1", len(calls))
		}
		got := calls[0].Command
		if !strings.Contains(got, `Set-Location -LiteralPath 'C:\Users\yuya\my work' -ErrorAction Stop`) {
			t.Errorf("expected Set-Location with Windows workdir, got: %s", got)
		}
		// Inner script must not contain raw double quotes (only the outer -Command "..." wrapper has 2 quotes).
		if strings.Count(got, `"`) != 2 {
			t.Errorf("expected only outer PowerShell -Command quotes (2), got %d in: %s", strings.Count(got, `"`), got)
		}
		if !strings.Contains(got, "[char]34") {
			t.Errorf("expected [char]34 encoding for inner double quote, got: %s", got)
		}

		failRes, err := fa.RunCommand(context.Background(), "exit 3")
		if err != nil || failRes.ExitCode != 3 || !strings.Contains(failRes.Output, "boom") {
			t.Errorf("RunCommand windows fail = (%+v, %v)", failRes, err)
		}
	})
}

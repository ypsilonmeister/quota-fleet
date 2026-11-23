package adapter

import (
	"context"
	"encoding/base64"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

func testWindowsFleetConfig() *config.Config {
	return &config.Config{
		InstanceID: "test-win-inst",
		Limits:     config.LimitsConfig{MaxClaimed: 2, AttemptsCap: 3},
		Hosts: map[string]config.HostConfig{
			"home-win": {
				Transport:      "ssh",
				SSH:            "worker@home-win.example.ts.net",
				OS:             "windows",
				WorkdirRoot:    `C:\Users\worker\agent-work`,
				OwnerFixup:     true,
				OwnerFixupUser: `WORKSTATION\worker`,
				DenyPaths:      []string{`H:\a`},
				Agents: map[string]config.HostAgentOverride{
					"claude": {
						Command: `C:\Users\worker\.local\bin\claude.exe`,
					},
					"codex": {
						Command: `C:\Users\worker\AppData\Roaming\npm\codex.cmd`,
					},
					"agy": {
						Command: `C:\Users\worker\.local\bin\agy.exe`,
					},
				},
			},
			"win-nofixup": {
				Transport:   "ssh",
				SSH:         "worker@win2.example.ts.net",
				OS:          "windows",
				WorkdirRoot: `C:\Users\worker\agent-work`,
				OwnerFixup:  false,
			},
			"win-dyn-owner": {
				Transport:   "ssh",
				SSH:         "worker@win3.example.ts.net",
				OS:          "windows",
				WorkdirRoot: `C:\Users\worker\agent-work`,
				OwnerFixup:  true,
			},
		},
		Agents: map[string]config.AgentConfig{
			"claude": {
				Preflight:      "claude --version",
				Launch:         "claude -p --model {model}",
				Resume:         "claude -p --resume {session_id}",
				SessionLogGlob: "~/.claude/projects/*/*.jsonl",
				SandboxFlag:    "--permission-mode bypassPermissions",
			},
			"codex": {
				Preflight:   "codex --version",
				Launch:      "codex exec",
				SandboxFlag: "--dangerously-bypass-approvals-and-sandbox",
			},
			"agy": {
				Preflight:   "agy --version",
				Launch:      "agy --print --model {model}",
				PromptStyle: config.PromptStyleArgument,
			},
		},
	}
}

// extractWindowsBase64Payloads extracts the two Base64 payloads (prompt.txt, agent-run.cmd)
// written via [System.Convert]::FromBase64String('...') in the PowerShell stage command.
func extractWindowsBase64Payloads(t *testing.T, stageCmd string) (string, string) {
	t.Helper()
	re := regexp.MustCompile(`FromBase64String\('([A-Za-z0-9+/=]*)'\)`)
	matches := re.FindAllStringSubmatch(stageCmd, -1)
	if len(matches) != 2 {
		t.Fatalf("expected 2 base64 payloads in windows stage command, got %d:\n%s", len(matches), stageCmd)
	}
	pBytes, err := base64.StdEncoding.DecodeString(matches[0][1])
	if err != nil {
		t.Fatalf("decode prompt.txt base64: %v", err)
	}
	sBytes, err := base64.StdEncoding.DecodeString(matches[1][1])
	if err != nil {
		t.Fatalf("decode agent-run.cmd base64: %v", err)
	}
	return string(pBytes), string(sBytes)
}

func TestWindowsAdapterPrepareWorkdirAndOwnerFixup(t *testing.T) {
	cfg := testWindowsFleetConfig()
	ctx := context.Background()

	t.Run("plain workdir with owner_fixup runs icacls on workdir only (no .git\\worktrees)", func(t *testing.T) {
		fake := NewFakeTransport()
		a := NewSSHAdapter(cfg, SSHAdapterOptions{Transport: fake})

		job := &store.Job{
			ID:      101,
			Host:    "home-win",
			Workdir: `C:\Users\worker\agent-work\job-101`,
		}
		if err := a.PrepareWorkdir(ctx, job); err != nil {
			t.Fatalf("PrepareWorkdir plain: %v", err)
		}

		calls := fake.Calls()
		if len(calls) != 2 {
			t.Fatalf("calls = %d, want 2 (mkdir + icacls)", len(calls))
		}
		if !strings.Contains(calls[0].Command, `New-Item -ItemType Directory -Force -Path 'C:\Users\worker\agent-work\job-101'`) {
			t.Errorf("unexpected mkdir command:\n%s", calls[0].Command)
		}

		fixupCmd := calls[1].Command
		for _, want := range []string{
			`$owner = 'WORKSTATION\worker'`,
			`& icacls 'C:\Users\worker\agent-work\job-101' '/setowner' $owner '/T' '/C' '/Q'`,
		} {
			if !strings.Contains(fixupCmd, want) {
				t.Errorf("owner_fixup command missing %q:\n%s", want, fixupCmd)
			}
		}
		if strings.Contains(fixupCmd, `.git\worktrees`) {
			t.Errorf("plain workdir owner_fixup must NOT include .git\\worktrees:\n%s", fixupCmd)
		}
	})

	t.Run("plain workdir without owner_fixup skips icacls", func(t *testing.T) {
		fake := NewFakeTransport()
		a := NewSSHAdapter(cfg, SSHAdapterOptions{Transport: fake})

		job := &store.Job{
			ID:      102,
			Host:    "win-nofixup",
			Workdir: `C:\Users\worker\agent-work\job-102`,
		}
		if err := a.PrepareWorkdir(ctx, job); err != nil {
			t.Fatalf("PrepareWorkdir nofixup: %v", err)
		}
		calls := fake.Calls()
		if len(calls) != 1 {
			t.Fatalf("calls = %d, want 1 (mkdir only)", len(calls))
		}
		if strings.Contains(calls[0].Command, "icacls") {
			t.Errorf("unexpected icacls when owner_fixup is false:\n%s", calls[0].Command)
		}
	})

	t.Run("owner_fixup without explicit owner_fixup_user uses dynamic COMPUTERNAME\\USERNAME", func(t *testing.T) {
		fake := NewFakeTransport()
		a := NewSSHAdapter(cfg, SSHAdapterOptions{Transport: fake})

		job := &store.Job{
			ID:      103,
			Host:    "win-dyn-owner",
			Workdir: `C:\Users\worker\agent-work\job-103`,
		}
		if err := a.PrepareWorkdir(ctx, job); err != nil {
			t.Fatalf("PrepareWorkdir dyn-owner: %v", err)
		}
		calls := fake.Calls()
		if len(calls) != 2 {
			t.Fatalf("calls = %d, want 2", len(calls))
		}
		if !strings.Contains(calls[1].Command, `$owner = $env:COMPUTERNAME + '\' + $env:USERNAME`) {
			t.Errorf("expected dynamic COMPUTERNAME\\USERNAME owner expression:\n%s", calls[1].Command)
		}
	})

	t.Run("worktree_spec add with owner_fixup includes workdir AND parent repo .git\\worktrees", func(t *testing.T) {
		fake := NewFakeTransport()
		fake.OnContains(`Test-Path -LiteralPath 'E:\worktrees\wt-104\.git'`, CommandResult{ExitCode: 1}, nil)
		fake.OnContains("worktree add", CommandResult{ExitCode: 0}, nil)
		fake.OnContains("icacls", CommandResult{ExitCode: 0}, nil)
		a := NewSSHAdapter(cfg, SSHAdapterOptions{Transport: fake})

		job := &store.Job{
			ID:      104,
			Host:    "home-win",
			Workdir: `E:\worktrees\wt-104`,
			WorktreeSpec: &store.WorktreeSpec{
				RepoPath: `E:\Projects\Git\github\sample-repo`,
				BaseRef:  "main",
				Branch:   "feat/win-adapter",
			},
		}
		if err := a.PrepareWorkdir(ctx, job); err != nil {
			t.Fatalf("PrepareWorkdir worktree add: %v", err)
		}

		calls := fake.Calls()
		if len(calls) != 3 {
			t.Fatalf("calls = %d, want 3 (check + worktree add + icacls)", len(calls))
		}
		if !strings.Contains(calls[0].Command, `Test-Path -LiteralPath 'E:\worktrees\wt-104\.git'`) {
			t.Errorf("unexpected check cmd:\n%s", calls[0].Command)
		}
		addCmd := calls[1].Command
		for _, want := range []string{
			`New-Item -ItemType Directory -Force -Path 'E:\worktrees'`,
			`& git -C 'E:\Projects\Git\github\sample-repo' show-ref --verify --quiet 'refs/heads/feat/win-adapter'`,
			`& git -C 'E:\Projects\Git\github\sample-repo' worktree add 'E:\worktrees\wt-104' 'feat/win-adapter'`,
			`& git -C 'E:\Projects\Git\github\sample-repo' worktree add -b 'feat/win-adapter' 'E:\worktrees\wt-104' 'main'`,
		} {
			if !strings.Contains(addCmd, want) {
				t.Errorf("worktree add cmd missing %q:\n%s", want, addCmd)
			}
		}

		fixupCmd := calls[2].Command
		for _, want := range []string{
			`$owner = 'WORKSTATION\worker'`,
			`& icacls 'E:\worktrees\wt-104' '/setowner' $owner '/T' '/C' '/Q'`,
			`& icacls 'E:\Projects\Git\github\sample-repo\.git\worktrees' '/setowner' $owner '/T' '/C' '/Q'`,
		} {
			if !strings.Contains(fixupCmd, want) {
				t.Errorf("worktree owner_fixup cmd missing %q:\n%s", want, fixupCmd)
			}
		}
	})

	t.Run("worktree_spec reuse when .git already exists skips worktree add", func(t *testing.T) {
		fake := NewFakeTransport()
		fake.OnContains(`Test-Path -LiteralPath 'E:\worktrees\wt-105\.git'`, CommandResult{ExitCode: 0}, nil)
		a := NewSSHAdapter(cfg, SSHAdapterOptions{Transport: fake})

		job := &store.Job{
			ID:      105,
			Host:    "home-win",
			Workdir: `E:\worktrees\wt-105`,
			WorktreeSpec: &store.WorktreeSpec{
				RepoPath: `E:\Projects\Git\github\sample-repo`,
				BaseRef:  "main",
				Branch:   "feat/reuse",
			},
		}
		if err := a.PrepareWorkdir(ctx, job); err != nil {
			t.Fatalf("PrepareWorkdir worktree reuse: %v", err)
		}
		if len(fake.Calls()) != 1 {
			t.Fatalf("calls = %d, want 1 (check only on reuse)", len(fake.Calls()))
		}
	})

	t.Run("worktree_spec detached when branch is omitted", func(t *testing.T) {
		fake := NewFakeTransport()
		fake.OnContains(`Test-Path -LiteralPath`, CommandResult{ExitCode: 1}, nil)
		a := NewSSHAdapter(cfg, SSHAdapterOptions{Transport: fake})

		job := &store.Job{
			ID:      106,
			Host:    "win-nofixup",
			Workdir: `E:\worktrees\wt-106`,
			WorktreeSpec: &store.WorktreeSpec{
				RepoPath: `E:\Projects\sample-repo`,
				BaseRef:  "origin/main",
			},
		}
		if err := a.PrepareWorkdir(ctx, job); err != nil {
			t.Fatalf("PrepareWorkdir detached: %v", err)
		}
		calls := fake.Calls()
		if len(calls) != 2 {
			t.Fatalf("calls = %d, want 2", len(calls))
		}
		if !strings.Contains(calls[1].Command, `& git -C 'E:\Projects\sample-repo' worktree add --detach 'E:\worktrees\wt-106' 'origin/main'`) {
			t.Errorf("unexpected detached worktree command:\n%s", calls[1].Command)
		}
	})

	t.Run("resolveWorkdir joins Windows backslash path when job.Workdir is empty", func(t *testing.T) {
		fake := NewFakeTransport()
		a := NewSSHAdapter(cfg, SSHAdapterOptions{Transport: fake})

		job := &store.Job{
			ID:   107,
			Host: "win-nofixup",
		}
		if err := a.PrepareWorkdir(ctx, job); err != nil {
			t.Fatalf("PrepareWorkdir default workdir: %v", err)
		}
		if !strings.Contains(fake.Calls()[0].Command, `'C:\Users\worker\agent-work\job-107'`) {
			t.Errorf("expected Windows backslash path in mkdir:\n%s", fake.Calls()[0].Command)
		}
	})
}

func TestWindowsAdapterLaunchWMIAndDrivers(t *testing.T) {
	cfg := testWindowsFleetConfig()
	ctx := context.Background()

	t.Run("claude fresh launch via WMI with bypassPermissions, model, UTF-8 prompt, and CRLF agent-run.cmd", func(t *testing.T) {
		fake := NewFakeTransport()
		fake.OnContains("Win32_Process", CommandResult{Stdout: "6123\r\n"}, nil)
		a := NewSSHAdapter(cfg, SSHAdapterOptions{Transport: fake})

		job := &store.Job{
			ID:           201,
			Title:        "Claude Windows task",
			Agent:        "claude",
			Host:         "home-win",
			Workdir:      `C:\Users\worker\agent-work\job-201`,
			Model:        "fable",
			Instructions: "\xef\xbb\xbf日本語のプロンプト\r\nテスト実行してください",
		}

		info, err := a.Launch(ctx, job)
		if err != nil {
			t.Fatalf("Launch claude: %v", err)
		}
		if info.PID != 6123 {
			t.Errorf("PID = %d, want 6123", info.PID)
		}
		if info.Meta["os"] != "windows" || info.Meta["launcher"] != "wmi" || info.Meta["sandbox_relaxed"] != "true" {
			t.Errorf("unexpected Meta: %+v", info.Meta)
		}

		calls := fake.Calls()
		if len(calls) != 2 {
			t.Fatalf("calls = %d, want 2 (stage + WMI launch)", len(calls))
		}

		// Verify stage command removes stale launch.ps1 and writes Base64 payloads.
		stageCmd := calls[0].Command
		if !strings.Contains(stageCmd, `launch.ps1`) || !strings.Contains(stageCmd, `WriteAllBytes`) {
			t.Errorf("stage cmd should clean legacy launch.ps1 and use WriteAllBytes:\n%s", stageCmd)
		}
		promptText, runCmd := extractWindowsBase64Payloads(t, stageCmd)
		if strings.HasPrefix(promptText, "\xef\xbb\xbf") {
			t.Errorf("prompt.txt must not contain UTF-8 BOM")
		}
		if promptText != "日本語のプロンプト\nテスト実行してください\n" {
			t.Errorf("promptText = %q", promptText)
		}
		if !strings.Contains(runCmd, "\r\n") {
			t.Errorf("agent-run.cmd must use CRLF line endings:\n%q", runCmd)
		}
		assertStrictCRLF(t, runCmd)
		for _, want := range []string{
			"@echo off\r\n",
			`cd /d "C:\Users\worker\agent-work\job-201" || exit /b 1`,
			`C:\Users\worker\.local\bin\claude.exe -p --model fable --permission-mode bypassPermissions < prompt.txt > agent.log 2> agent.err.log`,
			`agent.exit.tmp`,
			`agent.exit`,
			`echo EXIT:%QF_RC%`,
		} {
			if !strings.Contains(runCmd, want) {
				t.Errorf("claude agent-run.cmd missing %q:\n%s", want, runCmd)
			}
		}

		// Verify WMI Win32_Process.Create command (and NO Start-Process).
		wmiCmd := calls[1].Command
		for _, want := range []string{
			"Invoke-WmiMethod -Class Win32_Process -Name Create",
			`C:\Users\worker\agent-work\job-201\agent-run.cmd`,
			"ReturnValue -ne 0",
			`C:\Users\worker\agent-work\job-201\agent.pid`,
			"Write-Output $r.ProcessId",
		} {
			if !strings.Contains(wmiCmd, want) {
				t.Errorf("WMI launch cmd missing %q:\n%s", want, wmiCmd)
			}
		}
		if strings.Contains(wmiCmd, "Start-Process") {
			t.Errorf("Windows launcher must NEVER use Start-Process:\n%s", wmiCmd)
		}
	})

	t.Run("claude resume includes --resume <session-id>, keeps bypassPermissions, and omits --model even when job.Model is set", func(t *testing.T) {
		fake := NewFakeTransport()
		fake.OnContains("Win32_Process", CommandResult{Stdout: "6124\r\n"}, nil)
		a := NewSSHAdapter(cfg, SSHAdapterOptions{Transport: fake})

		job := &store.Job{
			ID:           202,
			Title:        "Claude resume task",
			Agent:        "claude",
			Host:         "home-win",
			Workdir:      `C:\Users\worker\agent-work\job-202`,
			Model:        "fable",
			SessionID:    "sess-9876-abcd",
			Instructions: "Fix review findings.",
		}

		info, err := a.Launch(ctx, job)
		if err != nil {
			t.Fatalf("Launch claude resume: %v", err)
		}
		if info.SessionID != "sess-9876-abcd" || info.Meta["resumed"] != "true" {
			t.Errorf("unexpected resume LaunchInfo: %+v", info)
		}

		_, runCmd := extractWindowsBase64Payloads(t, fake.Calls()[0].Command)
		wantLine := `C:\Users\worker\.local\bin\claude.exe -p --resume sess-9876-abcd --permission-mode bypassPermissions < prompt.txt > agent.log 2> agent.err.log`
		if !strings.Contains(runCmd, wantLine) {
			t.Errorf("claude resume agent-run.cmd missing %q:\n%s", wantLine, runCmd)
		}
		if strings.Contains(runCmd, "--model") || strings.Contains(runCmd, "fable") {
			t.Errorf("claude resume must omit --model even when job.Model is set:\n%s", runCmd)
		}
	})

	t.Run("codex launch uses sandbox bypass, stdin dash, and never appends </dev/null or < NUL", func(t *testing.T) {
		fake := NewFakeTransport()
		fake.OnContains("Win32_Process", CommandResult{Stdout: "7001\r\n"}, nil)
		a := NewSSHAdapter(cfg, SSHAdapterOptions{Transport: fake})

		job := &store.Job{
			ID:           203,
			Title:        "Codex Windows task",
			Agent:        "codex",
			Host:         "home-win",
			Workdir:      `E:\worktrees\wt-203`,
			WorktreeSpec: &store.WorktreeSpec{RepoPath: `E:\Projects\sample-repo`, BaseRef: "main", Branch: "feat/codex"},
			Instructions: "Implement feature and run tests.",
		}

		info, err := a.Launch(ctx, job)
		if err != nil {
			t.Fatalf("Launch codex: %v", err)
		}
		if info.PID != 7001 {
			t.Errorf("PID = %d, want 7001", info.PID)
		}

		_, runCmd := extractWindowsBase64Payloads(t, fake.Calls()[0].Command)
		assertStrictCRLF(t, runCmd)
		wantLine := `C:\Users\worker\AppData\Roaming\npm\codex.cmd exec --dangerously-bypass-approvals-and-sandbox - < prompt.txt > agent.log 2> agent.err.log`
		if !strings.Contains(runCmd, wantLine) {
			t.Errorf("codex agent-run.cmd missing %q:\n%s", wantLine, runCmd)
		}
		if strings.Contains(runCmd, "</dev/null") || strings.Contains(runCmd, "< NUL") {
			t.Errorf("codex agent-run.cmd must NEVER contain </dev/null or < NUL:\n%s", runCmd)
		}
	})

	t.Run("codex builtin default launch includes --skip-git-repo-check", func(t *testing.T) {
		cfgDefault := testWindowsFleetConfig()
		delete(cfgDefault.Agents, "codex") // exercise builtinDriverDefaults["codex"]

		fake := NewFakeTransport()
		fake.OnContains("Win32_Process", CommandResult{Stdout: "7004\r\n"}, nil)
		a := NewSSHAdapter(cfgDefault, SSHAdapterOptions{Transport: fake})

		job := &store.Job{
			ID:           2031,
			Title:        "Codex Windows builtin default",
			Agent:        "codex",
			Host:         "home-win",
			Workdir:      `C:\Users\worker\agent-work\job-2031`,
			Instructions: "Run in plain non-git workdir.",
		}

		if _, err := a.Launch(ctx, job); err != nil {
			t.Fatalf("Launch codex builtin default: %v", err)
		}

		_, runCmd := extractWindowsBase64Payloads(t, fake.Calls()[0].Command)
		wantLine := `C:\Users\worker\AppData\Roaming\npm\codex.cmd exec --skip-git-repo-check - < prompt.txt > agent.log 2> agent.err.log`
		if !strings.Contains(runCmd, wantLine) {
			t.Errorf("codex builtin default agent-run.cmd missing %q:\n%s", wantLine, runCmd)
		}
		if strings.Contains(runCmd, "</dev/null") || strings.Contains(runCmd, "< NUL") {
			t.Errorf("codex builtin default agent-run.cmd must NEVER contain </dev/null or < NUL:\n%s", runCmd)
		}
	})

	t.Run("sandbox flag omitted when workdir is outside workdir_root and no worktree_spec", func(t *testing.T) {
		fake := NewFakeTransport()
		fake.OnContains("Win32_Process", CommandResult{Stdout: "7002\r\n"}, nil)
		a := NewSSHAdapter(cfg, SSHAdapterOptions{Transport: fake})

		job := &store.Job{
			ID:           204,
			Title:        "Unisolated job",
			Agent:        "codex",
			Host:         "home-win",
			Workdir:      `E:\Projects\main-tree`,
			Instructions: "Read only check.",
		}

		info, err := a.Launch(ctx, job)
		if err != nil {
			t.Fatalf("Launch unisolated codex: %v", err)
		}
		if info.Meta["sandbox_relaxed"] != "false" {
			t.Errorf("sandbox_relaxed = %q, want false", info.Meta["sandbox_relaxed"])
		}
		_, runCmd := extractWindowsBase64Payloads(t, fake.Calls()[0].Command)
		if strings.Contains(runCmd, "--dangerously-bypass-approvals-and-sandbox") {
			t.Errorf("sandbox bypass must NOT be attached outside workdir_root without worktree_spec:\n%s", runCmd)
		}
	})

	t.Run("agy launch on windows uses PowerShell UTF-8 argument passing with < NUL", func(t *testing.T) {
		fake := NewFakeTransport()
		fake.OnContains("Win32_Process", CommandResult{Stdout: "7003\r\n"}, nil)
		a := NewSSHAdapter(cfg, SSHAdapterOptions{Transport: fake})

		job := &store.Job{
			ID:           206,
			Title:        "Agy Windows task",
			Agent:        "agy",
			Host:         "home-win",
			Workdir:      `C:\Users\worker\agent-work\job-206`,
			Model:        "gemini-2.5-pro",
			Instructions: "Summarize the repo.",
		}
		info, err := a.Launch(ctx, job)
		if err != nil {
			t.Fatalf("Launch agy: %v", err)
		}
		if info.PID != 7003 {
			t.Errorf("PID = %d, want 7003", info.PID)
		}
		_, runCmd := extractWindowsBase64Payloads(t, fake.Calls()[0].Command)
		for _, want := range []string{
			`C:\Users\worker\.local\bin\agy.exe`,
			`ReadAllText('prompt.txt', [System.Text.Encoding]::UTF8)`,
			`< NUL > agent.log 2> agent.err.log`,
		} {
			if !strings.Contains(runCmd, want) {
				t.Errorf("agy agent-run.cmd missing %q:\n%s", want, runCmd)
			}
		}
	})

	t.Run("WMI failure returns error", func(t *testing.T) {
		fake := NewFakeTransport()
		fake.OnContains("Win32_Process", CommandResult{ExitCode: 1, Stderr: "WMI Win32_Process.Create failed: ReturnValue=2"}, nil)
		a := NewSSHAdapter(cfg, SSHAdapterOptions{Transport: fake})

		job := &store.Job{
			ID:           205,
			Title:        "WMI fail",
			Agent:        "claude",
			Host:         "home-win",
			Workdir:      `C:\Users\worker\agent-work\job-205`,
			Instructions: "hello",
		}
		if _, err := a.Launch(ctx, job); err == nil || !strings.Contains(err.Error(), "ReturnValue=2") {
			t.Fatalf("expected WMI launch error containing ReturnValue=2, got %v", err)
		}
	})
}

func TestWindowsAdapterDenyPaths(t *testing.T) {
	cfg := testWindowsFleetConfig()
	ctx := context.Background()

	deniedCases := []struct {
		name string
		job  *store.Job
	}{
		{
			name: "exact H:\\a",
			job:  &store.Job{ID: 301, Agent: "claude", Host: "home-win", Workdir: `H:\a`, Instructions: "no"},
		},
		{
			name: "subdir under H:\\a",
			job:  &store.Job{ID: 302, Agent: "claude", Host: "home-win", Workdir: `H:\a\private\doc`, Instructions: "no"},
		},
		{
			name: "case-insensitive h:\\A\\sub",
			job:  &store.Job{ID: 303, Agent: "codex", Host: "home-win", Workdir: `h:\A\sub`, Instructions: "no"},
		},
		{
			name: "forward slash h:/a/sub",
			job:  &store.Job{ID: 304, Agent: "codex", Host: "home-win", Workdir: `h:/a/sub`, Instructions: "no"},
		},
		{
			name: "parent traversal into H:\\a",
			job:  &store.Job{ID: 305, Agent: "claude", Host: "home-win", Workdir: `H:\other\..\a\secret`, Instructions: "no"},
		},
		{
			name: "worktree repo_path under H:\\a",
			job: &store.Job{
				ID:           306,
				Agent:        "claude",
				Host:         "home-win",
				Workdir:      `C:\Users\worker\agent-work\wt-306`,
				WorktreeSpec: &store.WorktreeSpec{RepoPath: `H:\a\repo`},
				Instructions: "no",
			},
		},
	}

	for _, tc := range deniedCases {
		t.Run(tc.name, func(t *testing.T) {
			fake := NewFakeTransport()
			a := NewSSHAdapter(cfg, SSHAdapterOptions{Transport: fake})

			if err := a.PrepareWorkdir(ctx, tc.job); !errors.Is(err, ErrDeniedPath) {
				t.Errorf("PrepareWorkdir = %v, want ErrDeniedPath", err)
			}
			if _, err := a.Launch(ctx, tc.job); !errors.Is(err, ErrDeniedPath) {
				t.Errorf("Launch = %v, want ErrDeniedPath", err)
			}
			if _, err := a.Status(ctx, tc.job); !errors.Is(err, ErrDeniedPath) {
				t.Errorf("Status = %v, want ErrDeniedPath", err)
			}
			if _, err := a.CollectEvidence(ctx, tc.job); !errors.Is(err, ErrDeniedPath) {
				t.Errorf("CollectEvidence = %v, want ErrDeniedPath", err)
			}
			if len(fake.Calls()) != 0 {
				t.Errorf("expected 0 transport calls for denied path, got %d", len(fake.Calls()))
			}
		})
	}

	t.Run("sibling prefix H:\\archive is NOT denied by H:\\a", func(t *testing.T) {
		fake := NewFakeTransport()
		a := NewSSHAdapter(cfg, SSHAdapterOptions{Transport: fake})
		job := &store.Job{ID: 307, Agent: "claude", Host: "home-win", Workdir: `H:\archive\work`}
		if err := a.PrepareWorkdir(ctx, job); err != nil {
			t.Fatalf("PrepareWorkdir(H:\\archive\\work) unexpected error: %v", err)
		}
		if len(fake.Calls()) == 0 {
			t.Errorf("expected transport calls for allowed sibling path")
		}
	})
}

func TestWindowsAdapterStatusThreeStates(t *testing.T) {
	cfg := testWindowsFleetConfig()
	ctx := context.Background()
	job := &store.Job{ID: 401, Host: "home-win", Workdir: `C:\Users\worker\agent-work\job-401`}

	t.Run("running", func(t *testing.T) {
		fake := NewFakeTransport()
		fake.SetDefault(CommandResult{Stdout: "RUNNING:8192\r\n"}, nil)
		a := NewSSHAdapter(cfg, SSHAdapterOptions{Transport: fake})

		st, err := a.Status(ctx, job)
		if err != nil || st.State != StateRunning || st.ExitCodeUnknown || st.Detail != "pid=8192" {
			t.Fatalf("Status running = (%+v, %v)", st, err)
		}
		cmd := fake.Calls()[0].Command
		if !strings.Contains(cmd, "Get-Process -Id") || !strings.Contains(cmd, "agent.exit") || !strings.Contains(cmd, "agent.pid") {
			t.Errorf("unexpected windows status probe command:\n%s", cmd)
		}
	})

	t.Run("exited with known exit code 0", func(t *testing.T) {
		fake := NewFakeTransport()
		fake.SetDefault(CommandResult{Stdout: "EXITED:0\r\n"}, nil)
		a := NewSSHAdapter(cfg, SSHAdapterOptions{Transport: fake})

		st, err := a.Status(ctx, job)
		if err != nil || st.State != StateExited || st.ExitCode != 0 || st.ExitCodeUnknown {
			t.Fatalf("Status exited(0) = (%+v, %v)", st, err)
		}
	})

	t.Run("exited with known non-zero exit code", func(t *testing.T) {
		fake := NewFakeTransport()
		fake.SetDefault(CommandResult{Stdout: "EXITED:EXIT:7\r\n"}, nil)
		a := NewSSHAdapter(cfg, SSHAdapterOptions{Transport: fake})

		st, err := a.Status(ctx, job)
		if err != nil || st.State != StateExited || st.ExitCode != 7 || st.ExitCodeUnknown {
			t.Fatalf("Status exited(7) = (%+v, %v)", st, err)
		}
	})

	t.Run("exited with unknown exit code (e.g. Codex .cmd wrapper without agent.exit)", func(t *testing.T) {
		fake := NewFakeTransport()
		fake.SetDefault(CommandResult{Stdout: "EXITED_UNKNOWN:pid_exited_no_exit_file:8192\r\n"}, nil)
		a := NewSSHAdapter(cfg, SSHAdapterOptions{Transport: fake})

		st, err := a.Status(ctx, job)
		if err != nil {
			t.Fatalf("Status exited(unknown) error: %v", err)
		}
		if st.State != StateExited || !st.ExitCodeUnknown || st.ExitCode != 0 {
			t.Fatalf("Status exited(unknown) = %+v, want StateExited with ExitCodeUnknown=true and ExitCode=0", st)
		}
		if !strings.Contains(st.Detail, "pid_exited_no_exit_file:8192") {
			t.Errorf("Detail = %q", st.Detail)
		}
	})

	t.Run("missing when pid dead and no logs", func(t *testing.T) {
		fake := NewFakeTransport()
		fake.SetDefault(CommandResult{Stdout: "MISSING:pid_dead_no_logs:8192\r\n"}, nil)
		a := NewSSHAdapter(cfg, SSHAdapterOptions{Transport: fake})

		st, err := a.Status(ctx, job)
		if err != nil || st.State != StateMissing || st.Detail != "pid_dead_no_logs:8192" {
			t.Fatalf("Status missing(dead) = (%+v, %v)", st, err)
		}
	})

	t.Run("missing when no files exist", func(t *testing.T) {
		fake := NewFakeTransport()
		fake.SetDefault(CommandResult{Stdout: "MISSING:no_files\r\n"}, nil)
		a := NewSSHAdapter(cfg, SSHAdapterOptions{Transport: fake})

		st, err := a.Status(ctx, job)
		if err != nil || st.State != StateMissing || st.Detail != "no_files" {
			t.Fatalf("Status missing(no_files) = (%+v, %v)", st, err)
		}
	})

	t.Run("transport error propagates", func(t *testing.T) {
		fake := NewFakeTransport()
		netErr := errors.New("ssh connection closed")
		fake.SetDefault(CommandResult{}, netErr)
		a := NewSSHAdapter(cfg, SSHAdapterOptions{Transport: fake})

		if _, err := a.Status(ctx, job); !errors.Is(err, netErr) {
			t.Fatalf("Status transport err = %v, want %v", err, netErr)
		}
	})
}

func TestWindowsAdapterCollectEvidence(t *testing.T) {
	cfg := testWindowsFleetConfig()
	ctx := context.Background()
	job := &store.Job{ID: 501, Host: "home-win", Workdir: `E:\worktrees\wt-501`}

	t.Run("codex evidence with git status, diff --stat, agent.err.log, and unknown exit code", func(t *testing.T) {
		fake := NewFakeTransport()
		fake.OnContains("status --porcelain", CommandResult{
			Stdout: " M internal/adapter/windows_launcher.go\r\n?? prompt.txt\r\n?? agent-run.cmd\r\n?? agent.log\r\n?? agent.err.log\r\n?? agent.pid\r\n?? summary.md\r\n",
		}, nil)
		fake.OnContains("diff --stat", CommandResult{
			Stdout: " internal/adapter/windows_launcher.go | 120 ++++++++++++++++++++\r\n 1 file changed, 120 insertions(+)\r\n",
		}, nil)
		fake.OnContains("Write-Output '1:", CommandResult{
			Stdout: "1:agent.log\r\n1:agent.err.log\r\n0:agent.exit\r\n1:summary.md\r\n0:result.txt\r\n",
		}, nil)
		fake.OnContains(`agent.log' -Encoding UTF8 -Tail`, CommandResult{
			Stdout: "",
		}, nil)
		fake.OnContains(`agent.err.log' -Encoding UTF8 -Tail`, CommandResult{
			Stdout: "Final answer: all checks passed.\r\ntokens used: 12345\r\n",
		}, nil)
		fake.OnContains(`agent.exit`, CommandResult{
			Stdout: "",
		}, nil)

		a := NewSSHAdapter(cfg, SSHAdapterOptions{Transport: fake})
		ev, err := a.CollectEvidence(ctx, job)
		if err != nil {
			t.Fatalf("CollectEvidence: %v", err)
		}
		if !ev.HasDiff {
			t.Errorf("HasDiff = false, want true")
		}
		if !strings.Contains(ev.DiffSummary, "windows_launcher.go") || !strings.Contains(ev.DiffSummary, "1 file changed") {
			t.Errorf("DiffSummary missing status or diff --stat: %q", ev.DiffSummary)
		}
		for _, ctrl := range []string{"prompt.txt", "agent-run.cmd", "agent.log", "agent.err.log", "agent.pid"} {
			if strings.Contains(ev.DiffSummary, ctrl) {
				t.Errorf("DiffSummary should filter control file %q: %q", ctrl, ev.DiffSummary)
			}
		}
		if ev.CommandOutputs["git_status"] == "" || ev.CommandOutputs["git_diff_stat"] == "" {
			t.Errorf("CommandOutputs missing git_status/git_diff_stat: %+v", ev.CommandOutputs)
		}
		if !ev.Artifacts["agent.err.log"] || ev.Artifacts["agent.exit"] || !ev.Artifacts["summary.md"] || ev.Artifacts["result.txt"] {
			t.Errorf("unexpected Artifacts: %+v", ev.Artifacts)
		}
		if !strings.Contains(ev.ErrLogTail, "tokens used: 12345") {
			t.Errorf("ErrLogTail = %q", ev.ErrLogTail)
		}
		if !strings.Contains(ev.FinalReport, "Final answer: all checks passed.") {
			t.Errorf("FinalReport should fall back to agent.err.log when agent.log is empty, got %q", ev.FinalReport)
		}
		if !ev.ExitCodeUnknown || ev.ExitCode != 0 {
			t.Errorf("ExitCodeUnknown=%v ExitCode=%d, want true / 0", ev.ExitCodeUnknown, ev.ExitCode)
		}
		if len(ev.Notes) == 0 || !strings.Contains(ev.Notes[0], "exit code unknown") {
			t.Errorf("Notes should note unknown exit code, got %+v", ev.Notes)
		}
	})

	t.Run("claude evidence with known exit code and non-git workdir", func(t *testing.T) {
		fake := NewFakeTransport()
		fake.OnContains("status --porcelain", CommandResult{ExitCode: 128, Stderr: "not a git repository"}, nil)
		fake.OnContains("Write-Output '1:", CommandResult{
			Stdout: "1:agent.log\r\n1:agent.err.log\r\n1:agent.exit\r\n0:summary.md\r\n0:result.txt\r\n",
		}, nil)
		fake.OnContains(`agent.log' -Encoding UTF8 -Tail`, CommandResult{
			Stdout: "Done!\r\nEXIT:0\r\n",
		}, nil)
		fake.OnContains(`agent.err.log' -Encoding UTF8 -Tail`, CommandResult{
			Stdout: "",
		}, nil)
		fake.OnContains(`agent.exit`, CommandResult{
			Stdout: "0\r\n",
		}, nil)

		a := NewSSHAdapter(cfg, SSHAdapterOptions{Transport: fake})
		ev, err := a.CollectEvidence(ctx, job)
		if err != nil {
			t.Fatalf("CollectEvidence: %v", err)
		}
		if ev.HasDiff {
			t.Errorf("HasDiff = true for non-git dir, want false")
		}
		if ev.ExitCodeUnknown || ev.ExitCode != 0 {
			t.Errorf("ExitCodeUnknown=%v ExitCode=%d, want false / 0", ev.ExitCodeUnknown, ev.ExitCode)
		}
		if len(ev.Notes) != 0 {
			t.Errorf("expected no unknown-exit notes when agent.exit is present, got %+v", ev.Notes)
		}
		if !strings.Contains(ev.FinalReport, "Done!") {
			t.Errorf("FinalReport = %q", ev.FinalReport)
		}
	})
}

func TestWindowsAdapterPreflight(t *testing.T) {
	cfg := testWindowsFleetConfig()
	fake := NewFakeTransport()
	fake.SetDefault(CommandResult{Stdout: "2.1.325\r\n", ExitCode: 0}, nil)
	a := NewSSHAdapter(cfg, SSHAdapterOptions{Transport: fake})

	res, err := a.Preflight(context.Background(), "home-win", "claude")
	if err != nil {
		t.Fatalf("Preflight windows claude: %v", err)
	}
	if strings.TrimSpace(res.Stdout) != "2.1.325" {
		t.Errorf("Stdout = %q", res.Stdout)
	}
	cmd := fake.Calls()[0].Command
	if !strings.Contains(cmd, `powershell -NoProfile -NonInteractive -Command`) ||
		!strings.Contains(cmd, `& 'C:\Users\worker\.local\bin\claude.exe' '--version'`) {
		t.Errorf("unexpected windows preflight command:\n%s", cmd)
	}
}

func assertStrictCRLF(t *testing.T, script string) {
	t.Helper()
	if script == "" {
		t.Fatalf("expected non-empty script")
	}
	if !strings.HasSuffix(script, "\r\n") {
		t.Errorf("script must end with CRLF, got %q", script)
	}
	if strings.Contains(script, "\r\r\n") {
		t.Errorf("script must not contain doubled CR (\\r\\r\\n): %q", script)
	}
	for i := 0; i < len(script); i++ {
		if script[i] == '\n' && (i == 0 || script[i-1] != '\r') {
			t.Errorf("script contains bare LF at byte %d: %q", i, script)
		}
		if script[i] == '\r' && (i+1 >= len(script) || script[i+1] != '\n') {
			t.Errorf("script contains bare CR at byte %d: %q", i, script)
		}
	}
}

func TestBuildWindowsRunScriptCRLFNormalization(t *testing.T) {
	baseDriver := AgentDriver{
		Name: "codex",
		Env: map[string]string{
			"FOO": "bar",
		},
	}

	t.Run("standard single-line launchCmd has no bare LF and ends every line with CRLF", func(t *testing.T) {
		script := BuildWindowsRunScript(
			`C:\Users\worker\agent-work\job-900`,
			baseDriver,
			`C:\Users\worker\AppData\Roaming\npm\codex.cmd exec --skip-git-repo-check - < prompt.txt > agent.log 2> agent.err.log`,
		)
		assertStrictCRLF(t, script)
		if !strings.Contains(script, "@echo off\r\nset \"FOO=bar\"\r\ncd /d \"C:\\Users\\worker\\agent-work\\job-900\" || exit /b 1\r\n") {
			t.Errorf("unexpected script header:\n%q", script)
		}
	})

	t.Run("launchCmd containing bare LF and mixed CRLF is normalized without double CR", func(t *testing.T) {
		mixedLaunch := "echo step1\necho step2\r\necho step3\nC:\\tools\\codex.cmd exec - < prompt.txt > agent.log 2> agent.err.log\r\n"
		script := BuildWindowsRunScript(`C:\Users\worker\agent-work\job-901`, baseDriver, mixedLaunch)
		assertStrictCRLF(t, script)

		want := "echo step1\r\necho step2\r\necho step3\r\nC:\\tools\\codex.cmd exec - < prompt.txt > agent.log 2> agent.err.log\r\n"
		if !strings.Contains(script, want) {
			t.Errorf("normalized launchCmd not found in script:\nwant substring: %q\ngot script: %q", want, script)
		}
	})

	t.Run("launchCmd containing bare CR or double CR before LF is normalized to strict CRLF", func(t *testing.T) {
		weirdLaunch := "echo a\recho b\r\r\necho c"
		script := BuildWindowsRunScript(`C:\Users\worker\agent-work\job-902`, AgentDriver{Name: "codex"}, weirdLaunch)
		assertStrictCRLF(t, script)

		want := "echo a\r\necho b\r\necho c\r\n"
		if !strings.Contains(script, want) {
			t.Errorf("normalized launchCmd not found in script:\nwant substring: %q\ngot script: %q", want, script)
		}
	})
}

// Regression test: with `setlocal` present in agent-run.cmd, a WMI-launched batch's child
// processes receive C:\Windows\System32 as their working directory instead of the `cd /d`
// target (field-verified on home-win). Driver env vars must still be emitted as plain `set`
// lines in the dedicated WMI process.
func TestBuildWindowsRunScriptOmitsSetlocal(t *testing.T) {
	driver := AgentDriver{
		Name: "codex",
		Env: map[string]string{
			"ALPHA": "one",
			"BETA":  "two",
		},
	}

	script := BuildWindowsRunScript(
		`C:\Users\worker\agent-work\job-903`,
		driver,
		`C:\Users\worker\AppData\Roaming\npm\codex.cmd exec --skip-git-repo-check - < prompt.txt > agent.log 2> agent.err.log`,
	)

	if strings.Contains(strings.ToLower(script), "setlocal") {
		t.Fatalf("generated Windows run script must NOT contain setlocal (breaks child CWD under WMI):\n%s", script)
	}

	wantHeader := "@echo off\r\nset \"ALPHA=one\"\r\nset \"BETA=two\"\r\ncd /d \"C:\\Users\\worker\\agent-work\\job-903\" || exit /b 1\r\n"
	if !strings.HasPrefix(script, wantHeader) {
		t.Errorf("generated script header mismatch:\nwant prefix: %q\ngot script:  %q", wantHeader, script)
	}
}

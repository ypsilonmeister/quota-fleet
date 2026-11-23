package adapter

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

func testFleetConfig() *config.Config {
	return &config.Config{
		InstanceID: "test-inst",
		Limits:     config.LimitsConfig{MaxClaimed: 2, AttemptsCap: 3},
		Hosts: map[string]config.HostConfig{
			"ubuntu": {
				Transport:   "ssh",
				SSH:         "worker@ubuntu.example.ts.net",
				OS:          "linux",
				WorkdirRoot: "~/agent-work",
				DenyPaths:   []string{"/etc", "/home/worker/secret"},
				Agents: map[string]config.HostAgentOverride{
					"agy": {
						Command: "~/.local/bin/agy",
					},
				},
			},
			"penguin": {
				Transport:   "ssh",
				SSH:         "worker@penguin.example.ts.net",
				OS:          "linux",
				WorkdirRoot: "/home/worker/agent-work",
				Agents: map[string]config.HostAgentOverride{
					"codex": {
						Command: "codex",
						Env: map[string]string{
							"PATH": "~/.config/nvm/versions/node/v24.18.1/bin:$PATH",
						},
					},
				},
			},
			"home-win": {
				Transport:   "ssh",
				SSH:         "worker@home-win.example.ts.net",
				OS:          "windows",
				WorkdirRoot: `C:\Users\worker\agent-work`,
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
				Preflight: "agy --version",
				Launch:    "agy --print --model {model}",
			},
		},
	}
}

// extractBase64Payloads extracts the two base64 literals (prompt.txt, run.sh) from a stage command.
func extractBase64Payloads(t *testing.T, stageCmd string) (string, string) {
	t.Helper()
	re := regexp.MustCompile(`printf '%s' '([A-Za-z0-9+/=]*)' \| base64 -d`)
	matches := re.FindAllStringSubmatch(stageCmd, -1)
	if len(matches) != 2 {
		t.Fatalf("expected 2 base64 payloads in stage command, got %d:\n%s", len(matches), stageCmd)
	}
	pBytes, err := base64.StdEncoding.DecodeString(matches[0][1])
	if err != nil {
		t.Fatalf("decode prompt.txt base64: %v", err)
	}
	sBytes, err := base64.StdEncoding.DecodeString(matches[1][1])
	if err != nil {
		t.Fatalf("decode run.sh base64: %v", err)
	}
	return string(pBytes), string(sBytes)
}

func TestSSHAdapterPrepareWorkdirPlainAndWorktree(t *testing.T) {
	cfg := testFleetConfig()
	ctx := context.Background()

	// 1. Plain workdir without worktree_spec runs mkdir -p.
	t.Run("plain mkdir", func(t *testing.T) {
		fake := NewFakeTransport()
		a := NewSSHAdapter(cfg, SSHAdapterOptions{Transport: fake})

		job := &store.Job{ID: 1, Host: "ubuntu", Workdir: "~/agent-work/job-1"}
		if err := a.PrepareWorkdir(ctx, job); err != nil {
			t.Fatalf("PrepareWorkdir: %v", err)
		}
		calls := fake.Calls()
		if len(calls) != 1 {
			t.Fatalf("calls = %d, want 1", len(calls))
		}
		if !strings.Contains(calls[0].Command, `mkdir -p "$HOME"/'agent-work/job-1'`) {
			t.Errorf("unexpected mkdir command: %s", calls[0].Command)
		}
	})

	// 2. Worktree creation when workdir/.git does not exist yet.
	t.Run("worktree add when absent", func(t *testing.T) {
		fake := NewFakeTransport()
		fake.OnContains("test -e ", CommandResult{ExitCode: 1}, nil)
		fake.OnContains("worktree add", CommandResult{ExitCode: 0}, nil)
		a := NewSSHAdapter(cfg, SSHAdapterOptions{Transport: fake})

		job := &store.Job{
			ID:      2,
			Host:    "penguin",
			Workdir: "/home/worker/worktrees/wt-2",
			WorktreeSpec: &store.WorktreeSpec{
				RepoPath: "/home/worker/repos/quota-fleet",
				BaseRef:  "main",
				Branch:   "feat/adapter",
			},
		}
		if err := a.PrepareWorkdir(ctx, job); err != nil {
			t.Fatalf("PrepareWorkdir worktree add: %v", err)
		}
		calls := fake.Calls()
		if len(calls) != 2 {
			t.Fatalf("calls = %d, want 2", len(calls))
		}
		if !strings.Contains(calls[0].Command, "test -e '/home/worker/worktrees/wt-2/.git'") {
			t.Errorf("unexpected check cmd: %s", calls[0].Command)
		}
		addCmd := calls[1].Command
		for _, want := range []string{
			"mkdir -p '/home/worker/worktrees'",
			"git -C '/home/worker/repos/quota-fleet' show-ref --verify --quiet 'refs/heads/feat/adapter'",
			"git -C '/home/worker/repos/quota-fleet' worktree add '/home/worker/worktrees/wt-2' 'feat/adapter'",
			"git -C '/home/worker/repos/quota-fleet' worktree add -b 'feat/adapter' '/home/worker/worktrees/wt-2' 'main'",
		} {
			if !strings.Contains(addCmd, want) {
				t.Errorf("worktree add cmd missing %q:\n%s", want, addCmd)
			}
		}
	})

	// 3. Worktree reuse when workdir/.git already exists.
	t.Run("worktree reuse when present", func(t *testing.T) {
		fake := NewFakeTransport()
		fake.OnContains("test -e ", CommandResult{ExitCode: 0}, nil)
		a := NewSSHAdapter(cfg, SSHAdapterOptions{Transport: fake})

		job := &store.Job{
			ID:      3,
			Host:    "penguin",
			Workdir: "/home/worker/worktrees/wt-3",
			WorktreeSpec: &store.WorktreeSpec{
				RepoPath: "/home/worker/repos/quota-fleet",
				BaseRef:  "main",
				Branch:   "feat/reuse",
			},
		}
		if err := a.PrepareWorkdir(ctx, job); err != nil {
			t.Fatalf("PrepareWorkdir worktree reuse: %v", err)
		}
		calls := fake.Calls()
		if len(calls) != 1 {
			t.Fatalf("calls = %d, want 1 (should skip git worktree add on reuse)", len(calls))
		}
	})

	// 4. Denied path rejection.
	t.Run("deny_paths rejected", func(t *testing.T) {
		fake := NewFakeTransport()
		a := NewSSHAdapter(cfg, SSHAdapterOptions{Transport: fake})

		job := &store.Job{ID: 4, Host: "ubuntu", Workdir: "/home/worker/secret/sub"}
		err := a.PrepareWorkdir(ctx, job)
		if !errors.Is(err, ErrDeniedPath) {
			t.Fatalf("PrepareWorkdir deny_paths err = %v, want ErrDeniedPath", err)
		}
		if len(fake.Calls()) != 0 {
			t.Errorf("expected 0 transport calls for denied path, got %d", len(fake.Calls()))
		}
	})
}

func TestSSHAdapterLaunchDriversAndCodexTrap(t *testing.T) {
	cfg := testFleetConfig()
	ctx := context.Background()

	// 1. Claude on ubuntu under workdir_root (gets sandbox_flag, model substituted, BOM stripped).
	t.Run("claude launch with sandbox_flag and UTF-8 prompt", func(t *testing.T) {
		fake := NewFakeTransport()
		fake.OnContains("setsid nohup", CommandResult{Stdout: "4242\n"}, nil)
		a := NewSSHAdapter(cfg, SSHAdapterOptions{Transport: fake})

		job := &store.Job{
			ID:           10,
			Title:        "Implement adapter",
			Agent:        "claude",
			Host:         "ubuntu",
			Workdir:      "~/agent-work/job-10",
			Model:        "claude-sonnet-4-6",
			Instructions: "\xef\xbb\xbf日本語の指示文\r\nテストを通してね",
		}

		info, err := a.Launch(ctx, job)
		if err != nil {
			t.Fatalf("Launch claude: %v", err)
		}
		if info.PID != 4242 {
			t.Errorf("PID = %d, want 4242", info.PID)
		}
		if info.Meta["sandbox_relaxed"] != "true" {
			t.Errorf("sandbox_relaxed = %q, want true", info.Meta["sandbox_relaxed"])
		}

		calls := fake.Calls()
		if len(calls) != 2 {
			t.Fatalf("calls = %d, want 2 (stage + detach)", len(calls))
		}

		promptText, runSh := extractBase64Payloads(t, calls[0].Command)
		if strings.HasPrefix(promptText, "\xef\xbb\xbf") {
			t.Errorf("prompt.txt must not start with UTF-8 BOM")
		}
		if strings.Contains(promptText, "\r") {
			t.Errorf("prompt.txt should use LF line endings: %q", promptText)
		}
		if promptText != "日本語の指示文\nテストを通してね\n" {
			t.Errorf("promptText = %q", promptText)
		}

		for _, want := range []string{
			"#!/bin/sh",
			`cd "$HOME"/'agent-work/job-10' || exit 1`,
			`claude -p --model 'claude-sonnet-4-6' --permission-mode bypassPermissions < "$HOME"/'agent-work/job-10/prompt.txt' > "$HOME"/'agent-work/job-10/agent.log' 2>&1`,
			`rc=$?`,
			`agent.exit.tmp`,
			`agent.exit`,
		} {
			if !strings.Contains(runSh, want) {
				t.Errorf("claude run.sh missing %q:\n%s", want, runSh)
			}
		}

		if !strings.Contains(calls[1].Command, `setsid nohup sh "$HOME"/'agent-work/job-10/run.sh'`) ||
			!strings.Contains(calls[1].Command, `agent.pid`) {
			t.Errorf("unexpected detach command: %s", calls[1].Command)
		}
	})

	// 2. Codex on penguin: host env PATH override + stdin from prompt.txt + NO </dev/null on codex command.
	t.Run("codex launch respects stdin trap and host env override", func(t *testing.T) {
		fake := NewFakeTransport()
		fake.OnContains("setsid nohup", CommandResult{Stdout: "9001\n"}, nil)
		a := NewSSHAdapter(cfg, SSHAdapterOptions{Transport: fake})

		job := &store.Job{
			ID:           11,
			Title:        "Codex review",
			Agent:        "codex",
			Host:         "penguin",
			Workdir:      "/home/worker/agent-work/job-11",
			Instructions: "Review the diff carefully.",
		}

		info, err := a.Launch(ctx, job)
		if err != nil {
			t.Fatalf("Launch codex: %v", err)
		}
		if info.PID != 9001 {
			t.Errorf("PID = %d, want 9001", info.PID)
		}

		calls := fake.Calls()
		_, runSh := extractBase64Payloads(t, calls[0].Command)

		if !strings.Contains(runSh, `export PATH="$HOME/.config/nvm/versions/node/v24.18.1/bin:$PATH"`) {
			t.Errorf("codex run.sh missing nvm PATH export:\n%s", runSh)
		}
		wantCmd := `codex exec --dangerously-bypass-approvals-and-sandbox < '/home/worker/agent-work/job-11/prompt.txt' > '/home/worker/agent-work/job-11/agent.log' 2>&1`
		if !strings.Contains(runSh, wantCmd) {
			t.Errorf("codex run.sh missing expected launch line %q:\n%s", wantCmd, runSh)
		}
		// Crucial Codex gotcha check: run.sh must NEVER redirect </dev/null into codex.
		if strings.Contains(runSh, "</dev/null") {
			t.Errorf("codex run.sh must NOT contain </dev/null:\n%s", runSh)
		}
	})

	// 3. Agy on ubuntu: host command override (`~/.local/bin/agy`), argument prompt style, and empty model omits `--model`.
	t.Run("agy launch applies host command override, argument prompt style, and omits empty model", func(t *testing.T) {
		fake := NewFakeTransport()
		fake.OnContains("setsid nohup", CommandResult{Stdout: "7777\n"}, nil)
		a := NewSSHAdapter(cfg, SSHAdapterOptions{Transport: fake})

		job := &store.Job{
			ID:           12,
			Title:        "Agy task",
			Agent:        "agy",
			Host:         "ubuntu",
			Workdir:      "~/agent-work/job-12",
			Model:        "",
			Instructions: "Summarize changes with $HOME and 'quotes'.",
		}

		if _, err := a.Launch(ctx, job); err != nil {
			t.Fatalf("Launch agy: %v", err)
		}

		calls := fake.Calls()
		promptText, runSh := extractBase64Payloads(t, calls[0].Command)
		if !strings.Contains(promptText, "Summarize changes with $HOME and 'quotes'.") {
			t.Errorf("promptText = %q", promptText)
		}
		wantCmd := `"$HOME"/'.local/bin/agy' --print "$(cat "$HOME"/'agent-work/job-12/prompt.txt')" </dev/null > "$HOME"/'agent-work/job-12/agent.log' 2>&1`
		if !strings.Contains(runSh, wantCmd) {
			t.Errorf("agy run.sh missing expected launch line %q:\n%s", wantCmd, runSh)
		}
		if strings.Contains(runSh, "< \"$HOME\"/'agent-work/job-12/prompt.txt'") {
			t.Errorf("agy run.sh must not pass prompt via stdin redirection:\n%s", runSh)
		}
		if strings.Contains(runSh, "Summarize changes") {
			t.Errorf("agy run.sh must read prompt from file at runtime rather than embedding prompt text:\n%s", runSh)
		}
		if strings.Contains(runSh, "--model") || strings.Contains(runSh, "{model}") {
			t.Errorf("agy run.sh should omit --model when job.Model is empty:\n%s", runSh)
		}
	})

	// 4. Agy built-in default launch with explicit model puts --model before --print so --print receives prompt arg.
	t.Run("agy builtin default launch with model", func(t *testing.T) {
		cfgDefault := testFleetConfig()
		delete(cfgDefault.Agents, "agy") // exercise builtinDriverDefaults["agy"]

		fake := NewFakeTransport()
		fake.OnContains("setsid nohup", CommandResult{Stdout: "7778\n"}, nil)
		a := NewSSHAdapter(cfgDefault, SSHAdapterOptions{Transport: fake})

		job := &store.Job{
			ID:           13,
			Title:        "Agy model task",
			Agent:        "agy",
			Host:         "ubuntu",
			Workdir:      "~/agent-work/job-13",
			Model:        "gemini-2.5-pro",
			Instructions: "Check tests.",
		}

		if _, err := a.Launch(ctx, job); err != nil {
			t.Fatalf("Launch agy with model: %v", err)
		}

		_, runSh := extractBase64Payloads(t, fake.Calls()[0].Command)
		wantCmd := `"$HOME"/'.local/bin/agy' --model 'gemini-2.5-pro' --print "$(cat "$HOME"/'agent-work/job-13/prompt.txt')" </dev/null > "$HOME"/'agent-work/job-13/agent.log' 2>&1`
		if !strings.Contains(runSh, wantCmd) {
			t.Errorf("agy run.sh missing expected model launch line %q:\n%s", wantCmd, runSh)
		}
	})

	// 5. Config prompt_style overrides built-in defaults (both agy->stdin and claude->argument).
	t.Run("config prompt_style overrides defaults", func(t *testing.T) {
		cfgOverride := testFleetConfig()
		agyCfg := cfgOverride.Agents["agy"]
		agyCfg.PromptStyle = config.PromptStyleStdin
		cfgOverride.Agents["agy"] = agyCfg

		claudeCfg := cfgOverride.Agents["claude"]
		claudeCfg.PromptStyle = config.PromptStyleArgument
		cfgOverride.Agents["claude"] = claudeCfg

		fake := NewFakeTransport()
		fake.OnContains("setsid nohup", CommandResult{Stdout: "8888\n"}, nil)
		a := NewSSHAdapter(cfgOverride, SSHAdapterOptions{Transport: fake})

		agyJob := &store.Job{
			ID:           14,
			Title:        "Agy stdin override",
			Agent:        "agy",
			Host:         "ubuntu",
			Workdir:      "~/agent-work/job-14",
			Instructions: "hello",
		}
		if _, err := a.Launch(ctx, agyJob); err != nil {
			t.Fatalf("Launch agy stdin override: %v", err)
		}
		_, agyRunSh := extractBase64Payloads(t, fake.Calls()[0].Command)
		wantAgy := `"$HOME"/'.local/bin/agy' --print < "$HOME"/'agent-work/job-14/prompt.txt' > "$HOME"/'agent-work/job-14/agent.log' 2>&1`
		if !strings.Contains(agyRunSh, wantAgy) {
			t.Errorf("agy stdin override run.sh missing %q:\n%s", wantAgy, agyRunSh)
		}

		claudeJob := &store.Job{
			ID:           15,
			Title:        "Claude argument override",
			Agent:        "claude",
			Host:         "ubuntu",
			Workdir:      "~/agent-work/job-15",
			Model:        "claude-sonnet-4-6",
			Instructions: "hello",
		}
		if _, err := a.Launch(ctx, claudeJob); err != nil {
			t.Fatalf("Launch claude argument override: %v", err)
		}
		_, claudeRunSh := extractBase64Payloads(t, fake.Calls()[2].Command)
		wantClaude := `claude -p --model 'claude-sonnet-4-6' --permission-mode bypassPermissions "$(cat "$HOME"/'agent-work/job-15/prompt.txt')" </dev/null > "$HOME"/'agent-work/job-15/agent.log' 2>&1`
		if !strings.Contains(claudeRunSh, wantClaude) {
			t.Errorf("claude argument override run.sh missing %q:\n%s", wantClaude, claudeRunSh)
		}
	})

	// 6. Verify generated argument-style run.sh actually passes prompt.txt verbatim under POSIX /bin/sh.
	t.Run("argument style run.sh executes cleanly under sh with special chars", func(t *testing.T) {
		workdir := t.TempDir()
		binPath := filepath.Join(workdir, "fake-agy.sh")
		capturePath := filepath.Join(workdir, "captured-arg.txt")
		fakeBin := "#!/bin/sh\n" +
			"if [ \"$1\" != \"--print\" ]; then echo \"bad flag: $1\" >&2; exit 2; fi\n" +
			"printf '%s' \"$2\" > " + shellQuote(capturePath) + "\n"
		if err := os.WriteFile(binPath, []byte(fakeBin), 0o700); err != nil {
			t.Fatalf("write fake-agy.sh: %v", err)
		}

		job := &store.Job{
			ID:           16,
			Title:        "Shell roundtrip",
			Agent:        "agy",
			Host:         "local",
			Workdir:      workdir,
			Instructions: "Line 1 with $UNSET_VAR `echo nope` 'single' \"double\"\n日本語テスト",
		}
		drv := AgentDriver{
			Name:            "agy",
			Launch:          "agy --model {model} --print",
			PromptStyle:     PromptStyleArgument,
			CommandOverride: binPath,
		}
		promptPath := filepath.Join(workdir, promptFileName)
		logPath := filepath.Join(workdir, logFileName)
		runShPath := filepath.Join(workdir, runScriptName)

		promptText := BuildPromptText(job)
		if err := os.WriteFile(promptPath, []byte(promptText), 0o600); err != nil {
			t.Fatalf("write prompt.txt: %v", err)
		}
		launchCmd, err := drv.BuildLaunchCommand(job, workdir, promptPath, logPath, false)
		if err != nil {
			t.Fatalf("BuildLaunchCommand: %v", err)
		}
		runScript := BuildRunScript(workdir, drv, launchCmd)
		if err := os.WriteFile(runShPath, []byte(runScript), 0o700); err != nil {
			t.Fatalf("write run.sh: %v", err)
		}

		cmd := exec.Command("sh", runShPath)
		if out, err := cmd.CombinedOutput(); err != nil {
			logBytes, _ := os.ReadFile(logPath)
			t.Fatalf("sh run.sh failed: %v\ncombined: %s\nagent.log: %s\nrun.sh:\n%s", err, string(out), string(logBytes), runScript)
		}
		gotArg, err := os.ReadFile(capturePath)
		if err != nil {
			t.Fatalf("read captured arg: %v", err)
		}
		wantArg := strings.TrimRight(promptText, "\n")
		if string(gotArg) != wantArg {
			t.Errorf("captured arg = %q, want %q", string(gotArg), wantArg)
		}
		exitBytes, err := os.ReadFile(filepath.Join(workdir, exitFileName))
		if err != nil || strings.TrimSpace(string(exitBytes)) != "0" {
			t.Errorf("agent.exit = %q (err=%v), want 0", string(exitBytes), err)
		}
	})
}

func TestSSHAdapterSandboxFlagPolicy(t *testing.T) {
	cfg := testFleetConfig()
	ctx := context.Background()

	cases := []struct {
		name        string
		job         *store.Job
		wantSandbox bool
	}{
		{
			name: "allowed when workdir is under workdir_root",
			job: &store.Job{
				ID:           20,
				Agent:        "claude",
				Host:         "penguin",
				Workdir:      "/home/worker/agent-work/task-20",
				Instructions: "ok",
			},
			wantSandbox: true,
		},
		{
			name: "allowed when worktree_spec is set even outside workdir_root",
			job: &store.Job{
				ID:      21,
				Agent:   "claude",
				Host:    "penguin",
				Workdir: "/tmp/isolated-wt-21",
				WorktreeSpec: &store.WorktreeSpec{
					RepoPath: "/home/worker/repos/app",
					BaseRef:  "main",
					Branch:   "feat/21",
				},
				Instructions: "ok",
			},
			wantSandbox: true,
		},
		{
			name: "denied when workdir is outside workdir_root and no worktree_spec",
			job: &store.Job{
				ID:           22,
				Agent:        "claude",
				Host:         "penguin",
				Workdir:      "/home/worker/other-repo",
				Instructions: "ok",
			},
			wantSandbox: false,
		},
		{
			name: "denied on sibling prefix collision",
			job: &store.Job{
				ID:           23,
				Agent:        "claude",
				Host:         "penguin",
				Workdir:      "/home/worker/agent-work-evil/task-23",
				Instructions: "ok",
			},
			wantSandbox: false,
		},
		{
			name: "denied on parent traversal out of workdir_root",
			job: &store.Job{
				ID:           24,
				Agent:        "claude",
				Host:         "penguin",
				Workdir:      "/home/worker/agent-work/../outside",
				Instructions: "ok",
			},
			wantSandbox: false,
		},
		{
			name: "denied when workdir equals workdir_root itself",
			job: &store.Job{
				ID:           25,
				Agent:        "claude",
				Host:         "penguin",
				Workdir:      "/home/worker/agent-work",
				Instructions: "ok",
			},
			wantSandbox: false,
		},
		{
			name: "not attached when agent has no sandbox_flag in config",
			job: &store.Job{
				ID:           26,
				Agent:        "agy",
				Host:         "ubuntu",
				Workdir:      "~/agent-work/task-26",
				Instructions: "ok",
			},
			wantSandbox: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := NewFakeTransport()
			fake.OnContains("setsid nohup", CommandResult{Stdout: "1234\n"}, nil)
			a := NewSSHAdapter(cfg, SSHAdapterOptions{Transport: fake})

			if _, err := a.Launch(ctx, tc.job); err != nil {
				t.Fatalf("Launch: %v", err)
			}
			_, runSh := extractBase64Payloads(t, fake.Calls()[0].Command)
			hasFlag := strings.Contains(runSh, "--permission-mode bypassPermissions") ||
				strings.Contains(runSh, "--dangerously-bypass-approvals-and-sandbox")
			if hasFlag != tc.wantSandbox {
				t.Errorf("sandbox_flag present = %v, want %v; run.sh:\n%s", hasFlag, tc.wantSandbox, runSh)
			}
		})
	}
}

func TestSSHAdapterStatusThreeStates(t *testing.T) {
	cfg := testFleetConfig()
	ctx := context.Background()
	job := &store.Job{ID: 30, Host: "ubuntu", Workdir: "~/agent-work/job-30"}

	// Running.
	fake := NewFakeTransport()
	fake.SetDefault(CommandResult{Stdout: "RUNNING:54321\n"}, nil)
	a := NewSSHAdapter(cfg, SSHAdapterOptions{Transport: fake})
	st, err := a.Status(ctx, job)
	if err != nil || st.State != StateRunning || st.Detail != "pid=54321" {
		t.Fatalf("Status running = (%+v, %v)", st, err)
	}
	if !strings.Contains(fake.Calls()[0].Command, "kill -0") {
		t.Errorf("expected kill -0 in status probe command: %s", fake.Calls()[0].Command)
	}

	// Exited with 0.
	fake.SetDefault(CommandResult{Stdout: "EXITED:0\n"}, nil)
	st, err = a.Status(ctx, job)
	if err != nil || st.State != StateExited || st.ExitCode != 0 {
		t.Fatalf("Status exited(0) = (%+v, %v)", st, err)
	}

	// Exited with non-zero and EXIT: prefix compatibility.
	fake.SetDefault(CommandResult{Stdout: "EXITED:EXIT:13\n"}, nil)
	st, err = a.Status(ctx, job)
	if err != nil || st.State != StateExited || st.ExitCode != 13 {
		t.Fatalf("Status exited(13) = (%+v, %v)", st, err)
	}

	// Missing (pid dead).
	fake.SetDefault(CommandResult{Stdout: "MISSING:pid_dead:54321\n"}, nil)
	st, err = a.Status(ctx, job)
	if err != nil || st.State != StateMissing {
		t.Fatalf("Status missing(dead) = (%+v, %v)", st, err)
	}

	// Missing (no files).
	fake.SetDefault(CommandResult{Stdout: "MISSING:no_files\n"}, nil)
	st, err = a.Status(ctx, job)
	if err != nil || st.State != StateMissing {
		t.Fatalf("Status missing(no_files) = (%+v, %v)", st, err)
	}

	// Transport error must propagate as error (not StateMissing).
	netErr := errors.New("ssh connection refused")
	fake.SetDefault(CommandResult{}, netErr)
	if _, err := a.Status(ctx, job); !errors.Is(err, netErr) {
		t.Fatalf("Status transport err = %v, want %v", err, netErr)
	}
}

func TestSSHAdapterCollectEvidence(t *testing.T) {
	cfg := testFleetConfig()
	ctx := context.Background()
	job := &store.Job{ID: 40, Host: "penguin", Workdir: "/home/worker/agent-work/job-40"}

	// 1. Diff present (with control files filtered out), artifacts checked, log tail and exit code collected.
	t.Run("diff artifacts log and exit code", func(t *testing.T) {
		fake := NewFakeTransport()
		fake.OnContains("status --porcelain", CommandResult{
			Stdout: " M internal/adapter/ssh_adapter.go\n?? prompt.txt\n?? run.sh\n?? agent.log\n?? agent.exit\n?? agent.pid\n?? summary.md\n",
		}, nil)
		fake.OnContains("printf '1:", CommandResult{
			Stdout: "1:agent.log\n1:agent.exit\n1:summary.md\n0:result.txt\n",
		}, nil)
		fake.OnContains("tail -n", CommandResult{
			Stdout: "line 1\nall tests passed\n",
		}, nil)
		fake.OnContains("agent.exit", CommandResult{
			Stdout: "0\n",
		}, nil)

		a := NewSSHAdapter(cfg, SSHAdapterOptions{Transport: fake})
		ev, err := a.CollectEvidence(ctx, job)
		if err != nil {
			t.Fatalf("CollectEvidence: %v", err)
		}
		if !ev.HasDiff {
			t.Errorf("HasDiff = false, want true")
		}
		if !strings.Contains(ev.DiffSummary, "ssh_adapter.go") || !strings.Contains(ev.DiffSummary, "summary.md") {
			t.Errorf("DiffSummary missing real changes: %q", ev.DiffSummary)
		}
		for _, ctrl := range []string{"prompt.txt", "run.sh", "agent.log", "agent.exit", "agent.pid"} {
			if strings.Contains(ev.DiffSummary, ctrl) {
				t.Errorf("DiffSummary should filter out control file %q: %q", ctrl, ev.DiffSummary)
			}
		}
		if !ev.Artifacts["agent.log"] || !ev.Artifacts["summary.md"] || ev.Artifacts["result.txt"] {
			t.Errorf("unexpected Artifacts map: %+v", ev.Artifacts)
		}
		if ev.FinalReport != "line 1\nall tests passed" {
			t.Errorf("FinalReport = %q", ev.FinalReport)
		}
		if ev.ExitCode != 0 {
			t.Errorf("ExitCode = %d, want 0", ev.ExitCode)
		}
	})

	// 2. Only adapter control files untracked -> HasDiff must be false.
	t.Run("only control files yields no diff", func(t *testing.T) {
		fake := NewFakeTransport()
		fake.OnContains("status --porcelain", CommandResult{
			Stdout: "?? prompt.txt\n?? run.sh\n?? agent.pid\n?? agent.exit\n?? agent.log\n",
		}, nil)
		fake.OnContains("printf '1:", CommandResult{Stdout: "1:agent.log\n"}, nil)
		fake.OnContains("tail -n", CommandResult{Stdout: "done\n"}, nil)
		fake.OnContains("agent.exit", CommandResult{Stdout: "2\n"}, nil)

		a := NewSSHAdapter(cfg, SSHAdapterOptions{Transport: fake})
		ev, err := a.CollectEvidence(ctx, job)
		if err != nil {
			t.Fatalf("CollectEvidence: %v", err)
		}
		if ev.HasDiff || ev.DiffSummary != "" {
			t.Errorf("expected no diff when only control files exist, got HasDiff=%v DiffSummary=%q", ev.HasDiff, ev.DiffSummary)
		}
		if ev.ExitCode != 2 {
			t.Errorf("ExitCode = %d, want 2", ev.ExitCode)
		}
	})
}

func TestSSHAdapterUnsupportedOSReturnsError(t *testing.T) {
	cfg := testFleetConfig()
	cfg.Hosts["bsd-box"] = config.HostConfig{
		Transport:   "ssh",
		SSH:         "worker@bsd.example.ts.net",
		OS:          "freebsd",
		WorkdirRoot: "/home/worker/agent-work",
	}
	fake := NewFakeTransport()
	a := NewSSHAdapter(cfg, SSHAdapterOptions{Transport: fake})
	ctx := context.Background()

	bsdJob := &store.Job{
		ID:           50,
		Title:        "Unsupported OS job",
		Agent:        "claude",
		Host:         "bsd-box",
		Workdir:      "/home/worker/agent-work/job-50",
		Instructions: "hello",
	}

	if err := a.PrepareWorkdir(ctx, bsdJob); !errors.Is(err, ErrUnsupportedOS) {
		t.Errorf("PrepareWorkdir(freebsd) = %v, want ErrUnsupportedOS", err)
	}
	if _, err := a.Launch(ctx, bsdJob); !errors.Is(err, ErrUnsupportedOS) {
		t.Errorf("Launch(freebsd) = %v, want ErrUnsupportedOS", err)
	}
	if _, err := a.Status(ctx, bsdJob); !errors.Is(err, ErrUnsupportedOS) {
		t.Errorf("Status(freebsd) = %v, want ErrUnsupportedOS", err)
	}
	if _, err := a.CollectEvidence(ctx, bsdJob); !errors.Is(err, ErrUnsupportedOS) {
		t.Errorf("CollectEvidence(freebsd) = %v, want ErrUnsupportedOS", err)
	}
	if _, err := a.Preflight(ctx, "bsd-box", "claude"); !errors.Is(err, ErrUnsupportedOS) {
		t.Errorf("Preflight(freebsd) = %v, want ErrUnsupportedOS", err)
	}
	if len(fake.Calls()) != 0 {
		t.Errorf("expected 0 transport calls for unsupported OS host, got %d", len(fake.Calls()))
	}
}

func TestSSHAdapterPreflightConfigDriven(t *testing.T) {
	cfg := testFleetConfig()
	fake := NewFakeTransport()
	fake.SetDefault(CommandResult{Stdout: "v1.0.0\n", ExitCode: 0}, nil)
	a := NewSSHAdapter(cfg, SSHAdapterOptions{Transport: fake})

	ctx := context.Background()
	if _, err := a.Preflight(ctx, "ubuntu", "agy"); err != nil {
		t.Fatalf("Preflight agy: %v", err)
	}
	if _, err := a.Preflight(ctx, "penguin", "codex"); err != nil {
		t.Fatalf("Preflight codex: %v", err)
	}
	if _, err := a.Preflight(ctx, "ubuntu", "claude"); err != nil {
		t.Fatalf("Preflight claude: %v", err)
	}

	calls := fake.Calls()
	if len(calls) != 3 {
		t.Fatalf("calls = %d, want 3", len(calls))
	}
	if calls[0].Command != `"$HOME"/'.local/bin/agy' --version` {
		t.Errorf("agy preflight = %q", calls[0].Command)
	}
	if !strings.Contains(calls[1].Command, `export PATH="$HOME/.config/nvm/versions/node/v24.18.1/bin:$PATH"`) ||
		!strings.Contains(calls[1].Command, `codex --version`) {
		t.Errorf("codex preflight = %q", calls[1].Command)
	}
	if calls[2].Command != `claude --version` {
		t.Errorf("claude preflight = %q", calls[2].Command)
	}
}

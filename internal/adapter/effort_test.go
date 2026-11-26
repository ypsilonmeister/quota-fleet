package adapter

import (
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

func TestBuildLaunchCommandEffort(t *testing.T) {
	cases := []struct {
		name    string
		agent   string
		job     store.Job
		wantLin string
		wantWin string
	}{
		{
			name:    "codex with effort",
			agent:   "codex",
			job:     store.Job{ID: 1, Agent: "codex", Effort: "xhigh"},
			wantLin: `codex exec --skip-git-repo-check -c 'model_reasoning_effort=xhigh' < '/w/prompt.txt' > '/w/agent.log' 2>&1`,
			wantWin: `codex exec --skip-git-repo-check -c "model_reasoning_effort=xhigh" - < prompt.txt > agent.log 2> agent.err.log`,
		},
		{
			name:    "codex without effort is unchanged",
			agent:   "codex",
			job:     store.Job{ID: 1, Agent: "codex"},
			wantLin: `codex exec --skip-git-repo-check < '/w/prompt.txt' > '/w/agent.log' 2>&1`,
			wantWin: `codex exec --skip-git-repo-check - < prompt.txt > agent.log 2> agent.err.log`,
		},
		{
			name:    "claude with effort",
			agent:   "claude",
			job:     store.Job{ID: 1, Agent: "claude", Model: "claude-opus-5-5", Effort: "high"},
			wantLin: `claude -p --model 'claude-opus-5-5' --effort high < '/w/prompt.txt' > '/w/agent.log' 2>&1`,
			wantWin: `claude -p --model claude-opus-5-5 --effort high < prompt.txt > agent.log 2> agent.err.log`,
		},
		{
			name:    "claude without effort is unchanged",
			agent:   "claude",
			job:     store.Job{ID: 1, Agent: "claude", Model: "claude-opus-5-5"},
			wantLin: `claude -p --model 'claude-opus-5-5' < '/w/prompt.txt' > '/w/agent.log' 2>&1`,
			wantWin: `claude -p --model claude-opus-5-5 < prompt.txt > agent.log 2> agent.err.log`,
		},
		{
			name:    "claude resume keeps effort, drops model",
			agent:   "claude",
			job:     store.Job{ID: 1, Agent: "claude", Model: "claude-opus-5-5", SessionID: "s1", Effort: "low"},
			wantLin: `claude -p --resume 's1' --effort low < '/w/prompt.txt' > '/w/agent.log' 2>&1`,
			wantWin: `claude -p --resume s1 --effort low < prompt.txt > agent.log 2> agent.err.log`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			drv, err := ResolveDriver(nil, config.HostConfig{}, c.agent)
			if err != nil {
				t.Fatalf("ResolveDriver: %v", err)
			}
			job := c.job
			lin, err := drv.BuildLaunchCommand(&job, "/w", "/w/prompt.txt", "/w/agent.log", false)
			if err != nil {
				t.Fatalf("BuildLaunchCommand: %v", err)
			}
			if lin != c.wantLin {
				t.Errorf("linux = %q\n want %q", lin, c.wantLin)
			}
			win, err := drv.BuildWindowsLaunchCommand(&job, `C:\w`, "prompt.txt", "agent.log", "agent.err.log", false)
			if err != nil {
				t.Fatalf("BuildWindowsLaunchCommand: %v", err)
			}
			if win != c.wantWin {
				t.Errorf("windows = %q\n want %q", win, c.wantWin)
			}
		})
	}
}

func TestBuildLaunchCommandEffortBeforeSandboxFlag(t *testing.T) {
	cfg := &config.Config{Agents: map[string]config.AgentConfig{"codex": {SandboxFlag: "--dangerously-bypass-approvals-and-sandbox"}}}
	drv, err := ResolveDriver(cfg, config.HostConfig{}, "codex")
	if err != nil {
		t.Fatalf("ResolveDriver: %v", err)
	}
	job := store.Job{ID: 1, Agent: "codex", Effort: "medium"}
	got, err := drv.BuildLaunchCommand(&job, "/w", "/w/prompt.txt", "/w/agent.log", true)
	if err != nil {
		t.Fatalf("BuildLaunchCommand: %v", err)
	}
	want := `codex exec --skip-git-repo-check -c 'model_reasoning_effort=medium' --dangerously-bypass-approvals-and-sandbox < '/w/prompt.txt' > '/w/agent.log' 2>&1`
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func TestEffortArgsOtherAgents(t *testing.T) {
	if a := effortArgs("agy", "high"); a != nil {
		t.Errorf("effortArgs(agy) = %v, want nil", a)
	}
	if a := effortArgs("codex", ""); a != nil {
		t.Errorf("effortArgs(codex, empty) = %v, want nil", a)
	}
}

// Review item 2: options after the end-of-options marker `--` are positional arguments
// (codex exec ... -- -c model_reasoning_effort=low fails with "unexpected argument"), so the
// effort goes just before `--` when the template has one.
func TestBuildLaunchCommandEffortBeforeDoubleDash(t *testing.T) {
	cfg := &config.Config{Agents: map[string]config.AgentConfig{
		"codex":  {Launch: "codex exec --skip-git-repo-check --"},
		"claude": {Launch: "claude -p --model {model} -- {prompt_file}"},
	}}
	cases := []struct {
		agent, wantLin, wantWin string
		job                     store.Job
	}{
		{
			agent:   "codex",
			job:     store.Job{ID: 1, Agent: "codex", Effort: "low"},
			wantLin: `codex exec --skip-git-repo-check -c 'model_reasoning_effort=low' -- < '/w/prompt.txt' > '/w/agent.log' 2>&1`,
			wantWin: `codex exec --skip-git-repo-check -c "model_reasoning_effort=low" -- - < C:\w\prompt.txt > agent.log 2> agent.err.log`,
		},
		{
			agent:   "codex",
			job:     store.Job{ID: 1, Agent: "codex"},
			wantLin: `codex exec --skip-git-repo-check -- < '/w/prompt.txt' > '/w/agent.log' 2>&1`,
			wantWin: `codex exec --skip-git-repo-check -- - < C:\w\prompt.txt > agent.log 2> agent.err.log`,
		},
		{
			agent:   "claude",
			job:     store.Job{ID: 1, Agent: "claude", Model: "m1", Effort: "xhigh"},
			wantLin: `claude -p --model 'm1' --effort xhigh -- '/w/prompt.txt' </dev/null > '/w/agent.log' 2>&1`,
			wantWin: `claude -p --model m1 --effort xhigh -- C:\w\prompt.txt < NUL > agent.log 2> agent.err.log`,
		},
	}
	for _, c := range cases {
		drv, err := ResolveDriver(cfg, config.HostConfig{}, c.agent)
		if err != nil {
			t.Fatalf("ResolveDriver: %v", err)
		}
		job := c.job
		lin, err := drv.BuildLaunchCommand(&job, "/w", "/w/prompt.txt", "/w/agent.log", false)
		if err != nil || lin != c.wantLin {
			t.Errorf("%s linux = %q (err=%v)\n want %q", c.agent, lin, err, c.wantLin)
		}
		win, err := drv.BuildWindowsLaunchCommand(&job, `C:\w`, `C:\w\prompt.txt`, "agent.log", "agent.err.log", false)
		if err != nil || win != c.wantWin {
			t.Errorf("%s windows = %q (err=%v)\n want %q", c.agent, win, err, c.wantWin)
		}
	}
}

// Review item 1: the effort can only be passed when the launch command runs the agent's own CLI.
func TestCheckEffort(t *testing.T) {
	cfg := &config.Config{Agents: map[string]config.AgentConfig{
		"codex": {Launch: "codex-wrapper exec"},
	}}
	cases := []struct {
		name    string
		agent   string
		cfg     *config.Config
		host    config.HostConfig
		job     store.Job
		wantErr bool
	}{
		{"claude default", "claude", nil, config.HostConfig{}, store.Job{Agent: "claude", Effort: "high"}, false},
		{"claude resume default", "claude", nil, config.HostConfig{}, store.Job{Agent: "claude", Effort: "high", SessionID: "s"}, false},
		{"claude absolute path override", "claude", nil, hostWithCommand("claude", "/home/u/.local/bin/claude"), store.Job{Agent: "claude", Effort: "low"}, false},
		{"claude windows override", "claude", nil, hostWithCommand("claude", `C:\Users\u\AppData\Roaming\npm\claude.cmd`), store.Job{Agent: "claude", Effort: "low"}, false},
		{"claude wrapper override", "claude", nil, hostWithCommand("claude", "/opt/bin/run-agent.sh"), store.Job{Agent: "claude", Effort: "low"}, true},
		{"codex wrapper template", "codex", cfg, config.HostConfig{}, store.Job{Agent: "codex", Effort: "low"}, true},
		{"wrapper without effort", "codex", cfg, config.HostConfig{}, store.Job{Agent: "codex"}, false},
		{"agy", "agy", nil, config.HostConfig{}, store.Job{Agent: "agy", Effort: "low"}, true},
	}
	for _, c := range cases {
		drv, err := ResolveDriver(c.cfg, c.host, c.agent)
		if err != nil {
			t.Fatalf("%s: ResolveDriver: %v", c.name, err)
		}
		job := c.job
		if err := drv.CheckEffort(&job); (err != nil) != c.wantErr {
			t.Errorf("%s: CheckEffort err = %v, wantErr %v", c.name, err, c.wantErr)
		}
	}
}

func hostWithCommand(agent, command string) config.HostConfig {
	return config.HostConfig{Agents: map[string]config.HostAgentOverride{agent: {Command: command}}}
}

// Review item 8: run.sh hands template tokens to the shell as written, so a quoted terminator
// such as `'--'` is still the argument `--` for the CLI. The effort must go before it (codex-cli
// rejected `codex exec --skip-git-repo-check '--' -c model_reasoning_effort=low` with
// "unexpected argument"). The shell's own argv is checked so the test follows real quote removal.
func TestBuildLaunchCommandEffortBeforeQuotedDoubleDash(t *testing.T) {
	cases := []struct {
		launch, want string
	}{
		{`codex exec --skip-git-repo-check '--'`, `codex exec --skip-git-repo-check -c 'model_reasoning_effort=low' '--' < '/w/prompt.txt' > '/w/agent.log' 2>&1`},
		{`codex exec --skip-git-repo-check "--"`, `codex exec --skip-git-repo-check -c 'model_reasoning_effort=low' "--" < '/w/prompt.txt' > '/w/agent.log' 2>&1`},
		{`codex exec --skip-git-repo-check \-\-`, `codex exec --skip-git-repo-check -c 'model_reasoning_effort=low' \-\- < '/w/prompt.txt' > '/w/agent.log' 2>&1`},
		{`codex exec --skip-git-repo-check '-'-`, `codex exec --skip-git-repo-check -c 'model_reasoning_effort=low' '-'- < '/w/prompt.txt' > '/w/agent.log' 2>&1`},
		// Not a terminator after quote removal: the effort is appended as before.
		{`codex exec --skip-git-repo-check '--x'`, `codex exec --skip-git-repo-check '--x' -c 'model_reasoning_effort=low' < '/w/prompt.txt' > '/w/agent.log' 2>&1`},
		{`codex exec --skip-git-repo-check '-- '`, `codex exec --skip-git-repo-check '-- ' -c 'model_reasoning_effort=low' < '/w/prompt.txt' > '/w/agent.log' 2>&1`},
	}
	sh, shErr := exec.LookPath("sh")
	for _, c := range cases {
		cfg := &config.Config{Agents: map[string]config.AgentConfig{"codex": {Launch: c.launch}}}
		drv, err := ResolveDriver(cfg, config.HostConfig{}, "codex")
		if err != nil {
			t.Fatalf("ResolveDriver: %v", err)
		}
		job := store.Job{ID: 1, Agent: "codex", Effort: "low"}
		lin, err := drv.BuildLaunchCommand(&job, "/w", "/w/prompt.txt", "/w/agent.log", false)
		if err != nil || lin != c.want {
			t.Errorf("%s: linux = %q (err=%v)\n want %q", c.launch, lin, err, c.want)
			continue
		}
		if shErr != nil {
			continue
		}
		// Replace the executable with printf and drop the redirections to see the argv the
		// shell builds: every option must come before the first `--` argument.
		argsPart := strings.TrimPrefix(strings.SplitN(lin, " < ", 2)[0], "codex ")
		out, err := exec.Command(sh, "-c", `printf '%s\n' `+argsPart).Output()
		if err != nil {
			t.Fatalf("%s: sh: %v", c.launch, err)
		}
		argv := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
		effortAt, endAt := -1, -1
		for i, a := range argv {
			if a == "model_reasoning_effort=low" && effortAt < 0 {
				effortAt = i
			}
			if a == "--" && endAt < 0 {
				endAt = i
			}
		}
		if effortAt < 0 || (endAt >= 0 && effortAt > endAt) {
			t.Errorf("%s: shell argv %q puts the effort after `--`", c.launch, argv)
		}
	}
}

func TestShellWordValue(t *testing.T) {
	cases := []struct {
		word, want string
		ok         bool
	}{
		{`--`, "--", true},
		{`'--'`, "--", true},
		{`"--"`, "--", true},
		{`\-\-`, "--", true},
		{`"-"'-'`, "--", true},
		{`"a\"b"`, `a"b`, true},
		{`"a\b"`, `a\b`, true},
		{`'--`, "", false},
		{`"$X"`, "", false},
		{`$X`, "", false},
		{"`x`", "", false},
		{`-*`, "", false},
	}
	for _, c := range cases {
		got, ok := shellWordValue(c.word)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("shellWordValue(%s) = %q, %v; want %q, %v", c.word, got, ok, c.want, c.ok)
		}
	}
}

// Review round 2, item 1: the host's CLI is asked for its help; a same-named CLI whose help lacks
// the effort option is unsupported (ErrEffortUnsupported), while a failure to run the probe is not
// a verdict.
func TestEffortProbe(t *testing.T) {
	drv, err := ResolveDriver(nil, hostWithCommand("claude", "/opt/legacy/bin/claude"), "claude")
	if err != nil {
		t.Fatal(err)
	}
	if p, err := drv.BuildEffortProbe(&store.Job{Agent: "claude"}, false); p != nil || err != nil {
		t.Fatalf("no effort: probe=%v err=%v, want nil, nil", p, err)
	}
	p, err := drv.BuildEffortProbe(&store.Job{Agent: "claude", Effort: "high"}, false)
	if err != nil || p == nil {
		t.Fatalf("BuildEffortProbe: %v", err)
	}
	if p.Executable != "/opt/legacy/bin/claude" || !strings.Contains(p.Command, "/opt/legacy/bin/claude") || !strings.Contains(p.Command, "--help") {
		t.Errorf("probe = %+v", p)
	}
	win, err := drv.BuildEffortProbe(&store.Job{Agent: "claude", Effort: "high"}, true)
	if err != nil || !strings.Contains(win.Command, "'--help'") || !strings.Contains(win.Command, "Get-Command") {
		t.Errorf("windows probe = %+v (err=%v)", win, err)
	}
	wrapped, _ := ResolveDriver(nil, hostWithCommand("claude", "/opt/bin/run-agent.sh"), "claude")
	if _, err := wrapped.BuildEffortProbe(&store.Job{Agent: "claude", Effort: "high"}, false); err == nil {
		t.Error("wrapper: BuildEffortProbe err = nil, want not the claude CLI")
	}

	cases := []struct {
		name        string
		res         ExecResult
		runErr      error
		wantNil     bool
		unsupported bool
	}{
		{"lists --effort", ExecResult{Output: "  --effort <level>  Effort level"}, nil, true, false},
		{"legacy help", ExecResult{Output: "  -p, --print\n  --model <model>"}, nil, false, true},
		{"similar option only", ExecResult{Output: "  --effort-budget <n>"}, nil, false, true},
		{"not found", ExecResult{ExitCode: 127, Output: p.notFoundMarker + " /opt/legacy/bin/claude"}, nil, false, true},
		{"unknown flag error", ExecResult{ExitCode: 1, Output: "error: unknown option '--help'"}, nil, false, true},
		{"silent failure", ExecResult{ExitCode: 1}, nil, false, false},
		{"transport error", ExecResult{}, errors.New("ssh: connection refused"), false, false},
	}
	for _, c := range cases {
		err := p.Classify(c.res, c.runErr)
		if (err == nil) != c.wantNil || (err != nil && errors.Is(err, ErrEffortUnsupported) != c.unsupported) {
			t.Errorf("%s: Classify = %v, want nil=%v unsupported=%v", c.name, err, c.wantNil, c.unsupported)
		}
	}

	codex := &EffortProbe{agent: "codex", Executable: "codex"}
	if err := codex.Classify(ExecResult{Output: "  -c, --config <key=value>\n          Override"}, nil); err != nil {
		t.Errorf("codex help with --config: %v", err)
	}
	if err := codex.Classify(ExecResult{Output: "  -m, --model <MODEL>"}, nil); !errors.Is(err, ErrEffortUnsupported) {
		t.Errorf("codex help without --config: %v", err)
	}
}

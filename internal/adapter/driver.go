package adapter

import (
	"crypto/rand"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

// PromptStyle controls how prompt.txt is delivered to an agent CLI in run.sh.
type PromptStyle string

const (
	// PromptStyleStdin redirects prompt.txt to the agent's standard input (`< prompt.txt`).
	PromptStyleStdin PromptStyle = config.PromptStyleStdin
	// PromptStyleArgument reads prompt.txt at runtime and passes its contents as an argument (`"$(cat prompt.txt)"`).
	PromptStyleArgument PromptStyle = config.PromptStyleArgument
)

// AgentDriver holds resolved, config-driven execution metadata for a single agent on a host.
type AgentDriver struct {
	Name                string
	Preflight           string
	Launch              string
	Resume              string
	SessionLogGlob      string
	SandboxFlag         string
	PromptStyle         PromptStyle
	CommandOverride     string
	Env                 map[string]string
	ForceStdinNoDevNull bool
	WindowsStdinArg     string
}

// defaultDriverSpec provides minimal fallback metadata for the 3 built-in agent families
// (claude, codex, agy). Config values in fleet.yaml always override these fields.
// SandboxFlag is intentionally never defaulted here; it must be configured explicitly in fleet.yaml.
type defaultDriverSpec struct {
	preflight           string
	launch              string
	resume              string
	sessionLogGlob      string
	promptStyle         PromptStyle
	forceStdinNoDevNull bool
	windowsStdinArg     string
}

var builtinDriverDefaults = map[string]defaultDriverSpec{
	"claude": {
		preflight:      "claude --version",
		launch:         "claude -p --model {model}",
		resume:         "claude -p --resume {session_id}",
		sessionLogGlob: "~/.claude/projects/*/*.jsonl",
		promptStyle:    PromptStyleStdin,
	},
	"codex": {
		preflight:           "codex --version",
		launch:              "codex exec --skip-git-repo-check",
		sessionLogGlob:      "~/.codex/sessions/*/*.jsonl",
		promptStyle:         PromptStyleStdin,
		forceStdinNoDevNull: true,
		windowsStdinArg:     "-",
	},
	"agy": {
		preflight:   "agy --version",
		launch:      "agy --model {model} --print",
		promptStyle: PromptStyleArgument,
	},
}

// ResolveDriver merges minimal built-in defaults (for claude/codex/agy), global config.Agents,
// and per-host host.Agents overrides into an AgentDriver.
func ResolveDriver(cfg *config.Config, host config.HostConfig, agentName string) (AgentDriver, error) {
	name := strings.TrimSpace(agentName)
	if name == "" {
		return AgentDriver{}, errors.New("adapter: job agent must not be empty")
	}

	drv := AgentDriver{
		Name:        name,
		PromptStyle: PromptStyleStdin,
	}

	if def, ok := builtinDriverDefaults[name]; ok {
		drv.Preflight = def.preflight
		drv.Launch = def.launch
		drv.Resume = def.resume
		drv.SessionLogGlob = def.sessionLogGlob
		if def.promptStyle != "" {
			drv.PromptStyle = def.promptStyle
		}
		drv.ForceStdinNoDevNull = def.forceStdinNoDevNull
		drv.WindowsStdinArg = def.windowsStdinArg
	}

	if cfg != nil {
		if ac, ok := cfg.Agents[name]; ok {
			if strings.TrimSpace(ac.Preflight) != "" {
				drv.Preflight = strings.TrimSpace(ac.Preflight)
			}
			if strings.TrimSpace(ac.Launch) != "" {
				drv.Launch = strings.TrimSpace(ac.Launch)
			}
			if strings.TrimSpace(ac.Resume) != "" {
				drv.Resume = strings.TrimSpace(ac.Resume)
			}
			if strings.TrimSpace(ac.SessionLogGlob) != "" {
				drv.SessionLogGlob = strings.TrimSpace(ac.SessionLogGlob)
			}
			if strings.TrimSpace(ac.SandboxFlag) != "" {
				drv.SandboxFlag = strings.TrimSpace(ac.SandboxFlag)
			}
			if ps := strings.TrimSpace(ac.PromptStyle); ps != "" {
				switch PromptStyle(ps) {
				case PromptStyleStdin:
					drv.PromptStyle = PromptStyleStdin
				case PromptStyleArgument:
					drv.PromptStyle = PromptStyleArgument
					drv.ForceStdinNoDevNull = false
				default:
					return AgentDriver{}, fmt.Errorf("adapter: agent %q has unsupported prompt_style %q", name, ps)
				}
			}
		}
	}

	if host.Agents != nil {
		if ov, ok := host.Agents[name]; ok {
			if strings.TrimSpace(ov.Command) != "" {
				drv.CommandOverride = strings.TrimSpace(ov.Command)
			}
			if len(ov.Env) > 0 {
				drv.Env = make(map[string]string, len(ov.Env))
				for k, v := range ov.Env {
					drv.Env[k] = v
				}
			}
		}
	}

	if strings.TrimSpace(drv.Launch) == "" {
		return AgentDriver{}, fmt.Errorf("adapter: agent %q has no launch command configured", name)
	}
	return drv, nil
}

// BuildPreflightCommand renders the preflight check command with per-host command and env overrides.
func (d AgentDriver) BuildPreflightCommand() (string, error) {
	tmpl := strings.TrimSpace(d.Preflight)
	if tmpl == "" {
		return "", fmt.Errorf("adapter: agent %q has no preflight command configured", d.Name)
	}
	cmd := applyCommandOverride(tmpl, d.CommandOverride)
	if len(d.Env) == 0 {
		return cmd, nil
	}
	var lines []string
	lines = append(lines, formatEnvExports(d.Env)...)
	lines = append(lines, cmd)
	return strings.Join(lines, "; "), nil
}

// BuildWindowsPreflightCommand renders the preflight check command for a Windows host
// wrapped in a non-interactive PowerShell invocation.
func (d AgentDriver) BuildWindowsPreflightCommand() (string, error) {
	tmpl := strings.TrimSpace(d.Preflight)
	if tmpl == "" {
		return "", fmt.Errorf("adapter: agent %q has no preflight command configured", d.Name)
	}
	tokens := strings.Fields(tmpl)
	if len(tokens) == 0 {
		return "", fmt.Errorf("adapter: agent %q has no preflight command configured", d.Name)
	}
	exe := tokens[0]
	if strings.TrimSpace(d.CommandOverride) != "" {
		exe = strings.TrimSpace(d.CommandOverride)
	}
	var stmts []string
	stmts = append(stmts, formatPowerShellEnvAssigns(d.Env)...)
	psTokens := make([]string, 0, len(tokens))
	psTokens = append(psTokens, psExpr(exe))
	for _, arg := range tokens[1:] {
		psTokens = append(psTokens, psExpr(arg))
	}
	stmts = append(stmts, "& "+strings.Join(psTokens, " ")+"; exit $LASTEXITCODE")
	return wrapPowerShellCommand(strings.Join(stmts, "; ")), nil
}

// ResolveBaseCommand returns the executable name/path for this agent driver,
// honoring per-host CommandOverride first, then Launch, Preflight, or Name.
func (d AgentDriver) ResolveBaseCommand() string {
	if cmd := strings.TrimSpace(d.CommandOverride); cmd != "" {
		return cmd
	}
	if fields := strings.Fields(strings.TrimSpace(d.Launch)); len(fields) > 0 {
		return fields[0]
	}
	if fields := strings.Fields(strings.TrimSpace(d.Preflight)); len(fields) > 0 {
		return fields[0]
	}
	return strings.TrimSpace(d.Name)
}

// VersionProbeNotFoundMarker is the static prefix for per-probe not-found markers.
const VersionProbeNotFoundMarker = "__QUOTA_FLEET_CMD_NOT_FOUND__:"

// NewVersionProbeNotFoundMarker generates a unique per-probe marker embedding a
// random 128-bit hex nonce so target command output cannot spoof pre-check not_found.
func NewVersionProbeNotFoundMarker() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("adapter: generate version probe nonce: %w", err)
	}
	return fmt.Sprintf("__QUOTA_FLEET_CMD_NOT_FOUND_%x__:", b), nil
}

// BuildVersionCommand builds the shell command passed to FileAccess.RunCommand
// to query the agent's version (`<command> <version_args...>`), defaulting to `--version`.
// Before executing the target command, it checks whether the target executable can be resolved
// and emits a per-probe nonce-bearing marker with exit code 127 only when the executable itself is absent.
// It returns the rendered command string and the exact marker prefix expected for this probe.
// On Windows, it returns PowerShell statements suitable for windowsFileAccess.RunCommand.
func (d AgentDriver) BuildVersionCommand(versionArgs []string, isWindows bool) (string, string, error) {
	exe := d.ResolveBaseCommand()
	if exe == "" {
		return "", "", fmt.Errorf("adapter: agent %q has no executable command configured", d.Name)
	}
	notFoundMarker, err := NewVersionProbeNotFoundMarker()
	if err != nil {
		return "", "", err
	}
	args := versionArgs
	if len(args) == 0 {
		args = []string{"--version"}
	}

	if isWindows {
		var stmts []string
		stmts = append(stmts, formatPowerShellEnvAssigns(d.Env)...)
		exeExpr := psExpr(exe)
		if strings.ContainsAny(exe, `/\`) {
			stmts = append(stmts, fmt.Sprintf(
				"if (-not (Get-Command %s -ErrorAction SilentlyContinue) -and -not (Test-Path -LiteralPath %s)) { [Console]::Error.WriteLine(%s + ' ' + %s); exit 127 }",
				exeExpr, exeExpr, psExpr(notFoundMarker), exeExpr,
			))
		} else {
			stmts = append(stmts, fmt.Sprintf(
				"if (-not (Get-Command %s -ErrorAction SilentlyContinue) -and -not (Test-Path -LiteralPath %s) -and -not (($env:PATH -split ';') | Where-Object { $_ -and (Test-Path -LiteralPath (Join-Path $_ %s) -PathType Leaf) } | Select-Object -First 1)) { [Console]::Error.WriteLine(%s + ' ' + %s); exit 127 }",
				exeExpr, exeExpr, exeExpr, psExpr(notFoundMarker), exeExpr,
			))
		}
		psTokens := make([]string, 0, 1+len(args))
		psTokens = append(psTokens, exeExpr)
		for _, a := range args {
			psTokens = append(psTokens, psExpr(a))
		}
		stmts = append(stmts, "& "+strings.Join(psTokens, " "))
		return strings.Join(stmts, "; "), notFoundMarker, nil
	}

	exeToken := formatRemoteCommandToken(exe)
	var preCheck string
	if strings.Contains(exe, "/") {
		preCheck = fmt.Sprintf(
			"if [ ! -e %s ]; then printf '%%s %%s\\n' %s %s >&2; exit 127; fi",
			exeToken, shellQuote(notFoundMarker), shellQuote(exe),
		)
	} else {
		exeQuoted := shellQuote(exe)
		preCheck = fmt.Sprintf(
			"if ! command -v %s >/dev/null 2>&1; then "+
				"__qf_hit=''; "+
				"__qf_path=\"${PATH}:\"; "+
				"while [ -n \"$__qf_path\" ]; do "+
				"__qf_dir=\"${__qf_path%%%%:*}\"; "+
				"__qf_path=\"${__qf_path#*:}\"; "+
				"[ -n \"$__qf_dir\" ] || __qf_dir=.; "+
				"if [ -e \"$__qf_dir/\"%s ] && [ ! -d \"$__qf_dir/\"%s ]; then "+
				"__qf_hit=\"$__qf_dir/\"%s; break; "+
				"fi; "+
				"done; "+
				"if [ -n \"$__qf_hit\" ]; then "+
				"printf '%%s: permission denied (not executable)\\n' \"$__qf_hit\" >&2; exit 126; "+
				"fi; "+
				"printf '%%s %%s\\n' %s %s >&2; exit 127; "+
				"fi",
			exeToken, exeQuoted, exeQuoted, exeQuoted,
			shellQuote(notFoundMarker), exeQuoted,
		)
	}

	parts := make([]string, 0, 1+len(args))
	parts = append(parts, exeToken)
	for _, a := range args {
		if isSafeBareToken(a) {
			parts = append(parts, a)
		} else {
			parts = append(parts, shellQuote(a))
		}
	}
	cmd := preCheck + "; " + strings.Join(parts, " ")
	if len(d.Env) == 0 {
		return cmd, notFoundMarker, nil
	}
	lines := append(formatEnvExports(d.Env), cmd)
	return strings.Join(lines, "; "), notFoundMarker, nil
}

// effortArgs returns the CLI arguments that carry a job's effort (docs/21), or nil when the
// effort is empty. codex takes it as a config override (`-c model_reasoning_effort=<v>`) and
// claude as `--effort <v>`. Registration rejects effort for every other agent
// (store.ValidateEffort), so nil for them never silently drops a stored effort that passed validation.
func effortArgs(agentName, effort string) []string {
	effort = strings.TrimSpace(effort)
	if effort == "" {
		return nil
	}
	switch strings.TrimSpace(agentName) {
	case "codex":
		return []string{"-c", "model_reasoning_effort=" + effort}
	case "claude":
		return []string{"--effort", effort}
	}
	return nil
}

// insertBeforeOptionEnd inserts extra options into a command line (tokens[0] is the
// executable). When the command has an end-of-options marker `--` (endIndex returns the index of
// the token it starts at, or -1), the options go just before the first one, because a CLI treats
// everything after `--` as positional arguments (docs/21 §3: `codex exec ... -- -c
// model_reasoning_effort=low` fails with "unexpected argument"). Otherwise they are appended at
// the end.
func insertBeforeOptionEnd(tokens, extra []string, endIndex func([]string) int) []string {
	if len(extra) == 0 {
		return tokens
	}
	if i := endIndex(tokens); i >= 1 {
		out := make([]string, 0, len(tokens)+len(extra))
		out = append(out, tokens[:i]...)
		out = append(out, extra...)
		return append(out, tokens[i:]...)
	}
	return append(tokens, extra...)
}

// rawOptionEndIndex returns the index of the first argument (after the executable) whose raw
// value is `--`, or -1. The Windows builder quotes every template token as a literal value, so
// the value itself is what the CLI receives.
func rawOptionEndIndex(tokens []string) int {
	for i := 1; i < len(tokens); i++ {
		if tokens[i] == "--" {
			return i
		}
	}
	return -1
}

// shellOptionEndIndex returns the index of the token that starts the first shell word (after
// the executable) whose value is `--`, or -1 (docs/21 §3). The tokens come from strings.Fields
// and are joined with single spaces into a POSIX shell command line (run.sh), so a quoted string
// or an escaped space can span several tokens: in `--add-dir '/tmp/a -- b' --` the middle `--`
// token is inside the quoted argument `/tmp/a -- b`, and only the last one is the terminator.
// The quoting state is tracked over the whole command line, tokens are grouped into the words
// the shell will see, and each word is compared after quote removal (isShellOptionEnd).
func shellOptionEndIndex(tokens []string) int {
	for i := 0; i < len(tokens); {
		word, j := tokens[i], i
		for shellWordContinues(word) && j+1 < len(tokens) {
			j++
			word += " " + tokens[j]
		}
		if i >= 1 && isShellOptionEnd(word) {
			return i
		}
		i = j + 1
	}
	return -1
}

// shellWordContinues reports whether a shell word that ends where text ends would go on past a
// following space: an open single or double quote, or a trailing backslash that escapes it.
func shellWordContinues(text string) bool {
	inSingle, inDouble := false, false
	for i := 0; i < len(text); i++ {
		c := text[i]
		switch {
		case inSingle:
			if c == '\'' {
				inSingle = false
			}
		case c == '\\':
			if i+1 >= len(text) {
				return true
			}
			i++
		case inDouble:
			if c == '"' {
				inDouble = false
			}
		case c == '\'':
			inSingle = true
		case c == '"':
			inDouble = true
		}
	}
	return inSingle || inDouble
}

// isShellOptionEnd reports whether a word of a POSIX shell command line (run.sh) becomes the
// argument `--` after the shell removes quoting, e.g. `--`, `'--'`, `"--"`, `\-\-` or `'-'-`
// (docs/21 §3), so the comparison follows the shell's quote removal, not the raw text.
func isShellOptionEnd(word string) bool {
	v, ok := shellWordValue(word)
	return ok && v == "--"
}

// shellWordValue returns the value a POSIX shell gives a single word after quote removal. It
// reports false when the word contains anything whose value is decided only at run time
// (unquoted or double-quoted `$` / backquote, unquoted glob characters) or has unbalanced quotes,
// so such a word is never taken for `--`.
func shellWordValue(word string) (string, bool) {
	var b strings.Builder
	for i := 0; i < len(word); i++ {
		c := word[i]
		switch c {
		case '\\':
			if i+1 >= len(word) {
				return "", false
			}
			i++
			b.WriteByte(word[i])
		case '\'':
			j := strings.IndexByte(word[i+1:], '\'')
			if j < 0 {
				return "", false
			}
			b.WriteString(word[i+1 : i+1+j])
			i += 1 + j
		case '"':
			closed := false
			for i++; i < len(word); i++ {
				d := word[i]
				if d == '"' {
					closed = true
					break
				}
				if d == '$' || d == '`' {
					return "", false
				}
				if d == '\\' && i+1 < len(word) && strings.IndexByte("$`\"\\\n", word[i+1]) >= 0 {
					i++
					d = word[i]
				}
				b.WriteByte(d)
			}
			if !closed {
				return "", false
			}
		case '$', '`', '*', '?', '[', '~', '{':
			return "", false
		default:
			b.WriteByte(c)
		}
	}
	return b.String(), true
}

// CheckEffort reports why this driver cannot pass job.Effort to the agent CLI on launch, or nil
// when it can as far as the configuration shows (or the job has no effort). The API calls it at
// registration with the driver resolved for the job's host (docs/21 §3), before probing the
// host's CLI itself (BuildEffortProbe).
//
// The effort arguments are CLI-specific (`--effort` for claude, `-c model_reasoning_effort=` for
// codex), so the executable that the selected launch/resume template runs (after the per-host
// `command` override) must be that agent's own CLI: its base name, without directory or a
// Windows extension (.exe/.cmd/.bat/.ps1), must equal the agent name. A wrapper script or a
// different binary may not accept the arguments, so it is rejected. A same-named CLI that is too
// old for the arguments passes here and is caught by the probe (BuildEffortProbe). A relative
// path (./claude) is rejected as well: it resolves from the job's workdir at launch, so a probe
// run elsewhere could judge another file.
func (d AgentDriver) CheckEffort(job *store.Job) error {
	_, err := d.effortExecutable(job)
	return err
}

// effortExecutable returns the executable that would receive job.Effort ("" when the job has no
// effort), or why the effort cannot be passed to it.
func (d AgentDriver) effortExecutable(job *store.Job) (string, error) {
	if job == nil || strings.TrimSpace(job.Effort) == "" {
		return "", nil
	}
	if effortArgs(d.Name, job.Effort) == nil {
		return "", fmt.Errorf("adapter: effort is not supported for agent %q", d.Name)
	}
	tmpl, _ := d.selectLaunchTemplate(job)
	exe := strings.TrimSpace(d.CommandOverride)
	if exe == "" {
		fields := strings.Fields(tmpl)
		if len(fields) == 0 {
			return "", fmt.Errorf("adapter: agent %q has empty launch template", d.Name)
		}
		exe = fields[0]
	}
	if !strings.EqualFold(commandBaseName(exe), strings.TrimSpace(d.Name)) {
		return "", fmt.Errorf("adapter: agent %q is launched via %q, which is not the %s CLI, so the effort cannot be passed", d.Name, exe, d.Name)
	}
	if isRelativeCommandPath(exe) {
		return "", fmt.Errorf("adapter: agent %q is launched via the relative path %q, which is resolved from the job's workdir at launch, so its effort support cannot be checked at registration; use an absolute path or a bare command name", d.Name, exe)
	}
	return exe, nil
}

// isRelativeCommandPath reports whether exe names a file by a path relative to the current
// directory (./claude, bin/claude, .\claude.exe, \tools\claude.exe). The launcher runs it after
// entering the job's workdir, so the file it reaches depends on that directory and the
// registration probe, which runs elsewhere, cannot tell what will be launched (docs/21 §3). A bare
// name (looked up in PATH), an absolute path (/..., C:\..., \\server\share\...) and a home path
// (~/...) are not relative.
func isRelativeCommandPath(exe string) bool {
	exe = strings.Trim(strings.TrimSpace(exe), `"'`)
	if !strings.ContainsAny(exe, `/\`) {
		return false
	}
	switch {
	case strings.HasPrefix(exe, "/"), strings.HasPrefix(exe, `\\`), strings.HasPrefix(exe, "//"):
		return false
	case exe == "~" || strings.HasPrefix(exe, "~/") || strings.HasPrefix(exe, `~\`):
		return false
	case len(exe) >= 3 && exe[1] == ':' && (exe[2] == '\\' || exe[2] == '/') &&
		((exe[0] >= 'a' && exe[0] <= 'z') || (exe[0] >= 'A' && exe[0] <= 'Z')):
		return false
	}
	return true
}

// ErrEffortUnsupported marks a probe result that settles the question: the host's CLI has no way
// to take the effort (its help lacks the option, or the executable is not there). The API answers
// 400 for it; any other Classify error means the host could not be asked.
var ErrEffortUnsupported = errors.New("effort is not supported by the CLI on the host")

// effortHelpOption returns the pattern that the agent CLI's `--help` output contains when the CLI
// takes the effort arguments effortArgs produces: claude lists `--effort <level>` and codex lists
// `-c, --config <key=value>` (docs/21 §3.1).
func effortHelpOption(agentName string) (*regexp.Regexp, string) {
	switch strings.TrimSpace(agentName) {
	case "claude":
		return regexp.MustCompile(`(?m)(^|[\s,])--effort([\s=<\[]|$)`), "--effort"
	case "codex":
		return regexp.MustCompile(`(?m)(^|[\s,])--config([\s=<\[]|$)`), "-c/--config"
	}
	return nil, ""
}

// EffortProbe is the host command that asks the CLI receiving job.Effort for its help. Build it
// with BuildEffortProbe, run it on the job's host, and pass the result to Classify.
type EffortProbe struct {
	// Command is the shell (Linux) or PowerShell (Windows) command for FileAccess.RunCommand.
	Command string
	// Executable is the CLI that the launch command would run.
	Executable string

	agent          string
	notFoundMarker string
}

// BuildEffortProbe returns the probe for the CLI that would receive job.Effort on this driver's
// host: `<executable> --help` with the host's env and the same not-found pre-check as the
// version probe. It returns nil when the job has no effort, and CheckEffort's error when the
// configuration already rules the effort out.
func (d AgentDriver) BuildEffortProbe(job *store.Job, isWindows bool) (*EffortProbe, error) {
	exe, err := d.effortExecutable(job)
	if err != nil || exe == "" {
		return nil, err
	}
	probeDrv := AgentDriver{Name: d.Name, CommandOverride: exe, Env: d.Env}
	cmd, marker, err := probeDrv.BuildVersionCommand([]string{"--help"}, isWindows)
	if err != nil {
		return nil, err
	}
	return &EffortProbe{Command: cmd, Executable: exe, agent: d.Name, notFoundMarker: marker}, nil
}

// Classify interprets the probe's result. It returns nil when the help lists the effort option,
// an error wrapping ErrEffortUnsupported when the CLI answered without it (an older CLI, even one
// with the agent's name) or is absent, and another error when the command could not be run.
func (p *EffortProbe) Classify(res ExecResult, runErr error) error {
	if runErr != nil {
		return fmt.Errorf("adapter: could not run %q --help to check effort support: %w", p.Executable, runErr)
	}
	out := res.Output
	if out == "" {
		out = res.Stdout + "\n" + res.Stderr
	}
	if res.ExitCode == 127 && strings.Contains(out, p.notFoundMarker) {
		return fmt.Errorf("adapter: %w: %q was not found", ErrEffortUnsupported, p.Executable)
	}
	re, opt := effortHelpOption(p.agent)
	if re == nil {
		return fmt.Errorf("adapter: %w: agent %q", ErrEffortUnsupported, p.agent)
	}
	if re.MatchString(out) {
		return nil
	}
	if res.ExitCode != 0 && strings.TrimSpace(out) == "" {
		return fmt.Errorf("adapter: %q --help exited with code %d and no output", p.Executable, res.ExitCode)
	}
	return fmt.Errorf("adapter: %w: %q --help does not list %s", ErrEffortUnsupported, p.Executable, opt)
}

// commandBaseName returns the executable name without directory and Windows extension.
func commandBaseName(exe string) string {
	exe = strings.Trim(strings.TrimSpace(exe), `"'`)
	if i := strings.LastIndexAny(exe, `/\`); i >= 0 {
		exe = exe[i+1:]
	}
	lower := strings.ToLower(exe)
	for _, ext := range []string{".exe", ".cmd", ".bat", ".ps1"} {
		if strings.HasSuffix(lower, ext) {
			return exe[:len(exe)-len(ext)]
		}
	}
	return exe
}

// selectLaunchTemplate chooses the resume template when job.SessionID is set and the driver
// supports resume, otherwise falling back to the standard launch template.
func (d AgentDriver) selectLaunchTemplate(job *store.Job) (string, bool) {
	if job != nil && strings.TrimSpace(job.SessionID) != "" && strings.TrimSpace(d.Resume) != "" {
		return strings.TrimSpace(d.Resume), true
	}
	return strings.TrimSpace(d.Launch), false
}

// BuildLaunchCommand constructs the agent invocation line placed inside run.sh.
// It applies per-host command overrides, template placeholders ({model}, {workdir}, {prompt_file}, {session_id}),
// conditional sandbox_flag attachment, and stdin/stdout redirections.
// On resume (when job.SessionID is set and Resume is configured), --model/-m flags are omitted.
//
// Codex gotcha (design §5): Codex reads its prompt from stdin via `< prompt.txt` and MUST NOT
// have `</dev/null` appended (which would override stdin and feed an empty prompt).
func (d AgentDriver) BuildLaunchCommand(job *store.Job, workdir, promptFile, logFile string, allowSandbox bool) (string, error) {
	if job == nil {
		return "", ErrNilJob
	}
	tmpl, isResume := d.selectLaunchTemplate(job)
	if tmpl == "" {
		return "", fmt.Errorf("adapter: agent %q has empty launch template", d.Name)
	}

	tokens := strings.Fields(tmpl)
	if len(tokens) == 0 {
		return "", fmt.Errorf("adapter: agent %q has empty launch template", d.Name)
	}

	// Apply per-host command binary override to the leading command token.
	if d.CommandOverride != "" {
		tokens[0] = formatRemoteCommandToken(d.CommandOverride)
	} else {
		tokens[0] = formatRemoteCommandToken(tokens[0])
	}

	model := strings.TrimSpace(job.Model)
	if isResume {
		model = ""
	}
	sessionID := strings.TrimSpace(job.SessionID)
	usedPromptPlaceholder := false

	var expanded []string
	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]

		// Drop `--model {model}` or `-m {model}` cleanly when job.Model is empty or on resume.
		if (tok == "--model" || tok == "-m") && i+1 < len(tokens) && tokens[i+1] == "{model}" {
			if model == "" {
				i++
				continue
			}
			expanded = append(expanded, tok, shellQuote(model))
			i++
			continue
		}

		if strings.Contains(tok, "{model}") {
			if model == "" {
				continue
			}
			tok = strings.ReplaceAll(tok, "{model}", shellQuote(model))
		}
		if strings.Contains(tok, "{workdir}") {
			tok = strings.ReplaceAll(tok, "{workdir}", shellPath(workdir))
		}
		if strings.Contains(tok, "{prompt_file}") {
			usedPromptPlaceholder = true
			tok = strings.ReplaceAll(tok, "{prompt_file}", shellPath(promptFile))
		}
		if strings.Contains(tok, "{session_id}") {
			if sessionID == "" {
				continue
			}
			tok = strings.ReplaceAll(tok, "{session_id}", shellQuote(sessionID))
		}

		expanded = append(expanded, tok)
	}

	var effortToks []string
	for _, a := range effortArgs(d.Name, job.Effort) {
		if isSafeBareToken(a) {
			effortToks = append(effortToks, a)
		} else {
			effortToks = append(effortToks, shellQuote(a))
		}
	}
	expanded = insertBeforeOptionEnd(expanded, effortToks, shellOptionEndIndex)

	if allowSandbox && strings.TrimSpace(d.SandboxFlag) != "" {
		expanded = append(expanded, strings.Fields(d.SandboxFlag)...)
	}

	cmd := strings.Join(expanded, " ")

	// Wire prompt delivery (argument vs stdin) and log redirections.
	switch d.effectivePromptStyle() {
	case PromptStyleArgument:
		if usedPromptPlaceholder {
			cmd = fmt.Sprintf("%s </dev/null > %s 2>&1", cmd, shellPath(logFile))
		} else {
			cmd = fmt.Sprintf(`%s "$(cat %s)" </dev/null > %s 2>&1`, cmd, shellPath(promptFile), shellPath(logFile))
		}
	default:
		// Codex always receives prompt.txt on stdin and must NEVER get `</dev/null`.
		// Other stdin-style agents also receive prompt.txt on stdin unless their template used `{prompt_file}`.
		if d.ForceStdinNoDevNull || !usedPromptPlaceholder {
			cmd = fmt.Sprintf("%s < %s > %s 2>&1", cmd, shellPath(promptFile), shellPath(logFile))
		} else {
			cmd = fmt.Sprintf("%s </dev/null > %s 2>&1", cmd, shellPath(logFile))
		}
	}
	return cmd, nil
}

// BuildWindowsLaunchCommand constructs the agent invocation line placed inside agent-run.cmd.
// It uses the same config-driven driver fields as BuildLaunchCommand while applying Windows
// cmd.exe syntax, separate stdout/stderr log redirections (agent.log and agent.err.log),
// Codex's stdin `-` argument (without `</dev/null` or `< NUL`), and omitting `--model` on resume.
func (d AgentDriver) BuildWindowsLaunchCommand(job *store.Job, workdir, promptFile, logFile, errLogFile string, allowSandbox bool) (string, error) {
	if job == nil {
		return "", ErrNilJob
	}
	tmpl, isResume := d.selectLaunchTemplate(job)
	if tmpl == "" {
		return "", fmt.Errorf("adapter: agent %q has empty launch template", d.Name)
	}

	tokens := strings.Fields(tmpl)
	if len(tokens) == 0 {
		return "", fmt.Errorf("adapter: agent %q has empty launch template", d.Name)
	}

	exeRaw := tokens[0]
	if strings.TrimSpace(d.CommandOverride) != "" {
		exeRaw = strings.TrimSpace(d.CommandOverride)
	}

	model := strings.TrimSpace(job.Model)
	if isResume {
		model = ""
	}
	sessionID := strings.TrimSpace(job.SessionID)
	winWorkdir := normalizeWindowsPath(workdir)
	winPrompt := normalizeWindowsPath(promptFile)
	winLog := normalizeWindowsPath(logFile)
	winErrLog := normalizeWindowsPath(errLogFile)
	usedPromptPlaceholder := false

	rawTokens := make([]string, 0, len(tokens)+4)
	rawTokens = append(rawTokens, exeRaw)

	for i := 1; i < len(tokens); i++ {
		tok := tokens[i]

		// Drop `--model {model}` or `-m {model}` cleanly when job.Model is empty or on resume.
		if (tok == "--model" || tok == "-m") && i+1 < len(tokens) && tokens[i+1] == "{model}" {
			if model == "" {
				i++
				continue
			}
			rawTokens = append(rawTokens, tok, model)
			i++
			continue
		}

		if strings.Contains(tok, "{model}") {
			if model == "" {
				continue
			}
			tok = strings.ReplaceAll(tok, "{model}", model)
		}
		if strings.Contains(tok, "{workdir}") {
			tok = strings.ReplaceAll(tok, "{workdir}", winWorkdir)
		}
		if strings.Contains(tok, "{prompt_file}") {
			usedPromptPlaceholder = true
			tok = strings.ReplaceAll(tok, "{prompt_file}", winPrompt)
		}
		if strings.Contains(tok, "{session_id}") {
			if sessionID == "" {
				continue
			}
			tok = strings.ReplaceAll(tok, "{session_id}", sessionID)
		}

		rawTokens = append(rawTokens, tok)
	}

	rawTokens = insertBeforeOptionEnd(rawTokens, effortArgs(d.Name, job.Effort), rawOptionEndIndex)

	if allowSandbox && strings.TrimSpace(d.SandboxFlag) != "" {
		rawTokens = append(rawTokens, strings.Fields(d.SandboxFlag)...)
	}

	switch d.effectivePromptStyle() {
	case PromptStyleArgument:
		if usedPromptPlaceholder {
			cmd := formatWindowsCommandLine(rawTokens)
			return fmt.Sprintf("%s < NUL > %s 2> %s", cmd, cmdQuoteArg(winLog), cmdQuoteArg(winErrLog)), nil
		}
		// cmd.exe cannot safely pass multi-line UTF-8 file contents as a single argument;
		// delegate argument reading to PowerShell so prompt.txt is passed verbatim.
		psArgs := make([]string, 0, len(rawTokens)+1)
		for _, t := range rawTokens {
			psArgs = append(psArgs, psExpr(t))
		}
		psArgs = append(psArgs, fmt.Sprintf("([System.IO.File]::ReadAllText(%s, [System.Text.Encoding]::UTF8))", psExpr(winPrompt)))
		psCmd := wrapPowerShellCommand("& " + strings.Join(psArgs, " ") + "; exit $LASTEXITCODE")
		return fmt.Sprintf("%s < NUL > %s 2> %s", psCmd, cmdQuoteArg(winLog), cmdQuoteArg(winErrLog)), nil
	default:
		if d.WindowsStdinArg != "" && !usedPromptPlaceholder {
			if len(rawTokens) == 0 || rawTokens[len(rawTokens)-1] != d.WindowsStdinArg {
				rawTokens = append(rawTokens, d.WindowsStdinArg)
			}
		}
		cmd := formatWindowsCommandLine(rawTokens)
		// Codex always receives prompt.txt on stdin and must NEVER get `</dev/null` or `< NUL`.
		if d.ForceStdinNoDevNull || !usedPromptPlaceholder {
			return fmt.Sprintf("%s < %s > %s 2> %s", cmd, cmdQuoteArg(winPrompt), cmdQuoteArg(winLog), cmdQuoteArg(winErrLog)), nil
		}
		return fmt.Sprintf("%s < NUL > %s 2> %s", cmd, cmdQuoteArg(winLog), cmdQuoteArg(winErrLog)), nil
	}
}

func (d AgentDriver) effectivePromptStyle() PromptStyle {
	switch PromptStyle(strings.TrimSpace(string(d.PromptStyle))) {
	case PromptStyleArgument:
		return PromptStyleArgument
	default:
		return PromptStyleStdin
	}
}

// ShouldAttachSandboxFlag implements the explicit sandbox relaxation policy (design §5):
// sandbox_flag is permitted ONLY when:
//  1. Neither workdir nor worktree_spec.repo_path touches host.DenyPaths, AND
//  2. The job is isolated either via an explicit worktree_spec (repo_path set) OR
//     by running in a workdir strictly under host.WorkdirRoot.
func ShouldAttachSandboxFlag(host config.HostConfig, job *store.Job, resolvedWorkdir string) bool {
	if job == nil {
		return false
	}
	if isPathDenied(resolvedWorkdir, host.DenyPaths) {
		return false
	}
	if job.WorktreeSpec != nil && strings.TrimSpace(job.WorktreeSpec.RepoPath) != "" {
		if isPathDenied(job.WorktreeSpec.RepoPath, host.DenyPaths) {
			return false
		}
		return true
	}
	if strings.TrimSpace(host.WorkdirRoot) != "" && isSubpath(resolvedWorkdir, host.WorkdirRoot) {
		return true
	}
	return false
}

func applyCommandOverride(tmpl, override string) string {
	tokens := strings.Fields(tmpl)
	if len(tokens) == 0 {
		return ""
	}
	if strings.TrimSpace(override) != "" {
		tokens[0] = formatRemoteCommandToken(override)
	} else {
		tokens[0] = formatRemoteCommandToken(tokens[0])
	}
	return strings.Join(tokens, " ")
}

func formatRemoteCommandToken(cmd string) string {
	cmd = strings.TrimSpace(cmd)
	if cmd == "~" || strings.HasPrefix(cmd, "~/") {
		return shellPath(cmd)
	}
	// Simple bare binary names or paths without shell metacharacters stay readable.
	if isSafeBareToken(cmd) {
		return cmd
	}
	return shellQuote(cmd)
}

func isSafeBareToken(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := (c >= 'a' && c <= 'z') ||
			(c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') ||
			c == '_' || c == '-' || c == '.' || c == '/' || c == ':'
		if !ok {
			return false
		}
	}
	return true
}

func formatEnvExports(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	lines := make([]string, 0, len(keys))
	for _, k := range keys {
		if !isValidEnvKey(k) {
			continue
		}
		lines = append(lines, fmt.Sprintf("export %s=%s", k, shellEnvValue(env[k])))
	}
	return lines
}

func isValidEnvKey(k string) bool {
	if k == "" {
		return false
	}
	for i := 0; i < len(k); i++ {
		c := k[i]
		if i == 0 {
			if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_') {
				return false
			}
		} else {
			if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_') {
				return false
			}
		}
	}
	return true
}

// shellQuote single-quotes s for safe interpolation in POSIX sh.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// shellPath quotes a remote POSIX filesystem path while allowing a leading `~/` to expand
// via `$HOME` on the target host.
func shellPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "~" {
		return `"$HOME"`
	}
	if strings.HasPrefix(p, "~/") {
		rest := p[2:]
		if rest == "" {
			return `"$HOME"`
		}
		return `"$HOME"/` + shellQuote(rest)
	}
	return shellQuote(p)
}

// shellEnvValue formats an environment variable value for `export KEY=...` in POSIX sh.
// It converts leading `~/` and colon-separated `:~/` entries to `$HOME/` so PATH overrides
// such as `~/.config/nvm/.../bin:$PATH` expand cleanly in non-interactive shells.
func shellEnvValue(v string) string {
	if strings.HasPrefix(v, "~/") {
		v = "$HOME/" + v[2:]
	} else if v == "~" {
		v = "$HOME"
	}
	v = strings.ReplaceAll(v, ":~/", ":$HOME/")
	// Escape double quotes and backticks inside double-quoted string while keeping $VAR expansion.
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, `"`, `\"`)
	v = strings.ReplaceAll(v, "`", "\\`")
	return `"` + v + `"`
}

// normalizeRemotePath cleans a POSIX, tilde-prefixed, or Windows path for prefix/containment checks.
// Windows drive paths (e.g. `H:\a`) and UNC paths (`\\server\share`) are normalized to lowercase
// forward-slash form with traversal clamped to the drive/UNC root.
func normalizeRemotePath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	p = strings.ReplaceAll(p, `\`, `/`)
	if p == "~" {
		return "~"
	}
	if strings.HasPrefix(p, "~/") {
		cleaned := path.Clean("/" + p[2:])
		if cleaned == "/" {
			return "~"
		}
		return "~" + cleaned
	}
	if len(p) >= 2 && isASCIILetter(p[0]) && p[1] == ':' {
		drive := strings.ToLower(p[:2])
		rest := p[2:]
		if rest == "" || rest == "/" {
			return drive + "/"
		}
		cleaned := path.Clean("/" + rest)
		return drive + strings.ToLower(cleaned)
	}
	if strings.HasPrefix(p, "//") {
		rest := strings.TrimLeft(p, "/")
		if rest == "" {
			return "//"
		}
		cleaned := path.Clean("/" + rest)
		return "/" + strings.ToLower(cleaned)
	}
	return path.Clean(p)
}

func isASCIILetter(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// isSubpath reports whether target is strictly inside root after cleaning both paths.
func isSubpath(target, root string) bool {
	nt := normalizeRemotePath(target)
	nr := normalizeRemotePath(root)
	if nt == "" || nr == "" {
		return false
	}
	if nt == nr {
		return false
	}
	if strings.HasSuffix(nr, "/") {
		return strings.HasPrefix(nt, nr)
	}
	return strings.HasPrefix(nt, nr+"/")
}

// isPathDenied reports whether target equals or lies inside any configured deny path.
func isPathDenied(target string, denyPaths []string) bool {
	nt := normalizeRemotePath(target)
	if nt == "" {
		return false
	}
	for _, dp := range denyPaths {
		nd := normalizeRemotePath(dp)
		if nd == "" {
			continue
		}
		if nt == nd {
			return true
		}
		if strings.HasSuffix(nd, "/") {
			if strings.HasPrefix(nt, nd) {
				return true
			}
			continue
		}
		if strings.HasPrefix(nt, nd+"/") {
			return true
		}
	}
	return false
}

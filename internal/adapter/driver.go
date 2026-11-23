package adapter

import (
	"errors"
	"fmt"
	"path"
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

// VersionProbeNotFoundMarker is emitted by BuildVersionCommand when the target executable
// cannot be resolved before invocation, distinguishing command absence from runtime failures.
const VersionProbeNotFoundMarker = "__QUOTA_FLEET_CMD_NOT_FOUND__:"

// BuildVersionCommand builds the shell command passed to FileAccess.RunCommand
// to query the agent's version (`<command> <version_args...>`), defaulting to `--version`.
// Before executing the target command, it checks whether the target executable can be resolved
// and emits VersionProbeNotFoundMarker with exit code 127 only when the executable itself is absent.
// On Windows, it returns PowerShell statements suitable for windowsFileAccess.RunCommand.
func (d AgentDriver) BuildVersionCommand(versionArgs []string, isWindows bool) (string, error) {
	exe := d.ResolveBaseCommand()
	if exe == "" {
		return "", fmt.Errorf("adapter: agent %q has no executable command configured", d.Name)
	}
	args := versionArgs
	if len(args) == 0 {
		args = []string{"--version"}
	}

	if isWindows {
		var stmts []string
		stmts = append(stmts, formatPowerShellEnvAssigns(d.Env)...)
		exeExpr := psExpr(exe)
		stmts = append(stmts, fmt.Sprintf(
			"if (-not (Get-Command %s -ErrorAction SilentlyContinue) -and -not (Test-Path -LiteralPath %s)) { [Console]::Error.WriteLine(%s + ' ' + %s); exit 127 }",
			exeExpr, exeExpr, psExpr(VersionProbeNotFoundMarker), exeExpr,
		))
		psTokens := make([]string, 0, 1+len(args))
		psTokens = append(psTokens, exeExpr)
		for _, a := range args {
			psTokens = append(psTokens, psExpr(a))
		}
		stmts = append(stmts, "& "+strings.Join(psTokens, " "))
		return strings.Join(stmts, "; "), nil
	}

	exeToken := formatRemoteCommandToken(exe)
	var preCheck string
	if strings.Contains(exe, "/") {
		preCheck = fmt.Sprintf(
			"if [ ! -e %s ]; then printf '%%s %%s\\n' %s %s >&2; exit 127; fi",
			exeToken, shellQuote(VersionProbeNotFoundMarker), shellQuote(exe),
		)
	} else {
		preCheck = fmt.Sprintf(
			"if ! command -v %s >/dev/null 2>&1; then printf '%%s %%s\\n' %s %s >&2; exit 127; fi",
			exeToken, shellQuote(VersionProbeNotFoundMarker), shellQuote(exe),
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
		return cmd, nil
	}
	lines := append(formatEnvExports(d.Env), cmd)
	return strings.Join(lines, "; "), nil
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

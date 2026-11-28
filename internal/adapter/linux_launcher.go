package adapter

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"

	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

const (
	runArtifactsDirName = ".fleet"
	runArtifactsRunsRel = ".fleet/runs"

	promptFileName   = "prompt.txt"
	runScriptName    = "run.sh"
	winRunScriptName = "agent-run.cmd"
	legacyLaunchPS1  = "launch.ps1"
	pidFileName      = "agent.pid"
	exitFileName     = "agent.exit"
	exitTmpFileName  = "agent.exit.tmp"
	logFileName      = "agent.log"
	errLogFileName   = "agent.err.log"

	defaultLogTailLines = 100
	defaultLogMaxBytes  = 16 * 1024
)

// DefaultArtifactFiles lists the standard files checked during CollectEvidence.
var DefaultArtifactFiles = []string{
	logFileName,
	exitFileName,
	"summary.md",
	"result.txt",
}

// adapterControlFiles are ignored when determining whether git status --porcelain shows real diffs,
// and are routed to the per-job run artifacts directory (<workdir>/.fleet/runs/<job_id>/).
var adapterControlFiles = map[string]bool{
	promptFileName:   true,
	runScriptName:    true,
	winRunScriptName: true,
	legacyLaunchPS1:  true,
	pidFileName:      true,
	exitFileName:     true,
	exitTmpFileName:  true,
	logFileName:      true,
	errLogFileName:   true,
}

func isRunArtifactFile(name string) bool {
	return adapterControlFiles[strings.TrimSpace(name)]
}

func isAdapterControlPath(filePath string) bool {
	norm := strings.ReplaceAll(strings.TrimSpace(filePath), `\`, "/")
	norm = strings.TrimPrefix(norm, "./")
	if adapterControlFiles[norm] {
		return true
	}
	return norm == runArtifactsDirName || strings.HasPrefix(norm, runArtifactsDirName+"/")
}

func linuxRunDir(workdir string, job *store.Job) (string, error) {
	if job == nil {
		return "", ErrNilJob
	}
	if job.ID <= 0 {
		return "", errors.New("adapter: job id must be positive")
	}
	return joinRemotePath(workdir, fmt.Sprintf("%s/%d", runArtifactsRunsRel, job.ID)), nil
}

// ErrUnsupportedOS is returned when a host's OS has no OSLauncher implementation yet.
var ErrUnsupportedOS = errors.New("adapter: host OS is not yet implemented")

// ErrDeniedPath is returned when a job's workdir or repo_path touches host.DenyPaths.
var ErrDeniedPath = errors.New("adapter: path is denied by host deny_paths")

// LaunchSpec bundles resolved agent driver and policy inputs passed to an OSLauncher.
type LaunchSpec struct {
	Driver       AgentDriver
	AllowSandbox bool
}

// OSLauncher abstracts OS-specific workdir setup, detached process launch, liveness inspection,
// and evidence collection over a Transport.
type OSLauncher interface {
	PrepareWorkdir(ctx context.Context, host config.HostConfig, job *store.Job, workdir string) error
	Launch(ctx context.Context, host config.HostConfig, job *store.Job, workdir string, spec LaunchSpec) (LaunchInfo, error)
	Status(ctx context.Context, host config.HostConfig, job *store.Job, workdir string) (RunState, error)
	CollectEvidence(ctx context.Context, host config.HostConfig, job *store.Job, workdir string) (Evidence, error)
}

type linuxLauncher struct {
	transport     Transport
	artifactFiles []string
	logTailLines  int
	logMaxBytes   int
}

func newLinuxLauncher(transport Transport, artifactFiles []string, logTailLines, logMaxBytes int) *linuxLauncher {
	if len(artifactFiles) == 0 {
		artifactFiles = append([]string(nil), DefaultArtifactFiles...)
	} else {
		artifactFiles = append([]string(nil), artifactFiles...)
	}
	if logTailLines <= 0 {
		logTailLines = defaultLogTailLines
	}
	if logMaxBytes <= 0 {
		logMaxBytes = defaultLogMaxBytes
	}
	return &linuxLauncher{
		transport:     transport,
		artifactFiles: artifactFiles,
		logTailLines:  logTailLines,
		logMaxBytes:   logMaxBytes,
	}
}

func (l *linuxLauncher) PrepareWorkdir(ctx context.Context, host config.HostConfig, job *store.Job, workdir string) error {
	if job.WorktreeSpec == nil || strings.TrimSpace(job.WorktreeSpec.RepoPath) == "" {
		cmd := fmt.Sprintf("mkdir -p %s", shellPath(workdir))
		res, err := l.transport.Run(ctx, host, cmd)
		if err != nil {
			return fmt.Errorf("prepare workdir mkdir: %w", err)
		}
		if res.ExitCode != 0 {
			return fmt.Errorf("prepare workdir mkdir failed (exit %d): %s", res.ExitCode, strings.TrimSpace(res.Stderr))
		}
		return nil
	}

	// Worktree mode: reuse existing git worktree if workdir/.git already exists.
	checkCmd := fmt.Sprintf("test -e %s", shellPath(joinRemotePath(workdir, ".git")))
	checkRes, err := l.transport.Run(ctx, host, checkCmd)
	if err != nil {
		return fmt.Errorf("check existing worktree: %w", err)
	}
	if checkRes.ExitCode == 0 {
		return nil
	}

	repoPath := strings.TrimSpace(job.WorktreeSpec.RepoPath)
	baseRef := strings.TrimSpace(job.WorktreeSpec.BaseRef)
	branch := strings.TrimSpace(job.WorktreeSpec.Branch)

	parentDir := remoteDir(workdir)
	var addCmd string
	switch {
	case branch != "" && baseRef != "":
		addCmd = fmt.Sprintf(
			"mkdir -p %s && if git -C %s show-ref --verify --quiet %s; then git -C %s worktree add %s %s; else git -C %s worktree add -b %s %s %s; fi",
			shellPath(parentDir),
			shellPath(repoPath), shellQuote("refs/heads/"+branch),
			shellPath(repoPath), shellPath(workdir), shellQuote(branch),
			shellPath(repoPath), shellQuote(branch), shellPath(workdir), shellQuote(baseRef),
		)
	case branch != "":
		addCmd = fmt.Sprintf(
			"mkdir -p %s && if git -C %s show-ref --verify --quiet %s; then git -C %s worktree add %s %s; else git -C %s worktree add -b %s %s; fi",
			shellPath(parentDir),
			shellPath(repoPath), shellQuote("refs/heads/"+branch),
			shellPath(repoPath), shellPath(workdir), shellQuote(branch),
			shellPath(repoPath), shellQuote(branch), shellPath(workdir),
		)
	case baseRef != "":
		addCmd = fmt.Sprintf(
			"mkdir -p %s && git -C %s worktree add --detach %s %s",
			shellPath(parentDir), shellPath(repoPath), shellPath(workdir), shellQuote(baseRef),
		)
	default:
		addCmd = fmt.Sprintf(
			"mkdir -p %s && git -C %s worktree add --detach %s HEAD",
			shellPath(parentDir), shellPath(repoPath), shellPath(workdir),
		)
	}

	res, err := l.transport.Run(ctx, host, addCmd)
	if err != nil {
		return fmt.Errorf("git worktree add: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("git worktree add failed (exit %d): %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}

func (l *linuxLauncher) Launch(ctx context.Context, host config.HostConfig, job *store.Job, workdir string, spec LaunchSpec) (LaunchInfo, error) {
	runDir, err := linuxRunDir(workdir, job)
	if err != nil {
		return LaunchInfo{}, err
	}
	promptPath := joinRemotePath(runDir, promptFileName)
	runShPath := joinRemotePath(runDir, runScriptName)
	pidPath := joinRemotePath(runDir, pidFileName)
	exitPath := joinRemotePath(runDir, exitFileName)
	exitTmpPath := joinRemotePath(runDir, exitTmpFileName)
	logPath := joinRemotePath(runDir, logFileName)

	promptText := BuildPromptText(job)
	launchCmd, err := spec.Driver.BuildLaunchCommand(job, workdir, promptPath, logPath, spec.AllowSandbox)
	if err != nil {
		return LaunchInfo{}, err
	}
	runScript := BuildRunScript(workdir, runDir, spec.Driver, launchCmd)

	promptB64 := base64.StdEncoding.EncodeToString([]byte(promptText))
	scriptB64 := base64.StdEncoding.EncodeToString([]byte(runScript))

	stageCmd := fmt.Sprintf(
		"mkdir -p %s %s && rm -f %s %s %s && printf '%%s' %s | base64 -d > %s && printf '%%s' %s | base64 -d > %s && chmod 700 %s",
		shellPath(workdir),
		shellPath(runDir),
		shellPath(exitPath),
		shellPath(exitTmpPath),
		shellPath(pidPath),
		shellQuote(promptB64),
		shellPath(promptPath),
		shellQuote(scriptB64),
		shellPath(runShPath),
		shellPath(runShPath),
	)
	// A rate-limit re-run keeps the previous log and drops this step's stale verdict first.
	prep, err := linuxRetryPrepCommand(workdir, runDir, job)
	if err != nil {
		return LaunchInfo{}, err
	}
	if prep != "" {
		stageCmd = prep + " && " + stageCmd
	}
	stageRes, err := l.transport.Run(ctx, host, stageCmd)
	if err != nil {
		return LaunchInfo{}, fmt.Errorf("stage prompt.txt and run.sh: %w", err)
	}
	if stageRes.ExitCode != 0 {
		return LaunchInfo{}, fmt.Errorf("stage prompt.txt and run.sh failed (exit %d): %s", stageRes.ExitCode, strings.TrimSpace(stageRes.Stderr))
	}

	// Detach via setsid + nohup so the agent survives SSH disconnect; record PID in per-job agent.pid.
	detachCmd := fmt.Sprintf(
		"setsid nohup sh %s >/dev/null 2>&1 </dev/null & pid=$!; printf '%%d\\n' \"$pid\" > %s; printf '%%d\\n' \"$pid\"",
		shellPath(runShPath),
		shellPath(pidPath),
	)
	launchRes, err := l.transport.Run(ctx, host, detachCmd)
	if err != nil {
		return LaunchInfo{}, fmt.Errorf("detach launch: %w", err)
	}
	if launchRes.ExitCode != 0 {
		return LaunchInfo{}, fmt.Errorf("detach launch failed (exit %d): %s", launchRes.ExitCode, strings.TrimSpace(launchRes.Stderr))
	}

	pidStr := strings.TrimSpace(launchRes.Stdout)
	pid, err := strconv.Atoi(pidStr)
	if err != nil || pid <= 0 {
		return LaunchInfo{}, fmt.Errorf("invalid pid returned by launcher %q: %w", pidStr, err)
	}

	return LaunchInfo{
		PID: pid,
		Meta: map[string]string{
			"adapter":         "ssh",
			"os":              "linux",
			"agent":           job.Agent,
			"host":            job.Host,
			"workdir":         workdir,
			"run_dir":         runDir,
			"sandbox_relaxed": strconv.FormatBool(spec.AllowSandbox),
		},
	}, nil
}

func (l *linuxLauncher) Status(ctx context.Context, host config.HostConfig, job *store.Job, workdir string) (RunState, error) {
	runDir, err := linuxRunDir(workdir, job)
	if err != nil {
		return RunState{}, err
	}
	exitPath := joinRemotePath(runDir, exitFileName)
	pidPath := joinRemotePath(runDir, pidFileName)

	cmd := fmt.Sprintf(
		"if [ -f %s ]; then "+
			"if { printf 'EXITED:'; cat; } 2>/dev/null < %s; then :; "+
			"else printf 'MISSING:unreadable_exit_file\\n'; fi; "+
			"elif [ -f %s ]; then "+
			"if raw=\"$(LC_ALL=C tr '\\000' '?' 2>/dev/null < %s)\"; then "+
			"set -f; IFS=$(printf ' \\t\\r\\nx'); IFS=${IFS%%x}; set -- $raw; "+
			"if [ \"$#\" -ne 1 ]; then printf 'MISSING:invalid_pid:%%s\\n' \"$*\"; "+
			"else pid=\"$1\"; case \"$pid\" in ''|0*|*[!0123456789]*|??????????*) printf 'MISSING:invalid_pid:%%s\\n' \"$pid\" ;; "+
			"*) if [ \"$pid\" -gt 0 ] && kill -0 \"$pid\" 2>/dev/null; then printf 'RUNNING:%%s\\n' \"$pid\"; "+
			"else printf 'MISSING:pid_dead:%%s\\n' \"$pid\"; fi ;; esac; fi; "+
			"else printf 'MISSING:unreadable_pid_file\\n'; fi; "+
			"else printf 'MISSING:no_files\\n'; fi",
		shellPath(exitPath), shellPath(exitPath),
		shellPath(pidPath), shellPath(pidPath),
	)

	res, err := l.transport.Run(ctx, host, cmd)
	if err != nil {
		return RunState{}, fmt.Errorf("status probe: %w", err)
	}
	if res.ExitCode != 0 {
		return RunState{}, fmt.Errorf("status probe failed (exit %d): %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}

	stdout := res.Stdout
	switch {
	case strings.HasPrefix(stdout, "EXITED:"):
		rawCode := strings.TrimPrefix(stdout, "EXITED:")
		code, err := parseExitCode(rawCode)
		if err != nil {
			return RunState{
				State:  StateMissing,
				Detail: fmt.Sprintf("invalid_exit_file:%q", rawCode),
			}, nil
		}
		return RunState{
			State:    StateExited,
			ExitCode: code,
			Detail:   fmt.Sprintf("agent.exit=%d", code),
		}, nil
	case strings.HasPrefix(stdout, "RUNNING:"):
		pid := strings.TrimSpace(strings.TrimPrefix(stdout, "RUNNING:"))
		return RunState{
			State:  StateRunning,
			Detail: "pid=" + pid,
		}, nil
	case strings.HasPrefix(stdout, "MISSING:"):
		detail := strings.TrimSpace(strings.TrimPrefix(stdout, "MISSING:"))
		return RunState{
			State:  StateMissing,
			Detail: detail,
		}, nil
	default:
		return RunState{}, fmt.Errorf("unexpected status probe output: %q", strings.TrimSpace(stdout))
	}
}

func (l *linuxLauncher) CollectEvidence(ctx context.Context, host config.HostConfig, job *store.Job, workdir string) (Evidence, error) {
	runDir, err := linuxRunDir(workdir, job)
	if err != nil {
		return Evidence{}, err
	}
	ev := Evidence{
		Artifacts: make(map[string]bool, len(l.artifactFiles)),
	}

	// 1. Collect git status --porcelain (non-git dirs yield non-zero exit and HasDiff=false).
	gitCmd := fmt.Sprintf("git -C %s status --porcelain", shellPath(workdir))
	gitRes, err := l.transport.Run(ctx, host, gitCmd)
	if err != nil {
		return Evidence{}, fmt.Errorf("collect git status: %w", err)
	}
	if gitRes.ExitCode == 0 {
		diffLines := filterPorcelainLines(gitRes.Stdout)
		if len(diffLines) > 0 {
			ev.HasDiff = true
			ev.DiffSummary = strings.Join(diffLines, "\n")
		}
	}

	// 2. Check artifact files existence (run artifacts in runDir, deliverables in workdir).
	if len(l.artifactFiles) > 0 {
		var checks []string
		for _, name := range l.artifactFiles {
			base := workdir
			if isRunArtifactFile(name) {
				base = runDir
			}
			full := joinRemotePath(base, name)
			checks = append(checks, fmt.Sprintf("if [ -e %s ]; then printf '1:%s\\n'; else printf '0:%s\\n'; fi",
				shellPath(full), escapePrintfLiteral(name), escapePrintfLiteral(name)))
		}
		artCmd := strings.Join(checks, "; ")
		artRes, err := l.transport.Run(ctx, host, artCmd)
		if err != nil {
			return Evidence{}, fmt.Errorf("collect artifacts: %w", err)
		}
		for _, line := range strings.Split(strings.TrimSpace(artRes.Stdout), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			flag, name, ok := strings.Cut(line, ":")
			if !ok || name == "" {
				continue
			}
			ev.Artifacts[name] = (flag == "1")
		}
	}

	// 3. Collect tail of per-job agent.log into FinalReport.
	logPath := joinRemotePath(runDir, logFileName)
	logCmd := fmt.Sprintf("if [ -f %s ]; then tail -n %d %s; fi", shellPath(logPath), l.logTailLines, shellPath(logPath))
	logRes, err := l.transport.Run(ctx, host, logCmd)
	if err != nil {
		return Evidence{}, fmt.Errorf("collect agent.log tail: %w", err)
	}
	report := strings.TrimSpace(logRes.Stdout)
	if len(report) > l.logMaxBytes {
		report = report[len(report)-l.logMaxBytes:]
	}
	ev.FinalReport = report

	// 4. Read per-job agent.exit if present (preserving raw bytes so NUL/control chars cannot collapse).
	exitPath := joinRemotePath(runDir, exitFileName)
	exitCmd := fmt.Sprintf("if [ -f %s ]; then cat 2>/dev/null < %s; else exit 42; fi", shellPath(exitPath), shellPath(exitPath))
	exitRes, err := l.transport.Run(ctx, host, exitCmd)
	if err != nil {
		return Evidence{}, fmt.Errorf("collect agent.exit: %w", err)
	}
	switch {
	case exitRes.ExitCode == 42:
		ev.ExitCodeUnknown = true
		ev.Notes = append(ev.Notes, "exit code unknown: agent.exit not found")
	case exitRes.ExitCode != 0:
		ev.ExitCodeUnknown = true
		ev.Notes = append(ev.Notes, "exit code unknown: unreadable agent.exit")
	default:
		if code, perr := parseExitCode(exitRes.Stdout); perr == nil {
			ev.ExitCode = code
		} else {
			ev.ExitCodeUnknown = true
			ev.Notes = append(ev.Notes, fmt.Sprintf("exit code unknown: unparseable agent.exit (%q)", exitRes.Stdout))
		}
	}

	return ev, nil
}

// BuildPromptText builds the UTF-8 (BOM-free) contents of prompt.txt from job.Instructions
// (falling back to job.Title if Instructions is empty, and appending AcceptanceCriteria if present).
func BuildPromptText(job *store.Job) string {
	if job == nil {
		return ""
	}
	body := strings.TrimPrefix(job.Instructions, "\xef\xbb\xbf")
	body = strings.ReplaceAll(body, "\r\n", "\n")
	if strings.TrimSpace(body) == "" {
		body = strings.TrimPrefix(job.Title, "\xef\xbb\xbf")
	}

	ac := strings.TrimPrefix(job.AcceptanceCriteria, "\xef\xbb\xbf")
	ac = strings.ReplaceAll(ac, "\r\n", "\n")
	ac = strings.TrimSpace(ac)
	if ac != "" && !strings.Contains(body, ac) {
		body = strings.TrimRight(body, "\n") + "\n\nAcceptance Criteria:\n" + ac
	}
	if body != "" && !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	if job.RateLimitRetries > 0 {
		body = rateLimitRetryNote(job.RateLimitRetries) + body
	}
	return body
}

// rateLimitRetryNote is prepended to the prompt of a run that follows a rate-limit requeue
// (Issue #56 §5). The workdir is kept as is, so the agent is told to check it first.
func rateLimitRetryNote(retry int) string {
	return fmt.Sprintf("[quota-fleet] This is automatic re-run %d of this job: the previous run stopped on a provider usage/rate limit (429). "+
		"The working directory was kept, so it may contain partial changes or half-written output files (for example summary.md) from the previous run. "+
		"Before starting, check `git status` and the existing files, keep valid partial work, overwrite incomplete outputs, and make sure every acceptance criterion is met.\n\n", retry)
}

// BuildRunScript generates the POSIX shell wrapper script (run.sh) that sets per-host env vars,
// enters workdir, runs the agent command, atomically records the exit code to runDir/agent.exit, and exits.
// If runDir is empty, it defaults to workdir.
func BuildRunScript(workdir, runDir string, driver AgentDriver, launchCmd string) string {
	if strings.TrimSpace(runDir) == "" {
		runDir = workdir
	}
	exitPath := joinRemotePath(runDir, exitFileName)
	exitTmpPath := joinRemotePath(runDir, exitTmpFileName)

	var lines []string
	lines = append(lines, "#!/bin/sh")
	lines = append(lines, "set -u")
	lines = append(lines, formatEnvExports(driver.Env)...)
	lines = append(lines, fmt.Sprintf("cd %s || exit 1", shellPath(workdir)))
	lines = append(lines, fmt.Sprintf("rm -f %s %s", shellPath(exitPath), shellPath(exitTmpPath)))
	lines = append(lines, launchCmd)
	lines = append(lines, "rc=$?")
	lines = append(lines, fmt.Sprintf("printf '%%d\\n' \"$rc\" > %s && mv %s %s",
		shellPath(exitTmpPath), shellPath(exitTmpPath), shellPath(exitPath)))
	lines = append(lines, "exit \"$rc\"")
	return strings.Join(lines, "\n") + "\n"
}

func filterPorcelainLines(stdout string) []string {
	var out []string
	for _, raw := range strings.Split(strings.ReplaceAll(stdout, "\r\n", "\n"), "\n") {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		// git status --porcelain format: "XY path" (or "XY old -> new").
		filePath := raw
		if len(raw) >= 3 {
			filePath = strings.TrimSpace(raw[3:])
		}
		filePath = strings.Trim(filePath, `"`)
		if isAdapterControlPath(filePath) {
			continue
		}
		out = append(out, raw)
	}
	return out
}

func joinRemotePath(base, elem string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	elem = strings.TrimLeft(strings.TrimSpace(elem), "/")
	if base == "" {
		return elem
	}
	if elem == "" {
		return base
	}
	return base + "/" + elem
}

func remoteDir(p string) string {
	norm := normalizeRemotePath(p)
	if norm == "~" || norm == "" {
		return "~"
	}
	if strings.HasPrefix(norm, "~/") {
		d := path.Dir(norm[2:])
		if d == "." {
			return "~"
		}
		return "~/" + d
	}
	return path.Dir(norm)
}

func escapePrintfLiteral(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `%%`)
	s = strings.ReplaceAll(s, `'`, `'\''`)
	return s
}

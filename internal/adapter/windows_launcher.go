package adapter

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

func windowsRunDir(winWorkdir string, job *store.Job) (string, error) {
	if job == nil {
		return "", ErrNilJob
	}
	if job.ID <= 0 {
		return "", errors.New("adapter: job id must be positive")
	}
	return joinWindowsPath(winWorkdir, fmt.Sprintf(`%s\%d`, strings.ReplaceAll(runArtifactsRunsRel, "/", `\`), job.ID)), nil
}

// windowsDefaultArtifactFiles lists the standard files checked on Windows hosts during CollectEvidence.
var windowsDefaultArtifactFiles = []string{
	logFileName,
	errLogFileName,
	exitFileName,
	"summary.md",
	"result.txt",
}

// windowsLauncher implements OSLauncher for Windows worker hosts (default SSH shell cmd.exe),
// using PowerShell + Base64 file staging, icacls owner fixup, and WMI Win32_Process.Create
// detached launch so agent processes survive SSH disconnect.
type windowsLauncher struct {
	transport     Transport
	artifactFiles []string
	logTailLines  int
	logMaxBytes   int
}

func newWindowsLauncher(transport Transport, artifactFiles []string, logTailLines, logMaxBytes int) *windowsLauncher {
	if len(artifactFiles) == 0 {
		artifactFiles = append([]string(nil), windowsDefaultArtifactFiles...)
	} else {
		artifactFiles = append([]string(nil), artifactFiles...)
	}
	if logTailLines <= 0 {
		logTailLines = defaultLogTailLines
	}
	if logMaxBytes <= 0 {
		logMaxBytes = defaultLogMaxBytes
	}
	return &windowsLauncher{
		transport:     transport,
		artifactFiles: artifactFiles,
		logTailLines:  logTailLines,
		logMaxBytes:   logMaxBytes,
	}
}

func (w *windowsLauncher) PrepareWorkdir(ctx context.Context, host config.HostConfig, job *store.Job, workdir string) error {
	winWorkdir := normalizeWindowsPath(workdir)

	if job.WorktreeSpec == nil || strings.TrimSpace(job.WorktreeSpec.RepoPath) == "" {
		mkCmd := wrapPowerShellCommand(fmt.Sprintf("New-Item -ItemType Directory -Force -Path %s | Out-Null", psExpr(winWorkdir)))
		res, err := w.transport.Run(ctx, host, mkCmd)
		if err != nil {
			return fmt.Errorf("prepare windows workdir mkdir: %w", err)
		}
		if res.ExitCode != 0 {
			return fmt.Errorf("prepare windows workdir mkdir failed (exit %d): %s", res.ExitCode, strings.TrimSpace(res.Stderr))
		}
		if host.OwnerFixup {
			if err := w.runOwnerFixup(ctx, host, []string{winWorkdir}); err != nil {
				return err
			}
		}
		return nil
	}

	// Worktree mode: reuse existing git worktree if workdir\.git already exists.
	gitMarker := joinWindowsPath(winWorkdir, ".git")
	checkCmd := wrapPowerShellCommand(fmt.Sprintf("if (Test-Path -LiteralPath %s) { exit 0 } else { exit 1 }", psExpr(gitMarker)))
	checkRes, err := w.transport.Run(ctx, host, checkCmd)
	if err != nil {
		return fmt.Errorf("check existing windows worktree: %w", err)
	}
	if checkRes.ExitCode == 0 {
		return nil
	}

	repoPath := normalizeWindowsPath(job.WorktreeSpec.RepoPath)
	baseRef := strings.TrimSpace(job.WorktreeSpec.BaseRef)
	branch := strings.TrimSpace(job.WorktreeSpec.Branch)
	parentDir := windowsDir(winWorkdir)

	var psAdd string
	switch {
	case branch != "" && baseRef != "":
		psAdd = fmt.Sprintf(
			"New-Item -ItemType Directory -Force -Path %s | Out-Null; "+
				"& git -C %s show-ref --verify --quiet %s; "+
				"if ($LASTEXITCODE -eq 0) { & git -C %s worktree add %s %s } "+
				"else { & git -C %s worktree add -b %s %s %s }; "+
				"exit $LASTEXITCODE",
			psExpr(parentDir),
			psExpr(repoPath), psExpr("refs/heads/"+branch),
			psExpr(repoPath), psExpr(winWorkdir), psExpr(branch),
			psExpr(repoPath), psExpr(branch), psExpr(winWorkdir), psExpr(baseRef),
		)
	case branch != "":
		psAdd = fmt.Sprintf(
			"New-Item -ItemType Directory -Force -Path %s | Out-Null; "+
				"& git -C %s show-ref --verify --quiet %s; "+
				"if ($LASTEXITCODE -eq 0) { & git -C %s worktree add %s %s } "+
				"else { & git -C %s worktree add -b %s %s }; "+
				"exit $LASTEXITCODE",
			psExpr(parentDir),
			psExpr(repoPath), psExpr("refs/heads/"+branch),
			psExpr(repoPath), psExpr(winWorkdir), psExpr(branch),
			psExpr(repoPath), psExpr(branch), psExpr(winWorkdir),
		)
	case baseRef != "":
		psAdd = fmt.Sprintf(
			"New-Item -ItemType Directory -Force -Path %s | Out-Null; "+
				"& git -C %s worktree add --detach %s %s; "+
				"exit $LASTEXITCODE",
			psExpr(parentDir), psExpr(repoPath), psExpr(winWorkdir), psExpr(baseRef),
		)
	default:
		psAdd = fmt.Sprintf(
			"New-Item -ItemType Directory -Force -Path %s | Out-Null; "+
				"& git -C %s worktree add --detach %s HEAD; "+
				"exit $LASTEXITCODE",
			psExpr(parentDir), psExpr(repoPath), psExpr(winWorkdir),
		)
	}

	res, err := w.transport.Run(ctx, host, wrapPowerShellCommand(psAdd))
	if err != nil {
		return fmt.Errorf("windows git worktree add: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("windows git worktree add failed (exit %d): %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}

	if host.OwnerFixup {
		worktreesAdmin := joinWindowsPath(repoPath, `.git\worktrees`)
		if err := w.runOwnerFixup(ctx, host, []string{winWorkdir, worktreesAdmin}); err != nil {
			return err
		}
	}
	return nil
}

func (w *windowsLauncher) runOwnerFixup(ctx context.Context, host config.HostConfig, targets []string) error {
	cmd := buildOwnerFixupCommand(host, targets)
	res, err := w.transport.Run(ctx, host, cmd)
	if err != nil {
		return fmt.Errorf("windows owner fixup: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("windows owner fixup failed (exit %d): %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}

func buildOwnerFixupCommand(host config.HostConfig, targets []string) string {
	var ownerExpr string
	if owner := strings.TrimSpace(host.OwnerFixupUser); owner != "" {
		ownerExpr = psExpr(owner)
	} else {
		ownerExpr = `$env:COMPUTERNAME + '\' + $env:USERNAME`
	}

	stmts := make([]string, 0, len(targets)+1)
	stmts = append(stmts, "$owner = "+ownerExpr)
	for _, target := range targets {
		qp := psExpr(normalizeWindowsPath(target))
		stmts = append(stmts, fmt.Sprintf(
			"if (Test-Path -LiteralPath %s) { & icacls %s '/setowner' $owner '/T' '/C' '/Q'; if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE } }",
			qp, qp,
		))
	}
	return wrapPowerShellCommand(strings.Join(stmts, "; "))
}

func (w *windowsLauncher) Launch(ctx context.Context, host config.HostConfig, job *store.Job, workdir string, spec LaunchSpec) (LaunchInfo, error) {
	winWorkdir := normalizeWindowsPath(workdir)
	winRunDir, err := windowsRunDir(winWorkdir, job)
	if err != nil {
		return LaunchInfo{}, err
	}
	promptPath := joinWindowsPath(winRunDir, promptFileName)
	runCmdPath := joinWindowsPath(winRunDir, winRunScriptName)
	pidPath := joinWindowsPath(winRunDir, pidFileName)
	exitPath := joinWindowsPath(winRunDir, exitFileName)
	exitTmpPath := joinWindowsPath(winRunDir, exitTmpFileName)
	logPath := joinWindowsPath(winRunDir, logFileName)
	errLogPath := joinWindowsPath(winRunDir, errLogFileName)
	legacyLaunchPath := joinWindowsPath(winRunDir, legacyLaunchPS1)

	promptText := BuildPromptText(job)
	_, isResume := spec.Driver.selectLaunchTemplate(job)
	launchCmd, err := spec.Driver.BuildWindowsLaunchCommand(job, winWorkdir, promptPath, logPath, errLogPath, spec.AllowSandbox)
	if err != nil {
		return LaunchInfo{}, err
	}
	runScript := BuildWindowsRunScript(winWorkdir, winRunDir, spec.Driver, launchCmd)

	promptB64 := base64.StdEncoding.EncodeToString([]byte(promptText))
	scriptB64 := base64.StdEncoding.EncodeToString([]byte(runScript))

	// Stage prompt.txt (raw UTF-8 without BOM) and agent-run.cmd (CRLF) in the per-job runDir
	// via Base64 to avoid PowerShell 5.1 encoding / BOM issues and remove stale launcher or status files first.
	stagePS := fmt.Sprintf(
		"New-Item -ItemType Directory -Force -Path %s | Out-Null; "+
			"Remove-Item -Force -ErrorAction SilentlyContinue %s,%s,%s,%s,%s; "+
			"[System.IO.File]::WriteAllBytes(%s, [System.Convert]::FromBase64String(%s)); "+
			"[System.IO.File]::WriteAllBytes(%s, [System.Convert]::FromBase64String(%s))",
		psExpr(winRunDir),
		psExpr(legacyLaunchPath),
		psExpr(exitPath),
		psExpr(exitTmpPath),
		psExpr(pidPath),
		psExpr(runCmdPath),
		psExpr(promptPath), psExpr(promptB64),
		psExpr(runCmdPath), psExpr(scriptB64),
	)
	stageRes, err := w.transport.Run(ctx, host, wrapPowerShellCommand(stagePS))
	if err != nil {
		return LaunchInfo{}, fmt.Errorf("stage windows prompt.txt and agent-run.cmd: %w", err)
	}
	if stageRes.ExitCode != 0 {
		return LaunchInfo{}, fmt.Errorf("stage windows prompt.txt and agent-run.cmd failed (exit %d): %s", stageRes.ExitCode, strings.TrimSpace(stageRes.Stderr))
	}

	// Detach via WMI Win32_Process.Create (never Start-Process, which dies on SSH exit).
	wmiPS := fmt.Sprintf(
		"$r = Invoke-WmiMethod -Class Win32_Process -Name Create -ArgumentList ('cmd /c ' + [char]34 + %s + [char]34); "+
			"if ($null -eq $r -or $r.ReturnValue -ne 0 -or -not $r.ProcessId) { "+
			"[Console]::Error.WriteLine('WMI Win32_Process.Create failed: ReturnValue=' + $r.ReturnValue); exit 1 }; "+
			"[System.IO.File]::WriteAllText(%s, ([string]$r.ProcessId + [Environment]::NewLine), (New-Object System.Text.UTF8Encoding($false))); "+
			"Write-Output $r.ProcessId",
		psExpr(runCmdPath),
		psExpr(pidPath),
	)
	launchRes, err := w.transport.Run(ctx, host, wrapPowerShellCommand(wmiPS))
	if err != nil {
		return LaunchInfo{}, fmt.Errorf("windows WMI launch: %w", err)
	}
	if launchRes.ExitCode != 0 {
		return LaunchInfo{}, fmt.Errorf("windows WMI launch failed (exit %d): %s", launchRes.ExitCode, strings.TrimSpace(launchRes.Stderr))
	}

	pidStr := strings.TrimSpace(launchRes.Stdout)
	pid, err := strconv.Atoi(pidStr)
	if err != nil || pid <= 0 {
		return LaunchInfo{}, fmt.Errorf("invalid pid returned by windows WMI launcher %q: %w", pidStr, err)
	}

	info := LaunchInfo{
		PID: pid,
		Meta: map[string]string{
			"adapter":         "ssh",
			"os":              "windows",
			"launcher":        "wmi",
			"agent":           job.Agent,
			"host":            job.Host,
			"workdir":         winWorkdir,
			"run_dir":         winRunDir,
			"sandbox_relaxed": strconv.FormatBool(spec.AllowSandbox),
		},
	}
	if isResume {
		info.SessionID = strings.TrimSpace(job.SessionID)
		info.Meta["resumed"] = "true"
	}
	return info, nil
}

func (w *windowsLauncher) Status(ctx context.Context, host config.HostConfig, job *store.Job, workdir string) (RunState, error) {
	winWorkdir := normalizeWindowsPath(workdir)
	winRunDir, err := windowsRunDir(winWorkdir, job)
	if err != nil {
		return RunState{}, err
	}
	exitPath := joinWindowsPath(winRunDir, exitFileName)
	pidPath := joinWindowsPath(winRunDir, pidFileName)
	logPath := joinWindowsPath(winRunDir, logFileName)
	errLogPath := joinWindowsPath(winRunDir, errLogFileName)

	probePS := fmt.Sprintf(
		"$exitFile = %s; $pidFile = %s; $logFile = %s; $errFile = %s; "+
			"if (Test-Path -LiteralPath $exitFile) { "+
			"$raw = ([System.IO.File]::ReadAllText($exitFile, [System.Text.Encoding]::UTF8)).Trim(); "+
			"Write-Output ('EXITED:' + $raw); exit 0 }; "+
			"if (Test-Path -LiteralPath $pidFile) { "+
			"$rawPid = ([System.IO.File]::ReadAllText($pidFile, [System.Text.Encoding]::UTF8)).Trim(); "+
			"$pidNum = 0; "+
			"if ([int]::TryParse($rawPid, [ref]$pidNum) -and $pidNum -gt 0) { "+
			"$proc = Get-Process -Id $pidNum -ErrorAction SilentlyContinue; "+
			"if ($null -ne $proc) { Write-Output ('RUNNING:' + $pidNum); exit 0 }; "+
			"$hasLog = ((Test-Path -LiteralPath $logFile) -and ((Get-Item -LiteralPath $logFile).Length -gt 0)) -or "+
			"((Test-Path -LiteralPath $errFile) -and ((Get-Item -LiteralPath $errFile).Length -gt 0)); "+
			"if ($hasLog) { "+
			"if (Test-Path -LiteralPath $logFile) { "+
			"$tail = @(Get-Content -LiteralPath $logFile -Tail 5 -ErrorAction SilentlyContinue); "+
			"for ($i = $tail.Count - 1; $i -ge 0; $i--) { "+
			"if ($tail[$i] -match '^EXIT:(-?\\d+)\\s*$') { Write-Output ('EXITED:' + $Matches[1]); exit 0 } } }; "+
			"Write-Output ('EXITED_UNKNOWN:pid_exited_no_exit_file:' + $pidNum); exit 0 }; "+
			"Write-Output ('MISSING:pid_dead_no_logs:' + $pidNum); exit 0 }; "+
			"Write-Output ('MISSING:invalid_pid:' + $rawPid); exit 0 }; "+
			"Write-Output 'MISSING:no_files'",
		psExpr(exitPath),
		psExpr(pidPath),
		psExpr(logPath),
		psExpr(errLogPath),
	)

	res, err := w.transport.Run(ctx, host, wrapPowerShellCommand(probePS))
	if err != nil {
		return RunState{}, fmt.Errorf("windows status probe: %w", err)
	}
	if res.ExitCode != 0 {
		return RunState{}, fmt.Errorf("windows status probe failed (exit %d): %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}

	line := strings.TrimSpace(res.Stdout)
	switch {
	case strings.HasPrefix(line, "EXITED:"):
		rawCode := strings.TrimPrefix(line, "EXITED:")
		code, err := parseExitCode(rawCode)
		if err != nil {
			return RunState{
				State:  StateMissing,
				Detail: fmt.Sprintf("invalid_exit_file:%q", rawCode),
			}, nil
		}
		return RunState{
			State:           StateExited,
			ExitCode:        code,
			ExitCodeUnknown: false,
			Detail:          fmt.Sprintf("agent.exit=%d", code),
		}, nil
	case strings.HasPrefix(line, "EXITED_UNKNOWN:"):
		detail := strings.TrimPrefix(line, "EXITED_UNKNOWN:")
		return RunState{
			State:           StateExited,
			ExitCode:        0,
			ExitCodeUnknown: true,
			Detail:          detail,
		}, nil
	case strings.HasPrefix(line, "RUNNING:"):
		pid := strings.TrimPrefix(line, "RUNNING:")
		return RunState{
			State:  StateRunning,
			Detail: "pid=" + pid,
		}, nil
	case strings.HasPrefix(line, "MISSING:"):
		detail := strings.TrimPrefix(line, "MISSING:")
		return RunState{
			State:  StateMissing,
			Detail: detail,
		}, nil
	default:
		return RunState{}, fmt.Errorf("unexpected windows status probe output: %q", line)
	}
}

func (w *windowsLauncher) CollectEvidence(ctx context.Context, host config.HostConfig, job *store.Job, workdir string) (Evidence, error) {
	winWorkdir := normalizeWindowsPath(workdir)
	winRunDir, err := windowsRunDir(winWorkdir, job)
	if err != nil {
		return Evidence{}, err
	}
	ev := Evidence{
		Artifacts:      make(map[string]bool, len(w.artifactFiles)),
		CommandOutputs: make(map[string]string),
	}

	// 1. Collect git status --porcelain and git diff --stat when workdir is a git repo/worktree.
	gitStatusPS := fmt.Sprintf(
		"[Console]::OutputEncoding = [System.Text.Encoding]::UTF8; & git -C %s status --porcelain; exit $LASTEXITCODE",
		psExpr(winWorkdir),
	)
	gitRes, err := w.transport.Run(ctx, host, wrapPowerShellCommand(gitStatusPS))
	if err != nil {
		return Evidence{}, fmt.Errorf("collect windows git status: %w", err)
	}
	if gitRes.ExitCode == 0 {
		diffLines := filterPorcelainLines(gitRes.Stdout)
		var statusSummary string
		if len(diffLines) > 0 {
			ev.HasDiff = true
			statusSummary = strings.Join(diffLines, "\n")
			ev.CommandOutputs["git_status"] = statusSummary
		}

		gitDiffPS := fmt.Sprintf(
			"[Console]::OutputEncoding = [System.Text.Encoding]::UTF8; & git -C %s diff --stat; exit $LASTEXITCODE",
			psExpr(winWorkdir),
		)
		diffRes, err := w.transport.Run(ctx, host, wrapPowerShellCommand(gitDiffPS))
		if err != nil {
			return Evidence{}, fmt.Errorf("collect windows git diff --stat: %w", err)
		}
		var diffStat string
		if diffRes.ExitCode == 0 {
			diffStat = strings.TrimSpace(strings.ReplaceAll(diffRes.Stdout, "\r\n", "\n"))
			if diffStat != "" {
				ev.HasDiff = true
				ev.CommandOutputs["git_diff_stat"] = diffStat
			}
		}

		switch {
		case statusSummary != "" && diffStat != "":
			ev.DiffSummary = statusSummary + "\n\n" + diffStat
		case statusSummary != "":
			ev.DiffSummary = statusSummary
		case diffStat != "":
			ev.DiffSummary = diffStat
		}
	}

	// 2. Check artifact files existence (run artifacts in winRunDir, deliverables in winWorkdir).
	if len(w.artifactFiles) > 0 {
		checks := make([]string, 0, len(w.artifactFiles)+1)
		checks = append(checks, "[Console]::OutputEncoding = [System.Text.Encoding]::UTF8")
		for _, name := range w.artifactFiles {
			base := winWorkdir
			if isRunArtifactFile(name) {
				base = winRunDir
			}
			full := joinWindowsPath(base, name)
			checks = append(checks, fmt.Sprintf(
				"if (Test-Path -LiteralPath %s) { Write-Output %s } else { Write-Output %s }",
				psExpr(full), psExpr("1:"+name), psExpr("0:"+name),
			))
		}
		artRes, err := w.transport.Run(ctx, host, wrapPowerShellCommand(strings.Join(checks, "; ")))
		if err != nil {
			return Evidence{}, fmt.Errorf("collect windows artifacts: %w", err)
		}
		for _, line := range strings.Split(strings.ReplaceAll(strings.TrimSpace(artRes.Stdout), "\r\n", "\n"), "\n") {
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

	// 3. Collect tail of per-job agent.log.
	logPath := joinWindowsPath(winRunDir, logFileName)
	logPS := fmt.Sprintf(
		"[Console]::OutputEncoding = [System.Text.Encoding]::UTF8; "+
			"if (Test-Path -LiteralPath %s) { Get-Content -LiteralPath %s -Encoding UTF8 -Tail %d }",
		psExpr(logPath), psExpr(logPath), w.logTailLines,
	)
	logRes, err := w.transport.Run(ctx, host, wrapPowerShellCommand(logPS))
	if err != nil {
		return Evidence{}, fmt.Errorf("collect windows agent.log tail: %w", err)
	}
	logReport := trimLogTailUTF8(strings.TrimSpace(strings.ReplaceAll(logRes.Stdout, "\r\n", "\n")), w.logMaxBytes)

	// 4. Collect tail of per-job agent.err.log (critical on home-win Codex where final answer + tokens used appear here).
	errLogPath := joinWindowsPath(winRunDir, errLogFileName)
	errLogPS := fmt.Sprintf(
		"[Console]::OutputEncoding = [System.Text.Encoding]::UTF8; "+
			"if (Test-Path -LiteralPath %s) { Get-Content -LiteralPath %s -Encoding UTF8 -Tail %d }",
		psExpr(errLogPath), psExpr(errLogPath), w.logTailLines,
	)
	errLogRes, err := w.transport.Run(ctx, host, wrapPowerShellCommand(errLogPS))
	if err != nil {
		return Evidence{}, fmt.Errorf("collect windows agent.err.log tail: %w", err)
	}
	errReport := trimLogTailUTF8(strings.TrimSpace(strings.ReplaceAll(errLogRes.Stdout, "\r\n", "\n")), w.logMaxBytes)
	ev.ErrLogTail = errReport
	if logReport != "" {
		ev.FinalReport = logReport
	} else if errReport != "" {
		ev.FinalReport = errReport
	}

	// 5. Read per-job agent.exit (or fallback to trailing EXIT:<code> in agent.log); mark unknown if absent.
	exitPath := joinWindowsPath(winRunDir, exitFileName)
	exitPS := fmt.Sprintf(
		"[Console]::OutputEncoding = [System.Text.Encoding]::UTF8; "+
			"if (Test-Path -LiteralPath %s) { ([System.IO.File]::ReadAllText(%s, [System.Text.Encoding]::UTF8)).Trim() }",
		psExpr(exitPath), psExpr(exitPath),
	)
	exitRes, err := w.transport.Run(ctx, host, wrapPowerShellCommand(exitPS))
	if err != nil {
		return Evidence{}, fmt.Errorf("collect windows agent.exit: %w", err)
	}
	if raw := strings.TrimSpace(exitRes.Stdout); raw != "" {
		if code, perr := parseExitCode(raw); perr == nil {
			ev.ExitCode = code
			ev.ExitCodeUnknown = false
		} else {
			ev.ExitCodeUnknown = true
			ev.Notes = append(ev.Notes, fmt.Sprintf("exit code unknown: unparseable agent.exit (%q)", raw))
		}
	} else if code, ok := extractTrailingExitLine(logReport); ok {
		ev.ExitCode = code
		ev.ExitCodeUnknown = false
	} else {
		ev.ExitCode = 0
		ev.ExitCodeUnknown = true
		ev.Notes = append(ev.Notes, "exit code unknown: agent.exit not recorded (e.g., batch wrapper such as codex.cmd does not return control; verify completion via agent.err.log, artifacts, and git status)")
	}

	if len(ev.CommandOutputs) == 0 {
		ev.CommandOutputs = nil
	}
	return ev, nil
}

// BuildWindowsRunScript generates the CRLF batch wrapper (agent-run.cmd) executed via WMI.
// It applies per-host env vars, enters workdir, invokes the agent command, and records
// runDir\agent.exit + EXIT:<code> (in runDir\agent.log) whenever control returns to the wrapper.
// If runDir is empty, it defaults to workdir.
// The final output is normalized to strict CRLF regardless of any line endings in launchCmd.
//
// Do NOT emit `setlocal` here: field experiments on home-win established that when a
// WMI-launched batch script contains `setlocal`, child processes end up with
// C:\Windows\System32 as their working directory instead of the `cd /d` target.
// Because each invocation runs in a dedicated WMI cmd.exe process, plain `set` lines
// are already isolated to that process without `setlocal`.
func BuildWindowsRunScript(workdir, runDir string, driver AgentDriver, launchCmd string) string {
	winWorkdir := normalizeWindowsPath(workdir)
	winRunDir := normalizeWindowsPath(runDir)
	if winRunDir == "" {
		winRunDir = winWorkdir
	}
	exitPath := joinWindowsPath(winRunDir, exitFileName)
	exitTmpPath := joinWindowsPath(winRunDir, exitTmpFileName)
	logPath := joinWindowsPath(winRunDir, logFileName)

	var lines []string
	lines = append(lines, "@echo off")
	lines = append(lines, formatCmdEnvSets(driver.Env)...)
	lines = append(lines, fmt.Sprintf("cd /d %s || exit /b 1", cmdQuoteAlways(winWorkdir)))
	lines = append(lines, fmt.Sprintf(`del /f /q %s %s 2>nul`, cmdQuoteAlways(exitPath), cmdQuoteAlways(exitTmpPath)))
	lines = append(lines, launchCmd)
	lines = append(lines, `set "QF_RC=%ERRORLEVEL%"`)
	lines = append(lines, fmt.Sprintf(`> %s echo %%QF_RC%%`, cmdQuoteAlways(exitTmpPath)))
	lines = append(lines, fmt.Sprintf(`move /y %s %s >nul`, cmdQuoteAlways(exitTmpPath), cmdQuoteAlways(exitPath)))
	lines = append(lines, fmt.Sprintf(`>> %s echo EXIT:%%QF_RC%%`, cmdQuoteAlways(logPath)))
	lines = append(lines, `exit /b %QF_RC%`)
	return normalizeCRLF(strings.Join(lines, "\n") + "\n")
}

// normalizeCRLF converts all line endings (CRLF, bare LF, bare CR, and repeated CR before LF)
// to strict CRLF without introducing double \r\r\n sequences.
func normalizeCRLF(s string) string {
	for strings.Contains(s, "\r\n") {
		s = strings.ReplaceAll(s, "\r\n", "\n")
	}
	s = strings.ReplaceAll(s, "\r", "\n")
	return strings.ReplaceAll(s, "\n", "\r\n")
}

func isWindowsHost(host config.HostConfig) bool {
	return strings.EqualFold(strings.TrimSpace(host.OS), "windows")
}

func normalizeWindowsPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	p = strings.ReplaceAll(p, "/", `\`)
	isUNC := strings.HasPrefix(p, `\\`)
	for strings.Contains(p, `\\`) {
		if isUNC {
			p = `\\` + strings.ReplaceAll(p[2:], `\\`, `\`)
			if !strings.Contains(p[2:], `\\`) {
				break
			}
		} else {
			p = strings.ReplaceAll(p, `\\`, `\`)
		}
	}
	// Preserve drive root trailing backslash (e.g. C:\), trim trailing backslash elsewhere.
	if len(p) > 3 || (len(p) == 3 && !isASCIILetter(p[0])) {
		p = strings.TrimRight(p, `\`)
	}
	return p
}

func joinWindowsPath(base, elem string) string {
	base = normalizeWindowsPath(base)
	elem = strings.TrimLeft(strings.ReplaceAll(strings.TrimSpace(elem), "/", `\`), `\`)
	if base == "" {
		return elem
	}
	if elem == "" {
		return base
	}
	if strings.HasSuffix(base, `\`) {
		return base + elem
	}
	return base + `\` + elem
}

func windowsDir(p string) string {
	norm := normalizeWindowsPath(p)
	if norm == "" {
		return "."
	}
	if len(norm) == 3 && isASCIILetter(norm[0]) && norm[1] == ':' && norm[2] == '\\' {
		return norm
	}
	idx := strings.LastIndex(norm, `\`)
	if idx < 0 {
		return "."
	}
	if idx == 2 && isASCIILetter(norm[0]) && norm[1] == ':' {
		return norm[:3]
	}
	if idx == 0 {
		return `\`
	}
	return norm[:idx]
}

// psExpr produces a PowerShell string expression for s that contains no double-quote characters,
// making it safe to embed inside `powershell -NoProfile -NonInteractive -Command "..."`.
func psExpr(s string) string {
	if !strings.Contains(s, `"`) {
		return "'" + strings.ReplaceAll(s, "'", "''") + "'"
	}
	parts := strings.Split(s, `"`)
	quoted := make([]string, 0, len(parts))
	for _, part := range parts {
		quoted = append(quoted, "'"+strings.ReplaceAll(part, "'", "''")+"'")
	}
	return "(" + strings.Join(quoted, " + [char]34 + ") + ")"
}

func wrapPowerShellCommand(script string) string {
	return fmt.Sprintf(`powershell -NoProfile -NonInteractive -Command "%s"`, script)
}

func formatPowerShellEnvAssigns(env map[string]string) []string {
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
		lines = append(lines, fmt.Sprintf("$env:%s = %s", k, psExpr(env[k])))
	}
	return lines
}

func formatCmdEnvSets(env map[string]string) []string {
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
		val := strings.ReplaceAll(env[k], "\r", "")
		val = strings.ReplaceAll(val, "\n", "")
		val = strings.ReplaceAll(val, `"`, "")
		lines = append(lines, fmt.Sprintf(`set "%s=%s"`, k, val))
	}
	return lines
}

func formatWindowsCommandLine(tokens []string) string {
	out := make([]string, 0, len(tokens))
	for _, tok := range tokens {
		out = append(out, cmdQuoteArg(tok))
	}
	return strings.Join(out, " ")
}

func cmdQuoteArg(s string) string {
	s = strings.ReplaceAll(s, "\r", "")
	s = strings.ReplaceAll(s, "\n", "")
	if isSafeWindowsBareToken(s) {
		return s
	}
	return cmdQuoteAlways(s)
}

func cmdQuoteAlways(s string) string {
	s = strings.ReplaceAll(s, "\r", "")
	s = strings.ReplaceAll(s, "\n", "")
	s = strings.ReplaceAll(s, `"`, `""`)
	return `"` + s + `"`
}

func isSafeWindowsBareToken(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := (c >= 'a' && c <= 'z') ||
			(c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') ||
			c == '_' || c == '-' || c == '.' || c == '/' || c == '\\' || c == ':'
		if !ok {
			return false
		}
	}
	return true
}

func extractTrailingExitLine(logText string) (int, bool) {
	if strings.TrimSpace(logText) == "" {
		return 0, false
	}
	lines := strings.Split(strings.ReplaceAll(logText, "\r\n", "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "EXIT:") {
			if code, err := parseExitCode(line); err == nil {
				return code, true
			}
		}
		break
	}
	return 0, false
}

func trimLogTailUTF8(s string, maxBytes int) string {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s
	}
	start := len(s) - maxBytes
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return s[start:]
}

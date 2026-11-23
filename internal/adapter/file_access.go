package adapter

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

var (
	// ErrEmptyPath is returned when a FileAccess path argument is empty.
	ErrEmptyPath = errors.New("empty path")
	// ErrEmptyWorkdir is returned when a relative path is requested against an empty workdir.
	ErrEmptyWorkdir = errors.New("job workdir is empty")
)

const (
	remoteExitNotFound = 44
	remoteExitIsDir    = 21
)

// FileStat holds basic filesystem metadata for a path on a job's target host.
type FileStat struct {
	Exists bool
	IsDir  bool
	Size   int64
}

// ExecResult captures the outcome of running a shell command in a job's workdir.
type ExecResult struct {
	ExitCode int
	Output   string
}

// FileAccess abstracts host-aware file inspection, file reading, and command execution
// scoped to a job's workdir on its target host (local filesystem or remote via Transport).
type FileAccess interface {
	// Stat inspects relOrAbsPath, resolving relative paths against the job's workdir
	// using the target host's OS path conventions. When the path does not exist,
	// Stat returns FileStat{Exists: false}, nil.
	Stat(ctx context.Context, relOrAbsPath string) (FileStat, error)

	// ReadFile reads the contents of relOrAbsPath, resolving relative paths against
	// the job's workdir. If the path does not exist, it returns an error wrapping os.ErrNotExist.
	ReadFile(ctx context.Context, relOrAbsPath string) ([]byte, error)

	// RunCommand executes command with the job's workdir as current working directory.
	// Non-zero command exit codes are returned via ExecResult.ExitCode with a nil error.
	// A non-nil error indicates timeout, cancellation, transport failure, or invocation failure.
	RunCommand(ctx context.Context, command string) (ExecResult, error)
}

// FileAccessProvider is an optional interface implemented by adapters that can construct
// a host-appropriate FileAccess for a job.
type FileAccessProvider interface {
	FileAccessFor(job *store.Job) (FileAccess, error)
}

// LocalFileAccess implements FileAccess directly against the serve host's local filesystem and shell.
type LocalFileAccess struct {
	workdir   string
	denyPaths []string
}

// NewLocalFileAccess creates a LocalFileAccess scoped to workdir on the serve host.
func NewLocalFileAccess(workdir string) *LocalFileAccess {
	return &LocalFileAccess{workdir: strings.TrimSpace(workdir)}
}

func newLocalFileAccess(workdir string, denyPaths []string) *LocalFileAccess {
	return &LocalFileAccess{
		workdir:   strings.TrimSpace(workdir),
		denyPaths: append([]string(nil), denyPaths...),
	}
}

func (l *LocalFileAccess) resolvePath(rawPath string) (string, error) {
	p := strings.TrimSpace(rawPath)
	if p == "" {
		return "", ErrEmptyPath
	}
	var full string
	if filepath.IsAbs(p) {
		full = p
	} else {
		wd := strings.TrimSpace(l.workdir)
		if wd == "" {
			return "", ErrEmptyWorkdir
		}
		full = filepath.Join(wd, p)
	}
	if len(l.denyPaths) > 0 && isPathDenied(full, l.denyPaths) {
		return "", fmt.Errorf("%w: %s", ErrDeniedPath, p)
	}
	return full, nil
}

// Stat inspects relOrAbsPath on the local filesystem.
func (l *LocalFileAccess) Stat(ctx context.Context, relOrAbsPath string) (FileStat, error) {
	if err := ctx.Err(); err != nil {
		return FileStat{}, err
	}
	fullPath, err := l.resolvePath(relOrAbsPath)
	if err != nil {
		return FileStat{}, err
	}
	info, err := os.Stat(fullPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return FileStat{Exists: false}, nil
		}
		return FileStat{}, err
	}
	return FileStat{
		Exists: true,
		IsDir:  info.IsDir(),
		Size:   info.Size(),
	}, nil
}

// ReadFile reads relOrAbsPath from the local filesystem.
func (l *LocalFileAccess) ReadFile(ctx context.Context, relOrAbsPath string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	fullPath, err := l.resolvePath(relOrAbsPath)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(fullPath)
}

// RunCommand executes command locally via `sh -c` in workdir, cleaning up the process group on cancellation.
func (l *LocalFileAccess) RunCommand(ctx context.Context, command string) (ExecResult, error) {
	if err := ctx.Err(); err != nil {
		return ExecResult{}, err
	}

	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	if wd := strings.TrimSpace(l.workdir); wd != "" {
		cmd.Dir = wd
	}
	// Run in its own process group so timeouts clean up any child processes.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 2 * time.Second

	out, err := cmd.CombinedOutput()
	if err == nil {
		return ExecResult{ExitCode: 0, Output: string(out)}, nil
	}
	if ctx.Err() != nil {
		return ExecResult{Output: string(out)}, ctx.Err()
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return ExecResult{ExitCode: exitErr.ExitCode(), Output: string(out)}, nil
	}
	return ExecResult{Output: string(out)}, err
}

// NewRemoteFileAccess constructs a Transport-backed FileAccess matching host.OS (Linux or Windows).
func NewRemoteFileAccess(transport Transport, host config.HostConfig, workdir string) (FileAccess, error) {
	if transport == nil {
		return nil, errors.New("adapter: transport must not be nil")
	}
	switch strings.ToLower(strings.TrimSpace(host.OS)) {
	case "", "linux":
		return &linuxFileAccess{
			transport: transport,
			host:      host,
			workdir:   strings.TrimSpace(workdir),
		}, nil
	case "windows":
		return &windowsFileAccess{
			transport: transport,
			host:      host,
			workdir:   normalizeWindowsPath(workdir),
		}, nil
	default:
		return nil, fmt.Errorf("%w: unsupported host os %q", ErrUnsupportedOS, host.OS)
	}
}

// isLocalTarget reports whether commands for host execute on the serve host itself.
func isLocalTarget(tr Transport, host config.HostConfig) bool {
	if _, ok := tr.(*LocalTransport); ok {
		return true
	}
	mode := strings.ToLower(strings.TrimSpace(host.Transport))
	switch mode {
	case "local":
		return true
	case "ssh":
		return false
	case "":
		return strings.TrimSpace(host.SSH) == ""
	default:
		return false
	}
}

// --- Linux remote FileAccess (POSIX sh over Transport) ---

type linuxFileAccess struct {
	transport Transport
	host      config.HostConfig
	workdir   string
}

func (f *linuxFileAccess) resolvePath(rawPath string) (string, error) {
	p := strings.TrimSpace(rawPath)
	if p == "" {
		return "", ErrEmptyPath
	}
	var full string
	if isPOSIXAbs(p) {
		full = p
	} else {
		wd := strings.TrimSpace(f.workdir)
		if wd == "" {
			return "", ErrEmptyWorkdir
		}
		for strings.HasPrefix(p, "./") {
			p = strings.TrimPrefix(p, "./")
		}
		if p == "" || p == "." {
			full = wd
		} else {
			full = joinRemotePath(wd, p)
		}
	}
	if len(f.host.DenyPaths) > 0 && isPathDenied(full, f.host.DenyPaths) {
		return "", fmt.Errorf("%w: %s", ErrDeniedPath, p)
	}
	return full, nil
}

func (f *linuxFileAccess) Stat(ctx context.Context, relOrAbsPath string) (FileStat, error) {
	if err := ctx.Err(); err != nil {
		return FileStat{}, err
	}
	fullPath, err := f.resolvePath(relOrAbsPath)
	if err != nil {
		return FileStat{}, err
	}
	cmd := fmt.Sprintf(
		`p=%s; if [ -d "$p" ]; then printf 'DIR\n'; elif [ -e "$p" ]; then sz=$(stat -L -c '%%s' "$p" 2>/dev/null || wc -c < "$p"); printf 'FILE:%%s\n' "$(printf '%%s' "$sz" | tr -d ' \t\r\n')"; else printf 'MISSING\n'; fi`,
		shellPath(fullPath),
	)
	res, err := f.transport.Run(ctx, f.host, cmd)
	if err != nil {
		return FileStat{}, err
	}
	if res.ExitCode != 0 {
		detail := strings.TrimSpace(res.Stderr)
		if detail == "" {
			detail = strings.TrimSpace(res.Stdout)
		}
		return FileStat{}, fmt.Errorf("remote stat failed (exit %d): %s", res.ExitCode, detail)
	}
	return parseStatOutput(res.Stdout)
}

func (f *linuxFileAccess) ReadFile(ctx context.Context, relOrAbsPath string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	fullPath, err := f.resolvePath(relOrAbsPath)
	if err != nil {
		return nil, err
	}
	cmd := fmt.Sprintf(
		`p=%s; if [ ! -e "$p" ]; then exit %d; fi; if [ -d "$p" ]; then printf 'is a directory\n' >&2; exit %d; fi; cat -- "$p"`,
		shellPath(fullPath), remoteExitNotFound, remoteExitIsDir,
	)
	res, err := f.transport.Run(ctx, f.host, cmd)
	if err != nil {
		return nil, err
	}
	switch res.ExitCode {
	case 0:
		return []byte(res.Stdout), nil
	case remoteExitNotFound:
		return nil, fmt.Errorf("%w: %s", os.ErrNotExist, strings.TrimSpace(relOrAbsPath))
	case remoteExitIsDir:
		return nil, fmt.Errorf("read %s: is a directory", strings.TrimSpace(relOrAbsPath))
	default:
		detail := strings.TrimSpace(res.Stderr)
		if detail == "" {
			detail = strings.TrimSpace(res.Stdout)
		}
		return nil, fmt.Errorf("remote read failed (exit %d): %s", res.ExitCode, detail)
	}
}

func (f *linuxFileAccess) RunCommand(ctx context.Context, command string) (ExecResult, error) {
	if err := ctx.Err(); err != nil {
		return ExecResult{}, err
	}
	var shellCmd string
	if wd := strings.TrimSpace(f.workdir); wd != "" {
		shellCmd = fmt.Sprintf("cd %s && sh -c %s", shellPath(wd), shellQuote(command))
	} else {
		shellCmd = fmt.Sprintf("sh -c %s", shellQuote(command))
	}
	res, err := f.transport.Run(ctx, f.host, shellCmd)
	combined := combineOutputs(res.Stdout, res.Stderr)
	if err != nil {
		return ExecResult{Output: combined}, err
	}
	return ExecResult{ExitCode: res.ExitCode, Output: combined}, nil
}

// --- Windows remote FileAccess (PowerShell over Transport) ---

type windowsFileAccess struct {
	transport Transport
	host      config.HostConfig
	workdir   string
}

func (f *windowsFileAccess) resolvePath(rawPath string) (string, error) {
	p := strings.TrimSpace(rawPath)
	if p == "" {
		return "", ErrEmptyPath
	}
	var full string
	if isWindowsAbs(p) {
		full = normalizeWindowsPath(p)
	} else {
		wd := normalizeWindowsPath(f.workdir)
		if wd == "" {
			return "", ErrEmptyWorkdir
		}
		rel := strings.ReplaceAll(p, "/", `\`)
		for strings.HasPrefix(rel, `.\`) {
			rel = strings.TrimPrefix(rel, `.\`)
		}
		if rel == "" || rel == "." {
			full = wd
		} else {
			full = joinWindowsPath(wd, rel)
		}
	}
	if len(f.host.DenyPaths) > 0 && isPathDenied(full, f.host.DenyPaths) {
		return "", fmt.Errorf("%w: %s", ErrDeniedPath, p)
	}
	return full, nil
}

func (f *windowsFileAccess) Stat(ctx context.Context, relOrAbsPath string) (FileStat, error) {
	if err := ctx.Err(); err != nil {
		return FileStat{}, err
	}
	fullPath, err := f.resolvePath(relOrAbsPath)
	if err != nil {
		return FileStat{}, err
	}
	ps := fmt.Sprintf(
		"[Console]::OutputEncoding = [System.Text.Encoding]::UTF8; "+
			"$p = %s; "+
			"if (-not (Test-Path -LiteralPath $p)) { Write-Output 'MISSING'; exit 0 }; "+
			"$item = Get-Item -LiteralPath $p -Force; "+
			"if ($item.PSIsContainer) { Write-Output 'DIR' } else { Write-Output ('FILE:' + $item.Length) }",
		psSafeExpr(fullPath),
	)
	res, err := f.transport.Run(ctx, f.host, wrapPowerShellCommand(ps))
	if err != nil {
		return FileStat{}, err
	}
	if res.ExitCode != 0 {
		detail := strings.TrimSpace(res.Stderr)
		if detail == "" {
			detail = strings.TrimSpace(res.Stdout)
		}
		return FileStat{}, fmt.Errorf("remote windows stat failed (exit %d): %s", res.ExitCode, detail)
	}
	return parseStatOutput(res.Stdout)
}

func (f *windowsFileAccess) ReadFile(ctx context.Context, relOrAbsPath string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	fullPath, err := f.resolvePath(relOrAbsPath)
	if err != nil {
		return nil, err
	}
	ps := fmt.Sprintf(
		"[Console]::OutputEncoding = [System.Text.Encoding]::UTF8; "+
			"$p = %s; "+
			"if (-not (Test-Path -LiteralPath $p)) { exit %d }; "+
			"$item = Get-Item -LiteralPath $p -Force; "+
			"if ($item.PSIsContainer) { [Console]::Error.WriteLine('is a directory'); exit %d }; "+
			"[Console]::Write([System.IO.File]::ReadAllText($p, [System.Text.Encoding]::UTF8))",
		psSafeExpr(fullPath), remoteExitNotFound, remoteExitIsDir,
	)
	res, err := f.transport.Run(ctx, f.host, wrapPowerShellCommand(ps))
	if err != nil {
		return nil, err
	}
	switch res.ExitCode {
	case 0:
		return []byte(res.Stdout), nil
	case remoteExitNotFound:
		return nil, fmt.Errorf("%w: %s", os.ErrNotExist, strings.TrimSpace(relOrAbsPath))
	case remoteExitIsDir:
		return nil, fmt.Errorf("read %s: is a directory", strings.TrimSpace(relOrAbsPath))
	default:
		detail := strings.TrimSpace(res.Stderr)
		if detail == "" {
			detail = strings.TrimSpace(res.Stdout)
		}
		return nil, fmt.Errorf("remote windows read failed (exit %d): %s", res.ExitCode, detail)
	}
}

func (f *windowsFileAccess) RunCommand(ctx context.Context, command string) (ExecResult, error) {
	if err := ctx.Err(); err != nil {
		return ExecResult{}, err
	}
	var stmts []string
	stmts = append(stmts, "[Console]::OutputEncoding = [System.Text.Encoding]::UTF8")
	if wd := normalizeWindowsPath(f.workdir); wd != "" {
		stmts = append(stmts, fmt.Sprintf("Set-Location -LiteralPath %s -ErrorAction Stop", psSafeExpr(wd)))
	}
	stmts = append(stmts, "$global:LASTEXITCODE = 0")
	stmts = append(stmts, fmt.Sprintf("Invoke-Expression %s", psSafeExpr(command)))
	stmts = append(stmts, "if (-not $?) { if ($LASTEXITCODE -and $LASTEXITCODE -ne 0) { exit $LASTEXITCODE }; exit 1 }")
	stmts = append(stmts, "if ($LASTEXITCODE -and $LASTEXITCODE -ne 0) { exit $LASTEXITCODE }")

	res, err := f.transport.Run(ctx, f.host, wrapPowerShellCommand(strings.Join(stmts, "; ")))
	combined := combineOutputs(res.Stdout, res.Stderr)
	if err != nil {
		return ExecResult{Output: combined}, err
	}
	return ExecResult{ExitCode: res.ExitCode, Output: combined}, nil
}

// --- Shared helpers ---

func parseStatOutput(stdout string) (FileStat, error) {
	line := strings.TrimSpace(stdout)
	switch {
	case line == "MISSING":
		return FileStat{Exists: false}, nil
	case line == "DIR":
		return FileStat{Exists: true, IsDir: true}, nil
	case strings.HasPrefix(line, "FILE:"):
		rawSize := strings.TrimSpace(strings.TrimPrefix(line, "FILE:"))
		size, err := strconv.ParseInt(rawSize, 10, 64)
		if err != nil {
			return FileStat{}, fmt.Errorf("parse remote file size %q: %w", rawSize, err)
		}
		return FileStat{Exists: true, IsDir: false, Size: size}, nil
	default:
		return FileStat{}, fmt.Errorf("unexpected remote stat output: %q", line)
	}
}

func combineOutputs(stdout, stderr string) string {
	switch {
	case stdout == "":
		return stderr
	case stderr == "":
		return stdout
	case strings.HasSuffix(stdout, "\n"):
		return stdout + stderr
	default:
		return stdout + "\n" + stderr
	}
}

func isPOSIXAbs(p string) bool {
	return strings.HasPrefix(p, "/") || p == "~" || strings.HasPrefix(p, "~/")
}

func isWindowsAbs(p string) bool {
	if len(p) >= 3 && isASCIILetter(p[0]) && p[1] == ':' && (p[2] == '\\' || p[2] == '/') {
		return true
	}
	return strings.HasPrefix(p, `\\`) || strings.HasPrefix(p, "//")
}

// psSafeExpr encodes s into a PowerShell string expression free of double quotes and newlines,
// so it can be embedded safely inside `powershell -NoProfile -NonInteractive -Command "..."`.
func psSafeExpr(s string) string {
	if !strings.ContainsAny(s, "\"\r\n") {
		return "'" + strings.ReplaceAll(s, "'", "''") + "'"
	}
	var parts []string
	var buf strings.Builder
	flush := func() {
		if buf.Len() > 0 {
			parts = append(parts, "'"+strings.ReplaceAll(buf.String(), "'", "''")+"'")
			buf.Reset()
		}
	}
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"':
			flush()
			parts = append(parts, "[char]34")
		case '\r':
			flush()
			parts = append(parts, "[char]13")
		case '\n':
			flush()
			parts = append(parts, "[char]10")
		default:
			buf.WriteByte(s[i])
		}
	}
	flush()
	if len(parts) == 0 {
		return "''"
	}
	return "(" + strings.Join(parts, " + ") + ")"
}

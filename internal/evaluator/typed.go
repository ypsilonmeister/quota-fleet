package evaluator

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

const (
	prefixFileExists   = "file_exists:"
	prefixFileNonEmpty = "file_nonempty:"
	prefixContains     = "contains:"
	prefixCommand      = "command:"

	containsSeparator = " :: "

	// DefaultCommandTimeout is the per-command timeout for `command:` criteria.
	DefaultCommandTimeout = 60 * time.Second

	maxCommandOutputEvidenceBytes = 200
)

type typedKind string

const (
	typedKindFileExists   typedKind = "file_exists"
	typedKindFileNonEmpty typedKind = "file_nonempty"
	typedKindContains     typedKind = "contains"
	typedKindCommand      typedKind = "command"
)

type typedCriterion struct {
	kind typedKind
	raw  string
	rest string
}

// parseTypedCriterion inspects a single trimmed acceptance criterion line and
// classifies it as a deterministic typed check if it starts with a known prefix.
func parseTypedCriterion(line string) (typedCriterion, bool) {
	switch {
	case strings.HasPrefix(line, prefixFileExists):
		return typedCriterion{
			kind: typedKindFileExists,
			raw:  line,
			rest: strings.TrimPrefix(line, prefixFileExists),
		}, true
	case strings.HasPrefix(line, prefixFileNonEmpty):
		return typedCriterion{
			kind: typedKindFileNonEmpty,
			raw:  line,
			rest: strings.TrimPrefix(line, prefixFileNonEmpty),
		}, true
	case strings.HasPrefix(line, prefixContains):
		return typedCriterion{
			kind: typedKindContains,
			raw:  line,
			rest: strings.TrimPrefix(line, prefixContains),
		}, true
	case strings.HasPrefix(line, prefixCommand):
		return typedCriterion{
			kind: typedKindCommand,
			raw:  line,
			rest: strings.TrimPrefix(line, prefixCommand),
		}, true
	default:
		return typedCriterion{}, false
	}
}

func evaluateTypedCriterion(ctx context.Context, tc typedCriterion, workdir string, commandTimeout time.Duration) (CriterionResult, error) {
	if err := ctx.Err(); err != nil {
		return CriterionResult{}, err
	}

	switch tc.kind {
	case typedKindFileExists:
		return evalFileExists(tc, workdir), nil
	case typedKindFileNonEmpty:
		return evalFileNonEmpty(tc, workdir), nil
	case typedKindContains:
		return evalContains(tc, workdir), nil
	case typedKindCommand:
		return evalCommand(ctx, tc, workdir, commandTimeout)
	default:
		return CriterionResult{
			Criterion: tc.raw,
			Passed:    false,
			Evidence:  fmt.Sprintf("typed: unknown check kind %q", tc.kind),
		}, nil
	}
}

func resolveWorkdirPath(workdir, rawPath string) (relDisplay, fullPath string, err error) {
	p := strings.TrimSpace(rawPath)
	if p == "" {
		return "", "", errors.New("empty path")
	}
	if filepath.IsAbs(p) {
		return p, p, nil
	}
	wd := strings.TrimSpace(workdir)
	if wd == "" {
		return p, "", errors.New("job workdir is empty")
	}
	return p, filepath.Join(wd, p), nil
}

func evalFileExists(tc typedCriterion, workdir string) CriterionResult {
	display, fullPath, err := resolveWorkdirPath(workdir, tc.rest)
	if err != nil {
		return CriterionResult{
			Criterion: tc.raw,
			Passed:    false,
			Evidence:  fmt.Sprintf("typed file_exists: %v", err),
		}
	}
	info, err := os.Stat(fullPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return CriterionResult{
				Criterion: tc.raw,
				Passed:    false,
				Evidence:  fmt.Sprintf("typed file_exists: not found: %s", display),
			}
		}
		return CriterionResult{
			Criterion: tc.raw,
			Passed:    false,
			Evidence:  fmt.Sprintf("typed file_exists: stat error for %s: %v", display, err),
		}
	}
	if info.IsDir() {
		return CriterionResult{
			Criterion: tc.raw,
			Passed:    true,
			Evidence:  fmt.Sprintf("typed file_exists: found dir %s", display),
		}
	}
	return CriterionResult{
		Criterion: tc.raw,
		Passed:    true,
		Evidence:  fmt.Sprintf("typed file_exists: found file %s (%dB)", display, info.Size()),
	}
}

func evalFileNonEmpty(tc typedCriterion, workdir string) CriterionResult {
	display, fullPath, err := resolveWorkdirPath(workdir, tc.rest)
	if err != nil {
		return CriterionResult{
			Criterion: tc.raw,
			Passed:    false,
			Evidence:  fmt.Sprintf("typed file_nonempty: %v", err),
		}
	}
	info, err := os.Stat(fullPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return CriterionResult{
				Criterion: tc.raw,
				Passed:    false,
				Evidence:  fmt.Sprintf("typed file_nonempty: not found: %s", display),
			}
		}
		return CriterionResult{
			Criterion: tc.raw,
			Passed:    false,
			Evidence:  fmt.Sprintf("typed file_nonempty: stat error for %s: %v", display, err),
		}
	}
	if info.IsDir() {
		return CriterionResult{
			Criterion: tc.raw,
			Passed:    false,
			Evidence:  fmt.Sprintf("typed file_nonempty: %s is a directory", display),
		}
	}
	if info.Size() <= 0 {
		return CriterionResult{
			Criterion: tc.raw,
			Passed:    false,
			Evidence:  fmt.Sprintf("typed file_nonempty: %s is empty (0B)", display),
		}
	}
	return CriterionResult{
		Criterion: tc.raw,
		Passed:    true,
		Evidence:  fmt.Sprintf("typed file_nonempty: %s (%dB)", display, info.Size()),
	}
}

func evalContains(tc typedCriterion, workdir string) CriterionResult {
	parts := strings.SplitN(tc.rest, containsSeparator, 2)
	if len(parts) != 2 {
		return CriterionResult{
			Criterion: tc.raw,
			Passed:    false,
			Evidence:  "typed contains: invalid syntax (expected '<path> :: <text>')",
		}
	}
	rawPath := parts[0]
	needle := parts[1]
	if needle == "" {
		return CriterionResult{
			Criterion: tc.raw,
			Passed:    false,
			Evidence:  "typed contains: empty search text",
		}
	}

	display, fullPath, err := resolveWorkdirPath(workdir, rawPath)
	if err != nil {
		return CriterionResult{
			Criterion: tc.raw,
			Passed:    false,
			Evidence:  fmt.Sprintf("typed contains: %v", err),
		}
	}
	data, err := os.ReadFile(fullPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return CriterionResult{
				Criterion: tc.raw,
				Passed:    false,
				Evidence:  fmt.Sprintf("typed contains: not found: %s", display),
			}
		}
		return CriterionResult{
			Criterion: tc.raw,
			Passed:    false,
			Evidence:  fmt.Sprintf("typed contains: read error for %s: %v", display, err),
		}
	}
	if strings.Contains(string(data), needle) {
		return CriterionResult{
			Criterion: tc.raw,
			Passed:    true,
			Evidence:  fmt.Sprintf("typed contains: matched in %s", display),
		}
	}
	return CriterionResult{
		Criterion: tc.raw,
		Passed:    false,
		Evidence:  fmt.Sprintf("typed contains: text not found in %s", display),
	}
}

func evalCommand(ctx context.Context, tc typedCriterion, workdir string, commandTimeout time.Duration) (CriterionResult, error) {
	cmdStr := strings.TrimSpace(tc.rest)
	if cmdStr == "" {
		return CriterionResult{
			Criterion: tc.raw,
			Passed:    false,
			Evidence:  "typed command: empty command",
		}, nil
	}
	if commandTimeout <= 0 {
		commandTimeout = DefaultCommandTimeout
	}

	cmdCtx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, "sh", "-c", cmdStr)
	if wd := strings.TrimSpace(workdir); wd != "" {
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
		return CriterionResult{
			Criterion: tc.raw,
			Passed:    true,
			Evidence:  "typed command: exit 0",
		}, nil
	}

	// If parent context was cancelled, propagate cancellation rather than converting to FAIL.
	if ctx.Err() != nil {
		return CriterionResult{}, ctx.Err()
	}

	// Command-level timeout is treated as FAIL, not an evaluation error.
	if errors.Is(cmdCtx.Err(), context.DeadlineExceeded) {
		return CriterionResult{
			Criterion: tc.raw,
			Passed:    false,
			Evidence:  fmt.Sprintf("typed command: timed out after %s", commandTimeout),
		}, nil
	}

	var exitErr *exec.ExitError
	snippet := sanitizeOneLineSnippet(string(out), maxCommandOutputEvidenceBytes)
	if errors.As(err, &exitErr) {
		ev := fmt.Sprintf("typed command: exit %d", exitErr.ExitCode())
		if snippet != "" {
			ev = fmt.Sprintf("%s (%s)", ev, snippet)
		}
		return CriterionResult{
			Criterion: tc.raw,
			Passed:    false,
			Evidence:  ev,
		}, nil
	}

	ev := fmt.Sprintf("typed command: exec error: %v", err)
	if snippet != "" {
		ev = fmt.Sprintf("%s (%s)", ev, snippet)
	}
	return CriterionResult{
		Criterion: tc.raw,
		Passed:    false,
		Evidence:  ev,
	}, nil
}

func sanitizeOneLineSnippet(s string, maxBytes int) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.Join(strings.Fields(s), " ")
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s
	}
	end := maxBytes
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return strings.TrimSpace(s[:end]) + "..."
}

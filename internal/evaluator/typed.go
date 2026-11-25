package evaluator

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ypsilonmeister/quota-fleet/internal/adapter"
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

func evaluateTypedCriterion(ctx context.Context, tc typedCriterion, fa FileAccess, commandTimeout time.Duration) (CriterionResult, error) {
	if err := ctx.Err(); err != nil {
		return CriterionResult{}, err
	}

	switch tc.kind {
	case typedKindFileExists:
		return evalFileExists(ctx, tc, fa)
	case typedKindFileNonEmpty:
		return evalFileNonEmpty(ctx, tc, fa)
	case typedKindContains:
		return evalContains(ctx, tc, fa)
	case typedKindCommand:
		return evalCommand(ctx, tc, fa, commandTimeout)
	default:
		return CriterionResult{
			Criterion: tc.raw,
			Passed:    false,
			Evidence:  fmt.Sprintf("typed: unknown check kind %q", tc.kind),
		}, nil
	}
}

func evalFileExists(ctx context.Context, tc typedCriterion, fa FileAccess) (CriterionResult, error) {
	display := strings.TrimSpace(tc.rest)
	if display == "" {
		return CriterionResult{
			Criterion: tc.raw,
			Passed:    false,
			Evidence:  "typed file_exists: empty path",
		}, nil
	}
	info, err := fa.Stat(ctx, display)
	if err != nil {
		if ctx.Err() != nil {
			return CriterionResult{}, ctx.Err()
		}
		if errors.Is(err, adapter.ErrEmptyPath) || errors.Is(err, adapter.ErrEmptyWorkdir) {
			return CriterionResult{
				Criterion: tc.raw,
				Passed:    false,
				Evidence:  fmt.Sprintf("typed file_exists: %v", err),
			}, nil
		}
		return CriterionResult{
			Criterion: tc.raw,
			Passed:    false,
			Evidence:  fmt.Sprintf("typed file_exists: stat error for %s: %v", display, err),
		}, nil
	}
	if !info.Exists {
		return CriterionResult{
			Criterion: tc.raw,
			Passed:    false,
			Evidence:  fmt.Sprintf("typed file_exists: not found: %s", display),
		}, nil
	}
	if info.IsDir {
		return CriterionResult{
			Criterion: tc.raw,
			Passed:    true,
			Evidence:  fmt.Sprintf("typed file_exists: found dir %s", display),
		}, nil
	}
	return CriterionResult{
		Criterion: tc.raw,
		Passed:    true,
		Evidence:  fmt.Sprintf("typed file_exists: found file %s (%dB)", display, info.Size),
	}, nil
}

func evalFileNonEmpty(ctx context.Context, tc typedCriterion, fa FileAccess) (CriterionResult, error) {
	display := strings.TrimSpace(tc.rest)
	if display == "" {
		return CriterionResult{
			Criterion: tc.raw,
			Passed:    false,
			Evidence:  "typed file_nonempty: empty path",
		}, nil
	}
	info, err := fa.Stat(ctx, display)
	if err != nil {
		if ctx.Err() != nil {
			return CriterionResult{}, ctx.Err()
		}
		if errors.Is(err, adapter.ErrEmptyPath) || errors.Is(err, adapter.ErrEmptyWorkdir) {
			return CriterionResult{
				Criterion: tc.raw,
				Passed:    false,
				Evidence:  fmt.Sprintf("typed file_nonempty: %v", err),
			}, nil
		}
		return CriterionResult{
			Criterion: tc.raw,
			Passed:    false,
			Evidence:  fmt.Sprintf("typed file_nonempty: stat error for %s: %v", display, err),
		}, nil
	}
	if !info.Exists {
		return CriterionResult{
			Criterion: tc.raw,
			Passed:    false,
			Evidence:  fmt.Sprintf("typed file_nonempty: not found: %s", display),
		}, nil
	}
	if info.IsDir {
		return CriterionResult{
			Criterion: tc.raw,
			Passed:    false,
			Evidence:  fmt.Sprintf("typed file_nonempty: %s is a directory", display),
		}, nil
	}
	if info.Size <= 0 {
		return CriterionResult{
			Criterion: tc.raw,
			Passed:    false,
			Evidence:  fmt.Sprintf("typed file_nonempty: %s is empty (0B)", display),
		}, nil
	}
	return CriterionResult{
		Criterion: tc.raw,
		Passed:    true,
		Evidence:  fmt.Sprintf("typed file_nonempty: %s (%dB)", display, info.Size),
	}, nil
}

func evalContains(ctx context.Context, tc typedCriterion, fa FileAccess) (CriterionResult, error) {
	parts := strings.SplitN(tc.rest, containsSeparator, 2)
	if len(parts) != 2 {
		return CriterionResult{
			Criterion: tc.raw,
			Passed:    false,
			Evidence:  "typed contains: invalid syntax (expected '<path> :: <text>')",
		}, nil
	}
	display := strings.TrimSpace(parts[0])
	needle := parts[1]
	if needle == "" {
		return CriterionResult{
			Criterion: tc.raw,
			Passed:    false,
			Evidence:  "typed contains: empty search text",
		}, nil
	}
	if display == "" {
		return CriterionResult{
			Criterion: tc.raw,
			Passed:    false,
			Evidence:  "typed contains: empty path",
		}, nil
	}

	data, err := fa.ReadFile(ctx, display)
	if err != nil {
		if ctx.Err() != nil {
			return CriterionResult{}, ctx.Err()
		}
		if errors.Is(err, adapter.ErrEmptyPath) || errors.Is(err, adapter.ErrEmptyWorkdir) {
			return CriterionResult{
				Criterion: tc.raw,
				Passed:    false,
				Evidence:  fmt.Sprintf("typed contains: %v", err),
			}, nil
		}
		if errors.Is(err, fs.ErrNotExist) {
			return CriterionResult{
				Criterion: tc.raw,
				Passed:    false,
				Evidence:  fmt.Sprintf("typed contains: not found: %s", display),
			}, nil
		}
		return CriterionResult{
			Criterion: tc.raw,
			Passed:    false,
			Evidence:  fmt.Sprintf("typed contains: read error for %s: %v", display, err),
		}, nil
	}
	if strings.Contains(string(data), needle) {
		return CriterionResult{
			Criterion: tc.raw,
			Passed:    true,
			Evidence:  fmt.Sprintf("typed contains: matched in %s", display),
		}, nil
	}
	return CriterionResult{
		Criterion: tc.raw,
		Passed:    false,
		Evidence:  fmt.Sprintf("typed contains: text not found in %s", display),
	}, nil
}

func evalCommand(ctx context.Context, tc typedCriterion, fa FileAccess, commandTimeout time.Duration) (CriterionResult, error) {
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

	res, err := fa.RunCommand(cmdCtx, cmdStr)
	if err == nil && res.ExitCode == 0 {
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
	if errors.Is(cmdCtx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return CriterionResult{
			Criterion: tc.raw,
			Passed:    false,
			Evidence:  fmt.Sprintf("typed command: timed out after %s", commandTimeout),
		}, nil
	}

	snippet := sanitizeOneLineSnippet(res.Output, maxCommandOutputEvidenceBytes)
	if err == nil {
		ev := fmt.Sprintf("typed command: exit %d", res.ExitCode)
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

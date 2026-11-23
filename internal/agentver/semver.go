package agentver

import (
	"regexp"
	"strconv"
	"strings"
)

// Semver represents a parsed Semantic Version (MAJOR.MINOR.PATCH[-PRERELEASE][+BUILD]).
type Semver struct {
	Major      int
	Minor      int
	Patch      int
	Prerelease string
	Build      string
}

var (
	semverExtractRe = regexp.MustCompile(`(?i)\bv?(\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?)\b`)
	semverExactRe   = regexp.MustCompile(`^[vV]?(\d+)\.(\d+)\.(\d+)(?:-([0-9A-Za-z.-]+))?(?:\+([0-9A-Za-z.-]+))?$`)
)

// NormalizeVersion extracts a normalized version string from raw command output.
// If the first non-empty line contains a semver token (e.g. "2.1.76 (Claude Code)" or
// "codex-cli 0.169.0" or "v0.4.2"), that semver without leading "v" is returned.
// Otherwise, the trimmed first non-empty line is returned verbatim so equality checks
// still work without inventing a fake semver ordering.
func NormalizeVersion(raw string) string {
	line := firstNonEmptyLine(raw)
	if line == "" {
		return ""
	}
	if m := semverExtractRe.FindStringSubmatch(line); len(m) >= 2 {
		return m[1]
	}
	return line
}

// ParseSemver parses s as a SemVer 2.0.0 version (allowing an optional leading 'v'/'V').
// Returns ok=false when s is not a valid semver string.
func ParseSemver(s string) (Semver, bool) {
	s = strings.TrimSpace(s)
	m := semverExactRe.FindStringSubmatch(s)
	if len(m) < 4 {
		return Semver{}, false
	}
	major, err1 := strconv.Atoi(m[1])
	minor, err2 := strconv.Atoi(m[2])
	patch, err3 := strconv.Atoi(m[3])
	if err1 != nil || err2 != nil || err3 != nil {
		return Semver{}, false
	}
	return Semver{
		Major:      major,
		Minor:      minor,
		Patch:      patch,
		Prerelease: m[4],
		Build:      m[5],
	}, true
}

// CompareSemver compares two parsed Semver values according to SemVer 2.0.0 precedence.
// Returns -1 if a < b, 0 if a == b, and +1 if a > b. Build metadata is ignored.
func CompareSemver(a, b Semver) int {
	if a.Major != b.Major {
		return cmpInt(a.Major, b.Major)
	}
	if a.Minor != b.Minor {
		return cmpInt(a.Minor, b.Minor)
	}
	if a.Patch != b.Patch {
		return cmpInt(a.Patch, b.Patch)
	}
	if a.Prerelease == b.Prerelease {
		return 0
	}
	// A normal release has higher precedence than a pre-release.
	if a.Prerelease == "" {
		return 1
	}
	if b.Prerelease == "" {
		return -1
	}
	ap := strings.Split(a.Prerelease, ".")
	bp := strings.Split(b.Prerelease, ".")
	n := len(ap)
	if len(bp) < n {
		n = len(bp)
	}
	for i := 0; i < n; i++ {
		if ap[i] == bp[i] {
			continue
		}
		ai, aErr := strconv.Atoi(ap[i])
		bi, bErr := strconv.Atoi(bp[i])
		switch {
		case aErr == nil && bErr == nil:
			return cmpInt(ai, bi)
		case aErr == nil && bErr != nil:
			// Numeric identifiers always have lower precedence than non-numeric identifiers.
			return -1
		case aErr != nil && bErr == nil:
			return 1
		default:
			if ap[i] < bp[i] {
				return -1
			}
			return 1
		}
	}
	return cmpInt(len(ap), len(bp))
}

func cmpInt(a, b int) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

func firstNonEmptyLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		trimmed := strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if trimmed != "" {
			return trimmed
		}
	}
	return ""
}

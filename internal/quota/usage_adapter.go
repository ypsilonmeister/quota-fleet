package quota

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var shortWindow5hRe = regexp.MustCompile(`(?i)(?:^|\s)5h\s+(\d+)\s*%`)

// rawUsageFile mirrors the current muse-widget/usage.json schema.
type rawUsageFile struct {
	UpdatedAt string                     `json:"updatedAt"`
	Services  map[string]rawUsageService `json:"services"`
}

type rawUsageService struct {
	RemainingPercent *int   `json:"remainingPercent"`
	ResetAt          string `json:"resetAt"`
	SubText          string `json:"subText"`
	SubResetAt       string `json:"subResetAt"`
	AltPercent       *int   `json:"altPercent"`
	AltResetAt       string `json:"altResetAt"`
	AltSubText       string `json:"altSubText"`
	AltSubResetAt    string `json:"altSubResetAt"`
	Stale            bool   `json:"stale"`
	FetchedAt        string `json:"fetchedAt"`
}

// LoadUsageFile reads a usage.json file from path (expanding a leading "~/" if present)
// and converts it into a normalized Snapshot.
func LoadUsageFile(path string) (*Snapshot, error) {
	resolved, err := expandHomePath(strings.TrimSpace(path))
	if err != nil {
		return nil, fmt.Errorf("resolve usage_file %q: %w", path, err)
	}
	if resolved == "" {
		return nil, errors.New("usage_file path is empty")
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		return nil, fmt.Errorf("read usage_file %q: %w", resolved, err)
	}
	snap, err := ParseUsageJSON(data)
	if err != nil {
		return nil, fmt.Errorf("parse usage_file %q: %w", resolved, err)
	}
	return snap, nil
}

// ParseUsageJSON parses the legacy muse-widget usage.json format and normalizes
// services.claude, services.codex, and services.antigravity (primary + alt) into
// the canonical pools: claude, codex, agy-gemini, and agy-3p.
func ParseUsageJSON(data []byte) (*Snapshot, error) {
	var raw rawUsageFile
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("unmarshal usage json: %w", err)
	}

	updatedAtStr := strings.TrimSpace(raw.UpdatedAt)
	if updatedAtStr == "" {
		return nil, errors.New("usage json missing updatedAt")
	}
	measuredAt, err := time.Parse(time.RFC3339Nano, updatedAtStr)
	if err != nil {
		return nil, fmt.Errorf("parse updatedAt %q: %w", updatedAtStr, err)
	}

	snap := &Snapshot{
		MeasuredAt: measuredAt,
		Pools:      make(map[string]PoolSnapshot),
	}

	if svc, ok := raw.Services["claude"]; ok && svc.RemainingPercent != nil {
		ps, err := buildPoolSnapshot(
			*svc.RemainingPercent, svc.ResetAt, svc.SubText, svc.SubResetAt,
			measuredAt, svc.Stale, svc.FetchedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("normalize pool %q: %w", PoolClaude, err)
		}
		snap.Pools[PoolClaude] = ps
	}

	if svc, ok := raw.Services["codex"]; ok && svc.RemainingPercent != nil {
		ps, err := buildPoolSnapshot(
			*svc.RemainingPercent, svc.ResetAt, svc.SubText, svc.SubResetAt,
			measuredAt, svc.Stale, svc.FetchedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("normalize pool %q: %w", PoolCodex, err)
		}
		snap.Pools[PoolCodex] = ps
	}

	if svc, ok := raw.Services["antigravity"]; ok {
		if svc.RemainingPercent != nil {
			ps, err := buildPoolSnapshot(
				*svc.RemainingPercent, svc.ResetAt, svc.SubText, svc.SubResetAt,
				measuredAt, svc.Stale, svc.FetchedAt,
			)
			if err != nil {
				return nil, fmt.Errorf("normalize pool %q: %w", PoolAgyGemini, err)
			}
			snap.Pools[PoolAgyGemini] = ps
		}
		if svc.AltPercent != nil {
			ps, err := buildPoolSnapshot(
				*svc.AltPercent, svc.AltResetAt, svc.AltSubText, svc.AltSubResetAt,
				measuredAt, svc.Stale, svc.FetchedAt,
			)
			if err != nil {
				return nil, fmt.Errorf("normalize pool %q: %w", PoolAgy3P, err)
			}
			snap.Pools[PoolAgy3P] = ps
		}
	}

	return snap, nil
}

func buildPoolSnapshot(
	weeklyPct int,
	weeklyResetRaw string,
	shortSubText string,
	shortResetRaw string,
	measuredAt time.Time,
	serviceStale bool,
	fetchedAtRaw string,
) (PoolSnapshot, error) {
	weeklyResetAt, err := parseOptionalRFC3339(weeklyResetRaw)
	if err != nil {
		return PoolSnapshot{}, fmt.Errorf("parse weekly resetAt %q: %w", weeklyResetRaw, err)
	}
	shortResetAt, err := parseOptionalRFC3339(shortResetRaw)
	if err != nil {
		return PoolSnapshot{}, fmt.Errorf("parse short resetAt %q: %w", shortResetRaw, err)
	}
	fetchedAt, err := parseOptionalRFC3339(fetchedAtRaw)
	if err != nil {
		return PoolSnapshot{}, fmt.Errorf("parse fetchedAt %q: %w", fetchedAtRaw, err)
	}

	return PoolSnapshot{
		WeeklyRemainingPercent: weeklyPct,
		WeeklyResetAt:          weeklyResetAt,
		ShortRemainingPercent:  ParseShortRemainingPercent(shortSubText),
		ShortResetAt:           shortResetAt,
		MeasuredAt:             measuredAt,
		ServiceStale:           serviceStale,
		FetchedAt:              fetchedAt,
	}, nil
}

// ParseShortRemainingPercent extracts the numeric 5-hour window percentage from
// display text such as "5h 98%". Returns nil if the value cannot be parsed.
func ParseShortRemainingPercent(subText string) *int {
	m := shortWindow5hRe.FindStringSubmatch(subText)
	if len(m) < 2 {
		return nil
	}
	v, err := strconv.Atoi(m[1])
	if err != nil {
		return nil
	}
	return &v
}

func parseOptionalRFC3339(raw string) (*time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func expandHomePath(p string) (string, error) {
	if p == "" {
		return "", nil
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if p == "~" {
			return home, nil
		}
		return filepath.Join(home, p[2:]), nil
	}
	return p, nil
}

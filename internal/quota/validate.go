package quota

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/config"
)

// DiagnosticSeverity classifies findings returned by ValidateUsageFile.
type DiagnosticSeverity string

const (
	SeverityError    DiagnosticSeverity = "error"    // exit 1 (hard error; blocks deploy)
	SeverityDegraded DiagnosticSeverity = "degraded" // exit 2 in normal mode; warning (exit 0) in --check-deploy
	SeverityWarning  DiagnosticSeverity = "warning"  // exit 0 (advisory warning)
	SeverityInfo     DiagnosticSeverity = "info"     // exit 0 (informational)
)

// Diagnostic represents a single finding from usage validation.
type Diagnostic struct {
	Severity DiagnosticSeverity `json:"severity"`
	Code     string             `json:"code"`
	Pool     string             `json:"pool,omitempty"`
	Message  string             `json:"message"`
}

// ValidatePoolSummary summarizes one pool's validated state.
type ValidatePoolSummary struct {
	Pool               string     `json:"pool"`
	Status             string     `json:"status,omitempty"`
	Health             string     `json:"health,omitempty"`
	Reason             string     `json:"reason,omitempty"`
	BlockedBy          string     `json:"blocked_by,omitempty"`
	PoolError          string     `json:"pool_error,omitempty"`
	MeasuredAt         *time.Time `json:"measured_at,omitempty"`
	AgeSeconds         *int64     `json:"age_seconds,omitempty"`
	WeeklyRemainingPct *int       `json:"weekly_remaining_percent,omitempty"`
	ShortRemainingPct  *int       `json:"short_remaining_percent,omitempty"`
	ShortApplicable    *bool      `json:"short_applicable,omitempty"`
	Unmeasured         bool       `json:"unmeasured,omitempty"`
}

// ValidateOptions configures ValidateUsageFile (§4.3).
type ValidateOptions struct {
	File            string
	Format          string // "" (auto-detect only when Config == nil), "legacy", or "v1"
	Config          *config.Config
	StaleAfter      time.Duration
	IgnoreStale     bool
	CheckDeploy     bool
	RejectOlderThan string
	Now             time.Time
}

// ValidationResult is the structured output of ValidateUsageFile.
type ValidationResult struct {
	OK          bool                  `json:"ok"`
	Deployable  bool                  `json:"deployable"`
	Degraded    bool                  `json:"degraded"`
	ExitCode    int                   `json:"exit_code"`
	File        string                `json:"file"`
	Format      string                `json:"format"`
	SHA256      string                `json:"sha256,omitempty"`
	GeneratedAt *time.Time            `json:"generated_at,omitempty"`
	AgeSeconds  *int64                `json:"age_seconds,omitempty"`
	Pools       []ValidatePoolSummary `json:"pools,omitempty"`
	Diagnostics []Diagnostic          `json:"diagnostics,omitempty"`
	LegacyDiffs []string              `json:"legacy_diffs,omitempty"`
}

// ValidateUsageFile performs a full-scan validation of a usage snapshot file (§4.3 + Items C & D).
// It never short-circuits per-pool checks after snapshot_stale, and aggregates the final exit code
// with strict severity precedence (exit 1 > exit 2 > exit 0).
func ValidateUsageFile(opts ValidateOptions) ValidationResult {
	now := opts.Now
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}

	staleAfter := opts.StaleAfter
	if staleAfter <= 0 {
		if opts.Config != nil {
			staleAfter = opts.Config.Quota.EffectiveStaleAfter()
		} else {
			staleAfter = DefaultStaleAfter
		}
	}

	res := ValidationResult{
		File: strings.TrimSpace(opts.File),
	}
	if res.File == "" && opts.Config != nil {
		res.File = strings.TrimSpace(opts.Config.Quota.UsageFile)
	}

	addDiag := func(sev DiagnosticSeverity, code, pool, msg string) {
		res.Diagnostics = append(res.Diagnostics, Diagnostic{
			Severity: sev,
			Code:     code,
			Pool:     pool,
			Message:  msg,
		})
	}

	// Determine expected format: --format > --config quota.usage_format > standalone content detection.
	expectedFmt := strings.TrimSpace(opts.Format)
	if expectedFmt != "" {
		norm, err := NormalizeUsageFormat(expectedFmt)
		if err != nil {
			addDiag(SeverityError, "invalid_format", "", err.Error())
			return finalizeValidation(res, opts)
		}
		expectedFmt = norm
	} else if opts.Config != nil {
		expectedFmt = opts.Config.Quota.EffectiveUsageFormat()
	}

	resolved, err := expandHomePath(res.File)
	if err != nil {
		addDiag(SeverityError, "read_error", "", fmt.Sprintf("resolve file %q: %v", res.File, err))
		return finalizeValidation(res, opts)
	}
	if resolved == "" {
		addDiag(SeverityError, "read_error", "", "usage file path is empty")
		return finalizeValidation(res, opts)
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		addDiag(SeverityError, "read_error", "", fmt.Sprintf("read file %q: %v", resolved, err))
		return finalizeValidation(res, opts)
	}
	sum := sha256.Sum256(data)
	res.SHA256 = hex.EncodeToString(sum[:])

	// Standalone auto-detection when neither --format nor --config was given (§4.3.1).
	if expectedFmt == "" {
		info, ierr := inspectTopLevelJSON(data)
		if ierr != nil {
			addDiag(SeverityError, "invalid_json", "", fmt.Sprintf("invalid JSON: %v", ierr))
			return finalizeValidation(res, opts)
		}
		if !info.isObject {
			addDiag(SeverityError, "schema_violation", "", "usage snapshot must be a JSON object")
			return finalizeValidation(res, opts)
		}
		switch {
		case info.hasSchemaVersion:
			expectedFmt = FormatV1
		case info.hasUpdatedAt || info.hasServices:
			expectedFmt = FormatLegacy
		default:
			addDiag(SeverityError, ReasonFormatMismatch, "", "unable to detect format: missing schema_version (v1) and updatedAt/services (legacy)")
			return finalizeValidation(res, opts)
		}
	}
	res.Format = expectedFmt

	snap, err := ParseUsageJSONWithFormat(data, expectedFmt)
	if err != nil {
		addDiag(SeverityError, ParseErrorCode(err), "", err.Error())
		return finalizeValidation(res, opts)
	}

	if expectedFmt == FormatLegacy {
		validateLegacySnapshot(&res, data, snap, opts, now, staleAfter, addDiag)
	} else {
		validateV1Snapshot(&res, data, snap, opts, now, staleAfter, addDiag)
	}

	// Config-level unmapped agent warnings (Decision 8: Phase 1 warns, exit 0).
	if opts.Config != nil {
		if unmapped := opts.Config.UnmappedAgents(); len(unmapped) > 0 {
			addDiag(SeverityWarning, "unmapped_agents", "",
				fmt.Sprintf("configured agents not mapped by any pools.*.applies_to (gate will skip): %s", strings.Join(unmapped, ", ")))
		}
	}

	return finalizeValidation(res, opts)
}

func validateV1Snapshot(
	res *ValidationResult,
	rawData []byte,
	snap *Snapshot,
	opts ValidateOptions,
	now time.Time,
	staleAfter time.Duration,
	addDiag func(DiagnosticSeverity, string, string, string),
) {
	genAt := snap.GeneratedAt.UTC()
	res.GeneratedAt = &genAt
	age := now.Sub(genAt)
	ageSec := int64(age.Seconds())
	res.AgeSeconds = &ageSec

	// File-level generated_at future skew check (Rank 1 -> hard error exit 1).
	if genAt.After(now.Add(FutureSkewTolerance)) {
		addDiag(SeverityError, ReasonTimestampFuture, "",
			fmt.Sprintf("generated_at %s exceeds now+%s", genAt.Format(time.RFC3339), FutureSkewTolerance))
	}

	// File-level stale check (Rank 2 -> degraded exit 2; never short-circuits per-pool scan, Item D).
	if IsStale(genAt, now, staleAfter) {
		addDiag(SeverityDegraded, "snapshot_stale", "", FormatStaleUnarrivedMessage(age, staleAfter))
	}

	// --reject-older-than generation check (Item C).
	if existingPath := strings.TrimSpace(opts.RejectOlderThan); existingPath != "" {
		checkRejectOlderThan(existingPath, rawData, genAt, now, addDiag)
	}

	// Full per-pool scan across all pools in snap.Pools (sorted for deterministic output).
	snapPoolNames := make([]string, 0, len(snap.Pools))
	for name := range snap.Pools {
		snapPoolNames = append(snapPoolNames, name)
	}
	sort.Strings(snapPoolNames)

	evalOpts := EvalOptions{
		Now:             now,
		StaleAfter:      staleAfter,
		Format:          FormatV1,
		CheckPoolConfig: opts.Config != nil,
	}
	if opts.Config != nil {
		evalOpts.Pools = opts.Config.Pools
	}

	for _, name := range snapPoolNames {
		ps := snap.Pools[name]
		sanitizedErr := SanitizeCollectorError(ps.Error)
		summary := ValidatePoolSummary{
			Pool:            name,
			Status:          ps.EffectiveStatus(),
			PoolError:       sanitizedErr,
			ShortApplicable: ps.ShortApplicable,
		}
		if !ps.WeeklyAbsent {
			w := ps.WeeklyRemainingPercent
			summary.WeeklyRemainingPct = &w
		}
		summary.ShortRemainingPct = ps.ShortRemainingPercent
		if !ps.MeasuredAt.IsZero() {
			mt := ps.MeasuredAt.UTC()
			summary.MeasuredAt = &mt
			pAgeSec := int64(now.Sub(mt).Seconds())
			summary.AgeSeconds = &pAgeSec
		}

		// Per-pool timestamp checks (ALWAYS run regardless of snapshot_stale, Item D).
		if !ps.MeasuredAt.IsZero() {
			if ps.MeasuredAt.After(now.Add(FutureSkewTolerance)) {
				addDiag(SeverityError, ReasonTimestampFuture, name,
					fmt.Sprintf("pools.%s.measured_at %s exceeds now+%s", name, ps.MeasuredAt.UTC().Format(time.RFC3339), FutureSkewTolerance))
			}
			if ps.MeasuredAt.After(genAt.Add(MeasuredAfterGeneratedTolerance)) {
				addDiag(SeverityError, ReasonTimestampInconsistent, name,
					fmt.Sprintf("pools.%s.measured_at %s exceeds generated_at+%s", name, ps.MeasuredAt.UTC().Format(time.RFC3339), MeasuredAfterGeneratedTolerance))
			}
		}

		// Determine config-based pool settings.
		var (
			inConfig           bool
			cfgPool            config.PoolConfig
			shortWindowEnabled = true
		)
		if opts.Config != nil {
			cfgPool, inConfig = opts.Config.Pools[name]
			if inConfig {
				summary.Unmeasured = cfgPool.Unmeasured
				shortWindowEnabled = cfgPool.EffectiveShortWindow()
			}
		}

		if inConfig && cfgPool.Unmeasured {
			summary.Health = string(PoolUnmeasured)
			summary.Reason = ReasonUnmeasured
			res.Pools = append(res.Pools, summary)
			continue
		}

		// Evaluate Gate health for display (standalone when pool not in config).
		poolEvalOpts := evalOpts
		if !inConfig {
			poolEvalOpts.CheckPoolConfig = false
		}
		ev := evaluatePoolV1(name, snap, poolEvalOpts)
		summary.Health = string(ev.Status)
		summary.Reason = ev.Reason
		summary.BlockedBy = ev.BlockedBy

		// Per-pool collector error (Rank 5 -> degraded exit 2).
		if ps.EffectiveStatus() == PoolEntryStatusError {
			msg := fmt.Sprintf("pools.%s reported collector error", name)
			if sanitizedErr != "" {
				msg += fmt.Sprintf(" (%s)", sanitizedErr)
			}
			addDiag(SeverityDegraded, ReasonCollectorError, name, msg)
		}

		// Per-pool stale (Rank 6 -> degraded exit 2).
		if !ps.MeasuredAt.IsZero() && IsStale(ps.MeasuredAt, now, staleAfter) {
			pAge := now.Sub(ps.MeasuredAt)
			addDiag(SeverityDegraded, "pool_stale", name,
				fmt.Sprintf("pools.%s %s", name, FormatStaleUnarrivedMessage(pAge, staleAfter)))
		}

		// Normalization advisory info (§3.3).
		if ps.ShortNormalized {
			addDiag(SeverityInfo, "short_normalized_to_null", name,
				fmt.Sprintf("pools.%s.short lacked remaining_percent and was normalized to missing; collectors should emit short: null or omit short", name))
		}

		// Short window checks (§3.3 / §4.3.1).
		shortNotApplicable := ps.ShortApplicable != nil && !*ps.ShortApplicable
		if inConfig {
			if shortWindowEnabled {
				if shortNotApplicable {
					addDiag(SeverityError, ReasonShortWindowMismatch, name,
						fmt.Sprintf("pools.%s has short.applicable=false but fleet.yaml requires short_window=true", name))
				} else if ps.ShortRemainingPercent == nil && ps.EffectiveStatus() == PoolEntryStatusOK && !ps.WeeklyAbsent {
					if ps.WeeklyRemainingPercent == 0 && !windowExpired(ps.WeeklyResetAt, now) {
						addDiag(SeverityInfo, "short_window_missing_while_weekly_exhausted", name,
							fmt.Sprintf("pools.%s has missing short window while weekly=0%% (definitively exhausted)", name))
					} else {
						addDiag(SeverityDegraded, ReasonShortWindowMissing, name,
							fmt.Sprintf("pools.%s is missing short.remaining_percent while weekly=%d%%", name, ps.WeeklyRemainingPercent))
					}
				}
			} else {
				// short_window: false in fleet.yaml -> exempt from short check, warn if short measurement present.
				if ps.ShortRemainingPercent != nil {
					addDiag(SeverityWarning, "short_window_present_but_disabled", name,
						fmt.Sprintf("pools.%s provides short.remaining_percent=%d%% while fleet.yaml sets short_window=false (short check exempted)", name, *ps.ShortRemainingPercent))
				}
			}
		} else {
			// Without --config (or pool not in config): §4.3.1 rules.
			if shortNotApplicable {
				addDiag(SeverityInfo, "short_applicable_unverified", name,
					fmt.Sprintf("pools.%s has short.applicable=false (unverified without --config)", name))
			} else if ps.ShortRemainingPercent == nil && ps.EffectiveStatus() == PoolEntryStatusOK && !ps.WeeklyAbsent {
				if ps.WeeklyRemainingPercent == 0 && !windowExpired(ps.WeeklyResetAt, now) {
					addDiag(SeverityInfo, "short_window_missing_while_weekly_exhausted", name,
						fmt.Sprintf("pools.%s has missing short window while weekly=0%% (definitively exhausted)", name))
				} else {
					addDiag(SeverityDegraded, ReasonShortWindowMissing, name,
						fmt.Sprintf("pools.%s is missing short.remaining_percent while weekly=%d%% (default short_window=true without --config)", name, ps.WeeklyRemainingPercent))
				}
			}
		}

		res.Pools = append(res.Pools, summary)
	}

	// Config-defined measured pools missing from snapshot (Rank 3 -> hard error exit 1 with --config).
	if opts.Config != nil {
		cfgNames := make([]string, 0, len(opts.Config.Pools))
		for name := range opts.Config.Pools {
			cfgNames = append(cfgNames, name)
		}
		sort.Strings(cfgNames)
		for _, name := range cfgNames {
			pc := opts.Config.Pools[name]
			if pc.Unmeasured {
				continue
			}
			if _, exists := snap.Pools[name]; !exists {
				addDiag(SeverityError, ReasonPoolNotFound, name,
					fmt.Sprintf("configured measured pool %q is missing from usage snapshot", name))
				res.Pools = append(res.Pools, ValidatePoolSummary{
					Pool:   name,
					Health: string(PoolUnknown),
					Reason: ReasonPoolNotFound,
				})
			}
		}
	}
}

func validateLegacySnapshot(
	res *ValidationResult,
	rawData []byte,
	snap *Snapshot,
	opts ValidateOptions,
	now time.Time,
	staleAfter time.Duration,
	addDiag func(DiagnosticSeverity, string, string, string),
) {
	addDiag(SeverityWarning, "legacy_format_deprecated", "",
		"usage snapshot uses legacy format; migrate to Schema v1 (docs/23-usage-snapshot.md)")

	if dupPath, err := DetectDuplicateJSONKeys(rawData); err == nil && dupPath != "" {
		addDiag(SeverityWarning, "legacy_duplicate_key", "",
			fmt.Sprintf("legacy JSON contains duplicate key %q (rejected in Schema v1)", dupPath))
	}

	mt := snap.MeasuredAt.UTC()
	res.GeneratedAt = &mt
	age := now.Sub(mt)
	ageSec := int64(age.Seconds())
	res.AgeSeconds = &ageSec

	if IsStale(mt, now, staleAfter) {
		addDiag(SeverityDegraded, "snapshot_stale", "", FormatStaleUnarrivedMessage(age, staleAfter))
	}

	snapPoolNames := make([]string, 0, len(snap.Pools))
	for name := range snap.Pools {
		snapPoolNames = append(snapPoolNames, name)
	}
	sort.Strings(snapPoolNames)

	evalOpts := EvalOptions{
		Now:        now,
		StaleAfter: staleAfter,
		Format:     FormatLegacy,
	}
	if opts.Config != nil {
		evalOpts.Pools = opts.Config.Pools
	}

	for _, name := range snapPoolNames {
		ps := snap.Pools[name]
		ev := evaluatePoolLegacy(name, snap, evalOpts)
		w := ps.WeeklyRemainingPercent
		pmt := ps.MeasuredAt.UTC()
		pAgeSec := int64(now.Sub(pmt).Seconds())
		summary := ValidatePoolSummary{
			Pool:               name,
			Status:             PoolEntryStatusOK,
			Health:             string(ev.Status),
			Reason:             ev.Reason,
			BlockedBy:          ev.BlockedBy,
			MeasuredAt:         &pmt,
			AgeSeconds:         &pAgeSec,
			WeeklyRemainingPct: &w,
			ShortRemainingPct:  ps.ShortRemainingPercent,
			Unmeasured:         ev.Unmeasured,
		}
		res.Pools = append(res.Pools, summary)

		if ev.Unmeasured {
			continue
		}
		if ps.ServiceStale {
			addDiag(SeverityDegraded, "pool_stale", name, fmt.Sprintf("legacy pool %q is marked stale=true", name))
		}
		if ps.ShortRemainingPercent == nil {
			if ps.WeeklyRemainingPercent == 0 && !windowExpired(ps.WeeklyResetAt, now) {
				addDiag(SeverityInfo, "short_window_missing_while_weekly_exhausted", name,
					fmt.Sprintf("pool %q has missing short window while weekly=0%%", name))
			} else if ps.WeeklyRemainingPercent == 0 {
				addDiag(SeverityDegraded, ReasonShortWindowMissing, name,
					fmt.Sprintf("pool %q has missing short window and an expired weekly 0%% (legacy and v1 enforce hold)", name))
			} else {
				addDiag(SeverityDegraded, ReasonShortWindowMissing, name,
					fmt.Sprintf("pool %q has missing short window (legacy enforce passes; v1 enforce holds)", name))
				res.LegacyDiffs = append(res.LegacyDiffs,
					fmt.Sprintf("pool %q: short_window_null passes in legacy enforce but holds in v1 enforce", name))
			}
		}
		if ps.WeeklyResetAt != nil && !ps.WeeklyResetAt.After(now) {
			res.LegacyDiffs = append(res.LegacyDiffs,
				fmt.Sprintf("pool %q: weekly reset_at %s is in the past (legacy resetBefore prioritizes past timestamps; v1 treats past reset_at as unknown)",
					name, ps.WeeklyResetAt.UTC().Format(time.RFC3339)))
		}
	}

	if opts.Config != nil {
		cfgNames := make([]string, 0, len(opts.Config.Pools))
		for name := range opts.Config.Pools {
			cfgNames = append(cfgNames, name)
		}
		sort.Strings(cfgNames)
		for _, name := range cfgNames {
			pc := opts.Config.Pools[name]
			if pc.Unmeasured {
				continue
			}
			if _, exists := snap.Pools[name]; !exists {
				addDiag(SeverityError, ReasonPoolNotFound, name,
					fmt.Sprintf("configured measured pool %q is missing from legacy snapshot", name))
				res.LegacyDiffs = append(res.LegacyDiffs,
					fmt.Sprintf("pool %q: pool_not_found passes in legacy enforce but holds in v1 enforce", name))
			}
		}
	}
}

func checkRejectOlderThan(
	existingPath string,
	newData []byte,
	newGenAt time.Time,
	now time.Time,
	addDiag func(DiagnosticSeverity, string, string, string),
) {
	resolved, err := expandHomePath(existingPath)
	if err != nil || resolved == "" {
		addDiag(SeverityInfo, "existing_skipped", "", "reject-older-than path could not be resolved; skipping generation check")
		return
	}
	existingData, err := os.ReadFile(resolved)
	if err != nil {
		addDiag(SeverityInfo, "existing_skipped", "", fmt.Sprintf("existing file %q not readable (%v); skipping generation check", resolved, err))
		return
	}
	existingSnap, err := ParseUsageJSONV1(existingData)
	if err != nil {
		addDiag(SeverityInfo, "existing_skipped", "", fmt.Sprintf("existing file %q is not valid Schema v1 (%v); skipping generation check", resolved, err))
		return
	}

	existingGenAt := existingSnap.GeneratedAt.UTC()
	// Item C clock-recovery rule: if the existing deployed file itself has a future-skewed
	// generated_at (> now + 5m), ignore it as a generation baseline so current-time recovery
	// snapshots are not permanently blocked. Normal stale or per-pool errors do NOT disable protection.
	if existingGenAt.After(now.Add(FutureSkewTolerance)) {
		addDiag(SeverityWarning, "existing_generation_ignored_future_clock", "",
			fmt.Sprintf("existing file %q has future-skewed generated_at %s (> now+%s); ignoring as generation baseline to allow clock recovery",
				resolved, existingGenAt.Format(time.RFC3339), FutureSkewTolerance))
		return
	}

	if newGenAt.Before(existingGenAt) {
		addDiag(SeverityError, "older_generation", "",
			fmt.Sprintf("new generated_at %s is older than existing %s",
				newGenAt.Format(time.RFC3339), existingGenAt.Format(time.RFC3339)))
		return
	}

	// Item C equal-timestamp rule: identical bytes (SHA-256) are allowed as idempotent no-op;
	// different content with the exact same generated_at is rejected as generation_conflict.
	if newGenAt.Equal(existingGenAt) {
		newSum := sha256.Sum256(newData)
		existSum := sha256.Sum256(existingData)
		if newSum == existSum {
			addDiag(SeverityInfo, "same_generation_identical", "",
				fmt.Sprintf("new snapshot matches existing generated_at %s with identical SHA-256 (no-op)", newGenAt.Format(time.RFC3339)))
		} else {
			addDiag(SeverityError, "generation_conflict", "",
				fmt.Sprintf("new snapshot has identical generated_at %s as existing file but different content (SHA-256 mismatch)", newGenAt.Format(time.RFC3339)))
		}
	}
}

func finalizeValidation(res ValidationResult, opts ValidateOptions) ValidationResult {
	hardErrors := 0
	allDegraded := 0
	effectiveDegraded := 0

	for _, d := range res.Diagnostics {
		switch d.Severity {
		case SeverityError:
			hardErrors++
		case SeverityDegraded:
			allDegraded++
			if opts.IgnoreStale && (d.Code == "snapshot_stale" || d.Code == "pool_stale") {
				continue
			}
			effectiveDegraded++
		}
	}

	res.Degraded = allDegraded > 0
	res.Deployable = hardErrors == 0

	if opts.CheckDeploy {
		if hardErrors > 0 {
			res.OK = false
			res.ExitCode = 1
		} else {
			res.OK = true
			res.ExitCode = 0
		}
		return res
	}

	switch {
	case hardErrors > 0:
		res.OK = false
		res.ExitCode = 1
	case effectiveDegraded > 0:
		res.OK = false
		res.ExitCode = 2
	default:
		res.OK = true
		res.ExitCode = 0
	}
	return res
}

package quota

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/config"
)

// EvalOptions controls gate evaluation timing, staleness threshold, format, and pool metadata.
type EvalOptions struct {
	// Now is the evaluation timestamp. Defaults to time.Now().UTC() when zero.
	Now time.Time
	// StaleAfter is the maximum allowed snapshot age. Defaults to DefaultStaleAfter (45m).
	StaleAfter time.Duration
	// Pools provides pool metadata (e.g. unmeasured: true, short_window: false).
	Pools map[string]config.PoolConfig
	// Format selects evaluation rules ("legacy" default, or "v1").
	Format string
	// CheckPoolConfig enables fleet.yaml pool existence verification in v1 mode.
	CheckPoolConfig bool
	// HeldRelease enables §4.2.C held-job release rules after config changes (v1 mode).
	HeldRelease bool
}

func (o EvalOptions) effectiveNow() time.Time {
	if o.Now.IsZero() {
		return time.Now().UTC()
	}
	return o.Now.UTC()
}

func (o EvalOptions) effectiveStaleAfter() time.Duration {
	if o.StaleAfter <= 0 {
		return DefaultStaleAfter
	}
	return o.StaleAfter
}

func (o EvalOptions) effectiveFormat() string {
	if strings.TrimSpace(o.Format) == FormatV1 {
		return FormatV1
	}
	return FormatLegacy
}

// IsStale reports whether measuredAt is older than staleAfter relative to now.
func IsStale(measuredAt, now time.Time, staleAfter time.Duration) bool {
	if staleAfter <= 0 {
		staleAfter = DefaultStaleAfter
	}
	if measuredAt.IsZero() {
		return true
	}
	return now.Sub(measuredAt) > staleAfter
}

// EvaluatePool evaluates the health of a single pool against snap.
// When opts.Format is "v1", it applies the strict Schema v1 priority rules (§3.4 ranks 1..11).
// Otherwise it preserves the exact legacy behavior (§3.6).
func EvaluatePool(pool string, snap *Snapshot, opts EvalOptions) PoolEvaluation {
	if opts.effectiveFormat() == FormatV1 {
		return evaluatePoolV1(pool, snap, opts)
	}
	return evaluatePoolLegacy(pool, snap, opts)
}

func evaluatePoolLegacy(pool string, snap *Snapshot, opts EvalOptions) PoolEvaluation {
	pool = strings.TrimSpace(pool)
	if cfg, ok := opts.Pools[pool]; ok && cfg.Unmeasured {
		return PoolEvaluation{
			Pool:       pool,
			Status:     PoolUnmeasured,
			Reason:     ReasonUnmeasured,
			Unmeasured: true,
		}
	}

	if snap == nil {
		return PoolEvaluation{
			Pool:   pool,
			Status: PoolUnknown,
			Reason: ReasonSnapshotError,
		}
	}

	now := opts.effectiveNow()
	staleAfter := opts.effectiveStaleAfter()

	ps, ok := snap.Pools[pool]
	if !ok {
		if IsStale(snap.MeasuredAt, now, staleAfter) {
			return PoolEvaluation{
				Pool:   pool,
				Status: PoolUnknown,
				Reason: ReasonStale,
			}
		}
		return PoolEvaluation{
			Pool:   pool,
			Status: PoolUnknown,
			Reason: ReasonPoolNotFound,
		}
	}

	psCopy := ps
	psCopy.Error = SanitizeCollectorError(ps.Error)
	eval := PoolEvaluation{
		Pool:     pool,
		Snapshot: &psCopy,
	}

	measuredAt := ps.MeasuredAt
	if measuredAt.IsZero() {
		measuredAt = snap.MeasuredAt
	}
	if ps.ServiceStale || IsStale(measuredAt, now, staleAfter) {
		eval.Status = PoolUnknown
		eval.Reason = ReasonStale
		return eval
	}

	weeklyExhausted := ps.WeeklyRemainingPercent <= 0

	if ps.ShortRemainingPercent == nil {
		if weeklyExhausted {
			eval.Status = PoolExhausted
			eval.BlockedBy = BlockedByWeekly
			return eval
		}
		eval.Status = PoolUnknown
		eval.Reason = ReasonShortWindowNull
		return eval
	}

	shortExhausted := *ps.ShortRemainingPercent <= 0
	switch {
	case weeklyExhausted && shortExhausted:
		eval.Status = PoolExhausted
		eval.BlockedBy = BlockedByWeeklyAnd5
	case weeklyExhausted:
		eval.Status = PoolExhausted
		eval.BlockedBy = BlockedByWeekly
	case shortExhausted:
		eval.Status = PoolExhausted
		eval.BlockedBy = BlockedByShort
	default:
		eval.Status = PoolHealthy
	}
	return eval
}

func evaluatePoolV1(pool string, snap *Snapshot, opts EvalOptions) PoolEvaluation {
	pool = strings.TrimSpace(pool)
	shortWindowEnabled := true
	if opts.CheckPoolConfig {
		cfg, ok := opts.Pools[pool]
		if !ok {
			return PoolEvaluation{
				Pool:   pool,
				Status: PoolUnknown,
				Reason: ReasonPoolNotConfigured,
			}
		}
		if cfg.Unmeasured {
			return PoolEvaluation{
				Pool:       pool,
				Status:     PoolUnmeasured,
				Reason:     ReasonUnmeasured,
				Unmeasured: true,
			}
		}
		shortWindowEnabled = cfg.EffectiveShortWindow()
	} else if cfg, ok := opts.Pools[pool]; ok {
		if cfg.Unmeasured {
			return PoolEvaluation{
				Pool:       pool,
				Status:     PoolUnmeasured,
				Reason:     ReasonUnmeasured,
				Unmeasured: true,
			}
		}
		shortWindowEnabled = cfg.EffectiveShortWindow()
	}

	now := opts.effectiveNow()
	staleAfter := opts.effectiveStaleAfter()

	// Rank 1: missing snapshot or generated_at > now + 5m (file-wide blast radius).
	if snap == nil {
		return PoolEvaluation{
			Pool:   pool,
			Status: PoolUnknown,
			Reason: ReasonSnapshotError,
		}
	}
	genAt := snap.GeneratedAt
	if genAt.IsZero() {
		genAt = snap.MeasuredAt
	}
	if !genAt.IsZero() && genAt.After(now.Add(FutureSkewTolerance)) {
		return PoolEvaluation{
			Pool:   pool,
			Status: PoolUnknown,
			Reason: ReasonSnapshotError,
			Detail: fmt.Sprintf("%s: generated_at %s exceeds now+%s", ReasonTimestampFuture, genAt.UTC().Format(time.RFC3339), FutureSkewTolerance),
		}
	}

	// Rank 2: file-wide stale (now - generated_at > stale_after).
	if IsStale(genAt, now, staleAfter) {
		ageSec := int64(now.Sub(genAt).Seconds())
		ev := PoolEvaluation{
			Pool:       pool,
			Status:     PoolUnknown,
			Reason:     ReasonStale,
			Detail:     "snapshot_stale",
			AgeSeconds: &ageSec,
		}
		if ps, ok := snap.Pools[pool]; ok {
			psCopy := ps
			psCopy.Error = SanitizeCollectorError(ps.Error)
			ev.Snapshot = &psCopy
		}
		return ev
	}

	// Rank 3: pool missing from snapshot.
	ps, ok := snap.Pools[pool]
	if !ok {
		return PoolEvaluation{
			Pool:   pool,
			Status: PoolUnknown,
			Reason: ReasonPoolNotFound,
		}
	}

	psCopy := ps
	psCopy.Error = SanitizeCollectorError(ps.Error)
	eval := PoolEvaluation{
		Pool:     pool,
		Snapshot: &psCopy,
	}
	if !ps.MeasuredAt.IsZero() {
		ageSec := int64(now.Sub(ps.MeasuredAt).Seconds())
		eval.AgeSeconds = &ageSec
	}

	// Rank 4 (localized to this pool in Gate):
	// 4a timestamp_future (measured_at > now + 5m) takes precedence over
	// 4b timestamp_inconsistent (measured_at > generated_at + 60s) (Item D).
	if !ps.MeasuredAt.IsZero() {
		if ps.MeasuredAt.After(now.Add(FutureSkewTolerance)) {
			eval.Status = PoolUnknown
			eval.Reason = ReasonTimestampFuture
			eval.Detail = fmt.Sprintf("measured_at %s exceeds now+%s", ps.MeasuredAt.UTC().Format(time.RFC3339), FutureSkewTolerance)
			return eval
		}
		if !genAt.IsZero() && ps.MeasuredAt.After(genAt.Add(MeasuredAfterGeneratedTolerance)) {
			eval.Status = PoolUnknown
			eval.Reason = ReasonTimestampInconsistent
			eval.Detail = fmt.Sprintf("measured_at %s exceeds generated_at+%s", ps.MeasuredAt.UTC().Format(time.RFC3339), MeasuredAfterGeneratedTolerance)
			return eval
		}
	}

	// Rank 5: per-pool collector error.
	if ps.EffectiveStatus() == PoolEntryStatusError {
		sanitizedErr := SanitizeCollectorError(ps.Error)
		eval.Status = PoolUnknown
		eval.Reason = ReasonCollectorError
		eval.PoolError = sanitizedErr
		if sanitizedErr != "" {
			eval.Detail = sanitizedErr
		}
		return eval
	}

	// Rank 6: per-pool stale (now - measured_at > stale_after or service_stale).
	if ps.ServiceStale || IsStale(ps.MeasuredAt, now, staleAfter) {
		eval.Status = PoolUnknown
		eval.Reason = ReasonStale
		eval.Detail = "pool_stale"
		return eval
	}

	// Rank 7: short window config mismatch (fleet.yaml short_window: true vs snapshot short.applicable: false).
	shortNotApplicable := ps.ShortApplicable != nil && !*ps.ShortApplicable
	if shortWindowEnabled && shortNotApplicable {
		eval.Status = PoolUnknown
		eval.Reason = ReasonShortWindowMismatch
		return eval
	}

	// Rank 8: weekly exhausted (even when short window is missing).
	weeklyExhausted := !ps.WeeklyAbsent && ps.WeeklyRemainingPercent <= 0
	if weeklyExhausted {
		eval.Status = PoolExhausted
		if shortWindowEnabled && !shortNotApplicable && ps.ShortRemainingPercent != nil && *ps.ShortRemainingPercent <= 0 {
			eval.BlockedBy = "weekly," + ps.EffectiveShortWindow()
		} else {
			eval.BlockedBy = BlockedByWeekly
		}
		return eval
	}

	// Rank 9: short window missing while weekly > 0 and short_window enabled.
	if shortWindowEnabled && ps.ShortRemainingPercent == nil {
		eval.Status = PoolUnknown
		eval.Reason = ReasonShortWindowMissing
		return eval
	}

	// Rank 10: short window exhausted.
	if shortWindowEnabled && ps.ShortRemainingPercent != nil && *ps.ShortRemainingPercent <= 0 {
		eval.Status = PoolExhausted
		eval.BlockedBy = ps.EffectiveShortWindow()
		return eval
	}

	// Rank 11: healthy.
	eval.Status = PoolHealthy
	return eval
}

// EvaluateGate evaluates primaryPool and altPools against snap and picks the best
// healthy pool (or applies bypass/detour/unknown/hold rules per Format).
func EvaluateGate(primaryPool string, altPools []string, snap *Snapshot, opts EvalOptions) GateEvaluation {
	if opts.effectiveFormat() == FormatV1 {
		return evaluateGateV1(primaryPool, altPools, snap, opts)
	}
	return evaluateGateLegacy(primaryPool, altPools, snap, opts)
}

func evaluateGateLegacy(primaryPool string, altPools []string, snap *Snapshot, opts EvalOptions) GateEvaluation {
	now := opts.effectiveNow()
	primaryPool = strings.TrimSpace(primaryPool)
	cleanedAlts := cleanPools(altPools, primaryPool)
	candidates := buildCandidates(primaryPool, cleanedAlts)

	res := GateEvaluation{
		Format:        FormatLegacy,
		RequestedPool: primaryPool,
		AltPools:      cleanedAlts,
		DecidedAt:     now,
	}
	if snap != nil && !snap.MeasuredAt.IsZero() {
		t := snap.MeasuredAt
		res.MeasuredAt = &t
	}

	if len(candidates) == 0 {
		res.Decision = DecisionUnknown
		res.Reason = ReasonNoCandidatePools
		return res
	}

	// Primary unmeasured pool bypasses gate measurement immediately.
	if primaryPool != "" {
		if cfg, ok := opts.Pools[primaryPool]; ok && cfg.Unmeasured {
			res.Decision = DecisionWouldPass
			res.SelectedPool = primaryPool
			res.Bypassed = true
			res.Reason = ReasonUnmeasured
			res.Pools = []PoolEvaluation{{
				Pool:       primaryPool,
				Status:     PoolUnmeasured,
				Reason:     ReasonUnmeasured,
				Unmeasured: true,
			}}
			return res
		}
	}

	evals := make([]PoolEvaluation, 0, len(candidates))
	var (
		healthy    []PoolEvaluation
		unmeasured []PoolEvaluation
		exhausted  []PoolEvaluation
		unknowns   []PoolEvaluation
	)

	for _, name := range candidates {
		ev := evaluatePoolLegacy(name, snap, opts)
		evals = append(evals, ev)
		switch ev.Status {
		case PoolHealthy:
			healthy = append(healthy, ev)
		case PoolUnmeasured:
			unmeasured = append(unmeasured, ev)
		case PoolExhausted:
			exhausted = append(exhausted, ev)
		default:
			unknowns = append(unknowns, ev)
		}
	}
	res.Pools = evals

	// Prefer healthy measured pools, selecting the one with the soonest weekly reset (legacy Before).
	if len(healthy) > 0 {
		chosen := pickSoonestWeeklyReset(healthy)
		res.Decision = DecisionWouldPass
		res.SelectedPool = chosen.Pool
		return res
	}

	// Legacy behavior (preserved per Decision 2 & Item A): fall back to an unmeasured alt pool.
	if len(unmeasured) > 0 {
		res.Decision = DecisionWouldPass
		res.SelectedPool = unmeasured[0].Pool
		res.Bypassed = true
		res.Reason = ReasonUnmeasured
		return res
	}

	if len(unknowns) > 0 {
		res.Decision = DecisionUnknown
		res.Reason = unknowns[0].Reason
		if len(exhausted) > 0 && evals[0].Status == PoolExhausted {
			res.BlockedBy = evals[0].BlockedBy
		}
		return res
	}

	res.Decision = DecisionWouldHold
	if len(exhausted) > 0 {
		res.BlockedBy = exhausted[0].BlockedBy
	}
	return res
}

func evaluateGateV1(primaryPool string, altPools []string, snap *Snapshot, opts EvalOptions) GateEvaluation {
	now := opts.effectiveNow()
	primaryPool = strings.TrimSpace(primaryPool)
	cleanedAlts := cleanPools(altPools, primaryPool)
	candidates := buildCandidates(primaryPool, cleanedAlts)

	res := GateEvaluation{
		Format:        FormatV1,
		RequestedPool: primaryPool,
		AltPools:      cleanedAlts,
		DecidedAt:     now,
	}
	if snap != nil {
		refTime := snap.GeneratedAt
		if refTime.IsZero() {
			refTime = snap.MeasuredAt
		}
		if !refTime.IsZero() {
			t := refTime
			res.MeasuredAt = &t
			ageSec := int64(now.Sub(refTime).Seconds())
			res.AgeSeconds = &ageSec
		}
	}

	// Step 0: no candidate pools -> outside gate.
	if len(candidates) == 0 {
		res.Decision = DecisionUnknown
		res.Reason = ReasonNoCandidatePools
		return res
	}

	// Step 1 (registration): verify ALL candidates exist in fleet.yaml BEFORE bypass or snapshot evaluation (Item B).
	if opts.CheckPoolConfig && !opts.HeldRelease {
		var missing []string
		for _, c := range candidates {
			if _, ok := opts.Pools[c]; !ok {
				missing = append(missing, c)
			}
		}
		if len(missing) > 0 {
			res.Decision = DecisionUnknown
			res.Reason = ReasonPoolNotConfigured
			res.UndefinedPools = missing
			res.Detail = "unknown_pool: " + strings.Join(missing, ", ")
			evals := make([]PoolEvaluation, 0, len(candidates))
			for _, c := range candidates {
				if cfg, ok := opts.Pools[c]; !ok {
					evals = append(evals, PoolEvaluation{
						Pool:   c,
						Status: PoolUnknown,
						Reason: ReasonPoolNotConfigured,
					})
				} else if cfg.Unmeasured {
					evals = append(evals, PoolEvaluation{
						Pool:       c,
						Status:     PoolUnmeasured,
						Reason:     ReasonUnmeasured,
						Unmeasured: true,
					})
				} else {
					evals = append(evals, PoolEvaluation{
						Pool:   c,
						Status: PoolUnknown,
						Reason: ReasonPoolNotConfigured,
						Detail: "aborted due to undefined sibling pool",
					})
				}
			}
			res.Pools = evals
			return res
		}
	}

	// Step 2: Primary unmeasured bypass OR zero-measured-candidates (all unmeasured alts) bypass.
	// In HeldRelease mode (§4.2.C + Item B), if primaryPool is defined & unmeasured, allow bypass
	// even when some alt_pools were removed from fleet.yaml (recording UndefinedPools for WARNING).
	if primaryPool != "" {
		if cfg, ok := opts.Pools[primaryPool]; ok && cfg.Unmeasured {
			res.Decision = DecisionWouldPass
			res.SelectedPool = primaryPool
			res.Bypassed = true
			res.Reason = ReasonUnmeasured
			res.Pools = []PoolEvaluation{{
				Pool:       primaryPool,
				Status:     PoolUnmeasured,
				Reason:     ReasonUnmeasured,
				Unmeasured: true,
			}}
			if opts.CheckPoolConfig && opts.HeldRelease {
				for _, alt := range cleanedAlts {
					if _, defined := opts.Pools[alt]; !defined {
						res.UndefinedPools = append(res.UndefinedPools, alt)
					}
				}
			}
			return res
		}
	} else {
		// primaryPool == "": check if ALL candidates are defined and unmeasured.
		allUnmeasured := true
		for _, c := range candidates {
			cfg, ok := opts.Pools[c]
			if !ok || !cfg.Unmeasured {
				allUnmeasured = false
				break
			}
		}
		if allUnmeasured {
			res.Decision = DecisionWouldPass
			res.SelectedPool = candidates[0]
			res.Bypassed = true
			res.Reason = ReasonUnmeasured
			evals := make([]PoolEvaluation, 0, len(candidates))
			for _, c := range candidates {
				evals = append(evals, PoolEvaluation{
					Pool:       c,
					Status:     PoolUnmeasured,
					Reason:     ReasonUnmeasured,
					Unmeasured: true,
				})
			}
			res.Pools = evals
			return res
		}
	}

	// Step 3: evaluate all candidates against snapshot and config.
	evals := make([]PoolEvaluation, 0, len(candidates))
	var (
		healthy    []PoolEvaluation
		unmeasured []PoolEvaluation
		exhausted  []PoolEvaluation
		unknowns   []PoolEvaluation
		undefined  []string
	)

	for _, name := range candidates {
		ev := evaluatePoolV1(name, snap, opts)
		evals = append(evals, ev)
		if ev.Reason == ReasonPoolNotConfigured {
			undefined = append(undefined, name)
		}
		switch ev.Status {
		case PoolHealthy:
			healthy = append(healthy, ev)
		case PoolUnmeasured:
			unmeasured = append(unmeasured, ev)
		case PoolExhausted:
			exhausted = append(exhausted, ev)
		default:
			unknowns = append(unknowns, ev)
		}
	}
	res.Pools = evals
	if len(undefined) > 0 {
		res.UndefinedPools = undefined
	}

	// Step 4: prefer healthy measured candidates using §3.5 future non-approx weekly reset ordering.
	if len(healthy) > 0 {
		chosen := pickSoonestWeeklyResetV1(healthy, now)
		res.Decision = DecisionWouldPass
		res.SelectedPool = chosen.Pool
		return res
	}

	// Step 5 (Decision 6): detour to unmeasured alt ONLY when there is >= 1 measured candidate
	// and EVERY measured candidate is definitively exhausted (zero unknowns).
	measuredCount := len(healthy) + len(exhausted) + len(unknowns)
	if measuredCount > 0 && len(unknowns) == 0 && len(exhausted) == measuredCount && len(unmeasured) > 0 {
		res.Decision = DecisionWouldPass
		res.SelectedPool = unmeasured[0].Pool
		res.Bypassed = true
		res.Reason = ReasonUnmeasured
		return res
	}

	// Step 6: Hold / Unknown aggregation.
	if len(unknowns) > 0 {
		primary := pickPrimaryUnknown(unknowns)
		res.Decision = DecisionUnknown
		res.Reason = primary.Reason
		res.Detail = primary.Detail
		res.PoolError = primary.PoolError
		if primary.AgeSeconds != nil {
			res.AgeSeconds = primary.AgeSeconds
		}
		return res
	}

	// All measured candidates are definitively exhausted and no unmeasured alt exists.
	res.Decision = DecisionWouldHold
	if len(exhausted) > 0 {
		res.BlockedBy = exhausted[0].BlockedBy
	}
	return res
}

func unknownReasonRank(ev PoolEvaluation) int {
	switch ev.Reason {
	case ReasonSnapshotError:
		return 10
	case ReasonStale:
		if ev.Detail == "snapshot_stale" {
			return 20
		}
		return 60
	case ReasonPoolNotFound, ReasonPoolNotConfigured:
		return 30
	case ReasonTimestampFuture:
		return 40
	case ReasonTimestampInconsistent:
		return 41
	case ReasonCollectorError:
		return 50
	case ReasonShortWindowMismatch:
		return 70
	case ReasonShortWindowMissing, ReasonShortWindowNull:
		return 90
	default:
		return 99
	}
}

func pickPrimaryUnknown(unknowns []PoolEvaluation) PoolEvaluation {
	best := unknowns[0]
	bestRank := unknownReasonRank(best)
	for _, cand := range unknowns[1:] {
		r := unknownReasonRank(cand)
		if r < bestRank {
			best = cand
			bestRank = r
		}
	}
	return best
}

// EvaluateFromFile loads usageFile and evaluates the gate for primaryPool and altPools
// according to opts.Format (defaulting to "legacy").
func EvaluateFromFile(usageFile, primaryPool string, altPools []string, opts EvalOptions) (GateEvaluation, error) {
	primaryPool = strings.TrimSpace(primaryPool)
	cleanedAlts := cleanPools(altPools, primaryPool)
	candidates := buildCandidates(primaryPool, cleanedAlts)
	format := opts.effectiveFormat()

	if len(candidates) == 0 {
		return EvaluateGate(primaryPool, altPools, nil, opts), nil
	}

	if format == FormatV1 {
		// Step 1 (registration): reject/record undefined pools before reading file (Item B).
		if opts.CheckPoolConfig && !opts.HeldRelease {
			for _, c := range candidates {
				if _, ok := opts.Pools[c]; !ok {
					return EvaluateGate(primaryPool, altPools, nil, opts), nil
				}
			}
		}
		// Step 2: bypass if primary is unmeasured OR (primary empty and all candidates defined & unmeasured).
		if primaryPool != "" {
			if cfg, ok := opts.Pools[primaryPool]; ok && cfg.Unmeasured {
				return EvaluateGate(primaryPool, altPools, nil, opts), nil
			}
		} else {
			allUnmeasured := true
			for _, c := range candidates {
				cfg, ok := opts.Pools[c]
				if !ok || !cfg.Unmeasured {
					allUnmeasured = false
					break
				}
			}
			if allUnmeasured {
				return EvaluateGate(primaryPool, altPools, nil, opts), nil
			}
		}
		// In HeldRelease mode, if all candidates were deleted, no need to read file.
		if opts.CheckPoolConfig && opts.HeldRelease {
			anyDefined := false
			for _, c := range candidates {
				if _, ok := opts.Pools[c]; ok {
					anyDefined = true
					break
				}
			}
			if !anyDefined {
				return EvaluateGate(primaryPool, altPools, nil, opts), nil
			}
		}
	} else {
		// Legacy bypass check.
		if primaryPool != "" {
			if cfg, ok := opts.Pools[primaryPool]; ok && cfg.Unmeasured {
				return EvaluateGate(primaryPool, altPools, nil, opts), nil
			}
		}
	}

	snap, err := LoadUsageFileWithFormat(usageFile, format)
	if err != nil {
		eval := EvaluateGate(primaryPool, altPools, nil, opts)
		if !eval.Bypassed {
			eval.Decision = DecisionUnknown
			if format == FormatV1 {
				eval.Reason = ReasonSnapshotError
				eval.Detail = err.Error()
			} else {
				eval.Reason = fmt.Sprintf("%s: %v", ReasonSnapshotError, err)
			}
		}
		return eval, err
	}
	return EvaluateGate(primaryPool, altPools, snap, opts), nil
}

// ShouldHoldOnEnforce reports whether eval should hold the job in gate_mode=enforce.
//
// In Schema v1 (eval.Format == "v1"): fail-closed — any non-pass verdict (exhausted or any
// unknown reason, except no_candidate_pools) holds the job.
//
// In Legacy mode (eval.Format == "" or "legacy"): preserves legacy compatibility — holds
// only when all candidate pools are exhausted or stale; provisional unknown (short_window_null),
// pool_not_found, and snapshot_error proceed without holding.
func ShouldHoldOnEnforce(eval GateEvaluation) bool {
	if eval.Bypassed || eval.Decision == DecisionWouldPass {
		return false
	}
	if eval.Reason == ReasonNoCandidatePools {
		return false
	}
	if eval.Format == FormatV1 {
		return true
	}
	if eval.Decision == DecisionWouldHold {
		return true
	}
	if len(eval.Pools) == 0 {
		return false
	}
	for _, p := range eval.Pools {
		if p.Status == PoolExhausted {
			continue
		}
		if p.Status == PoolUnknown && p.Reason == ReasonStale {
			continue
		}
		return false
	}
	return true
}

// CompareLegacyWithV1 evaluates the same snapshot and candidates under v1 strict rules
// (without rejecting undefined pools) and returns human-readable diagnostics describing
// any behavioral difference from legacyEval (for Decision 2 legacy_compat_diff logging).
func CompareLegacyWithV1(primaryPool string, altPools []string, snap *Snapshot, opts EvalOptions, legacyEval GateEvaluation) []string {
	v1Opts := opts
	v1Opts.Format = FormatV1
	v1Opts.CheckPoolConfig = false
	v1Eval := EvaluateGate(primaryPool, altPools, snap, v1Opts)

	var diffs []string
	legacyHold := ShouldHoldOnEnforce(legacyEval)
	v1Hold := ShouldHoldOnEnforce(v1Eval)
	if legacyHold != v1Hold {
		diffs = append(diffs, fmt.Sprintf("enforce_hold_diff: legacy_hold=%t (decision=%s reason=%s) vs v1_hold=%t (decision=%s reason=%s)",
			legacyHold, legacyEval.Decision, legacyEval.Reason, v1Hold, v1Eval.Decision, v1Eval.Reason))
	}
	if legacyEval.Decision == DecisionWouldPass && v1Eval.Decision == DecisionWouldPass &&
		legacyEval.SelectedPool != v1Eval.SelectedPool {
		diffs = append(diffs, fmt.Sprintf("selected_pool_diff: legacy=%s vs v1=%s (weekly reset ordering or unmeasured detour)",
			legacyEval.SelectedPool, v1Eval.SelectedPool))
	}
	return diffs
}

// FormatHoldReason builds a concise machine- and human-readable hold reason from eval.
func FormatHoldReason(eval GateEvaluation) string {
	if eval.Decision == DecisionWouldHold {
		if eval.BlockedBy != "" {
			return "exhausted:" + eval.BlockedBy
		}
		return "exhausted"
	}
	if eval.Reason != "" {
		detail := eval.Detail
		if eval.Reason == ReasonCollectorError && detail != "" {
			detail = SanitizeCollectorError(detail)
		}
		if detail != "" && !strings.Contains(eval.Reason, detail) {
			return eval.Reason + ": " + detail
		}
		return eval.Reason
	}
	return string(eval.Decision)
}

// FormatDecisionReason formats the reason stored on gate_decisions rows so diagnostic detail
// (e.g. format_mismatch, snapshot_stale, pool_stale, sanitized collector error) is preserved.
func FormatDecisionReason(eval GateEvaluation) string {
	if eval.Reason == "" {
		return ""
	}
	detail := eval.Detail
	if eval.Reason == ReasonCollectorError && detail != "" {
		detail = SanitizeCollectorError(detail)
	}
	if detail != "" && !strings.Contains(eval.Reason, detail) {
		return eval.Reason + ": " + detail
	}
	return eval.Reason
}

// FormatStaleUnarrivedMessage formats the neutral non-attributing stale warning (§4.3.2 / §5.2.3).
func FormatStaleUnarrivedMessage(age, staleAfter time.Duration) string {
	ageMin := int(age.Minutes())
	boundMin := int(staleAfter.Minutes())
	if ageMin < 0 {
		ageMin = 0
	}
	return fmt.Sprintf("new snapshot has not arrived (elapsed %dm > stale_after %dm)", ageMin, boundMin)
}

type poolSnapshotSummary struct {
	Pool                   string           `json:"pool"`
	Status                 PoolHealthStatus `json:"status"`
	BlockedBy              string           `json:"blocked_by,omitempty"`
	Reason                 string           `json:"reason,omitempty"`
	Detail                 string           `json:"detail,omitempty"`
	PoolError              string           `json:"pool_error,omitempty"`
	AgeSeconds             *int64           `json:"age_seconds,omitempty"`
	WeeklyRemainingPercent *int             `json:"weekly_remaining_percent,omitempty"`
	WeeklyResetAt          *time.Time       `json:"weekly_reset_at,omitempty"`
	WeeklyResetAtApprox    bool             `json:"weekly_reset_at_approx,omitempty"`
	ShortRemainingPercent  *int             `json:"short_remaining_percent,omitempty"`
	ShortResetAt           *time.Time       `json:"short_reset_at,omitempty"`
	ShortResetAtApprox     bool             `json:"short_reset_at_approx,omitempty"`
	ShortWindow            string           `json:"short_window,omitempty"`
	MeasuredAt             *time.Time       `json:"measured_at,omitempty"`
	ServiceStale           bool             `json:"service_stale,omitempty"`
}

type evaluationSnapshotSummary struct {
	Format     string                `json:"format,omitempty"`
	MeasuredAt *time.Time            `json:"measured_at,omitempty"`
	DecidedAt  time.Time             `json:"decided_at"`
	Decision   DecisionKind          `json:"decision"`
	BlockedBy  string                `json:"blocked_by,omitempty"`
	Reason     string                `json:"reason,omitempty"`
	Detail     string                `json:"detail,omitempty"`
	PoolError  string                `json:"pool_error,omitempty"`
	AgeSeconds *int64                `json:"age_seconds,omitempty"`
	Pools      []poolSnapshotSummary `json:"pools,omitempty"`
}

// SummarizeEvaluation serializes a compact JSON summary of the snapshot and pool
// states observed at gate decision time for persistence on held_jobs.
func SummarizeEvaluation(eval GateEvaluation) string {
	detail := eval.Detail
	poolErr := SanitizeCollectorError(eval.PoolError)
	if eval.Reason == ReasonCollectorError && detail != "" {
		detail = SanitizeCollectorError(detail)
	}
	summary := evaluationSnapshotSummary{
		Format:     eval.Format,
		MeasuredAt: eval.MeasuredAt,
		DecidedAt:  eval.DecidedAt,
		Decision:   eval.Decision,
		BlockedBy:  eval.BlockedBy,
		Reason:     eval.Reason,
		Detail:     detail,
		PoolError:  poolErr,
		AgeSeconds: eval.AgeSeconds,
	}
	if len(eval.Pools) > 0 {
		summary.Pools = make([]poolSnapshotSummary, 0, len(eval.Pools))
		for _, p := range eval.Pools {
			pDetail := p.Detail
			pPoolErr := SanitizeCollectorError(p.PoolError)
			if p.Reason == ReasonCollectorError && pDetail != "" {
				pDetail = SanitizeCollectorError(pDetail)
			}
			ps := poolSnapshotSummary{
				Pool:       p.Pool,
				Status:     p.Status,
				BlockedBy:  p.BlockedBy,
				Reason:     p.Reason,
				Detail:     pDetail,
				PoolError:  pPoolErr,
				AgeSeconds: p.AgeSeconds,
			}
			if p.Snapshot != nil {
				if !p.Snapshot.WeeklyAbsent {
					w := p.Snapshot.WeeklyRemainingPercent
					ps.WeeklyRemainingPercent = &w
				}
				ps.WeeklyResetAt = p.Snapshot.WeeklyResetAt
				ps.WeeklyResetAtApprox = p.Snapshot.WeeklyResetAtApprox
				ps.ShortRemainingPercent = p.Snapshot.ShortRemainingPercent
				ps.ShortResetAt = p.Snapshot.ShortResetAt
				ps.ShortResetAtApprox = p.Snapshot.ShortResetAtApprox
				ps.ShortWindow = p.Snapshot.ShortWindow
				if !p.Snapshot.MeasuredAt.IsZero() {
					mt := p.Snapshot.MeasuredAt
					ps.MeasuredAt = &mt
				}
				ps.ServiceStale = p.Snapshot.ServiceStale
			}
			summary.Pools = append(summary.Pools, ps)
		}
	}
	b, err := json.Marshal(summary)
	if err != nil {
		return ""
	}
	return string(b)
}

const (
	// ModelFamilyGemini classifies gemini-* models.
	ModelFamilyGemini = "gemini"
	// ModelFamilyClaude classifies claude-* models.
	ModelFamilyClaude = "claude"
	// ModelFamilyGPT classifies gpt-* models.
	ModelFamilyGPT = "gpt"
	// ModelFamilyUnknown classifies models whose prefix does not match a known family.
	ModelFamilyUnknown = "unknown"
)

// ModelFamily derives the canonical model family from a model ID (docs/11 §3):
// gemini-* -> gemini, claude-* -> claude, gpt-* -> gpt, otherwise unknown.
func ModelFamily(modelID string) string {
	m := strings.ToLower(strings.TrimSpace(modelID))
	switch {
	case m == ModelFamilyGemini || strings.HasPrefix(m, "gemini-"):
		return ModelFamilyGemini
	case m == ModelFamilyClaude || strings.HasPrefix(m, "claude-"):
		return ModelFamilyClaude
	case m == ModelFamilyGPT || strings.HasPrefix(m, "gpt-"):
		return ModelFamilyGPT
	default:
		return ModelFamilyUnknown
	}
}

// ResolvePoolByAppliesTo finds the first pool (in sorted pool-name order) whose
// applies_to entries match the given agent and model. Returns "" if none matches.
func ResolvePoolByAppliesTo(pools map[string]config.PoolConfig, agent, model string) string {
	agent = strings.TrimSpace(agent)
	model = strings.TrimSpace(model)
	if agent == "" || len(pools) == 0 {
		return ""
	}

	names := make([]string, 0, len(pools))
	for name := range pools {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		cfg := pools[name]
		for _, target := range cfg.AppliesTo {
			if !strings.EqualFold(strings.TrimSpace(target.Agent), agent) {
				continue
			}
			family := strings.TrimSpace(target.ModelFamily)
			if family == "" || matchesModelFamily(model, family) {
				return name
			}
		}
	}
	return ""
}

func matchesModelFamily(model, family string) bool {
	canonical := ModelFamily(model)
	if canonical == ModelFamilyUnknown {
		return false
	}
	f := strings.ToLower(strings.TrimSpace(family))
	if f == "" {
		return false
	}
	if canonical == f {
		return true
	}
	tokens := strings.FieldsFunc(f, func(r rune) bool {
		return r == '-' || r == '/' || r == ',' || r == '_'
	})
	for _, tok := range tokens {
		if tok == canonical {
			return true
		}
	}
	return false
}

func pickSoonestWeeklyReset(healthy []PoolEvaluation) PoolEvaluation {
	best := healthy[0]
	for _, cand := range healthy[1:] {
		if resetBefore(cand.Snapshot, best.Snapshot) {
			best = cand
		}
	}
	return best
}

func resetBefore(a, b *PoolSnapshot) bool {
	var ta, tb *time.Time
	if a != nil {
		ta = a.WeeklyResetAt
	}
	if b != nil {
		tb = b.WeeklyResetAt
	}
	if ta == nil {
		return false
	}
	if tb == nil {
		return true
	}
	return ta.Before(*tb)
}

// pickSoonestWeeklyResetV1 implements §3.5 weekly reset ordering:
//   - Valid measured future resets (reset_at != nil && reset_at > now && !approx) always precede unknown resets.
//   - Among valid measured future resets, earlier reset_at wins.
//   - Ties (equal times or both unknown) preserve candidate declaration order.
func pickSoonestWeeklyResetV1(healthy []PoolEvaluation, now time.Time) PoolEvaluation {
	best := healthy[0]
	for _, cand := range healthy[1:] {
		if resetBeforeV1(cand.Snapshot, best.Snapshot, now) {
			best = cand
		}
	}
	return best
}

func validMeasuredFutureReset(ps *PoolSnapshot, now time.Time) (*time.Time, bool) {
	if ps == nil || ps.WeeklyResetAt == nil || ps.WeeklyResetAtApprox {
		return nil, false
	}
	if !ps.WeeklyResetAt.After(now) {
		return nil, false
	}
	return ps.WeeklyResetAt, true
}

func resetBeforeV1(a, b *PoolSnapshot, now time.Time) bool {
	ta, okA := validMeasuredFutureReset(a, now)
	tb, okB := validMeasuredFutureReset(b, now)
	if !okA {
		return false
	}
	if !okB {
		return true
	}
	return ta.Before(*tb)
}

func cleanPools(altPools []string, primary string) []string {
	seen := make(map[string]bool)
	if primary != "" {
		seen[primary] = true
	}
	var out []string
	for _, p := range altPools {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

func buildCandidates(primary string, alts []string) []string {
	out := make([]string, 0, 1+len(alts))
	if primary != "" {
		out = append(out, primary)
	}
	out = append(out, alts...)
	return out
}

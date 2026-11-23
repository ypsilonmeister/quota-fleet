package quota

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/config"
)

// EvalOptions controls gate evaluation timing, staleness threshold, and pool metadata.
type EvalOptions struct {
	// Now is the evaluation timestamp. Defaults to time.Now().UTC() when zero.
	Now time.Time
	// StaleAfter is the maximum allowed snapshot age. Defaults to DefaultStaleAfter (45m).
	StaleAfter time.Duration
	// Pools provides pool metadata (e.g. unmeasured: true).
	Pools map[string]config.PoolConfig
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
//
// Health rules (matching the legacy quota-gate spec):
//   - Unmeasured pools (config pools.<name>.unmeasured == true) bypass measurement.
//   - Snapshots older than stale_after (default 45m), or pools flagged service_stale,
//     are NOT treated as healthy; they evaluate as unknown with reason="stale".
//   - Healthy iff weekly_remaining_percent > 0 AND short_remaining_percent > 0.
//   - Exhausted if weekly_remaining_percent <= 0 (blocked_by="weekly") or
//     short_remaining_percent <= 0 (blocked_by="5h"), or both ("weekly,5h").
//   - PROVISIONAL (pending spec decision, see docs/06-implementation-design.md §7-6):
//     When weekly_remaining_percent > 0 and short_remaining_percent is nil (for example,
//     immediately after a 5h window reset or when subText could not be parsed), the pool
//     is treated as "unknown" (neither healthy nor exhausted) with reason="short_window_null"
//     so callers and dry-run logs can distinguish it clearly until the final policy is decided.
func EvaluatePool(pool string, snap *Snapshot, opts EvalOptions) PoolEvaluation {
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

	// PROVISIONAL: short_remaining_percent == nil is unresolved in the spec (docs/06 §7-6).
	// If weekly is already 0, the pool is definitely exhausted by weekly; otherwise we classify
	// it as unknown ("short_window_null") rather than assuming healthy or exhausted.
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

// EvaluateGate evaluates primaryPool and altPools against snap and picks the best
// healthy pool (preferring the earliest weekly_reset_at to spend soon-expiring quota first).
func EvaluateGate(primaryPool string, altPools []string, snap *Snapshot, opts EvalOptions) GateEvaluation {
	now := opts.effectiveNow()
	primaryPool = strings.TrimSpace(primaryPool)
	cleanedAlts := cleanPools(altPools, primaryPool)
	candidates := buildCandidates(primaryPool, cleanedAlts)

	res := GateEvaluation{
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
		ev := EvaluatePool(name, snap, opts)
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

	// Prefer healthy measured pools, selecting the one with the soonest weekly reset.
	if len(healthy) > 0 {
		chosen := pickSoonestWeeklyReset(healthy)
		res.Decision = DecisionWouldPass
		res.SelectedPool = chosen.Pool
		return res
	}

	// If no measured candidate is healthy, fall back to an explicitly declared unmeasured alt pool.
	if len(unmeasured) > 0 {
		res.Decision = DecisionWouldPass
		res.SelectedPool = unmeasured[0].Pool
		res.Bypassed = true
		res.Reason = ReasonUnmeasured
		return res
	}

	// No healthy candidates remain. If any candidate is unknown (e.g. stale snapshot or
	// provisional short_window_null), we cannot definitively conclude exhausted vs healthy.
	if len(unknowns) > 0 {
		res.Decision = DecisionUnknown
		res.Reason = unknowns[0].Reason
		if len(exhausted) > 0 && evals[0].Status == PoolExhausted {
			res.BlockedBy = evals[0].BlockedBy
		}
		return res
	}

	// All candidates are definitively exhausted.
	res.Decision = DecisionWouldHold
	if len(exhausted) > 0 {
		res.BlockedBy = exhausted[0].BlockedBy
	}
	return res
}

// EvaluateFromFile loads usageFile and evaluates the gate for primaryPool and altPools.
// If the primary pool is configured as unmeasured, usageFile is not required.
// If loading usageFile fails for measured pools, an unknown GateEvaluation is returned
// along with the underlying error so dry-run callers can still record the unknown verdict.
func EvaluateFromFile(usageFile, primaryPool string, altPools []string, opts EvalOptions) (GateEvaluation, error) {
	primaryPool = strings.TrimSpace(primaryPool)
	if primaryPool != "" {
		if cfg, ok := opts.Pools[primaryPool]; ok && cfg.Unmeasured {
			return EvaluateGate(primaryPool, altPools, nil, opts), nil
		}
	}

	snap, err := LoadUsageFile(usageFile)
	if err != nil {
		eval := EvaluateGate(primaryPool, altPools, nil, opts)
		if !eval.Bypassed {
			eval.Decision = DecisionUnknown
			eval.Reason = fmt.Sprintf("%s: %v", ReasonSnapshotError, err)
		}
		return eval, err
	}
	return EvaluateGate(primaryPool, altPools, snap, opts), nil
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
	m := strings.ToLower(strings.TrimSpace(model))
	f := strings.ToLower(strings.TrimSpace(family))
	if m == "" || f == "" {
		return false
	}
	if m == f || strings.Contains(m, f) {
		return true
	}
	// Support composite family tags like "claude-gpt" by matching any component token.
	tokens := strings.FieldsFunc(f, func(r rune) bool {
		return r == '-' || r == '/' || r == ',' || r == '_'
	})
	for _, tok := range tokens {
		if len(tok) < 2 {
			continue
		}
		if strings.Contains(m, tok) {
			return true
		}
		if tok == "claude" && (strings.Contains(m, "sonnet") || strings.Contains(m, "opus") || strings.Contains(m, "haiku")) {
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

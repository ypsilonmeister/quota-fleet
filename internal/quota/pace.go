package quota

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/config"
)

const (
	weeklyPaceFloor = 2
	shortPaceFloor  = 5
)

type PaceCandidate struct {
	Pool    string
	Pace    float64
	Floored bool
	InBand  bool
	Load    int
}

type PaceRanking struct {
	// Index is the winning index in the input evaluations, or -1 if none are eligible.
	Index      int
	Mode       string
	Candidates []PaceCandidate
}

// RankHealthy ranks measured, healthy candidates in declaration order.
// The caller owns load accounting so a failed release cannot affect later comparisons.
func RankHealthy(evals []PoolEvaluation, opts EvalOptions, cfg config.PaceConfig, load map[string]int) PaceRanking {
	result := PaceRanking{Index: -1, Mode: "equal"}
	now := opts.effectiveNow()
	var indices []int
	bestOpen, bestAny := math.Inf(-1), math.Inf(-1)
	for i, ev := range evals {
		if ev.Status != PoolHealthy || ev.Snapshot == nil || ev.Snapshot.WeeklyAbsent {
			continue
		}
		pool := opts.Pools[ev.Pool]
		pace, floored := poolPace(ev.Pool, *ev.Snapshot, pool, now)
		result.Candidates = append(result.Candidates, PaceCandidate{Pool: ev.Pool, Pace: pace, Floored: floored, Load: load[ev.Pool]})
		indices = append(indices, i)
		bestAny = max(bestAny, pace)
		if !floored {
			bestOpen = max(bestOpen, pace)
		}
	}
	allFloored := math.IsInf(bestOpen, -1)
	best := bestOpen
	if allFloored {
		best = bestAny
	}
	minPace := math.Inf(-1)
	if best > cfg.EffectiveUrgentPace() {
		result.Mode = "urgent"
		minPace = best * (1 - cfg.EffectiveTie())
	}
	bestLoad := 0
	for i := range result.Candidates {
		c := &result.Candidates[i]
		c.InBand = (allFloored || !c.Floored) && c.Pace >= minPace
		if c.InBand && (result.Index < 0 || c.Load < bestLoad) {
			result.Index, bestLoad = indices[i], c.Load
		}
	}
	return result
}

func poolPace(name string, ps PoolSnapshot, p config.PoolConfig, now time.Time) (float64, bool) {
	period := p.EffectiveWeeklyHours()
	remaining, hoursLeft := ps.WeeklyRemainingPercent, period
	if reset := ps.WeeklyResetAt; reset != nil {
		if reset.Before(now) {
			remaining = 100
		} else if !ps.WeeklyResetAtApprox {
			hoursLeft = reset.Sub(now).Hours()
		}
	}
	threshold := float64(weeklyPaceFloor)
	if hoursLeft > 2 {
		threshold = max(threshold, p.ReservePercent)
	}
	headroom := float64(remaining) - threshold
	shortFloored := false
	if p.EffectiveShortWindow() && ps.EffectiveShortWindow() == DefaultShortWindowLabel &&
		(ps.ShortApplicable == nil || *ps.ShortApplicable) && ps.ShortRemainingPercent != nil {
		reset := ps.ShortResetAt
		active := reset != nil && !reset.Before(now) || reset == nil && name != PoolClaude
		shortFloored = active && *ps.ShortRemainingPercent <= shortPaceFloor
	}
	return (headroom / 100) * (period / max(hoursLeft, .1)), headroom <= 0 || shortFloored
}

// Describe records the selection comparison and the inputs that determined it.
func (r PaceRanking) Describe(current, shadow string) string {
	label := "pace_shadow"
	if current != shadow {
		label = "pace_diff"
	}
	candidates := make([]string, 0, len(r.Candidates))
	for _, c := range r.Candidates {
		candidates = append(candidates, fmt.Sprintf("%s(pace=%.4f floored=%t in_band=%t load=%d)", c.Pool, c.Pace, c.Floored, c.InBand, c.Load))
	}
	return fmt.Sprintf("%s current=%s shadow=%s mode=%s candidates=[%s]", label, current, shadow, r.Mode, strings.Join(candidates, ", "))
}

package quota

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/config"
)

func TestPaceDesignExamples(t *testing.T) {
	now := time.Date(2026, 11, 28, 0, 0, 0, 0, time.UTC)
	type window struct {
		pool             string
		remaining, hours int
		short            int
		pace             float64
	}
	const codex, agy, claude = PoolCodex, PoolAgyGemini, PoolClaude
	tests := []struct {
		name    string
		windows []window
		gate    string
		mode    string
		band    string
		shadow  []string
	}{
		{"1 near reset", []window{{codex, 60, 30, 50, 3.25}, {agy, 70, 100, 50, 1.14}}, codex, "urgent", codex, []string{codex}},
		{"2 almost empty", []window{{codex, 5, 24, 50, .21}, {agy, 80, 72, 50, 1.82}}, codex, "urgent", agy, []string{agy}},
		{"3 surplus", []window{{codex, 40, 120, 50, .53}, {agy, 90, 30, 50, 4.93}, {claude, 50, 60, 50, 1.34}}, agy, "urgent", agy, []string{agy}},
		{"4 equal mode", []window{{codex, 50, 140, 50, .58}, {agy, 60, 150, 50, .65}, {claude, 45, 130, 50, .56}}, claude, "equal", codex + "," + agy + "," + claude, []string{codex}},
		{"5 batch", []window{{codex, 60, 34, 50, 2.87}, {agy, 70, 42, 50, 2.72}, {claude, 80, 140, 50, .94}}, codex, "urgent", codex + "," + agy, []string{codex, agy, codex, agy}},
		{"6 short floor", []window{{codex, 70, 20, 3, 5.71}, {agy, 60, 60, 50, 1.62}}, codex, "urgent", agy, []string{agy}},
		{"7 unknown reset first", []window{{claude, 100, 0, 50, .98}, {codex, 30, 48, 50, .98}}, codex, "equal", claude + "," + codex, []string{claude}},
		{"7 table order", []window{{codex, 30, 48, 50, .98}, {claude, 100, 0, 50, .98}}, codex, "equal", codex + "," + claude, []string{codex}},
		{"8 single", []window{{agy, 70, 100, 50, 1.14}}, agy, "urgent", agy, []string{agy}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snap := &Snapshot{GeneratedAt: now, Pools: make(map[string]PoolSnapshot)}
			opts := EvalOptions{Now: now, Format: FormatV1, Pools: make(map[string]config.PoolConfig)}
			var pools []string
			for _, w := range tt.windows {
				ps := PoolSnapshot{MeasuredAt: now, WeeklyRemainingPercent: w.remaining, ShortRemainingPercent: &w.short}
				if w.hours > 0 {
					reset := now.Add(time.Duration(w.hours) * time.Hour)
					ps.WeeklyResetAt = &reset
				}
				snap.Pools[w.pool] = ps
				opts.Pools[w.pool] = config.PoolConfig{}
				pools = append(pools, w.pool)
			}
			ev := EvaluateGate(pools[0], pools[1:], snap, opts)
			if ev.SelectedPool != tt.gate {
				t.Fatalf("current Gate = %q, want %q, evaluation %+v", ev.SelectedPool, tt.gate, ev)
			}
			load := make(map[string]int)
			for _, wantPool := range tt.shadow {
				rank := RankHealthy(ev.Pools, opts, config.PaceConfig{}, load)
				if rank.Index < 0 || ev.Pools[rank.Index].Pool != wantPool || rank.Mode != tt.mode {
					t.Fatalf("got %+v, want %s in %s mode", rank, wantPool, tt.mode)
				}
				var band []string
				for i, c := range rank.Candidates {
					w := tt.windows[i]
					if c.Pool != w.pool || math.Abs(c.Pace-w.pace) >= .005 || c.Floored != (w.short <= 5) {
						t.Errorf("candidate %+v, want %+v", c, w)
					}
					if c.InBand {
						band = append(band, c.Pool)
					}
				}
				if strings.Join(band, ",") != tt.band {
					t.Errorf("band %v, want %s", band, tt.band)
				}
				load[wantPool]++
			}
		})
	}
}

func TestPoolPaceWindowRules(t *testing.T) {
	now := time.Now()
	past, twoHours, fourHours := now.Add(-time.Second), now.Add(2*time.Hour), now.Add(4*time.Hour)
	short := 5
	noShort := false
	tests := []struct {
		name    string
		ps      PoolSnapshot
		cfg     config.PoolConfig
		pool    string
		want    float64
		floored bool
	}{
		{"default week and null reset", PoolSnapshot{WeeklyRemainingPercent: 50}, config.PoolConfig{}, PoolCodex, .48, false},
		{"custom week and reserve", PoolSnapshot{WeeklyRemainingPercent: 10, WeeklyResetAt: &fourHours}, config.PoolConfig{WeeklyHours: 100, ReservePercent: 10}, PoolCodex, 0, true},
		{"reserve waived at two hours", PoolSnapshot{WeeklyRemainingPercent: 10, WeeklyResetAt: &twoHours}, config.PoolConfig{WeeklyHours: 100, ReservePercent: 10}, PoolCodex, 4, false},
		{"approx reset uses full week", PoolSnapshot{WeeklyRemainingPercent: 50, WeeklyResetAt: &twoHours, WeeklyResetAtApprox: true}, config.PoolConfig{ReservePercent: 10}, PoolCodex, .40, false},
		{"expired resets refill both windows", PoolSnapshot{WeeklyResetAt: &past, ShortRemainingPercent: &short, ShortResetAt: &past}, config.PoolConfig{}, PoolCodex, .98, false},
		{"exact reset clamps denominator", PoolSnapshot{WeeklyRemainingPercent: 3, WeeklyResetAt: &now}, config.PoolConfig{}, PoolCodex, 16.8, false},
		{"weekly floor inclusive", PoolSnapshot{WeeklyRemainingPercent: 2}, config.PoolConfig{}, PoolCodex, 0, true},
		{"five hour floor inclusive", PoolSnapshot{WeeklyRemainingPercent: 50, ShortRemainingPercent: &short, ShortResetAt: &twoHours}, config.PoolConfig{}, PoolCodex, .48, true},
		{"one hour excluded", PoolSnapshot{WeeklyRemainingPercent: 50, ShortRemainingPercent: &short, ShortResetAt: &twoHours, ShortWindow: "1h"}, config.PoolConfig{}, PoolAgyGemini, .48, false},
		{"disabled five hour window", PoolSnapshot{WeeklyRemainingPercent: 50, ShortRemainingPercent: &short, ShortResetAt: &twoHours}, config.PoolConfig{ShortWindow: &noShort}, PoolCodex, .48, false},
		{"unstarted Claude window", PoolSnapshot{WeeklyRemainingPercent: 50, ShortRemainingPercent: &short}, config.PoolConfig{}, PoolClaude, .48, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pace, floored := poolPace(tt.pool, tt.ps, tt.cfg, now)
			if math.Abs(pace-tt.want) > 1e-9 || floored != tt.floored {
				t.Errorf("got pace=%v floored=%v, want pace=%v floored=%v", pace, floored, tt.want, tt.floored)
			}
		})
	}
}

func TestPaceSoftFloorAndHealth(t *testing.T) {
	p := &PoolSnapshot{WeeklyRemainingPercent: 2}
	evals := []PoolEvaluation{
		{Pool: "cooling", Status: PoolCooldown, Snapshot: p},
		{Pool: "unmeasured", Status: PoolUnmeasured, Snapshot: p},
		{Pool: "unknown", Status: PoolUnknown, Snapshot: p},
		{Pool: "exhausted", Status: PoolExhausted, Snapshot: p},
		{Pool: "first", Status: PoolHealthy, Snapshot: p},
		{Pool: "second", Status: PoolHealthy, Snapshot: p},
	}
	cfg := config.PaceConfig{}
	if rank := RankHealthy(evals[:4], EvalOptions{}, cfg, nil); rank.Index != -1 {
		t.Fatalf("non-healthy candidate selected: %+v", rank)
	}
	if rank := RankHealthy(evals, EvalOptions{}, cfg, map[string]int{"first": 1}); rank.Index != 5 || len(rank.Candidates) != 2 {
		t.Fatalf("all floored with first loaded: %+v", rank)
	}
}

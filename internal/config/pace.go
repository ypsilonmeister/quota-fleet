package config

import "strings"

// PaceConfig controls shadow measurement for Selector and Gate release.
type PaceConfig struct {
	Mode       string   `yaml:"mode"`
	UrgentPace *float64 `yaml:"urgent_pace"`
	Tie        *float64 `yaml:"tie"`
}

func (p PaceConfig) ShadowEnabled() bool { return strings.TrimSpace(p.Mode) == "shadow" }

func (p PaceConfig) EffectiveUrgentPace() float64 {
	if p.UrgentPace == nil {
		return 1
	}
	return *p.UrgentPace
}

func (p PaceConfig) EffectiveTie() float64 {
	if p.Tie == nil {
		return .20
	}
	return *p.Tie
}

func (p PoolConfig) EffectiveWeeklyHours() float64 {
	if p.WeeklyHours == 0 {
		return 168
	}
	return p.WeeklyHours
}

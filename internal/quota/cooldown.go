package quota

import (
	"fmt"
	"time"
)

type CooldownState struct {
	Pool            string     `json:"pool"`
	ActiveUntil     time.Time  `json:"until"`
	TriggeredAt     time.Time  `json:"triggered_at"`
	ConsecutiveHits int        `json:"consecutive_hits"`
	LastJobID       *int64     `json:"last_job_id,omitempty"`
	LastAgent       string     `json:"last_agent"`
	LastPattern     string     `json:"last_pattern"`
	ClearedAt       *time.Time `json:"cleared_at,omitempty"`
	ClearedReason   string     `json:"cleared_reason,omitempty"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

func (c CooldownState) Active(now time.Time) bool {
	return c.ClearedAt == nil && now.Before(c.ActiveUntil)
}

func (c CooldownState) Reason() string {
	var job int64
	if c.LastJobID != nil {
		job = *c.LastJobID
	}
	return fmt.Sprintf("cooldown: until %s (hits=%d, job=#%d)", c.ActiveUntil.Format(time.RFC3339Nano), c.ConsecutiveHits, job)
}

func SnapshotRecovered(ev PoolEvaluation, snap *Snapshot, c CooldownState) bool {
	if ev.Status != PoolHealthy || snap == nil {
		return false
	}
	measured := snap.MeasuredAt
	if ev.Snapshot != nil && !ev.Snapshot.MeasuredAt.IsZero() {
		measured = ev.Snapshot.MeasuredAt
	}
	return measured.After(c.TriggeredAt)
}

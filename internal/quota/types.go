package quota

import "time"

const (
	// DefaultStaleAfter is the default staleness bound for usage snapshots (45m).
	DefaultStaleAfter = 45 * time.Minute

	// Canonical normalized pool names produced from usage.json.
	PoolClaude    = "claude"
	PoolCodex     = "codex"
	PoolAgyGemini = "agy-gemini"
	PoolAgy3P     = "agy-3p"
)

// PoolHealthStatus classifies a single pool's health from a snapshot.
type PoolHealthStatus string

const (
	PoolHealthy    PoolHealthStatus = "healthy"
	PoolExhausted  PoolHealthStatus = "exhausted"
	PoolUnknown    PoolHealthStatus = "unknown"
	PoolUnmeasured PoolHealthStatus = "unmeasured"
)

// DecisionKind is the top-level gate verdict recorded for a job or check.
type DecisionKind string

const (
	DecisionWouldPass DecisionKind = "would_pass"
	DecisionWouldHold DecisionKind = "would_hold"
	DecisionUnknown   DecisionKind = "unknown"
)

// BlockedBy constants identify which quota window exhausted a pool.
const (
	BlockedByNone       = ""
	BlockedByWeekly     = "weekly"
	BlockedByShort      = "5h"
	BlockedByWeeklyAnd5 = "weekly,5h"
)

// Reason constants provide machine-readable context for non-trivial verdicts.
const (
	ReasonNone             = ""
	ReasonUnmeasured       = "unmeasured"
	ReasonStale            = "stale"
	ReasonShortWindowNull  = "short_window_null"
	ReasonPoolNotFound     = "pool_not_found"
	ReasonSnapshotError    = "snapshot_error"
	ReasonNoCandidatePools = "no_candidate_pools"
)

// PoolSnapshot is the normalized quota measurement for a single pool.
type PoolSnapshot struct {
	WeeklyRemainingPercent int        `json:"weekly_remaining_percent"`
	WeeklyResetAt          *time.Time `json:"weekly_reset_at"`
	ShortRemainingPercent  *int       `json:"short_remaining_percent"`
	ShortResetAt           *time.Time `json:"short_reset_at"`
	MeasuredAt             time.Time  `json:"measured_at"`
	ServiceStale           bool       `json:"service_stale,omitempty"`
	FetchedAt              *time.Time `json:"fetched_at,omitempty"`
}

// Snapshot is a normalized collection of pool measurements taken at MeasuredAt.
type Snapshot struct {
	MeasuredAt time.Time               `json:"measured_at"`
	Pools      map[string]PoolSnapshot `json:"pools"`
}

// PoolEvaluation is the health verdict for a single candidate pool.
type PoolEvaluation struct {
	Pool       string           `json:"pool"`
	Status     PoolHealthStatus `json:"status"`
	BlockedBy  string           `json:"blocked_by,omitempty"`
	Reason     string           `json:"reason,omitempty"`
	Snapshot   *PoolSnapshot    `json:"snapshot,omitempty"`
	Unmeasured bool             `json:"unmeasured,omitempty"`
}

// GateEvaluation is the full gate decision for a primary pool and optional alt pools.
type GateEvaluation struct {
	Decision      DecisionKind     `json:"decision"`
	RequestedPool string           `json:"requested_pool"`
	AltPools      []string         `json:"alt_pools,omitempty"`
	SelectedPool  string           `json:"selected_pool,omitempty"`
	BlockedBy     string           `json:"blocked_by,omitempty"`
	Bypassed      bool             `json:"bypassed,omitempty"`
	Reason        string           `json:"reason,omitempty"`
	MeasuredAt    *time.Time       `json:"measured_at,omitempty"`
	DecidedAt     time.Time        `json:"decided_at"`
	Pools         []PoolEvaluation `json:"pools,omitempty"`
}

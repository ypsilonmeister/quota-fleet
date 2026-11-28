package quota

import "time"

const (
	// DefaultStaleAfter is the default staleness bound for usage snapshots (45m).
	DefaultStaleAfter = 45 * time.Minute

	// FutureSkewTolerance is the maximum allowed future clock skew for generated_at and measured_at (5m).
	FutureSkewTolerance = 5 * time.Minute

	// MeasuredAfterGeneratedTolerance is the maximum allowed amount by which a pool's measured_at
	// may exceed the snapshot's generated_at (60s).
	MeasuredAfterGeneratedTolerance = 60 * time.Second

	// Usage snapshot format identifiers (matching config.UsageFormatLegacy / config.UsageFormatV1).
	FormatLegacy = "legacy"
	FormatV1     = "v1"

	// Canonical normalized pool names produced from legacy usage.json.
	PoolClaude    = "claude"
	PoolCodex     = "codex"
	PoolAgyGemini = "agy-gemini"
	PoolAgy3P     = "agy-3p"

	// Schema v1 pool entry status values.
	PoolEntryStatusOK    = "ok"
	PoolEntryStatusError = "error"

	// DefaultShortWindowLabel is the default short rolling window label when omitted.
	DefaultShortWindowLabel = "5h"
)

// PoolHealthStatus classifies a single pool's health from a snapshot.
type PoolHealthStatus string

const PoolCooldown PoolHealthStatus = "cooldown"

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
	ReasonNone                  = ""
	ReasonUnmeasured            = "unmeasured"
	ReasonStale                 = "stale"
	ReasonShortWindowNull       = "short_window_null"
	ReasonShortWindowMissing    = "short_window_missing"
	ReasonShortWindowMismatch   = "short_window_mismatch"
	ReasonPoolNotFound          = "pool_not_found"
	ReasonPoolNotConfigured     = "pool_not_configured"
	ReasonSnapshotError         = "snapshot_error"
	ReasonFormatMismatch        = "format_mismatch"
	ReasonTimestampFuture       = "timestamp_future"
	ReasonTimestampInconsistent = "timestamp_inconsistent"
	ReasonCollectorError        = "collector_error"
	ReasonNoCandidatePools      = "no_candidate_pools"
)

// CollectorInfo holds optional diagnostic metadata from Schema v1 snapshots.
type CollectorInfo struct {
	Name            string `json:"name,omitempty"`
	Version         string `json:"version,omitempty"`
	Host            string `json:"host,omitempty"`
	IntervalSeconds *int   `json:"interval_seconds,omitempty"`
}

// PoolSnapshot is the normalized quota measurement for a single pool.
type PoolSnapshot struct {
	Status                 string     `json:"status,omitempty"`
	Error                  string     `json:"error,omitempty"`
	WeeklyAbsent           bool       `json:"-"`
	WeeklyRemainingPercent int        `json:"weekly_remaining_percent"`
	WeeklyResetAt          *time.Time `json:"weekly_reset_at"`
	WeeklyResetAtApprox    bool       `json:"weekly_reset_at_approx,omitempty"`
	ShortApplicable        *bool      `json:"short_applicable,omitempty"`
	ShortRemainingPercent  *int       `json:"short_remaining_percent"`
	ShortResetAt           *time.Time `json:"short_reset_at"`
	ShortResetAtApprox     bool       `json:"short_reset_at_approx,omitempty"`
	ShortWindow            string     `json:"short_window,omitempty"`
	ShortNormalized        bool       `json:"-"`
	MeasuredAt             time.Time  `json:"measured_at"`
	ServiceStale           bool       `json:"service_stale,omitempty"`
	FetchedAt              *time.Time `json:"fetched_at,omitempty"`
}

// EffectiveStatus returns "ok" unless Status is explicitly "error".
func (p PoolSnapshot) EffectiveStatus() string {
	if p.Status == PoolEntryStatusError {
		return PoolEntryStatusError
	}
	return PoolEntryStatusOK
}

// EffectiveShortWindow returns ShortWindow or DefaultShortWindowLabel ("5h").
func (p PoolSnapshot) EffectiveShortWindow() string {
	if p.ShortWindow == "" {
		return DefaultShortWindowLabel
	}
	return p.ShortWindow
}

// windowExpired reports whether a window's reset time has already passed at now. The
// remaining percentage of such a window is a lapsed value: sources may keep returning it
// until the window is next used, so it is not a reason to treat the pool as exhausted
// (docs/23 §3). An unknown (nil or zero) reset time never expires a window.
func windowExpired(resetAt *time.Time, now time.Time) bool {
	return resetAt != nil && !resetAt.IsZero() && resetAt.Before(now)
}

// windowExhausted reports whether a window with pct remaining exhausts the pool at now.
// Expiry clears only an observed 0%: a negative value (the Legacy parser passes it
// through) is not a valid observation and stays exhausted.
func windowExhausted(pct int, resetAt *time.Time, now time.Time) bool {
	if pct == 0 {
		return !windowExpired(resetAt, now)
	}
	return pct < 0
}

// weeklyExhaustedAt reports whether the weekly window exhausts the pool at now.
// Callers handle WeeklyAbsent.
func (p PoolSnapshot) weeklyExhaustedAt(now time.Time) bool {
	return windowExhausted(p.WeeklyRemainingPercent, p.WeeklyResetAt, now)
}

// shortExhaustedAt reports whether the short window exhausts the pool at now.
// A missing short value is not "exhausted"; callers handle it separately.
func (p PoolSnapshot) shortExhaustedAt(now time.Time) bool {
	return p.ShortRemainingPercent != nil && windowExhausted(*p.ShortRemainingPercent, p.ShortResetAt, now)
}

// Snapshot is a normalized collection of pool measurements taken at MeasuredAt.
type Snapshot struct {
	Format        string                  `json:"format,omitempty"`
	SchemaVersion int                     `json:"schema_version,omitempty"`
	GeneratedAt   time.Time               `json:"generated_at,omitempty"`
	Collector     *CollectorInfo          `json:"collector,omitempty"`
	MeasuredAt    time.Time               `json:"measured_at"`
	Pools         map[string]PoolSnapshot `json:"pools"`
}

// PoolEvaluation is the health verdict for a single candidate pool.
type PoolEvaluation struct {
	Cooldown   *CooldownState   `json:"cooldown,omitempty"`
	Pool       string           `json:"pool"`
	Status     PoolHealthStatus `json:"status"`
	BlockedBy  string           `json:"blocked_by,omitempty"`
	Reason     string           `json:"reason,omitempty"`
	Detail     string           `json:"detail,omitempty"`
	PoolError  string           `json:"pool_error,omitempty"`
	AgeSeconds *int64           `json:"age_seconds,omitempty"`
	Snapshot   *PoolSnapshot    `json:"snapshot,omitempty"`
	Unmeasured bool             `json:"unmeasured,omitempty"`
}

// GateEvaluation is the full gate decision for a primary pool and optional alt pools.
type GateEvaluation struct {
	Format         string           `json:"format,omitempty"`
	Decision       DecisionKind     `json:"decision"`
	RequestedPool  string           `json:"requested_pool"`
	AltPools       []string         `json:"alt_pools,omitempty"`
	SelectedPool   string           `json:"selected_pool,omitempty"`
	BlockedBy      string           `json:"blocked_by,omitempty"`
	Bypassed       bool             `json:"bypassed,omitempty"`
	Reason         string           `json:"reason,omitempty"`
	Detail         string           `json:"detail,omitempty"`
	PoolError      string           `json:"pool_error,omitempty"`
	AgeSeconds     *int64           `json:"age_seconds,omitempty"`
	UndefinedPools []string         `json:"undefined_pools,omitempty"`
	MeasuredAt     *time.Time       `json:"measured_at,omitempty"`
	DecidedAt      time.Time        `json:"decided_at"`
	Pools          []PoolEvaluation `json:"pools,omitempty"`
}

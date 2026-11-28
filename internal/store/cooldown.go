package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/quota"
)

var ErrCooldownBlocked = errors.New("cooldown blocks release")

const cooldownColumns = "pool, active_until, triggered_at, consecutive_hits, last_job_id, last_agent, last_pattern, cleared_at, cleared_reason, updated_at"

func scanCooldown(row interface{ Scan(...any) error }) (quota.CooldownState, error) {
	var c quota.CooldownState
	var until, triggered, updated string
	var cleared *string
	err := row.Scan(&c.Pool, &until, &triggered, &c.ConsecutiveHits, &c.LastJobID, &c.LastAgent, &c.LastPattern, &cleared, &c.ClearedReason, &updated)
	if err != nil {
		return c, err
	}
	for _, dst := range []struct {
		raw string
		dst *time.Time
	}{
		{until, &c.ActiveUntil}, {triggered, &c.TriggeredAt}, {updated, &c.UpdatedAt},
	} {
		if *dst.dst, err = parseTime(dst.raw); err != nil {
			return c, err
		}
	}
	if cleared != nil {
		t, err := parseTime(*cleared)
		if err != nil {
			return c, err
		}
		c.ClearedAt = &t
	}
	return c, nil
}

func (s *Store) ListCooldowns(ctx context.Context) ([]quota.CooldownState, error) {
	rows, err := s.readDB.QueryContext(ctx, "SELECT "+cooldownColumns+" FROM pool_cooldowns ORDER BY pool")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []quota.CooldownState{}
	for rows.Next() {
		c, err := scanCooldown(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) ClearCooldown(ctx context.Context, pool string) (bool, error) {
	var cleared bool
	err := s.withImmediateTx(ctx, func(conn *sql.Conn) error {
		var err error
		cleared, err = clearCooldown(ctx, conn, pool, "manual", time.Now(), "")
		return err
	})
	return cleared, err
}

func clearCooldown(ctx context.Context, conn *sql.Conn, pool, reason string, now time.Time, expected string) (bool, error) {
	at := formatTime(now)
	q := `UPDATE pool_cooldowns SET cleared_at=?, cleared_reason=?, consecutive_hits=0, updated_at=? WHERE pool=? AND cleared_at IS NULL`
	args := []any{at, reason, at, pool}
	if expected != "" {
		q += " AND triggered_at=?"
		args = append(args, expected)
	}
	res, err := conn.ExecContext(ctx, q, args...)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n != 0, err
}

func (s *Store) cooldownPool(pool, agent, model string) string {
	if pool == "" && s.config != nil {
		return quota.ResolvePoolByAppliesTo(s.config.Pools, agent, model)
	}
	return pool
}

func (s *Store) CooldownPolicy(pool string) (config.CooldownPolicy, bool) {
	if s.config != nil {
		if _, ok := s.config.Pools[pool]; ok {
			p := s.config.CooldownFor(pool)
			return p, p.Enabled
		}
	}
	return config.CooldownPolicy{}, false
}

func (s *Store) CooldownsEnabled() bool {
	return s.config != nil && s.config.HasCooldown() && s.config.Quota.EffectiveGateMode() != config.GateModeOff
}

func (s *Store) enforceCooldown() bool {
	return s.CooldownsEnabled() && s.config.Quota.EffectiveGateMode() == config.GateModeEnforce
}

// ReadCooldowns populates evaluation options without changing persisted state.
func (s *Store) ReadCooldowns(ctx context.Context, opts *quota.EvalOptions) error {
	if !s.CooldownsEnabled() {
		return nil
	}
	rows, err := s.ListCooldowns(ctx)
	if err != nil {
		return err
	}
	opts.Cooldowns = make(map[string]quota.CooldownState)
	for _, c := range rows {
		if _, enabled := s.CooldownPolicy(c.Pool); enabled && c.ClearedAt == nil {
			opts.Cooldowns[c.Pool] = c
		}
	}
	return nil
}

// LoadCooldowns applies snapshot recovery with a compare-and-set on the trigger.
func (s *Store) LoadCooldowns(ctx context.Context, snap *quota.Snapshot, opts *quota.EvalOptions) error {
	if err := s.ReadCooldowns(ctx, opts); err != nil {
		return err
	}
	return s.RecoverCooldowns(ctx, snap, opts)
}

func (s *Store) RecoverCooldowns(ctx context.Context, snap *quota.Snapshot, opts *quota.EvalOptions) error {
	if snap == nil {
		return nil
	}
	for pool, c := range opts.Cooldowns {
		if quota.SnapshotRecovered(quota.EvaluatePool(c.Pool, snap, *opts), snap, c) {
			var cleared bool
			expected := formatTime(c.TriggeredAt)
			err := s.withImmediateTx(ctx, func(conn *sql.Conn) error {
				var err error
				cleared, err = clearCooldown(ctx, conn, c.Pool, "snapshot_recovered", time.Now(), expected)
				if err != nil || cleared {
					return err
				}
				c, err = scanCooldown(conn.QueryRowContext(ctx, "SELECT "+cooldownColumns+" FROM pool_cooldowns WHERE pool=?", c.Pool))
				return err
			})
			if err != nil {
				return err
			}
			if cleared || c.ClearedAt != nil {
				delete(opts.Cooldowns, pool)
				continue
			}
		}
		opts.Cooldowns[pool] = c
	}
	return nil
}

func (s *Store) recordCooldown(ctx context.Context, conn *sql.Conn, job *Job, pattern string, now time.Time) (*quota.CooldownState, bool, error) {
	pool := s.cooldownPool(job.Pool, job.Agent, job.Model)
	p, enabled := s.CooldownPolicy(pool)
	if !enabled {
		return nil, false, nil
	}
	c, err := scanCooldown(conn.QueryRowContext(ctx, "SELECT "+cooldownColumns+" FROM pool_cooldowns WHERE pool=?", pool))
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, false, err
	}
	at := formatTime(now)
	if c.Active(now) && (job.ClaimedAt == nil || !job.ClaimedAt.After(c.TriggeredAt)) {
		_, err = conn.ExecContext(ctx, `UPDATE pool_cooldowns SET last_job_id=?, last_agent=?, last_pattern=?, updated_at=? WHERE pool=?`, job.ID, job.Agent, pattern, at, pool)
		return &c, false, err
	}

	hits := 1
	if c.ClearedAt == nil && (p.StreakResetAfter == 0 || now.Sub(c.ActiveUntil) < p.StreakResetAfter) {
		hits += c.ConsecutiveHits
	}
	duration := p.Initial
	if hits > 1 {
		backoff := float64(p.Initial) * math.Pow(p.Multiplier, float64(hits-1))
		duration = p.Max
		if backoff < float64(p.Max) {
			duration = time.Duration(backoff)
		}
	}
	until := now.Add(duration)
	c = quota.CooldownState{Pool: pool, ActiveUntil: until, TriggeredAt: now, ConsecutiveHits: hits, LastJobID: &job.ID, LastAgent: job.Agent, LastPattern: pattern, UpdatedAt: now}
	_, err = conn.ExecContext(ctx, `INSERT INTO pool_cooldowns (`+cooldownColumns+`) VALUES (?,?,?,?,?,?,?,NULL,'',?)
	ON CONFLICT(pool) DO UPDATE SET active_until=excluded.active_until, triggered_at=excluded.triggered_at,
	consecutive_hits=excluded.consecutive_hits, last_job_id=excluded.last_job_id, last_agent=excluded.last_agent,
	last_pattern=excluded.last_pattern, cleared_at=NULL, cleared_reason='', updated_at=excluded.updated_at`,
		pool, formatTime(until), at, hits, job.ID, job.Agent, pattern, at)
	if err == nil && s.config.Quota.EffectiveGateMode() == config.GateModeDryRun {
		_, err = conn.ExecContext(ctx, `INSERT INTO gate_decisions (job_id, mode, requested_pool, decision, blocked_by, reason, decided_at) VALUES (?,'dry-run',?,'would_hold','cooldown',?,?)`, job.ID, pool, c.Reason(), at)
	}
	return &c, true, err
}

// blockedCooldownPools runs under the same write transaction as claim or release.
// A released job remains queued until it can take the probe slot.
func (s *Store) blockedCooldownPools(ctx context.Context, conn *sql.Conn, now time.Time, release bool) (map[string]string, error) {
	if !s.enforceCooldown() {
		return nil, nil
	}
	rows, err := conn.QueryContext(ctx, "SELECT "+cooldownColumns+" FROM pool_cooldowns WHERE cleared_at IS NULL")
	if err != nil {
		return nil, err
	}
	blocked := make(map[string]string)
	probes := make(map[string]quota.CooldownState)
	for rows.Next() {
		c, err := scanCooldown(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		if _, enabled := s.CooldownPolicy(c.Pool); !enabled {
			continue
		}
		if c.Active(now) {
			blocked[c.Pool] = fmt.Sprintf("pool %s is in cooldown until %s (remaining %s); clear cooldown to force release", c.Pool, formatTime(c.ActiveUntil), c.ActiveUntil.Sub(now).Round(time.Second))
		} else if c.ConsecutiveHits > 0 {
			probes[c.Pool] = c
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil || len(probes) == 0 {
		return blocked, err
	}
	rows, err = conn.QueryContext(ctx, `SELECT pool, agent, model, status, claimed_at FROM jobs WHERE status='claimed' OR (? AND (status='queued' OR (status='waiting' AND EXISTS (
		SELECT 1 FROM held_jobs WHERE released_job_id=jobs.id AND status='released'
	))))`, release)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var pool, agent, model, status string
		var claimed *string
		if err := rows.Scan(&pool, &agent, &model, &status, &claimed); err != nil {
			return nil, err
		}
		pool = s.cooldownPool(pool, agent, model)
		c, ok := probes[pool]
		if !ok {
			continue
		}
		if status == "claimed" {
			if claimed == nil {
				continue
			}
			at, err := parseTime(*claimed)
			if err != nil {
				return nil, err
			}
			if !at.After(c.TriggeredAt) {
				continue
			}
		}
		blocked[pool] = fmt.Sprintf("pool %s is waiting for its cooldown probe", pool)
	}
	return blocked, rows.Err()
}

func (s *Store) claimCandidate(ctx context.Context, conn *sql.Conn, now time.Time, id int64) (int64, error) {
	blocked, err := s.blockedCooldownPools(ctx, conn, now, false)
	if err != nil {
		return 0, err
	}
	q := `SELECT id, pool, agent, model FROM jobs WHERE status='queued' AND (?=0 OR id=?) ORDER BY created_at,id`
	if len(blocked) == 0 {
		q += " LIMIT 1"
	}
	rows, err := conn.QueryContext(ctx, q, id, id)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var pool, agent, model string
		if err := rows.Scan(&id, &pool, &agent, &model); err != nil {
			return 0, err
		}
		if blocked[s.cooldownPool(pool, agent, model)] == "" {
			return id, nil
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	return 0, sql.ErrNoRows
}

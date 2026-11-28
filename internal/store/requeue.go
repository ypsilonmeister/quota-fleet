package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/config"
)

// RequeueDecision says whether a rate-limited claimed job goes back to queued (Issue #56).
type RequeueDecision struct {
	Requeue    bool
	MaxRetries int
	// Why explains a "no" for logs (empty when Requeue is true).
	Why string
}

// requeueDecision applies the requeue policy to job on its cooldown pool. chainStep is the
// chain_steps.step of the job ("" outside a chain run).
func (s *Store) requeueDecision(job *Job, pool, chainStep string) RequeueDecision {
	if s.config == nil {
		return RequeueDecision{Why: "no config"}
	}
	if job.OnRateLimit == OnRateLimitFail {
		return RequeueDecision{Why: "on_rate_limit=fail"}
	}
	if _, ok := s.config.Pools[pool]; !ok {
		return RequeueDecision{Why: fmt.Sprintf("pool %q is not configured", pool)}
	}
	p := s.config.RequeueFor(pool)
	enabled := p.Enabled || job.OnRateLimit == OnRateLimitRequeue
	switch {
	case !enabled:
		return RequeueDecision{Why: "requeue disabled"}
	case p.MaxRetries <= 0:
		return RequeueDecision{Why: "requeue max_retries is 0"}
	case !s.config.CooldownFor(pool).Enabled:
		return RequeueDecision{Why: "pool cooldown disabled"}
	case s.config.Quota.EffectiveGateMode() != config.GateModeEnforce:
		return RequeueDecision{Why: "gate_mode is not enforce"}
	}
	// The chain publish step pushes and opens PRs; a rerun could repeat those side
	// effects, so it is only requeued when the job asks for it explicitly (decision 5).
	if chainStep == ChainStepPublish && job.OnRateLimit != OnRateLimitRequeue {
		return RequeueDecision{Why: "chain publish step is not requeued by default"}
	}
	return RequeueDecision{Requeue: true, MaxRetries: p.MaxRetries}
}

func jobChainStep(ctx context.Context, conn *sql.Conn, jobID int64) (string, error) {
	var step string
	err := conn.QueryRowContext(ctx, `SELECT step FROM chain_steps WHERE job_id = ?`, jobID).Scan(&step)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return step, err
}

// RequeueOnRateLimit settles a claimed job whose run ended on a provider rate limit
// (pattern from detectRateLimit). In one transaction it records the pool cooldown and then:
//
//   - requeues the job (claimed → queued) when the requeue policy allows it and
//     rate_limit_retries < max_retries: attempts is given back, rate_limit_retries is
//     incremented, retry_after mirrors the cooldown's active_until, and waiting
//     descendants are left untouched;
//   - cancels it when its chain run is no longer running (docs/22 §5.2);
//   - otherwise fails it like FailWithCooldown (cascading to waiting descendants), with
//     "rate limit retries exhausted (N/N)" when the retry budget is spent.
//
// FailResult.Requeued tells the caller whether the job is still live (no delivery).
func (s *Store) RequeueOnRateLimit(ctx context.Context, jobID int64, claimToken, reason, resultText, pattern string) (*FailResult, error) {
	var out *FailResult
	err := s.withJobTx(ctx, func(conn *sql.Conn, changes *[]statusChange) error {
		out = nil
		now := time.Now().UTC()
		nowStr := formatTime(now)

		job, err := queryJobByID(ctx, conn, jobID)
		if err != nil {
			return err
		}
		if job.Status != StatusClaimed || job.ClaimedBy != claimToken {
			return classifyClaimedConflict(ctx, conn, jobID, claimToken)
		}
		step, err := jobChainStep(ctx, conn, jobID)
		if err != nil {
			return err
		}
		pool := s.cooldownPool(job.Pool, job.Agent, job.Model)
		dec := s.requeueDecision(job, pool, step)
		if !dec.Requeue || pattern == "" {
			out, err = s.failClaimedTx(ctx, conn, changes, jobID, claimToken, reason, resultText, pattern, now)
			return err
		}

		if terminal, runStatus, err := jobChainRunTerminal(ctx, conn, jobID); err != nil {
			return err
		} else if terminal {
			c, started, err := s.recordCooldown(ctx, conn, job, pattern, now)
			if err != nil {
				return err
			}
			j, err := cancelClaimedChainJobTx(ctx, conn, changes, jobID, runStatus, nowStr)
			if err != nil {
				return err
			}
			out = &FailResult{Job: j}
			if started {
				out.Cooldown = c
			}
			return nil
		}

		if job.RateLimitRetries >= dec.MaxRetries {
			exhausted := fmt.Sprintf("rate limit retries exhausted (%d/%d): %s", job.RateLimitRetries, dec.MaxRetries, reason)
			out, err = s.failClaimedTx(ctx, conn, changes, jobID, claimToken, exhausted, resultText, pattern, now)
			return err
		}

		c, started, err := s.recordCooldown(ctx, conn, job, pattern, now)
		if err != nil {
			return err
		}
		retry := job.RateLimitRetries + 1
		lastError := fmt.Sprintf("rate limited; requeued (retry %d/%d): %s", retry, dec.MaxRetries, reason)
		var retryAfter any
		if c != nil {
			lastError += " [" + c.Reason() + "]"
			retryAfter = formatTime(c.ActiveUntil)
		}
		res, err := conn.ExecContext(ctx, `
			UPDATE jobs
			SET status = 'queued',
				claimed_by = '',
				claimed_at = NULL,
				attempts = CASE WHEN attempts > 0 THEN attempts - 1 ELSE 0 END,
				rate_limit_retries = rate_limit_retries + 1,
				retry_after = ?,
				last_error = ?,
				updated_at = ?
			WHERE id = ? AND status = 'claimed' AND claimed_by = ?
		`, retryAfter, lastError, nowStr, jobID, claimToken)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			return classifyClaimedConflict(ctx, conn, jobID, claimToken)
		}
		if _, err := conn.ExecContext(ctx, `DELETE FROM leases WHERE job_id = ?`, jobID); err != nil {
			return err
		}
		if err := s.insertOutboxTx(ctx, conn, jobID, "rate_limit_requeue", "queued", nowStr); err != nil {
			return err
		}
		j, err := queryJobByID(ctx, conn, jobID)
		if err != nil {
			return err
		}
		out = &FailResult{Job: j, Requeued: true}
		if started {
			out.Cooldown = c
		}
		*changes = append(*changes, statusChange{jobID: jobID, prev: StatusClaimed, next: StatusQueued})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// clearRetryAfterForPool drops retry_after from queued jobs on pool after its cooldown was
// cleared early (snapshot recovery or manual clear), so they become claimable at once.
func (s *Store) clearRetryAfterForPool(ctx context.Context, conn *sql.Conn, pool string) error {
	rows, err := conn.QueryContext(ctx, `SELECT id, pool, agent, model FROM jobs WHERE status = 'queued' AND retry_after IS NOT NULL`)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		var p, agent, model string
		if err := rows.Scan(&id, &p, &agent, &model); err != nil {
			rows.Close()
			return err
		}
		if s.cooldownPool(p, agent, model) == pool {
			ids = append(ids, id)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := conn.ExecContext(ctx, `UPDATE jobs SET retry_after = NULL WHERE id = ? AND status = 'queued'`, id); err != nil {
			return err
		}
	}
	return nil
}

// CountRateLimitRequeued returns the number of queued jobs waiting after a rate-limit requeue.
func (s *Store) CountRateLimitRequeued(ctx context.Context) (int64, error) {
	var n int64
	err := s.readDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE status = 'queued' AND rate_limit_retries > 0`).Scan(&n)
	return n, err
}

// ListStalledRateLimitRequeued returns queued, rate-limit requeued jobs whose last requeue
// (updated_at) is older than the stall threshold of their pool.
func (s *Store) ListStalledRateLimitRequeued(ctx context.Context, now time.Time) ([]*Job, error) {
	rows, err := s.readDB.QueryContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE status = 'queued' AND rate_limit_retries > 0 ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		stall := config.DefaultRequeueStallAfter
		if s.config != nil {
			stall = s.config.RequeueFor(s.cooldownPool(j.Pool, j.Agent, j.Model)).StallAfter
		}
		if stall > 0 && now.Sub(j.UpdatedAt) >= stall {
			out = append(out, j)
		}
	}
	return out, rows.Err()
}

// RequeueGateVerdict is the Claim-time Gate result for one rate-limit requeued job.
type RequeueGateVerdict struct {
	// Allow is false when the Gate would hold the job (exhausted, cooldown, stale, ...).
	Allow bool
	// SelectedPool is the pool to run on when it differs from the job's pool (declared
	// alt_pools failover); empty to stay on the current pool.
	SelectedPool string
	// Gate is recorded in gate_decisions (job_id filled in) when the pool switches.
	Gate *GateDecision
}

// RequeueGateFunc evaluates the Gate for a requeued job on its (resolved) pool and, when
// failover is true, its declared alt_pools. It runs outside the claim transaction.
type RequeueGateFunc func(ctx context.Context, job *Job, pool string, failover bool) RequeueGateVerdict

// SetRequeueGate installs the Claim-time Gate for rate-limit requeued jobs. Without one,
// requeued jobs are claimable once retry_after passes and their pool cooldown allows it.
func (s *Store) SetRequeueGate(fn RequeueGateFunc) {
	if fn == nil {
		s.requeueGate.Store(nil)
		return
	}
	s.requeueGate.Store(&fn)
}

// requeueVerdicts evaluates the Gate for every queued requeued job (or only jobID when
// non-zero) before the claim transaction starts.
func (s *Store) requeueVerdicts(ctx context.Context, jobID int64) (map[int64]RequeueGateVerdict, error) {
	fnp := s.requeueGate.Load()
	if fnp == nil {
		return nil, nil
	}
	rows, err := s.readDB.QueryContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE status = 'queued' AND rate_limit_retries > 0 AND (?=0 OR id=?) ORDER BY created_at, id`, jobID, jobID)
	if err != nil {
		return nil, err
	}
	var jobs []*Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		jobs = append(jobs, j)
	}
	err = rows.Err()
	rows.Close()
	if err != nil || len(jobs) == 0 {
		return nil, err
	}
	out := make(map[int64]RequeueGateVerdict, len(jobs))
	for _, j := range jobs {
		pool := s.cooldownPool(j.Pool, j.Agent, j.Model)
		failover := true
		if s.config != nil {
			failover = s.config.RequeueFor(pool).FailoverAltPools
		}
		v := (*fnp)(ctx, j, pool, failover)
		if v.SelectedPool == pool {
			v.SelectedPool = ""
		}
		out[j.ID] = v
	}
	return out, nil
}

// applyRequeueFailover moves a just-claimed requeued job to the alt pool its verdict
// selected. The previous pool joins alt_pools so a later failover can return to it.
func (s *Store) applyRequeueFailover(ctx context.Context, conn *sql.Conn, jobID int64, v *RequeueGateVerdict) error {
	if v == nil || v.SelectedPool == "" {
		return nil
	}
	job, err := queryJobByID(ctx, conn, jobID)
	if err != nil {
		return err
	}
	from := s.cooldownPool(job.Pool, job.Agent, job.Model)
	alts := []string{}
	if from != "" {
		alts = append(alts, from)
	}
	for _, p := range job.AltPools {
		if p != v.SelectedPool && p != from {
			alts = append(alts, p)
		}
	}
	altJSON, err := encodeAltPools(alts)
	if err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `UPDATE jobs SET pool = ?, alt_pools = ? WHERE id = ?`, v.SelectedPool, altJSON, jobID); err != nil {
		return err
	}
	if v.Gate != nil {
		d := *v.Gate
		d.JobID = jobID
		if err := insertGateDecisionTx(ctx, conn, d); err != nil {
			return err
		}
	}
	return nil
}

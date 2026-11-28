package runtime

import (
	"context"
	"log"

	"github.com/ypsilonmeister/quota-fleet/internal/admission"
	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/quota"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

// NewRequeueGate returns the Claim-time Gate for rate-limit requeued jobs (Issue #56 §3.2).
// It runs the same evaluation as job admission (usage snapshot + pool cooldowns) so that a
// requeued job is not claimed while its pool is exhausted, cooling down, or its snapshot is
// stale/unreadable (fail-closed in enforce). With failover, a healthy declared alt pool is
// selected; the agent and model never change.
func NewRequeueGate(adm *admission.Admission, logger *log.Logger) store.RequeueGateFunc {
	return func(ctx context.Context, job *store.Job, pool string, failover bool) store.RequeueGateVerdict {
		if adm == nil || adm.GateMode() != config.GateModeEnforce {
			return store.RequeueGateVerdict{Allow: true}
		}
		var alts []string
		if failover {
			alts = job.AltPools
		}
		if pool == "" && len(alts) == 0 {
			return store.RequeueGateVerdict{Allow: true}
		}
		eval, err := adm.EvaluateGate(pool, alts, adm.NewUsage())
		if quota.ShouldHoldOnEnforce(eval) {
			if logger != nil {
				logger.Printf("requeue gate: job #%d stays queued (pool=%s alt=%v): %s", job.ID, pool, alts, quota.FormatHoldReason(eval))
			}
			return store.RequeueGateVerdict{}
		}
		if err != nil && logger != nil {
			logger.Printf("requeue gate: job #%d usage snapshot error (decision=%s): %v", job.ID, eval.Decision, err)
		}
		v := store.RequeueGateVerdict{Allow: true}
		if eval.Decision == quota.DecisionWouldPass && eval.SelectedPool != "" && eval.SelectedPool != pool {
			v.SelectedPool = eval.SelectedPool
			v.Gate = &store.GateDecision{
				Mode:          config.GateModeEnforce,
				RequestedPool: eval.RequestedPool,
				AltPools:      eval.AltPools,
				Decision:      string(eval.Decision),
				BlockedBy:     eval.BlockedBy,
				SelectedPool:  eval.SelectedPool,
				Bypassed:      eval.Bypassed,
				Reason:        "rate_limit_requeue_failover: " + quota.FormatDecisionReason(eval),
				MeasuredAt:    eval.MeasuredAt,
				DecidedAt:     eval.DecidedAt,
			}
		}
		return v
	}
}

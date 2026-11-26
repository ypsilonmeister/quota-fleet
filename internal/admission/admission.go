// Package admission is the write-free admission decision shared by POST /jobs and the
// Chain Driver (docs/22 §4.4): Selector (docs/19) then Quota Gate (docs/03). Callers
// write the result themselves — POST /jobs through CreateJob/HoldJob, the Chain Driver
// inside AdvanceChain — so there is one decision path and no "CreateJob directly" bypass.
package admission

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/quota"
	"github.com/ypsilonmeister/quota-fleet/internal/selector"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

// Options configures an Admission.
type Options struct {
	GateMode     string
	SelectorMode string
	Pools        map[string]config.PoolConfig
	StaleAfter   time.Duration
	// UsageFile is read when LoadSnapshot is nil.
	UsageFile    string
	Now          func() time.Time
	LoadSnapshot func() (*quota.Snapshot, error)
	// GetJob looks up a parent for the fix-follows-parent rule (docs/10 §4).
	GetJob func(ctx context.Context, id int64) (*store.Job, error)
}

// Admission evaluates job requests against the Selector and the Gate.
type Admission struct {
	gateMode     string
	selectorMode string
	pools        map[string]config.PoolConfig
	staleAfter   time.Duration
	usageFile    string
	now          func() time.Time
	loadSnapshot func() (*quota.Snapshot, error)
	getJob       func(ctx context.Context, id int64) (*store.Job, error)
}

// New constructs an Admission with the same defaults as the API server.
func New(opts Options) *Admission {
	a := &Admission{
		gateMode:     strings.TrimSpace(opts.GateMode),
		selectorMode: strings.TrimSpace(opts.SelectorMode),
		pools:        opts.Pools,
		staleAfter:   opts.StaleAfter,
		usageFile:    strings.TrimSpace(opts.UsageFile),
		now:          opts.Now,
		loadSnapshot: opts.LoadSnapshot,
		getJob:       opts.GetJob,
	}
	if a.gateMode == "" {
		a.gateMode = config.GateModeOff
	}
	if a.selectorMode == "" {
		a.selectorMode = config.SelectorModeOff
	}
	if a.staleAfter <= 0 {
		a.staleAfter = config.DefaultQuotaStaleAfter
	}
	if a.now == nil {
		a.now = time.Now
	}
	return a
}

// GateMode returns the configured gate mode.
func (a *Admission) GateMode() string { return a.gateMode }

// SelectorMode returns the configured selector mode.
func (a *Admission) SelectorMode() string { return a.selectorMode }

// Request is a job request as the client sent it.
type Request struct {
	// Params carries the client's values. Pool is the raw requested pool.
	Params store.CreateJobParams
	Role   string
	Weight string
}

// Usage loads the usage snapshot at most once per request so the Selector and the Gate
// judge the same data.
type Usage struct {
	load func() (*quota.Snapshot, error)
	done bool
	snap *quota.Snapshot
	err  error
}

// NewUsage returns a lazily loaded snapshot holder for one request.
func (a *Admission) NewUsage() *Usage { return &Usage{load: a.LoadSnapshot} }

// Get returns the snapshot, loading it on the first call.
func (u *Usage) Get() (*quota.Snapshot, error) {
	if !u.done {
		u.snap, u.err = u.load()
		u.done = true
	}
	return u.snap, u.err
}

// LoadSnapshot reads the usage snapshot through the same path as the Gate.
func (a *Admission) LoadSnapshot() (*quota.Snapshot, error) {
	if a.loadSnapshot != nil {
		return a.loadSnapshot()
	}
	if a.usageFile == "" {
		return nil, errors.New("quota.usage_file is not configured")
	}
	return quota.LoadUsageFile(a.usageFile)
}

// ResolvePool returns the explicit pool or the pool whose applies_to matches agent/model.
func (a *Admission) ResolvePool(agent, model, pool string) string {
	reqPool := strings.TrimSpace(pool)
	if reqPool == "" {
		reqPool = quota.ResolvePoolByAppliesTo(a.pools, agent, model)
	}
	return reqPool
}

// ValidateRequest checks role and weight when the Selector is on.
func (a *Admission) ValidateRequest(req Request) error {
	if a.selectorMode != config.SelectorModeOff && !selector.ValidRole(strings.TrimSpace(req.Role)) {
		return fmt.Errorf("%w: role %q is not supported (implement|review|judge|fix|publish|misc)", store.ErrInvalidInput, req.Role)
	}
	if a.selectorMode != config.SelectorModeOff && !selector.ValidWeight(strings.TrimSpace(req.Weight)) {
		return fmt.Errorf("%w: weight %q is not supported (light|standard|heavy)", store.ErrInvalidInput, req.Weight)
	}
	return nil
}

// Select runs the Selector in front of the Gate (docs/19). Explicit agent/model/pool
// values are never overwritten; only empty fields are filled, and only in apply mode with
// the selection (record mode fills them with the documented defaults instead).
// params.Pool must already hold ResolvePool's result.
func (a *Admission) Select(ctx context.Context, params *store.CreateJobParams, req Request, usage *Usage) {
	if a.selectorMode == config.SelectorModeOff {
		return
	}
	agent := strings.TrimSpace(req.Params.Agent)
	model := strings.TrimSpace(req.Params.Model)
	explicitPool := strings.TrimSpace(req.Params.Pool)
	resolve := func(ag, m string) string { return quota.ResolvePoolByAppliesTo(a.pools, ag, m) }

	if agent != "" && model != "" {
		if params.Pool == "" {
			// Known table pair: fill the pool so the Gate sees it (apply mode only fills).
			if p := selector.PoolFor(agent, model); p != "" && a.selectorMode == config.SelectorModeApply {
				params.Pool = p
			}
		}
		params.SelectedAgent, params.SelectedModel, params.SelectedPool = agent, model, params.Pool
		params.SelectionMode = selector.ModeExplicit
		params.SelectionReason = "agent and model explicitly specified; selector not consulted"
		return
	}

	reqAgent, reqModel := agent, model // what the client sent; record mode executes with these + defaults
	var parentNote string
	var parentJob selector.Candidate // the followed parent, with the pool it actually runs on
	parentFollowed := false
	if strings.TrimSpace(req.Role) == selector.RoleFix && req.Params.ParentID != nil && agent == "" && model == "" && a.getJob != nil {
		// docs/10 §4: fix runs on the same implementer as the parent job.
		if parent, err := a.getJob(ctx, *req.Params.ParentID); err == nil && parent.Agent != "" {
			agent, model = parent.Agent, parent.Model
			parentNote = fmt.Sprintf("fix follows parent job #%d (%s/%s); ", parent.ID, agent, model)
			parentFollowed = true
			parentJob = selector.Candidate{Agent: parent.Agent, Model: parent.Model, Pool: parent.Pool}
			if parentJob.Pool == "" {
				// No pool stored on the parent: derive it from the parent's own agent/model.
				parentJob.Pool = resolve(parent.Agent, parent.Model)
			}
			if parentJob.Pool == "" {
				parentJob.Pool = selector.PoolFor(parent.Agent, parent.Model)
			}
		}
	}

	snap, snapErr := usage.Get()
	evalOpts := quota.EvalOptions{Now: a.now(), StaleAfter: a.staleAfter, Pools: a.pools}
	res := selector.Select(selector.Input{
		Role:        strings.TrimSpace(req.Role),
		Weight:      strings.TrimSpace(req.Weight),
		Agent:       agent,
		Model:       model,
		Pool:        explicitPool,
		Usage:       snap,
		UsageErr:    snapErr,
		Eval:        evalOpts,
		ResolvePool: resolve,
	})
	if parentFollowed && !res.Abstain && snap != nil {
		// docs/10 §4: when the parent's pool is exhausted and another pool is healthy,
		// propose switching to another line (the proposal is recorded only).
		// The parent's own pool is judged, not the pool of this job's candidate.
		if ev := quota.EvaluatePool(parentJob.Pool, snap, evalOpts); ev.Status == quota.PoolExhausted {
			if alt, ok := selector.SwitchTarget(selector.RoleFix, strings.TrimSpace(req.Weight), parentJob, snap, evalOpts); ok {
				parentNote += fmt.Sprintf("parent pool %s is exhausted and pool %s is healthy: proposing a switch to another line %s/%s (docs/10 §4); ", parentJob.Pool, alt.Pool, alt.Agent, alt.Model)
			}
		}
	}
	if parentFollowed && res.Abstain {
		parentNote = fmt.Sprintf("parent job target %s/%s not followed; ", agent, model)
	}
	// res.Candidate is empty on abstain, so no selected_* value is recorded then.
	params.SelectedAgent, params.SelectedModel, params.SelectedPool = res.Agent, res.Model, res.Pool
	params.SelectionReason = parentNote + res.Reason

	if a.selectorMode == config.SelectorModeApply && !res.Abstain {
		params.SelectionMode = selector.ModeApplied
		if reqAgent == "" {
			params.Agent = res.Agent
		}
		if reqModel == "" {
			params.Model = res.Model
		}
		if explicitPool == "" && res.Pool != "" {
			params.Pool = res.Pool
		}
		return
	}

	// Record mode and abstain both execute explicit values plus the documented defaults.
	def := selector.Defaults(reqAgent, reqModel, explicitPool, resolve)
	if reqAgent == "" {
		params.Agent = def.Agent
	}
	if reqModel == "" {
		params.Model = def.Model
	}
	if explicitPool == "" {
		params.Pool = def.Pool
	}
	if res.Abstain {
		params.SelectionMode = selector.ModeAbstained
		params.SelectionReason += fmt.Sprintf("; registered with %s %s %s",
			executedField("agent", params.Agent, reqAgent),
			executedField("model", params.Model, reqModel),
			executedField("pool", params.Pool, explicitPool))
		return
	}
	params.SelectionMode = selector.ModeRecorded
	params.SelectionReason += fmt.Sprintf("; record mode executes with defaults %s/%s on pool %s", params.Agent, params.Model, params.Pool)
}

// executedField describes an executed value and whether the client specified it.
func executedField(name, executed, requested string) string {
	switch {
	case requested != "":
		return fmt.Sprintf("%s=%s(explicit)", name, executed)
	case executed != "":
		return fmt.Sprintf("%s=%s(default)", name, executed)
	}
	return name + "=(unset)"
}

// EvaluateGate evaluates the Gate for the pool the job will run on. A snapshot read error
// turns a measured evaluation into unknown and is returned alongside.
func (a *Admission) EvaluateGate(primaryPool string, altPools []string, usage *Usage) (quota.GateEvaluation, error) {
	opts := quota.EvalOptions{
		Now:        a.now(),
		StaleAfter: a.staleAfter,
		Pools:      a.pools,
	}
	primaryPool = strings.TrimSpace(primaryPool)
	if primaryPool != "" {
		if cfg, ok := a.pools[primaryPool]; ok && cfg.Unmeasured {
			return quota.EvaluateGate(primaryPool, altPools, nil, opts), nil
		}
	}
	snap, err := usage.Get()
	if err != nil {
		eval := quota.EvaluateGate(primaryPool, altPools, nil, opts)
		if !eval.Bypassed {
			eval.Decision = quota.DecisionUnknown
			eval.Reason = fmt.Sprintf("%s: %v", quota.ReasonSnapshotError, err)
		}
		return eval, err
	}
	return quota.EvaluateGate(primaryPool, altPools, snap, opts), nil
}

// Kind is the outcome of Decide.
type Kind string

const (
	KindCreate Kind = "create"
	KindHold   Kind = "hold"
	KindReject Kind = "reject"
)

// Decision is the write-free admission result.
type Decision struct {
	Kind Kind
	// Params is the job payload to write (pool already set to the Gate's selection when
	// it passed in enforce mode).
	Params store.CreateJobParams
	// Hold fields are set when Kind is hold.
	HoldPool     string
	HoldAltPools []string
	HoldReason   string
	HoldSummary  string
	// Gate is the gate_decisions row to record with the written job or hold (nil when the
	// Gate is off or there is no pool to judge).
	Gate *store.GateDecision
	// Reject is set when Kind is reject: the request itself is wrong.
	Reject error
	// SnapshotErr reports a usage snapshot read failure (a transient condition: enforce
	// treats it as unknown and holds, like POST /jobs).
	SnapshotErr error
}

func gateRecord(mode string, eval quota.GateEvaluation) *store.GateDecision {
	return &store.GateDecision{
		Mode:          mode,
		RequestedPool: eval.RequestedPool,
		AltPools:      eval.AltPools,
		Decision:      string(eval.Decision),
		BlockedBy:     eval.BlockedBy,
		SelectedPool:  eval.SelectedPool,
		Bypassed:      eval.Bypassed,
		Reason:        eval.Reason,
		MeasuredAt:    eval.MeasuredAt,
		DecidedAt:     eval.DecidedAt,
	}
}

// Prepare validates the request, resolves the pool, runs the Selector and checks the
// effort against the agent the job will run on. An error means reject.
func (a *Admission) Prepare(ctx context.Context, req Request, usage *Usage) (store.CreateJobParams, error) {
	if err := a.ValidateRequest(req); err != nil {
		return store.CreateJobParams{}, err
	}
	params := req.Params
	params.Pool = a.ResolvePool(req.Params.Agent, req.Params.Model, req.Params.Pool)
	params.Effort = strings.TrimSpace(params.Effort)
	a.Select(ctx, &params, req, usage)
	if err := store.ValidateEffort(params.Agent, params.Effort); err != nil {
		return store.CreateJobParams{}, err
	}
	return params, nil
}

// Gate applies the Quota Gate to prepared params (docs/03): off → create; dry-run →
// create with a would-* record; enforce → hold when ShouldHoldOnEnforce, otherwise create
// on the selected pool. altPools are the client's alternative pools.
func (a *Admission) Gate(params store.CreateJobParams, altPools []string, usage *Usage) Decision {
	d := Decision{Kind: KindCreate, Params: params}
	reqPool := params.Pool
	if reqPool == "" && len(altPools) == 0 {
		return d
	}
	switch a.gateMode {
	case config.GateModeDryRun:
		eval, err := a.EvaluateGate(reqPool, altPools, usage)
		d.SnapshotErr = err
		d.Gate = gateRecord(config.GateModeDryRun, eval)
	case config.GateModeEnforce:
		eval, err := a.EvaluateGate(reqPool, altPools, usage)
		d.SnapshotErr = err
		if quota.ShouldHoldOnEnforce(eval) {
			d.Kind = KindHold
			d.HoldPool = reqPool
			d.HoldAltPools = eval.AltPools
			d.HoldReason = quota.FormatHoldReason(eval)
			d.HoldSummary = quota.SummarizeEvaluation(eval)
			g := gateRecord(config.GateModeEnforce, eval)
			g.Held = true
			d.Gate = g
			return d
		}
		// Not held (healthy pool selected, unmeasured bypass, or provisional unknown only).
		if eval.Decision == quota.DecisionWouldPass && eval.SelectedPool != "" {
			d.Params.Pool = eval.SelectedPool
		}
		d.Gate = gateRecord(config.GateModeEnforce, eval)
	}
	return d
}

// Decide is Prepare followed by Gate. The Chain Driver uses it; POST /jobs calls the two
// halves itself because it probes the host's effort support in between (docs/21 §3).
func (a *Admission) Decide(ctx context.Context, req Request) Decision {
	usage := a.NewUsage()
	params, err := a.Prepare(ctx, req, usage)
	if err != nil {
		return Decision{Kind: KindReject, Reject: err}
	}
	return a.Gate(params, req.Params.AltPools, usage)
}

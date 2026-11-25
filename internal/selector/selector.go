// Package selector chooses agent / model / pool for jobs registered without them
// (docs/10, docs/19). It sits in front of the Quota Gate: it only decides the Gate's
// input and never changes how the Gate judges.
package selector

import (
	"fmt"
	"strings"

	"github.com/ypsilonmeister/quota-fleet/internal/quota"
)

// Role hints.
const (
	RoleImplement = "implement"
	RoleReview    = "review"
	RoleJudge     = "judge"
	RoleFix       = "fix"
	RolePublish   = "publish"
	RoleMisc      = "misc"
)

// Weight hints.
const (
	WeightLight    = "light"
	WeightStandard = "standard"
	WeightHeavy    = "heavy"
)

// Selection modes recorded on jobs.
const (
	ModeExplicit = "explicit"
	ModeRecorded = "recorded"
	ModeApplied  = "applied"
)

// Candidate is one agent/model/pool combination.
type Candidate struct {
	Agent string
	Model string
	Pool  string
}

// DefaultCandidate is the deterministic fallback used when the usage snapshot is stale or
// missing, and the value record mode executes with (docs/10 §4: standard implementation,
// first candidate "Codex sol").
var DefaultCandidate = Candidate{Agent: "codex", Model: "gpt-6.2-sol", Pool: quota.PoolCodex}

var (
	agyNeonMedium = Candidate{"agy", "gemini-4-neon-medium", quota.PoolAgyGemini}
	agyNeonXHigh  = Candidate{"agy", "gemini-4-neon-xhigh", quota.PoolAgyGemini}
	codexSol      = DefaultCandidate
	codexAstra    = Candidate{"codex", "gpt-6.2-astra", quota.PoolCodex}
	claudeSonnet  = Candidate{"claude", "claude-sonnet-5-5", quota.PoolClaude}
	claudeOpus    = Candidate{"claude", "claude-opus-5-5", quota.PoolClaude}
)

// table is the role x weight table of docs/10 §4, in preference order. The first healthy
// candidate wins; the first entry doubles as the deterministic default when usage is unreliable.
var table = map[string]map[string][]Candidate{
	RoleImplement: {
		WeightLight:    {agyNeonMedium, codexSol},
		WeightStandard: {codexSol, agyNeonMedium, claudeSonnet},
		WeightHeavy:    {claudeOpus, codexAstra, agyNeonXHigh},
	},
	RoleReview: {
		WeightLight:    {agyNeonMedium},
		WeightStandard: {codexSol, claudeSonnet},
		WeightHeavy:    {claudeOpus, codexAstra, agyNeonXHigh},
	},
	RoleJudge: {
		// docs/10: judge is the Gemini Decision evaluator; as a job it runs on agy Neon.
		WeightLight:    {agyNeonMedium},
		WeightStandard: {agyNeonMedium},
		WeightHeavy:    {agyNeonXHigh},
	},
}

func init() {
	// fix: same as implement (session continuation is the coordinator's job, docs/10 §4).
	table[RoleFix] = table[RoleImplement]
	// publish / misc: no row of their own in docs/10; they use the implement row for the
	// given weight (standard when unset).
	table[RolePublish] = table[RoleImplement]
	table[RoleMisc] = table[RoleImplement]
}

// ValidRole reports whether r is empty or a supported role.
func ValidRole(r string) bool {
	if r == "" {
		return true
	}
	_, ok := table[r]
	return ok
}

// ValidWeight reports whether w is empty or a supported weight.
func ValidWeight(w string) bool {
	switch w {
	case "", WeightLight, WeightStandard, WeightHeavy:
		return true
	}
	return false
}

// Candidates returns the table row for role/weight. Empty role means implement, empty
// weight means standard (docs/10 §2: no inference from title or instructions).
func Candidates(role, weight string) []Candidate {
	if role == "" {
		role = RoleImplement
	}
	if weight == "" {
		weight = WeightStandard
	}
	return append([]Candidate(nil), table[role][weight]...)
}

// Input is what the Selector needs for one decision.
type Input struct {
	Role   string
	Weight string
	// Explicit fields. Non-empty values are never overridden.
	Agent string
	Model string
	Pool  string
	// Usage is the snapshot read the same way as the Gate. Nil means missing.
	Usage    *quota.Snapshot
	UsageErr error
	Eval     quota.EvalOptions
	// ResolvePool maps agent+model to a pool via pools.applies_to. May be nil.
	ResolvePool func(agent, model string) string
}

// Result is the Selector's choice. Fields always honour explicit input.
type Result struct {
	Candidate
	Reason   string
	Fallback bool // true when usage was unreliable and the deterministic default order was used
}

func (in Input) compatible(c Candidate) bool {
	if in.Agent != "" && !strings.EqualFold(in.Agent, c.Agent) {
		return false
	}
	if in.Model != "" && in.Model != c.Model {
		return false
	}
	if in.Pool != "" && in.Pool != c.Pool {
		return false
	}
	return true
}

// Select picks a candidate. It never overrides explicit fields: the returned Candidate
// keeps the explicit values and only fills the empty ones.
func Select(in Input) Result {
	head := fmt.Sprintf("role=%s weight=%s", orDefault(in.Role, RoleImplement+"(default)"), orDefault(in.Weight, WeightStandard+"(default)"))

	var cands []Candidate
	for _, c := range Candidates(in.Role, in.Weight) {
		if in.compatible(c) {
			cands = append(cands, c)
		}
	}
	if len(cands) == 0 {
		return noCandidate(in, head)
	}

	var (
		notes     []string
		firstLive *Candidate // first candidate that is not exhausted (usage may be unknown)
		healthy   *Candidate
		exhausted int
	)
	for i := range cands {
		c := cands[i]
		ev := quota.EvaluatePool(c.Pool, in.Usage, in.Eval)
		switch ev.Status {
		case quota.PoolHealthy:
			notes = append(notes, fmt.Sprintf("%s: healthy", c.Pool))
			if healthy == nil {
				healthy = &cands[i]
			}
			if firstLive == nil {
				firstLive = &cands[i]
			}
		case quota.PoolExhausted:
			exhausted++
			notes = append(notes, fmt.Sprintf("%s: exhausted(%s) excluded", c.Pool, ev.BlockedBy))
		default:
			why := ev.Reason
			if in.UsageErr != nil {
				why = "usage_unavailable"
			}
			notes = append(notes, fmt.Sprintf("%s: %s(%s) excluded", c.Pool, ev.Status, why))
			if firstLive == nil {
				firstLive = &cands[i]
			}
		}
	}

	if healthy != nil {
		return Result{Candidate: in.complete(*healthy), Reason: fmt.Sprintf("%s; chose %s/%s on pool %s; %s", head, healthy.Agent, healthy.Model, healthy.Pool, strings.Join(notes, ", "))}
	}
	if firstLive != nil {
		// No pool could be confirmed healthy: usage is stale/missing/unmeasured. Say so.
		return Result{
			Candidate: in.complete(*firstLive),
			Fallback:  true,
			Reason:    fmt.Sprintf("%s; FALLBACK to deterministic default order (usage stale or missing): %s/%s on pool %s; %s", head, firstLive.Agent, firstLive.Model, firstLive.Pool, strings.Join(notes, ", ")),
		}
	}
	// Every candidate pool is exhausted. Keep the table's first candidate and let the Gate hold.
	c := cands[0]
	return Result{
		Candidate: in.complete(c),
		Fallback:  true,
		Reason:    fmt.Sprintf("%s; all candidate pools exhausted, kept first table candidate %s/%s on pool %s for the Gate to hold; %s", head, c.Agent, c.Model, c.Pool, strings.Join(notes, ", ")),
	}
}

// complete merges explicit input over the candidate (explicit always wins).
func (in Input) complete(c Candidate) Candidate {
	out := c
	if in.Agent != "" {
		out.Agent = in.Agent
	}
	if in.Model != "" {
		out.Model = in.Model
	}
	if in.Pool != "" {
		out.Pool = in.Pool
	}
	return out
}

// noCandidate handles explicit fields that match no table row (e.g. an agent outside the
// table). Explicit values are kept, the pool is resolved from config where possible.
func noCandidate(in Input, head string) Result {
	out := Candidate{Agent: in.Agent, Model: in.Model, Pool: in.Pool}
	if out.Pool == "" && in.ResolvePool != nil {
		out.Pool = in.ResolvePool(out.Agent, out.Model)
	}
	return Result{
		Candidate: out,
		Fallback:  true,
		Reason:    fmt.Sprintf("%s; no table candidate is compatible with the explicit fields (agent=%q model=%q pool=%q); explicit values kept, missing fields left empty", head, in.Agent, in.Model, in.Pool),
	}
}

// Defaults returns what record mode executes with for the given explicit fields: the
// documented default (codex / gpt-6.2-sol / codex) only fills what is empty. A model is
// only defaulted when the agent is the default agent.
func Defaults(agent, model, pool string, resolvePool func(agent, model string) string) Candidate {
	out := Candidate{Agent: agent, Model: model, Pool: pool}
	if out.Agent == "" {
		out.Agent = DefaultCandidate.Agent
	}
	if out.Model == "" && strings.EqualFold(out.Agent, DefaultCandidate.Agent) {
		out.Model = DefaultCandidate.Model
	}
	if out.Pool == "" {
		if resolvePool != nil {
			out.Pool = resolvePool(out.Agent, out.Model)
		}
		if out.Pool == "" && strings.EqualFold(out.Agent, DefaultCandidate.Agent) {
			out.Pool = DefaultCandidate.Pool
		}
	}
	return out
}

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

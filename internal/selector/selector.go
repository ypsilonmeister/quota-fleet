// Package selector chooses agent / model / pool for jobs registered without them
// (docs/10, docs/19). It sits in front of the Quota Gate: it only decides the Gate's
// input and never changes how the Gate judges.
package selector

import (
	"fmt"
	"strings"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/config"
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
	// ModeAbstained: every candidate is unmeasured, so nothing was selected (docs/10 §1).
	ModeAbstained = "abstained"
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
		// docs/10 §4: the judge role is a Selector target and routes to agy Gemini (Neon).
		// (The Gemini Decision evaluator's internal processing is a separate matter and is
		// not a job assignment.)
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
	Pace     config.PaceConfig
	PaceLoad map[string]int
	// ResolvePool maps agent+model to a pool via pools.applies_to. May be nil.
	ResolvePool func(agent, model string) string
}

// Result is the Selector's choice. Fields always honour explicit input.
type Result struct {
	Candidate
	Shadow   *Candidate
	PaceDiff string
	Reason   string
	Fallback bool // true when usage was unreliable and the deterministic default order was used
	// Abstain is true when every candidate pool is unmeasured: the Selector selects nothing
	// (Candidate is empty) and the job is registered with existing defaults.
	Abstain bool
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
	widened := false
	if len(cands) == 0 {
		// The role/weight row has nothing compatible with the explicit fields: widen to
		// every known candidate (fixed, deterministic order) before giving up.
		for _, c := range allCandidates {
			if in.compatible(c) {
				cands = append(cands, c)
			}
		}
		widened = true
	}
	if len(cands) == 0 {
		return noCandidate(in, head)
	}
	if widened {
		head += "; role/weight row has no candidate compatible with the explicit fields, widened to all known candidates"
	}

	var paceEvals []quota.PoolEvaluation
	var paceCandidates []Candidate
	var (
		notes     []string
		excluded  = map[int]string{} // index in notes -> note without the exclusion suffix
		noteOf    = make([]int, len(cands))
		firstLive *Candidate // first candidate that is not exhausted/unmeasured (usage may be unknown)
		healthy   *Candidate
		missing   bool
		stale     bool
		allUnmeas = true
		anyUnmeas bool

		exhaustedIdx = -1
		coolingCount int
	)
	for i := range cands {
		c := cands[i]
		ev := quota.EvaluatePool(c.Pool, in.Usage, in.Eval)
		if ev.Status != quota.PoolUnmeasured {
			allUnmeas = false
		}
		switch ev.Status {
		case quota.PoolHealthy:
			if in.PaceLoad != nil {
				paceEvals = append(paceEvals, ev)
				paceCandidates = append(paceCandidates, c)
			}
			notes = append(notes, fmt.Sprintf("%s: healthy", c.Pool))
			if healthy == nil {
				healthy = &cands[i]
			}
			if firstLive == nil {
				firstLive = &cands[i]
			}
		case quota.PoolCooldown:
			coolingCount++
			noteOf[i] = len(notes)
			excluded[len(notes)] = fmt.Sprintf("%s: cooldown(until=%s)", c.Pool, ev.Cooldown.ActiveUntil.Format(time.RFC3339Nano))
			notes = append(notes, excluded[len(notes)]+", excluded")
		case quota.PoolExhausted:
			noteOf[i] = len(notes)
			excluded[len(notes)] = fmt.Sprintf("%s: exhausted(%s)", c.Pool, ev.BlockedBy)
			notes = append(notes, excluded[len(notes)]+", excluded")
			if exhaustedIdx < 0 {
				exhaustedIdx = i
			}
		case quota.PoolUnmeasured:
			anyUnmeas = true
			// docs/10: pools without a measurement are never chosen automatically.
			noteOf[i] = len(notes)
			excluded[len(notes)] = fmt.Sprintf("%s: unmeasured (pools.%s.unmeasured)", c.Pool, c.Pool)
			notes = append(notes, excluded[len(notes)]+", excluded")
		default:
			why := ev.Reason
			if in.UsageErr != nil {
				why = "usage_unavailable"
			}
			if strings.Contains(strings.ToLower(string(ev.Status)+why), "stale") {
				stale = true
			} else {
				missing = true
			}
			notes = append(notes, fmt.Sprintf("%s: %s(%s), unconfirmed", c.Pool, ev.Status, why))
			if firstLive == nil {
				firstLive = &cands[i]
			}
		}
	}

	if healthy != nil {
		res := Result{Candidate: in.complete(*healthy), Reason: fmt.Sprintf("%s; chose %s/%s on pool %s; %s", head, healthy.Agent, healthy.Model, healthy.Pool, strings.Join(notes, ", "))}
		if ranking := quota.RankHealthy(paceEvals, in.Eval, in.Pace, in.PaceLoad); ranking.Index >= 0 {
			shadow := in.complete(paceCandidates[ranking.Index])
			target := func(c Candidate) string { return fmt.Sprintf("%s/%s@%s", c.Agent, c.Model, c.Pool) }
			comparison := ranking.Describe(target(res.Candidate), target(shadow))
			res.Shadow = &shadow
			res.Reason += "; " + comparison
			if shadow != res.Candidate {
				res.PaceDiff = comparison
			}
		}
		return res
	}
	if firstLive != nil {
		kind := "usage missing"
		if stale && !missing {
			kind = "usage stale"
		} else if stale && missing {
			kind = "usage stale or missing"
		}
		return Result{
			Candidate: in.complete(*firstLive),
			Fallback:  true,
			Reason:    fmt.Sprintf("%s; FALLBACK to deterministic default order (%s): chose %s/%s on pool %s; %s", head, kind, firstLive.Agent, firstLive.Model, firstLive.Pool, strings.Join(notes, ", ")),
		}
	}
	if allUnmeas || coolingCount > 0 && exhaustedIdx < 0 {
		if coolingCount > 0 {
			return Result{Abstain: true, Reason: fmt.Sprintf("%s; no selectable pool; %s", head, strings.Join(notes, ", "))}
		}
		// docs/10 §1: unmeasured pools are never candidates. Abstain instead of adopting one.
		// An explicit or default pool may still be executed, so don't call any of them "excluded".
		for i := range cands {
			notes[noteOf[i]] = excluded[noteOf[i]]
		}
		return Result{
			Abstain: true,
			Reason:  fmt.Sprintf("%s; all candidates unmeasured, automatic selection skipped (全候補が未測定のため自動選択を見送った): job registered with existing defaults; %s", head, strings.Join(notes, ", ")),
		}
	}
	// No pool is selectable (all exhausted, or exhausted mixed with unmeasured). The candidate that
	// is kept is NOT recorded as excluded: its note says it was adopted and why.
	// Exhausted candidates are preferred (the Gate holds them, docs/10 §1); unmeasured pools
	// are adopted only when nothing else exists (the Gate bypasses them).
	ki := 0
	adopt := "kept for the Gate to judge (it holds exhausted pools)"
	if exhaustedIdx >= 0 {
		ki = exhaustedIdx
	}
	c := cands[ki]
	notes[noteOf[ki]] = excluded[noteOf[ki]] + ", " + adopt
	why := "all candidate pools exhausted"
	if anyUnmeas {
		why = "all candidate pools exhausted or unmeasured"
	}
	return Result{
		Candidate: in.complete(c),
		Fallback:  true,
		Reason:    fmt.Sprintf("%s; %s, chose %s/%s on pool %s; %s", head, why, c.Agent, c.Model, c.Pool, strings.Join(notes, ", ")),
	}
}

var allCandidates = []Candidate{codexSol, agyNeonMedium, claudeSonnet, claudeOpus, codexAstra, agyNeonXHigh}

// SwitchTarget finds where a fix could move when the pool of current (the followed parent)
// is exhausted: the first candidate on a healthy pool from another model line (docs/10 §2:
// the line follows the model, not the agent). It searches the role x weight row, then the
// other weights of the role, then every known candidate. It only proposes; nothing is applied.
func SwitchTarget(role, weight string, current Candidate, usage *quota.Snapshot, eval quota.EvalOptions) (Candidate, bool) {
	search := Candidates(role, weight)
	for _, w := range []string{WeightLight, WeightStandard, WeightHeavy} {
		search = append(search, Candidates(role, w)...)
	}
	search = append(search, allCandidates...)

	from := modelLine(current)
	for _, c := range search {
		if modelLine(c) != from && quota.EvaluatePool(c.Pool, usage, eval).Status == quota.PoolHealthy {
			return c, true
		}
	}
	return Candidate{}, false
}

// modelLine is the model family of c, falling back to the agent's native family when the
// model ID does not tell.
func modelLine(c Candidate) string {
	if f := quota.ModelFamily(c.Model); f != quota.ModelFamilyUnknown {
		return f
	}
	switch strings.ToLower(c.Agent) {
	case "claude":
		return quota.ModelFamilyClaude
	case "codex":
		return quota.ModelFamilyGPT
	case "agy":
		return quota.ModelFamilyGemini
	}
	return quota.ModelFamilyUnknown
}

// PoolFor returns the pool of a known table candidate matching agent and model, or "".
func PoolFor(agent, model string) string {
	for _, c := range allCandidates {
		if strings.EqualFold(c.Agent, agent) && c.Model == model {
			return c.Pool
		}
	}
	return ""
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

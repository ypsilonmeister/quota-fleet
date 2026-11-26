// Package chain holds the deterministic parts of chain runs (docs/22, Issue #26): the
// fixed C1 definition, template expansion, the branch function and the review verdict
// parser. It performs no I/O; the Store persists runs and the Chain Driver (package
// runtime) calls these functions.
package chain

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"

	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

const (
	// DefinitionVersion is the version of the template expander stored with each run.
	DefinitionVersion = 1
	// MaxDefinitionBytes bounds the whole registration body.
	MaxDefinitionBytes = 64 << 10
	// MaxInstructionsBytes bounds one step's instructions template.
	MaxInstructionsBytes = 32 << 10
	// DefaultMaxRounds applies when max_rounds is omitted.
	DefaultMaxRounds = 3
	// DefaultMaxRoundsCap is the server-side cap when chains.max_rounds_cap is unset.
	DefaultMaxRoundsCap = 10
	// StartImplement is the only start point in C1 (§3.4 is deferred).
	StartImplement = "implement"
)

// ErrInvalidDefinition marks a definition the server rejects with 400.
var ErrInvalidDefinition = errors.New("invalid chain definition")

// StepTemplate is what a user may write for one step. Everything else (request_id,
// parent_id, draft, host/workdir, chain columns) is decided by the server; unknown keys
// are rejected.
type StepTemplate struct {
	Title              string   `json:"title"`
	Agent              string   `json:"agent"`
	Model              string   `json:"model,omitempty"`
	Pool               string   `json:"pool,omitempty"`
	AltPools           []string `json:"alt_pools,omitempty"`
	Instructions       string   `json:"instructions"`
	AcceptanceCriteria string   `json:"acceptance_criteria,omitempty"`
	TargetRef          string   `json:"target_ref,omitempty"`
}

// Steps is the fixed set of C1 step templates.
type Steps struct {
	Implement *StepTemplate `json:"implement"`
	Review    *StepTemplate `json:"review"`
	Fix       *StepTemplate `json:"fix"`
	Publish   *StepTemplate `json:"publish"`
}

// Workspace is shared by all steps of a run.
type Workspace struct {
	Host         string              `json:"host,omitempty"`
	Workdir      string              `json:"workdir,omitempty"`
	WorktreeSpec *store.WorktreeSpec `json:"worktree_spec,omitempty"`
}

// Definition is the POST /chains body (docs/22 §3.1).
type Definition struct {
	RequestID       string    `json:"request_id"`
	Title           string    `json:"title"`
	Scope           string    `json:"scope,omitempty"`
	MaxRounds       int       `json:"max_rounds,omitempty"`
	Start           string    `json:"start,omitempty"`
	StartAfterJobID *int64    `json:"start_after_job_id,omitempty"`
	Workspace       Workspace `json:"workspace"`
	Steps           Steps     `json:"steps"`
}

// Step returns the template for a step name.
func (d *Definition) Step(name string) *StepTemplate {
	switch name {
	case store.ChainStepImplement:
		return d.Steps.Implement
	case store.ChainStepReview:
		return d.Steps.Review
	case store.ChainStepFix:
		return d.Steps.Fix
	case store.ChainStepPublish:
		return d.Steps.Publish
	}
	return nil
}

// ParseDefinition decodes a registration body strictly (unknown keys, trailing data and
// oversize bodies are rejected), applies defaults and validates it against maxRoundsCap.
func ParseDefinition(raw []byte, maxRoundsCap int) (*Definition, error) {
	if len(raw) > MaxDefinitionBytes {
		return nil, fmt.Errorf("%w: definition exceeds %d bytes", ErrInvalidDefinition, MaxDefinitionBytes)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var d Definition
	if err := dec.Decode(&d); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidDefinition, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: trailing data after the definition", ErrInvalidDefinition)
	}
	if err := d.normalize(maxRoundsCap); err != nil {
		return nil, err
	}
	return &d, nil
}

func (d *Definition) normalize(maxRoundsCap int) error {
	if maxRoundsCap <= 0 {
		maxRoundsCap = DefaultMaxRoundsCap
	}
	d.RequestID = strings.TrimSpace(d.RequestID)
	d.Title = strings.TrimSpace(d.Title)
	d.Scope = strings.TrimSpace(d.Scope)
	d.Start = strings.TrimSpace(d.Start)
	if d.RequestID == "" {
		return fmt.Errorf("%w: request_id must not be empty", ErrInvalidDefinition)
	}
	if d.Title == "" {
		return fmt.Errorf("%w: title must not be empty", ErrInvalidDefinition)
	}
	if d.MaxRounds == 0 {
		d.MaxRounds = DefaultMaxRounds
	}
	if d.MaxRounds < 1 || d.MaxRounds > maxRoundsCap {
		return fmt.Errorf("%w: max_rounds must be between 1 and %d (got %d)", ErrInvalidDefinition, maxRoundsCap, d.MaxRounds)
	}
	if d.Start == "" {
		d.Start = StartImplement
	}
	if d.Start != StartImplement {
		return fmt.Errorf("%w: start %q is not supported (C1 starts at implement; attaching to existing jobs is deferred)", ErrInvalidDefinition, d.Start)
	}
	if d.StartAfterJobID != nil {
		return fmt.Errorf("%w: start_after_job_id is not supported in C1", ErrInvalidDefinition)
	}
	d.Workspace.Host = strings.TrimSpace(d.Workspace.Host)
	d.Workspace.Workdir = strings.TrimSpace(d.Workspace.Workdir)
	if ws := d.Workspace.WorktreeSpec; ws != nil {
		ws.RepoPath = strings.TrimSpace(ws.RepoPath)
		ws.BaseRef = strings.TrimSpace(ws.BaseRef)
		ws.Branch = strings.TrimSpace(ws.Branch)
		if ws.RepoPath == "" && ws.BaseRef == "" && ws.Branch == "" {
			d.Workspace.WorktreeSpec = nil
		}
	}
	// Every step must run in the same directory (docs/22 §7.2). An empty workdir is
	// resolved per job by the adapter (WorkdirRoot/job-<id>), so worktree_spec alone would
	// give each step its own worktree; the shared workdir is therefore required, and
	// worktree_spec only says how to create it the first time (later steps reuse it).
	if d.Workspace.Workdir == "" {
		if d.Workspace.WorktreeSpec != nil {
			return fmt.Errorf("%w: workspace.workdir is required with workspace.worktree_spec (without it each step would get its own worktree)", ErrInvalidDefinition)
		}
		return fmt.Errorf("%w: workspace.workdir is required (all steps share it)", ErrInvalidDefinition)
	}
	for _, name := range []string{store.ChainStepImplement, store.ChainStepReview, store.ChainStepFix, store.ChainStepPublish} {
		t := d.Step(name)
		if t == nil {
			return fmt.Errorf("%w: steps.%s is required", ErrInvalidDefinition, name)
		}
		t.Title = strings.TrimSpace(t.Title)
		t.Agent = strings.TrimSpace(t.Agent)
		t.Model = strings.TrimSpace(t.Model)
		t.Pool = strings.TrimSpace(t.Pool)
		t.TargetRef = strings.TrimSpace(t.TargetRef)
		if t.Title == "" {
			return fmt.Errorf("%w: steps.%s.title must not be empty", ErrInvalidDefinition, name)
		}
		if t.Agent == "" {
			return fmt.Errorf("%w: steps.%s.agent is required in C1", ErrInvalidDefinition, name)
		}
		if strings.TrimSpace(t.Instructions) == "" {
			return fmt.Errorf("%w: steps.%s.instructions must not be empty", ErrInvalidDefinition, name)
		}
		if len(t.Instructions) > MaxInstructionsBytes {
			return fmt.Errorf("%w: steps.%s.instructions exceeds %d bytes", ErrInvalidDefinition, name, MaxInstructionsBytes)
		}
		for _, field := range []struct{ name, text string }{
			{"title", t.Title}, {"instructions", t.Instructions}, {"acceptance_criteria", t.AcceptanceCriteria},
		} {
			if _, err := Expand(field.text, placeholderVars(name)); err != nil {
				return fmt.Errorf("%w: steps.%s.%s: %v", ErrInvalidDefinition, name, field.name, err)
			}
		}
	}
	for _, v := range []string{"verdict.path", "verdict.nonce"} {
		if !mentions(d.Steps.Review.Instructions, v) {
			return fmt.Errorf("%w: steps.review.instructions must contain {{%s}} so the reviewer can write the verdict", ErrInvalidDefinition, v)
		}
	}
	return nil
}

// CanonicalJSON returns the normalized definition and its SHA-256 (definition_hash).
func (d *Definition) CanonicalJSON() (string, string, error) {
	b, err := json.Marshal(d)
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256(b)
	return string(b), hex.EncodeToString(sum[:]), nil
}

// MaxSteps is the step budget stored on the run: 2 × max_rounds + 1 (§3.2).
func MaxSteps(maxRounds int) int { return 2*maxRounds + 1 }

// VerdictPath is the workdir-relative verdict file of review step seq (§7.1).
func VerdictPath(runID string, seq int) string {
	return ".qf-chain/" + runID + "/" + strconv.Itoa(seq) + "/verdict.json"
}

var placeholderRe = regexp.MustCompile(`\{\{(.*?)\}\}`)

func mentions(text, name string) bool {
	for _, m := range placeholderRe.FindAllStringSubmatch(text, -1) {
		if strings.TrimSpace(m[1]) == name {
			return true
		}
	}
	return false
}

// allowedVars lists the variables a step may use (§3.1).
func allowedVars(step string) []string {
	vars := []string{"round", "prev.job_id", "run.id", "run.scope"}
	switch step {
	case store.ChainStepReview:
		vars = append(vars, "verdict.path", "verdict.nonce")
	case store.ChainStepFix, store.ChainStepPublish:
		vars = append(vars, "verdict.reviewed_commit")
	}
	return vars
}

func placeholderVars(step string) map[string]string {
	out := map[string]string{}
	for _, v := range allowedVars(step) {
		out[v] = "x"
	}
	return out
}

// Expand replaces {{name}} with vars[name]. A placeholder whose name is not in vars is an
// error, so registration-time validation and generation-time expansion (both through this
// function) reject the same inputs.
func Expand(text string, vars map[string]string) (string, error) {
	var firstErr error
	out := placeholderRe.ReplaceAllStringFunc(text, func(m string) string {
		name := strings.TrimSpace(m[2 : len(m)-2])
		v, ok := vars[name]
		if !ok && firstErr == nil {
			firstErr = fmt.Errorf("unknown or disallowed variable {{%s}}", name)
		}
		return v
	})
	if firstErr != nil {
		return "", firstErr
	}
	return out, nil
}

// StepInput is what the Driver knows when it materializes a step.
type StepInput struct {
	Run            *store.ChainRun
	Step           string
	Seq            int
	Round          int
	PrevJobID      int64 // 0 for the first step
	VerdictNonce   string
	ReviewedCommit string
}

// BuildStepParams expands the step template into job parameters. RequestID and ParentID
// are left for the Store; CreatedBy is the run owner.
func BuildStepParams(d *Definition, in StepInput) (store.CreateJobParams, error) {
	t := d.Step(in.Step)
	if t == nil {
		return store.CreateJobParams{}, fmt.Errorf("%w: unknown step %q", ErrInvalidDefinition, in.Step)
	}
	vars := map[string]string{
		"round":       strconv.Itoa(in.Round),
		"prev.job_id": "",
		"run.id":      in.Run.ID,
		"run.scope":   in.Run.Scope,
	}
	if in.PrevJobID > 0 {
		vars["prev.job_id"] = strconv.FormatInt(in.PrevJobID, 10)
	}
	switch in.Step {
	case store.ChainStepReview:
		vars["verdict.path"] = VerdictPath(in.Run.ID, in.Seq)
		vars["verdict.nonce"] = in.VerdictNonce
	case store.ChainStepFix, store.ChainStepPublish:
		vars["verdict.reviewed_commit"] = in.ReviewedCommit
	}
	title, err := Expand(t.Title, vars)
	if err != nil {
		return store.CreateJobParams{}, err
	}
	instr, err := Expand(t.Instructions, vars)
	if err != nil {
		return store.CreateJobParams{}, err
	}
	acc, err := Expand(t.AcceptanceCriteria, vars)
	if err != nil {
		return store.CreateJobParams{}, err
	}
	var wt *store.WorktreeSpec
	if d.Workspace.WorktreeSpec != nil {
		cp := *d.Workspace.WorktreeSpec
		wt = &cp
	}
	return store.CreateJobParams{
		Title:              title,
		Agent:              t.Agent,
		Model:              t.Model,
		Pool:               t.Pool,
		AltPools:           append([]string(nil), t.AltPools...),
		Host:               d.Workspace.Host,
		Workdir:            d.Workspace.Workdir,
		WorktreeSpec:       wt,
		TargetRef:          t.TargetRef,
		Instructions:       instr,
		AcceptanceCriteria: acc,
		CreatedBy:          in.Run.Owner,
	}, nil
}

// DecodeStored decodes a definition_json written by CanonicalJSON.
func DecodeStored(raw string) (*Definition, error) {
	var d Definition
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		return nil, fmt.Errorf("decode stored chain definition: %w", err)
	}
	return &d, nil
}

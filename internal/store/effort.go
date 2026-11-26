package store

import (
	"fmt"
	"strings"
)

// Per-job reasoning effort values (docs/21).
const (
	EffortLow    = "low"
	EffortMedium = "medium"
	EffortHigh   = "high"
	EffortXHigh  = "xhigh"
)

// ValidEfforts lists the accepted effort values in ascending order.
var ValidEfforts = []string{EffortLow, EffortMedium, EffortHigh, EffortXHigh}

// EffortAgents lists the agents whose launch command can carry an effort (docs/21).
var EffortAgents = []string{"claude", "codex"}

// ValidateEffort checks a job's effort against its agent (docs/21).
// An empty effort is always valid (the agent's own default is used). A non-empty effort must be
// one of ValidEfforts and the agent must be one that receives it on launch (claude, codex).
// agy is rejected because its effort is part of the model ID; any other or empty agent is
// rejected because the effort could not be applied. Errors wrap ErrInvalidInput.
func ValidateEffort(agent, effort string) error {
	effort = strings.TrimSpace(effort)
	if effort == "" {
		return nil
	}
	valid := false
	for _, v := range ValidEfforts {
		if effort == v {
			valid = true
			break
		}
	}
	if !valid {
		return fmt.Errorf("%w: effort %q is not supported (%s)", ErrInvalidInput, effort, strings.Join(ValidEfforts, "|"))
	}
	agent = strings.TrimSpace(agent)
	for _, a := range EffortAgents {
		if agent == a {
			return nil
		}
	}
	switch agent {
	case "agy":
		return fmt.Errorf("%w: effort cannot be combined with agent agy (agy selects effort through the model ID, e.g. gemini-4-neon-medium)", ErrInvalidInput)
	case "":
		return fmt.Errorf("%w: effort %q requires an agent (%s); the job has no agent", ErrInvalidInput, effort, strings.Join(EffortAgents, "|"))
	default:
		return fmt.Errorf("%w: effort is not supported for agent %q (supported: %s)", ErrInvalidInput, agent, strings.Join(EffortAgents, "|"))
	}
}

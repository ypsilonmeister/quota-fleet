package runtime

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/adapter"
	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/quota"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

var rateLimitPatterns = map[string]*regexp.Regexp{
	"claude": regexp.MustCompile(`(?i)(you've hit your.*limit|usage limit reached|rate_limit_error|api error:\s*429|anthropic.*429.*too many requests)`),
	"codex":  regexp.MustCompile(`(?i)(you've hit your usage limit|usage_limit_reached|rate_limit_exceeded|exceeded your current quota|status\s*429.*too many requests|http[/ ]?[\d.]*\s+429)`),
	"agy":    regexp.MustCompile(`(?i)(RESOURCE_EXHAUSTED|quota exceeded|rate.?limit.*exceeded|429 too many requests|status[:= ]+429)`),
	"":       regexp.MustCompile(`(?i)(429 too many requests|rate_limit_exceeded|rate_limit_error|usage_limit_reached|RESOURCE_EXHAUSTED)`),
}

var nonProviderOutput = regexp.MustCompile(`(?i)^\s*(--- FAIL:|FAIL\b|[+-]{1,3}\s|npm\b|gh:|(?:curl|wget|fetch|tool output)\b|\x60|["'])|\b(?:api\.github\.com|registry\.npmjs\.org|npm ERR!)\b`)

// Detect only in the final CLI output. Custom patterns replace the built-in set.
func detectRateLimit(agent string, cfg config.AgentConfig, state adapter.RunState, ev adapter.Evidence, evaluatorFailed bool) string {
	if state.ExitCodeUnknown {
		if !evaluatorFailed {
			return ""
		}
	} else {
		if state.ExitCode == 0 {
			return ""
		}
		if slices.Contains(cfg.RateLimitExitCodes, state.ExitCode) {
			return fmt.Sprintf("exit_code:%d", state.ExitCode)
		}
	}
	pattern := rateLimitPatterns[agent]
	if pattern == nil {
		pattern = rateLimitPatterns[""]
	}
	customPatterns := make([]*regexp.Regexp, len(cfg.RateLimitPatterns))
	for i, raw := range cfg.RateLimitPatterns {
		customPatterns[i], _ = regexp.Compile(raw) // fleet.yaml validation reports invalid patterns
	}

	for _, tail := range []string{ev.ErrLogTail, ev.FinalReport} {
		if len(tail) > 4096 {
			tail = tail[len(tail)-4096:]
			// Discard the partial line so truncation cannot hide its prefix.
			_, tail, _ = strings.Cut(tail, "\n")
		}
		lines := strings.Split(strings.TrimRight(tail, "\r\n"), "\n")
		if len(lines) > 30 {
			lines = lines[len(lines)-30:]
		}
		for i := len(lines) - 1; i >= 0; i-- {
			line := lines[i]
			if nonProviderOutput.MatchString(line) {
				continue
			}
			if cfg.RateLimitPatterns == nil {
				// Codex command output can contain arbitrary HTTP headers.
				// Require an error envelope for generic HTTP status signatures.
				match := pattern.FindString(line)
				if match != "" {
					normalized := strings.ToLower(match)
					if strings.HasPrefix(normalized, "http") &&
						!strings.Contains(strings.ToLower(line), "error") &&
						!strings.Contains(strings.ToLower(line), "openai") {
						continue
					}
					switch {
					case strings.Contains(normalized, "429"):
						return agent + ":http_429"
					default:
						return agent + ":quota_limit"
					}
				}
			} else {
				for j, re := range customPatterns {
					if re != nil && re.MatchString(line) {
						return fmt.Sprintf("custom:%d", j+1)
					}
				}
			}
		}
	}
	return ""
}

func (s *Shepherd) failExited(ctx context.Context, job *store.Job, state adapter.RunState, ev adapter.Evidence, evaluatorFailed bool, reason, result string) (*store.FailResult, error) {
	pattern := ""
	if cfg := s.config; cfg != nil {
		pool := job.Pool
		if pool == "" {
			pool = quota.ResolvePoolByAppliesTo(cfg.Pools, job.Agent, job.Model)
		}
		if _, defined := cfg.Pools[pool]; defined && cfg.CooldownFor(pool).Enabled {
			pattern = detectRateLimit(job.Agent, cfg.Agents[job.Agent], state, ev, evaluatorFailed)
		}
	}
	out, err := s.store.FailWithCooldown(ctx, job.ID, job.ClaimedBy, reason, result, pattern)
	if err != nil || pattern == "" {
		return out, err
	}
	if s.config.Quota.EffectiveGateMode() == config.GateModeOff {
		s.logger.Printf("pool rate limit detected: job #%d pattern=%s (gate off)", job.ID, pattern)
	}
	if c := out.Cooldown; c != nil && s.config.Quota.EffectiveGateMode() == config.GateModeEnforce && s.notifier != nil {
		err := s.notifier.Report(ctx, Report{
			DedupKey: "pool-cooldown:" + c.Pool + ":" + c.TriggeredAt.Format(time.RFC3339Nano),
			Kind:     "pool_cooldown", JobID: job.ID, Message: "pool " + c.Pool + " " + c.Reason(), Timestamp: c.TriggeredAt,
		})
		if err != nil {
			s.logger.Printf("pool cooldown notification error: %v", err)
		}
	}
	return out, nil
}

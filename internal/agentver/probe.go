package agentver

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/adapter"
	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

const (
	// DefaultProbeTimeout is the per-(host, agent) command timeout for version probes.
	DefaultProbeTimeout = 15 * time.Second

	maxRawOutputBytes   = 512
	maxErrorDetailBytes = 512
)

// EventHook is invoked after each new AgentVersionEvent is persisted to the store.
type EventHook func(ctx context.Context, ev *store.AgentVersionEvent)

// CheckerOptions configures a Checker.
type CheckerOptions struct {
	Store        *store.Store
	Config       *config.Config
	Provider     adapter.HostFileAccessProvider
	ProbeTimeout time.Duration
	Now          func() time.Time
	OnEvent      EventHook
	Logger       *log.Logger
}

// CheckResult is returned by Checker.Check with the updated matrix and any newly recorded events.
type CheckResult struct {
	Matrix Matrix                     `json:"matrix"`
	Events []*store.AgentVersionEvent `json:"events"`
}

// Checker probes agent versions across configured hosts via FileAccess.RunCommand,
// persists results in agent_versions, detects changes/drift, and records agent_version_events.
type Checker struct {
	store        *store.Store
	cfg          *config.Config
	provider     adapter.HostFileAccessProvider
	probeTimeout time.Duration
	now          func() time.Time
	onEvent      EventHook
	logger       *log.Logger
}

// NewChecker constructs a Checker.
func NewChecker(opts CheckerOptions) (*Checker, error) {
	if opts.Store == nil {
		return nil, errors.New("agentver: Store must not be nil")
	}
	if opts.Config == nil {
		return nil, errors.New("agentver: Config must not be nil")
	}
	if opts.Provider == nil {
		return nil, errors.New("agentver: Provider must not be nil")
	}
	timeout := opts.ProbeTimeout
	if timeout <= 0 {
		timeout = DefaultProbeTimeout
	}
	nowFn := opts.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	logger := opts.Logger
	if logger == nil {
		logger = log.Default()
	}
	return &Checker{
		store:        opts.Store,
		cfg:          opts.Config,
		provider:     opts.Provider,
		probeTimeout: timeout,
		now:          nowFn,
		onEvent:      opts.OnEvent,
		logger:       logger,
	}, nil
}

// Check probes every configured host × resolvable agent, atomically persists agent_versions
// and change/drift/failure events in agent_version_events within a single transaction, and
// invokes OnEvent for each event after commit.
func (c *Checker) Check(ctx context.Context) (*CheckResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	hosts, agents := configuredHostsAndAgents(c.cfg)
	probed := make([]store.AgentVersion, 0, len(hosts)*len(agents))

	for _, hostName := range hosts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		hostCfg := c.cfg.Hosts[hostName]
		fa, faErr := c.provider.FileAccessForHost(hostName)

		for _, agentName := range agents {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			now := c.now().UTC()
			var next store.AgentVersion
			if faErr != nil {
				next = store.AgentVersion{
					Host:        hostName,
					Agent:       agentName,
					Status:      store.AgentVersionStatusError,
					ErrorDetail: truncateString(faErr.Error(), maxErrorDetailBytes),
					CheckedAt:   now,
				}
			} else {
				next = c.probeOne(ctx, fa, hostName, hostCfg, agentName, now)
			}
			probed = append(probed, next)
		}
	}

	type pairKey struct {
		host  string
		agent string
	}

	allRows, recorded, err := c.store.ApplyAgentVersionSnapshot(ctx, probed, func(prevRows, nextRows []*store.AgentVersion) ([]store.AgentVersionEvent, error) {
		prevMatrix := BuildMatrix(c.cfg, prevRows)
		nextMatrix := BuildMatrix(c.cfg, nextRows)

		prevByKey := make(map[pairKey]*store.AgentVersion, len(prevRows))
		for _, r := range prevRows {
			prevByKey[pairKey{host: r.Host, agent: r.Agent}] = r
		}

		var newEvents []store.AgentVersionEvent
		for _, next := range probed {
			_, manual := resolvePolicy(c.cfg, next.Host, next.Agent)
			prev := prevByKey[pairKey{host: next.Host, agent: next.Agent}]
			if ev, ok := diffCellEvent(prev, next, manual); ok {
				newEvents = append(newEvents, ev)
			}
		}

		prevSumByAgent := make(map[string]AgentSummary, len(prevMatrix.Summaries))
		for _, s := range prevMatrix.Summaries {
			prevSumByAgent[s.Agent] = s
		}
		eventTime := c.now().UTC()
		for _, nextSum := range nextMatrix.Summaries {
			prevSum := prevSumByAgent[nextSum.Agent]
			if ev, ok := diffDriftEvent(prevSum, nextSum, eventTime); ok {
				newEvents = append(newEvents, ev)
			}
		}
		return newEvents, nil
	})
	if err != nil {
		return nil, err
	}

	nextMatrix := BuildMatrix(c.cfg, allRows)
	for _, rec := range recorded {
		if c.onEvent != nil {
			c.onEvent(ctx, rec)
		}
	}

	return &CheckResult{
		Matrix: nextMatrix,
		Events: recorded,
	}, nil
}

func (c *Checker) probeOne(ctx context.Context, fa adapter.FileAccess, hostName string, hostCfg config.HostConfig, agentName string, now time.Time) store.AgentVersion {
	cmd, targetExe, notFoundMarker, err := buildAgentVersionCommand(c.cfg, hostCfg, agentName)
	if err != nil {
		return store.AgentVersion{
			Host:        hostName,
			Agent:       agentName,
			Status:      store.AgentVersionStatusError,
			ErrorDetail: truncateString(err.Error(), maxErrorDetailBytes),
			CheckedAt:   now,
		}
	}

	probeCtx, cancel := context.WithTimeout(ctx, c.probeTimeout)
	defer cancel()

	res, runErr := fa.RunCommand(probeCtx, cmd)
	return classifyProbeResult(hostName, agentName, targetExe, notFoundMarker, res, runErr, now)
}

func buildAgentVersionCommand(cfg *config.Config, hostCfg config.HostConfig, agentName string) (string, string, string, error) {
	driver, err := adapter.ResolveDriver(cfg, hostCfg, agentName)
	if err != nil {
		// Allow agents that only specify a host command override or agent name without a launch template.
		driver = adapter.AgentDriver{Name: agentName}
		if hostCfg.Agents != nil {
			if ov, ok := hostCfg.Agents[agentName]; ok {
				driver.CommandOverride = strings.TrimSpace(ov.Command)
				if len(ov.Env) > 0 {
					driver.Env = make(map[string]string, len(ov.Env))
					for k, v := range ov.Env {
						driver.Env[k] = v
					}
				}
			}
		}
		if cfg != nil {
			if ac, ok := cfg.Agents[agentName]; ok {
				driver.Preflight = strings.TrimSpace(ac.Preflight)
				driver.Launch = strings.TrimSpace(ac.Launch)
			}
		}
	}

	versionArgs := resolveVersionArgs(cfg, hostCfg, agentName)
	isWin := strings.EqualFold(strings.TrimSpace(hostCfg.OS), "windows")
	cmd, notFoundMarker, err := driver.BuildVersionCommand(versionArgs, isWin)
	if err != nil {
		return "", "", "", err
	}
	return cmd, driver.ResolveBaseCommand(), notFoundMarker, nil
}

func resolveVersionArgs(cfg *config.Config, hostCfg config.HostConfig, agentName string) []string {
	if hostCfg.Agents != nil {
		if ov, ok := hostCfg.Agents[agentName]; ok && len(ov.VersionArgs) > 0 {
			return ov.VersionArgs
		}
	}
	if cfg != nil {
		if ac, ok := cfg.Agents[agentName]; ok && len(ac.VersionArgs) > 0 {
			return ac.VersionArgs
		}
	}
	return nil
}

func classifyProbeResult(hostName, agentName, targetExe, notFoundMarker string, res adapter.ExecResult, runErr error, now time.Time) store.AgentVersion {
	if runErr != nil {
		detail := runErr.Error()
		if trimmed := strings.TrimSpace(res.Output); trimmed != "" {
			detail = fmt.Sprintf("%s: %s", detail, trimmed)
		}
		return store.AgentVersion{
			Host:        hostName,
			Agent:       agentName,
			Status:      store.AgentVersionStatusError,
			ErrorDetail: truncateString(detail, maxErrorDetailBytes),
			CheckedAt:   now,
		}
	}

	trimmedOut := strings.TrimSpace(res.Output)
	if res.ExitCode != 0 {
		if detail, ok := detectTargetCommandNotFound(targetExe, notFoundMarker, res.ExitCode, res.Output); ok {
			return store.AgentVersion{
				Host:        hostName,
				Agent:       agentName,
				Status:      store.AgentVersionStatusNotFound,
				ErrorDetail: truncateString(detail, maxErrorDetailBytes),
				CheckedAt:   now,
			}
		}
		detail := fmt.Sprintf("exit code %d", res.ExitCode)
		if trimmedOut != "" {
			detail = fmt.Sprintf("exit code %d: %s", res.ExitCode, trimmedOut)
		}
		return store.AgentVersion{
			Host:        hostName,
			Agent:       agentName,
			Status:      store.AgentVersionStatusError,
			ErrorDetail: truncateString(detail, maxErrorDetailBytes),
			CheckedAt:   now,
		}
	}

	if trimmedOut == "" {
		return store.AgentVersion{
			Host:        hostName,
			Agent:       agentName,
			Status:      store.AgentVersionStatusError,
			ErrorDetail: "empty version output",
			CheckedAt:   now,
		}
	}

	norm := NormalizeVersion(trimmedOut)
	if norm == "" {
		return store.AgentVersion{
			Host:        hostName,
			Agent:       agentName,
			Status:      store.AgentVersionStatusError,
			ErrorDetail: "empty normalized version",
			CheckedAt:   now,
		}
	}

	return store.AgentVersion{
		Host:       hostName,
		Agent:      agentName,
		VersionRaw: truncateString(trimmedOut, maxRawOutputBytes),
		Version:    norm,
		Status:     store.AgentVersionStatusOK,
		CheckedAt:  now,
	}
}

// detectTargetCommandNotFound classifies a probe failure as not_found strictly when
// BuildVersionCommand's pre-check emitted the exact per-probe nonce-bearing marker line
// with exit code 127. Output-text heuristics and static markers are intentionally rejected
// so an existing executable cannot spoof not_found.
func detectTargetCommandNotFound(targetExe, notFoundMarker string, exitCode int, output string) (string, bool) {
	if exitCode != 127 {
		return "", false
	}
	marker := strings.TrimSpace(notFoundMarker)
	if marker == "" {
		return "", false
	}
	target := strings.TrimSpace(targetExe)
	expectedLine := marker
	if target != "" {
		expectedLine = marker + " " + target
	}
	for _, line := range strings.Split(output, "\n") {
		if strings.TrimSuffix(line, "\r") == expectedLine {
			if target != "" {
				return fmt.Sprintf("command not found: %s", target), true
			}
			return "command not found (exit 127)", true
		}
	}
	return "", false
}

func diffCellEvent(prev *store.AgentVersion, next store.AgentVersion, manualUpdate bool) (store.AgentVersionEvent, bool) {
	if next.Status == store.AgentVersionStatusOK {
		if prev == nil {
			return store.AgentVersionEvent{}, false
		}
		if prev.Status != store.AgentVersionStatusOK {
			// Recovery from previous probe failure (docs/08 §2: 初回失敗と回復も含む).
			return store.AgentVersionEvent{
				EventType:    store.AgentEventProbeFailed,
				Agent:        next.Agent,
				Host:         next.Host,
				OldVersion:   string(prev.Status),
				NewVersion:   next.Version,
				Detail:       fmt.Sprintf("probe recovered on %s/%s: %s -> ok (%s)", next.Host, next.Agent, prev.Status, next.Version),
				ManualUpdate: manualUpdate,
				CreatedAt:    next.CheckedAt,
			}, true
		}
		if prev.Version != next.Version {
			return store.AgentVersionEvent{
				EventType:    store.AgentEventVersionChanged,
				Agent:        next.Agent,
				Host:         next.Host,
				OldVersion:   prev.Version,
				NewVersion:   next.Version,
				Detail:       fmt.Sprintf("%s on %s changed: %s -> %s", next.Agent, next.Host, prev.Version, next.Version),
				ManualUpdate: manualUpdate,
				CreatedAt:    next.CheckedAt,
			}, true
		}
		return store.AgentVersionEvent{}, false
	}

	// next.Status is not_found or error.
	if prev == nil || prev.Status != next.Status {
		oldVer := ""
		if prev != nil {
			if prev.Status == store.AgentVersionStatusOK {
				oldVer = prev.Version
			} else {
				oldVer = string(prev.Status)
			}
		}
		return store.AgentVersionEvent{
			EventType:    store.AgentEventProbeFailed,
			Agent:        next.Agent,
			Host:         next.Host,
			OldVersion:   oldVer,
			NewVersion:   string(next.Status),
			Detail:       fmt.Sprintf("probe %s on %s/%s: %s", next.Status, next.Host, next.Agent, next.ErrorDetail),
			ManualUpdate: manualUpdate,
			CreatedAt:    next.CheckedAt,
		}, true
	}
	return store.AgentVersionEvent{}, false
}

func diffDriftEvent(prev, next AgentSummary, now time.Time) (store.AgentVersionEvent, bool) {
	if !prev.Drifted && next.Drifted {
		var parts []string
		if next.Comparable && next.MaxVersion != "" {
			parts = append(parts, fmt.Sprintf("drift detected for %s (max=%s, versions=%s)",
				next.Agent, next.MaxVersion, strings.Join(next.Versions, ",")))
			if len(next.LaggingHosts) > 0 {
				parts = append(parts, fmt.Sprintf("lagging=[%s]", strings.Join(next.LaggingHosts, ",")))
			}
		} else {
			parts = append(parts, fmt.Sprintf("drift detected for %s (比較不能/non-semver, versions=%s)",
				next.Agent, strings.Join(next.Versions, ",")))
		}
		manualRequired := len(next.ManualUpdateHosts) > 0
		if manualRequired {
			parts = append(parts, fmt.Sprintf("自動更新不可・手作業が必要 (manual_update: %s)", strings.Join(next.ManualUpdateHosts, ",")))
		}
		return store.AgentVersionEvent{
			EventType:    store.AgentEventDriftStarted,
			Agent:        next.Agent,
			Host:         strings.Join(next.LaggingHosts, ","),
			NewVersion:   next.MaxVersion,
			Detail:       strings.Join(parts, "; "),
			ManualUpdate: manualRequired,
			CreatedAt:    now,
		}, true
	}

	if prev.Drifted && !next.Drifted {
		converged := next.MaxVersion
		if converged == "" && len(next.Versions) == 1 {
			converged = next.Versions[0]
		}
		detail := fmt.Sprintf("drift resolved for %s", next.Agent)
		if converged != "" {
			detail = fmt.Sprintf("drift resolved for %s (version=%s)", next.Agent, converged)
		}
		return store.AgentVersionEvent{
			EventType:  store.AgentEventDriftResolved,
			Agent:      next.Agent,
			OldVersion: prev.MaxVersion,
			NewVersion: converged,
			Detail:     detail,
			CreatedAt:  now,
		}, true
	}

	return store.AgentVersionEvent{}, false
}

func configuredHostsAndAgents(cfg *config.Config) ([]string, []string) {
	if cfg == nil {
		return nil, nil
	}
	hosts := make([]string, 0, len(cfg.Hosts))
	agentSet := make(map[string]struct{})
	for h, hc := range cfg.Hosts {
		if name := strings.TrimSpace(h); name != "" {
			hosts = append(hosts, name)
		}
		for a := range hc.Agents {
			if name := strings.TrimSpace(a); name != "" {
				agentSet[name] = struct{}{}
			}
		}
	}
	for a := range cfg.Agents {
		if name := strings.TrimSpace(a); name != "" {
			agentSet[name] = struct{}{}
		}
	}
	sort.Strings(hosts)

	agents := make([]string, 0, len(agentSet))
	for a := range agentSet {
		agents = append(agents, a)
	}
	sort.Strings(agents)
	return hosts, agents
}

func truncateString(s string, maxLen int) string {
	s = strings.TrimSpace(s)
	if maxLen <= 0 || len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

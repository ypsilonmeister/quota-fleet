package agentmodels

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
	// DefaultProbeTimeout is the per-(host, agent) timeout for model catalog probes.
	DefaultProbeTimeout = 30 * time.Second

	SourceAgyCLI     = "agy_cli"
	SourceCodexCache = "codex_cache"
	SourceConfig     = "config"

	maxErrorDetailBytes = 512
)

// ProberOptions configures a model catalog Prober.
type ProberOptions struct {
	Store        *store.Store
	Config       *config.Config
	Provider     adapter.HostFileAccessProvider
	ProbeTimeout time.Duration
	Now          func() time.Time
	Logger       *log.Logger
}

// Prober discovers available models per host × agent and persists snapshots in agent_models.
type Prober struct {
	store        *store.Store
	cfg          *config.Config
	provider     adapter.HostFileAccessProvider
	probeTimeout time.Duration
	now          func() time.Time
	logger       *log.Logger
}

// NewProber constructs a model catalog Prober.
func NewProber(opts ProberOptions) (*Prober, error) {
	if opts.Store == nil {
		return nil, errors.New("agentmodels: Store must not be nil")
	}
	if opts.Config == nil {
		return nil, errors.New("agentmodels: Config must not be nil")
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
	return &Prober{
		store:        opts.Store,
		cfg:          opts.Config,
		provider:     opts.Provider,
		probeTimeout: timeout,
		now:          nowFn,
		logger:       logger,
	}, nil
}

// Probe iterates all configured host × agent pairs, updates agent_models on success,
// and preserves any previously stored catalog when a probe fails.
func (p *Prober) Probe(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	hosts := sortedHostNames(p.cfg)
	topAgents := sortedAgentNames(p.cfg.Agents)

	for _, hostName := range hosts {
		if err := ctx.Err(); err != nil {
			return err
		}
		hostCfg := p.cfg.Hosts[hostName]
		agents := agentsForHost(hostCfg, topAgents)

		var (
			fa    adapter.FileAccess
			faErr error
		)
		if p.provider != nil {
			fa, faErr = p.provider.FileAccessForHost(hostName)
		}

		for _, agentName := range agents {
			if err := ctx.Err(); err != nil {
				return err
			}
			now := p.now().UTC()
			if err := p.probeOne(ctx, fa, faErr, hostName, hostCfg, agentName, now); err != nil {
				p.logger.Printf("agentmodels: persist error for %s/%s: %v", hostName, agentName, err)
			}
		}
	}
	return nil
}

func (p *Prober) probeOne(
	ctx context.Context,
	fa adapter.FileAccess,
	faErr error,
	hostName string,
	hostCfg config.HostConfig,
	agentName string,
	now time.Time,
) error {
	switch strings.ToLower(strings.TrimSpace(agentName)) {
	case "claude":
		// For claude, fleet.yaml (agents.claude.models) is the source of truth.
		// Always upsert (including an empty slice) so deleting models from config
		// clears any previously stored snapshot.
		items := CatalogFromConfig(p.cfg, agentName)
		return p.store.UpsertAgentModels(ctx, store.AgentModelCatalog{
			Host:      hostName,
			Agent:     agentName,
			Models:    items,
			Source:    SourceConfig,
			CheckedAt: now,
		})

	case "agy":
		if p.provider == nil {
			errMsg := "host file access provider unavailable"
			p.logger.Printf("agentmodels: agy probe failed for %s/%s: %s", hostName, agentName, errMsg)
			return p.fallbackOrRecordError(ctx, hostName, agentName, errMsg, now)
		}
		if faErr != nil {
			p.logger.Printf("agentmodels: agy probe failed for %s/%s: %v", hostName, agentName, faErr)
			return p.fallbackOrRecordError(ctx, hostName, agentName, faErr.Error(), now)
		}
		items, err := p.probeAgy(ctx, fa, hostName, hostCfg, agentName)
		if err != nil {
			p.logger.Printf("agentmodels: agy probe failed for %s/%s: %v", hostName, agentName, err)
			return p.fallbackOrRecordError(ctx, hostName, agentName, err.Error(), now)
		}
		return p.store.UpsertAgentModels(ctx, store.AgentModelCatalog{
			Host:      hostName,
			Agent:     agentName,
			Models:    items,
			Source:    SourceAgyCLI,
			CheckedAt: now,
		})

	case "codex":
		if p.provider == nil {
			return p.fallbackOrRecordError(ctx, hostName, agentName, "host file access provider unavailable", now)
		}
		if faErr != nil {
			return p.fallbackOrRecordError(ctx, hostName, agentName, faErr.Error(), now)
		}
		items, err := p.probeCodex(ctx, fa, hostCfg)
		if err != nil {
			return p.fallbackOrRecordError(ctx, hostName, agentName, err.Error(), now)
		}
		return p.store.UpsertAgentModels(ctx, store.AgentModelCatalog{
			Host:      hostName,
			Agent:     agentName,
			Models:    items,
			Source:    SourceCodexCache,
			CheckedAt: now,
		})

	default:
		items := CatalogFromConfig(p.cfg, agentName)
		if len(items) == 0 {
			return nil
		}
		return p.store.UpsertAgentModels(ctx, store.AgentModelCatalog{
			Host:      hostName,
			Agent:     agentName,
			Models:    items,
			Source:    SourceConfig,
			CheckedAt: now,
		})
	}
}

// fallbackOrRecordError handles a probe failure:
//   - If a non-empty catalog already exists in DB for (host, agent), record the error without
//     overwriting the stored catalog (docs/11 §3: プローブ失敗時は前回のカタログを維持).
//   - Otherwise, if a static config catalog exists in fleet.yaml for this agent, store it as
//     a fallback (docs/11 §3: 読めなければ設定のカタログにフォールバック).
//   - Otherwise, record the error in DB.
func (p *Prober) fallbackOrRecordError(ctx context.Context, hostName, agentName, errMsg string, now time.Time) error {
	prev, err := p.store.GetAgentModels(ctx, hostName, agentName)
	if err == nil && prev != nil && len(prev.Models) > 0 {
		return p.store.RecordAgentModelsError(ctx, hostName, agentName, truncateString(errMsg, maxErrorDetailBytes), now)
	}
	if cfgItems := CatalogFromConfig(p.cfg, agentName); len(cfgItems) > 0 {
		return p.store.UpsertAgentModels(ctx, store.AgentModelCatalog{
			Host:      hostName,
			Agent:     agentName,
			Models:    cfgItems,
			Source:    SourceConfig,
			CheckedAt: now,
		})
	}
	return p.store.RecordAgentModelsError(ctx, hostName, agentName, truncateString(errMsg, maxErrorDetailBytes), now)
}

func (p *Prober) probeAgy(ctx context.Context, fa adapter.FileAccess, hostName string, hostCfg config.HostConfig, agentName string) ([]store.AgentModelItem, error) {
	driver, err := adapter.ResolveDriver(p.cfg, hostCfg, agentName)
	if err != nil {
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
	}
	isWin := strings.EqualFold(strings.TrimSpace(hostCfg.OS), "windows")

	// Primary path: structured JSON output (`agy --output-format json models`),
	// non-TTY with stdout and stderr strictly separated.
	items, jsonErr := p.probeAgyJSON(ctx, fa, driver, isWin)
	if jsonErr == nil {
		return items, nil
	}
	if ctx.Err() != nil {
		return nil, jsonErr
	}

	p.logger.Printf("agentmodels: agy json probe failed for %s/%s (%v); falling back to TSV", hostName, agentName, jsonErr)

	// Fallback path: legacy `agy models`, non-TTY with stdout and stderr strictly separated,
	// parsing stdout as 2-column TSV (<id>\t<label>).
	items, tsvErr := p.probeAgyTSV(ctx, fa, driver, isWin)
	if tsvErr == nil {
		return items, nil
	}
	return nil, fmt.Errorf("agy json probe failed (%v); tsv fallback failed (%v)", jsonErr, tsvErr)
}

func (p *Prober) probeAgyJSON(ctx context.Context, fa adapter.FileAccess, driver adapter.AgentDriver, isWin bool) ([]store.AgentModelItem, error) {
	cmd, _, err := driver.BuildVersionCommand([]string{"--output-format", "json", "models"}, isWin)
	if err != nil {
		return nil, fmt.Errorf("build agy json models command: %w", err)
	}

	probeCtx, cancel := context.WithTimeout(ctx, p.probeTimeout)
	defer cancel()

	res, runErr := fa.RunCommand(probeCtx, cmd)
	if runErr != nil {
		return nil, fmt.Errorf("run agy --output-format json models: %w", runErr)
	}
	if res.ExitCode != 0 {
		return nil, formatAgyExitError("agy --output-format json models", res)
	}
	return ParseAgyModelsJSON([]byte(res.Stdout))
}

func (p *Prober) probeAgyTSV(ctx context.Context, fa adapter.FileAccess, driver adapter.AgentDriver, isWin bool) ([]store.AgentModelItem, error) {
	cmd, _, err := driver.BuildVersionCommand([]string{"models"}, isWin)
	if err != nil {
		return nil, fmt.Errorf("build agy models command: %w", err)
	}

	probeCtx, cancel := context.WithTimeout(ctx, p.probeTimeout)
	defer cancel()

	res, runErr := fa.RunCommand(probeCtx, cmd)
	if runErr != nil {
		return nil, fmt.Errorf("run agy models: %w", runErr)
	}
	if res.ExitCode != 0 {
		return nil, formatAgyExitError("agy models", res)
	}
	return ParseAgyModelsTSV(res.Stdout)
}

func formatAgyExitError(label string, res adapter.ExecResult) error {
	detail := strings.TrimSpace(res.Stderr)
	if detail == "" {
		detail = strings.TrimSpace(res.Stdout)
	}
	if detail != "" {
		return fmt.Errorf("%s exited with code %d: %s", label, res.ExitCode, truncateString(detail, 200))
	}
	return fmt.Errorf("%s exited with code %d", label, res.ExitCode)
}

func (p *Prober) probeCodex(ctx context.Context, fa adapter.FileAccess, hostCfg config.HostConfig) ([]store.AgentModelItem, error) {
	probeCtx, cancel := context.WithTimeout(ctx, p.probeTimeout)
	defer cancel()

	isWin := strings.EqualFold(strings.TrimSpace(hostCfg.OS), "windows")
	var cmd string
	if isWin {
		cmd = `$p = Join-Path $env:USERPROFILE '.codex\models_cache.json'; ` +
			`if (-not (Test-Path -LiteralPath $p)) { exit 44 }; ` +
			`[Console]::Write([System.IO.File]::ReadAllText($p, [System.Text.Encoding]::UTF8))`
	} else {
		cmd = `p="$HOME/.codex/models_cache.json"; if [ ! -f "$p" ]; then exit 44; fi; cat -- "$p"`
	}

	res, runErr := fa.RunCommand(probeCtx, cmd)
	if runErr != nil {
		return nil, fmt.Errorf("read codex models_cache.json: %w", runErr)
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("read codex models_cache.json exited with code %d", res.ExitCode)
	}
	return ParseCodexModelsCache([]byte(res.Output))
}

func sortedHostNames(cfg *config.Config) []string {
	if cfg == nil || len(cfg.Hosts) == 0 {
		return nil
	}
	out := make([]string, 0, len(cfg.Hosts))
	for h := range cfg.Hosts {
		if name := strings.TrimSpace(h); name != "" {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

func sortedAgentNames(m map[string]config.AgentConfig) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for a := range m {
		if name := strings.TrimSpace(a); name != "" {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

func agentsForHost(hostCfg config.HostConfig, fallbackAgents []string) []string {
	if len(hostCfg.Agents) == 0 {
		return fallbackAgents
	}
	out := make([]string, 0, len(hostCfg.Agents))
	for a := range hostCfg.Agents {
		if name := strings.TrimSpace(a); name != "" {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

func truncateString(s string, maxLen int) string {
	s = strings.TrimSpace(s)
	if maxLen <= 0 || len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

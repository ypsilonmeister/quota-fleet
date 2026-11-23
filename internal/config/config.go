package config

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	// DefaultAPIListen is the default listen address for quota-fleet serve.
	DefaultAPIListen = "127.0.0.1:8737"
	// DefaultRuntimeAdapter is the default adapter ("mock").
	DefaultRuntimeAdapter = "mock"
	// RuntimeAdapterSSH selects the real SSH/local host adapter.
	RuntimeAdapterSSH        = "ssh"
	DefaultDispatchInterval  = 1 * time.Second
	DefaultShepherdInterval  = 5 * time.Second
	DefaultReconcileInterval = 30 * time.Second
	DefaultStaleClaimedAfter = 15 * time.Minute
	DefaultSignalMaxBytes         = int64(1 << 20) // 1 MiB
	DefaultQuotaStaleAfter        = 45 * time.Minute
	DefaultQuotaGateCheckInterval = 1 * time.Minute

	// GateModeOff disables Quota Gate checks on job registration.
	GateModeOff = "off"
	// GateModeDryRun evaluates Quota Gate on job registration and records decisions without holding jobs.
	GateModeDryRun = "dry-run"
	// GateModeEnforce evaluates Quota Gate, holds jobs when pools are exhausted/stale, and releases them when recovered.
	GateModeEnforce = "enforce"

	// PromptStyleStdin passes prompt.txt to the agent via stdin redirection (`< prompt.txt`).
	PromptStyleStdin = "stdin"
	// PromptStyleArgument passes prompt.txt contents as a command-line argument (`"$(cat prompt.txt)"`).
	PromptStyleArgument = "argument"

	// EvaluatorModeTypedDecision runs deterministic typed checks plus Gemini Decision 1 for free-form criteria.
	EvaluatorModeTypedDecision = "typed+decision"
	// EvaluatorModeAlwaysPass marks all acceptance criteria as PASS without running checks or calling Decision 1.
	EvaluatorModeAlwaysPass = "always-pass"
	// DefaultEvaluatorMode is the default evaluator mode ("typed+decision").
	DefaultEvaluatorMode = EvaluatorModeTypedDecision
	// DefaultEvaluatorAPIKeyEnv is the default environment variable holding the Gemini API key.
	DefaultEvaluatorAPIKeyEnv = "GEMINI_API_KEY"
	// DefaultEvaluatorDecisionThreshold is the default minimum noul probability required to pass.
	DefaultEvaluatorDecisionThreshold = 0.5
	// DefaultEvaluatorMaxEvalRetries is the default number of consecutive evaluation errors before failing a job.
	DefaultEvaluatorMaxEvalRetries = 3
)

// Config represents the quota-fleet configuration loaded from fleet.yaml.
type Config struct {
	InstanceID string                 `yaml:"instance_id"`
	StateDir   string                 `yaml:"state_dir"`
	API        APIConfig              `yaml:"api"`
	Runtime    RuntimeConfig          `yaml:"runtime"`
	Limits     LimitsConfig           `yaml:"limits"`
	Hosts      map[string]HostConfig  `yaml:"hosts"`
	Agents     map[string]AgentConfig `yaml:"agents"`
	Pools      map[string]PoolConfig  `yaml:"pools"`
	Quota      QuotaConfig            `yaml:"quota"`
	Evaluator  EvaluatorConfig        `yaml:"evaluator"`
}

// EvaluatorConfig holds acceptance criteria evaluator settings.
type EvaluatorConfig struct {
	Mode              string   `yaml:"mode"`
	APIKeyEnv         string   `yaml:"api_key_env"`
	DecisionThreshold *float64 `yaml:"decision_threshold"`
	MaxEvalRetries    *int     `yaml:"max_eval_retries"`
}

// EffectiveMode returns the configured evaluator mode, defaulting to "typed+decision".
func (e EvaluatorConfig) EffectiveMode() string {
	v := strings.TrimSpace(e.Mode)
	if v == "" {
		return DefaultEvaluatorMode
	}
	return v
}

// EffectiveAPIKeyEnv returns the configured environment variable name for the API key,
// defaulting to "GEMINI_API_KEY".
func (e EvaluatorConfig) EffectiveAPIKeyEnv() string {
	v := strings.TrimSpace(e.APIKeyEnv)
	if v == "" {
		return DefaultEvaluatorAPIKeyEnv
	}
	return v
}

// EffectiveDecisionThreshold returns the configured Decision 1 noul probability threshold,
// defaulting to 0.5.
func (e EvaluatorConfig) EffectiveDecisionThreshold() float64 {
	if e.DecisionThreshold == nil {
		return DefaultEvaluatorDecisionThreshold
	}
	return *e.DecisionThreshold
}

// EffectiveMaxEvalRetries returns the configured consecutive evaluation error limit,
// defaulting to 3.
func (e EvaluatorConfig) EffectiveMaxEvalRetries() int {
	if e.MaxEvalRetries == nil || *e.MaxEvalRetries <= 0 {
		return DefaultEvaluatorMaxEvalRetries
	}
	return *e.MaxEvalRetries
}

// QuotaConfig holds Quota Gate and usage snapshot settings.
type QuotaConfig struct {
	UsageFile         string `yaml:"usage_file"`
	StaleAfter        string `yaml:"stale_after"`
	GateMode          string `yaml:"gate_mode"`
	GateCheckInterval string `yaml:"gate_check_interval"`
}

// EffectiveStaleAfter returns the configured staleness threshold or DefaultQuotaStaleAfter (45m).
func (q QuotaConfig) EffectiveStaleAfter() time.Duration {
	return parseDurationOrDefault(q.StaleAfter, DefaultQuotaStaleAfter)
}

// EffectiveGateCheckInterval returns the configured periodic Gate check interval or DefaultQuotaGateCheckInterval (1m).
func (q QuotaConfig) EffectiveGateCheckInterval() time.Duration {
	return parseDurationOrDefault(q.GateCheckInterval, DefaultQuotaGateCheckInterval)
}

// EffectiveGateMode returns the configured gate mode, defaulting to "off".
func (q QuotaConfig) EffectiveGateMode() string {
	v := strings.TrimSpace(q.GateMode)
	if v == "" {
		return GateModeOff
	}
	return v
}

// RuntimeConfig holds settings for resident runtime components (Dispatcher,
// Launcher, Shepherd, Reconciler).
type RuntimeConfig struct {
	// Adapter selects the host/agent adapter ("mock" or "ssh").
	Adapter           string `yaml:"adapter"`
	DispatchInterval  string `yaml:"dispatch_interval"`
	ShepherdInterval  string `yaml:"shepherd_interval"`
	ReconcileInterval string `yaml:"reconcile_interval"`
	StaleClaimedAfter string `yaml:"stale_claimed_after"`
	SignalMaxBytes    int64  `yaml:"signal_max_bytes"`
	DeliveriesDir     string `yaml:"deliveries_dir"`
}

// EffectiveAdapter returns the configured adapter name, defaulting to "mock".
func (r RuntimeConfig) EffectiveAdapter() string {
	v := strings.TrimSpace(r.Adapter)
	if v == "" {
		return DefaultRuntimeAdapter
	}
	return v
}

// EffectiveDispatchInterval returns the Dispatcher poll interval or DefaultDispatchInterval.
func (r RuntimeConfig) EffectiveDispatchInterval() time.Duration {
	return parseDurationOrDefault(r.DispatchInterval, DefaultDispatchInterval)
}

// EffectiveShepherdInterval returns the Shepherd poll interval or DefaultShepherdInterval.
func (r RuntimeConfig) EffectiveShepherdInterval() time.Duration {
	return parseDurationOrDefault(r.ShepherdInterval, DefaultShepherdInterval)
}

// EffectiveReconcileInterval returns the Reconciler poll interval or DefaultReconcileInterval.
func (r RuntimeConfig) EffectiveReconcileInterval() time.Duration {
	return parseDurationOrDefault(r.ReconcileInterval, DefaultReconcileInterval)
}

// EffectiveStaleClaimedAfter returns the Reconciler stale claimed threshold or DefaultStaleClaimedAfter.
func (r RuntimeConfig) EffectiveStaleClaimedAfter() time.Duration {
	return parseDurationOrDefault(r.StaleClaimedAfter, DefaultStaleClaimedAfter)
}

// EffectiveSignalMaxBytes returns the signal.jsonl rotation threshold or DefaultSignalMaxBytes.
func (r RuntimeConfig) EffectiveSignalMaxBytes() int64 {
	if r.SignalMaxBytes <= 0 {
		return DefaultSignalMaxBytes
	}
	return r.SignalMaxBytes
}

// EffectiveDeliveriesDir returns the directory for file-based delivery outputs,
// resolved relative to stateDir when DeliveriesDir is empty or relative.
func (r RuntimeConfig) EffectiveDeliveriesDir(stateDir string) string {
	dir := strings.TrimSpace(r.DeliveriesDir)
	sd := strings.TrimSpace(stateDir)
	if dir == "" {
		if sd == "" {
			return "deliveries"
		}
		return filepath.Join(sd, "deliveries")
	}
	if filepath.IsAbs(dir) || sd == "" {
		return dir
	}
	return filepath.Join(sd, dir)
}

func parseDurationOrDefault(raw string, def time.Duration) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return def
	}
	return d
}

// APIConfig holds HTTP API server settings.
type APIConfig struct {
	Listen      string `yaml:"listen"`
	Token       string `yaml:"token"`
	RequireAuth bool   `yaml:"require_auth"`
}

// ListenAddr returns the configured listen address or DefaultAPIListen if empty.
func (a APIConfig) ListenAddr() string {
	if strings.TrimSpace(a.Listen) == "" {
		return DefaultAPIListen
	}
	return strings.TrimSpace(a.Listen)
}

// LimitsConfig holds concurrency and retry limits.
type LimitsConfig struct {
	MaxClaimed  int `yaml:"max_claimed"`
	AttemptsCap int `yaml:"attempts_cap"`
}

// HostConfig defines a worker host and optional per-host agent overrides.
type HostConfig struct {
	Transport             string                       `yaml:"transport"`
	SSH                   string                       `yaml:"ssh"`
	OS                    string                       `yaml:"os"`
	ProxyCommand          *string                      `yaml:"proxy_command"`
	KeyFile               string                       `yaml:"key_file"`
	KnownHostsFile        string                       `yaml:"known_hosts_file"`
	StrictHostKeyChecking string                       `yaml:"strict_host_key_checking"`
	WorkdirRoot           string                       `yaml:"workdir_root"`
	OwnerFixup            bool                         `yaml:"owner_fixup"`
	DenyPaths             []string                     `yaml:"deny_paths"`
	Agents                map[string]HostAgentOverride `yaml:"agents"`
}

// HostAgentOverride overrides agent execution settings on a specific host.
type HostAgentOverride struct {
	Command string            `yaml:"command"`
	Env     map[string]string `yaml:"env"`
}

// AgentConfig defines an agent driver configuration.
type AgentConfig struct {
	Preflight      string `yaml:"preflight"`
	Launch         string `yaml:"launch"`
	Resume         string `yaml:"resume"`
	SessionLogGlob string `yaml:"session_log_glob"`
	SandboxFlag    string `yaml:"sandbox_flag"`
	PromptStyle    string `yaml:"prompt_style"`
}

// PoolConfig defines a quota pool.
type PoolConfig struct {
	Provider   string       `yaml:"provider"`
	Unmeasured bool         `yaml:"unmeasured"`
	AppliesTo  []PoolTarget `yaml:"applies_to"`
}

// PoolTarget maps an agent and model family to a quota pool.
type PoolTarget struct {
	Agent       string `yaml:"agent"`
	ModelFamily string `yaml:"model_family"`
}

// Load reads, parses, and validates a fleet.yaml file from path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}
	cfg, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("parse config %q: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate config %q: %w", path, err)
	}
	return cfg, nil
}

// Parse unmarshals YAML bytes into Config. Unknown keys are ignored leniently.
func Parse(data []byte) (*Config, error) {
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Validate checks required configuration fields and basic invariants.
func (c *Config) Validate() error {
	if c == nil {
		return errors.New("config is nil")
	}

	var errs []error

	if strings.TrimSpace(c.InstanceID) == "" {
		errs = append(errs, errors.New("instance_id must not be empty"))
	}
	if c.Limits.MaxClaimed <= 0 {
		errs = append(errs, fmt.Errorf("limits.max_claimed must be > 0 (got %d)", c.Limits.MaxClaimed))
	}
	if c.Limits.AttemptsCap <= 0 {
		errs = append(errs, fmt.Errorf("limits.attempts_cap must be > 0 (got %d)", c.Limits.AttemptsCap))
	}

	for name, h := range c.Hosts {
		if h.Transport != "" && h.Transport != "ssh" && h.Transport != "local" {
			errs = append(errs, fmt.Errorf("hosts.%s.transport must be \"ssh\" or \"local\" (got %q)", name, h.Transport))
		}
		if h.OS != "" && h.OS != "linux" && h.OS != "windows" {
			errs = append(errs, fmt.Errorf("hosts.%s.os must be \"linux\" or \"windows\" (got %q)", name, h.OS))
		}
		if sk := strings.TrimSpace(h.StrictHostKeyChecking); sk != "" && sk != "yes" && sk != "accept-new" && sk != "no" {
			errs = append(errs, fmt.Errorf("hosts.%s.strict_host_key_checking must be \"yes\", \"accept-new\", or \"no\" (got %q)", name, h.StrictHostKeyChecking))
		}
	}

	for name, a := range c.Agents {
		if ps := strings.TrimSpace(a.PromptStyle); ps != "" && ps != PromptStyleStdin && ps != PromptStyleArgument {
			errs = append(errs, fmt.Errorf("agents.%s.prompt_style must be %q or %q (got %q)", name, PromptStyleStdin, PromptStyleArgument, a.PromptStyle))
		}
	}

	if adapter := strings.TrimSpace(c.Runtime.Adapter); adapter != "" && adapter != DefaultRuntimeAdapter && adapter != RuntimeAdapterSSH {
		errs = append(errs, fmt.Errorf("runtime.adapter %q is not supported (allowed: %q, %q)", adapter, DefaultRuntimeAdapter, RuntimeAdapterSSH))
	}
	if err := validatePositiveDuration("runtime.dispatch_interval", c.Runtime.DispatchInterval); err != nil {
		errs = append(errs, err)
	}
	if err := validatePositiveDuration("runtime.shepherd_interval", c.Runtime.ShepherdInterval); err != nil {
		errs = append(errs, err)
	}
	if err := validatePositiveDuration("runtime.reconcile_interval", c.Runtime.ReconcileInterval); err != nil {
		errs = append(errs, err)
	}
	if err := validatePositiveDuration("runtime.stale_claimed_after", c.Runtime.StaleClaimedAfter); err != nil {
		errs = append(errs, err)
	}
	if c.Runtime.SignalMaxBytes < 0 {
		errs = append(errs, fmt.Errorf("runtime.signal_max_bytes must be >= 0 (got %d)", c.Runtime.SignalMaxBytes))
	}

	if err := validatePositiveDuration("quota.stale_after", c.Quota.StaleAfter); err != nil {
		errs = append(errs, err)
	}
	if err := validatePositiveDuration("quota.gate_check_interval", c.Quota.GateCheckInterval); err != nil {
		errs = append(errs, err)
	}
	if mode := strings.TrimSpace(c.Quota.GateMode); mode != "" && mode != GateModeOff && mode != GateModeDryRun && mode != GateModeEnforce {
		errs = append(errs, fmt.Errorf("quota.gate_mode %q is not supported (allowed: %q, %q, %q)", mode, GateModeOff, GateModeDryRun, GateModeEnforce))
	}

	if mode := strings.TrimSpace(c.Evaluator.Mode); mode != "" && mode != EvaluatorModeTypedDecision && mode != EvaluatorModeAlwaysPass {
		errs = append(errs, fmt.Errorf("evaluator.mode %q is not supported (allowed: %q, %q)", mode, EvaluatorModeTypedDecision, EvaluatorModeAlwaysPass))
	}
	if c.Evaluator.DecisionThreshold != nil {
		th := *c.Evaluator.DecisionThreshold
		if math.IsNaN(th) || math.IsInf(th, 0) || th < 0 || th > 1 {
			errs = append(errs, fmt.Errorf("evaluator.decision_threshold must be between 0 and 1 (got %v)", th))
		}
	}
	if c.Evaluator.MaxEvalRetries != nil && *c.Evaluator.MaxEvalRetries <= 0 {
		errs = append(errs, fmt.Errorf("evaluator.max_eval_retries must be > 0 (got %d)", *c.Evaluator.MaxEvalRetries))
	}

	return errors.Join(errs...)
}

func validatePositiveDuration(field, raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return fmt.Errorf("%s is not a valid duration (%q): %w", field, raw, err)
	}
	if d <= 0 {
		return fmt.Errorf("%s must be > 0 (got %q)", field, raw)
	}
	return nil
}

// DBPath returns the SQLite database file path derived from StateDir.
func (c *Config) DBPath() string {
	if c == nil || strings.TrimSpace(c.StateDir) == "" {
		return "fleet.db"
	}
	return filepath.Join(c.StateDir, "fleet.db")
}

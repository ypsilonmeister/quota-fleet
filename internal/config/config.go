package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// DefaultAPIListen is the default listen address for quota-fleet serve.
const DefaultAPIListen = "127.0.0.1:8737"

// Config represents the quota-fleet configuration loaded from fleet.yaml.
type Config struct {
	InstanceID string                 `yaml:"instance_id"`
	StateDir   string                 `yaml:"state_dir"`
	API        APIConfig              `yaml:"api"`
	Limits     LimitsConfig           `yaml:"limits"`
	Hosts      map[string]HostConfig  `yaml:"hosts"`
	Agents     map[string]AgentConfig `yaml:"agents"`
	Pools      map[string]PoolConfig  `yaml:"pools"`
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
	Transport    string                       `yaml:"transport"`
	SSH          string                       `yaml:"ssh"`
	OS           string                       `yaml:"os"`
	ProxyCommand *string                      `yaml:"proxy_command"`
	KeyFile      string                       `yaml:"key_file"`
	WorkdirRoot  string                       `yaml:"workdir_root"`
	OwnerFixup   bool                         `yaml:"owner_fixup"`
	DenyPaths    []string                     `yaml:"deny_paths"`
	Agents       map[string]HostAgentOverride `yaml:"agents"`
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
	}

	return errors.Join(errs...)
}

// DBPath returns the SQLite database file path derived from StateDir.
func (c *Config) DBPath() string {
	if c == nil || strings.TrimSpace(c.StateDir) == "" {
		return "fleet.db"
	}
	return filepath.Join(c.StateDir, "fleet.db")
}

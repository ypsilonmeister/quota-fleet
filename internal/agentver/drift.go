package agentver

import (
	"sort"
	"strings"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

// LimitationNote documents the inherent boundary of relative drift detection (docs/08 §3).
const LimitationNote = "遅れ判定はホスト間の最大版との相対比較です。全ホストが一斉に古い場合は検出できません。"

// Cell represents one (host, agent) cell in the agent version matrix.
type Cell struct {
	Host         string                   `json:"host"`
	Agent        string                   `json:"agent"`
	Status       store.AgentVersionStatus `json:"status"`
	VersionRaw   string                   `json:"version_raw,omitempty"`
	Version      string                   `json:"version,omitempty"`
	ErrorDetail  string                   `json:"error_detail,omitempty"`
	CheckedAt    *time.Time               `json:"checked_at,omitempty"`
	Pin          string                   `json:"pin,omitempty"`
	Pinned       bool                     `json:"pinned"`
	PinMismatch  bool                     `json:"pin_mismatch,omitempty"`
	ManualUpdate bool                     `json:"manual_update,omitempty"`
	Behind       bool                     `json:"behind"`
	Comparable   bool                     `json:"comparable"`
}

// AgentSummary summarizes version consensus and drift status for a single agent across hosts.
type AgentSummary struct {
	Agent             string   `json:"agent"`
	Drifted           bool     `json:"drifted"`
	Comparable        bool     `json:"comparable"`
	MaxVersion        string   `json:"max_version,omitempty"`
	Versions          []string `json:"versions"`
	LaggingHosts      []string `json:"lagging_hosts"`
	PinnedHosts       []string `json:"pinned_hosts"`
	ManualUpdateHosts []string `json:"manual_update_hosts"`
	FailedHosts       []string `json:"failed_hosts"`
	LimitationNote    string   `json:"limitation_note"`
}

// Matrix is the canonical host × agent version view shared by CLI, API, and UI.
type Matrix struct {
	Hosts          []string       `json:"hosts"`
	Agents         []string       `json:"agents"`
	Entries        []Cell         `json:"entries"`
	Summaries      []AgentSummary `json:"summaries"`
	HasDrift       bool           `json:"has_drift"`
	LimitationNote string         `json:"limitation_note"`
}

// TargetHostsAndAgents returns the sorted union of hosts and agents from cfg and stored rows.
func TargetHostsAndAgents(cfg *config.Config, rows []*store.AgentVersion) ([]string, []string) {
	hostSet := make(map[string]struct{})
	agentSet := make(map[string]struct{})

	if cfg != nil {
		for h, hc := range cfg.Hosts {
			if name := strings.TrimSpace(h); name != "" {
				hostSet[name] = struct{}{}
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
	}
	for _, r := range rows {
		if r == nil {
			continue
		}
		if h := strings.TrimSpace(r.Host); h != "" {
			hostSet[h] = struct{}{}
		}
		if a := strings.TrimSpace(r.Agent); a != "" {
			agentSet[a] = struct{}{}
		}
	}

	hosts := make([]string, 0, len(hostSet))
	for h := range hostSet {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)

	agents := make([]string, 0, len(agentSet))
	for a := range agentSet {
		agents = append(agents, a)
	}
	sort.Strings(agents)

	return hosts, agents
}

// BuildMatrix evaluates drift, lag, pin exclusions, and manual_update flags across all hosts and agents.
func BuildMatrix(cfg *config.Config, rows []*store.AgentVersion) Matrix {
	hosts, agents := TargetHostsAndAgents(cfg, rows)

	type key struct {
		host  string
		agent string
	}
	rowByKey := make(map[key]*store.AgentVersion, len(rows))
	for _, r := range rows {
		if r == nil {
			continue
		}
		rowByKey[key{host: strings.TrimSpace(r.Host), agent: strings.TrimSpace(r.Agent)}] = r
	}

	// Build initial cells for every (host, agent).
	cells := make([]Cell, 0, len(hosts)*len(agents))
	cellIdx := make(map[key]int, len(hosts)*len(agents))

	for _, h := range hosts {
		for _, a := range agents {
			pin, manual := resolvePolicy(cfg, h, a)
			cell := Cell{
				Host:         h,
				Agent:        a,
				Pin:          pin,
				Pinned:       pin != "",
				ManualUpdate: manual,
			}
			if r, ok := rowByKey[key{host: h, agent: a}]; ok {
				cell.Status = r.Status
				cell.VersionRaw = r.VersionRaw
				cell.Version = r.Version
				cell.ErrorDetail = r.ErrorDetail
				t := r.CheckedAt
				cell.CheckedAt = &t
				if cell.Pinned && cell.Status == store.AgentVersionStatusOK {
					normPin := NormalizeVersion(cell.Pin)
					if normPin == "" {
						normPin = cell.Pin
					}
					if cell.Version != normPin && cell.Version != cell.Pin {
						cell.PinMismatch = true
					}
				}
			}
			cellIdx[key{host: h, agent: a}] = len(cells)
			cells = append(cells, cell)
		}
	}

	summaries := make([]AgentSummary, 0, len(agents))
	hasDrift := false

	for _, a := range agents {
		summary := AgentSummary{
			Agent:             a,
			Comparable:        true,
			Versions:          []string{},
			LaggingHosts:      []string{},
			PinnedHosts:       []string{},
			ManualUpdateHosts: []string{},
			FailedHosts:       []string{},
			LimitationNote:    LimitationNote,
		}

		allOKVersions := make(map[string]struct{})
		allOKCells := make([]int, 0, len(hosts))

		for _, h := range hosts {
			idx := cellIdx[key{host: h, agent: a}]
			c := &cells[idx]
			if c.Pinned {
				summary.PinnedHosts = append(summary.PinnedHosts, h)
			}
			switch c.Status {
			case store.AgentVersionStatusOK:
				if c.Version != "" {
					allOKVersions[c.Version] = struct{}{}
					allOKCells = append(allOKCells, idx)
				}
			case store.AgentVersionStatusNotFound, store.AgentVersionStatusError:
				summary.FailedHosts = append(summary.FailedHosts, h)
			}
		}

		// Collect sorted distinct ok versions across all hosts (including pinned).
		for v := range allOKVersions {
			summary.Versions = append(summary.Versions, v)
		}
		sort.Strings(summary.Versions)

		// Drift rule (docs/08 §3):
		// Drift exists when 2+ distinct normalized ok versions exist across hosts.
		// Pinned hosts still participate in drift detection and max version calculation,
		// and are excluded ONLY from lag ("遅れ") classification/notifications.
		if len(allOKVersions) >= 2 {
			summary.Drifted = true
			hasDrift = true
		}

		// Determine semver comparability and max version across all ok hosts.
		if len(allOKVersions) > 0 {
			allSemver := true
			var (
				maxSV  Semver
				maxStr string
				hasMax bool
			)
			for v := range allOKVersions {
				sv, ok := ParseSemver(v)
				if !ok {
					allSemver = false
					break
				}
				if !hasMax || CompareSemver(sv, maxSV) > 0 {
					maxSV = sv
					maxStr = v
					hasMax = true
				}
			}
			summary.Comparable = allSemver
			if allSemver && hasMax {
				summary.MaxVersion = maxStr
			}

			// Mark Comparable on every ok cell, and Behind only on unpinned ok cells below max.
			for _, idx := range allOKCells {
				c := &cells[idx]
				c.Comparable = allSemver
				if c.Pinned {
					c.Behind = false
					continue
				}
				if allSemver && hasMax {
					if sv, ok := ParseSemver(c.Version); ok && CompareSemver(sv, maxSV) < 0 {
						c.Behind = true
						summary.LaggingHosts = append(summary.LaggingHosts, c.Host)
						if c.ManualUpdate {
							summary.ManualUpdateHosts = append(summary.ManualUpdateHosts, c.Host)
						}
					}
				}
			}
		}

		sort.Strings(summary.LaggingHosts)
		sort.Strings(summary.PinnedHosts)
		sort.Strings(summary.ManualUpdateHosts)
		sort.Strings(summary.FailedHosts)

		summaries = append(summaries, summary)
	}

	return Matrix{
		Hosts:          hosts,
		Agents:         agents,
		Entries:        cells,
		Summaries:      summaries,
		HasDrift:       hasDrift,
		LimitationNote: LimitationNote,
	}
}

func resolvePolicy(cfg *config.Config, hostName, agentName string) (pin string, manualUpdate bool) {
	if cfg == nil {
		return "", false
	}
	if ac, ok := cfg.Agents[agentName]; ok {
		if ac.ManualUpdate {
			manualUpdate = true
		}
	}
	if hc, ok := cfg.Hosts[hostName]; ok && hc.Agents != nil {
		if ov, ok := hc.Agents[agentName]; ok {
			pin = strings.TrimSpace(ov.Pin)
			if ov.ManualUpdate {
				manualUpdate = true
			}
		}
	}
	return pin, manualUpdate
}

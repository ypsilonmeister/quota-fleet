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

		unpinnedVersions := make(map[string]struct{})
		allOKVersions := make(map[string]struct{})
		unpinnedOKCells := make([]int, 0, len(hosts))

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
					if !c.Pinned {
						unpinnedVersions[c.Version] = struct{}{}
						unpinnedOKCells = append(unpinnedOKCells, idx)
					}
				}
			case store.AgentVersionStatusNotFound, store.AgentVersionStatusError:
				summary.FailedHosts = append(summary.FailedHosts, h)
			}
		}

		// Collect sorted distinct unpinned ok versions.
		for v := range unpinnedVersions {
			summary.Versions = append(summary.Versions, v)
		}
		sort.Strings(summary.Versions)

		// Drift rule (docs/08 §3):
		// Drift exists when 2+ distinct normalized ok versions exist among unpinned hosts.
		// Pinned hosts are excluded from lag/drift notifications and clearly marked as pinned.
		if len(unpinnedVersions) >= 2 {
			summary.Drifted = true
			hasDrift = true
		}

		// Determine semver comparability and max version among unpinned ok hosts
		// (falling back to all ok hosts for display max_version if all ok hosts are pinned).
		versionPool := unpinnedVersions
		if len(versionPool) == 0 {
			versionPool = allOKVersions
		}

		if len(versionPool) > 0 {
			allSemver := true
			var (
				maxSV  Semver
				maxStr string
				hasMax bool
			)
			for v := range versionPool {
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

			// Mark Comparable and Behind on each unpinned ok cell.
			for _, idx := range unpinnedOKCells {
				c := &cells[idx]
				c.Comparable = allSemver
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
			// Mark Comparable on pinned ok cells too (for display info), but NEVER Behind.
			for _, h := range hosts {
				idx := cellIdx[key{host: h, agent: a}]
				c := &cells[idx]
				if c.Pinned && c.Status == store.AgentVersionStatusOK {
					_, ok := ParseSemver(c.Version)
					c.Comparable = ok
					c.Behind = false
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

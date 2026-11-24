package agentmodels

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/quota"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

var ansiEscapeRe = regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]`)

type agyModelsJSONEnvelope struct {
	Status  string                `json:"status"`
	Command *agyModelsJSONCommand `json:"command"`
}

type agyModelsJSONCommand struct {
	Name string             `json:"name"`
	Data *agyModelsJSONData `json:"data"`
}

type agyModelsJSONData struct {
	Models []agyModelsJSONEntry `json:"models"`
}

type agyModelsJSONEntry struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// ParseAgyModelsJSON parses the structured stdout of `agy --output-format json models`.
// It verifies status=="SUCCESS" and command.name=="models" before reading command.data.models.
func ParseAgyModelsJSON(data []byte) ([]store.AgentModelItem, error) {
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil, errors.New("agy models json stdout is empty")
	}

	var env agyModelsJSONEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("parse agy models json: %w", err)
	}
	if env.Status != "SUCCESS" {
		return nil, fmt.Errorf("agy models json status = %q, want %q", env.Status, "SUCCESS")
	}
	if env.Command == nil {
		return nil, errors.New("agy models json missing command object")
	}
	if env.Command.Name != "models" {
		return nil, fmt.Errorf("agy models json command.name = %q, want %q", env.Command.Name, "models")
	}
	if env.Command.Data == nil {
		return nil, errors.New("agy models json missing command.data object")
	}

	seen := make(map[string]struct{}, len(env.Command.Data.Models))
	items := make([]store.AgentModelItem, 0, len(env.Command.Data.Models))
	for _, m := range env.Command.Data.Models {
		id := strings.TrimSpace(m.ID)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		label := strings.TrimSpace(m.Label)
		if label == "" {
			label = id
		}
		items = append(items, store.AgentModelItem{
			ID:          id,
			DisplayName: label,
			Family:      quota.ModelFamily(id),
		})
	}

	if len(items) == 0 {
		return nil, errors.New("agy models json contained no valid models")
	}
	return items, nil
}

// ValidateAgyTSVStderr checks stderr from a legacy non-TTY `agy models` invocation.
// On success, `agy models` writes nothing or only its known progress line
// ("Fetching available models..."). Any other non-empty stderr segment is treated
// as an error so partial stdout from an interrupted fetch does not overwrite a
// previously stored catalog.
func ValidateAgyTSVStderr(stderr string) error {
	for _, rawLine := range strings.Split(stderr, "\n") {
		clean := ansiEscapeRe.ReplaceAllString(rawLine, "")
		for _, part := range strings.Split(clean, "\r") {
			seg := strings.TrimSpace(part)
			if seg == "" {
				continue
			}
			if isKnownAgyProgressLine(seg) {
				continue
			}
			return fmt.Errorf("agy models wrote unexpected stderr output: %q", seg)
		}
	}
	return nil
}

func isKnownAgyProgressLine(line string) bool {
	trimmed := line
	if r, size := utf8.DecodeRuneInString(trimmed); r >= 0x2800 && r <= 0x28FF {
		trimmed = strings.TrimSpace(trimmed[size:])
	}
	return strings.EqualFold(trimmed, "Fetching available models...")
}

// ParseAgyModelsTSV parses the non-TTY stdout of legacy `agy models`, where each non-empty line
// is a two-column tab-separated `<id>\t<label>` record.
func ParseAgyModelsTSV(stdout string) ([]store.AgentModelItem, error) {
	lines := strings.Split(stdout, "\n")
	seen := make(map[string]struct{})
	var items []store.AgentModelItem

	for _, rawLine := range lines {
		line := strings.TrimSuffix(rawLine, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		rawID, rawLabel, ok := strings.Cut(line, "\t")
		if !ok {
			return nil, fmt.Errorf("agy models TSV line missing tab separator: %q", line)
		}
		if strings.ContainsRune(rawLabel, '\t') {
			return nil, fmt.Errorf("agy models TSV line has more than two columns: %q", line)
		}
		id := strings.TrimSpace(rawID)
		label := strings.TrimSpace(rawLabel)
		if id == "" || label == "" {
			return nil, fmt.Errorf("agy models TSV line has empty id or label: %q", line)
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		items = append(items, store.AgentModelItem{
			ID:          id,
			DisplayName: label,
			Family:      quota.ModelFamily(id),
		})
	}

	if len(items) == 0 {
		return nil, errors.New("agy models TSV output did not contain any valid models")
	}
	return items, nil
}

type codexModelsCache struct {
	Models []codexCacheModel `json:"models"`
}

type codexCacheModel struct {
	Slug        string `json:"slug"`
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Visibility  string `json:"visibility"`
	Priority    int    `json:"priority"`
}

// ParseCodexModelsCache parses ~/.codex/models_cache.json into a model catalog.
// Hidden entries (visibility == "hide") are excluded; remaining entries are ordered
// by priority ascending (preserving input order for ties).
func ParseCodexModelsCache(data []byte) ([]store.AgentModelItem, error) {
	var cache codexModelsCache
	if err := json.Unmarshal(data, &cache); err != nil {
		return nil, fmt.Errorf("parse codex models_cache.json: %w", err)
	}

	type indexedModel struct {
		idx int
		m   codexCacheModel
	}
	var visible []indexedModel
	for i, m := range cache.Models {
		if strings.EqualFold(strings.TrimSpace(m.Visibility), "hide") {
			continue
		}
		id := strings.TrimSpace(m.Slug)
		if id == "" {
			id = strings.TrimSpace(m.ID)
		}
		if id == "" {
			continue
		}
		m.Slug = id
		visible = append(visible, indexedModel{idx: i, m: m})
	}

	sort.SliceStable(visible, func(i, j int) bool {
		pi, pj := visible[i].m.Priority, visible[j].m.Priority
		if pi != pj {
			// Treat priority <= 0 as unset (sort after positive priorities).
			if pi <= 0 {
				return false
			}
			if pj <= 0 {
				return true
			}
			return pi < pj
		}
		return visible[i].idx < visible[j].idx
	})

	seen := make(map[string]struct{}, len(visible))
	items := make([]store.AgentModelItem, 0, len(visible))
	for _, entry := range visible {
		id := entry.m.Slug
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		displayName := strings.TrimSpace(entry.m.DisplayName)
		if displayName == "" {
			displayName = id
		}
		items = append(items, store.AgentModelItem{
			ID:          id,
			DisplayName: displayName,
			Family:      quota.ModelFamily(id),
		})
	}

	if len(items) == 0 {
		return nil, errors.New("codex models_cache.json contained no visible models")
	}
	return items, nil
}

// CatalogFromConfig returns the static model catalog configured under agents.<agent>.models in fleet.yaml.
func CatalogFromConfig(cfg *config.Config, agentName string) []store.AgentModelItem {
	if cfg == nil {
		return nil
	}
	ac, ok := cfg.Agents[strings.TrimSpace(agentName)]
	if !ok || len(ac.Models) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(ac.Models))
	items := make([]store.AgentModelItem, 0, len(ac.Models))
	for _, m := range ac.Models {
		id := strings.TrimSpace(m.ID)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		dn := strings.TrimSpace(m.DisplayName)
		if dn == "" {
			dn = id
		}
		items = append(items, store.AgentModelItem{
			ID:          id,
			DisplayName: dn,
			Family:      quota.ModelFamily(id),
		})
	}
	return items
}

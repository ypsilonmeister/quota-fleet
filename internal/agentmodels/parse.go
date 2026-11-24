package agentmodels

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/quota"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

var (
	ansiEscapeRe = regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]`)
	modelIDRe    = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:/-]*$`)
)

// ParseAgyModelsOutput parses the stdout/combined output of `agy models` into a model catalog.
// It tolerates terminal spinner lines separated by '\r' and ANSI escape codes, and extracts
// `<id>   <display_name>` rows. Returns an error if no valid models are parsed.
func ParseAgyModelsOutput(output string) ([]store.AgentModelItem, error) {
	lines := strings.Split(output, "\n")
	seen := make(map[string]struct{})
	var items []store.AgentModelItem

	for _, rawLine := range lines {
		clean := ansiEscapeRe.ReplaceAllString(rawLine, "")
		// When a spinner overwrites the line with '\r' before printing a model entry,
		// keep the last non-empty carriage-return segment.
		seg := lastNonEmptyCRSegment(clean)
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		if isSpinnerOrHeaderLine(seg) {
			continue
		}

		id, displayName := splitIDAndDisplayName(seg)
		if id == "" || !modelIDRe.MatchString(id) {
			continue
		}
		// Require model IDs to look like actual slugs (avoid matching prose words like "Error:").
		if strings.HasSuffix(id, ":") {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
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
		return nil, errors.New("agy models output contained no parseable models")
	}
	return items, nil
}

func lastNonEmptyCRSegment(s string) string {
	if !strings.ContainsRune(s, '\r') {
		return s
	}
	parts := strings.Split(s, "\r")
	for i := len(parts) - 1; i >= 0; i-- {
		if strings.TrimSpace(parts[i]) != "" {
			return parts[i]
		}
	}
	return ""
}

func isSpinnerOrHeaderLine(line string) bool {
	lower := strings.ToLower(line)
	if strings.Contains(lower, "fetching available models") {
		return true
	}
	if strings.HasPrefix(line, "---") || strings.HasPrefix(line, "===") {
		return true
	}
	// Skip non-ASCII leading spinner glyphs (e.g. braille ⠋..⠴).
	if len(line) > 0 && line[0] >= 0x80 {
		return true
	}
	fields := strings.Fields(lower)
	if len(fields) >= 2 {
		if (fields[0] == "id" || fields[0] == "model" || fields[0] == "slug") &&
			(fields[1] == "name" || fields[1] == "display" || fields[1] == "display_name" || fields[1] == "description") {
			return true
		}
	}
	return false
}

func splitIDAndDisplayName(line string) (string, string) {
	idx := strings.IndexAny(line, " \t")
	if idx < 0 {
		return strings.TrimSpace(line), ""
	}
	id := strings.TrimSpace(line[:idx])
	rest := strings.TrimSpace(line[idx:])
	return id, rest
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

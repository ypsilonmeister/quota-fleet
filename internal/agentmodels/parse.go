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
	ansiEscapeRe     = regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]`)
	agyModelIDRe     = regexp.MustCompile(`^[a-z0-9][a-z0-9._:/-]*$`)
	agyColSepRe      = regexp.MustCompile(`(?:\t| {2,})`)
	agySlugMarkerRe  = regexp.MustCompile(`[-_./0-9]`)
)

// ParseAgyModelsOutput parses the stdout/combined output of `agy models` into a model catalog.
// It tolerates terminal spinner lines separated by '\r' and ANSI escape codes, and extracts
// `<id>   <display_name>` rows (two-column tabular output separated by 2+ spaces or a tab, or
// a bare lowercase slug ID on its own line). Diagnostic / prose lines such as "No models available"
// are rejected so an empty-list message never overwrites a previously stored catalog.
// Returns an error if no valid models are parsed.
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

		id, displayName, ok := parseAgyModelRow(seg)
		if !ok {
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
		if (fields[0] == "id" || fields[0] == "model" || fields[0] == "model-id" || fields[0] == "model_id" || fields[0] == "slug") &&
			(fields[1] == "name" || fields[1] == "display" || fields[1] == "display_name" || fields[1] == "description") {
			return true
		}
	}
	return false
}

// parseAgyModelRow extracts (id, displayName, true) from a single trimmed `agy models` line.
// Real `agy models` rows are tabular (`<slug-id>  +<display name>` or `<slug-id>\t<display name>`),
// or a single bare slug ID. Single-space prose sentences like "No models available" are rejected.
func parseAgyModelRow(line string) (string, string, bool) {
	if !strings.ContainsAny(line, " \t") {
		if isValidAgyModelID(line) {
			return line, "", true
		}
		return "", "", false
	}
	loc := agyColSepRe.FindStringIndex(line)
	if loc == nil {
		// Words separated only by single spaces (e.g. "No models available") are prose, not a table row.
		return "", "", false
	}
	id := strings.TrimSpace(line[:loc[0]])
	rest := strings.TrimSpace(line[loc[1]:])
	if !isValidAgyModelID(id) {
		return "", "", false
	}
	return id, rest, true
}

func isValidAgyModelID(id string) bool {
	if id == "" || strings.ContainsAny(id, " \t") {
		return false
	}
	if strings.HasSuffix(id, ":") || strings.HasSuffix(id, ".") {
		return false
	}
	if !agyModelIDRe.MatchString(id) {
		return false
	}
	// Require at least one slug separator or digit so plain English words ("no", "none", "empty")
	// are never mistaken for model IDs.
	return agySlugMarkerRe.MatchString(id)
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

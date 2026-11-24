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
	ansiEscapeRe    = regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]`)
	agyModelIDRe    = regexp.MustCompile(`^[a-z0-9][a-z0-9._:/-]*$`)
	agySpaceSepRe   = regexp.MustCompile(` {2,}`)
	agySlugMarkerRe = regexp.MustCompile(`[-_./0-9]`)

	agyDiagnosticPrefixes = []string{
		"error:",
		"err:",
		"warning:",
		"warn:",
		"fatal:",
		"panic:",
		"failed:",
		"usage:",
	}

	agyDiagnosticPhrases = []string{
		"authentication required",
		"not authenticated",
		"unauthenticated",
		"unauthorized",
		"forbidden",
		"permission denied",
		"access denied",
		"not logged in",
		"login required",
		"failed to ",
		"rate limit",
		"timed out",
		"timeout",
		"connection refused",
		"service unavailable",
		"temporarily unavailable",
		"no models",
		"not found",
		"invalid token",
		"token expired",
		"internal error",
		"unknown error",
		"bad request",
	}
)

type agySepKind int

const (
	agySepNone agySepKind = iota
	agySepTab
	agySepSpaces
)

// ValidateAgyStderr checks stderr from an `agy models` invocation.
// Real `agy models` writes only its progress message ("Fetching available models..." / spinner)
// to stderr on success. Any other non-empty stderr line is treated as a diagnostic/warning so
// the prober preserves the previously stored catalog even if the command exited with code 0.
func ValidateAgyStderr(stderr string) error {
	for _, rawLine := range strings.Split(stderr, "\n") {
		clean := ansiEscapeRe.ReplaceAllString(rawLine, "")
		seg := strings.TrimSpace(lastNonEmptyCRSegment(clean))
		if seg == "" {
			continue
		}
		if isSpinnerOrProgressLine(seg) {
			continue
		}
		return fmt.Errorf("agy models wrote unexpected stderr output: %q", seg)
	}
	return nil
}

// ParseAgyModelsOutput parses the stdout/combined output of `agy models` into a model catalog.
// Observed `agy models` output formats:
//   - Non-TTY (pipe / SSH): stderr "Fetching available models..." + stdout "<id>\t<display_name>"
//   - TTY (interactive): "\r⠋ Fetching available models...\r\x1b[K<id>  +<display_name>\r\n"
//
// List validity is intentionally decoupled from model family classification (docs/11 §3: unknown
// prefixes are imported with family=unknown) and does not impose an artificial minimum count so
// hosts exposing a single model are still imported:
//   - Every non-empty, non-spinner/header line must be a valid two-column model row (no silent skips).
//   - All rows must share the same column separator style (all tabs or all space-padded).
//   - Tab-separated rows preserve internal consecutive spaces in the display name.
//   - Rows with diagnostic IDs, diagnostic messages, or display names unrelated to the model ID are rejected.
func ParseAgyModelsOutput(output string) ([]store.AgentModelItem, error) {
	lines := strings.Split(output, "\n")
	seen := make(map[string]struct{})
	var items []store.AgentModelItem
	sepStyle := agySepNone

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

		id, displayName, sep, ok := parseAgyModelRow(seg)
		if !ok {
			return nil, fmt.Errorf("agy models output contained non-model line: %q", seg)
		}
		if sepStyle == agySepNone {
			sepStyle = sep
		} else if sepStyle != sep {
			return nil, fmt.Errorf("agy models output mixed column separator styles at line: %q", seg)
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		items = append(items, store.AgentModelItem{
			ID:          id,
			DisplayName: displayName,
			Family:      quota.ModelFamily(id),
		})
	}

	if len(items) == 0 {
		return nil, errors.New("agy models output did not contain any valid models")
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

func isSpinnerOrProgressLine(line string) bool {
	lower := strings.ToLower(line)
	if strings.Contains(lower, "fetching available models") {
		return true
	}
	// Skip non-ASCII leading spinner glyphs (e.g. braille ⠋..⠴).
	if len(line) > 0 && line[0] >= 0x80 {
		return true
	}
	return false
}

func isSpinnerOrHeaderLine(line string) bool {
	if isSpinnerOrProgressLine(line) {
		return true
	}
	if strings.HasPrefix(line, "---") || strings.HasPrefix(line, "===") {
		return true
	}
	lower := strings.ToLower(line)
	fields := strings.Fields(lower)
	if len(fields) >= 2 {
		if (fields[0] == "id" || fields[0] == "model" || fields[0] == "model-id" || fields[0] == "model_id" || fields[0] == "slug") &&
			(fields[1] == "name" || fields[1] == "display" || fields[1] == "display_name" || fields[1] == "description") {
			return true
		}
	}
	return false
}

// parseAgyModelRow extracts (id, displayName, sepKind, true) from a single trimmed `agy models` line.
// Real `agy models` rows are always two-column tabular rows:
//   - `<slug-id>\t<Display Name>` (non-TTY stdout; internal spaces in Display Name are preserved)
//   - `<slug-id>  +<Display Name>` (TTY space-padded table)
//
// Bare single-column tokens, single-space prose ("No models available"), multi-column rows,
// diagnostic rows, and rows whose display name shares no token with the model ID
// (e.g. `gemini-4-neon-high\tCatalog retrieval interrupted`) are rejected.
func parseAgyModelRow(line string) (string, string, agySepKind, bool) {
	var (
		id   string
		rest string
		sep  agySepKind
	)
	if tabIdx := strings.IndexByte(line, '\t'); tabIdx >= 0 {
		sep = agySepTab
		id = strings.TrimSpace(line[:tabIdx])
		rest = strings.TrimSpace(line[tabIdx+1:])
	} else if loc := agySpaceSepRe.FindStringIndex(line); loc != nil {
		sep = agySepSpaces
		id = strings.TrimSpace(line[:loc[0]])
		rest = strings.TrimSpace(line[loc[1]:])
	} else {
		return "", "", agySepNone, false
	}
	if !isValidAgyModelID(id) || !isValidAgyDisplayName(rest, sep) {
		return "", "", agySepNone, false
	}
	if !displayNameMatchesModelID(id, rest) {
		return "", "", agySepNone, false
	}
	return id, rest, sep, true
}

func isValidAgyDisplayName(name string, sep agySepKind) bool {
	if name == "" || strings.ContainsRune(name, '\t') {
		return false
	}
	// Space-padded TTY tables use 2+ spaces as column separators, so consecutive spaces
	// inside the remainder indicate an unexpected third column. Tab-separated non-TTY rows
	// use '\t' as the sole column delimiter and preserve internal consecutive spaces.
	if sep == agySepSpaces && strings.Contains(name, "  ") {
		return false
	}
	c := name[0]
	if !((c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')) {
		return false
	}
	lower := strings.ToLower(name)
	for _, prefix := range agyDiagnosticPrefixes {
		if strings.HasPrefix(lower, prefix) {
			return false
		}
	}
	for _, phrase := range agyDiagnosticPhrases {
		if strings.Contains(lower, phrase) {
			return false
		}
	}
	return true
}

func alphaNumTokens(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'))
	})
}

// displayNameMatchesModelID verifies that a row's display name shares at least one token with
// its model ID. This prevents unlisted diagnostic sentences paired with a valid model ID
// (e.g. `gemini-4-neon-high\tCatalog retrieval interrupted`) from being mistaken for model rows.
func displayNameMatchesModelID(id, displayName string) bool {
	idTokens := alphaNumTokens(id)
	nameTokens := alphaNumTokens(displayName)
	if len(idTokens) == 0 || len(nameTokens) == 0 {
		return false
	}
	nameSet := make(map[string]struct{}, len(nameTokens))
	for _, tok := range nameTokens {
		nameSet[tok] = struct{}{}
	}
	// Prefer matching tokens of length >= 2 so single-letter words like "a" cannot match prose.
	hasSignificant := false
	for _, tok := range idTokens {
		if len(tok) >= 2 {
			hasSignificant = true
			if _, ok := nameSet[tok]; ok {
				return true
			}
		}
	}
	if hasSignificant {
		return false
	}
	for _, tok := range idTokens {
		if _, ok := nameSet[tok]; ok {
			return true
		}
	}
	return false
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
	if !agySlugMarkerRe.MatchString(id) {
		return false
	}
	return !isDiagnosticAgyID(id)
}

func isDiagnosticAgyID(id string) bool {
	tokens := strings.FieldsFunc(strings.ToLower(id), func(r rune) bool {
		return r == '-' || r == '_' || r == '.' || r == ':' || r == '/'
	})
	for _, tok := range tokens {
		switch tok {
		case "error", "err", "warn", "warning", "fatal", "panic", "fail", "failed":
			return true
		}
	}
	return false
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

package quota

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

// ParseError classifies usage snapshot read/parse/schema failures so callers and CLI tools
// can distinguish format mismatches, duplicate keys, JSON syntax errors, and schema violations.
type ParseError struct {
	Code string
	Err  error
}

func (e *ParseError) Error() string {
	if e == nil {
		return ""
	}
	if e.Err == nil {
		return e.Code
	}
	return e.Err.Error()
}

func (e *ParseError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// ParseErrorCode extracts the structured error code from err if it wraps *ParseError.
func ParseErrorCode(err error) string {
	var pe *ParseError
	if errors.As(err, &pe) && pe.Code != "" {
		return pe.Code
	}
	return ReasonSnapshotError
}

// NormalizeUsageFormat normalizes an expected usage_format string ("legacy" when empty).
func NormalizeUsageFormat(format string) (string, error) {
	f := strings.TrimSpace(format)
	switch f {
	case "", FormatLegacy:
		return FormatLegacy, nil
	case FormatV1:
		return FormatV1, nil
	default:
		return "", fmt.Errorf("unsupported usage_format %q (allowed: %q, %q)", format, FormatLegacy, FormatV1)
	}
}

// LoadUsageFileWithFormat reads a usage snapshot file from path (expanding leading "~/")
// and parses it strictly according to format ("legacy" or "v1").
func LoadUsageFileWithFormat(path, format string) (*Snapshot, error) {
	normFmt, err := NormalizeUsageFormat(format)
	if err != nil {
		return nil, &ParseError{Code: ReasonFormatMismatch, Err: err}
	}
	resolved, err := expandHomePath(strings.TrimSpace(path))
	if err != nil {
		return nil, &ParseError{Code: "read_error", Err: fmt.Errorf("resolve usage_file %q: %w", path, err)}
	}
	if resolved == "" {
		return nil, &ParseError{Code: "read_error", Err: errors.New("usage_file path is empty")}
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		return nil, &ParseError{Code: "read_error", Err: fmt.Errorf("read usage_file %q: %w", resolved, err)}
	}
	snap, err := ParseUsageJSONWithFormat(data, normFmt)
	if err != nil {
		return nil, &ParseError{Code: ParseErrorCode(err), Err: fmt.Errorf("parse usage_file %q: %w", resolved, err)}
	}
	return snap, nil
}

// ParseUsageJSONWithFormat parses usage snapshot JSON according to the expected format.
// It never falls back between Schema v1 and Legacy formats:
//   - format="v1": requires strict Schema v1 (no duplicate keys, exact schema_version: 1).
//   - format="legacy": rejects any JSON containing a top-level "schema_version" key.
func ParseUsageJSONWithFormat(data []byte, format string) (*Snapshot, error) {
	normFmt, err := NormalizeUsageFormat(format)
	if err != nil {
		return nil, &ParseError{Code: ReasonFormatMismatch, Err: err}
	}
	switch normFmt {
	case FormatV1:
		return ParseUsageJSONV1(data)
	default:
		return parseLegacyUsageJSON(data)
	}
}

// topLevelInspection records top-level keys of a JSON object without rejecting duplicate keys
// inside nested objects (preserving Legacy runtime parsing compatibility per Item A).
type topLevelInspection struct {
	isObject         bool
	hasSchemaVersion bool
	schemaVersionRaw string
	hasUpdatedAt     bool
	hasServices      bool
}

func inspectTopLevelJSON(data []byte) (topLevelInspection, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return topLevelInspection{}, err
	}
	delim, ok := tok.(json.Delim)
	if !ok || delim != '{' {
		return topLevelInspection{isObject: false}, nil
	}
	info := topLevelInspection{isObject: true}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return topLevelInspection{}, err
		}
		key, ok := keyTok.(string)
		if !ok {
			return topLevelInspection{}, fmt.Errorf("expected string key, got %T", keyTok)
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return topLevelInspection{}, err
		}
		switch key {
		case "schema_version":
			info.hasSchemaVersion = true
			info.schemaVersionRaw = strings.TrimSpace(string(raw))
		case "updatedAt":
			info.hasUpdatedAt = true
		case "services":
			info.hasServices = true
		}
	}
	// Consume closing '}'
	if _, err := dec.Token(); err != nil {
		return topLevelInspection{}, err
	}
	return info, nil
}

// DetectDuplicateJSONKeys validates JSON syntax and checks every nested JSON object
// for duplicate keys, returning the first duplicate key path found (or a syntax error).
func DetectDuplicateJSONKeys(data []byte) (dupPath string, syntaxErr error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	dup, err := walkNoDupKeys(dec, "")
	if err != nil {
		return "", err
	}
	if dup != "" {
		return dup, nil
	}
	// Ensure no trailing garbage after top-level value.
	if tok, err := dec.Token(); err != io.EOF {
		if err != nil {
			return "", err
		}
		return "", fmt.Errorf("unexpected trailing token %v", tok)
	}
	return "", nil
}

func walkNoDupKeys(dec *json.Decoder, path string) (string, error) {
	tok, err := dec.Token()
	if err != nil {
		return "", err
	}
	return walkTokenNoDupKeys(dec, tok, path)
}

func walkTokenNoDupKeys(dec *json.Decoder, tok json.Token, path string) (string, error) {
	delim, ok := tok.(json.Delim)
	if !ok {
		return "", nil
	}
	switch delim {
	case '{':
		seen := make(map[string]struct{})
		for dec.More() {
			kTok, err := dec.Token()
			if err != nil {
				return "", err
			}
			k, ok := kTok.(string)
			if !ok {
				return "", fmt.Errorf("expected object key string, got %T", kTok)
			}
			childPath := k
			if path != "" {
				childPath = path + "." + k
			}
			if _, dup := seen[k]; dup {
				return childPath, nil
			}
			seen[k] = struct{}{}
			if dup, err := walkNoDupKeys(dec, childPath); err != nil || dup != "" {
				return dup, err
			}
		}
		endTok, err := dec.Token()
		if err != nil {
			return "", err
		}
		if endDelim, ok := endTok.(json.Delim); !ok || endDelim != '}' {
			return "", fmt.Errorf("expected '}', got %v", endTok)
		}
		return "", nil
	case '[':
		idx := 0
		for dec.More() {
			childPath := fmt.Sprintf("%s[%d]", path, idx)
			if dup, err := walkNoDupKeys(dec, childPath); err != nil || dup != "" {
				return dup, err
			}
			idx++
		}
		endTok, err := dec.Token()
		if err != nil {
			return "", err
		}
		if endDelim, ok := endTok.(json.Delim); !ok || endDelim != ']' {
			return "", fmt.Errorf("expected ']', got %v", endTok)
		}
		return "", nil
	default:
		return "", fmt.Errorf("unexpected delimiter %q", delim)
	}
}

// ParseUsageJSONV1 strictly decodes a Schema v1 usage snapshot (docs/23-usage-snapshot.md).
func ParseUsageJSONV1(data []byte) (*Snapshot, error) {
	dupPath, synErr := DetectDuplicateJSONKeys(data)
	if synErr != nil {
		return nil, &ParseError{Code: "invalid_json", Err: fmt.Errorf("invalid usage v1 json: %w", synErr)}
	}
	if dupPath != "" {
		return nil, &ParseError{Code: "duplicate_key", Err: fmt.Errorf("duplicate JSON key %q", dupPath)}
	}

	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return nil, &ParseError{Code: "invalid_json", Err: fmt.Errorf("usage v1 must be a JSON object: %w", err)}
	}
	if top == nil {
		return nil, &ParseError{Code: "schema_violation", Err: errors.New("usage v1 must be a JSON object, got null")}
	}

	// schema_version: must exist and be exact JSON integer 1 (no fallback to legacy).
	svRaw, hasSV := top["schema_version"]
	if !hasSV {
		if _, hasU := top["updatedAt"]; hasU {
			return nil, &ParseError{Code: ReasonFormatMismatch, Err: errors.New("format_mismatch: expected v1 (missing schema_version), got legacy format")}
		}
		if _, hasS := top["services"]; hasS {
			return nil, &ParseError{Code: ReasonFormatMismatch, Err: errors.New("format_mismatch: expected v1 (missing schema_version), got legacy format")}
		}
		return nil, &ParseError{Code: ReasonFormatMismatch, Err: errors.New("format_mismatch: missing required schema_version")}
	}
	if strings.TrimSpace(string(svRaw)) != "1" {
		return nil, &ParseError{Code: "schema_violation", Err: fmt.Errorf("schema_version must be integer 1, got %s", strings.TrimSpace(string(svRaw)))}
	}

	// generated_at: required RFC3339 string with timezone.
	genRaw, hasGen := top["generated_at"]
	if !hasGen {
		return nil, &ParseError{Code: "schema_violation", Err: errors.New("missing required generated_at")}
	}
	generatedAt, err := decodeRequiredRFC3339(genRaw, "generated_at")
	if err != nil {
		return nil, &ParseError{Code: "schema_violation", Err: err}
	}

	// collector: optional object or null.
	var collector *CollectorInfo
	if colRaw, hasCol := top["collector"]; hasCol && !isJSONNull(colRaw) {
		c, err := decodeCollectorV1(colRaw)
		if err != nil {
			return nil, &ParseError{Code: "schema_violation", Err: err}
		}
		collector = c
	}

	// pools: required JSON object.
	poolsRaw, hasPools := top["pools"]
	if !hasPools {
		return nil, &ParseError{Code: "schema_violation", Err: errors.New("missing required pools object")}
	}
	if isJSONNull(poolsRaw) {
		return nil, &ParseError{Code: "schema_violation", Err: errors.New("pools must be a JSON object, got null")}
	}
	var rawPools map[string]json.RawMessage
	if err := json.Unmarshal(poolsRaw, &rawPools); err != nil {
		return nil, &ParseError{Code: "schema_violation", Err: fmt.Errorf("pools must be a JSON object: %w", err)}
	}

	snap := &Snapshot{
		Format:        FormatV1,
		SchemaVersion: 1,
		GeneratedAt:   generatedAt,
		Collector:     collector,
		MeasuredAt:    generatedAt,
		Pools:         make(map[string]PoolSnapshot, len(rawPools)),
	}

	for name, entryRaw := range rawPools {
		if name == "" || strings.TrimSpace(name) != name {
			return nil, &ParseError{Code: "schema_violation", Err: fmt.Errorf("invalid pool key %q: must be non-empty without leading/trailing whitespace", name)}
		}
		ps, err := decodePoolEntryV1(name, entryRaw)
		if err != nil {
			return nil, &ParseError{Code: "schema_violation", Err: err}
		}
		snap.Pools[name] = ps
	}

	return snap, nil
}

func decodeCollectorV1(raw json.RawMessage) (*CollectorInfo, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, fmt.Errorf("collector must be a JSON object")
	}
	info := &CollectorInfo{}
	if v, ok := fields["name"]; ok && !isJSONNull(v) {
		s, err := decodeStrictString(v, "collector.name")
		if err != nil {
			return nil, err
		}
		info.Name = s
	}
	if v, ok := fields["version"]; ok && !isJSONNull(v) {
		s, err := decodeStrictString(v, "collector.version")
		if err != nil {
			return nil, err
		}
		info.Version = s
	}
	if v, ok := fields["host"]; ok && !isJSONNull(v) {
		s, err := decodeStrictString(v, "collector.host")
		if err != nil {
			return nil, err
		}
		info.Host = s
	}
	if v, ok := fields["interval_seconds"]; ok && !isJSONNull(v) {
		n, err := decodeStrictInt(v, "collector.interval_seconds")
		if err != nil {
			return nil, err
		}
		if n <= 0 {
			return nil, fmt.Errorf("collector.interval_seconds must be > 0, got %d", n)
		}
		info.IntervalSeconds = &n
	}
	return info, nil
}

func decodePoolEntryV1(name string, raw json.RawMessage) (PoolSnapshot, error) {
	if isJSONNull(raw) {
		return PoolSnapshot{}, fmt.Errorf("pools.%s must be a JSON object, got null", name)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return PoolSnapshot{}, fmt.Errorf("pools.%s must be a JSON object", name)
	}

	// status: optional, defaults to "ok"; null or other values are schema violations.
	status := PoolEntryStatusOK
	if v, ok := fields["status"]; ok {
		s, err := decodeStrictString(v, fmt.Sprintf("pools.%s.status", name))
		if err != nil {
			return PoolSnapshot{}, err
		}
		if s != PoolEntryStatusOK && s != PoolEntryStatusError {
			return PoolSnapshot{}, fmt.Errorf("pools.%s.status must be %q or %q, got %q", name, PoolEntryStatusOK, PoolEntryStatusError, s)
		}
		status = s
	}

	// error: optional string or null.
	var errSummary string
	if v, ok := fields["error"]; ok && !isJSONNull(v) {
		s, err := decodeStrictString(v, fmt.Sprintf("pools.%s.error", name))
		if err != nil {
			return PoolSnapshot{}, err
		}
		errSummary = s
	}

	// measured_at: key MUST exist; non-null RFC3339 required when status=="ok"; null allowed when status=="error".
	measRaw, hasMeas := fields["measured_at"]
	if !hasMeas {
		return PoolSnapshot{}, fmt.Errorf("pools.%s.measured_at is required", name)
	}
	var measuredAt time.Time
	if isJSONNull(measRaw) {
		if status == PoolEntryStatusOK {
			return PoolSnapshot{}, fmt.Errorf("pools.%s.measured_at must not be null when status is %q", name, PoolEntryStatusOK)
		}
	} else {
		t, err := decodeRequiredRFC3339(measRaw, fmt.Sprintf("pools.%s.measured_at", name))
		if err != nil {
			return PoolSnapshot{}, err
		}
		measuredAt = t
	}

	// weekly: key MUST exist; object required when status=="ok"; null allowed when status=="error".
	weeklyRaw, hasWeekly := fields["weekly"]
	if !hasWeekly {
		return PoolSnapshot{}, fmt.Errorf("pools.%s.weekly is required", name)
	}

	ps := PoolSnapshot{
		Status:     status,
		Error:      errSummary,
		MeasuredAt: measuredAt,
	}

	if isJSONNull(weeklyRaw) {
		if status == PoolEntryStatusOK {
			return PoolSnapshot{}, fmt.Errorf("pools.%s.weekly must not be null when status is %q", name, PoolEntryStatusOK)
		}
		ps.WeeklyAbsent = true
	} else {
		wPct, wReset, wApprox, err := decodeWeeklyV1(name, weeklyRaw)
		if err != nil {
			return PoolSnapshot{}, err
		}
		ps.WeeklyRemainingPercent = wPct
		ps.WeeklyResetAt = wReset
		ps.WeeklyResetAtApprox = wApprox
	}

	// short: optional; omitted or null -> missing; object -> decode with §3.3 normalization rules.
	if shortRaw, hasShort := fields["short"]; hasShort && !isJSONNull(shortRaw) {
		if err := decodeShortV1(name, shortRaw, &ps); err != nil {
			return PoolSnapshot{}, err
		}
	}

	return ps, nil
}

func decodeWeeklyV1(poolName string, raw json.RawMessage) (int, *time.Time, bool, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return 0, nil, false, fmt.Errorf("pools.%s.weekly must be a JSON object", poolName)
	}

	pctRaw, hasPct := fields["remaining_percent"]
	if !hasPct || isJSONNull(pctRaw) {
		return 0, nil, false, fmt.Errorf("pools.%s.weekly.remaining_percent is required and must be an integer 0..100", poolName)
	}
	pct, err := decodePercentInt(pctRaw, fmt.Sprintf("pools.%s.weekly.remaining_percent", poolName))
	if err != nil {
		return 0, nil, false, err
	}

	var resetAt *time.Time
	if rRaw, ok := fields["reset_at"]; ok && !isJSONNull(rRaw) {
		t, err := decodeRequiredRFC3339(rRaw, fmt.Sprintf("pools.%s.weekly.reset_at", poolName))
		if err != nil {
			return 0, nil, false, err
		}
		resetAt = &t
	}

	var approx bool
	if aRaw, ok := fields["reset_at_approx"]; ok {
		b, err := decodeStrictBool(aRaw, fmt.Sprintf("pools.%s.weekly.reset_at_approx", poolName))
		if err != nil {
			return 0, nil, false, err
		}
		approx = b
	}

	return pct, resetAt, approx, nil
}

func decodeShortV1(poolName string, raw json.RawMessage, ps *PoolSnapshot) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return fmt.Errorf("pools.%s.short must be a JSON object or null", poolName)
	}

	applicable := true
	if appRaw, ok := fields["applicable"]; ok {
		b, err := decodeStrictBool(appRaw, fmt.Sprintf("pools.%s.short.applicable", poolName))
		if err != nil {
			return err
		}
		applicable = b
		ps.ShortApplicable = &b
	}

	if !applicable {
		for _, forbidden := range []string{"remaining_percent", "reset_at", "reset_at_approx", "window"} {
			if _, exists := fields[forbidden]; exists {
				return fmt.Errorf("pools.%s.short must not combine applicable=false with %q", poolName, forbidden)
			}
		}
		return nil
	}

	// applicable != false:
	// - remaining_percent omitted or explicit null -> normalize to missing (NOT schema violation, §3.3).
	// - remaining_percent present and non-null -> strict integer 0..100 (out-of-range/bad type IS schema violation).
	if pctRaw, hasPct := fields["remaining_percent"]; !hasPct || isJSONNull(pctRaw) {
		ps.ShortRemainingPercent = nil
		ps.ShortNormalized = true
	} else {
		pct, err := decodePercentInt(pctRaw, fmt.Sprintf("pools.%s.short.remaining_percent", poolName))
		if err != nil {
			return err
		}
		ps.ShortRemainingPercent = &pct
	}

	if rRaw, ok := fields["reset_at"]; ok && !isJSONNull(rRaw) {
		t, err := decodeRequiredRFC3339(rRaw, fmt.Sprintf("pools.%s.short.reset_at", poolName))
		if err != nil {
			return err
		}
		ps.ShortResetAt = &t
	}

	if aRaw, ok := fields["reset_at_approx"]; ok {
		b, err := decodeStrictBool(aRaw, fmt.Sprintf("pools.%s.short.reset_at_approx", poolName))
		if err != nil {
			return err
		}
		ps.ShortResetAtApprox = b
	}

	if wRaw, ok := fields["window"]; ok {
		w, err := decodeStrictString(wRaw, fmt.Sprintf("pools.%s.short.window", poolName))
		if err != nil {
			return err
		}
		if strings.TrimSpace(w) == "" {
			return fmt.Errorf("pools.%s.short.window must not be empty", poolName)
		}
		ps.ShortWindow = w
	}

	return nil
}

func isJSONNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func decodeStrictString(raw json.RawMessage, field string) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '"' {
		return "", fmt.Errorf("%s must be a JSON string, got %s", field, string(trimmed))
	}
	var s string
	if err := json.Unmarshal(trimmed, &s); err != nil {
		return "", fmt.Errorf("%s must be a JSON string: %w", field, err)
	}
	return s, nil
}

func decodeStrictBool(raw json.RawMessage, field string) (bool, error) {
	trimmed := bytes.TrimSpace(raw)
	if bytes.Equal(trimmed, []byte("true")) {
		return true, nil
	}
	if bytes.Equal(trimmed, []byte("false")) {
		return false, nil
	}
	return false, fmt.Errorf("%s must be a JSON boolean, got %s", field, string(trimmed))
}

func decodeStrictInt(raw json.RawMessage, field string) (int, error) {
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return 0, fmt.Errorf("%s must be a JSON integer", field)
	}
	// Reject strings, booleans, floats ('.', 'e', 'E'), leading '+'.
	i := 0
	if s[0] == '-' {
		i = 1
		if len(s) == 1 {
			return 0, fmt.Errorf("%s must be a JSON integer, got %s", field, s)
		}
	}
	for ; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, fmt.Errorf("%s must be a JSON integer, got %s", field, s)
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("%s must be a JSON integer: %w", field, err)
	}
	return n, nil
}

func decodePercentInt(raw json.RawMessage, field string) (int, error) {
	n, err := decodeStrictInt(raw, field)
	if err != nil {
		return 0, err
	}
	if n < 0 || n > 100 {
		return 0, fmt.Errorf("%s must be between 0 and 100, got %d", field, n)
	}
	return n, nil
}

func decodeRequiredRFC3339(raw json.RawMessage, field string) (time.Time, error) {
	s, err := decodeStrictString(raw, field)
	if err != nil {
		return time.Time{}, err
	}
	if s == "" || strings.TrimSpace(s) != s {
		return time.Time{}, fmt.Errorf("%s must be a non-empty RFC3339 timestamp, got %q", field, s)
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s must be a valid RFC3339 timestamp with timezone (%q): %w", field, s, err)
	}
	return t, nil
}

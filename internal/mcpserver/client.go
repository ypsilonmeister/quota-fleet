package mcpserver

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ypsilonmeister/quota-fleet/internal/config"
)

const (
	defaultHTTPTimeout = 30 * time.Second
	// Single-resource 2xx responses: a job has up to 3 API-writable 1 MiB text fields
	// (instructions/create payload, result_text, last_error); Go's encoding/json expands
	// HTML-escaped bytes (<, >, &) up to 6x (~18 MiB/job + metadata).
	defaultMaxSingleResponseBytes = int64(32) << 20 // 32 MiB
	// List 2xx HTTP stream cap: streaming element-by-element decoding validates the full
	// 2xx JSON response to EOF while keeping only bounded list projections in memory.
	defaultMaxListResponseBytes = int64(64) << 20 // 64 MiB
	// MCP SDK v1.8.0 DefaultMaxLineLength is 16 MiB per JSON-RPC frame. Because mcp.AddTool
	// emits both structuredContent (raw JSON) and content[0].text (JSON-escaped string of
	// the same JSON), list tools budget the entire serialized output + JSON-RPC wrapper
	// to <= 14 MiB on the wire (2 MiB below 16 MiB).
	defaultMaxMCPFrameBytes  = int64(14) << 20 // 14 MiB
	mcpJSONRPCWrapperReserve = int64(512)      // reserve for JSON-RPC envelope around tool result
	defaultMaxListTextBytes  = 4096            // max UTF-8 bytes per long text field in list projections
	maxListCursorStringBytes = 256             // safety cap on upstream string cursors
	defaultUpstreamListLimit = 50              // matches store.DefaultListLimit
	maxUpstreamListLimit     = 200             // matches store.MaxListLimit
	maxErrorResponseBytes    = 64 << 10        // 64 KiB cap for non-2xx error bodies
	maxErrorMessageRunes     = 256
)

var errResponseTooLarge = errors.New("response body exceeded size limit")

type responseTooLargeError struct {
	method string
	path   string
	limit  int64
}

func (e *responseTooLargeError) Error() string {
	return fmt.Sprintf("response from %s %s exceeded %d bytes", e.method, e.path, e.limit)
}

func (e *responseTooLargeError) Unwrap() error {
	return errResponseTooLarge
}

var (
	authHeaderRe  = regexp.MustCompile(`(?i)\bauthorization\s*[:=]\s*[^\r\n,;"]+`)
	bearerTokenRe = regexp.MustCompile(`(?i)\bbearer\s+\S+`)
	basicTokenRe  = regexp.MustCompile(`(?i)\bbasic\s+[A-Za-z0-9+/=]{4,}`)
	safeStatusRe  = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
)

// Client is a thin HTTP client for the quota-fleet JSON API.
type Client struct {
	baseURL          string
	token            string
	httpClient       *http.Client
	maxResponseBytes int64
	maxMCPFrameBytes int64
	maxListTextBytes int
}

// APIError represents a non-success HTTP response from the quota-fleet API.
type APIError struct {
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("quota-fleet API error (HTTP %d): %s", e.StatusCode, e.Message)
	}
	return fmt.Sprintf("quota-fleet API error (HTTP %d)", e.StatusCode)
}

// NewClient constructs an HTTP API client for quota-fleet.
func NewClient(baseURL, token string, httpClient *http.Client) (*Client, error) {
	trimmed := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if trimmed == "" {
		return nil, errors.New("QUOTA_FLEET_API_URL must not be empty")
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return nil, fmt.Errorf("invalid QUOTA_FLEET_API_URL %q: %w", trimmed, err)
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, fmt.Errorf("invalid QUOTA_FLEET_API_URL %q: must include http(s) scheme and host", trimmed)
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultHTTPTimeout}
	}
	return &Client{
		baseURL:    trimmed,
		token:      strings.TrimSpace(token),
		httpClient: httpClient,
	}, nil
}

type maxBytesReader struct {
	r        io.Reader
	remain   int64
	exceeded bool
}

func (m *maxBytesReader) Read(p []byte) (int, error) {
	if m.remain <= 0 {
		var probe [1]byte
		n, err := m.r.Read(probe[:])
		if n > 0 {
			m.exceeded = true
			return 0, errResponseTooLarge
		}
		return 0, err
	}
	if int64(len(p)) > m.remain {
		p = p[:m.remain]
	}
	n, err := m.r.Read(p)
	m.remain -= int64(n)
	return n, err
}

func ensureJSONEOF(dec *json.Decoder) error {
	_, err := dec.Token()
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("unexpected trailing data after JSON value")
	}
	return err
}

func decodeJSONStream(r io.Reader, dst any) error {
	dec := json.NewDecoder(r)
	if err := dec.Decode(dst); err != nil {
		return err
	}
	return ensureJSONEOF(dec)
}

func (c *Client) effectiveResponseLimit(defaultLimit int64) int64 {
	if c.maxResponseBytes > 0 {
		return c.maxResponseBytes
	}
	if defaultLimit > 0 {
		return defaultLimit
	}
	return defaultMaxSingleResponseBytes
}

func (c *Client) effectiveMCPFrameBudget() int64 {
	if c.maxMCPFrameBytes > 0 {
		return c.maxMCPFrameBytes
	}
	return defaultMaxMCPFrameBytes
}

func (c *Client) effectiveListTextLimit() int {
	if c.maxListTextBytes > 0 {
		return c.maxListTextBytes
	}
	return defaultMaxListTextBytes
}

// ListJobsResult is the typed response envelope for list_jobs.
type ListJobsResult struct {
	Jobs            []json.RawMessage `json:"jobs"`
	NextCursor      int64             `json:"next_cursor,omitempty"`
	HasMore         bool              `json:"has_more"`
	Truncated       bool              `json:"truncated,omitempty"`
	TruncatedReason string            `json:"truncated_reason,omitempty"`
}

// ListHeldResult is the typed response envelope for list_held.
type ListHeldResult struct {
	Held            []json.RawMessage `json:"held"`
	NextCursor      string            `json:"next_cursor,omitempty"`
	HasMore         bool              `json:"has_more"`
	Truncated       bool              `json:"truncated,omitempty"`
	TruncatedReason string            `json:"truncated_reason,omitempty"`
}

// GateDecisionsResult is the typed response envelope for gate_decisions.
type GateDecisionsResult struct {
	Decisions       []json.RawMessage `json:"decisions"`
	NextCursor      int64             `json:"next_cursor,omitempty"`
	HasMore         bool              `json:"has_more"`
	Truncated       bool              `json:"truncated,omitempty"`
	TruncatedReason string            `json:"truncated_reason,omitempty"`
}

// ListDeliveriesResult is the typed response envelope for list_deliveries.
type ListDeliveriesResult struct {
	Jobs            []json.RawMessage `json:"jobs"`
	NextCursor      string            `json:"next_cursor,omitempty"`
	HasMore         bool              `json:"has_more"`
	Truncated       bool              `json:"truncated,omitempty"`
	TruncatedReason string            `json:"truncated_reason,omitempty"`
}

type listCursorMode int

const (
	listCursorInt64ID listCursorMode = iota
	listCursorHeldCreatedID
	listCursorDeliveryUpdatedID
	listCursorDeliveryDeliveredID
)

const (
	listTruncatedReason     = "response exceeded MCP frame byte budget"
	listHTTPTruncatedReason = "upstream HTTP response exceeded byte limit; returned smaller page"
)

// mcpWireItemBytes computes the exact upper-bound byte contribution of a single
// compact HTML-escaped JSON item to an MCP CallToolResult JSON-RPC frame.
// Because mcp.AddTool emits both structuredContent (raw JSON) and content[0].text
// (a JSON-encoded string of the same JSON), every byte appears twice plus 1 extra
// backslash for each '"' or '\\' in content[0].text, plus 2 bytes for array commas.
func mcpWireItemBytes(compactHTMLJSON []byte) int64 {
	var extra int64
	for _, b := range compactHTMLJSON {
		if b == '"' || b == '\\' {
			extra++
		}
	}
	return int64(len(compactHTMLJSON))*2 + extra + 2
}

// mcpWireFrameBytes computes the exact upper-bound wire byte size of an MCP CallToolResult
// JSON-RPC frame carrying compactHTMLJSON as both structuredContent and content[0].text,
// including the JSON-RPC wrapper reserve.
func mcpWireFrameBytes(compactHTMLJSON []byte) int64 {
	var extra int64
	for _, b := range compactHTMLJSON {
		if b == '"' || b == '\\' {
			extra++
		}
	}
	return int64(len(compactHTMLJSON))*2 + extra + 2 + mcpJSONRPCWrapperReserve
}

// skipJSONValue consumes and validates a single JSON value from dec without retaining it.
func skipJSONValue(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	if delim != '{' && delim != '[' {
		return fmt.Errorf("unexpected JSON delimiter %q", delim)
	}
	depth := 1
	for depth > 0 {
		t, err := dec.Token()
		if err != nil {
			return err
		}
		if d, ok := t.(json.Delim); ok {
			switch d {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
		}
	}
	return nil
}

func truncateUTF8Bytes(s string, maxBytes int) (string, bool) {
	if len(s) <= maxBytes {
		return s, false
	}
	if maxBytes <= 0 {
		return "", true
	}
	if maxBytes <= 3 {
		n := 0
		for n < len(s) {
			_, size := utf8.DecodeRuneInString(s[n:])
			if n+size > maxBytes {
				break
			}
			n += size
		}
		return s[:n], true
	}
	target := maxBytes - 3
	n := 0
	for n < len(s) {
		_, size := utf8.DecodeRuneInString(s[n:])
		if n+size > target {
			break
		}
		n += size
	}
	return s[:n] + "...", true
}

func projectJSONValue(v any, prefix string, maxTextBytes int, depth int, truncated *[]string) any {
	switch val := v.(type) {
	case string:
		if out, did := truncateUTF8Bytes(val, maxTextBytes); did {
			if prefix != "" {
				*truncated = append(*truncated, prefix)
			}
			return out
		}
		return val
	case map[string]any:
		if depth >= 4 {
			return val
		}
		for k, child := range val {
			if k == "fields_truncated" {
				continue
			}
			path := k
			if prefix != "" {
				path = prefix + "." + k
			}
			val[k] = projectJSONValue(child, path, maxTextBytes, depth+1, truncated)
		}
		return val
	case []any:
		if depth >= 4 {
			return val
		}
		var elemTruncated bool
		for i, elem := range val {
			if s, ok := elem.(string); ok {
				if out, did := truncateUTF8Bytes(s, maxTextBytes); did {
					val[i] = out
					elemTruncated = true
				}
			} else {
				var nested []string
				val[i] = projectJSONValue(elem, prefix, maxTextBytes, depth+1, &nested)
				if len(nested) > 0 {
					elemTruncated = true
				}
			}
		}
		if elemTruncated && prefix != "" {
			*truncated = append(*truncated, prefix)
		}
		return val
	default:
		return v
	}
}

// projectListItem bounds long string fields inside a list item to maxTextBytes (UTF-8 safe)
// and records any truncated field paths in "fields_truncated".
func projectListItem(raw json.RawMessage, maxTextBytes int) (json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var val any
	if err := dec.Decode(&val); err != nil {
		return nil, err
	}
	obj, ok := val.(map[string]any)
	if !ok {
		return json.Marshal(val)
	}
	var truncated []string
	projectJSONValue(obj, "", maxTextBytes, 0, &truncated)
	if len(truncated) > 0 {
		sort.Strings(truncated)
		dedup := truncated[:0]
		for _, f := range truncated {
			if len(dedup) == 0 || dedup[len(dedup)-1] != f {
				dedup = append(dedup, f)
			}
		}
		obj["fields_truncated"] = dedup
	}
	return json.Marshal(obj)
}

// projectMinimalItem produces a minimal identifying stub for a list item when even the
// normal bounded projection exceeds a tight MCP frame budget, ensuring forward pagination
// can still proceed.
func projectMinimalItem(raw json.RawMessage) (json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var val any
	if err := dec.Decode(&val); err != nil {
		return nil, err
	}
	obj, ok := val.(map[string]any)
	if !ok {
		return json.Marshal(val)
	}
	stub := make(map[string]any, 8)
	for _, key := range []string{"id", "status", "created_at", "updated_at", "delivered_at"} {
		if v, exists := obj[key]; exists {
			stub[key] = v
		}
	}
	if title, ok := obj["title"].(string); ok {
		stub["title"], _ = truncateUTF8Bytes(title, 128)
	}
	stub["fields_truncated"] = []string{"*"}
	return json.Marshal(stub)
}

func extractInt64ID(raw []byte) (int64, bool) {
	var envelope struct {
		ID *int64 `json:"id"`
	}
	if err := json.Unmarshal(raw, &envelope); err == nil && envelope.ID != nil {
		return *envelope.ID, true
	}
	return 0, false
}

func extractHeldCursor(raw []byte) (string, bool) {
	var envelope struct {
		ID        string `json:"id"`
		CreatedAt string `json:"created_at"`
	}
	if err := json.Unmarshal(raw, &envelope); err == nil && envelope.ID != "" && envelope.CreatedAt != "" {
		return envelope.CreatedAt + "|" + envelope.ID, true
	}
	return "", false
}

func extractDeliveryCursor(raw []byte, useDeliveredAt bool) (string, bool) {
	var envelope struct {
		ID          *int64 `json:"id"`
		UpdatedAt   string `json:"updated_at"`
		DeliveredAt string `json:"delivered_at"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil || envelope.ID == nil || *envelope.ID <= 0 {
		return "", false
	}
	ts := envelope.UpdatedAt
	if useDeliveredAt && envelope.DeliveredAt != "" {
		ts = envelope.DeliveredAt
	}
	if ts == "" {
		return "", false
	}
	return ts + "|" + strconv.FormatInt(*envelope.ID, 10), true
}

type budgetedListPage struct {
	items           []json.RawMessage
	nextCursorInt   int64
	nextCursorStr   string
	hasMore         bool
	truncated       bool
	truncatedReason string
}

func computeCursorFromLastItem(mode listCursorMode, item json.RawMessage) (int64, string) {
	switch mode {
	case listCursorInt64ID:
		if id, ok := extractInt64ID(item); ok {
			return id, ""
		}
	case listCursorHeldCreatedID:
		if c, ok := extractHeldCursor(item); ok {
			return 0, c
		}
	case listCursorDeliveryUpdatedID:
		if c, ok := extractDeliveryCursor(item, false); ok {
			return 0, c
		}
	case listCursorDeliveryDeliveredID:
		if c, ok := extractDeliveryCursor(item, true); ok {
			return 0, c
		}
	}
	return 0, ""
}

func buildTypedListEnvelope(itemsKey string, cursorMode listCursorMode, page *budgetedListPage) any {
	items := page.items
	if items == nil {
		items = []json.RawMessage{}
	}
	switch {
	case itemsKey == "jobs" && cursorMode == listCursorInt64ID:
		return &ListJobsResult{
			Jobs:            items,
			NextCursor:      page.nextCursorInt,
			HasMore:         page.hasMore,
			Truncated:       page.truncated,
			TruncatedReason: page.truncatedReason,
		}
	case itemsKey == "held":
		return &ListHeldResult{
			Held:            items,
			NextCursor:      page.nextCursorStr,
			HasMore:         page.hasMore,
			Truncated:       page.truncated,
			TruncatedReason: page.truncatedReason,
		}
	case itemsKey == "decisions":
		return &GateDecisionsResult{
			Decisions:       items,
			NextCursor:      page.nextCursorInt,
			HasMore:         page.hasMore,
			Truncated:       page.truncated,
			TruncatedReason: page.truncatedReason,
		}
	default:
		return &ListDeliveriesResult{
			Jobs:            items,
			NextCursor:      page.nextCursorStr,
			HasMore:         page.hasMore,
			Truncated:       page.truncated,
			TruncatedReason: page.truncatedReason,
		}
	}
}

// decodeBudgetedList decodes a list envelope JSON object element-by-element so that
// json.Decoder only retains projected items within the MCP frame byte budget.
//
// Contract guarantees:
//  1. Trailing validation: Always validates the entire 2xx JSON stream to EOF (including
//     skipped array items, closing delimiters, remaining envelope keys, and trailing data),
//     whether truncated or not.
//  2. Output-wide budget: Only known typed envelope fields are retained (unknown keys are
//     validated and discarded), and the exact serialized envelope + JSON-RPC wrapper wire
//     size is verified to fit within maxMCPFrameBytes.
//  3. Continuability: Long text fields are projected to <= maxTextBytes (marking
//     fields_truncated) so single large items never block pagination; truncated pages set
//     truncated=true, has_more=true, and next_cursor from the last kept item.
func decodeBudgetedList(
	r io.Reader,
	itemsKey string,
	cursorMode listCursorMode,
	maxMCPFrameBytes int64,
	maxTextBytes int,
) (any, error) {
	return decodeBudgetedListWithShrink(r, itemsKey, cursorMode, maxMCPFrameBytes, maxTextBytes, false)
}

func decodeBudgetedListWithShrink(
	r io.Reader,
	itemsKey string,
	cursorMode listCursorMode,
	maxMCPFrameBytes int64,
	maxTextBytes int,
	httpPageShrunk bool,
) (any, error) {
	if maxTextBytes <= 0 {
		maxTextBytes = defaultMaxListTextBytes
	}

	dec := json.NewDecoder(r)
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return nil, fmt.Errorf("expected JSON object, got %v", tok)
	}

	// Measure the empty-envelope overhead (including worst-case truncation metadata)
	// so incremental item budgeting closely tracks the final serialized frame size.
	probeReason := listTruncatedReason
	if len(listHTTPTruncatedReason) > len(probeReason) {
		probeReason = listHTTPTruncatedReason
	}
	probePage := &budgetedListPage{
		items:           []json.RawMessage{},
		nextCursorInt:   9223372036854775807,
		nextCursorStr:   strings.Repeat("x", 64),
		hasMore:         true,
		truncated:       true,
		truncatedReason: probeReason,
	}
	probeJSON, err := json.Marshal(buildTypedListEnvelope(itemsKey, cursorMode, probePage))
	if err != nil {
		return nil, err
	}
	envelopeOverhead := mcpWireFrameBytes(probeJSON)
	itemBudget := maxMCPFrameBytes - envelopeOverhead
	if itemBudget < 0 {
		itemBudget = 0
	}

	page := &budgetedListPage{
		items: make([]json.RawMessage, 0),
	}
	var usedWireBytes int64

	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil, fmt.Errorf("expected object key string, got %T", keyTok)
		}

		switch key {
		case itemsKey:
			arrTok, err := dec.Token()
			if err != nil {
				return nil, err
			}
			if arrTok == nil {
				page.items = []json.RawMessage{}
				continue
			}
			if delim, ok := arrTok.(json.Delim); !ok || delim != '[' {
				return nil, fmt.Errorf("expected JSON array for %q, got %v", itemsKey, arrTok)
			}

			for dec.More() {
				if page.truncated {
					if err := skipJSONValue(dec); err != nil {
						return nil, err
					}
					continue
				}

				var rawItem json.RawMessage
				if err := dec.Decode(&rawItem); err != nil {
					return nil, err
				}
				projected, err := projectListItem(rawItem, maxTextBytes)
				if err != nil {
					return nil, err
				}
				itemWire := mcpWireItemBytes(projected)
				if usedWireBytes+itemWire > itemBudget {
					if len(page.items) == 0 {
						// Even the first projected item exceeded the tight budget;
						// fall back to a minimal identifying stub so pagination can still advance.
						stub, stubErr := projectMinimalItem(rawItem)
						if stubErr != nil {
							return nil, stubErr
						}
						stubWire := mcpWireItemBytes(stub)
						if stubWire <= itemBudget {
							usedWireBytes += stubWire
							page.items = append(page.items, stub)
							continue
						}
						return nil, fmt.Errorf("first item in %q (%d wire bytes) exceeds MCP frame budget (%d bytes)", itemsKey, stubWire, maxMCPFrameBytes)
					}
					page.truncated = true
					continue
				}
				usedWireBytes += itemWire
				page.items = append(page.items, projected)
			}

			endArr, err := dec.Token()
			if err != nil {
				return nil, err
			}
			if delim, ok := endArr.(json.Delim); !ok || delim != ']' {
				return nil, fmt.Errorf("expected closing ']' for %q, got %v", itemsKey, endArr)
			}

		case "has_more":
			var hm bool
			if err := dec.Decode(&hm); err != nil {
				return nil, err
			}
			page.hasMore = hm

		case "next_cursor":
			if cursorMode == listCursorInt64ID {
				var nc *int64
				if err := dec.Decode(&nc); err != nil {
					return nil, err
				}
				if nc != nil {
					page.nextCursorInt = *nc
				}
			} else {
				var nc *string
				if err := dec.Decode(&nc); err != nil {
					return nil, err
				}
				if nc != nil {
					if len(*nc) > maxListCursorStringBytes {
						return nil, fmt.Errorf("next_cursor length %d exceeds limit %d", len(*nc), maxListCursorStringBytes)
					}
					page.nextCursorStr = *nc
				}
			}

		default:
			// Validate and discard unknown top-level envelope fields so arbitrary
			// upstream metadata cannot bloat the MCP frame beyond the budget.
			if err := skipJSONValue(dec); err != nil {
				return nil, err
			}
		}
	}

	endObj, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if delim, ok := endObj.(json.Delim); !ok || delim != '}' {
		return nil, fmt.Errorf("expected closing '}', got %v", endObj)
	}

	if err := ensureJSONEOF(dec); err != nil {
		return nil, err
	}

	if page.truncated {
		page.truncatedReason = listTruncatedReason
		page.hasMore = true
		if len(page.items) > 0 {
			page.nextCursorInt, page.nextCursorStr = computeCursorFromLastItem(cursorMode, page.items[len(page.items)-1])
		}
	} else if httpPageShrunk && page.hasMore {
		page.truncated = true
		page.truncatedReason = listHTTPTruncatedReason
		if page.nextCursorInt == 0 && page.nextCursorStr == "" && len(page.items) > 0 {
			page.nextCursorInt, page.nextCursorStr = computeCursorFromLastItem(cursorMode, page.items[len(page.items)-1])
		}
	}

	// Exact whole-output wire verification: marshal the complete typed envelope
	// and trim items if needed so the actual serialized frame never exceeds maxMCPFrameBytes.
	for {
		env := buildTypedListEnvelope(itemsKey, cursorMode, page)
		rawEnv, err := json.Marshal(env)
		if err != nil {
			return nil, err
		}
		wireSize := mcpWireFrameBytes(rawEnv)
		if wireSize <= maxMCPFrameBytes {
			return env, nil
		}
		if len(page.items) > 1 {
			page.items = page.items[:len(page.items)-1]
			page.truncated = true
			page.truncatedReason = listTruncatedReason
			page.hasMore = true
			page.nextCursorInt, page.nextCursorStr = computeCursorFromLastItem(cursorMode, page.items[len(page.items)-1])
			continue
		}
		if len(page.items) == 1 {
			stub, stubErr := projectMinimalItem(page.items[0])
			if stubErr == nil && !bytes.Equal(stub, page.items[0]) {
				page.items[0] = stub
				continue
			}
		}
		return nil, fmt.Errorf("serialized %q response (%d wire bytes) exceeds MCP frame budget (%d bytes)", itemsKey, wireSize, maxMCPFrameBytes)
	}
}

func cloneURLValues(q url.Values) url.Values {
	if len(q) == 0 {
		return url.Values{}
	}
	cp := make(url.Values, len(q))
	for k, vs := range q {
		cp[k] = append([]string(nil), vs...)
	}
	return cp
}

func (c *Client) doBudgetedList(ctx context.Context, path string, query url.Values, itemsKey string, cursorMode listCursorMode) (any, error) {
	curQuery := cloneURLValues(query)
	curLimit := defaultUpstreamListLimit
	explicitLimit := false
	if rawLim := strings.TrimSpace(curQuery.Get("limit")); rawLim != "" {
		if n, err := strconv.Atoi(rawLim); err == nil && n > 0 {
			curLimit = n
			explicitLimit = true
		}
	}
	httpPageShrunk := false

	for {
		var out any
		err := c.doJSON(ctx, http.MethodGet, path, curQuery, nil, defaultMaxListResponseBytes, func(status int, r io.Reader) error {
			if status != http.StatusOK {
				errBody, _ := io.ReadAll(io.LimitReader(r, maxErrorResponseBytes))
				return c.parseAPIError(status, errBody)
			}
			decoded, err := decodeBudgetedListWithShrink(r, itemsKey, cursorMode, c.effectiveMCPFrameBudget(), c.effectiveListTextLimit(), httpPageShrunk)
			if err != nil {
				return err
			}
			out = decoded
			return nil
		}, nil)
		if err == nil {
			return out, nil
		}
		if !errors.Is(err, errResponseTooLarge) || ctx.Err() != nil {
			return nil, err
		}
		// Preserve cursor and all filters while shrinking limit so an oversized HTTP list
		// page can still be validated to EOF and continued via next_cursor.
		if explicitLimit && curLimit <= 1 {
			return nil, err
		}
		if curLimit > maxUpstreamListLimit {
			curLimit = maxUpstreamListLimit
		}
		nextLimit := curLimit / 2
		if nextLimit < 1 {
			nextLimit = 1
		}
		if explicitLimit && nextLimit >= curLimit {
			return nil, err
		}
		curLimit = nextLimit
		explicitLimit = true
		httpPageShrunk = true
		curQuery.Set("limit", strconv.Itoa(curLimit))
	}
}

func (c *Client) doJSON(
	ctx context.Context,
	method, path string,
	query url.Values,
	body any,
	maxBytes int64,
	decode2xx func(status int, r io.Reader) error,
	onNon2xx func(status int, body []byte) error,
) error {
	endpoint := c.baseURL + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}

	var reqBody io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal request body: %w", err)
		}
		reqBody = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, method, endpoint, reqBody)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request %s %s failed: %s", method, path, c.sanitizeErrorText(err.Error()))
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBytes, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorResponseBytes))
		if err != nil {
			return fmt.Errorf("read response from %s %s: %s", method, path, c.sanitizeErrorText(err.Error()))
		}
		if onNon2xx != nil {
			return onNon2xx(resp.StatusCode, respBytes)
		}
		return c.parseAPIError(resp.StatusCode, respBytes)
	}

	limit := c.effectiveResponseLimit(maxBytes)
	mr := &maxBytesReader{r: resp.Body, remain: limit}
	if err := decode2xx(resp.StatusCode, mr); err != nil {
		if mr.exceeded || errors.Is(err, errResponseTooLarge) {
			return &responseTooLargeError{method: method, path: path, limit: limit}
		}
		var apiErr *APIError
		if errors.As(err, &apiErr) {
			return apiErr
		}
		return fmt.Errorf("decode response from %s %s: %s", method, path, c.sanitizeErrorText(err.Error()))
	}
	return nil
}

func (c *Client) doJSONObject(ctx context.Context, method, path string, query url.Values, body any, maxBytes int64) (map[string]any, error) {
	var out map[string]any
	err := c.doJSON(ctx, method, path, query, body, maxBytes, func(status int, r io.Reader) error {
		if status != http.StatusOK {
			errBody, _ := io.ReadAll(io.LimitReader(r, maxErrorResponseBytes))
			return c.parseAPIError(status, errBody)
		}
		return decodeJSONStream(r, &out)
	}, nil)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Client) parseAPIError(status int, body []byte) error {
	// Never echo upstream response bodies for authentication/authorization failures. The
	// only detail taken from a 403 is required_scope, and only if it is a known scope name.
	switch status {
	case http.StatusUnauthorized:
		return &APIError{StatusCode: status, Message: "unauthorized"}
	case http.StatusForbidden:
		var denied struct {
			RequiredScope string `json:"required_scope"`
		}
		if json.Unmarshal(body, &denied) == nil && config.ValidAPIScope(denied.RequiredScope) {
			return &APIError{StatusCode: status, Message: fmt.Sprintf("forbidden: missing required scope %q", denied.RequiredScope)}
		}
		return &APIError{StatusCode: status, Message: "forbidden"}
	}

	var envelope struct {
		Error         string `json:"error"`
		Reason        string `json:"reason"`
		CurrentStatus string `json:"current_status"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil {
		msg := strings.TrimSpace(envelope.Error)
		if msg == "" {
			msg = strings.TrimSpace(envelope.Reason)
		}
		msg = c.sanitizeErrorText(msg)
		if msg != "" {
			cs := strings.TrimSpace(envelope.CurrentStatus)
			if cs != "" && safeStatusRe.MatchString(cs) && (c.token == "" || !strings.Contains(cs, c.token)) && !strings.Contains(msg, cs) {
				msg = fmt.Sprintf("%s (current_status=%s)", msg, cs)
			}
			return &APIError{StatusCode: status, Message: msg}
		}
	}

	// Do not echo non-JSON (plain text / HTML) upstream or proxy bodies.
	return &APIError{StatusCode: status, Message: safeStatusMessage(status)}
}

func safeStatusMessage(status int) string {
	if text := strings.TrimSpace(http.StatusText(status)); text != "" {
		return text
	}
	return "unexpected error"
}

func (c *Client) sanitizeErrorText(s string) string {
	if s == "" {
		return ""
	}
	if c.token != "" {
		s = strings.ReplaceAll(s, c.token, "[REDACTED]")
	}
	s = authHeaderRe.ReplaceAllString(s, "Authorization: [REDACTED]")
	s = bearerTokenRe.ReplaceAllString(s, "Bearer [REDACTED]")
	s = basicTokenRe.ReplaceAllString(s, "Basic [REDACTED]")

	var b strings.Builder
	b.Grow(len(s))
	lastSpace := false
	for _, r := range s {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			if !lastSpace {
				b.WriteByte(' ')
				lastSpace = true
			}
			continue
		}
		b.WriteRune(r)
		lastSpace = false
	}
	out := strings.TrimSpace(b.String())
	runes := []rune(out)
	if len(runes) > maxErrorMessageRunes {
		out = string(runes[:maxErrorMessageRunes]) + "..."
	}
	return out
}

// WorktreeSpec defines optional git worktree creation parameters for a job.
type WorktreeSpec struct {
	RepoPath string `json:"repo_path" jsonschema:"Path to the base git repository on the worker host"`
	BaseRef  string `json:"base_ref,omitempty" jsonschema:"Base git ref to branch from"`
	Branch   string `json:"branch,omitempty" jsonschema:"Target branch name for the worktree"`
}

// CreateJobInput is the input schema for the create_job MCP tool.
type CreateJobInput struct {
	Title              string        `json:"title" jsonschema:"Job title (required)"`
	RequestID          string        `json:"request_id,omitempty" jsonschema:"Idempotency key; auto-generated if omitted"`
	Draft              bool          `json:"draft,omitempty" jsonschema:"Create job in draft status instead of queued"`
	Agent              string        `json:"agent,omitempty" jsonschema:"Agent name (e.g. claude, codex, agy)"`
	Host               string        `json:"host,omitempty" jsonschema:"Target worker host name"`
	Workdir            string        `json:"workdir,omitempty" jsonschema:"Working directory on the worker host"`
	WorktreeSpec       *WorktreeSpec `json:"worktree_spec,omitempty" jsonschema:"Optional git worktree specification"`
	TargetRef          string        `json:"target_ref,omitempty" jsonschema:"Target git ref or branch"`
	Instructions       string        `json:"instructions,omitempty" jsonschema:"Task instructions for the worker agent"`
	AcceptanceCriteria string        `json:"acceptance_criteria,omitempty" jsonschema:"Acceptance criteria evaluated upon completion"`
	ParentID           *int64        `json:"parent_id,omitempty" jsonschema:"Parent job ID for chaining"`
	Model              string        `json:"model,omitempty" jsonschema:"Model override"`
	Pool               string        `json:"pool,omitempty" jsonschema:"Primary quota pool name"`
	AltPools           []string      `json:"alt_pools,omitempty" jsonschema:"Alternative quota pool names"`
	SessionID          string        `json:"session_id,omitempty" jsonschema:"Session identifier"`
}

// CreateJobOutput is the structured result returned by create_job.
// HTTP 202 (Quota Gate hold) is returned as a normal result with Held=true, HeldID, and Reason.
type CreateJobOutput struct {
	Held            bool           `json:"held"`
	Created         bool           `json:"created"`
	Job             map[string]any `json:"job,omitempty"`
	HeldID          string         `json:"held_id,omitempty"`
	Reason          string         `json:"reason,omitempty"`
	SnapshotSummary string         `json:"snapshot_summary,omitempty"`
	HeldJob         map[string]any `json:"held_job,omitempty"`
}

func (c *Client) CreateJob(ctx context.Context, in CreateJobInput) (*CreateJobOutput, error) {
	if strings.TrimSpace(in.Title) == "" {
		return nil, errors.New("title is required")
	}
	if strings.TrimSpace(in.RequestID) == "" {
		reqID, err := generateMCPRequestID()
		if err != nil {
			return nil, fmt.Errorf("generate request_id: %w", err)
		}
		in.RequestID = reqID
	}

	var result *CreateJobOutput
	err := c.doJSON(ctx, http.MethodPost, "/jobs", nil, in, defaultMaxSingleResponseBytes, func(status int, r io.Reader) error {
		switch status {
		case http.StatusOK, http.StatusCreated:
			var resp struct {
				Job     map[string]any `json:"job"`
				Created bool           `json:"created"`
			}
			if err := decodeJSONStream(r, &resp); err != nil {
				return err
			}
			result = &CreateJobOutput{
				Held:    false,
				Created: resp.Created,
				Job:     resp.Job,
			}
			return nil

		case http.StatusAccepted:
			var resp struct {
				HeldID  string         `json:"held_id"`
				Held    map[string]any `json:"held"`
				Created bool           `json:"created"`
			}
			if err := decodeJSONStream(r, &resp); err != nil {
				return err
			}
			var reason, snapshotSummary string
			if resp.Held != nil {
				if v, ok := resp.Held["reason"].(string); ok {
					reason = v
				}
				if s, ok := resp.Held["snapshot_summary"].(string); ok {
					snapshotSummary = s
				}
			}
			result = &CreateJobOutput{
				Held:            true,
				Created:         resp.Created,
				HeldID:          resp.HeldID,
				Reason:          reason,
				SnapshotSummary: snapshotSummary,
				HeldJob:         resp.Held,
			}
			return nil

		default:
			errBody, _ := io.ReadAll(io.LimitReader(r, maxErrorResponseBytes))
			return c.parseAPIError(status, errBody)
		}
	}, nil)
	if err != nil {
		return nil, err
	}
	return result, nil
}

// ListJobsInput is the input schema for the list_jobs MCP tool.
type ListJobsInput struct {
	Status   string `json:"status" jsonschema:"Job status filter (required): draft, queued, claimed, waiting, done, failed, or cancelled"`
	Agent    string `json:"agent,omitempty" jsonschema:"Filter by agent name"`
	Host     string `json:"host,omitempty" jsonschema:"Filter by worker host name"`
	Query    string `json:"q,omitempty" jsonschema:"Substring filter on job title"`
	ParentID *int64 `json:"parent_id,omitempty" jsonschema:"Filter by parent job ID"`
	Cursor   *int64 `json:"cursor,omitempty" jsonschema:"Pagination cursor (job ID)"`
	Limit    *int   `json:"limit,omitempty" jsonschema:"Maximum number of jobs to return"`
	Desc     *bool  `json:"desc,omitempty" jsonschema:"Order descending by job ID"`
}

func (c *Client) ListJobs(ctx context.Context, in ListJobsInput) (*ListJobsResult, error) {
	st := strings.TrimSpace(in.Status)
	if st == "" {
		return nil, errors.New("status is required")
	}
	q := url.Values{}
	q.Set("status", st)
	if v := strings.TrimSpace(in.Agent); v != "" {
		q.Set("agent", v)
	}
	if v := strings.TrimSpace(in.Host); v != "" {
		q.Set("host", v)
	}
	if v := strings.TrimSpace(in.Query); v != "" {
		q.Set("q", v)
	}
	if in.ParentID != nil {
		q.Set("parent_id", strconv.FormatInt(*in.ParentID, 10))
	}
	if in.Cursor != nil {
		q.Set("cursor", strconv.FormatInt(*in.Cursor, 10))
	}
	if in.Limit != nil {
		q.Set("limit", strconv.Itoa(*in.Limit))
	}
	if in.Desc != nil {
		q.Set("desc", strconv.FormatBool(*in.Desc))
	}

	raw, err := c.doBudgetedList(ctx, "/jobs", q, "jobs", listCursorInt64ID)
	if err != nil {
		return nil, err
	}
	return raw.(*ListJobsResult), nil
}

// JobIDInput is the input schema for job-ID-only tools (get_job, requeue_job, mark_delivered).
type JobIDInput struct {
	ID int64 `json:"id" jsonschema:"Job ID (positive integer)"`
}

func (c *Client) GetJob(ctx context.Context, id int64) (map[string]any, error) {
	if id <= 0 {
		return nil, fmt.Errorf("invalid job id %d: must be positive", id)
	}
	return c.doJSONObject(ctx, http.MethodGet, fmt.Sprintf("/jobs/%d", id), nil, nil, defaultMaxSingleResponseBytes)
}

// CancelJobInput is the input schema for the cancel_job MCP tool.
type CancelJobInput struct {
	ID     int64  `json:"id" jsonschema:"Job ID to cancel (must be in draft, queued, or waiting status)"`
	Reason string `json:"reason,omitempty" jsonschema:"Optional cancellation reason"`
}

func (c *Client) CancelJob(ctx context.Context, in CancelJobInput) (map[string]any, error) {
	if in.ID <= 0 {
		return nil, fmt.Errorf("invalid job id %d: must be positive", in.ID)
	}
	var payload any
	if r := strings.TrimSpace(in.Reason); r != "" {
		payload = map[string]string{"reason": r}
	}
	return c.doJSONObject(ctx, http.MethodPost, fmt.Sprintf("/jobs/%d/cancel", in.ID), nil, payload, defaultMaxSingleResponseBytes)
}

func (c *Client) RequeueJob(ctx context.Context, id int64) (map[string]any, error) {
	if id <= 0 {
		return nil, fmt.Errorf("invalid job id %d: must be positive", id)
	}
	return c.doJSONObject(ctx, http.MethodPost, fmt.Sprintf("/jobs/%d/requeue", id), nil, nil, defaultMaxSingleResponseBytes)
}

func (c *Client) MarkDelivered(ctx context.Context, id int64) (map[string]any, error) {
	if id <= 0 {
		return nil, fmt.Errorf("invalid job id %d: must be positive", id)
	}
	return c.doJSONObject(ctx, http.MethodPost, fmt.Sprintf("/jobs/%d/deliver", id), nil, nil, defaultMaxSingleResponseBytes)
}

// ListHeldInput is the input schema for the list_held MCP tool.
type ListHeldInput struct {
	Status string `json:"status,omitempty" jsonschema:"Held job status filter: held, released, cancelled, or all (default: held)"`
	Cursor string `json:"cursor,omitempty" jsonschema:"Pagination cursor from previous response's next_cursor"`
	Limit  *int   `json:"limit,omitempty" jsonschema:"Maximum number of held jobs to return"`
}

func (c *Client) ListHeld(ctx context.Context, in ListHeldInput) (*ListHeldResult, error) {
	q := url.Values{}
	if v := strings.TrimSpace(in.Status); v != "" {
		q.Set("status", v)
	}
	if v := strings.TrimSpace(in.Cursor); v != "" {
		q.Set("cursor", v)
	}
	if in.Limit != nil {
		q.Set("limit", strconv.Itoa(*in.Limit))
	}
	raw, err := c.doBudgetedList(ctx, "/gate/held", q, "held", listCursorHeldCreatedID)
	if err != nil {
		return nil, err
	}
	return raw.(*ListHeldResult), nil
}

// HeldIDInput is the input schema for held-ID-only tools (release_held, cancel_held).
type HeldIDInput struct {
	ID string `json:"id" jsonschema:"Held job ID"`
}

// ReleaseHeldOutput is the structured result returned by release_held.
type ReleaseHeldOutput struct {
	Released bool   `json:"released"`
	JobID    int64  `json:"job_id,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

func (c *Client) ReleaseHeld(ctx context.Context, id string) (*ReleaseHeldOutput, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, errors.New("held job id is required")
	}
	path := "/gate/held/" + url.PathEscape(id) + "/release"
	var result *ReleaseHeldOutput
	err := c.doJSON(ctx, http.MethodPost, path, nil, nil, defaultMaxSingleResponseBytes, func(status int, r io.Reader) error {
		if status != http.StatusOK {
			errBody, _ := io.ReadAll(io.LimitReader(r, maxErrorResponseBytes))
			return c.parseAPIError(status, errBody)
		}
		var out ReleaseHeldOutput
		if err := decodeJSONStream(r, &out); err != nil {
			return err
		}
		result = &out
		return nil
	}, func(status int, body []byte) error {
		if status == http.StatusConflict {
			var out ReleaseHeldOutput
			if err := json.Unmarshal(body, &out); err == nil && !out.Released {
				out.Reason = c.sanitizeErrorText(out.Reason)
				if out.Reason != "" {
					result = &out
					return nil
				}
			}
		}
		return c.parseAPIError(status, body)
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (c *Client) CancelHeld(ctx context.Context, id string) (map[string]any, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, errors.New("held job id is required")
	}
	path := "/gate/held/" + url.PathEscape(id)
	return c.doJSONObject(ctx, http.MethodDelete, path, nil, nil, defaultMaxSingleResponseBytes)
}

// GateDecisionsInput is the input schema for the gate_decisions MCP tool.
type GateDecisionsInput struct {
	JobID  *int64 `json:"job_id,omitempty" jsonschema:"Filter by job ID"`
	HeldID string `json:"held_id,omitempty" jsonschema:"Filter by held job ID"`
	Cursor *int64 `json:"cursor,omitempty" jsonschema:"Pagination cursor (decision ID from previous response's next_cursor)"`
	Limit  *int   `json:"limit,omitempty" jsonschema:"Maximum number of decisions to return"`
}

func (c *Client) GateDecisions(ctx context.Context, in GateDecisionsInput) (*GateDecisionsResult, error) {
	q := url.Values{}
	if in.JobID != nil {
		q.Set("job_id", strconv.FormatInt(*in.JobID, 10))
	}
	if v := strings.TrimSpace(in.HeldID); v != "" {
		q.Set("held_id", v)
	}
	if in.Cursor != nil {
		q.Set("cursor", strconv.FormatInt(*in.Cursor, 10))
	}
	if in.Limit != nil {
		q.Set("limit", strconv.Itoa(*in.Limit))
	}
	raw, err := c.doBudgetedList(ctx, "/gate/decisions", q, "decisions", listCursorInt64ID)
	if err != nil {
		return nil, err
	}
	return raw.(*GateDecisionsResult), nil
}

// ListDeliveriesInput is the input schema for the list_deliveries MCP tool.
type ListDeliveriesInput struct {
	Status string `json:"status,omitempty" jsonschema:"Delivery status filter: undelivered, delivered, or all (default: undelivered)"`
	Cursor string `json:"cursor,omitempty" jsonschema:"Pagination cursor from previous response's next_cursor"`
	Limit  *int   `json:"limit,omitempty" jsonschema:"Maximum number of deliveries to return"`
}

func (c *Client) ListDeliveries(ctx context.Context, in ListDeliveriesInput) (*ListDeliveriesResult, error) {
	q := url.Values{}
	status := strings.TrimSpace(in.Status)
	if status != "" {
		q.Set("status", status)
	}
	if v := strings.TrimSpace(in.Cursor); v != "" {
		q.Set("cursor", v)
	}
	if in.Limit != nil {
		q.Set("limit", strconv.Itoa(*in.Limit))
	}
	cursorMode := listCursorDeliveryUpdatedID
	if status == "delivered" {
		cursorMode = listCursorDeliveryDeliveredID
	}
	raw, err := c.doBudgetedList(ctx, "/deliveries", q, "jobs", cursorMode)
	if err != nil {
		return nil, err
	}
	return raw.(*ListDeliveriesResult), nil
}

// FleetStatusInput is the empty input schema for the fleet_status MCP tool.
type FleetStatusInput struct{}

func (c *Client) FleetStatus(ctx context.Context) (map[string]any, error) {
	return c.doJSONObject(ctx, http.MethodGet, "/status", nil, nil, defaultMaxSingleResponseBytes)
}

func generateMCPRequestID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "mcp-" + hex.EncodeToString(b[:]), nil
}

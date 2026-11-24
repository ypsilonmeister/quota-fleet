package mcpserver

import (
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// inProcessBaseURL is a placeholder origin; requests never leave the process.
const inProcessBaseURL = "http://quota-fleet.invalid"

// handlerTransport dispatches HTTP requests straight into an http.Handler.
type handlerTransport struct{ h http.Handler }

func (t handlerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	t.h.ServeHTTP(rec, req)
	return rec.Result(), nil
}

// NewHTTPHandler returns the Streamable HTTP MCP handler (docs/13). backend is the
// authenticated quota-fleet API handler. A new MCP server is built per request and its
// API client is bound to that request's Bearer token, so scope enforcement and
// created_by come from the API's normal path for each caller. Sessions are stateless.
// Requests carrying an Origin header are rejected with 403 unless its host is loopback
// or listed in allowedOriginHosts.
func NewHTTPHandler(backend http.Handler, allowedOriginHosts []string) http.Handler {
	httpClient := &http.Client{Transport: handlerTransport{h: backend}, Timeout: defaultHTTPTimeout}
	streamable := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		token := bearerToken(r.Header.Get("Authorization"))
		client, err := NewClient(inProcessBaseURL, token, httpClient)
		if err != nil {
			return nil
		}
		return newServerWithClient(client, "")
	}, &mcp.StreamableHTTPOptions{Stateless: true})
	return checkOrigin(streamable, allowedOriginHosts)
}

// checkOrigin is the DNS rebinding defence required by docs/13 §3. A request without an
// Origin header (non-browser clients) passes. http.CrossOriginProtection is not enough
// here: it accepts any Origin that matches Host, which is what a rebound page sends.
func checkOrigin(next http.Handler, allowedHosts []string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origins := r.Header.Values("Origin")
		if len(origins) > 1 || (len(origins) == 1 && !originAllowed(origins[0], allowedHosts)) {
			http.Error(w, "Forbidden: origin not allowed", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// originAllowed reports whether origin is an http(s) origin on a loopback or allowed host.
// Hosts are compared exactly (case-insensitively); any port is accepted.
func originAllowed(origin string, allowedHosts []string) bool {
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") ||
		u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	host := u.Hostname()
	if host == "" {
		return false
	}
	if ip := net.ParseIP(host); strings.EqualFold(host, "localhost") || (ip != nil && ip.IsLoopback()) {
		return true
	}
	return slices.ContainsFunc(allowedHosts, func(allowed string) bool {
		return strings.EqualFold(host, allowed)
	})
}

func bearerToken(header string) string {
	scheme, tok, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(tok)
}

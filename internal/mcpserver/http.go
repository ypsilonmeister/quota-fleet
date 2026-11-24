package mcpserver

import (
	"net/http"
	"net/http/httptest"
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
func NewHTTPHandler(backend http.Handler) http.Handler {
	httpClient := &http.Client{Transport: handlerTransport{h: backend}, Timeout: defaultHTTPTimeout}
	return mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		token := bearerToken(r.Header.Get("Authorization"))
		client, err := NewClient(inProcessBaseURL, token, httpClient)
		if err != nil {
			return nil
		}
		return newServerWithClient(client, "")
	}, &mcp.StreamableHTTPOptions{Stateless: true})
}

func bearerToken(header string) string {
	scheme, tok, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(tok)
}

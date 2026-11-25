package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/mcpserver"
)

const (
	mcpFormatAll        = "all"
	mcpFormatClaudeCode = "claude-code"
	mcpFormatJSON       = "json"
	mcpFormatHTTP       = "http"

	mcpTokenEnvRef = "${QUOTA_FLEET_TOKEN}"
)

func runMCP(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return runMCPServeContext(ctx, &mcp.StdioTransport{}, stderr)
	}

	switch args[0] {
	case "config":
		return runMCPConfig(args[1:], stdout, stderr)
	case "-h", "--help", "help":
		printMCPUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown mcp subcommand %q\n\n", args[0])
		printMCPUsage(stderr)
		return 1
	}
}

func runMCPServeContext(ctx context.Context, transport mcp.Transport, stderr io.Writer) int {
	apiURL := resolveDefaultAPIURL()
	token := strings.TrimSpace(os.Getenv("QUOTA_FLEET_TOKEN"))

	srv, err := mcpserver.NewServer(mcpserver.Options{
		APIURL: apiURL,
		Token:  token,
	})
	if err != nil {
		fmt.Fprintf(stderr, "mcp server init failed: %v\n", err)
		return 1
	}

	if err := srv.Run(ctx, transport); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(stderr, "mcp server error: %v\n", err)
		return 1
	}
	return 0
}

func printMCPUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  quota-fleet mcp")
	fmt.Fprintln(w, "  quota-fleet mcp config [--format all|claude-code|json|http] [--url http://127.0.0.1:8737] [--binary quota-fleet]")
}

func resolveDefaultAPIURL() string {
	if env := strings.TrimSpace(os.Getenv("QUOTA_FLEET_API_URL")); env != "" {
		return env
	}
	return "http://" + config.DefaultAPIListen
}

func runMCPConfig(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("mcp config", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		format string
		apiURL string
		binary string
	)
	fs.StringVar(&format, "format", mcpFormatAll, "output format: all, claude-code, json, or http")
	fs.StringVar(&apiURL, "url", resolveDefaultAPIURL(), "quota-fleet HTTP API URL")
	fs.StringVar(&binary, "binary", "quota-fleet", "path or name of the quota-fleet binary")

	if err := fs.Parse(args); err != nil {
		return 1
	}

	format = strings.ToLower(strings.TrimSpace(format))
	apiURL = strings.TrimSpace(apiURL)
	binary = strings.TrimSpace(binary)

	if apiURL == "" {
		fmt.Fprintln(stderr, "mcp config failed: --url must not be empty")
		return 1
	}
	if binary == "" {
		fmt.Fprintln(stderr, "mcp config failed: --binary must not be empty")
		return 1
	}

	switch format {
	case mcpFormatAll:
		fmt.Fprintln(stdout, "# Claude Code CLI:")
		writeClaudeCodeMCPSnippet(stdout, apiURL, binary)
		fmt.Fprintln(stdout)
		fmt.Fprintln(stdout, "# Generic MCP client JSON (.mcp.json / Claude Desktop / Cursor):")
		if err := writeGenericJSONMCPSnippet(stdout, apiURL, binary); err != nil {
			fmt.Fprintf(stderr, "mcp config failed: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout)
		fmt.Fprintln(stdout, "# Streamable HTTP (requires api.mcp_http: true on the server):")
		if err := writeHTTPMCPSnippet(stdout, apiURL); err != nil {
			fmt.Fprintf(stderr, "mcp config failed: %v\n", err)
			return 1
		}
		return 0
	case mcpFormatClaudeCode:
		writeClaudeCodeMCPSnippet(stdout, apiURL, binary)
		return 0
	case mcpFormatJSON:
		if err := writeGenericJSONMCPSnippet(stdout, apiURL, binary); err != nil {
			fmt.Fprintf(stderr, "mcp config failed: %v\n", err)
			return 1
		}
		return 0
	case mcpFormatHTTP:
		if err := writeHTTPMCPSnippet(stdout, apiURL); err != nil {
			fmt.Fprintf(stderr, "mcp config failed: %v\n", err)
			return 1
		}
		return 0
	default:
		fmt.Fprintf(stderr, "mcp config failed: unsupported --format %q (allowed: all, claude-code, json, http)\n", format)
		return 1
	}
}

func writeClaudeCodeMCPSnippet(w io.Writer, apiURL, binary string) {
	fmt.Fprintf(w, "claude mcp add quota-fleet \\\n")
	fmt.Fprintf(w, "  -e QUOTA_FLEET_API_URL=%q \\\n", apiURL)
	fmt.Fprintf(w, "  -e QUOTA_FLEET_TOKEN=\"%s\" \\\n", mcpTokenEnvRef)
	fmt.Fprintf(w, "  -- %s mcp\n", binary)
}

type mcpClientConfigJSON struct {
	MCPServers map[string]mcpServerEntryJSON `json:"mcpServers"`
}

type mcpServerEntryJSON struct {
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
}

func writeGenericJSONMCPSnippet(w io.Writer, apiURL, binary string) error {
	payload := mcpClientConfigJSON{
		MCPServers: map[string]mcpServerEntryJSON{
			"quota-fleet": {
				Command: binary,
				Args:    []string{"mcp"},
				Env: map[string]string{
					"QUOTA_FLEET_API_URL": apiURL,
					"QUOTA_FLEET_TOKEN":   mcpTokenEnvRef,
				},
			},
		},
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(payload)
}

type mcpHTTPClientConfigJSON struct {
	MCPServers map[string]mcpHTTPServerEntryJSON `json:"mcpServers"`
}

type mcpHTTPServerEntryJSON struct {
	Type    string            `json:"type"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
}

// writeHTTPMCPSnippet prints a Streamable HTTP client entry. The token is only an
// environment variable reference; its value is never printed.
func writeHTTPMCPSnippet(w io.Writer, apiURL string) error {
	payload := mcpHTTPClientConfigJSON{
		MCPServers: map[string]mcpHTTPServerEntryJSON{
			"quota-fleet": {
				Type:    "http",
				URL:     strings.TrimRight(apiURL, "/") + "/mcp",
				Headers: map[string]string{"Authorization": "Bearer " + mcpTokenEnvRef},
			},
		},
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(payload)
}

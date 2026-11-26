package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// chain commands are a thin HTTP layer over /chains (docs/22 §9). They talk to a running
// `quota-fleet serve` so registration goes through the same admission and authorization.

func runChain(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printChainUsage(stderr)
		return 1
	}
	switch args[0] {
	case "create":
		return runChainCreate(args[1:], stdout, stderr)
	case "list":
		return runChainList(args[1:], stdout, stderr)
	case "show":
		return runChainShow(args[1:], stdout, stderr)
	case "cancel":
		return runChainCancel(args[1:], stdout, stderr)
	case "-h", "--help", "help":
		printChainUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown chain subcommand %q\n\n", args[0])
		printChainUsage(stderr)
		return 1
	}
}

func printChainUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage (talks to the HTTP API; token from $QUOTA_FLEET_TOKEN, URL from $QUOTA_FLEET_API_URL or --url):")
	fmt.Fprintln(w, "  quota-fleet chain create -f definition.(yaml|json) [--url URL]")
	fmt.Fprintln(w, "  quota-fleet chain list [--status running|completed|stopped|cancelled] [--scope S] [--limit N] [--cursor C] [--url URL]")
	fmt.Fprintln(w, "  quota-fleet chain show <run_id> [--url URL]")
	fmt.Fprintln(w, "  quota-fleet chain cancel <run_id> [--reason ...] [--url URL]")
}

type chainClient struct {
	base  string
	token string
	http  *http.Client
}

func newChainFlagSet(name string, stderr io.Writer) (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	u := fs.String("url", resolveDefaultAPIURL(), "quota-fleet HTTP API URL")
	return fs, u
}

func newChainClient(apiURL string) *chainClient {
	return &chainClient{
		base:  strings.TrimRight(strings.TrimSpace(apiURL), "/"),
		token: strings.TrimSpace(os.Getenv("QUOTA_FLEET_TOKEN")),
		http:  &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *chainClient) do(method, path string, body []byte, stdout, stderr io.Writer) int {
	req, err := http.NewRequestWithContext(context.Background(), method, c.base+path, bytes.NewReader(body))
	if err != nil {
		fmt.Fprintf(stderr, "chain: %v\n", err)
		return 1
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		fmt.Fprintf(stderr, "chain: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	var pretty bytes.Buffer
	if json.Indent(&pretty, data, "", "  ") == nil {
		data = append(pretty.Bytes(), '\n')
	}
	if resp.StatusCode >= 300 {
		fmt.Fprintf(stderr, "chain: HTTP %d\n%s", resp.StatusCode, data)
		return 1
	}
	_, _ = stdout.Write(data)
	return 0
}

// definitionJSON reads a YAML or JSON definition file and returns JSON.
func definitionJSON(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && trimmed[0] == '{' {
		return raw, nil
	}
	var v any
	if err := yaml.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return json.Marshal(v)
}

func runChainCreate(args []string, stdout, stderr io.Writer) int {
	fs, apiURL := newChainFlagSet("chain create", stderr)
	file := fs.String("f", "", "definition file (YAML or JSON)")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if strings.TrimSpace(*file) == "" {
		fmt.Fprintln(stderr, "chain create failed: -f is required")
		return 1
	}
	body, err := definitionJSON(*file)
	if err != nil {
		fmt.Fprintf(stderr, "chain create failed: %v\n", err)
		return 1
	}
	return newChainClient(*apiURL).do(http.MethodPost, "/chains", body, stdout, stderr)
}

func runChainList(args []string, stdout, stderr io.Writer) int {
	fs, apiURL := newChainFlagSet("chain list", stderr)
	status := fs.String("status", "", "status filter")
	scope := fs.String("scope", "", "scope filter")
	limit := fs.Int("limit", 0, "page size")
	cursor := fs.String("cursor", "", "cursor from a previous page")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	q := url.Values{}
	if *status != "" {
		q.Set("status", *status)
	}
	if *scope != "" {
		q.Set("scope", *scope)
	}
	if *limit > 0 {
		q.Set("limit", fmt.Sprint(*limit))
	}
	if *cursor != "" {
		q.Set("cursor", *cursor)
	}
	path := "/chains"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	return newChainClient(*apiURL).do(http.MethodGet, path, nil, stdout, stderr)
}

func chainIDArg(fs *flag.FlagSet, args []string, stderr io.Writer) (string, bool) {
	var id string
	rest := args
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		id, rest = rest[0], rest[1:]
	}
	if err := fs.Parse(rest); err != nil {
		return "", false
	}
	if id == "" && fs.NArg() > 0 {
		id = fs.Arg(0)
	}
	if strings.TrimSpace(id) == "" {
		fmt.Fprintf(stderr, "%s failed: run id is required\n", fs.Name())
		return "", false
	}
	return url.PathEscape(strings.TrimSpace(id)), true
}

func runChainShow(args []string, stdout, stderr io.Writer) int {
	fs, apiURL := newChainFlagSet("chain show", stderr)
	id, ok := chainIDArg(fs, args, stderr)
	if !ok {
		return 1
	}
	return newChainClient(*apiURL).do(http.MethodGet, "/chains/"+id, nil, stdout, stderr)
}

func runChainCancel(args []string, stdout, stderr io.Writer) int {
	fs, apiURL := newChainFlagSet("chain cancel", stderr)
	reason := fs.String("reason", "", "reason recorded on the run")
	id, ok := chainIDArg(fs, args, stderr)
	if !ok {
		return 1
	}
	body, _ := json.Marshal(map[string]string{"reason": *reason})
	return newChainClient(*apiURL).do(http.MethodPost, "/chains/"+id+"/cancel", body, stdout, stderr)
}

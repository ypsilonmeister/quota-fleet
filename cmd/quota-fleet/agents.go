package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/adapter"
	"github.com/ypsilonmeister/quota-fleet/internal/agentver"
	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/runtime"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

func runAgents(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		switch args[0] {
		case "check":
			return runAgentsCheck(args[1:], stdout, stderr)
		case "events":
			return runAgentsEvents(args[1:], stdout, stderr)
		case "-h", "--help", "help":
			printAgentsUsage(stdout)
			return 0
		default:
			if !strings.HasPrefix(args[0], "-") {
				fmt.Fprintf(stderr, "unknown agents subcommand %q\n\n", args[0])
				printAgentsUsage(stderr)
				return 1
			}
		}
	}
	return runAgentsList(args, stdout, stderr)
}

func printAgentsUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  quota-fleet agents [--config fleet.yaml] [--db path] [--json]")
	fmt.Fprintln(w, "  quota-fleet agents check [--config fleet.yaml] [--db path] [--timeout 15s] [--json]")
	fmt.Fprintln(w, "  quota-fleet agents events [--since RFC3339] [--agent name] [--host name] [--limit N] [--config fleet.yaml] [--db path] [--json]")
}

func runAgentsList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("agents", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		cfgPath string
		dbPath  string
		asJSON  bool
	)
	fs.StringVar(&cfgPath, "config", resolveDefaultConfigPath(), "path to fleet.yaml")
	fs.StringVar(&cfgPath, "c", resolveDefaultConfigPath(), "path to fleet.yaml (shorthand)")
	fs.StringVar(&dbPath, "db", "", "override SQLite database path")
	fs.BoolVar(&asJSON, "json", false, "output result as JSON")
	if err := fs.Parse(args); err != nil {
		return 1
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "agents failed: %v\n", err)
		return 1
	}
	if dbPath == "" {
		dbPath = cfg.DBPath()
	}

	ctx := context.Background()
	st, err := store.Open(ctx, dbPath, store.Options{InstanceID: cfg.InstanceID})
	if err != nil {
		fmt.Fprintf(stderr, "open store %q: %v\n", dbPath, err)
		return 1
	}
	defer st.Close()

	rows, err := st.ListAgentVersions(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "list agent versions: %v\n", err)
		return 1
	}

	matrix := agentver.BuildMatrix(cfg, rows)
	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(matrix); err != nil {
			fmt.Fprintf(stderr, "encode json: %v\n", err)
			return 1
		}
		return 0
	}

	printAgentMatrix(stdout, matrix)
	return 0
}

func runAgentsCheck(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("agents check", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		cfgPath    string
		dbPath     string
		timeoutRaw string
		asJSON     bool
	)
	fs.StringVar(&cfgPath, "config", resolveDefaultConfigPath(), "path to fleet.yaml")
	fs.StringVar(&cfgPath, "c", resolveDefaultConfigPath(), "path to fleet.yaml (shorthand)")
	fs.StringVar(&dbPath, "db", "", "override SQLite database path")
	fs.StringVar(&timeoutRaw, "timeout", "", "per-(host,agent) probe timeout (default 15s)")
	fs.BoolVar(&asJSON, "json", false, "output result as JSON")
	if err := fs.Parse(args); err != nil {
		return 1
	}

	var probeTimeout time.Duration
	if strings.TrimSpace(timeoutRaw) != "" {
		d, err := time.ParseDuration(strings.TrimSpace(timeoutRaw))
		if err != nil || d <= 0 {
			fmt.Fprintf(stderr, "agents check failed: invalid --timeout %q\n", timeoutRaw)
			return 1
		}
		probeTimeout = d
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "agents check failed: %v\n", err)
		return 1
	}
	if dbPath == "" {
		dbPath = cfg.DBPath()
	}

	ctx := context.Background()
	st, err := store.Open(ctx, dbPath, store.Options{InstanceID: cfg.InstanceID})
	if err != nil {
		fmt.Fprintf(stderr, "open store %q: %v\n", dbPath, err)
		return 1
	}
	defer st.Close()

	stateDir := cfg.StateDir
	if stateDir == "" {
		stateDir = filepath.Dir(dbPath)
	}
	deliveriesDir := cfg.Runtime.EffectiveDeliveriesDir(stateDir)
	logger := log.New(stderr, "quota-fleet: ", log.LstdFlags)

	fileNotifier := runtime.NewFileNotifier(deliveriesDir, st)
	var notifier runtime.Notifier = fileNotifier
	if webhookURL := cfg.Notifier.EffectiveWebhookURL(); webhookURL != "" {
		webhookNotifier := runtime.NewWebhookNotifier(runtime.WebhookNotifierOptions{
			URL:        webhookURL,
			Timeout:    cfg.Notifier.EffectiveWebhookTimeout(),
			Retries:    cfg.Notifier.EffectiveWebhookRetries(),
			RetriesSet: cfg.Notifier.WebhookRetries != nil,
			Logger:     logger,
		})
		notifier = runtime.NewMultiNotifier(fileNotifier, webhookNotifier)
	}

	sshAdapter := adapter.NewSSHAdapter(cfg, adapter.SSHAdapterOptions{
		Transport: adapter.NewRoutingTransport(
			adapter.NewSSHTransport(adapter.SSHTransportOptions{}),
			adapter.NewLocalTransport(adapter.LocalTransportOptions{}),
		),
	})

	checker := runtime.NewAgentVersionChecker(runtime.AgentVersionCheckerOptions{
		Store:        st,
		Config:       cfg,
		Provider:     sshAdapter,
		Notifier:     notifier,
		ProbeTimeout: probeTimeout,
		Logger:       logger,
	})

	res, err := checker.CheckOnce(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "agents check failed: %v\n", err)
		return 1
	}

	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(res); err != nil {
			fmt.Fprintf(stderr, "encode json: %v\n", err)
			return 1
		}
		return 0
	}

	printAgentMatrix(stdout, res.Matrix)
	if len(res.Events) > 0 {
		fmt.Fprintf(stdout, "\nEvents recorded (%d):\n", len(res.Events))
		for _, ev := range res.Events {
			fmt.Fprintf(stdout, "  [%s] %s", ev.EventType, ev.Agent)
			if ev.Host != "" {
				fmt.Fprintf(stdout, " (%s)", ev.Host)
			}
			if ev.Detail != "" {
				fmt.Fprintf(stdout, ": %s", ev.Detail)
			}
			fmt.Fprintln(stdout)
		}
	}
	return 0
}

func runAgentsEvents(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("agents events", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		cfgPath  string
		dbPath   string
		sinceRaw string
		agent    string
		host     string
		limit    int
		asJSON   bool
	)
	fs.StringVar(&cfgPath, "config", resolveDefaultConfigPath(), "path to fleet.yaml")
	fs.StringVar(&cfgPath, "c", resolveDefaultConfigPath(), "path to fleet.yaml (shorthand)")
	fs.StringVar(&dbPath, "db", "", "override SQLite database path")
	fs.StringVar(&sinceRaw, "since", "", "filter events strictly after RFC3339 timestamp")
	fs.StringVar(&agent, "agent", "", "filter by agent name")
	fs.StringVar(&host, "host", "", "filter by host name")
	fs.IntVar(&limit, "limit", 50, "maximum number of events to return")
	fs.BoolVar(&asJSON, "json", false, "output result as JSON")
	if err := fs.Parse(args); err != nil {
		return 1
	}

	var filter store.AgentVersionEventsFilter
	filter.Agent = strings.TrimSpace(agent)
	filter.Host = strings.TrimSpace(host)
	filter.Limit = limit
	if strings.TrimSpace(sinceRaw) != "" {
		t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(sinceRaw))
		if err != nil {
			fmt.Fprintf(stderr, "agents events failed: invalid --since %q (expected RFC3339)\n", sinceRaw)
			return 1
		}
		t = t.UTC()
		filter.Since = &t
	}

	var instanceID string
	if dbPath == "" {
		cfg, err := config.Load(cfgPath)
		if err != nil {
			fmt.Fprintf(stderr, "agents events failed: %v\n", err)
			return 1
		}
		dbPath = cfg.DBPath()
		instanceID = cfg.InstanceID
	}

	ctx := context.Background()
	st, err := store.Open(ctx, dbPath, store.Options{InstanceID: instanceID})
	if err != nil {
		fmt.Fprintf(stderr, "open store %q: %v\n", dbPath, err)
		return 1
	}
	defer st.Close()

	events, err := st.ListAgentVersionEvents(ctx, filter)
	if err != nil {
		fmt.Fprintf(stderr, "list agent version events: %v\n", err)
		return 1
	}

	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(map[string]any{"events": events}); err != nil {
			fmt.Fprintf(stderr, "encode json: %v\n", err)
			return 1
		}
		return 0
	}

	if len(events) == 0 {
		fmt.Fprintln(stdout, "No agent version events recorded.")
		return 0
	}
	tw := tabwriter.NewWriter(stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tTIME\tTYPE\tAGENT\tHOST\tDETAIL")
	for _, ev := range events {
		hostCol := ev.Host
		if hostCol == "" {
			hostCol = "-"
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\n",
			ev.ID, ev.CreatedAt.Format(time.RFC3339), ev.EventType, ev.Agent, hostCol, ev.Detail)
	}
	_ = tw.Flush()
	return 0
}

func printAgentMatrix(w io.Writer, m agentver.Matrix) {
	if len(m.Hosts) == 0 || len(m.Agents) == 0 {
		fmt.Fprintln(w, "No hosts or agents configured.")
		return
	}

	type pairKey struct {
		host  string
		agent string
	}
	cellByKey := make(map[pairKey]agentver.Cell, len(m.Entries))
	for _, c := range m.Entries {
		cellByKey[pairKey{host: c.Host, agent: c.Agent}] = c
	}

	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintf(tw, "HOST")
	for _, a := range m.Agents {
		fmt.Fprintf(tw, "\t%s", a)
	}
	fmt.Fprintln(tw)

	for _, h := range m.Hosts {
		fmt.Fprintf(tw, "%s", h)
		for _, a := range m.Agents {
			c := cellByKey[pairKey{host: h, agent: a}]
			fmt.Fprintf(tw, "\t%s", formatMatrixCell(c))
		}
		fmt.Fprintln(tw)
	}
	_ = tw.Flush()

	fmt.Fprintln(w)
	fmt.Fprintln(w, "Drift Summary:")
	for _, s := range m.Summaries {
		state := "OK"
		if s.Drifted {
			state = "DRIFT"
		}
		var extras []string
		if s.MaxVersion != "" {
			extras = append(extras, "max="+s.MaxVersion)
		} else if !s.Comparable && len(s.Versions) > 0 {
			extras = append(extras, "比較不能")
		}
		if len(s.LaggingHosts) > 0 {
			extras = append(extras, "lagging="+strings.Join(s.LaggingHosts, ","))
		}
		if len(s.ManualUpdateHosts) > 0 {
			extras = append(extras, "manual_update="+strings.Join(s.ManualUpdateHosts, ","))
		}
		if len(s.PinnedHosts) > 0 {
			extras = append(extras, "pinned="+strings.Join(s.PinnedHosts, ","))
		}
		if len(s.FailedHosts) > 0 {
			extras = append(extras, "failed="+strings.Join(s.FailedHosts, ","))
		}
		if len(extras) > 0 {
			fmt.Fprintf(w, "  %-12s %-5s  %s\n", s.Agent, state, strings.Join(extras, "  "))
		} else {
			fmt.Fprintf(w, "  %-12s %-5s\n", s.Agent, state)
		}
	}
	fmt.Fprintf(w, "\nNote: %s\n", m.LimitationNote)
}

func formatMatrixCell(c agentver.Cell) string {
	var base string
	switch c.Status {
	case store.AgentVersionStatusOK:
		base = c.Version
	case store.AgentVersionStatusNotFound:
		base = "not_found"
	case store.AgentVersionStatusError:
		base = "error"
	default:
		base = "-"
	}

	var tags []string
	if c.Pinned {
		if c.PinMismatch {
			tags = append(tags, "pinned="+c.Pin+"!")
		} else {
			tags = append(tags, "pinned")
		}
	}
	if c.Behind {
		tags = append(tags, "behind")
	}
	if c.ManualUpdate {
		tags = append(tags, "manual")
	}
	if len(tags) > 0 {
		return fmt.Sprintf("%s [%s]", base, strings.Join(tags, ","))
	}
	return base
}

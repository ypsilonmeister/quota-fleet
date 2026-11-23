package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

const defaultConfigPath = "fleet.yaml"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printUsage(stderr)
		return 1
	}

	switch args[0] {
	case "config":
		return runConfig(args[1:], stdout, stderr)
	case "status":
		return runStatus(args[1:], stdout, stderr)
	case "serve":
		return runServe(args[1:], stdout, stderr)
	case "job":
		return runJob(args[1:], stdout, stderr)
	case "gate":
		return runGate(args[1:], stdout, stderr)
	case "-h", "--help", "help":
		printUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n", args[0])
		printUsage(stderr)
		return 1
	}
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  quota-fleet serve [--config fleet.yaml] [--db path/to/fleet.db] [--listen 127.0.0.1:8737]")
	fmt.Fprintln(w, "  quota-fleet job <create|list|get|cancel|requeue> [options]")
	fmt.Fprintln(w, "  quota-fleet gate <check|decisions> [options]")
	fmt.Fprintln(w, "  quota-fleet config check [--config fleet.yaml]")
	fmt.Fprintln(w, "  quota-fleet status [--config fleet.yaml] [--db path/to/fleet.db]")
}

func resolveDefaultConfigPath() string {
	if env := os.Getenv("QUOTA_FLEET_CONFIG"); env != "" {
		return env
	}
	return defaultConfigPath
}

func runConfig(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "check" {
		fmt.Fprintln(stderr, "Usage: quota-fleet config check [--config fleet.yaml]")
		return 1
	}

	fs := flag.NewFlagSet("config check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var cfgPath string
	fs.StringVar(&cfgPath, "config", resolveDefaultConfigPath(), "path to fleet.yaml")
	fs.StringVar(&cfgPath, "c", resolveDefaultConfigPath(), "path to fleet.yaml (shorthand)")
	if err := fs.Parse(args[1:]); err != nil {
		return 1
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "config check failed: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "config OK: %s\n", cfgPath)
	fmt.Fprintf(stdout, "  instance_id:  %s\n", cfg.InstanceID)
	fmt.Fprintf(stdout, "  state_dir:    %s\n", cfg.StateDir)
	fmt.Fprintf(stdout, "  limits:       max_claimed=%d attempts_cap=%d\n", cfg.Limits.MaxClaimed, cfg.Limits.AttemptsCap)
	fmt.Fprintf(stdout, "  hosts:        %d\n", len(cfg.Hosts))
	fmt.Fprintf(stdout, "  agents:       %d\n", len(cfg.Agents))
	fmt.Fprintf(stdout, "  pools:        %d\n", len(cfg.Pools))
	return 0
}

func runStatus(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		cfgPath string
		dbPath  string
	)
	fs.StringVar(&cfgPath, "config", resolveDefaultConfigPath(), "path to fleet.yaml")
	fs.StringVar(&cfgPath, "c", resolveDefaultConfigPath(), "path to fleet.yaml (shorthand)")
	fs.StringVar(&dbPath, "db", "", "override SQLite database path")
	if err := fs.Parse(args); err != nil {
		return 1
	}

	var instanceID string
	if dbPath == "" {
		cfg, err := config.Load(cfgPath)
		if err != nil {
			fmt.Fprintf(stderr, "status failed: %v\n", err)
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

	if instanceID != "" {
		if err := st.VerifyInstanceID(ctx, instanceID); err != nil && !errors.Is(err, store.ErrNotFound) {
			fmt.Fprintf(stderr, "instance_id check failed: %v\n", err)
			return 1
		}
	}

	counts, err := st.CountByStatus(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "count jobs: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "store: %s\n", dbPath)
	if instanceID != "" {
		fmt.Fprintf(stdout, "instance_id: %s\n", instanceID)
	}
	var total int64
	fmt.Fprintln(stdout, "jobs:")
	for _, status := range store.AllStatuses {
		n := counts[status]
		total += n
		fmt.Fprintf(stdout, "  %-10s %d\n", status, n)
	}
	fmt.Fprintf(stdout, "  %-10s %d\n", "total", total)
	return 0
}

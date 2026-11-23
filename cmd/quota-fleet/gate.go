package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/quota"
)

func runGate(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printGateUsage(stderr)
		return 1
	}

	switch args[0] {
	case "check":
		return runGateCheck(args[1:], stdout, stderr)
	case "decisions":
		return runGateDecisions(args[1:], stdout, stderr)
	case "-h", "--help", "help":
		printGateUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown gate subcommand %q\n\n", args[0])
		printGateUsage(stderr)
		return 1
	}
}

func printGateUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  quota-fleet gate check --pool <name> [--alt-pools a,b] [--usage-file path] [--stale-after 45m] [--json] [--config fleet.yaml]")
	fmt.Fprintln(w, "  quota-fleet gate decisions [--limit N] [--db path] [--config fleet.yaml]")
}

func runGateCheck(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("gate check", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		cfgPath       string
		usageFileFlag string
		staleAfterRaw string
		pool          string
		altPoolsRaw   string
		asJSON        bool
	)
	fs.StringVar(&cfgPath, "config", resolveDefaultConfigPath(), "path to fleet.yaml")
	fs.StringVar(&cfgPath, "c", resolveDefaultConfigPath(), "path to fleet.yaml (shorthand)")
	fs.StringVar(&usageFileFlag, "usage-file", "", "override usage.json path")
	fs.StringVar(&staleAfterRaw, "stale-after", "", "override staleness duration (e.g. 45m)")
	fs.StringVar(&pool, "pool", "", "primary quota pool name (required)")
	fs.StringVar(&altPoolsRaw, "alt-pools", "", "comma-separated alternative pool names")
	fs.BoolVar(&asJSON, "json", false, "output result as JSON")

	if err := fs.Parse(args); err != nil {
		return 1
	}

	pool = strings.TrimSpace(pool)
	if pool == "" {
		fmt.Fprintln(stderr, "gate check failed: --pool is required")
		return 1
	}

	var (
		usageFile  = strings.TrimSpace(usageFileFlag)
		staleAfter = config.DefaultQuotaStaleAfter
		poolsCfg   map[string]config.PoolConfig
	)

	if cfg, err := config.Load(cfgPath); err == nil {
		poolsCfg = cfg.Pools
		staleAfter = cfg.Quota.EffectiveStaleAfter()
		if usageFile == "" {
			usageFile = strings.TrimSpace(cfg.Quota.UsageFile)
		}
	} else {
		// Allow standalone checks with --usage-file when the default fleet.yaml does not exist.
		if usageFile == "" || !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(stderr, "gate check failed: %v\n", err)
			return 1
		}
	}

	if strings.TrimSpace(staleAfterRaw) != "" {
		d, err := time.ParseDuration(strings.TrimSpace(staleAfterRaw))
		if err != nil || d <= 0 {
			fmt.Fprintf(stderr, "gate check failed: invalid --stale-after %q\n", staleAfterRaw)
			return 1
		}
		staleAfter = d
	}

	altPools := parseCommaList(altPoolsRaw)
	eval, evalErr := quota.EvaluateFromFile(usageFile, pool, altPools, quota.EvalOptions{
		StaleAfter: staleAfter,
		Pools:      poolsCfg,
	})

	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(eval); err != nil {
			fmt.Fprintf(stderr, "gate check failed: encode json: %v\n", err)
			return 1
		}
		return 0
	}

	fmt.Fprintf(stdout, "decision:      %s\n", eval.Decision)
	fmt.Fprintf(stdout, "pool:          %s\n", eval.RequestedPool)
	if len(eval.AltPools) > 0 {
		fmt.Fprintf(stdout, "alt_pools:     %s\n", strings.Join(eval.AltPools, ", "))
	}
	if eval.SelectedPool != "" {
		fmt.Fprintf(stdout, "selected_pool: %s\n", eval.SelectedPool)
	}
	if eval.Bypassed {
		fmt.Fprintln(stdout, "bypassed:      true")
	}
	if eval.BlockedBy != "" {
		fmt.Fprintf(stdout, "blocked_by:    %s\n", eval.BlockedBy)
	}
	if eval.Reason != "" {
		fmt.Fprintf(stdout, "reason:        %s\n", eval.Reason)
	}
	if eval.MeasuredAt != nil {
		fmt.Fprintf(stdout, "measured_at:   %s\n", eval.MeasuredAt.Format(time.RFC3339))
	}
	fmt.Fprintf(stdout, "decided_at:    %s\n", eval.DecidedAt.Format(time.RFC3339))
	if evalErr != nil {
		fmt.Fprintf(stdout, "error:         %v\n", evalErr)
	}
	if len(eval.Pools) > 0 {
		fmt.Fprintln(stdout, "candidates:")
		for _, p := range eval.Pools {
			var parts []string
			parts = append(parts, "status="+string(p.Status))
			if p.BlockedBy != "" {
				parts = append(parts, "blocked_by="+p.BlockedBy)
			}
			if p.Reason != "" {
				parts = append(parts, "reason="+p.Reason)
			}
			if p.Snapshot != nil {
				parts = append(parts, fmt.Sprintf("weekly=%d%%", p.Snapshot.WeeklyRemainingPercent))
				if p.Snapshot.ShortRemainingPercent != nil {
					parts = append(parts, fmt.Sprintf("short=%d%%", *p.Snapshot.ShortRemainingPercent))
				} else {
					parts = append(parts, "short=null")
				}
				if p.Snapshot.WeeklyResetAt != nil {
					parts = append(parts, "weekly_reset="+p.Snapshot.WeeklyResetAt.Format(time.RFC3339))
				}
			}
			fmt.Fprintf(stdout, "  %-12s %s\n", p.Pool, strings.Join(parts, " "))
		}
	}
	return 0
}

func runGateDecisions(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("gate decisions", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		cfgPath string
		dbPath  string
		limit   int
	)
	fs.StringVar(&cfgPath, "config", resolveDefaultConfigPath(), "path to fleet.yaml")
	fs.StringVar(&cfgPath, "c", resolveDefaultConfigPath(), "path to fleet.yaml (shorthand)")
	fs.StringVar(&dbPath, "db", "", "override SQLite database path")
	fs.IntVar(&limit, "limit", 0, "maximum number of decisions to show")

	if err := fs.Parse(args); err != nil {
		return 1
	}
	if limit < 0 {
		fmt.Fprintf(stderr, "gate decisions failed: --limit must be >= 0 (got %d)\n", limit)
		return 1
	}

	ctx := context.Background()
	st, _, err := openLocalStore(ctx, cfgPath, dbPath)
	if err != nil {
		fmt.Fprintf(stderr, "gate decisions failed: %v\n", err)
		return 1
	}
	defer st.Close()

	decisions, err := st.ListGateDecisions(ctx, limit)
	if err != nil {
		fmt.Fprintf(stderr, "gate decisions failed: %v\n", err)
		return 1
	}

	if len(decisions) == 0 {
		fmt.Fprintln(stdout, "no gate decisions recorded")
		return 0
	}

	for _, d := range decisions {
		var details []string
		if d.RequestedPool != "" {
			details = append(details, "pool="+d.RequestedPool)
		}
		if len(d.AltPools) > 0 {
			details = append(details, "alt="+strings.Join(d.AltPools, ","))
		}
		if d.SelectedPool != "" {
			details = append(details, "selected="+d.SelectedPool)
		}
		if d.BlockedBy != "" {
			details = append(details, "blocked_by="+d.BlockedBy)
		}
		if d.Bypassed {
			details = append(details, "bypassed=true")
		}
		if d.Reason != "" {
			details = append(details, "reason="+d.Reason)
		}
		if d.MeasuredAt != nil {
			details = append(details, "measured_at="+d.MeasuredAt.Format(time.RFC3339))
		}
		details = append(details, "decided_at="+d.DecidedAt.Format(time.RFC3339))

		fmt.Fprintf(stdout, "#%d\tjob=#%d\t%-10s\t%s\n", d.ID, d.JobID, d.Decision, strings.Join(details, " "))
	}
	return 0
}

func parseCommaList(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	var out []string
	for _, p := range parts {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

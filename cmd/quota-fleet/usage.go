package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/quota"
)

func runUsage(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printUsageCmdHelp(stderr)
		return 1
	}
	switch args[0] {
	case "validate":
		return runUsageValidate(args[1:], stdout, stderr)
	case "-h", "--help", "help":
		printUsageCmdHelp(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown usage subcommand %q\n\n", args[0])
		printUsageCmdHelp(stderr)
		return 1
	}
}

func printUsageCmdHelp(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  quota-fleet usage validate [--file path/to/usage.json] [--config fleet.yaml] [--format legacy|v1] [--stale-after 45m] [--ignore-stale] [--check-deploy] [--reject-older-than path/to/existing.json] [--now RFC3339] [--json]")
}

func runUsageValidate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("usage validate", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		filePath        string
		cfgPath         string
		format          string
		staleAfterRaw   string
		ignoreStale     bool
		checkDeploy     bool
		rejectOlderThan string
		nowRaw          string
		asJSON          bool
	)
	fs.StringVar(&filePath, "file", "", "path to usage snapshot JSON file")
	fs.StringVar(&cfgPath, "config", "", "path to fleet.yaml (optional)")
	fs.StringVar(&cfgPath, "c", "", "path to fleet.yaml (shorthand)")
	fs.StringVar(&format, "format", "", "expected usage format: legacy | v1")
	fs.StringVar(&staleAfterRaw, "stale-after", "", "override staleness duration (e.g. 45m)")
	fs.BoolVar(&ignoreStale, "ignore-stale", false, "ignore elapsed-time staleness for exit 2 in normal diagnostic mode")
	fs.BoolVar(&checkDeploy, "check-deploy", false, "deployment gate mode (exit 0 when deployable even if degraded; exit 1 on hard errors)")
	fs.StringVar(&rejectOlderThan, "reject-older-than", "", "reject if generated_at is older than (or conflicts at same timestamp with) existing Schema v1 file")
	fs.StringVar(&nowRaw, "now", "", "override current UTC time (RFC3339, for testing/replay)")
	fs.BoolVar(&asJSON, "json", false, "output structured validation report as JSON")

	if err := fs.Parse(args); err != nil {
		return 1
	}

	filePath = strings.TrimSpace(filePath)
	cfgPath = strings.TrimSpace(cfgPath)
	format = strings.TrimSpace(format)

	if format != "" {
		if _, err := quota.NormalizeUsageFormat(format); err != nil {
			fmt.Fprintf(stderr, "usage validate failed: %v\n", err)
			return 1
		}
	}

	var (
		cfg        *config.Config
		staleAfter time.Duration
		nowTime    time.Time
	)

	if cfgPath == "" && filePath == "" {
		cfgPath = resolveDefaultConfigPath()
	}
	if cfgPath != "" {
		loaded, err := config.Load(cfgPath)
		if err != nil {
			fmt.Fprintf(stderr, "usage validate failed: %v\n", err)
			return 1
		}
		cfg = loaded
		staleAfter = cfg.Quota.EffectiveStaleAfter()
		if filePath == "" {
			filePath = strings.TrimSpace(cfg.Quota.UsageFile)
		}
	}

	if strings.TrimSpace(staleAfterRaw) != "" {
		d, err := time.ParseDuration(strings.TrimSpace(staleAfterRaw))
		if err != nil || d <= 0 {
			fmt.Fprintf(stderr, "usage validate failed: invalid --stale-after %q\n", staleAfterRaw)
			return 1
		}
		staleAfter = d
	}

	if strings.TrimSpace(nowRaw) != "" {
		t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(nowRaw))
		if err != nil {
			fmt.Fprintf(stderr, "usage validate failed: invalid --now %q: %v\n", nowRaw, err)
			return 1
		}
		nowTime = t.UTC()
	}

	res := quota.ValidateUsageFile(quota.ValidateOptions{
		File:            filePath,
		Format:          format,
		Config:          cfg,
		StaleAfter:      staleAfter,
		IgnoreStale:     ignoreStale,
		CheckDeploy:     checkDeploy,
		RejectOlderThan: strings.TrimSpace(rejectOlderThan),
		Now:             nowTime,
	})

	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(res); err != nil {
			fmt.Fprintf(stderr, "usage validate failed: encode json: %v\n", err)
			return 1
		}
		return res.ExitCode
	}

	statusLabel := "OK"
	switch {
	case res.ExitCode == 1:
		statusLabel = "ERROR"
	case res.ExitCode == 2:
		statusLabel = "DEGRADED"
	case checkDeploy && res.Degraded:
		statusLabel = "DEPLOYABLE (DEGRADED)"
	}

	fmt.Fprintf(stdout, "status:       %s (exit=%d)\n", statusLabel, res.ExitCode)
	if res.File != "" {
		fmt.Fprintf(stdout, "file:         %s\n", res.File)
	}
	if res.Format != "" {
		fmt.Fprintf(stdout, "format:       %s\n", res.Format)
	}
	if res.GeneratedAt != nil {
		fmt.Fprintf(stdout, "generated_at: %s\n", res.GeneratedAt.Format(time.RFC3339))
	}
	if res.AgeSeconds != nil {
		fmt.Fprintf(stdout, "age:          %s\n", (time.Duration(*res.AgeSeconds) * time.Second).String())
	}
	if checkDeploy {
		fmt.Fprintf(stdout, "deployable:   %t\n", res.Deployable)
	}
	if len(res.Pools) > 0 {
		fmt.Fprintln(stdout, "pools:")
		for _, p := range res.Pools {
			var parts []string
			if p.Health != "" {
				parts = append(parts, "health="+p.Health)
			}
			if p.Status != "" {
				parts = append(parts, "status="+p.Status)
			}
			if p.BlockedBy != "" {
				parts = append(parts, "blocked_by="+p.BlockedBy)
			}
			if p.Reason != "" {
				parts = append(parts, "reason="+p.Reason)
			}
			if p.PoolError != "" {
				parts = append(parts, "error="+p.PoolError)
			}
			if p.WeeklyRemainingPct != nil {
				parts = append(parts, fmt.Sprintf("weekly=%d%%", *p.WeeklyRemainingPct))
			}
			if p.ShortRemainingPct != nil {
				parts = append(parts, fmt.Sprintf("short=%d%%", *p.ShortRemainingPct))
			} else if p.ShortApplicable != nil && !*p.ShortApplicable {
				parts = append(parts, "short=n/a")
			} else if !p.Unmeasured {
				parts = append(parts, "short=null")
			}
			if p.AgeSeconds != nil {
				parts = append(parts, fmt.Sprintf("age=%s", (time.Duration(*p.AgeSeconds)*time.Second).String()))
			}
			fmt.Fprintf(stdout, "  %-16s %s\n", p.Pool, strings.Join(parts, " "))
		}
	}
	if len(res.Diagnostics) > 0 {
		fmt.Fprintln(stdout, "diagnostics:")
		for _, d := range res.Diagnostics {
			target := ""
			if d.Pool != "" {
				target = fmt.Sprintf("[%s] ", d.Pool)
			}
			fmt.Fprintf(stdout, "  %-8s %-36s %s%s\n", strings.ToUpper(string(d.Severity)), d.Code, target, d.Message)
		}
	}
	if len(res.LegacyDiffs) > 0 {
		fmt.Fprintln(stdout, "legacy_compat_diff:")
		for _, diff := range res.LegacyDiffs {
			fmt.Fprintf(stdout, "  - %s\n", diff)
		}
	}
	return res.ExitCode
}

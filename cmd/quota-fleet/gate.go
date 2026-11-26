package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/quota"
	"github.com/ypsilonmeister/quota-fleet/internal/runtime"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
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
	case "held":
		return runGateHeld(args[1:], stdout, stderr)
	case "release":
		return runGateRelease(args[1:], stdout, stderr)
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
	fmt.Fprintln(w, "  quota-fleet gate check [--pool <name>] [--alt-pools a,b] [--usage-file path] [--format legacy|v1] [--stale-after 45m] [--now RFC3339] [--json] [--config fleet.yaml]")
	fmt.Fprintln(w, "  quota-fleet gate decisions [--limit N] [--db path] [--config fleet.yaml]")
	fmt.Fprintln(w, "  quota-fleet gate held [--status held|released|cancelled|all] [--limit N] [--json] [--db path] [--config fleet.yaml]")
	fmt.Fprintln(w, "  quota-fleet gate release <held_id> [--usage-file path] [--format legacy|v1] [--stale-after 45m] [--json] [--db path] [--config fleet.yaml]")
}

type gateCheckJSONOutput struct {
	quota.GateEvaluation
	Error       string   `json:"error,omitempty"`
	LegacyDiffs []string `json:"legacy_diffs,omitempty"`
}

func runGateCheck(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("gate check", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		cfgPath       string
		usageFileFlag string
		formatFlag    string
		staleAfterRaw string
		nowRaw        string
		pool          string
		altPoolsRaw   string
		asJSON        bool
	)
	fs.StringVar(&cfgPath, "config", resolveDefaultConfigPath(), "path to fleet.yaml")
	fs.StringVar(&cfgPath, "c", resolveDefaultConfigPath(), "path to fleet.yaml (shorthand)")
	fs.StringVar(&usageFileFlag, "usage-file", "", "override usage snapshot path")
	fs.StringVar(&formatFlag, "format", "", "override usage format (legacy|v1)")
	fs.StringVar(&staleAfterRaw, "stale-after", "", "override staleness duration (e.g. 45m)")
	fs.StringVar(&nowRaw, "now", "", "override evaluation time (RFC3339, for replay/comparison)")
	fs.StringVar(&pool, "pool", "", "primary quota pool name (optional if --alt-pools is specified)")
	fs.StringVar(&altPoolsRaw, "alt-pools", "", "comma-separated alternative pool names")
	fs.BoolVar(&asJSON, "json", false, "output result as JSON")

	if err := fs.Parse(args); err != nil {
		return 1
	}

	pool = strings.TrimSpace(pool)
	altPools := parseCommaList(altPoolsRaw)
	if pool == "" && len(altPools) == 0 {
		fmt.Fprintln(stderr, "gate check failed: --pool or --alt-pools is required")
		return 1
	}

	formatFlag = strings.TrimSpace(formatFlag)
	if formatFlag != "" {
		if _, err := quota.NormalizeUsageFormat(formatFlag); err != nil {
			fmt.Fprintf(stderr, "gate check failed: %v\n", err)
			return 1
		}
	}

	var (
		usageFile   = strings.TrimSpace(usageFileFlag)
		usageFormat = config.UsageFormatLegacy
		staleAfter  = config.DefaultQuotaStaleAfter
		poolsCfg    map[string]config.PoolConfig
		cfgLoaded   bool
		nowTime     time.Time
	)

	if cfg, err := config.Load(cfgPath); err == nil {
		cfgLoaded = true
		poolsCfg = cfg.Pools
		staleAfter = cfg.Quota.EffectiveStaleAfter()
		usageFormat = cfg.Quota.EffectiveUsageFormat()
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

	// Format precedence (Item E): --format > --config quota.usage_format > legacy.
	if formatFlag != "" {
		usageFormat = formatFlag
	}

	if strings.TrimSpace(staleAfterRaw) != "" {
		d, err := time.ParseDuration(strings.TrimSpace(staleAfterRaw))
		if err != nil || d <= 0 {
			fmt.Fprintf(stderr, "gate check failed: invalid --stale-after %q\n", staleAfterRaw)
			return 1
		}
		staleAfter = d
	}

	if strings.TrimSpace(nowRaw) != "" {
		t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(nowRaw))
		if err != nil {
			fmt.Fprintf(stderr, "gate check failed: invalid --now %q: %v\n", nowRaw, err)
			return 1
		}
		nowTime = t.UTC()
	}

	evalOpts := quota.EvalOptions{
		Now:             nowTime,
		StaleAfter:      staleAfter,
		Pools:           poolsCfg,
		Format:          usageFormat,
		CheckPoolConfig: cfgLoaded && usageFormat == config.UsageFormatV1,
	}
	eval, evalErr := quota.EvaluateFromFile(usageFile, pool, altPools, evalOpts)

	var legacyDiffs []string
	if usageFormat == config.UsageFormatLegacy && evalErr == nil && !eval.Bypassed && usageFile != "" {
		if snap, serr := quota.LoadUsageFileWithFormat(usageFile, config.UsageFormatLegacy); serr == nil {
			legacyDiffs = quota.CompareLegacyWithV1(pool, altPools, snap, evalOpts, eval)
		}
	}

	// Decision 9: exit 1 on snapshot read/parse/format/generated_at-future errors.
	exitCode := 0
	if evalErr != nil || eval.Reason == quota.ReasonSnapshotError {
		exitCode = 1
	}

	if asJSON {
		out := gateCheckJSONOutput{
			GateEvaluation: eval,
			LegacyDiffs:    legacyDiffs,
		}
		if evalErr != nil {
			out.Error = evalErr.Error()
		}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			fmt.Fprintf(stderr, "gate check failed: encode json: %v\n", err)
			return 1
		}
		return exitCode
	}

	fmt.Fprintf(stdout, "decision:      %s\n", eval.Decision)
	if eval.Format != "" {
		fmt.Fprintf(stdout, "format:        %s\n", eval.Format)
	}
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
	if eval.Detail != "" {
		fmt.Fprintf(stdout, "detail:        %s\n", eval.Detail)
	}
	if eval.PoolError != "" {
		fmt.Fprintf(stdout, "pool_error:    %s\n", eval.PoolError)
	}
	if eval.AgeSeconds != nil {
		fmt.Fprintf(stdout, "age:           %s\n", (time.Duration(*eval.AgeSeconds) * time.Second).String())
	}
	if eval.MeasuredAt != nil {
		fmt.Fprintf(stdout, "measured_at:   %s\n", eval.MeasuredAt.Format(time.RFC3339))
	}
	fmt.Fprintf(stdout, "decided_at:    %s\n", eval.DecidedAt.Format(time.RFC3339))
	if evalErr != nil {
		fmt.Fprintf(stdout, "error:         %v\n", evalErr)
	}
	if len(legacyDiffs) > 0 {
		fmt.Fprintln(stdout, "legacy_compat_diff:")
		for _, diff := range legacyDiffs {
			fmt.Fprintf(stdout, "  - %s\n", diff)
		}
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
			if p.PoolError != "" {
				parts = append(parts, "error="+p.PoolError)
			}
			if p.AgeSeconds != nil {
				parts = append(parts, fmt.Sprintf("age=%s", (time.Duration(*p.AgeSeconds)*time.Second).String()))
			}
			if p.Snapshot != nil {
				if !p.Snapshot.WeeklyAbsent {
					parts = append(parts, fmt.Sprintf("weekly=%d%%", p.Snapshot.WeeklyRemainingPercent))
				}
				if p.Snapshot.ShortRemainingPercent != nil {
					parts = append(parts, fmt.Sprintf("short=%d%%", *p.Snapshot.ShortRemainingPercent))
				} else if p.Snapshot.ShortApplicable != nil && !*p.Snapshot.ShortApplicable {
					parts = append(parts, "short=n/a")
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
	return exitCode
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
		if d.Mode != "" {
			details = append(details, "mode="+d.Mode)
		}
		if d.Held {
			details = append(details, "held=true")
		}
		if d.HeldID != "" && d.JobID > 0 {
			details = append(details, "held_id="+d.HeldID)
		}
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

		target := fmt.Sprintf("job=#%d", d.JobID)
		if d.JobID == 0 && d.HeldID != "" {
			target = "held=" + d.HeldID
		}
		fmt.Fprintf(stdout, "#%d\t%s\t%-10s\t%s\n", d.ID, target, d.Decision, strings.Join(details, " "))
	}
	return 0
}

func runGateHeld(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("gate held", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		cfgPath   string
		dbPath    string
		statusRaw string
		limit     int
		asJSON    bool
	)
	fs.StringVar(&cfgPath, "config", resolveDefaultConfigPath(), "path to fleet.yaml")
	fs.StringVar(&cfgPath, "c", resolveDefaultConfigPath(), "path to fleet.yaml (shorthand)")
	fs.StringVar(&dbPath, "db", "", "override SQLite database path")
	fs.StringVar(&statusRaw, "status", string(store.HeldStatusHeld), "status filter (held|released|cancelled|all)")
	fs.IntVar(&limit, "limit", 0, "maximum number of held jobs to show")
	fs.BoolVar(&asJSON, "json", false, "output result as JSON")

	if err := fs.Parse(args); err != nil {
		return 1
	}
	if limit < 0 {
		fmt.Fprintf(stderr, "gate held failed: --limit must be >= 0 (got %d)\n", limit)
		return 1
	}
	status := store.HeldStatus(strings.TrimSpace(statusRaw))
	if status == "" {
		status = store.HeldStatusHeld
	}
	if status != "all" && !status.Valid() {
		fmt.Fprintf(stderr, "gate held failed: invalid --status %q (allowed: held, released, cancelled, all)\n", statusRaw)
		return 1
	}

	ctx := context.Background()
	st, _, err := openLocalStore(ctx, cfgPath, dbPath)
	if err != nil {
		fmt.Fprintf(stderr, "gate held failed: %v\n", err)
		return 1
	}
	defer st.Close()

	items, err := st.ListHeldJobs(ctx, status, limit)
	if err != nil {
		fmt.Fprintf(stderr, "gate held failed: %v\n", err)
		return 1
	}

	if asJSON {
		if items == nil {
			items = []*store.HeldJob{}
		}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(items); err != nil {
			fmt.Fprintf(stderr, "gate held failed: encode json: %v\n", err)
			return 1
		}
		return 0
	}

	if len(items) == 0 {
		fmt.Fprintf(stdout, "no held jobs (status=%s)\n", status)
		return 0
	}

	for _, h := range items {
		var details []string
		if h.Pool != "" {
			details = append(details, "pool="+h.Pool)
		}
		if len(h.AltPools) > 0 {
			details = append(details, "alt="+strings.Join(h.AltPools, ","))
		}
		if h.Reason != "" {
			details = append(details, "reason="+h.Reason)
		}
		if h.ReleasedJobID != nil {
			details = append(details, fmt.Sprintf("released_job=#%d", *h.ReleasedJobID))
		}
		if h.ReleasedPool != "" {
			details = append(details, "released_pool="+h.ReleasedPool)
		}
		if h.LastError != "" {
			details = append(details, fmt.Sprintf("last_error=%q", h.LastError))
		}
		details = append(details, "created_at="+h.CreatedAt.Format(time.RFC3339))
		fmt.Fprintf(stdout, "%s\t%-9s\t%s\t%s\n", h.ID, h.Status, h.Payload.Title, strings.Join(details, " "))
	}
	return 0
}

func runGateRelease(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("gate release", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		cfgPath       string
		dbPath        string
		usageFileFlag string
		formatFlag    string
		staleAfterRaw string
		asJSON        bool
	)
	fs.StringVar(&cfgPath, "config", resolveDefaultConfigPath(), "path to fleet.yaml")
	fs.StringVar(&cfgPath, "c", resolveDefaultConfigPath(), "path to fleet.yaml (shorthand)")
	fs.StringVar(&dbPath, "db", "", "override SQLite database path")
	fs.StringVar(&usageFileFlag, "usage-file", "", "override usage snapshot path")
	fs.StringVar(&formatFlag, "format", "", "override usage format (legacy|v1)")
	fs.StringVar(&staleAfterRaw, "stale-after", "", "override staleness duration (e.g. 45m)")
	fs.BoolVar(&asJSON, "json", false, "output result as JSON")

	heldID, err := parseStringIDAndFlags(fs, args, "held_id")
	if err != nil {
		fmt.Fprintf(stderr, "gate release failed: %v\n", err)
		return 1
	}

	formatFlag = strings.TrimSpace(formatFlag)
	if formatFlag != "" {
		if _, err := quota.NormalizeUsageFormat(formatFlag); err != nil {
			fmt.Fprintf(stderr, "gate release failed: %v\n", err)
			return 1
		}
	}

	var (
		usageFile   = strings.TrimSpace(usageFileFlag)
		usageFormat = config.UsageFormatLegacy
		staleAfter  = config.DefaultQuotaStaleAfter
		poolsCfg    map[string]config.PoolConfig
	)

	if cfg, err := config.Load(cfgPath); err == nil {
		poolsCfg = cfg.Pools
		staleAfter = cfg.Quota.EffectiveStaleAfter()
		usageFormat = cfg.Quota.EffectiveUsageFormat()
		if usageFile == "" {
			usageFile = strings.TrimSpace(cfg.Quota.UsageFile)
		}
	} else {
		if dbPath == "" || !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(stderr, "gate release failed: %v\n", err)
			return 1
		}
	}
	if formatFlag != "" {
		usageFormat = formatFlag
	}

	if strings.TrimSpace(staleAfterRaw) != "" {
		d, err := time.ParseDuration(strings.TrimSpace(staleAfterRaw))
		if err != nil || d <= 0 {
			fmt.Fprintf(stderr, "gate release failed: invalid --stale-after %q\n", staleAfterRaw)
			return 1
		}
		staleAfter = d
	}

	ctx := context.Background()
	st, _, err := openLocalStore(ctx, cfgPath, dbPath)
	if err != nil {
		fmt.Fprintf(stderr, "gate release failed: %v\n", err)
		return 1
	}
	defer st.Close()

	checker := runtime.NewGateChecker(runtime.GateCheckerOptions{
		Store:       st,
		GateMode:    config.GateModeEnforce,
		UsageFile:   usageFile,
		UsageFormat: usageFormat,
		StaleAfter:  staleAfter,
		Pools:       poolsCfg,
		Logger:      log.New(io.Discard, "", 0),
	})

	res, relErr := checker.ReleaseByID(ctx, heldID)
	if asJSON && res != nil {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(res)
		if relErr != nil || !res.Released {
			return 1
		}
		return 0
	}

	if relErr != nil {
		if res != nil && res.Reason != "" {
			fmt.Fprintf(stderr, "gate release failed: held %s not released: %s\n", heldID, res.Reason)
		} else {
			fmt.Fprintf(stderr, "gate release failed: %v\n", relErr)
		}
		return 1
	}
	if !res.Released {
		fmt.Fprintf(stderr, "gate release: held %s not released: %s\n", heldID, res.Reason)
		return 1
	}

	fmt.Fprintf(stdout, "released held %s -> job #%d [%s] %s\n", res.Held.ID, res.Job.ID, res.Job.Status, res.Job.Title)
	fmt.Fprintf(stdout, "  selected_pool: %s\n", res.SelectedPool)
	fmt.Fprintf(stdout, "  request_id:    %s\n", res.Job.RequestID)
	return 0
}

// parseStringIDAndFlags allows both `<id> [--flags...]` and `[--flags...] <id>`.
func parseStringIDAndFlags(fs *flag.FlagSet, args []string, label string) (string, error) {
	var idRaw string
	flagArgs := args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		idRaw = strings.TrimSpace(args[0])
		flagArgs = args[1:]
	}
	if err := fs.Parse(flagArgs); err != nil {
		return "", err
	}
	if idRaw == "" {
		if fs.NArg() == 0 {
			return "", fmt.Errorf("%s is required", label)
		}
		idRaw = strings.TrimSpace(fs.Arg(0))
	}
	if idRaw == "" {
		return "", fmt.Errorf("%s is required", label)
	}
	return idRaw, nil
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

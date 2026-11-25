package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

func runJob(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printJobUsage(stderr)
		return 1
	}

	switch args[0] {
	case "create":
		return runJobCreate(args[1:], stdout, stderr)
	case "list":
		return runJobList(args[1:], stdout, stderr)
	case "get":
		return runJobGet(args[1:], stdout, stderr)
	case "cancel":
		return runJobCancel(args[1:], stdout, stderr)
	case "requeue":
		return runJobRequeue(args[1:], stdout, stderr)
	case "-h", "--help", "help":
		printJobUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown job subcommand %q\n\n", args[0])
		printJobUsage(stderr)
		return 1
	}
}

func printJobUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  quota-fleet job create --title <title> [--agent ...] [--host ...] [--workdir ...] [--instructions ...] [--acceptance ...] [--request-id ...] [--parent <id>] [--target-ref ...] [--model ...] [--pool ...] [--db path] [--config fleet.yaml]")
	fmt.Fprintln(w, "  quota-fleet job list --status <status> [--limit N] [--cursor ID] [--db path] [--config fleet.yaml]")
	fmt.Fprintln(w, "  quota-fleet job get <id> [--db path] [--config fleet.yaml]")
	fmt.Fprintln(w, "  quota-fleet job cancel <id> [--reason ...] [--db path] [--config fleet.yaml]")
	fmt.Fprintln(w, "  quota-fleet job requeue <id> [--db path] [--config fleet.yaml]")
}

func openLocalStore(ctx context.Context, cfgPath, dbPath string) (*store.Store, string, error) {
	var instanceID string
	if dbPath == "" {
		cfg, err := config.Load(cfgPath)
		if err != nil {
			return nil, "", err
		}
		dbPath = cfg.DBPath()
		instanceID = cfg.InstanceID
	}

	st, err := store.Open(ctx, dbPath, store.Options{InstanceID: instanceID})
	if err != nil {
		return nil, "", fmt.Errorf("open store %q: %w", dbPath, err)
	}

	if instanceID != "" {
		if err := st.VerifyInstanceID(ctx, instanceID); err != nil && !errors.Is(err, store.ErrNotFound) {
			_ = st.Close()
			return nil, "", fmt.Errorf("instance_id check failed: %w", err)
		}
	}
	return st, dbPath, nil
}

func runJobCreate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("job create", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		cfgPath      string
		dbPath       string
		title        string
		agent        string
		host         string
		workdir      string
		instructions string
		acceptance   string
		requestID    string
		parentID     int64
		targetRef    string
		model        string
		pool         string
		draft        bool
	)

	fs.StringVar(&cfgPath, "config", resolveDefaultConfigPath(), "path to fleet.yaml")
	fs.StringVar(&cfgPath, "c", resolveDefaultConfigPath(), "path to fleet.yaml (shorthand)")
	fs.StringVar(&dbPath, "db", "", "override SQLite database path")
	fs.StringVar(&title, "title", "", "job title (required)")
	fs.StringVar(&agent, "agent", "", "agent name")
	fs.StringVar(&host, "host", "", "target worker host")
	fs.StringVar(&workdir, "workdir", "", "working directory")
	fs.StringVar(&instructions, "instructions", "", "job instructions")
	fs.StringVar(&acceptance, "acceptance", "", "acceptance criteria")
	fs.StringVar(&requestID, "request-id", "", "idempotency request_id (auto-generated if omitted)")
	fs.Int64Var(&parentID, "parent", 0, "parent job ID")
	fs.StringVar(&targetRef, "target-ref", "", "target git ref/branch")
	fs.StringVar(&model, "model", "", "model name")
	fs.StringVar(&pool, "pool", "", "quota pool name")
	fs.BoolVar(&draft, "draft", false, "create job in draft status")

	if err := fs.Parse(args); err != nil {
		return 1
	}

	if strings.TrimSpace(title) == "" {
		fmt.Fprintln(stderr, "job create failed: --title is required")
		return 1
	}
	if parentID < 0 {
		fmt.Fprintf(stderr, "job create failed: invalid --parent %d\n", parentID)
		return 1
	}
	if strings.TrimSpace(requestID) == "" {
		gen, err := generateCLIRequestID()
		if err != nil {
			fmt.Fprintf(stderr, "job create failed: generate request-id: %v\n", err)
			return 1
		}
		requestID = gen
	}

	ctx := context.Background()
	st, _, err := openLocalStore(ctx, cfgPath, dbPath)
	if err != nil {
		fmt.Fprintf(stderr, "job create failed: %v\n", err)
		return 1
	}
	defer st.Close()

	params := store.CreateJobParams{
		RequestID:          requestID,
		Title:              title,
		Draft:              draft,
		Agent:              agent,
		Host:               host,
		Workdir:            workdir,
		TargetRef:          targetRef,
		Instructions:       instructions,
		AcceptanceCriteria: acceptance,
		Model:              model,
		Pool:               pool,
	}
	if parentID > 0 {
		p := parentID
		params.ParentID = &p
	}

	job, created, err := st.CreateJob(ctx, params)
	if err != nil {
		fmt.Fprintf(stderr, "job create failed: %v\n", err)
		return 1
	}

	verb := "created"
	if !created {
		verb = "existing"
	}
	fmt.Fprintf(stdout, "%s job #%d [%s] %s\n", verb, job.ID, job.Status, job.Title)
	fmt.Fprintf(stdout, "  request_id: %s\n", job.RequestID)
	return 0
}

func runJobList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("job list", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		cfgPath   string
		dbPath    string
		statusStr string
		limit     int
		cursor    int64
		desc      bool
	)
	fs.StringVar(&cfgPath, "config", resolveDefaultConfigPath(), "path to fleet.yaml")
	fs.StringVar(&cfgPath, "c", resolveDefaultConfigPath(), "path to fleet.yaml (shorthand)")
	fs.StringVar(&dbPath, "db", "", "override SQLite database path")
	fs.StringVar(&statusStr, "status", "", "job status filter (required)")
	fs.IntVar(&limit, "limit", 0, "maximum number of jobs to return")
	fs.Int64Var(&cursor, "cursor", 0, "pagination cursor (job ID)")
	fs.BoolVar(&desc, "desc", false, "order descending by ID")

	if err := fs.Parse(args); err != nil {
		return 1
	}

	status := store.Status(strings.TrimSpace(statusStr))
	if !status.Valid() {
		fmt.Fprintf(stderr, "job list failed: valid --status is required (got %q)\n", statusStr)
		return 1
	}

	ctx := context.Background()
	st, _, err := openLocalStore(ctx, cfgPath, dbPath)
	if err != nil {
		fmt.Fprintf(stderr, "job list failed: %v\n", err)
		return 1
	}
	defer st.Close()

	page, err := st.ListJobs(ctx, store.ListJobsFilter{
		Status:     status,
		Cursor:     cursor,
		Limit:      limit,
		Descending: desc,
	})
	if err != nil {
		fmt.Fprintf(stderr, "job list failed: %v\n", err)
		return 1
	}

	if len(page.Jobs) == 0 {
		fmt.Fprintf(stdout, "no jobs with status %s\n", status)
		return 0
	}

	for _, j := range page.Jobs {
		var meta []string
		if j.Agent != "" {
			meta = append(meta, "agent="+j.Agent)
		}
		if j.Host != "" {
			meta = append(meta, "host="+j.Host)
		}
		if len(meta) > 0 {
			fmt.Fprintf(stdout, "#%d\t%-9s\t%s\t(%s)\n", j.ID, j.Status, j.Title, strings.Join(meta, ", "))
		} else {
			fmt.Fprintf(stdout, "#%d\t%-9s\t%s\n", j.ID, j.Status, j.Title)
		}
	}
	if page.HasMore {
		fmt.Fprintf(stdout, "next_cursor: %d\n", page.NextCursor)
	}
	return 0
}

func runJobGet(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("job get", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		cfgPath string
		dbPath  string
	)
	fs.StringVar(&cfgPath, "config", resolveDefaultConfigPath(), "path to fleet.yaml")
	fs.StringVar(&cfgPath, "c", resolveDefaultConfigPath(), "path to fleet.yaml (shorthand)")
	fs.StringVar(&dbPath, "db", "", "override SQLite database path")

	jobID, err := parseIDAndFlags(fs, args)
	if err != nil {
		fmt.Fprintf(stderr, "job get failed: %v\n", err)
		return 1
	}

	ctx := context.Background()
	st, _, err := openLocalStore(ctx, cfgPath, dbPath)
	if err != nil {
		fmt.Fprintf(stderr, "job get failed: %v\n", err)
		return 1
	}
	defer st.Close()

	job, err := st.GetJob(ctx, jobID)
	if err != nil {
		fmt.Fprintf(stderr, "job get failed: %v\n", err)
		return 1
	}

	printJobDetails(stdout, job)
	return 0
}

func runJobCancel(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("job cancel", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		cfgPath string
		dbPath  string
		reason  string
	)
	fs.StringVar(&cfgPath, "config", resolveDefaultConfigPath(), "path to fleet.yaml")
	fs.StringVar(&cfgPath, "c", resolveDefaultConfigPath(), "path to fleet.yaml (shorthand)")
	fs.StringVar(&dbPath, "db", "", "override SQLite database path")
	fs.StringVar(&reason, "reason", "", "cancellation reason")

	jobID, err := parseIDAndFlags(fs, args)
	if err != nil {
		fmt.Fprintf(stderr, "job cancel failed: %v\n", err)
		return 1
	}

	ctx := context.Background()
	st, _, err := openLocalStore(ctx, cfgPath, dbPath)
	if err != nil {
		fmt.Fprintf(stderr, "job cancel failed: %v\n", err)
		return 1
	}
	defer st.Close()

	res, err := st.Cancel(ctx, jobID, reason)
	if err != nil {
		fmt.Fprintf(stderr, "job cancel failed: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "cancelled job #%d [%s] %s\n", res.Job.ID, res.Job.Status, res.Job.Title)
	if len(res.CancelledChildIDs) > 0 {
		fmt.Fprintf(stdout, "  cancelled_children: %v\n", res.CancelledChildIDs)
	}
	return 0
}

func runJobRequeue(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("job requeue", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		cfgPath string
		dbPath  string
	)
	fs.StringVar(&cfgPath, "config", resolveDefaultConfigPath(), "path to fleet.yaml")
	fs.StringVar(&cfgPath, "c", resolveDefaultConfigPath(), "path to fleet.yaml (shorthand)")
	fs.StringVar(&dbPath, "db", "", "override SQLite database path")

	jobID, err := parseIDAndFlags(fs, args)
	if err != nil {
		fmt.Fprintf(stderr, "job requeue failed: %v\n", err)
		return 1
	}

	ctx := context.Background()
	st, _, err := openLocalStore(ctx, cfgPath, dbPath)
	if err != nil {
		fmt.Fprintf(stderr, "job requeue failed: %v\n", err)
		return 1
	}
	defer st.Close()

	job, err := st.Requeue(ctx, jobID)
	if err != nil {
		fmt.Fprintf(stderr, "job requeue failed: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "requeued job #%d [%s] %s\n", job.ID, job.Status, job.Title)
	return 0
}

// parseIDAndFlags allows both `<id> [--flags...]` and `[--flags...] <id>`.
func parseIDAndFlags(fs *flag.FlagSet, args []string) (int64, error) {
	var idRaw string
	flagArgs := args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		idRaw = args[0]
		flagArgs = args[1:]
	}
	if err := fs.Parse(flagArgs); err != nil {
		return 0, err
	}
	if idRaw == "" {
		if fs.NArg() == 0 {
			return 0, errors.New("job id is required")
		}
		idRaw = fs.Arg(0)
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(idRaw, "#"), 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("invalid job id %q", idRaw)
	}
	return id, nil
}

func printJobDetails(w io.Writer, j *store.Job) {
	fmt.Fprintf(w, "id:          %d\n", j.ID)
	fmt.Fprintf(w, "status:      %s\n", j.Status)
	fmt.Fprintf(w, "title:       %s\n", j.Title)
	fmt.Fprintf(w, "request_id:  %s\n", j.RequestID)
	if j.Agent != "" {
		fmt.Fprintf(w, "agent:       %s\n", j.Agent)
	}
	if j.Host != "" {
		fmt.Fprintf(w, "host:        %s\n", j.Host)
	}
	if j.Workdir != "" {
		fmt.Fprintf(w, "workdir:     %s\n", j.Workdir)
	}
	if j.TargetRef != "" {
		fmt.Fprintf(w, "target_ref:  %s\n", j.TargetRef)
	}
	if j.Model != "" {
		fmt.Fprintf(w, "model:       %s\n", j.Model)
	}
	if j.Pool != "" {
		fmt.Fprintf(w, "pool:        %s\n", j.Pool)
	}
	if len(j.AltPools) > 0 {
		fmt.Fprintf(w, "alt_pools:   %s\n", strings.Join(j.AltPools, ", "))
	}
	if j.ParentID != nil {
		fmt.Fprintf(w, "parent_id:   %d\n", *j.ParentID)
	}
	fmt.Fprintf(w, "attempts:    %d\n", j.Attempts)
	if j.ClaimedBy != "" {
		fmt.Fprintf(w, "claimed_by:  %s\n", j.ClaimedBy)
	}
	if j.ClaimedAt != nil {
		fmt.Fprintf(w, "claimed_at:  %s\n", j.ClaimedAt.Format(time.RFC3339))
	}
	if j.Instructions != "" {
		fmt.Fprintf(w, "instructions: %s\n", j.Instructions)
	}
	if j.AcceptanceCriteria != "" {
		fmt.Fprintf(w, "acceptance:  %s\n", j.AcceptanceCriteria)
	}
	if j.ResultText != "" {
		fmt.Fprintf(w, "result_text: %s\n", j.ResultText)
	}
	if j.LastError != "" {
		fmt.Fprintf(w, "last_error:  %s\n", j.LastError)
	}
	if j.DeliveredAt != nil {
		fmt.Fprintf(w, "delivered_at: %s\n", j.DeliveredAt.Format(time.RFC3339))
	}
	fmt.Fprintf(w, "created_at:  %s\n", j.CreatedAt.Format(time.RFC3339))
	fmt.Fprintf(w, "updated_at:  %s\n", j.UpdatedAt.Format(time.RFC3339))
}

func generateCLIRequestID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "cli-" + hex.EncodeToString(b[:]), nil
}

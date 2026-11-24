package mcpserver

import (
	"context"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const defaultServerVersion = "0.1.0"

// Options configures the quota-fleet MCP server.
type Options struct {
	APIURL     string
	Token      string
	HTTPClient *http.Client
	Version    string
}

// NewServer creates an MCP server wired to the quota-fleet HTTP API with the 12 tools defined in docs/07 §2-2.
func NewServer(opts Options) (*mcp.Server, error) {
	client, err := NewClient(opts.APIURL, opts.Token, opts.HTTPClient)
	if err != nil {
		return nil, err
	}

	version := strings.TrimSpace(opts.Version)
	if version == "" {
		version = defaultServerVersion
	}

	srv := mcp.NewServer(&mcp.Implementation{
		Name:    "quota-fleet",
		Version: version,
	}, nil)

	registerTools(srv, client)
	return srv, nil
}

func registerTools(srv *mcp.Server, client *Client) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "create_job",
		Description: "Create a new job in quota-fleet (POST /jobs). If Quota Gate holds the job (HTTP 202), returns a normal result with held=true, held_id, and reason.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in CreateJobInput) (*mcp.CallToolResult, any, error) {
		out, err := client.CreateJob(ctx, in)
		if err != nil {
			return nil, nil, err
		}
		return nil, out, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_jobs",
		Description: "List jobs filtered by status (required) and optional agent, host, title query (q), parent_id, cursor, limit, and desc (GET /jobs). Pages are capped to fit the MCP frame byte budget; if truncated=true, continue with next_cursor.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in ListJobsInput) (*mcp.CallToolResult, any, error) {
		out, err := client.ListJobs(ctx, in)
		if err != nil {
			return nil, nil, err
		}
		return nil, out, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_job",
		Description: "Get full details of a job by ID, including instructions, acceptance_criteria, result_text, and last_error (GET /jobs/{id}).",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in JobIDInput) (*mcp.CallToolResult, any, error) {
		out, err := client.GetJob(ctx, in.ID)
		if err != nil {
			return nil, nil, err
		}
		return nil, out, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "cancel_job",
		Description: "Cancel a job in draft, queued, or waiting status (POST /jobs/{id}/cancel). Running (claimed) jobs cannot be cancelled via MCP.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in CancelJobInput) (*mcp.CallToolResult, any, error) {
		out, err := client.CancelJob(ctx, in)
		if err != nil {
			return nil, nil, err
		}
		return nil, out, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "requeue_job",
		Description: "Requeue a failed or cancelled job back to queued status (POST /jobs/{id}/requeue).",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in JobIDInput) (*mcp.CallToolResult, any, error) {
		out, err := client.RequeueJob(ctx, in.ID)
		if err != nil {
			return nil, nil, err
		}
		return nil, out, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "mark_delivered",
		Description: "Mark a completed or failed job as delivered after retrieving its results (POST /jobs/{id}/deliver).",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in JobIDInput) (*mcp.CallToolResult, any, error) {
		out, err := client.MarkDelivered(ctx, in.ID)
		if err != nil {
			return nil, nil, err
		}
		return nil, out, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_held",
		Description: "List jobs held by Quota Gate (GET /gate/held).",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in ListHeldInput) (*mcp.CallToolResult, any, error) {
		out, err := client.ListHeld(ctx, in)
		if err != nil {
			return nil, nil, err
		}
		return nil, out, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "release_held",
		Description: "Attempt to release a held job via immediate quota re-evaluation (POST /gate/held/{id}/release). Returns released=true with job_id on success, or released=false with reason if still blocked.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in HeldIDInput) (*mcp.CallToolResult, any, error) {
		out, err := client.ReleaseHeld(ctx, in.ID)
		if err != nil {
			return nil, nil, err
		}
		return nil, out, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "cancel_held",
		Description: "Cancel a held job so it will not be released (DELETE /gate/held/{id}).",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in HeldIDInput) (*mcp.CallToolResult, any, error) {
		out, err := client.CancelHeld(ctx, in.ID)
		if err != nil {
			return nil, nil, err
		}
		return nil, out, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "gate_decisions",
		Description: "List recent Quota Gate decisions, optionally filtered by job_id, held_id, or limit (GET /gate/decisions).",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in GateDecisionsInput) (*mcp.CallToolResult, any, error) {
		out, err := client.GateDecisions(ctx, in)
		if err != nil {
			return nil, nil, err
		}
		return nil, out, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_deliveries",
		Description: "List job deliveries filtered by status (undelivered, delivered, or all) and optional limit (GET /deliveries).",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in ListDeliveriesInput) (*mcp.CallToolResult, any, error) {
		out, err := client.ListDeliveries(ctx, in)
		if err != nil {
			return nil, nil, err
		}
		return nil, out, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "fleet_status",
		Description: "Get fleet summary including instance_id, per-status job counts, and total job count (GET /status).",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ FleetStatusInput) (*mcp.CallToolResult, any, error) {
		out, err := client.FleetStatus(ctx)
		if err != nil {
			return nil, nil, err
		}
		return nil, out, nil
	})
}

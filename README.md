# quota-fleet

A design and specification for running a **fleet of coding agents on flat-rate
subscription quotas**, with a verification pipeline that makes their output
trustworthy enough to merge.

> Status: **active implementation**. Core phases 1–3 and Phase 4 preparation
> (SQLite job store, HTTP API, Dispatcher/Launcher/Shepherd/Reconciler runtime,
> SSH/local host adapter, Quota Gate, typed+decision Evaluator, file/webhook
> Notifier, and Phase UI-0 bootstrap CLI) are implemented. The embedded web UI
> is planned for **Phase UI-1**, and the MCP server (`quota-fleet mcp`) is
> planned for **Phase UI-2** (see [docs/07-ui-and-mcp.md](docs/07-ui-and-mcp.md)).

## Quickstart

1. **Get the binary** (single static Go binary, `CGO_ENABLED=0`):
   ```bash
   CGO_ENABLED=0 go build -o quota-fleet ./cmd/quota-fleet
   ```
2. **Initialize and start the server**:
   ```bash
   ./quota-fleet init
   ./quota-fleet serve
   ```
   `quota-fleet init` writes a minimal `fleet.yaml` (mode `0600`) with a
   generated `instance_id` and Bearer API token. `quota-fleet serve` prints a
   startup banner showing the active adapter, gate mode, evaluator mode,
   notifier, API auth status, and planned UI URL.
3. **Register your first job in the UI**:
   Open the UI URL shown in the startup banner (default `http://127.0.0.1:8737/ui/`).
   *Note: the embedded web UI is planned for **Phase UI-1**; until then, you can
   register jobs via CLI (`quota-fleet job create --title "..."`) or the HTTP API.
   To generate MCP client config snippets for AI clients, run `quota-fleet mcp config`.*

## Supported OS boundaries (v1)

- **`quota-fleet serve` and local worker execution**: Linux (including Windows
  **WSL2**).
- **Native macOS / Windows**: CLI and MCP client connections to a remote
  `quota-fleet serve` instance only (native local `serve` is out of scope for v1).

## Why this exists

Hosted AI programmers charge per seat and per task. Meanwhile, most
developers already pay for flat-rate agent subscriptions (coding-agent CLIs
with weekly and rolling-window usage limits). Those limits are a *scheduling*
problem:

- A job sent to an exhausted pool fails, and the work is lost or must be
  babysat.
- A pool that resets with 40% left is money quietly evaporating.
- Several subscriptions, each with several model pools resetting at
  *different times*, cannot be juggled by a human.

This design treats subscription quotas as the compute substrate and builds
the missing control plane around them.

## The three pillars

1. **An asynchronous job substrate** ([architecture](docs/01-architecture.md),
   [job model](docs/02-job-model.md))
   Jobs live in a store that is the single source of truth. A signal file
   only *hints* that work exists; dispatchers, workers, a shepherd and a
   reconciler keep the queue moving even when individual pieces die, are
   duplicated, or lose messages. Claims are compare-and-swap; delivery is
   at-least-once; failed jobs are never retried automatically.

2. **Quota-aware scheduling** ([quota gate](docs/03-quota-gate.md))
   Before a job is queued, the gate checks *measured* remaining quota for
   the pool that would run it. Exhausted pools do not fail jobs — jobs are
   **held** and released automatically when measurement shows recovery.
   Assignment prefers the pool whose weekly window expires soonest, so
   capacity is spent before it resets. Leftover capacity near a reset can
   optionally be burned on pre-approved, read-only research jobs.

3. **A verification pipeline** ([conventions](docs/04-pipeline-conventions.md))
   Every ticket runs implementation → independent review by a *different
   model family* → fixes by the *original implementer, resuming its
   session* → merge-ready, without returning to the human in between.
   Review findings must be reproduced before they are fixed. Judgment calls
   are parked on the pull request for merge time, not used as stop points.
   The human keeps exactly one job: the merge decision.

## What this is not

- Not another coding agent. The workers are existing agent CLIs (Claude
  Code, Codex, Gemini-based agents, …) launched as ordinary processes on
  ordinary machines.
- Not a hosted service. The design assumes a small fleet of machines you
  own, and one coordinator.
- Not pay-as-you-go optimisation. If you run on metered API keys, you do
  not need the quota gate; the job substrate and the pipeline conventions
  still apply.

## Repository layout

- `cmd/quota-fleet/` — CLI entrypoint (`init`, `serve`, `job`, `gate`, `mcp config`, `config check`, `status`)
- `internal/` — store, config, API server, runtime, SSH/local adapter, quota gate, evaluator
- `docs/01-architecture.md` — components and how they interact
- `docs/02-job-model.md` — job states, claims, chains, failure semantics
- `docs/03-quota-gate.md` — pools, the hold/release gate, burn-down jobs
- `docs/04-pipeline-conventions.md` — the implement/review/fix pipeline rules
- `docs/05-case-study.md` — the system in production: a real OSS release
- `docs/06-implementation-design.md` — standalone implementation specification
- `docs/07-ui-and-mcp.md` — embedded UI and MCP design (v2)

## License

MIT — see [LICENSE](LICENSE).

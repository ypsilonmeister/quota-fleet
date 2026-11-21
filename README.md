# quota-fleet

A design and specification for running a **fleet of coding agents on flat-rate
subscription quotas**, with a verification pipeline that makes their output
trustworthy enough to merge.

> Status: **design phase**. This repository currently contains the
> architecture and the specifications. It is extracted from a working private
> system that has already developed and shipped real software (see the
> [case study](docs/05-case-study.md)). A standalone reference implementation
> is planned; the specifications here are written so that the core can be
> rebuilt without the original hosting environment.

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

- `docs/01-architecture.md` — components and how they interact
- `docs/02-job-model.md` — job states, claims, chains, failure semantics
- `docs/03-quota-gate.md` — pools, the hold/release gate, burn-down jobs
- `docs/04-pipeline-conventions.md` — the implement/review/fix pipeline rules
- `docs/05-case-study.md` — the system in production: a real OSS release

## License

MIT — see [LICENSE](LICENSE).

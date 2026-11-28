# 01 — Architecture

## Components

| Component | Role |
|---|---|
| **Coordinator** | The only writer of new jobs. Registers work, assigns pools, restructures chains when agents die, verifies deliveries, and prepares merge decisions for the human. May be a human-assisted agent; the design does not require it to be a specific product. |
| **Job store** | The single source of truth for jobs: a database with an action/API layer. Every other component re-reads state from the store instead of trusting messages. |
| **Signal file** | An append-only JSONL file. Each line is a *hint* that a job entered `queued`. A lost, duplicated or forged signal can never cause damage by itself, because workers re-read the store. |
| **Dispatcher** | A lightweight poller that watches the signal file and wakes a worker. Caps itself at one wake per poll; carries no job data in the wake payload. Writes a heartbeat so its own death is detectable. |
| **Worker (launcher)** | On wake, sweeps the store for queued jobs (oldest first, up to a per-run cap), claims them, and launches the actual agent processes on worker hosts. Launch-only: it does not evaluate results. |
| **Shepherd** | A periodic patrol that evaluates finished jobs against their acceptance criteria, delivers results, creates pull requests, and registers follow-up jobs (for example the review that follows an implementation). |
| **Reconciler** | A slower safety net. Processes stale queued jobs itself, reports claimed jobs stuck beyond twice their expected duration and completed jobs whose delivery is missing, and stays silent when nothing is wrong. |
| **Quota gate** | Sits in front of job creation. Holds jobs whose pool is exhausted and releases them when measured quota recovers. See [03-quota-gate.md](03-quota-gate.md). |
| **Quota collector** | Periodically measures remaining quota per pool and publishes a snapshot the gate reads. The gate never trusts a snapshot older than a fixed staleness bound. |
| **Worker hosts** | Ordinary machines (desktops, VMs, laptops) with agent CLIs installed. A job names its host; the launcher connects, writes a prompt and a run script into an isolated work directory, and starts the agent detached. |

## Flow of a single job

```
Coordinator ──create──▶ Job store ──signal──▶ Dispatcher ──wake──▶ Worker
                                                                      │ claim (CAS)
                                                                      ▼
                                              Agent CLI on a worker host
                                                                      │ result in store
                                                                      ▼
Shepherd ──evaluate──▶ deliver result / open PR / register next job in chain
Reconciler ──patrol──▶ recover or report anything stuck
Quota gate ──in front of create──▶ hold or release by measured pool health
```

## Design principles

- **The store is the truth; every message is a hint.** Any component can be
  restarted, duplicated or lost, and the system converges by re-reading the
  store.
- **The claim is the lock.** Every later transition checks the claim owner
  with a per-run worker token.
- **At-least-once, never exactly-once.** Waking twice is expected and
  harmless; designing it away costs more than it saves.
- **No automatic retries of failed jobs.** A human (or the coordinator)
  requeues after reading the failure, so a broken job cannot loop forever.
  Attempts are counted at claim time, and a job that reaches the attempts
  cap leaves the sweep and is reported instead of consuming the queue.
- **Launch order is FIFO; chained children do not jump the queue.** A
  global concurrency cap counts *claimed* jobs.
- **Separation of roles.** Launching, evaluating and recovering are
  different components, so a failure in one cannot silently corrupt the
  others' view. Generation, rebuttal, audit and decision are likewise kept
  apart in the pipeline conventions (see
  [04-pipeline-conventions.md](04-pipeline-conventions.md)).

## Environment assumptions

- Worker hosts are reachable over SSH (or an equivalent remote shell).
- Agent CLIs run non-interactively: a prompt in, a session that can be
  *resumed* later by id (fix rounds depend on this), and a final report out.
- The signal route requires that the component creating jobs can append to
  a file the dispatcher can read. Verify this one capability before
  building anything else; the whole signal design depends on it.

## Differences in the current implementation

The conceptual architecture above predates the Go binary (`quota-fleet serve`).
The current implementation differs in four places:

- **Quota collector → Quota gate:** No collector is bundled in `quota-fleet`.
  Operators supply an external usage snapshot file
  ([23-usage-snapshot.md](23-usage-snapshot.md)) or use
  [reactive pool cooldown](03-quota-gate.md#reactive-pool-cooldown).
- **Job store → Signal file → Dispatcher:** The Dispatcher polls pending rows
  from a `signal_outbox` table in the SQLite store, appends audit entries to
  the signal file (`signal.jsonl`), and wakes the Launcher in-process.
- **Agent CLI → Job store:** Agent CLIs do not write results to the store. The
  Shepherd inspects worker hosts, collects each finished run's artifacts and
  exit status, evaluates acceptance criteria, and records `done` or `failed` in
  the store.
- **Shepherd → Pull request / follow-up job:** In the current binary, the
  Shepherd evaluates, records `done` or `failed` (and any declared review
  verdict), and delivers results; it does not open pull requests or register
  follow-up jobs. Pull requests come from a chain's publish step (an agent job)
  or the Coordinator, and follow-up jobs come from the
  [Chain Driver](22-chain-runs.md) or the Coordinator
  ([06-implementation-design.md](06-implementation-design.md) §7-4).

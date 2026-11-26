# 03 — Quota gate

## Purpose

Agent subscriptions do not have one quota; they have **pools**. A single
provider may expose several pools (for example, one per model family), each
with its own weekly window and its own short rolling window (such as 5
hours), resetting at independent times. A job is always bound, through its
agent and model choice, to exactly one primary pool.

The gate has three jobs:

1. Never let a job fail just because its pool was empty at launch time.
2. Spend capacity before it expires.
3. Do both from **measurement**, never from estimates.

## Pools and measurement

- A collector periodically publishes a snapshot: per pool, the remaining
  percentage and reset time of the weekly window, and of the short window.
  See [docs/23-usage-snapshot.md](23-usage-snapshot.md) for the formal
  Schema v1 JSON contract, strict validation (`quota-fleet usage validate`),
  `quota.usage_format: legacy | v1`, and atomic deployment recipe.
- A pool is **healthy** when both windows have remaining quota above zero.
  Which window blocked a pool is recorded (`weekly` / `short`).
- If the snapshot is older than the staleness bound (45 minutes in the
  reference deployment), the gate trusts nothing and releases nothing that
  round.
- Pools the collector cannot measure are **outside the gate**. The
  coordinator checks them by eye at registration time; pretending to gate
  an unmeasured pool is worse than not gating it.

## Hold and release

Registration-time procedure for the coordinator:

1. Check the snapshot for the job's pool.
2. Healthy → register the job normally. (Assignment itself already prefers
   soon-expiring pools; see below.)
3. Exhausted → do **not** register. Write a *held entry* containing the
   full creation arguments, the idempotent `request_id`, the pool, any
   acceptable alternative pools, and the measured reset times. Report the
   job as held, not failed.

A periodic gatekeeper (15 minutes in the reference deployment):

- For each held entry, considers the entry's pool plus its declared
  alternative pools — alternatives are only those the coordinator judged
  acceptable at registration time; the gate never invents substitutions.
- Among healthy candidates it picks the pool whose **weekly reset is
  soonest**: capacity that is about to expire is spent first.
- Releases by creating the job with the stored arguments and `request_id`
  (idempotent, so a repeated release is harmless). The title stored in the
  entry is always included.
- A failed release stays held, records `last_error`, and is reported once —
  the same error is never reported twice.

Scope limit, stated honestly: a pool that empties *after* registration but
before launch is outside the gate. Launches normally follow within about a
minute of the signal; a job that still dies of quota is surfaced as a
normal failure and re-registered by the coordinator.

## Burn-down research jobs

Capacity that will reset unused is waste. Under standing operator policy,
the gate may spend it without asking each time, under all of these
conditions:

- The pool is healthy, its weekly reset is within 24 hours, and its weekly
  remainder is at least 40%.
- No held entry wants that pool. **Committed work always outranks
  burn-down.**
- At most one burn-down job per pool per weekly cycle, and at most one per
  gate run.
- The job belongs to a pre-approved category of **read-only research**
  (for example: deep-diving one open issue, drafting improvement proposals
  for a repository). It must not change code, push, open pull requests,
  post externally, or spend money. Its deliverable is a report.
- Topics rotate and are logged, so the same research is never run twice.

## Why this matters

Without the gate, a fleet run on subscriptions degrades to a human
watching dashboards and re-queueing jobs by hand at odd hours. With it,
"the pool resets Tuesday 09:00 with 48% left" stops being an anecdote and
becomes a scheduling input — and a job registered Friday night simply
starts when capacity returns, instead of dying at 01:30.

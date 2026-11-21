# 02 — Job model

## States

`draft → queued → claimed → done | failed | cancelled`

Domain-specific display names, if any, are mapped onto these states in one
place; new state names are never invented per use case.

## Fields (from day one)

`status`, `claimed_at`, `claimed_by`, `attempts`, `last_error`,
`result_ref`, `delivered_at`, `cancel_requested`, `updated_at`, plus the
job payload: title, instructions, acceptance criteria, target agent, target
host, work directory, and an optional `parent_id`.

- **Title is mandatory.** A job without a human-readable title is
  indistinguishable in every list it appears in, and a registered job's
  title cannot be changed afterwards.
- **The signal never carries payload.** Instructions, file paths and any
  personal data stay in the store; the worker re-reads them after claiming.

## Creation and idempotency

- Every creation carries a unique, client-generated `request_id`.
  Re-creating with the same `request_id` returns the existing job
  unchanged, so a double click or a resend after an error can never
  register the same job twice.
- Every action that moves a job into `queued` (submit, manual requeue,
  reset) emits a signal line. A job that enters `queued` silently sits
  still until an unrelated signal or the reconciler arrives.

## Claims and transitions

- Every status transition is a **compare-and-swap**: it writes only when
  the current status matches the expected one, and reports whether it
  wrote. The claim token *is* the worker identity for that run; later
  transitions present it.
- A job whose payload cannot be read or validated is moved to `failed`
  through the queued-state fail path. It must not stay `queued`, where it
  would block the head of the queue on every sweep.
- A manual requeue resets `attempts` to 0: the attempts cap guards
  automatic paths, not explicit human retries.

## Chains

- A job may name a `parent_id`. Children wait until the parent completes.
- **A failed parent auto-cancels its waiting children.** Chains that must
  survive a parent failure are restructured explicitly by the coordinator,
  never left to chance.
- Typical chains are registered up front, at the moment the first job is
  created: implementation → review → gate (fix). The review job identifies
  its target commit itself when it starts, so the chain needs no human
  hand-off between links.

## Delivery

Delivery happens in a fixed order:

1. Mark the job complete, with `result_ref`.
2. Deliver the result to its destination.
3. Record `delivered_at`.

If step 1 fails, delivery stops: the job no longer belongs to this worker.
The reconciler reports `done` jobs whose `delivered_at` is missing.

## Signal format

One JSON object per line, appended as a single write of the stringified
JSON plus a newline:

`v, event_id, job_id, env, src, hop, ts, event`

- `event_id` is a fresh UUID per emit. Dispatchers key their seen-record on
  `event_id`, never on the job id — job ids are only unique inside one
  store and collide across copies.
- `env` carries the store's instance id, read from a meta row created in
  the production store. The dispatcher ignores lines for other instances.
  (Verify that sandbox copies do not carry the row: a full data copy
  carries the same instance id and defeats the marker.)
- The dispatcher parses each line strictly, drops lines that fail the
  field patterns, and ignores a trailing line without a newline — it may
  still be mid-write.
- A failed emit never fails the creating action; the reconciler recovers
  the job later.
- Optional self-continuation: a worker may emit one continuation signal,
  only if its run moved at least one job out of `queued`, with `hop`
  incremented and capped by the dispatcher.

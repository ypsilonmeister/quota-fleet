# 04 — Pipeline conventions

The job substrate moves work. These conventions are what make the work's
*output* trustworthy. They are written for coding work, but the shape
generalises.

## The chain

Once a ticket starts, it runs to merge-ready without returning to the
human:

```
implementation → review → (fix → re-review) → merge-ready
```

- The full chain is registered when the ticket starts, linked by
  `parent_id`. The gate (fix) job branches on the review verdict: no
  findings, no fix.
- **Judgment calls do not stop the chain.** Anything that needs a human
  decision is written onto the pull request and handled at merge time.
- The human's only scheduled decision is the merge itself.

## Role separation

- **Implementation and review are always different model families.** The
  reviewer judges the same materials independently; where a decision needs
  a second opinion, both judgments are presented side by side and the
  human decides.
- **Reviewers never push.** They read, test and report.
- **Implementers may push only their own designated branch**:
  fast-forward only, never the main branch, never force, and only after
  the full test suite passes and the work is committed.
- **Pull requests are created by the coordinator side**, not by workers.
- **Merges are the human's alone.**

## Fix rounds

- A fix resumes the **original implementer's session** by default. The
  context that produced the code is part of the fix's quality.
- To make that possible, every delivery is recorded in an **implementation
  ledger**: pull request, job, agent, host, work directory, branch, and
  the agent's session id (recovered from the host's session logs at
  delivery time).
- Review findings are **reproduced before they are fixed**. A finding that
  cannot be reproduced is reported as such, not silently "fixed".

## Reporting discipline

- Agents preserve **raw error text** in their summaries. A paraphrased
  error is a destroyed diagnostic.
- Working reports (`summary.md` and similar) are **never committed** to
  the branch. The coordinator checks every new pull request's file list
  for contamination.
- Every job is registered with a short human-readable **title** naming the
  task and its role (implementation / review / gate).
- A delivery report distinguishes what was verified from what was not:
  confirmed facts, unverified items, their effect on the conclusion, and
  the verification still needed, in that order.

## Failure handling inside chains

- When an agent dies mid-chain (quota, crash, host loss), the coordinator
  restructures the remaining chain — reassigning to another pool or agent
  family — rather than restarting the ticket. Partial work left by the
  dead agent is inventoried first; usable partial work is taken over, not
  discarded.
- Stacked pull requests carry merge-order notes, including which base
  branches to delete or retarget, because a stack merged in the wrong
  shape silently lands changes on the wrong branch. Integration bugs
  between individually-green stacked changes are expected and are fixed as
  their own small chain, with the root cause recorded (test-side
  assumptions are a common culprit).

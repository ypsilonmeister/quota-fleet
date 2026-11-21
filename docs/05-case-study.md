# 05 — Case study: shipping a real release through the pipeline

The design in this repository was not written in the abstract. It was
extracted from a private deployment that develops
[clipboard-translator](https://github.com/ypsilonmeister/clipboard-translator),
an open-source (MIT) local clipboard-translation app, and it shipped that
app's **v0.11.0** release.

## Scale of one release cycle

Between 17 and 19 November 2026, the pipeline processed more than 60 pull
requests for this single repository (PRs #201–#273), covering:

- a browser-extension line-break preservation fix, verified afterwards on
  a real Chromebook by the maintainer;
- a new application icon, generated as candidates, selected by the
  maintainer, and landed through the standard implement → review → gate
  chain;
- **Wayland support, Phase 1**: nine sub-tasks (session decoupling, portal
  capability degradation, shortcut binding states, a rebind queue, lazy
  capture during reconnect, control-window polish, documentation),
  developed as *stacked* pull requests, including one cross-PR integration
  bug — two changes that were each green alone and failed together — which
  was root-caused to a test-side assumption and fixed as its own chain;
- release automation: the release workflow built all platform artifacts,
  and the package-registry (Scoop) manifest automation ran end-to-end live
  for the first time, opening its own bump pull request.

Every one of those pull requests was implemented by one agent family,
reviewed by another, and merged — or not — by the maintainer.

## Incidents the design absorbed

- **Quota exhaustion mid-chain, twice in one night.** Two chains lost
  their gate jobs when a provider's short window emptied; the coordinator
  restructured the remaining links onto a different agent family instead
  of restarting the tickets. This incident is why the quota gate
  ([03-quota-gate.md](03-quota-gate.md)) holds jobs instead of letting
  them fail.
- **A second agent runtime's per-model quota** that no dashboard showed:
  two large jobs consumed roughly half of a weekly pool in about fifteen
  minutes and died. The lesson — measure per pool, and match job weight to
  pool size — is encoded in the gate's pool definitions.
- **A stacked-merge mishap**: several stacked PRs merged into their stack
  bases instead of the main branch. Nothing was lost; landing PRs were
  created from the bases. The merge-order note convention in
  [04-pipeline-conventions.md](04-pipeline-conventions.md) comes from this
  morning.
- **A host whose pushes returned 403 while its API token reported full
  access.** The eventual root cause was in the agent launch environment,
  not the credentials. The standing rule that came out of it: preserve raw
  errors, and never treat "reinstall the credentials" as a fix without
  evidence.

## What the maintainer actually did

During the v0.11.0 cycle the maintainer's hands-on work was: choosing an
icon from candidates, running one manual verification on a real device,
confirming two semantic decisions on pull requests, merging, and giving
the release go. Everything else — registration, assignment, review,
fix rounds, PR creation, incident restructuring — ran through the
pipeline.

That ratio is the point of the design.

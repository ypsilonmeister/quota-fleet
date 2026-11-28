(function () {
  "use strict";

  const TOKEN_KEY = "quota_fleet_token";
  const AUTH_MODE_KEY = "quota_fleet_auth_mode";
  const JOBS_VIEW_KEY = "qf-jobs-view";
  const JOBS_VIEWS = new Set(["chain", "flat"]);
  const directionHelper =
    (typeof globalThis !== "undefined" && globalThis.__qfDirection) ||
    (typeof window !== "undefined" && window.__qfDirection) ||
    (typeof require === "function" ? require("./direction.js") : null);
  const DIRECTION_KEY = directionHelper ? directionHelper.DIRECTION_KEY : "qf-direction";
  const detectPreferredDirection = directionHelper.detectPreferredDirection;
  const resolveDirection = directionHelper.resolveDirection;
  const i18n =
    (typeof globalThis !== "undefined" && globalThis.__qfI18n) ||
    (typeof window !== "undefined" && window.__qfI18n) ||
    (typeof require === "function" ? require("./i18n.js") : null);
  const t = i18n.t;
  // L(key, params) is a message that is redrawn when the language changes (docs/17).
  const L = i18n.lazy;
  const ALL_STATUSES = [
    "draft",
    "queued",
    "claimed",
    "waiting",
    "done",
    "failed",
    "cancelled"
  ];
  const CANCELLABLE_STATUSES = new Set(["draft", "queued", "waiting"]);
  const REQUEUEABLE_STATUSES = new Set(["failed", "cancelled"]);
  const SUMMARY_STATUS_ORDER = [
    { key: "done", labelKey: "status.done" },
    { key: "claimed", labelKey: "status.claimed" },
    { key: "queued", labelKey: "status.queued" },
    { key: "waiting", labelKey: "status.waiting" },
    { key: "failed", labelKey: "status.failed" },
    { key: "cancelled", labelKey: "status.cancelled" },
    { key: "draft", labelKey: "status.draft" }
  ];

  // ==================== Pure Chain / Tree Helpers ====================
  function buildJobForest(jobs) {
    const byId = new Map();
    if (Array.isArray(jobs)) {
      for (const j of jobs) {
        if (!j || typeof j.id !== "number" || !Number.isFinite(j.id)) {
          continue;
        }
        if (!byId.has(j.id)) {
          byId.set(j.id, j);
        }
      }
    }

    const childrenByParent = new Map();
    const obviousRoots = [];

    for (const j of byId.values()) {
      const pid = j.parent_id;
      const hasValidParent = Boolean(pid && pid !== j.id && byId.has(pid));
      if (hasValidParent) {
        if (!childrenByParent.has(pid)) {
          childrenByParent.set(pid, []);
        }
        childrenByParent.get(pid).push(j);
      } else {
        obviousRoots.push(j);
      }
    }

    for (const list of childrenByParent.values()) {
      list.sort(function (a, b) {
        return a.id - b.id;
      });
    }
    obviousRoots.sort(function (a, b) {
      return a.id - b.id;
    });

    const visited = new Set();

    function buildSubtree(job, depth, isOrphanRoot) {
      visited.add(job.id);
      const node = {
        job: job,
        depth: depth,
        isOrphanRoot: Boolean(isOrphanRoot),
        children: []
      };
      const rawChildren = childrenByParent.get(job.id) || [];
      for (const child of rawChildren) {
        if (visited.has(child.id)) {
          continue;
        }
        node.children.push(buildSubtree(child, depth + 1, false));
      }
      return node;
    }

    const forest = [];

    for (const rootJob of obviousRoots) {
      if (visited.has(rootJob.id)) {
        continue;
      }
      const isOrphanRoot = Boolean(rootJob.parent_id && rootJob.parent_id !== rootJob.id && !byId.has(rootJob.parent_id));
      forest.push(buildSubtree(rootJob, 0, isOrphanRoot));
    }

    // Any remaining unvisited jobs belong to pure parent_id cycles (e.g. A -> B -> A).
    // Break each cycle deterministically by picking the lowest unvisited ID as root.
    if (visited.size < byId.size) {
      const remaining = Array.from(byId.values())
        .filter(function (j) { return !visited.has(j.id); })
        .sort(function (a, b) { return a.id - b.id; });
      for (const cycleJob of remaining) {
        if (visited.has(cycleJob.id)) {
          continue;
        }
        forest.push(buildSubtree(cycleJob, 0, false));
      }
    }

    function maxIdInTree(node, seen) {
      if (!node || !node.job || seen.has(node.job.id)) {
        return 0;
      }
      seen.add(node.job.id);
      let maxId = node.job.id;
      for (const ch of node.children) {
        const childMax = maxIdInTree(ch, seen);
        if (childMax > maxId) {
          maxId = childMax;
        }
      }
      return maxId;
    }

    forest.sort(function (a, b) {
      const maxDiff = maxIdInTree(b, new Set()) - maxIdInTree(a, new Set());
      if (maxDiff !== 0) {
        return maxDiff;
      }
      return b.job.id - a.job.id;
    });

    return forest;
  }

  function summarizeChainProgress(rootNode) {
    const counts = {};
    let total = 0;
    const seen = new Set();

    function walk(node) {
      if (!node || !node.job || seen.has(node.job.id)) {
        return;
      }
      seen.add(node.job.id);
      total += 1;
      const st = node.job.status || "unknown";
      counts[st] = (counts[st] || 0) + 1;
      if (Array.isArray(node.children)) {
        for (const ch of node.children) {
          walk(ch);
        }
      }
    }

    walk(rootNode);
    return { total: total, counts: counts };
  }

  function formatChainProgressSummary(summary) {
    if (!summary || !summary.total) {
      return t("chain.summaryEmpty");
    }
    const counts = summary.counts || {};
    const parts = [];
    const knownKeys = new Set();
    for (const item of SUMMARY_STATUS_ORDER) {
      knownKeys.add(item.key);
      const n = counts[item.key] || 0;
      if (n > 0) {
        parts.push(t("chain.summaryItem", { label: t(item.labelKey), count: n }));
      }
    }
    for (const key of Object.keys(counts)) {
      if (!knownKeys.has(key) && counts[key] > 0) {
        parts.push(t("chain.summaryItem", { label: key, count: counts[key] }));
      }
    }
    if (parts.length === 0) {
      return t("chain.summaryTotal", { total: summary.total });
    }
    return t("chain.summaryBreakdown", {
      total: summary.total,
      parts: parts.join(t("chain.summarySeparator"))
    });
  }

  function flattenChainTree(rootNode) {
    const rows = [];
    const seen = new Set();

    function walk(node, depth, guides, isLast) {
      if (!node || !node.job || seen.has(node.job.id)) {
        return;
      }
      seen.add(node.job.id);
      rows.push({
        job: node.job,
        depth: depth,
        guides: guides.slice(),
        isLast: isLast,
        isRoot: depth === 0,
        isOrphanRoot: Boolean(node.isOrphanRoot),
        summary: depth === 0 ? summarizeChainProgress(node) : null
      });
      const kids = Array.isArray(node.children) ? node.children : [];
      for (let i = 0; i < kids.length; i += 1) {
        const childIsLast = i === kids.length - 1;
        const nextGuides = depth === 0 ? [] : guides.concat([!isLast]);
        walk(kids[i], depth + 1, nextGuides, childIsLast);
      }
    }

    walk(rootNode, 0, [], true);
    return rows;
  }

  function treeContainsStatus(rootNode, targetStatus) {
    if (!targetStatus || targetStatus === "all") {
      return true;
    }
    const seen = new Set();
    function walk(node) {
      if (!node || !node.job || seen.has(node.job.id)) {
        return false;
      }
      seen.add(node.job.id);
      if (node.job.status === targetStatus) {
        return true;
      }
      if (Array.isArray(node.children)) {
        for (const ch of node.children) {
          if (walk(ch)) {
            return true;
          }
        }
      }
      return false;
    }
    return walk(rootNode);
  }

  const CHAIN_PAGE_LIMIT = 200;
  const DEFAULT_MAX_UP_STEPS = 50;
  const DEFAULT_MAX_EXPAND_NODES = 50;
  const DEFAULT_MAX_PAGES_PER_STATUS = 50;
  const DEFAULT_EXPAND_BATCH_SIZE = 10;

  async function fetchChildrenAcrossStatuses(parentID, fetcher, options) {
    const opts = options || {};
    const pageLimit = opts.pageLimit || CHAIN_PAGE_LIMIT;
    const maxPages = opts.maxPagesPerStatus || DEFAULT_MAX_PAGES_PER_STATUS;

    const statusResults = await Promise.all(ALL_STATUSES.map(async function (st) {
      const jobs = [];
      let cursor = 0;
      let pages = 0;
      let truncated = false;
      let error = null;

      while (pages < maxPages) {
        pages += 1;
        let path = "/jobs?status=" + encodeURIComponent(st) +
          "&parent_id=" + encodeURIComponent(String(parentID)) +
          "&limit=" + encodeURIComponent(String(pageLimit));
        if (cursor > 0) {
          path += "&cursor=" + encodeURIComponent(String(cursor));
        }
        let cr;
        try {
          cr = await fetcher(path);
        } catch (err) {
          if (err && err.status === 401) {
            throw err;
          }
          error = {
            parentId: parentID,
            status: st,
            httpStatus: (err && typeof err.status === "number") ? err.status : 0,
            message: (err && err.message) ? String(err.message) : String(err || ""),
            messageKey: "chain.fetchNetworkError"
          };
          break;
        }
        if (!cr || !cr.ok || !cr.data || !Array.isArray(cr.data.jobs)) {
          error = {
            parentId: parentID,
            status: st,
            httpStatus: (cr && typeof cr.status === "number") ? cr.status : 0,
            message: (cr && cr.data && cr.data.error)
              ? String(cr.data.error)
              : (cr && !cr.ok
                ? ("HTTP " + (typeof cr.status === "number" ? cr.status : "?"))
                : ""),
            messageKey: "chain.fetchInvalidResponse"
          };
          break;
        }
        for (const cj of cr.data.jobs) {
          jobs.push(cj);
        }
        const hasMore = Boolean(cr.data.has_more);
        const nextCursor = Number(cr.data.next_cursor || 0);
        if (!hasMore) {
          break;
        }
        if (!Number.isInteger(nextCursor) || nextCursor <= cursor) {
          truncated = true;
          break;
        }
        cursor = nextCursor;
        if (pages >= maxPages) {
          truncated = true;
        }
      }

      return { jobs: jobs, truncated: truncated, error: error };
    }));

    const seen = new Set();
    const children = [];
    const errors = [];
    let truncated = false;
    for (const sr of statusResults) {
      if (sr.truncated) {
        truncated = true;
      }
      if (sr.error) {
        errors.push(sr.error);
      }
      for (const cj of sr.jobs) {
        if (!cj || typeof cj.id !== "number" || seen.has(cj.id)) {
          continue;
        }
        seen.add(cj.id);
        children.push(cj);
      }
    }
    return {
      jobs: children,
      truncated: truncated,
      errors: errors,
      hasErrors: errors.length > 0
    };
  }

  async function fetchFullJobChain(job, fetcher, options) {
    const opts = options || {};
    const maxUpSteps = opts.maxUpSteps || DEFAULT_MAX_UP_STEPS;
    const maxExpandNodes = opts.maxExpandNodes || DEFAULT_MAX_EXPAND_NODES;
    const batchSize = opts.batchSize || DEFAULT_EXPAND_BATCH_SIZE;

    const knownJobs = new Map();
    knownJobs.set(job.id, job);

    // 1. Walk upward via parent_id to locate the chain root (cycle-safe).
    let current = job;
    const seenUp = new Set([job.id]);
    const ancestorPath = [job.id];
    let steps = 0;
    let upTruncated = false;
    while (current.parent_id && Number.isInteger(current.parent_id) && current.parent_id > 0) {
      if (seenUp.has(current.parent_id)) {
        break;
      }
      if (steps >= maxUpSteps) {
        upTruncated = true;
        break;
      }
      seenUp.add(current.parent_id);
      steps += 1;
      const parentRes = await fetcher("/jobs/" + current.parent_id);
      if (!parentRes || !parentRes.ok || !parentRes.data || typeof parentRes.data.id !== "number") {
        break;
      }
      knownJobs.set(parentRes.data.id, parentRes.data);
      ancestorPath.push(parentRes.data.id);
      current = parentRes.data;
    }
    ancestorPath.reverse(); // [root, ..., parent, job]

    const expanded = new Set();
    let pagesTruncated = false;
    let currentJobChildrenTruncated = false;
    const fetchErrors = [];
    const currentJobChildrenErrors = [];

    const priorityQueue = [];
    const priorityQueued = new Set();
    const siblingQueue = [];
    const siblingQueued = new Set();

    function enqueuePriority(id) {
      if (!expanded.has(id) && !priorityQueued.has(id)) {
        priorityQueued.add(id);
        priorityQueue.push(id);
      }
    }

    function enqueueSibling(id) {
      if (!expanded.has(id) && !priorityQueued.has(id) && !siblingQueued.has(id)) {
        siblingQueued.add(id);
        siblingQueue.push(id);
      }
    }

    async function expandBatch(ids, targetQueueKind) {
      const results = await Promise.all(ids.map(function (id) {
        return fetchChildrenAcrossStatuses(id, fetcher, opts).then(function (res) {
          return { id: id, res: res };
        });
      }));
      for (const item of results) {
        if (item.res.truncated) {
          pagesTruncated = true;
          if (item.id === job.id) {
            currentJobChildrenTruncated = true;
          }
        }
        if (Array.isArray(item.res.errors) && item.res.errors.length > 0) {
          for (const errItem of item.res.errors) {
            fetchErrors.push(errItem);
            if (item.id === job.id) {
              currentJobChildrenErrors.push(errItem);
            }
          }
        }
        for (const cj of item.res.jobs) {
          if (!cj || typeof cj.id !== "number") {
            continue;
          }
          if (!knownJobs.has(cj.id)) {
            knownJobs.set(cj.id, cj);
          }
          if (targetQueueKind === "priority") {
            enqueuePriority(cj.id);
          } else {
            enqueueSibling(cj.id);
          }
        }
      }
    }

    // 2. Always expand the currently displayed job first so its direct children
    //    are guaranteed to be fetched regardless of sibling count or BFS budget.
    expanded.add(job.id);
    await expandBatch([job.id], "priority");

    // 3. Expand the ancestor spine (root -> ... -> parent) so the path to the
    //    current job and its siblings are included in the mini-tree.
    const ancestorsToExpand = ancestorPath.filter(function (id) {
      return !expanded.has(id);
    });
    for (let i = 0; i < ancestorsToExpand.length && expanded.size < maxExpandNodes; ) {
      const budget = Math.min(batchSize, maxExpandNodes - expanded.size);
      const batch = [];
      while (i < ancestorsToExpand.length && batch.length < budget) {
        const aid = ancestorsToExpand[i];
        i += 1;
        if (!expanded.has(aid)) {
          expanded.add(aid);
          batch.push(aid);
        }
      }
      if (batch.length > 0) {
        await expandBatch(batch, "sibling");
      }
    }
    for (const aid of ancestorsToExpand) {
      if (!expanded.has(aid)) {
        enqueueSibling(aid);
      }
    }

    // 4. BFS downward: prioritize descendants of the current job first,
    //    then expand remaining siblings/branches within maxExpandNodes.
    while (expanded.size < maxExpandNodes && (priorityQueue.length > 0 || siblingQueue.length > 0)) {
      const budget = Math.min(batchSize, maxExpandNodes - expanded.size);
      const priorityBatch = [];
      while (priorityQueue.length > 0 && priorityBatch.length < budget) {
        const nextID = priorityQueue.shift();
        if (!expanded.has(nextID)) {
          expanded.add(nextID);
          priorityBatch.push(nextID);
        }
      }
      if (priorityBatch.length > 0) {
        await expandBatch(priorityBatch, "priority");
        continue;
      }

      const siblingBatch = [];
      while (siblingQueue.length > 0 && siblingBatch.length < budget) {
        const nextID = siblingQueue.shift();
        if (!expanded.has(nextID)) {
          expanded.add(nextID);
          siblingBatch.push(nextID);
        }
      }
      if (siblingBatch.length > 0) {
        await expandBatch(siblingBatch, "sibling");
      }
    }

    const unexpandedIDs = new Set();
    for (const id of priorityQueue) {
      if (!expanded.has(id)) {
        unexpandedIDs.add(id);
      }
    }
    for (const id of siblingQueue) {
      if (!expanded.has(id)) {
        unexpandedIDs.add(id);
      }
    }

    const expandTruncated = unexpandedIDs.size > 0;
    const truncated = upTruncated || pagesTruncated || expandTruncated;
    const hasFetchErrors = fetchErrors.length > 0;
    const currentJobChildrenFailed = currentJobChildrenErrors.length > 0;
    const currentJobChildrenComplete = !currentJobChildrenTruncated && !currentJobChildrenFailed;

    return {
      jobs: Array.from(knownJobs.values()),
      truncated: truncated,
      upTruncated: upTruncated,
      pagesTruncated: pagesTruncated,
      currentJobChildrenTruncated: currentJobChildrenTruncated,
      expandTruncated: expandTruncated,
      unexpandedCount: unexpandedIDs.size,
      fetchErrors: fetchErrors,
      hasFetchErrors: hasFetchErrors,
      currentJobChildrenErrors: currentJobChildrenErrors,
      currentJobChildrenFailed: currentJobChildrenFailed,
      currentJobChildrenComplete: currentJobChildrenComplete
    };
  }

  function formatFetchErrorDetail(errItem) {
    if (!errItem) {
      return t("chain.fetchGenericError");
    }
    const httpStatus = Number(errItem.httpStatus || 0);
    // messageKey describes a failure that came without a message of its own.
    const msg = errItem.message
      ? String(errItem.message)
      : (errItem.messageKey ? t(errItem.messageKey) : "");
    if (httpStatus > 0) {
      if (msg && msg.indexOf("HTTP " + httpStatus) === 0) {
        return msg;
      }
      return msg ? ("HTTP " + httpStatus + " " + msg) : ("HTTP " + httpStatus);
    }
    return msg || t("chain.fetchGenericError");
  }

  function formatChainFetchErrorNotice(chainMeta) {
    if (!chainMeta || !Array.isArray(chainMeta.fetchErrors) || chainMeta.fetchErrors.length === 0) {
      return null;
    }
    const maxItems = 6;
    const items = chainMeta.fetchErrors.slice(0, maxItems).map(function (errItem) {
      const parentLabel = (errItem && errItem.parentId !== undefined && errItem.parentId !== null)
        ? ("#" + errItem.parentId)
        : t("chain.unknownParent");
      const statusLabel = (errItem && errItem.status) ? String(errItem.status) : t("chain.unknownStatus");
      return parentLabel + " [" + statusLabel + ": " + formatFetchErrorDetail(errItem) + "]";
    });
    const extra = chainMeta.fetchErrors.length > maxItems
      ? t("chain.errorMore", { count: chainMeta.fetchErrors.length - maxItems })
      : "";
    return t("chain.errorNotice", {
      items: items.join(t("chain.errorSeparator")),
      extra: extra
    });
  }

  function formatDirectChildrenFetchErrorNotice(chainMeta, currentJobID) {
    if (!chainMeta || !Array.isArray(chainMeta.currentJobChildrenErrors) || chainMeta.currentJobChildrenErrors.length === 0) {
      return null;
    }
    const items = chainMeta.currentJobChildrenErrors.map(function (errItem) {
      const statusLabel = (errItem && errItem.status) ? String(errItem.status) : t("chain.unknownStatus");
      return statusLabel + " [" + formatFetchErrorDetail(errItem) + "]";
    });
    const targetLabel = (currentJobID !== undefined && currentJobID !== null)
      ? t("common.jobLabel", { id: currentJobID })
      : t("chain.currentJob");
    return t("chain.directErrorNotice", {
      target: targetLabel,
      items: items.join(t("chain.errorSeparator"))
    });
  }

  function formatChainTruncationNotice(chainMeta) {
    if (!chainMeta || !chainMeta.truncated) {
      return null;
    }
    const reasons = [];
    if (chainMeta.expandTruncated) {
      reasons.push(t("chain.truncatedExpand", { count: chainMeta.unexpandedCount }));
    }
    if (chainMeta.upTruncated) {
      reasons.push(t("chain.truncatedUp"));
    }
    if (chainMeta.pagesTruncated) {
      reasons.push(t("chain.truncatedPages"));
    }
    const hasErrors = Boolean(
      chainMeta.hasFetchErrors ||
      chainMeta.currentJobChildrenFailed ||
      (Array.isArray(chainMeta.fetchErrors) && chainMeta.fetchErrors.length > 0) ||
      (Array.isArray(chainMeta.currentJobChildrenErrors) && chainMeta.currentJobChildrenErrors.length > 0)
    );
    const directNote = (chainMeta.currentJobChildrenTruncated || hasErrors)
      ? ""
      : t("chain.truncatedDirectComplete");
    return t("chain.truncatedNotice", {
      reasons: reasons.length > 0
        ? reasons.join(t("chain.truncatedSeparator"))
        : t("chain.truncatedLimit"),
      note: directNote
    });
  }

  // ==================== Pure Form Helpers (Agent / Host / Model / Pool) ====================
  function getHostsForAgent(meta, agent) {
    const allHosts = (meta && Array.isArray(meta.hosts)) ? meta.hosts : [];
    const target = String(agent || "").trim();
    if (!target || !meta || !meta.host_agents || typeof meta.host_agents !== "object") {
      return allHosts.slice();
    }
    if (Object.keys(meta.host_agents).length === 0) {
      return allHosts.slice();
    }
    return allHosts.filter(function (h) {
      const list = meta.host_agents[h];
      if (!Array.isArray(list) || list.length === 0) {
        return true;
      }
      return list.includes(target);
    });
  }

  function formatNoMatchingHostNotice(agent, filteredHosts) {
    const a = String(agent || "").trim();
    if (!a || (Array.isArray(filteredHosts) && filteredHosts.length > 0)) {
      return "";
    }
    return t("form.noMatchingHost", { agent: a });
  }

  function getModelsForHostAgent(meta, host, agent) {
    const h = String(host || "").trim();
    const a = String(agent || "").trim();
    if (!h || !a || !meta || !meta.models || typeof meta.models !== "object") {
      return [];
    }
    const byAgent = meta.models[h];
    if (!byAgent || typeof byAgent !== "object") {
      return [];
    }
    const list = byAgent[a];
    if (!Array.isArray(list)) {
      return [];
    }
    return list.filter(function (m) {
      return m && typeof m.id === "string" && m.id.trim() !== "";
    });
  }

  function formatModelOptionLabel(modelItem) {
    if (!modelItem) {
      return "";
    }
    const id = String(modelItem.id || "").trim();
    const dn = String(modelItem.display_name || "").trim();
    if (dn && dn !== id) {
      return dn + " (" + id + ")";
    }
    return id;
  }

  function deriveModelFamily(modelID) {
    const m = String(modelID || "").trim().toLowerCase();
    if (m === "gemini" || m.startsWith("gemini-")) {
      return "gemini";
    }
    if (m === "claude" || m.startsWith("claude-")) {
      return "claude";
    }
    if (m === "gpt" || m.startsWith("gpt-")) {
      return "gpt";
    }
    return "unknown";
  }

  function matchesModelFamily(model, family) {
    const canonical = deriveModelFamily(model);
    const f = String(family || "").trim().toLowerCase();
    if (!f || canonical === "unknown") {
      return false;
    }
    if (canonical === f) {
      return true;
    }
    const tokens = f.split(/[-/,_]+/);
    for (const tok of tokens) {
      if (tok && tok === canonical) {
        return true;
      }
    }
    return false;
  }

  function derivePoolForSelection(meta, agent, model) {
    const a = String(agent || "").trim().toLowerCase();
    const m = String(model || "").trim();
    if (!a || !meta || !meta.pool_rules || typeof meta.pool_rules !== "object") {
      return "";
    }
    const poolNames = Object.keys(meta.pool_rules).sort();
    for (const poolName of poolNames) {
      const rules = meta.pool_rules[poolName];
      if (!Array.isArray(rules)) {
        continue;
      }
      for (const rule of rules) {
        if (!rule || String(rule.agent || "").trim().toLowerCase() !== a) {
          continue;
        }
        const fam = String(rule.model_family || "").trim();
        if (!fam || matchesModelFamily(m, fam)) {
          return poolName;
        }
      }
    }
    return "";
  }

  function formatDerivedPoolLabel(poolName) {
    const p = String(poolName || "").trim();
    if (p) {
      return t("form.derivedPool", { pool: p });
    }
    return t("form.derivedPoolNone");
  }

  function pad2(n) {
    return String(n).padStart(2, "0");
  }

  function formatUtcOffset(date) {
    const d = date instanceof Date && !Number.isNaN(date.getTime()) ? date : new Date();
    const offsetMin = -d.getTimezoneOffset();
    if (offsetMin === 0) {
      return "UTC";
    }
    const sign = offsetMin > 0 ? "+" : "-";
    const abs = Math.abs(offsetMin);
    return "UTC" + sign + pad2(Math.floor(abs / 60)) + ":" + pad2(abs % 60);
  }

  function getDisplayTimeZone(refDate) {
    try {
      if (typeof Intl !== "undefined" && Intl && typeof Intl.DateTimeFormat === "function") {
        const resolved = Intl.DateTimeFormat().resolvedOptions();
        if (resolved && typeof resolved.timeZone === "string" && resolved.timeZone.trim() !== "") {
          const tz = resolved.timeZone.trim();
          if (tz === "Etc/UTC" || tz === "Etc/GMT" || tz === "GMT" || tz === "+00:00" || tz === "-00:00") {
            return "UTC";
          }
          if (tz.indexOf("Etc/GMT") === 0 || /^[+-]\d{2}:\d{2}$/.test(tz)) {
            return formatUtcOffset(refDate);
          }
          return tz;
        }
      }
    } catch (_err) {
      // Fall through to numeric UTC offset below.
    }
    return formatUtcOffset(refDate);
  }

  function parseValidDate(iso) {
    if (typeof iso !== "string" || iso.trim() === "") {
      return null;
    }
    const d = new Date(iso);
    return Number.isNaN(d.getTime()) ? null : d;
  }

  function formatWeeklyResetTime(iso) {
    const d = parseValidDate(iso);
    if (!d) {
      return "";
    }
    return (d.getMonth() + 1) + "/" + d.getDate() + " " + pad2(d.getHours()) + ":" + pad2(d.getMinutes());
  }

  function formatShortResetTime(iso) {
    const d = parseValidDate(iso);
    if (!d) {
      return "";
    }
    return pad2(d.getHours()) + ":" + pad2(d.getMinutes());
  }

  function formatPoolResetLabels(ps) {
    const item = ps && typeof ps === "object" ? ps : {};
    const shortWindow = (typeof item.short_window === "string" && item.short_window.trim())
      ? item.short_window.trim()
      : "5h";
    const weeklyRaw = typeof item.weekly_reset_at === "string" ? item.weekly_reset_at.trim() : "";
    const shortRaw = typeof item.short_reset_at === "string" ? item.short_reset_at.trim() : "";

    const weeklyDate = parseValidDate(weeklyRaw);
    const shortDate = parseValidDate(shortRaw);
    const weeklyFormatted = weeklyDate ? formatWeeklyResetTime(weeklyRaw) : "";
    const shortFormatted = shortDate ? formatShortResetTime(shortRaw) : "";

    const weeklyTz = weeklyDate ? getDisplayTimeZone(weeklyDate) : "";
    const shortTz = shortDate ? getDisplayTimeZone(shortDate) : "";
    const hasMixedTimeZones = Boolean(weeklyTz && shortTz && weeklyTz !== shortTz);
    const timeZone = hasMixedTimeZones ? "" : (weeklyTz || shortTz);

    let weeklyText;
    let weeklyMissing = false;
    if (weeklyFormatted) {
      const wt = (item.weekly_reset_at_approx ? ("~" + weeklyFormatted) : weeklyFormatted)
        + (hasMixedTimeZones ? (" (" + weeklyTz + ")") : "");
      weeklyText = t("dashboard.weeklyReset", { time: wt });
    } else {
      weeklyText = t("dashboard.weeklyResetMissing");
      weeklyMissing = true;
    }

    let shortText;
    let shortMissing = false;
    if (shortFormatted) {
      const st = (item.short_reset_at_approx ? ("~" + shortFormatted) : shortFormatted)
        + (hasMixedTimeZones ? (" (" + shortTz + ")") : "");
      shortText = t("dashboard.shortReset", { window: shortWindow, time: st });
    } else if (item.short_applicable === false) {
      shortText = t("dashboard.shortResetNotApplicable", { window: shortWindow });
      shortMissing = true;
    } else {
      shortText = t("dashboard.shortResetMissing", { window: shortWindow });
      shortMissing = true;
    }

    const labelText = timeZone
      ? t("dashboard.resetLabelWithTz", { tz: timeZone })
      : t("dashboard.resetLabel");

    return {
      labelText: labelText,
      timeZone: timeZone,
      weeklyTimeZone: weeklyTz,
      shortTimeZone: shortTz,
      weeklyText: weeklyText,
      weeklyMissing: weeklyMissing,
      weeklyRaw: weeklyFormatted ? weeklyRaw : "",
      shortText: shortText,
      shortMissing: shortMissing,
      shortRaw: shortFormatted ? shortRaw : ""
    };
  }

  if (typeof module !== "undefined" && module.exports) {
    module.exports = {
      detectPreferredDirection: detectPreferredDirection,
      resolveDirection: resolveDirection,
      buildJobForest: buildJobForest,
      summarizeChainProgress: summarizeChainProgress,
      formatChainProgressSummary: formatChainProgressSummary,
      flattenChainTree: flattenChainTree,
      treeContainsStatus: treeContainsStatus,
      fetchChildrenAcrossStatuses: fetchChildrenAcrossStatuses,
      fetchFullJobChain: fetchFullJobChain,
      formatChainFetchErrorNotice: formatChainFetchErrorNotice,
      formatDirectChildrenFetchErrorNotice: formatDirectChildrenFetchErrorNotice,
      formatChainTruncationNotice: formatChainTruncationNotice,
      getHostsForAgent: getHostsForAgent,
      formatNoMatchingHostNotice: formatNoMatchingHostNotice,
      getModelsForHostAgent: getModelsForHostAgent,
      formatModelOptionLabel: formatModelOptionLabel,
      deriveModelFamily: deriveModelFamily,
      matchesModelFamily: matchesModelFamily,
      derivePoolForSelection: derivePoolForSelection,
      formatDerivedPoolLabel: formatDerivedPoolLabel,
      getDisplayTimeZone: getDisplayTimeZone,
      formatWeeklyResetTime: formatWeeklyResetTime,
      formatShortResetTime: formatShortResetTime,
      formatPoolResetLabels: formatPoolResetLabels
    };
  }

  if (typeof document === "undefined" || typeof window === "undefined") {
    return;
  }

  const notifyHelper = globalThis.__qfNotify;
  // List views that are re-fetched when a job event arrives (docs/14).
  const LIVE_ROUTES = new Set(["dashboard", "jobs", "deliveries"]);

  const state = {
    meta: null,
    status: null,
    authenticated: false,
    // renderSeq changes whenever the view is redrawn by the user; a background
    // refresh started before that is discarded.
    renderSeq: 0,
    rendering: 0,
    viewDirty: false,
    flatPages: 1,
    // The button of the latest unanswered "load more", and the number of clicks.
    loadMoreButton: null,
    loadMoreSeq: 0
  };

  const dom = {
    app: document.getElementById("app"),
    authGate: document.getElementById("auth-gate"),
    authForm: document.getElementById("auth-form"),
    authTokenInput: document.getElementById("auth-token-input"),
    authError: document.getElementById("auth-error"),
    btnConnectNoAuth: document.getElementById("btn-connect-no-auth"),
    btnNotify: document.getElementById("btn-notify"),
    btnRefresh: document.getElementById("btn-refresh"),
    btnToken: document.getElementById("btn-token"),
    directionSwitch: document.getElementById("direction-switch"),
    localeSwitch: document.getElementById("locale-switch"),
    metaBadges: document.getElementById("meta-badges"),
    bannerArea: document.getElementById("banner-area")
  };

  function readStoredDirection() {
    return directionHelper.readStoredDirection();
  }

  function applyDirection(dir, persist) {
    return directionHelper.applyDirection(dir, persist);
  }

  applyDirection(readStoredDirection(), false);

  // The jobs list view the user picked with the toggle (docs/07).
  function readStoredJobsView() {
    try {
      return localStorage.getItem(JOBS_VIEW_KEY);
    } catch (_err) {
      return null;
    }
  }

  function storeJobsView(jobsView) {
    try {
      localStorage.setItem(JOBS_VIEW_KEY, jobsView);
    } catch (_err) {
      // Storage may be unavailable; the URL still carries the view.
    }
  }

  // resolveJobsView returns the first valid one of: the URL's view parameter, the view
  // this history entry was already shown in, the stored choice. The default is chain.
  function resolveJobsView(searchParams) {
    const candidates = [
      searchParams && searchParams.get("view"),
      history.state && history.state.jobsView,
      readStoredJobsView()
    ];
    return candidates.find(function (v) { return JOBS_VIEWS.has(v); }) || "chain";
  }

  function el(tag, attrs, children) {
    const node = document.createElement(tag);
    if (attrs) {
      for (const key of Object.keys(attrs)) {
        const val = attrs[key];
        if (val === undefined || val === null || val === false) {
          continue;
        }
        if (key === "className") {
          node.className = String(val);
        } else if (key === "text") {
          i18n.setText(node, val);
        } else if (key === "hidden") {
          node.hidden = Boolean(val);
        } else if (key === "disabled") {
          node.disabled = Boolean(val);
        } else if (key === "checked") {
          node.checked = Boolean(val);
        } else if (key === "selected") {
          node.selected = Boolean(val);
        } else if (key === "required") {
          node.required = Boolean(val);
        } else if (key === "readOnly") {
          node.readOnly = Boolean(val);
        } else if (key === "value") {
          node.value = String(val);
        } else if (key.startsWith("on") && typeof val === "function") {
          node.addEventListener(key.slice(2).toLowerCase(), val);
        } else if (key === "dataset" && typeof val === "object") {
          for (const dk of Object.keys(val)) {
            node.dataset[dk] = String(val[dk]);
          }
        } else {
          i18n.setAttr(node, key, val);
        }
      }
    }
    if (children !== undefined && children !== null) {
      const list = Array.isArray(children) ? children : [children];
      for (const child of list) {
        if (child === undefined || child === null || child === false) {
          continue;
        }
        if (typeof child === "string" || typeof child === "number") {
          node.appendChild(document.createTextNode(String(child)));
        } else if (typeof child === "function") {
          const textNode = document.createTextNode("");
          i18n.setText(textNode, child);
          node.appendChild(textNode);
        } else {
          node.appendChild(child);
        }
      }
    }
    return node;
  }

  function navLink(href, text, className) {
    return el("a", {
      href: href,
      className: className || "",
      dataset: { nav: "true" },
      text: text
    });
  }

  function statusBadge(value) {
    const label = value || "-";
    return el("span", {
      className: "badge badge-" + String(label).replace(/[^a-zA-Z0-9_-]/g, ""),
      text: label === "cooldown" ? L("cooldown.badge") : label
    });
  }


  function jobStatusNodes(job) {
    const nodes = [statusBadge(job.status)];
    if (job.status !== "queued" || !state.meta || state.meta.gate_mode !== "enforce") return nodes;
    const pool = job.pool || derivePoolForSelection(state.meta, job.agent, job.model);
    const summary = (state.meta.pool_summaries || []).find(function (ps) {
      return ps.pool === pool && ps.cooldown && ps.cooldown.active;
    });
    if (summary && Date.parse(summary.cooldown.until) > Date.now()) {
      nodes.push(el("div", { className: "small muted", text: L("cooldown.queued", { until: formatTime(summary.cooldown.until) }) }));
    }
    return nodes;
  }

function formatTime(iso) {
    if (!iso) {
      return "-";
    }
    const d = new Date(iso);
    if (Number.isNaN(d.getTime())) {
      return String(iso);
    }
    const pad = function (n) {
      return String(n).padStart(2, "0");
    };
    return (
      d.getFullYear() + "-" +
      pad(d.getMonth() + 1) + "-" +
      pad(d.getDate()) + " " +
      pad(d.getHours()) + ":" +
      pad(d.getMinutes()) + ":" +
      pad(d.getSeconds())
    );
  }

  function generateRequestID() {
    if (window.crypto && typeof window.crypto.randomUUID === "function") {
      return "ui-" + window.crypto.randomUUID();
    }
    const bytes = new Uint8Array(16);
    if (window.crypto && typeof window.crypto.getRandomValues === "function") {
      window.crypto.getRandomValues(bytes);
    } else {
      for (let i = 0; i < bytes.length; i += 1) {
        bytes[i] = Math.floor(Math.random() * 256);
      }
    }
    let hex = "";
    for (let i = 0; i < bytes.length; i += 1) {
      hex += bytes[i].toString(16).padStart(2, "0");
    }
    return "ui-" + hex;
  }

  function getStoredToken() {
    return sessionStorage.getItem(TOKEN_KEY) || "";
  }

  function isNoAuthMode() {
    return sessionStorage.getItem(AUTH_MODE_KEY) === "none";
  }

  function hasCredentialsConfigured() {
    return getStoredToken() !== "" || isNoAuthMode();
  }

  function clearBanner() {
    dom.bannerArea.replaceChildren();
  }

  function showBanner(kind, message, extraNode) {
    const box = el("div", { className: "alert alert-" + kind }, [
      el("div", { text: message }),
      extraNode || null
    ]);
    dom.bannerArea.replaceChildren(box);
  }

  // i18nError carries its message function, so that the error can be redrawn.
  function i18nError(message) {
    const err = new Error(i18n.resolve(message));
    err.localized = message;
    return err;
  }

  function errorText(err) {
    if (err && err.localized) {
      return i18n.resolve(err.localized);
    }
    return err ? err.message : String(err);
  }

  // errorAlert is the message "key: <error>" for a caught error.
  function errorAlert(key, err) {
    return function () {
      return t(key, { message: errorText(err) });
    };
  }

  function showAuthGate(errorMessage) {
    state.authenticated = false;
    dom.app.hidden = true;
    dom.authGate.hidden = false;
    dom.authTokenInput.value = getStoredToken();
    if (errorMessage) {
      i18n.setText(dom.authError, errorMessage);
      dom.authError.hidden = false;
    } else {
      i18n.setText(dom.authError, "");
      dom.authError.hidden = true;
    }
    dom.authTokenInput.focus();
  }

  function hideAuthGate() {
    dom.authGate.hidden = true;
    dom.app.hidden = false;
    i18n.setText(dom.authError, "");
    dom.authError.hidden = true;
  }

  async function apiFetch(path, options) {
    const opts = options ? Object.assign({}, options) : {};
    const headers = new Headers(opts.headers || {});
    headers.set("Accept", "application/json");
    const token = getStoredToken();
    if (token) {
      headers.set("Authorization", "Bearer " + token);
    }
    if (opts.body !== undefined && !headers.has("Content-Type")) {
      headers.set("Content-Type", "application/json");
    }
    opts.headers = headers;

    const resp = await fetch(path, opts);
    let data = null;
    const text = await resp.text();
    if (text) {
      try {
        data = JSON.parse(text);
      } catch (_err) {
        data = { raw: text };
      }
    }

    if (resp.status === 401) {
      const msg = (data && data.error) ? data.error : L("auth.required");
      showAuthGate(msg);
      const err = i18nError(msg);
      err.status = 401;
      err.data = data;
      throw err;
    }

    return { ok: resp.ok, status: resp.status, data: data };
  }

  // errorMessageFromResp returns the server's error message, or else the message of
  // fallbackKey with the HTTP status.
  function errorMessageFromResp(res, fallbackKey, params) {
    return function () {
      if (res && res.data && res.data.error) {
        return String(res.data.error);
      }
      return t(fallbackKey, params) + " (HTTP " + (res ? res.status : "?") + ")";
    };
  }

  async function refreshHeaderMeta() {
    try {
      const results = await Promise.all([
        apiFetch("/meta"),
        apiFetch("/status")
      ]);
      if (results[0].ok && results[0].data) {
        state.meta = results[0].data;
      }
      if (results[1].ok && results[1].data) {
        state.status = results[1].data;
      }
      renderMetaBadges();
    } catch (_err) {
      // 401 is handled in apiFetch; other errors are surfaced by individual views.
    }
  }

  function renderMetaBadges() {
    const items = [];
    if (state.status && state.status.instance_id) {
      items.push(el("span", { className: "badge", text: "id: " + state.status.instance_id }));
    }
    if (state.meta && state.meta.gate_mode) {
      items.push(el("span", { className: "badge", text: "gate: " + state.meta.gate_mode }));
    }
    if (state.meta && state.meta.evaluator_mode) {
      items.push(el("span", { className: "badge", text: "eval: " + state.meta.evaluator_mode }));
    }
    dom.metaBadges.replaceChildren.apply(dom.metaBadges, items);
  }

  function parseRoute(urlObj) {
    const p = urlObj.pathname;
    if (p === "/ui" || p === "/ui/") {
      return { name: "dashboard" };
    }
    if (p === "/ui/jobs") {
      return { name: "jobs", search: urlObj.searchParams };
    }
    if (p === "/ui/jobs/new") {
      return { name: "new-job" };
    }
    const jobMatch = p.match(/^\/ui\/jobs\/([^/]+)$/);
    if (jobMatch) {
      return { name: "job-detail", id: decodeURIComponent(jobMatch[1]) };
    }
    if (p === "/ui/gate/held") {
      return { name: "held", search: urlObj.searchParams };
    }
    if (p === "/ui/gate/decisions") {
      return { name: "decisions", search: urlObj.searchParams };
    }
    if (p === "/ui/deliveries") {
      return { name: "deliveries", search: urlObj.searchParams };
    }
    if (p === "/ui/agents") {
      return { name: "agents" };
    }
    return { name: "not-found", path: p };
  }

  function updateActiveNav(routeName) {
    const links = document.querySelectorAll(".nav-link[data-route]");
    for (const link of links) {
      const target = link.getAttribute("data-route");
      const active = target === routeName || (routeName === "job-detail" && target === "jobs");
      link.classList.toggle("active", active);
    }
  }

  function navigate(href, replace) {
    clearBanner();
    if (replace) {
      history.replaceState(null, "", href);
    } else {
      history.pushState(null, "", href);
    }
    void renderCurrentRoute();
  }

  function setLoading(title) {
    dom.app.replaceChildren(
      el("div", { className: "page-header" }, [el("h1", { text: title })]),
      el("div", { className: "card empty-state", text: L("common.loading") })
    );
  }

  async function renderCurrentRoute() {
    if (!hasCredentialsConfigured()) {
      showAuthGate("");
      return;
    }
    hideAuthGate();

    const urlObj = new URL(window.location.href);
    const route = parseRoute(urlObj);
    updateActiveNav(route.name);

    state.renderSeq += 1;
    state.rendering += 1;
    state.viewDirty = false;
    try {
      switch (route.name) {
        case "dashboard":
          await renderDashboard();
          break;
        case "jobs":
          await renderJobsList(route.search);
          break;
        case "new-job":
          await renderNewJobForm();
          break;
        case "job-detail":
          await renderJobDetail(route.id);
          break;
        case "held":
          await renderHeldJobs(route.search);
          break;
        case "decisions":
          await renderGateDecisions(route.search);
          break;
        case "deliveries":
          await renderDeliveries(route.search);
          break;
        case "agents":
          await renderAgentVersions();
          break;
        default:
          dom.app.replaceChildren(
            el("div", { className: "card" }, [
              el("h1", { text: L("page.notFound") }),
              el("p", { className: "muted", text: route.path }),
              navLink("/ui/", L("page.backToDashboard"), "btn btn-primary")
            ])
          );
      }
      state.authenticated = true;
    } catch (err) {
      if (err && err.status === 401) {
        return;
      }
      dom.app.replaceChildren(
        el("div", { className: "alert alert-error", text: errorAlert("common.error", err) })
      );
    } finally {
      state.rendering -= 1;
    }
  }

  // ==================== Live list refresh (job events) ====================
  function isEditingView() {
    const active = document.activeElement;
    const focused = Boolean(active && dom.app.contains(active) && /^(INPUT|SELECT|TEXTAREA)$/.test(active.tagName));
    return focused || state.viewDirty;
  }

  // True while the displayed list waits for another page; redrawing it would drop that page.
  function isLoadingMore() {
    return Boolean(state.loadMoreButton && state.loadMoreButton.isConnected);
  }

  // refreshListView redraws the current list view without a loading placeholder.
  // It returns false when it has to be retried later (a render, "load more" or user input
  // is in progress).
  async function refreshListView() {
    const route = parseRoute(new URL(window.location.href));
    if (!LIVE_ROUTES.has(route.name) || dom.app.hidden || !hasCredentialsConfigured()) {
      return true;
    }
    if (state.rendering > 0 || isLoadingMore() || isEditingView()) {
      return false;
    }
    const seq = state.renderSeq;
    const loadMoreSeq = state.loadMoreSeq;
    const view = {
      isStale: function () {
        return seq !== state.renderSeq || loadMoreSeq !== state.loadMoreSeq ||
          dom.app.hidden || isEditingView();
      }
    };
    if (route.name === "dashboard") {
      await renderDashboard(view);
    } else if (route.name === "jobs") {
      await renderJobsList(route.search, view);
    } else {
      await renderDeliveries(route.search, view);
    }
    // Retry a refresh of this view that was dropped for "load more" or user input.
    return !(seq === state.renderSeq && (loadMoreSeq !== state.loadMoreSeq || isEditingView()));
  }

  // ==================== 1. Dashboard (/ui/) ====================
  // view is set only for a background refresh (see refreshListView).
  async function renderDashboard(view) {
    if (!view) {
      setLoading(L("dashboard.title"));
    }
    const [statusRes, metaRes, claimedRes, heldRes, delivRes] = await Promise.all([
      apiFetch("/status"),
      apiFetch("/meta"),
      apiFetch("/jobs?status=claimed&desc=true&limit=20"),
      apiFetch("/gate/held?status=held&limit=20"),
      apiFetch("/deliveries?status=all&limit=10")
    ]);

    if (!statusRes.ok) {
      throw i18nError(errorMessageFromResp(statusRes, "common.statusFetchFailed"));
    }
    state.status = statusRes.data;
    if (metaRes.ok && metaRes.data) {
      state.meta = metaRes.data;
    }
    renderMetaBadges();

    const counts = (statusRes.data && statusRes.data.counts) || {};
    const statCards = ALL_STATUSES.map(function (st) {
      return el("a", {
        href: "/ui/jobs?status=" + encodeURIComponent(st),
        className: "stat-card",
        dataset: { nav: "true" }
      }, [
        el("div", { className: "stat-label", text: st }),
        el("div", { className: "stat-value", text: String(counts[st] || 0) })
      ]);
    });
    statCards.push(
      el("div", { className: "stat-card" }, [
        el("div", { className: "stat-label", text: L("dashboard.total") }),
        el("div", { className: "stat-value", text: String(statusRes.data.total || 0) })
      ])
    );

    const sections = [
      el("div", { className: "page-header" }, [
        el("h1", { text: L("dashboard.title") }),
        el("div", { className: "btn-row" }, [
          navLink("/ui/jobs/new", L("common.newJobButton"), "btn btn-primary")
        ])
      ]),
      el("section", { className: "section" }, [
        el("h2", { className: "section-title", text: L("dashboard.statusCounts") }),
        el("div", { className: "stat-grid" }, statCards)
      ])
    ];

    if (state.meta && Array.isArray(state.meta.pool_summaries) && state.meta.pool_summaries.length > 0) {
      const poolCards = state.meta.pool_summaries.map(function (ps) {
        const lines = [];
        if (ps.unmeasured) {
          if (ps.status !== "cooldown") {
            lines.push(el("div", { className: "small muted", text: L("dashboard.unmeasured") }));
            lines.push(el("div", { className: "pool-resets small" }, [
              el("span", { className: "pool-reset-label", text: L("dashboard.resetLabel") }),
              el("span", { className: "pool-reset-item is-missing", text: L("dashboard.resetNone") })
            ]));
          }
        } else {
          if (ps.weekly_remaining_percent !== undefined && ps.weekly_remaining_percent !== null) {
            lines.push(el("div", { className: "small", text: L("dashboard.weeklyRemaining", { percent: ps.weekly_remaining_percent }) }));
          }
          if (ps.short_remaining_percent !== undefined && ps.short_remaining_percent !== null) {
            lines.push(el("div", { className: "small", text: L("dashboard.shortRemaining", { percent: ps.short_remaining_percent }) }));
          }
          const resets = formatPoolResetLabels(ps);
          lines.push(el("div", { className: "pool-resets small" }, [
            el("span", {
              className: "pool-reset-label",
              text: function () { return formatPoolResetLabels(ps).labelText; }
            }),
            el("span", {
              className: resets.weeklyMissing ? "pool-reset-item is-missing" : "pool-reset-item mono",
              title: resets.weeklyRaw || null,
              text: function () { return formatPoolResetLabels(ps).weeklyText; }
            }),
            el("span", {
              className: resets.shortMissing ? "pool-reset-item is-missing" : "pool-reset-item mono",
              title: resets.shortRaw || null,
              text: function () { return formatPoolResetLabels(ps).shortText; }
            })
          ]));
          if (ps.measured_at) {
            lines.push(el("div", { className: "small muted" }, [
              L("dashboard.measuredAt"),
              el("span", { className: "mono small", text: formatTime(ps.measured_at) })
            ]));
          }
        }
        if (ps.cooldown && ps.cooldown.active) {
          const cd = ps.cooldown;
          lines.push(el("div", { className: "small", text: function () {
            return t("cooldown.until", {
              until: formatTime(cd.until),
              minutes: Math.max(0, Math.ceil((Date.parse(cd.until) - Date.now()) / 60000))
            });
          } }));
          const details = [L("cooldown.hits", { hits: cd.consecutive_hits })];
          if (cd.last_job_id) {
            details.push(" · ", navLink("/ui/jobs/" + cd.last_job_id, "#" + cd.last_job_id, "mono"));
          }
          lines.push(el("div", { className: "small" }, details));
          if ((state.meta.scopes || []).includes("operate")) {
            let clearing = false;
            const button = el("button", {
              type: "button",
              className: "btn btn-sm",
              text: L("cooldown.clear"),
              onClick: async function () {
                if (clearing || !window.confirm(t("cooldown.confirm", { pool: ps.pool }))) return;
                clearing = true;
                button.setAttribute("aria-disabled", "true");
                try {
                  const response = await apiFetch("/gate/cooldowns/" + encodeURIComponent(ps.pool), { method: "DELETE" });
                  if (!response.ok) {
                    showBanner("error", errorMessageFromResp(response, "cooldown.clearFailed"));
                    return;
                  }
                  if (location.pathname === "/ui" || location.pathname === "/ui/") await renderDashboard();
                } catch (_) {
                  showBanner("error", L("cooldown.clearFailed"));
                } finally {
                  clearing = false;
                  button.removeAttribute("aria-disabled");
                }
              }
            });
            lines.push(button);
          }
        }
        if (ps.reason && ps.status !== "cooldown") {
          lines.push(el("div", { className: "small muted", text: ps.reason }));
        }
        return el("div", { className: "pool-card" }, [
          el("div", { className: "pool-header" }, [
            el("strong", { text: ps.pool }),
            statusBadge(ps.status)
          ]),
          ...lines
        ]);
      });
      sections.push(
        el("section", { className: "section" }, [
          el("h2", { className: "section-title", text: L("dashboard.poolSummary") }),
          el("div", { className: "pool-grid" }, poolCards)
        ])
      );
    }

    const claimedJobs = (claimedRes.ok && claimedRes.data && claimedRes.data.jobs) || [];
    sections.push(
      el("section", { className: "section" }, [
        el("h2", { className: "section-title", text: L("dashboard.runningJobs", { count: claimedJobs.length }) }),
        buildJobsTable(claimedJobs, L("dashboard.noRunningJobs"))
      ])
    );

    const heldItems = (heldRes.ok && heldRes.data && heldRes.data.held) || [];
    sections.push(
      el("section", { className: "section" }, [
        el("h2", { className: "section-title", text: L("dashboard.heldJobs", { count: heldItems.length }) }),
        buildHeldSummaryTable(heldItems)
      ])
    );

    const deliveries = (delivRes.ok && delivRes.data && delivRes.data.jobs) || [];
    sections.push(
      el("section", { className: "section" }, [
        el("h2", { className: "section-title", text: L("dashboard.recentDeliveries", { count: deliveries.length }) }),
        buildDeliveriesTable(deliveries, null)
      ])
    );

    if (view && (view.isStale() || !claimedRes.ok || !heldRes.ok || !delivRes.ok)) {
      return;
    }
    dom.app.replaceChildren.apply(dom.app, sections);
  }

  function buildHeldSummaryTable(items) {
    if (!items || items.length === 0) {
      return el("div", { className: "card empty-state", text: L("dashboard.noHeldJobs") });
    }
    const rows = items.map(function (h) {
      const title = (h.payload && h.payload.title) ? h.payload.title : L("common.noTitle");
      return el("tr", null, [
        el("td", { className: "mono" }, [navLink("/ui/gate/held", h.id)]),
        el("td", { text: title }),
        el("td", { text: h.pool || "-" }),
        el("td", { text: h.reason || "-" }),
        el("td", { className: "mono small muted", text: formatTime(h.created_at) })
      ]);
    });
    return el("div", { className: "table-wrap" }, [
      el("table", null, [
        el("thead", null, [
          el("tr", null, [
            el("th", { text: L("field.heldID") }),
            el("th", { text: L("column.title") }),
            el("th", { text: L("column.pool") }),
            el("th", { text: L("column.holdReason") }),
            el("th", { text: L("column.createdAt") })
          ])
        ]),
        el("tbody", null, rows)
      ])
    ]);
  }

  function buildJobsTable(jobs, emptyText) {
    if (!jobs || jobs.length === 0) {
      return el("div", { className: "card empty-state", text: emptyText || L("jobs.empty") });
    }
    const rows = jobs.map(function (j) {
      const agentHost = (j.agent || "-") + " / " + (j.host || "-");
      const parentCell = j.parent_id
        ? navLink("/ui/jobs/" + j.parent_id, "#" + j.parent_id, "mono")
        : el("span", { className: "muted", text: "-" });
      return el("tr", null, [
        el("td", { className: "mono" }, [navLink("/ui/jobs/" + j.id, "#" + j.id)]),
        el("td", null, [navLink("/ui/jobs/" + j.id, j.title || L("common.untitled"))]),
        el("td", null, jobStatusNodes(j)),
        el("td", { className: "mono", text: agentHost }),
        el("td", { className: "mono small muted", text: formatTime(j.created_at) }),
        el("td", null, [parentCell])
      ]);
    });
    return el("div", { className: "table-wrap" }, [
      el("table", null, [
        el("thead", null, [
          el("tr", null, [
            el("th", { text: L("field.id") }),
            el("th", { text: L("column.title") }),
            el("th", { text: L("column.status") }),
            el("th", { text: L("field.agentHost") }),
            el("th", { text: L("column.createdAt") }),
            el("th", { text: L("column.parentJob") })
          ])
        ]),
        el("tbody", null, rows)
      ])
    ]);
  }

  function buildTreePrefix(guides, isLast) {
    const items = [];
    for (let i = 0; i < guides.length; i += 1) {
      items.push(el("span", {
        className: guides[i] ? "tree-guide tree-guide-line" : "tree-guide"
      }));
    }
    items.push(el("span", {
      className: isLast ? "tree-branch tree-branch-last" : "tree-branch tree-branch-mid"
    }));
    return el("span", { className: "tree-prefix", "aria-hidden": "true" }, items);
  }

  function buildChainJobsTable(forest, emptyText) {
    if (!forest || forest.length === 0) {
      return el("div", { className: "card empty-state", text: emptyText || L("jobs.noMatch") });
    }
    const rows = [];
    for (const rootNode of forest) {
      const flatRows = flattenChainTree(rootNode);
      for (const item of flatRows) {
        const j = item.job;
        const agentHost = (j.agent || "-") + " / " + (j.host || "-");
        const lineChildren = [
          navLink("/ui/jobs/" + j.id, "#" + j.id, "mono"),
          navLink("/ui/jobs/" + j.id, j.title || L("common.untitled"))
        ];
        if (item.isOrphanRoot && j.parent_id) {
          lineChildren.push(
            el("span", { className: "chain-orphan-note" }, [
              L("chain.orphanBefore"),
              navLink("/ui/jobs/" + j.parent_id, "#" + j.parent_id, "mono"),
              L("chain.orphanAfter")
            ])
          );
        }

        const nodeMainChildren = [
          el("div", { className: "chain-node-line" }, lineChildren)
        ];
        if (item.isRoot && item.summary) {
          nodeMainChildren.push(
            el("div", {
              className: "chain-summary",
              text: function () { return formatChainProgressSummary(item.summary); }
            })
          );
        }

        const cellChildren = [];
        if (item.depth > 0) {
          cellChildren.push(buildTreePrefix(item.guides, item.isLast));
        }
        cellChildren.push(el("div", { className: "chain-node-main" }, nodeMainChildren));

        rows.push(el("tr", { className: item.isRoot ? "chain-root-row" : "chain-child-row" }, [
          el("td", null, [el("div", { className: "chain-node-cell" }, cellChildren)]),
          el("td", null, jobStatusNodes(j)),
          el("td", { className: "mono", text: agentHost }),
          el("td", { className: "mono small muted", text: formatTime(j.created_at) })
        ]));
      }
    }

    return el("div", { className: "table-wrap" }, [
      el("table", { className: "chain-table" }, [
        el("thead", null, [
          el("tr", null, [
            el("th", { scope: "col", text: L("chain.tableHeading") }),
            el("th", { scope: "col", text: L("column.status") }),
            el("th", { scope: "col", text: L("field.agentHost") }),
            el("th", { scope: "col", text: L("column.createdAt") })
          ])
        ]),
        el("tbody", null, rows)
      ])
    ]);
  }

  // ==================== 2. Jobs List (/ui/jobs) ====================
  async function renderJobsList(searchParams, view) {
    const activeView = resolveJobsView(searchParams);
    if (!view) {
      // Back, reload and refresh keep this view even if the stored choice changes later.
      history.replaceState({ jobsView: activeView }, "");
    }
    const requestedStatus = (searchParams && searchParams.get("status")) || (activeView === "chain" ? "all" : "queued");
    const activeStatus = activeView === "chain"
      ? (requestedStatus === "all" || ALL_STATUSES.includes(requestedStatus) ? requestedStatus : "all")
      : (ALL_STATUSES.includes(requestedStatus) ? requestedStatus : "queued");
    const filterAgent = (searchParams && searchParams.get("agent")) || "";
    const filterHost = (searchParams && searchParams.get("host")) || "";
    const filterQ = (searchParams && searchParams.get("q")) || "";

    if (!view) {
      setLoading(L("jobs.title"));
    }

    function buildJobsURL(view, st, ag, ho, q) {
      const sp = new URLSearchParams();
      sp.set("view", view);
      if (view === "chain") {
        if (st && st !== "all") {
          sp.set("status", st);
        }
      } else {
        sp.set("status", ALL_STATUSES.includes(st) ? st : "queued");
      }
      if (ag) sp.set("agent", ag);
      if (ho) sp.set("host", ho);
      if (q) sp.set("q", q);
      return "/ui/jobs?" + sp.toString();
    }

    let statusRes;
    let metaRes;
    let loadedJobs = [];
    let nextCursor = 0;
    let hasMore = false;
    let flatParams = null;
    const truncatedStatuses = [];

    if (activeView === "chain") {
      const statusFetches = ALL_STATUSES.map(function (st) {
        const sp = new URLSearchParams();
        sp.set("status", st);
        sp.set("desc", "true");
        sp.set("limit", "100");
        if (filterAgent) sp.set("agent", filterAgent);
        if (filterHost) sp.set("host", filterHost);
        if (filterQ) sp.set("q", filterQ);
        return apiFetch("/jobs?" + sp.toString()).then(function (res) {
          return { status: st, res: res };
        });
      });

      const results = await Promise.all([
        apiFetch("/status"),
        apiFetch("/meta"),
        ...statusFetches
      ]);
      statusRes = results[0];
      metaRes = results[1];
      const listResults = results.slice(2);

      if (!statusRes.ok) {
        throw i18nError(errorMessageFromResp(statusRes, "common.statusFetchFailed"));
      }
      for (const item of listResults) {
        if (!item.res.ok) {
          throw i18nError(errorMessageFromResp(item.res, "jobs.statusListFetchFailed", { status: item.status }));
        }
        const jobs = (item.res.data && Array.isArray(item.res.data.jobs)) ? item.res.data.jobs : [];
        loadedJobs = loadedJobs.concat(jobs);
        if (item.res.data && item.res.data.has_more) {
          truncatedStatuses.push(item.status);
        }
      }
    } else {
      flatParams = new URLSearchParams();
      flatParams.set("status", activeStatus);
      flatParams.set("desc", "true");
      flatParams.set("limit", "50");
      if (filterAgent) flatParams.set("agent", filterAgent);
      if (filterHost) flatParams.set("host", filterHost);
      if (filterQ) flatParams.set("q", filterQ);

      const results = await Promise.all([
        apiFetch("/status"),
        apiFetch("/meta"),
        apiFetch("/jobs?" + flatParams.toString())
      ]);
      statusRes = results[0];
      metaRes = results[1];
      const jobsRes = results[2];

      if (!statusRes.ok) {
        throw i18nError(errorMessageFromResp(statusRes, "common.statusFetchFailed"));
      }
      if (!jobsRes.ok) {
        throw i18nError(errorMessageFromResp(jobsRes, "jobs.listFetchFailed"));
      }
      loadedJobs = (jobsRes.data && jobsRes.data.jobs) ? jobsRes.data.jobs.slice() : [];
      nextCursor = jobsRes.data ? jobsRes.data.next_cursor : 0;
      hasMore = Boolean(jobsRes.data && jobsRes.data.has_more);

      // A background refresh keeps the pages the user had already loaded.
      for (let page = 1; view && page < state.flatPages && hasMore && nextCursor; page += 1) {
        const pageParams = new URLSearchParams(flatParams.toString());
        pageParams.set("cursor", String(nextCursor));
        const pageRes = await apiFetch("/jobs?" + pageParams.toString());
        if (!pageRes.ok) {
          throw i18nError(errorMessageFromResp(pageRes, "jobs.listFetchFailed"));
        }
        loadedJobs = loadedJobs.concat((pageRes.data && pageRes.data.jobs) || []);
        nextCursor = pageRes.data ? pageRes.data.next_cursor : 0;
        hasMore = Boolean(pageRes.data && pageRes.data.has_more);
      }
    }

    state.status = statusRes.data;
    if (metaRes.ok && metaRes.data) {
      state.meta = metaRes.data;
    }
    renderMetaBadges();

    const counts = (statusRes.data && statusRes.data.counts) || {};
    const totalCount = (statusRes.data && statusRes.data.total) || 0;

    function selectView(nextView, nextStatus) {
      storeJobsView(nextView);
      if (activeView !== nextView) {
        navigate(buildJobsURL(nextView, nextStatus, filterAgent, filterHost, filterQ));
      }
    }

    const viewToggle = el("div", { className: "view-toggle", role: "group", "aria-label": L("jobs.viewSwitchLabel") }, [
      el("button", {
        type: "button",
        className: "view-toggle-btn" + (activeView === "flat" ? " active" : ""),
        "aria-pressed": activeView === "flat" ? "true" : "false",
        text: L("jobs.viewFlat"),
        onClick: function () {
          selectView("flat", activeStatus === "all" ? "queued" : activeStatus);
        }
      }),
      el("button", {
        type: "button",
        className: "view-toggle-btn" + (activeView === "chain" ? " active" : ""),
        "aria-pressed": activeView === "chain" ? "true" : "false",
        text: L("jobs.viewChain"),
        onClick: function () {
          selectView("chain", "all");
        }
      })
    ]);

    const tabItems = [];
    if (activeView === "chain") {
      tabItems.push(
        el("button", {
          type: "button",
          className: "tab" + (activeStatus === "all" ? " active" : ""),
          onClick: function () {
            navigate(buildJobsURL("chain", "all", filterAgent, filterHost, filterQ));
          }
        }, [
          el("span", { text: L("common.all") }),
          el("span", { className: "tab-count", text: String(totalCount) })
        ])
      );
    }
    for (const st of ALL_STATUSES) {
      tabItems.push(
        el("button", {
          type: "button",
          className: "tab" + (st === activeStatus ? " active" : ""),
          onClick: function () {
            navigate(buildJobsURL(activeView, st, filterAgent, filterHost, filterQ));
          }
        }, [
          el("span", { text: st }),
          el("span", { className: "tab-count", text: String(counts[st] || 0) })
        ])
      );
    }
    const tabs = el("div", { className: "tabs" }, tabItems);

    const agentOpts = [el("option", { value: "", text: L("common.all") })];
    const agents = (state.meta && Array.isArray(state.meta.agents)) ? state.meta.agents : [];
    for (const a of agents) {
      agentOpts.push(el("option", { value: a, text: a, selected: a === filterAgent }));
    }
    if (filterAgent && !agents.includes(filterAgent)) {
      agentOpts.push(el("option", { value: filterAgent, text: filterAgent, selected: true }));
    }

    const hostOpts = [el("option", { value: "", text: L("common.all") })];
    const hosts = (state.meta && Array.isArray(state.meta.hosts)) ? state.meta.hosts : [];
    for (const h of hosts) {
      hostOpts.push(el("option", { value: h, text: h, selected: h === filterHost }));
    }
    if (filterHost && !hosts.includes(filterHost)) {
      hostOpts.push(el("option", { value: filterHost, text: filterHost, selected: true }));
    }

    const agentSelect = el("select", { name: "agent" }, agentOpts);
    const hostSelect = el("select", { name: "host" }, hostOpts);
    const qInput = el("input", {
      type: "text",
      name: "q",
      value: filterQ,
      placeholder: L("jobs.searchPlaceholder")
    });

    const filterForm = el("form", {
      className: "filter-bar",
      onSubmit: function (ev) {
        ev.preventDefault();
        navigate(buildJobsURL(activeView, activeStatus, agentSelect.value, hostSelect.value, qInput.value.trim()));
      }
    }, [
      el("label", { className: "field" }, [
        el("span", { className: "field-label", text: L("field.agent") }),
        agentSelect
      ]),
      el("label", { className: "field" }, [
        el("span", { className: "field-label", text: L("field.host") }),
        hostSelect
      ]),
      el("label", { className: "field" }, [
        el("span", { className: "field-label", text: L("jobs.searchLabel") }),
        qInput
      ]),
      el("div", { className: "btn-row" }, [
        el("button", { type: "submit", className: "btn btn-primary", text: L("common.filter") }),
        el("button", {
          type: "button",
          className: "btn btn-ghost",
          text: L("common.clear"),
          onClick: function () {
            navigate(buildJobsURL(activeView, activeStatus, "", "", ""));
          }
        })
      ])
    ]);

    const truncationNotice = (activeView === "chain" && truncatedStatuses.length > 0)
      ? el("div", {
          className: "alert alert-info small",
          text: L("jobs.chainTruncated", { statuses: truncatedStatuses.join(", ") })
        })
      : null;

    let tableNode;
    if (activeView === "chain") {
      const fullForest = buildJobForest(loadedJobs);
      const visibleForest = activeStatus === "all"
        ? fullForest
        : fullForest.filter(function (rootNode) {
            return treeContainsStatus(rootNode, activeStatus);
          });
      tableNode = buildChainJobsTable(visibleForest, L("jobs.noMatchingChain"));
    } else {
      tableNode = buildJobsTable(loadedJobs, L("jobs.noMatch"));
    }

    const tableContainer = el("div", null, [tableNode]);
    const loadMoreContainer = el("div", { className: "load-more-row" });

    function updateLoadMore() {
      loadMoreContainer.replaceChildren();
      if (activeView !== "flat" || !hasMore || !nextCursor || !flatParams) {
        return;
      }
      const btn = el("button", {
        type: "button",
        className: "btn",
        text: L("jobs.loadMore"),
        onClick: async function () {
          btn.disabled = true;
          i18n.setText(btn, L("common.loading"));
          state.loadMoreSeq += 1;
          state.loadMoreButton = btn;
          try {
            const nextParams = new URLSearchParams(flatParams.toString());
            nextParams.set("cursor", String(nextCursor));
            const moreRes = await apiFetch("/jobs?" + nextParams.toString());
            // The list was redrawn while waiting: this page belongs to the old one.
            if (!btn.isConnected) {
              return;
            }
            if (!moreRes.ok) {
              showBanner("error", errorMessageFromResp(moreRes, "jobs.loadMoreFailed"));
              return;
            }
            const moreJobs = (moreRes.data && moreRes.data.jobs) || [];
            loadedJobs = loadedJobs.concat(moreJobs);
            nextCursor = moreRes.data ? moreRes.data.next_cursor : 0;
            hasMore = Boolean(moreRes.data && moreRes.data.has_more);
            state.flatPages += 1;
            tableContainer.replaceChildren(buildJobsTable(loadedJobs, L("jobs.noMatch")));
            updateLoadMore();
          } catch (err) {
            if (!err || err.status !== 401) {
              showBanner("error", errorAlert("jobs.loadMoreError", err));
            }
          } finally {
            if (state.loadMoreButton === btn) {
              state.loadMoreButton = null;
            }
          }
        }
      });
      loadMoreContainer.appendChild(btn);
    }
    updateLoadMore();

    const pageNodes = [
      el("div", { className: "page-header" }, [
        el("h1", { text: L("jobs.title") }),
        el("div", { className: "btn-row" }, [
          viewToggle,
          navLink("/ui/jobs/new", L("common.newJobButton"), "btn btn-primary")
        ])
      ]),
      tabs,
      filterForm
    ];
    if (truncationNotice) {
      pageNodes.push(truncationNotice);
    }
    pageNodes.push(tableContainer, loadMoreContainer);

    if (view && view.isStale()) {
      return;
    }
    if (!view) {
      state.flatPages = 1;
    }
    dom.app.replaceChildren.apply(dom.app, pageNodes);
  }

  function buildMiniChainSection(chainJobs, currentJobID, chainMeta) {
    const forest = buildJobForest(chainJobs);
    let primaryRoot = null;
    for (const rootNode of forest) {
      const flat = flattenChainTree(rootNode);
      if (flat.some(function (row) { return row.job.id === currentJobID; })) {
        primaryRoot = rootNode;
        break;
      }
    }
    if (!primaryRoot && forest.length > 0) {
      primaryRoot = forest[0];
    }

    if (!primaryRoot) {
      return el("section", { className: "section card" }, [
        el("h2", { className: "section-title", text: L("chain.sectionTitle") }),
        el("div", { className: "empty-state", text: L("chain.unavailable") })
      ]);
    }

    const summary = summarizeChainProgress(primaryRoot);
    const rendered = new Set();

    function renderTreeItem(node) {
      if (!node || !node.job || rendered.has(node.job.id)) {
        return null;
      }
      rendered.add(node.job.id);
      const j = node.job;
      const isCurrent = j.id === currentJobID;

      const linkAttrs = {
        href: "/ui/jobs/" + j.id,
        dataset: { nav: "true" },
        text: function () { return "#" + j.id + " " + (j.title || t("common.untitled")); }
      };
      if (isCurrent) {
        linkAttrs["aria-current"] = "page";
      }

      const nodeParts = [
        el("a", linkAttrs),
        statusBadge(j.status)
      ];
      if (isCurrent) {
        nodeParts.push(el("span", { className: "mini-tree-current-tag", text: L("chain.currentTag") }));
      }
      if (node.isOrphanRoot && j.parent_id) {
        nodeParts.push(
          el("span", { className: "chain-orphan-note" }, [
            L("chain.orphanBefore"),
            navLink("/ui/jobs/" + j.parent_id, "#" + j.parent_id, "mono"),
            L("chain.orphanAfter")
          ])
        );
      }

      const itemChildren = [
        el("div", { className: "mini-tree-node" + (isCurrent ? " is-current" : "") }, nodeParts)
      ];

      if (Array.isArray(node.children) && node.children.length > 0) {
        const childLis = node.children.map(renderTreeItem).filter(Boolean);
        if (childLis.length > 0) {
          itemChildren.push(el("ul", { className: "mini-tree-children", role: "list" }, childLis));
        }
      }

      return el("li", { className: "mini-tree-item" }, itemChildren);
    }

    const rootLi = renderTreeItem(primaryRoot);

    const sectionChildren = [
      el("div", { className: "mini-tree-header" }, [
        el("h2", { className: "section-title", text: L("chain.sectionTitle") }),
        el("span", { className: "chain-summary", text: function () { return formatChainProgressSummary(summary); } })
      ])
    ];

    const fetchErrorNotice = function () { return formatChainFetchErrorNotice(chainMeta); };
    if (fetchErrorNotice()) {
      sectionChildren.push(
        el("div", { className: "alert alert-error small", role: "alert", text: fetchErrorNotice })
      );
    }

    const truncationNotice = function () { return formatChainTruncationNotice(chainMeta); };
    if (truncationNotice()) {
      sectionChildren.push(
        el("div", { className: "alert alert-info small", text: truncationNotice })
      );
    }

    sectionChildren.push(
      el("ul", { className: "mini-tree", role: "list" }, rootLi ? [rootLi] : [])
    );

    return el("section", { className: "section card" }, sectionChildren);
  }

  // ==================== 3. Job Detail (/ui/jobs/{id}) ====================
  async function renderJobDetail(rawID) {
    const jobID = Number(rawID);
    if (!Number.isInteger(jobID) || jobID <= 0) {
      dom.app.replaceChildren(
        el("div", { className: "alert alert-error", text: L("detail.invalidID", { id: rawID }) }),
        navLink("/ui/jobs", L("detail.backToJobs"), "btn")
      );
      return;
    }

    setLoading(L("common.jobLabel", { id: jobID }));

    const [jobRes, metaRes] = await Promise.all([apiFetch("/jobs/" + jobID), apiFetch("/meta")]);
    if (metaRes.ok) state.meta = metaRes.data;
    if (!jobRes.ok) {
      throw i18nError(errorMessageFromResp(jobRes, "detail.fetchFailed", { id: jobID }));
    }
    const job = jobRes.data;

    const [decisionsRes, chainResult] = await Promise.all([
      apiFetch("/gate/decisions?job_id=" + jobID + "&limit=50"),
      fetchFullJobChain(job, apiFetch)
    ]);
    const chainJobs = chainResult.jobs;

    const children = chainJobs
      .filter(function (cj) { return cj.parent_id === jobID; })
      .sort(function (a, b) { return a.id - b.id; });

    const decisions = (decisionsRes.ok && decisionsRes.data && decisionsRes.data.decisions) || [];

    function kv(label, valueNode) {
      const content = (valueNode instanceof Node)
        ? valueNode
        : el("span", { text: (valueNode !== undefined && valueNode !== null && valueNode !== "") ? String(valueNode) : "-" });
      return el("div", { className: "kv-item" }, [
        el("div", { className: "kv-key", text: label }),
        el("div", { className: "kv-val" }, [content])
      ]);
    }

    const parentNode = job.parent_id
      ? navLink("/ui/jobs/" + job.parent_id, L("common.jobLabel", { id: job.parent_id }), "mono")
      : "-";

    const wtText = job.worktree_spec
      ? [
          "repo_path=" + (job.worktree_spec.repo_path || ""),
          job.worktree_spec.base_ref ? ("base_ref=" + job.worktree_spec.base_ref) : "",
          job.worktree_spec.branch ? ("branch=" + job.worktree_spec.branch) : ""
        ].filter(Boolean).join(", ")
      : "-";

    const metaGrid = el("div", { className: "kv-grid" }, [
      kv(L("column.status"), el("div", null, jobStatusNodes(job))),
      kv(L("field.requestID"), el("span", { className: "mono", text: job.request_id || "-" })),
      kv(L("field.agent"), job.agent),
      kv(L("field.host"), job.host),
      kv(L("field.workdir"), el("span", { className: "mono", text: job.workdir || "-" })),
      kv(L("field.model"), job.model),
      kv(L("field.pool"), job.pool),
      kv(L("field.altPools"), (Array.isArray(job.alt_pools) && job.alt_pools.length > 0) ? job.alt_pools.join(", ") : "-"),
      kv(L("column.parentJob"), parentNode),
      kv(L("field.selectionMode"), job.selection_mode || "-"),
      kv(L("field.selectedTarget"), job.selection_mode
        ? el("span", { className: "mono", text: [job.selected_agent, job.selected_model, job.selected_pool].map(function (v) { return v || "-"; }).join(" / ") })
        : "-"),
      kv(L("field.selectionReason"), job.selection_reason || "-"),
      kv(L("field.targetRef"), el("span", { className: "mono", text: job.target_ref || "-" })),
      kv(L("field.worktreeSpec"), el("span", { className: "mono", text: wtText })),
      kv(L("field.attempts"), String(job.attempts || 0)),
      kv(L("field.claimedBy"), el("span", { className: "mono", text: job.claimed_by || "-" })),
      kv(L("field.claimedAt"), el("span", { className: "mono", text: formatTime(job.claimed_at) })),
      kv(L("field.createdAt"), el("span", { className: "mono", text: formatTime(job.created_at) })),
      kv(L("field.updatedAt"), el("span", { className: "mono", text: formatTime(job.updated_at) })),
      kv(L("field.deliveredAt"), el("span", { className: "mono", text: formatTime(job.delivered_at) })),
      kv(L("field.sessionID"), el("span", { className: "mono", text: job.session_id || "-" }))
    ]);

    // Allowed actions strictly per state:
    // - cancel: draft | queued | waiting (NEVER for claimed)
    // - requeue: failed | cancelled
    const actionNodes = [];
    if (CANCELLABLE_STATUSES.has(job.status)) {
      const reasonInput = el("input", {
        type: "text",
        placeholder: L("detail.cancelReasonPlaceholder")
      });
      const cancelBtn = el("button", {
        type: "button",
        className: "btn btn-danger",
        text: L("common.cancel"),
        onClick: async function () {
          if (!window.confirm(t("detail.cancelConfirm", { id: job.id }))) {
            return;
          }
          cancelBtn.disabled = true;
          try {
            const body = reasonInput.value.trim() ? { reason: reasonInput.value.trim() } : {};
            const res = await apiFetch("/jobs/" + job.id + "/cancel", {
              method: "POST",
              body: JSON.stringify(body)
            });
            if (!res.ok) {
              showBanner("error", errorMessageFromResp(res, "common.cancelFailed"));
              cancelBtn.disabled = false;
              return;
            }
            const cascaded = (res.data && Array.isArray(res.data.cancelled_child_ids)) ? res.data.cancelled_child_ids : [];
            showBanner("success", function () {
              const ids = cascaded.map(function (c) { return "#" + c; }).join(", ");
              const extra = cascaded.length > 0 ? t("detail.cascaded", { ids: ids }) : "";
              return t("detail.cancelled", { id: job.id, extra: extra });
            });
            await renderJobDetail(String(job.id));
          } catch (err) {
            if (!err || err.status !== 401) {
              showBanner("error", errorAlert("common.cancelError", err));
            }
          }
        }
      });
      actionNodes.push(
        el("div", { className: "input-row" }, [reasonInput, cancelBtn])
      );
    }

    if (REQUEUEABLE_STATUSES.has(job.status)) {
      const requeueBtn = el("button", {
        type: "button",
        className: "btn btn-primary",
        text: L("detail.requeue"),
        onClick: async function () {
          requeueBtn.disabled = true;
          try {
            const res = await apiFetch("/jobs/" + job.id + "/requeue", { method: "POST" });
            if (!res.ok) {
              showBanner("error", errorMessageFromResp(res, "detail.requeueFailed"));
              requeueBtn.disabled = false;
              return;
            }
            showBanner("success", L("detail.requeued", { id: job.id }));
            await renderJobDetail(String(job.id));
          } catch (err) {
            if (!err || err.status !== 401) {
              showBanner("error", errorAlert("detail.requeueError", err));
            }
          }
        }
      });
      actionNodes.push(requeueBtn);
    }

    const overviewCard = el("div", { className: "card section" }, [
      metaGrid,
      actionNodes.length > 0 ? el("div", { className: "actions-box" }, actionNodes) : null
    ]);

    const deliverySummary = (function () {
      if (job.status !== "done" && job.status !== "failed") {
        return el("div", { className: "card empty-state", text: L("detail.notDeliverable") });
      }
      const badge = job.delivered_at ? statusBadge("delivered") : statusBadge("undelivered");
      return el("div", { className: "card" }, [
        el("div", { className: "kv-grid" }, [
          kv(L("detail.deliveryState"), badge),
          kv(L("detail.deliveredAt"), el("span", { className: "mono", text: formatTime(job.delivered_at) })),
          kv(L("detail.jobState"), statusBadge(job.status))
        ])
      ]);
    })();

    const miniChainSection = buildMiniChainSection(chainJobs, job.id, chainResult);
    const directChildrenErrorNotice = formatDirectChildrenFetchErrorNotice(chainResult, job.id);
    const childrenSectionHeading = directChildrenErrorNotice
      ? L("detail.childrenPartial", { count: children.length })
      : L("detail.children", { count: children.length });
    const childrenEmptyText = directChildrenErrorNotice
      ? L("detail.noChildrenFetched")
      : L("detail.noChildren");
    const childrenSectionNodes = [
      el("h2", { className: "section-title", text: childrenSectionHeading })
    ];
    if (directChildrenErrorNotice) {
      childrenSectionNodes.push(
        el("div", {
          className: "alert alert-error small",
          role: "alert",
          text: function () { return formatDirectChildrenFetchErrorNotice(chainResult, job.id); }
        })
      );
    }
    childrenSectionNodes.push(buildJobsTable(children, childrenEmptyText));

    dom.app.replaceChildren(
      el("div", { className: "page-header" }, [
        el("h1", {
          text: function () {
            return t("detail.heading", { id: job.id, title: job.title || t("common.untitled") });
          }
        }),
        el("div", { className: "btn-row" }, [
          navLink("/ui/jobs?view=chain", L("detail.chainList"), "btn"),
          navLink("/ui/jobs?status=" + encodeURIComponent(job.status), L("detail.backToList"), "btn")
        ])
      ]),
      overviewCard,
      miniChainSection,
      el("section", { className: "section card" }, [
        el("h2", { className: "section-title", text: L("detail.instructions") }),
        el("pre", { text: job.instructions || L("common.emptyText") })
      ]),
      el("section", { className: "section card" }, [
        el("h2", { className: "section-title", text: L("detail.acceptance") }),
        el("pre", { text: job.acceptance_criteria || L("common.emptyText") })
      ]),
      el("section", { className: "section card" }, [
        el("h2", { className: "section-title", text: L("detail.result") }),
        el("pre", { text: job.result_text || L("common.emptyText") })
      ]),
      el("section", { className: "section card" }, [
        el("h2", { className: "section-title", text: L("detail.lastError") }),
        el("pre", { text: job.last_error || L("common.emptyText") })
      ]),
      el("section", { className: "section" }, childrenSectionNodes),
      el("section", { className: "section" }, [
        el("h2", { className: "section-title", text: L("detail.gateHistory", { count: decisions.length }) }),
        buildGateDecisionsTable(decisions)
      ]),
      el("section", { className: "section" }, [
        el("h2", { className: "section-title", text: L("detail.deliveryRecord") }),
        deliverySummary
      ])
    );
  }

  // ==================== 4. New Job Form (/ui/jobs/new) ====================
  async function renderNewJobForm() {
    setLoading(L("newJob.title"));

    const parentStatuses = ["queued", "claimed", "waiting", "draft", "done"];
    const [metaRes, ...parentResults] = await Promise.all([
      apiFetch("/meta"),
      ...parentStatuses.map(function (st) {
        return apiFetch("/jobs?status=" + encodeURIComponent(st) + "&desc=true&limit=25");
      })
    ]);

    if (!metaRes.ok) {
      throw i18nError(errorMessageFromResp(metaRes, "newJob.metaFetchFailed"));
    }
    state.meta = metaRes.data;
    renderMetaBadges();

    const parentCandidates = [];
    const seenParentIDs = new Set();
    for (const pr of parentResults) {
      if (pr.ok && pr.data && Array.isArray(pr.data.jobs)) {
        for (const j of pr.data.jobs) {
          if (!seenParentIDs.has(j.id)) {
            seenParentIDs.add(j.id);
            parentCandidates.push(j);
          }
        }
      }
    }
    parentCandidates.sort(function (a, b) {
      return b.id - a.id;
    });

    const agents = Array.isArray(state.meta.agents) ? state.meta.agents : [];
    const pools = Array.isArray(state.meta.pools) ? state.meta.pools : [];

    const requestIDInput = el("input", {
      type: "text",
      name: "request_id",
      value: generateRequestID(),
      required: true
    });
    const regenBtn = el("button", {
      type: "button",
      className: "btn btn-sm",
      text: L("newJob.regenerate"),
      onClick: function () {
        requestIDInput.value = generateRequestID();
      }
    });

    const titleInput = el("input", {
      type: "text",
      name: "title",
      placeholder: L("newJob.titlePlaceholder"),
      required: true
    });

    const agentSelect = el("select", { name: "agent" }, [
      el("option", { value: "", text: L("common.selectOption") }),
      ...agents.map(function (a, idx) {
        return el("option", { value: a, text: a, selected: idx === 0 });
      })
    ]);

    const hostSelect = el("select", { name: "host" });
    const hostNotice = el("div", {
      className: "host-empty-notice small",
      "aria-live": "polite",
      hidden: true
    });

    const workdirInput = el("input", {
      type: "text",
      name: "workdir",
      placeholder: "/path/to/workdir"
    });

    const modelSelect = el("select", { name: "model_select", hidden: true });
    const modelInput = el("input", {
      type: "text",
      name: "model",
      placeholder: L("newJob.modelPlaceholder")
    });
    const modelControlWrap = el("div", { className: "model-control" }, [modelSelect, modelInput]);

    const poolDisplay = el("div", {
      id: "derived-pool-display",
      className: "derived-pool-display mono",
      "aria-live": "polite"
    });

    function getSelectedModelValue() {
      if (!modelSelect.hidden) {
        return modelSelect.value.trim();
      }
      return modelInput.value.trim();
    }

    function updateDerivedPoolDisplay() {
      const derived = derivePoolForSelection(state.meta, agentSelect.value, getSelectedModelValue());
      i18n.setText(poolDisplay, function () { return formatDerivedPoolLabel(derived); });
    }

    function updateModelControl() {
      const catalog = getModelsForHostAgent(state.meta, hostSelect.value, agentSelect.value);
      const prevVal = getSelectedModelValue();
      if (catalog.length > 0) {
        const opts = [el("option", { value: "", text: L("newJob.modelDefault") })];
        let matchedPrev = false;
        for (const m of catalog) {
          const isSelected = prevVal !== "" && m.id === prevVal;
          if (isSelected) {
            matchedPrev = true;
          }
          opts.push(el("option", {
            value: m.id,
            text: formatModelOptionLabel(m),
            selected: isSelected
          }));
        }
        modelSelect.replaceChildren(...opts);
        if (!matchedPrev && catalog.length > 0) {
          modelSelect.value = catalog[0].id;
        }
        modelSelect.hidden = false;
        modelInput.hidden = true;
      } else {
        modelSelect.replaceChildren();
        modelSelect.hidden = true;
        modelInput.hidden = false;
      }
      updateDerivedPoolDisplay();
    }

    function updateHostOptions() {
      const selectedAgent = agentSelect.value;
      const filteredHosts = getHostsForAgent(state.meta, selectedAgent);
      const notice = formatNoMatchingHostNotice(selectedAgent, filteredHosts);
      const prevHost = hostSelect.value;
      if (notice) {
        hostSelect.replaceChildren(el("option", { value: "", text: L("newJob.noHostOption") }));
        hostSelect.value = "";
        hostSelect.disabled = true;
        i18n.setText(hostNotice, function () {
          return formatNoMatchingHostNotice(selectedAgent, filteredHosts);
        });
        hostNotice.hidden = false;
      } else {
        const opts = [el("option", { value: "", text: L("common.selectOption") })];
        let hasPrev = false;
        for (let i = 0; i < filteredHosts.length; i += 1) {
          const h = filteredHosts[i];
          if (h === prevHost) {
            hasPrev = true;
          }
          opts.push(el("option", { value: h, text: h }));
        }
        hostSelect.replaceChildren(...opts);
        hostSelect.disabled = false;
        if (hasPrev) {
          hostSelect.value = prevHost;
        } else if (filteredHosts.length > 0) {
          hostSelect.value = filteredHosts[0];
        } else {
          hostSelect.value = "";
        }
        i18n.setText(hostNotice, "");
        hostNotice.hidden = true;
      }
      updateModelControl();
    }

    agentSelect.addEventListener("change", updateHostOptions);
    hostSelect.addEventListener("change", updateModelControl);
    modelSelect.addEventListener("change", updateDerivedPoolDisplay);
    modelInput.addEventListener("input", updateDerivedPoolDisplay);
    updateHostOptions();

    const altPoolCheckboxes = [];
    const altPoolsGroup = el("div", { className: "checkbox-group" },
      pools.length > 0
        ? pools.map(function (p) {
            const cb = el("input", { type: "checkbox", value: p });
            altPoolCheckboxes.push(cb);
            return el("label", { className: "field-inline" }, [cb, el("span", { text: p })]);
          })
        : [el("span", { className: "muted small", text: L("newJob.noPools") })]
    );

    const parentSelect = el("select", { name: "parent_id" }, [
      el("option", { value: "", text: L("common.noneOption") }),
      ...parentCandidates.map(function (j) {
        return el("option", {
          value: String(j.id),
          text: function () {
            return "#" + j.id + " [" + j.status + "] " + (j.title || t("common.untitled"));
          }
        });
      })
    ]);

    const draftCheckbox = el("input", { type: "checkbox", name: "draft" });

    const instructionsInput = el("textarea", {
      name: "instructions",
      placeholder: L("newJob.instructionsPlaceholder")
    });

    const acceptanceInput = el("textarea", {
      name: "acceptance_criteria",
      placeholder: L("newJob.acceptancePlaceholder")
    });

    const targetRefInput = el("input", {
      type: "text",
      name: "target_ref",
      placeholder: L("newJob.targetRefPlaceholder")
    });
    const repoPathInput = el("input", {
      type: "text",
      name: "wt_repo_path",
      placeholder: "/path/to/git/repo"
    });
    const baseRefInput = el("input", {
      type: "text",
      name: "wt_base_ref",
      placeholder: "main"
    });
    const branchInput = el("input", {
      type: "text",
      name: "wt_branch",
      placeholder: "feat/my-branch"
    });

    const resultArea = el("div", { className: "section" });
    const submitBtn = el("button", { type: "submit", className: "btn btn-primary", text: L("newJob.submit") });

    const form = el("form", {
      className: "card form-stack",
      onSubmit: async function (ev) {
        ev.preventDefault();
        resultArea.replaceChildren();

        const reqID = requestIDInput.value.trim();
        const title = titleInput.value.trim();
        if (!reqID || !title) {
          resultArea.replaceChildren(
            el("div", { className: "alert alert-error", text: L("newJob.required") })
          );
          return;
        }

        const payload = {
          request_id: reqID,
          title: title,
          draft: Boolean(draftCheckbox.checked),
          agent: agentSelect.value,
          host: hostSelect.value,
          workdir: workdirInput.value.trim(),
          model: getSelectedModelValue(),
          instructions: instructionsInput.value,
          acceptance_criteria: acceptanceInput.value
        };

        const altPools = altPoolCheckboxes
          .filter(function (cb) { return cb.checked; })
          .map(function (cb) { return cb.value; });
        if (altPools.length > 0) {
          payload.alt_pools = altPools;
        }

        if (parentSelect.value) {
          const pid = Number(parentSelect.value);
          if (Number.isInteger(pid) && pid > 0) {
            payload.parent_id = pid;
          }
        }

        const targetRef = targetRefInput.value.trim();
        if (targetRef) {
          payload.target_ref = targetRef;
        }

        const repoPath = repoPathInput.value.trim();
        const baseRef = baseRefInput.value.trim();
        const branch = branchInput.value.trim();
        if (repoPath || baseRef || branch) {
          payload.worktree_spec = {
            repo_path: repoPath,
            base_ref: baseRef,
            branch: branch
          };
        }

        submitBtn.disabled = true;
        i18n.setText(submitBtn, L("newJob.submitting"));
        try {
          const res = await apiFetch("/jobs", {
            method: "POST",
            body: JSON.stringify(payload)
          });

          if (res.status === 202) {
            const heldID = (res.data && res.data.held_id) || (res.data && res.data.held && res.data.held.id) || "-";
            const reason = (res.data && res.data.held && res.data.held.reason) || L("newJob.heldDefaultReason");
            const snap = (res.data && res.data.held && res.data.held.snapshot_summary) || "";
            resultArea.replaceChildren(
              el("div", { className: "alert alert-warn" }, [
                el("strong", { text: L("newJob.heldHeading") }),
                el("div", { className: "mono", text: "held_id: " + heldID }),
                el("div", {
                  text: function () { return t("newJob.heldReason", { reason: i18n.resolve(reason) }); }
                }),
                snap ? el("div", { className: "small muted", text: L("newJob.heldSnapshot", { snapshot: snap }) }) : null,
                el("div", { className: "btn-row" }, [
                  navLink("/ui/gate/held", L("newJob.checkHeld"), "btn btn-sm")
                ])
              ])
            );
          } else if (res.status === 201 || res.status === 200) {
            const job = res.data && res.data.job;
            const created = Boolean(res.data && res.data.created);
            const heading = created
              ? L("newJob.created")
              : L("newJob.existing");
            resultArea.replaceChildren(
              el("div", { className: "alert alert-success" }, [
                el("strong", { text: heading }),
                job
                  ? el("div", { text: L("newJob.createdSummary", { id: job.id, title: job.title, status: job.status }) })
                  : null,
                job
                  ? el("div", { className: "btn-row" }, [
                      navLink("/ui/jobs/" + job.id, L("newJob.openDetail", { id: job.id }), "btn btn-primary btn-sm"),
                      el("button", {
                        type: "button",
                        className: "btn btn-sm",
                        text: L("newJob.again"),
                        onClick: function () {
                          requestIDInput.value = generateRequestID();
                          resultArea.replaceChildren();
                        }
                      })
                    ])
                  : null
              ])
            );
          } else {
            resultArea.replaceChildren(
              el("div", { className: "alert alert-error", text: errorMessageFromResp(res, "newJob.failed") })
            );
          }
        } catch (err) {
          if (!err || err.status !== 401) {
            resultArea.replaceChildren(
              el("div", { className: "alert alert-error", text: errorAlert("newJob.submitError", err) })
            );
          }
        } finally {
          submitBtn.disabled = false;
          i18n.setText(submitBtn, L("newJob.submit"));
        }
      }
    }, [
      el("div", { className: "field" }, [
        el("span", { className: "field-label", text: L("newJob.requestIDLabel") }),
        el("div", { className: "input-row" }, [requestIDInput, regenBtn])
      ]),
      el("label", { className: "field" }, [
        el("span", { className: "field-label", text: L("newJob.titleLabel") }),
        titleInput
      ]),
      el("div", { className: "form-grid" }, [
        el("label", { className: "field" }, [
          el("span", { className: "field-label", text: L("field.agent") }),
          agentSelect
        ]),
        el("label", { className: "field" }, [
          el("span", { className: "field-label", text: L("field.host") }),
          hostSelect,
          hostNotice
        ]),
        el("div", { className: "field" }, [
          el("span", { className: "field-label", text: L("field.model") }),
          modelControlWrap
        ]),
        el("div", { className: "field" }, [
          el("span", { className: "field-label", text: L("newJob.poolLabel") }),
          poolDisplay
        ])
      ]),
      el("div", { className: "form-grid" }, [
        el("label", { className: "field" }, [
          el("span", { className: "field-label", text: L("field.workdir") }),
          workdirInput
        ]),
        el("label", { className: "field" }, [
          el("span", { className: "field-label", text: L("newJob.parentLabel") }),
          parentSelect
        ])
      ]),
      el("div", { className: "field" }, [
        el("span", { className: "field-label", text: L("newJob.altPoolsLabel") }),
        altPoolsGroup
      ]),
      el("label", { className: "field-inline" }, [
        draftCheckbox,
        el("span", { text: L("newJob.draftLabel") })
      ]),
      el("label", { className: "field" }, [
        el("span", { className: "field-label", text: L("detail.instructions") }),
        instructionsInput
      ]),
      el("label", { className: "field" }, [
        el("span", { className: "field-label", text: L("detail.acceptance") }),
        acceptanceInput
      ]),
      el("details", { className: "advanced-box" }, [
        el("summary", { text: L("newJob.advanced") }),
        el("div", { className: "form-stack" }, [
          el("label", { className: "field" }, [
            el("span", { className: "field-label", text: "target_ref" }),
            targetRefInput
          ]),
          el("div", { className: "form-grid" }, [
            el("label", { className: "field" }, [
              el("span", { className: "field-label", text: "worktree_spec.repo_path" }),
              repoPathInput
            ]),
            el("label", { className: "field" }, [
              el("span", { className: "field-label", text: "worktree_spec.base_ref" }),
              baseRefInput
            ]),
            el("label", { className: "field" }, [
              el("span", { className: "field-label", text: "worktree_spec.branch" }),
              branchInput
            ])
          ])
        ])
      ]),
      el("div", { className: "btn-row" }, [
        submitBtn,
        navLink("/ui/jobs", L("common.cancel"), "btn btn-ghost")
      ])
    ]);

    dom.app.replaceChildren(
      el("div", { className: "page-header" }, [
        el("h1", { text: L("newJob.title") })
      ]),
      resultArea,
      form
    );
  }

  // ==================== 5. Held Jobs (/ui/gate/held) ====================
  async function renderHeldJobs(searchParams) {
    const validStatuses = ["held", "released", "cancelled", "all"];
    const reqStatus = (searchParams && searchParams.get("status")) || "held";
    const activeStatus = validStatuses.includes(reqStatus) ? reqStatus : "held";

    setLoading(L("held.title"));
    const res = await apiFetch("/gate/held?status=" + encodeURIComponent(activeStatus) + "&limit=100");
    if (!res.ok) {
      throw i18nError(errorMessageFromResp(res, "held.fetchFailed"));
    }
    const items = (res.data && res.data.held) || [];

    const tabs = el("div", { className: "tabs" }, validStatuses.map(function (st) {
      return el("button", {
        type: "button",
        className: "tab" + (st === activeStatus ? " active" : ""),
        text: st,
        onClick: function () {
          navigate("/ui/gate/held?status=" + encodeURIComponent(st));
        }
      });
    }));

    const outcomeArea = el("div", { className: "section" });

    let contentNode;
    if (items.length === 0) {
      contentNode = el("div", { className: "card empty-state", text: L("held.noMatch") });
    } else {
      const rows = items.map(function (h) {
        const title = (h.payload && h.payload.title) ? h.payload.title : L("common.noTitle");
        const agentHost = ((h.payload && h.payload.agent) || "-") + " / " + ((h.payload && h.payload.host) || "-");
        const poolsText = (h.pool || "-") + (Array.isArray(h.alt_pools) && h.alt_pools.length > 0 ? " (alt: " + h.alt_pools.join(", ") + ")" : "");

        const actionCellChildren = [];
        if (h.status === "held") {
          const releaseBtn = el("button", {
            type: "button",
            className: "btn btn-primary btn-sm",
            text: L("held.release"),
            onClick: async function () {
              releaseBtn.disabled = true;
              i18n.setText(releaseBtn, L("held.releasing"));
              try {
                const relRes = await apiFetch("/gate/held/" + encodeURIComponent(h.id) + "/release", {
                  method: "POST"
                });
                if (relRes.status === 200 && relRes.data && relRes.data.released) {
                  const jid = relRes.data.job_id;
                  showBanner(
                    "success",
                    L("held.released", { id: h.id, jobID: jid }),
                    navLink("/ui/jobs/" + jid, L("common.openJob", { id: jid }), "btn btn-sm")
                  );
                  await renderHeldJobs(searchParams);
                } else if (relRes.status === 409) {
                  const reason = (relRes.data && (relRes.data.reason || relRes.data.error)) || L("held.skippedDefaultReason");
                  outcomeArea.replaceChildren(
                    el("div", {
                      className: "alert alert-warn",
                      text: function () { return t("held.skipped", { id: h.id, reason: i18n.resolve(reason) }); }
                    })
                  );
                  releaseBtn.disabled = false;
                  i18n.setText(releaseBtn, L("held.release"));
                } else {
                  outcomeArea.replaceChildren(
                    el("div", { className: "alert alert-error", text: errorMessageFromResp(relRes, "held.releaseFailed") })
                  );
                  releaseBtn.disabled = false;
                  i18n.setText(releaseBtn, L("held.release"));
                }
              } catch (err) {
                if (!err || err.status !== 401) {
                  outcomeArea.replaceChildren(
                    el("div", { className: "alert alert-error", text: errorAlert("held.releaseError", err) })
                  );
                  releaseBtn.disabled = false;
                  i18n.setText(releaseBtn, L("held.release"));
                }
              }
            }
          });

          const cancelBtn = el("button", {
            type: "button",
            className: "btn btn-danger btn-sm",
            text: L("common.cancel"),
            onClick: async function () {
              if (!window.confirm(t("held.cancelConfirm", { id: h.id }))) {
                return;
              }
              cancelBtn.disabled = true;
              try {
                const delRes = await apiFetch("/gate/held/" + encodeURIComponent(h.id), {
                  method: "DELETE"
                });
                if (!delRes.ok) {
                  outcomeArea.replaceChildren(
                    el("div", { className: "alert alert-error", text: errorMessageFromResp(delRes, "common.cancelFailed") })
                  );
                  cancelBtn.disabled = false;
                  return;
                }
                showBanner("success", L("held.cancelled", { id: h.id }));
                await renderHeldJobs(searchParams);
              } catch (err) {
                if (!err || err.status !== 401) {
                  outcomeArea.replaceChildren(
                    el("div", { className: "alert alert-error", text: errorAlert("common.cancelError", err) })
                  );
                  cancelBtn.disabled = false;
                }
              }
            }
          });

          actionCellChildren.push(el("div", { className: "btn-row" }, [releaseBtn, cancelBtn]));
        } else if (h.released_job_id) {
          actionCellChildren.push(navLink("/ui/jobs/" + h.released_job_id, L("common.jobLabel", { id: h.released_job_id }), "mono"));
        } else {
          actionCellChildren.push(el("span", { className: "muted", text: "-" }));
        }

        return el("tr", null, [
          el("td", { className: "mono", text: h.id }),
          el("td", null, [statusBadge(h.status)]),
          el("td", null, [
            el("div", { text: title }),
            el("div", { className: "small muted mono", text: agentHost })
          ]),
          el("td", { className: "mono small", text: poolsText }),
          el("td", { text: h.reason || "-" }),
          el("td", { className: "mono small", text: h.snapshot_summary || "-" }),
          el("td", { className: "mono small muted", text: formatTime(h.created_at) }),
          el("td", null, actionCellChildren)
        ]);
      });

      contentNode = el("div", { className: "table-wrap" }, [
        el("table", null, [
          el("thead", null, [
            el("tr", null, [
              el("th", { text: L("field.heldID") }),
              el("th", { text: L("column.status") }),
              el("th", { text: L("held.titleAgentHost") }),
              el("th", { text: L("column.pool") }),
              el("th", { text: L("column.holdReason") }),
              el("th", { text: L("held.snapshot") }),
              el("th", { text: L("column.createdAt") }),
              el("th", { text: L("held.actions") })
            ])
          ]),
          el("tbody", null, rows)
        ])
      ]);
    }

    dom.app.replaceChildren(
      el("div", { className: "page-header" }, [
        el("h1", { text: L("held.title") })
      ]),
      tabs,
      outcomeArea,
      contentNode
    );
  }

  // ==================== 6. Gate Decisions (/ui/gate/decisions) ====================
  async function renderGateDecisions(searchParams) {
    const jobIDFilter = (searchParams && searchParams.get("job_id")) || "";
    const heldIDFilter = (searchParams && searchParams.get("held_id")) || "";

    setLoading(L("decisions.title"));
    const params = new URLSearchParams();
    params.set("limit", "100");
    if (jobIDFilter) params.set("job_id", jobIDFilter);
    if (heldIDFilter) params.set("held_id", heldIDFilter);

    const res = await apiFetch("/gate/decisions?" + params.toString());
    if (!res.ok) {
      throw i18nError(errorMessageFromResp(res, "decisions.fetchFailed"));
    }
    const decisions = (res.data && res.data.decisions) || [];

    const jobInput = el("input", { type: "number", name: "job_id", value: jobIDFilter, placeholder: L("decisions.jobIDPlaceholder"), min: "1" });
    const heldInput = el("input", { type: "text", name: "held_id", value: heldIDFilter, placeholder: "held_id" });

    const filterForm = el("form", {
      className: "filter-bar",
      onSubmit: function (ev) {
        ev.preventDefault();
        const sp = new URLSearchParams();
        if (jobInput.value.trim()) sp.set("job_id", jobInput.value.trim());
        if (heldInput.value.trim()) sp.set("held_id", heldInput.value.trim());
        const qs = sp.toString();
        navigate("/ui/gate/decisions" + (qs ? "?" + qs : ""));
      }
    }, [
      el("label", { className: "field" }, [
        el("span", { className: "field-label", text: L("field.jobID") }),
        jobInput
      ]),
      el("label", { className: "field" }, [
        el("span", { className: "field-label", text: L("field.heldID") }),
        heldInput
      ]),
      el("div", { className: "btn-row" }, [
        el("button", { type: "submit", className: "btn btn-primary", text: L("common.filter") }),
        el("button", {
          type: "button",
          className: "btn btn-ghost",
          text: L("common.clear"),
          onClick: function () {
            navigate("/ui/gate/decisions");
          }
        })
      ])
    ]);

    dom.app.replaceChildren(
      el("div", { className: "page-header" }, [
        el("h1", { text: L("decisions.title") })
      ]),
      filterForm,
      buildGateDecisionsTable(decisions)
    );
  }

  function buildGateDecisionsTable(decisions) {
    if (!decisions || decisions.length === 0) {
      return el("div", { className: "card empty-state", text: L("decisions.empty") });
    }
    const rows = decisions.map(function (d) {
      let targetNode;
      if (d.job_id) {
        targetNode = navLink("/ui/jobs/" + d.job_id, L("decisions.targetJob", { id: d.job_id }), "mono");
      } else if (d.held_id) {
        targetNode = navLink("/ui/gate/held", L("decisions.targetHeld", { id: d.held_id }), "mono");
      } else {
        targetNode = el("span", { className: "muted", text: "-" });
      }
      return el("tr", null, [
        el("td", { className: "mono", text: String(d.id) }),
        el("td", null, [statusBadge(d.decision)]),
        el("td", { text: d.mode || "-" }),
        el("td", null, [targetNode]),
        el("td", { className: "mono small", text: d.requested_pool || "-" }),
        el("td", { className: "mono small", text: d.selected_pool || "-" }),
        el("td", { className: "small", text: d.blocked_by || (d.bypassed ? "bypassed" : "-") }),
        el("td", { className: "small", text: d.reason || "-" }),
        el("td", { className: "mono small muted", text: formatTime(d.measured_at) }),
        el("td", { className: "mono small muted", text: formatTime(d.decided_at) })
      ]);
    });
    return el("div", { className: "table-wrap" }, [
      el("table", null, [
        el("thead", null, [
          el("tr", null, [
            el("th", { text: L("field.id") }),
            el("th", { text: L("decisions.decision") }),
            el("th", { text: L("decisions.mode") }),
            el("th", { text: L("decisions.target") }),
            el("th", { text: L("decisions.requestedPool") }),
            el("th", { text: L("decisions.selectedPool") }),
            el("th", { text: L("decisions.blockedBy") }),
            el("th", { text: L("decisions.reason") }),
            el("th", { text: L("decisions.measuredAt") }),
            el("th", { text: L("decisions.decidedAt") })
          ])
        ]),
        el("tbody", null, rows)
      ])
    ]);
  }

  // ==================== 7. Deliveries (/ui/deliveries) ====================
  async function renderDeliveries(searchParams, view) {
    const validStatuses = [
      { key: "undelivered", label: L("deliveries.undelivered") },
      { key: "delivered", label: L("deliveries.delivered") },
      { key: "all", label: L("common.all") }
    ];
    const reqStatus = (searchParams && searchParams.get("status")) || "undelivered";
    const activeStatus = validStatuses.some(function (s) { return s.key === reqStatus; }) ? reqStatus : "undelivered";

    if (!view) {
      setLoading(L("deliveries.title"));
    }
    const res = await apiFetch("/deliveries?status=" + encodeURIComponent(activeStatus) + "&limit=100");
    if (!res.ok) {
      throw i18nError(errorMessageFromResp(res, "deliveries.fetchFailed"));
    }
    const jobs = (res.data && res.data.jobs) || [];

    const tabs = el("div", { className: "tabs" }, validStatuses.map(function (st) {
      return el("button", {
        type: "button",
        className: "tab" + (st.key === activeStatus ? " active" : ""),
        text: st.label,
        onClick: function () {
          navigate("/ui/deliveries?status=" + encodeURIComponent(st.key));
        }
      });
    }));

    if (view && view.isStale()) {
      return;
    }
    dom.app.replaceChildren(
      el("div", { className: "page-header" }, [
        el("h1", { text: L("deliveries.title") })
      ]),
      tabs,
      buildDeliveriesTable(jobs, function () {
        state.renderSeq += 1;
        void renderDeliveries(searchParams);
      })
    );
  }

  function buildDeliveriesTable(jobs, onDelivered) {
    if (!jobs || jobs.length === 0) {
      return el("div", { className: "card empty-state", text: L("deliveries.empty") });
    }
    const rows = jobs.map(function (j) {
      const deliveredBadge = j.delivered_at ? statusBadge("delivered") : statusBadge("undelivered");
      const previewText = j.status === "failed" ? (j.last_error || "-") : (j.result_text || "-");
      const actionNodes = [];
      if (!j.delivered_at && typeof onDelivered === "function") {
        const markBtn = el("button", {
          type: "button",
          className: "btn btn-sm",
          text: L("deliveries.mark"),
          onClick: async function () {
            markBtn.disabled = true;
            try {
              const res = await apiFetch("/jobs/" + j.id + "/deliver", { method: "POST" });
              if (!res.ok) {
                showBanner("error", errorMessageFromResp(res, "deliveries.markFailed"));
                markBtn.disabled = false;
                return;
              }
              showBanner("success", L("deliveries.marked", { id: j.id }));
              onDelivered();
            } catch (err) {
              if (!err || err.status !== 401) {
                showBanner("error", errorAlert("deliveries.markError", err));
                markBtn.disabled = false;
              }
            }
          }
        });
        actionNodes.push(markBtn);
      } else {
        actionNodes.push(el("span", { className: "mono small muted", text: formatTime(j.delivered_at) }));
      }

      return el("tr", null, [
        el("td", { className: "mono" }, [navLink("/ui/jobs/" + j.id, "#" + j.id)]),
        el("td", null, [navLink("/ui/jobs/" + j.id, j.title || L("common.untitled"))]),
        el("td", null, [statusBadge(j.status)]),
        el("td", null, [deliveredBadge]),
        el("td", { className: "mono small", text: (j.agent || "-") + " / " + (j.host || "-") }),
        el("td", { className: "preview-cell small", text: previewText }),
        el("td", { className: "mono small muted", text: formatTime(j.updated_at) }),
        el("td", null, actionNodes)
      ]);
    });

    return el("div", { className: "table-wrap" }, [
      el("table", null, [
        el("thead", null, [
          el("tr", null, [
            el("th", { text: L("field.id") }),
            el("th", { text: L("column.title") }),
            el("th", { text: L("column.status") }),
            el("th", { text: L("deliveries.columnDelivery") }),
            el("th", { text: L("field.agentHost") }),
            el("th", { text: L("deliveries.columnExcerpt") }),
            el("th", { text: L("deliveries.columnUpdatedAt") }),
            el("th", { text: L("deliveries.columnAction") })
          ])
        ]),
        el("tbody", null, rows)
      ])
    ]);
  }

  // ==================== 8. Agent Versions (/ui/agents) ====================
  async function renderAgentVersions() {
    setLoading(L("agents.title"));
    const [agentsRes, eventsRes] = await Promise.all([
      apiFetch("/agents"),
      apiFetch("/agents/events?limit=50")
    ]);
    if (!agentsRes.ok) {
      throw i18nError(errorMessageFromResp(agentsRes, "agents.fetchFailed"));
    }
    const matrix = agentsRes.data || {};
    const events = (eventsRes.ok && eventsRes.data && Array.isArray(eventsRes.data.events))
      ? eventsRes.data.events
      : [];

    const checkBtn = el("button", {
      type: "button",
      className: "btn btn-primary btn-sm",
      text: L("agents.checkNow"),
      onClick: async function () {
        checkBtn.disabled = true;
        i18n.setText(checkBtn, L("agents.checking"));
        try {
          const res = await apiFetch("/agents/check", { method: "POST" });
          if (!res.ok) {
            showBanner("error", errorMessageFromResp(res, "agents.probeFailed"));
            checkBtn.disabled = false;
            i18n.setText(checkBtn, L("agents.checkNow"));
            return;
          }
          const newEvents = (res.data && Array.isArray(res.data.events)) ? res.data.events : [];
          showBanner(
            res.data && res.data.has_drift ? "warn" : "success",
            L("agents.probeDone", { count: newEvents.length })
          );
          await renderAgentVersions();
        } catch (err) {
          if (!err || err.status !== 401) {
            showBanner("error", errorAlert("agents.probeError", err));
            checkBtn.disabled = false;
            i18n.setText(checkBtn, L("agents.checkNow"));
          }
        }
      }
    });

    const sections = [
      el("div", { className: "page-header" }, [
        el("h1", { text: L("agents.title") }),
        el("div", { className: "btn-row" }, [checkBtn])
      ])
    ];

    const summaries = Array.isArray(matrix.summaries) ? matrix.summaries : [];
    const driftedAgents = summaries.filter(function (s) { return s && s.drifted; });
    if (matrix.has_drift && driftedAgents.length > 0) {
      const driftNames = driftedAgents.map(function (s) { return s.agent; }).join(", ");
      const manualHosts = [];
      for (const s of driftedAgents) {
        if (Array.isArray(s.manual_update_hosts)) {
          for (const h of s.manual_update_hosts) {
            manualHosts.push(s.agent + "@" + h);
          }
        }
      }
      const alertMsg = function () {
        const manual = manualHosts.length > 0
          ? t("agents.driftManual", { hosts: manualHosts.join(", ") })
          : "";
        return t("agents.driftAlert", { agents: driftNames }) + manual;
      };
      sections.push(el("div", { className: "alert alert-warn agent-drift-alert", text: alertMsg }));
    }

    if (summaries.length > 0) {
      sections.push(
        el("section", { className: "section" }, [
          el("h2", { className: "section-title", text: L("agents.driftSummary") }),
          buildAgentSummaryCards(summaries)
        ])
      );
    }

    sections.push(
      el("section", { className: "section" }, [
        el("h2", { className: "section-title", text: L("agents.matrix") }),
        buildAgentMatrixTable(matrix)
      ])
    );

    if (matrix.limitation_note) {
      // The server sends this note in Japanese only, so the UI shows its own copy.
      sections.push(el("p", { className: "muted small", text: L("agents.limitationNote") }));
    }

    sections.push(
      el("section", { className: "section" }, [
        el("h2", { className: "section-title", text: L("agents.events") }),
        buildAgentEventsTable(events)
      ])
    );

    dom.app.replaceChildren.apply(dom.app, sections);
  }

  function buildAgentSummaryCards(summaries) {
    const cards = summaries.map(function (s) {
      const cardClass = "card agent-card" + (s.drifted ? " agent-card-drift" : "");
      const badges = [
        el("span", {
          className: "badge " + (s.drifted ? "badge-drift" : "badge-ok"),
          text: s.drifted ? "DRIFT" : "OK"
        })
      ];
      if (!s.comparable && Array.isArray(s.versions) && s.versions.length > 0) {
        badges.push(el("span", { className: "badge badge-warn", text: L("agents.notComparable") }));
      }

      const details = [];
      if (s.max_version) {
        details.push(el("div", { className: "small mono", text: L("agents.maxVersion", { version: s.max_version }) }));
      }
      if (Array.isArray(s.lagging_hosts) && s.lagging_hosts.length > 0) {
        details.push(el("div", { className: "small", text: L("agents.laggingHosts", { hosts: s.lagging_hosts.join(", ") }) }));
      }
      if (Array.isArray(s.manual_update_hosts) && s.manual_update_hosts.length > 0) {
        details.push(el("div", {
          className: "small agent-manual-note",
          text: L("agents.manualHosts", { hosts: s.manual_update_hosts.join(", ") })
        }));
      }
      if (Array.isArray(s.pinned_hosts) && s.pinned_hosts.length > 0) {
        details.push(el("div", { className: "small muted", text: "pinned: " + s.pinned_hosts.join(", ") }));
      }
      if (Array.isArray(s.failed_hosts) && s.failed_hosts.length > 0) {
        details.push(el("div", { className: "small agent-error-note", text: L("agents.failedHosts", { hosts: s.failed_hosts.join(", ") }) }));
      }
      if (details.length === 0) {
        details.push(el("div", { className: "small muted", text: L("agents.noDetails") }));
      }

      return el("div", { className: cardClass }, [
        el("div", { className: "agent-card-header" }, [
          el("span", { className: "mono agent-card-name", text: s.agent }),
          el("span", { className: "agent-card-badges" }, badges)
        ]),
        el("div", { className: "agent-card-body" }, details)
      ]);
    });
    return el("div", { className: "agent-summary-grid" }, cards);
  }

  function buildAgentMatrixTable(matrix) {
    const hosts = Array.isArray(matrix.hosts) ? matrix.hosts : [];
    const agents = Array.isArray(matrix.agents) ? matrix.agents : [];
    const entries = Array.isArray(matrix.entries) ? matrix.entries : [];

    if (hosts.length === 0 || agents.length === 0) {
      return el("div", { className: "card empty-state", text: L("agents.notConfigured") });
    }

    const cellMap = new Map();
    for (const entry of entries) {
      if (entry) {
        cellMap.set(entry.host + "\x00" + entry.agent, entry);
      }
    }

    const headerCells = [el("th", { text: L("column.host") })];
    for (const a of agents) {
      headerCells.push(el("th", { className: "mono", text: a }));
    }

    const rows = hosts.map(function (h) {
      const tds = [el("td", { className: "mono", text: h })];
      for (const a of agents) {
        const c = cellMap.get(h + "\x00" + a);
        tds.push(buildAgentMatrixCell(c));
      }
      return el("tr", null, tds);
    });

    return el("div", { className: "table-wrap" }, [
      el("table", null, [
        el("thead", null, [el("tr", null, headerCells)]),
        el("tbody", null, rows)
      ])
    ]);
  }

  function buildAgentMatrixCell(c) {
    if (!c || !c.status) {
      const notes = [el("span", { className: "muted", text: L("agents.unchecked") })];
      if (c && c.pinned) {
        notes.push(el("span", { className: "badge badge-pinned", text: "pinned: " + c.pin }));
      }
      if (c && c.manual_update) {
        notes.push(el("span", { className: "badge badge-manual", text: L("agents.manualBadge") }));
      }
      return el("td", { className: "agent-cell" }, [el("div", { className: "agent-cell-main" }, notes)]);
    }

    let tdClass = "agent-cell";
    if (c.behind) {
      tdClass += " agent-cell-drift";
    } else if (c.pinned) {
      tdClass += " agent-cell-pinned";
    } else if (c.status === "error" || c.status === "not_found") {
      tdClass += " agent-cell-error";
    }

    const topNodes = [];
    if (c.status === "ok") {
      topNodes.push(el("span", { className: "mono agent-ver-text", text: c.version || "-" }));
    } else {
      topNodes.push(el("span", {
        className: "badge badge-" + String(c.status).replace(/[^a-zA-Z0-9_-]/g, ""),
        text: c.status
      }));
    }

    if (c.pinned) {
      const pinText = c.pin_mismatch ? L("agents.pinMismatch", { pin: c.pin }) : ("pinned: " + c.pin);
      topNodes.push(el("span", { className: "badge badge-pinned", text: pinText }));
    }
    if (c.behind) {
      topNodes.push(el("span", { className: "badge badge-behind", text: L("agents.behind") }));
    }
    if (c.status === "ok" && !c.comparable) {
      topNodes.push(el("span", { className: "badge badge-warn", text: L("agents.notComparable") }));
    }
    if (c.manual_update) {
      topNodes.push(el("span", { className: "badge badge-manual", text: L("agents.manualBadge") }));
    }

    const cellChildren = [el("div", { className: "agent-cell-main" }, topNodes)];
    if (c.status !== "ok" && c.error_detail) {
      cellChildren.push(el("div", { className: "small agent-error-note", text: c.error_detail }));
    } else if (c.version_raw && c.version_raw !== c.version) {
      cellChildren.push(el("div", { className: "small muted mono", text: c.version_raw }));
    }
    if (c.checked_at) {
      cellChildren.push(el("div", { className: "mono small muted", text: formatTime(c.checked_at) }));
    }

    return el("td", { className: tdClass }, cellChildren);
  }

  function buildAgentEventsTable(events) {
    if (!events || events.length === 0) {
      return el("div", { className: "card empty-state", text: L("agents.noEvents") });
    }
    const rows = events.map(function (ev) {
      let changeText = "-";
      if (ev.old_version || ev.new_version) {
        changeText = (ev.old_version || "-") + " -> " + (ev.new_version || "-");
      }
      const detailNodes = [el("span", { text: ev.detail || "-" })];
      if (ev.manual_update) {
        detailNodes.push(document.createTextNode(" "));
        detailNodes.push(el("span", { className: "badge badge-manual", text: L("agents.manualBadge") }));
      }
      return el("tr", null, [
        el("td", { className: "mono small", text: "#" + ev.id }),
        el("td", { className: "mono small muted", text: formatTime(ev.created_at) }),
        el("td", null, [statusBadge(ev.event_type)]),
        el("td", { className: "mono small", text: ev.agent || "-" }),
        el("td", { className: "mono small", text: ev.host || "-" }),
        el("td", { className: "mono small", text: changeText }),
        el("td", { className: "small" }, detailNodes)
      ]);
    });
    return el("div", { className: "table-wrap" }, [
      el("table", null, [
        el("thead", null, [
          el("tr", null, [
            el("th", { text: L("field.id") }),
            el("th", { text: L("agents.columnTime") }),
            el("th", { text: L("agents.columnType") }),
            el("th", { text: L("agents.columnAgent") }),
            el("th", { text: L("column.host") }),
            el("th", { text: L("agents.columnChange") }),
            el("th", { text: L("agents.columnDetail") })
          ])
        ]),
        el("tbody", null, rows)
      ])
    ]);
  }

  // ==================== Job Notifications (docs/14) ====================
  const listRefresher = notifyHelper.createRefresher({
    refresh: refreshListView,
    delayMs: 500,
    retryMs: 2000
  });

  function getNotificationCtor() {
    return typeof window.Notification === "function" ? window.Notification : null;
  }

  const notificationPresenter = notifyHelper.createNotificationPresenter({
    // Absent in insecure contexts and some private browsing modes.
    getServiceWorker: function () { return navigator.serviceWorker || null; },
    getNotification: getNotificationCtor,
    getUserAgent: function () { return navigator.userAgent; },
    // Read when about to display: the toggle may change while the worker is activating.
    isEnabled: function () { return notifyController.isEnabled(); },
    focus: function () { window.focus(); },
    openJob: function (jobID) { navigate("/ui/jobs/" + jobID); }
  });

  // The latest view of the notification toggle (docs/14); the Web Push view is added to
  // it when the button is drawn (docs/16).
  let notifyView = { enabled: false, problemKey: "" };

  // notifyLabelKey names the state of the toggle together with the Web Push state.
  function notifyLabelKey(enabled, state) {
    if (!enabled) {
      // Notifications are off here, but pushes may still arrive.
      if (state === "removing") {
        return "push.labelRemoving";
      }
      return state === "unconfirmed" ? "push.labelUnconfirmed" : "header.notify";
    }
    if (state === "active") {
      return "push.labelActive";
    }
    if (state === "pending") {
      return "push.labelPending";
    }
    if (state === "unsupported" || state === "server-off") {
      return "push.labelTabOnly";
    }
    // Otherwise an earlier registration may still deliver.
    return state === "off" ? "header.notify" : "push.labelUnchecked";
  }

  function renderNotifyButton() {
    const view = notifyView;
    const push = pushController.view();
    const removal = !view.enabled &&
      (push.state === "removing" || push.state === "unconfirmed");
    i18n.setText(dom.btnNotify, L(notifyLabelKey(view.enabled, push.state)));
    dom.btnNotify.setAttribute("aria-pressed", view.enabled ? "true" : "false");
    // An active subscription still delivers when only the event stream has a problem.
    dom.btnNotify.classList.toggle("notify-problem",
      Boolean(view.problemKey) && push.state !== "active");
    i18n.setAttr(dom.btnNotify, "title", function () {
      return [
        t(view.problemKey || (view.enabled ? "notify.toggleOn" : "notify.toggleOff")),
        view.enabled || removal ? t(push.messageKey) : "",
        t("push.iosNote")
      ].filter(Boolean).join("\n");
    });
  }

  const pushController = notifyHelper.createPushController({
    isSupported: function () {
      return window.isSecureContext !== false && Boolean(navigator.serviceWorker) &&
        typeof window.PushManager === "function";
    },
    getRegistration: notificationPresenter.register,
    findRegistration: function () {
      return navigator.serviceWorker.getRegistration("/ui/");
    },
    // Not apiFetch: a token without the operate scope must not open the auth gate.
    fetch: function (url, opts) { return window.fetch(url, opts); },
    getToken: getStoredToken,
    isEnabled: function () { return notifyController.isEnabled(); },
    onChange: function () { renderNotifyButton(); }
  });

  const notifyController = notifyHelper.createNotifyController({
    storage: {
      getItem: function (key) { return localStorage.getItem(key); },
      setItem: function (key, value) { localStorage.setItem(key, value); }
    },
    getNotification: getNotificationCtor,
    isSecureContext: function () { return window.isSecureContext !== false; },
    hasCredentials: hasCredentialsConfigured,
    hasToken: function () { return getStoredToken() !== ""; },
    createStream: function (handlers) {
      return notifyHelper.createEventStream(Object.assign({
        url: "/events",
        fetch: function (url, opts) { return window.fetch(url, opts); },
        getToken: getStoredToken
      }, handlers));
    },
    prepare: function () { void notificationPresenter.register(); },
    notify: notifyHelper.createJobNotifier({
      getNotification: getNotificationCtor,
      show: notificationPresenter.show,
      onError: function () {
        showBanner("error", L("notify.showFailed"));
      }
    }),
    onListChanged: function () { listRefresher.request(); },
    isIOS: function () {
      return notifyHelper.isIOS(navigator.userAgent, navigator.maxTouchPoints);
    },
    onChange: function (view) {
      notifyView = view;
      renderNotifyButton();
      void pushController.sync();
    },
    onMessage: function (_message, key) { showBanner("error", L(key)); },
    onUnauthorized: function (_message, key) {
      // Do not wipe a token the user is already typing.
      if (dom.authGate.hidden) {
        showAuthGate(L(key));
      }
    }
  });

  // ==================== Global Event Wiring ====================
  async function tryConnect(tokenValue, noAuth) {
    dom.authError.hidden = true;
    // The open stream belongs to the previous credentials.
    notifyController.stop();
    if (noAuth) {
      sessionStorage.removeItem(TOKEN_KEY);
      sessionStorage.setItem(AUTH_MODE_KEY, "none");
    } else {
      sessionStorage.setItem(TOKEN_KEY, tokenValue);
      sessionStorage.removeItem(AUTH_MODE_KEY);
    }

    try {
      const res = await apiFetch("/status");
      if (!res.ok) {
        i18n.setText(dom.authError, errorMessageFromResp(res, "auth.connectFailed"));
        dom.authError.hidden = false;
        return;
      }
      state.status = res.data;
      hideAuthGate();
      notifyController.restart();
      await refreshHeaderMeta();
      await renderCurrentRoute();
    } catch (err) {
      if (err && err.status === 401) {
        sessionStorage.removeItem(AUTH_MODE_KEY);
        return;
      }
      i18n.setText(dom.authError, errorAlert("auth.connectError", err));
      dom.authError.hidden = false;
    }
  }

  dom.authForm.addEventListener("submit", function (ev) {
    ev.preventDefault();
    const token = dom.authTokenInput.value.trim();
    if (!token) {
      i18n.setText(dom.authError, L("auth.tokenMissing"));
      dom.authError.hidden = false;
      return;
    }
    void tryConnect(token, false);
  });

  dom.btnConnectNoAuth.addEventListener("click", function () {
    void tryConnect("", true);
  });

  // After turning notifications on, say whether they also arrive with the page closed;
  // after turning them off, say if pushes may still arrive.
  async function toggleNotifications() {
    const wasEnabled = notifyController.isEnabled();
    const before = pushController.view().state;
    // Until a removal is confirmed, the button retries it instead of turning on.
    if (!wasEnabled && (before === "removing" || before === "unconfirmed")) {
      const retried = await pushController.sync();
      if (retried.state === "unconfirmed") {
        showBanner("error", L(retried.messageKey));
      } else if (retried.state === "off") {
        showBanner("success", L("push.removed"));
      }
      return;
    }
    await notifyController.toggle();
    if (wasEnabled) {
      const removed = await pushController.settled();
      if (removed.state === "unconfirmed") {
        showBanner("error", L(removed.messageKey));
      }
      return;
    }
    if (!notifyController.isEnabled()) {
      return;
    }
    const view = await pushController.settled();
    // A problem of the event stream has its own banner, which stays.
    if (view.state !== "off" && !notifyView.problemKey && notifyController.isEnabled()) {
      showBanner(view.state === "active" ? "success" : "info", function () {
        return t("push.banner", { message: t(view.messageKey), note: t("push.iosNote") });
      });
    }
  }

  dom.btnNotify.addEventListener("click", function () {
    clearBanner();
    void toggleNotifications();
  });

  ["input", "change"].forEach(function (type) {
    dom.app.addEventListener(type, function () {
      state.viewDirty = true;
    });
  });

  dom.btnRefresh.addEventListener("click", function () {
    clearBanner();
    void refreshHeaderMeta();
    void renderCurrentRoute();
  });

  dom.btnToken.addEventListener("click", function () {
    showAuthGate("");
  });

  if (dom.directionSwitch) {
    dom.directionSwitch.addEventListener("click", function (ev) {
      const btn = ev.target && ev.target.closest ? ev.target.closest("[data-set-direction]") : null;
      if (!btn) {
        return;
      }
      applyDirection(btn.getAttribute("data-set-direction"), true);
    });
  }

  if (dom.localeSwitch) {
    dom.localeSwitch.addEventListener("click", function (ev) {
      const btn = ev.target && ev.target.closest ? ev.target.closest("[data-set-locale]") : null;
      if (!btn) {
        return;
      }
      i18n.setLocale(btn.getAttribute("data-set-locale"), true);
    });
  }

  window.addEventListener("storage", function (ev) {
    if (ev && ev.key === i18n.LOCALE_KEY) {
      i18n.setLocale(ev.newValue, false);
    }
    if (ev && ev.key === DIRECTION_KEY) {
      applyDirection(ev.newValue, false);
    }
    if (ev && ev.key === notifyHelper.NOTIFY_KEY) {
      notifyController.reload();
    }
  });

  document.addEventListener("click", function (ev) {
    if (ev.defaultPrevented || ev.button !== 0 || ev.metaKey || ev.ctrlKey || ev.shiftKey || ev.altKey) {
      return;
    }
    const link = ev.target && ev.target.closest ? ev.target.closest("a[data-nav='true']") : null;
    if (!link) {
      return;
    }
    const href = link.getAttribute("href");
    if (!href || !href.startsWith("/ui")) {
      return;
    }
    ev.preventDefault();
    navigate(href);
  });

  window.addEventListener("popstate", function () {
    clearBanner();
    void renderCurrentRoute();
  });

  // Boot
  // Registered even with notifications off: the app shell cache needs it (docs/15).
  void notificationPresenter.cacheShell();
  window.addEventListener("online", function () { void notificationPresenter.cacheShell(); });
  notifyController.sync();
  if (hasCredentialsConfigured()) {
    void refreshHeaderMeta();
    void renderCurrentRoute();
  } else {
    showAuthGate("");
  }
})();

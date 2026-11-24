(function () {
  "use strict";

  const TOKEN_KEY = "quota_fleet_token";
  const AUTH_MODE_KEY = "quota_fleet_auth_mode";
  const directionHelper =
    (typeof globalThis !== "undefined" && globalThis.__qfDirection) ||
    (typeof window !== "undefined" && window.__qfDirection) ||
    (typeof require === "function" ? require("./direction.js") : null);
  const DIRECTION_KEY = directionHelper ? directionHelper.DIRECTION_KEY : "qf-direction";
  const detectPreferredDirection = directionHelper.detectPreferredDirection;
  const resolveDirection = directionHelper.resolveDirection;
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
    { key: "done", label: "done" },
    { key: "claimed", label: "実行中" },
    { key: "queued", label: "queued" },
    { key: "waiting", label: "waiting" },
    { key: "failed", label: "失敗" },
    { key: "cancelled", label: "キャンセル" },
    { key: "draft", label: "draft" }
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
      return "全0件";
    }
    const counts = summary.counts || {};
    const parts = [];
    const knownKeys = new Set();
    for (const item of SUMMARY_STATUS_ORDER) {
      knownKeys.add(item.key);
      const n = counts[item.key] || 0;
      if (n > 0) {
        parts.push(item.label + " " + n + "件");
      }
    }
    for (const key of Object.keys(counts)) {
      if (!knownKeys.has(key) && counts[key] > 0) {
        parts.push(key + " " + counts[key] + "件");
      }
    }
    if (parts.length === 0) {
      return "全" + summary.total + "件";
    }
    return "全" + summary.total + "件中 " + parts.join("・");
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
            message: (err && err.message) ? String(err.message) : String(err || "通信エラー")
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
                : "不正なレスポンス形式")
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
      return "取得エラー";
    }
    const httpStatus = Number(errItem.httpStatus || 0);
    const msg = errItem.message ? String(errItem.message) : "";
    if (httpStatus > 0) {
      if (msg && msg.indexOf("HTTP " + httpStatus) === 0) {
        return msg;
      }
      return msg ? ("HTTP " + httpStatus + " " + msg) : ("HTTP " + httpStatus);
    }
    return msg || "取得エラー";
  }

  function formatChainFetchErrorNotice(chainMeta) {
    if (!chainMeta || !Array.isArray(chainMeta.fetchErrors) || chainMeta.fetchErrors.length === 0) {
      return null;
    }
    const maxItems = 6;
    const items = chainMeta.fetchErrors.slice(0, maxItems).map(function (errItem) {
      const parentLabel = (errItem && errItem.parentId !== undefined && errItem.parentId !== null)
        ? ("#" + errItem.parentId)
        : "不明な親";
      const statusLabel = (errItem && errItem.status) ? String(errItem.status) : "不明な状態";
      return parentLabel + " [" + statusLabel + ": " + formatFetchErrorDetail(errItem) + "]";
    });
    const extra = chainMeta.fetchErrors.length > maxItems
      ? (" 他 " + (chainMeta.fetchErrors.length - maxItems) + " 件")
      : "";
    return "一部の子ジョブ取得に失敗したため、チェーン構成に欠落がある可能性があります（失敗: " +
      items.join("、") + extra + "）。取得できた分のみ表示しています。";
  }

  function formatDirectChildrenFetchErrorNotice(chainMeta, currentJobID) {
    if (!chainMeta || !Array.isArray(chainMeta.currentJobChildrenErrors) || chainMeta.currentJobChildrenErrors.length === 0) {
      return null;
    }
    const items = chainMeta.currentJobChildrenErrors.map(function (errItem) {
      const statusLabel = (errItem && errItem.status) ? String(errItem.status) : "不明な状態";
      return statusLabel + " [" + formatFetchErrorDetail(errItem) + "]";
    });
    const targetLabel = (currentJobID !== undefined && currentJobID !== null)
      ? ("ジョブ #" + currentJobID)
      : "表示中ジョブ";
    return targetLabel + " の直下の子ジョブ取得が一部失敗したため、一覧に欠落がある可能性があります（失敗状態: " +
      items.join("、") + "）。取得できた分のみ表示しています。";
  }

  function formatChainTruncationNotice(chainMeta) {
    if (!chainMeta || !chainMeta.truncated) {
      return null;
    }
    const reasons = [];
    if (chainMeta.expandTruncated) {
      reasons.push("他 " + chainMeta.unexpandedCount + " 件のノード配下の探索を上限で打ち切りました");
    }
    if (chainMeta.upTruncated) {
      reasons.push("祖先の探索を上限で打ち切りました");
    }
    if (chainMeta.pagesTruncated) {
      reasons.push("一部の子ジョブ一覧がページ取得上限に達しました");
    }
    const hasErrors = Boolean(
      chainMeta.hasFetchErrors ||
      chainMeta.currentJobChildrenFailed ||
      (Array.isArray(chainMeta.fetchErrors) && chainMeta.fetchErrors.length > 0) ||
      (Array.isArray(chainMeta.currentJobChildrenErrors) && chainMeta.currentJobChildrenErrors.length > 0)
    );
    const directNote = (chainMeta.currentJobChildrenTruncated || hasErrors)
      ? ""
      : "表示中ジョブの直下の子ジョブは全件取得済みです。";
    return "チェーンが大きいため一部省略されています（" +
      (reasons.length > 0 ? reasons.join("・") : "探索上限に到達") +
      "）。" + directNote;
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
    return "選択中の agent (" + a + ") に対応するホストがありません。";
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
      return "pool: " + p + "（自動）";
    }
    return "pool: なし（自動判定対象外）";
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
      formatDerivedPoolLabel: formatDerivedPoolLabel
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
    flatPages: 1
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
          node.textContent = String(val);
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
          node.setAttribute(key, String(val));
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
      text: label
    });
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

  function showAuthGate(errorMessage) {
    state.authenticated = false;
    dom.app.hidden = true;
    dom.authGate.hidden = false;
    dom.authTokenInput.value = getStoredToken();
    if (errorMessage) {
      dom.authError.textContent = errorMessage;
      dom.authError.hidden = false;
    } else {
      dom.authError.textContent = "";
      dom.authError.hidden = true;
    }
    dom.authTokenInput.focus();
  }

  function hideAuthGate() {
    dom.authGate.hidden = true;
    dom.app.hidden = false;
    dom.authError.textContent = "";
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
      const msg = (data && data.error) ? data.error : "認証が必要です（401 Unauthorized）";
      showAuthGate(msg);
      const err = new Error(msg);
      err.status = 401;
      err.data = data;
      throw err;
    }

    return { ok: resp.ok, status: resp.status, data: data };
  }

  function errorMessageFromResp(res, fallback) {
    if (res && res.data && res.data.error) {
      return String(res.data.error);
    }
    return fallback + " (HTTP " + (res ? res.status : "?") + ")";
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
      el("div", { className: "card empty-state", text: "読み込み中..." })
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
              el("h1", { text: "ページが見つかりません" }),
              el("p", { className: "muted", text: route.path }),
              navLink("/ui/", "ダッシュボードに戻る", "btn btn-primary")
            ])
          );
      }
      state.authenticated = true;
    } catch (err) {
      if (err && err.status === 401) {
        return;
      }
      dom.app.replaceChildren(
        el("div", { className: "alert alert-error", text: "エラー: " + (err ? err.message : String(err)) })
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

  // refreshListView redraws the current list view without a loading placeholder.
  // It returns false when it has to be retried later (a render or user input is in progress).
  async function refreshListView() {
    const route = parseRoute(new URL(window.location.href));
    if (!LIVE_ROUTES.has(route.name) || dom.app.hidden || !hasCredentialsConfigured()) {
      return true;
    }
    if (state.rendering > 0 || isEditingView()) {
      return false;
    }
    const seq = state.renderSeq;
    const view = {
      isStale: function () {
        return seq !== state.renderSeq || dom.app.hidden || isEditingView();
      }
    };
    if (route.name === "dashboard") {
      await renderDashboard(view);
    } else if (route.name === "jobs") {
      await renderJobsList(route.search, view);
    } else {
      await renderDeliveries(route.search, view);
    }
    return !(seq === state.renderSeq && isEditingView());
  }

  // ==================== 1. Dashboard (/ui/) ====================
  // view is set only for a background refresh (see refreshListView).
  async function renderDashboard(view) {
    if (!view) {
      setLoading("ダッシュボード");
    }
    const [statusRes, metaRes, claimedRes, heldRes, delivRes] = await Promise.all([
      apiFetch("/status"),
      apiFetch("/meta"),
      apiFetch("/jobs?status=claimed&desc=true&limit=20"),
      apiFetch("/gate/held?status=held&limit=20"),
      apiFetch("/deliveries?status=all&limit=10")
    ]);

    if (!statusRes.ok) {
      throw new Error(errorMessageFromResp(statusRes, "ステータス取得に失敗しました"));
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
        el("div", { className: "stat-label", text: "total" }),
        el("div", { className: "stat-value", text: String(statusRes.data.total || 0) })
      ])
    );

    const sections = [
      el("div", { className: "page-header" }, [
        el("h1", { text: "ダッシュボード" }),
        el("div", { className: "btn-row" }, [
          navLink("/ui/jobs/new", "+ 新規ジョブ登録", "btn btn-primary")
        ])
      ]),
      el("section", { className: "section" }, [
        el("h2", { className: "section-title", text: "状態別件数" }),
        el("div", { className: "stat-grid" }, statCards)
      ])
    ];

    if (state.meta && Array.isArray(state.meta.pool_summaries) && state.meta.pool_summaries.length > 0) {
      const poolCards = state.meta.pool_summaries.map(function (ps) {
        const lines = [];
        if (ps.unmeasured) {
          lines.push(el("div", { className: "small muted", text: "unmeasured (バイパス)" }));
        } else {
          if (ps.weekly_remaining_percent !== undefined && ps.weekly_remaining_percent !== null) {
            lines.push(el("div", { className: "small", text: "週次残量: " + ps.weekly_remaining_percent + "%" }));
          }
          if (ps.short_remaining_percent !== undefined && ps.short_remaining_percent !== null) {
            lines.push(el("div", { className: "small", text: "短期残量: " + ps.short_remaining_percent + "%" }));
          }
          if (ps.measured_at) {
            lines.push(el("div", { className: "small muted" }, [
              "測定: ",
              el("span", { className: "mono small", text: formatTime(ps.measured_at) })
            ]));
          }
        }
        if (ps.reason) {
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
          el("h2", { className: "section-title", text: "プール残量サマリ" }),
          el("div", { className: "pool-grid" }, poolCards)
        ])
      );
    }

    const claimedJobs = (claimedRes.ok && claimedRes.data && claimedRes.data.jobs) || [];
    sections.push(
      el("section", { className: "section" }, [
        el("h2", { className: "section-title", text: "実行中ジョブ (" + claimedJobs.length + ")" }),
        buildJobsTable(claimedJobs, "実行中のジョブはありません。")
      ])
    );

    const heldItems = (heldRes.ok && heldRes.data && heldRes.data.held) || [];
    sections.push(
      el("section", { className: "section" }, [
        el("h2", { className: "section-title", text: "保持中ジョブ (" + heldItems.length + ")" }),
        buildHeldSummaryTable(heldItems)
      ])
    );

    const deliveries = (delivRes.ok && delivRes.data && delivRes.data.jobs) || [];
    sections.push(
      el("section", { className: "section" }, [
        el("h2", { className: "section-title", text: "直近の納品 (" + deliveries.length + ")" }),
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
      return el("div", { className: "card empty-state", text: "保持中のジョブはありません。" });
    }
    const rows = items.map(function (h) {
      const title = (h.payload && h.payload.title) ? h.payload.title : "(タイトルなし)";
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
            el("th", { text: "Held ID" }),
            el("th", { text: "タイトル" }),
            el("th", { text: "プール" }),
            el("th", { text: "保持理由" }),
            el("th", { text: "作成時刻" })
          ])
        ]),
        el("tbody", null, rows)
      ])
    ]);
  }

  function buildJobsTable(jobs, emptyText) {
    if (!jobs || jobs.length === 0) {
      return el("div", { className: "card empty-state", text: emptyText || "ジョブはありません。" });
    }
    const rows = jobs.map(function (j) {
      const agentHost = (j.agent || "-") + " / " + (j.host || "-");
      const parentCell = j.parent_id
        ? navLink("/ui/jobs/" + j.parent_id, "#" + j.parent_id, "mono")
        : el("span", { className: "muted", text: "-" });
      return el("tr", null, [
        el("td", { className: "mono" }, [navLink("/ui/jobs/" + j.id, "#" + j.id)]),
        el("td", null, [navLink("/ui/jobs/" + j.id, j.title || "(無題)")]),
        el("td", null, [statusBadge(j.status)]),
        el("td", { className: "mono", text: agentHost }),
        el("td", { className: "mono small muted", text: formatTime(j.created_at) }),
        el("td", null, [parentCell])
      ]);
    });
    return el("div", { className: "table-wrap" }, [
      el("table", null, [
        el("thead", null, [
          el("tr", null, [
            el("th", { text: "ID" }),
            el("th", { text: "タイトル" }),
            el("th", { text: "状態" }),
            el("th", { text: "agent / host" }),
            el("th", { text: "作成時刻" }),
            el("th", { text: "親ジョブ" })
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
      return el("div", { className: "card empty-state", text: emptyText || "該当するジョブはありません。" });
    }
    const rows = [];
    for (const rootNode of forest) {
      const flatRows = flattenChainTree(rootNode);
      for (const item of flatRows) {
        const j = item.job;
        const agentHost = (j.agent || "-") + " / " + (j.host || "-");
        const lineChildren = [
          navLink("/ui/jobs/" + j.id, "#" + j.id, "mono"),
          navLink("/ui/jobs/" + j.id, j.title || "(無題)")
        ];
        if (item.isOrphanRoot && j.parent_id) {
          lineChildren.push(
            el("span", { className: "chain-orphan-note" }, [
              "(親 ",
              navLink("/ui/jobs/" + j.parent_id, "#" + j.parent_id, "mono"),
              " 未取得)"
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
              text: formatChainProgressSummary(item.summary)
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
          el("td", null, [statusBadge(j.status)]),
          el("td", { className: "mono", text: agentHost }),
          el("td", { className: "mono small muted", text: formatTime(j.created_at) })
        ]));
      }
    }

    return el("div", { className: "table-wrap" }, [
      el("table", { className: "chain-table" }, [
        el("thead", null, [
          el("tr", null, [
            el("th", { scope: "col", text: "チェーン / ジョブ" }),
            el("th", { scope: "col", text: "状態" }),
            el("th", { scope: "col", text: "agent / host" }),
            el("th", { scope: "col", text: "作成時刻" })
          ])
        ]),
        el("tbody", null, rows)
      ])
    ]);
  }

  // ==================== 2. Jobs List (/ui/jobs) ====================
  async function renderJobsList(searchParams, view) {
    const requestedView = (searchParams && searchParams.get("view")) || "flat";
    const activeView = requestedView === "chain" ? "chain" : "flat";
    const requestedStatus = (searchParams && searchParams.get("status")) || (activeView === "chain" ? "all" : "queued");
    const activeStatus = activeView === "chain"
      ? (requestedStatus === "all" || ALL_STATUSES.includes(requestedStatus) ? requestedStatus : "all")
      : (ALL_STATUSES.includes(requestedStatus) ? requestedStatus : "queued");
    const filterAgent = (searchParams && searchParams.get("agent")) || "";
    const filterHost = (searchParams && searchParams.get("host")) || "";
    const filterQ = (searchParams && searchParams.get("q")) || "";

    if (!view) {
      setLoading("ジョブ一覧");
    }

    function buildJobsURL(view, st, ag, ho, q) {
      const sp = new URLSearchParams();
      if (view === "chain") {
        sp.set("view", "chain");
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
        state.meta ? Promise.resolve({ ok: true, data: state.meta }) : apiFetch("/meta"),
        ...statusFetches
      ]);
      statusRes = results[0];
      metaRes = results[1];
      const listResults = results.slice(2);

      if (!statusRes.ok) {
        throw new Error(errorMessageFromResp(statusRes, "ステータス取得に失敗しました"));
      }
      for (const item of listResults) {
        if (!item.res.ok) {
          throw new Error(errorMessageFromResp(item.res, "ジョブ一覧 (" + item.status + ") の取得に失敗しました"));
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
        state.meta ? Promise.resolve({ ok: true, data: state.meta }) : apiFetch("/meta"),
        apiFetch("/jobs?" + flatParams.toString())
      ]);
      statusRes = results[0];
      metaRes = results[1];
      const jobsRes = results[2];

      if (!statusRes.ok) {
        throw new Error(errorMessageFromResp(statusRes, "ステータス取得に失敗しました"));
      }
      if (!jobsRes.ok) {
        throw new Error(errorMessageFromResp(jobsRes, "ジョブ一覧の取得に失敗しました"));
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
          throw new Error(errorMessageFromResp(pageRes, "ジョブ一覧の取得に失敗しました"));
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

    const viewToggle = el("div", { className: "view-toggle", role: "group", "aria-label": "表示切替" }, [
      el("button", {
        type: "button",
        className: "view-toggle-btn" + (activeView === "flat" ? " active" : ""),
        "aria-pressed": activeView === "flat" ? "true" : "false",
        text: "フラット表示",
        onClick: function () {
          if (activeView !== "flat") {
            const nextStatus = activeStatus === "all" ? "queued" : activeStatus;
            navigate(buildJobsURL("flat", nextStatus, filterAgent, filterHost, filterQ));
          }
        }
      }),
      el("button", {
        type: "button",
        className: "view-toggle-btn" + (activeView === "chain" ? " active" : ""),
        "aria-pressed": activeView === "chain" ? "true" : "false",
        text: "チェーン表示",
        onClick: function () {
          if (activeView !== "chain") {
            navigate(buildJobsURL("chain", "all", filterAgent, filterHost, filterQ));
          }
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
          el("span", { text: "すべて" }),
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

    const agentOpts = [el("option", { value: "", text: "すべて" })];
    const agents = (state.meta && Array.isArray(state.meta.agents)) ? state.meta.agents : [];
    for (const a of agents) {
      agentOpts.push(el("option", { value: a, text: a, selected: a === filterAgent }));
    }
    if (filterAgent && !agents.includes(filterAgent)) {
      agentOpts.push(el("option", { value: filterAgent, text: filterAgent, selected: true }));
    }

    const hostOpts = [el("option", { value: "", text: "すべて" })];
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
      placeholder: "タイトル部分一致..."
    });

    const filterForm = el("form", {
      className: "filter-bar",
      onSubmit: function (ev) {
        ev.preventDefault();
        navigate(buildJobsURL(activeView, activeStatus, agentSelect.value, hostSelect.value, qInput.value.trim()));
      }
    }, [
      el("label", { className: "field" }, [
        el("span", { className: "field-label", text: "Agent" }),
        agentSelect
      ]),
      el("label", { className: "field" }, [
        el("span", { className: "field-label", text: "Host" }),
        hostSelect
      ]),
      el("label", { className: "field" }, [
        el("span", { className: "field-label", text: "タイトル検索" }),
        qInput
      ]),
      el("div", { className: "btn-row" }, [
        el("button", { type: "submit", className: "btn btn-primary", text: "絞り込み" }),
        el("button", {
          type: "button",
          className: "btn btn-ghost",
          text: "クリア",
          onClick: function () {
            navigate(buildJobsURL(activeView, activeStatus, "", "", ""));
          }
        })
      ])
    ]);

    const truncationNotice = (activeView === "chain" && truncatedStatuses.length > 0)
      ? el("div", {
          className: "alert alert-info small",
          text: "一部の状態 (" + truncatedStatuses.join(", ") + ") は最新 100 件まで取得してチェーンを構築しています。"
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
      tableNode = buildChainJobsTable(visibleForest, "該当するチェーンはありません。");
    } else {
      tableNode = buildJobsTable(loadedJobs, "該当するジョブはありません。");
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
        text: "さらに読み込む",
        onClick: async function () {
          btn.disabled = true;
          btn.textContent = "読み込み中...";
          try {
            const nextParams = new URLSearchParams(flatParams.toString());
            nextParams.set("cursor", String(nextCursor));
            const moreRes = await apiFetch("/jobs?" + nextParams.toString());
            if (!moreRes.ok) {
              showBanner("error", errorMessageFromResp(moreRes, "追加取得に失敗しました"));
              return;
            }
            const moreJobs = (moreRes.data && moreRes.data.jobs) || [];
            loadedJobs = loadedJobs.concat(moreJobs);
            nextCursor = moreRes.data ? moreRes.data.next_cursor : 0;
            hasMore = Boolean(moreRes.data && moreRes.data.has_more);
            state.flatPages += 1;
            state.renderSeq += 1;
            tableContainer.replaceChildren(buildJobsTable(loadedJobs, "該当するジョブはありません。"));
            updateLoadMore();
          } catch (err) {
            if (!err || err.status !== 401) {
              showBanner("error", "追加取得エラー: " + (err ? err.message : String(err)));
            }
          }
        }
      });
      loadMoreContainer.appendChild(btn);
    }
    updateLoadMore();

    const pageNodes = [
      el("div", { className: "page-header" }, [
        el("h1", { text: "ジョブ一覧" }),
        el("div", { className: "btn-row" }, [
          viewToggle,
          navLink("/ui/jobs/new", "+ 新規ジョブ登録", "btn btn-primary")
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
        el("h2", { className: "section-title", text: "チェーン構成" }),
        el("div", { className: "empty-state", text: "チェーン情報を取得できませんでした。" })
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
        text: "#" + j.id + " " + (j.title || "(無題)")
      };
      if (isCurrent) {
        linkAttrs["aria-current"] = "page";
      }

      const nodeParts = [
        el("a", linkAttrs),
        statusBadge(j.status)
      ];
      if (isCurrent) {
        nodeParts.push(el("span", { className: "mini-tree-current-tag", text: "現在表示中" }));
      }
      if (node.isOrphanRoot && j.parent_id) {
        nodeParts.push(
          el("span", { className: "chain-orphan-note" }, [
            "(親 ",
            navLink("/ui/jobs/" + j.parent_id, "#" + j.parent_id, "mono"),
            " 未取得)"
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
        el("h2", { className: "section-title", text: "チェーン構成" }),
        el("span", { className: "chain-summary", text: formatChainProgressSummary(summary) })
      ])
    ];

    const fetchErrorNotice = formatChainFetchErrorNotice(chainMeta);
    if (fetchErrorNotice) {
      sectionChildren.push(
        el("div", { className: "alert alert-error small", role: "alert", text: fetchErrorNotice })
      );
    }

    const truncationNotice = formatChainTruncationNotice(chainMeta);
    if (truncationNotice) {
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
        el("div", { className: "alert alert-error", text: "不正なジョブ ID です: " + rawID }),
        navLink("/ui/jobs", "ジョブ一覧に戻る", "btn")
      );
      return;
    }

    setLoading("ジョブ #" + jobID);

    const jobRes = await apiFetch("/jobs/" + jobID);
    if (!jobRes.ok) {
      throw new Error(errorMessageFromResp(jobRes, "ジョブ #" + jobID + " の取得に失敗しました"));
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
      ? navLink("/ui/jobs/" + job.parent_id, "ジョブ #" + job.parent_id, "mono")
      : "-";

    const wtText = job.worktree_spec
      ? [
          "repo_path=" + (job.worktree_spec.repo_path || ""),
          job.worktree_spec.base_ref ? ("base_ref=" + job.worktree_spec.base_ref) : "",
          job.worktree_spec.branch ? ("branch=" + job.worktree_spec.branch) : ""
        ].filter(Boolean).join(", ")
      : "-";

    const metaGrid = el("div", { className: "kv-grid" }, [
      kv("状態", statusBadge(job.status)),
      kv("Request ID", el("span", { className: "mono", text: job.request_id || "-" })),
      kv("Agent", job.agent),
      kv("Host", job.host),
      kv("Workdir", el("span", { className: "mono", text: job.workdir || "-" })),
      kv("Model", job.model),
      kv("Pool", job.pool),
      kv("Alt Pools", (Array.isArray(job.alt_pools) && job.alt_pools.length > 0) ? job.alt_pools.join(", ") : "-"),
      kv("親ジョブ", parentNode),
      kv("Target Ref", el("span", { className: "mono", text: job.target_ref || "-" })),
      kv("Worktree Spec", el("span", { className: "mono", text: wtText })),
      kv("Attempts", String(job.attempts || 0)),
      kv("Claimed By", el("span", { className: "mono", text: job.claimed_by || "-" })),
      kv("Claimed At", el("span", { className: "mono", text: formatTime(job.claimed_at) })),
      kv("Created At", el("span", { className: "mono", text: formatTime(job.created_at) })),
      kv("Updated At", el("span", { className: "mono", text: formatTime(job.updated_at) })),
      kv("Delivered At", el("span", { className: "mono", text: formatTime(job.delivered_at) })),
      kv("Session ID", el("span", { className: "mono", text: job.session_id || "-" }))
    ]);

    // Allowed actions strictly per state:
    // - cancel: draft | queued | waiting (NEVER for claimed)
    // - requeue: failed | cancelled
    const actionNodes = [];
    if (CANCELLABLE_STATUSES.has(job.status)) {
      const reasonInput = el("input", {
        type: "text",
        placeholder: "キャンセル理由（任意）"
      });
      const cancelBtn = el("button", {
        type: "button",
        className: "btn btn-danger",
        text: "キャンセル",
        onClick: async function () {
          if (!window.confirm("ジョブ #" + job.id + " をキャンセルしますか？")) {
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
              showBanner("error", errorMessageFromResp(res, "キャンセルに失敗しました"));
              cancelBtn.disabled = false;
              return;
            }
            const cascaded = (res.data && Array.isArray(res.data.cancelled_child_ids)) ? res.data.cancelled_child_ids : [];
            const extra = cascaded.length > 0 ? "（子ジョブ " + cascaded.map(function (c) { return "#" + c; }).join(", ") + " もキャンセル）" : "";
            showBanner("success", "ジョブ #" + job.id + " をキャンセルしました。" + extra);
            await renderJobDetail(String(job.id));
          } catch (err) {
            if (!err || err.status !== 401) {
              showBanner("error", "キャンセルエラー: " + (err ? err.message : String(err)));
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
        text: "Requeue（再投入）",
        onClick: async function () {
          requeueBtn.disabled = true;
          try {
            const res = await apiFetch("/jobs/" + job.id + "/requeue", { method: "POST" });
            if (!res.ok) {
              showBanner("error", errorMessageFromResp(res, "Requeue に失敗しました"));
              requeueBtn.disabled = false;
              return;
            }
            showBanner("success", "ジョブ #" + job.id + " を queued に戻しました。");
            await renderJobDetail(String(job.id));
          } catch (err) {
            if (!err || err.status !== 401) {
              showBanner("error", "Requeue エラー: " + (err ? err.message : String(err)));
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
        return el("div", { className: "card empty-state", text: "このジョブはまだ完了／失敗していないため納品対象外です。" });
      }
      const badge = job.delivered_at ? statusBadge("delivered") : statusBadge("undelivered");
      return el("div", { className: "card" }, [
        el("div", { className: "kv-grid" }, [
          kv("納品状態", badge),
          kv("納品時刻", el("span", { className: "mono", text: formatTime(job.delivered_at) })),
          kv("ジョブ状態", statusBadge(job.status))
        ])
      ]);
    })();

    const miniChainSection = buildMiniChainSection(chainJobs, job.id, chainResult);
    const directChildrenErrorNotice = formatDirectChildrenFetchErrorNotice(chainResult, job.id);
    const childrenSectionHeading = directChildrenErrorNotice
      ? ("子ジョブ一覧 (" + children.length + "件取得・一部取得失敗)")
      : ("子ジョブ一覧 (" + children.length + ")");
    const childrenEmptyText = directChildrenErrorNotice
      ? "取得できた子ジョブはありません（一部取得に失敗しているため、未表示の子ジョブがある可能性があります）。"
      : "子ジョブはありません。";
    const childrenSectionNodes = [
      el("h2", { className: "section-title", text: childrenSectionHeading })
    ];
    if (directChildrenErrorNotice) {
      childrenSectionNodes.push(
        el("div", { className: "alert alert-error small", role: "alert", text: directChildrenErrorNotice })
      );
    }
    childrenSectionNodes.push(buildJobsTable(children, childrenEmptyText));

    dom.app.replaceChildren(
      el("div", { className: "page-header" }, [
        el("h1", { text: "ジョブ #" + job.id + ": " + (job.title || "(無題)") }),
        el("div", { className: "btn-row" }, [
          navLink("/ui/jobs?view=chain", "チェーン一覧", "btn"),
          navLink("/ui/jobs?status=" + encodeURIComponent(job.status), "一覧に戻る", "btn")
        ])
      ]),
      overviewCard,
      miniChainSection,
      el("section", { className: "section card" }, [
        el("h2", { className: "section-title", text: "指示文 (instructions)" }),
        el("pre", { text: job.instructions || "(なし)" })
      ]),
      el("section", { className: "section card" }, [
        el("h2", { className: "section-title", text: "受け入れ条件 (acceptance_criteria)" }),
        el("pre", { text: job.acceptance_criteria || "(なし)" })
      ]),
      el("section", { className: "section card" }, [
        el("h2", { className: "section-title", text: "結果 (result_text)" }),
        el("pre", { text: job.result_text || "(なし)" })
      ]),
      el("section", { className: "section card" }, [
        el("h2", { className: "section-title", text: "エラー (last_error)" }),
        el("pre", { text: job.last_error || "(なし)" })
      ]),
      el("section", { className: "section" }, childrenSectionNodes),
      el("section", { className: "section" }, [
        el("h2", { className: "section-title", text: "ゲート判定履歴 (" + decisions.length + ")" }),
        buildGateDecisionsTable(decisions)
      ]),
      el("section", { className: "section" }, [
        el("h2", { className: "section-title", text: "納品記録" }),
        deliverySummary
      ])
    );
  }

  // ==================== 4. New Job Form (/ui/jobs/new) ====================
  async function renderNewJobForm() {
    setLoading("新規ジョブ登録");

    const parentStatuses = ["queued", "claimed", "waiting", "draft", "done"];
    const [metaRes, ...parentResults] = await Promise.all([
      apiFetch("/meta"),
      ...parentStatuses.map(function (st) {
        return apiFetch("/jobs?status=" + encodeURIComponent(st) + "&desc=true&limit=25");
      })
    ]);

    if (!metaRes.ok) {
      throw new Error(errorMessageFromResp(metaRes, "メタ情報の取得に失敗しました"));
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
      text: "再生成",
      onClick: function () {
        requestIDInput.value = generateRequestID();
      }
    });

    const titleInput = el("input", {
      type: "text",
      name: "title",
      placeholder: "ジョブのタイトル（必須）",
      required: true
    });

    const agentSelect = el("select", { name: "agent" }, [
      el("option", { value: "", text: "-- 選択 --" }),
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
      placeholder: "任意（例: claude-sonnet-4-5）"
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
      poolDisplay.textContent = formatDerivedPoolLabel(derived);
    }

    function updateModelControl() {
      const catalog = getModelsForHostAgent(state.meta, hostSelect.value, agentSelect.value);
      const prevVal = getSelectedModelValue();
      if (catalog.length > 0) {
        const opts = [el("option", { value: "", text: "（指定なし / デフォルト）" })];
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
        hostSelect.replaceChildren(el("option", { value: "", text: "（対応ホストなし）" }));
        hostSelect.value = "";
        hostSelect.disabled = true;
        hostNotice.textContent = notice;
        hostNotice.hidden = false;
      } else {
        const opts = [el("option", { value: "", text: "-- 選択 --" })];
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
        hostNotice.textContent = "";
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
        : [el("span", { className: "muted small", text: "利用可能なプール候補がありません" })]
    );

    const parentSelect = el("select", { name: "parent_id" }, [
      el("option", { value: "", text: "（なし）" }),
      ...parentCandidates.map(function (j) {
        return el("option", {
          value: String(j.id),
          text: "#" + j.id + " [" + j.status + "] " + (j.title || "(無題)")
        });
      })
    ]);

    const draftCheckbox = el("input", { type: "checkbox", name: "draft" });

    const instructionsInput = el("textarea", {
      name: "instructions",
      placeholder: "エージェントへの指示文"
    });

    const acceptanceInput = el("textarea", {
      name: "acceptance_criteria",
      placeholder: "受け入れ条件（任意）"
    });

    const targetRefInput = el("input", {
      type: "text",
      name: "target_ref",
      placeholder: "任意（例: origin/main や PR URL）"
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
    const submitBtn = el("button", { type: "submit", className: "btn btn-primary", text: "ジョブを登録" });

    const form = el("form", {
      className: "card form-stack",
      onSubmit: async function (ev) {
        ev.preventDefault();
        resultArea.replaceChildren();

        const reqID = requestIDInput.value.trim();
        const title = titleInput.value.trim();
        if (!reqID || !title) {
          resultArea.replaceChildren(
            el("div", { className: "alert alert-error", text: "request_id と title は必須です。" })
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
        submitBtn.textContent = "送信中...";
        try {
          const res = await apiFetch("/jobs", {
            method: "POST",
            body: JSON.stringify(payload)
          });

          if (res.status === 202) {
            const heldID = (res.data && res.data.held_id) || (res.data && res.data.held && res.data.held.id) || "-";
            const reason = (res.data && res.data.held && res.data.held.reason) || "クォータゲートにより保持されました";
            const snap = (res.data && res.data.held && res.data.held.snapshot_summary) || "";
            resultArea.replaceChildren(
              el("div", { className: "alert alert-warn" }, [
                el("strong", { text: "ゲート保持 (HTTP 202 Accepted)" }),
                el("div", { className: "mono", text: "held_id: " + heldID }),
                el("div", { text: "理由: " + reason }),
                snap ? el("div", { className: "small muted", text: "スナップショット: " + snap }) : null,
                el("div", { className: "btn-row" }, [
                  navLink("/ui/gate/held", "保持中ジョブ画面で確認する", "btn btn-sm")
                ])
              ])
            );
          } else if (res.status === 201 || res.status === 200) {
            const job = res.data && res.data.job;
            const created = Boolean(res.data && res.data.created);
            const heading = created
              ? "ジョブを作成しました (HTTP 201 Created)"
              : "既存のジョブを返しました（冪等再送 / HTTP 200 OK）";
            resultArea.replaceChildren(
              el("div", { className: "alert alert-success" }, [
                el("strong", { text: heading }),
                job
                  ? el("div", { text: "ID: #" + job.id + " / タイトル: " + job.title + " / 状態: " + job.status })
                  : null,
                job
                  ? el("div", { className: "btn-row" }, [
                      navLink("/ui/jobs/" + job.id, "ジョブ #" + job.id + " の詳細を開く", "btn btn-primary btn-sm"),
                      el("button", {
                        type: "button",
                        className: "btn btn-sm",
                        text: "新しい request_id を発行して続けて登録",
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
              el("div", { className: "alert alert-error", text: errorMessageFromResp(res, "登録に失敗しました") })
            );
          }
        } catch (err) {
          if (!err || err.status !== 401) {
            resultArea.replaceChildren(
              el("div", { className: "alert alert-error", text: "送信エラー: " + (err ? err.message : String(err)) })
            );
          }
        } finally {
          submitBtn.disabled = false;
          submitBtn.textContent = "ジョブを登録";
        }
      }
    }, [
      el("div", { className: "field" }, [
        el("span", { className: "field-label", text: "request_id（自動生成・冪等キー）" }),
        el("div", { className: "input-row" }, [requestIDInput, regenBtn])
      ]),
      el("label", { className: "field" }, [
        el("span", { className: "field-label", text: "タイトル (title) *" }),
        titleInput
      ]),
      el("div", { className: "form-grid" }, [
        el("label", { className: "field" }, [
          el("span", { className: "field-label", text: "Agent" }),
          agentSelect
        ]),
        el("label", { className: "field" }, [
          el("span", { className: "field-label", text: "Host" }),
          hostSelect,
          hostNotice
        ]),
        el("div", { className: "field" }, [
          el("span", { className: "field-label", text: "Model" }),
          modelControlWrap
        ]),
        el("div", { className: "field" }, [
          el("span", { className: "field-label", text: "Pool（自動判定）" }),
          poolDisplay
        ])
      ]),
      el("div", { className: "form-grid" }, [
        el("label", { className: "field" }, [
          el("span", { className: "field-label", text: "Workdir" }),
          workdirInput
        ]),
        el("label", { className: "field" }, [
          el("span", { className: "field-label", text: "親ジョブ (parent_id)" }),
          parentSelect
        ])
      ]),
      el("div", { className: "field" }, [
        el("span", { className: "field-label", text: "代替プール (alt_pools)" }),
        altPoolsGroup
      ]),
      el("label", { className: "field-inline" }, [
        draftCheckbox,
        el("span", { text: "ドラフトとして登録する (draft)" })
      ]),
      el("label", { className: "field" }, [
        el("span", { className: "field-label", text: "指示文 (instructions)" }),
        instructionsInput
      ]),
      el("label", { className: "field" }, [
        el("span", { className: "field-label", text: "受け入れ条件 (acceptance_criteria)" }),
        acceptanceInput
      ]),
      el("details", { className: "advanced-box" }, [
        el("summary", { text: "詳細設定（worktree_spec / target_ref）" }),
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
        navLink("/ui/jobs", "キャンセル", "btn btn-ghost")
      ])
    ]);

    dom.app.replaceChildren(
      el("div", { className: "page-header" }, [
        el("h1", { text: "新規ジョブ登録" })
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

    setLoading("保持中ジョブ");
    const res = await apiFetch("/gate/held?status=" + encodeURIComponent(activeStatus) + "&limit=100");
    if (!res.ok) {
      throw new Error(errorMessageFromResp(res, "保持中ジョブの取得に失敗しました"));
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
      contentNode = el("div", { className: "card empty-state", text: "該当する保持ジョブはありません。" });
    } else {
      const rows = items.map(function (h) {
        const title = (h.payload && h.payload.title) ? h.payload.title : "(タイトルなし)";
        const agentHost = ((h.payload && h.payload.agent) || "-") + " / " + ((h.payload && h.payload.host) || "-");
        const poolsText = (h.pool || "-") + (Array.isArray(h.alt_pools) && h.alt_pools.length > 0 ? " (alt: " + h.alt_pools.join(", ") + ")" : "");

        const actionCellChildren = [];
        if (h.status === "held") {
          const releaseBtn = el("button", {
            type: "button",
            className: "btn btn-primary btn-sm",
            text: "解放試行",
            onClick: async function () {
              releaseBtn.disabled = true;
              releaseBtn.textContent = "評価中...";
              try {
                const relRes = await apiFetch("/gate/held/" + encodeURIComponent(h.id) + "/release", {
                  method: "POST"
                });
                if (relRes.status === 200 && relRes.data && relRes.data.released) {
                  const jid = relRes.data.job_id;
                  showBanner(
                    "success",
                    "保持ジョブ " + h.id + " を解放しました（ジョブ #" + jid + " を作成）。",
                    navLink("/ui/jobs/" + jid, "ジョブ #" + jid + " を開く", "btn btn-sm")
                  );
                  await renderHeldJobs(searchParams);
                } else if (relRes.status === 409) {
                  const reason = (relRes.data && (relRes.data.reason || relRes.data.error)) || "条件未回復のため見送り";
                  outcomeArea.replaceChildren(
                    el("div", { className: "alert alert-warn", text: "解放見送り (" + h.id + "): " + reason })
                  );
                  releaseBtn.disabled = false;
                  releaseBtn.textContent = "解放試行";
                } else {
                  outcomeArea.replaceChildren(
                    el("div", { className: "alert alert-error", text: errorMessageFromResp(relRes, "解放試行に失敗しました") })
                  );
                  releaseBtn.disabled = false;
                  releaseBtn.textContent = "解放試行";
                }
              } catch (err) {
                if (!err || err.status !== 401) {
                  outcomeArea.replaceChildren(
                    el("div", { className: "alert alert-error", text: "解放試行エラー: " + (err ? err.message : String(err)) })
                  );
                  releaseBtn.disabled = false;
                  releaseBtn.textContent = "解放試行";
                }
              }
            }
          });

          const cancelBtn = el("button", {
            type: "button",
            className: "btn btn-danger btn-sm",
            text: "キャンセル",
            onClick: async function () {
              if (!window.confirm("保持ジョブ " + h.id + " をキャンセルしますか？")) {
                return;
              }
              cancelBtn.disabled = true;
              try {
                const delRes = await apiFetch("/gate/held/" + encodeURIComponent(h.id), {
                  method: "DELETE"
                });
                if (!delRes.ok) {
                  outcomeArea.replaceChildren(
                    el("div", { className: "alert alert-error", text: errorMessageFromResp(delRes, "キャンセルに失敗しました") })
                  );
                  cancelBtn.disabled = false;
                  return;
                }
                showBanner("success", "保持ジョブ " + h.id + " をキャンセルしました。");
                await renderHeldJobs(searchParams);
              } catch (err) {
                if (!err || err.status !== 401) {
                  outcomeArea.replaceChildren(
                    el("div", { className: "alert alert-error", text: "キャンセルエラー: " + (err ? err.message : String(err)) })
                  );
                  cancelBtn.disabled = false;
                }
              }
            }
          });

          actionCellChildren.push(el("div", { className: "btn-row" }, [releaseBtn, cancelBtn]));
        } else if (h.released_job_id) {
          actionCellChildren.push(navLink("/ui/jobs/" + h.released_job_id, "ジョブ #" + h.released_job_id, "mono"));
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
              el("th", { text: "Held ID" }),
              el("th", { text: "状態" }),
              el("th", { text: "タイトル / agent・host" }),
              el("th", { text: "プール" }),
              el("th", { text: "保持理由" }),
              el("th", { text: "判定スナップショット" }),
              el("th", { text: "作成時刻" }),
              el("th", { text: "操作" })
            ])
          ]),
          el("tbody", null, rows)
        ])
      ]);
    }

    dom.app.replaceChildren(
      el("div", { className: "page-header" }, [
        el("h1", { text: "保持中ジョブ" })
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

    setLoading("ゲート判定履歴");
    const params = new URLSearchParams();
    params.set("limit", "100");
    if (jobIDFilter) params.set("job_id", jobIDFilter);
    if (heldIDFilter) params.set("held_id", heldIDFilter);

    const res = await apiFetch("/gate/decisions?" + params.toString());
    if (!res.ok) {
      throw new Error(errorMessageFromResp(res, "ゲート判定履歴の取得に失敗しました"));
    }
    const decisions = (res.data && res.data.decisions) || [];

    const jobInput = el("input", { type: "number", name: "job_id", value: jobIDFilter, placeholder: "例: 42", min: "1" });
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
        el("span", { className: "field-label", text: "Job ID" }),
        jobInput
      ]),
      el("label", { className: "field" }, [
        el("span", { className: "field-label", text: "Held ID" }),
        heldInput
      ]),
      el("div", { className: "btn-row" }, [
        el("button", { type: "submit", className: "btn btn-primary", text: "絞り込み" }),
        el("button", {
          type: "button",
          className: "btn btn-ghost",
          text: "クリア",
          onClick: function () {
            navigate("/ui/gate/decisions");
          }
        })
      ])
    ]);

    dom.app.replaceChildren(
      el("div", { className: "page-header" }, [
        el("h1", { text: "ゲート判定履歴" })
      ]),
      filterForm,
      buildGateDecisionsTable(decisions)
    );
  }

  function buildGateDecisionsTable(decisions) {
    if (!decisions || decisions.length === 0) {
      return el("div", { className: "card empty-state", text: "ゲート判定履歴はありません。" });
    }
    const rows = decisions.map(function (d) {
      let targetNode;
      if (d.job_id) {
        targetNode = navLink("/ui/jobs/" + d.job_id, "Job #" + d.job_id, "mono");
      } else if (d.held_id) {
        targetNode = navLink("/ui/gate/held", "Held " + d.held_id, "mono");
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
            el("th", { text: "ID" }),
            el("th", { text: "判定" }),
            el("th", { text: "モード" }),
            el("th", { text: "対象" }),
            el("th", { text: "要求プール" }),
            el("th", { text: "選択プール" }),
            el("th", { text: "ブロック / バイパス" }),
            el("th", { text: "理由" }),
            el("th", { text: "測定時刻" }),
            el("th", { text: "判定時刻" })
          ])
        ]),
        el("tbody", null, rows)
      ])
    ]);
  }

  // ==================== 7. Deliveries (/ui/deliveries) ====================
  async function renderDeliveries(searchParams, view) {
    const validStatuses = [
      { key: "undelivered", label: "未納品" },
      { key: "delivered", label: "納品済み" },
      { key: "all", label: "すべて" }
    ];
    const reqStatus = (searchParams && searchParams.get("status")) || "undelivered";
    const activeStatus = validStatuses.some(function (s) { return s.key === reqStatus; }) ? reqStatus : "undelivered";

    if (!view) {
      setLoading("納品");
    }
    const res = await apiFetch("/deliveries?status=" + encodeURIComponent(activeStatus) + "&limit=100");
    if (!res.ok) {
      throw new Error(errorMessageFromResp(res, "納品一覧の取得に失敗しました"));
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
        el("h1", { text: "納品" })
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
      return el("div", { className: "card empty-state", text: "該当する納品対象ジョブはありません。" });
    }
    const rows = jobs.map(function (j) {
      const deliveredBadge = j.delivered_at ? statusBadge("delivered") : statusBadge("undelivered");
      const previewText = j.status === "failed" ? (j.last_error || "-") : (j.result_text || "-");
      const actionNodes = [];
      if (!j.delivered_at && typeof onDelivered === "function") {
        const markBtn = el("button", {
          type: "button",
          className: "btn btn-sm",
          text: "納品済みにする",
          onClick: async function () {
            markBtn.disabled = true;
            try {
              const res = await apiFetch("/jobs/" + j.id + "/deliver", { method: "POST" });
              if (!res.ok) {
                showBanner("error", errorMessageFromResp(res, "納品済みの記録に失敗しました"));
                markBtn.disabled = false;
                return;
              }
              showBanner("success", "ジョブ #" + j.id + " を納品済みにしました。");
              onDelivered();
            } catch (err) {
              if (!err || err.status !== 401) {
                showBanner("error", "納品更新エラー: " + (err ? err.message : String(err)));
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
        el("td", null, [navLink("/ui/jobs/" + j.id, j.title || "(無題)")]),
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
            el("th", { text: "ID" }),
            el("th", { text: "タイトル" }),
            el("th", { text: "状態" }),
            el("th", { text: "納品" }),
            el("th", { text: "agent / host" }),
            el("th", { text: "結果 / エラー抜粋" }),
            el("th", { text: "更新時刻" }),
            el("th", { text: "納品操作 / 時刻" })
          ])
        ]),
        el("tbody", null, rows)
      ])
    ]);
  }

  // ==================== 8. Agent Versions (/ui/agents) ====================
  async function renderAgentVersions() {
    setLoading("エージェント版数");
    const [agentsRes, eventsRes] = await Promise.all([
      apiFetch("/agents"),
      apiFetch("/agents/events?limit=50")
    ]);
    if (!agentsRes.ok) {
      throw new Error(errorMessageFromResp(agentsRes, "エージェント版数の取得に失敗しました"));
    }
    const matrix = agentsRes.data || {};
    const events = (eventsRes.ok && eventsRes.data && Array.isArray(eventsRes.data.events))
      ? eventsRes.data.events
      : [];

    const checkBtn = el("button", {
      type: "button",
      className: "btn btn-primary btn-sm",
      text: "今すぐ確認",
      onClick: async function () {
        checkBtn.disabled = true;
        checkBtn.textContent = "確認中...";
        try {
          const res = await apiFetch("/agents/check", { method: "POST" });
          if (!res.ok) {
            showBanner("error", errorMessageFromResp(res, "プローブ実行に失敗しました"));
            checkBtn.disabled = false;
            checkBtn.textContent = "今すぐ確認";
            return;
          }
          const newEvents = (res.data && Array.isArray(res.data.events)) ? res.data.events : [];
          showBanner(
            res.data && res.data.has_drift ? "warn" : "success",
            "プローブが完了しました（新規イベント: " + newEvents.length + "件）。"
          );
          await renderAgentVersions();
        } catch (err) {
          if (!err || err.status !== 401) {
            showBanner("error", "プローブエラー: " + (err ? err.message : String(err)));
            checkBtn.disabled = false;
            checkBtn.textContent = "今すぐ確認";
          }
        }
      }
    });

    const sections = [
      el("div", { className: "page-header" }, [
        el("h1", { text: "エージェント版数" }),
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
      let alertMsg = "ドリフトを検出しています: " + driftNames;
      if (manualHosts.length > 0) {
        alertMsg += "（自動更新不可・手作業が必要: " + manualHosts.join(", ") + "）";
      }
      sections.push(el("div", { className: "alert alert-warn agent-drift-alert", text: alertMsg }));
    }

    if (summaries.length > 0) {
      sections.push(
        el("section", { className: "section" }, [
          el("h2", { className: "section-title", text: "ドリフト要約" }),
          buildAgentSummaryCards(summaries)
        ])
      );
    }

    sections.push(
      el("section", { className: "section" }, [
        el("h2", { className: "section-title", text: "ホスト × エージェント 版マトリクス" }),
        buildAgentMatrixTable(matrix)
      ])
    );

    if (matrix.limitation_note) {
      sections.push(el("p", { className: "muted small", text: matrix.limitation_note }));
    }

    sections.push(
      el("section", { className: "section" }, [
        el("h2", { className: "section-title", text: "変化イベント履歴（直近50件）" }),
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
        badges.push(el("span", { className: "badge badge-warn", text: "比較不能" }));
      }

      const details = [];
      if (s.max_version) {
        details.push(el("div", { className: "small mono", text: "最大版: " + s.max_version }));
      }
      if (Array.isArray(s.lagging_hosts) && s.lagging_hosts.length > 0) {
        details.push(el("div", { className: "small", text: "遅れ: " + s.lagging_hosts.join(", ") }));
      }
      if (Array.isArray(s.manual_update_hosts) && s.manual_update_hosts.length > 0) {
        details.push(el("div", {
          className: "small agent-manual-note",
          text: "要手作業: " + s.manual_update_hosts.join(", ")
        }));
      }
      if (Array.isArray(s.pinned_hosts) && s.pinned_hosts.length > 0) {
        details.push(el("div", { className: "small muted", text: "pinned: " + s.pinned_hosts.join(", ") }));
      }
      if (Array.isArray(s.failed_hosts) && s.failed_hosts.length > 0) {
        details.push(el("div", { className: "small agent-error-note", text: "失敗: " + s.failed_hosts.join(", ") }));
      }
      if (details.length === 0) {
        details.push(el("div", { className: "small muted", text: "未取得または差異なし" }));
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
      return el("div", { className: "card empty-state", text: "ホストまたはエージェントが設定されていません。" });
    }

    const cellMap = new Map();
    for (const entry of entries) {
      if (entry) {
        cellMap.set(entry.host + "\x00" + entry.agent, entry);
      }
    }

    const headerCells = [el("th", { text: "ホスト" })];
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
      const notes = [el("span", { className: "muted", text: "未確認" })];
      if (c && c.pinned) {
        notes.push(el("span", { className: "badge badge-pinned", text: "pinned: " + c.pin }));
      }
      if (c && c.manual_update) {
        notes.push(el("span", { className: "badge badge-manual", text: "要手作業" }));
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
      const pinText = c.pin_mismatch ? ("pinned: " + c.pin + " (不一致)") : ("pinned: " + c.pin);
      topNodes.push(el("span", { className: "badge badge-pinned", text: pinText }));
    }
    if (c.behind) {
      topNodes.push(el("span", { className: "badge badge-behind", text: "遅れ" }));
    }
    if (c.status === "ok" && !c.comparable) {
      topNodes.push(el("span", { className: "badge badge-warn", text: "比較不能" }));
    }
    if (c.manual_update) {
      topNodes.push(el("span", { className: "badge badge-manual", text: "要手作業" }));
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
      return el("div", { className: "card empty-state", text: "記録されたエージェント版数イベントはありません。" });
    }
    const rows = events.map(function (ev) {
      let changeText = "-";
      if (ev.old_version || ev.new_version) {
        changeText = (ev.old_version || "-") + " -> " + (ev.new_version || "-");
      }
      const detailNodes = [el("span", { text: ev.detail || "-" })];
      if (ev.manual_update) {
        detailNodes.push(document.createTextNode(" "));
        detailNodes.push(el("span", { className: "badge badge-manual", text: "要手作業" }));
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
            el("th", { text: "ID" }),
            el("th", { text: "時刻" }),
            el("th", { text: "種別" }),
            el("th", { text: "エージェント" }),
            el("th", { text: "ホスト" }),
            el("th", { text: "変化" }),
            el("th", { text: "詳細" })
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

  const notifyController = notifyHelper.createNotifyController({
    storage: {
      getItem: function (key) { return localStorage.getItem(key); },
      setItem: function (key, value) { localStorage.setItem(key, value); }
    },
    getNotification: getNotificationCtor,
    isSecureContext: function () { return window.isSecureContext !== false; },
    hasCredentials: hasCredentialsConfigured,
    createStream: function (handlers) {
      return notifyHelper.createEventStream(Object.assign({
        url: "/events",
        fetch: function (url, opts) { return window.fetch(url, opts); },
        getToken: getStoredToken
      }, handlers));
    },
    notify: notifyHelper.createJobNotifier({
      getNotification: getNotificationCtor,
      focus: function () { window.focus(); },
      openJob: function (jobID) { navigate("/ui/jobs/" + jobID); },
      onError: function () {
        showBanner("error", "このブラウザではページから通知を表示できませんでした。");
      }
    }),
    onListChanged: function () { listRefresher.request(); },
    onChange: function (view) {
      dom.btnNotify.setAttribute("aria-pressed", view.enabled ? "true" : "false");
      dom.btnNotify.classList.toggle("notify-problem", Boolean(view.problem));
      dom.btnNotify.title = view.problem ||
        ("ジョブ完了のブラウザ通知: " + (view.enabled ? "オン" : "オフ"));
    },
    onMessage: function (message) { showBanner("error", message); },
    onUnauthorized: function (message) {
      // Do not wipe a token the user is already typing.
      if (dom.authGate.hidden) {
        showAuthGate(message);
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
        dom.authError.textContent = errorMessageFromResp(res, "接続に失敗しました");
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
      dom.authError.textContent = "接続エラー: " + (err ? err.message : String(err));
      dom.authError.hidden = false;
    }
  }

  dom.authForm.addEventListener("submit", function (ev) {
    ev.preventDefault();
    const token = dom.authTokenInput.value.trim();
    if (!token) {
      dom.authError.textContent = "トークンを入力してください。";
      dom.authError.hidden = false;
      return;
    }
    void tryConnect(token, false);
  });

  dom.btnConnectNoAuth.addEventListener("click", function () {
    void tryConnect("", true);
  });

  dom.btnNotify.addEventListener("click", function () {
    clearBanner();
    void notifyController.toggle();
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

  window.addEventListener("storage", function (ev) {
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
  notifyController.sync();
  if (hasCredentialsConfigured()) {
    void refreshHeaderMeta();
    void renderCurrentRoute();
  } else {
    showAuthGate("");
  }
})();

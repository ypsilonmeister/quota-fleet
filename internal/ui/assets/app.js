(function () {
  "use strict";

  const TOKEN_KEY = "quota_fleet_token";
  const AUTH_MODE_KEY = "quota_fleet_auth_mode";
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

  if (typeof module !== "undefined" && module.exports) {
    module.exports = {
      buildJobForest: buildJobForest,
      summarizeChainProgress: summarizeChainProgress,
      formatChainProgressSummary: formatChainProgressSummary,
      flattenChainTree: flattenChainTree,
      treeContainsStatus: treeContainsStatus
    };
  }

  if (typeof document === "undefined" || typeof window === "undefined") {
    return;
  }

  const state = {
    meta: null,
    status: null,
    authenticated: false
  };

  const dom = {
    app: document.getElementById("app"),
    authGate: document.getElementById("auth-gate"),
    authForm: document.getElementById("auth-form"),
    authTokenInput: document.getElementById("auth-token-input"),
    authError: document.getElementById("auth-error"),
    btnConnectNoAuth: document.getElementById("btn-connect-no-auth"),
    btnRefresh: document.getElementById("btn-refresh"),
    btnToken: document.getElementById("btn-token"),
    metaBadges: document.getElementById("meta-badges"),
    bannerArea: document.getElementById("banner-area")
  };

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
    }
  }

  // ==================== 1. Dashboard (/ui/) ====================
  async function renderDashboard() {
    setLoading("ダッシュボード");
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
            lines.push(el("div", { className: "small muted", text: "測定: " + formatTime(ps.measured_at) }));
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
        el("td", { className: "small muted", text: formatTime(h.created_at) })
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
        el("td", { className: "small muted", text: formatTime(j.created_at) }),
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
          el("td", { className: "small muted", text: formatTime(j.created_at) })
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
  async function renderJobsList(searchParams) {
    const requestedView = (searchParams && searchParams.get("view")) || "flat";
    const activeView = requestedView === "chain" ? "chain" : "flat";
    const requestedStatus = (searchParams && searchParams.get("status")) || (activeView === "chain" ? "all" : "queued");
    const activeStatus = activeView === "chain"
      ? (requestedStatus === "all" || ALL_STATUSES.includes(requestedStatus) ? requestedStatus : "all")
      : (ALL_STATUSES.includes(requestedStatus) ? requestedStatus : "queued");
    const filterAgent = (searchParams && searchParams.get("agent")) || "";
    const filterHost = (searchParams && searchParams.get("host")) || "";
    const filterQ = (searchParams && searchParams.get("q")) || "";

    setLoading("ジョブ一覧");

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

    dom.app.replaceChildren.apply(dom.app, pageNodes);
  }

  async function fetchChildrenAcrossStatuses(parentID) {
    const results = await Promise.all(ALL_STATUSES.map(function (st) {
      return apiFetch("/jobs?status=" + encodeURIComponent(st) + "&parent_id=" + parentID + "&limit=100");
    }));
    const children = [];
    for (const cr of results) {
      if (cr.ok && cr.data && Array.isArray(cr.data.jobs)) {
        for (const cj of cr.data.jobs) {
          children.push(cj);
        }
      }
    }
    return children;
  }

  async function fetchFullJobChain(job) {
    const MAX_UP_STEPS = 50;
    const MAX_EXPAND_NODES = 50;

    const knownJobs = new Map();
    knownJobs.set(job.id, job);

    // 1. Walk upward via parent_id to locate the chain root (cycle-safe).
    let current = job;
    const seenUp = new Set([job.id]);
    let steps = 0;
    while (current.parent_id && Number.isInteger(current.parent_id) && current.parent_id > 0 && steps < MAX_UP_STEPS) {
      if (seenUp.has(current.parent_id)) {
        break;
      }
      seenUp.add(current.parent_id);
      steps += 1;
      const parentRes = await apiFetch("/jobs/" + current.parent_id);
      if (!parentRes.ok || !parentRes.data || typeof parentRes.data.id !== "number") {
        break;
      }
      knownJobs.set(parentRes.data.id, parentRes.data);
      current = parentRes.data;
    }

    // 2. Walk downward (BFS) from the discovered root across all statuses.
    const queue = [current.id];
    const expanded = new Set();
    while (queue.length > 0 && expanded.size < MAX_EXPAND_NODES) {
      const budget = MAX_EXPAND_NODES - expanded.size;
      const batch = [];
      while (queue.length > 0 && batch.length < budget) {
        const nextID = queue.shift();
        if (!expanded.has(nextID)) {
          expanded.add(nextID);
          batch.push(nextID);
        }
      }
      if (batch.length === 0) {
        break;
      }
      const batchResults = await Promise.all(batch.map(fetchChildrenAcrossStatuses));
      for (const kids of batchResults) {
        for (const cj of kids) {
          if (!cj || typeof cj.id !== "number") {
            continue;
          }
          if (!knownJobs.has(cj.id)) {
            knownJobs.set(cj.id, cj);
          }
          if (!expanded.has(cj.id)) {
            queue.push(cj.id);
          }
        }
      }
    }

    return Array.from(knownJobs.values());
  }

  function buildMiniChainSection(chainJobs, currentJobID) {
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

    return el("section", { className: "section card" }, [
      el("div", { className: "mini-tree-header" }, [
        el("h2", { className: "section-title", text: "チェーン構成" }),
        el("span", { className: "chain-summary", text: formatChainProgressSummary(summary) })
      ]),
      el("ul", { className: "mini-tree", role: "list" }, rootLi ? [rootLi] : [])
    ]);
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

    const [decisionsRes, chainJobs] = await Promise.all([
      apiFetch("/gate/decisions?job_id=" + jobID + "&limit=50"),
      fetchFullJobChain(job)
    ]);

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
      kv("Claimed At", formatTime(job.claimed_at)),
      kv("Created At", formatTime(job.created_at)),
      kv("Updated At", formatTime(job.updated_at)),
      kv("Delivered At", formatTime(job.delivered_at)),
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
          kv("納品時刻", formatTime(job.delivered_at)),
          kv("ジョブ状態", statusBadge(job.status))
        ])
      ]);
    })();

    const miniChainSection = buildMiniChainSection(chainJobs, job.id);

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
      el("section", { className: "section" }, [
        el("h2", { className: "section-title", text: "子ジョブ一覧 (" + children.length + ")" }),
        buildJobsTable(children, "子ジョブはありません。")
      ]),
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
    const hosts = Array.isArray(state.meta.hosts) ? state.meta.hosts : [];
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

    const hostSelect = el("select", { name: "host" }, [
      el("option", { value: "", text: "-- 選択 --" }),
      ...hosts.map(function (h, idx) {
        return el("option", { value: h, text: h, selected: idx === 0 });
      })
    ]);

    const workdirInput = el("input", {
      type: "text",
      name: "workdir",
      placeholder: "/path/to/workdir"
    });

    const modelInput = el("input", {
      type: "text",
      name: "model",
      placeholder: "任意（例: claude-sonnet-4-5）"
    });

    const poolSelect = el("select", { name: "pool" }, [
      el("option", { value: "", text: "（指定なし / 自動解決）" }),
      ...pools.map(function (p) {
        return el("option", { value: p, text: p });
      })
    ]);

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
          model: modelInput.value.trim(),
          pool: poolSelect.value,
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
          hostSelect
        ]),
        el("label", { className: "field" }, [
          el("span", { className: "field-label", text: "Pool" }),
          poolSelect
        ]),
        el("label", { className: "field" }, [
          el("span", { className: "field-label", text: "Model" }),
          modelInput
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
          el("td", { className: "small muted", text: formatTime(h.created_at) }),
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
        el("td", { className: "small muted", text: formatTime(d.measured_at) }),
        el("td", { className: "small muted", text: formatTime(d.decided_at) })
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
  async function renderDeliveries(searchParams) {
    const validStatuses = [
      { key: "undelivered", label: "未納品" },
      { key: "delivered", label: "納品済み" },
      { key: "all", label: "すべて" }
    ];
    const reqStatus = (searchParams && searchParams.get("status")) || "undelivered";
    const activeStatus = validStatuses.some(function (s) { return s.key === reqStatus; }) ? reqStatus : "undelivered";

    setLoading("納品");
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

    dom.app.replaceChildren(
      el("div", { className: "page-header" }, [
        el("h1", { text: "納品" })
      ]),
      tabs,
      buildDeliveriesTable(jobs, function () {
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
        actionNodes.push(el("span", { className: "small muted", text: formatTime(j.delivered_at) }));
      }

      return el("tr", null, [
        el("td", { className: "mono" }, [navLink("/ui/jobs/" + j.id, "#" + j.id)]),
        el("td", null, [navLink("/ui/jobs/" + j.id, j.title || "(無題)")]),
        el("td", null, [statusBadge(j.status)]),
        el("td", null, [deliveredBadge]),
        el("td", { className: "mono small", text: (j.agent || "-") + " / " + (j.host || "-") }),
        el("td", { className: "preview-cell small", text: previewText }),
        el("td", { className: "small muted", text: formatTime(j.updated_at) }),
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

  // ==================== Global Event Wiring ====================
  async function tryConnect(tokenValue, noAuth) {
    dom.authError.hidden = true;
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

  dom.btnRefresh.addEventListener("click", function () {
    clearBanner();
    void refreshHeaderMeta();
    void renderCurrentRoute();
  });

  dom.btnToken.addEventListener("click", function () {
    showAuthGate("");
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
  if (hasCredentialsConfigured()) {
    void refreshHeaderMeta();
    void renderCurrentRoute();
  } else {
    showAuthGate("");
  }
})();

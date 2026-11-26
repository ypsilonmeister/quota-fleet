package ui

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestUIShellRoutesReturn200WithStrictCSP(t *testing.T) {
	h := NewHandler()

	routes := []string{
		"/ui/",
		"/ui/jobs",
		"/ui/jobs?view=chain",
		"/ui/jobs/new",
		"/ui/jobs/42",
		"/ui/gate/held",
		"/ui/gate/decisions",
		"/ui/deliveries",
		"/ui/agents",
	}

	for _, path := range routes {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s status = %d, want 200", path, rec.Code)
			}
			if got := rec.Header().Get("Content-Security-Policy"); got != ContentSecurityPolicy {
				t.Errorf("GET %s Content-Security-Policy = %q, want %q", path, got, ContentSecurityPolicy)
			}
			if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
				t.Errorf("GET %s Content-Type = %q, want text/html", path, ct)
			}
			body := rec.Body.String()
			if !strings.Contains(body, `src="/ui/assets/direction.js"`) {
				t.Errorf("GET %s body missing direction.js reference", path)
			}
			if !strings.Contains(body, `src="/ui/assets/notify.js"`) {
				t.Errorf("GET %s body missing notify.js reference", path)
			}
			if !strings.Contains(body, `src="/ui/assets/app.js"`) {
				t.Errorf("GET %s body missing app.js reference", path)
			}
			if !strings.Contains(body, `href="/ui/assets/style.css"`) {
				t.Errorf("GET %s body missing style.css reference", path)
			}
		})
	}
}

func TestUIRedirectAndAssetsAndNotFound(t *testing.T) {
	h := NewHandler()

	// /ui redirects to /ui/
	req := httptest.NewRequest(http.MethodGet, "/ui", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("GET /ui status = %d, want 302", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/ui/" {
		t.Errorf("GET /ui Location = %q, want /ui/", loc)
	}

	// Embedded CSS and JS assets
	for _, tc := range []struct {
		path   string
		wantCT string
	}{
		{path: "/ui/assets/style.css", wantCT: "text/css"},
		{path: "/ui/assets/direction.js", wantCT: "application/javascript"},
		{path: "/ui/assets/notify.js", wantCT: "application/javascript"},
		{path: "/ui/assets/app.js", wantCT: "application/javascript"},
	} {
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d, want 200", tc.path, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, tc.wantCT) {
			t.Errorf("GET %s Content-Type = %q, want prefix %q", tc.path, ct, tc.wantCT)
		}
		if got := rec.Header().Get("Content-Security-Policy"); got != ContentSecurityPolicy {
			t.Errorf("GET %s CSP = %q, want %q", tc.path, got, ContentSecurityPolicy)
		}
		if rec.Body.Len() == 0 {
			t.Errorf("GET %s returned empty body", tc.path)
		}
	}

	// Unknown route under /ui/ returns 404
	req = httptest.NewRequest(http.MethodGet, "/ui/does-not-exist", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /ui/does-not-exist status = %d, want 404", rec.Code)
	}
}

func TestUIShellAndAssetsFollowSecurityRules(t *testing.T) {
	html := string(indexHTML)
	if strings.Contains(html, "http://") || strings.Contains(html, "https://") {
		t.Errorf("index.html must not reference external resources")
	}
	if strings.Contains(html, "<style") || strings.Contains(html, "style=") {
		t.Errorf("index.html must not contain inline styles")
	}

	css := string(styleCSS)
	if strings.Contains(css, "http://") || strings.Contains(css, "https://") {
		t.Errorf("style.css must not reference external resources")
	}

	for _, asset := range []struct {
		name string
		src  string
	}{
		{name: "direction.js", src: string(directionJS)},
		{name: "i18n.js", src: string(i18nJS)},
		{name: "notify.js", src: string(notifyJS)},
		{name: "app.js", src: string(appJS)},
		{name: "sw.js", src: string(swJS)},
	} {
		for _, forbidden := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write"} {
			if strings.Contains(asset.src, forbidden) {
				t.Errorf("%s must not use %q (XSS prevention requires textContent)", asset.name, forbidden)
			}
		}
		if strings.Contains(asset.src, "http://") || strings.Contains(asset.src, "https://") {
			t.Errorf("%s must not reference external URLs", asset.name)
		}
	}
}

func TestUIChainViewAndMiniTreeAssetsPresent(t *testing.T) {
	js := string(appJS)
	for _, token := range []string{
		"buildJobForest",
		"summarizeChainProgress",
		"formatChainProgressSummary",
		"flattenChainTree",
		"buildChainJobsTable",
		"fetchFullJobChain",
		"buildMiniChainSection",
		`L("jobs.viewChain")`,
		`L("jobs.viewFlat")`,
		`L("chain.sectionTitle")`,
		`L("chain.currentTag")`,
	} {
		if !strings.Contains(js, token) {
			t.Errorf("app.js missing expected chain UI token %q", token)
		}
	}

	assertJapaneseMessages(t, map[string]string{
		"jobs.viewChain":     "チェーン表示",
		"jobs.viewFlat":      "フラット表示",
		"chain.sectionTitle": "チェーン構成",
		"chain.currentTag":   "現在表示中",
	})

	css := string(styleCSS)
	for _, cls := range []string{
		".view-toggle",
		".chain-root-row",
		".tree-prefix",
		".tree-branch",
		".chain-summary",
		".mini-tree",
		".mini-tree-node.is-current",
	} {
		if !strings.Contains(css, cls) {
			t.Errorf("style.css missing expected chain UI selector %q", cls)
		}
	}
}

func TestUIAgentVersionsAssetsPresent(t *testing.T) {
	html := string(indexHTML)
	if !strings.Contains(html, `href="/ui/agents"`) || !strings.Contains(html, `data-i18n="nav.agents"`) {
		t.Errorf("index.html missing navigation link for /ui/agents")
	}

	js := string(appJS)
	for _, token := range []string{
		"renderAgentVersions",
		"buildAgentSummaryCards",
		"buildAgentMatrixTable",
		"buildAgentEventsTable",
		"/agents/check",
		"/agents/events",
		`L("agents.title")`,
		`L("agents.checkNow")`,
		"pinned:",
		`L("agents.manualBadge")`,
	} {
		if !strings.Contains(js, token) {
			t.Errorf("app.js missing expected agent versions UI token %q", token)
		}
	}

	assertJapaneseMessages(t, map[string]string{
		"nav.agents":         "エージェント版数",
		"agents.title":       "エージェント版数",
		"agents.checkNow":    "今すぐ確認",
		"agents.manualBadge": "要手作業",
	})

	css := string(styleCSS)
	for _, cls := range []string{
		".agent-summary-grid",
		".agent-card-drift",
		".agent-cell-drift",
		".agent-cell-pinned",
		".badge-pinned",
		".badge-manual",
	} {
		if !strings.Contains(css, cls) {
			t.Errorf("style.css missing expected agent versions UI selector %q", cls)
		}
	}
}

func TestUIDirectionSwitcherAssetsPresent(t *testing.T) {
	html := string(indexHTML)
	for _, token := range []string{
		`<script src="/ui/assets/direction.js"></script>`,
		`id="direction-switch"`,
		`data-set-direction="console"`,
		`data-set-direction="ledger"`,
		`data-i18n-aria-label="theme.darkLabel" data-i18n="theme.dark"></button>`,
		`data-i18n-aria-label="theme.lightLabel" data-i18n="theme.light"></button>`,
	} {
		if !strings.Contains(html, token) {
			t.Errorf("index.html missing expected direction switcher token %q", token)
		}
	}
	for _, forbidden := range []string{
		`data-direction="console"`,
		`direction-btn active`,
		`aria-pressed="true"`,
	} {
		if strings.Contains(html, forbidden) {
			t.Errorf("index.html must not hardcode initial direction state %q (prevents FOUC)", forbidden)
		}
	}

	assertJapaneseMessages(t, map[string]string{
		"theme.darkLabel":  "ダーク（計器盤）",
		"theme.dark":       "ダーク",
		"theme.lightLabel": "ライト（台帳）",
		"theme.light":      "ライト",
	})

	css := string(styleCSS)
	for _, token := range []string{
		`data-direction="console"`,
		`data-direction="ledger"`,
		`.direction-switch`,
		`html[data-direction="ledger"] .direction-btn.active:focus-visible`,
		`Hiragino Mincho ProN`,
	} {
		if !strings.Contains(css, token) {
			t.Errorf("style.css missing expected direction styling token %q", token)
		}
	}

	dirJS := string(directionJS)
	for _, token := range []string{
		`qf-direction`,
		`data-direction`,
		`applyDirection`,
		`prefers-color-scheme`,
		`resolveDirection`,
	} {
		if !strings.Contains(dirJS, token) {
			t.Errorf("direction.js missing expected direction persistence token %q", token)
		}
	}

	js := string(appJS)
	for _, token := range []string{
		`applyDirection`,
		`resolveDirection`,
		`__qfDirection`,
	} {
		if !strings.Contains(js, token) {
			t.Errorf("app.js missing expected direction helper token %q", token)
		}
	}
}

func TestChainTreeLogicWithNode(t *testing.T) {
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available in PATH; skipping JS logic test")
	}

	// 1. Verify JS syntax is valid.
	for _, assetPath := range []string{"assets/direction.js", "assets/app.js"} {
		syntaxCmd := exec.Command(nodeBin, "--check", assetPath)
		if out, err := syntaxCmd.CombinedOutput(); err != nil {
			t.Fatalf("node --check %s failed: %v\n%s", assetPath, err, string(out))
		}
	}

	// 2. Run unit tests against exported pure chain helpers in assets/app.js.
	script := `
const assert = require("assert");
const chain = require("./assets/app.js");

// 0. Direction resolution: stored choice wins; otherwise prefers-color-scheme decides
{
  const mmDark = q => ({ matches: q === "(prefers-color-scheme: dark)" });
  const mmLight = q => ({ matches: q === "(prefers-color-scheme: light)" });
  const mmNone = () => ({ matches: false });

  assert.strictEqual(chain.resolveDirection(null, mmDark), "console");
  assert.strictEqual(chain.resolveDirection(null, mmLight), "ledger");
  assert.strictEqual(chain.resolveDirection(undefined, mmLight), "ledger");
  assert.strictEqual(chain.resolveDirection("invalid", mmLight), "ledger");
  assert.strictEqual(chain.resolveDirection(null, mmNone), "console");
  assert.strictEqual(chain.resolveDirection(null, null), "console");

  // Stored choice always overrides OS preference
  assert.strictEqual(chain.resolveDirection("console", mmLight), "console");
  assert.strictEqual(chain.resolveDirection("ledger", mmDark), "ledger");
}

// 1. Multi-level tree construction & sorting
{
  const jobs = [
    { id: 10, title: "Root A", status: "done", parent_id: null },
    { id: 12, title: "Child A2", status: "claimed", parent_id: 10 },
    { id: 11, title: "Child A1", status: "done", parent_id: 10 },
    { id: 13, title: "Grandchild A1-1", status: "waiting", parent_id: 11 },
    { id: 20, title: "Root B", status: "queued", parent_id: null }
  ];
  const forest = chain.buildJobForest(jobs);
  assert.strictEqual(forest.length, 2, "expected 2 root chains");
  // Sorted by max ID desc: Root B (20) first, then Root A (max 13)
  assert.strictEqual(forest[0].job.id, 20);
  assert.strictEqual(forest[1].job.id, 10);

  const rootA = forest[1];
  assert.strictEqual(rootA.children.length, 2);
  assert.strictEqual(rootA.children[0].job.id, 11);
  assert.strictEqual(rootA.children[1].job.id, 12);
  assert.strictEqual(rootA.children[0].children.length, 1);
  assert.strictEqual(rootA.children[0].children[0].job.id, 13);

  const sumA = chain.summarizeChainProgress(rootA);
  assert.strictEqual(sumA.total, 4);
  assert.strictEqual(sumA.counts.done, 2);
  assert.strictEqual(sumA.counts.claimed, 1);
  assert.strictEqual(sumA.counts.waiting, 1);
  assert.strictEqual(
    chain.formatChainProgressSummary(sumA),
    "全4件中 done 2件・実行中 1件・waiting 1件"
  );

  // Flatten check: depths, guides, isLast
  const flatA = chain.flattenChainTree(rootA);
  assert.deepStrictEqual(flatA.map(r => r.job.id), [10, 11, 13, 12]);
  assert.deepStrictEqual(flatA.map(r => r.depth), [0, 1, 2, 1]);
  assert.strictEqual(flatA[0].isRoot, true);
  assert.strictEqual(flatA[1].isLast, false);
  assert.deepStrictEqual(flatA[2].guides, [true]); // 11 has sibling 12, so depth-1 guide line continues
  assert.strictEqual(flatA[2].isLast, true);
  assert.strictEqual(flatA[3].isLast, true);
  assert.deepStrictEqual(flatA[3].guides, []);

  assert.strictEqual(chain.treeContainsStatus(rootA, "waiting"), true);
  assert.strictEqual(chain.treeContainsStatus(rootA, "failed"), false);
  assert.strictEqual(chain.treeContainsStatus(rootA, "all"), true);
}

// 2. Orphan job (parent not in fetched set) becomes root, preserving its descendants
{
  const jobs = [
    { id: 50, title: "Orphan Root", status: "claimed", parent_id: 999 },
    { id: 51, title: "Orphan Child", status: "waiting", parent_id: 50 }
  ];
  const forest = chain.buildJobForest(jobs);
  assert.strictEqual(forest.length, 1);
  assert.strictEqual(forest[0].job.id, 50);
  assert.strictEqual(forest[0].isOrphanRoot, true);
  assert.strictEqual(forest[0].children.length, 1);
  assert.strictEqual(forest[0].children[0].job.id, 51);
  assert.strictEqual(forest[0].children[0].isOrphanRoot, false);
}

// 3. Cycle safety: self-loop, 2-cycle, 3-cycle with branch, root-into-cycle
{
  // Self-loop
  const selfForest = chain.buildJobForest([
    { id: 1, title: "Self", status: "queued", parent_id: 1 }
  ]);
  assert.strictEqual(selfForest.length, 1);
  assert.strictEqual(selfForest[0].job.id, 1);
  assert.strictEqual(selfForest[0].children.length, 0);

  // 2-node cycle
  const twoCycle = chain.buildJobForest([
    { id: 2, title: "B", status: "waiting", parent_id: 1 },
    { id: 1, title: "A", status: "claimed", parent_id: 2 }
  ]);
  assert.strictEqual(twoCycle.length, 1);
  const flatTwo = chain.flattenChainTree(twoCycle[0]);
  assert.deepStrictEqual(flatTwo.map(r => r.job.id).sort((a, b) => a - b), [1, 2]);

  // 3-node cycle with branch hanging off node 2
  const threeCycle = chain.buildJobForest([
    { id: 1, title: "C1", status: "done", parent_id: 3 },
    { id: 2, title: "C2", status: "claimed", parent_id: 1 },
    { id: 3, title: "C3", status: "waiting", parent_id: 2 },
    { id: 4, title: "Leaf", status: "draft", parent_id: 2 }
  ]);
  const allIds = threeCycle.flatMap(r => chain.flattenChainTree(r).map(x => x.job.id)).sort((a, b) => a - b);
  assert.deepStrictEqual(allIds, [1, 2, 3, 4]);

  // Root leading into cycle (1 -> 2 -> 3 -> 2)
  const tailCycle = chain.buildJobForest([
    { id: 1, title: "Root", status: "done", parent_id: null },
    { id: 2, title: "LoopA", status: "claimed", parent_id: 1 },
    { id: 3, title: "LoopB", status: "waiting", parent_id: 2 }
  ]);
  assert.strictEqual(tailCycle.length, 1);
  assert.deepStrictEqual(chain.flattenChainTree(tailCycle[0]).map(r => r.job.id), [1, 2, 3]);
}

// 4. Progress summary exact example check: 全3件中 done 2件・実行中 1件
{
  const forest = chain.buildJobForest([
    { id: 1, title: "Step 1", status: "done", parent_id: null },
    { id: 2, title: "Step 2", status: "done", parent_id: 1 },
    { id: 3, title: "Step 3", status: "claimed", parent_id: 2 }
  ]);
  const summary = chain.summarizeChainProgress(forest[0]);
  assert.strictEqual(
    chain.formatChainProgressSummary(summary),
    "全3件中 done 2件・実行中 1件"
  );
}

// Helper: in-memory API mock implementing GET /jobs/{id} and paginated GET /jobs
function makeMockFetcher(allJobs, serverMaxLimit) {
  const maxLim = serverMaxLimit || 200;
  const byId = new Map(allJobs.map(j => [j.id, j]));
  return async function mockFetch(path) {
    const u = new URL("http://localhost" + path);
    const detailMatch = u.pathname.match(/^\/jobs\/(\d+)$/);
    if (detailMatch) {
      const id = Number(detailMatch[1]);
      const found = byId.get(id);
      if (!found) {
        return { ok: false, status: 404, data: { error: "not found" } };
      }
      return { ok: true, status: 200, data: found };
    }
    if (u.pathname === "/jobs") {
      const status = u.searchParams.get("status");
      const parentID = Number(u.searchParams.get("parent_id") || 0);
      const cursor = Number(u.searchParams.get("cursor") || 0);
      const reqLimit = Number(u.searchParams.get("limit") || 50);
      const limit = Math.min(reqLimit, maxLim);
      const matched = allJobs
        .filter(j => j.status === status && j.parent_id === parentID && j.id > cursor)
        .sort((a, b) => a.id - b.id);
      const hasMore = matched.length > limit;
      const pageJobs = hasMore ? matched.slice(0, limit) : matched;
      const nextCursor = hasMore ? pageJobs[pageJobs.length - 1].id : 0;
      return {
        ok: true,
        status: 200,
        data: { jobs: pageJobs, next_cursor: nextCursor, has_more: hasMore }
      };
    }
    return { ok: false, status: 404, data: { error: "unknown path" } };
  };
}

(async function runAsyncChainTests() {
  // 5. Reproduction case: root has 51 children (#2..#52); viewing #52 which has direct children (#53, #54) and grandchild (#55)
  {
    const jobs = [{ id: 1, title: "Root", status: "done", parent_id: null }];
    for (let i = 2; i <= 52; i += 1) {
      jobs.push({ id: i, title: "Sibling " + i, status: "done", parent_id: 1 });
    }
    jobs.push({ id: 53, title: "Direct Child 1", status: "claimed", parent_id: 52 });
    jobs.push({ id: 54, title: "Direct Child 2", status: "waiting", parent_id: 52 });
    jobs.push({ id: 55, title: "Grandchild 1", status: "draft", parent_id: 53 });

    const fetcher = makeMockFetcher(jobs, 200);
    const currentJob = jobs.find(j => j.id === 52);
    const res = await chain.fetchFullJobChain(currentJob, fetcher);

    const directChildren = res.jobs
      .filter(j => j.parent_id === 52)
      .map(j => j.id)
      .sort((a, b) => a - b);
    assert.deepStrictEqual(directChildren, [53, 54], "direct children of #52 must not be dropped");

    const ids = new Set(res.jobs.map(j => j.id));
    assert.strictEqual(ids.has(1), true, "root must be present");
    assert.strictEqual(ids.has(52), true, "current job must be present");
    assert.strictEqual(ids.has(55), true, "grandchild of current job should be prioritized and present");
    for (let i = 2; i <= 52; i += 1) {
      assert.strictEqual(ids.has(i), true, "sibling #" + i + " must be present");
    }

    // Mini-tree structure check
    const forest = chain.buildJobForest(res.jobs);
    assert.strictEqual(forest.length, 1);
    const node52 = forest[0].children.find(c => c.job.id === 52);
    assert.ok(node52, "#52 must be in mini-tree");
    assert.deepStrictEqual(node52.children.map(c => c.job.id), [53, 54]);
    assert.deepStrictEqual(node52.children[0].children.map(c => c.job.id), [55]);

    // Truncation metadata check (some other siblings beyond MAX_EXPAND_NODES=50 were not expanded)
    assert.strictEqual(res.truncated, true, "should report truncation when siblings exceed expand limit");
    assert.strictEqual(res.expandTruncated, true);
    assert.ok(res.unexpandedCount > 0);
    assert.strictEqual(res.currentJobChildrenTruncated, false);
    assert.strictEqual(res.hasFetchErrors, false);
    assert.strictEqual(res.currentJobChildrenFailed, false);
    assert.strictEqual(res.currentJobChildrenComplete, true);
    assert.strictEqual(chain.formatChainFetchErrorNotice(res), null);
    assert.strictEqual(chain.formatDirectChildrenFetchErrorNotice(res, 52), null);
    const truncMsg = chain.formatChainTruncationNotice(res);
    assert.ok(truncMsg && truncMsg.includes("表示中ジョブの直下の子ジョブは全件取得済みです。"), "complete direct children without errors should show full-fetch note");
  }

  // 6. Cursor pagination case: >200 children under both root and current job
  {
    const jobs = [{ id: 1, title: "Big Root", status: "done", parent_id: null }];
    for (let i = 2; i <= 251; i += 1) {
      jobs.push({ id: i, title: "Root Child " + i, status: "done", parent_id: 1 });
    }
    for (let i = 300; i < 505; i += 1) {
      jobs.push({ id: i, title: "Current Direct Child " + i, status: "queued", parent_id: 251 });
    }

    const fetcher = makeMockFetcher(jobs, 100); // enforce 100 per page to exercise multi-page cursor pagination
    const currentJob = jobs.find(j => j.id === 251);
    const res = await chain.fetchFullJobChain(currentJob, fetcher, { pageLimit: 100, maxExpandNodes: 10 });

    const directChildren = res.jobs.filter(j => j.parent_id === 251);
    assert.strictEqual(directChildren.length, 205, "all 205 paginated direct children must be fetched");
    const rootChildren = res.jobs.filter(j => j.parent_id === 1);
    assert.strictEqual(rootChildren.length, 250, "all 250 paginated siblings under root must be fetched");
    assert.strictEqual(res.pagesTruncated, false);
    assert.strictEqual(res.currentJobChildrenTruncated, false);
    assert.strictEqual(res.expandTruncated, true);
    assert.strictEqual(res.truncated, true);
  }

  // 7. Deep chain exceeding maxUpSteps still fetches current job's direct children and reports upTruncated
  {
    const jobs = [];
    for (let i = 1; i <= 15; i += 1) {
      jobs.push({ id: i, title: "Node " + i, status: "done", parent_id: i === 1 ? null : i - 1 });
    }
    jobs.push({ id: 16, title: "Leaf Child A", status: "waiting", parent_id: 15 });
    jobs.push({ id: 17, title: "Leaf Child B", status: "queued", parent_id: 15 });

    const fetcher = makeMockFetcher(jobs, 200);
    const res = await chain.fetchFullJobChain(jobs[14], fetcher, { maxUpSteps: 5, maxExpandNodes: 50 });
    assert.strictEqual(res.upTruncated, true);
    assert.strictEqual(res.truncated, true);
    assert.strictEqual(res.currentJobChildrenTruncated, false);
    const directChildren = res.jobs.filter(j => j.parent_id === 15).map(j => j.id).sort((a, b) => a - b);
    assert.deepStrictEqual(directChildren, [16, 17]);
  }

  // 8. Small chain reports truncated === false
  {
    const jobs = [
      { id: 1, title: "R", status: "done", parent_id: null },
      { id: 2, title: "C", status: "claimed", parent_id: 1 },
      { id: 3, title: "G", status: "waiting", parent_id: 2 }
    ];
    const fetcher = makeMockFetcher(jobs, 200);
    const res = await chain.fetchFullJobChain(jobs[1], fetcher);
    assert.strictEqual(res.truncated, false);
    assert.strictEqual(res.unexpandedCount, 0);
    assert.deepStrictEqual(res.jobs.map(j => j.id).sort((a, b) => a - b), [1, 2, 3]);
  }

  // 9. Non-advancing cursor guard prevents infinite loops and marks pagesTruncated
  {
    const badFetcher = async function () {
      return {
        ok: true,
        status: 200,
        data: { jobs: [{ id: 2, title: "Dup", status: "done", parent_id: 1 }], has_more: true, next_cursor: 0 }
      };
    };
    const res = await chain.fetchChildrenAcrossStatuses(1, badFetcher, { maxPagesPerStatus: 5 });
    assert.strictEqual(res.truncated, true);
    assert.strictEqual(res.hasErrors, false);
    assert.deepStrictEqual(res.jobs.map(j => j.id), [2]);
  }

  // 10. Regression (review-qf33 P2): HTTP 500 on one status of current job's children
  //     must surface fetch failure, keep fetched children, and NOT claim "全件取得済み"
  {
    const jobs = [{ id: 100, title: "Root", status: "done", parent_id: null }];
    for (let i = 101; i <= 151; i += 1) {
      jobs.push({ id: i, title: "Child " + i, status: "done", parent_id: 100 });
    }
    jobs.push({ id: 152, title: "Direct Waiting", status: "waiting", parent_id: 151 });
    jobs.push({ id: 153, title: "Direct Queued", status: "queued", parent_id: 151 });

    const baseFetcher = makeMockFetcher(jobs, 200);
    const flakyFetcher = async function (path) {
      const u = new URL("http://localhost" + path);
      if (u.pathname === "/jobs" && u.searchParams.get("status") === "waiting" && u.searchParams.get("parent_id") === "151") {
        return { ok: false, status: 500, data: { error: "internal server error" } };
      }
      return baseFetcher(path);
    };

    const currentJob = jobs.find(j => j.id === 151);
    const res = await chain.fetchFullJobChain(currentJob, flakyFetcher);

    // Partial display continues: #153 (queued) is still present even though #152 (waiting) failed
    const directChildren = res.jobs.filter(j => j.parent_id === 151).map(j => j.id);
    assert.deepStrictEqual(directChildren, [153], "successfully fetched children must still be returned");

    // Failure metadata is recorded accurately
    assert.strictEqual(res.hasFetchErrors, true);
    assert.strictEqual(res.currentJobChildrenFailed, true);
    assert.strictEqual(res.currentJobChildrenComplete, false);
    assert.strictEqual(res.fetchErrors.length, 1);
    assert.strictEqual(res.fetchErrors[0].parentId, 151);
    assert.strictEqual(res.fetchErrors[0].status, "waiting");
    assert.strictEqual(res.fetchErrors[0].httpStatus, 500);
    assert.strictEqual(res.currentJobChildrenErrors.length, 1);

    // Failure notices clearly identify target, status, and HTTP 500
    const chainErrNotice = chain.formatChainFetchErrorNotice(res);
    assert.ok(chainErrNotice && chainErrNotice.includes("#151") && chainErrNotice.includes("waiting") && chainErrNotice.includes("HTTP 500"),
      "chain error notice must describe failed parent, status, and HTTP code: " + chainErrNotice);
    const directErrNotice = chain.formatDirectChildrenFetchErrorNotice(res, 151);
    assert.ok(directErrNotice && directErrNotice.includes("#151") && directErrNotice.includes("waiting") && directErrNotice.includes("HTTP 500"),
      "direct children error notice must describe failed job, status, and HTTP code: " + directErrNotice);

    // Truncation notice must NOT claim "全件取得済み" when any fetch error occurred
    assert.strictEqual(res.truncated, true);
    const truncNotice = chain.formatChainTruncationNotice(res);
    assert.ok(truncNotice, "truncation notice should still be shown for expansion limit");
    assert.strictEqual(truncNotice.includes("全件取得済み"), false,
      "truncation notice must NOT claim 全件取得済み on partial fetch error: " + truncNotice);
  }

  // 11. Small chain (no truncation) with partial HTTP 500 still surfaces failure notices
  {
    const jobs = [
      { id: 1, title: "Root", status: "done", parent_id: null },
      { id: 2, title: "Current", status: "claimed", parent_id: 1 },
      { id: 3, title: "Failed Child", status: "waiting", parent_id: 2 },
      { id: 4, title: "OK Child", status: "queued", parent_id: 2 }
    ];
    const baseFetcher = makeMockFetcher(jobs, 200);
    const flakyFetcher = async function (path) {
      const u = new URL("http://localhost" + path);
      if (u.pathname === "/jobs" && u.searchParams.get("status") === "waiting" && u.searchParams.get("parent_id") === "2") {
        return { ok: false, status: 500, data: null };
      }
      return baseFetcher(path);
    };
    const res = await chain.fetchFullJobChain(jobs[1], flakyFetcher);
    assert.strictEqual(res.truncated, false);
    assert.strictEqual(res.hasFetchErrors, true);
    assert.strictEqual(res.currentJobChildrenFailed, true);
    assert.strictEqual(chain.formatChainTruncationNotice(res), null);
    const chainErrNotice = chain.formatChainFetchErrorNotice(res);
    assert.ok(chainErrNotice && chainErrNotice.includes("#2") && chainErrNotice.includes("waiting") && chainErrNotice.includes("HTTP 500"));
    const directErrNotice = chain.formatDirectChildrenFetchErrorNotice(res, 2);
    assert.ok(directErrNotice && directErrNotice.includes("#2") && directErrNotice.includes("waiting") && directErrNotice.includes("HTTP 500"));
    assert.deepStrictEqual(res.jobs.filter(j => j.parent_id === 2).map(j => j.id), [4]);
  }

  // 12. Error on a non-current node in a truncated chain also suppresses "全件取得済み"
  {
    const jobs = [{ id: 100, title: "Root", status: "done", parent_id: null }];
    for (let i = 101; i <= 151; i += 1) {
      jobs.push({ id: i, title: "Child " + i, status: "done", parent_id: 100 });
    }
    jobs.push({ id: 152, title: "Direct Child", status: "queued", parent_id: 151 });
    const baseFetcher = makeMockFetcher(jobs, 200);
    const flakyFetcher = async function (path) {
      const u = new URL("http://localhost" + path);
      if (u.pathname === "/jobs" && u.searchParams.get("status") === "failed" && u.searchParams.get("parent_id") === "100") {
        return { ok: false, status: 503, data: { error: "unavailable" } };
      }
      return baseFetcher(path);
    };
    const res = await chain.fetchFullJobChain(jobs.find(j => j.id === 151), flakyFetcher);
    assert.strictEqual(res.hasFetchErrors, true);
    assert.strictEqual(res.currentJobChildrenFailed, false);
    assert.strictEqual(chain.formatDirectChildrenFetchErrorNotice(res, 151), null);
    const chainErrNotice = chain.formatChainFetchErrorNotice(res);
    assert.ok(chainErrNotice && chainErrNotice.includes("#100") && chainErrNotice.includes("failed") && chainErrNotice.includes("HTTP 503"));
    const truncNotice = chain.formatChainTruncationNotice(res);
    assert.ok(truncNotice && !truncNotice.includes("全件取得済み"), "any chain fetch error must suppress 全件取得済み");
  }

  // 13. Mid-pagination failure keeps page 1 jobs and records error; 401 still rethrows
  {
    const jobs = [{ id: 1, title: "Parent", status: "done", parent_id: null }];
    for (let i = 2; i <= 15; i += 1) {
      jobs.push({ id: i, title: "C" + i, status: "queued", parent_id: 1 });
    }
    const baseFetcher = makeMockFetcher(jobs, 5);
    const midFailFetcher = async function (path) {
      const u = new URL("http://localhost" + path);
      if (u.pathname === "/jobs" && u.searchParams.get("status") === "queued" && Number(u.searchParams.get("cursor") || 0) > 0) {
        return { ok: false, status: 500, data: { error: "db timeout" } };
      }
      return baseFetcher(path);
    };
    const res = await chain.fetchChildrenAcrossStatuses(1, midFailFetcher, { pageLimit: 5 });
    assert.strictEqual(res.hasErrors, true);
    assert.strictEqual(res.errors.length, 1);
    assert.strictEqual(res.errors[0].status, "queued");
    assert.strictEqual(res.errors[0].httpStatus, 500);
    assert.deepStrictEqual(res.jobs.map(j => j.id), [2, 3, 4, 5, 6], "page 1 jobs must be preserved on page 2 failure");

    let caught401 = false;
    try {
      await chain.fetchChildrenAcrossStatuses(1, async function () {
        const e = new Error("unauthorized");
        e.status = 401;
        throw e;
      });
    } catch (err) {
      caught401 = Boolean(err && err.status === 401);
    }
    assert.strictEqual(caught401, true, "401 error must propagate to trigger auth gate");
  }

  console.log("ALL_CHAIN_JS_TESTS_PASSED");
})().catch(err => {
  console.error(err);
  process.exit(1);
});
`
	cmd := exec.Command(nodeBin, "-e", script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node chain unit tests failed: %v\n%s", err, string(out))
	}
	if !strings.Contains(string(out), "ALL_CHAIN_JS_TESTS_PASSED") {
		t.Fatalf("unexpected node test output: %s", string(out))
	}
}

func TestUIModelPickerAndPoolAutoAssetsPresent(t *testing.T) {
	js := string(appJS)
	for _, token := range []string{
		"getHostsForAgent",
		"formatNoMatchingHostNotice",
		"getModelsForHostAgent",
		"formatModelOptionLabel",
		"deriveModelFamily",
		"derivePoolForSelection",
		"formatDerivedPoolLabel",
		"derived-pool-display",
		"host-empty-notice",
		"model_select",
		`L("newJob.poolLabel")`,
	} {
		if !strings.Contains(js, token) {
			t.Errorf("app.js missing expected model-picker / pool-auto UI token %q", token)
		}
	}
	if strings.Contains(js, `el("select", { name: "pool" }`) {
		t.Errorf("app.js must not render a manual pool <select> dropdown in the job form")
	}

	assertJapaneseMessages(t, map[string]string{"newJob.poolLabel": "Pool（自動判定）"})

	css := string(styleCSS)
	for _, cls := range []string{
		".model-control",
		".derived-pool-display",
	} {
		if !strings.Contains(css, cls) {
			t.Errorf("style.css missing expected model/pool form selector %q", cls)
		}
	}
}

func TestUIModelPickerAndPoolDerivationWithNode(t *testing.T) {
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available in PATH; skipping JS model/pool form test")
	}

	script := `
const assert = require("assert");
const ui = require("./assets/app.js");

const meta = {
  hosts: ["home-win", "ubuntu"],
  agents: ["agy", "claude", "codex"],
  host_agents: {
    "ubuntu": ["agy", "claude", "codex"],
    "home-win": ["claude", "codex"]
  },
  models: {
    "ubuntu": {
      "agy": [
        { id: "gemini-4-carbon-medium", display_name: "Gemini 4 Carbon (Medium)", family: "gemini" },
        { id: "claude-opus-5-5-high", display_name: "Claude Opus 5.5 (High)", family: "claude" },
        { id: "gpt-oss-120b-medium", display_name: "GPT-OSS 120B (Medium)", family: "gpt" }
      ],
      "claude": [
        { id: "claude-sonnet-4-6", display_name: "Claude Sonnet 4.6", family: "claude" }
      ]
    },
    "home-win": {
      "codex": [
        { id: "gpt-6.2-sol", display_name: "GPT-6.2-Sol", family: "gpt" }
      ]
    }
  },
  pool_rules: {
    "claude": [{ agent: "claude", model_family: "" }],
    "codex": [{ agent: "codex", model_family: "" }],
    "agy-gemini": [{ agent: "agy", model_family: "gemini" }],
    "agy-3p": [{ agent: "agy", model_family: "claude-gpt" }]
  }
};

// 1. Host filtering by agent
assert.deepStrictEqual(ui.getHostsForAgent(meta, "agy"), ["ubuntu"]);
assert.deepStrictEqual(ui.getHostsForAgent(meta, "claude"), ["home-win", "ubuntu"]);
assert.deepStrictEqual(ui.getHostsForAgent(meta, "codex"), ["home-win", "ubuntu"]);
assert.deepStrictEqual(ui.getHostsForAgent(meta, ""), ["home-win", "ubuntu"]);

// 1b. Zero matching hosts when host_agents is present must stay empty (not fall back to all hosts)
const agyOnlyMeta = {
  hosts: ["ubuntu"],
  agents: ["agy", "claude"],
  host_agents: { "ubuntu": ["agy"] }
};
assert.deepStrictEqual(ui.getHostsForAgent(agyOnlyMeta, "claude"), []);
assert.strictEqual(ui.formatNoMatchingHostNotice("claude", []), "選択中の agent (claude) に対応するホストがありません。");
assert.strictEqual(ui.formatNoMatchingHostNotice("agy", ["ubuntu"]), "");
assert.strictEqual(ui.formatNoMatchingHostNotice("", []), "");

// 2. Model catalog lookup per host x agent (and empty fallback when missing)
assert.strictEqual(ui.getModelsForHostAgent(meta, "ubuntu", "agy").length, 3);
assert.strictEqual(ui.getModelsForHostAgent(meta, "home-win", "codex").length, 1);
assert.deepStrictEqual(ui.getModelsForHostAgent(meta, "ubuntu", "codex"), []);
assert.deepStrictEqual(ui.getModelsForHostAgent(meta, "home-win", "claude"), []);

// 3. Model option label formatting
assert.strictEqual(
  ui.formatModelOptionLabel({ id: "gemini-4-carbon-medium", display_name: "Gemini 4 Carbon (Medium)" }),
  "Gemini 4 Carbon (Medium) (gemini-4-carbon-medium)"
);
assert.strictEqual(
  ui.formatModelOptionLabel({ id: "claude-sonnet-4-6", display_name: "claude-sonnet-4-6" }),
  "claude-sonnet-4-6"
);

// 4. Model family classification
assert.strictEqual(ui.deriveModelFamily("gemini-4-neon-medium"), "gemini");
assert.strictEqual(ui.deriveModelFamily("claude-opus-5-5-high"), "claude");
assert.strictEqual(ui.deriveModelFamily("gpt-oss-120b-medium"), "gpt");
assert.strictEqual(ui.deriveModelFamily("custom-model"), "unknown");

// 5. Pool auto-derivation (including agy gemini vs claude/gpt branching via canonical family)
assert.strictEqual(ui.derivePoolForSelection(meta, "claude", "claude-sonnet-4-6"), "claude");
assert.strictEqual(ui.derivePoolForSelection(meta, "claude", ""), "claude");
assert.strictEqual(ui.derivePoolForSelection(meta, "codex", "gpt-6.2-sol"), "codex");
assert.strictEqual(ui.derivePoolForSelection(meta, "agy", "gemini-4-carbon-medium"), "agy-gemini");
assert.strictEqual(ui.derivePoolForSelection(meta, "agy", "gemini-gpt-preview"), "agy-gemini");
assert.strictEqual(ui.derivePoolForSelection(meta, "agy", "claude-opus-5-5-high"), "agy-3p");
assert.strictEqual(ui.derivePoolForSelection(meta, "agy", "gpt-oss-120b-medium"), "agy-3p");
assert.strictEqual(ui.derivePoolForSelection(meta, "agy", "custom-gpt-model"), "");
assert.strictEqual(ui.derivePoolForSelection(meta, "agy", "custom-model"), "");
assert.strictEqual(ui.derivePoolForSelection(meta, "agy", ""), "");

// 6. Pool display label formatting
assert.strictEqual(ui.formatDerivedPoolLabel("agy-3p"), "pool: agy-3p（自動）");
assert.strictEqual(ui.formatDerivedPoolLabel(""), "pool: なし（自動判定対象外）");

console.log("ALL_UI_MODELS_TESTS_PASSED");
`
	cmd := exec.Command(nodeBin, "-e", script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node model/pool unit tests failed: %v\n%s", err, string(out))
	}
	if !strings.Contains(string(out), "ALL_UI_MODELS_TESTS_PASSED") {
		t.Fatalf("unexpected node test output: %s", string(out))
	}
}

func TestUIPoolResetTimesAssetsAndLogicWithNode(t *testing.T) {
	js := string(appJS)
	for _, token := range []string{
		"formatWeeklyResetTime",
		"formatShortResetTime",
		"formatPoolResetLabels",
		"pool-resets",
		"pool-reset-label",
		"pool-reset-item",
		`L("dashboard.resetLabel")`,
		`L("dashboard.resetNone")`,
	} {
		if !strings.Contains(js, token) {
			t.Errorf("app.js missing expected pool reset time token %q", token)
		}
	}

	assertJapaneseMessages(t, map[string]string{
		"dashboard.resetLabel":              "リセット:",
		"dashboard.resetNone":               "なし",
		"dashboard.weeklyReset":             "週次 {time}",
		"dashboard.weeklyResetMissing":      "週次 なし",
		"dashboard.shortReset":              "{window} {time}",
		"dashboard.shortResetMissing":       "{window} なし",
		"dashboard.shortResetNotApplicable": "{window} 対象外",
	})

	css := string(styleCSS)
	for _, cls := range []string{
		".pool-resets",
		".pool-reset-label",
		".pool-reset-item",
		".pool-reset-item.is-missing",
	} {
		if !strings.Contains(css, cls) {
			t.Errorf("style.css missing expected pool reset selector %q", cls)
		}
	}

	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available in PATH; skipping JS pool reset time test")
	}

	scriptJST := `
const assert = require("assert");
const i18n = require("./assets/i18n.js");
const ui = require("./assets/app.js");

// 1. Direct formatters in JST
assert.strictEqual(ui.formatWeeklyResetTime("2026-11-26T20:00:00+09:00"), "11/26 20:00");
assert.strictEqual(ui.formatWeeklyResetTime("2026-12-03T00:05:00+09:00"), "12/3 00:05");
assert.strictEqual(ui.formatWeeklyResetTime("2026-12-03T11:00:00Z"), "12/3 20:00");
assert.strictEqual(ui.formatWeeklyResetTime(""), "");
assert.strictEqual(ui.formatWeeklyResetTime(null), "");
assert.strictEqual(ui.formatWeeklyResetTime("invalid-date"), "");

assert.strictEqual(ui.formatShortResetTime("2026-11-23T14:30:00+09:00"), "14:30");
assert.strictEqual(ui.formatShortResetTime("2026-11-23T05:07:00+09:00"), "05:07");
assert.strictEqual(ui.formatShortResetTime("2026-11-26T22:54:25Z"), "07:54");
assert.strictEqual(ui.formatShortResetTime(""), "");
assert.strictEqual(ui.formatShortResetTime(undefined), "");
assert.strictEqual(ui.formatShortResetTime("not-a-timestamp"), "");

// 2. Pool reset labels in JA (both present, alt short missing, both missing, short_applicable=false, approx + custom window)
const both = ui.formatPoolResetLabels({
  weekly_reset_at: "2026-11-26T20:00:00+09:00",
  short_reset_at: "2026-11-23T14:30:00+09:00"
});
assert.strictEqual(both.weeklyText, "週次 11/26 20:00");
assert.strictEqual(both.weeklyMissing, false);
assert.strictEqual(both.weeklyRaw, "2026-11-26T20:00:00+09:00");
assert.strictEqual(both.shortText, "5h 14:30");
assert.strictEqual(both.shortMissing, false);
assert.strictEqual(both.shortRaw, "2026-11-23T14:30:00+09:00");

const altNoShort = ui.formatPoolResetLabels({
  weekly_reset_at: "2026-11-26T01:32:24+09:00",
  short_reset_at: null
});
assert.strictEqual(altNoShort.weeklyText, "週次 11/26 01:32");
assert.strictEqual(altNoShort.weeklyMissing, false);
assert.strictEqual(altNoShort.shortText, "5h なし");
assert.strictEqual(altNoShort.shortMissing, true);
assert.strictEqual(altNoShort.shortRaw, "");

const none = ui.formatPoolResetLabels({});
assert.strictEqual(none.weeklyText, "週次 なし");
assert.strictEqual(none.weeklyMissing, true);
assert.strictEqual(none.shortText, "5h なし");
assert.strictEqual(none.shortMissing, true);

const weeklyOnly = ui.formatPoolResetLabels({
  weekly_reset_at: "2026-12-03T00:00:00Z",
  weekly_reset_at_approx: true,
  short_applicable: false
});
assert.strictEqual(weeklyOnly.weeklyText, "週次 ~12/3 09:00");
assert.strictEqual(weeklyOnly.weeklyMissing, false);
assert.strictEqual(weeklyOnly.shortText, "5h 対象外");
assert.strictEqual(weeklyOnly.shortMissing, true);

const customWin = ui.formatPoolResetLabels({
  weekly_reset_at: "2026-12-03T09:29:03Z",
  short_reset_at: "2026-11-26T23:29:03Z",
  short_reset_at_approx: true,
  short_window: "1h"
});
assert.strictEqual(customWin.weeklyText, "週次 12/3 18:29");
assert.strictEqual(customWin.shortText, "1h ~08:29");

// 3. EN locale output
i18n.setLocale("en", false);
assert.strictEqual(ui.formatPoolResetLabels({
  weekly_reset_at: "2026-11-26T20:00:00+09:00",
  short_reset_at: "2026-11-23T14:30:00+09:00"
}).weeklyText, "Weekly 11/26 20:00");
assert.strictEqual(ui.formatPoolResetLabels({}).weeklyText, "Weekly none");
assert.strictEqual(ui.formatPoolResetLabels({}).shortText, "5h none");
assert.strictEqual(ui.formatPoolResetLabels({ short_applicable: false }).shortText, "5h N/A");

console.log("ALL_UI_POOL_RESET_JST_TESTS_PASSED");
`
	cmdJST := exec.Command(nodeBin, "-e", scriptJST)
	cmdJST.Env = append(os.Environ(), "TZ=Asia/Tokyo")
	outJST, err := cmdJST.CombinedOutput()
	if err != nil || !strings.Contains(string(outJST), "ALL_UI_POOL_RESET_JST_TESTS_PASSED") {
		t.Fatalf("node pool reset JST tests failed: %v\n%s", err, string(outJST))
	}

	scriptUTC := `
const assert = require("assert");
const ui = require("./assets/app.js");
assert.strictEqual(ui.formatWeeklyResetTime("2026-12-03T11:00:00Z"), "12/3 11:00");
assert.strictEqual(ui.formatShortResetTime("2026-11-26T22:54:25Z"), "22:54");
console.log("ALL_UI_POOL_RESET_UTC_TESTS_PASSED");
`
	cmdUTC := exec.Command(nodeBin, "-e", scriptUTC)
	cmdUTC.Env = append(os.Environ(), "TZ=UTC")
	outUTC, err := cmdUTC.CombinedOutput()
	if err != nil || !strings.Contains(string(outUTC), "ALL_UI_POOL_RESET_UTC_TESTS_PASSED") {
		t.Fatalf("node pool reset UTC tests failed: %v\n%s", err, string(outUTC))
	}
}

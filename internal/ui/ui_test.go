package ui

import (
	"net/http"
	"net/http/httptest"
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

	js := string(appJS)
	for _, forbidden := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write"} {
		if strings.Contains(js, forbidden) {
			t.Errorf("app.js must not use %q (XSS prevention requires textContent)", forbidden)
		}
	}
	if strings.Contains(js, "http://") || strings.Contains(js, "https://") {
		t.Errorf("app.js must not reference external URLs")
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
		"チェーン表示",
		"フラット表示",
		"チェーン構成",
		"現在表示中",
	} {
		if !strings.Contains(js, token) {
			t.Errorf("app.js missing expected chain UI token %q", token)
		}
	}

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
	if !strings.Contains(html, `href="/ui/agents"`) || !strings.Contains(html, "エージェント版数") {
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
		"エージェント版数",
		"今すぐ確認",
		"pinned:",
		"要手作業",
	} {
		if !strings.Contains(js, token) {
			t.Errorf("app.js missing expected agent versions UI token %q", token)
		}
	}

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

func TestChainTreeLogicWithNode(t *testing.T) {
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available in PATH; skipping JS logic test")
	}

	// 1. Verify JS syntax is valid.
	syntaxCmd := exec.Command(nodeBin, "--check", "assets/app.js")
	if out, err := syntaxCmd.CombinedOutput(); err != nil {
		t.Fatalf("node --check assets/app.js failed: %v\n%s", err, string(out))
	}

	// 2. Run unit tests against exported pure chain helpers in assets/app.js.
	script := `
const assert = require("assert");
const chain = require("./assets/app.js");

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

console.log("ALL_CHAIN_JS_TESTS_PASSED");
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

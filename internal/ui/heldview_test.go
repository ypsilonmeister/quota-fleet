package ui

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// docs/20: held entries that share a title are told apart by age, parent and key, both on
// the held page and on the dashboard.
func TestUIHeldListsShowAgeParentAndSupersedeKeyWithNode(t *testing.T) {
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available in PATH; skipping JS held view test")
	}
	assets, err := filepath.Abs("assets")
	if err != nil {
		t.Fatal(err)
	}

	script := `
const assert = require("assert");
const path = require("path");
const dir = process.argv[1];
Object.defineProperty(globalThis, "navigator", { value: { language: "ja-JP" }, configurable: true });

class El {
  constructor(tag) {
    this.tagName = tag.toUpperCase();
    this.children = [];
    this.attrs = {};
    this.dataset = {};
    this.className = "";
    this.classList = { toggle() {}, add() {}, remove() {} };
  }
  appendChild(c) { this.children.push(c); return c; }
  replaceChildren(...c) { this.children = c; }
  setAttribute(k, v) { this.attrs[k] = String(v); }
  getAttribute(k) { return k in this.attrs ? this.attrs[k] : null; }
  addEventListener() {}
  contains() { return false; }
  querySelectorAll() { return []; }
  focus() {}
  set textContent(v) { this.children = [{ textContent: String(v) }]; }
  get textContent() { return this.children.map((c) => c.textContent).join(""); }
}

function storage(init) {
  const m = new Map(Object.entries(init || {}));
  return {
    getItem: (k) => (m.has(k) ? m.get(k) : null),
    setItem: (k, v) => { m.set(k, String(v)); },
    removeItem: (k) => { m.delete(k); }
  };
}

const base = { status: "held", pool: "claude", payload: { title: "publish" }, created_at: "2026-11-25T00:00:00Z" };
const held = [
  Object.assign({}, base, { id: "held-a", age_seconds: 42, parent_id: 7, parent_status: "done", supersede_key: "pub-42" }),
  Object.assign({}, base, { id: "held-b", age_seconds: 3 * 3600 + 5 * 60, parent_id: 9, parent_status: "missing" }),
  Object.assign({}, base, { id: "held-c", age_seconds: 2 * 86400 + 3600 }),
  Object.assign({}, base, { id: "held-d", age_seconds: 600, status: "cancelled", cancel_reason: "superseded: by x" })
];

function all(node, out) {
  if (node instanceof El) {
    out.push(node);
    for (const c of node.children) all(c, out);
  }
  return out;
}

async function open(href) {
  const ids = {};
  const url = new URL(href, "http://ui.test");
  globalThis.localStorage = storage();
  globalThis.sessionStorage = storage({ quota_fleet_auth_mode: "none" });
  globalThis.history = { state: null, pushState() {}, replaceState() {} };
  globalThis.window = { location: { href: url.href }, addEventListener() {}, focus() {} };
  globalThis.document = {
    documentElement: new El("html"),
    getElementById(id) {
      if (id === "direction-switch") return null;
      return (ids[id] = ids[id] || new El("div"));
    },
    createElement: (tag) => new El(tag),
    createTextNode: (text) => ({ textContent: text }),
    querySelector: () => null,
    querySelectorAll: () => [],
    addEventListener() {}
  };
  globalThis.fetch = async (u) => {
    const body = u.startsWith("/gate/held") ? { held: held }
      : u.startsWith("/jobs") || u.startsWith("/deliveries") ? { jobs: [] } : { counts: {}, total: 0 };
    return { ok: true, status: 200, text: async () => JSON.stringify(body) };
  };
  for (const name of ["direction.js", "i18n.js", "notify.js", "app.js"]) {
    delete require.cache[path.join(dir, name)];
  }
  globalThis.__qfDirection = require(path.join(dir, "direction.js"));
  require(path.join(dir, "i18n.js"));
  require(path.join(dir, "notify.js"));
  require(path.join(dir, "app.js"));
  for (let i = 0; i < 10; i += 1) await new Promise((r) => setImmediate(r));
  return all(ids.app, []);
}

(async () => {
  for (const href of ["/ui/gate/held", "/ui/"]) {
    const nodes = await open(href);
    const tables = nodes.filter((n) => n.tagName === "TABLE" && n.textContent.includes("held-a"));
    assert.strictEqual(tables.length, 1, href + ": held table");
    const rows = all(tables[0], []).filter((n) => n.tagName === "TR");
    const head = rows[0].children.map((c) => c.textContent);
    assert.strictEqual(rows.length, 5, href);
    for (const row of rows) assert.strictEqual(row.children.length, head.length, href + ": column count");
    const parentColumn = head.indexOf("親ジョブ");
    assert.ok(parentColumn >= 0, href + ": parent column");
    const text = rows.slice(1).map((r) => r.textContent);
    for (const want of ["supersede_key: pub-42", "42秒経過"]) assert.ok(text[0].includes(want), href + ": " + want + " in " + text[0]);
    assert.strictEqual(rows[1].children[parentColumn].textContent, "#7 done", href);
    assert.strictEqual(rows[2].children[parentColumn].textContent, "#9 missing", href);
    assert.strictEqual(rows[3].children[parentColumn].textContent, "-", href);
    assert.ok(text[1].includes("3時間5分経過"), href + ": " + text[1]);
    assert.ok(text[2].includes("2日1時間経過"), href + ": " + text[2]);
    assert.ok(text[3].includes("10分経過"), href + ": " + text[3]);
    assert.ok(!text[1].includes("supersede_key"), href);
    assert.strictEqual(text[3].includes("superseded: by x"), href === "/ui/gate/held", href);
  }
  console.log("OK");
  process.exit(0);
})().catch((err) => { console.error(err); process.exit(1); });
`
	out, err := exec.Command(nodeBin, "-e", script, assets).CombinedOutput()
	if err != nil {
		t.Fatalf("node held view tests failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "OK") {
		t.Fatalf("unexpected node test output: %s", out)
	}
}

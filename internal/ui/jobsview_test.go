package ui

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The script boots app.js against a minimal DOM and drives the jobs list the way a user
// does: opening URLs, clicking links and buttons, and going back.
func TestUIJobsListViewDefaultAndPersistenceWithNode(t *testing.T) {
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available in PATH; skipping JS jobs view test")
	}
	assets, err := filepath.Abs("assets")
	if err != nil {
		t.Fatal(err)
	}

	script := `
const assert = require("assert");
const path = require("path");
const dir = process.argv[1];
const ORIGIN = "http://ui.test";
const KEY = "qf-jobs-view";
const CHAIN_STATUSES = ["draft", "queued", "claimed", "waiting", "done", "failed", "cancelled"];
Object.defineProperty(globalThis, "navigator", { value: { language: "ja-JP" }, configurable: true });

class El {
  constructor(tag) {
    this.tagName = tag.toUpperCase();
    this.children = [];
    this.attrs = {};
    this.listeners = {};
    this.dataset = {};
    this.className = "";
    this.classList = { toggle() {}, add() {}, remove() {} };
  }
  appendChild(c) { this.children.push(c); return c; }
  replaceChildren(...c) { this.children = c; }
  setAttribute(k, v) { this.attrs[k] = String(v); }
  getAttribute(k) { return k in this.attrs ? this.attrs[k] : null; }
  addEventListener(type, fn) { (this.listeners[type] = this.listeners[type] || []).push(fn); }
  fire(type, ev) { for (const fn of this.listeners[type] || []) fn(ev || { preventDefault() {} }); }
  contains() { return false; }
  querySelectorAll() { return []; }
  focus() {}
  closest() { return this.tagName === "A" && this.dataset.nav === "true" ? this : null; }
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

const brokenStorage = {
  getItem() { throw new Error("storage disabled"); },
  setItem() { throw new Error("storage disabled"); },
  removeItem() { throw new Error("storage disabled"); }
};

async function settle() {
  for (let i = 0; i < 10; i += 1) await new Promise((r) => setImmediate(r));
}

// open loads a fresh page at href, like a new visit that shares localStorage.
async function open(href, local) {
  const ids = {};
  const docEvents = new El("document");
  const winEvents = new El("window");
  const requests = [];
  const entries = [{ url: new URL(href, ORIGIN), state: null }];
  let index = 0;
  globalThis.localStorage = local;
  globalThis.sessionStorage = storage({ quota_fleet_auth_mode: "none" });
  globalThis.history = {
    get state() { return entries[index].state; },
    pushState(state, _title, url) {
      entries.splice(index + 1);
      entries.push({ url: new URL(url, entries[index].url), state: state });
      index += 1;
    },
    replaceState(state, _title, url) {
      entries[index] = { url: url ? new URL(url, entries[index].url) : entries[index].url, state: state };
    }
  };
  globalThis.window = {
    location: { get href() { return entries[index].url.href; } },
    addEventListener: (type, fn) => winEvents.addEventListener(type, fn),
    focus() {}
  };
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
    addEventListener: (type, fn) => docEvents.addEventListener(type, fn)
  };
  globalThis.fetch = async (url) => {
    requests.push(url);
    const body = url.startsWith("/jobs") ? { jobs: [] } : { counts: {}, total: 0 };
    return { ok: true, status: 200, text: async () => JSON.stringify(body) };
  };
  for (const name of ["direction.js", "i18n.js", "notify.js", "app.js"]) {
    delete require.cache[path.join(dir, name)];
  }
  globalThis.__qfDirection = require(path.join(dir, "direction.js"));
  require(path.join(dir, "i18n.js"));
  require(path.join(dir, "notify.js"));
  require(path.join(dir, "app.js"));
  await settle();

  function all(node, out) {
    if (node instanceof El) {
      out.push(node);
      for (const c of node.children) all(c, out);
    }
    return out;
  }
  const nodes = () => all(ids.app, []);
  function button(label) {
    const found = nodes().find((n) => n.tagName === "BUTTON" && n.textContent.startsWith(label));
    assert.ok(found, "button " + label);
    return found;
  }
  return {
    requests: requests,
    url: () => entries[index].url.pathname + entries[index].url.search,
    async click(label) {
      requests.length = 0;
      button(label).fire("click");
      await settle();
    },
    async link(target) {
      requests.length = 0;
      const a = new El("a");
      a.dataset.nav = "true";
      a.setAttribute("href", target);
      docEvents.fire("click", { button: 0, target: a, preventDefault() {} });
      await settle();
    },
    async back() {
      assert.ok(index > 0, "nothing to go back to");
      requests.length = 0;
      index -= 1;
      winEvents.fire("popstate");
      await settle();
    },
    async refresh() {
      requests.length = 0;
      ids["btn-refresh"].fire("click");
      await settle();
    },
    // shown reports the rendered view and checks that the requests match it.
    shown() {
      const pressed = nodes().filter((n) => n.attrs["aria-pressed"] === "true").map((n) => n.textContent);
      assert.strictEqual(pressed.length, 1, "one active view: " + pressed);
      const view = { "チェーン表示": "chain", "フラット表示": "flat" }[pressed[0]];
      assert.ok(view, "unknown active view label: " + pressed[0]);
      const tab = nodes().find((n) => n.className === "tab active");
      const status = tab.children[0].textContent;
      const lists = requests.filter((u) => u.startsWith("/jobs?"));
      const expected = view === "chain"
        ? CHAIN_STATUSES.map((st) => "/jobs?status=" + st + "&desc=true&limit=100")
        : ["/jobs?status=" + status + "&desc=true&limit=50"];
      assert.deepStrictEqual(lists.sort(), expected.sort(), view + " requests");
      return view + " " + status;
    }
  };
}

(async () => {
  // Default: chain with every status, and nothing is stored until the user picks.
  let local = storage();
  let page = await open("/ui/jobs", local);
  assert.strictEqual(page.shown(), "chain すべて");
  assert.strictEqual(page.url(), "/ui/jobs");
  assert.strictEqual(local.getItem(KEY), null);

  // Picking flat is stored and survives other screens and a new visit.
  await page.click("フラット表示");
  assert.strictEqual(page.shown(), "flat queued");
  assert.strictEqual(page.url(), "/ui/jobs?view=flat&status=queued");
  assert.strictEqual(local.getItem(KEY), "flat");
  await page.link("/ui/");
  await page.link("/ui/jobs");
  assert.strictEqual(page.shown(), "flat queued");
  page = await open("/ui/jobs", local);
  assert.strictEqual(page.shown(), "flat queued");

  // Status tabs and filters stay in flat.
  await page.click("done");
  assert.strictEqual(page.shown(), "flat done");
  assert.strictEqual(page.url(), "/ui/jobs?view=flat&status=done");
  await page.click("クリア");
  assert.strictEqual(page.shown(), "flat done");

  // view=chain in the URL wins over the stored flat and does not replace it.
  page = await open("/ui/jobs?view=chain", local);
  assert.strictEqual(page.shown(), "chain すべて");
  await page.click("failed");
  assert.strictEqual(page.shown(), "chain failed");
  assert.strictEqual(page.url(), "/ui/jobs?view=chain&status=failed");
  await page.click("すべて");
  assert.strictEqual(page.shown(), "chain すべて");
  assert.strictEqual(local.getItem(KEY), "flat");
  await page.link("/ui/jobs");
  assert.strictEqual(page.shown(), "flat queued");

  // view=flat in the URL wins over the default and over a stored chain.
  local = storage();
  page = await open("/ui/jobs?view=flat", local);
  assert.strictEqual(page.shown(), "flat queued");
  assert.strictEqual(local.getItem(KEY), null);
  local.setItem(KEY, "chain");
  page = await open("/ui/jobs?view=flat&status=failed", local);
  assert.strictEqual(page.shown(), "flat failed");
  assert.strictEqual(local.getItem(KEY), "chain");

  // Picking chain is stored too.
  local.setItem(KEY, "flat");
  page = await open("/ui/jobs", local);
  await page.click("チェーン表示");
  assert.strictEqual(page.shown(), "chain すべて");
  assert.strictEqual(local.getItem(KEY), "chain");

  // Clicking the already active view of an explicit URL stores it.
  local.setItem(KEY, "flat");
  page = await open("/ui/jobs?view=chain", local);
  await page.click("チェーン表示");
  assert.strictEqual(local.getItem(KEY), "chain");

  // Links without a view (dashboard cards, back to list) follow the stored choice.
  page = await open("/ui/", storage());
  await page.link("/ui/jobs?status=done");
  assert.strictEqual(page.shown(), "chain done");
  page = await open("/ui/", storage({ [KEY]: "flat" }));
  await page.link("/ui/jobs?status=done");
  assert.strictEqual(page.shown(), "flat done");

  // Back and refresh show an entry the way it was shown, not the newer choice.
  local = storage();
  page = await open("/ui/jobs", local);
  await page.click("フラット表示");
  await page.back();
  assert.strictEqual(page.url(), "/ui/jobs");
  assert.strictEqual(page.shown(), "chain すべて");
  assert.strictEqual(local.getItem(KEY), "flat");
  await page.refresh();
  assert.strictEqual(page.shown(), "chain すべて");

  // Unknown values are ignored, in the URL and in storage.
  page = await open("/ui/jobs?view=bogus", storage({ [KEY]: "flat" }));
  assert.strictEqual(page.shown(), "flat queued");
  page = await open("/ui/jobs", storage({ [KEY]: "bogus" }));
  assert.strictEqual(page.shown(), "chain すべて");

  // Without usable storage the page still works.
  page = await open("/ui/jobs", brokenStorage);
  assert.strictEqual(page.shown(), "chain すべて");
  await page.click("フラット表示");
  assert.strictEqual(page.shown(), "flat queued");

  console.log("OK");
  process.exit(0);
})().catch((err) => { console.error(err); process.exit(1); });
`
	out, err := exec.Command(nodeBin, "-e", script, assets).CombinedOutput()
	if err != nil {
		t.Fatalf("node jobs view tests failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "OK") {
		t.Fatalf("unexpected node test output: %s", out)
	}
}

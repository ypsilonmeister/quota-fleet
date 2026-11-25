package ui

import (
	"bytes"
	"encoding/json"
	"image"
	"image/png"
	"math"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func get(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	NewHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

// servedPNG fetches an icon, checks its headers and decodes it.
func servedPNG(t *testing.T, path string) image.Image {
	t.Helper()
	rec := get(t, path)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200", path, rec.Code)
	}
	for header, want := range map[string]string{
		"Content-Type":            "image/png",
		"Cache-Control":           "no-cache",
		"Content-Security-Policy": ContentSecurityPolicy,
		"X-Content-Type-Options":  "nosniff",
	} {
		if got := rec.Header().Get(header); got != want {
			t.Errorf("GET %s %s = %q, want %q", path, header, got, want)
		}
	}
	img, err := png.Decode(rec.Body)
	if err != nil {
		t.Fatalf("GET %s is not a PNG: %v", path, err)
	}
	return img
}

// themeColor reads a color token of a direction from style.css.
func themeColor(t *testing.T, direction, token string) string {
	t.Helper()
	pattern := `(?s)\nhtml\[data-direction="` + direction + `"\] \{[^}]*?` + regexp.QuoteMeta(token) + `:\s*(#[0-9a-fA-F]{6});`
	m := regexp.MustCompile(pattern).FindSubmatch(styleCSS)
	if m == nil {
		t.Fatalf("style.css has no %s for %s", token, direction)
	}
	return string(m[1])
}

func TestUIManifestServed(t *testing.T) {
	const path = "/ui/manifest.webmanifest"
	rec := get(t, path)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200", path, rec.Code)
	}
	for header, want := range map[string]string{
		"Content-Type":            "application/manifest+json",
		"Cache-Control":           "no-cache",
		"Content-Security-Policy": ContentSecurityPolicy,
		"X-Content-Type-Options":  "nosniff",
	} {
		if got := rec.Header().Get(header); got != want {
			t.Errorf("GET %s %s = %q, want %q", path, header, got, want)
		}
	}
	if !bytes.Equal(rec.Body.Bytes(), manifestJSON) {
		t.Errorf("GET %s did not return the embedded manifest", path)
	}

	var manifest struct {
		ID              string `json:"id"`
		Name            string `json:"name"`
		ShortName       string `json:"short_name"`
		StartURL        string `json:"start_url"`
		Scope           string `json:"scope"`
		Display         string `json:"display"`
		ThemeColor      string `json:"theme_color"`
		BackgroundColor string `json:"background_color"`
		Icons           []struct {
			Src     string `json:"src"`
			Sizes   string `json:"sizes"`
			Type    string `json:"type"`
			Purpose string `json:"purpose"`
		} `json:"icons"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &manifest); err != nil {
		t.Fatalf("manifest is not valid JSON: %v", err)
	}
	for field, pair := range map[string][2]string{
		"id":               {manifest.ID, "/ui/"},
		"name":             {manifest.Name, "quota-fleet"},
		"short_name":       {manifest.ShortName, "quota-fleet"},
		"start_url":        {manifest.StartURL, "/ui/"},
		"scope":            {manifest.Scope, "/ui/"},
		"display":          {manifest.Display, "standalone"},
		"theme_color":      {manifest.ThemeColor, themeColor(t, "console", "--chrome-bg")},
		"background_color": {manifest.BackgroundColor, themeColor(t, "console", "--bg")},
	} {
		if pair[0] != pair[1] {
			t.Errorf("manifest %s = %q, want %q", field, pair[0], pair[1])
		}
	}

	var declared []string
	for _, icon := range manifest.Icons {
		declared = append(declared, icon.Sizes+" "+icon.Purpose)
		if icon.Type != "image/png" {
			t.Errorf("icon %s type = %q, want image/png", icon.Src, icon.Type)
		}
		size := servedPNG(t, icon.Src).Bounds().Size()
		if got := strconv.Itoa(size.X) + "x" + strconv.Itoa(size.Y); got != icon.Sizes {
			t.Errorf("icon %s is %s, manifest says %s", icon.Src, got, icon.Sizes)
		}
	}
	want := []string{"192x192 any", "512x512 any", "512x512 maskable"}
	if !slices.Equal(declared, want) {
		t.Errorf("manifest icons = %q, want %q", declared, want)
	}
}

func TestUIFullBleedIcons(t *testing.T) {
	for _, tc := range []struct {
		path string
		size int
		// safeRadius is the radius, relative to the size, that must hold the whole mark.
		safeRadius float64
	}{
		{"/ui/assets/icon-maskable-512.png", 512, 0.4},
		{"/ui/assets/apple-touch-icon.png", 180, 0.5},
	} {
		img := servedPNG(t, tc.path)
		if got := img.Bounds().Size(); got != image.Pt(tc.size, tc.size) {
			t.Fatalf("%s size = %v, want %d", tc.path, got, tc.size)
		}
		bgR, bgG, bgB, _ := img.At(0, 0).RGBA()
		marked, transparent, outside := 0, 0, 0
		center := float64(tc.size) / 2
		for y := range tc.size {
			for x := range tc.size {
				r, g, b, a := img.At(x, y).RGBA()
				if a != 0xffff {
					transparent++
				}
				if r != bgR || g != bgG || b != bgB {
					marked++
					if math.Hypot(float64(x)+0.5-center, float64(y)+0.5-center) > tc.safeRadius*float64(tc.size) {
						outside++
					}
				}
			}
		}
		if transparent != 0 || outside != 0 || marked == 0 {
			t.Errorf("%s: transparent=%d outside safe zone=%d marked=%d, want 0, 0, >0",
				tc.path, transparent, outside, marked)
		}
	}
}

func TestUIIndexHasPWAMeta(t *testing.T) {
	html := string(indexHTML)
	for _, token := range []string{
		`<link rel="manifest" href="/ui/manifest.webmanifest">`,
		`<meta name="theme-color" content="` + themeColor(t, "console", "--chrome-bg") + `">`,
		`<meta name="mobile-web-app-capable" content="yes">`,
		`<meta name="apple-mobile-web-app-capable" content="yes">`,
		`<meta name="apple-mobile-web-app-status-bar-style" content="black">`,
		`<meta name="apple-mobile-web-app-title" content="quota-fleet">`,
		`<link rel="apple-touch-icon" sizes="180x180" href="/ui/assets/apple-touch-icon.png">`,
	} {
		if !strings.Contains(html, token) {
			t.Errorf("index.html missing %q", token)
		}
	}
	// direction.js updates the meta element, so it must already exist when the script runs.
	if strings.Index(html, `name="theme-color"`) > strings.Index(html, `src="/ui/assets/direction.js"`) {
		t.Errorf("index.html must declare theme-color before loading direction.js")
	}
	// The app shell cache needs the worker even while notifications are off.
	// cacheShell registers the worker and asks it for the cache.
	for _, token := range []string{
		"\n  void notificationPresenter.cacheShell();\n",
		`window.addEventListener("online", function () { void notificationPresenter.cacheShell(); });`,
	} {
		if !strings.Contains(string(appJS), token) {
			t.Errorf("app.js missing app shell cache wiring %q", token)
		}
	}
}

func TestUIServiceWorkerShellListAndVersion(t *testing.T) {
	sw := string(swJS)
	block := regexp.MustCompile(`(?s)const SHELL_ASSETS = \[(.*?)\];`).FindStringSubmatch(sw)
	if block == nil {
		t.Fatal("sw.js has no SHELL_ASSETS list")
	}
	var cached, served []string
	for _, m := range regexp.MustCompile(`"([^"]*)"`).FindAllStringSubmatch(block[1], -1) {
		cached = append(cached, m[1])
	}
	for _, asset := range shellAssets {
		served = append(served, asset.path)
	}
	if !slices.Equal(cached, served) {
		t.Errorf("sw.js SHELL_ASSETS = %q, want the served shell assets %q", cached, served)
	}
	// Everything index.html loads from this server is available offline.
	for _, m := range regexp.MustCompile(`(?:src|href)="(/ui/[^"]*\.[a-z]+)"`).FindAllStringSubmatch(string(indexHTML), -1) {
		if !slices.Contains(cached, m[1]) {
			t.Errorf("index.html loads %s, which sw.js does not cache", m[1])
		}
	}

	if strings.Contains(sw, shellVersionPlaceholder) {
		t.Errorf("served sw.js still contains the version placeholder")
	}
	source := mustRead("assets/sw.js")
	version := shellVersion(source)
	if !strings.Contains(sw, `const SHELL_VERSION = "`+version+`";`) || len(version) != 12 {
		t.Errorf("served sw.js does not carry shell version %q", version)
	}
	if shellVersion(append(slices.Clone(source), '\n')) == version {
		t.Errorf("shell version must change with the worker source")
	}
}

func TestUIThemeColorFollowsDirectionWithNode(t *testing.T) {
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available in PATH; skipping JS theme color test")
	}
	script := `
const assert = require("assert");
const direction = require("./assets/direction.js");
const [dark, light] = process.argv.slice(1);
const metas = {};
globalThis.document = {
  documentElement: { setAttribute() {} },
  getElementById: () => null,
  querySelector: selector => ({ setAttribute: (_name, value) => { metas[selector] = value; } })
};
direction.applyDirection("ledger", false);
assert.strictEqual(metas['meta[name="theme-color"]'], light);
direction.applyDirection("console", false);
assert.strictEqual(metas['meta[name="theme-color"]'], dark);
`
	out, err := exec.Command(nodeBin, "-e", script,
		themeColor(t, "console", "--chrome-bg"), themeColor(t, "ledger", "--chrome-bg")).CombinedOutput()
	if err != nil {
		t.Fatalf("node theme color test failed: %v\n%s", err, out)
	}
}

func TestUIServiceWorkerCacheWithNode(t *testing.T) {
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available in PATH; skipping JS service worker cache test")
	}
	// The worker's page routes must be exactly those the handler serves the shell for.
	pages := []string{"/ui/", "/ui/jobs", "/ui/jobs/new", "/ui/jobs/42", "/ui/gate/held",
		"/ui/gate/decisions", "/ui/deliveries", "/ui/agents"}
	for _, path := range pages {
		if rec := get(t, path); rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), indexHTML) {
			t.Fatalf("GET %s does not serve the shell (status %d)", path, rec.Code)
		}
	}
	input, err := json.Marshal(map[string]any{"source": string(swJS), "pages": pages})
	if err != nil {
		t.Fatal(err)
	}

	script := `
const assert = require("assert");
const fs = require("fs");
const vm = require("vm");
const input = JSON.parse(fs.readFileSync(0, "utf8"));
const ORIGIN = "https://fleet.example";
const VERSION = /const SHELL_VERSION = "([0-9a-f]+)";/.exec(input.source)[1];
const CACHE = "qf-shell-" + VERSION;
const ASSETS = ["/ui/assets/style.css", "/ui/assets/direction.js", "/ui/assets/notify.js",
  "/ui/assets/app.js", "/ui/manifest.webmanifest", "/ui/assets/icon-192.png",
  "/ui/assets/icon-512.png", "/ui/assets/icon-maskable-512.png", "/ui/assets/apple-touch-icon.png"];

function load(options) {
  // gate, if set, holds every addAll back until it resolves (a slow network).
  const opts = Object.assign({ active: null, offline: false, addAllFails: false, gate: null, deleteFails: false, caches: [] }, options);
  const listeners = {};
  const log = { fetched: [], addAlls: 0, added: [], matched: [], deleted: [], skipped: 0, claimed: 0 };
  const stores = new Map(opts.caches.map(name => [name, new Map()]));
  const cacheFor = name => ({
    addAll: async requests => {
      log.addAlls++;
      await opts.gate;
      if (opts.addAllFails) throw new TypeError("addAll failed");
      for (const r of requests) {
        log.added.push([name, r.url, r.cache]);
        stores.get(name).set(r.url, "cached " + r.url);
      }
    },
    match: async key => { log.matched.push([name, key]); return stores.get(name).get(key); },
    put: async () => assert.fail("the cache is only written as a whole shell"),
    add: async () => assert.fail("the cache is only written as a whole shell")
  });
  const caches = {
    open: async name => { if (!stores.has(name)) stores.set(name, new Map()); return cacheFor(name); },
    keys: async () => Array.from(stores.keys()),
    delete: async name => {
      if (opts.deleteFails) throw new Error("delete failed");
      log.deleted.push(name);
      return stores.delete(name);
    }
  };
  const self = {
    location: { origin: ORIGIN },
    registration: { active: opts.active },
    addEventListener: (type, fn) => { listeners[type] = fn; },
    skipWaiting: async () => { log.skipped++; },
    clients: { claim: async () => { log.claimed++; } }
  };
  const fetch = async request => {
    log.fetched.push(request.url);
    if (opts.offline) throw new TypeError("offline");
    return "network " + request.url;
  };
  function Request(url, init) { this.url = url; this.cache = init && init.cache; }
  vm.runInNewContext(input.source, { self, caches, fetch, Request, URL, setTimeout, clearTimeout });

  const lifecycle = type => {
    let waited = null;
    listeners[type]({ waitUntil: p => { waited = p; } });
    assert.ok(waited, type + " must call waitUntil");
    return waited;
  };
  // request resolves to null when the worker leaves the request to the browser.
  const request = (path, extra) => {
    let response = null;
    const url = path.startsWith("/") ? ORIGIN + path : path;
    listeners.fetch({
      request: Object.assign({ method: "GET", mode: "cors", url }, extra),
      respondWith: p => { response = p; }
    });
    return response;
  };
  // message resolves to null when the worker ignores the data.
  const message = data => {
    let waited = null;
    listeners.message({ data, waitUntil: p => { waited = p; } });
    return waited;
  };
  return { opts, log, stores, lifecycle, request, message };
}

const CACHE_SHELL = { type: "qf-cache-shell" };
// settles tells whether the promise resolves without waiting for anything external.
const settles = p => Promise.race([p.then(() => true), new Promise(r => setImmediate(r, false))]);
function gate() {
  let open;
  const promise = new Promise(r => { open = r; });
  return { promise, open };
}

async function main() {
  const SHELL = ["/ui/"].concat(ASSETS).map(path => [CACHE, path, "reload"]);

  // An update precaches exactly the app shell into the versioned cache before it takes over.
  const slow = gate();
  const w = load({ active: {}, gate: slow.promise });
  const installing = w.lifecycle("install");
  assert.strictEqual(await settles(installing), false, "an update waits for its cache");
  assert.strictEqual(w.log.skipped, 0);
  slow.open();
  await installing;
  assert.deepStrictEqual(w.log.added, SHELL);
  assert.strictEqual(w.log.skipped, 1);
  // A complete cache is not downloaded again.
  await w.message(CACHE_SHELL);
  assert.strictEqual(w.log.addAlls, 1);

  // A first installation does not wait for the shell: on a slow network the worker must
  // still become active in time to show notifications.
  const network = gate();
  const first = load({ gate: network.promise });
  assert.strictEqual(await settles(first.lifecycle("install")), true, "first installation");
  assert.strictEqual(first.log.skipped, 1);
  assert.strictEqual(await settles(first.lifecycle("activate")), true, "first activation");
  assert.strictEqual(first.log.claimed, 1);
  // The cache is filled on request, once for all the tabs asking meanwhile.
  const asked = [first.message(CACHE_SHELL), first.message(CACHE_SHELL)];
  assert.strictEqual(await settles(asked[0]), false, "the worker is kept alive for the download");
  network.open();
  await Promise.all(asked);
  assert.strictEqual(first.log.addAlls, 1);
  assert.deepStrictEqual(first.log.added, SHELL);
  // One missing entry makes the whole shell be fetched again.
  first.stores.get(CACHE).delete("/ui/assets/app.js");
  await first.message(CACHE_SHELL);
  assert.strictEqual(first.log.addAlls, 2);
  assert.ok(first.stores.get(CACHE).has("/ui/assets/app.js"));
  for (const data of [null, undefined, "qf-cache-shell", {}, { type: "other" }]) {
    assert.strictEqual(first.message(data), null, JSON.stringify(data));
  }

  // API, event stream, MCP, non-GET and foreign requests are never intercepted,
  // whether the network is up or not.
  const api = ["/jobs", "/jobs?status=queued", "/jobs/1", "/status", "/meta", "/deliveries",
    "/gate/held", "/gate/decisions", "/agents", "/agents/events", "/events", "/mcp", "/mcp/", "/", "/ui"];
  for (const offline of [false, true]) {
    const x = load({ offline, caches: [CACHE] });
    // Even a matching cache entry must not be used.
    for (const path of api) x.stores.get(CACHE).set(path, "poisoned");
    for (const path of api) {
      for (const mode of ["cors", "same-origin", "navigate"]) {
        assert.strictEqual(x.request(path, { mode }), null, mode + " " + path);
      }
    }
    for (const method of ["POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"]) {
      for (const path of api.concat(["/ui/", "/ui/jobs", "/ui/assets/app.js"])) {
        assert.strictEqual(x.request(path, { method }), null, method + " " + path);
        assert.strictEqual(x.request(path, { method, mode: "navigate" }), null, method + " navigate " + path);
      }
    }
    for (const path of ["/ui/sw.js", "/ui/assets/app.js?v=1", "/ui/unknown", "/ui/assets/other.js", "/ui/jobs",
      "/ui/jobs/1", "https://other.example/ui/assets/app.js", "http://fleet.example/ui/assets/app.js"]) {
      assert.strictEqual(x.request(path), null, "subresource " + path);
    }
    for (const path of ["/ui/sw.js", "/ui/manifest.webmanifest", "/ui/assets/app.js", "/ui/unknown",
      "/ui/jobs/1/extra", "/ui/gate", "https://other.example/ui/"]) {
      assert.strictEqual(x.request(path, { mode: "navigate" }), null, "navigate " + path);
    }
    assert.deepStrictEqual(x.log.fetched, []);
    assert.deepStrictEqual(x.log.matched, []);
    assert.deepStrictEqual(x.log.added, []);
  }

  // Online: the network answers and the cache is not consulted.
  w.log.matched.length = 0;
  for (const path of ASSETS) {
    assert.strictEqual(await w.request(path), "network " + ORIGIN + path);
  }
  for (const path of input.pages) {
    assert.strictEqual(await w.request(path, { mode: "navigate" }), "network " + ORIGIN + path);
  }
  assert.deepStrictEqual(w.log.matched, []);

  // Offline: assets come from the cache, and every page falls back to the one shell.
  const off = load({ offline: true });
  await off.message(CACHE_SHELL);
  for (const path of ASSETS) {
    assert.strictEqual(await off.request(path), "cached " + path);
  }
  for (const path of input.pages.concat(["/ui/jobs?view=chain"])) {
    assert.strictEqual(await off.request(path, { mode: "navigate" }), "cached /ui/", path);
  }
  await assert.rejects(load({ offline: true }).request("/ui/assets/app.js"), /offline/);

  // A first installation whose download fails recovers with the same version once the
  // network is back.
  const flaky = load({ addAllFails: true });
  await flaky.lifecycle("install");
  await flaky.lifecycle("activate");
  await flaky.message(CACHE_SHELL);
  await flaky.message(CACHE_SHELL);
  assert.strictEqual(flaky.log.addAlls, 2, "every request tries again after a failure");
  assert.deepStrictEqual(flaky.log.added, []);
  flaky.opts.addAllFails = false;
  await flaky.message(CACHE_SHELL);
  assert.deepStrictEqual(flaky.log.added, SHELL);
  flaky.opts.offline = true;
  assert.strictEqual(await flaky.request("/ui/jobs/7", { mode: "navigate" }), "cached /ui/");
  assert.strictEqual(await flaky.request("/ui/assets/app.js"), "cached /ui/assets/app.js");

  // An update whose download fails is abandoned.
  const update = load({ addAllFails: true, active: {} });
  await assert.rejects(update.lifecycle("install"), /addAll failed/);
  assert.strictEqual(update.log.skipped, 0);

  // Activation drops older shell caches only, then claims the tabs.
  const old = load({ caches: ["qf-shell-old", CACHE, "unrelated", "qf-shell-older"] });
  await old.lifecycle("activate");
  assert.deepStrictEqual(old.log.deleted.sort(), ["qf-shell-old", "qf-shell-older"]);
  assert.deepStrictEqual(Array.from(old.stores.keys()).sort(), [CACHE, "unrelated"].sort());
  assert.strictEqual(old.log.claimed, 1);
  const stuck = load({ caches: ["qf-shell-old"], deleteFails: true });
  await stuck.lifecycle("activate");
  assert.strictEqual(stuck.log.claimed, 1);

  console.log("ALL_UI_SW_CACHE_TESTS_PASSED");
}

main().catch(err => { console.error(err); process.exit(1); });
`
	cmd := exec.Command(nodeBin, "-e", script)
	cmd.Stdin = bytes.NewReader(input)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node service worker cache tests failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "ALL_UI_SW_CACHE_TESTS_PASSED") {
		t.Fatalf("unexpected node test output: %s", out)
	}
}

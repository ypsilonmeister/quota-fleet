package ui

import (
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
)

func TestUINotifyAssetsPresent(t *testing.T) {
	html := string(indexHTML)
	notifyAt := strings.Index(html, `<script defer src="/ui/assets/notify.js"></script>`)
	appAt := strings.Index(html, `<script defer src="/ui/assets/app.js"></script>`)
	if notifyAt < 0 || appAt < 0 || notifyAt > appAt {
		t.Errorf("index.html must load notify.js before app.js (notify=%d app=%d)", notifyAt, appAt)
	}
	if !strings.Contains(html, `id="btn-notify"`) {
		t.Errorf("index.html missing the notification toggle")
	}

	// docs/14: the token travels only in the Authorization header.
	for name, src := range map[string]string{"notify.js": string(notifyJS), "app.js": string(appJS), "sw.js": string(swJS)} {
		for _, forbidden := range []string{"new EventSource", "token="} {
			if strings.Contains(src, forbidden) {
				t.Errorf("%s must not contain %q", name, forbidden)
			}
		}
	}

	js := string(appJS)
	for _, token := range []string{`createNotifyController`, `createEventStream`, `createJobNotifier`, `refreshListView`,
		`createNotificationPresenter`, `navigator.serviceWorker`, `show: notificationPresenter.show`, `notificationPresenter.register()`} {
		if !strings.Contains(js, token) {
			t.Errorf("app.js missing notification wiring %q", token)
		}
	}
}

func TestUIServiceWorkerServed(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/ui/sw.js", nil)
	rec := httptest.NewRecorder()
	NewHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /ui/sw.js status = %d, want 200", rec.Code)
	}
	for header, want := range map[string]string{
		"Content-Type":            "application/javascript; charset=utf-8",
		"Cache-Control":           "no-store",
		"Content-Security-Policy": ContentSecurityPolicy,
		"X-Content-Type-Options":  "nosniff",
	} {
		if got := rec.Header().Get(header); got != want {
			t.Errorf("GET /ui/sw.js %s = %q, want %q", header, got, want)
		}
	}
	if got := rec.Body.String(); got != string(swJS) || !strings.Contains(got, `"notificationclick"`) {
		t.Errorf("GET /ui/sw.js did not return the embedded service worker (%d bytes)", len(got))
	}
	// The module registers exactly the path served here.
	for _, token := range []string{`const SW_URL = "/ui/sw.js";`, `const SW_SCOPE = "/ui/";`} {
		if !strings.Contains(string(notifyJS), token) {
			t.Errorf("notify.js missing %q", token)
		}
	}
}

func TestUIServiceWorkerWithNode(t *testing.T) {
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available in PATH; skipping JS service worker test")
	}
	if out, err := exec.Command(nodeBin, "--check", "assets/sw.js").CombinedOutput(); err != nil {
		t.Fatalf("node --check assets/sw.js failed: %v\n%s", err, string(out))
	}

	script := `
const assert = require("assert");
const fs = require("fs");
const vm = require("vm");
const source = fs.readFileSync("assets/sw.js", "utf8");
const ORIGIN = "https://fleet.example";

function load(windows) {
  const listeners = {};
  const log = { opened: [], queries: [], skipped: 0 };
  const self = {
    location: { origin: ORIGIN },
    addEventListener: (type, fn) => { listeners[type] = fn; },
    skipWaiting: () => { log.skipped++; },
    clients: {
      matchAll: async query => { log.queries.push(query); return windows; },
      openWindow: async url => { log.opened.push(url); }
    }
  };
  vm.runInNewContext(source, { self, URL });
  const click = async data => {
    let closed = false, waited = null;
    listeners.notificationclick({
      notification: { data, close: () => { closed = true; } },
      waitUntil: p => { waited = p; }
    });
    assert.ok(closed, "the notification is closed");
    await waited;
    return waited !== null;
  };
  return { listeners, log, click };
}

function client(path, extra) {
  const c = Object.assign({ url: ORIGIN + path, focused: false, visibilityState: "hidden", focuses: 0, messages: [] }, extra);
  c.focus = async () => { c.focuses++; if (c.gone) throw new Error("gone"); return c; };
  c.postMessage = m => { c.messages.push(JSON.parse(JSON.stringify(m))); };
  return c;
}

async function main() {
  {
    const w = load([]);
    assert.deepStrictEqual(Object.keys(w.listeners).sort(), ["install", "notificationclick"], "no fetch handler");
    w.listeners.install({});
    assert.strictEqual(w.log.skipped, 1);

    assert.ok(await w.click({ jobId: 7 }));
    assert.deepStrictEqual(w.log.opened, [ORIGIN + "/ui/jobs/7"], "no tab: open one");
    assert.strictEqual(w.log.queries[0].type, "window");
    assert.strictEqual(w.log.queries[0].includeUncontrolled, true);

    for (const bad of [undefined, null, {}, { jobId: 0 }, { jobId: "7" }, { jobId: "../x" }]) {
      assert.strictEqual(await w.click(bad), false, JSON.stringify(bad));
    }
    assert.strictEqual(w.log.opened.length, 1);
  }

  // Tab choice: already on the job, then focused, then visible, then any UI tab.
  const preference = [
    client("/ui/jobs/7"),
    client("/ui/jobs", { focused: true }),
    client("/ui/", { visibilityState: "visible" }),
    client("/ui/deliveries")
  ];
  for (let skip = 0; skip < preference.length; skip++) {
    const outside = client("/status", { focused: true, visibilityState: "visible" });
    const tabs = [outside].concat(preference.slice(skip).reverse());
    // Reversed order shows the choice is by preference; the last case has one tab left.
    const w = load(tabs);
    await w.click({ jobId: 7 });
    const chosen = preference[skip];
    assert.strictEqual(chosen.focuses, 1, "focus " + chosen.url);
    assert.deepStrictEqual(chosen.messages, [{ type: "qf-open-job", jobId: 7 }]);
    assert.strictEqual(tabs.reduce((sum, c) => sum + c.messages.length, 0), 1);
    assert.strictEqual(outside.focuses, 0, "pages outside the UI are left alone");
    assert.deepStrictEqual(w.log.opened, []);
    for (const c of preference) { c.focuses = 0; c.messages.length = 0; }
  }

  {
    const gone = client("/ui/", { gone: true });
    const w = load([gone]);
    await w.click({ jobId: 3 });
    assert.deepStrictEqual(gone.messages, []);
    assert.deepStrictEqual(w.log.opened, [ORIGIN + "/ui/jobs/3"], "unfocusable tab: open one");
  }

  console.log("ALL_UI_SW_TESTS_PASSED");
}

main().catch(err => { console.error(err); process.exit(1); });
`
	out, err := exec.Command(nodeBin, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("node service worker unit tests failed: %v\n%s", err, string(out))
	}
	if !strings.Contains(string(out), "ALL_UI_SW_TESTS_PASSED") {
		t.Fatalf("unexpected node test output: %s", string(out))
	}
}

func TestUINotifyModuleWithNode(t *testing.T) {
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available in PATH; skipping JS notification test")
	}
	if out, err := exec.Command(nodeBin, "--check", "assets/notify.js").CombinedOutput(); err != nil {
		t.Fatalf("node --check assets/notify.js failed: %v\n%s", err, string(out))
	}

	script := `
const assert = require("assert");
const n = require("./assets/notify.js");

const tick = () => new Promise(r => setImmediate(r));
const flush = async () => { for (let i = 0; i < 10; i++) await tick(); };

function fakeNotification(permission, answer) {
  const created = [];
  function Ctor(title, options) {
    this.title = title; this.options = options; this.closed = false;
    this.close = () => { this.closed = true; };
    created.push(this);
  }
  Ctor.permission = permission;
  Ctor.prompts = 0;
  Ctor.requestPermission = async () => { Ctor.prompts++; Ctor.permission = answer; return answer; };
  Ctor.created = created;
  return Ctor;
}

function fakeTimers() {
  let next = 1;
  const pending = new Map();
  return {
    setTimeout: (fn, ms) => { const id = next++; pending.set(id, { fn, ms }); return id; },
    clearTimeout: id => { pending.delete(id); },
    delays: () => Array.from(pending.values(), t => t.ms),
    fire: async ms => {
      const hit = Array.from(pending).find(([, t]) => t.ms === ms);
      assert.ok(hit, "no timer for " + ms + "ms; pending " + Array.from(pending.values(), t => t.ms));
      pending.delete(hit[0]);
      hit[1].fn();
      await flush();
    }
  };
}

const ev = (id, status, prev, title) => ({ job_id: id, status, prev_status: prev, title: title || "t" + id });

async function main() {
  // 1. SSE parsing across arbitrary chunk boundaries, comments and CRLF
  {
    const wire = ": connected\r\n\r\nevent: job_status\r\ndata: {\"a\":1}\r\n\r\n: heartbeat\n\ndata: x\ndata:y\n\nevent: empty\n\n";
    for (const size of [1, 2, 3, 7, wire.length]) {
      const got = [];
      const p = n.createSSEParser((name, data) => got.push([name, data]));
      for (let i = 0; i < wire.length; i += size) p.push(wire.slice(i, i + size));
      assert.deepStrictEqual(got, [["job_status", "{\"a\":1}"], ["message", "x\ny"]], "chunk size " + size);
    }
  }

  // 2. Transition -> notification mapping and deduplication
  {
    const N = fakeNotification("granted");
    const shown = [];
    const notify = n.createJobNotifier({ getNotification: () => N, show: async m => { shown.push(m); } });
    for (const quiet of [ev(1, "queued", ""), ev(1, "claimed", "queued"), ev(1, "waiting", ""), ev(1, "draft", ""),
                         ev(1, "done", "done"), ev(0, "done", "claimed"), ev(1, "toString", "claimed"), null, {}]) {
      assert.strictEqual(notify(quiet), null, JSON.stringify(quiet));
    }
    assert.strictEqual(shown.length, 0);

    assert.strictEqual(await notify(ev(1, "done", "claimed", "Build")), true);
    await notify(ev(2, "failed", "claimed", "Deploy"));
    await notify(ev(3, "cancelled", "waiting"));
    assert.deepStrictEqual(shown.map(x => x.title),
      ["ジョブ #1 が完了しました", "ジョブ #2 が失敗しました", "ジョブ #3 がキャンセルされました"]);
    assert.deepStrictEqual(shown[1], {
      jobID: 2, title: "ジョブ #2 が失敗しました", body: "Deploy\n状態: failed（失敗）", tag: "qf-job-2:failed"
    });

    assert.strictEqual(notify(ev(1, "done", "claimed")), null, "duplicate job_id+status");
    await notify(ev(2, "done", "claimed"));
    assert.strictEqual(shown.length, 4, "same job, other status still notifies");

    N.permission = "denied";
    assert.strictEqual(notify(ev(9, "done", "claimed")), null, "no permission");
    N.permission = "granted";
    assert.ok(await notify(ev(9, "done", "claimed")), "an unshown event is not remembered");
  }
  {
    const N = fakeNotification("granted");
    let errors = 0, calls = 0, fail;
    const notify = n.createJobNotifier({
      getNotification: () => N,
      show: () => { calls++; return new Promise((_res, rej) => { fail = rej; }); },
      onError: () => errors++
    });
    const first = notify(ev(1, "done", "claimed"));
    assert.strictEqual(notify(ev(1, "done", "claimed")), null, "duplicate while still being shown");
    fail(new Error("no display"));
    assert.strictEqual(await first, false);
    assert.strictEqual(errors, 1);
    assert.ok(notify(ev(1, "done", "claimed")), "a failed notification may be retried");
    assert.strictEqual(calls, 2);
  }

  // 2b. Display path: service worker first, new Notification() as the fallback
  const message = { jobID: 5, title: "T", body: "B", tag: "qf-job-5:done" };
  function fakeWorker(opts) {
    const log = { registers: [], shown: [], listeners: {} };
    const registration = {
      showNotification: async (title, options) => {
        if (opts.showFails) throw new TypeError("No active registration");
        log.shown.push({ title, options });
      }
    };
    const container = {
      addEventListener: (type, fn) => { log.listeners[type] = fn; },
      register: async (url, options) => {
        log.registers.push([url, options.scope]);
        if (opts.registerFails) throw new Error("SecurityError");
        return registration;
      },
      ready: opts.neverReady ? new Promise(() => {}) : Promise.resolve(opts.registration || registration)
    };
    return { container, log, opts };
  }
  function presenterFixture(worker, Ctor) {
    const timers = fakeTimers();
    const calls = [];
    const presenter = n.createNotificationPresenter({
      getServiceWorker: () => worker && worker.container,
      getNotification: () => Ctor,
      focus: () => calls.push("focus"),
      openJob: id => calls.push("open " + id),
      setTimeout: timers.setTimeout,
      clearTimeout: timers.clearTimeout
    });
    return { presenter, timers, calls };
  }
  function illegalConstructor() {
    function Ctor() { Ctor.attempts++; throw new TypeError("Illegal constructor"); }
    Ctor.permission = "granted";
    Ctor.attempts = 0;
    return Ctor;
  }
  {
    // Android Chrome: the constructor throws, the registration works.
    const w = fakeWorker({});
    const Ctor = illegalConstructor();
    const f = presenterFixture(w, Ctor);
    await Promise.all([f.presenter.show(message), f.presenter.show(message)]);
    await f.presenter.show(message);
    assert.deepStrictEqual(w.log.registers, [["/ui/sw.js", "/ui/"]], "registered once");
    assert.strictEqual(w.log.shown.length, 3);
    assert.deepStrictEqual(w.log.shown[0], { title: "T", options: { body: "B", tag: "qf-job-5:done", data: { jobId: 5 } } });
    assert.strictEqual(Ctor.attempts, 0, "service worker is preferred");
    assert.deepStrictEqual(f.timers.delays(), []);

    // clicks reach the page as messages from the worker
    for (const data of [null, "x", {}, { type: "other", jobId: 5 }, { type: "qf-open-job" }, { type: "qf-open-job", jobId: "5" }, { type: "qf-open-job", jobId: -1 }]) {
      w.log.listeners.message({ data });
    }
    assert.deepStrictEqual(f.calls, []);
    w.log.listeners.message({ data: { type: "qf-open-job", jobId: 5 } });
    assert.deepStrictEqual(f.calls, ["open 5"]);
  }
  for (const [name, worker] of [
    ["no service worker", null],
    ["register fails", fakeWorker({ registerFails: true })],
    ["showNotification fails", fakeWorker({ showFails: true })],
    ["no showNotification", fakeWorker({ registration: {} })]
  ]) {
    const N = fakeNotification("granted");
    const f = presenterFixture(worker, N);
    await f.presenter.show(message);
    assert.strictEqual(N.created.length, 1, name);
    assert.strictEqual(N.created[0].title, "T", name);
    assert.deepStrictEqual(N.created[0].options, { body: "B", tag: "qf-job-5:done" }, name);
    assert.deepStrictEqual(f.timers.delays(), [], name);

    let prevented = false;
    N.created[0].onclick({ preventDefault: () => { prevented = true; } });
    assert.ok(prevented, name);
    assert.deepStrictEqual(f.calls, ["focus", "open 5"], name);
    assert.strictEqual(N.created[0].closed, true, name);

    await assert.rejects(presenterFixture(worker, illegalConstructor()).presenter.show(message), /Illegal constructor/, name);
  }
  {
    // a failed registration is tried again
    const w = fakeWorker({ registerFails: true });
    const N = fakeNotification("granted");
    const f = presenterFixture(w, N);
    assert.strictEqual(await f.presenter.register(), null);
    w.opts.registerFails = false;
    await f.presenter.show(message);
    assert.strictEqual(w.log.registers.length, 2);
    assert.strictEqual(w.log.shown.length, 1);
    assert.strictEqual(N.created.length, 0);
  }
  {
    // a worker that never activates does not hold the notification back forever
    const w = fakeWorker({ neverReady: true });
    const N = fakeNotification("granted");
    const f = presenterFixture(w, N);
    const shown = f.presenter.show(message);
    await flush();
    assert.strictEqual(N.created.length, 0);
    await f.timers.fire(10000);
    await shown;
    assert.strictEqual(N.created.length, 1);
    w.container.ready = Promise.resolve({ showNotification: async () => w.log.shown.push("late") });
    await f.presenter.show(message);
    assert.deepStrictEqual(w.log.shown, ["late"]);
  }

  // 3. Event stream: bearer header only, events, reconnect with backoff, idle abort, fatal status
  {
    const timers = fakeTimers();
    const requests = [], events = [], fatals = [];
    let opens = 0;
    let respond;
    const stream = n.createEventStream({
      url: "/events",
      getToken: () => "sekret",
      setTimeout: timers.setTimeout,
      clearTimeout: timers.clearTimeout,
      fetch: async (url, opts) => { requests.push({ url, opts }); return respond(opts); },
      onOpen: () => opens++,
      onEvent: (name, data) => events.push([name, data]),
      onFatal: status => fatals.push(status)
    });
    const sse = body => new Response(body, { status: 200, headers: { "Content-Type": "text/event-stream" } });

    // network error -> 1s, 2s, 4s ... capped at 30s
    respond = () => { throw new Error("offline"); };
    stream.start();
    await flush();
    for (const ms of [1000, 2000, 4000, 8000, 16000, 30000, 30000]) {
      assert.deepStrictEqual(timers.delays(), [ms]);
      await timers.fire(ms);
    }

    // a stream that delivers data and ends resets the backoff and reconnects
    respond = () => sse(": connected\n\nevent: job_status\ndata: {\"job_id\":1}\n\n");
    await timers.fire(30000);
    assert.strictEqual(opens, 1);
    assert.deepStrictEqual(events, [["job_status", "{\"job_id\":1}"]]);
    assert.deepStrictEqual(timers.delays(), [1000]);
    const last = requests[requests.length - 1];
    assert.strictEqual(last.url, "/events", "no token in the URL");
    assert.strictEqual(last.opts.headers.Authorization, "Bearer sekret");

    // 5xx and non-SSE 200 responses are retried, not treated as open
    respond = () => new Response("bad gateway", { status: 502 });
    await timers.fire(1000);
    respond = () => new Response("<html>", { status: 200, headers: { "Content-Type": "text/html" } });
    await timers.fire(2000);
    assert.strictEqual(opens, 1);

    // a silent connection is aborted after 60s and retried
    let signal;
    respond = opts => {
      signal = opts.signal;
      return sse(new ReadableStream({
        start(c) { opts.signal.addEventListener("abort", () => c.error(new Error("aborted"))); }
      }));
    };
    await timers.fire(4000);
    assert.strictEqual(opens, 2);
    assert.deepStrictEqual(timers.delays(), [60000]);
    await timers.fire(60000);
    assert.strictEqual(signal.aborted, true);
    assert.deepStrictEqual(timers.delays(), [8000]);

    // stop() aborts the connection and schedules nothing
    await timers.fire(8000);
    stream.stop();
    await flush();
    assert.strictEqual(signal.aborted, true);
    assert.deepStrictEqual(timers.delays(), []);

    for (const status of [401, 403, 404]) {
      respond = () => new Response("{}", { status });
      const before = requests.length;
      stream.start();
      await flush();
      assert.strictEqual(requests.length, before + 1);
      assert.deepStrictEqual(timers.delays(), [], "no retry after " + status);
    }
    assert.deepStrictEqual(fatals, [401, 403, 404]);
  }

  // 4. Toggle: default off, permission on enable, persistence, no backlog notifications
  function controllerFixture(opts) {
    const store = new Map(opts.stored ? [[n.NOTIFY_KEY, opts.stored]] : []);
    const log = { streams: [], messages: [], views: [], notified: [], listChanged: 0, unauthorized: 0, prepared: 0 };
    const N = "notification" in opts ? opts.notification : fakeNotification("default", "granted");
    const env = { secure: true, credentials: true, token: true };
    const controller = n.createNotifyController({
      storage: { getItem: k => store.has(k) ? store.get(k) : null, setItem: (k, v) => store.set(k, v) },
      getNotification: () => N,
      isSecureContext: () => env.secure,
      hasCredentials: () => env.credentials,
      hasToken: () => env.token,
      createStream: handlers => {
        const s = { handlers, running: false, start() { this.running = true; }, stop() { this.running = false; } };
        // Like the real stream, a fatal status ends the connection before reporting it.
        s.fatal = status => { s.running = false; handlers.onFatal(status); };
        log.streams.push(s);
        return s;
      },
      prepare: () => log.prepared++,
      notify: e => log.notified.push(e),
      onListChanged: () => log.listChanged++,
      onChange: v => log.views.push(v),
      onMessage: m => log.messages.push(m),
      onUnauthorized: () => log.unauthorized++
    });
    const running = () => log.streams.filter(s => s.running).length;
    const view = () => log.views[log.views.length - 1];
    return { controller, store, log, N, env, running, view };
  }
  {
    const f = controllerFixture({});
    f.controller.sync();
    assert.strictEqual(f.running(), 0, "default off");
    assert.strictEqual(f.view().enabled, false);
    assert.strictEqual(f.N.prompts, 0, "no permission prompt without a click");
    assert.strictEqual(f.log.prepared, 0, "no service worker while off");

    await f.controller.toggle();
    assert.strictEqual(f.N.prompts, 1);
    assert.strictEqual(f.store.get("qf-job-notify"), "on");
    assert.strictEqual(f.running(), 1);
    assert.strictEqual(f.view().enabled, true);
    assert.ok(f.log.prepared > 0, "enabling readies the display path");
    f.controller.sync();
    assert.strictEqual(f.log.streams.length, 1, "sync does not reconnect");

    const h = f.log.streams[0].handlers;
    h.onOpen();
    h.onOpen();
    assert.strictEqual(f.log.listChanged, 2, "each connection reloads the list");
    assert.strictEqual(f.log.notified.length, 0, "reloading never notifies");
    h.onEvent("job_status", JSON.stringify(ev(4, "done", "claimed")));
    h.onEvent("other", "{}");
    h.onEvent("job_status", "not json");
    assert.deepStrictEqual(f.log.notified.map(e => e.job_id), [4]);
    assert.strictEqual(f.log.listChanged, 3);

    await f.controller.toggle();
    assert.strictEqual(f.store.get("qf-job-notify"), "off");
    assert.strictEqual(f.running(), 0);

    f.store.set("qf-job-notify", "on"); // another tab
    f.controller.reload();
    assert.strictEqual(f.running(), 1);
  }
  {
    const f = controllerFixture({ stored: "on", notification: fakeNotification("granted") });
    f.env.credentials = false;
    f.controller.sync();
    assert.strictEqual(f.running(), 0, "waits for credentials");
    f.env.credentials = true;
    f.controller.restart();
    assert.strictEqual(f.running(), 1, "stored preference is restored");

    f.log.streams[0].fatal(404);
    assert.ok(f.view().problem.includes("api.job_events"));
    assert.strictEqual(f.log.messages.length, 1);
    f.controller.sync();
    assert.strictEqual(f.log.streams.length, 1, "no retry while disabled on the server");
    await f.controller.toggle();
    assert.strictEqual(f.store.get("qf-job-notify"), "off");
    assert.strictEqual(f.view().problem, "");

    await f.controller.toggle();
    f.log.streams[f.log.streams.length - 1].fatal(401);
    assert.strictEqual(f.log.unauthorized, 1);
    f.controller.restart(); // new token
    assert.strictEqual(f.running(), 1);

    // Without a token there is nothing to re-enter, so the auth gate stays closed.
    f.env.token = false;
    const messages = f.log.messages.length;
    f.log.streams[f.log.streams.length - 1].fatal(401);
    assert.strictEqual(f.log.unauthorized, 1, "no auth gate without a token");
    assert.strictEqual(f.log.messages.length, messages + 1);
    assert.ok(f.view().problem.includes("API トークンが必要"));
    f.controller.sync();
    assert.strictEqual(f.running(), 0, "no retry without a token");
  }
  {
    const f = controllerFixture({ notification: fakeNotification("default", "denied") });
    await f.controller.toggle();
    assert.strictEqual(f.running(), 0);
    assert.strictEqual(f.store.get("qf-job-notify"), undefined);
    assert.strictEqual(f.view().enabled, false);
    assert.strictEqual(f.log.messages.length, 1);
  }
  {
    const f = controllerFixture({ stored: "on", notification: fakeNotification("default", "granted") });
    f.controller.sync();
    assert.strictEqual(f.running(), 0, "stored on without permission stays off");
    assert.strictEqual(f.N.prompts, 0);
  }
  {
    const insecure = controllerFixture({});
    insecure.env.secure = false;
    await insecure.controller.toggle();
    assert.strictEqual(insecure.N.prompts, 0);
    assert.ok(insecure.log.messages[0].includes("HTTPS"));
    const unsupported = controllerFixture({ notification: null });
    await unsupported.controller.toggle();
    assert.strictEqual(unsupported.running(), 0);
    assert.strictEqual(unsupported.log.messages.length, 1);
  }

  // 5. Refresher coalesces bursts and retries a deferred refresh
  {
    const timers = fakeTimers();
    let calls = 0, result = true, release;
    const r = n.createRefresher({
      setTimeout: timers.setTimeout, delayMs: 500, retryMs: 2000,
      refresh: () => { calls++; return release === null ? new Promise(res => { release = res; }) : Promise.resolve(result); }
    });
    r.request(); r.request(); r.request();
    assert.deepStrictEqual(timers.delays(), [500]);
    await timers.fire(500);
    assert.strictEqual(calls, 1);
    assert.deepStrictEqual(timers.delays(), []);

    result = false;
    r.request();
    await timers.fire(500);
    assert.deepStrictEqual(timers.delays(), [2000]);
    result = true;
    await timers.fire(2000);
    assert.strictEqual(calls, 3);
    assert.deepStrictEqual(timers.delays(), []);

    // a signal during a running refresh is not lost
    release = null;
    r.request();
    await timers.fire(500);
    r.request();
    await timers.fire(500);
    assert.strictEqual(calls, 4, "no concurrent refresh");
    release(true);
    release = undefined;
    await flush();
    await timers.fire(500);
    assert.strictEqual(calls, 5);
  }

  console.log("ALL_UI_NOTIFY_TESTS_PASSED");
}

main().catch(err => { console.error(err); process.exit(1); });
`
	out, err := exec.Command(nodeBin, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("node notification unit tests failed: %v\n%s", err, string(out))
	}
	// A hung promise would let node exit 0 early, so require the final marker.
	if !strings.Contains(string(out), "ALL_UI_NOTIFY_TESTS_PASSED") {
		t.Fatalf("unexpected node test output: %s", string(out))
	}
}

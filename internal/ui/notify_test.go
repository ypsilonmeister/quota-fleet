package ui

import (
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
	for name, src := range map[string]string{"notify.js": string(notifyJS), "app.js": string(appJS)} {
		for _, forbidden := range []string{"new EventSource", "token="} {
			if strings.Contains(src, forbidden) {
				t.Errorf("%s must not contain %q", name, forbidden)
			}
		}
	}

	js := string(appJS)
	for _, token := range []string{`createNotifyController`, `createEventStream`, `createJobNotifier`, `refreshListView`} {
		if !strings.Contains(js, token) {
			t.Errorf("app.js missing notification wiring %q", token)
		}
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

  // 2. Transition -> notification mapping, deduplication, click
  {
    const N = fakeNotification("granted");
    const calls = [];
    const notify = n.createJobNotifier({
      getNotification: () => N,
      focus: () => calls.push("focus"),
      openJob: id => calls.push("open " + id)
    });
    for (const quiet of [ev(1, "queued", ""), ev(1, "claimed", "queued"), ev(1, "waiting", ""), ev(1, "draft", ""),
                         ev(1, "done", "done"), ev(0, "done", "claimed"), ev(1, "toString", "claimed"), null, {}]) {
      assert.strictEqual(notify(quiet), null, JSON.stringify(quiet));
    }
    assert.strictEqual(N.created.length, 0);

    notify(ev(1, "done", "claimed", "Build"));
    notify(ev(2, "failed", "claimed", "Deploy"));
    notify(ev(3, "cancelled", "waiting"));
    assert.deepStrictEqual(N.created.map(x => x.title),
      ["ジョブ #1 が完了しました", "ジョブ #2 が失敗しました", "ジョブ #3 がキャンセルされました"]);
    assert.strictEqual(N.created[1].options.body, "Deploy\n状態: failed（失敗）");
    assert.strictEqual(N.created[1].options.tag, "qf-job-2:failed");

    assert.strictEqual(notify(ev(1, "done", "claimed")), null, "duplicate job_id+status");
    notify(ev(2, "done", "claimed"));
    assert.strictEqual(N.created.length, 4, "same job, other status still notifies");

    let prevented = false;
    N.created[1].onclick({ preventDefault: () => { prevented = true; } });
    assert.ok(prevented);
    assert.deepStrictEqual(calls, ["focus", "open 2"]);
    assert.strictEqual(N.created[1].closed, true);

    N.permission = "denied";
    assert.strictEqual(notify(ev(9, "done", "claimed")), null, "no permission");
    N.permission = "granted";
    assert.ok(notify(ev(9, "done", "claimed")), "an unshown event is not remembered");
  }
  {
    let errors = 0;
    function Throwing() { throw new TypeError("Illegal constructor"); }
    Throwing.permission = "granted";
    const notify = n.createJobNotifier({ getNotification: () => Throwing, onError: () => errors++ });
    assert.strictEqual(notify(ev(1, "done", "claimed")), null);
    assert.strictEqual(errors, 1);
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
    const log = { streams: [], messages: [], views: [], notified: [], listChanged: 0, unauthorized: 0 };
    const N = "notification" in opts ? opts.notification : fakeNotification("default", "granted");
    const env = { secure: true, credentials: true };
    const controller = n.createNotifyController({
      storage: { getItem: k => store.has(k) ? store.get(k) : null, setItem: (k, v) => store.set(k, v) },
      getNotification: () => N,
      isSecureContext: () => env.secure,
      hasCredentials: () => env.credentials,
      createStream: handlers => {
        const s = { handlers, running: false, start() { this.running = true; }, stop() { this.running = false; } };
        // Like the real stream, a fatal status ends the connection before reporting it.
        s.fatal = status => { s.running = false; handlers.onFatal(status); };
        log.streams.push(s);
        return s;
      },
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

    await f.controller.toggle();
    assert.strictEqual(f.N.prompts, 1);
    assert.strictEqual(f.store.get("qf-job-notify"), "on");
    assert.strictEqual(f.running(), 1);
    assert.strictEqual(f.view().enabled, true);
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

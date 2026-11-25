package ui

import (
	"os/exec"
	"strings"
	"testing"
)

func TestUIWebPushAssetsPresent(t *testing.T) {
	js := string(appJS)
	for _, token := range []string{
		`createPushController`,
		`getRegistration: notificationPresenter.register`,
		`navigator.serviceWorker.getRegistration("/ui/")`,
		`"push.labelRemoving"`,
		`"push.labelUnconfirmed"`,
		`isEnabled: function () { return notifyController.isEnabled(); }`,
		`void pushController.sync();`,
		`pushController.settled()`,
		`isIOS: function () {`,
		`t("push.iosNote")`,
		`navigator.maxTouchPoints`,
		`"push.labelActive"`,
		`"push.labelTabOnly"`,
	} {
		if !strings.Contains(js, token) {
			t.Errorf("app.js missing web push wiring %q", token)
		}
	}
	// docs/16: the iOS limit is explained in the UI.
	if !strings.Contains(string(indexHTML), `data-i18n-title="header.notifyTitle"`) {
		t.Errorf("index.html does not title the notification button")
	}
	for locale, want := range map[string]string{"ja": "ホーム画面に追加した PWA のみ、iOS 16.4 以降", "en": "iOS 16.4 or later"} {
		if got := dictionaries(t)[locale]["header.notifyTitle"]; !strings.Contains(got, want) {
			t.Errorf("%s notification title does not explain the iOS limit: %q", locale, got)
		}
	}
}

func TestUIPushControllerWithNode(t *testing.T) {
	nodeBin, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available in PATH; skipping JS web push test")
	}

	script := `
const assert = require("assert");
const n = require("./assets/notify.js");
const i18n = require("./assets/i18n.js");
const text = view => i18n.t(view.messageKey);

const tick = () => new Promise(r => setImmediate(r));
const flush = async () => { for (let i = 0; i < 20; i++) await tick(); };

const KEY = Buffer.from([4].concat(Array.from({ length: 64 }, (_, i) => (i * 7 + 250) % 256)));
const KEY_TEXT = KEY.toString("base64url");
const OTHER = Buffer.from(KEY); OTHER[10] ^= 1;
const buffer = b => b.buffer.slice(b.byteOffset, b.byteOffset + b.byteLength);

function fixture(opts) {
  const o = Object.assign({ enabled: true, supported: true, token: "sekret", existing: null }, opts);
  const log = { requests: [], subscribes: [], unsubscribed: [], views: [], timers: new Map() };
  const make = (endpoint, key) => ({
    endpoint,
    options: key === null ? undefined : { applicationServerKey: buffer(key) },
    toJSON: () => ({ endpoint, keys: { p256dh: "p", auth: "a" } }),
    unsubscribe: async () => {
      log.unsubscribed.push(endpoint);
      env.current = null;
      return true;
    }
  });
  const env = { enabled: o.enabled, supported: o.supported, token: o.token, current: null, counter: 0,
    status: { GET: 200, POST: 201, DELETE: 200 }, offline: false, subscribeError: null, hang: false,
    registration: null };
  if (o.existing) env.current = make("https://push.example/old", o.existing === "no-key" ? null : o.existing);
  const manager = {
    getSubscription: async () => env.current,
    subscribe: async options => {
      if (env.hang) return new Promise(() => {});
      if (env.subscribeError) throw env.subscribeError;
      log.subscribes.push({ userVisibleOnly: options.userVisibleOnly, key: Buffer.from(options.applicationServerKey) });
      env.current = make("https://push.example/new" + (++env.counter), Buffer.from(options.applicationServerKey));
      return env.current;
    }
  };
  env.registration = { pushManager: manager };
  let timer = 0;
  const controller = n.createPushController({
    isSupported: () => env.supported,
    getRegistration: async () => env.registration,
    findRegistration: async () => {
      if (env.findError) throw env.findError;
      return env.registration;
    },
    getToken: () => env.token,
    isEnabled: () => env.enabled,
    setTimeout: (fn, ms) => { log.timers.set(++timer, { fn, ms }); return timer; },
    clearTimeout: id => log.timers.delete(id),
    onChange: v => log.views.push(v.state),
    fetch: async (url, init) => {
      log.requests.push({ method: init.method, url, headers: init.headers,
        body: init.body === undefined ? undefined : JSON.parse(init.body) });
      if (env.offline) throw new TypeError("offline");
      // A list answers one call after another.
      const answer = env.status[init.method];
      const status = Array.isArray(answer) ? answer.shift() : answer;
      return { status, ok: status >= 200 && status < 300, json: async () => ({ public_key: KEY_TEXT }) };
    }
  });
  const calls = () => log.requests.map(r => r.method + " " + r.url);
  const reset = () => { log.requests.length = 0; log.subscribes.length = 0; log.unsubscribed.length = 0; log.views.length = 0; };
  return { controller, env, log, calls, reset, make };
}

const GET = "GET /push/vapid-public-key";
const POST = "POST /push/subscriptions";
const DELETE = "DELETE /push/subscriptions";

async function main() {
  {
    // Enabling subscribes with the server key and registers the subscription.
    const f = fixture({});
    assert.strictEqual(f.controller.view().state, "off");
    const view = await f.controller.sync();
    assert.strictEqual(view.state, "active");
    assert.deepStrictEqual(f.log.views, ["pending", "active"]);
    assert.deepStrictEqual(f.calls(), [GET, POST]);
    assert.strictEqual(f.log.subscribes.length, 1);
    assert.strictEqual(f.log.subscribes[0].userVisibleOnly, true);
    assert.deepStrictEqual(f.log.subscribes[0].key, KEY);
    assert.deepStrictEqual(f.log.requests[1].body, { endpoint: "https://push.example/new1", keys: { p256dh: "p", auth: "a" } });
    for (const r of f.log.requests) {
      assert.strictEqual(r.headers.Authorization, "Bearer sekret");
      assert.ok(!r.url.includes("sekret"));
    }
    assert.strictEqual(f.log.timers.size, 0, "no timer is left behind");

    // Later syncs reuse the subscription, register it again and do not flicker.
    f.reset();
    await f.controller.sync();
    assert.deepStrictEqual(f.log.views, ["active"]);
    assert.deepStrictEqual(f.calls(), [GET, POST]);
    assert.strictEqual(f.log.subscribes.length, 0);

    // Turning notifications off removes it on the server and in the browser.
    f.reset();
    f.env.enabled = false;
    assert.strictEqual((await f.controller.sync()).state, "off");
    assert.deepStrictEqual(f.calls(), [DELETE]);
    assert.deepStrictEqual(f.log.requests[0].body, { endpoint: "https://push.example/new1" });
    assert.deepStrictEqual(f.log.unsubscribed, ["https://push.example/new1"]);
    f.reset();
    await f.controller.sync();
    assert.deepStrictEqual(f.calls(), [], "nothing to remove");
  }
  {
    // The browser is unsubscribed even when the server cannot be told.
    for (const change of [e => { e.offline = true; }, e => { e.status.DELETE = 403; }, e => { e.token = ""; }]) {
      const f = fixture({ enabled: false, existing: KEY });
      change(f.env);
      assert.strictEqual((await f.controller.sync()).state, "off");
      assert.deepStrictEqual(f.log.unsubscribed, ["https://push.example/old"]);
    }
  }
  {
    // Off is not reported while neither the server nor the browser has confirmed it.
    const breakBrowser = {
      rejects: s => { s.unsubscribe = async () => { throw new Error("boom"); }; },
      "answers false": s => { s.unsubscribe = async () => false; }
    };
    const breakServer = {
      "503": e => { e.status.DELETE = 503; },
      "403": e => { e.status.DELETE = 403; },
      offline: e => { e.offline = true; },
      "no token": e => { e.token = ""; }
    };
    for (const [browserName, browser] of Object.entries(breakBrowser)) {
      for (const [serverName, server] of Object.entries(breakServer)) {
        const label = browserName + " / " + serverName;
        const f = fixture({ enabled: false, existing: KEY });
        const unsubscribe = f.env.current.unsubscribe;
        browser(f.env.current);
        server(f.env);
        const view = await f.controller.sync();
        assert.strictEqual(view.state, "unconfirmed", label);
        assert.ok(text(view).includes("解除を確認できません"), text(view));
        assert.deepStrictEqual(f.log.views, ["removing", "unconfirmed"], label);

        // A retry that still fails says so again; one that works ends in off.
        assert.strictEqual((await f.controller.sync()).state, "unconfirmed", label);
        f.env.current.unsubscribe = unsubscribe;
        assert.strictEqual((await f.controller.sync()).state, "off", label);
        assert.strictEqual(f.env.current, null, label);
      }
      // The server alone confirming is enough: it sends nothing any more.
      const f = fixture({ enabled: false, existing: KEY });
      browser(f.env.current);
      assert.strictEqual((await f.controller.sync()).state, "off", browserName);
      assert.deepStrictEqual(f.calls(), [DELETE]);
    }
    // false from a subscription that is already gone counts as unsubscribed.
    const gone = fixture({ enabled: false, existing: KEY });
    gone.env.status.DELETE = 503;
    gone.env.current.unsubscribe = async () => { gone.env.current = null; return false; };
    assert.strictEqual((await gone.controller.sync()).state, "off");

    // A subscription that cannot be looked up is not known to be gone.
    for (const breakLookup of [
      f => { f.env.registration.pushManager.getSubscription = async () => { throw new Error("boom"); }; },
      f => { f.env.findError = new Error("boom"); }]) {
      const f = fixture({ enabled: false, existing: KEY });
      breakLookup(f);
      assert.strictEqual((await f.controller.sync()).state, "unconfirmed");
    }
    // Without a registration, or without push in it, there is nothing to remove.
    for (const registration of [undefined, {}]) {
      const f = fixture({ enabled: false });
      f.env.registration = registration;
      assert.strictEqual((await f.controller.sync()).state, "off");
      assert.deepStrictEqual(f.log.views, ["off"]);
    }

    // An unsubscribe() that never settles times out as unconfirmed.
    const hung = fixture({ enabled: false, existing: KEY });
    hung.env.status.DELETE = 503;
    hung.env.current.unsubscribe = () => new Promise(() => {});
    const running = hung.controller.sync();
    await flush();
    assert.strictEqual(hung.controller.view().state, "removing");
    for (const [id, t] of hung.log.timers) { hung.log.timers.delete(id); t.fn(); }
    assert.strictEqual((await running).state, "unconfirmed");

    // Turning off an active subscription does not show off before it is confirmed.
    const active = fixture({});
    await active.controller.sync();
    active.reset();
    active.env.enabled = false;
    active.env.status.DELETE = 503;
    active.env.current.unsubscribe = async () => { throw new Error("boom"); };
    await active.controller.sync();
    assert.ok(!active.log.views.includes("off"), String(active.log.views));
    assert.strictEqual(active.controller.view().state, "unconfirmed");
  }
  {
    // A new server key pair replaces the subscription.
    const f = fixture({ existing: OTHER });
    assert.strictEqual((await f.controller.sync()).state, "active");
    assert.deepStrictEqual(f.calls(), [GET, DELETE, POST]);
    assert.deepStrictEqual(f.log.requests[1].body, { endpoint: "https://push.example/old" });
    assert.deepStrictEqual(f.log.unsubscribed, ["https://push.example/old"]);
    assert.strictEqual(f.log.subscribes.length, 1);
    // A matching one, or one that does not say its key, is kept.
    for (const existing of [KEY, "no-key"]) {
      const g = fixture({ existing });
      await g.controller.sync();
      assert.deepStrictEqual(g.calls(), [GET, POST]);
      assert.strictEqual(g.log.subscribes.length, 0);
    }
  }
  {
    // The server never takes back an endpoint it was told to delete (410): the
    // subscription is replaced and registered.
    const f = fixture({ existing: KEY });
    f.env.status.POST = [410, 201];
    assert.strictEqual((await f.controller.sync()).state, "active");
    assert.deepStrictEqual(f.calls(), [GET, POST, POST]);
    assert.deepStrictEqual(f.log.unsubscribed, ["https://push.example/old"]);
    assert.strictEqual(f.log.subscribes.length, 1);
    assert.deepStrictEqual(f.log.subscribes[0].key, KEY);
    assert.strictEqual(f.log.requests[2].body.endpoint, "https://push.example/new1");

    // It is tried once.
    const twice = fixture({ existing: KEY });
    twice.env.status.POST = [410, 410, 201];
    assert.strictEqual((await twice.controller.sync()).state, "error");
    assert.deepStrictEqual(twice.calls(), [GET, POST, POST]);

    // A subscription that cannot be removed is not replaced.
    const stuck = fixture({ existing: KEY });
    stuck.env.status.POST = [410];
    stuck.env.current.unsubscribe = async () => { throw new Error("boom"); };
    assert.strictEqual((await stuck.controller.sync()).state, "error");
    assert.deepStrictEqual(stuck.calls(), [GET, POST]);
    assert.strictEqual(stuck.log.subscribes.length, 0);

    // Another tab replaced it first: its new subscription is registered.
    const raced = fixture({ existing: KEY });
    raced.env.status.POST = [410, 200];
    raced.env.current.unsubscribe = async () => {
      raced.env.current = raced.make("https://push.example/other-tab", KEY);
      return false;
    };
    assert.strictEqual((await raced.controller.sync()).state, "active");
    assert.deepStrictEqual(raced.calls(), [GET, POST, POST]);
    assert.strictEqual(raced.log.requests[2].body.endpoint, "https://push.example/other-tab");
    assert.strictEqual(raced.log.subscribes.length, 0);

    // Turned off meanwhile: removed, and nothing new is made.
    const off = fixture({ existing: KEY });
    off.env.status.POST = [410];
    const unsubscribe = off.env.current.unsubscribe;
    off.env.current.unsubscribe = () => { off.env.enabled = false; return unsubscribe(); };
    assert.strictEqual((await off.controller.sync()).state, "off");
    assert.deepStrictEqual(off.calls(), [GET, POST]);
    assert.strictEqual(off.log.subscribes.length, 0);
    assert.strictEqual(off.env.current, null);
  }
  {
    // Fallbacks: nothing is thrown and the state says why only open tabs are notified.
    const unsupported = fixture({ supported: false });
    assert.strictEqual((await unsupported.controller.sync()).state, "unsupported");
    assert.deepStrictEqual(unsupported.calls(), []);
    unsupported.env.enabled = false;
    assert.strictEqual((await unsupported.controller.sync()).state, "off");

    for (const registration of [null, {}]) {
      const f = fixture({});
      f.env.registration = registration;
      assert.strictEqual((await f.controller.sync()).state, "unsupported");
      assert.deepStrictEqual(f.calls(), [GET]);
    }

    const anonymous = fixture({ token: "", existing: KEY });
    assert.strictEqual((await anonymous.controller.sync()).state, "no-token");
    assert.deepStrictEqual(anonymous.calls(), []);
    assert.deepStrictEqual(anonymous.log.unsubscribed, [], "kept until a token is entered");

    for (const [method, status, state] of [
      ["GET", 404, "server-off"], ["GET", 401, "unauthorized"], ["GET", 403, "forbidden"], ["GET", 500, "error"],
      ["POST", 403, "forbidden"], ["POST", 401, "unauthorized"], ["POST", 400, "error"]]) {
      const f = fixture({ existing: KEY });
      f.env.status[method] = status;
      const view = await f.controller.sync();
      assert.strictEqual(view.state, state, method + " " + status);
      for (const locale of ["ja", "en"]) {
        i18n.setLocale(locale, false);
        assert.ok(text(view).startsWith("Web Push: "), locale + " " + text(view));
      }
      i18n.setLocale("ja", false);
      assert.deepStrictEqual(f.log.unsubscribed, [], "an earlier registration is left alone");
    }

    const offline = fixture({});
    offline.env.offline = true;
    assert.strictEqual((await offline.controller.sync()).state, "error");
    offline.env.offline = false;
    assert.strictEqual((await offline.controller.sync()).state, "active", "the next sync recovers");

    const refused = fixture({});
    refused.env.subscribeError = new Error("push service not available");
    assert.strictEqual((await refused.controller.sync()).state, "error");
    assert.deepStrictEqual(refused.calls(), [GET]);
  }
  {
    // A subscribe() that never settles times out and does not stall later calls.
    const f = fixture({});
    f.env.hang = true;
    const first = f.controller.sync();
    await flush();
    assert.deepStrictEqual(Array.from(f.log.timers.values(), t => t.ms), [20000]);
    for (const [id, t] of f.log.timers) { f.log.timers.delete(id); t.fn(); }
    assert.strictEqual((await first).state, "error");
    f.env.hang = false;
    assert.strictEqual((await f.controller.sync()).state, "active");
  }
  {
    // Turning off while subscribing: nothing is registered and the queued sync cleans up.
    const f = fixture({});
    const manager = f.env.registration.pushManager;
    const get = manager.getSubscription;
    manager.getSubscription = async () => { f.env.enabled = false; return get(); };
    const running = f.controller.sync();
    await tick();
    const view = await f.controller.sync();
    assert.strictEqual((await running).state, "off");
    assert.strictEqual(view.state, "off");
    assert.deepStrictEqual(f.calls(), [GET]);
    assert.strictEqual(f.log.subscribes.length, 0);
  }
  {
    // Calls made before a run starts share it; one made during a run gets another, so
    // runs never overlap. settled() waits without adding one.
    const f = fixture({});
    const early = [f.controller.sync(), f.controller.sync()];
    assert.strictEqual(early[0], early[1]);
    await tick();
    const late = [f.controller.sync(), f.controller.sync()];
    assert.notStrictEqual(late[0], early[0]);
    assert.strictEqual(late[0], late[1]);
    assert.strictEqual((await f.controller.settled()).state, "active");
    assert.deepStrictEqual(f.calls(), [GET, POST, GET, POST]);
    assert.strictEqual(f.log.subscribes.length, 1);
    await f.controller.settled();
    assert.strictEqual(f.log.requests.length, 4);
  }
  {
    // iOS and iPadOS, which poses as a Mac with a touch screen.
    assert.strictEqual(n.isIOS("Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X)", 5), true);
    assert.strictEqual(n.isIOS("Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7)", 5), true);
    assert.strictEqual(n.isIOS("Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7)", 0), false);
    assert.strictEqual(n.isIOS("Mozilla/5.0 (Linux; Android 14)", 5), false);
    const note = i18n.t("push.iosNote");
    assert.ok(note.includes("ホーム画面") && note.includes("16.4"));

    // An iOS Safari tab has no Notification: the toggle explains what to do.
    const messages = [];
    const controller = n.createNotifyController({
      storage: { getItem: () => null, setItem: () => {} },
      getNotification: () => null,
      isSecureContext: () => true,
      isIOS: () => true,
      onMessage: (m, key) => messages.push([m, key])
    });
    await controller.toggle();
    assert.deepStrictEqual(messages, [[i18n.t("notify.unsupportedIOS"), "notify.unsupportedIOS"]]);
    assert.ok(messages[0][0].includes("ホーム画面"));
  }
  console.log("ALL_UI_PUSH_TESTS_PASSED");
}

main().catch(err => { console.error(err); process.exit(1); });
`
	out, err := exec.Command(nodeBin, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("node web push unit tests failed: %v\n%s", err, string(out))
	}
	// A hung promise would let node exit 0 early, so require the final marker.
	if !strings.Contains(string(out), "ALL_UI_PUSH_TESTS_PASSED") {
		t.Fatalf("unexpected node test output: %s", string(out))
	}
}

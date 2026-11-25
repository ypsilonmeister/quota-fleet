// Job completion browser notifications over server-sent events (docs/14) and the Web Push
// subscription (docs/16).
// Everything here takes its browser dependencies as arguments so it can be unit tested.
(function (root) {
  "use strict";

  // In the page the dictionary arrives with app.js, after this file has run, so it is
  // looked up each time a message is needed (docs/17).
  const i18n = {
    t: function (key, params) {
      return ((root && root.__qfI18n) || require("./i18n.js")).t(key, params);
    }
  };
  const NOTIFY_KEY = "qf-job-notify";
  // Message keys, looked up when shown so that they follow the current locale (docs/17).
  const TERMINAL_KEYS = {
    done: { title: "notify.titleDone", state: "notify.stateDone" },
    failed: { title: "notify.titleFailed", state: "notify.stateFailed" },
    cancelled: { title: "notify.titleCancelled", state: "notify.stateCancelled" }
  };
  const FATAL_KEYS = {
    401: "notify.fatalUnauthorized",
    403: "notify.fatalForbidden",
    404: "notify.fatalDisabled"
  };
  const NO_TOKEN_KEY = "notify.noToken";
  const MIN_RETRY_MS = 1000;
  const MAX_RETRY_MS = 30000;
  // The server sends a heartbeat every 25s; silence this long means a dead connection.
  const IDLE_TIMEOUT_MS = 60000;
  const SW_URL = "/ui/sw.js";
  const SW_SCOPE = "/ui/";
  // A worker that fails to activate leaves serviceWorker.ready pending forever.
  const SW_READY_TIMEOUT_MS = 10000;
  // A push service that cannot be reached may leave subscribe() pending.
  const PUSH_STEP_TIMEOUT_MS = 20000;
  // The Web Push message of each state.
  const PUSH_KEYS = {
    off: "push.off",
    removing: "push.removing",
    unconfirmed: "push.unconfirmed",
    pending: "push.pending",
    active: "push.active",
    unsupported: "push.unsupported",
    "no-token": "push.noToken",
    "server-off": "push.serverOff",
    unauthorized: "push.unauthorized",
    forbidden: "push.forbidden",
    error: "push.error"
  };

  function defaultSetTimeout(fn, ms) {
    return setTimeout(fn, ms);
  }

  function defaultClearTimeout(id) {
    clearTimeout(id);
  }

  function createSSEParser(onEvent) {
    let buffer = "";
    let eventName = "";
    let data = [];

    function processLine(line) {
      if (line === "") {
        if (data.length > 0) {
          onEvent(eventName || "message", data.join("\n"));
        }
        eventName = "";
        data = [];
        return;
      }
      if (line.charAt(0) === ":") {
        return;
      }
      const colon = line.indexOf(":");
      const field = colon < 0 ? line : line.slice(0, colon);
      let value = colon < 0 ? "" : line.slice(colon + 1);
      if (value.charAt(0) === " ") {
        value = value.slice(1);
      }
      if (field === "event") {
        eventName = value;
      } else if (field === "data") {
        data.push(value);
      }
    }

    return {
      push: function (chunk) {
        buffer += chunk;
        const lineBreak = /\r\n|\n|\r/;
        let match = lineBreak.exec(buffer);
        while (match) {
          // A trailing CR may be the first half of a CRLF split across chunks.
          if (match[0] === "\r" && match.index === buffer.length - 1) {
            break;
          }
          processLine(buffer.slice(0, match.index));
          buffer = buffer.slice(match.index + match[0].length);
          match = lineBreak.exec(buffer);
        }
      }
    };
  }

  // createEventStream reads an SSE endpoint with fetch (EventSource cannot send an
  // Authorization header) and reconnects with exponential backoff. 401, 403 and 404
  // end the stream through onFatal.
  function createEventStream(options) {
    const url = options.url || "/events";
    const setTimer = options.setTimeout || defaultSetTimeout;
    const clearTimer = options.clearTimeout || defaultClearTimeout;
    // generation changes on start/stop, which retires the previous connection loop.
    let generation = 0;
    let abort = null;
    let retryTimer = null;
    let retryDelay = MIN_RETRY_MS;

    async function connect(gen) {
      const controller = new AbortController();
      abort = controller;
      let idleTimer = null;
      function armIdleTimer() {
        clearTimer(idleTimer);
        idleTimer = setTimer(function () {
          controller.abort();
        }, IDLE_TIMEOUT_MS);
      }

      try {
        const headers = { Accept: "text/event-stream" };
        const token = options.getToken ? options.getToken() : "";
        if (token) {
          headers.Authorization = "Bearer " + token;
        }
        armIdleTimer();
        const resp = await options.fetch(url, {
          headers: headers,
          cache: "no-store",
          signal: controller.signal
        });
        if (gen !== generation) {
          return;
        }
        if (FATAL_KEYS[resp.status]) {
          generation += 1;
          options.onFatal(resp.status);
          return;
        }
        const contentType = resp.headers.get("Content-Type") || "";
        if (!resp.ok || !resp.body || contentType.indexOf("text/event-stream") !== 0) {
          throw new Error("event stream unavailable (HTTP " + resp.status + ")");
        }
        const parser = createSSEParser(function (name, data) {
          if (gen === generation) {
            options.onEvent(name, data);
          }
        });
        const reader = resp.body.getReader();
        const decoder = new TextDecoder();
        options.onOpen();
        for (;;) {
          const chunk = await reader.read();
          if (gen !== generation || chunk.done) {
            break;
          }
          retryDelay = MIN_RETRY_MS;
          armIdleTimer();
          parser.push(decoder.decode(chunk.value, { stream: true }));
        }
      } catch (_err) {
        // Network errors, aborts and bad responses all lead to a retry below.
      } finally {
        clearTimer(idleTimer);
        controller.abort();
      }

      if (gen !== generation) {
        return;
      }
      retryTimer = setTimer(function () {
        void connect(gen);
      }, retryDelay);
      retryDelay = Math.min(retryDelay * 2, MAX_RETRY_MS);
    }

    function stop() {
      generation += 1;
      clearTimer(retryTimer);
      if (abort) {
        abort.abort();
        abort = null;
      }
    }

    return {
      start: function () {
        stop();
        retryDelay = MIN_RETRY_MS;
        void connect(generation);
      },
      stop: stop
    };
  }

  // createNotificationPresenter displays notifications through the service worker
  // registration. new Notification() is used only where the browser has no service worker
  // API: Android Chrome rejects the constructor, and its "desktop site" User-Agent cannot
  // be told apart from desktop Linux, so a failing worker is not a reason to try it.
  function createNotificationPresenter(options) {
    const setTimer = options.setTimeout || defaultSetTimeout;
    const clearTimer = options.clearTimeout || defaultClearTimeout;
    const container = options.getServiceWorker();
    let pending = null;

    if (container) {
      // sw.js sends this after a notification click focused this tab.
      container.addEventListener("message", function (ev) {
        const data = ev.data;
        if (data && data.type === "qf-open-job" &&
            Number.isInteger(data.jobId) && data.jobId > 0) {
          // Without this answer sw.js loads the job URL in the tab instead.
          if (ev.ports && ev.ports[0]) {
            ev.ports[0].postMessage("ok");
          }
          options.openJob(data.jobId);
        }
      });
    }

    // activate registers the worker and resolves to the active registration. register()
    // waits behind an update that is still downloading its app shell (docs/15), so a
    // registration that is already active is used without waiting for it.
    async function activate() {
      // A failed lookup leaves the normal registration path.
      const existing = await Promise.resolve().then(function () {
        return container.getRegistration(SW_SCOPE);
      }).catch(function () { return null; });
      const registered = new Promise(function (done) {
        done(container.register(SW_URL, { scope: SW_SCOPE }));
      });
      if (existing && existing.active) {
        // The active worker serves even if this update check fails.
        registered.catch(function () {});
        return existing;
      }
      await registered;
      return container.ready;
    }

    // register resolves to the active registration, or null when there is none. Only a
    // success is remembered, so the next call tries again after a failure.
    function register() {
      if (!container) {
        return Promise.resolve(null);
      }
      if (!pending) {
        pending = new Promise(function (resolve) {
          const timer = setTimer(function () { finish(null); }, SW_READY_TIMEOUT_MS);
          function finish(registration) {
            clearTimer(timer);
            if (!registration) {
              pending = null;
            }
            resolve(registration);
          }
          activate().then(finish, function () {
            finish(null);
          });
        });
      }
      return pending;
    }

    // cacheShell asks the worker to complete the app shell cache (docs/15).
    async function cacheShell() {
      const registration = await register();
      if (registration && registration.active) {
        registration.active.postMessage({ type: "qf-cache-shell" });
      }
    }

    function showFromPage(message) {
      // Android Chrome exposes the constructor but throws when it is called, and calling
      // it is the only feature test, so an Android User-Agent rules it out as well.
      if (/Android/i.test(options.getUserAgent())) {
        throw new Error("page notifications are not supported");
      }
      const NotificationCtor = options.getNotification();
      const notification = new NotificationCtor(message.title, {
        body: message.body,
        tag: message.tag
      });
      notification.onclick = function (ev) {
        if (ev && ev.preventDefault) {
          ev.preventDefault();
        }
        options.focus();
        options.openJob(message.jobID);
        notification.close();
      };
    }

    // show resolves to false if notifications were turned off while it was waiting, and
    // rejects when the notification cannot be displayed.
    async function show(message) {
      const registration = await register();
      if (!options.isEnabled()) {
        return false;
      }
      if (!container) {
        showFromPage(message);
        return true;
      }
      if (!registration || typeof registration.showNotification !== "function") {
        throw new Error("service worker is not available");
      }
      try {
        await registration.showNotification(message.title, {
          body: message.body,
          tag: message.tag,
          data: { jobId: message.jobID }
        });
      } catch (err) {
        if (!options.isEnabled()) {
          return false;
        }
        throw err;
      }
      return true;
    }

    return {
      register: register,
      cacheShell: cacheShell,
      show: show
    };
  }

  // createJobNotifier turns job_status events into browser notifications: only
  // transitions into a terminal status, once per job_id and status. It returns null for
  // an ignored event, otherwise a promise of whether the notification was shown.
  function createJobNotifier(options) {
    const notified = new Set();

    return function notify(event) {
      if (!event || !Number.isInteger(event.job_id) || event.job_id <= 0) {
        return null;
      }
      if (!Object.prototype.hasOwnProperty.call(TERMINAL_KEYS, event.status) ||
          event.prev_status === event.status) {
        return null;
      }
      const NotificationCtor = options.getNotification();
      if (!NotificationCtor || NotificationCtor.permission !== "granted") {
        return null;
      }
      const key = event.job_id + ":" + event.status;
      if (notified.has(key)) {
        return null;
      }
      const keys = TERMINAL_KEYS[event.status];
      // Remembered before showing so that a repeat arriving meanwhile is suppressed.
      notified.add(key);
      return new Promise(function (resolve) {
        resolve(options.show({
          jobID: event.job_id,
          title: i18n.t(keys.title, { id: event.job_id }),
          body: i18n.t("notify.body", {
            title: event.title || i18n.t("common.untitled"),
            state: i18n.t(keys.state)
          }),
          // Tabs sharing an origin replace each other's notification instead of stacking.
          tag: "qf-job-" + key
        }));
      }).then(function (shown) {
        if (shown === false) {
          // Cancelled, so a later repeat of the event may still be shown.
          notified.delete(key);
          return false;
        }
        return true;
      }, function () {
        notified.delete(key);
        if (options.onError) {
          options.onError();
        }
        return false;
      });
    };
  }

  function readPreference(storage) {
    try {
      return storage.getItem(NOTIFY_KEY) === "on";
    } catch (_err) {
      return false;
    }
  }

  function writePreference(storage, enabled) {
    try {
      storage.setItem(NOTIFY_KEY, enabled ? "on" : "off");
    } catch (_err) {
      // Storage may be unavailable; the choice still applies to this page.
    }
  }

  // createNotifyController owns the on/off toggle (default off) and runs the event
  // stream only while notifications are enabled, permitted and credentials exist.
  function createNotifyController(options) {
    let wanted = readPreference(options.storage);
    let stream = null;
    let problemKey = "";

    // report sends the toggle state, with the problem both as text and as its key.
    function report() {
      options.onChange({
        enabled: isEnabled(),
        problem: problemKey ? i18n.t(problemKey) : "",
        problemKey: problemKey
      });
    }

    // unsupportedKey returns the message key of why notifications cannot work, or "".
    function unsupportedKey() {
      if (!options.isSecureContext()) {
        return "notify.insecureContext";
      }
      if (!options.getNotification()) {
        // iOS browsers offer notifications only to a web app added to the home screen.
        return options.isIOS && options.isIOS() ? "notify.unsupportedIOS" : "notify.unsupported";
      }
      return "";
    }

    function isEnabled() {
      return wanted && unsupportedKey() === "" &&
        options.getNotification().permission === "granted";
    }

    function stop() {
      if (stream) {
        stream.stop();
        stream = null;
      }
    }

    function handleEvent(name, data) {
      if (name !== "job_status") {
        return;
      }
      let event;
      try {
        event = JSON.parse(data);
      } catch (_err) {
        return;
      }
      options.notify(event);
      options.onListChanged();
    }

    function handleFatal(status) {
      stream = null;
      // A server without tokens serves the UI anonymously but rejects the stream. Asking
      // for a token there would keep the user from the rest of the UI.
      const noToken = status === 401 && !options.hasToken();
      problemKey = noToken ? NO_TOKEN_KEY : FATAL_KEYS[status];
      report();
      if (status === 401 && !noToken) {
        options.onUnauthorized(i18n.t(problemKey), problemKey);
      } else {
        message(problemKey);
      }
    }

    function message(key) {
      options.onMessage(i18n.t(key), key);
    }

    function sync() {
      if (isEnabled() && options.prepare) {
        // Ready the display path before the first event needs it.
        options.prepare();
      }
      const run = isEnabled() && problemKey === "" && options.hasCredentials();
      if (!run) {
        stop();
      } else if (!stream) {
        stream = options.createStream({
          onEvent: handleEvent,
          // Nothing is replayed, so every (re)connection reloads the list.
          onOpen: options.onListChanged,
          onFatal: handleFatal
        });
        stream.start();
      }
      report();
    }

    function restart() {
      stop();
      problemKey = "";
      sync();
    }

    function setWanted(next) {
      wanted = next;
      writePreference(options.storage, next);
      restart();
    }

    // toggle must be called from a user gesture: enabling may prompt for permission.
    async function toggle() {
      if (isEnabled()) {
        setWanted(false);
        return;
      }
      const reason = unsupportedKey();
      if (reason) {
        message(reason);
        return;
      }
      const NotificationCtor = options.getNotification();
      let permission = NotificationCtor.permission;
      if (permission === "default") {
        permission = await NotificationCtor.requestPermission();
      }
      if (permission !== "granted") {
        message("notify.denied");
        sync();
        return;
      }
      setWanted(true);
    }

    return {
      sync: sync,
      stop: stop,
      restart: restart,
      toggle: toggle,
      isEnabled: isEnabled,
      // reload picks up a preference changed by another tab.
      reload: function () {
        wanted = readPreference(options.storage);
        restart();
      }
    };
  }

  // isIOS also recognizes iPadOS, which calls itself a Macintosh but has a touch screen.
  function isIOS(userAgent, maxTouchPoints) {
    return /iPad|iPhone|iPod/.test(userAgent) ||
      (/Macintosh/.test(userAgent) && maxTouchPoints > 1);
  }

  function base64URLToBytes(text) {
    const base64 = text.replace(/-/g, "+").replace(/_/g, "/");
    const raw = atob(base64 + "=".repeat((4 - (base64.length % 4)) % 4));
    const bytes = new Uint8Array(raw.length);
    for (let i = 0; i < raw.length; i += 1) {
      bytes[i] = raw.charCodeAt(i);
    }
    return bytes;
  }

  // sameKey tells whether a subscription was made for the server's current key. A browser
  // that does not expose the key is given the benefit of the doubt.
  function sameKey(subscription, key) {
    const used = subscription.options && subscription.options.applicationServerKey;
    if (!used) {
      return true;
    }
    const bytes = new Uint8Array(used);
    return bytes.length === key.length && bytes.every(function (b, i) { return b === key[i]; });
  }

  // createPushController keeps this browser's Web Push subscription, here and on the
  // server, in step with the notification toggle (docs/16). It never turns notifications
  // off: whatever fails here, the event stream keeps working as before. It reports "off"
  // only when nothing can arrive any more: there is no subscription, or the server or the
  // browser confirmed removing it.
  function createPushController(options) {
    const setTimer = options.setTimeout || defaultSetTimeout;
    const clearTimer = options.clearTimeout || defaultClearTimeout;
    let chain = Promise.resolve();
    // The run that has been asked for but has not started yet.
    let waiting = null;
    // The dictionary is not loaded yet when this runs in the page, so the first view
    // carries the key only.
    let view = { state: "off", messageKey: PUSH_KEYS.off };

    function report(state) {
      view = { state: state, messageKey: PUSH_KEYS[state] };
      options.onChange(view);
      return view;
    }

    function within(promise) {
      return new Promise(function (resolve, reject) {
        const timer = setTimer(function () {
          reject(new Error("web push step timed out"));
        }, PUSH_STEP_TIMEOUT_MS);
        Promise.resolve(promise).then(function (value) {
          clearTimer(timer);
          resolve(value);
        }, function (err) {
          clearTimer(timer);
          reject(err);
        });
      });
    }

    function call(method, path, body) {
      const headers = { Accept: "application/json", Authorization: "Bearer " + options.getToken() };
      const init = { method: method, headers: headers, cache: "no-store" };
      if (body !== undefined) {
        headers["Content-Type"] = "application/json";
        init.body = JSON.stringify(body);
      }
      return within(options.fetch(path, init));
    }

    // forget removes the endpoint from the server and resolves to whether the server
    // confirmed it.
    async function forget(endpoint) {
      if (!options.getToken()) {
        return false;
      }
      try {
        return (await call("DELETE", "/push/subscriptions", { endpoint: endpoint })).ok;
      } catch (_err) {
        return false;
      }
    }

    // release unsubscribes in the browser and resolves to whether the subscription is
    // gone. If only the server fails to hear of it, the push service reports the endpoint
    // gone and the server deletes it then.
    async function release(manager, subscription) {
      try {
        if (await within(subscription.unsubscribe())) {
          return true;
        }
        // false also means that there was nothing left to unsubscribe.
        return !(await within(manager.getSubscription()));
      } catch (_err) {
        return false;
      }
    }

    async function pushManager() {
      if (!options.isSupported()) {
        return null;
      }
      const registration = await options.getRegistration();
      return (registration && registration.pushManager) || null;
    }

    // remove rejects, like its steps, when it cannot tell whether a subscription exists.
    async function remove() {
      if (!options.isSupported()) {
        return "off";
      }
      if (view.state !== "off") {
        report("removing");
      }
      // Looking up does not register the worker: without one there is no subscription.
      const registration = await within(options.findRegistration());
      const manager = registration && registration.pushManager;
      const subscription = manager && await within(manager.getSubscription());
      if (!subscription) {
        return "off";
      }
      if (view.state !== "removing") {
        report("removing");
      }
      const deleted = await forget(subscription.endpoint);
      const released = await release(manager, subscription);
      return deleted || released ? "off" : "unconfirmed";
    }

    function failure(status) {
      if (status === 401) {
        return "unauthorized";
      }
      if (status === 403) {
        return "forbidden";
      }
      return status === 404 ? "server-off" : "error";
    }

    async function ensure() {
      if (!options.isSupported()) {
        return "unsupported";
      }
      if (!options.getToken()) {
        return "no-token";
      }
      if (view.state !== "active") {
        report("pending");
      }
      const resp = await call("GET", "/push/vapid-public-key");
      if (!resp.ok) {
        return failure(resp.status);
      }
      const key = base64URLToBytes((await within(resp.json())).public_key);
      const manager = await pushManager();
      if (!manager) {
        return "unsupported";
      }
      let subscription = await within(manager.getSubscription());
      if (subscription && !sameKey(subscription, key)) {
        // The server has a new key pair; the old subscription can no longer be used.
        await forget(subscription.endpoint);
        await within(subscription.unsubscribe());
        subscription = null;
      }
      if (!options.isEnabled()) {
        return "off";
      }
      function subscribe() {
        return within(manager.subscribe({ userVisibleOnly: true, applicationServerKey: key }));
      }
      if (!subscription) {
        subscription = await subscribe();
      }
      // Sent every time: the server may have dropped it, and registering is idempotent.
      let saved = await call("POST", "/push/subscriptions", subscription.toJSON());
      if (saved.status === 410) {
        // The server was told to delete this endpoint and never takes it back, so that a
        // late registration cannot undo a removal. Only a new subscription can be used.
        await release(manager, subscription);
        if (!options.isEnabled()) {
          return "off";
        }
        // Another tab may have replaced it already.
        const current = await within(manager.getSubscription());
        if (current && current.endpoint === subscription.endpoint) {
          return "error";
        }
        saved = await call("POST", "/push/subscriptions", (current || await subscribe()).toJSON());
      }
      return saved.ok ? "active" : failure(saved.status);
    }

    // sync brings the subscription in line with the toggle and resolves to the resulting
    // view. Runs never overlap, and each reads the toggle and the token when its turn
    // comes, so calls made before then share it.
    function sync() {
      if (!waiting) {
        waiting = chain.then(async function () {
          waiting = null;
          const enabled = options.isEnabled();
          try {
            return report(await (enabled ? ensure() : remove()));
          } catch (_err) {
            return report(enabled ? "error" : "unconfirmed");
          }
        });
        chain = waiting;
      }
      return waiting;
    }

    return {
      sync: sync,
      // settled resolves to the view once the calls made so far are done.
      settled: function () { return chain.then(function () { return view; }); },
      view: function () { return view; }
    };
  }

  // createRefresher coalesces bursts of change signals into one refresh per delay.
  // refresh resolves to false when it could not run yet and should be retried.
  function createRefresher(options) {
    const setTimer = options.setTimeout || defaultSetTimeout;
    let scheduled = false;
    let running = false;

    function request(delayMs) {
      if (scheduled) {
        return;
      }
      scheduled = true;
      setTimer(function () {
        void fire();
      }, delayMs || options.delayMs);
    }

    async function fire() {
      scheduled = false;
      if (running) {
        request();
        return;
      }
      running = true;
      let settled = true;
      try {
        settled = (await options.refresh()) !== false;
      } catch (_err) {
        // The current view stays; the next change signal tries again.
      } finally {
        running = false;
      }
      if (!settled) {
        request(options.retryMs);
      }
    }

    return {
      request: function () {
        request();
      }
    };
  }

  const api = {
    NOTIFY_KEY: NOTIFY_KEY,
    createSSEParser: createSSEParser,
    createEventStream: createEventStream,
    createNotificationPresenter: createNotificationPresenter,
    createJobNotifier: createJobNotifier,
    createNotifyController: createNotifyController,
    createPushController: createPushController,
    isIOS: isIOS,
    createRefresher: createRefresher
  };

  if (typeof module !== "undefined" && module.exports) {
    module.exports = api;
  }
  if (root) {
    root.__qfNotify = api;
  }
})(typeof globalThis !== "undefined" ? globalThis : (typeof window !== "undefined" ? window : null));

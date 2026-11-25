// Job completion browser notifications over server-sent events (docs/14).
// Everything here takes its browser dependencies as arguments so it can be unit tested.
(function (root) {
  "use strict";

  const NOTIFY_KEY = "qf-job-notify";
  const TERMINAL_TEXT = {
    done: { headline: "完了しました", state: "done（完了）" },
    failed: { headline: "失敗しました", state: "failed（失敗）" },
    cancelled: { headline: "キャンセルされました", state: "cancelled（キャンセル）" }
  };
  const FATAL_MESSAGES = {
    401: "通知ストリームの認証に失敗しました（401）。",
    403: "通知ストリームには read スコープのトークンが必要です（403）。",
    404: "サーバー側でジョブ通知が無効です（fleet.yaml の api.job_events）。"
  };
  const NO_TOKEN_MESSAGE =
    "通知には read スコープの API トークンが必要です。トークンなしの接続では利用できません。";
  const MIN_RETRY_MS = 1000;
  const MAX_RETRY_MS = 30000;
  // The server sends a heartbeat every 25s; silence this long means a dead connection.
  const IDLE_TIMEOUT_MS = 60000;

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
        if (FATAL_MESSAGES[resp.status]) {
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

  // createJobNotifier turns job_status events into browser notifications: only
  // transitions into a terminal status, once per job_id and status.
  function createJobNotifier(options) {
    const notified = new Set();

    return function notify(event) {
      if (!event || !Number.isInteger(event.job_id) || event.job_id <= 0) {
        return null;
      }
      if (!Object.prototype.hasOwnProperty.call(TERMINAL_TEXT, event.status) ||
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
      const text = TERMINAL_TEXT[event.status];
      let notification;
      try {
        notification = new NotificationCtor("ジョブ #" + event.job_id + " が" + text.headline, {
          body: (event.title || "(無題)") + "\n状態: " + text.state,
          // Tabs sharing an origin replace each other's notification instead of stacking.
          tag: "qf-job-" + key
        });
      } catch (_err) {
        // Some browsers (e.g. Android Chrome) only allow service worker notifications.
        if (options.onError) {
          options.onError();
        }
        return null;
      }
      notified.add(key);
      notification.onclick = function (ev) {
        if (ev && ev.preventDefault) {
          ev.preventDefault();
        }
        options.focus();
        options.openJob(event.job_id);
        notification.close();
      };
      return notification;
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
    let problem = "";

    function unsupportedReason() {
      if (!options.isSecureContext()) {
        return "ブラウザ通知は HTTPS または localhost で開いた場合のみ利用できます。";
      }
      if (!options.getNotification()) {
        return "このブラウザは通知に対応していません。";
      }
      return "";
    }

    function isEnabled() {
      return wanted && unsupportedReason() === "" &&
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
      problem = noToken ? NO_TOKEN_MESSAGE : FATAL_MESSAGES[status];
      options.onChange({ enabled: isEnabled(), problem: problem });
      if (status === 401 && !noToken) {
        options.onUnauthorized(problem);
      } else {
        options.onMessage(problem);
      }
    }

    function sync() {
      const run = isEnabled() && problem === "" && options.hasCredentials();
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
      options.onChange({ enabled: isEnabled(), problem: problem });
    }

    function restart() {
      stop();
      problem = "";
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
      const reason = unsupportedReason();
      if (reason) {
        options.onMessage(reason);
        return;
      }
      const NotificationCtor = options.getNotification();
      let permission = NotificationCtor.permission;
      if (permission === "default") {
        permission = await NotificationCtor.requestPermission();
      }
      if (permission !== "granted") {
        options.onMessage("ブラウザで通知が許可されていないため有効にできません。ブラウザのサイト設定を確認してください。");
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
      // reload picks up a preference changed by another tab.
      reload: function () {
        wanted = readPreference(options.storage);
        restart();
      }
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
    createJobNotifier: createJobNotifier,
    createNotifyController: createNotifyController,
    createRefresher: createRefresher
  };

  if (typeof module !== "undefined" && module.exports) {
    module.exports = api;
  }
  if (root) {
    root.__qfNotify = api;
  }
})(typeof globalThis !== "undefined" ? globalThis : (typeof window !== "undefined" ? window : null));

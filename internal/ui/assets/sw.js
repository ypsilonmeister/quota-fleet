// Service worker for job completion notifications (docs/14) and the PWA app shell
// (docs/15). The page shows notifications through this registration because some browsers
// (e.g. Android Chrome) reject new Notification(). Only the app shell is cached, as an
// offline fallback; API responses, /events and /mcp are never intercepted.
"use strict";

// A tab waking from the background may take a moment to answer.
const ANSWER_TIMEOUT_MS = 2000;

// The server replaces the placeholder with a hash of the embedded app shell.
const SHELL_VERSION = "__QF_SHELL_VERSION__";
const SHELL_CACHE_PREFIX = "qf-shell-";
const SHELL_CACHE = SHELL_CACHE_PREFIX + SHELL_VERSION;
// Every UI page is the same index.html, cached once under this path.
const SHELL_PAGE = "/ui/";
const SHELL_ASSETS = [
  "/ui/assets/style.css",
  "/ui/assets/direction.js",
  "/ui/assets/notify.js",
  "/ui/assets/app.js",
  "/ui/manifest.webmanifest",
  "/ui/assets/icon-192.png",
  "/ui/assets/icon-512.png",
  "/ui/assets/icon-maskable-512.png",
  "/ui/assets/apple-touch-icon.png"
];

// isShellPage mirrors the page routes of the UI handler (embed.go).
function isShellPage(pathname) {
  return ["/ui/", "/ui/jobs", "/ui/gate/held", "/ui/gate/decisions", "/ui/deliveries", "/ui/agents"]
    .includes(pathname) || /^\/ui\/jobs\/[^/]+$/.test(pathname);
}

// shellCacheKey is the allowlist: it returns the cache entry that can stand in for the
// request, or null for everything the browser must handle without this worker.
function shellCacheKey(request) {
  if (request.method !== "GET") {
    return null;
  }
  const url = new URL(request.url);
  if (url.origin !== self.location.origin) {
    return null;
  }
  if (request.mode === "navigate") {
    return isShellPage(url.pathname) ? SHELL_PAGE : null;
  }
  return url.search === "" && SHELL_ASSETS.includes(url.pathname) ? url.pathname : null;
}

async function precacheShell() {
  const cache = await caches.open(SHELL_CACHE);
  // "reload" skips the HTTP cache. addAll stores all of the files or none of them.
  await cache.addAll([SHELL_PAGE].concat(SHELL_ASSETS).map(function (path) {
    return new Request(path, { cache: "reload" });
  }));
}

// The network comes first so that a deployed change shows up at once. The cache is only
// written during installation, which keeps it a consistent copy of one version.
async function fetchOrFallBack(request, key) {
  try {
    return await fetch(request);
  } catch (err) {
    const cache = await caches.open(SHELL_CACHE);
    const cached = await cache.match(key);
    if (cached) {
      return cached;
    }
    throw err;
  }
}

async function deleteOldShellCaches() {
  const names = await caches.keys();
  await Promise.all(names.filter(function (name) {
    return name.startsWith(SHELL_CACHE_PREFIX) && name !== SHELL_CACHE;
  }).map(function (name) {
    return caches.delete(name);
  }));
}

self.addEventListener("install", function (event) {
  event.waitUntil(precacheShell().catch(function (err) {
    // An update that cannot be cached is retried later while the active worker stays.
    // A first installation goes on without a cache: notifications need the worker.
    if (self.registration.active) {
      throw err;
    }
  }).then(function () {
    // Pages load all of their files up front, so a new version can take over at once.
    return self.skipWaiting();
  }));
});

self.addEventListener("activate", function (event) {
  event.waitUntil(deleteOldShellCaches().catch(function () {
    // A leftover cache is never read; it must not keep the tabs from being claimed.
  }).then(function () {
    // Take control of tabs that are already open: navigate() only works on controlled ones.
    return self.clients.claim();
  }));
});

self.addEventListener("fetch", function (event) {
  const key = shellCacheKey(event.request);
  // Without respondWith() the request goes to the network as if there were no worker.
  if (key !== null) {
    event.respondWith(fetchOrFallBack(event.request, key));
  }
});

// askPage asks a tab to show the job and resolves to whether it answered.
function askPage(client, jobID) {
  return new Promise(function (resolve) {
    const channel = new MessageChannel();
    const timer = setTimeout(function () { finish(false); }, ANSWER_TIMEOUT_MS);
    function finish(answered) {
      clearTimeout(timer);
      channel.port1.close();
      resolve(answered);
    }
    channel.port1.onmessage = function () { finish(true); };
    client.postMessage({ type: "qf-open-job", jobId: jobID }, [channel.port2]);
  });
}

// openJob brings an open UI tab to the front and asks it to show the job, which keeps the
// tab's state. A tab that does not answer (e.g. loaded before pages could receive this) is
// navigated to the job instead. That needs no second user activation, unlike openWindow()
// after focus(), and keeps the tab's sessionStorage. A new window is opened when there is
// no tab that can be focused.
async function openJob(jobID) {
  const url = new URL("/ui/jobs/" + jobID, self.location.origin).href;
  const windows = await self.clients.matchAll({ type: "window", includeUncontrolled: true });
  const pages = windows.filter(function (client) {
    return new URL(client.url).pathname.startsWith("/ui/");
  });
  const target = pages.find(function (client) { return client.url === url; }) ||
    pages.find(function (client) { return client.focused; }) ||
    pages.find(function (client) { return client.visibilityState === "visible"; }) ||
    pages[0];
  if (target) {
    try {
      const focused = (await target.focus()) || target;
      if (!(await askPage(focused, jobID))) {
        await focused.navigate(url);
      }
      return;
    } catch (_err) {
      // The tab may be gone or not controllable; fall through to a new window.
    }
  }
  await self.clients.openWindow(url);
}

self.addEventListener("notificationclick", function (event) {
  event.notification.close();
  const data = event.notification.data;
  const jobID = data ? data.jobId : null;
  if (!Number.isInteger(jobID) || jobID <= 0) {
    return;
  }
  event.waitUntil(openJob(jobID));
});

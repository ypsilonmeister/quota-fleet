// Service worker for job completion notifications (docs/14). The page shows them through
// this registration because some browsers (e.g. Android Chrome) reject new Notification().
// There is deliberately no fetch handler: nothing is cached or intercepted.
"use strict";

// A tab waking from the background may take a moment to answer.
const ANSWER_TIMEOUT_MS = 2000;

self.addEventListener("install", function () {
  // With nothing cached, a new version can replace the old one at once.
  self.skipWaiting();
});

self.addEventListener("activate", function (event) {
  // Take control of tabs that are already open: navigate() only works on controlled ones.
  event.waitUntil(self.clients.claim());
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

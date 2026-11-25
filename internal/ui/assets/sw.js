// Service worker for job completion notifications (docs/14). The page shows them through
// this registration because some browsers (e.g. Android Chrome) reject new Notification().
// There is deliberately no fetch handler: nothing is cached or intercepted.
"use strict";

self.addEventListener("install", function () {
  // With nothing cached, a new version can replace the old one at once.
  self.skipWaiting();
});

// openJob brings an open UI tab to the front and asks it to show the job, which keeps the
// tab's state. A new window is opened when there is no tab that can be focused.
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
      const focused = await target.focus();
      (focused || target).postMessage({ type: "qf-open-job", jobId: jobID });
      return;
    } catch (_err) {
      // The tab may be gone; fall through to a new window.
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

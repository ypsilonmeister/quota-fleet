(function (root) {
  "use strict";

  const DIRECTION_KEY = "qf-direction";
  const DEFAULT_DIRECTION = "console";
  const VALID_DIRECTIONS = new Set(["console", "ledger"]);
  // Each direction's --chrome-bg (style.css), for the browser and app window chrome.
  const THEME_COLORS = { console: "#0c0d10", ledger: "#efe9dc" };

  function detectPreferredDirection(matchMediaFn) {
    const mm = typeof matchMediaFn === "function"
      ? matchMediaFn
      : (typeof window !== "undefined" && typeof window.matchMedia === "function"
        ? window.matchMedia.bind(window)
        : null);
    if (!mm) {
      return DEFAULT_DIRECTION;
    }
    try {
      if (mm("(prefers-color-scheme: dark)").matches) {
        return "console";
      }
      if (mm("(prefers-color-scheme: light)").matches) {
        return "ledger";
      }
    } catch (_err) {
      // Fall through to default direction.
    }
    return DEFAULT_DIRECTION;
  }

  function resolveDirection(value, matchMediaFn) {
    if (VALID_DIRECTIONS.has(value)) {
      return value;
    }
    return detectPreferredDirection(matchMediaFn);
  }

  function readStoredDirection() {
    try {
      return resolveDirection(localStorage.getItem(DIRECTION_KEY));
    } catch (_err) {
      return resolveDirection(null);
    }
  }

  function syncDirectionButtons(dir) {
    if (typeof document === "undefined") {
      return false;
    }
    const container = document.getElementById("direction-switch");
    if (!container) {
      return false;
    }
    const buttons = container.querySelectorAll("[data-set-direction]");
    if (buttons.length < 2) {
      return false;
    }
    for (const btn of buttons) {
      const active = btn.getAttribute("data-set-direction") === dir;
      btn.classList.toggle("active", active);
      btn.setAttribute("aria-pressed", active ? "true" : "false");
    }
    return true;
  }

  function applyDirection(dir, persist) {
    const next = resolveDirection(dir);
    if (typeof document !== "undefined") {
      document.documentElement.setAttribute("data-direction", next);
      const schemeMeta = document.querySelector('meta[name="color-scheme"]');
      if (schemeMeta) {
        schemeMeta.setAttribute("content", next === "ledger" ? "light" : "dark");
      }
      const themeMeta = document.querySelector('meta[name="theme-color"]');
      if (themeMeta) {
        themeMeta.setAttribute("content", THEME_COLORS[next]);
      }
      syncDirectionButtons(next);
    }
    if (persist) {
      try {
        localStorage.setItem(DIRECTION_KEY, next);
      } catch (_err) {
        // Storage may be unavailable; direction still applies for this session.
      }
    }
    return next;
  }

  const api = {
    DIRECTION_KEY: DIRECTION_KEY,
    DEFAULT_DIRECTION: DEFAULT_DIRECTION,
    detectPreferredDirection: detectPreferredDirection,
    resolveDirection: resolveDirection,
    readStoredDirection: readStoredDirection,
    syncDirectionButtons: syncDirectionButtons,
    applyDirection: applyDirection
  };

  if (typeof module !== "undefined" && module.exports) {
    module.exports = api;
  }
  if (root) {
    root.__qfDirection = api;
  }

  if (typeof document !== "undefined" && typeof window !== "undefined") {
    const initial = applyDirection(readStoredDirection(), false);
    if (!syncDirectionButtons(initial) && typeof MutationObserver === "function") {
      const observer = new MutationObserver(function () {
        const current = document.documentElement.getAttribute("data-direction") || initial;
        if (syncDirectionButtons(current)) {
          observer.disconnect();
        }
      });
      observer.observe(document.documentElement, { childList: true, subtree: true });
      document.addEventListener("DOMContentLoaded", function () {
        const current = document.documentElement.getAttribute("data-direction") || initial;
        syncDirectionButtons(current);
        observer.disconnect();
      }, { once: true });
    }
  }
})(typeof globalThis !== "undefined" ? globalThis : (typeof window !== "undefined" ? window : null));

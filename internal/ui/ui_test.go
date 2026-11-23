package ui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUIShellRoutesReturn200WithStrictCSP(t *testing.T) {
	h := NewHandler()

	routes := []string{
		"/ui/",
		"/ui/jobs",
		"/ui/jobs/new",
		"/ui/jobs/42",
		"/ui/gate/held",
		"/ui/gate/decisions",
		"/ui/deliveries",
	}

	for _, path := range routes {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s status = %d, want 200", path, rec.Code)
			}
			if got := rec.Header().Get("Content-Security-Policy"); got != ContentSecurityPolicy {
				t.Errorf("GET %s Content-Security-Policy = %q, want %q", path, got, ContentSecurityPolicy)
			}
			if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
				t.Errorf("GET %s Content-Type = %q, want text/html", path, ct)
			}
			body := rec.Body.String()
			if !strings.Contains(body, `src="/ui/assets/app.js"`) {
				t.Errorf("GET %s body missing app.js reference", path)
			}
			if !strings.Contains(body, `href="/ui/assets/style.css"`) {
				t.Errorf("GET %s body missing style.css reference", path)
			}
		})
	}
}

func TestUIRedirectAndAssetsAndNotFound(t *testing.T) {
	h := NewHandler()

	// /ui redirects to /ui/
	req := httptest.NewRequest(http.MethodGet, "/ui", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("GET /ui status = %d, want 302", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/ui/" {
		t.Errorf("GET /ui Location = %q, want /ui/", loc)
	}

	// Embedded CSS and JS assets
	for _, tc := range []struct {
		path   string
		wantCT string
	}{
		{path: "/ui/assets/style.css", wantCT: "text/css"},
		{path: "/ui/assets/app.js", wantCT: "application/javascript"},
	} {
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d, want 200", tc.path, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, tc.wantCT) {
			t.Errorf("GET %s Content-Type = %q, want prefix %q", tc.path, ct, tc.wantCT)
		}
		if got := rec.Header().Get("Content-Security-Policy"); got != ContentSecurityPolicy {
			t.Errorf("GET %s CSP = %q, want %q", tc.path, got, ContentSecurityPolicy)
		}
		if rec.Body.Len() == 0 {
			t.Errorf("GET %s returned empty body", tc.path)
		}
	}

	// Unknown route under /ui/ returns 404
	req = httptest.NewRequest(http.MethodGet, "/ui/does-not-exist", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET /ui/does-not-exist status = %d, want 404", rec.Code)
	}
}

func TestUIShellAndAssetsFollowSecurityRules(t *testing.T) {
	html := string(indexHTML)
	if strings.Contains(html, "http://") || strings.Contains(html, "https://") {
		t.Errorf("index.html must not reference external resources")
	}
	if strings.Contains(html, "<style") || strings.Contains(html, "style=") {
		t.Errorf("index.html must not contain inline styles")
	}

	css := string(styleCSS)
	if strings.Contains(css, "http://") || strings.Contains(css, "https://") {
		t.Errorf("style.css must not reference external resources")
	}

	js := string(appJS)
	for _, forbidden := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write"} {
		if strings.Contains(js, forbidden) {
			t.Errorf("app.js must not use %q (XSS prevention requires textContent)", forbidden)
		}
	}
	if strings.Contains(js, "http://") || strings.Contains(js, "https://") {
		t.Errorf("app.js must not reference external URLs")
	}
}

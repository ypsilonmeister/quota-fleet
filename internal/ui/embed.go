package ui

import (
	"embed"
	"net/http"
)

//go:embed index.html assets/style.css assets/direction.js assets/notify.js assets/app.js assets/sw.js
var assetsFS embed.FS

// ContentSecurityPolicy is the strict CSP header attached to UI shell and static asset responses.
const ContentSecurityPolicy = "default-src 'self'"

var (
	indexHTML   []byte
	styleCSS    []byte
	directionJS []byte
	notifyJS    []byte
	appJS       []byte
	swJS        []byte
)

func init() {
	var err error
	if indexHTML, err = assetsFS.ReadFile("index.html"); err != nil {
		panic("ui: read embedded index.html: " + err.Error())
	}
	if styleCSS, err = assetsFS.ReadFile("assets/style.css"); err != nil {
		panic("ui: read embedded assets/style.css: " + err.Error())
	}
	if directionJS, err = assetsFS.ReadFile("assets/direction.js"); err != nil {
		panic("ui: read embedded assets/direction.js: " + err.Error())
	}
	if notifyJS, err = assetsFS.ReadFile("assets/notify.js"); err != nil {
		panic("ui: read embedded assets/notify.js: " + err.Error())
	}
	if appJS, err = assetsFS.ReadFile("assets/app.js"); err != nil {
		panic("ui: read embedded assets/app.js: " + err.Error())
	}
	if swJS, err = assetsFS.ReadFile("assets/sw.js"); err != nil {
		panic("ui: read embedded assets/sw.js: " + err.Error())
	}
}

// NewHandler returns an http.Handler that serves the embedded CSR UI shell and
// static assets under /ui/. No job or configuration data is ever embedded in
// responses served by this handler.
func NewHandler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /ui", func(w http.ResponseWriter, r *http.Request) {
		setCommonHeaders(w)
		http.Redirect(w, r, "/ui/", http.StatusFound)
	})

	serveShell := func(w http.ResponseWriter, r *http.Request) {
		setCommonHeaders(w)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(indexHTML)
	}

	mux.HandleFunc("GET /ui/{$}", serveShell)
	mux.HandleFunc("GET /ui/jobs", serveShell)
	mux.HandleFunc("GET /ui/jobs/new", serveShell)
	mux.HandleFunc("GET /ui/jobs/{id}", serveShell)
	mux.HandleFunc("GET /ui/gate/held", serveShell)
	mux.HandleFunc("GET /ui/gate/decisions", serveShell)
	mux.HandleFunc("GET /ui/deliveries", serveShell)
	mux.HandleFunc("GET /ui/agents", serveShell)

	// The service worker is served from /ui/ rather than /ui/assets/ because its path
	// limits its scope, and it has to cover the UI pages.
	mux.HandleFunc("GET /ui/sw.js", func(w http.ResponseWriter, r *http.Request) {
		setCommonHeaders(w)
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(swJS)
	})

	mux.HandleFunc("GET /ui/assets/style.css", func(w http.ResponseWriter, r *http.Request) {
		setCommonHeaders(w)
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(styleCSS)
	})

	mux.HandleFunc("GET /ui/assets/direction.js", func(w http.ResponseWriter, r *http.Request) {
		setCommonHeaders(w)
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(directionJS)
	})

	mux.HandleFunc("GET /ui/assets/notify.js", func(w http.ResponseWriter, r *http.Request) {
		setCommonHeaders(w)
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(notifyJS)
	})

	mux.HandleFunc("GET /ui/assets/app.js", func(w http.ResponseWriter, r *http.Request) {
		setCommonHeaders(w)
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(appJS)
	})

	return mux
}

func setCommonHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Security-Policy", ContentSecurityPolicy)
	w.Header().Set("X-Content-Type-Options", "nosniff")
}

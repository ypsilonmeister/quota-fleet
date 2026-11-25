package ui

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"net/http"
)

//go:generate go run gen_icons.go

//go:embed index.html assets/style.css assets/direction.js assets/i18n.js assets/notify.js assets/app.js assets/sw.js
//go:embed assets/manifest.webmanifest assets/icon-192.png assets/icon-512.png
//go:embed assets/icon-maskable-512.png assets/apple-touch-icon.png
var assetsFS embed.FS

// ContentSecurityPolicy is the strict CSP header attached to UI shell and static asset responses.
const ContentSecurityPolicy = "default-src 'self'"

// shellVersionPlaceholder in sw.js is replaced by the version of the embedded app shell.
const shellVersionPlaceholder = "__QF_SHELL_VERSION__"

// shellAsset is a static file of the app shell served under /ui/.
type shellAsset struct {
	path string
	// files are the embedded files served under path, joined in this order.
	files       []string
	contentType string
}

// body returns what is served for the asset. The separator keeps one script from
// running into the next.
func (a shellAsset) body() []byte {
	parts := make([][]byte, len(a.files))
	for i, file := range a.files {
		parts[i] = mustRead(file)
	}
	return bytes.Join(parts, []byte(";\n"))
}

// shellAssets lists the static files of the app shell besides index.html. sw.js
// precaches the same paths (SHELL_ASSETS), which a test keeps in step.
var shellAssets = []shellAsset{
	{"/ui/assets/style.css", []string{"assets/style.css"}, "text/css; charset=utf-8"},
	{"/ui/assets/direction.js", []string{"assets/direction.js"}, "application/javascript; charset=utf-8"},
	{"/ui/assets/notify.js", []string{"assets/notify.js"}, "application/javascript; charset=utf-8"},
	// The label dictionary travels with the application, so that a page cannot end up
	// with one but not the other (docs/17).
	{"/ui/assets/app.js", []string{"assets/i18n.js", "assets/app.js"}, "application/javascript; charset=utf-8"},
	{"/ui/manifest.webmanifest", []string{"assets/manifest.webmanifest"}, "application/manifest+json"},
	{"/ui/assets/icon-192.png", []string{"assets/icon-192.png"}, "image/png"},
	{"/ui/assets/icon-512.png", []string{"assets/icon-512.png"}, "image/png"},
	{"/ui/assets/icon-maskable-512.png", []string{"assets/icon-maskable-512.png"}, "image/png"},
	{"/ui/assets/apple-touch-icon.png", []string{"assets/apple-touch-icon.png"}, "image/png"},
}

var (
	indexHTML    []byte
	styleCSS     []byte
	directionJS  []byte
	i18nJS       []byte
	notifyJS     []byte
	appJS        []byte
	swJS         []byte
	manifestJSON []byte
)

func init() {
	indexHTML = mustRead("index.html")
	styleCSS = mustRead("assets/style.css")
	directionJS = mustRead("assets/direction.js")
	i18nJS = mustRead("assets/i18n.js")
	notifyJS = mustRead("assets/notify.js")
	appJS = mustRead("assets/app.js")
	manifestJSON = mustRead("assets/manifest.webmanifest")

	swSource := mustRead("assets/sw.js")
	if !bytes.Contains(swSource, []byte(shellVersionPlaceholder)) {
		panic("ui: assets/sw.js has no shell version placeholder")
	}
	swJS = bytes.Replace(swSource, []byte(shellVersionPlaceholder), []byte(shellVersion(swSource)), 1)
}

func mustRead(file string) []byte {
	data, err := assetsFS.ReadFile(file)
	if err != nil {
		panic("ui: read embedded " + file + ": " + err.Error())
	}
	return data
}

// shellVersion hashes everything a cached app shell consists of, so that any change to it
// changes the bytes of /ui/sw.js and makes browsers install the new worker. The CSP is
// included because cached responses keep their headers.
func shellVersion(swSource []byte) string {
	h := sha256.New()
	add := func(name string, data []byte) {
		// The length keeps adjacent entries from running into each other.
		fmt.Fprintf(h, "%s\x00%d\x00", name, len(data))
		h.Write(data)
	}
	add("csp", []byte(ContentSecurityPolicy))
	add("sw.js", swSource)
	add("index.html", mustRead("index.html"))
	for _, asset := range shellAssets {
		add(asset.path, asset.body())
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
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

	// sw.js treats the same paths as app shell navigations (isShellPage).
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

	for _, asset := range shellAssets {
		body := asset.body()
		mux.HandleFunc("GET "+asset.path, func(w http.ResponseWriter, r *http.Request) {
			setCommonHeaders(w)
			w.Header().Set("Content-Type", asset.contentType)
			w.Header().Set("Cache-Control", "no-cache")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body)
		})
	}

	return mux
}

func setCommonHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Security-Policy", ContentSecurityPolicy)
	w.Header().Set("X-Content-Type-Options", "nosniff")
}

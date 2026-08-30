package gocommerce

import (
	"errors"
	"io/fs"
	"net/http"
	"path"
	"strings"

	"github.com/misiki/gocommerce/admin"
)

// AdminPanelPath is where the admin panel is served.
//
// The root: the whole API lives under /api (plus /health, /doc, and a module's
// /x/), so nothing else wants this URL. Typing the store's address gets you the
// dashboard, which is what a person expects.
//
// The consequence, stated plainly: this binary cannot also host a storefront at
// the root. A headless store's storefront is a separate application anyway —
// put it on its own origin, or in front of this one behind a proxy.
const AdminPanelPath = "/"

// apiPrefixes are the paths the API owns. A request under one of these that
// matched no route is a missing endpoint, and must get the JSON error every
// other endpoint gives — not the panel's HTML.
//
// Without this, the root catch-all below would answer GET /api/typo with an
// index.html, and a client decoding JSON would report a syntax error instead of
// "no route for GET /api/typo".
var apiPrefixes = []string{"/api", "/health", "/doc", "/docs", "/x"}

func isAPIPath(p string) bool {
	for _, prefix := range apiPrefixes {
		if p == prefix || strings.HasPrefix(p, prefix+"/") {
			return true
		}
	}
	return false
}

// mountAdminPanel serves the embedded admin panel, if this binary has one.
//
// A build with -tags no_admin has no panel and mounts nothing, so an API-only
// deployment does not carry a megabyte of JavaScript it will never serve — and
// the root stays free.
func (a *App) mountAdminPanel() {
	if admin.DistFS == nil {
		a.log.Debug("built without the admin panel; the root is not served")
		return
	}

	// The panel used to live at /_/, matching PocketBase. Keep those URLs
	// working rather than breaking a bookmark.
	redirect := func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, AdminPanelPath, http.StatusMovedPermanently)
	}
	a.handleUI("GET /_", redirect)
	a.handleUI("GET /_/{path...}", redirect)

	// A catch-all. Go's ServeMux prefers the most specific pattern, so every
	// real API route still wins over this one.
	a.handleUI("GET /{path...}", a.handleAdminPanel)

	a.log.Info("admin panel available", "path", AdminPanelPath)
}

// handleUI mounts a route that serves the panel's own files, marking it so the
// OpenAPI coverage check knows it is not an API endpoint.
func (a *App) handleUI(pattern string, h http.HandlerFunc) {
	a.HandleFunc(pattern, h)
	if n := len(a.routes); n > 0 {
		a.routes[n-1].UI = true
	}
}

func (a *App) handleAdminPanel(w http.ResponseWriter, r *http.Request) {
	requested := path.Clean("/" + strings.TrimPrefix(r.URL.Path, "/"))

	// An unmatched API path is a missing endpoint, not a client-side route.
	if isAPIPath(requested) {
		RespondError(w, r, NotFoundf(
			"no route for %s %s — see /docs for what this store serves", r.Method, r.URL.Path))
		return
	}

	name := strings.TrimPrefix(requested, "/")
	if name == "" || name == "." {
		name = "index.html"
	}

	file, err := admin.DistFS.Open(name)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			RespondError(w, r, Internalf(err, "could not read the admin panel"))
			return
		}
		// Single-page app: an unknown path is a client-side route, so hand
		// back index.html and let the panel route it. A missing *asset* is a
		// build problem, and answering those with HTML would hide it — so only
		// extension-less paths fall through.
		if path.Ext(name) != "" {
			RespondError(w, r, NotFoundf("no such file in the admin panel: %s", name))
			return
		}
		name = "index.html"
		file, err = admin.DistFS.Open(name)
		if err != nil {
			RespondError(w, r, Internalf(err, "the admin panel is missing its index.html"))
			return
		}
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil || info.IsDir() {
		file.Close()
		name = "index.html"
		file, err = admin.DistFS.Open(name)
		if err != nil {
			RespondError(w, r, NotFoundf("not found"))
			return
		}
		defer file.Close()
		info, err = file.Stat()
		if err != nil {
			RespondError(w, r, Internalf(err, "could not read the admin panel"))
			return
		}
	}

	setAdminCacheHeaders(w, name)

	// Lock the panel to its own origin so a compromised dependency could not
	// exfiltrate an admin token.
	//
	// script-src permits inline here because SvelteKit emits one inline
	// bootstrap script that cannot be removed. That is not the end of the
	// story: the page also carries a <meta> CSP listing the SHA-256 of that
	// exact script (see admin/svelte.config.js). Both policies are enforced
	// and a script must satisfy both, so the hash is what actually decides —
	// an injected inline script is refused by the meta policy even though this
	// header would have allowed it.
	//
	// frame-ancestors lives here rather than in the meta, because meta CSP
	// ignores it.
	w.Header().Set("Content-Security-Policy",
		"default-src 'self'; script-src 'self' 'unsafe-inline'; "+
			"style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; "+
			"font-src 'self'; connect-src 'self'; base-uri 'self'; "+
			"object-src 'none'; frame-ancestors 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "same-origin")

	readSeeker, ok := file.(interface {
		Read([]byte) (int, error)
		Seek(int64, int) (int64, error)
	})
	if !ok {
		http.Error(w, "unreadable asset", http.StatusInternalServerError)
		return
	}
	http.ServeContent(w, r, info.Name(), info.ModTime(), readSeeker)
}

// setAdminCacheHeaders caches fingerprinted assets hard and the shell not at
// all. SvelteKit hashes everything under _app/immutable, so those filenames
// change whenever their contents do; index.html must never be cached or a
// deploy would not reach anyone.
func setAdminCacheHeaders(w http.ResponseWriter, name string) {
	switch {
	case strings.HasPrefix(name, "_app/immutable/"):
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	case strings.HasPrefix(name, "fonts/"):
		w.Header().Set("Cache-Control", "public, max-age=1209600")
	case name == "index.html":
		w.Header().Set("Cache-Control", "no-cache")
	default:
		w.Header().Set("Cache-Control", "public, max-age=3600")
	}
}

// HasAdminPanel reports whether this binary was built with the panel.
func HasAdminPanel() bool { return admin.DistFS != nil }

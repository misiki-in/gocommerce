package gocommerce

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"testing"
)

// TestAdminPanelIsServed checks the panel is reachable from the same binary as
// the API — the whole point of embedding it.
func TestAdminPanelIsServed(t *testing.T) {
	if !HasAdminPanel() {
		t.Skip("built with -tags no_admin")
	}
	app := newTestApp(t)

	t.Run("the root is the panel", func(t *testing.T) {
		rec := do(t, app, http.MethodGet, "/")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
			t.Errorf("Content-Type = %q, want HTML", ct)
		}
		if !strings.Contains(rec.Body.String(), "_app/immutable") {
			t.Errorf("the response does not look like the built panel: %.200s", rec.Body)
		}
	})

	t.Run("client-side routes fall back to the shell", func(t *testing.T) {
		// The panel routes itself, so a deep link must return index.html or a
		// refresh on /orders would break.
		for _, path := range []string{"/orders", "/products", "/inventory", "/settings", "/data"} {
			rec := do(t, app, http.MethodGet, path)
			if rec.Code != http.StatusOK {
				t.Errorf("GET %s = %d, want 200 (SPA fallback)", path, rec.Code)
			}
		}
	})

	t.Run("the old /_/ location still redirects", func(t *testing.T) {
		for _, path := range []string{"/_", "/_/", "/_/orders"} {
			rec := do(t, app, http.MethodGet, path)
			if rec.Code != http.StatusMovedPermanently {
				t.Errorf("GET %s = %d, want 301", path, rec.Code)
			}
			if loc := rec.Header().Get("Location"); loc != AdminPanelPath {
				t.Errorf("GET %s Location = %q, want %q", path, loc, AdminPanelPath)
			}
		}
	})

	t.Run("assets are served", func(t *testing.T) {
		rec := do(t, app, http.MethodGet, "/fonts/remixicon/remixicon.woff2")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if rec.Body.Len() < 1000 {
			t.Errorf("font is %d bytes, too small to be real", rec.Body.Len())
		}
	})

	t.Run("a missing asset is a 404, not the shell", func(t *testing.T) {
		// Answering a missing .js with HTML would turn a build problem into a
		// baffling syntax error in the console.
		rec := do(t, app, http.MethodGet, "/_app/immutable/nope.js")
		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", rec.Code)
		}
	})

	t.Run("caching matches how the files are named", func(t *testing.T) {
		shell := do(t, app, http.MethodGet, "/")
		if got := shell.Header().Get("Cache-Control"); got != "no-cache" {
			t.Errorf("index Cache-Control = %q, want no-cache", got)
		}
		font := do(t, app, http.MethodGet, "/fonts/remixicon/remixicon.woff2")
		if got := font.Header().Get("Cache-Control"); !strings.Contains(got, "max-age") {
			t.Errorf("font Cache-Control = %q, want a max-age", got)
		}
	})

	t.Run("the panel is locked to its own origin", func(t *testing.T) {
		rec := do(t, app, http.MethodGet, "/")
		csp := rec.Header().Get("Content-Security-Policy")
		for _, want := range []string{"default-src 'self'", "frame-ancestors 'none'", "object-src 'none'"} {
			if !strings.Contains(csp, want) {
				t.Errorf("CSP %q is missing %q", csp, want)
			}
		}
	})
}

// TestCSPDoesNotBlockThePanelFromBooting is a regression test for a real bug:
// a `script-src 'self'` header blocked SvelteKit's inline bootstrap script, so
// the page loaded, ran nothing, and rendered blank. Every asset returned 200 —
// the failure was invisible to any test that only checked status codes.
func TestCSPDoesNotBlockThePanelFromBooting(t *testing.T) {
	if !HasAdminPanel() {
		t.Skip("built with -tags no_admin")
	}
	app := newTestApp(t)

	rec := do(t, app, http.MethodGet, "/")
	html := rec.Body.String()
	header := rec.Header().Get("Content-Security-Policy")

	// Inline scripts with no src attribute. SvelteKit emits one and will not
	// let us remove it.
	inline := regexp.MustCompile(`<script(?:\s[^>]*)?>`)
	withSrc := regexp.MustCompile(`<script[^>]*\ssrc=`)
	inlineCount := 0
	for _, tag := range inline.FindAllString(html, -1) {
		if !withSrc.MatchString(tag) {
			inlineCount++
		}
	}

	if inlineCount > 0 {
		scriptSrc := directive(header, "script-src")
		if scriptSrc == "" {
			t.Fatalf("the page has %d inline script(s) and the CSP has no script-src", inlineCount)
		}
		permitsInline := strings.Contains(scriptSrc, "'unsafe-inline'") ||
			strings.Contains(scriptSrc, "'nonce-") ||
			strings.Contains(scriptSrc, "'sha256-")
		if !permitsInline {
			t.Errorf("the page has %d inline script(s) but script-src is %q — "+
				"the browser will refuse them and the panel will render blank",
				inlineCount, scriptSrc)
		}
	}

	// The header allows inline broadly, so the narrowing has to come from the
	// hashes SvelteKit puts in a meta policy. Both are enforced, and a script
	// must satisfy both — without the meta, inline would be wide open.
	if !strings.Contains(html, `http-equiv="content-security-policy"`) {
		t.Error("the page ships no meta CSP, so nothing restricts inline scripts")
	}
	if !strings.Contains(html, "sha256-") {
		t.Error("the meta CSP carries no script hash")
	}
}

// TestPanelAssetsTheShellNeedsAllResolve catches a build that emitted a shell
// referencing files that are not there — another way to get a blank page with
// a 200 on the document itself.
func TestPanelAssetsTheShellNeedsAllResolve(t *testing.T) {
	if !HasAdminPanel() {
		t.Skip("built with -tags no_admin")
	}
	app := newTestApp(t)

	html := do(t, app, http.MethodGet, "/").Body.String()

	refs := regexp.MustCompile(`(?:href|src)="(/[^"]+)"`).FindAllStringSubmatch(html, -1)
	if len(refs) == 0 {
		t.Fatal("the shell references no assets at all")
	}

	seen := map[string]bool{}
	for _, match := range refs {
		asset := match[1]
		if seen[asset] {
			continue
		}
		seen[asset] = true

		rec := do(t, app, http.MethodGet, asset)
		if rec.Code != http.StatusOK {
			t.Errorf("the shell references %s but it returns %d", asset, rec.Code)
		}
	}
	t.Logf("checked %d referenced assets", len(seen))
}

// directive pulls one directive's value out of a CSP header.
func directive(csp, name string) string {
	for _, part := range strings.Split(csp, ";") {
		part = strings.TrimSpace(part)
		if after, ok := strings.CutPrefix(part, name+" "); ok {
			return after
		}
	}
	return ""
}

// TestPanelDoesNotSwallowTheAPI is the one that matters after moving the panel
// to the root.
//
// A catch-all at / matches every path, so without an explicit guard an
// unmatched /api/… would be answered with index.html — and a client decoding
// JSON would report a syntax error instead of reading "no route for …".
func TestPanelDoesNotSwallowTheAPI(t *testing.T) {
	if !HasAdminPanel() {
		t.Skip("built with -tags no_admin")
	}
	app := newTestApp(t)

	t.Run("real API routes still win over the catch-all", func(t *testing.T) {
		for _, path := range []string{"/api/products", "/health", "/health/ready", "/doc"} {
			rec := do(t, app, http.MethodGet, path)
			if rec.Code != http.StatusOK {
				t.Errorf("GET %s = %d, want 200", path, rec.Code)
			}
			if ct := rec.Header().Get("Content-Type"); strings.Contains(ct, "text/html") {
				t.Errorf("GET %s returned HTML — the panel shadowed the API", path)
			}
		}
	})

	t.Run("an unmatched API path is a JSON 404, not the panel", func(t *testing.T) {
		for _, path := range []string{
			"/api/nope", "/api/products/x/y/z", "/health/nope", "/x/nosuchmodule/thing",
		} {
			rec := do(t, app, http.MethodGet, path)
			if rec.Code != http.StatusNotFound {
				t.Errorf("GET %s = %d, want 404", path, rec.Code)
				continue
			}
			if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
				t.Errorf("GET %s Content-Type = %q, want JSON", path, ct)
				continue
			}
			var got envelope
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Errorf("GET %s did not return JSON: %v", path, err)
				continue
			}
			if got.Error == nil || got.Error.Code != "not_found" {
				t.Errorf("GET %s error = %+v, want not_found", path, got.Error)
			}
		}
	})

	t.Run("admin routes still require a token", func(t *testing.T) {
		rec := do(t, app, http.MethodGet, "/api/admin/orders")
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401 — the catch-all must not bypass auth", rec.Code)
		}
	})
}

// TestAdminPanelRoutesAreNotInTheContract guards the exemption in the coverage
// test: panel routes must be marked UI, or they would silently start being
// treated as undocumented API endpoints.
func TestAdminPanelRoutesAreNotInTheContract(t *testing.T) {
	if !HasAdminPanel() {
		t.Skip("built with -tags no_admin")
	}
	app := newTestApp(t)

	var found int
	for _, r := range app.Routes() {
		if !r.UI {
			continue
		}
		found++
	}
	if found == 0 {
		t.Error("no panel routes were mounted")
	}

	// And the catch-all specifically must be marked, or every API path would
	// look documented-by-accident.
	for _, r := range app.Routes() {
		if r.Path == "/{path...}" && !r.UI {
			t.Error("the root catch-all is not marked UI")
		}
	}
}

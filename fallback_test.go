package gocommerce

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestUnroutedRequestsAnswerInJSON: the engine promises one response shape, so
// it has to hold for the requests that miss too. Without this, net/http's
// default replies "404 page not found" as plain text and a client that has
// been decoding JSON all along gets a parse error instead of an error it can
// read.
func TestUnroutedRequestsAnswerInJSON(t *testing.T) {
	app := newTestApp(t)

	tests := []struct {
		name       string
		method     string
		target     string
		wantStatus int
		wantCode   string
		wantInMsg  string
	}{
		{
			name: "a plausible but wrong path",
			// Health lives at /health, matching the reference API. Reaching
			// for /api/health is an easy mistake, and the answer should say so
			// rather than being unparseable.
			method: http.MethodGet, target: "/api/health",
			wantStatus: http.StatusNotFound, wantCode: "not_found",
			wantInMsg: "/api/health",
		},
		{
			name: "an unknown path under an API namespace",
			// With the panel at the root, a catch-all matches everything —
			// so the guard that keeps API paths answering in JSON is the
			// thing worth testing.
			method: http.MethodGet, target: "/api/nope",
			wantStatus: http.StatusNotFound, wantCode: "not_found",
			wantInMsg: "/api/nope",
		},
		{
			name:   "an unknown module namespace",
			method: http.MethodGet, target: "/x/nosuchmodule/thing",
			wantStatus: http.StatusNotFound, wantCode: "not_found",
			wantInMsg: "/x/nosuchmodule",
		},
		{
			name: "a real path with the wrong method",
			// The path exists; the method does not. That is worth telling
			// apart from "no such thing".
			method: http.MethodPost, target: "/health",
			wantStatus: http.StatusMethodNotAllowed, wantCode: "method_not_allowed",
			wantInMsg: "POST",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, app, tc.method, tc.target)

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				t.Errorf("Content-Type = %q, want JSON", ct)
			}

			var got envelope
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("response is not JSON: %v (body %q)", err, rec.Body)
			}
			if got.Error == nil {
				t.Fatalf("no error member in %s", rec.Body)
			}
			if got.Error.Code != tc.wantCode {
				t.Errorf("code = %q, want %q", got.Error.Code, tc.wantCode)
			}
			if !strings.Contains(got.Error.Message, tc.wantInMsg) {
				t.Errorf("message %q should mention %q", got.Error.Message, tc.wantInMsg)
			}
		})
	}
}

// TestMethodNotAllowedKeepsAllowHeader: the Allow header is how a client
// discovers what it should have sent, so buffering the mux's reply must not
// swallow it.
func TestMethodNotAllowedKeepsAllowHeader(t *testing.T) {
	app := newTestApp(t)

	rec := do(t, app, http.MethodPost, "/health")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
	allow := rec.Header().Get("Allow")
	if !strings.Contains(allow, http.MethodGet) {
		t.Errorf("Allow = %q, want it to include GET", allow)
	}
}

// TestUnknownNonAPIPath: what happens outside the API's namespaces depends on
// whether this binary carries the panel, and both answers are correct.
func TestUnknownNonAPIPath(t *testing.T) {
	app := newTestApp(t)

	rec := do(t, app, http.MethodGet, "/some/client/route")

	if HasAdminPanel() {
		// The panel routes itself, so this is one of its routes until proven
		// otherwise — a refresh on a deep link has to work.
		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200 (the panel's SPA fallback)", rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
			t.Errorf("Content-Type = %q, want HTML", ct)
		}
		return
	}

	// Without a panel nothing owns the root, so it is an ordinary 404 — and
	// still JSON, like every other error.
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q, want JSON", ct)
	}
}

// TestRoutedRequestsAreUntouched guards against the fallback wrapper changing
// anything about a request that does match.
func TestRoutedRequestsAreUntouched(t *testing.T) {
	app := newTestApp(t)

	rec := do(t, app, http.MethodGet, "/health")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Errorf("body = %s", rec.Body)
	}

	// A 404 produced by a handler — rather than by the router — keeps its own
	// message instead of being replaced by the router's.
	rec = do(t, app, http.MethodGet, "/api/products/999999")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "product not found") {
		t.Errorf("a handler's own 404 was replaced: %s", rec.Body)
	}
}

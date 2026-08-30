package gocommerce

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Pagination bounds. Every list endpoint paginates; an unbounded list is an
// outage waiting for the catalog to grow.
const (
	DefaultLimit = 50
	MaxLimit     = 200
)

// APIError is the engine's error type. Handlers return one and the response
// layer renders it as {"error": {"code", "message"}} with the right status,
// so every endpoint — core or module — reports failures identically.
type APIError struct {
	Status  int    `json:"-"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Details any    `json:"details,omitempty"`

	wrapped error
}

func (e *APIError) Error() string {
	if e.wrapped != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.wrapped)
	}
	return e.Code + ": " + e.Message
}

func (e *APIError) Unwrap() error { return e.wrapped }

// Is compares by code, so errors.Is(err, gocommerce.ErrNotFound) works
// regardless of the message a particular call site chose.
func (e *APIError) Is(target error) bool {
	var t *APIError
	return errors.As(target, &t) && t.Code == e.Code
}

// Sentinel errors for errors.Is comparison.
var (
	ErrNotFound         = &APIError{Status: http.StatusNotFound, Code: "not_found", Message: "resource not found"}
	ErrMethodNotAllowed = &APIError{Status: http.StatusMethodNotAllowed, Code: "method_not_allowed", Message: "method not allowed"}
	ErrValidation       = &APIError{Status: http.StatusBadRequest, Code: "validation_failed", Message: "invalid request"}
	ErrConflict         = &APIError{Status: http.StatusConflict, Code: "conflict", Message: "conflicting request"}
	ErrUnauthorized     = &APIError{Status: http.StatusUnauthorized, Code: "unauthorized", Message: "authentication required"}
	// Forbidden is not Unauthorized: the caller proved who they are and the
	// answer is still no. A panel that cannot tell the two apart signs the
	// operator out when it should be explaining what their role does not carry.
	ErrForbidden = &APIError{Status: http.StatusForbidden, Code: "forbidden", Message: "not permitted"}
	ErrInternal  = &APIError{Status: http.StatusInternalServerError, Code: "internal_error", Message: "internal error"}
)

// NotFoundf, Validationf, Conflictf and Internalf build errors of each kind.
func NotFoundf(format string, args ...any) *APIError {
	return &APIError{Status: http.StatusNotFound, Code: "not_found", Message: fmt.Sprintf(format, args...)}
}

func Validationf(format string, args ...any) *APIError {
	return &APIError{Status: http.StatusBadRequest, Code: "validation_failed", Message: fmt.Sprintf(format, args...)}
}

func Conflictf(format string, args ...any) *APIError {
	return &APIError{Status: http.StatusConflict, Code: "conflict", Message: fmt.Sprintf(format, args...)}
}

func Forbiddenf(format string, args ...any) *APIError {
	return &APIError{Status: http.StatusForbidden, Code: "forbidden", Message: fmt.Sprintf(format, args...)}
}

// Internalf wraps an underlying error. The wrapped detail is logged, never
// sent to the client.
func Internalf(err error, format string, args ...any) *APIError {
	return &APIError{
		Status: http.StatusInternalServerError, Code: "internal_error",
		Message: fmt.Sprintf(format, args...), wrapped: err,
	}
}

// WithDetails attaches structured detail, such as the per-line reasons a
// checkout was rejected.
func (e *APIError) WithDetails(d any) *APIError {
	c := *e
	c.Details = d
	return &c
}

// envelope is the single response shape: exactly one of data or error.
type envelope struct {
	Data  any       `json:"data,omitempty"`
	Error *APIError `json:"error,omitempty"`
	Meta  *ListMeta `json:"meta,omitempty"`
}

// ListMeta accompanies every paginated collection.
//
// Both coordinate systems are reported: offset for a client walking a cursor,
// and page for a UI drawing "3 of 12". Page and TotalPages are derived, never
// supplied, so the two can never disagree.
type ListMeta struct {
	Total      int `json:"total"`
	Limit      int `json:"limit"`
	Offset     int `json:"offset"`
	Page       int `json:"page"`
	TotalPages int `json:"total_pages"`
}

// derive fills in the page view of an offset.
func (m *ListMeta) derive() {
	if m.Limit <= 0 {
		m.Limit = DefaultLimit
	}
	if m.Offset < 0 {
		m.Offset = 0
	}
	// An offset that is not a whole number of pages still has an answer: the
	// page it falls inside.
	m.Page = m.Offset/m.Limit + 1
	m.TotalPages = (m.Total + m.Limit - 1) / m.Limit
}

// Respond writes {"data": v} with the given status.
func Respond(w http.ResponseWriter, status int, v any) {
	writeJSON(w, status, envelope{Data: v})
}

// RespondList writes a paginated collection with its meta block.
func RespondList(w http.ResponseWriter, v any, meta ListMeta) {
	meta.derive()
	writeJSON(w, http.StatusOK, envelope{Data: v, Meta: &meta})
}

// RespondError renders any error as the error envelope. Non-APIError values
// become a generic internal error: an unexpected failure must never leak a
// driver message or a query fragment to a client.
func RespondError(w http.ResponseWriter, r *http.Request, err error) {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		apiErr = Internalf(err, "internal error")
	}
	if apiErr.Status == 0 {
		apiErr.Status = http.StatusInternalServerError
	}
	if apiErr.Status >= 500 {
		logFrom(r).Error("request failed",
			"code", apiErr.Code, "error", apiErr.Error(),
			"method", r.Method, "path", r.URL.Path)
		safe := *apiErr
		safe.wrapped = nil
		safe.Message = "internal error"
		writeJSON(w, safe.Status, envelope{Error: &safe})
		return
	}
	writeJSON(w, apiErr.Status, envelope{Error: apiErr})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	buf, err := json.Marshal(v)
	if err != nil {
		http.Error(w, `{"error":{"code":"internal_error","message":"response encoding failed"}}`,
			http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(buf)
}

// DecodeJSON reads a JSON request body under a size limit, rejecting unknown
// fields so a typo in a client payload fails loudly instead of being ignored.
func DecodeJSON(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return Validationf("request body exceeds %d bytes", maxJSONBytes)
		}
		return Validationf("malformed JSON body: %v", err)
	}
	if dec.More() {
		return Validationf("request body must contain a single JSON value")
	}
	return nil
}

// Page parses the pagination query parameters and returns the window to
// fetch, applying the engine's bounds.
//
// A collection can be walked either way:
//
//	?limit=20&offset=40    the 20 records after the first 40
//	?limit=20&page=3       the third page of 20 — the same window
//
// Page numbers start at 1, because that is what a person reading "page 3 of
// 12" means. When a request carries both, page wins: it is the more specific
// intent, and silently honouring the offset instead would send a client to a
// different page than the one it asked for.
//
// The signature stays (limit, offset) so callers — including modules — need no
// change; page is simply another way of arriving at an offset.
func Page(r *http.Request) (limit, offset int, err error) {
	limit, offset = DefaultLimit, 0
	q := r.URL.Query()

	// Limit is parsed first: the page arithmetic below depends on it.
	if s := q.Get("limit"); s != "" {
		limit, err = strconv.Atoi(s)
		if err != nil || limit < 1 {
			return 0, 0, Validationf("limit must be a positive integer")
		}
		if limit > MaxLimit {
			return 0, 0, Validationf("limit must not exceed %d", MaxLimit)
		}
	}
	if s := q.Get("offset"); s != "" {
		offset, err = strconv.Atoi(s)
		if err != nil || offset < 0 {
			return 0, 0, Validationf("offset must be a non-negative integer")
		}
	}
	if s := q.Get("page"); s != "" {
		page, perr := strconv.Atoi(s)
		if perr != nil || page < 1 {
			return 0, 0, Validationf("page must be a positive integer; pages start at 1")
		}
		offset = (page - 1) * limit
	}
	return limit, offset, nil
}

// ---------------------------------------------------------------- middleware

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
		w.ResponseWriter.WriteHeader(code)
	}
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

func (a *App) recoverMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if p := recover(); p != nil {
				a.log.Error("panic recovered",
					"panic", p, "method", r.Method, "path", r.URL.Path)
				writeJSON(w, http.StatusInternalServerError, envelope{Error: ErrInternal})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// fallbackJSON answers unrouted requests in the same envelope as everything
// else.
//
// Without it, net/http's default handlers reply "404 page not found" as plain
// text — so a client that has been decoding JSON all along gets a parse error
// instead of an error it can read. Since the engine promises one response
// shape, it has to hold for the requests that miss too.
func (a *App) fallbackJSON(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, pattern := a.mux.Handler(r); pattern != "" {
			next.ServeHTTP(w, r)
			return
		}

		// No pattern matched, so what follows is one of the mux's own
		// fallbacks. Run it against a recorder to learn which — a path that
		// exists under another method answers 405 and sets Allow, and that
		// distinction is worth keeping.
		rec := &fallbackRecorder{header: http.Header{}}
		next.ServeHTTP(rec, r)

		for key, values := range rec.header {
			for _, v := range values {
				w.Header().Add(key, v)
			}
		}

		if rec.status == http.StatusMethodNotAllowed {
			allowed := rec.header.Get("Allow")
			message := fmt.Sprintf("%s is not allowed on %s", r.Method, r.URL.Path)
			if allowed != "" {
				message += fmt.Sprintf(" (try %s)", allowed)
			}
			writeJSON(w, http.StatusMethodNotAllowed, envelope{Error: &APIError{
				Status: http.StatusMethodNotAllowed, Code: "method_not_allowed", Message: message,
			}})
			return
		}

		writeJSON(w, http.StatusNotFound, envelope{Error: &APIError{
			Status: http.StatusNotFound, Code: "not_found",
			Message: fmt.Sprintf("no route for %s %s — see /docs for what this store serves",
				r.Method, r.URL.Path),
		}})
	})
}

// fallbackRecorder captures a handler's response instead of sending it, so the
// status and headers can be inspected before anything reaches the client.
type fallbackRecorder struct {
	header http.Header
	status int
}

func (f *fallbackRecorder) Header() http.Header { return f.header }

func (f *fallbackRecorder) WriteHeader(code int) {
	if f.status == 0 {
		f.status = code
	}
}

func (f *fallbackRecorder) Write(b []byte) (int, error) {
	if f.status == 0 {
		f.status = http.StatusOK
	}
	return len(b), nil // discarded: the caller writes its own body
}

func (a *App) logMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, withLogger(r, a.log))
		if sw.status == 0 {
			sw.status = http.StatusOK
		}
		a.log.Info("request",
			"method", r.Method, "path", r.URL.Path, "status", sw.status,
			"bytes", sw.bytes, "duration", time.Since(start))
	})
}

// ------------------------------------------------------------- admin auth

// adminAuth returns the middleware guarding admin routes: the configured
// replacement if one was supplied, otherwise bearer-token authentication.
//
// Config.AdminAuth is the seam a future identity module replaces to add
// sessions, OIDC or RBAC. The engine never learns what a user is.
func (a *App) adminAuth() func(http.Handler) http.Handler {
	if a.cfg.AdminAuth != nil {
		return a.cfg.AdminAuth
	}
	return a.bearerAuth
}

// bearerAuth accepts two kinds of credential, both as "Bearer <x>":
//
//   - a static token from Config.AdminTokens, for scripts and CI; and
//   - a superuser session token, for a person signed in to the panel.
//
// Static tokens are checked first because the check is a memory compare,
// while a session costs a query. An unauthenticated request therefore never
// reaches the database, which is what keeps a flood of bad tokens from
// becoming load on it.
func (a *App) bearerAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearerToken(r)
		if !ok {
			a.denyAdmin(w, r)
			return
		}
		if a.validAdminToken(token) {
			next.ServeHTTP(w, r)
			return
		}
		if su, ok := a.superusers.Resolve(r.Context(), token); ok {
			next.ServeHTTP(w, withSuperuser(r, su))
			return
		}
		a.denyAdmin(w, r)
	})
}

func (a *App) denyAdmin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="gocommerce admin"`)
	RespondError(w, r, ErrUnauthorized)
}

func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	return strings.TrimSpace(h[len(prefix):]), true
}

// validAdminToken compares in constant time against every configured token,
// without an early exit, so neither the match nor its position is timeable.
func (a *App) validAdminToken(token string) bool {
	var match int
	for _, want := range a.cfg.AdminTokens {
		match |= subtle.ConstantTimeCompare([]byte(token), []byte(want))
	}
	return match == 1
}

// ------------------------------------------------------------ core routes

func (a *App) mountCoreRoutes() {
	a.HandleFunc("GET /health", a.handleHealth)
	a.HandleFunc("GET /health/ready", a.handleReady)
	a.HandleFunc("GET /doc", a.handleOpenAPI)
	a.HandleFunc("GET /docs", a.handleDocs)

	a.mountSuperuserRoutes()
	a.mountMediaRoutes()
	a.mountCatalogRoutes()
	a.mountCollectionRoutes()
	a.mountCategoryRoutes()
	a.mountDiscountRoutes()
	a.mountTaxRoutes()
	a.mountLocationRoutes()
	a.mountTeamRoutes()
	a.mountCartRoutes()
	a.mountCheckoutRoutes()
	a.mountOrderRoutes()
	a.mountTransferRoutes()
	a.mountAdminPanel()
}

// handleHealth is liveness: the process is up. It touches nothing, so an
// orchestrator does not restart a healthy process because the database
// hiccuped.
func (a *App) handleHealth(w http.ResponseWriter, r *http.Request) {
	Respond(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"version": Version,
	})
}

// handleReady is readiness: this process can serve traffic, which means the
// database answers.
func (a *App) handleReady(w http.ResponseWriter, r *http.Request) {
	if err := dbReady(r.Context(), a.db); err != nil {
		a.log.Warn("readiness check failed", "error", err)
		writeJSON(w, http.StatusServiceUnavailable, envelope{Error: &APIError{
			Status: http.StatusServiceUnavailable, Code: "not_ready",
			Message: "database unavailable",
		}})
		return
	}
	Respond(w, http.StatusOK, map[string]any{
		"status":   "ready",
		"version":  Version,
		"currency": a.cfg.Currency,
		"language": a.cfg.DefaultLanguage,
	})
}

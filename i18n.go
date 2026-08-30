package gocommerce

import (
	"context"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

type ctxKey int

const (
	ctxKeyLanguage ctxKey = iota
	ctxKeyLogger
	ctxKeySuperuser
)

// WithLanguage returns a context carrying the resolved request language.
func WithLanguage(ctx context.Context, lang string) context.Context {
	return context.WithValue(ctx, ctxKeyLanguage, lang)
}

// Language returns the language resolved for the request, or "" outside a
// request. Handlers should prefer [App.RequestLanguage], which falls back to
// the store default.
func Language(ctx context.Context) string {
	lang, _ := ctx.Value(ctxKeyLanguage).(string)
	return lang
}

// RequestLanguage is the language to render this request in, always a member
// of Config.Languages.
func (a *App) RequestLanguage(r *http.Request) string {
	if lang := Language(r.Context()); lang != "" {
		return lang
	}
	return a.cfg.DefaultLanguage
}

func withLogger(r *http.Request, log *slog.Logger) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), ctxKeyLogger, log))
}

func logFrom(r *http.Request) *slog.Logger {
	if log, ok := r.Context().Value(ctxKeyLogger).(*slog.Logger); ok && log != nil {
		return log
	}
	return slog.Default()
}

// languageMW resolves the request language once, so handlers and any
// registered translator agree on it.
func (a *App) languageMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lang := negotiateLanguage(
			a.cfg.Languages, a.cfg.DefaultLanguage,
			r.URL.Query().Get("lang"), r.Header.Get("Accept-Language"))
		w.Header().Set("Content-Language", lang)
		next.ServeHTTP(w, r.WithContext(WithLanguage(r.Context(), lang)))
	})
}

// negotiateLanguage picks the language to serve: an explicit ?lang= wins, then
// Accept-Language in client preference order, then the store default.
//
// Matching is deliberately shallow — exact tag, else primary subtag, so a
// client asking for "en-GB" is served "en". Full RFC 4647 lookup would mean
// taking golang.org/x/text as a dependency for a case no store has yet.
func negotiateLanguage(available []string, def, query, acceptHeader string) string {
	if len(available) == 0 {
		return def
	}
	if query != "" {
		if m, ok := matchLanguage(available, strings.TrimSpace(query)); ok {
			return m
		}
		// An explicit, unsupported ?lang= falls back to the default rather
		// than to Accept-Language: the client asked a specific question and
		// deserves a predictable answer.
		return def
	}
	for _, candidate := range parseAcceptLanguage(acceptHeader) {
		if candidate == "*" {
			return def
		}
		if m, ok := matchLanguage(available, candidate); ok {
			return m
		}
	}
	return def
}

func matchLanguage(available []string, candidate string) (string, bool) {
	if candidate == "" {
		return "", false
	}
	for _, a := range available {
		if strings.EqualFold(a, candidate) {
			return a, true
		}
	}
	want := primarySubtag(candidate)
	for _, a := range available {
		if strings.EqualFold(primarySubtag(a), want) {
			return a, true
		}
	}
	return "", false
}

func primarySubtag(tag string) string {
	if i := strings.IndexByte(tag, '-'); i > 0 {
		return tag[:i]
	}
	return tag
}

// parseAcceptLanguage returns the header's tags ordered by descending quality.
// Malformed entries are skipped rather than failing the request: a bad header
// is the client's problem, not a reason to refuse to serve a page.
func parseAcceptLanguage(h string) []string {
	if h = strings.TrimSpace(h); h == "" {
		return nil
	}
	type pref struct {
		tag   string
		q     float64
		order int
	}
	var prefs []pref
	for i, part := range strings.Split(h, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		tag, q := part, 1.0
		if semi := strings.IndexByte(part, ';'); semi >= 0 {
			tag = strings.TrimSpace(part[:semi])
			for _, param := range strings.Split(part[semi+1:], ";") {
				param = strings.TrimSpace(param)
				if v, ok := strings.CutPrefix(param, "q="); ok {
					if parsed, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
						q = parsed
					}
				}
			}
		}
		if tag == "" || q <= 0 {
			continue
		}
		prefs = append(prefs, pref{tag: tag, q: q, order: i})
	}
	sort.SliceStable(prefs, func(i, j int) bool { return prefs[i].q > prefs[j].q })

	tags := make([]string, 0, len(prefs))
	for _, p := range prefs {
		tags = append(tags, p.tag)
	}
	return tags
}

func containsFold(list []string, s string) bool {
	for _, v := range list {
		if strings.EqualFold(v, s) {
			return true
		}
	}
	return false
}

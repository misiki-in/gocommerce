// Package cms adds content pages — about, terms, delivery, a landing page —
// to a gocommerce store.
//
// It is a module rather than part of the engine because a store can sell
// without it, which is the test every feature has to pass to be in core. It
// owns its own table and mounts its own routes, and it never touches a
// commerce table.
//
//	app, err := gocommerce.New(cfg, cms.New(cms.Config{}))
//
// Pages are then served at /x/cms/pages/{slug} and managed under
// /api/admin/x/cms/pages.
package cms

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/misiki/gocommerce"
)

// Config configures the module.
type Config struct {
	// PublishedOnly hides draft pages from the public endpoint. It defaults
	// to true, and there is no good reason to turn it off outside a preview
	// environment.
	PublishedOnly *bool
}

// Page is a content page.
type Page struct {
	ID          int64             `json:"id"`
	Slug        string            `json:"slug"`
	Title       string            `json:"title"`
	Body        string            `json:"body"`
	Excerpt     string            `json:"excerpt,omitempty"`
	Status      string            `json:"status"`
	Language    string            `json:"language"`
	SEO         map[string]string `json:"seo,omitempty"`
	PublishedAt *time.Time        `json:"published_at,omitempty"`
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
}

// Page statuses.
const (
	StatusDraft     = "draft"
	StatusPublished = "published"
)

// Module is the CMS.
type Module struct {
	cfg           Config
	db            *sql.DB
	publishedOnly bool
	defaultLang   string
}

// New constructs the module.
func New(cfg Config) *Module { return &Module{cfg: cfg} }

// Name implements gocommerce.Module.
func (m *Module) Name() string { return "cms" }

// Migrations implements gocommerce.Module.
func (m *Module) Migrations() []gocommerce.Migration {
	return []gocommerce.Migration{{
		ID: "0001_pages",
		SQL: `
			CREATE TABLE cms_pages (
			    id           bigserial   PRIMARY KEY,
			    slug         text        NOT NULL,
			    language     text        NOT NULL DEFAULT 'en',
			    title        text        NOT NULL,
			    body         text        NOT NULL DEFAULT '',
			    excerpt      text        NOT NULL DEFAULT '',
			    status       text        NOT NULL DEFAULT 'draft'
			                             CHECK (status IN ('draft', 'published')),
			    seo          jsonb       NOT NULL DEFAULT '{}',
			    published_at timestamptz,
			    created_at   timestamptz NOT NULL DEFAULT now(),
			    updated_at   timestamptz NOT NULL DEFAULT now(),
			    -- A slug is unique per language, so the same page can exist in
			    -- several languages without colliding.
			    UNIQUE (slug, language)
			);
			CREATE INDEX cms_pages_published_idx ON cms_pages (status, published_at DESC);`,
	}}
}

// Register implements gocommerce.Module.
func (m *Module) Register(app *gocommerce.App) error {
	m.db = app.DB()
	m.publishedOnly = true
	if m.cfg.PublishedOnly != nil {
		m.publishedOnly = *m.cfg.PublishedOnly
	}
	m.defaultLang = app.Config().DefaultLanguage

	app.HandleFunc("GET /x/cms/pages", m.handleListPublic)
	app.HandleFunc("GET /x/cms/pages/{slug}", m.handleGetPublic)

	app.HandleAdminFunc("GET /api/admin/x/cms/pages", m.handleListAdmin)
	app.HandleAdminFunc("POST /api/admin/x/cms/pages", m.handleCreate)
	app.HandleAdminFunc("GET /api/admin/x/cms/pages/{id}", m.handleGetAdmin)
	app.HandleAdminFunc("PATCH /api/admin/x/cms/pages/{id}", m.handleUpdate)
	app.HandleAdminFunc("DELETE /api/admin/x/cms/pages/{id}", m.handleDelete)
	return nil
}

// ------------------------------------------------------------------- public

func (m *Module) handleListPublic(w http.ResponseWriter, r *http.Request) {
	limit, offset, err := gocommerce.Page(r)
	if err != nil {
		gocommerce.RespondError(w, r, err)
		return
	}
	lang := m.requestLanguage(r)
	pages, total, err := m.list(r.Context(), listQuery{
		Language: lang, Status: m.publicStatus(), Limit: limit, Offset: offset,
	})
	if err != nil {
		gocommerce.RespondError(w, r, err)
		return
	}
	gocommerce.RespondList(w, pages, gocommerce.ListMeta{Total: total, Limit: limit, Offset: offset})
}

func (m *Module) handleGetPublic(w http.ResponseWriter, r *http.Request) {
	lang := m.requestLanguage(r)
	slug := r.PathValue("slug")

	page, err := m.bySlug(r.Context(), slug, lang, m.publicStatus())
	if errors.Is(err, sql.ErrNoRows) && lang != m.defaultLang {
		// Fall back to the store's own language rather than 404ing a shopper
		// whose browser asked for a translation nobody has written yet.
		page, err = m.bySlug(r.Context(), slug, m.defaultLang, m.publicStatus())
	}
	if errors.Is(err, sql.ErrNoRows) {
		gocommerce.RespondError(w, r, gocommerce.NotFoundf("no page at %q", slug))
		return
	}
	if err != nil {
		gocommerce.RespondError(w, r, err)
		return
	}
	gocommerce.Respond(w, http.StatusOK, page)
}

func (m *Module) publicStatus() string {
	if m.publishedOnly {
		return StatusPublished
	}
	return ""
}

func (m *Module) requestLanguage(r *http.Request) string {
	if lang := gocommerce.Language(r.Context()); lang != "" {
		return lang
	}
	return m.defaultLang
}

// -------------------------------------------------------------------- admin

func (m *Module) handleListAdmin(w http.ResponseWriter, r *http.Request) {
	limit, offset, err := gocommerce.Page(r)
	if err != nil {
		gocommerce.RespondError(w, r, err)
		return
	}
	pages, total, err := m.list(r.Context(), listQuery{
		Language: r.URL.Query().Get("language"),
		Status:   r.URL.Query().Get("status"),
		Limit:    limit, Offset: offset,
	})
	if err != nil {
		gocommerce.RespondError(w, r, err)
		return
	}
	gocommerce.RespondList(w, pages, gocommerce.ListMeta{Total: total, Limit: limit, Offset: offset})
}

func (m *Module) handleGetAdmin(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		gocommerce.RespondError(w, r, err)
		return
	}
	page, err := m.byID(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		gocommerce.RespondError(w, r, gocommerce.NotFoundf("page %d does not exist", id))
		return
	}
	if err != nil {
		gocommerce.RespondError(w, r, err)
		return
	}
	gocommerce.Respond(w, http.StatusOK, page)
}

type pageInput struct {
	Slug     string            `json:"slug"`
	Title    string            `json:"title"`
	Body     string            `json:"body"`
	Excerpt  string            `json:"excerpt"`
	Status   string            `json:"status"`
	Language string            `json:"language"`
	SEO      map[string]string `json:"seo"`
}

func (m *Module) handleCreate(w http.ResponseWriter, r *http.Request) {
	var in pageInput
	if err := gocommerce.DecodeJSON(w, r, &in); err != nil {
		gocommerce.RespondError(w, r, err)
		return
	}
	if strings.TrimSpace(in.Title) == "" {
		gocommerce.RespondError(w, r, gocommerce.Validationf("title is required"))
		return
	}
	if in.Slug = strings.TrimSpace(in.Slug); in.Slug == "" {
		gocommerce.RespondError(w, r, gocommerce.Validationf("slug is required"))
		return
	}
	if in.Language == "" {
		in.Language = m.defaultLang
	}
	if in.Status == "" {
		in.Status = StatusDraft
	}
	if in.Status != StatusDraft && in.Status != StatusPublished {
		gocommerce.RespondError(w, r, gocommerce.Validationf("status must be draft or published"))
		return
	}
	seo, err := json.Marshal(orEmpty(in.SEO))
	if err != nil {
		gocommerce.RespondError(w, r, err)
		return
	}

	var id int64
	err = m.db.QueryRowContext(r.Context(), `
		INSERT INTO cms_pages (slug, language, title, body, excerpt, status, seo, published_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7, CASE WHEN $6 = 'published' THEN now() END)
		RETURNING id`,
		in.Slug, in.Language, in.Title, in.Body, in.Excerpt, in.Status, seo).Scan(&id)
	if err != nil {
		if strings.Contains(err.Error(), "cms_pages_slug_language_key") {
			gocommerce.RespondError(w, r,
				gocommerce.Conflictf("a %s page already exists at %q", in.Language, in.Slug))
			return
		}
		gocommerce.RespondError(w, r, err)
		return
	}
	page, err := m.byID(r.Context(), id)
	if err != nil {
		gocommerce.RespondError(w, r, err)
		return
	}
	gocommerce.Respond(w, http.StatusCreated, page)
}

func (m *Module) handleUpdate(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		gocommerce.RespondError(w, r, err)
		return
	}
	var in struct {
		Slug    *string            `json:"slug"`
		Title   *string            `json:"title"`
		Body    *string            `json:"body"`
		Excerpt *string            `json:"excerpt"`
		Status  *string            `json:"status"`
		SEO     *map[string]string `json:"seo"`
	}
	if err := gocommerce.DecodeJSON(w, r, &in); err != nil {
		gocommerce.RespondError(w, r, err)
		return
	}

	sets, args := []string{}, []any{}
	add := func(expr string, v any) {
		args = append(args, v)
		sets = append(sets, fmt.Sprintf("%s = $%d", expr, len(args)))
	}
	if in.Slug != nil {
		add("slug", *in.Slug)
	}
	if in.Title != nil {
		add("title", *in.Title)
	}
	if in.Body != nil {
		add("body", *in.Body)
	}
	if in.Excerpt != nil {
		add("excerpt", *in.Excerpt)
	}
	if in.Status != nil {
		if *in.Status != StatusDraft && *in.Status != StatusPublished {
			gocommerce.RespondError(w, r, gocommerce.Validationf("status must be draft or published"))
			return
		}
		add("status", *in.Status)
		if *in.Status == StatusPublished {
			sets = append(sets, "published_at = coalesce(published_at, now())")
		}
	}
	if in.SEO != nil {
		seo, err := json.Marshal(*in.SEO)
		if err != nil {
			gocommerce.RespondError(w, r, err)
			return
		}
		add("seo", seo)
	}
	if len(sets) == 0 {
		page, err := m.byID(r.Context(), id)
		if err != nil {
			gocommerce.RespondError(w, r, err)
			return
		}
		gocommerce.Respond(w, http.StatusOK, page)
		return
	}
	sets = append(sets, "updated_at = now()")
	args = append(args, id)

	res, err := m.db.ExecContext(r.Context(),
		"UPDATE cms_pages SET "+strings.Join(sets, ", ")+fmt.Sprintf(" WHERE id = $%d", len(args)), args...)
	if err != nil {
		gocommerce.RespondError(w, r, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		gocommerce.RespondError(w, r, gocommerce.NotFoundf("page %d does not exist", id))
		return
	}
	page, err := m.byID(r.Context(), id)
	if err != nil {
		gocommerce.RespondError(w, r, err)
		return
	}
	gocommerce.Respond(w, http.StatusOK, page)
}

func (m *Module) handleDelete(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		gocommerce.RespondError(w, r, err)
		return
	}
	res, err := m.db.ExecContext(r.Context(), `DELETE FROM cms_pages WHERE id = $1`, id)
	if err != nil {
		gocommerce.RespondError(w, r, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		gocommerce.RespondError(w, r, gocommerce.NotFoundf("page %d does not exist", id))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ------------------------------------------------------------------ queries

const pageColumns = `id, slug, language, title, body, excerpt, status, seo,
	published_at, created_at, updated_at`

func scanPage(row interface{ Scan(...any) error }) (*Page, error) {
	var p Page
	var seo []byte
	var publishedAt sql.NullTime
	if err := row.Scan(&p.ID, &p.Slug, &p.Language, &p.Title, &p.Body, &p.Excerpt,
		&p.Status, &seo, &publishedAt, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return nil, err
	}
	if publishedAt.Valid {
		p.PublishedAt = &publishedAt.Time
	}
	if len(seo) > 0 {
		_ = json.Unmarshal(seo, &p.SEO)
	}
	return &p, nil
}

func (m *Module) byID(ctx context.Context, id int64) (*Page, error) {
	return scanPage(m.db.QueryRowContext(ctx,
		`SELECT `+pageColumns+` FROM cms_pages WHERE id = $1`, id))
}

func (m *Module) bySlug(ctx context.Context, slug, lang, status string) (*Page, error) {
	if status == "" {
		return scanPage(m.db.QueryRowContext(ctx,
			`SELECT `+pageColumns+` FROM cms_pages WHERE slug = $1 AND language = $2`, slug, lang))
	}
	return scanPage(m.db.QueryRowContext(ctx,
		`SELECT `+pageColumns+` FROM cms_pages WHERE slug = $1 AND language = $2 AND status = $3`,
		slug, lang, status))
}

type listQuery struct {
	Language, Status string
	Limit, Offset    int
}

func (m *Module) list(ctx context.Context, q listQuery) ([]*Page, int, error) {
	where, args := []string{"1 = 1"}, []any{}
	add := func(expr string, v any) {
		args = append(args, v)
		where = append(where, fmt.Sprintf(expr, len(args)))
	}
	if q.Language != "" {
		add("language = $%d", q.Language)
	}
	if q.Status != "" {
		add("status = $%d", q.Status)
	}
	clause := strings.Join(where, " AND ")

	var total int
	if err := m.db.QueryRowContext(ctx,
		`SELECT count(*) FROM cms_pages WHERE `+clause, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	args = append(args, q.Limit, q.Offset)
	rows, err := m.db.QueryContext(ctx,
		`SELECT `+pageColumns+` FROM cms_pages WHERE `+clause+
			fmt.Sprintf(" ORDER BY id DESC LIMIT $%d OFFSET $%d", len(args)-1, len(args)), args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	pages := []*Page{}
	for rows.Next() {
		p, err := scanPage(rows)
		if err != nil {
			return nil, 0, err
		}
		pages = append(pages, p)
	}
	return pages, total, rows.Err()
}

func pathID(r *http.Request) (int64, error) {
	var id int64
	if _, err := fmt.Sscanf(r.PathValue("id"), "%d", &id); err != nil || id <= 0 {
		return 0, gocommerce.Validationf("id must be a positive integer")
	}
	return id, nil
}

func orEmpty(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

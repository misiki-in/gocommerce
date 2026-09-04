package gocommerce

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// A collection is a named, hand-curated grouping of products: "New in", a
// seasonal edit, the six things on the home page.
//
// Rule-based ("smart") collections are deliberately absent. They need a query
// language, a re-evaluation schedule and a story about what happens when the
// rule changes under a live storefront; what a catalog this size actually uses
// is a list somebody picked, in the order they picked it.
type Collection struct {
	ID          int64  `json:"id"`
	Slug        string `json:"slug"`
	Title       string `json:"title"`
	Description string `json:"description"`
	// Position orders collections against each other — a navigation menu, not
	// the order of products inside one.
	Position  int       `json:"position"`
	Metadata  Metadata  `json:"metadata"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ProductCollection is the shape a collection takes when it rides along on a
// product: enough to render a chip and link to the collection, and no more.
// Carrying the whole Collection would put a description on every row of a
// product listing that nothing renders.
type ProductCollection struct {
	ID    int64  `json:"id"`
	Slug  string `json:"slug"`
	Title string `json:"title"`
}

// CollectionInput creates a collection.
type CollectionInput struct {
	Slug        string   `json:"slug"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Position    *int     `json:"position"`
	Metadata    Metadata `json:"metadata"`
}

// CollectionPatch updates one. A nil field is left alone, which is what
// distinguishes "not mentioned" from "set to empty".
type CollectionPatch struct {
	Slug        *string   `json:"slug"`
	Title       *string   `json:"title"`
	Description *string   `json:"description"`
	Position    *int      `json:"position"`
	Metadata    *Metadata `json:"metadata"`
}

// Collections owns collections and product membership in them.
//
// Nothing here emits an event. Merchandising is not a state machine: moving a
// product between collections changes what a storefront shows, not what the
// store owes anyone, and an event nothing could act on is a promise the outbox
// would have to keep forever.
type Collections struct {
	app *App
}

// Collections returns the collections service. It is built per call rather
// than held on App because it carries nothing but the App itself.
func (a *App) Collections() *Collections { return &Collections{app: a} }

// -------------------------------------------------------------------- service

const collectionColumns = `id, slug, title, description, position, metadata, created_at, updated_at`

func scanCollection(row interface{ Scan(...any) error }) (*Collection, error) {
	var c Collection
	var meta []byte
	if err := row.Scan(&c.ID, &c.Slug, &c.Title, &c.Description, &c.Position,
		&meta, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return nil, err
	}
	if err := scanMetadata(meta, &c.Metadata); err != nil {
		return nil, err
	}
	return &c, nil
}

// Create adds a collection. The slug is derived from the title when omitted,
// the same way a product's is.
func (s *Collections) Create(ctx context.Context, in CollectionInput) (*Collection, error) {
	if err := normalizeCollectionInput(&in); err != nil {
		return nil, err
	}
	meta, err := in.Metadata.value()
	if err != nil {
		return nil, Validationf("metadata is not valid JSON: %v", err)
	}
	position := 0
	if in.Position != nil {
		position = *in.Position
	}
	c, err := scanCollection(s.app.db.QueryRowContext(ctx, `
		INSERT INTO collections (slug, title, description, position, metadata)
		VALUES ($1, $2, $3, $4, $5) RETURNING `+collectionColumns,
		in.Slug, in.Title, in.Description, position, meta))
	if err != nil {
		return nil, translateCollectionErr(err)
	}
	return c, nil
}

// Get loads a collection by id.
func (s *Collections) Get(ctx context.Context, id int64) (*Collection, error) {
	return s.one(ctx, `id = $1`, id)
}

// GetBySlug loads a collection by its URL slug.
func (s *Collections) GetBySlug(ctx context.Context, slug string) (*Collection, error) {
	return s.one(ctx, `slug = $1`, strings.TrimSpace(slug))
}

func (s *Collections) one(ctx context.Context, where string, arg any) (*Collection, error) {
	c, err := scanCollection(s.app.db.QueryRowContext(ctx,
		`SELECT `+collectionColumns+` FROM collections WHERE `+where, arg))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, NotFoundf("collection not found")
		}
		return nil, err
	}
	return c, nil
}

// List returns a page of every collection and the total count.
func (s *Collections) List(ctx context.Context, limit, offset int) ([]*Collection, int, error) {
	return s.list(ctx, `true`, limit, offset)
}

// activeCollections is the storefront's idea of a collection worth showing:
// one with something in it a shopper can actually buy. A collection holding
// nothing but drafts is a work in progress, and linking to an empty page is
// worse than not linking at all.
const activeCollections = `EXISTS (
	SELECT 1 FROM product_collections pc
	JOIN products p ON p.id = pc.product_id
	WHERE pc.collection_id = collections.id AND p.status = 'active')`

// ListActive returns the collections a storefront should show.
func (s *Collections) ListActive(ctx context.Context, limit, offset int) ([]*Collection, int, error) {
	return s.list(ctx, activeCollections, limit, offset)
}

func (s *Collections) list(ctx context.Context, where string, limit, offset int) ([]*Collection, int, error) {
	var total int
	if err := s.app.db.QueryRowContext(ctx,
		`SELECT count(*) FROM collections WHERE `+where).Scan(&total); err != nil {
		return nil, 0, err
	}
	if limit <= 0 {
		limit = DefaultLimit
	}
	rows, err := s.app.db.QueryContext(ctx,
		`SELECT `+collectionColumns+` FROM collections WHERE `+where+
			` ORDER BY position, id LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	out := []*Collection{}
	for rows.Next() {
		c, err := scanCollection(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, c)
	}
	return out, total, rows.Err()
}

// Update applies a patch.
func (s *Collections) Update(ctx context.Context, id int64, patch CollectionPatch) (*Collection, error) {
	sets, args := []string{}, []any{}
	add := func(column string, v any) {
		args = append(args, v)
		sets = append(sets, fmt.Sprintf("%s = $%d", column, len(args)))
	}
	if patch.Slug != nil {
		slug := strings.TrimSpace(*patch.Slug)
		if slug == "" {
			return nil, Validationf("slug must not be empty")
		}
		add("slug", slug)
	}
	if patch.Title != nil {
		title := strings.TrimSpace(*patch.Title)
		if title == "" {
			return nil, Validationf("title must not be empty")
		}
		add("title", title)
	}
	if patch.Description != nil {
		add("description", strings.TrimSpace(*patch.Description))
	}
	if patch.Position != nil {
		add("position", *patch.Position)
	}
	if patch.Metadata != nil {
		meta, err := patch.Metadata.value()
		if err != nil {
			return nil, Validationf("metadata is not valid JSON: %v", err)
		}
		add("metadata", meta)
	}
	if len(sets) == 0 {
		return s.Get(ctx, id)
	}
	sets = append(sets, "updated_at = now()")
	args = append(args, id)

	c, err := scanCollection(s.app.db.QueryRowContext(ctx,
		"UPDATE collections SET "+strings.Join(sets, ", ")+
			fmt.Sprintf(" WHERE id = $%d RETURNING ", len(args))+collectionColumns, args...))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, NotFoundf("collection %d does not exist", id)
		}
		return nil, translateCollectionErr(err)
	}
	return c, nil
}

// Delete removes a collection and the membership rows pointing at it. The
// products themselves are untouched: a collection groups products, it does not
// own them, and deleting "Summer sale" must not delete the summer stock.
func (s *Collections) Delete(ctx context.Context, id int64) error {
	res, err := s.app.db.ExecContext(ctx, `DELETE FROM collections WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return NotFoundf("collection %d does not exist", id)
	}
	return nil
}

// SetProductCollections replaces a product's membership with exactly
// collectionIDs, in the order given.
//
// Replace rather than merge, because the caller holds the whole list: an "add"
// endpoint quietly re-creates a membership the operator removed in another tab,
// and there is no way for them to see that it happened.
func (s *Collections) SetProductCollections(ctx context.Context, productID int64, collectionIDs []int64) error {
	ids := dedupeIDs(collectionIDs)
	return InTx(ctx, s.app.db, func(tx *sql.Tx) error {
		var exists bool
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM products WHERE id = $1)`, productID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return NotFoundf("product %d does not exist", productID)
		}
		if err := requireCollections(ctx, tx, ids); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM product_collections WHERE product_id = $1`, productID); err != nil {
			return err
		}
		for i, id := range ids {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO product_collections (product_id, collection_id, position)
				VALUES ($1, $2, $3)`, productID, id, i); err != nil {
				return err
			}
		}
		return nil
	})
}

// requireCollections rejects the whole request when any id is unknown, so a
// typo in one of five ids does not silently store the other four.
func requireCollections(ctx context.Context, tx *sql.Tx, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT id FROM collections WHERE id = ANY($1::bigint[])`, int64Array(ids))
	if err != nil {
		return err
	}
	defer rows.Close()

	found := make(map[int64]bool, len(ids))
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return err
		}
		found[id] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range ids {
		if !found[id] {
			return NotFoundf("collection %d does not exist", id)
		}
	}
	return nil
}

// ProductsInCollection returns a page of a collection's members in the order
// they were curated. Narrow it with q — Status in particular, since a
// storefront wants the active ones and the panel wants all of them.
func (s *Collections) ProductsInCollection(ctx context.Context, collectionID int64, q ProductQuery) ([]*Product, int, error) {
	q.CollectionID = collectionID
	return s.app.catalog.ListProducts(ctx, q)
}

// loadProductCollections attaches each product's collections in one query for
// a whole page, which is what keeps a catalog listing from costing a query per
// product.
func (a *App) loadProductCollections(ctx context.Context, byID map[int64]*Product, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	rows, err := a.db.QueryContext(ctx, `
		SELECT pc.product_id, c.id, c.slug, c.title
		FROM product_collections pc
		JOIN collections c ON c.id = pc.collection_id
		WHERE pc.product_id = ANY($1::bigint[])
		ORDER BY pc.position, c.id`, int64Array(ids))
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var productID int64
		var pc ProductCollection
		if err := rows.Scan(&productID, &pc.ID, &pc.Slug, &pc.Title); err != nil {
			return err
		}
		if p := byID[productID]; p != nil {
			p.Collections = append(p.Collections, pc)
		}
	}
	return rows.Err()
}

// ------------------------------------------------------------------- helpers

func normalizeCollectionInput(in *CollectionInput) error {
	in.Title = strings.TrimSpace(in.Title)
	if in.Title == "" {
		return Validationf("title is required")
	}
	if in.Slug = strings.TrimSpace(in.Slug); in.Slug == "" {
		in.Slug = slugify(in.Title)
	}
	if in.Slug == "" {
		return Validationf("slug could not be derived from the title; supply one")
	}
	in.Description = strings.TrimSpace(in.Description)
	return nil
}

// dedupeIDs keeps the first occurrence of each id. Order is the caller's
// curation, so it survives; a repeated id is an intent expressed twice, not a
// primary-key violation to report back.
func dedupeIDs(ids []int64) []int64 {
	out := make([]int64, 0, len(ids))
	seen := make(map[int64]bool, len(ids))
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

func translateCollectionErr(err error) error {
	if err != nil && strings.Contains(err.Error(), "collections_slug_key") {
		return Conflictf("that slug is already used by another collection")
	}
	return err
}

// -------------------------------------------------------------------- routes

// mountCollectionRoutes wires the collection endpoints. It is called from
// mountCoreRoutes alongside the other mount*Routes.
func (a *App) mountCollectionRoutes() {
	a.HandleFunc("GET /api/collections", a.handleListCollections)
	a.HandleFunc("GET /api/collections/{slug}", a.handleGetCollectionBySlug)

	a.HandleAdminFunc("GET /api/admin/collections", a.handleAdminListCollections, RightCatalogRead)
	a.HandleAdminFunc("POST /api/admin/collections", a.handleCreateCollection, RightCatalogWrite)
	a.HandleAdminFunc("GET /api/admin/collections/{id}", a.handleAdminGetCollection, RightCatalogRead)
	a.HandleAdminFunc("PATCH /api/admin/collections/{id}", a.handleUpdateCollection, RightCatalogWrite)
	a.HandleAdminFunc("DELETE /api/admin/collections/{id}", a.handleDeleteCollection, RightCatalogWrite)
	a.HandleAdminFunc("PUT /api/admin/products/{id}/collections", a.handleSetProductCollections, RightCatalogWrite)
}

// ------------------------------------------------------------------- public

func (a *App) handleListCollections(w http.ResponseWriter, r *http.Request) {
	limit, offset, err := Page(r)
	if err != nil {
		RespondError(w, r, err)
		return
	}
	list, total, err := a.Collections().ListActive(r.Context(), limit, offset)
	if err != nil {
		RespondError(w, r, err)
		return
	}
	RespondList(w, list, ListMeta{Total: total, Limit: limit, Offset: offset})
}

// collectionPage is the storefront's collection view: the collection, flattened
// into the response, plus the page of active products in it. One request, not
// two, because there is no useful moment at which a client has one and not the
// other.
type collectionPage struct {
	*Collection
	Products []*Product `json:"products"`
}

func (a *App) handleGetCollectionBySlug(w http.ResponseWriter, r *http.Request) {
	limit, offset, err := Page(r)
	if err != nil {
		RespondError(w, r, err)
		return
	}
	collections := a.Collections()
	c, err := collections.GetBySlug(r.Context(), r.PathValue("slug"))
	if err != nil {
		RespondError(w, r, err)
		return
	}
	products, total, err := collections.ProductsInCollection(r.Context(), c.ID, ProductQuery{
		Status: ProductActive, Limit: limit, Offset: offset,
	})
	if err != nil {
		RespondError(w, r, err)
		return
	}
	if products == nil {
		products = []*Product{}
	}
	a.translateProducts(r, products)

	// meta describes the products: the collection is a single record, and the
	// only thing on this response a client can page through is what is in it.
	RespondList(w, collectionPage{Collection: c, Products: products},
		ListMeta{Total: total, Limit: limit, Offset: offset})
}

// -------------------------------------------------------------------- admin

func (a *App) handleAdminListCollections(w http.ResponseWriter, r *http.Request) {
	limit, offset, err := Page(r)
	if err != nil {
		RespondError(w, r, err)
		return
	}
	list, total, err := a.Collections().List(r.Context(), limit, offset)
	if err != nil {
		RespondError(w, r, err)
		return
	}
	RespondList(w, list, ListMeta{Total: total, Limit: limit, Offset: offset})
}

func (a *App) handleAdminGetCollection(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		RespondError(w, r, err)
		return
	}
	c, err := a.Collections().Get(r.Context(), id)
	if err != nil {
		RespondError(w, r, err)
		return
	}
	Respond(w, http.StatusOK, c)
}

func (a *App) handleCreateCollection(w http.ResponseWriter, r *http.Request) {
	var in CollectionInput
	if err := DecodeJSON(w, r, &in); err != nil {
		RespondError(w, r, err)
		return
	}
	c, err := a.Collections().Create(r.Context(), in)
	if err != nil {
		RespondError(w, r, err)
		return
	}
	Respond(w, http.StatusCreated, c)
}

func (a *App) handleUpdateCollection(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		RespondError(w, r, err)
		return
	}
	var patch CollectionPatch
	if err := DecodeJSON(w, r, &patch); err != nil {
		RespondError(w, r, err)
		return
	}
	c, err := a.Collections().Update(r.Context(), id, patch)
	if err != nil {
		RespondError(w, r, err)
		return
	}
	Respond(w, http.StatusOK, c)
}

func (a *App) handleDeleteCollection(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		RespondError(w, r, err)
		return
	}
	if err := a.Collections().Delete(r.Context(), id); err != nil {
		RespondError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleSetProductCollections replaces the product's set and answers with the
// product, so the caller sees the membership the store now holds rather than
// the one it just asked for.
func (a *App) handleSetProductCollections(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		RespondError(w, r, err)
		return
	}
	var in struct {
		CollectionIDs []int64 `json:"collection_ids"`
	}
	if err := DecodeJSON(w, r, &in); err != nil {
		RespondError(w, r, err)
		return
	}
	if err := a.Collections().SetProductCollections(r.Context(), id, in.CollectionIDs); err != nil {
		RespondError(w, r, err)
		return
	}
	p, err := a.catalog.GetProduct(r.Context(), id)
	if err != nil {
		RespondError(w, r, err)
		return
	}
	Respond(w, http.StatusOK, p)
}

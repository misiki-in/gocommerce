package gocommerce

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// A category is where a product sits in a taxonomy: Apparel / Clothing /
// Shirts. One product, one category, one parent per category.
//
// It is not a collection, and the two are easy to confuse. A collection is a
// list somebody curated and a product can be in six of them; a category answers
// "what kind of thing is this", which has exactly one answer. Feeds, tax rules
// and shipping profiles all key off that single answer, so it has to be
// singular or none of them can be written.
type Category struct {
	ID int64 `json:"id"`
	// ParentID is nil for a root.
	ParentID *int64 `json:"parent_id"`
	Slug     string `json:"slug"`
	Title    string `json:"title"`
	Position int    `json:"position"`
	// FullName is the ancestry joined for display — "Apparel / Shirts". It is
	// computed, never stored: a stored path would need rewriting across a whole
	// subtree every time somebody renamed a parent, and the first missed rewrite
	// is a lie that never gets found.
	FullName string `json:"full_name"`
	// Depth is 0 for a root. Present so a flat listing can be indented without
	// the client rebuilding the tree.
	Depth int `json:"depth"`
	// ChildCount is how many categories sit directly under this one.
	//
	// It travels with every row so a browser can tell a branch from a leaf
	// without asking. Without it every row has to offer an expander and half of
	// them open onto nothing, which reads as broken rather than as empty.
	ChildCount int `json:"child_count"`
	// Children is populated by Tree and empty everywhere else, so a flat list
	// does not carry the same rows twice.
	Children []*Category `json:"children,omitempty"`
	// Attributes is what this category asks of a product filed under it: the
	// fields its metadata declares, with the choices the shared table holds for
	// each one. Filled in by Ancestors and nowhere else — a listing would carry
	// the same value lists on every row, which is hundreds of kilobytes of
	// repetition for a picker that never reads them.
	Attributes []CategoryAttribute `json:"attributes,omitempty"`
	Metadata   Metadata            `json:"metadata"`
	CreatedAt  time.Time           `json:"created_at"`
	UpdatedAt  time.Time           `json:"updated_at"`
}

// ProductCategory is the shape a category takes riding along on a product:
// enough to render a breadcrumb and link, and no more.
type ProductCategory struct {
	ID       int64  `json:"id"`
	Slug     string `json:"slug"`
	Title    string `json:"title"`
	FullName string `json:"full_name"`
}

// CategoryInput creates a category.
type CategoryInput struct {
	ParentID *int64   `json:"parent_id"`
	Slug     string   `json:"slug"`
	Title    string   `json:"title"`
	Position *int     `json:"position"`
	Metadata Metadata `json:"metadata"`
}

// CategoryPatch updates one. ParentID uses NullableID because moving a category
// to the root is a real edit that a plain pointer cannot express.
type CategoryPatch struct {
	ParentID NullableID `json:"parent_id"`
	Slug     *string    `json:"slug"`
	Title    *string    `json:"title"`
	Position *int       `json:"position"`
	Metadata *Metadata  `json:"metadata"`
}

// MaxCategoryDepth caps the tree. Real taxonomies run four or five levels deep;
// the limit exists so that a mistake in a bulk import cannot build a thousand-
// level chain that every recursive query then has to walk.
const MaxCategoryDepth = 8

// Categories owns the product category tree.
//
// Like collections, nothing here emits an event: filing a product under a
// different category changes what a storefront shows, not what the store owes
// anybody.
type Categories struct {
	app *App
}

// Categories returns the category service.
func (a *App) Categories() *Categories { return &Categories{app: a} }

// -------------------------------------------------------------------- service

const categoryColumns = `id, parent_id, slug, title, position, metadata, created_at, updated_at`

func scanCategory(row interface{ Scan(...any) error }) (*Category, error) {
	var c Category
	var meta []byte
	if err := row.Scan(&c.ID, &c.ParentID, &c.Slug, &c.Title, &c.Position,
		&meta, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return nil, err
	}
	if err := scanMetadata(meta, &c.Metadata); err != nil {
		return nil, err
	}
	return &c, nil
}

// Create adds a category, optionally under a parent.
func (s *Categories) Create(ctx context.Context, in CategoryInput) (*Category, error) {
	in.Title = strings.TrimSpace(in.Title)
	if in.Title == "" {
		return nil, Validationf("title is required")
	}
	if in.Slug = strings.TrimSpace(in.Slug); in.Slug == "" {
		in.Slug = slugify(in.Title)
	}
	if in.Slug == "" {
		return nil, Validationf("slug could not be derived from the title; supply one")
	}
	meta, err := in.Metadata.value()
	if err != nil {
		return nil, Validationf("metadata is not valid JSON: %v", err)
	}

	var out *Category
	err = InTx(ctx, s.app.db, func(tx *sql.Tx) error {
		if in.ParentID != nil {
			depth, err := depthOf(ctx, tx, *in.ParentID)
			if err != nil {
				return err
			}
			if depth+1 >= MaxCategoryDepth {
				return Validationf("categories nest at most %d deep", MaxCategoryDepth)
			}
		}
		position := 0
		if in.Position != nil {
			position = *in.Position
		} else {
			// Default to the end of its siblings, so creating three categories
			// in a row leaves them in the order they were created rather than
			// all sharing position 0 and falling back to id order by accident.
			if err := tx.QueryRowContext(ctx, `
				SELECT coalesce(max(position), -1) + 1 FROM categories
				WHERE parent_id IS NOT DISTINCT FROM $1`, in.ParentID).Scan(&position); err != nil {
				return err
			}
		}
		c, err := scanCategory(tx.QueryRowContext(ctx, `
			INSERT INTO categories (parent_id, slug, title, position, metadata)
			VALUES ($1, $2, $3, $4, $5) RETURNING `+categoryColumns,
			in.ParentID, in.Slug, in.Title, position, meta))
		if err != nil {
			return translateCategoryErr(err)
		}
		out = c
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.decorate(ctx, out)
}

// Get loads a category by id.
func (s *Categories) Get(ctx context.Context, id int64) (*Category, error) {
	return s.one(ctx, `id = $1`, id)
}

// GetBySlug loads a category by its URL slug.
func (s *Categories) GetBySlug(ctx context.Context, slug string) (*Category, error) {
	return s.one(ctx, `slug = $1`, strings.TrimSpace(slug))
}

func (s *Categories) one(ctx context.Context, where string, arg any) (*Category, error) {
	c, err := scanCategory(s.app.db.QueryRowContext(ctx,
		`SELECT `+categoryColumns+` FROM categories WHERE `+where, arg))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, NotFoundf("category not found")
		}
		return nil, err
	}
	return s.decorate(ctx, c)
}

// decorate fills in FullName and Depth for a single category.
func (s *Categories) decorate(ctx context.Context, c *Category) (*Category, error) {
	paths, err := categoryPaths(ctx, s.app.db, []int64{c.ID})
	if err != nil {
		return nil, err
	}
	if p, ok := paths[c.ID]; ok {
		c.FullName, c.Depth = p.fullName, p.depth
	}
	return c, nil
}

// DefaultCategoryPage caps a search. A picker shows a handful of matches and
// the operator narrows further; sending a thousand rows so a dropdown can
// render fifty is the difference between a search box and a download.
const DefaultCategoryPage = 50

// MaxWholeCategoryTree is the largest tree a listing will send in full.
//
// A few hundred categories is a hand-built taxonomy: small enough to ship, and
// an indented picker over it needs no search at all. Past that the tree is an
// import, and the listing switches to a bounded slice plus the true total. The
// number is a judgement about payload, not a limit on how many categories a
// store may have.
const MaxWholeCategoryTree = 500

// Search finds categories by name or by any part of their ancestry, so typing
// "apparel shirt" or just "shirt" both work.
//
// It exists because the tree can be very large. Importing Shopify's taxonomy
// puts fourteen thousand rows in this table, and a picker that fetched all of
// them would move two megabytes of JSON to render one dropdown. List and Tree
// stay for the small, hand-built case; anything that types into a box comes
// here instead.
func (s *Categories) Search(ctx context.Context, term string, limit int) ([]*Category, int, error) {
	term = strings.TrimSpace(term)
	// Clamped, not replaced. Folding an over-large limit back to the default
	// silently turned a request for 500 into 50, which looked like a working
	// search returning a short answer.
	if limit <= 0 {
		limit = DefaultCategoryPage
	}
	if limit > MaxLimit {
		limit = MaxLimit
	}

	where, args := "true", []any{}
	if term != "" {
		// Matching the rendered path rather than the title is what lets a
		// parent's name narrow the search: the path is built by the same
		// recursive walk that produces full_name, so what is searched is
		// exactly what is shown.
		// Both sides folded by PostgreSQL, not one by each. This cluster's
		// lower() leaves "É" alone while Go's ToLower folds it, so lowering the
		// term in Go and the path in SQL meant searching for "éclairs" against a
		// stored "Éclairs" — an exact-name search that found nothing.
		//
		// It is still accent-*sensitive*: "eclair" will not find "Éclairs". Making
		// it insensitive needs the unaccent extension, which is a deployment
		// dependency this engine does not otherwise have.
		args = append(args, "%"+term+"%")
		where = `lower(path) LIKE lower($1)`
	}

	query := `
		WITH RECURSIVE down AS (
		    SELECT id, parent_id, title::text AS path, 0 AS depth
		    FROM categories WHERE parent_id IS NULL
		  UNION ALL
		    SELECT c.id, c.parent_id, d.path || ' / ' || c.title, d.depth + 1
		    FROM categories c JOIN down d ON d.id = c.parent_id
		    WHERE d.depth < ` + fmt.Sprint(MaxCategoryDepth) + `
		)
		SELECT count(*) OVER (), ` + prefixColumns(categoryColumns, "cat") + `, down.path, down.depth
		FROM down JOIN categories cat ON cat.id = down.id
		WHERE ` + where + `
		ORDER BY down.depth, down.path
		LIMIT ` + fmt.Sprint(limit)

	rows, err := s.app.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	out := []*Category{}
	total := 0
	for rows.Next() {
		var c Category
		var meta []byte
		if err := rows.Scan(&total, &c.ID, &c.ParentID, &c.Slug, &c.Title, &c.Position,
			&meta, &c.CreatedAt, &c.UpdatedAt, &c.FullName, &c.Depth); err != nil {
			return nil, 0, err
		}
		if err := scanMetadata(meta, &c.Metadata); err != nil {
			return nil, 0, err
		}
		out = append(out, &c)
	}
	return out, total, rows.Err()
}

// Count returns how many categories exist, which is what tells a picker whether
// it can afford to load the whole tree or has to search.
func (s *Categories) Count(ctx context.Context) (int, error) {
	var n int
	err := s.app.db.QueryRowContext(ctx, `SELECT count(*) FROM categories`).Scan(&n)
	return n, err
}

// Children returns the categories directly under parentID, or the roots when it
// is nil.
//
// This is what makes a large tree browsable. Tree and List both walk the whole
// thing, which is fine for the few dozen somebody typed and impossible for the
// fourteen thousand an import leaves behind — a page that wants to show a
// hierarchy asks for one level at a time and expands on demand.
func (s *Categories) Children(ctx context.Context, parentID *int64) ([]*Category, error) {
	rows, err := s.app.db.QueryContext(ctx, `
		SELECT `+prefixColumns(categoryColumns, "c")+`,
		       (SELECT count(*) FROM categories k WHERE k.parent_id = c.id)
		FROM categories c
		WHERE c.parent_id IS NOT DISTINCT FROM $1
		ORDER BY c.position, c.id`, parentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []*Category{}
	ids := []int64{}
	for rows.Next() {
		var c Category
		var meta []byte
		if err := rows.Scan(&c.ID, &c.ParentID, &c.Slug, &c.Title, &c.Position,
			&meta, &c.CreatedAt, &c.UpdatedAt, &c.ChildCount); err != nil {
			return nil, err
		}
		if err := scanMetadata(meta, &c.Metadata); err != nil {
			return nil, err
		}
		out = append(out, &c)
		ids = append(ids, c.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// The path still travels, because a row taken out of its tree — a search
	// result, a breadcrumb — has to be able to say where it came from.
	paths, err := categoryPaths(ctx, s.app.db, ids)
	if err != nil {
		return nil, err
	}
	for _, c := range out {
		if p, ok := paths[c.ID]; ok {
			c.FullName, c.Depth = p.fullName, p.depth
		}
	}
	return out, nil
}

// Ancestors returns the chain from the root down to id, id last.
//
// A client that has one category and needs what the whole branch says about it
// cannot get there from a listing: a store with Shopify's taxonomy in it has
// fourteen thousand categories and the listing sends a page. So the chain is
// asked for by id, and comes back with each node's metadata, which is where a
// category declares the fields it asks of a product.
//
// An unknown id is not an error — it is a category that was deleted while
// somebody had the product open — and comes back as an empty chain.
func (s *Categories) Ancestors(ctx context.Context, id int64) ([]*Category, error) {
	rows, err := s.app.db.QueryContext(ctx, `
		WITH RECURSIVE up AS (
		    SELECT `+prefixColumns(categoryColumns, "c")+`, 0 AS climbed
		    FROM categories c WHERE c.id = $1
		  UNION ALL
		    SELECT `+prefixColumns(categoryColumns, "c")+`, up.climbed + 1
		    FROM up JOIN categories c ON c.id = up.parent_id
		    WHERE up.climbed < $2
		)
		SELECT id, parent_id, slug, title, position, metadata, created_at, updated_at,
		       (SELECT count(*) FROM categories k WHERE k.parent_id = up.id)
		FROM up ORDER BY climbed DESC`, id, MaxCategoryDepth)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []*Category{}
	for rows.Next() {
		var c Category
		var meta []byte
		if err := rows.Scan(&c.ID, &c.ParentID, &c.Slug, &c.Title, &c.Position,
			&meta, &c.CreatedAt, &c.UpdatedAt, &c.ChildCount); err != nil {
			return nil, err
		}
		if err := scanMetadata(meta, &c.Metadata); err != nil {
			return nil, err
		}
		// Depth falls out of the position in the chain, so the walk that
		// categoryPaths would do a second time is not worth doing.
		c.Depth = len(out)
		out = append(out, &c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	titles := make([]string, 0, len(out))
	for _, c := range out {
		titles = append(titles, c.Title)
		c.FullName = strings.Join(titles, " / ")
	}

	// The whole point of asking for the chain: what the branch asks of a
	// product, its own fields and every one it inherits, with their choices.
	if err := attachAttributes(ctx, s.app.db, out); err != nil {
		return nil, err
	}
	return out, nil
}

// List returns every category, ordered so that a parent always precedes its
// children and siblings keep their position. That is depth-first order, which
// is what an indented flat list needs — the panel renders it directly and only
// calls Tree when it needs actual nesting.
func (s *Categories) List(ctx context.Context) ([]*Category, error) {
	flat, err := s.all(ctx)
	if err != nil {
		return nil, err
	}
	roots := buildCategoryTree(flat)
	out := make([]*Category, 0, len(flat))
	var walk func(nodes []*Category)
	walk = func(nodes []*Category) {
		for _, n := range nodes {
			children := n.Children
			// A flat listing must not also carry the subtree; the same rows
			// would then appear twice in one response.
			n.Children = nil
			out = append(out, n)
			walk(children)
		}
	}
	walk(roots)
	return out, nil
}

// Tree returns the roots, each with its children nested.
func (s *Categories) Tree(ctx context.Context) ([]*Category, error) {
	flat, err := s.all(ctx)
	if err != nil {
		return nil, err
	}
	return buildCategoryTree(flat), nil
}

// all loads every category with FullName and Depth filled in.
//
// The whole table, deliberately: a taxonomy is tens or hundreds of rows, the
// picker needs all of them to draw a tree, and paging a tree gives you a
// forest of stumps.
func (s *Categories) all(ctx context.Context) ([]*Category, error) {
	rows, err := s.app.db.QueryContext(ctx,
		`SELECT `+categoryColumns+` FROM categories ORDER BY position, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []*Category{}
	ids := []int64{}
	for rows.Next() {
		c, err := scanCategory(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
		ids = append(ids, c.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	paths, err := categoryPaths(ctx, s.app.db, ids)
	if err != nil {
		return nil, err
	}
	for _, c := range out {
		if p, ok := paths[c.ID]; ok {
			c.FullName, c.Depth = p.fullName, p.depth
		}
	}
	return out, nil
}

// buildCategoryTree links a flat, position-ordered slice into roots and
// children. It mutates Children on the given nodes rather than copying, so the
// caller owns exactly one object per category.
//
// A node whose parent is missing is treated as a root. That cannot happen
// through the API — the foreign key sees to it — but a tree builder that drops
// rows it cannot place would hide a category from the picker with no error
// anywhere, and a visibly misplaced category is far easier to notice.
func buildCategoryTree(flat []*Category) []*Category {
	byID := make(map[int64]*Category, len(flat))
	for _, c := range flat {
		c.Children = nil
		byID[c.ID] = c
	}
	roots := []*Category{}
	for _, c := range flat {
		if c.ParentID != nil {
			if parent, ok := byID[*c.ParentID]; ok {
				parent.Children = append(parent.Children, c)
				continue
			}
		}
		roots = append(roots, c)
	}
	return roots
}

// Update applies a patch, including re-parenting.
func (s *Categories) Update(ctx context.Context, id int64, patch CategoryPatch) (*Category, error) {
	var out *Category
	err := InTx(ctx, s.app.db, func(tx *sql.Tx) error {
		sets, args := []string{}, []any{}
		add := func(column string, v any) {
			args = append(args, v)
			sets = append(sets, fmt.Sprintf("%s = $%d", column, len(args)))
		}
		if patch.ParentID.Present {
			if err := checkReparent(ctx, tx, id, patch.ParentID.Value); err != nil {
				return err
			}
			add("parent_id", patch.ParentID.Value)
		}
		if patch.Slug != nil {
			slug := strings.TrimSpace(*patch.Slug)
			if slug == "" {
				return Validationf("slug must not be empty")
			}
			add("slug", slug)
		}
		if patch.Title != nil {
			title := strings.TrimSpace(*patch.Title)
			if title == "" {
				return Validationf("title must not be empty")
			}
			add("title", title)
		}
		if patch.Position != nil {
			add("position", *patch.Position)
		}
		if patch.Metadata != nil {
			meta, err := patch.Metadata.value()
			if err != nil {
				return Validationf("metadata is not valid JSON: %v", err)
			}
			add("metadata", meta)
		}
		if len(sets) == 0 {
			c, err := scanCategory(tx.QueryRowContext(ctx,
				`SELECT `+categoryColumns+` FROM categories WHERE id = $1`, id))
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return NotFoundf("category %d does not exist", id)
				}
				return err
			}
			out = c
			return nil
		}
		sets = append(sets, "updated_at = now()")
		args = append(args, id)

		c, err := scanCategory(tx.QueryRowContext(ctx,
			"UPDATE categories SET "+strings.Join(sets, ", ")+
				fmt.Sprintf(" WHERE id = $%d RETURNING ", len(args))+categoryColumns, args...))
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return NotFoundf("category %d does not exist", id)
			}
			return translateCategoryErr(err)
		}
		out = c
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.decorate(ctx, out)
}

// checkReparent refuses a move that would make the tree not a tree.
//
// Two ways that happens: the new parent is the node itself or one of its own
// descendants, which closes a cycle and makes every recursive query hang; or
// the subtree being moved would end up deeper than the limit, dragging its
// children past it.
func checkReparent(ctx context.Context, tx *sql.Tx, id int64, parentID *int64) error {
	if parentID == nil {
		return nil
	}
	if *parentID == id {
		return Validationf("a category cannot be its own parent")
	}
	// Walk up from the proposed parent. Reaching id means id is above it, so
	// hanging id underneath would close the loop.
	ancestors, err := ancestorIDs(ctx, tx, *parentID)
	if err != nil {
		return err
	}
	for _, a := range ancestors {
		if a == id {
			return Validationf("a category cannot be moved inside one of its own descendants")
		}
	}
	parentDepth := len(ancestors) // ancestorIDs excludes the node itself
	subtree, err := subtreeHeight(ctx, tx, id)
	if err != nil {
		return err
	}
	if parentDepth+1+subtree >= MaxCategoryDepth {
		return Validationf("that move would nest categories more than %d deep", MaxCategoryDepth)
	}
	return nil
}

// ancestorIDs returns the ids above id, nearest first, excluding id itself.
//
// The seed row comes back with the rest and is dropped here rather than in the
// WHERE clause, because its presence is also the existence check: no rows at
// all means the id is unknown, and saying so beats letting a foreign key phrase
// it later. One query, not two — a *sql.Tx holds a single connection, so a
// second query issued while these rows are open fails with "conn busy".
func ancestorIDs(ctx context.Context, tx *sql.Tx, id int64) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, `
		WITH RECURSIVE up AS (
		    SELECT id, parent_id, 0 AS depth FROM categories WHERE id = $1
		  UNION ALL
		    SELECT c.id, c.parent_id, up.depth + 1
		    FROM up JOIN categories c ON c.id = up.parent_id
		    WHERE up.depth < $2
		)
		SELECT id, depth FROM up ORDER BY depth`, id, MaxCategoryDepth)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []int64{}
	seen := false
	for rows.Next() {
		var a int64
		var depth int
		if err := rows.Scan(&a, &depth); err != nil {
			return nil, err
		}
		seen = true
		if depth > 0 {
			out = append(out, a)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if !seen {
		return nil, NotFoundf("category %d does not exist", id)
	}
	return out, nil
}

// depthOf returns how many levels sit above id. A root is 0.
func depthOf(ctx context.Context, tx *sql.Tx, id int64) (int, error) {
	ancestors, err := ancestorIDs(ctx, tx, id)
	if err != nil {
		return 0, err
	}
	return len(ancestors), nil
}

// subtreeHeight returns how many levels hang below id. A leaf is 0.
func subtreeHeight(ctx context.Context, tx *sql.Tx, id int64) (int, error) {
	var height int
	err := tx.QueryRowContext(ctx, `
		WITH RECURSIVE down AS (
		    SELECT id, 0 AS depth FROM categories WHERE id = $1
		  UNION ALL
		    SELECT c.id, down.depth + 1
		    FROM down JOIN categories c ON c.parent_id = down.id
		    WHERE down.depth < $2
		)
		SELECT coalesce(max(depth), 0) FROM down`, id, MaxCategoryDepth).Scan(&height)
	return height, err
}

// Delete removes a category.
//
// It refuses while anything still points at it, and says what. Cascading would
// be worse in both directions: deleting "Apparel" would silently take every
// subcategory, and uncategorising forty products is a change nobody asked for
// and nobody would see.
func (s *Categories) Delete(ctx context.Context, id int64) error {
	return InTx(ctx, s.app.db, func(tx *sql.Tx) error {
		var children, products int
		if err := tx.QueryRowContext(ctx, `
			SELECT (SELECT count(*) FROM categories WHERE parent_id = $1),
			       (SELECT count(*) FROM products   WHERE category_id = $1)`,
			id).Scan(&children, &products); err != nil {
			return err
		}
		switch {
		case children > 0:
			return Conflictf("that category still has %s; move or delete them first",
				plural(children, "subcategory", "subcategories"))
		case products > 0:
			return Conflictf("that category is still used by %s; recategorise them first",
				plural(products, "product", "products"))
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM categories WHERE id = $1`, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return NotFoundf("category %d does not exist", id)
		}
		return nil
	})
}

// ------------------------------------------------------------------- reading

// categoryPath is what a recursive walk up the tree produces for one node.
type categoryPath struct {
	fullName string
	depth    int
}

// categoryPaths resolves the display ancestry of each requested id in one
// query, so a page of products costs one round trip rather than one per row.
//
// It walks upward from every seed at once and keeps, per seed, the row that got
// furthest — that row's accumulated title chain is the full path.
func categoryPaths(ctx context.Context, db *sql.DB, ids []int64) (map[int64]categoryPath, error) {
	out := map[int64]categoryPath{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := db.QueryContext(ctx, `
		WITH RECURSIVE up AS (
		    SELECT id AS seed, parent_id, title::text AS path, 0 AS depth
		    FROM categories WHERE id = ANY($1::bigint[])
		  UNION ALL
		    SELECT up.seed, c.parent_id, c.title || ' / ' || up.path, up.depth + 1
		    FROM up JOIN categories c ON c.id = up.parent_id
		    WHERE up.depth < $2
		)
		SELECT DISTINCT ON (seed) seed, path, depth
		FROM up ORDER BY seed, depth DESC`, int64Array(ids), MaxCategoryDepth)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var id int64
		var p categoryPath
		if err := rows.Scan(&id, &p.fullName, &p.depth); err != nil {
			return nil, err
		}
		out[id] = p
	}
	return out, rows.Err()
}

// loadProductCategories attaches each product's category in one query for a
// whole page.
func (a *App) loadProductCategories(ctx context.Context, byID map[int64]*Product, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	rows, err := a.db.QueryContext(ctx, `
		SELECT p.id, c.id, c.slug, c.title
		FROM products p JOIN categories c ON c.id = p.category_id
		WHERE p.id = ANY($1::bigint[])`, int64Array(ids))
	if err != nil {
		return err
	}
	defer rows.Close()

	pending := map[int64][]*Product{} // category id -> products awaiting its path
	categoryIDs := []int64{}
	for rows.Next() {
		var productID int64
		var pc ProductCategory
		if err := rows.Scan(&productID, &pc.ID, &pc.Slug, &pc.Title); err != nil {
			return err
		}
		p := byID[productID]
		if p == nil {
			continue
		}
		p.Category = &pc
		if _, seen := pending[pc.ID]; !seen {
			categoryIDs = append(categoryIDs, pc.ID)
		}
		pending[pc.ID] = append(pending[pc.ID], p)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	paths, err := categoryPaths(ctx, a.db, categoryIDs)
	if err != nil {
		return err
	}
	for id, products := range pending {
		for _, p := range products {
			p.Category.FullName = paths[id].fullName
		}
	}
	return nil
}

// categoryFilter narrows a product listing to a category and everything under
// it, given the placeholder holding the category id.
//
// Descendants are included because that is what the operator means: someone who
// picks "Apparel" wants the shirts too, and a filter that returned only products
// filed at the exact node would show an empty page for every branch category.
func categoryFilter(placeholder string) string {
	return fmt.Sprintf(`p.category_id IN (
		WITH RECURSIVE down AS (
		    SELECT id, 0 AS depth FROM categories WHERE id = %s
		  UNION ALL
		    SELECT c.id, down.depth + 1
		    FROM down JOIN categories c ON c.parent_id = down.id
		    WHERE down.depth < %d
		)
		SELECT id FROM down)`, placeholder, MaxCategoryDepth)
}

// ------------------------------------------------------------------- helpers

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

func translateCategoryErr(err error) error {
	if err == nil {
		return nil
	}
	switch msg := err.Error(); {
	case strings.Contains(msg, "categories_slug_key"):
		return Conflictf("that slug is already used by another category")
	case strings.Contains(msg, "categories_parent_id_fkey"):
		return NotFoundf("that parent category does not exist")
	case strings.Contains(msg, "categories_not_own_parent"):
		return Validationf("a category cannot be its own parent")
	}
	return err
}

// -------------------------------------------------------------------- routes

func (a *App) mountCategoryRoutes() {
	a.HandleFunc("GET /api/categories", a.handleListCategories)
	a.HandleFunc("GET /api/categories/{slug}", a.handleGetCategoryBySlug)

	a.HandleAdminFunc("GET /api/admin/categories", a.handleAdminListCategories, RightCatalogRead)
	a.HandleAdminFunc("POST /api/admin/categories", a.handleCreateCategory, RightCatalogWrite)
	a.HandleAdminFunc("GET /api/admin/categories/{id}", a.handleAdminGetCategory, RightCatalogRead)
	a.HandleAdminFunc("GET /api/admin/categories/{id}/ancestors", a.handleAdminCategoryAncestors, RightCatalogRead)
	a.HandleAdminFunc("PATCH /api/admin/categories/{id}", a.handleUpdateCategory, RightCatalogWrite)
	a.HandleAdminFunc("DELETE /api/admin/categories/{id}", a.handleDeleteCategory, RightCatalogWrite)
}

// -------------------------------------------------------------------- public

// handleListCategories serves the tree. Nested by default because a storefront
// menu is nested; ?flat=1 gives the depth-first list an indented picker wants.
func (a *App) handleListCategories(w http.ResponseWriter, r *http.Request) {
	a.respondCategories(w, r)
}

func (a *App) handleGetCategoryBySlug(w http.ResponseWriter, r *http.Request) {
	limit, offset, err := Page(r)
	if err != nil {
		RespondError(w, r, err)
		return
	}
	c, err := a.Categories().GetBySlug(r.Context(), r.PathValue("slug"))
	if err != nil {
		RespondError(w, r, err)
		return
	}
	products, total, err := a.catalog.ListProducts(r.Context(), ProductQuery{
		CategoryID: c.ID, Status: ProductActive, Limit: limit, Offset: offset,
	})
	if err != nil {
		RespondError(w, r, err)
		return
	}
	if products == nil {
		products = []*Product{}
	}
	a.translateProducts(r, products)

	RespondList(w, categoryPage{Category: c, Products: products},
		ListMeta{Total: total, Limit: limit, Offset: offset})
}

// categoryPage mirrors collectionPage: the category flattened into the
// response, plus the page of products filed under it or under anything beneath
// it. The meta describes the products, since they are the pageable part.
type categoryPage struct {
	*Category
	Products []*Product `json:"products"`
}

// --------------------------------------------------------------------- admin

func (a *App) handleAdminListCategories(w http.ResponseWriter, r *http.Request) {
	a.respondCategories(w, r)
}

func (a *App) respondCategories(w http.ResponseWriter, r *http.Request) {
	svc := a.Categories()
	q := r.URL.Query()

	// A search is a different request from a listing: it is bounded, it may
	// match anywhere in the tree, and its answer is a flat set of paths rather
	// than a shape. Importing a full taxonomy makes this the normal path.
	if term := q.Get("q"); term != "" || q.Get("search") != "" {
		if term == "" {
			term = q.Get("search")
		}
		limit, _, err := Page(r)
		if err != nil {
			RespondError(w, r, err)
			return
		}
		list, total, err := svc.Search(r.Context(), term, limit)
		if err != nil {
			RespondError(w, r, err)
			return
		}
		RespondList(w, list, ListMeta{Total: total, Limit: limit, Offset: 0})
		return
	}

	// One level of the hierarchy. This is the shape a browsable tree asks for,
	// and it is bounded by construction — a category has as many children as it
	// has, not as many as the table does.
	if parent := q.Get("parent"); parent != "" {
		var parentID *int64
		if parent != "root" {
			id, err := strconv.ParseInt(parent, 10, 64)
			if err != nil || id <= 0 {
				RespondError(w, r, Validationf("parent must be a category id or \"root\""))
				return
			}
			parentID = &id
		}
		list, err := svc.Children(r.Context(), parentID)
		if err != nil {
			RespondError(w, r, err)
			return
		}
		RespondList(w, list, ListMeta{Total: len(list), Limit: len(list), Offset: 0})
		return
	}

	// How big the tree is decides what a listing can honestly return. A
	// hand-built one goes whole; an imported taxonomy is fourteen thousand rows
	// and two megabytes of JSON, which no picker should be made to download to
	// render a dropdown.
	//
	// Rather than refuse, it sends a bounded slice and reports the real total.
	// A client that compares the two can see it holds part of the tree and
	// switch to searching, which is exactly what the panel's picker does — and
	// a client that does not compare them still gets a usable list rather than
	// a timeout.
	total, err := svc.Count(r.Context())
	if err != nil {
		RespondError(w, r, err)
		return
	}
	if total > MaxWholeCategoryTree {
		list, _, err := svc.Search(r.Context(), "", MaxLimit)
		if err != nil {
			RespondError(w, r, err)
			return
		}
		RespondList(w, list, ListMeta{Total: total, Limit: len(list), Offset: 0})
		return
	}

	var list []*Category
	// The whole tree comes back here, so the meta reports what was sent rather
	// than pretending a page was taken from it.
	if q.Get("flat") != "" {
		list, err = svc.List(r.Context())
	} else {
		list, err = svc.Tree(r.Context())
	}
	if err != nil {
		RespondError(w, r, err)
		return
	}
	RespondList(w, list, ListMeta{Total: len(list), Limit: len(list), Offset: 0})
}

func (a *App) handleAdminGetCategory(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		RespondError(w, r, err)
		return
	}
	c, err := a.Categories().Get(r.Context(), id)
	if err != nil {
		RespondError(w, r, err)
		return
	}
	Respond(w, http.StatusOK, c)
}

// handleAdminCategoryAncestors serves the branch a category sits on, root
// first. The product editor asks for it to know which fields the category
// declares, its own and every one it inherits.
func (a *App) handleAdminCategoryAncestors(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		RespondError(w, r, err)
		return
	}
	chain, err := a.Categories().Ancestors(r.Context(), id)
	if err != nil {
		RespondError(w, r, err)
		return
	}
	RespondList(w, chain, ListMeta{Total: len(chain), Limit: len(chain), Offset: 0})
}

func (a *App) handleCreateCategory(w http.ResponseWriter, r *http.Request) {
	var in CategoryInput
	if err := DecodeJSON(w, r, &in); err != nil {
		RespondError(w, r, err)
		return
	}
	c, err := a.Categories().Create(r.Context(), in)
	if err != nil {
		RespondError(w, r, err)
		return
	}
	Respond(w, http.StatusCreated, c)
}

func (a *App) handleUpdateCategory(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		RespondError(w, r, err)
		return
	}
	var patch CategoryPatch
	if err := DecodeJSON(w, r, &patch); err != nil {
		RespondError(w, r, err)
		return
	}
	c, err := a.Categories().Update(r.Context(), id, patch)
	if err != nil {
		RespondError(w, r, err)
		return
	}
	Respond(w, http.StatusOK, c)
}

func (a *App) handleDeleteCategory(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		RespondError(w, r, err)
		return
	}
	if err := a.Categories().Delete(r.Context(), id); err != nil {
		RespondError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

package gocommerce

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// The categories suite. A tree has failure modes a flat list does not: a cycle
// that hangs every recursive query, a delete that silently orphans a subtree, a
// filter that returns only the exact node when the operator meant the branch,
// and a "None" that cannot be chosen because the patch cannot say null.

// ---------------------------------------------------------------- fixtures

// categoriesApp boots an engine with the category routes mounted. Same guard as
// collectionsApp: mounting twice would panic on a duplicate ServeMux pattern.
func categoriesApp(t *testing.T) *App {
	t.Helper()
	app := newTestApp(t)
	for _, r := range app.Routes() {
		if r.Path == "/api/categories" {
			return app
		}
	}
	app.mountCategoryRoutes()
	return app
}

func newCategory(t *testing.T, app *App, in CategoryInput) *Category {
	t.Helper()
	c, err := app.Categories().Create(context.Background(), in)
	if err != nil {
		t.Fatalf("create category %q: %v", in.Title, err)
	}
	return c
}

// tree builds Apparel / Clothing / Shirts plus a Footwear sibling, which is
// enough shape to exercise depth, ancestry and descendant filtering.
func tree(t *testing.T, app *App) (apparel, clothing, shirts, footwear *Category) {
	t.Helper()
	apparel = newCategory(t, app, CategoryInput{Title: "Apparel"})
	clothing = newCategory(t, app, CategoryInput{Title: "Clothing", ParentID: &apparel.ID})
	shirts = newCategory(t, app, CategoryInput{Title: "Shirts", ParentID: &clothing.ID})
	footwear = newCategory(t, app, CategoryInput{Title: "Footwear", ParentID: &apparel.ID})
	return
}

// ------------------------------------------------------------------- CRUD

func TestCategoryCRUD(t *testing.T) {
	app := categoriesApp(t)
	ctx := context.Background()
	svc := app.Categories()

	root := newCategory(t, app, CategoryInput{
		Title: "  Apparel & Accessories  ", Metadata: Metadata{"icon": "shirt"},
	})
	if root.Slug != "apparel-accessories" {
		t.Errorf("slug = %q, want apparel-accessories", root.Slug)
	}
	if root.Title != "Apparel & Accessories" {
		t.Errorf("title = %q, want it trimmed", root.Title)
	}
	if root.ParentID != nil {
		t.Errorf("parent_id = %v, want nil for a root", root.ParentID)
	}
	if root.Depth != 0 || root.FullName != "Apparel & Accessories" {
		t.Errorf("root depth/full_name = %d/%q, want 0/its own title", root.Depth, root.FullName)
	}

	child := newCategory(t, app, CategoryInput{Title: "Shirts", ParentID: &root.ID})
	if child.Depth != 1 {
		t.Errorf("child depth = %d, want 1", child.Depth)
	}
	if child.FullName != "Apparel & Accessories / Shirts" {
		t.Errorf("child full_name = %q, want the ancestry joined", child.FullName)
	}

	bySlug, err := svc.GetBySlug(ctx, "shirts")
	if err != nil {
		t.Fatalf("GetBySlug: %v", err)
	}
	if bySlug.ID != child.ID || bySlug.FullName != child.FullName {
		t.Errorf("GetBySlug = %d/%q, want %d/%q", bySlug.ID, bySlug.FullName, child.ID, child.FullName)
	}

	updated, err := svc.Update(ctx, child.ID, CategoryPatch{Title: ptr("Tops")})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Title != "Tops" {
		t.Errorf("title = %q, want Tops", updated.Title)
	}
	// The path is computed, so a rename anywhere above shows up immediately
	// without a stored path column to rewrite.
	if updated.FullName != "Apparel & Accessories / Tops" {
		t.Errorf("full_name = %q, want it to follow the rename", updated.FullName)
	}
	if updated.Slug != "shirts" {
		t.Errorf("slug = %q after a title patch, want it unchanged", updated.Slug)
	}

	if _, err := svc.Get(ctx, 99999); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get(missing) = %v, want a not-found error", err)
	}
	if _, err := svc.Create(ctx, CategoryInput{Title: "Apparel & Accessories"}); !errors.Is(err, ErrConflict) {
		t.Errorf("duplicate slug = %v, want a conflict", err)
	}
	if _, err := svc.Create(ctx, CategoryInput{Title: "   "}); !errors.Is(err, ErrValidation) {
		t.Errorf("blank title = %v, want a validation error", err)
	}
	missing := int64(99999)
	if _, err := svc.Create(ctx, CategoryInput{Title: "Orphan", ParentID: &missing}); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown parent = %v, want a not-found error", err)
	}
}

// ------------------------------------------------------------------- shape

func TestCategoryTreeAndFlatList(t *testing.T) {
	app := categoriesApp(t)
	ctx := context.Background()
	svc := app.Categories()
	apparel, clothing, shirts, footwear := tree(t, app)

	roots, err := svc.Tree(ctx)
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}
	if len(roots) != 1 || roots[0].ID != apparel.ID {
		t.Fatalf("Tree returned %d roots, want just Apparel", len(roots))
	}
	// Children keep their creation order: Clothing was made before Footwear and
	// neither was given a position, so the default end-of-siblings position is
	// what puts them in that order rather than id order by luck.
	if len(roots[0].Children) != 2 ||
		roots[0].Children[0].ID != clothing.ID ||
		roots[0].Children[1].ID != footwear.ID {
		t.Fatalf("Apparel's children = %v, want Clothing then Footwear",
			titles(roots[0].Children))
	}
	if len(roots[0].Children[0].Children) != 1 ||
		roots[0].Children[0].Children[0].ID != shirts.ID {
		t.Errorf("Clothing's children = %v, want just Shirts", titles(roots[0].Children[0].Children))
	}

	flat, err := svc.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	// Depth-first: a parent always immediately precedes its own subtree, which
	// is what an indented picker renders straight down the page.
	want := []string{"Apparel", "Clothing", "Shirts", "Footwear"}
	if got := titles(flat); !equalStrings(got, want) {
		t.Errorf("List order = %v, want %v", got, want)
	}
	for _, c := range flat {
		if c.Children != nil {
			t.Errorf("%s carries Children in a flat list; the same rows would appear twice", c.Title)
		}
	}
	if flat[2].FullName != "Apparel / Clothing / Shirts" || flat[2].Depth != 2 {
		t.Errorf("Shirts = %q at depth %d, want the full path at depth 2", flat[2].FullName, flat[2].Depth)
	}
}

// ------------------------------------------------------------------ cycles

func TestCategoryRejectsCycles(t *testing.T) {
	app := categoriesApp(t)
	ctx := context.Background()
	svc := app.Categories()
	apparel, clothing, shirts, _ := tree(t, app)

	// Its own parent. The CHECK constraint would catch this too; the service
	// answers first so the message is about categories rather than about a
	// constraint name.
	if _, err := svc.Update(ctx, apparel.ID, CategoryPatch{ParentID: SetID(apparel.ID)}); !errors.Is(err, ErrValidation) {
		t.Errorf("self-parent = %v, want a validation error", err)
	}
	// Inside its own child, and inside its own grandchild. Both close a loop
	// that would make every recursive query walk forever.
	if _, err := svc.Update(ctx, apparel.ID, CategoryPatch{ParentID: SetID(clothing.ID)}); !errors.Is(err, ErrValidation) {
		t.Errorf("parent under its child = %v, want a validation error", err)
	}
	if _, err := svc.Update(ctx, apparel.ID, CategoryPatch{ParentID: SetID(shirts.ID)}); !errors.Is(err, ErrValidation) {
		t.Errorf("parent under its grandchild = %v, want a validation error", err)
	}

	// The tree is untouched by the refusals.
	roots, err := svc.Tree(ctx)
	if err != nil {
		t.Fatalf("Tree: %v", err)
	}
	if len(roots) != 1 || roots[0].ID != apparel.ID {
		t.Fatalf("after refused moves the tree has %d roots, want 1", len(roots))
	}

	// A legal move still works, and the paths below it follow.
	if _, err := svc.Update(ctx, shirts.ID, CategoryPatch{ParentID: SetID(apparel.ID)}); err != nil {
		t.Fatalf("legal re-parent: %v", err)
	}
	moved, err := svc.Get(ctx, shirts.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if moved.FullName != "Apparel / Shirts" || moved.Depth != 1 {
		t.Errorf("moved = %q at depth %d, want Apparel / Shirts at 1", moved.FullName, moved.Depth)
	}

	// Moving to the root is the case a plain *int64 patch could not express.
	rooted, err := svc.Update(ctx, shirts.ID, CategoryPatch{ParentID: ClearID()})
	if err != nil {
		t.Fatalf("move to root: %v", err)
	}
	if rooted.ParentID != nil || rooted.Depth != 0 {
		t.Errorf("after clearing the parent = %v at depth %d, want nil at 0", rooted.ParentID, rooted.Depth)
	}
}

func TestCategoryDepthLimit(t *testing.T) {
	app := categoriesApp(t)
	ctx := context.Background()
	svc := app.Categories()

	// Fill every legal level: MaxCategoryDepth levels occupy depths 0 through
	// MaxCategoryDepth-1.
	var parent *int64
	var deepest int64
	for i := 0; i < MaxCategoryDepth; i++ {
		c := newCategory(t, app, CategoryInput{
			Title: "Level " + string(rune('A'+i)), ParentID: parent,
		})
		deepest = c.ID
		parent = &deepest
	}
	// One more would sit at MaxCategoryDepth, which is past the last legal
	// index — the limit exists so a bad import cannot build a chain every
	// recursive query then has to walk.
	if _, err := svc.Create(ctx, CategoryInput{Title: "Too deep", ParentID: parent}); !errors.Is(err, ErrValidation) {
		t.Errorf("create past the depth limit = %v, want a validation error", err)
	}

	// The same limit applies to a move, counting the height of what is being
	// moved rather than just the node itself.
	tall := newCategory(t, app, CategoryInput{Title: "Tall"})
	mid := newCategory(t, app, CategoryInput{Title: "Tall mid", ParentID: &tall.ID})
	newCategory(t, app, CategoryInput{Title: "Tall leaf", ParentID: &mid.ID})
	if _, err := svc.Update(ctx, tall.ID, CategoryPatch{ParentID: SetID(deepest)}); !errors.Is(err, ErrValidation) {
		t.Errorf("move a three-level subtree under the deepest node = %v, want a validation error", err)
	}
}

// ------------------------------------------------------------------ delete

func TestCategoryDeleteRefusesWhileInUse(t *testing.T) {
	app := categoriesApp(t)
	ctx := context.Background()
	svc := app.Categories()
	apparel, clothing, shirts, _ := tree(t, app)

	// A parent with children: cascading would take the whole subtree with it.
	err := svc.Delete(ctx, apparel.ID)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("delete a parent = %v, want a conflict", err)
	}
	if !strings.Contains(err.Error(), "2 subcategories") {
		t.Errorf("message = %q, want it to say how many are in the way", err)
	}

	taggedProduct(t, app, "CAT-1", ProductInput{CategoryID: &shirts.ID})
	err = svc.Delete(ctx, shirts.ID)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("delete a category in use = %v, want a conflict", err)
	}
	if !strings.Contains(err.Error(), "1 product") {
		t.Errorf("message = %q, want it to count the products", err)
	}

	// Emptying it first is what makes the delete legal.
	list, _, err := app.Products().ListProducts(ctx, ProductQuery{CategoryID: shirts.ID})
	if err != nil {
		t.Fatalf("ListProducts: %v", err)
	}
	for _, p := range list {
		if _, err := app.Products().UpdateProduct(ctx, p.ID, ProductPatch{CategoryID: ClearID()}); err != nil {
			t.Fatalf("uncategorise: %v", err)
		}
	}
	if err := svc.Delete(ctx, shirts.ID); err != nil {
		t.Fatalf("delete after emptying: %v", err)
	}
	if err := svc.Delete(ctx, clothing.ID); err != nil {
		t.Fatalf("delete the now-childless parent: %v", err)
	}
}

// ----------------------------------------------------------------- product

func TestProductCategoryAssignment(t *testing.T) {
	app := categoriesApp(t)
	ctx := context.Background()
	_, _, shirts, footwear := tree(t, app)

	p := taggedProduct(t, app, "CAT-P1", ProductInput{CategoryID: &shirts.ID})
	if p.Category == nil {
		t.Fatalf("category = nil after creating with one")
	}
	if p.Category.ID != shirts.ID || p.Category.FullName != "Apparel / Clothing / Shirts" {
		t.Errorf("category = %d/%q, want Shirts with its full path", p.Category.ID, p.Category.FullName)
	}

	moved, err := app.Products().UpdateProduct(ctx, p.ID, ProductPatch{CategoryID: SetID(footwear.ID)})
	if err != nil {
		t.Fatalf("recategorise: %v", err)
	}
	if moved.Category == nil || moved.Category.ID != footwear.ID {
		t.Errorf("category = %v, want Footwear", moved.Category)
	}

	// A patch that does not mention the category leaves it alone — the case a
	// *int64 gets right and the one below is the case it cannot.
	untouched, err := app.Products().UpdateProduct(ctx, p.ID, ProductPatch{Title: ptr("Renamed")})
	if err != nil {
		t.Fatalf("unrelated patch: %v", err)
	}
	if untouched.Category == nil || untouched.Category.ID != footwear.ID {
		t.Errorf("category = %v after an unrelated patch, want it unchanged", untouched.Category)
	}

	cleared, err := app.Products().UpdateProduct(ctx, p.ID, ProductPatch{CategoryID: ClearID()})
	if err != nil {
		t.Fatalf("clear the category: %v", err)
	}
	if cleared.Category != nil {
		t.Errorf("category = %v after clearing, want nil", cleared.Category)
	}

	if _, err := app.Products().UpdateProduct(ctx, p.ID, ProductPatch{CategoryID: SetID(99999)}); !errors.Is(err, ErrNotFound) {
		t.Errorf("patch to an unknown category = %v, want a not-found error", err)
	}
}

// TestProductCategoryFilterIncludesDescendants pins the reading of the filter:
// picking a branch means everything under it, not the handful of products filed
// at the branch node itself.
func TestProductCategoryFilterIncludesDescendants(t *testing.T) {
	app := categoriesApp(t)
	ctx := context.Background()
	apparel, clothing, shirts, footwear := tree(t, app)

	taggedProduct(t, app, "CAT-F1", ProductInput{CategoryID: &shirts.ID})
	taggedProduct(t, app, "CAT-F2", ProductInput{CategoryID: &clothing.ID})
	taggedProduct(t, app, "CAT-F3", ProductInput{CategoryID: &footwear.ID})
	taggedProduct(t, app, "CAT-F4", ProductInput{})

	for _, tc := range []struct {
		name  string
		id    int64
		count int
	}{
		{"the whole branch", apparel.ID, 3},
		{"a mid branch", clothing.ID, 2},
		{"a leaf", shirts.ID, 1},
	} {
		list, total, err := app.Products().ListProducts(ctx, ProductQuery{CategoryID: tc.id})
		if err != nil {
			t.Fatalf("%s: ListProducts: %v", tc.name, err)
		}
		if total != tc.count || len(list) != tc.count {
			t.Errorf("%s: got %d of %d, want %d", tc.name, len(list), total, tc.count)
		}
	}

	// The uncategorised product is in none of them, and no filter at all
	// returns everything — which is what proves the counts above are a filter
	// working rather than a fixture that only made three products.
	_, total, err := app.Products().ListProducts(ctx, ProductQuery{})
	if err != nil {
		t.Fatalf("ListProducts: %v", err)
	}
	if total != 4 {
		t.Errorf("unfiltered total = %d, want 4", total)
	}
}

// ------------------------------------------------------------------- HTTP

func TestCategoryRoutes(t *testing.T) {
	app := categoriesApp(t)
	apparel, _, _, _ := tree(t, app)

	// The public listing nests by default and flattens on request. Both return
	// the whole tree, because a page of a tree is a forest of stumps.
	nested := do(t, app, http.MethodGet, "/api/categories")
	if nested.Code != http.StatusOK {
		t.Fatalf("GET /api/categories = %d, want 200: %s", nested.Code, nested.Body)
	}
	var nestedBody struct {
		Data []struct {
			Title    string `json:"title"`
			Children []struct {
				Title string `json:"title"`
			} `json:"children"`
		} `json:"data"`
	}
	decodeJSONBody(t, nested.Body.Bytes(), &nestedBody)
	if len(nestedBody.Data) != 1 || len(nestedBody.Data[0].Children) != 2 {
		t.Errorf("nested listing = %+v, want one root with two children", nestedBody.Data)
	}

	flat := do(t, app, http.MethodGet, "/api/categories?flat=1")
	var flatBody struct {
		Data []struct {
			Title    string `json:"title"`
			FullName string `json:"full_name"`
			Depth    int    `json:"depth"`
		} `json:"data"`
	}
	decodeJSONBody(t, flat.Body.Bytes(), &flatBody)
	if len(flatBody.Data) != 4 {
		t.Fatalf("flat listing returned %d, want 4", len(flatBody.Data))
	}
	if flatBody.Data[2].FullName != "Apparel / Clothing / Shirts" {
		t.Errorf("full_name = %q, want the joined ancestry", flatBody.Data[2].FullName)
	}

	// Admin CRUD, including the null that clears a parent over the wire — the
	// panel's "move to top level", and the reason CategoryPatch is a NullableID.
	created := do(t, app, http.MethodPost, "/api/admin/categories", withAdmin,
		jsonBody(t, map[string]any{"title": "Outerwear"}))
	if created.Code != http.StatusCreated {
		t.Fatalf("POST = %d, want 201: %s", created.Code, created.Body)
	}
	made := decodeCategory(t, created.Body.Bytes())
	target := "/api/admin/categories/" + strconv.FormatInt(made.ID, 10)

	nestUnder := do(t, app, http.MethodPatch, target, withAdmin,
		jsonBody(t, map[string]any{"parent_id": apparel.ID}))
	if nestUnder.Code != http.StatusOK {
		t.Fatalf("PATCH parent = %d, want 200: %s", nestUnder.Code, nestUnder.Body)
	}
	if under := decodeCategory(t, nestUnder.Body.Bytes()); under.ParentID == nil || *under.ParentID != apparel.ID {
		t.Errorf("parent_id = %v, want Apparel", under.ParentID)
	}

	// An omitted field and an explicit null decode to the same *int64, and
	// telling them apart is the whole reason for NullableID: this patch mentions
	// only the title and must leave the parent where it is.
	kept := do(t, app, http.MethodPatch, target, withAdmin,
		jsonBody(t, map[string]any{"title": "Coats"}))
	if still := decodeCategory(t, kept.Body.Bytes()); still.ParentID == nil || *still.ParentID != apparel.ID {
		t.Errorf("parent_id = %v after a title-only patch, want it unchanged", still.ParentID)
	}

	cleared := do(t, app, http.MethodPatch, target, withAdmin,
		jsonBody(t, map[string]any{"parent_id": nil}))
	if cleared.Code != http.StatusOK {
		t.Fatalf("PATCH parent null = %d, want 200: %s", cleared.Code, cleared.Body)
	}
	if rooted := decodeCategory(t, cleared.Body.Bytes()); rooted.ParentID != nil {
		t.Errorf("parent_id = %v after sending null, want nil", rooted.ParentID)
	}

	if got := do(t, app, http.MethodGet, "/api/admin/categories").Code; got == http.StatusOK {
		t.Errorf("unauthenticated admin listing = %d, want it refused", got)
	}
	if got := do(t, app, http.MethodDelete, target, withAdmin).Code; got != http.StatusNoContent {
		t.Errorf("DELETE = %d, want 204", got)
	}
}

// ------------------------------------------------------------------ helpers

func titles(cs []*Category) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Title)
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func decodeJSONBody(t *testing.T, raw []byte, dst any) {
	t.Helper()
	if err := json.Unmarshal(raw, dst); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
}

// decodeCategory unwraps the single-record envelope. Reading the body straight
// into a Category silently yields a zero value, which then shows up as a
// baffling "id must be a positive integer" three requests later.
func decodeCategory(t *testing.T, raw []byte) Category {
	t.Helper()
	var body struct {
		Data Category `json:"data"`
	}
	decodeJSONBody(t, raw, &body)
	if body.Data.ID == 0 {
		t.Fatalf("no category in %s", raw)
	}
	return body.Data
}

// ------------------------------------------------------------- taxonomy

// TestImportTaxonomy covers the shape of an import: the tree it builds, that a
// second run adds nothing, and that it folds into categories somebody already
// typed rather than duplicating them beside their own.
func TestImportTaxonomy(t *testing.T) {
	app := categoriesApp(t)
	ctx := context.Background()
	svc := app.Categories()

	const sample = `# Shopify Product Taxonomy - Categories: test
# Format: {GID} : {Ancestor name} > ... > {Category name}

gid://shopify/TaxonomyCategory/aa      : Apparel & Accessories
gid://shopify/TaxonomyCategory/aa-1    : Apparel & Accessories > Clothing
gid://shopify/TaxonomyCategory/aa-1-1  : Apparel & Accessories > Clothing > Shirts
gid://shopify/TaxonomyCategory/aa-2    : Apparel & Accessories > Shoes
gid://shopify/TaxonomyCategory/ap      : Animals & Pet Supplies
this line has no separator and is skipped
`

	first, err := svc.ImportTaxonomy(ctx, strings.NewReader(sample))
	if err != nil {
		t.Fatalf("ImportTaxonomy: %v", err)
	}
	if first.Created != 5 {
		t.Errorf("created = %d, want 5", first.Created)
	}
	if first.Skipped != 1 {
		t.Errorf("skipped = %d, want the malformed line counted", first.Skipped)
	}

	flat, err := svc.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []string{"Apparel & Accessories", "Clothing", "Shirts", "Shoes", "Animals & Pet Supplies"}
	if got := titles(flat); !equalStrings(got, want) {
		t.Errorf("tree = %v, want %v", got, want)
	}
	if flat[2].FullName != "Apparel & Accessories / Clothing / Shirts" || flat[2].Depth != 2 {
		t.Errorf("Shirts = %q at depth %d", flat[2].FullName, flat[2].Depth)
	}
	// The leaf carries its taxonomy id, so a row can be traced back to the file
	// it came from.
	if got := flat[2].Metadata["taxonomy_gid"]; got != "gid://shopify/TaxonomyCategory/aa-1-1" {
		t.Errorf("taxonomy_gid = %v, want the leaf's gid", got)
	}

	// Idempotent: a second run writes nothing.
	second, err := svc.ImportTaxonomy(ctx, strings.NewReader(sample))
	if err != nil {
		t.Fatalf("second import: %v", err)
	}
	if second.Created != 0 {
		t.Errorf("second run created %d rows, want none", second.Created)
	}
	again, err := svc.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(again) != len(flat) {
		t.Errorf("tree grew from %d to %d on a re-import", len(flat), len(again))
	}
}

// TestImportTaxonomyFoldsIntoExisting is the case that decides whether the
// natural key is right: a category typed by hand, in the same place and spelled
// differently only in case, must be matched rather than duplicated.
func TestImportTaxonomyFoldsIntoExisting(t *testing.T) {
	app := categoriesApp(t)
	ctx := context.Background()
	svc := app.Categories()

	mine := newCategory(t, app, CategoryInput{Title: "apparel & accessories"})
	kept := newCategory(t, app, CategoryInput{Title: "My Own Thing", ParentID: &mine.ID})

	result, err := svc.ImportTaxonomy(ctx, strings.NewReader(
		"gid://shopify/TaxonomyCategory/aa   : Apparel & Accessories\n"+
			"gid://shopify/TaxonomyCategory/aa-1 : Apparel & Accessories > Clothing\n"))
	if err != nil {
		t.Fatalf("ImportTaxonomy: %v", err)
	}
	if result.Created != 1 {
		t.Errorf("created = %d, want only Clothing", result.Created)
	}

	flat, err := svc.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(flat) != 3 {
		t.Fatalf("tree has %d categories (%v), want 3", len(flat), titles(flat))
	}
	// The hand-made branch is untouched, and the import hung off the same root.
	var clothing *Category
	for _, c := range flat {
		if c.Title == "Clothing" {
			clothing = c
		}
	}
	if clothing == nil || clothing.ParentID == nil || *clothing.ParentID != mine.ID {
		t.Errorf("Clothing = %+v, want it under the existing root", clothing)
	}
	if _, err := svc.Get(ctx, kept.ID); err != nil {
		t.Errorf("the hand-made child did not survive: %v", err)
	}
}

// TestImportTaxonomyIsCaseFoldedInGo pins the bug that a re-import found: this
// cluster's lower() leaves "É" alone while Go's folds it, so matching on a
// lower-cased path built in SQL created a second "Éclairs" beside the first.
func TestImportTaxonomyIsCaseFoldedInGo(t *testing.T) {
	app := categoriesApp(t)
	ctx := context.Background()
	svc := app.Categories()

	const accented = "gid://shopify/TaxonomyCategory/x   : Bakery > Éclairs\n"
	if _, err := svc.ImportTaxonomy(ctx, strings.NewReader(accented)); err != nil {
		t.Fatalf("first import: %v", err)
	}
	second, err := svc.ImportTaxonomy(ctx, strings.NewReader(accented))
	if err != nil {
		t.Fatalf("second import: %v", err)
	}
	if second.Created != 0 {
		t.Errorf("re-importing an accented name created %d rows, want none", second.Created)
	}
	flat, err := svc.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got := titles(flat); !equalStrings(got, []string{"Bakery", "Éclairs"}) {
		t.Errorf("tree = %v, want one Bakery and one Éclairs", got)
	}
}

// TestCategorySearch is what makes a fourteen-thousand-row tree usable: the
// picker stops downloading it and asks instead.
func TestCategorySearch(t *testing.T) {
	app := categoriesApp(t)
	ctx := context.Background()
	svc := app.Categories()
	tree(t, app)

	for _, tc := range []struct {
		term string
		want []string
	}{
		{"shirt", []string{"Shirts"}},
		// The path is searched, not just the title, so a parent's name narrows.
		{"clothing", []string{"Clothing", "Shirts"}},
		{"apparel / clothing", []string{"Clothing", "Shirts"}},
		{"SHIRTS", []string{"Shirts"}},
		{"nothing here", []string{}},
	} {
		got, _, err := svc.Search(ctx, tc.term, 0)
		if err != nil {
			t.Fatalf("Search(%q): %v", tc.term, err)
		}
		if !equalStrings(titles(got), tc.want) {
			t.Errorf("Search(%q) = %v, want %v", tc.term, titles(got), tc.want)
		}
	}

	// A match carries its path, because a bare "Shirts" does not say which.
	got, total, err := svc.Search(ctx, "shirt", 0)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(got) != 1 || got[0].FullName != "Apparel / Clothing / Shirts" {
		t.Errorf("match = %+v, want the full path", got)
	}
	if total != 1 {
		t.Errorf("total = %d, want 1", total)
	}

	// The limit bounds the rows but not the count, which is what tells a client
	// there is more than it was sent.
	page, total, err := svc.Search(ctx, "", 2)
	if err != nil {
		t.Fatalf("Search(all): %v", err)
	}
	if len(page) != 2 || total != 4 {
		t.Errorf("bounded search returned %d of %d, want 2 of 4", len(page), total)
	}
}

// TestCategoryChildren covers the one-level listing a browsable tree walks:
// only direct children, in position order, each saying how many it has of its
// own so a leaf can be told from a branch without asking.
func TestCategoryChildren(t *testing.T) {
	app := categoriesApp(t)
	ctx := context.Background()
	svc := app.Categories()
	apparel, clothing, shirts, footwear := tree(t, app)

	roots, err := svc.Children(ctx, nil)
	if err != nil {
		t.Fatalf("Children(nil): %v", err)
	}
	if got := titles(roots); !equalStrings(got, []string{"Apparel"}) {
		t.Errorf("roots = %v, want just Apparel", got)
	}
	if roots[0].ChildCount != 2 {
		t.Errorf("Apparel's child_count = %d, want 2", roots[0].ChildCount)
	}

	kids, err := svc.Children(ctx, &apparel.ID)
	if err != nil {
		t.Fatalf("Children(apparel): %v", err)
	}
	// Direct children only — Shirts is a grandchild and must not appear.
	if got := titles(kids); !equalStrings(got, []string{"Clothing", "Footwear"}) {
		t.Errorf("Apparel's children = %v, want Clothing then Footwear", got)
	}
	if kids[0].ChildCount != 1 || kids[1].ChildCount != 0 {
		t.Errorf("child counts = %d/%d, want 1 for Clothing and 0 for Footwear",
			kids[0].ChildCount, kids[1].ChildCount)
	}
	// The path travels too, so a row lifted out of its tree can still say where
	// it came from.
	if kids[0].FullName != "Apparel / Clothing" || kids[0].Depth != 1 {
		t.Errorf("Clothing = %q at depth %d", kids[0].FullName, kids[0].Depth)
	}

	leaves, err := svc.Children(ctx, &shirts.ID)
	if err != nil {
		t.Fatalf("Children(shirts): %v", err)
	}
	if len(leaves) != 0 {
		t.Errorf("a leaf reported %d children", len(leaves))
	}
	_ = clothing
	_ = footwear

	// Over the wire, including the root sentinel.
	rec := do(t, app, http.MethodGet, "/api/admin/categories?parent=root", withAdmin)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET ?parent=root = %d: %s", rec.Code, rec.Body)
	}
	var body struct {
		Data []struct {
			Title      string `json:"title"`
			ChildCount int    `json:"child_count"`
		} `json:"data"`
	}
	decodeJSONBody(t, rec.Body.Bytes(), &body)
	if len(body.Data) != 1 || body.Data[0].ChildCount != 2 {
		t.Errorf("?parent=root = %+v, want one root with two children", body.Data)
	}
	// A bad parent is refused rather than silently listed as the roots, which
	// would look like a working request returning the wrong level.
	if got := do(t, app, http.MethodGet, "/api/admin/categories?parent=nope", withAdmin).Code; got != http.StatusBadRequest {
		t.Errorf("?parent=nope = %d, want 400", got)
	}
}

// TestImportCategoryAttributes covers the whole round trip: the dictionary is
// written once, the categories learn which fields they ask for, and Ancestors
// puts the two back together with the inherited fields included.
func TestImportCategoryAttributes(t *testing.T) {
	app := categoriesApp(t)
	ctx := context.Background()
	svc := app.Categories()

	const tree = `gid://shopify/TaxonomyCategory/hb     : Health & Beauty
gid://shopify/TaxonomyCategory/hb-1   : Health & Beauty > Bath & Body
gid://shopify/TaxonomyCategory/hb-1-1 : Health & Beauty > Bath & Body > Bar Soap
`
	if _, err := svc.ImportTaxonomy(ctx, strings.NewReader(tree)); err != nil {
		t.Fatalf("ImportTaxonomy: %v", err)
	}

	const fields = `# Shopify Product Taxonomy - Category attributes: test
# Format: two sections.

age-group = Age group : Adults | Kids | All ages
scent = Scent : Citrus | Floral | Unscented
free-text = Free text :

gid://shopify/TaxonomyCategory/hb-1   : age-group
gid://shopify/TaxonomyCategory/hb-1-1 : scent , free-text , not-in-the-dictionary
gid://shopify/TaxonomyCategory/nope   : scent
this line has no separator at all
`

	got, err := svc.ImportCategoryAttributes(ctx, strings.NewReader(fields))
	if err != nil {
		t.Fatalf("ImportCategoryAttributes: %v", err)
	}
	if got.Attributes != 3 {
		t.Errorf("attributes = %d, want 3", got.Attributes)
	}
	if got.Categories != 2 {
		t.Errorf("categories = %d, want the two that exist", got.Categories)
	}
	if got.Unmatched != 1 {
		t.Errorf("unmatched = %d, want the line naming a category this store lacks", got.Unmatched)
	}
	// One malformed line, plus one handle the dictionary does not define.
	if got.Skipped != 2 {
		t.Errorf("skipped = %d, want 2", got.Skipped)
	}

	flat, err := svc.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var soap *Category
	for _, c := range flat {
		if c.Title == "Bar Soap" {
			soap = c
		}
		// The listing carries no value lists: written onto every category,
		// Shopify's come to 29MB.
		if len(c.Attributes) != 0 {
			t.Errorf("%s came out of a listing with %d attributes, want none",
				c.Title, len(c.Attributes))
		}
	}
	if soap == nil {
		t.Fatal("Bar Soap is missing from the tree")
	}

	chain, err := svc.Ancestors(ctx, soap.ID)
	if err != nil {
		t.Fatalf("Ancestors: %v", err)
	}
	if len(chain) != 3 {
		t.Fatalf("chain = %d deep, want 3", len(chain))
	}

	// Root first, and each node carries only what it declares. The product
	// editor is what unions them, so a parent's field is not copied down here.
	if len(chain[0].Attributes) != 0 {
		t.Errorf("Health & Beauty declares %d fields, want none", len(chain[0].Attributes))
	}
	if len(chain[1].Attributes) != 1 || chain[1].Attributes[0].Key != "age-group" {
		t.Errorf("Bath & Body = %+v, want just age-group", chain[1].Attributes)
	}
	if !equalStrings(chain[1].Attributes[0].Choices, []string{"Adults", "Kids", "All ages"}) {
		t.Errorf("age-group choices = %v", chain[1].Attributes[0].Choices)
	}

	leaf := chain[2].Attributes
	if len(leaf) != 2 {
		t.Fatalf("Bar Soap = %+v, want scent and free-text (the undefined handle dropped)", leaf)
	}
	if leaf[0].Key != "scent" || leaf[0].Label != "Scent" {
		t.Errorf("first leaf field = %+v", leaf[0])
	}
	if !equalStrings(leaf[0].Choices, []string{"Citrus", "Floral", "Unscented"}) {
		t.Errorf("scent choices = %v", leaf[0].Choices)
	}
	// An attribute with no published values is a free-text field, and its
	// choices must be an empty list rather than null: a client should not have
	// to tell those apart.
	if leaf[1].Key != "free-text" || leaf[1].Choices == nil || len(leaf[1].Choices) != 0 {
		t.Errorf("free-text field = %+v, want an empty choice list", leaf[1])
	}

	// Reading a category must not grow it. The choices belong to the shared
	// table and are joined on the way out, never written back into metadata.
	if raw, ok := chain[2].Metadata["attributes"]; ok {
		encoded, err := json.Marshal(raw)
		if err != nil {
			t.Fatalf("marshal metadata: %v", err)
		}
		if strings.Contains(string(encoded), "Citrus") {
			t.Errorf("metadata gained the value lists: %s", encoded)
		}
	}

	// Idempotent, like the tree import.
	again, err := svc.ImportCategoryAttributes(ctx, strings.NewReader(fields))
	if err != nil {
		t.Fatalf("second import: %v", err)
	}
	if again.Attributes != got.Attributes || again.Categories != got.Categories {
		t.Errorf("second run = %+v, want the same as the first %+v", again, got)
	}
}

// TestImportCategoryAttributesLeavesHandBuiltCategoriesAlone is the guard on
// whose data this is. A category somebody typed has no taxonomy id, so there is
// nothing to match it by, and its metadata is the operator's own.
func TestImportCategoryAttributesLeavesHandBuiltCategoriesAlone(t *testing.T) {
	app := categoriesApp(t)
	ctx := context.Background()
	svc := app.Categories()

	mine := newCategory(t, app, CategoryInput{
		Title:    "My Own Shelf",
		Metadata: Metadata{"mine": "untouched"},
	})

	const fields = `scent = Scent : Citrus

gid://shopify/TaxonomyCategory/hb-1-1 : scent
`
	if _, err := svc.ImportCategoryAttributes(ctx, strings.NewReader(fields)); err != nil {
		t.Fatalf("ImportCategoryAttributes: %v", err)
	}

	after, err := svc.Get(ctx, mine.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if after.Metadata["mine"] != "untouched" {
		t.Errorf("metadata = %v, want the operator's own key kept", after.Metadata)
	}
	if _, ok := after.Metadata["attributes"]; ok {
		t.Error("a hand-built category was given a field list it never asked for")
	}
}

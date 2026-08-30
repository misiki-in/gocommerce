package gocommerce

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

func (a *App) mountCatalogRoutes() {
	// Public catalog. Only active products are visible; a draft or archived
	// product is not found rather than forbidden, because its existence is
	// not a shopper's business.
	a.HandleFunc("GET /api/products", a.handleListProducts)
	a.HandleFunc("GET /api/products/{id}", a.handleGetProduct)
	a.HandleFunc("GET /api/products/slug/{slug}", a.handleGetProductBySlug)
	a.HandleFunc("GET /api/products/sku/{sku}", a.handleGetProductBySKU)
	// Variants are their own collection rather than a sub-path of a product.
	// "/api/products/{id}/variants" would collide with the slug and sku
	// lookups above — "/api/products/slug/variants" matches both patterns and
	// neither is more specific — and Litekart compatibility makes those
	// lookups the ones that have to keep their shape.
	a.HandleFunc("GET /api/variants", a.handleListVariants)
	a.HandleFunc("GET /api/variants/{id}", a.handleGetVariant)

	// Admin catalog.
	a.HandleAdminFunc("GET /api/admin/products", a.handleAdminListProducts, RightCatalogRead)
	a.HandleAdminFunc("POST /api/admin/products", a.handleCreateProduct, RightCatalogWrite)
	a.HandleAdminFunc("GET /api/admin/products/{id}", a.handleAdminGetProduct, RightCatalogRead)
	a.HandleAdminFunc("PATCH /api/admin/products/{id}", a.handleUpdateProduct, RightCatalogWrite)
	a.HandleAdminFunc("DELETE /api/admin/products/{id}", a.handleDeleteProduct, RightCatalogWrite)
	a.HandleAdminFunc("POST /api/admin/products/{id}/options", a.handleAddOption, RightCatalogWrite)
	// PUT replaces the whole matrix and reconciles the variants with it. That
	// is what an editor needs: renaming an axis or dropping a value is one
	// intent, and splitting it across calls leaves the product incoherent in
	// between.
	a.HandleAdminFunc("PUT /api/admin/products/{id}/options", a.handleSetOptions, RightCatalogWrite)
	a.HandleAdminFunc("POST /api/admin/products/{id}/variants", a.handleCreateVariant, RightCatalogWrite)
	a.HandleAdminFunc("PATCH /api/admin/variants/{id}", a.handleUpdateVariant, RightCatalogWrite)
	a.HandleAdminFunc("DELETE /api/admin/variants/{id}", a.handleDeleteVariant, RightCatalogWrite)
}

// ------------------------------------------------------------------- public

func (a *App) handleListProducts(w http.ResponseWriter, r *http.Request) {
	limit, offset, err := Page(r)
	if err != nil {
		RespondError(w, r, err)
		return
	}
	products, total, err := a.catalog.ListProducts(r.Context(), ProductQuery{
		Search: r.URL.Query().Get("q"),
		Status: ProductActive,
		Limit:  limit,
		Offset: offset,
	})
	if err != nil {
		RespondError(w, r, err)
		return
	}
	a.translateProducts(r, products)
	RespondList(w, products, ListMeta{Total: total, Limit: limit, Offset: offset})
}

func (a *App) handleGetProduct(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		RespondError(w, r, err)
		return
	}
	p, err := a.catalog.GetProduct(r.Context(), id)
	if err != nil {
		RespondError(w, r, err)
		return
	}
	a.respondPublicProduct(w, r, p)
}

func (a *App) handleGetProductBySlug(w http.ResponseWriter, r *http.Request) {
	p, err := a.catalog.GetProductBySlug(r.Context(), r.PathValue("slug"))
	if err != nil {
		RespondError(w, r, err)
		return
	}
	a.respondPublicProduct(w, r, p)
}

// handleGetProductBySKU resolves a variant SKU to its product. SKU is already
// the catalog's stable key for import and export, so it is the natural handle
// for a storefront deep link too.
func (a *App) handleGetProductBySKU(w http.ResponseWriter, r *http.Request) {
	v, err := a.catalog.GetVariantBySKU(r.Context(), r.PathValue("sku"))
	if err != nil {
		RespondError(w, r, err)
		return
	}
	p, err := a.catalog.GetProduct(r.Context(), v.ProductID)
	if err != nil {
		RespondError(w, r, err)
		return
	}
	a.respondPublicProduct(w, r, p)
}

func (a *App) respondPublicProduct(w http.ResponseWriter, r *http.Request, p *Product) {
	if p.Status != ProductActive {
		RespondError(w, r, NotFoundf("product not found"))
		return
	}
	a.translateProducts(r, []*Product{p})
	Respond(w, http.StatusOK, p)
}

func (a *App) handleListVariants(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("product_id")
	if raw == "" {
		RespondError(w, r, Validationf("product_id is required"))
		return
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		RespondError(w, r, Validationf("product_id must be a positive integer"))
		return
	}
	limit, offset, err := Page(r)
	if err != nil {
		RespondError(w, r, err)
		return
	}
	variants, err := a.catalog.ListVariants(r.Context(), id)
	if err != nil {
		RespondError(w, r, err)
		return
	}

	// One product's variants are a bounded set, so this pages in memory rather
	// than in SQL. What matters is that the endpoint honours the same
	// limit/offset/page contract as every other collection: a client that asks
	// for ten and silently receives fifty has no way to know it happened.
	total := len(variants)
	if offset > total {
		offset = total
	}
	end := min(offset+limit, total)
	RespondList(w, variants[offset:end], ListMeta{Total: total, Limit: limit, Offset: offset})
}

func (a *App) handleGetVariant(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		RespondError(w, r, err)
		return
	}
	v, err := a.catalog.GetVariant(r.Context(), id)
	if err != nil {
		RespondError(w, r, err)
		return
	}
	Respond(w, http.StatusOK, v)
}

// -------------------------------------------------------------------- admin

func (a *App) handleAdminListProducts(w http.ResponseWriter, r *http.Request) {
	limit, offset, err := Page(r)
	if err != nil {
		RespondError(w, r, err)
		return
	}
	q := r.URL.Query()
	status := q.Get("status")
	if status != "" && !validProductStatus(status) {
		RespondError(w, r, Validationf("status must be draft, active or archived"))
		return
	}
	categoryID, err := queryInt64(q, "category_id")
	if err != nil {
		RespondError(w, r, err)
		return
	}
	products, total, err := a.catalog.ListProducts(r.Context(), ProductQuery{
		Search:      q.Get("q"),
		Status:      status,
		Vendor:      q.Get("vendor"),
		ProductType: q.Get("product_type"),
		Tag:         q.Get("tag"),
		CategoryID:  categoryID,
		Limit:       limit,
		Offset:      offset,
	})
	if err != nil {
		RespondError(w, r, err)
		return
	}
	RespondList(w, products, ListMeta{Total: total, Limit: limit, Offset: offset})
}

func (a *App) handleAdminGetProduct(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
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

func (a *App) handleCreateProduct(w http.ResponseWriter, r *http.Request) {
	var in ProductInput
	if err := DecodeJSON(w, r, &in); err != nil {
		RespondError(w, r, err)
		return
	}
	p, err := a.catalog.CreateProduct(r.Context(), in)
	if err != nil {
		RespondError(w, r, err)
		return
	}
	Respond(w, http.StatusCreated, p)
}

func (a *App) handleUpdateProduct(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		RespondError(w, r, err)
		return
	}
	var patch ProductPatch
	if err := DecodeJSON(w, r, &patch); err != nil {
		RespondError(w, r, err)
		return
	}
	p, err := a.catalog.UpdateProduct(r.Context(), id, patch)
	if err != nil {
		RespondError(w, r, err)
		return
	}
	Respond(w, http.StatusOK, p)
}

func (a *App) handleDeleteProduct(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		RespondError(w, r, err)
		return
	}
	if err := a.catalog.DeleteProduct(r.Context(), id); err != nil {
		RespondError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleSetOptions replaces the option matrix. The response carries what
// changed alongside the product, so the panel can tell the operator which
// variants it just created or removed rather than leaving them to notice.
func (a *App) handleSetOptions(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		RespondError(w, r, err)
		return
	}
	var in OptionSet
	if err := DecodeJSON(w, r, &in); err != nil {
		RespondError(w, r, err)
		return
	}
	p, change, err := a.catalog.SetOptions(r.Context(), id, in)
	if err != nil {
		RespondError(w, r, err)
		return
	}
	Respond(w, http.StatusOK, map[string]any{"product": p, "changed": change})
}

func (a *App) handleAddOption(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		RespondError(w, r, err)
		return
	}
	var in OptionInput
	if err := DecodeJSON(w, r, &in); err != nil {
		RespondError(w, r, err)
		return
	}
	p, err := a.catalog.AddOption(r.Context(), id, in)
	if err != nil {
		RespondError(w, r, err)
		return
	}
	Respond(w, http.StatusCreated, p)
}

func (a *App) handleCreateVariant(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		RespondError(w, r, err)
		return
	}
	var in VariantInput
	if err := DecodeJSON(w, r, &in); err != nil {
		RespondError(w, r, err)
		return
	}
	v, err := a.catalog.CreateVariant(r.Context(), id, in)
	if err != nil {
		RespondError(w, r, err)
		return
	}
	Respond(w, http.StatusCreated, v)
}

func (a *App) handleUpdateVariant(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		RespondError(w, r, err)
		return
	}
	var patch VariantPatch
	if err := DecodeJSON(w, r, &patch); err != nil {
		RespondError(w, r, err)
		return
	}
	v, err := a.catalog.UpdateVariant(r.Context(), id, patch)
	if err != nil {
		RespondError(w, r, err)
		return
	}
	Respond(w, http.StatusOK, v)
}

func (a *App) handleDeleteVariant(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		RespondError(w, r, err)
		return
	}
	if err := a.catalog.DeleteVariant(r.Context(), id); err != nil {
		RespondError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ------------------------------------------------------------------ helpers

func pathInt64(r *http.Request, name string) (int64, error) {
	raw := r.PathValue(name)
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, Validationf("%s must be a positive integer", name)
	}
	return id, nil
}

// queryInt64 reads an optional positive id from the query string. Absent is 0
// and not an error; present but unparseable is an error rather than a silent 0,
// because a filter that quietly matches everything is how a typo in a category
// id turns into "why is this listing showing the whole catalogue".
func queryInt64(q url.Values, name string) (int64, error) {
	raw := strings.TrimSpace(q.Get(name))
	if raw == "" {
		return 0, nil
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, Validationf("%s must be a positive integer", name)
	}
	return id, nil
}

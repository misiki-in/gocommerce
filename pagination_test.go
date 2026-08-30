package gocommerce

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"testing"
)

// TestPaginationOverHTTP walks a real collection both ways and checks that
// page and offset describe the same windows.
func TestPaginationOverHTTP(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	const total = 12
	for i := 0; i < total; i++ {
		price := int64(1000 + i)
		stock := 5
		if _, err := app.Products().CreateProduct(ctx, ProductInput{
			Title: fmt.Sprintf("Product %02d", i), Status: ProductActive,
			SKU: fmt.Sprintf("PAGE-%02d", i), PriceMinor: &price, Stock: &stock,
		}); err != nil {
			t.Fatalf("create product %d: %v", i, err)
		}
	}

	type page struct {
		Data []struct {
			Slug string `json:"slug"`
		} `json:"data"`
		Meta ListMeta `json:"meta"`
	}
	get := func(query string) page {
		t.Helper()
		rec := do(t, app, http.MethodGet, "/api/products"+query)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /api/products%s = %d: %s", query, rec.Code, rec.Body)
		}
		var p page
		if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return p
	}

	t.Run("page numbers walk the collection", func(t *testing.T) {
		first := get("?limit=5&page=1")
		if len(first.Data) != 5 {
			t.Fatalf("page 1 returned %d items, want 5", len(first.Data))
		}
		if first.Meta.Total != total {
			t.Errorf("total = %d, want %d", first.Meta.Total, total)
		}
		if first.Meta.Page != 1 || first.Meta.TotalPages != 3 {
			t.Errorf("meta says page %d of %d, want 1 of 3", first.Meta.Page, first.Meta.TotalPages)
		}

		last := get("?limit=5&page=3")
		if len(last.Data) != 2 {
			t.Errorf("the last page returned %d items, want the remaining 2", len(last.Data))
		}
		if last.Meta.Page != 3 {
			t.Errorf("meta page = %d, want 3", last.Meta.Page)
		}

		// Past the end is empty, not an error: a client paging forward finds
		// out by getting nothing back.
		beyond := get("?limit=5&page=9")
		if len(beyond.Data) != 0 {
			t.Errorf("page 9 returned %d items, want none", len(beyond.Data))
		}
		if beyond.Meta.Total != total {
			t.Errorf("total past the end = %d, want %d", beyond.Meta.Total, total)
		}
	})

	t.Run("page and offset describe the same window", func(t *testing.T) {
		byPage := get("?limit=4&page=2")
		byOffset := get("?limit=4&offset=4")

		if len(byPage.Data) != len(byOffset.Data) {
			t.Fatalf("page 2 has %d items, offset 4 has %d", len(byPage.Data), len(byOffset.Data))
		}
		for i := range byPage.Data {
			if byPage.Data[i].Slug != byOffset.Data[i].Slug {
				t.Errorf("item %d: page gave %q, offset gave %q",
					i, byPage.Data[i].Slug, byOffset.Data[i].Slug)
			}
		}
	})

	t.Run("every page together is the whole collection, once", func(t *testing.T) {
		seen := map[string]int{}
		for p := 1; p <= 3; p++ {
			for _, item := range get(fmt.Sprintf("?limit=5&page=%d", p)).Data {
				seen[item.Slug]++
			}
		}
		if len(seen) != total {
			t.Errorf("paging saw %d distinct products, want %d", len(seen), total)
		}
		for slug, count := range seen {
			if count != 1 {
				t.Errorf("%s appeared %d times across the pages", slug, count)
			}
		}
	})

	t.Run("page wins when both are sent", func(t *testing.T) {
		both := get("?limit=4&offset=999&page=2")
		expected := get("?limit=4&offset=4")
		if len(both.Data) == 0 {
			t.Fatal("the offset was honoured instead of the page: nothing came back")
		}
		if both.Data[0].Slug != expected.Data[0].Slug {
			t.Errorf("first item = %q, want %q from page 2", both.Data[0].Slug, expected.Data[0].Slug)
		}
		if both.Meta.Offset != 4 {
			t.Errorf("meta offset = %d, want 4 — the page's offset", both.Meta.Offset)
		}
	})

	t.Run("a bad page is refused rather than guessed at", func(t *testing.T) {
		for _, query := range []string{"?page=0", "?page=-1", "?page=one"} {
			rec := do(t, app, http.MethodGet, "/api/products"+query)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("GET /api/products%s = %d, want 400", query, rec.Code)
			}
		}
	})
}

// Variants used to be the one collection that ignored the paging contract
// entirely: it answered every request with the whole set and a meta block
// claiming that was the limit. A client asking for two and silently receiving
// five has no way to discover it, which is the worst shape a bug can take.
func TestVariantListPaginates(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	sizes := []string{"XS", "S", "M", "L", "XL"}
	variants := make([]VariantInput, len(sizes))
	for i, size := range sizes {
		variants[i] = VariantInput{
			SKU:        fmt.Sprintf("PAGED-%d", i),
			PriceMinor: 1000,
			Options:    []string{size},
		}
	}
	p, err := app.Products().CreateProduct(ctx, ProductInput{
		Title:    "Paged tee",
		Status:   ProductActive,
		Options:  []OptionInput{{Name: "Size", Values: sizes}},
		Variants: variants,
	})
	if err != nil {
		t.Fatalf("create product: %v", err)
	}

	read := func(query string) ([]*Variant, ListMeta) {
		t.Helper()
		rec := do(t, app, http.MethodGet, "/api/variants?product_id="+
			strconv.FormatInt(p.ID, 10)+"&"+query)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET variants?%s = %d: %s", query, rec.Code, rec.Body)
		}
		var body struct {
			Data []*Variant `json:"data"`
			Meta ListMeta   `json:"meta"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return body.Data, body.Meta
	}

	all, meta := read("limit=100")
	if len(all) < 5 {
		t.Fatalf("expected at least 5 variants, got %d", len(all))
	}
	total := meta.Total

	page1, meta1 := read("limit=2&page=1")
	if len(page1) != 2 {
		t.Errorf("page 1 returned %d variants, want 2", len(page1))
	}
	// Total must count the whole set, not the page — otherwise a client cannot
	// tell there is more to fetch.
	if meta1.Total != total {
		t.Errorf("meta total = %d, want the full count %d", meta1.Total, total)
	}
	if meta1.Limit != 2 || meta1.Page != 1 {
		t.Errorf("meta = limit %d page %d, want limit 2 page 1", meta1.Limit, meta1.Page)
	}

	page2, _ := read("limit=2&page=2")
	if len(page2) != 2 {
		t.Fatalf("page 2 returned %d variants, want 2", len(page2))
	}
	if page1[0].ID == page2[0].ID {
		t.Error("page 2 repeated page 1 — the offset is being ignored")
	}

	// Past the end is an empty page, not an error and not a wrapped one.
	beyond, _ := read("limit=2&page=99")
	if len(beyond) != 0 {
		t.Errorf("page 99 returned %d variants, want none", len(beyond))
	}
}

// TestPaginationIsConsistentEverywhere: admin listings and module listings all
// go through the same helper, so the page view should be present on each.
func TestPaginationIsConsistentEverywhere(t *testing.T) {
	app := newTestApp(t)

	paths := []struct {
		path  string
		admin bool
	}{
		{path: "/api/products?limit=5&page=1"},
		{path: "/api/admin/products?limit=5&page=1", admin: true},
		{path: "/api/admin/orders?limit=5&page=1", admin: true},
		{path: "/api/admin/inventory/low-stock?limit=5&page=1", admin: true},
		{path: "/api/variants?product_id=1&limit=5&page=1"},
	}

	for _, tc := range paths {
		var rec = do(t, app, http.MethodGet, tc.path)
		if tc.admin {
			rec = do(t, app, http.MethodGet, tc.path, withAdmin)
		}
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d: %s", tc.path, rec.Code, rec.Body)
			continue
		}
		var body struct {
			Meta *ListMeta `json:"meta"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Errorf("GET %s: decode: %v", tc.path, err)
			continue
		}
		if body.Meta == nil {
			t.Errorf("GET %s returned no meta block", tc.path)
			continue
		}
		if body.Meta.Page != 1 {
			t.Errorf("GET %s: meta page = %d, want 1", tc.path, body.Meta.Page)
		}
		if body.Meta.Limit != 5 {
			t.Errorf("GET %s: meta limit = %d, want 5", tc.path, body.Meta.Limit)
		}
	}
}

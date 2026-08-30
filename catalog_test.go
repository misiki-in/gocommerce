package gocommerce

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// Catalog fields that are neither price nor stock but sit beside them.
// TestVariantCostAndTax covers the two fields Shopify's pricing card needs
// beside a price. Cost is nullable on purpose: an emptied box means "nobody has
// costed this", and storing zero instead would report a 100% margin.
func TestVariantCostAndTax(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()
	price := int64(2000)

	p, err := app.Products().CreateProduct(ctx, ProductInput{
		Title: "Costed tee", SKU: "COST-1", PriceMinor: &price,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	v := p.Variants[0]
	if v.Cost != nil {
		t.Errorf("a fresh variant has cost %v, want none recorded", v.Cost)
	}
	if !v.Taxable {
		t.Error("taxable defaulted to false; tax not charged is not recoverable")
	}

	updated, err := app.Products().UpdateVariant(ctx, v.ID, VariantPatch{CostMinor: SetAmount(800)})
	if err != nil {
		t.Fatalf("set cost: %v", err)
	}
	if updated.Cost == nil || updated.Cost.AmountMinor != 800 {
		t.Fatalf("cost = %v, want 800", updated.Cost)
	}
	if updated.Cost.Currency != updated.Price.Currency {
		t.Errorf("cost currency %q, want the store's %q",
			updated.Cost.Currency, updated.Price.Currency)
	}

	off := false
	updated, err = app.Products().UpdateVariant(ctx, v.ID, VariantPatch{Taxable: &off})
	if err != nil {
		t.Fatalf("clear taxable: %v", err)
	}
	if updated.Taxable {
		t.Error("taxable stayed true after being patched false")
	}
	// Clearing records "no cost", which is not a cost of zero — a zero would
	// report a 100% margin on something nobody has costed.
	cleared, err := app.Products().UpdateVariant(ctx, v.ID, VariantPatch{CostMinor: ClearAmount()})
	if err != nil {
		t.Fatalf("clear cost: %v", err)
	}
	if cleared.Cost != nil {
		t.Errorf("cost = %v after clearing, want none recorded", cleared.Cost)
	}
	if _, err := app.Products().UpdateVariant(ctx, v.ID, VariantPatch{CostMinor: SetAmount(800)}); err != nil {
		t.Fatalf("restore cost: %v", err)
	}

	// A patch that mentions neither leaves both alone.
	untouched, err := app.Products().UpdateVariant(ctx, v.ID, VariantPatch{SKU: ptr("COST-1B")})
	if err != nil {
		t.Fatalf("unrelated patch: %v", err)
	}
	if untouched.Cost == nil || untouched.Cost.AmountMinor != 800 || untouched.Taxable {
		t.Errorf("cost/taxable = %v/%v after an unrelated patch, want them unchanged",
			untouched.Cost, untouched.Taxable)
	}

	// And they arrive on creation too.
	seed := int64(500)
	no := false
	p2, err := app.Products().CreateProduct(ctx, ProductInput{
		Title: "Gift card", Status: ProductActive,
		Variants: []VariantInput{{
			SKU: "COST-2", PriceMinor: 5000, CostMinor: &seed, Taxable: &no,
		}},
	})
	if err != nil {
		t.Fatalf("create with cost: %v", err)
	}
	if got := p2.Variants[0]; got.Cost == nil || got.Cost.AmountMinor != 500 || got.Taxable {
		t.Errorf("created variant cost/taxable = %v/%v, want 500/false", got.Cost, got.Taxable)
	}
}

// ------------------------------------------------------- customs and oversell

// Country of origin and HS code are typed by people copying from documents, so
// what counts is what the engine accepts and what it stores.
func TestCustomsFieldsAreNormalized(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()
	price := int64(1000)
	stock := 5

	p, err := app.Products().CreateProduct(ctx, ProductInput{
		Title: "Customs tee", Status: ProductActive,
		SKU: "CUSTOMS-1", PriceMinor: &price, Stock: &stock,
	})
	if err != nil {
		t.Fatalf("create product: %v", err)
	}
	v := p.DefaultVariant()

	// The three spellings of one tariff number, and a lowercase country.
	for _, given := range []string{"610910", "6109.10", "6109 10"} {
		country := "gb"
		updated, err := app.Products().UpdateVariant(ctx, v.ID, VariantPatch{
			OriginCountry: &country,
			HSCode:        &given,
		})
		if err != nil {
			t.Fatalf("patch with hs_code %q: %v", given, err)
		}
		if updated.HSCode != "610910" {
			t.Errorf("hs_code %q stored as %q, want 610910", given, updated.HSCode)
		}
		if updated.OriginCountry != "GB" {
			t.Errorf("origin_country stored as %q, want GB", updated.OriginCountry)
		}
	}

	// And what it refuses, with a reason rather than a constraint violation.
	for _, bad := range []struct{ field, value string }{
		{"hs_code", "61"},
		{"hs_code", "shirt"},
		{"origin_country", "GBR"},
		{"origin_country", "12"},
	} {
		patch := VariantPatch{}
		if bad.field == "hs_code" {
			patch.HSCode = &bad.value
		} else {
			patch.OriginCountry = &bad.value
		}
		_, err := app.Products().UpdateVariant(ctx, v.ID, patch)
		if err == nil {
			t.Errorf("%s = %q was accepted", bad.field, bad.value)
			continue
		}
		if !strings.Contains(err.Error(), bad.field) {
			t.Errorf("%s = %q refused with %q, which does not name the field",
				bad.field, bad.value, err.Error())
		}
	}

	// Emptied means "not recorded", and empty is not a validation failure.
	empty := ""
	cleared, err := app.Products().UpdateVariant(ctx, v.ID, VariantPatch{
		OriginCountry: &empty, HSCode: &empty,
	})
	if err != nil {
		t.Fatalf("clear the customs fields: %v", err)
	}
	if cleared.HSCode != "" || cleared.OriginCountry != "" {
		t.Errorf("cleared to %q / %q", cleared.OriginCountry, cleared.HSCode)
	}
}

// The customs fields and the oversell switch ride the CSV like every other
// variant column: a store that keeps its catalog in a spreadsheet keeps these
// there too, and a round trip that quietly dropped them would reset them on the
// next import.
func TestProductCSVCarriesCustomsAndOversell(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	price := int64(2500)
	stock := 4
	p, err := app.Products().CreateProduct(ctx, ProductInput{
		Title: "CSV tee", Status: ProductActive,
		SKU: "CSV-CUSTOMS-1", PriceMinor: &price, Stock: &stock,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	on := true
	country, code := "PT", "6109.10"
	if _, err := app.Products().UpdateVariant(ctx, p.DefaultVariant().ID, VariantPatch{
		ContinueSelling: &on, OriginCountry: &country, HSCode: &code,
	}); err != nil {
		t.Fatalf("set the fields: %v", err)
	}

	var buf bytes.Buffer
	if err := app.Data().ExportProducts(ctx, &buf); err != nil {
		t.Fatalf("export: %v", err)
	}
	csv := buf.String()
	for _, want := range []string{"continue_selling", "origin_country", "hs_code", "PT", "610910"} {
		if !strings.Contains(csv, want) {
			t.Errorf("the export does not carry %q", want)
		}
	}

	// Reset them, then import the file back: the values must return.
	off := false
	empty := ""
	if _, err := app.Products().UpdateVariant(ctx, p.DefaultVariant().ID, VariantPatch{
		ContinueSelling: &off, OriginCountry: &empty, HSCode: &empty,
	}); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if _, err := app.Data().ImportProducts(ctx, strings.NewReader(csv), false); err != nil {
		t.Fatalf("import: %v", err)
	}
	back, err := app.Products().GetVariantBySKU(ctx, "CSV-CUSTOMS-1")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !back.ContinueSelling || back.OriginCountry != "PT" || back.HSCode != "610910" {
		t.Errorf("round trip lost them: continue_selling=%v origin=%q hs=%q",
			back.ContinueSelling, back.OriginCountry, back.HSCode)
	}

	// A file from before these columns existed still imports, and says nothing
	// about fields it does not carry — that is what keying by column name buys.
	old := "product_slug,product_title,product_status,sku,price_minor\n" +
		"csv-tee,CSV tee,active,CSV-CUSTOMS-1,2500\n"
	if _, err := app.Data().ImportProducts(ctx, strings.NewReader(old), false); err != nil {
		t.Fatalf("import an older file: %v", err)
	}
	after, err := app.Products().GetVariantBySKU(ctx, "CSV-CUSTOMS-1")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if after.OriginCountry != "" {
		t.Errorf("origin = %q after a file without the column; an absent column is a default, "+
			"and the default is empty", after.OriginCountry)
	}
}

package gocommerce

import (
	"context"
	"strconv"
	"strings"
	"testing"
)

// SetOptions is the one call that can destroy a catalog by accident: it edits
// the axes and the variants that depend on them together. These tests pin what
// survives an edit and what does not, because "my prices reset" is the kind of
// bug that is only discovered after the damage.

func optionsProduct(t *testing.T, app *App, sku string) *Product {
	t.Helper()
	price := int64(2500)
	stock := 10
	p, err := app.Products().CreateProduct(context.Background(), ProductInput{
		Title: "Tee " + sku, Status: ProductActive,
		SKU: sku, PriceMinor: &price, Stock: &stock,
	})
	if err != nil {
		t.Fatalf("create product: %v", err)
	}
	return p
}

func TestSetOptionsGeneratesTheMatrix(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()
	p := optionsProduct(t, app, "MATRIX-1")

	price := int64(1999)
	fresh, change, err := app.Products().SetOptions(ctx, p.ID, OptionSet{
		Options: []OptionSpec{
			{Name: "Size", Values: []string{"S", "M", "L"}},
			{Name: "Color", Values: []string{"Black", "White"}},
		},
		GenerateVariants: true,
		PriceMinor:       &price,
	})
	if err != nil {
		t.Fatalf("set options: %v", err)
	}

	if len(fresh.Options) != 2 {
		t.Fatalf("product has %d axes, want 2", len(fresh.Options))
	}
	// 3 sizes x 2 colours = 6 combinations. The product's original default
	// variant has no options, so it survives alongside them.
	if len(change.VariantsCreated) != 6 {
		t.Errorf("created %d variants, want 6: %v", len(change.VariantsCreated), change.VariantsCreated)
	}
	for _, v := range fresh.Variants {
		if len(v.Options) == 2 && v.Price.AmountMinor != price {
			t.Errorf("generated variant %s priced at %d, want %d", v.SKU, v.Price.AmountMinor, price)
		}
	}
}

// Renaming an axis must not touch what is being sold. A variant's price, SKU
// and stock describe the thing, not the label above it.
func TestRenamingAnAxisKeepsVariantsIntact(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()
	p := optionsProduct(t, app, "RENAME-1")

	if _, _, err := app.Products().SetOptions(ctx, p.ID, OptionSet{
		Options:          []OptionSpec{{Name: "Size", Values: []string{"S", "M"}}},
		GenerateVariants: true,
	}); err != nil {
		t.Fatalf("first set: %v", err)
	}

	before, err := app.Products().GetProduct(ctx, p.ID)
	if err != nil {
		t.Fatalf("read product: %v", err)
	}
	// Give one variant a distinctive price and some stock, so its survival is
	// visible rather than assumed.
	var target *Variant
	for i := range before.Variants {
		if len(before.Variants[i].Options) == 1 {
			target = &before.Variants[i]
			break
		}
	}
	if target == nil {
		t.Fatal("no variant with an option to test with")
	}
	newPrice := int64(4242)
	if _, err := app.Products().UpdateVariant(ctx, target.ID, VariantPatch{PriceMinor: &newPrice}); err != nil {
		t.Fatalf("reprice: %v", err)
	}
	if _, err := app.Stock().SetOnHand(ctx, target.ID, 0, 7); err != nil {
		t.Fatalf("set stock: %v", err)
	}

	// Rename the axis, keeping the same values. The id is what says "this is
	// the same axis" — without it the engine cannot tell a rename from a
	// delete-plus-add, and every variant would lose its size.
	axisID := before.Options[0].ID
	_, change, err := app.Products().SetOptions(ctx, p.ID, OptionSet{
		Options: []OptionSpec{{ID: &axisID, Name: "Größe", Values: []string{"S", "M"}}},
	})
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	if len(change.VariantsRemoved) != 0 {
		t.Errorf("renaming an axis removed variants: %v", change.VariantsRemoved)
	}

	after, err := app.Products().GetVariant(ctx, target.ID)
	if err != nil {
		t.Fatalf("the variant did not survive the rename: %v", err)
	}
	if after.Price.AmountMinor != newPrice {
		t.Errorf("price = %d after rename, want %d", after.Price.AmountMinor, newPrice)
	}
	if after.StockOnHand != 7 {
		t.Errorf("stock = %d after rename, want 7", after.StockOnHand)
	}
	if after.SKU != target.SKU {
		t.Errorf("sku = %q after rename, want %q", after.SKU, target.SKU)
	}
}

// Dropping a value takes its variants with it — that is the point — but only
// those.
func TestDroppingAValueRemovesOnlyItsVariants(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()
	p := optionsProduct(t, app, "DROP-1")

	if _, _, err := app.Products().SetOptions(ctx, p.ID, OptionSet{
		Options:          []OptionSpec{{Name: "Size", Values: []string{"S", "M", "L"}}},
		GenerateVariants: true,
	}); err != nil {
		t.Fatalf("first set: %v", err)
	}

	withL, err := app.Products().GetProduct(ctx, p.ID)
	if err != nil {
		t.Fatalf("read product: %v", err)
	}
	sizeID := withL.Options[0].ID

	fresh, change, err := app.Products().SetOptions(ctx, p.ID, OptionSet{
		Options: []OptionSpec{{ID: &sizeID, Name: "Size", Values: []string{"S", "M"}}},
	})
	if err != nil {
		t.Fatalf("drop L: %v", err)
	}

	if len(change.VariantsRemoved) != 1 {
		t.Errorf("removed %d variants, want exactly the L one: %v",
			len(change.VariantsRemoved), change.VariantsRemoved)
	}
	if len(change.ValuesRemoved) != 1 || !strings.Contains(change.ValuesRemoved[0], "L") {
		t.Errorf("values removed = %v, want Size: L", change.ValuesRemoved)
	}
	for _, v := range fresh.Variants {
		for _, opt := range v.Options {
			if opt == "L" {
				t.Errorf("variant %s still carries the dropped value L", v.SKU)
			}
		}
	}
}

// The engine resolves a variant's options by value alone, so the same value on
// two axes cannot be told apart. Refusing beats accepting and being silently
// wrong — this is the hole the panel used to paper over client-side.
func TestSetOptionsRefusesAmbiguousValues(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()
	p := optionsProduct(t, app, "AMBIG-1")

	_, _, err := app.Products().SetOptions(ctx, p.ID, OptionSet{
		Options: []OptionSpec{
			{Name: "Size", Values: []string{"Small", "Large"}},
			{Name: "Cup", Values: []string{"Small", "Grande"}},
		},
	})
	if err == nil {
		t.Fatal("accepted the same value on two axes; a variant's options could not be resolved")
	}
	if !strings.Contains(err.Error(), "Small") {
		t.Errorf("error = %q, want it to name the ambiguous value", err.Error())
	}

	// Two axes with distinct values are fine.
	if _, _, err := app.Products().SetOptions(ctx, p.ID, OptionSet{
		Options: []OptionSpec{
			{Name: "Size", Values: []string{"Small", "Large"}},
			{Name: "Cup", Values: []string{"Single", "Double"}},
		},
	}); err != nil {
		t.Errorf("distinct values across axes were refused: %v", err)
	}
}

func TestSetOptionsValidatesInput(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()
	p := optionsProduct(t, app, "VALID-1")

	for _, tc := range []struct {
		name string
		set  OptionSet
		want string
	}{
		{"no name", OptionSet{Options: []OptionSpec{{Name: "  ", Values: []string{"S"}}}}, "name"},
		{"no values", OptionSet{Options: []OptionSpec{{Name: "Size", Values: []string{" "}}}}, "no values"},
		{"duplicate axes", OptionSet{Options: []OptionSpec{
			{Name: "Size", Values: []string{"S"}},
			{Name: "size", Values: []string{"M"}},
		}}, "both called"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := app.Products().SetOptions(ctx, p.ID, tc.set); err == nil {
				t.Fatalf("accepted %s", tc.name)
			} else if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tc.want)) {
				t.Errorf("error = %q, want it to mention %q", err.Error(), tc.want)
			}
		})
	}

	// Duplicate values within one axis are de-duplicated rather than refused:
	// typing "S, S, M" is a slip, not a decision.
	fresh, _, err := app.Products().SetOptions(ctx, p.ID, OptionSet{
		Options: []OptionSpec{{Name: "Size", Values: []string{"S", " S ", "M"}}},
	})
	if err != nil {
		t.Fatalf("de-duplication: %v", err)
	}
	if len(fresh.Options) != 1 || len(fresh.Options[0].Values) != 2 {
		t.Errorf("axis has %d values, want 2 after de-duplication", len(fresh.Options[0].Values))
	}
}

func TestSetOptionsOverHTTP(t *testing.T) {
	app := newTestApp(t)
	p := optionsProduct(t, app, "HTTP-OPT-1")

	rec := do(t, app, "PUT", "/api/admin/products/"+strconv.FormatInt(p.ID, 10)+"/options", withAdmin,
		jsonBody(t, map[string]any{
			"options": []map[string]any{
				{"name": "Size", "values": []string{"S", "M"}},
			},
			"generate_variants": true,
		}))
	if rec.Code != 200 {
		t.Fatalf("PUT options = %d: %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"changed"`) || !strings.Contains(body, `"product"`) {
		t.Errorf("response should carry both the product and what changed: %s", body)
	}
	if !strings.Contains(body, "variants_created") {
		t.Errorf("response does not report the generated variants: %s", body)
	}

	// It needs admin auth like everything else under /api/admin.
	rec = do(t, app, "PUT", "/api/admin/products/"+strconv.FormatInt(p.ID, 10)+"/options",
		jsonBody(t, map[string]any{"options": []map[string]any{{"name": "Size", "values": []string{"S"}}}}))
	if rec.Code != 401 {
		t.Errorf("unauthenticated = %d, want 401", rec.Code)
	}
}

// Deleting an axis collapses the variants under it onto each other: without
// Colour, S/Red and S/Blue are both just S. The engine used to leave that to
// the unique index on option_key, which meant the operator pressed "Save
// options" and got an internal error instead of a deleted option.
func TestDroppingAnAxisMergesTheVariantsItCollapses(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()
	p := optionsProduct(t, app, "MERGE-1")

	if _, _, err := app.Products().SetOptions(ctx, p.ID, OptionSet{
		Options: []OptionSpec{
			{Name: "Size", Values: []string{"S", "M"}},
			{Name: "Colour", Values: []string{"Red", "Blue"}},
		},
		GenerateVariants: true,
	}); err != nil {
		t.Fatalf("first set: %v", err)
	}

	before, err := app.Products().GetProduct(ctx, p.ID)
	if err != nil {
		t.Fatalf("read product: %v", err)
	}
	sizeID := before.Options[0].ID

	// The survivor is the first of each collapsed group in the catalog's own
	// order, and it keeps what it was selling for.
	survivors := map[string]*Variant{}
	for i := range before.Variants {
		size := ""
		for _, opt := range before.Variants[i].Options {
			if opt == "S" || opt == "M" {
				size = opt
			}
		}
		if size == "" {
			continue
		}
		if _, seen := survivors[size]; !seen {
			survivors[size] = &before.Variants[i]
		}
	}
	if len(survivors) != 2 {
		t.Fatalf("expected variants on both sizes to start with, got %d", len(survivors))
	}
	price := int64(3131)
	if _, err := app.Products().UpdateVariant(ctx, survivors["S"].ID, VariantPatch{PriceMinor: &price}); err != nil {
		t.Fatalf("reprice: %v", err)
	}

	fresh, change, err := app.Products().SetOptions(ctx, p.ID, OptionSet{
		Options: []OptionSpec{{ID: &sizeID, Name: "Size", Values: []string{"S", "M"}}},
	})
	if err != nil {
		t.Fatalf("drop the colour axis: %v", err)
	}

	if len(change.AxesRemoved) != 1 || change.AxesRemoved[0] != "Colour" {
		t.Errorf("axes removed = %v, want Colour", change.AxesRemoved)
	}
	// Two of the four colour variants are merged away, and the operator is told
	// which — a deletion nobody is told about is the one that gets noticed in
	// the order that fails.
	if len(change.VariantsRemoved) != 2 {
		t.Errorf("removed %d variants, want the 2 duplicates: %v",
			len(change.VariantsRemoved), change.VariantsRemoved)
	}
	for _, sku := range change.VariantsRemoved {
		if sku == survivors["S"].SKU || sku == survivors["M"].SKU {
			t.Errorf("removed %s, which was first in its group and should have been kept", sku)
		}
	}

	sizes := map[string]int{}
	for _, v := range fresh.Variants {
		for _, opt := range v.Options {
			sizes[opt]++
		}
	}
	if sizes["S"] != 1 || sizes["M"] != 1 {
		t.Errorf("variants per size = %v, want exactly one each", sizes)
	}

	kept, err := app.Products().GetVariant(ctx, survivors["S"].ID)
	if err != nil {
		t.Fatalf("the survivor did not survive: %v", err)
	}
	if kept.Price.AmountMinor != price {
		t.Errorf("survivor price = %d, want %d", kept.Price.AmountMinor, price)
	}
}

// Removing the last axis leaves the product sold as one thing, which is one
// variant — the same merge, all the way down.
func TestDroppingEveryAxisLeavesOneVariant(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()
	p := optionsProduct(t, app, "MERGE-2")

	if _, _, err := app.Products().SetOptions(ctx, p.ID, OptionSet{
		Options:          []OptionSpec{{Name: "Size", Values: []string{"S", "M", "L"}}},
		GenerateVariants: true,
	}); err != nil {
		t.Fatalf("first set: %v", err)
	}

	fresh, _, err := app.Products().SetOptions(ctx, p.ID, OptionSet{Options: nil})
	if err != nil {
		t.Fatalf("clear the options: %v", err)
	}
	if len(fresh.Options) != 0 {
		t.Errorf("product kept %d axes", len(fresh.Options))
	}
	if len(fresh.Variants) != 1 {
		t.Errorf("product has %d variants with no options, want 1", len(fresh.Variants))
	}
}

// Retyping a value in a different case is the same value — normalizeOptionValues
// folds them — so the variants holding it must be re-linked, not orphaned onto a
// value row that does not exist.
func TestRecasingAValueKeepsItsVariants(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()
	p := optionsProduct(t, app, "CASE-1")

	if _, _, err := app.Products().SetOptions(ctx, p.ID, OptionSet{
		Options:          []OptionSpec{{Name: "Size", Values: []string{"S", "M"}}},
		GenerateVariants: true,
	}); err != nil {
		t.Fatalf("first set: %v", err)
	}
	before, err := app.Products().GetProduct(ctx, p.ID)
	if err != nil {
		t.Fatalf("read product: %v", err)
	}
	sizeID := before.Options[0].ID

	_, change, err := app.Products().SetOptions(ctx, p.ID, OptionSet{
		Options: []OptionSpec{{ID: &sizeID, Name: "size", Values: []string{"s", "m"}}},
	})
	if err != nil {
		t.Fatalf("recase the values: %v", err)
	}
	if len(change.VariantsRemoved) != 0 {
		t.Errorf("recasing removed variants: %v", change.VariantsRemoved)
	}
	if len(change.ValuesRemoved) != 0 || len(change.ValuesAdded) != 0 {
		t.Errorf("recasing counted as a value change: removed %v, added %v",
			change.ValuesRemoved, change.ValuesAdded)
	}
	for i := range before.Variants {
		if _, err := app.Products().GetVariant(ctx, before.Variants[i].ID); err != nil {
			t.Errorf("variant %s did not survive the recase: %v", before.Variants[i].SKU, err)
		}
	}
}

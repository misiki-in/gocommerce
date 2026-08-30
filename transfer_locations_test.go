package gocommerce

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// A CSV is a document people edit and mail to each other, so the format has two
// obligations that pull against each other: it has to express stock in more than
// one place, and it has to keep meaning what it meant to every file and script
// written before locations existed. These are the tests for both halves.

func exportCSV(t *testing.T, app *App) string {
	t.Helper()
	var buf bytes.Buffer
	if err := app.Data().ExportProducts(context.Background(), &buf); err != nil {
		t.Fatalf("export: %v", err)
	}
	return buf.String()
}

func headerOf(csv string) string {
	line, _, _ := strings.Cut(csv, "\n")
	return strings.TrimSpace(line)
}

// column returns one field of the row whose first cell chain contains sku.
func column(t *testing.T, csv, sku, col string) string {
	t.Helper()
	lines := strings.Split(strings.ReplaceAll(csv, "\r\n", "\n"), "\n")
	head := strings.Split(lines[0], ",")
	idx := -1
	for i, h := range head {
		if strings.TrimSpace(h) == col {
			idx = i
		}
	}
	if idx < 0 {
		t.Fatalf("no column %q in header %q", col, lines[0])
	}
	for _, line := range lines[1:] {
		fields := strings.Split(line, ",")
		for _, f := range fields {
			if f == sku {
				if idx >= len(fields) {
					t.Fatalf("row for %s has %d fields, wanted index %d", sku, len(fields), idx)
				}
				return fields[idx]
			}
		}
	}
	t.Fatalf("no row for %s in\n%s", sku, csv)
	return ""
}

func TestASingleLocationStoreExportsTheOldHeader(t *testing.T) {
	app := newTestApp(t)

	// The whole point of the default location is that a store which has never
	// heard of locations cannot tell it exists. That includes its spreadsheets.
	simpleProduct(t, app, "CSV-ONE", 1000, 9)
	csv := exportCSV(t, app)

	if got, want := headerOf(csv), strings.Join(productCSVHeader, ","); got != want {
		t.Errorf("header = %q,\nwant   %q", got, want)
	}
	if got := column(t, csv, "CSV-ONE", "stock_on_hand"); got != "9" {
		t.Errorf("stock_on_hand = %q, want 9", got)
	}
}

func TestExportSplitsStockAcrossLocationColumns(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	p := simpleProduct(t, app, "CSV-SPLIT", 1000, 7)
	vid := p.DefaultVariant().ID
	shop := newLocation(t, app, "shop", "The shop", 1)
	def := defaultLocation(t, app)
	if _, err := app.Stock().Move(ctx, vid, def.ID, shop.ID, 3); err != nil {
		t.Fatalf("move: %v", err)
	}

	csv := exportCSV(t, app)
	head := headerOf(csv)
	if strings.Contains(head, ",stock_on_hand,") {
		t.Errorf("header still carries a bare stock_on_hand column: %q", head)
	}
	for _, want := range []string{"stock_on_hand:default", "stock_on_hand:shop"} {
		if !strings.Contains(head, want) {
			t.Errorf("header is missing %q: %q", want, head)
		}
	}
	if got := column(t, csv, "CSV-SPLIT", "stock_on_hand:default"); got != "4" {
		t.Errorf("default column = %q, want 4", got)
	}
	if got := column(t, csv, "CSV-SPLIT", "stock_on_hand:shop"); got != "3" {
		t.Errorf("shop column = %q, want 3", got)
	}
}

func TestAnEmptyLocationStillGetsAColumn(t *testing.T) {
	app := newTestApp(t)

	// A column per location, not a column per location that happens to hold
	// something: the header would otherwise change shape as stock moved, and
	// there would be no cell to type into to receive stock somewhere empty.
	simpleProduct(t, app, "CSV-EMPTY", 1000, 4)
	newLocation(t, app, "overflow", "Overflow unit", 9)

	csv := exportCSV(t, app)
	if !strings.Contains(headerOf(csv), "stock_on_hand:overflow") {
		t.Errorf("an empty location got no column: %q", headerOf(csv))
	}
	if got := column(t, csv, "CSV-EMPTY", "stock_on_hand:overflow"); got != "0" {
		t.Errorf("empty location column = %q, want 0", got)
	}
}

func TestASplitFileRoundTrips(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	p := simpleProduct(t, app, "CSV-TRIP", 1000, 10)
	vid := p.DefaultVariant().ID
	shop := newLocation(t, app, "shop", "The shop", 1)
	def := defaultLocation(t, app)
	if _, err := app.Stock().Move(ctx, vid, def.ID, shop.ID, 6); err != nil {
		t.Fatalf("move: %v", err)
	}
	csv := exportCSV(t, app)

	// Flatten it back into one place, then import the file: the split has to
	// come back exactly, because that is the entire promise of the format.
	if _, err := app.Stock().Move(ctx, vid, shop.ID, def.ID, 6); err != nil {
		t.Fatalf("flatten: %v", err)
	}
	if _, err := app.Data().ImportProducts(ctx, strings.NewReader(csv), false); err != nil {
		t.Fatalf("import: %v", err)
	}

	if onHand, _ := stockAt(t, app, vid, def.ID); onHand != 4 {
		t.Errorf("default holds %d after the round trip, want 4", onHand)
	}
	if onHand, _ := stockAt(t, app, vid, shop.ID); onHand != 6 {
		t.Errorf("shop holds %d after the round trip, want 6", onHand)
	}
	if total, _ := variantStock(t, app, vid); total != 10 {
		t.Errorf("store total = %d, want 10", total)
	}
}

func TestABareStockColumnStillMeansTheDefault(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	// A file written years ago, or by a script that has never been told about
	// locations, has to keep working — even in a store that has since split its
	// stock across several.
	p := simpleProduct(t, app, "CSV-OLD", 1000, 2)
	vid := p.DefaultVariant().ID
	shop := newLocation(t, app, "shop", "The shop", 1)
	if _, err := app.Stock().Adjust(ctx, vid, shop.ID, 5); err != nil {
		t.Fatalf("stock the shop: %v", err)
	}

	old := "product_slug,product_title,product_status,sku,price_minor,stock_on_hand\n" +
		"test-csv-old,Test CSV-OLD,active,CSV-OLD,1000,11\n"
	if _, err := app.Data().ImportProducts(ctx, strings.NewReader(old), false); err != nil {
		t.Fatalf("import: %v", err)
	}

	if onHand, _ := stockAt(t, app, vid, defaultLocation(t, app).ID); onHand != 11 {
		t.Errorf("default holds %d, want 11 — the bare column means the default", onHand)
	}
	if onHand, _ := stockAt(t, app, vid, shop.ID); onHand != 5 {
		t.Errorf("shop holds %d, want 5 — a file that says nothing about a location "+
			"must not empty it", onHand)
	}
}

func TestAnAbsentColumnSaysNothingAboutThatLocation(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	p := simpleProduct(t, app, "CSV-PART", 1000, 3)
	vid := p.DefaultVariant().ID
	shop := newLocation(t, app, "shop", "The shop", 1)
	if _, err := app.Stock().Adjust(ctx, vid, shop.ID, 8); err != nil {
		t.Fatalf("stock the shop: %v", err)
	}

	// A file naming one of the two locations. The other must be untouched, and
	// so must a row whose cell is blank.
	partial := "product_slug,sku,price_minor,stock_on_hand:shop\n" +
		"test-csv-part,CSV-PART,1000,2\n"
	if _, err := app.Data().ImportProducts(ctx, strings.NewReader(partial), false); err != nil {
		t.Fatalf("import: %v", err)
	}
	if onHand, _ := stockAt(t, app, vid, shop.ID); onHand != 2 {
		t.Errorf("shop holds %d, want 2", onHand)
	}
	if onHand, _ := stockAt(t, app, vid, defaultLocation(t, app).ID); onHand != 3 {
		t.Errorf("default holds %d, want 3 — the file never mentioned it", onHand)
	}

	blank := "product_slug,sku,price_minor,stock_on_hand:shop\n" +
		"test-csv-part,CSV-PART,1000,\n"
	if _, err := app.Data().ImportProducts(ctx, strings.NewReader(blank), false); err != nil {
		t.Fatalf("import a blank cell: %v", err)
	}
	if onHand, _ := stockAt(t, app, vid, shop.ID); onHand != 2 {
		t.Errorf("shop holds %d after a blank cell, want 2 — an empty cell is not a zero", onHand)
	}
}

func TestAMisspeltLocationFailsTheWholeFile(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	// One mistake in one header cell affects every line, so it is worth one
	// sentence rather than a row error per line burying it.
	simpleProduct(t, app, "CSV-TYPO", 1000, 1)
	bad := "product_slug,sku,price_minor,stock_on_hand:warehowse\n" +
		"test-csv-typo,CSV-TYPO,1000,4\n"

	_, err := app.Data().ImportProducts(ctx, strings.NewReader(bad), false)
	if err == nil {
		t.Fatal("imported a file naming a location that does not exist")
	}
	if !strings.Contains(err.Error(), "warehowse") {
		t.Errorf("the error does not name the bad code: %v", err)
	}
}

func TestBothStockColumnFormsAtOnceIsRefused(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	// There is no answer to which one a row means, and picking one silently
	// would put the units somewhere the operator did not ask for.
	simpleProduct(t, app, "CSV-BOTH", 1000, 1)
	newLocation(t, app, "shop", "The shop", 1)
	bad := "product_slug,sku,price_minor,stock_on_hand,stock_on_hand:shop\n" +
		"test-csv-both,CSV-BOTH,1000,4,5\n"

	if _, err := app.Data().ImportProducts(ctx, strings.NewReader(bad), false); err == nil {
		t.Fatal("imported a file carrying both stock column forms")
	}
}

func TestAnImportCannotDropStockBelowWhatIsReserved(t *testing.T) {
	app := newTestApp(t, &gatewayModule{})
	ctx := context.Background()

	// A count taken on the shop floor does not know about the order that came in
	// while it was being taken. The reservation wins; the file loses.
	p := simpleProduct(t, app, "CSV-HELD", 1000, 5)
	vid := p.DefaultVariant().ID
	hold(t, app, vid, 4)

	file := "product_slug,sku,price_minor,stock_on_hand\n" +
		"test-csv-held,CSV-HELD,1000,1\n"
	if _, err := app.Data().ImportProducts(ctx, strings.NewReader(file), false); err != nil {
		t.Fatalf("import: %v", err)
	}

	onHand, reserved := stockAt(t, app, vid, defaultLocation(t, app).ID)
	if onHand != 4 || reserved != 4 {
		t.Errorf("stock = (%d, %d), want (4, 4) — the import may not go below the reservation",
			onHand, reserved)
	}
}

func TestARoundTripDoesNotWriteOffAnOversell(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	// A variant that sells past zero is *meant* to go negative: -2 is two units
	// owed to customers who have already ordered. Exporting the catalog, editing
	// a price and importing it back is the most ordinary thing an operator does
	// with this format, and it must not quietly cancel that debt.
	p := simpleProduct(t, app, "CSV-OWED", 1000, 0)
	vid := p.DefaultVariant().ID
	on := true
	if _, err := app.Products().UpdateVariant(ctx, vid,
		VariantPatch{ContinueSelling: &on}); err != nil {
		t.Fatalf("allow overselling: %v", err)
	}
	buy(t, app, vid, 2)

	if onHand, _ := variantStock(t, app, vid); onHand != -2 {
		t.Fatalf("on hand = %d before the round trip, want -2", onHand)
	}

	csv := exportCSV(t, app)
	if _, err := app.Data().ImportProducts(ctx, strings.NewReader(csv), false); err != nil {
		t.Fatalf("import: %v", err)
	}

	if onHand, _ := variantStock(t, app, vid); onHand != -2 {
		t.Errorf("on hand = %d after re-importing the export, want -2 — the round trip "+
			"wrote off %d unit(s) the store owes", onHand, onHand+2)
	}

	// The floor still applies to a variant that does not sell past zero.
	q := simpleProduct(t, app, "CSV-FLOOR", 1000, 5)
	qid := q.DefaultVariant().ID
	file := "product_slug,sku,price_minor,stock_on_hand\n" +
		"test-csv-floor,CSV-FLOOR,1000,-3\n"
	if _, err := app.Data().ImportProducts(ctx, strings.NewReader(file), false); err != nil {
		t.Fatalf("import a negative for a tracked variant: %v", err)
	}
	if onHand, _ := variantStock(t, app, qid); onHand != 0 {
		t.Errorf("on hand = %d, want 0 — a variant that does not sell past zero is floored",
			onHand)
	}
}

package gocommerce

import (
	"context"
	"testing"
)

// The invariant every one of these is defending: a variant's stock_on_hand and
// stock_reserved mean exactly what they meant before M17 — the store has this
// many — while the per-location rows underneath are what actually moves. If a
// client cannot tell the difference, the split is done right.

func newLocation(t *testing.T, app *App, code, name string, priority int) *Location {
	t.Helper()
	l, err := app.Places().Create(context.Background(), LocationInput{
		Code: code, Name: name, Priority: &priority,
	})
	if err != nil {
		t.Fatalf("create location %s: %v", code, err)
	}
	return l
}

func defaultLocation(t *testing.T, app *App) *Location {
	t.Helper()
	list, err := app.Places().List(context.Background())
	if err != nil {
		t.Fatalf("list locations: %v", err)
	}
	for _, l := range list {
		if l.IsDefault {
			return l
		}
	}
	t.Fatal("no default location")
	return nil
}

// buy is a cash-on-delivery sale, which confirms on the spot: the units come
// off the shelf they were picked from rather than sitting reserved on it.
func buy(t *testing.T, app *App, variantID int64, qty int) *Order {
	t.Helper()
	cart := newCart(t, app)
	addToCart(t, app, cart.Token, variantID, qty)
	result, err := app.Order().Checkout(context.Background(), CodeCOD, checkoutInput(cart.Token), "")
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}
	return result.Order
}

// hold is a gateway sale, which stops at pending: the units stay on the shelf
// and are reserved there, which is the state a transfer has to respect.
func hold(t *testing.T, app *App, variantID int64, qty int) *Order {
	t.Helper()
	cart := newCart(t, app)
	addToCart(t, app, cart.Token, variantID, qty)
	result, err := app.Order().Checkout(context.Background(), "testgateway", checkoutInput(cart.Token), "")
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}
	return result.Order
}

// stockAt is what one shelf holds, which is the fact the variant totals hide.
func stockAt(t *testing.T, app *App, variantID, locationID int64) (onHand, reserved int) {
	t.Helper()
	rows, err := app.Stock().ByLocation(context.Background(), variantID)
	if err != nil {
		t.Fatalf("by location: %v", err)
	}
	for _, r := range rows {
		if r.LocationID == locationID {
			return r.OnHand, r.Reserved
		}
	}
	t.Fatalf("variant %d has no row at location %d", variantID, locationID)
	return 0, 0
}

func TestEveryStoreStartsWithOneLocation(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	list, err := app.Places().List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("a new store has %d location(s), want exactly 1", len(list))
	}
	if !list[0].IsDefault || !list[0].Active {
		t.Errorf("the only location is default=%v active=%v, want both true",
			list[0].IsDefault, list[0].Active)
	}

	// And a product created without a word about locations lands in it, which
	// is what lets a single-location store never learn the concept.
	p := simpleProduct(t, app, "LOC-1", 1000, 7)
	onHand, _ := stockAt(t, app, p.DefaultVariant().ID, list[0].ID)
	if onHand != 7 {
		t.Errorf("opening stock at the default location = %d, want 7", onHand)
	}
}

func TestVariantTotalsAreTheSumAcrossLocations(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	p := simpleProduct(t, app, "LOC-SUM", 1000, 4)
	vid := p.DefaultVariant().ID
	shop := newLocation(t, app, "shop", "The shop", 1)

	if _, err := app.Stock().Adjust(ctx, vid, shop.ID, 6); err != nil {
		t.Fatalf("adjust at the shop: %v", err)
	}

	onHand, reserved := variantStock(t, app, vid)
	if onHand != 10 || reserved != 0 {
		t.Errorf("variant totals = (%d, %d), want (10, 0) — 4 at the default plus 6 at the shop",
			onHand, reserved)
	}

	// The breakdown lists both places, including the one the variant was never
	// explicitly given stock at.
	rows, err := app.Stock().ByLocation(ctx, vid)
	if err != nil {
		t.Fatalf("by location: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("breakdown has %d row(s), want 2", len(rows))
	}
}

func TestReservationPrefersTheHigherPriorityLocation(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	// The default is created at priority 0, so a location at -1 outranks it.
	p := simpleProduct(t, app, "LOC-PRIO", 1000, 5)
	vid := p.DefaultVariant().ID
	near := newLocation(t, app, "near", "Near warehouse", -1)
	if _, err := app.Stock().Adjust(ctx, vid, near.ID, 5); err != nil {
		t.Fatalf("stock the near warehouse: %v", err)
	}

	buy(t, app, vid, 2)

	if onHand, _ := stockAt(t, app, vid, near.ID); onHand != 3 {
		t.Errorf("the preferred location holds %d, want 3 — the sale came off it", onHand)
	}
	def := defaultLocation(t, app)
	if onHand, _ := stockAt(t, app, vid, def.ID); onHand != 5 {
		t.Errorf("the default location holds %d, want 5 — it is not preferred", onHand)
	}
}

func TestReservationFallsThroughToWhereThereIsEnough(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	// One unit at the preferred place, plenty at the second: an order for three
	// has to skip the first rather than split or fail. Splitting a line across
	// shelves is a bigger promise than this engine makes — one line, one origin.
	p := simpleProduct(t, app, "LOC-FALL", 1000, 1)
	vid := p.DefaultVariant().ID
	far := newLocation(t, app, "far", "Far warehouse", 5)
	if _, err := app.Stock().Adjust(ctx, vid, far.ID, 10); err != nil {
		t.Fatalf("stock the far warehouse: %v", err)
	}

	buy(t, app, vid, 3)

	if onHand, _ := stockAt(t, app, vid, far.ID); onHand != 7 {
		t.Errorf("the far warehouse holds %d, want 7 — it filled the line", onHand)
	}
	def := defaultLocation(t, app)
	if onHand, _ := stockAt(t, app, vid, def.ID); onHand != 1 {
		t.Errorf("the one-unit location holds %d, want 1 — it could not cover the line", onHand)
	}
}

func TestCancelPutsUnitsBackOnTheShelfTheyLeft(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	// This is the reason the location is written onto the order line. If a
	// cancellation restocked "the default", a sale picked from the warehouse
	// would silently teleport into the shop weeks later.
	p := simpleProduct(t, app, "LOC-BACK", 1000, 0)
	vid := p.DefaultVariant().ID
	shop := newLocation(t, app, "shop", "The shop", 3)
	if _, err := app.Stock().Adjust(ctx, vid, shop.ID, 5); err != nil {
		t.Fatalf("stock the shop: %v", err)
	}

	order := buy(t, app, vid, 2)
	if order.Lines[0].LocationID == nil || *order.Lines[0].LocationID != shop.ID {
		t.Fatalf("line location = %v, want the shop (%d)", order.Lines[0].LocationID, shop.ID)
	}

	if _, err := app.Order().Cancel(ctx, order.ID, "test"); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	onHand, reserved := stockAt(t, app, vid, shop.ID)
	if onHand != 5 || reserved != 0 {
		t.Errorf("the shop is back to (%d, %d), want (5, 0)", onHand, reserved)
	}
	def := defaultLocation(t, app)
	if onHand, _ := stockAt(t, app, vid, def.ID); onHand != 0 {
		t.Errorf("the default location gained %d unit(s) it never held", onHand)
	}
}

func TestTransferKeepsTheStoreTotalUnchanged(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	p := simpleProduct(t, app, "LOC-XFER", 1000, 10)
	vid := p.DefaultVariant().ID
	def := defaultLocation(t, app)
	shop := newLocation(t, app, "shop", "The shop", 1)

	if _, err := app.Stock().Move(ctx, vid, def.ID, shop.ID, 4); err != nil {
		t.Fatalf("move: %v", err)
	}

	onHand, _ := variantStock(t, app, vid)
	if onHand != 10 {
		t.Errorf("store total = %d after a transfer, want 10 — a transfer is not a sale", onHand)
	}
	if got, _ := stockAt(t, app, vid, def.ID); got != 6 {
		t.Errorf("default holds %d, want 6", got)
	}
	if got, _ := stockAt(t, app, vid, shop.ID); got != 4 {
		t.Errorf("shop holds %d, want 4", got)
	}
}

func TestTransferWillNotMoveReservedUnits(t *testing.T) {
	app := newTestApp(t, &gatewayModule{})
	ctx := context.Background()

	// Two of the three units are promised to an order that will be picked from
	// the default location. Moving them would send the picker to the wrong
	// shelf, so only the free one may go.
	p := simpleProduct(t, app, "LOC-HELD", 1000, 3)
	vid := p.DefaultVariant().ID
	def := defaultLocation(t, app)
	shop := newLocation(t, app, "shop", "The shop", 1)

	hold(t, app, vid, 2)
	if _, reserved := stockAt(t, app, vid, def.ID); reserved != 2 {
		t.Fatalf("reserved at the default = %d, want 2", reserved)
	}

	if _, err := app.Stock().Move(ctx, vid, def.ID, shop.ID, 2); err == nil {
		t.Fatal("moved 2 units when only 1 was free")
	}
	if _, err := app.Stock().Move(ctx, vid, def.ID, shop.ID, 1); err != nil {
		t.Fatalf("moving the one free unit: %v", err)
	}
}

func TestAStrandedLocationCannotBeClosed(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	// An inactive location holding stock would have its units counted in
	// stock_on_hand while nothing could ever reserve them — the store would
	// believe it could sell something it could not reach.
	p := simpleProduct(t, app, "LOC-STUCK", 1000, 0)
	vid := p.DefaultVariant().ID
	shop := newLocation(t, app, "shop", "The shop", 1)
	if _, err := app.Stock().Adjust(ctx, vid, shop.ID, 3); err != nil {
		t.Fatalf("stock the shop: %v", err)
	}

	off := false
	if _, err := app.Places().Update(ctx, shop.ID, LocationPatch{Active: &off}); err == nil {
		t.Error("deactivated a location holding 3 units")
	}
	if err := app.Places().Delete(ctx, shop.ID); err == nil {
		t.Error("deleted a location holding 3 units")
	}

	// Emptying it clears both refusals.
	def := defaultLocation(t, app)
	if _, err := app.Stock().Move(ctx, vid, shop.ID, def.ID, 3); err != nil {
		t.Fatalf("empty the shop: %v", err)
	}
	if err := app.Places().Delete(ctx, shop.ID); err != nil {
		t.Errorf("deleting an empty location: %v", err)
	}
}

func TestTheDefaultLocationCannotBeDeleted(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	def := defaultLocation(t, app)
	if err := app.Places().Delete(ctx, def.ID); err == nil {
		t.Fatal("deleted the default location, leaving nowhere for stock to land")
	}

	// Moving the default first is the way out.
	shop := newLocation(t, app, "shop", "The shop", 1)
	if _, err := app.Places().SetDefault(ctx, shop.ID); err != nil {
		t.Fatalf("set default: %v", err)
	}
	if err := app.Places().Delete(ctx, def.ID); err != nil {
		t.Fatalf("deleting the former default: %v", err)
	}
	if got := defaultLocation(t, app); got.ID != shop.ID {
		t.Errorf("default is %d, want the shop (%d)", got.ID, shop.ID)
	}
}

func TestStockCannotDropBelowWhatIsReservedAtThatLocation(t *testing.T) {
	app := newTestApp(t, &gatewayModule{})
	ctx := context.Background()

	// The invariant is per shelf, not per store: five spare units in the
	// warehouse do not entitle the shop to go negative, because the order
	// being picked from the shop cannot be filled from the warehouse.
	p := simpleProduct(t, app, "LOC-FLOOR", 1000, 2)
	vid := p.DefaultVariant().ID
	def := defaultLocation(t, app)
	warehouse := newLocation(t, app, "warehouse", "Warehouse", 9)
	if _, err := app.Stock().Adjust(ctx, vid, warehouse.ID, 5); err != nil {
		t.Fatalf("stock the warehouse: %v", err)
	}

	hold(t, app, vid, 2)

	if _, err := app.Stock().SetOnHand(ctx, vid, def.ID, 1); err == nil {
		t.Error("set on-hand below the 2 units reserved at that location")
	}
	if _, err := app.Stock().Adjust(ctx, vid, def.ID, -1); err == nil {
		t.Error("adjusted on-hand below the 2 units reserved at that location")
	}
}

func TestUnknownLocationIsRefusedRatherThanDefaulted(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	// 0 means "the default"; 424242 means the caller believes in a location
	// that does not exist, and quietly putting their stock somewhere else would
	// be worse than telling them.
	p := simpleProduct(t, app, "LOC-GHOST", 1000, 1)
	vid := p.DefaultVariant().ID

	if _, err := app.Stock().Adjust(ctx, vid, 424242, 5); err == nil {
		t.Fatal("adjusted stock at a location that does not exist")
	}
	onHand, _ := variantStock(t, app, vid)
	if onHand != 1 {
		t.Errorf("store total = %d, want 1 — the refused adjustment moved something", onHand)
	}
}

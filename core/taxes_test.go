package gocommerce

import (
	"context"
	"testing"
)

// newTaxApp boots a store with the config tweaked, which is the only way to
// exercise inclusive pricing: whether prices contain tax is decided before the
// engine starts, because it decides what every figure means.
func newTaxApp(t *testing.T, tweak func(*Config)) *App {
	t.Helper()
	dsn := requireDB(t)
	resetSchema(t, dsn)
	cfg := testConfig(dsn)
	tweak(&cfg)
	app, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = app.Close() })
	return app
}

func newTaxRate(t *testing.T, app *App, in TaxRateInput) *TaxRate {
	t.Helper()
	r, err := app.Taxes().Create(context.Background(), in)
	if err != nil {
		t.Fatalf("create tax rate: %v", err)
	}
	return r
}

// checkoutTo buys into a destination, which is what decides the rate.
func checkoutTo(t *testing.T, app *App, variantID int64, qty int, country, state string) (*Order, error) {
	t.Helper()
	ctx := context.Background()
	cart := newCart(t, app)
	addToCart(t, app, cart.Token, variantID, qty)
	in := checkoutInput(cart.Token)
	in.Address.Country = country
	in.Address.State = state
	result, err := app.Order().Checkout(ctx, CodeCOD, in, "")
	if err != nil {
		return nil, err
	}
	return result.Order, nil
}

// A store with no rates charges no tax and its totals are exactly what they
// were before tax existed. That is the property that makes this migration safe
// to ship to a running store.
func TestNoRatesChangesNothing(t *testing.T) {
	app := newTestApp(t)
	product := simpleProduct(t, app, "TAX-0", 1000, 5)

	order, err := checkoutTo(t, app, product.DefaultVariant().ID, 2, "IN", "KA")
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}
	if order.Tax.AmountMinor != 0 {
		t.Errorf("tax = %d, want none", order.Tax.AmountMinor)
	}
	if order.Total.AmountMinor != order.Subtotal.AmountMinor+order.Shipping.AmountMinor {
		t.Errorf("total = %d, want subtotal plus shipping", order.Total.AmountMinor)
	}
}

// Tax exclusive: the rate is added, and the per-line figures sum to the order's.
func TestTaxExclusiveIsAdded(t *testing.T) {
	app := newTestApp(t)
	product := simpleProduct(t, app, "TAX-1", 1000, 5)
	newTaxRate(t, app, TaxRateInput{Name: "GST 18%", RateBP: 1800, Country: "IN"})

	order, err := checkoutTo(t, app, product.DefaultVariant().ID, 2, "IN", "KA")
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}
	if order.Subtotal.AmountMinor != 2000 {
		t.Fatalf("subtotal = %d", order.Subtotal.AmountMinor)
	}
	if order.Tax.AmountMinor != 360 {
		t.Errorf("tax = %d, want 360 (18%% of 2000)", order.Tax.AmountMinor)
	}
	if order.TaxInclusive {
		t.Error("tax_inclusive is set on an exclusive store")
	}
	want := order.Subtotal.AmountMinor + order.Shipping.AmountMinor + order.Tax.AmountMinor
	if order.Total.AmountMinor != want {
		t.Errorf("total = %d, want %d", order.Total.AmountMinor, want)
	}

	var lineTax int64
	for _, l := range order.Lines {
		lineTax += l.Tax.AmountMinor
		if l.Tax.RateBP != 1800 || l.Tax.Name != "GST 18%" {
			t.Errorf("line tax = %+v, want the rate that was applied", l.Tax)
		}
	}
	if lineTax != order.Tax.AmountMinor {
		t.Errorf("lines sum to %d, order says %d", lineTax, order.Tax.AmountMinor)
	}
}

// Tax inclusive: the price already contains it, so the total does not move and
// the tax is extracted rather than added.
func TestTaxInclusiveIsExtracted(t *testing.T) {
	app := newTaxApp(t, func(c *Config) { c.PricesIncludeTax = true })
	product := simpleProduct(t, app, "TAX-2", 1180, 5)
	newTaxRate(t, app, TaxRateInput{Name: "GST 18%", RateBP: 1800, Country: "IN"})

	order, err := checkoutTo(t, app, product.DefaultVariant().ID, 1, "IN", "KA")
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}
	if !order.TaxInclusive {
		t.Error("tax_inclusive is not set on an inclusive store")
	}
	// 1180 inclusive of 18% is 1000 net and 180 tax.
	if order.Tax.AmountMinor != 180 {
		t.Errorf("tax = %d, want 180 extracted from 1180", order.Tax.AmountMinor)
	}
	if order.Total.AmountMinor != order.Subtotal.AmountMinor+order.Shipping.AmountMinor {
		t.Errorf("total = %d â€” an inclusive price must not grow", order.Total.AmountMinor)
	}
}

// The most specific rule that fits a line is the one that applies, and a rule
// on a category reaches the whole subtree under it.
func TestTaxRateSpecificity(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	parent, err := app.Categories().Create(ctx, CategoryInput{Title: "Apparel"})
	if err != nil {
		t.Fatalf("category: %v", err)
	}
	child, err := app.Categories().Create(ctx, CategoryInput{Title: "Shirts", ParentID: &parent.ID})
	if err != nil {
		t.Fatalf("category: %v", err)
	}

	product := simpleProduct(t, app, "TAX-3", 1000, 10)
	if _, err := app.catalog.UpdateProduct(ctx, product.ID,
		ProductPatch{CategoryID: SetID(child.ID)}); err != nil {
		t.Fatalf("categorise: %v", err)
	}

	// A country-wide fallback, and a rule on the parent category.
	newTaxRate(t, app, TaxRateInput{Name: "Standard", RateBP: 1800, Country: "IN"})
	newTaxRate(t, app, TaxRateInput{
		Name: "Apparel", RateBP: 500, Country: "IN", CategoryID: &parent.ID})

	// The category rule wins, and it reaches down to Shirts.
	order, err := checkoutTo(t, app, product.DefaultVariant().ID, 1, "IN", "KA")
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}
	if order.Lines[0].Tax.Name != "Apparel" || order.Tax.AmountMinor != 50 {
		t.Errorf("tax = %+v / %d, want the parent category's 5%%",
			order.Lines[0].Tax, order.Tax.AmountMinor)
	}

	// A rule on the leaf beats the one on its parent.
	newTaxRate(t, app, TaxRateInput{
		Name: "Shirts", RateBP: 1200, Country: "IN", CategoryID: &child.ID})
	order2, err := checkoutTo(t, app, product.DefaultVariant().ID, 1, "IN", "KA")
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}
	if order2.Lines[0].Tax.Name != "Shirts" {
		t.Errorf("tax = %+v, want the deeper rule", order2.Lines[0].Tax)
	}

	// And a rule naming the state beats the same category without one.
	newTaxRate(t, app, TaxRateInput{
		Name: "Shirts KA", RateBP: 1400, Country: "IN", State: "KA", CategoryID: &child.ID})
	order3, err := checkoutTo(t, app, product.DefaultVariant().ID, 1, "IN", "KA")
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}
	if order3.Lines[0].Tax.Name != "Shirts KA" {
		t.Errorf("tax = %+v, want the state-specific rule", order3.Lines[0].Tax)
	}
	// Somewhere else in the country falls back to the plain category rule.
	order4, err := checkoutTo(t, app, product.DefaultVariant().ID, 1, "IN", "MH")
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}
	if order4.Lines[0].Tax.Name != "Shirts" {
		t.Errorf("tax = %+v, want the rule without a state", order4.Lines[0].Tax)
	}
}

// A destination with no rule is not taxed, which is how a store sells abroad
// without pretending to know somebody else's tax law.
func TestUnknownDestinationIsNotTaxed(t *testing.T) {
	app := newTestApp(t)
	product := simpleProduct(t, app, "TAX-4", 1000, 5)
	newTaxRate(t, app, TaxRateInput{Name: "GST", RateBP: 1800, Country: "IN"})

	order, err := checkoutTo(t, app, product.DefaultVariant().ID, 1, "DE", "")
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}
	if order.Tax.AmountMinor != 0 {
		t.Errorf("tax = %d, want none for a country with no rule", order.Tax.AmountMinor)
	}
}

// A variant marked not taxable is not taxed, whatever the destination says.
func TestNonTaxableVariant(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()
	product := simpleProduct(t, app, "TAX-5", 1000, 5)
	newTaxRate(t, app, TaxRateInput{Name: "GST", RateBP: 1800, Country: "IN"})

	no := false
	if _, err := app.catalog.UpdateVariant(ctx, product.DefaultVariant().ID,
		VariantPatch{Taxable: &no}); err != nil {
		t.Fatalf("untax: %v", err)
	}
	order, err := checkoutTo(t, app, product.DefaultVariant().ID, 1, "IN", "KA")
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}
	if order.Tax.AmountMinor != 0 {
		t.Errorf("tax = %d, want none on an untaxed variant", order.Tax.AmountMinor)
	}
}

// allocateDiscount is the part with the arithmetic trap: the parts must sum to
// the whole, whatever the remainders do.
func TestAllocateDiscountSumsExactly(t *testing.T) {
	cases := []struct {
		name     string
		lines    []int64
		discount int64
	}{
		{"even split", []int64{1000, 1000}, 300},
		{"awkward thirds", []int64{100, 100, 100}, 100},
		{"one big one small", []int64{999, 1}, 500},
		{"discount is the whole basket", []int64{700, 300}, 1000},
		{"discount larger than the basket", []int64{700, 300}, 5000},
		{"nothing off", []int64{500, 500}, 0},
		{"one line", []int64{1234}, 567},
	}
	for _, c := range cases {
		lines := make([]taxableLine, len(c.lines))
		var gross int64
		for i, total := range c.lines {
			lines[i] = taxableLine{Total: total, Taxable: true}
			gross += total
		}
		got := allocateDiscount(lines, c.discount)

		want := c.discount
		if want > gross {
			want = gross
		}
		var sum int64
		for i, v := range got {
			if v < 0 {
				t.Errorf("%s: line %d got a negative share", c.name, i)
			}
			if v > c.lines[i] {
				t.Errorf("%s: line %d got %d off a line worth %d", c.name, i, v, c.lines[i])
			}
			sum += v
		}
		if sum != want {
			t.Errorf("%s: allocations sum to %d, want %d", c.name, sum, want)
		}
	}
}

// Tax is charged on what was actually paid, so a discount reduces it.
func TestTaxIsChargedAfterTheDiscount(t *testing.T) {
	app := newTestApp(t)
	product := simpleProduct(t, app, "TAX-6", 1000, 10)
	newTaxRate(t, app, TaxRateInput{Name: "GST 18%", RateBP: 1800, Country: "IN"})
	newDiscount(t, app, DiscountInput{
		Code: "HALF", Title: "Half off", Kind: DiscountPercentage, ValueBP: 5000,
	})

	ctx := context.Background()
	cart := newCart(t, app)
	addToCart(t, app, cart.Token, product.DefaultVariant().ID, 2)
	if _, err := app.db.ExecContext(ctx,
		`UPDATE carts SET discount_code = 'HALF' WHERE token = $1`, cart.Token); err != nil {
		t.Fatalf("attach: %v", err)
	}
	in := checkoutInput(cart.Token)
	in.Address.Country = "IN"
	result, err := app.Order().Checkout(ctx, CodeCOD, in, "")
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}
	o := result.Order
	if o.Subtotal.AmountMinor != 2000 || o.Discount.AmountMinor != 1000 {
		t.Fatalf("subtotal %d discount %d", o.Subtotal.AmountMinor, o.Discount.AmountMinor)
	}
	// 18% of the 1000 that was actually paid, not of the 2000 listed.
	if o.Tax.AmountMinor != 180 {
		t.Errorf("tax = %d, want 180 â€” charged on what was paid", o.Tax.AmountMinor)
	}
	want := o.Subtotal.AmountMinor - o.Discount.AmountMinor + o.Shipping.AmountMinor + o.Tax.AmountMinor
	if o.Total.AmountMinor != want {
		t.Errorf("total = %d, want %d", o.Total.AmountMinor, want)
	}
}

// A rate is a rule about one place and one kind of thing. Two rules for the
// same pair is a mistake to refuse at entry, not a preference to resolve at
// checkout.
func TestTaxRateValidation(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	newTaxRate(t, app, TaxRateInput{Name: "GST", RateBP: 1800, Country: "IN"})
	if _, err := app.Taxes().Create(ctx, TaxRateInput{
		Name: "GST again", RateBP: 500, Country: "IN"}); err == nil {
		t.Error("two rates cover the same country and category")
	}

	bad := []struct {
		name string
		in   TaxRateInput
	}{
		{"no name", TaxRateInput{RateBP: 1800, Country: "IN"}},
		{"over 100%", TaxRateInput{Name: "x", RateBP: 10001}},
		{"negative", TaxRateInput{Name: "x", RateBP: -1}},
		{"state with no country", TaxRateInput{Name: "x", RateBP: 500, State: "KA"}},
	}
	for _, c := range bad {
		if _, err := app.Taxes().Create(ctx, c.in); err == nil {
			t.Errorf("%s was accepted", c.name)
		}
	}
}

// Changing a rate does not rewrite what an order was charged.
func TestChangingARateLeavesOrdersAlone(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()
	product := simpleProduct(t, app, "TAX-7", 1000, 5)
	rate := newTaxRate(t, app, TaxRateInput{Name: "GST 18%", RateBP: 1800, Country: "IN"})

	order, err := checkoutTo(t, app, product.DefaultVariant().ID, 1, "IN", "")
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}
	if order.Tax.AmountMinor != 180 {
		t.Fatalf("tax = %d", order.Tax.AmountMinor)
	}

	newBP := 500
	if _, err := app.Taxes().Update(ctx, rate.ID, TaxRatePatch{RateBP: &newBP}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := app.Taxes().Delete(ctx, rate.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	after, err := app.Order().Get(ctx, order.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if after.Tax.AmountMinor != 180 || after.Lines[0].Tax.RateBP != 1800 {
		t.Errorf("order now says %d at %d bp â€” an invoice must not change",
			after.Tax.AmountMinor, after.Lines[0].Tax.RateBP)
	}
	if after.Lines[0].Tax.Name != "GST 18%" {
		t.Errorf("line tax name = %q, want the one it was charged under", after.Lines[0].Tax.Name)
	}
}

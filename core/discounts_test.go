package gocommerce

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// newDiscount is the shorthand every test here needs.
func newDiscount(t *testing.T, app *App, in DiscountInput) *Discount {
	t.Helper()
	d, err := app.Discounts().Create(context.Background(), in)
	if err != nil {
		t.Fatalf("create discount: %v", err)
	}
	return d
}

// checkoutWithCode puts a code on the cart and buys, returning the order or the
// refusal. It is the only path a discount is ever applied through.
func checkoutWithCode(t *testing.T, app *App, code string, variantID int64, qty int, email string) (*Order, error) {
	t.Helper()
	ctx := context.Background()
	cart := newCart(t, app)
	addToCart(t, app, cart.Token, variantID, qty)
	if code != "" {
		if _, err := app.db.ExecContext(ctx,
			`UPDATE carts SET discount_code = $2 WHERE token = $1`, cart.Token, code); err != nil {
			t.Fatalf("attach code: %v", err)
		}
	}
	in := checkoutInput(cart.Token)
	if email != "" {
		in.Email = email
	}
	result, err := app.Order().Checkout(ctx, CodeCOD, in, "")
	if err != nil {
		return nil, err
	}
	return result.Order, nil
}

// TestDiscountArithmetic is the whole of the money maths: one rounding, a floor
// at the basket, and totals that add up.
func TestDiscountArithmetic(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	// 999 * 3 = 2997. Ten percent is 299.7, which must round to 300 exactly
	// once rather than 3 × 99.9 rounded per line.
	product := simpleProduct(t, app, "DISC-1", 999, 10)
	tenPercent := newDiscount(t, app, DiscountInput{
		Code: "TENOFF", Title: "Ten percent", Kind: DiscountPercentage, ValueBP: 1000,
	})

	order, err := checkoutWithCode(t, app, "TENOFF", product.DefaultVariant().ID, 3, "")
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}
	if order.Subtotal.AmountMinor != 2997 {
		t.Fatalf("subtotal = %d, want 2997", order.Subtotal.AmountMinor)
	}
	if order.Discount.AmountMinor != 300 {
		t.Errorf("discount = %d, want 300 — one rounding, half up", order.Discount.AmountMinor)
	}
	want := order.Subtotal.AmountMinor + order.Shipping.AmountMinor - order.Discount.AmountMinor
	if order.Total.AmountMinor != want {
		t.Errorf("total = %d, want %d — the figures must add up", order.Total.AmountMinor, want)
	}
	if len(order.Discounts) != 1 || order.Discounts[0].Title != "Ten percent" {
		t.Errorf("snapshot = %+v, want the discount recorded on the order", order.Discounts)
	}

	// The usage count moved exactly once.
	after, err := app.Discounts().Get(ctx, tenPercent.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if after.UsedCount != 1 {
		t.Errorf("used_count = %d, want 1", after.UsedCount)
	}
}

// A fixed discount larger than the basket takes the basket, not more. The
// alternative is a negative total, refused by a CHECK far from the cause.
func TestDiscountNeverExceedsTheBasket(t *testing.T) {
	app := newTestApp(t)
	product := simpleProduct(t, app, "DISC-2", 300, 5)
	newDiscount(t, app, DiscountInput{
		Code: "BIG", Title: "Too generous", Kind: DiscountFixed, ValueMinor: 50000,
	})

	order, err := checkoutWithCode(t, app, "BIG", product.DefaultVariant().ID, 1, "")
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}
	if order.Discount.AmountMinor != 300 {
		t.Errorf("discount = %d, want the basket's 300", order.Discount.AmountMinor)
	}
	if order.Total.AmountMinor != order.Shipping.AmountMinor {
		t.Errorf("total = %d, want shipping alone", order.Total.AmountMinor)
	}
}

// TestDiscountUsageLimitUnderConcurrency is the race that matters: two
// checkouts reaching for the last use of a code at the same moment.
func TestDiscountUsageLimitUnderConcurrency(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	product := simpleProduct(t, app, "DISC-3", 1000, 20)
	limit := 1
	d := newDiscount(t, app, DiscountInput{
		Code: "ONLYONE", Title: "One use", Kind: DiscountFixed, ValueMinor: 100,
		UsageLimit: &limit,
	})

	// Two carts prepared up front, so the race is over the code and not over
	// creating the carts.
	tokens := make([]string, 2)
	for i := range tokens {
		cart := newCart(t, app)
		addToCart(t, app, cart.Token, product.DefaultVariant().ID, 1)
		if _, err := app.db.ExecContext(ctx,
			`UPDATE carts SET discount_code = 'ONLYONE' WHERE token = $1`, cart.Token); err != nil {
			t.Fatalf("attach: %v", err)
		}
		tokens[i] = cart.Token
	}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	orders := make([]*Order, 2)
	start := make(chan struct{})
	for i := range tokens {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			result, err := app.Order().Checkout(ctx, CodeCOD, checkoutInput(tokens[i]), "")
			errs[i] = err
			if result != nil {
				orders[i] = result.Order
			}
		}(i)
	}
	close(start)
	wg.Wait()

	won, lost := 0, 0
	for i := range errs {
		if errs[i] == nil {
			won++
		} else {
			lost++
		}
	}
	if won != 1 || lost != 1 {
		t.Fatalf("won %d, lost %d — exactly one checkout may take the last use (errs: %v)", won, lost, errs)
	}
	after, err := app.Discounts().Get(ctx, d.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if after.UsedCount != 1 {
		t.Errorf("used_count = %d, want 1", after.UsedCount)
	}
}

// A checkout that fails after the discount was counted must not consume a use.
// Stock is the failure that is easiest to arrange and is on the same path.
func TestDiscountUseIsRolledBackWithTheOrder(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	product := simpleProduct(t, app, "DISC-4", 1000, 1)
	d := newDiscount(t, app, DiscountInput{
		Code: "ROLLBACK", Title: "Rolled back", Kind: DiscountFixed, ValueMinor: 100,
	})

	cart := newCart(t, app)
	addToCart(t, app, cart.Token, product.DefaultVariant().ID, 1)
	if _, err := app.db.ExecContext(ctx,
		`UPDATE carts SET discount_code = 'ROLLBACK' WHERE token = $1`, cart.Token); err != nil {
		t.Fatalf("attach: %v", err)
	}
	// Sell the only unit out from under the cart.
	if _, err := app.inventory.Adjust(ctx, product.DefaultVariant().ID, 0, -1); err != nil {
		t.Fatalf("adjust: %v", err)
	}

	if _, err := app.Order().Checkout(ctx, CodeCOD, checkoutInput(cart.Token), ""); err == nil {
		t.Fatal("checkout succeeded with no stock")
	}
	after, err := app.Discounts().Get(ctx, d.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if after.UsedCount != 0 {
		t.Errorf("used_count = %d, want 0 — a rolled-back order consumes nothing", after.UsedCount)
	}
}

// A code that expired while the cart was sitting there is refused at checkout,
// which is the only place that can honestly refuse it.
func TestExpiredDiscountIsRefusedAtCheckout(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	product := simpleProduct(t, app, "DISC-5", 1000, 5)
	past := time.Now().Add(-time.Hour)
	older := past.Add(-time.Hour)
	d := newDiscount(t, app, DiscountInput{
		Code: "GONE", Title: "Expired", Kind: DiscountFixed, ValueMinor: 100,
		StartsAt: &older, EndsAt: &past,
	})

	if _, err := checkoutWithCode(t, app, "GONE", product.DefaultVariant().ID, 1, ""); err == nil {
		t.Error("an expired code was accepted")
	}

	// And a deactivated one, which is the other way a live code stops working.
	inactive := false
	if _, err := app.Discounts().Update(ctx, d.ID, DiscountPatch{
		Active: &inactive, EndsAt: nil,
	}); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	if _, err := checkoutWithCode(t, app, "GONE", product.DefaultVariant().ID, 1, ""); err == nil {
		t.Error("an inactive code was accepted")
	}
}

// The minimum is a floor on the basket, checked against the subtotal the order
// actually has.
func TestDiscountMinimumSubtotal(t *testing.T) {
	app := newTestApp(t)
	product := simpleProduct(t, app, "DISC-6", 500, 10)
	min := int64(2000)
	newDiscount(t, app, DiscountInput{
		Code: "BULK", Title: "Bulk only", Kind: DiscountFixed, ValueMinor: 200,
		MinSubtotalMinor: &min,
	})

	if _, err := checkoutWithCode(t, app, "BULK", product.DefaultVariant().ID, 2, ""); err == nil {
		t.Error("a 1000 basket met a 2000 minimum")
	}
	order, err := checkoutWithCode(t, app, "BULK", product.DefaultVariant().ID, 4, "")
	if err != nil {
		t.Fatalf("checkout at the minimum: %v", err)
	}
	if order.Discount.AmountMinor != 200 {
		t.Errorf("discount = %d, want 200", order.Discount.AmountMinor)
	}
}

// Codes are typed off posters. Matching folds case; the stored code keeps the
// case it was created with.
func TestDiscountCodeIsCaseInsensitive(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()
	product := simpleProduct(t, app, "DISC-7", 1000, 10)
	newDiscount(t, app, DiscountInput{
		Code: "SpringSale", Title: "Spring", Kind: DiscountFixed, ValueMinor: 100,
	})

	order, err := checkoutWithCode(t, app, "springsale", product.DefaultVariant().ID, 1, "")
	if err != nil {
		t.Fatalf("lower-case code refused: %v", err)
	}
	if order.Discounts[0].Code != "SpringSale" {
		t.Errorf("stored code = %q, want the case it was created with", order.Discounts[0].Code)
	}
	// And a second discount may not claim the same code in another case.
	if _, err := app.Discounts().Create(ctx, DiscountInput{
		Code: "SPRINGSALE", Title: "Clash", Kind: DiscountFixed, ValueMinor: 50,
	}); err == nil {
		t.Error("two discounts share a code that differs only in case")
	}
}

// once_per_email is a deterrent and the test says so: the same address is
// refused, a different one is not.
func TestDiscountOncePerEmail(t *testing.T) {
	app := newTestApp(t)
	product := simpleProduct(t, app, "DISC-8", 1000, 10)
	newDiscount(t, app, DiscountInput{
		Code: "ONCE", Title: "One each", Kind: DiscountFixed, ValueMinor: 100,
		OncePerEmail: true,
	})

	if _, err := checkoutWithCode(t, app, "ONCE", product.DefaultVariant().ID, 1, "one@example.com"); err != nil {
		t.Fatalf("first use: %v", err)
	}
	if _, err := checkoutWithCode(t, app, "ONCE", product.DefaultVariant().ID, 1, "ONE@example.com"); err == nil {
		t.Error("the same address used the code twice, in another case")
	}
	if _, err := checkoutWithCode(t, app, "ONCE", product.DefaultVariant().ID, 1, "two@example.com"); err != nil {
		t.Errorf("a different address was refused: %v", err)
	}
}

// Deleting a finished promotion leaves every order it touched reading correctly.
func TestDeletingADiscountKeepsTheOrderTrue(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()
	product := simpleProduct(t, app, "DISC-9", 1000, 10)
	d := newDiscount(t, app, DiscountInput{
		Code: "TEMP", Title: "Temporary", Kind: DiscountFixed, ValueMinor: 250,
	})

	order, err := checkoutWithCode(t, app, "TEMP", product.DefaultVariant().ID, 1, "")
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}
	if err := app.Discounts().Delete(ctx, d.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	after, err := app.Order().Get(ctx, order.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if after.Discount.AmountMinor != 250 {
		t.Errorf("discount = %d, want the 250 it was given", after.Discount.AmountMinor)
	}
	if len(after.Discounts) != 1 || after.Discounts[0].Title != "Temporary" {
		t.Errorf("snapshot = %+v, want the title it had", after.Discounts)
	}
	if after.Discounts[0].DiscountID != 0 {
		t.Errorf("discount_id = %d, want it nulled by the delete", after.Discounts[0].DiscountID)
	}
}

// D24: an edit recomputes a percentage, keeps a fixed amount, and refuses when
// the basket falls below the minimum the discount needed.
func TestEditedOrderFollowsD24(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	// Percentage: 2 × 1000 = 2000, ten percent is 200. Drop to one and it must
	// become 100 rather than staying 200.
	pct := simpleProduct(t, app, "D24-PCT", 1000, 10)
	newDiscount(t, app, DiscountInput{
		Code: "PCT", Title: "Percent", Kind: DiscountPercentage, ValueBP: 1000,
	})
	order, err := checkoutWithCode(t, app, "PCT", pct.DefaultVariant().ID, 2, "")
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}
	if order.Discount.AmountMinor != 200 {
		t.Fatalf("discount = %d, want 200", order.Discount.AmountMinor)
	}
	edited, _, err := app.Order().EditLines(ctx, order.ID, OrderEdit{
		Lines: []OrderLineEdit{{ID: order.Lines[0].ID, Quantity: 1}},
	})
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if edited.Discount.AmountMinor != 100 {
		t.Errorf("discount = %d, want 100 — a percentage follows its basket",
			edited.Discount.AmountMinor)
	}
	if edited.Total.AmountMinor !=
		edited.Subtotal.AmountMinor+edited.Shipping.AmountMinor-edited.Discount.AmountMinor {
		t.Error("the edited totals do not add up")
	}

	// Fixed: the amount is not a function of the basket and does not move.
	fix := simpleProduct(t, app, "D24-FIX", 1000, 10)
	newDiscount(t, app, DiscountInput{
		Code: "FIX", Title: "Fixed", Kind: DiscountFixed, ValueMinor: 150,
	})
	order2, err := checkoutWithCode(t, app, "FIX", fix.DefaultVariant().ID, 2, "")
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}
	edited2, _, err := app.Order().EditLines(ctx, order2.ID, OrderEdit{
		Lines: []OrderLineEdit{{ID: order2.Lines[0].ID, Quantity: 1}},
	})
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if edited2.Discount.AmountMinor != 150 {
		t.Errorf("discount = %d, want it unchanged at 150", edited2.Discount.AmountMinor)
	}

	// Below the minimum: refused, with the reason.
	minP := simpleProduct(t, app, "D24-MIN", 1000, 10)
	min := int64(1500)
	newDiscount(t, app, DiscountInput{
		Code: "MIN", Title: "Minimum", Kind: DiscountPercentage, ValueBP: 1000,
		MinSubtotalMinor: &min,
	})
	order3, err := checkoutWithCode(t, app, "MIN", minP.DefaultVariant().ID, 2, "")
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}
	_, _, err = app.Order().EditLines(ctx, order3.ID, OrderEdit{
		Lines: []OrderLineEdit{{ID: order3.Lines[0].ID, Quantity: 1}},
	})
	if err == nil {
		t.Fatal("an order was edited below the minimum its discount needed")
	}
	if !strings.Contains(err.Error(), "remove the discount") {
		t.Errorf("error = %q, want it to say what to do", err)
	}
}

// A rule has to be coherent with itself, and the message has to say which part
// is not.
func TestDiscountValidation(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	cases := []struct {
		name string
		in   DiscountInput
	}{
		{"no title", DiscountInput{Kind: DiscountFixed, ValueMinor: 100}},
		{"unknown kind", DiscountInput{Title: "x", Kind: "half-price"}},
		{"percentage with no value", DiscountInput{Title: "x", Kind: DiscountPercentage}},
		{"percentage over 100%", DiscountInput{Title: "x", Kind: DiscountPercentage, ValueBP: 10001}},
		{"percentage with an amount", DiscountInput{
			Title: "x", Kind: DiscountPercentage, ValueBP: 500, ValueMinor: 5}},
		{"fixed with no amount", DiscountInput{Title: "x", Kind: DiscountFixed}},
		{"free shipping with a value", DiscountInput{
			Title: "x", Kind: DiscountFreeShipping, ValueMinor: 100}},
		{"unknown scope", DiscountInput{
			Title: "x", Kind: DiscountFixed, ValueMinor: 100, Scope: "everything"}},
	}
	for _, c := range cases {
		if _, err := app.Discounts().Create(ctx, c.in); err == nil {
			t.Errorf("%s was accepted", c.name)
		}
	}

	// Changing the kind without the value is the mistake worth catching: a
	// percentage rule given a fixed kind would take one basis point off.
	d := newDiscount(t, app, DiscountInput{
		Title: "Percent", Kind: DiscountPercentage, ValueBP: 1000,
	})
	fixed := DiscountFixed
	if _, err := app.Discounts().Update(ctx, d.ID, DiscountPatch{Kind: &fixed}); err == nil {
		t.Error("a percentage was turned into a fixed discount with no amount")
	}
}

// A scoped discount is stored and refused rather than quietly applying to the
// whole basket — the failure that would cost a store money unnoticed.
func TestScopedDiscountsAreRefusedNotGuessed(t *testing.T) {
	app := newTestApp(t)
	product := simpleProduct(t, app, "DISC-10", 1000, 5)
	newDiscount(t, app, DiscountInput{
		Code: "SCOPED", Title: "Products only", Kind: DiscountFixed, ValueMinor: 100,
		Scope: DiscountScopeProducts,
	})

	if _, err := checkoutWithCode(t, app, "SCOPED", product.DefaultVariant().ID, 1, ""); err == nil {
		t.Error("a scoped discount was applied to the whole basket")
	}
}

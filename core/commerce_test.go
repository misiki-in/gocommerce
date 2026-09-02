package gocommerce

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// These are the engine's reliability requirements, written as tests rather
// than as documentation. Each one corresponds to a way a commerce system can
// quietly lose money or trust.

// ---------------------------------------------------------------- fixtures

func simpleProduct(t *testing.T, app *App, sku string, priceMinor int64, stock int) *Product {
	t.Helper()
	p, err := app.Products().CreateProduct(context.Background(), ProductInput{
		Title: "Test " + sku, Status: ProductActive,
		SKU: sku, PriceMinor: &priceMinor, Stock: &stock,
	})
	if err != nil {
		t.Fatalf("create product %s: %v", sku, err)
	}
	return p
}

func addToCart(t *testing.T, app *App, cartToken string, variantID int64, qty int) *Cart {
	t.Helper()
	cart, err := app.Cart().AddLine(context.Background(), cartToken, variantID, qty)
	if err != nil {
		t.Fatalf("add to cart: %v", err)
	}
	return cart
}

func newCart(t *testing.T, app *App) *Cart {
	t.Helper()
	cart, err := app.Cart().Create(context.Background(), "")
	if err != nil {
		t.Fatalf("create cart: %v", err)
	}
	return cart
}

func checkoutInput(cartToken string) CheckoutInput {
	return CheckoutInput{
		CartID: cartToken, Email: "shopper@example.com", Name: "A Shopper",
		Address: Address{Line1: "1 Test Street", City: "Testville",
			PostalCode: "12345", Country: "US"},
	}
}

func variantStock(t *testing.T, app *App, variantID int64) (onHand, reserved int) {
	t.Helper()
	v, err := app.Products().GetVariant(context.Background(), variantID)
	if err != nil {
		t.Fatalf("get variant: %v", err)
	}
	return v.StockOnHand, v.StockReserved
}

// recordingNotifier captures deliveries so a test can assert on them.
type recordingNotifier struct {
	mu   sync.Mutex
	sent []Notification
}

func (n *recordingNotifier) Notify(ctx context.Context, note Notification) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.sent = append(n.sent, note)
	return nil
}

func (n *recordingNotifier) count(event string) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	c := 0
	for _, s := range n.sent {
		if s.Event == event {
			c++
		}
	}
	return c
}

// notifyModule installs a recording notifier through the public module surface.
type notifyModule struct{ rec *recordingNotifier }

func (m *notifyModule) Name() string            { return "recorder" }
func (m *notifyModule) Migrations() []Migration { return nil }
func (m *notifyModule) Register(app *App) error {
	app.RegisterNotifier(ChannelEmail, m.rec)
	return nil
}

// refundableModule installs a payment method that can refund, so tests can
// reach the refunded state. Cash on delivery — the only method core ships —
// deliberately cannot, which otherwise leaves that state testable only from a
// gateway module.
type refundableModule struct{}

func (refundableModule) Name() string            { return "refundable" }
func (refundableModule) Migrations() []Migration { return nil }
func (refundableModule) Register(app *App) error {
	app.RegisterPayment(refundableProvider{})
	return nil
}

type refundableProvider struct{}

func (refundableProvider) Code() string { return "refundable" }
func (refundableProvider) Initiate(context.Context, *Order, PayOptions) (PaymentIntent, error) {
	return PaymentIntent{Kind: IntentNone, Provider: "refundable"}, nil
}
func (refundableProvider) Refund(context.Context, *Order, int64) error { return nil }

// ---------------------------------------------------------- the purchase path

// TestPurchasePathCOD walks the canonical sale end to end and checks what
// each step was supposed to change.
func TestPurchasePathCOD(t *testing.T) {
	rec := &recordingNotifier{}
	app := newTestApp(t, &notifyModule{rec: rec})
	ctx := context.Background()

	product := simpleProduct(t, app, "TEE-001", 2500, 10)
	variant := product.DefaultVariant()
	if variant == nil {
		t.Fatal("a product with no options should still have one default variant")
	}

	cart := newCart(t, app)
	cart = addToCart(t, app, cart.Token, variant.ID, 2)
	if cart.Subtotal.AmountMinor != 5000 {
		t.Errorf("subtotal = %d, want 5000", cart.Subtotal.AmountMinor)
	}
	if cart.Subtotal.Currency != "USD" {
		t.Errorf("currency = %q, want USD", cart.Subtotal.Currency)
	}

	result, err := app.Order().Checkout(ctx, CodeCOD, checkoutInput(cart.Token), "")
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}

	// Cash on delivery needs nothing further from the client, so the order is
	// confirmed immediately — but it is not paid until someone hands over cash.
	if result.Payment.Kind != IntentNone {
		t.Errorf("payment kind = %q, want %q", result.Payment.Kind, IntentNone)
	}
	order := result.Order
	if order.Status != OrderConfirmed {
		t.Errorf("status = %q, want %q", order.Status, OrderConfirmed)
	}
	if order.PaymentStatus != PaymentPending {
		t.Errorf("payment status = %q, want %q", order.PaymentStatus, PaymentPending)
	}
	if order.Total.AmountMinor != 5000 {
		t.Errorf("total = %d, want 5000", order.Total.AmountMinor)
	}
	if order.AccessToken == "" {
		t.Error("checkout must return the access token: it is the only way a guest reads their order back")
	}

	// The sale is committed, so the units have left the shelf entirely rather
	// than merely being reserved.
	onHand, reserved := variantStock(t, app, variant.ID)
	if onHand != 8 || reserved != 0 {
		t.Errorf("stock = (on hand %d, reserved %d), want (8, 0)", onHand, reserved)
	}

	if _, err := app.DrainOutbox(ctx); err != nil {
		t.Fatalf("drain outbox: %v", err)
	}
	if got := rec.count(EventOrderCreated); got != 1 {
		t.Errorf("order.created notifications = %d, want 1", got)
	}

	// The guest can read their order back with the token, and cannot without.
	if _, err := app.Order().GetForGuest(ctx, order.Number, order.AccessToken); err != nil {
		t.Errorf("guest lookup with the right token: %v", err)
	}
	if _, err := app.Order().GetForGuest(ctx, order.Number, "wrong-token"); err == nil {
		t.Error("a wrong access token must not return the order")
	}

	// Settle, ship, deliver.
	if _, err := app.Pay().MarkPaid(ctx, order.ID, "cash"); err != nil {
		t.Fatalf("mark paid: %v", err)
	}
	shipped, err := app.Ship().Create(ctx, order.ID, ProviderManual, ShipRequest{Tracking: "TRACK-1"})
	if err != nil {
		t.Fatalf("ship: %v", err)
	}
	if shipped.Status != OrderShipped || len(shipped.Fulfillments) != 1 {
		t.Errorf("after shipping: status %q with %d fulfillments", shipped.Status, len(shipped.Fulfillments))
	}
	delivered, err := app.Order().MarkDelivered(ctx, order.ID)
	if err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if delivered.Status != OrderDelivered {
		t.Errorf("status = %q, want %q", delivered.Status, OrderDelivered)
	}

	if _, err := app.DrainOutbox(ctx); err != nil {
		t.Fatalf("drain outbox: %v", err)
	}
	for _, event := range []string{EventOrderPaid, EventOrderShipped, EventOrderDelivered} {
		if got := rec.count(event); got != 1 {
			t.Errorf("%s notifications = %d, want 1", event, got)
		}
	}

	pending, dead, err := app.PendingEvents(ctx)
	if err != nil {
		t.Fatalf("pending events: %v", err)
	}
	if pending != 0 || dead != 0 {
		t.Errorf("outbox left %d pending and %d dead events", pending, dead)
	}
}

// TestConcurrentCheckoutCannotOversell is the requirement that justifies
// PostgreSQL. Many shoppers race for the last unit; exactly one may win, and
// stock may never go negative.
func TestConcurrentCheckoutCannotOversell(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	product := simpleProduct(t, app, "SCARCE-1", 1000, 1)
	variant := product.DefaultVariant()

	const shoppers = 12
	carts := make([]string, shoppers)
	for i := range carts {
		cart := newCart(t, app)
		addToCart(t, app, cart.Token, variant.ID, 1)
		carts[i] = cart.Token
	}

	var succeeded, conflicted int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < shoppers; i++ {
		wg.Add(1)
		go func(cartToken string) {
			defer wg.Done()
			<-start
			_, err := app.Order().Checkout(ctx, CodeCOD, checkoutInput(cartToken), "")
			switch {
			case err == nil:
				atomic.AddInt64(&succeeded, 1)
			case Conflictf("").Is(err):
				atomic.AddInt64(&conflicted, 1)
			default:
				t.Errorf("unexpected checkout error: %v", err)
			}
		}(carts[i])
	}
	close(start)
	wg.Wait()

	if succeeded != 1 {
		t.Errorf("%d checkouts succeeded for 1 unit of stock, want exactly 1", succeeded)
	}
	if conflicted != shoppers-1 {
		t.Errorf("%d checkouts were rejected, want %d", conflicted, shoppers-1)
	}

	onHand, reserved := variantStock(t, app, variant.ID)
	if onHand != 0 || reserved != 0 {
		t.Errorf("stock = (on hand %d, reserved %d), want (0, 0)", onHand, reserved)
	}
	if onHand < 0 {
		t.Fatal("stock went negative: the reservation is not atomic")
	}
}

// TestIdempotencyKeyPreventsDoubleOrder covers the double-tapped button and
// the mobile network that retried underneath the user.
func TestIdempotencyKeyPreventsDoubleOrder(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	product := simpleProduct(t, app, "IDEM-1", 1500, 5)
	cart := newCart(t, app)
	addToCart(t, app, cart.Token, product.DefaultVariant().ID, 1)

	in := checkoutInput(cart.Token)
	first, err := app.Order().Checkout(ctx, CodeCOD, in, "key-abc")
	if err != nil {
		t.Fatalf("first checkout: %v", err)
	}
	second, err := app.Order().Checkout(ctx, CodeCOD, in, "key-abc")
	if err != nil {
		t.Fatalf("replayed checkout: %v", err)
	}

	if first.Order.ID != second.Order.ID {
		t.Errorf("replay created a second order (%d then %d)", first.Order.ID, second.Order.ID)
	}
	orders, total, err := app.Order().List(ctx, OrderQuery{})
	if err != nil {
		t.Fatalf("list orders: %v", err)
	}
	if total != 1 || len(orders) != 1 {
		t.Errorf("order count = %d, want 1", total)
	}

	// Only one unit should have been sold, not two.
	onHand, _ := variantStock(t, app, product.DefaultVariant().ID)
	if onHand != 4 {
		t.Errorf("stock on hand = %d, want 4", onHand)
	}

	// The same key with a different body is a client bug, and answering it
	// with the first request's result would hide that.
	different := in
	different.Email = "someone.else@example.com"
	if _, err := app.Order().Checkout(ctx, CodeCOD, different, "key-abc"); err == nil {
		t.Error("reusing a key for a different request should be refused")
	}
}

// TestMarkPaidIsIdempotent covers the webhook a gateway sends twice.
func TestMarkPaidIsIdempotent(t *testing.T) {
	rec := &recordingNotifier{}
	app := newTestApp(t, &notifyModule{rec: rec})
	ctx := context.Background()

	product := simpleProduct(t, app, "PAY-1", 2000, 3)
	cart := newCart(t, app)
	addToCart(t, app, cart.Token, product.DefaultVariant().ID, 1)
	result, err := app.Order().Checkout(ctx, CodeCOD, checkoutInput(cart.Token), "")
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}

	for i := 0; i < 3; i++ {
		if _, err := app.Pay().MarkPaid(ctx, result.Order.ID, "ref-1"); err != nil {
			t.Fatalf("mark paid (attempt %d): %v", i+1, err)
		}
	}

	order, err := app.Order().Get(ctx, result.Order.ID)
	if err != nil {
		t.Fatalf("get order: %v", err)
	}
	if order.PaymentStatus != PaymentPaid {
		t.Errorf("payment status = %q, want paid", order.PaymentStatus)
	}
	// Settling three times must not sell the stock three times.
	onHand, reserved := variantStock(t, app, product.DefaultVariant().ID)
	if onHand != 2 || reserved != 0 {
		t.Errorf("stock = (on hand %d, reserved %d), want (2, 0)", onHand, reserved)
	}

	if _, err := app.DrainOutbox(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if got := rec.count(EventOrderPaid); got != 1 {
		t.Errorf("order.paid notifications = %d, want 1 — a replayed webhook must not re-announce the sale", got)
	}
}

// TestGatewayOrderBecomesShippable is the bug this engine must not have: a
// gateway-paid order that nobody can ship because nothing confirmed it.
func TestGatewayOrderBecomesShippable(t *testing.T) {
	app := newTestApp(t, &gatewayModule{})
	ctx := context.Background()

	product := simpleProduct(t, app, "GATE-1", 4000, 2)
	cart := newCart(t, app)
	addToCart(t, app, cart.Token, product.DefaultVariant().ID, 1)

	result, err := app.Order().Checkout(ctx, "testgateway", checkoutInput(cart.Token), "")
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}
	if result.Payment.Kind != IntentClientAction {
		t.Fatalf("payment kind = %q, want client_action", result.Payment.Kind)
	}
	// Before payment the order is pending and its stock is only reserved.
	if result.Order.Status != OrderPending {
		t.Errorf("status = %q, want pending before payment", result.Order.Status)
	}
	onHand, reserved := variantStock(t, app, product.DefaultVariant().ID)
	if onHand != 2 || reserved != 1 {
		t.Errorf("stock = (on hand %d, reserved %d), want (2, 1)", onHand, reserved)
	}

	if _, err := app.Ship().Create(ctx, result.Order.ID, ProviderManual, ShipRequest{}); err == nil {
		t.Error("an unpaid, unconfirmed order must not be shippable")
	}

	// The gateway's webhook arrives.
	paid, err := app.Pay().MarkPaid(ctx, result.Order.ID, "pi_123")
	if err != nil {
		t.Fatalf("mark paid: %v", err)
	}
	if paid.Status != OrderConfirmed {
		t.Fatalf("status after payment = %q, want confirmed — otherwise the order can never ship", paid.Status)
	}
	if _, err := app.Ship().Create(ctx, result.Order.ID, ProviderManual, ShipRequest{Tracking: "T-1"}); err != nil {
		t.Errorf("a paid order should be shippable: %v", err)
	}
}

// gatewayModule stands in for a real payment gateway: it needs a client
// action and settles later, exactly like Stripe.
type gatewayModule struct{}

func (m *gatewayModule) Name() string            { return "testgateway" }
func (m *gatewayModule) Migrations() []Migration { return nil }
func (m *gatewayModule) Register(app *App) error { app.RegisterPayment(m); return nil }
func (m *gatewayModule) Code() string            { return "testgateway" }

func (m *gatewayModule) Initiate(ctx context.Context, o *Order, opts PayOptions) (PaymentIntent, error) {
	return PaymentIntent{
		Kind: IntentClientAction, Reference: "pi_" + o.Number,
		ClientData: map[string]string{"client_secret": "secret_" + o.Number},
	}, nil
}

// TestCancelRestoresInventory checks both cancellation paths, which move
// different quantities: a pending order only reserved stock, while a confirmed
// one already took it off the shelf.
func TestCancelRestoresInventory(t *testing.T) {
	ctx := context.Background()

	t.Run("cancelling a confirmed order restocks it", func(t *testing.T) {
		app := newTestApp(t)
		product := simpleProduct(t, app, "CANC-1", 900, 5)
		cart := newCart(t, app)
		addToCart(t, app, cart.Token, product.DefaultVariant().ID, 2)
		result, err := app.Order().Checkout(ctx, CodeCOD, checkoutInput(cart.Token), "")
		if err != nil {
			t.Fatalf("checkout: %v", err)
		}
		if onHand, _ := variantStock(t, app, product.DefaultVariant().ID); onHand != 3 {
			t.Fatalf("stock after sale = %d, want 3", onHand)
		}
		if _, err := app.Order().Cancel(ctx, result.Order.ID, "customer changed their mind"); err != nil {
			t.Fatalf("cancel: %v", err)
		}
		onHand, reserved := variantStock(t, app, product.DefaultVariant().ID)
		if onHand != 5 || reserved != 0 {
			t.Errorf("stock after cancel = (on hand %d, reserved %d), want (5, 0)", onHand, reserved)
		}
	})

	t.Run("cancelling a pending order releases the reservation", func(t *testing.T) {
		app := newTestApp(t, &gatewayModule{})
		product := simpleProduct(t, app, "CANC-2", 900, 5)
		cart := newCart(t, app)
		addToCart(t, app, cart.Token, product.DefaultVariant().ID, 2)
		result, err := app.Order().Checkout(ctx, "testgateway", checkoutInput(cart.Token), "")
		if err != nil {
			t.Fatalf("checkout: %v", err)
		}
		if onHand, reserved := variantStock(t, app, product.DefaultVariant().ID); onHand != 5 || reserved != 2 {
			t.Fatalf("stock while pending = (%d, %d), want (5, 2)", onHand, reserved)
		}
		if _, err := app.Order().Cancel(ctx, result.Order.ID, "payment abandoned"); err != nil {
			t.Fatalf("cancel: %v", err)
		}
		onHand, reserved := variantStock(t, app, product.DefaultVariant().ID)
		if onHand != 5 || reserved != 0 {
			t.Errorf("stock after cancel = (%d, %d), want (5, 0) — the units were never sold", onHand, reserved)
		}
	})

	t.Run("a shipped order cannot be cancelled", func(t *testing.T) {
		app := newTestApp(t)
		product := simpleProduct(t, app, "CANC-3", 900, 5)
		cart := newCart(t, app)
		addToCart(t, app, cart.Token, product.DefaultVariant().ID, 1)
		result, _ := app.Order().Checkout(ctx, CodeCOD, checkoutInput(cart.Token), "")
		if _, err := app.Ship().Create(ctx, result.Order.ID, ProviderManual, ShipRequest{}); err != nil {
			t.Fatalf("ship: %v", err)
		}
		if _, err := app.Order().Cancel(ctx, result.Order.ID, "too late"); err == nil {
			t.Error("cancelling a shipped order should be refused — that is a return")
		}
	})
}

// TestVariantCombinationUniqueness proves the constraint is in the database,
// not merely in the service that happens to check it.
func TestVariantCombinationUniqueness(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	product, err := app.Products().CreateProduct(ctx, ProductInput{
		Title: "Shirt", Status: ProductActive,
		Options: []OptionInput{
			{Name: "Size", Values: []string{"S", "M"}},
			{Name: "Color", Values: []string{"Black", "White"}},
		},
		Variants: []VariantInput{
			{SKU: "SH-S-BLK", PriceMinor: 1000, Options: []string{"S", "Black"}},
			{SKU: "SH-M-BLK", PriceMinor: 1000, Options: []string{"M", "Black"}},
		},
	})
	if err != nil {
		t.Fatalf("create product: %v", err)
	}
	if len(product.Variants) != 2 {
		t.Fatalf("variants = %d, want 2", len(product.Variants))
	}

	// The same combination, named in the other order, is the same combination.
	_, err = app.Products().CreateVariant(ctx, product.ID, VariantInput{
		SKU: "SH-DUPE", PriceMinor: 1000, Options: []string{"Black", "M"},
	})
	if err == nil {
		t.Fatal("a duplicate option combination must be rejected")
	}
	if !strings.Contains(err.Error(), "combination") {
		t.Errorf("error = %v, want it to name the duplicate combination", err)
	}

	// A different combination is fine.
	if _, err := app.Products().CreateVariant(ctx, product.ID, VariantInput{
		SKU: "SH-S-WHT", PriceMinor: 1000, Options: []string{"S", "White"},
	}); err != nil {
		t.Errorf("a new combination should be accepted: %v", err)
	}
}

// TestSingleVariantProductHasOneDefault checks the zero-configuration case,
// and that the schema will not let a second default sneak in.
func TestSingleVariantProductHasOneDefault(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	product := simpleProduct(t, app, "SIMPLE-1", 500, 1)
	if len(product.Variants) != 1 {
		t.Fatalf("variants = %d, want 1", len(product.Variants))
	}
	if _, err := app.Products().CreateVariant(ctx, product.ID, VariantInput{
		SKU: "SIMPLE-1-B", PriceMinor: 500,
	}); err == nil {
		t.Error("a product with no options must not accept a second variant")
	}
	if err := app.Products().DeleteVariant(ctx, product.Variants[0].ID); err == nil {
		t.Error("the last variant of a product must not be deletable")
	}
}

// TestOrderLinesSurviveCatalogChanges is what "snapshot" has to mean: an
// order from last year still says what was bought and what it cost.
// Re-adding a variant used to overwrite the line's price snapshot with the
// current price, which quietly repriced the units already in the cart. That
// erased the very evidence checkout's re-validation depends on: the whole
// point of the snapshot is that a price change is *noticed* and re-confirmed,
// not absorbed. A shopper who added at 10.00 and added one more after a rise
// would have been charged the new price for both, with no 409 and no prompt.
func TestAddLineKeepsTheOriginalPriceSnapshot(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	p := simpleProduct(t, app, "SNAP-1", 1000, 10)
	variantID := p.Variants[0].ID

	cart := newCart(t, app)
	addToCart(t, app, cart.Token, variantID, 1)

	newPrice := int64(1500)
	if _, err := app.Products().UpdateVariant(ctx, variantID, VariantPatch{
		PriceMinor: &newPrice,
	}); err != nil {
		t.Fatalf("reprice variant: %v", err)
	}

	updated := addToCart(t, app, cart.Token, variantID, 1)
	if len(updated.Lines) != 1 {
		t.Fatalf("cart has %d lines, want 1", len(updated.Lines))
	}
	line := updated.Lines[0]

	if line.Quantity != 2 {
		t.Errorf("quantity = %d, want 2", line.Quantity)
	}
	if line.UnitPrice.AmountMinor != 1000 {
		t.Errorf("snapshot price = %d, want the original 1000 — re-adding repriced the line",
			line.UnitPrice.AmountMinor)
	}
	// The live price is still reported alongside it, so a storefront can warn
	// before checkout rather than surprising the shopper at the end.
	if line.CurrentPrice.AmountMinor != newPrice {
		t.Errorf("current price = %d, want %d", line.CurrentPrice.AmountMinor, newPrice)
	}

	// And the divergence must actually stop checkout, which is what the
	// snapshot exists for.
	_, err := app.Order().Checkout(ctx, CodeCOD, checkoutInput(cart.Token), "")
	if err == nil {
		t.Fatal("checkout succeeded despite a changed price; the shopper was never asked")
	}
	if !errors.Is(err, ErrConflict) {
		t.Errorf("checkout error = %v, want a conflict naming the price change", err)
	}
}

func TestOrderLinesSurviveCatalogChanges(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	product := simpleProduct(t, app, "HIST-1", 3000, 5)
	cart := newCart(t, app)
	addToCart(t, app, cart.Token, product.DefaultVariant().ID, 1)
	result, err := app.Order().Checkout(ctx, CodeCOD, checkoutInput(cart.Token), "")
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}

	newPrice := int64(9999)
	if _, err := app.Products().UpdateVariant(ctx, product.DefaultVariant().ID,
		VariantPatch{PriceMinor: &newPrice}); err != nil {
		t.Fatalf("reprice: %v", err)
	}
	if err := app.Products().DeleteProduct(ctx, product.ID); err != nil {
		t.Fatalf("delete product: %v", err)
	}

	order, err := app.Order().Get(ctx, result.Order.ID)
	if err != nil {
		t.Fatalf("get order after the product was deleted: %v", err)
	}
	if len(order.Lines) != 1 {
		t.Fatalf("order lines = %d, want 1", len(order.Lines))
	}
	line := order.Lines[0]
	if line.UnitPrice.AmountMinor != 3000 {
		t.Errorf("line price = %d, want the 3000 it was sold at", line.UnitPrice.AmountMinor)
	}
	if line.SKU != "HIST-1" {
		t.Errorf("line sku = %q, want HIST-1", line.SKU)
	}
	if line.ProductID != nil {
		t.Error("the product reference should be nulled, not dangling")
	}
	if order.Total.AmountMinor != 3000 {
		t.Errorf("order total = %d, want 3000", order.Total.AmountMinor)
	}
}

// TestCheckoutRefusesOnPriceChange covers the promise that the engine never
// silently reprices a confirmed order.
func TestCheckoutRefusesOnPriceChange(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	product := simpleProduct(t, app, "PRICE-1", 1000, 5)
	variant := product.DefaultVariant()
	cart := newCart(t, app)
	addToCart(t, app, cart.Token, variant.ID, 1)

	newPrice := int64(1200)
	if _, err := app.Products().UpdateVariant(ctx, variant.ID, VariantPatch{PriceMinor: &newPrice}); err != nil {
		t.Fatalf("reprice: %v", err)
	}

	_, err := app.Order().Checkout(ctx, CodeCOD, checkoutInput(cart.Token), "")
	if err == nil {
		t.Fatal("checkout should refuse when the price moved")
	}
	var apiErr *APIError
	if !asAPIError(err, &apiErr) || apiErr.Code != "conflict" {
		t.Fatalf("error = %v, want a conflict", err)
	}
	conflicts, ok := apiErr.Details.([]LineConflict)
	if !ok || len(conflicts) != 1 || conflicts[0].Reason != ReasonPriceChanged {
		t.Fatalf("details = %#v, want one price_changed conflict", apiErr.Details)
	}
	if conflicts[0].CurrentPriceMinor != 1200 {
		t.Errorf("current price = %d, want 1200", conflicts[0].CurrentPriceMinor)
	}

	// No stock was reserved by the failed attempt, and the cart now shows the
	// new price so the shopper can confirm.
	if onHand, reserved := variantStock(t, app, variant.ID); onHand != 5 || reserved != 0 {
		t.Errorf("stock = (%d, %d), want (5, 0) after a refused checkout", onHand, reserved)
	}
	refreshed, err := app.Cart().GetByToken(ctx, cart.Token)
	if err != nil {
		t.Fatalf("get cart: %v", err)
	}
	if refreshed.Lines[0].UnitPrice.AmountMinor != 1200 {
		t.Errorf("cart price after conflict = %d, want the current 1200",
			refreshed.Lines[0].UnitPrice.AmountMinor)
	}

	// Confirming at the new price succeeds.
	if _, err := app.Order().Checkout(ctx, CodeCOD, checkoutInput(cart.Token), ""); err != nil {
		t.Errorf("checkout at the refreshed price: %v", err)
	}
}

// TestRollbackLeavesNoEvent is the other half of the outbox guarantee: an
// event exists if and only if the change it describes was committed.
func TestRollbackLeavesNoEvent(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	product := simpleProduct(t, app, "ROLL-1", 1000, 1)
	cart := newCart(t, app)
	addToCart(t, app, cart.Token, product.DefaultVariant().ID, 1)

	// Make the checkout fail after it has already written its event, by
	// selling the only unit out from under it.
	if _, err := app.Stock().SetOnHand(ctx, product.DefaultVariant().ID, 0, 0); err != nil {
		t.Fatalf("zero the stock: %v", err)
	}
	if _, err := app.Order().Checkout(ctx, CodeCOD, checkoutInput(cart.Token), ""); err == nil {
		t.Fatal("checkout should fail with no stock")
	}

	pending, dead, err := app.PendingEvents(ctx)
	if err != nil {
		t.Fatalf("pending events: %v", err)
	}
	if pending != 0 || dead != 0 {
		t.Errorf("a rolled-back checkout left %d pending and %d dead events; it must leave none",
			pending, dead)
	}
	orders, total, err := app.Order().List(ctx, OrderQuery{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 0 || len(orders) != 0 {
		t.Errorf("a rolled-back checkout left %d orders", total)
	}
}

// TestOutboxRetriesUntilTheConsumerRecovers is at-least-once delivery in
// practice: a handler that fails is asked again, and does not lose the event.
func TestOutboxRetriesUntilTheConsumerRecovers(t *testing.T) {
	flaky := &flakyModule{failures: 2}
	app := newTestApp(t, flaky)
	ctx := context.Background()

	product := simpleProduct(t, app, "RETRY-1", 1000, 5)
	cart := newCart(t, app)
	addToCart(t, app, cart.Token, product.DefaultVariant().ID, 1)
	if _, err := app.Order().Checkout(ctx, CodeCOD, checkoutInput(cart.Token), ""); err != nil {
		t.Fatalf("checkout: %v", err)
	}

	// First pass: the handler fails, so the event stays pending.
	if _, err := app.DrainOutbox(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}
	pending, dead, err := app.PendingEvents(ctx)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if pending == 0 {
		t.Fatal("a failed delivery must leave the event pending, not consume it")
	}
	if dead != 0 {
		t.Errorf("dead events = %d, want 0 — two failures is not enough to give up", dead)
	}

	// Backoff pushes the retry into the future, so bring it forward rather
	// than waiting out the exponential delay in a test.
	if _, err := app.DB().ExecContext(ctx,
		`UPDATE outbox_events SET available_at = now() WHERE published_at IS NULL`); err != nil {
		t.Fatalf("reset backoff: %v", err)
	}
	if _, err := app.DrainOutbox(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if _, err := app.DB().ExecContext(ctx,
		`UPDATE outbox_events SET available_at = now() WHERE published_at IS NULL`); err != nil {
		t.Fatalf("reset backoff: %v", err)
	}
	if _, err := app.DrainOutbox(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}

	pending, dead, err = app.PendingEvents(ctx)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if pending != 0 || dead != 0 {
		t.Errorf("after the consumer recovered: %d pending, %d dead; want none", pending, dead)
	}
	if got := flaky.attempts(); got < 3 {
		t.Errorf("handler attempts = %d, want at least 3 (two failures then a success)", got)
	}
}

// flakyModule fails a fixed number of deliveries, then succeeds.
type flakyModule struct {
	failures int
	seen     int64
}

func (m *flakyModule) Name() string            { return "flaky" }
func (m *flakyModule) Migrations() []Migration { return nil }
func (m *flakyModule) attempts() int64         { return atomic.LoadInt64(&m.seen) }

func (m *flakyModule) Register(app *App) error {
	app.Subscribe(EventOrderCreated, func(ctx context.Context, e Event) error {
		n := atomic.AddInt64(&m.seen, 1)
		if int(n) <= m.failures {
			return fmt.Errorf("consumer is having a bad day (attempt %d)", n)
		}
		return nil
	})
	return nil
}

// TestUnpaidOrderSweepReleasesStock proves an abandoned gateway redirect does
// not hold inventory out of sale forever.
func TestUnpaidOrderSweepReleasesStock(t *testing.T) {
	app := newTestApp(t, &gatewayModule{})
	ctx := context.Background()

	product := simpleProduct(t, app, "SWEEP-1", 1000, 4)
	cart := newCart(t, app)
	addToCart(t, app, cart.Token, product.DefaultVariant().ID, 3)
	result, err := app.Order().Checkout(ctx, "testgateway", checkoutInput(cart.Token), "")
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}
	if _, reserved := variantStock(t, app, product.DefaultVariant().ID); reserved != 3 {
		t.Fatalf("reserved = %d, want 3", reserved)
	}

	// Age the reservation past its deadline.
	if _, err := app.DB().ExecContext(ctx,
		`UPDATE orders SET reservation_expires_at = now() - interval '1 hour' WHERE id = $1`,
		result.Order.ID); err != nil {
		t.Fatalf("age the order: %v", err)
	}

	swept, err := app.Order().SweepUnpaid(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if swept != 1 {
		t.Errorf("swept %d orders, want 1", swept)
	}
	onHand, reserved := variantStock(t, app, product.DefaultVariant().ID)
	if onHand != 4 || reserved != 0 {
		t.Errorf("stock after sweep = (%d, %d), want (4, 0)", onHand, reserved)
	}
	order, err := app.Order().Get(ctx, result.Order.ID)
	if err != nil {
		t.Fatalf("get order: %v", err)
	}
	if order.Status != OrderCancelled {
		t.Errorf("status = %q, want cancelled", order.Status)
	}
}

// TestRefundRequiresACapableProvider: cash on delivery cannot refund, and says
// so rather than pretending.
func TestRefundRequiresACapableProvider(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	product := simpleProduct(t, app, "REF-1", 2000, 2)
	cart := newCart(t, app)
	addToCart(t, app, cart.Token, product.DefaultVariant().ID, 1)
	result, _ := app.Order().Checkout(ctx, CodeCOD, checkoutInput(cart.Token), "")

	if _, err := app.Pay().Refund(ctx, result.Order.ID, 0); err == nil {
		t.Error("refunding an unpaid order should be refused")
	}
	if _, err := app.Pay().MarkPaid(ctx, result.Order.ID, ""); err != nil {
		t.Fatalf("mark paid: %v", err)
	}
	_, err := app.Pay().Refund(ctx, result.Order.ID, 0)
	if err == nil {
		t.Fatal("cash on delivery cannot refund; that should be reported")
	}
	if !strings.Contains(err.Error(), "does not support refunds") {
		t.Errorf("error = %v, want it to say the method cannot refund", err)
	}
}

func asAPIError(err error, target **APIError) bool {
	for err != nil {
		if e, ok := err.(*APIError); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// Selling past zero is the one inventory rule a store opts into, so it gets a
// test on both sides of the switch: off, the last unit is the last unit; on,
// the sale goes through and the count says how far past zero the store now is.
func TestContinueSellingTakesTheOrderAnyway(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	product := simpleProduct(t, app, "OVERSELL-1", 1500, 1)
	variant := product.DefaultVariant()

	// Off — the default — is the existing guarantee: one unit, one sale.
	first := newCart(t, app)
	addToCart(t, app, first.Token, variant.ID, 1)
	if _, err := app.Order().Checkout(ctx, CodeCOD, checkoutInput(first.Token), ""); err != nil {
		t.Fatalf("first checkout: %v", err)
	}
	blocked := newCart(t, app)
	if _, err := app.Cart().AddLine(ctx, blocked.Token, variant.ID, 1); err == nil {
		t.Fatal("the cart took a second unit of a one-unit variant")
	}

	// On, and the same cart line is accepted.
	on := true
	if _, err := app.Products().UpdateVariant(ctx, variant.ID, VariantPatch{
		ContinueSelling: &on,
	}); err != nil {
		t.Fatalf("turn on continue_selling: %v", err)
	}
	addToCart(t, app, blocked.Token, variant.ID, 2)
	if _, err := app.Order().Checkout(ctx, CodeCOD, checkoutInput(blocked.Token), ""); err != nil {
		t.Fatalf("checkout with continue_selling on: %v", err)
	}

	// The count keeps counting rather than stopping at zero: a store that
	// oversold by two needs to see the two, which is the whole difference
	// between this and simply not tracking inventory.
	onHand, reserved := variantStock(t, app, variant.ID)
	if onHand-reserved != -2 {
		t.Errorf("available = %d after overselling by 2, want -2 (on hand %d, reserved %d)",
			onHand-reserved, onHand, reserved)
	}

	// And it is still reported as sellable, which is what a storefront asks.
	v, err := app.Products().GetVariant(ctx, variant.ID)
	if err != nil {
		t.Fatalf("get variant: %v", err)
	}
	if !v.InStock(1) {
		t.Error("an oversellable variant reports itself out of stock")
	}
}

// Switching it back off restores the promise that stock never goes negative,
// which an already-oversold variant cannot keep. The refusal has to say so:
// "inventory inconsistent" from the CHECK leaves the operator with no idea that
// the way out is to add stock.
func TestContinueSellingOffAgainRefusesFromNegative(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	product := simpleProduct(t, app, "OVERSELL-2", 1500, 1)
	variant := product.DefaultVariant()
	on, off := true, false
	if _, err := app.Products().UpdateVariant(ctx, variant.ID, VariantPatch{ContinueSelling: &on}); err != nil {
		t.Fatalf("turn on: %v", err)
	}
	cart := newCart(t, app)
	addToCart(t, app, cart.Token, variant.ID, 3)
	if _, err := app.Order().Checkout(ctx, CodeCOD, checkoutInput(cart.Token), ""); err != nil {
		t.Fatalf("oversold checkout: %v", err)
	}
	_, err := app.Products().UpdateVariant(ctx, variant.ID, VariantPatch{ContinueSelling: &off})
	if err == nil {
		t.Fatal("switched off while oversold, leaving stock negative for a variant that promises it is not")
	}
	if !strings.Contains(err.Error(), "oversold by 2") {
		t.Errorf("refused with %q, which does not say how far past zero it is", err.Error())
	}

	// Restock to cover the hole and the switch goes off, as it must — the rule
	// is "not while negative", not "never again".
	if _, err := app.Stock().Adjust(ctx, variant.ID, 0, 2); err != nil {
		t.Fatalf("restock: %v", err)
	}
	if _, err := app.Products().UpdateVariant(ctx, variant.ID, VariantPatch{ContinueSelling: &off}); err != nil {
		t.Fatalf("turn off after restocking: %v", err)
	}

	after := newCart(t, app)
	if _, err := app.Cart().AddLine(ctx, after.Token, variant.ID, 1); err == nil {
		t.Fatal("a variant back at zero kept selling after the switch went off")
	}
}

// ------------------------------------------------------------ editing an order

// Editing an order moves stock, and which pool it moves depends on how far the
// order got. These pin both sides of that fork: getting it wrong either sells a
// unit twice or loses one off the shelf.

func TestEditOrderBeforePaymentAdjustsTheReservation(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	product := simpleProduct(t, app, "EDIT-1", 2500, 10)
	variant := product.DefaultVariant()
	cart := newCart(t, app)
	addToCart(t, app, cart.Token, variant.ID, 4)
	result, err := app.Order().Checkout(ctx, CodeCOD, checkoutInput(cart.Token), "")
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}
	order := result.Order

	// COD confirms on the spot, so this walks the order back to pending: an
	// unpaid order holds a reservation rather than having taken the units.
	if _, err := app.DB().ExecContext(ctx,
		`UPDATE orders SET status = 'pending' WHERE id = $1`, order.ID); err != nil {
		t.Fatalf("set pending: %v", err)
	}
	if _, err := app.DB().ExecContext(ctx,
		`UPDATE variant_stock SET on_hand = 10, reserved = 4 WHERE variant_id = $1`,
		variant.ID); err != nil {
		t.Fatalf("reset stock: %v", err)
	}

	edited, change, err := app.Order().EditLines(ctx, order.ID, OrderEdit{
		Lines: []OrderLineEdit{{ID: order.Lines[0].ID, Quantity: 2}},
	})
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if edited.Total.AmountMinor != 5000 {
		t.Errorf("total = %d after halving the line, want 5000", edited.Total.AmountMinor)
	}
	if change.BalanceMinor != -5000 {
		t.Errorf("balance = %d, want -5000 owed back to the customer", change.BalanceMinor)
	}
	onHand, reserved := variantStock(t, app, variant.ID)
	if onHand != 10 || reserved != 2 {
		t.Errorf("stock = %d on hand, %d reserved; want 10 and 2 — the reservation shrinks, not the shelf",
			onHand, reserved)
	}
}

func TestEditOrderAfterConfirmationMovesTheShelf(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	product := simpleProduct(t, app, "EDIT-2", 2500, 10)
	variant := product.DefaultVariant()
	cart := newCart(t, app)
	addToCart(t, app, cart.Token, variant.ID, 4)
	result, err := app.Order().Checkout(ctx, CodeCOD, checkoutInput(cart.Token), "")
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}
	order := result.Order
	if order.Status != OrderConfirmed {
		t.Fatalf("expected a confirmed order to test the committed side, got %s", order.Status)
	}
	onHand, reserved := variantStock(t, app, variant.ID)
	if onHand != 6 || reserved != 0 {
		t.Fatalf("stock = %d/%d after a confirmed sale, want 6/0", onHand, reserved)
	}

	// Down two: the units go back on the shelf.
	_, change, err := app.Order().EditLines(ctx, order.ID, OrderEdit{
		Lines: []OrderLineEdit{{ID: order.Lines[0].ID, Quantity: 2}},
	})
	if err != nil {
		t.Fatalf("reduce: %v", err)
	}
	if onHand, reserved = variantStock(t, app, variant.ID); onHand != 8 || reserved != 0 {
		t.Errorf("stock = %d/%d after removing two, want 8/0", onHand, reserved)
	}
	if len(change.LinesChanged) != 1 || !strings.Contains(change.LinesChanged[0], "4 to 2") {
		t.Errorf("change = %v, want it to say the line went 4 to 2", change.LinesChanged)
	}

	// Up three: they come off it.
	if _, _, err := app.Order().EditLines(ctx, order.ID, OrderEdit{
		Lines: []OrderLineEdit{{ID: order.Lines[0].ID, Quantity: 5}},
	}); err != nil {
		t.Fatalf("increase: %v", err)
	}
	if onHand, reserved = variantStock(t, app, variant.ID); onHand != 5 || reserved != 0 {
		t.Errorf("stock = %d/%d after going up to five, want 5/0", onHand, reserved)
	}
}

func TestEditOrderAddsALineAtTodaysPrice(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	product := simpleProduct(t, app, "EDIT-3", 2500, 10)
	extra := simpleProduct(t, app, "EDIT-3-EXTRA", 999, 5)
	variant := product.DefaultVariant()
	added := extra.DefaultVariant()

	cart := newCart(t, app)
	addToCart(t, app, cart.Token, variant.ID, 1)
	result, err := app.Order().Checkout(ctx, CodeCOD, checkoutInput(cart.Token), "")
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}
	order := result.Order

	// The extra is repriced after the order was placed. The amendment is agreed
	// now, so the line added by it is at the price it is now.
	newPrice := int64(1499)
	if _, err := app.Products().UpdateVariant(ctx, added.ID, VariantPatch{PriceMinor: &newPrice}); err != nil {
		t.Fatalf("reprice: %v", err)
	}

	edited, change, err := app.Order().EditLines(ctx, order.ID, OrderEdit{
		Lines: []OrderLineEdit{
			{ID: order.Lines[0].ID, Quantity: 1},
			{VariantID: added.ID, Quantity: 2},
		},
	})
	if err != nil {
		t.Fatalf("add a line: %v", err)
	}
	if len(edited.Lines) != 2 {
		t.Fatalf("order has %d lines, want 2", len(edited.Lines))
	}
	var line *OrderLine
	for i := range edited.Lines {
		if edited.Lines[i].SKU == "EDIT-3-EXTRA" {
			line = &edited.Lines[i]
		}
	}
	if line == nil {
		t.Fatal("the added line is not on the order")
	}
	if line.UnitPrice.AmountMinor != 1499 {
		t.Errorf("added line priced at %d, want today's 1499", line.UnitPrice.AmountMinor)
	}
	if edited.Total.AmountMinor != 2500+2*1499 {
		t.Errorf("total = %d, want %d", edited.Total.AmountMinor, 2500+2*1499)
	}
	if change.BalanceMinor != 2*1499 {
		t.Errorf("balance = %d, want the customer to owe %d", change.BalanceMinor, 2*1499)
	}
	if onHand, _ := variantStock(t, app, added.ID); onHand != 3 {
		t.Errorf("the added line left %d on hand, want 3", onHand)
	}
}

func TestEditOrderRefusesWhatItShould(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	product := simpleProduct(t, app, "EDIT-4", 2500, 10)
	variant := product.DefaultVariant()
	cart := newCart(t, app)
	addToCart(t, app, cart.Token, variant.ID, 2)
	result, err := app.Order().Checkout(ctx, CodeCOD, checkoutInput(cart.Token), "")
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}
	order := result.Order

	// An order cannot be emptied: that is a cancellation, which has its own
	// operation and its own event.
	if _, _, err := app.Order().EditLines(ctx, order.ID, OrderEdit{
		Lines: []OrderLineEdit{{ID: order.Lines[0].ID, Quantity: 0}},
	}); err == nil {
		t.Error("emptied an order through an edit")
	} else if !strings.Contains(err.Error(), "cancel") {
		t.Errorf("refusal = %q, which does not point at cancelling", err.Error())
	}

	// A line the order does not have is not addressable.
	if _, _, err := app.Order().EditLines(ctx, order.ID, OrderEdit{
		Lines: []OrderLineEdit{{ID: order.Lines[0].ID + 9999, Quantity: 1}},
	}); err == nil {
		t.Error("accepted a line id the order does not have")
	}

	// More than the shelf holds is refused, and the order is left alone.
	if _, _, err := app.Order().EditLines(ctx, order.ID, OrderEdit{
		Lines: []OrderLineEdit{{ID: order.Lines[0].ID, Quantity: 500}},
	}); err == nil {
		t.Error("an edit sold more than the shelf holds")
	}
	after, err := app.Order().Get(ctx, order.ID)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if after.Lines[0].Quantity != 2 || after.Total.AmountMinor != 5000 {
		t.Errorf("a refused edit changed the order: %d at %d",
			after.Lines[0].Quantity, after.Total.AmountMinor)
	}
}

// The amendment is the part an edited order no longer says about itself, so it
// has to be in the event stream instead.
func TestEditOrderAnnouncesItself(t *testing.T) {
	notifier := &recordingNotifier{}
	app := newTestApp(t, &notifyModule{rec: notifier})
	ctx := context.Background()

	product := simpleProduct(t, app, "EDIT-5", 2500, 10)
	variant := product.DefaultVariant()
	cart := newCart(t, app)
	addToCart(t, app, cart.Token, variant.ID, 3)
	result, err := app.Order().Checkout(ctx, CodeCOD, checkoutInput(cart.Token), "")
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}

	if _, _, err := app.Order().EditLines(ctx, result.Order.ID, OrderEdit{
		Lines: []OrderLineEdit{{ID: result.Order.Lines[0].ID, Quantity: 1}},
	}); err != nil {
		t.Fatalf("edit: %v", err)
	}
	if _, err := app.DrainOutbox(ctx); err != nil {
		t.Fatalf("drain outbox: %v", err)
	}
	if got := notifier.count(EventOrderEdited); got != 1 {
		t.Errorf("order.edited delivered %d times, want 1", got)
	}

	// An edit that changes nothing says nothing.
	before := notifier.count(EventOrderEdited)
	if _, _, err := app.Order().EditLines(ctx, result.Order.ID, OrderEdit{
		Lines: []OrderLineEdit{{ID: result.Order.Lines[0].ID, Quantity: 1}},
	}); err != nil {
		t.Fatalf("no-op edit: %v", err)
	}
	if _, err := app.DrainOutbox(ctx); err != nil {
		t.Fatalf("drain outbox: %v", err)
	}
	if got := notifier.count(EventOrderEdited); got != before {
		t.Error("an edit that changed nothing announced itself")
	}
}

// TestMarkUnpaid covers what it is for: taking back a payment somebody recorded
// on the wrong order, without disturbing anything else about it.
//
// The stock assertions are the point. A confirmed order awaiting payment is a
// normal state — every cash-on-delivery order is one — so unpaying must not put
// the units back on the shelf while the order is still live.
func TestMarkUnpaid(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	product := simpleProduct(t, app, "UNPAY-1", 2000, 3)
	variant := product.DefaultVariant().ID
	cart := newCart(t, app)
	addToCart(t, app, cart.Token, variant, 1)
	result, err := app.Order().Checkout(ctx, CodeCOD, checkoutInput(cart.Token), "")
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}
	id := result.Order.ID

	if _, err := app.Pay().MarkPaid(ctx, id, "ref-oops"); err != nil {
		t.Fatalf("mark paid: %v", err)
	}
	onHand, reserved := variantStock(t, app, variant)

	order, err := app.Pay().MarkUnpaid(ctx, id)
	if err != nil {
		t.Fatalf("mark unpaid: %v", err)
	}
	if order.PaymentStatus != PaymentPending {
		t.Errorf("payment status = %q, want pending", order.PaymentStatus)
	}
	if order.Status != OrderConfirmed {
		t.Errorf("status = %q, want the order left where it was", order.Status)
	}
	if order.PaymentReference != "" {
		t.Errorf("payment reference = %q, want it gone with the payment", order.PaymentReference)
	}
	if got, want := stockPair(t, app, variant), [2]int{onHand, reserved}; got != want {
		t.Errorf("stock moved to %v, want %v — unpaying is not a cancellation", got, want)
	}

	// Idempotent, and payable again afterwards.
	if _, err := app.Pay().MarkUnpaid(ctx, id); err != nil {
		t.Fatalf("second mark unpaid: %v", err)
	}
	again, err := app.Pay().MarkPaid(ctx, id, "ref-real")
	if err != nil {
		t.Fatalf("mark paid again: %v", err)
	}
	if again.PaymentStatus != PaymentPaid {
		t.Errorf("payment status = %q, want paid", again.PaymentStatus)
	}
	if got, want := stockPair(t, app, variant), [2]int{onHand, reserved}; got != want {
		t.Errorf("stock moved to %v across the round trip, want %v", got, want)
	}
}

func stockPair(t *testing.T, app *App, variantID int64) [2]int {
	t.Helper()
	onHand, reserved := variantStock(t, app, variantID)
	return [2]int{onHand, reserved}
}

// TestMarkUnpaidAfterShippingIsAllowed is the cash-on-delivery case: the parcel
// goes out unpaid and the payment is recorded when the money arrives at the
// door, so a payment marked on the wrong row is most likely to be found after
// the order has shipped. Refusing there would rule out the main use.
func TestMarkUnpaidAfterShippingIsAllowed(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	product := simpleProduct(t, app, "UNPAY-2", 2000, 3)
	cart := newCart(t, app)
	addToCart(t, app, cart.Token, product.DefaultVariant().ID, 1)
	result, err := app.Order().Checkout(ctx, CodeCOD, checkoutInput(cart.Token), "")
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}
	id := result.Order.ID
	if _, err := app.Ship().Create(ctx, id, ProviderManual, ShipRequest{Tracking: "1Z999AA10123456784"}); err != nil {
		t.Fatalf("ship: %v", err)
	}
	if _, err := app.Pay().MarkPaid(ctx, id, "cash"); err != nil {
		t.Fatalf("mark paid: %v", err)
	}

	order, err := app.Pay().MarkUnpaid(ctx, id)
	if err != nil {
		t.Fatalf("mark unpaid after shipping: %v", err)
	}
	if order.PaymentStatus != PaymentPending {
		t.Errorf("payment status = %q, want pending", order.PaymentStatus)
	}
	if order.Status != OrderShipped {
		t.Errorf("status = %q, want the shipment untouched", order.Status)
	}
}

// A refund is a payment that happened and came back. There is nothing to erase,
// and offering to would let an operator hide a real movement of money.
func TestMarkUnpaidRefusesARefund(t *testing.T) {
	app := newTestApp(t, refundableModule{})
	ctx := context.Background()

	product := simpleProduct(t, app, "UNPAY-3", 2000, 3)
	cart := newCart(t, app)
	addToCart(t, app, cart.Token, product.DefaultVariant().ID, 1)
	result, err := app.Order().Checkout(ctx, "refundable", checkoutInput(cart.Token), "")
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}
	id := result.Order.ID
	if _, err := app.Pay().MarkPaid(ctx, id, "ref"); err != nil {
		t.Fatalf("mark paid: %v", err)
	}
	if _, err := app.Pay().Refund(ctx, id, 0); err != nil {
		t.Fatalf("refund: %v", err)
	}
	if _, err := app.Pay().MarkUnpaid(ctx, id); err == nil {
		t.Error("a refunded order was allowed to become unpaid")
	}
}

// TestMarkUndelivered walks the delivery back, including the fulfillment rows
// that were marked with it.
func TestMarkUndelivered(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	product := simpleProduct(t, app, "UNDEL-1", 2000, 3)
	cart := newCart(t, app)
	addToCart(t, app, cart.Token, product.DefaultVariant().ID, 1)
	result, err := app.Order().Checkout(ctx, CodeCOD, checkoutInput(cart.Token), "")
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}
	id := result.Order.ID
	if _, err := app.Pay().MarkPaid(ctx, id, "ref"); err != nil {
		t.Fatalf("mark paid: %v", err)
	}
	if _, err := app.Ship().Create(ctx, id, ProviderManual, ShipRequest{Tracking: "EE123456789IN"}); err != nil {
		t.Fatalf("ship: %v", err)
	}
	if _, err := app.Order().MarkDelivered(ctx, id); err != nil {
		t.Fatalf("deliver: %v", err)
	}

	order, err := app.Order().MarkUndelivered(ctx, id)
	if err != nil {
		t.Fatalf("undeliver: %v", err)
	}
	if order.Status != OrderShipped {
		t.Errorf("status = %q, want shipped", order.Status)
	}
	if len(order.Fulfillments) != 1 || order.Fulfillments[0].Status != "shipped" {
		t.Errorf("fulfillments = %+v, want the one row back to shipped", order.Fulfillments)
	}

	// Idempotent, and it can be delivered again afterwards.
	if _, err := app.Order().MarkUndelivered(ctx, id); err != nil {
		t.Fatalf("second undeliver: %v", err)
	}
	if _, err := app.Order().MarkDelivered(ctx, id); err != nil {
		t.Fatalf("deliver again: %v", err)
	}
}

// TestShipDetectsTheCarrier and TestUpdateFulfillment cover the tracking
// number: it names its carrier on the way in, and correcting it re-answers the
// question rather than leaving the old answer behind.
func TestShipDetectsTheCarrier(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	product := simpleProduct(t, app, "CARRY-1", 2000, 3)
	cart := newCart(t, app)
	addToCart(t, app, cart.Token, product.DefaultVariant().ID, 1)
	result, err := app.Order().Checkout(ctx, CodeCOD, checkoutInput(cart.Token), "")
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}
	id := result.Order.ID
	if _, err := app.Pay().MarkPaid(ctx, id, "ref"); err != nil {
		t.Fatalf("mark paid: %v", err)
	}
	order, err := app.Ship().Create(ctx, id, ProviderManual, ShipRequest{Tracking: "1Z999AA10123456784"})
	if err != nil {
		t.Fatalf("ship: %v", err)
	}
	if len(order.Fulfillments) != 1 {
		t.Fatalf("fulfillments = %d, want 1", len(order.Fulfillments))
	}
	f := order.Fulfillments[0]
	if f.Carrier != "ups" || f.CarrierName != "UPS" {
		t.Errorf("carrier = %q/%q, want ups/UPS", f.Carrier, f.CarrierName)
	}
	if f.TrackingURL == "" {
		t.Error("a recognised carrier should come with a link to follow the parcel")
	}

	// Correcting the number re-identifies the carrier.
	fixed, err := app.Ship().Update(ctx, f.ID, FulfillmentPatch{Tracking: strPtr("EE123456789IN")})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if fixed.Tracking != "EE123456789IN" {
		t.Errorf("tracking = %q", fixed.Tracking)
	}
	if fixed.Carrier != "india-post" {
		t.Errorf("carrier = %q, want the new number's carrier, not the old one's", fixed.Carrier)
	}

	// An explicit carrier wins over the guess: the operator can see the parcel.
	forced, err := app.Ship().Update(ctx, f.ID, FulfillmentPatch{
		Tracking: strPtr("12345678901"), Carrier: strPtr("bluedart"),
	})
	if err != nil {
		t.Fatalf("update with an explicit carrier: %v", err)
	}
	if forced.Carrier != "bluedart" {
		t.Errorf("carrier = %q, want the one that was asked for", forced.Carrier)
	}

	// A number that names nobody leaves the field empty rather than keeping a
	// carrier that described a different number.
	cleared, err := app.Ship().Update(ctx, f.ID, FulfillmentPatch{Tracking: strPtr("nonsense")})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if cleared.Carrier != "" {
		t.Errorf("carrier = %q, want none", cleared.Carrier)
	}
	if _, err := app.Ship().Update(ctx, f.ID, FulfillmentPatch{Carrier: strPtr("not-a-carrier")}); err == nil {
		t.Error("an unknown carrier was accepted")
	}

	// Correcting the number does not un-ship the order.
	after, err := app.Order().Get(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if after.Status != OrderShipped {
		t.Errorf("status = %q, want the order still shipped", after.Status)
	}
}

func strPtr(s string) *string { return &s }

// TestUpdateOrderCorrectsContactAndPayment covers what the patch is for, and
// the two things it must refuse.
func TestUpdateOrderCorrectsContactAndPayment(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	product := simpleProduct(t, app, "PATCH-1", 2000, 3)
	cart := newCart(t, app)
	addToCart(t, app, cart.Token, product.DefaultVariant().ID, 1)
	result, err := app.Order().Checkout(ctx, CodeCOD, checkoutInput(cart.Token), "")
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}
	id := result.Order.ID

	name, email, phone := "Corrected Name", "right@example.com", "+919000000000"
	addr := Address{Line1: "9 New Road", City: "Mysuru", PostalCode: "570001", Country: "IN"}
	ref := "UTR-991122"
	order, err := app.Order().Update(ctx, id, OrderPatch{
		Name: &name, Email: &email, Phone: &phone, Address: &addr, PaymentReference: &ref,
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if order.Name != name || order.Email != email || order.Phone != phone {
		t.Errorf("contact = %q/%q/%q", order.Name, order.Email, order.Phone)
	}
	if order.Address.City != "Mysuru" {
		t.Errorf("address = %+v", order.Address)
	}
	if order.PaymentReference != ref {
		t.Errorf("reference = %q, want %q", order.PaymentReference, ref)
	}
	// It reads back the same way, so the write actually landed.
	again, err := app.Order().Get(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if again.Email != email || again.Address.City != "Mysuru" {
		t.Errorf("re-read = %q / %+v", again.Email, again.Address)
	}

	// An order with no email is an order the customer never hears about.
	empty := "  "
	if _, err := app.Order().Update(ctx, id, OrderPatch{Email: &empty}); err == nil {
		t.Error("an empty email was accepted")
	}
	// A method this build does not have would be a refund booked through
	// nothing.
	bogus := "not-installed"
	if _, err := app.Order().Update(ctx, id, OrderPatch{PaymentProvider: &bogus}); err == nil {
		t.Error("an uninstalled payment method was accepted")
	}
	// Nothing here may move the money or the state.
	if again.PaymentStatus != result.Order.PaymentStatus || again.Status != result.Order.Status {
		t.Errorf("a contact edit changed the order's state to %s/%s", again.Status, again.PaymentStatus)
	}
}

// TestDeleteFulfillmentWalksTheOrderBack is the consequence worth pinning: the
// order was shipped because that record existed, so removing the last one has
// to leave a state somebody can act on.
func TestDeleteFulfillmentWalksTheOrderBack(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	product := simpleProduct(t, app, "DELSHIP-1", 2000, 3)
	cart := newCart(t, app)
	addToCart(t, app, cart.Token, product.DefaultVariant().ID, 1)
	result, err := app.Order().Checkout(ctx, CodeCOD, checkoutInput(cart.Token), "")
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}
	id := result.Order.ID

	shipped, err := app.Ship().Create(ctx, id, ProviderManual, ShipRequest{Tracking: "EE123456789IN"})
	if err != nil {
		t.Fatalf("ship: %v", err)
	}
	if shipped.Status != OrderShipped || len(shipped.Fulfillments) != 1 {
		t.Fatalf("after shipping = %s with %d shipments", shipped.Status, len(shipped.Fulfillments))
	}

	back, err := app.Ship().Delete(ctx, shipped.Fulfillments[0].ID)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if back.Status != OrderConfirmed {
		t.Errorf("status = %q, want confirmed — nothing is shipping it any more", back.Status)
	}
	if len(back.Fulfillments) != 0 {
		t.Errorf("fulfillments = %d, want none", len(back.Fulfillments))
	}
	// And it can ship again, which is the point of putting it back rather than
	// leaving it stranded.
	if _, err := app.Ship().Create(ctx, id, ProviderManual, ShipRequest{Tracking: "1Z999AA10123456784"}); err != nil {
		t.Fatalf("ship again: %v", err)
	}

	// A parcel somebody received is not a record to erase.
	if _, err := app.Order().MarkDelivered(ctx, id); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	delivered, err := app.Order().Get(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if _, err := app.Ship().Delete(ctx, delivered.Fulfillments[0].ID); err == nil {
		t.Error("a delivered order let its shipment be deleted")
	}
	if _, err := app.Ship().Delete(ctx, 987654); err == nil {
		t.Error("deleting a fulfillment that does not exist was allowed")
	}
}

// TestOrderLineImages: the picture is what the product looks like now, joined
// on the way out, and a variant's own wins over the product's first.
func TestOrderLineImages(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	product := simpleProduct(t, app, "IMG-1", 2000, 3)
	// The media service has no exported accessor — it is reached through the
	// HTTP routes — and this test is in the package, so it uses the field.
	item, err := app.media.AddURL(ctx, "https://example.test/shirt.jpg", "image", "A shirt")
	if err != nil {
		t.Fatalf("add media: %v", err)
	}
	if err := app.media.SetProductMedia(ctx, product.ID, []int64{item.ID}); err != nil {
		t.Fatalf("set media: %v", err)
	}

	cart := newCart(t, app)
	addToCart(t, app, cart.Token, product.DefaultVariant().ID, 1)
	result, err := app.Order().Checkout(ctx, CodeCOD, checkoutInput(cart.Token), "")
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}
	order, err := app.Order().Get(ctx, result.Order.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(order.Lines) != 1 {
		t.Fatalf("lines = %d", len(order.Lines))
	}
	if order.Lines[0].ImageURL != "https://example.test/shirt.jpg" {
		t.Errorf("image = %q, want the product's picture", order.Lines[0].ImageURL)
	}
}

// ------------------------------------------------------ placing by hand

// An order an operator places is a sale, not a record of one: it must take
// stock, carry an access token the customer can use, and announce itself the
// same way a shopper's does. If any of those drift, there are two definitions
// of "order" in the engine.
func TestOperatorPlacedOrderIsAnOrdinaryOrder(t *testing.T) {
	notifier := &recordingNotifier{}
	app := newTestApp(t, &notifyModule{rec: notifier})
	ctx := context.Background()

	product := simpleProduct(t, app, "PHONE-1", 2500, 10)
	variant := product.DefaultVariant()

	result, err := app.Order().Create(ctx, NewOrderInput{
		Email: "caller@example.com",
		Name:  "A Caller",
		Phone: "+441234567890",
		Address: Address{
			Line1: "1 Test Street", City: "Testville",
			PostalCode: "12345", Country: "US",
		},
		Lines: []NewOrderLine{{VariantID: variant.ID, Quantity: 3}},
	})
	if err != nil {
		t.Fatalf("place the order: %v", err)
	}
	order := result.Order

	if order.Number == "" {
		t.Error("no order number")
	}
	// The access token is the whole of D22: no account, so this is how the
	// customer reads their own order back.
	if order.AccessToken == "" {
		t.Error("no access token, so the customer cannot look the order up")
	}
	if _, err := app.Order().GetForGuest(ctx, order.Number, order.AccessToken); err != nil {
		t.Errorf("the customer cannot read their own order: %v", err)
	}
	if order.Total.AmountMinor != 7500 {
		t.Errorf("total = %d, want 7500", order.Total.AmountMinor)
	}
	if order.Email != "caller@example.com" {
		t.Errorf("email = %q", order.Email)
	}

	// It took the stock, like any other sale.
	onHand, reserved := variantStock(t, app, variant.ID)
	if onHand != 7 || reserved != 0 {
		t.Errorf("stock = %d/%d after a confirmed phone order, want 7/0", onHand, reserved)
	}

	if _, err := app.DrainOutbox(ctx); err != nil {
		t.Fatalf("drain outbox: %v", err)
	}
	if got := notifier.count(EventOrderCreated); got != 1 {
		t.Errorf("order.created delivered %d times, want 1", got)
	}
}

// The refusals are the storefront's own, because the path is the storefront's.
func TestOperatorPlacedOrderRefusesWhatCheckoutRefuses(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	product := simpleProduct(t, app, "PHONE-2", 2500, 2)
	variant := product.DefaultVariant()
	good := Address{Line1: "1 Test Street", City: "Testville", PostalCode: "12345", Country: "US"}

	cases := []struct {
		name string
		in   NewOrderInput
		want string
	}{
		{
			name: "no lines",
			in:   NewOrderInput{Email: "a@example.com", Address: good},
			want: "at least one line",
		},
		{
			name: "no email",
			in: NewOrderInput{Address: good,
				Lines: []NewOrderLine{{VariantID: variant.ID, Quantity: 1}}},
			want: "email",
		},
		{
			name: "more than the shelf holds",
			in: NewOrderInput{Email: "a@example.com", Address: good,
				Lines: []NewOrderLine{{VariantID: variant.ID, Quantity: 99}}},
			want: "stock",
		},
		{
			name: "a variant that does not exist",
			in: NewOrderInput{Email: "a@example.com", Address: good,
				Lines: []NewOrderLine{{VariantID: variant.ID + 9999, Quantity: 1}}},
			want: "exist",
		},
		{
			name: "a payment method nobody registered",
			in: NewOrderInput{Email: "a@example.com", Address: good, PaymentMethod: "carrier-pigeon",
				Lines: []NewOrderLine{{VariantID: variant.ID, Quantity: 1}}},
			want: "payment method",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := app.Order().Create(ctx, tc.in)
			if err == nil {
				t.Fatalf("%s was accepted", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err.Error(), tc.want)
			}
		})
	}

	// None of that sold anything.
	if onHand, reserved := variantStock(t, app, variant.ID); onHand != 2 || reserved != 0 {
		t.Errorf("stock = %d/%d after five refusals, want 2/0", onHand, reserved)
	}
}

package gocommerce

import (
	"context"
	"strings"
	"testing"
)

// Customers are a reading of the orders, not a table — D22 says a shopper never
// needs an account, so "a customer" can only be every order sharing an email.
// These pin what that reading claims.
func TestCustomersAreGroupedOrders(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	product := simpleProduct(t, app, "CUST-1", 1000, 50)
	variant := product.DefaultVariant()
	address := Address{Line1: "1 Test Street", City: "Testville", PostalCode: "12345", Country: "US"}

	place := func(email, name string, qty int) *Order {
		t.Helper()
		res, err := app.Order().Create(ctx, NewOrderInput{
			Email: email, Name: name, Address: address,
			Lines: []NewOrderLine{{VariantID: variant.ID, Quantity: qty}},
		})
		if err != nil {
			t.Fatalf("place for %s: %v", email, err)
		}
		return res.Order
	}

	first := place("Regular@Example.com", "Reg Ular", 2)
	// The same person, typed differently: an email is a handle, not a string.
	place("regular@example.com", "Reg Ular Jr", 1)
	place("once@example.com", "One Timer", 1)

	// Only what was actually paid counts as spent.
	if _, err := app.Pay().MarkPaid(ctx, first.ID, "cash"); err != nil {
		t.Fatalf("mark paid: %v", err)
	}

	customers, total, err := app.Order().Customers(ctx, CustomerQuery{})
	if err != nil {
		t.Fatalf("customers: %v", err)
	}
	if total != 2 {
		t.Fatalf("total = %d customers, want 2 — the two spellings are one person", total)
	}

	var regular *Customer
	for _, c := range customers {
		if c.Email == "regular@example.com" {
			regular = c
		}
	}
	if regular == nil {
		t.Fatalf("the grouped customer is missing: %+v", customers)
	}
	if regular.Orders != 2 {
		t.Errorf("orders = %d, want 2", regular.Orders)
	}
	if regular.Spent.AmountMinor != 2000 {
		t.Errorf("spent = %d, want only the paid order's 2000", regular.Spent.AmountMinor)
	}
	// The newest order is what the store knows about them now.
	if regular.Name != "Reg Ular Jr" {
		t.Errorf("name = %q, want the most recent order's", regular.Name)
	}
	if regular.Address.City != "Testville" {
		t.Errorf("address did not survive the grouping: %+v", regular.Address)
	}
	if !regular.LastOrderAt.After(regular.FirstOrderAt) && regular.LastOrderAt != regular.FirstOrderAt {
		t.Error("last order is before the first")
	}
}

// A cancelled order still means the person exists; it is not a sale.
func TestCustomersCountAndSearch(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	product := simpleProduct(t, app, "CUST-2", 1000, 50)
	variant := product.DefaultVariant()
	address := Address{Line1: "1 Test Street", City: "Testville", PostalCode: "12345", Country: "US"}
	res, err := app.Order().Create(ctx, NewOrderInput{
		Email: "gone@example.com", Name: "Cancelled Carol", Address: address,
		Lines: []NewOrderLine{{VariantID: variant.ID, Quantity: 1}},
	})
	if err != nil {
		t.Fatalf("place: %v", err)
	}
	if _, err := app.Order().Cancel(ctx, res.Order.ID, "changed their mind"); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	customers, _, err := app.Order().Customers(ctx, CustomerQuery{Search: "carol"})
	if err != nil {
		t.Fatalf("search by name: %v", err)
	}
	if len(customers) != 1 {
		t.Fatalf("search for a name found %d, want 1", len(customers))
	}
	if customers[0].Orders != 0 {
		t.Errorf("orders = %d after the only one was cancelled, want 0", customers[0].Orders)
	}
	if customers[0].Spent.AmountMinor != 0 {
		t.Errorf("spent = %d on a cancelled order", customers[0].Spent.AmountMinor)
	}

	byEmail, _, err := app.Order().Customers(ctx, CustomerQuery{Search: "GONE@example"})
	if err != nil {
		t.Fatalf("search by email: %v", err)
	}
	if len(byEmail) != 1 || byEmail[0].Email != "gone@example.com" {
		t.Errorf("case-insensitive email search returned %+v", byEmail)
	}

	none, _, err := app.Order().Customers(ctx, CustomerQuery{Search: "nobody-here"})
	if err != nil {
		t.Fatalf("search for nobody: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("a search that matches nothing returned %d", len(none))
	}
}

// The route is admin-only, like everything else that reads the whole book.
func TestCustomersRouteNeedsAdmin(t *testing.T) {
	app := newTestApp(t)
	rec := do(t, app, "GET", "/api/admin/customers")
	if rec.Code != 401 {
		t.Errorf("unauthenticated = %d, want 401", rec.Code)
	}
	rec = do(t, app, "GET", "/api/admin/customers", withAdmin)
	if rec.Code != 200 {
		t.Fatalf("admin = %d: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"data"`) {
		t.Errorf("body has no data envelope: %s", rec.Body)
	}
}

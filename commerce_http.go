package gocommerce

import (
	"io"
	"net/http"
	"strings"
	"time"
)

// ------------------------------------------------------------------- carts

func (a *App) mountCartRoutes() {
	a.HandleFunc("POST /api/carts", a.handleCreateCart)
	a.HandleFunc("GET /api/carts/{cartId}", a.handleGetCart)
	a.HandleFunc("POST /api/carts/{cartId}/line-items", a.handleAddLineItem)
	a.HandleFunc("PATCH /api/carts/{cartId}/line-items/{lineId}", a.handleUpdateLineItem)
	a.HandleFunc("DELETE /api/carts/{cartId}/line-items/{lineId}", a.handleDeleteLineItem)
}

func (a *App) handleCreateCart(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email string `json:"email"`
	}
	// A cart may be created with no body at all: shopping starts before a
	// shopper has told you anything about themselves.
	if r.ContentLength > 0 {
		if err := DecodeJSON(w, r, &in); err != nil {
			RespondError(w, r, err)
			return
		}
	}
	cart, err := a.carts.Create(r.Context(), in.Email)
	if err != nil {
		RespondError(w, r, err)
		return
	}
	Respond(w, http.StatusCreated, cart)
}

func (a *App) handleGetCart(w http.ResponseWriter, r *http.Request) {
	cart, err := a.carts.GetByToken(r.Context(), r.PathValue("cartId"))
	if err != nil {
		RespondError(w, r, err)
		return
	}
	Respond(w, http.StatusOK, cart)
}

func (a *App) handleAddLineItem(w http.ResponseWriter, r *http.Request) {
	var in struct {
		VariantID int64 `json:"variant_id"`
		Quantity  int   `json:"quantity"`
	}
	if err := DecodeJSON(w, r, &in); err != nil {
		RespondError(w, r, err)
		return
	}
	if in.Quantity == 0 {
		in.Quantity = 1
	}
	cart, err := a.carts.AddLine(r.Context(), r.PathValue("cartId"), in.VariantID, in.Quantity)
	if err != nil {
		RespondError(w, r, err)
		return
	}
	Respond(w, http.StatusOK, cart)
}

func (a *App) handleUpdateLineItem(w http.ResponseWriter, r *http.Request) {
	lineID, err := pathInt64(r, "lineId")
	if err != nil {
		RespondError(w, r, err)
		return
	}
	var in struct {
		Quantity int `json:"quantity"`
	}
	if err := DecodeJSON(w, r, &in); err != nil {
		RespondError(w, r, err)
		return
	}
	cart, err := a.carts.UpdateLine(r.Context(), r.PathValue("cartId"), lineID, in.Quantity)
	if err != nil {
		RespondError(w, r, err)
		return
	}
	Respond(w, http.StatusOK, cart)
}

func (a *App) handleDeleteLineItem(w http.ResponseWriter, r *http.Request) {
	lineID, err := pathInt64(r, "lineId")
	if err != nil {
		RespondError(w, r, err)
		return
	}
	cart, err := a.carts.RemoveLine(r.Context(), r.PathValue("cartId"), lineID)
	if err != nil {
		RespondError(w, r, err)
		return
	}
	Respond(w, http.StatusOK, cart)
}

// ---------------------------------------------------------------- checkout

func (a *App) mountCheckoutRoutes() {
	a.HandleFunc("GET /api/checkout", a.handlePaymentMethods)
	a.HandleFunc("POST /api/checkout/{code}", a.handleCheckout)
	// Gateway callbacks ride a core-owned route so a payment module gets a
	// documented webhook URL without mounting anything itself — and so the
	// raw body reaches it untouched.
	a.HandleFunc("POST /api/checkout/{code}/webhook", a.handlePaymentWebhook)
}

func (a *App) handlePaymentMethods(w http.ResponseWriter, r *http.Request) {
	Respond(w, http.StatusOK, map[string]any{
		"payment_methods": a.payments.Methods(),
		"currency":        a.cfg.Currency,
	})
}

func (a *App) handleCheckout(w http.ResponseWriter, r *http.Request) {
	var in CheckoutInput
	if err := DecodeJSON(w, r, &in); err != nil {
		RespondError(w, r, err)
		return
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	result, err := a.orders.Checkout(r.Context(), r.PathValue("code"), in, key)
	if err != nil {
		RespondError(w, r, err)
		return
	}
	Respond(w, http.StatusCreated, result)
}

func (a *App) handlePaymentWebhook(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("code")
	provider, ok := a.payments.provider(code)
	if !ok {
		RespondError(w, r, NotFoundf("no payment method named %q", code))
		return
	}
	receiver, ok := provider.(WebhookProvider)
	if !ok {
		RespondError(w, r, NotFoundf("payment method %q does not accept webhooks", code))
		return
	}
	// No body-consuming middleware and no engine-imposed limit: signature
	// verification needs the bytes exactly as they were sent, and only the
	// provider knows how big its own callbacks get.
	receiver.Webhook().ServeHTTP(w, r)
}

// ------------------------------------------------------------------ orders

func (a *App) mountOrderRoutes() {
	// Guest order lookup. There is no account to log into, so the order's
	// access token is the credential.
	a.HandleFunc("GET /api/orders/{number}", a.handleGuestOrder)

	a.HandleAdminFunc("GET /api/admin/orders", a.handleListOrders, RightOrdersRead)
	a.HandleAdminFunc("GET /api/admin/customers", a.handleListCustomers, RightCustomersRead)
	a.HandleAdminFunc("POST /api/admin/orders", a.handleCreateOrder, RightOrdersWrite)
	a.HandleAdminFunc("GET /api/admin/orders/{id}", a.handleGetOrder, RightOrdersRead)
	a.HandleAdminFunc("POST /api/admin/orders/{id}/cancel", a.handleCancelOrder, RightOrdersWrite)
	a.HandleAdminFunc("POST /api/admin/orders/{id}/mark-paid", a.handleMarkPaid, RightOrdersWrite)
	a.HandleAdminFunc("POST /api/admin/orders/{id}/mark-unpaid", a.handleMarkUnpaid, RightOrdersWrite)
	a.HandleAdminFunc("POST /api/admin/orders/{id}/refund", a.handleRefund, RightOrdersRefund)
	a.HandleAdminFunc("POST /api/admin/orders/{id}/deliver", a.handleDeliver, RightOrdersWrite)
	a.HandleAdminFunc("POST /api/admin/orders/{id}/undeliver", a.handleUndeliver, RightOrdersWrite)
	a.HandleAdminFunc("PATCH /api/admin/orders/{id}", a.handleUpdateOrder, RightOrdersWrite)
	a.HandleAdminFunc("PUT /api/admin/orders/{id}/lines", a.handleEditOrderLines, RightOrdersWrite)
	a.HandleAdminFunc("POST /api/admin/create-fulfillment", a.handleCreateFulfillment, RightOrdersFulfill)
	a.HandleAdminFunc("PATCH /api/admin/fulfillments/{id}", a.handleUpdateFulfillment, RightOrdersFulfill)
	a.HandleAdminFunc("DELETE /api/admin/fulfillments/{id}", a.handleDeleteFulfillment, RightOrdersFulfill)
	a.HandleAdminFunc("GET /api/admin/carriers", a.handleListCarriers, RightOrdersRead)

	a.HandleAdminFunc("POST /api/admin/variants/{id}/inventory", a.handleAdjustInventory, RightInventoryWrite)
	a.HandleAdminFunc("GET /api/admin/inventory/low-stock", a.handleLowStock, RightInventoryRead)
}

func (a *App) handleGuestOrder(w http.ResponseWriter, r *http.Request) {
	tok := r.URL.Query().Get("token")
	if tok == "" {
		RespondError(w, r, Validationf("a token query parameter is required"))
		return
	}
	order, err := a.orders.GetForGuest(r.Context(), r.PathValue("number"), tok)
	if err != nil {
		RespondError(w, r, err)
		return
	}
	Respond(w, http.StatusOK, order)
}

func (a *App) handleListOrders(w http.ResponseWriter, r *http.Request) {
	limit, offset, err := Page(r)
	if err != nil {
		RespondError(w, r, err)
		return
	}
	q := r.URL.Query()
	query := OrderQuery{
		Status:        q.Get("status"),
		PaymentStatus: q.Get("payment_status"),
		Email:         q.Get("email"),
		Limit:         limit,
		Offset:        offset,
	}
	if query.From, err = parseDate(q.Get("from")); err != nil {
		RespondError(w, r, err)
		return
	}
	if query.To, err = parseDate(q.Get("to")); err != nil {
		RespondError(w, r, err)
		return
	}
	orders, total, err := a.orders.List(r.Context(), query)
	if err != nil {
		RespondError(w, r, err)
		return
	}
	RespondList(w, orders, ListMeta{Total: total, Limit: limit, Offset: offset})
}

func (a *App) handleGetOrder(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		RespondError(w, r, err)
		return
	}
	order, err := a.orders.Get(r.Context(), id)
	if err != nil {
		RespondError(w, r, err)
		return
	}
	Respond(w, http.StatusOK, order)
}

func (a *App) handleCancelOrder(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		RespondError(w, r, err)
		return
	}
	var in struct {
		Reason string `json:"reason"`
	}
	if r.ContentLength > 0 {
		if err := DecodeJSON(w, r, &in); err != nil {
			RespondError(w, r, err)
			return
		}
	}
	order, err := a.orders.Cancel(r.Context(), id, in.Reason)
	if err != nil {
		RespondError(w, r, err)
		return
	}
	Respond(w, http.StatusOK, order)
}

func (a *App) handleMarkPaid(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		RespondError(w, r, err)
		return
	}
	var in struct {
		Reference string `json:"reference"`
	}
	if r.ContentLength > 0 {
		if err := DecodeJSON(w, r, &in); err != nil {
			RespondError(w, r, err)
			return
		}
	}
	order, err := a.payments.MarkPaid(r.Context(), id, in.Reference)
	if err != nil {
		RespondError(w, r, err)
		return
	}
	Respond(w, http.StatusOK, order)
}

// handleMarkUnpaid takes back a payment recorded in error. No body: there is
// nothing to say about a payment that did not happen.
func (a *App) handleMarkUnpaid(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		RespondError(w, r, err)
		return
	}
	order, err := a.payments.MarkUnpaid(r.Context(), id)
	if err != nil {
		RespondError(w, r, err)
		return
	}
	Respond(w, http.StatusOK, order)
}

func (a *App) handleUndeliver(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		RespondError(w, r, err)
		return
	}
	order, err := a.orders.MarkUndelivered(r.Context(), id)
	if err != nil {
		RespondError(w, r, err)
		return
	}
	Respond(w, http.StatusOK, order)
}

// handleUpdateFulfillment corrects a tracking number after the fact.
func (a *App) handleUpdateFulfillment(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		RespondError(w, r, err)
		return
	}
	var patch FulfillmentPatch
	if err := DecodeJSON(w, r, &patch); err != nil {
		RespondError(w, r, err)
		return
	}
	f, err := a.fulfillment.Update(r.Context(), id, patch)
	if err != nil {
		RespondError(w, r, err)
		return
	}
	Respond(w, http.StatusOK, f)
}

// handleDeleteFulfillment removes a shipment recorded in error, and returns the
// order, whose status may have moved with it.
func (a *App) handleDeleteFulfillment(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		RespondError(w, r, err)
		return
	}
	order, err := a.fulfillment.Delete(r.Context(), id)
	if err != nil {
		RespondError(w, r, err)
		return
	}
	Respond(w, http.StatusOK, order)
}

// handleListCarriers serves the carriers the engine can name, and — given a
// tracking number — the ones whose numbering it fits, best first. The panel
// uses it to fill the field in and to offer the alternatives.
func (a *App) handleListCarriers(w http.ResponseWriter, r *http.Request) {
	list := Carriers()
	if tracking := r.URL.Query().Get("tracking"); tracking != "" {
		list = DetectCarriers(tracking)
		if list == nil {
			list = []Carrier{}
		}
	}
	RespondList(w, list, ListMeta{Total: len(list), Limit: len(list), Offset: 0})
}

// handleUpdateOrder corrects the contact details and the payment record. What
// the order *is* — its status, its money, its lines — has its own operations.
func (a *App) handleUpdateOrder(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		RespondError(w, r, err)
		return
	}
	var patch OrderPatch
	if err := DecodeJSON(w, r, &patch); err != nil {
		RespondError(w, r, err)
		return
	}
	order, err := a.orders.Update(r.Context(), id, patch)
	if err != nil {
		RespondError(w, r, err)
		return
	}
	Respond(w, http.StatusOK, order)
}

func (a *App) handleRefund(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		RespondError(w, r, err)
		return
	}
	var in struct {
		AmountMinor int64 `json:"amount_minor"`
	}
	if r.ContentLength > 0 {
		if err := DecodeJSON(w, r, &in); err != nil {
			RespondError(w, r, err)
			return
		}
	}
	order, err := a.payments.Refund(r.Context(), id, in.AmountMinor)
	if err != nil {
		RespondError(w, r, err)
		return
	}
	Respond(w, http.StatusOK, order)
}

func (a *App) handleDeliver(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		RespondError(w, r, err)
		return
	}
	order, err := a.orders.MarkDelivered(r.Context(), id)
	if err != nil {
		RespondError(w, r, err)
		return
	}
	Respond(w, http.StatusOK, order)
}

func (a *App) handleCreateFulfillment(w http.ResponseWriter, r *http.Request) {
	var in struct {
		OrderID  int64             `json:"order_id"`
		Provider string            `json:"provider"`
		Tracking string            `json:"tracking"`
		Carrier  string            `json:"carrier"`
		Meta     map[string]string `json:"meta"`
	}
	if err := DecodeJSON(w, r, &in); err != nil {
		RespondError(w, r, err)
		return
	}
	if in.OrderID <= 0 {
		RespondError(w, r, Validationf("order_id is required"))
		return
	}
	order, err := a.fulfillment.Create(r.Context(), in.OrderID, in.Provider,
		ShipRequest{Tracking: in.Tracking, Carrier: in.Carrier, Meta: in.Meta})
	if err != nil {
		RespondError(w, r, err)
		return
	}
	Respond(w, http.StatusCreated, order)
}

// --------------------------------------------------------------- inventory

func (a *App) handleAdjustInventory(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		RespondError(w, r, err)
		return
	}
	var in struct {
		// Exactly one: adjust moves the count, set replaces it. Both exist
		// because receiving stock and counting a shelf are different acts.
		Adjust *int `json:"adjust"`
		Set    *int `json:"set"`
		// LocationID is which shelf. Absent means the default one, which is
		// what a single-location store always sends and what every client
		// written before locations existed still sends.
		LocationID int64 `json:"location_id"`
	}
	if err := DecodeJSON(w, r, &in); err != nil {
		RespondError(w, r, err)
		return
	}
	switch {
	case in.Adjust != nil && in.Set != nil:
		RespondError(w, r, Validationf("send either adjust or set, not both"))
	case in.Adjust != nil:
		v, err := a.inventory.Adjust(r.Context(), id, in.LocationID, *in.Adjust)
		respondOr(w, r, v, err)
	case in.Set != nil:
		v, err := a.inventory.SetOnHand(r.Context(), id, in.LocationID, *in.Set)
		respondOr(w, r, v, err)
	default:
		RespondError(w, r, Validationf("send either adjust or set"))
	}
}

func (a *App) handleLowStock(w http.ResponseWriter, r *http.Request) {
	limit, offset, err := Page(r)
	if err != nil {
		RespondError(w, r, err)
		return
	}
	threshold := 5
	if s := r.URL.Query().Get("threshold"); s != "" {
		n, err := parseInt(s)
		if err != nil || n < 0 {
			RespondError(w, r, Validationf("threshold must be a non-negative integer"))
			return
		}
		threshold = n
	}
	variants, total, err := a.inventory.LowStock(r.Context(), threshold, limit, offset)
	if err != nil {
		RespondError(w, r, err)
		return
	}
	RespondList(w, variants, ListMeta{Total: total, Limit: limit, Offset: offset})
}

// ------------------------------------------------------------------ helpers

func respondOr(w http.ResponseWriter, r *http.Request, v any, err error) {
	if err != nil {
		RespondError(w, r, err)
		return
	}
	Respond(w, http.StatusOK, v)
}

func parseDate(s string) (*time.Time, error) {
	if s == "" {
		return nil, nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return &t, nil
		}
	}
	return nil, Validationf("%q is not a date (use YYYY-MM-DD or RFC 3339)", s)
}

func parseInt(s string) (int, error) {
	var n int
	var neg bool
	for i, r := range s {
		if i == 0 && r == '-' {
			neg = true
			continue
		}
		if r < '0' || r > '9' {
			return 0, Validationf("%q is not an integer", s)
		}
		n = n*10 + int(r-'0')
	}
	if neg {
		n = -n
	}
	return n, nil
}

// limitedBody caps a request body at n bytes for endpoints that accept
// uploads, which need more room than the JSON limit but not unlimited room.
func limitedBody(w http.ResponseWriter, r *http.Request, n int64) io.Reader {
	return http.MaxBytesReader(w, r.Body, n)
}

// handleEditOrderLines replaces what is on an order. The response carries what
// changed alongside the order, so the panel can tell the operator what the
// amendment came to — and what is now owed either way — rather than leaving
// them to compare two totals.
func (a *App) handleEditOrderLines(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		RespondError(w, r, err)
		return
	}
	var in OrderEdit
	if err := DecodeJSON(w, r, &in); err != nil {
		RespondError(w, r, err)
		return
	}
	order, change, err := a.orders.EditLines(r.Context(), id, in)
	if err != nil {
		RespondError(w, r, err)
		return
	}
	Respond(w, http.StatusOK, map[string]any{"order": order, "changed": change})
}

// handleCreateOrder places an order on a customer's behalf. It answers with the
// same shape checkout does — the order and the payment intent — because it is
// the same operation: the panel may still have to send the operator somewhere
// to take the money.
func (a *App) handleCreateOrder(w http.ResponseWriter, r *http.Request) {
	var in NewOrderInput
	if err := DecodeJSON(w, r, &in); err != nil {
		RespondError(w, r, err)
		return
	}
	result, err := a.orders.Create(r.Context(), in)
	if err != nil {
		RespondError(w, r, err)
		return
	}
	Respond(w, http.StatusCreated, result)
}

// handleListCustomers serves the orders grouped by who placed them. There is no
// customer resource behind it — see customers.go — so there is no GET by id, no
// PATCH and no DELETE: correcting a customer means correcting an order.
func (a *App) handleListCustomers(w http.ResponseWriter, r *http.Request) {
	limit, offset, err := Page(r)
	if err != nil {
		RespondError(w, r, err)
		return
	}
	customers, total, err := a.orders.Customers(r.Context(), CustomerQuery{
		Search: r.URL.Query().Get("q"),
		Limit:  limit,
		Offset: offset,
	})
	if err != nil {
		RespondError(w, r, err)
		return
	}
	RespondList(w, customers, ListMeta{Total: total, Limit: limit, Offset: offset})
}

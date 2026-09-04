package gocommerce

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Order statuses. Cancellation is a controlled transition from pending or
// confirmed, not a generic update.
const (
	OrderPending   = "pending"
	OrderConfirmed = "confirmed"
	OrderShipped   = "shipped"
	OrderDelivered = "delivered"
	OrderCancelled = "cancelled"
)

// Payment statuses.
const (
	PaymentPending  = "pending"
	PaymentPaid     = "paid"
	PaymentFailed   = "failed"
	PaymentRefunded = "refunded"
)

// Order is an immutable record of a sale in progress. Its lines and its
// customer details are snapshots: a historical order must stay readable and
// legally meaningful after the catalog moves on.
type Order struct {
	ID               int64  `json:"id"`
	Number           string `json:"number"`
	Status           string `json:"status"`
	PaymentStatus    string `json:"payment_status"`
	PaymentProvider  string `json:"payment_provider"`
	PaymentReference string `json:"payment_reference,omitempty"`
	Currency         string `json:"currency"`

	Subtotal Money `json:"subtotal"`
	Shipping Money `json:"shipping"`
	Discount Money `json:"discount"`
	// Tax is what was charged across every line. When TaxInclusive it is part
	// of Subtotal rather than added to it, which is what that flag is for.
	Tax          Money `json:"tax"`
	TaxInclusive bool  `json:"tax_inclusive"`
	Total        Money `json:"total"`

	Email   string  `json:"email"`
	Phone   string  `json:"phone,omitempty"`
	Name    string  `json:"name,omitempty"`
	Address Address `json:"address"`

	Language string `json:"language"`
	// Discounts is what this order was given, by value. It is a snapshot: the
	// rule that produced it may be edited or deleted, and this stays true.
	Discounts    []AppliedDiscount `json:"discounts,omitempty"`
	Lines        []OrderLine       `json:"line_items"`
	Fulfillments []Fulfillment     `json:"fulfillments"`
	Metadata     Metadata          `json:"metadata"`

	// AccessToken is how a guest reads their own order back. It is returned
	// once, at checkout, and never included in an admin listing.
	AccessToken string `json:"access_token,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// OrderLine is a frozen snapshot of what was bought.
type OrderLine struct {
	ID        int64  `json:"id"`
	ProductID *int64 `json:"product_id,omitempty"`
	VariantID *int64 `json:"variant_id,omitempty"`
	SKU       string `json:"sku"`
	Title     string `json:"title"`
	// ImageURL is what the product looks like *now*, filled in on the way out
	// and never stored. The rest of this row is a snapshot taken at checkout
	// and must not move; a picture is not part of what was agreed, it is how
	// somebody recognises the thing in the box. Empty when the product has been
	// deleted or never had a picture.
	ImageURL     string `json:"image_url,omitempty"`
	VariantLabel string `json:"variant_label,omitempty"`
	// Tax is what this line was charged, snapshotted like its price. An invoice
	// has to print the same figures next year as it does today, and a rate that
	// has since changed must not be able to rewrite them.
	Tax       LineTax `json:"tax"`
	Quantity  int     `json:"quantity"`
	UnitPrice Money   `json:"unit_price"`
	Total     Money   `json:"total"`
	// LocationID is the place these units were taken from. Recorded at
	// checkout so that cancelling puts them back where they were, and nil for
	// a line placed before M17 or one whose location has since been closed —
	// in which case the movement falls back to the default.
	LocationID *int64   `json:"location_id,omitempty"`
	Metadata   Metadata `json:"metadata"`
}

// Fulfillment is a shipment against an order.
type Fulfillment struct {
	ID       int64  `json:"id"`
	Provider string `json:"provider"`
	Tracking string `json:"tracking,omitempty"`
	// Carrier is who is actually carrying the parcel, which is a different
	// fact from Provider: a store that packs its own boxes books every
	// shipment as "manual" and hands them to whichever courier serves the
	// pincode. Empty when nobody has said.
	Carrier string `json:"carrier,omitempty"`
	// CarrierName and TrackingURL are derived from Carrier and Tracking on the
	// way out, never stored. A stored URL is a URL that goes stale the day the
	// carrier changes its site.
	CarrierName string    `json:"carrier_name,omitempty"`
	TrackingURL string    `json:"tracking_url,omitempty"`
	LabelURL    string    `json:"label_url,omitempty"`
	Status      string    `json:"status"`
	Metadata    Metadata  `json:"metadata"`
	CreatedAt   time.Time `json:"created_at"`
}

// decorate fills in the carrier's name and the link to follow the parcel.
func (f *Fulfillment) decorate() {
	if f.Carrier == "" {
		return
	}
	if c, ok := CarrierByCode(f.Carrier, f.Tracking); ok {
		f.CarrierName, f.TrackingURL = c.Name, c.TrackURL
	}
}

// stockCommitted reports whether the order's inventory has left the shelf, as
// opposed to merely being reserved. It is derived from status rather than
// stored, so the two can never disagree.
func stockCommitted(status string) bool {
	switch status {
	case OrderConfirmed, OrderShipped, OrderDelivered:
		return true
	}
	return false
}

// OrderQuery filters an order listing.
type OrderQuery struct {
	Status        string
	PaymentStatus string
	Email         string
	From, To      *time.Time
	Limit, Offset int
}

// Orders owns the order state machine. Every transition lives here — no
// integration gets to invent its own version of confirming or cancelling.
type Orders struct {
	app *App
}

// Order returns the order service.
func (a *App) Order() *Orders { return a.orders }

// ------------------------------------------------------------------ reading

const orderColumns = `o.id, o.number, o.status, o.payment_status, o.payment_provider,
	coalesce(o.payment_reference, ''), o.currency, o.subtotal_minor, o.shipping_minor,
	o.discount_minor, o.tax_minor, o.tax_inclusive, o.total_minor,
	o.email, coalesce(o.phone, ''), coalesce(o.name, ''),
	o.address, o.lang, o.metadata, o.created_at, o.updated_at`

func (s *Orders) scanOrder(row interface{ Scan(...any) error }) (*Order, error) {
	o := &Order{}
	var addr, meta []byte
	var subtotal, shipping, discount, tax, total int64
	if err := row.Scan(&o.ID, &o.Number, &o.Status, &o.PaymentStatus, &o.PaymentProvider,
		&o.PaymentReference, &o.Currency, &subtotal, &shipping, &discount,
		&tax, &o.TaxInclusive, &total,
		&o.Email, &o.Phone, &o.Name, &addr, &o.Language, &meta,
		&o.CreatedAt, &o.UpdatedAt); err != nil {
		return nil, err
	}
	o.Subtotal = money(subtotal, o.Currency)
	o.Shipping = money(shipping, o.Currency)
	o.Discount = money(discount, o.Currency)
	o.Tax = money(tax, o.Currency)
	o.Total = money(total, o.Currency)
	if len(addr) > 0 {
		if err := json.Unmarshal(addr, &o.Address); err != nil {
			return nil, err
		}
	}
	if err := scanMetadata(meta, &o.Metadata); err != nil {
		return nil, err
	}
	o.Lines = []OrderLine{}
	o.Fulfillments = []Fulfillment{}
	return o, nil
}

// Get loads an order by id.
func (s *Orders) Get(ctx context.Context, id int64) (*Order, error) {
	return s.getWhere(ctx, "o.id = $1", id)
}

// GetByNumber loads an order by its human number.
func (s *Orders) GetByNumber(ctx context.Context, number string) (*Order, error) {
	return s.getWhere(ctx, "o.number = $1", number)
}

// GetForGuest loads an order for a shopper holding its access token. The
// token is compared in constant time and a mismatch is reported as not-found,
// so the endpoint cannot be used to discover which order numbers exist.
func (s *Orders) GetForGuest(ctx context.Context, number, accessToken string) (*Order, error) {
	var stored string
	err := s.app.db.QueryRowContext(ctx,
		`SELECT access_token FROM orders WHERE number = $1`, number).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, NotFoundf("order not found")
	}
	if err != nil {
		return nil, err
	}
	if !constantTimeEqual(stored, accessToken) {
		return nil, NotFoundf("order not found")
	}
	return s.GetByNumber(ctx, number)
}

func (s *Orders) getWhere(ctx context.Context, where string, arg any) (*Order, error) {
	o, err := s.scanOrder(s.app.db.QueryRowContext(ctx,
		`SELECT `+orderColumns+` FROM orders o WHERE `+where, arg))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, NotFoundf("order not found")
		}
		return nil, err
	}
	if err := s.loadChildren(ctx, []*Order{o}); err != nil {
		return nil, err
	}
	return o, nil
}

// List returns a page of orders and the total matching count.
func (s *Orders) List(ctx context.Context, q OrderQuery) ([]*Order, int, error) {
	where, args := []string{"1 = 1"}, []any{}
	add := func(expr string, v any) {
		args = append(args, v)
		where = append(where, fmt.Sprintf(expr, len(args)))
	}
	if q.Status != "" {
		add("o.status = $%d", q.Status)
	}
	if q.PaymentStatus != "" {
		add("o.payment_status = $%d", q.PaymentStatus)
	}
	if q.Email != "" {
		add("lower(o.email) = $%d", strings.ToLower(q.Email))
	}
	if q.From != nil {
		add("o.created_at >= $%d", *q.From)
	}
	if q.To != nil {
		add("o.created_at < $%d", *q.To)
	}
	clause := strings.Join(where, " AND ")

	var total int
	if err := s.app.db.QueryRowContext(ctx,
		`SELECT count(*) FROM orders o WHERE `+clause, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	limit := q.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}
	args = append(args, limit, q.Offset)
	rows, err := s.app.db.QueryContext(ctx,
		`SELECT `+orderColumns+` FROM orders o WHERE `+clause+
			fmt.Sprintf(" ORDER BY o.id DESC LIMIT $%d OFFSET $%d", len(args)-1, len(args)), args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var orders []*Order
	for rows.Next() {
		o, err := s.scanOrder(rows)
		if err != nil {
			return nil, 0, err
		}
		orders = append(orders, o)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	if err := s.loadChildren(ctx, orders); err != nil {
		return nil, 0, err
	}
	return orders, total, nil
}

func (s *Orders) loadChildren(ctx context.Context, orders []*Order) error {
	if len(orders) == 0 {
		return nil
	}
	byID := make(map[int64]*Order, len(orders))
	ids := make([]int64, 0, len(orders))
	for _, o := range orders {
		byID[o.ID] = o
		ids = append(ids, o.ID)
	}

	lineRows, err := s.app.db.QueryContext(ctx, `
		SELECT id, order_id, product_id, variant_id, sku, title, variant_label,
		       quantity, unit_price_minor, total_minor, tax_minor, tax_rate_bp, tax_name,
		       location_id, metadata
		FROM order_lines WHERE order_id = ANY($1::bigint[]) ORDER BY id`, int64Array(ids))
	if err != nil {
		return err
	}
	defer lineRows.Close()
	for lineRows.Next() {
		var l OrderLine
		var orderID int64
		var productID, variantID, locationID sql.NullInt64
		var meta []byte
		if err := lineRows.Scan(&l.ID, &orderID, &productID, &variantID, &l.SKU, &l.Title,
			&l.VariantLabel, &l.Quantity, &l.UnitPrice.AmountMinor, &l.Total.AmountMinor,
			&l.Tax.AmountMinor, &l.Tax.RateBP, &l.Tax.Name, &locationID, &meta); err != nil {
			return err
		}
		if locationID.Valid {
			l.LocationID = &locationID.Int64
		}
		if productID.Valid {
			l.ProductID = &productID.Int64
		}
		if variantID.Valid {
			l.VariantID = &variantID.Int64
		}
		if err := scanMetadata(meta, &l.Metadata); err != nil {
			return err
		}
		if o := byID[orderID]; o != nil {
			l.UnitPrice.Currency = o.Currency
			l.Total.Currency = o.Currency
			o.Lines = append(o.Lines, l)
		}
	}
	if err := lineRows.Err(); err != nil {
		return err
	}

	if err := s.loadLineImages(ctx, orders); err != nil {
		return err
	}
	if err := s.loadOrderDiscounts(ctx, byID, ids); err != nil {
		return err
	}

	fRows, err := s.app.db.QueryContext(ctx, `
		SELECT id, order_id, provider, tracking, carrier, label_url, status, metadata, created_at
		FROM fulfillments WHERE order_id = ANY($1::bigint[]) ORDER BY id`, int64Array(ids))
	if err != nil {
		return err
	}
	defer fRows.Close()
	for fRows.Next() {
		var f Fulfillment
		var orderID int64
		var meta []byte
		if err := fRows.Scan(&f.ID, &orderID, &f.Provider, &f.Tracking, &f.Carrier,
			&f.LabelURL, &f.Status, &meta, &f.CreatedAt); err != nil {
			return err
		}
		if err := scanMetadata(meta, &f.Metadata); err != nil {
			return err
		}
		f.decorate()
		if o := byID[orderID]; o != nil {
			o.Fulfillments = append(o.Fulfillments, f)
		}
	}
	return fRows.Err()
}

// -------------------------------------------------------------- transitions

// Confirm moves a pending order to confirmed and turns its inventory
// reservation into a committed sale. It is idempotent: confirming an
// already-confirmed order is a no-op, because a webhook may well arrive twice.
func (s *Orders) Confirm(ctx context.Context, id int64) (*Order, error) {
	return s.transition(ctx, id, func(ctx context.Context, tx *sql.Tx, o *Order) (string, any, error) {
		switch o.Status {
		case OrderConfirmed, OrderShipped, OrderDelivered:
			return "", nil, nil // already done
		case OrderCancelled:
			return "", nil, Conflictf("order %s has been cancelled", o.Number)
		}
		if err := commitOrderStock(ctx, tx, o); err != nil {
			return "", nil, err
		}
		if err := setOrderStatus(ctx, tx, o.ID, OrderConfirmed); err != nil {
			return "", nil, err
		}
		o.Status = OrderConfirmed
		return "", nil, nil // order.paid or order.created already told the story
	})
}

// Cancel voids an order and returns its inventory. Which movement is correct
// depends on how far the order got: a pending order only ever reserved stock,
// while a confirmed one has already taken it off the shelf.
func (s *Orders) Cancel(ctx context.Context, id int64, reason string) (*Order, error) {
	return s.transition(ctx, id, func(ctx context.Context, tx *sql.Tx, o *Order) (string, any, error) {
		switch o.Status {
		case OrderCancelled:
			return "", nil, nil
		case OrderShipped, OrderDelivered:
			return "", nil, Conflictf("order %s has already shipped; cancelling it is a return, not a cancellation", o.Number)
		}
		if stockCommitted(o.Status) {
			if err := restockOrder(ctx, tx, o); err != nil {
				return "", nil, err
			}
		} else if err := releaseOrderStock(ctx, tx, o); err != nil {
			return "", nil, err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE orders SET status = $2, reservation_expires_at = NULL, updated_at = now()
			WHERE id = $1`, o.ID, OrderCancelled); err != nil {
			return "", nil, err
		}
		o.Status = OrderCancelled
		payload := s.eventPayload(o)
		payload.Reason = reason
		return EventOrderCancelled, payload, nil
	})
}

// MarkDelivered records that the customer received the order.
func (s *Orders) MarkDelivered(ctx context.Context, id int64) (*Order, error) {
	return s.transition(ctx, id, func(ctx context.Context, tx *sql.Tx, o *Order) (string, any, error) {
		if o.Status == OrderDelivered {
			return "", nil, nil
		}
		if o.Status != OrderShipped {
			return "", nil, Conflictf("order %s must be shipped before it can be delivered (it is %s)", o.Number, o.Status)
		}
		if err := setOrderStatus(ctx, tx, o.ID, OrderDelivered); err != nil {
			return "", nil, err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE fulfillments SET status = 'delivered', updated_at = now()
			 WHERE order_id = $1 AND status = 'shipped'`, o.ID); err != nil {
			return "", nil, err
		}
		o.Status = OrderDelivered
		return EventOrderDelivered, s.eventPayload(o), nil
	})
}

// MarkUndelivered takes back a delivery recorded by mistake: the order goes
// back to shipped, and so do the fulfillments that were marked with it.
//
// The simplest of the corrections, because delivery is only ever a record of
// something somebody observed. Nothing moved when it was set — no stock, no
// money — so nothing has to move back. It refuses from anywhere but delivered:
// there is no other state a delivery can be undone from.
func (s *Orders) MarkUndelivered(ctx context.Context, id int64) (*Order, error) {
	return s.transition(ctx, id, func(ctx context.Context, tx *sql.Tx, o *Order) (string, any, error) {
		if o.Status == OrderShipped {
			return "", nil, nil
		}
		if o.Status != OrderDelivered {
			return "", nil, Conflictf("order %s is not delivered (it is %s)", o.Number, o.Status)
		}
		if err := setOrderStatus(ctx, tx, o.ID, OrderShipped); err != nil {
			return "", nil, err
		}
		// Only the ones this delivery moved. A fulfillment cancelled separately
		// stays cancelled — it was not part of what is being undone.
		if _, err := tx.ExecContext(ctx,
			`UPDATE fulfillments SET status = 'shipped', updated_at = now()
			 WHERE order_id = $1 AND status = 'delivered'`, o.ID); err != nil {
			return "", nil, err
		}
		o.Status = OrderShipped
		return EventOrderUndelivered, s.eventPayload(o), nil
	})
}

// transition runs a state change and its event in one transaction, which is
// what makes the event and the change inseparable.
//
// The callback returns the event to emit, or an empty name to emit nothing —
// so an idempotent no-op transition stays quiet instead of announcing a change
// that did not happen.
func (s *Orders) transition(ctx context.Context, id int64,
	fn func(context.Context, *sql.Tx, *Order) (string, any, error)) (*Order, error) {

	err := InTx(ctx, s.app.db, func(tx *sql.Tx) error {
		o, err := lockOrder(ctx, tx, id)
		if err != nil {
			return err
		}
		if err := loadOrderLinesTx(ctx, tx, o); err != nil {
			return err
		}
		name, payload, err := fn(ctx, tx, o)
		if err != nil {
			return err
		}
		if name == "" {
			return nil
		}
		return s.app.outbox.write(ctx, tx, name, AggregateOrder, o.ID, payload)
	})
	if err != nil {
		return nil, err
	}
	s.app.nudgeOutbox()
	return s.Get(ctx, id)
}

// lockOrder reads an order FOR UPDATE so concurrent transitions serialize.
func lockOrder(ctx context.Context, tx *sql.Tx, id int64) (*Order, error) {
	o := &Order{}
	err := tx.QueryRowContext(ctx, `
		SELECT id, number, status, payment_status, payment_provider, currency,
		       total_minor, email, coalesce(phone,''), coalesce(name,''), lang
		FROM orders WHERE id = $1 FOR UPDATE`, id,
	).Scan(&o.ID, &o.Number, &o.Status, &o.PaymentStatus, &o.PaymentProvider, &o.Currency,
		&o.Total.AmountMinor, &o.Email, &o.Phone, &o.Name, &o.Language)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, NotFoundf("order %d does not exist", id)
	}
	if err != nil {
		return nil, err
	}
	o.Total.Currency = o.Currency
	return o, nil
}

func loadOrderLinesTx(ctx context.Context, tx *sql.Tx, o *Order) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, variant_id, sku, title, variant_label, quantity, unit_price_minor,
		       total_minor, tax_minor, tax_rate_bp, tax_name, location_id
		FROM order_lines WHERE order_id = $1 ORDER BY id`, o.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	o.Lines = nil
	for rows.Next() {
		var l OrderLine
		var variantID, locationID sql.NullInt64
		if err := rows.Scan(&l.ID, &variantID, &l.SKU, &l.Title, &l.VariantLabel,
			&l.Quantity, &l.UnitPrice.AmountMinor, &l.Total.AmountMinor,
			&l.Tax.AmountMinor, &l.Tax.RateBP, &l.Tax.Name, &locationID); err != nil {
			return err
		}
		if variantID.Valid {
			l.VariantID = &variantID.Int64
		}
		if locationID.Valid {
			l.LocationID = &locationID.Int64
		}
		l.UnitPrice.Currency = o.Currency
		l.Total.Currency = o.Currency
		o.Lines = append(o.Lines, l)
	}
	return rows.Err()
}

func setOrderStatus(ctx context.Context, tx *sql.Tx, id int64, status string) error {
	_, err := tx.ExecContext(ctx,
		`UPDATE orders SET status = $2, updated_at = now() WHERE id = $1`, id, status)
	return err
}

func commitOrderStock(ctx context.Context, tx *sql.Tx, o *Order) error {
	for _, l := range o.Lines {
		if l.VariantID == nil {
			continue
		}
		loc, err := lineLocation(ctx, tx, l)
		if err != nil {
			return err
		}
		if err := commitStock(ctx, tx, *l.VariantID, loc, l.Quantity); err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx,
		`UPDATE orders SET reservation_expires_at = NULL WHERE id = $1`, o.ID)
	return err
}

func releaseOrderStock(ctx context.Context, tx *sql.Tx, o *Order) error {
	for _, l := range o.Lines {
		if l.VariantID == nil {
			continue
		}
		loc, err := lineLocation(ctx, tx, l)
		if err != nil {
			return err
		}
		if err := releaseStock(ctx, tx, *l.VariantID, loc, l.Quantity); err != nil {
			return err
		}
	}
	return nil
}

func restockOrder(ctx context.Context, tx *sql.Tx, o *Order) error {
	for _, l := range o.Lines {
		if l.VariantID == nil {
			continue
		}
		loc, err := lineLocation(ctx, tx, l)
		if err != nil {
			return err
		}
		if err := restockStock(ctx, tx, *l.VariantID, loc, l.Quantity); err != nil {
			return err
		}
	}
	return nil
}

// eventPayload builds the public shape of an order event.
func (s *Orders) eventPayload(o *Order) *OrderEvent {
	ev := &OrderEvent{
		OrderID: o.ID, Number: o.Number, Status: o.Status,
		PaymentStatus: o.PaymentStatus, Provider: o.PaymentProvider,
		Currency: o.Currency, TotalMinor: o.Total.AmountMinor,
		Email: o.Email, Phone: o.Phone, Name: o.Name, Language: o.Language,
	}
	for _, l := range o.Lines {
		ev.Lines = append(ev.Lines, OrderEventLine{
			SKU: l.SKU, Title: l.Title, VariantLabel: l.VariantLabel,
			Quantity: l.Quantity, UnitPriceMinor: l.UnitPrice.AmountMinor,
			TotalMinor: l.Total.AmountMinor,
		})
	}
	return ev
}

// SweepUnpaid cancels pending orders whose payment never arrived, returning
// their stock to the shelf. Without it an abandoned redirect holds inventory
// out of sale forever, which is invisible until the day it sells out a product
// that is actually in stock.
func (s *Orders) SweepUnpaid(ctx context.Context) (int, error) {
	rows, err := s.app.db.QueryContext(ctx, `
		SELECT id FROM orders
		WHERE status = $1 AND payment_status = $2
		  AND reservation_expires_at IS NOT NULL AND reservation_expires_at < now()
		LIMIT 200`, OrderPending, PaymentPending)
	if err != nil {
		return 0, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	var swept int
	for _, id := range ids {
		if _, err := s.Cancel(ctx, id, "payment not completed in time"); err != nil {
			s.app.log.Warn("could not cancel expired order", "order_id", id, "error", err)
			continue
		}
		swept++
	}
	if swept > 0 {
		s.app.log.Info("released inventory from expired unpaid orders", "orders", swept)
	}
	return swept, nil
}

// ------------------------------------------------------------- editing lines

// OrderEdit is the desired set of lines on an order.
//
// The whole set, not a diff: the panel holds the lines it is showing, and
// "here is what the order should be" is the request it can actually make
// without racing another edit. A line the order already has is named by its id;
// one being added names a variant instead. A line the request leaves out is
// removed.
type OrderEdit struct {
	Lines []OrderLineEdit `json:"lines"`
}

// OrderLineEdit is one line of a desired order.
type OrderLineEdit struct {
	// ID names a line the order already has. Zero means this is a new line,
	// and VariantID says what to add.
	ID        int64 `json:"id"`
	VariantID int64 `json:"variant_id"`
	Quantity  int   `json:"quantity"`
}

// OrderChange reports what an edit did, so the operator can be told rather
// than left to compare two screens.
type OrderChange struct {
	LinesAdded   []string `json:"lines_added"`
	LinesRemoved []string `json:"lines_removed"`
	LinesChanged []string `json:"lines_changed"`
	TotalBefore  Money    `json:"total_before"`
	TotalAfter   Money    `json:"total_after"`
	// BalanceMinor is what the edit moved the total by: positive is owed by the
	// customer, negative is owed to them. It is reported rather than settled —
	// taking a payment or making a refund is its own operation, with its own
	// provider and its own record.
	BalanceMinor int64 `json:"balance_minor"`
}

// EditLines changes what an order is for, and moves the stock the change
// implies.
//
// PLAN §10.3 calls an order line an immutable snapshot, and the reason it gives
// is that a historical order must stay readable when the product behind it
// changes or is deleted. That reason is about the catalog moving underneath an
// order; it is not about the operator and the customer agreeing to something
// different. So the snapshot stays a snapshot — a line still holds its own sku,
// title and price, and still survives its variant being deleted — and the
// amendment is recorded as order.edited, carrying the totals either side of it.
// The order says what is now agreed; the event stream says how it got there.
//
// The stock movement is the same fork Cancel makes: an order that only reserved
// its stock has its reservation adjusted, and one that has already taken the
// units off the shelf gives them back or takes more.
//
// What it will not do is settle the money. The new total can be under what was
// paid or over it, and both are reported as a balance for the operator to act
// on — a refund is a provider operation with its own record, and inventing one
// here would hide it.
func (s *Orders) EditLines(ctx context.Context, id int64, in OrderEdit) (*Order, *OrderChange, error) {
	change := &OrderChange{}
	_, err := s.transition(ctx, id, func(ctx context.Context, tx *sql.Tx, o *Order) (string, any, error) {
		switch o.Status {
		case OrderCancelled:
			return "", nil, Conflictf("order %s has been cancelled", o.Number)
		case OrderShipped, OrderDelivered:
			return "", nil, Conflictf(
				"order %s has already shipped; changing what is in it is a return, not an edit", o.Number)
		}
		if o.PaymentStatus == PaymentRefunded {
			return "", nil, Conflictf("order %s has been refunded", o.Number)
		}

		existing := map[int64]*OrderLine{}
		for i := range o.Lines {
			existing[o.Lines[i].ID] = &o.Lines[i]
		}

		// The whole request is checked before anything moves: a half-applied
		// edit is worse than a refused one.
		seen := map[int64]bool{}
		for _, l := range in.Lines {
			if l.Quantity < 0 {
				return "", nil, Validationf("a quantity cannot be negative")
			}
			if l.ID == 0 {
				if l.VariantID == 0 {
					return "", nil, Validationf("a new line needs a variant_id")
				}
				continue
			}
			if _, ok := existing[l.ID]; !ok {
				return "", nil, Validationf("order %s has no line %d", o.Number, l.ID)
			}
			if seen[l.ID] {
				return "", nil, Validationf("line %d is named twice", l.ID)
			}
			seen[l.ID] = true
		}

		committed := stockCommitted(o.Status)

		// 1. The lines already there: requantified, or gone.
		for _, line := range o.Lines {
			want := 0
			for _, l := range in.Lines {
				if l.ID == line.ID {
					want = l.Quantity
				}
			}
			if want == line.Quantity {
				continue
			}
			if err := moveOrderStock(ctx, tx, line, want-line.Quantity, committed); err != nil {
				return "", nil, err
			}
			if want == 0 {
				if _, err := tx.ExecContext(ctx,
					`DELETE FROM order_lines WHERE id = $1`, line.ID); err != nil {
					return "", nil, err
				}
				change.LinesRemoved = append(change.LinesRemoved, line.SKU)
				continue
			}
			if _, err := tx.ExecContext(ctx, `
				UPDATE order_lines
				SET quantity = $2::integer, total_minor = unit_price_minor * $2::integer
				WHERE id = $1`, line.ID, want); err != nil {
				return "", nil, err
			}
			change.LinesChanged = append(change.LinesChanged,
				fmt.Sprintf("%s: %d to %d", line.SKU, line.Quantity, want))
		}

		// 2. New lines, priced as they are today: an amendment is agreed now,
		//    so today's price is the one that was agreed — not the price the
		//    rest of the order was placed at.
		for _, l := range in.Lines {
			if l.ID != 0 || l.Quantity == 0 {
				continue
			}
			sku, err := addOrderLine(ctx, tx, o, l.VariantID, l.Quantity, committed)
			if err != nil {
				return "", nil, err
			}
			change.LinesAdded = append(change.LinesAdded, sku)
		}

		// 3. Retotal from the lines that are actually there.
		var subtotal int64
		var lines int
		if err := tx.QueryRowContext(ctx, `
			SELECT coalesce(sum(total_minor), 0), count(*)
			FROM order_lines WHERE order_id = $1`, o.ID).Scan(&subtotal, &lines); err != nil {
			return "", nil, err
		}
		if lines == 0 {
			return "", nil, Validationf(
				"an order must keep at least one line; cancel order %s instead", o.Number)
		}

		var shipping, discount int64
		if err := tx.QueryRowContext(ctx,
			`SELECT shipping_minor, discount_minor FROM orders WHERE id = $1`,
			o.ID).Scan(&shipping, &discount); err != nil {
			return "", nil, err
		}
		// The discount follows the basket it came off (D24). A fixed amount is
		// a fixed amount whatever is left; a percentage was a percentage of a
		// basket that no longer exists, so it is taken again. An order that has
		// dropped below the minimum its discount required is refused rather
		// than quietly kept — the promotion it qualified for is one it no
		// longer qualifies for, and only an operator can decide what to do.
		discount, err := recomputeOrderDiscount(ctx, tx, o.ID, subtotal, discount)
		if err != nil {
			return "", nil, err
		}
		total := subtotal + shipping - discount
		if total < 0 {
			// A discount larger than what is left of the order. The floor is the
			// database's own CHECK; saying so beats a constraint violation.
			return "", nil, Conflictf(
				"the discount on order %s is larger than the lines that would be left", o.Number)
		}

		change.TotalBefore = money(o.Total.AmountMinor, o.Currency)
		change.TotalAfter = money(total, o.Currency)
		change.BalanceMinor = total - o.Total.AmountMinor

		if _, err := tx.ExecContext(ctx, `
			UPDATE orders SET subtotal_minor = $2, total_minor = $3,
			                  discount_minor = $4, updated_at = now()
			WHERE id = $1`, o.ID, subtotal, total, discount); err != nil {
			return "", nil, err
		}
		if len(change.LinesAdded) == 0 && len(change.LinesRemoved) == 0 &&
			len(change.LinesChanged) == 0 {
			return "", nil, nil // nothing happened, so there is nothing to announce
		}

		o.Total = change.TotalAfter
		payload := s.eventPayload(o)
		payload.Change = change
		return EventOrderEdited, payload, nil
	})
	if err != nil {
		return nil, nil, err
	}
	o, err := s.Get(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	return o, change, nil
}

// moveOrderStock applies a quantity delta to a variant, in whichever direction
// and against whichever pool the order's own state says is right.
//
// A line whose variant is gone — the product was deleted — moves nothing. The
// snapshot is still readable history; there is simply no shelf to put it back
// on.
func moveOrderStock(ctx context.Context, tx *sql.Tx, line OrderLine, delta int, committed bool) error {
	if line.VariantID == nil || delta == 0 {
		return nil
	}
	loc, err := lineLocation(ctx, tx, line)
	if err != nil {
		return err
	}
	switch {
	case delta > 0 && committed:
		return sellStock(ctx, tx, *line.VariantID, loc, delta)
	case delta > 0:
		return reserveStock(ctx, tx, *line.VariantID, loc, delta)
	case committed:
		return restockStock(ctx, tx, *line.VariantID, loc, -delta)
	default:
		return releaseStock(ctx, tx, *line.VariantID, loc, -delta)
	}
}

// lineLocation is where this line's units live. A line from before M17, or one
// whose location has since been closed, has none recorded; the default is the
// only honest answer left, and it is at least somewhere the store can count.
//
// It also makes sure the row exists, because a movement against a missing
// (variant, location) pair matches nothing and would silently lose the units.
func lineLocation(ctx context.Context, tx *sql.Tx, line OrderLine) (int64, error) {
	var loc int64
	if line.LocationID != nil {
		loc = *line.LocationID
	}
	loc, err := resolveLocation(ctx, tx, loc)
	if err != nil {
		return 0, err
	}
	if line.VariantID == nil {
		return loc, nil
	}
	return loc, ensureStockRow(ctx, tx, *line.VariantID, loc)
}

// addOrderLine snapshots a variant onto an order and takes its stock.
func addOrderLine(ctx context.Context, tx *sql.Tx, o *Order, variantID int64, qty int, committed bool) (string, error) {
	if qty <= 0 {
		return "", Validationf("a new line needs a quantity")
	}
	var (
		productID int64
		sku       string
		title     string
		price     int64
		sellable  bool
	)
	err := tx.QueryRowContext(ctx, `
		SELECT v.product_id, v.sku, p.title, v.price_minor, v.active AND p.status = 'active'
		FROM variants v JOIN products p ON p.id = v.product_id
		WHERE v.id = $1`, variantID).Scan(&productID, &sku, &title, &price, &sellable)
	if errors.Is(err, sql.ErrNoRows) {
		return "", NotFoundf("variant %d does not exist", variantID)
	}
	if err != nil {
		return "", err
	}
	if !sellable {
		return "", Conflictf("variant %s is not on sale", sku)
	}

	var label string
	if err := tx.QueryRowContext(ctx, `
		SELECT coalesce(string_agg(pov.value, ' / ' ORDER BY po.position, po.id), '')
		FROM variant_option_values vov
		JOIN product_option_values pov ON pov.id = vov.option_value_id
		JOIN product_options po ON po.id = pov.option_id
		WHERE vov.variant_id = $1`, variantID).Scan(&label); err != nil {
		return "", err
	}

	// A line added by hand takes its stock the same way checkout does: from
	// wherever can cover it, in priority order.
	locationID, err := pickLocation(ctx, tx, variantID, qty)
	if err != nil {
		return "", err
	}
	line := OrderLine{VariantID: &variantID, LocationID: &locationID}
	if err := moveOrderStock(ctx, tx, line, qty, committed); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO order_lines (order_id, product_id, variant_id, sku, title, variant_label,
		                         quantity, unit_price_minor, total_minor, location_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		o.ID, productID, variantID, sku, title, label, qty, price, price*int64(qty),
		locationID); err != nil {
		return "", err
	}
	return sku, nil
}

// OrderPatch corrects what was recorded about an order: who it is for, and how
// it was paid for. Pointers throughout, because "" is a real value for most of
// these — clearing a phone number nobody can reach is a correction.
//
// It deliberately cannot reach status, payment status, totals or lines. Each of
// those is a state change with consequences — stock, events, money — and each
// already has an operation that performs them properly. This is for the fields
// somebody typed.
type OrderPatch struct {
	Email   *string  `json:"email"`
	Phone   *string  `json:"phone"`
	Name    *string  `json:"name"`
	Address *Address `json:"address"`

	PaymentProvider  *string `json:"payment_provider"`
	PaymentReference *string `json:"payment_reference"`
}

// Update corrects an order's contact details and payment record.
//
// Both halves are the same kind of fix. An email with a typo in it means the
// customer never hears from the shop again; an address with the wrong house
// number means the parcel goes to the wrong door; a payment reference typed off
// a screen is the thing somebody reconciles against a bank statement. None of
// them is a state change, and until now the only way to correct any of them was
// a row in the database.
//
// The one that needs care is the provider, because Refund books through it. It
// may be changed while nothing has been refunded — an order taken as cash on
// delivery and actually settled by transfer should say so — but not after,
// where the money went out through the provider that is on the order now, and
// rewriting it would leave the refund pointing at a gateway that never saw it.
func (s *Orders) Update(ctx context.Context, id int64, patch OrderPatch) (*Order, error) {
	if patch.Email != nil {
		if strings.TrimSpace(*patch.Email) == "" {
			return nil, Validationf("an order needs an email address; it is how the customer hears about it")
		}
	}
	if patch.Address != nil {
		if err := patch.Address.Validate(); err != nil {
			return nil, err
		}
	}
	if patch.PaymentProvider != nil {
		if _, ok := s.app.payments.provider(*patch.PaymentProvider); !ok {
			return nil, Validationf("payment method %q is not installed in this build", *patch.PaymentProvider)
		}
	}

	return s.transition(ctx, id, func(ctx context.Context, tx *sql.Tx, o *Order) (string, any, error) {
		if o.Status == OrderCancelled {
			return "", nil, Conflictf("order %s has been cancelled", o.Number)
		}
		if patch.PaymentProvider != nil && *patch.PaymentProvider != o.PaymentProvider &&
			o.PaymentStatus == PaymentRefunded {
			return "", nil, Conflictf(
				"order %s was refunded through %s; the method it was settled by is part of that record",
				o.Number, o.PaymentProvider)
		}

		set, args := []string{}, []any{id}
		add := func(col string, v any) {
			args = append(args, v)
			set = append(set, fmt.Sprintf("%s = $%d", col, len(args)))
		}
		if patch.Email != nil {
			o.Email = strings.TrimSpace(*patch.Email)
			add("email", o.Email)
		}
		if patch.Phone != nil {
			o.Phone = strings.TrimSpace(*patch.Phone)
			add("phone", nullString(o.Phone))
		}
		if patch.Name != nil {
			o.Name = strings.TrimSpace(*patch.Name)
			add("name", nullString(o.Name))
		}
		if patch.Address != nil {
			encoded, err := json.Marshal(*patch.Address)
			if err != nil {
				return "", nil, Internalf(err, "encode the address")
			}
			o.Address = *patch.Address
			add("address", encoded)
		}
		if patch.PaymentProvider != nil {
			o.PaymentProvider = *patch.PaymentProvider
			add("payment_provider", o.PaymentProvider)
		}
		if patch.PaymentReference != nil {
			o.PaymentReference = strings.TrimSpace(*patch.PaymentReference)
			add("payment_reference", o.PaymentReference)
		}
		if len(set) == 0 {
			return "", nil, Validationf("nothing to change")
		}

		if _, err := tx.ExecContext(ctx,
			`UPDATE orders SET `+strings.Join(set, ", ")+`, updated_at = now() WHERE id = $1`,
			args...); err != nil {
			return "", nil, err
		}
		// order.edited, because that is what happened and a notifier keyed to
		// this order needs to know the address it holds has changed.
		return EventOrderEdited, s.eventPayload(o), nil
	})
}

// loadLineImages attaches a picture to every line that still has a product.
//
// One query for a whole page of orders, like the categories: a line at a time
// would be one round trip per item, and the orders list is the busiest screen
// in the panel.
//
// A variant's own picture wins over the product's first, because that is what
// was bought — a red shirt should not show the blue one. Lines whose product
// has been deleted simply come back without one; the order is a record of a
// sale and does not stop being true because the catalog moved on.
func (s *Orders) loadLineImages(ctx context.Context, orders []*Order) error {
	products := map[int64]bool{}
	for _, o := range orders {
		for _, l := range o.Lines {
			if l.ProductID != nil {
				products[*l.ProductID] = true
			}
		}
	}
	if len(products) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(products))
	for id := range products {
		ids = append(ids, id)
	}

	rows, err := s.app.db.QueryContext(ctx, `
		SELECT pm.product_id, pm.variant_id, m.url
		FROM product_media pm
		JOIN media m ON m.id = pm.media_id
		WHERE pm.product_id = ANY($1::bigint[]) AND m.kind = 'image'
		ORDER BY pm.product_id, pm.position, pm.media_id`, int64Array(ids))
	if err != nil {
		return err
	}
	defer rows.Close()

	byProduct := map[int64]string{}
	byVariant := map[int64]string{}
	for rows.Next() {
		var productID int64
		var variantID *int64
		var url string
		if err := rows.Scan(&productID, &variantID, &url); err != nil {
			return err
		}
		if variantID != nil {
			byVariant[*variantID] = url
		}
		// Position order, so the first row for a product is its lead image.
		if _, seen := byProduct[productID]; !seen {
			byProduct[productID] = url
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for _, o := range orders {
		for i := range o.Lines {
			l := &o.Lines[i]
			if l.VariantID != nil {
				if url, ok := byVariant[*l.VariantID]; ok {
					l.ImageURL = url
					continue
				}
			}
			if l.ProductID != nil {
				l.ImageURL = byProduct[*l.ProductID]
			}
		}
	}
	return nil
}

package gocommerce

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// CheckoutInput is what a client posts to POST /api/checkout/{code}.
type CheckoutInput struct {
	CartID      string            `json:"cart_id"`
	Email       string            `json:"email"`
	Phone       string            `json:"phone"`
	Name        string            `json:"name"`
	Address     Address           `json:"address"`
	PaymentData map[string]string `json:"payment_data"`
	ReturnURL   string            `json:"return_url"`
	Metadata    Metadata          `json:"metadata"`
}

// CheckoutResult is the order and what the client must do to pay for it.
type CheckoutResult struct {
	Order   *Order        `json:"order"`
	Payment PaymentIntent `json:"payment"`
}

// LineConflict explains, per line, why a checkout was refused. Returning the
// current values lets a storefront redisplay the cart and ask the shopper to
// confirm, which is the only honest thing to do when the price moved.
type LineConflict struct {
	VariantID         int64  `json:"variant_id"`
	SKU               string `json:"sku"`
	Reason            string `json:"reason"`
	CurrentPriceMinor int64  `json:"current_price_minor,omitempty"`
	Available         int    `json:"available,omitempty"`
	Requested         int    `json:"requested,omitempty"`
}

// Conflict reasons.
const (
	ReasonPriceChanged      = "price_changed"
	ReasonInactive          = "inactive"
	ReasonInsufficientStock = "insufficient_stock"
)

// conflictError carries line conflicts out of the transaction so the cart can
// be refreshed after the rollback.
type conflictError struct{ conflicts []LineConflict }

func (e *conflictError) Error() string {
	return fmt.Sprintf("%d line(s) are no longer valid", len(e.conflicts))
}

// Checkout turns a cart into an order and starts payment.
//
// It is split in two deliberately. Phase A validates, reserves inventory and
// creates the order in one transaction. Phase B calls the payment provider
// after that transaction has committed — an external network call must never
// run while core write locks are held, because a gateway having a slow day
// would otherwise become the store having a slow day.
func (s *Orders) Checkout(ctx context.Context, code string, in CheckoutInput, idempotencyKey string) (*CheckoutResult, error) {
	provider, ok := s.app.payments.provider(code)
	if !ok {
		return nil, NotFoundf("no payment method named %q", code)
	}
	if err := validateCheckoutInput(&in); err != nil {
		return nil, err
	}

	scope := "checkout:" + code
	hash := requestHash(code, in)

	// A retry with the same key must never create a second order. If the
	// previous attempt got as far as creating one, we resume it rather than
	// starting again.
	if idempotencyKey != "" {
		result, resume, err := s.replayCheckout(ctx, scope, idempotencyKey, hash)
		if err != nil {
			return nil, err
		}
		if result != nil {
			return result, nil
		}
		if resume != nil {
			return s.initiatePayment(ctx, provider, resume, in, scope, idempotencyKey)
		}
	}

	order, err := s.createOrderFromCart(ctx, code, in, scope, idempotencyKey, hash)
	if err != nil {
		return nil, err
	}
	s.app.nudgeOutbox()

	return s.initiatePayment(ctx, provider, order, in, scope, idempotencyKey)
}

// replayCheckout looks up a previous attempt under the same key.
func (s *Orders) replayCheckout(ctx context.Context, scope, key, hash string) (*CheckoutResult, *Order, error) {
	var storedHash string
	var orderID sql.NullInt64
	var response []byte
	err := s.app.db.QueryRowContext(ctx,
		`SELECT request_hash, order_id, response FROM idempotency_keys WHERE scope = $1 AND key = $2`,
		scope, key).Scan(&storedHash, &orderID, &response)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if storedHash != hash {
		// The same key with a different body is a client bug, and answering it
		// with the first request's result would be worse than refusing.
		return nil, nil, Validationf("this Idempotency-Key was already used for a different request")
	}
	if len(response) > 0 {
		var result CheckoutResult
		if err := json.Unmarshal(response, &result); err != nil {
			return nil, nil, err
		}
		if orderID.Valid {
			// Re-read so the replay reflects the order's current state rather
			// than a stale snapshot from the first attempt.
			if o, err := s.Get(ctx, orderID.Int64); err == nil {
				result.Order = o
			}
		}
		return &result, nil, nil
	}
	if orderID.Valid {
		o, err := s.Get(ctx, orderID.Int64)
		if err != nil {
			return nil, nil, err
		}
		return nil, o, nil
	}
	return nil, nil, nil
}

// createOrderFromCart is phase A: one transaction, no network calls.
func (s *Orders) createOrderFromCart(ctx context.Context, code string, in CheckoutInput, scope, key, hash string) (*Order, error) {
	var orderID int64

	err := InTx(ctx, s.app.db, func(tx *sql.Tx) error {
		cartID, currency, cartCode, err := lockCartForCheckout(ctx, tx, in.CartID)
		if err != nil {
			return err
		}

		lines, err := loadCheckoutLines(ctx, tx, cartID)
		if err != nil {
			return err
		}
		if len(lines) == 0 {
			return Validationf("the cart is empty")
		}

		// Reserve in a stable order. Two concurrent checkouts sharing variants
		// would otherwise be able to take each other's row locks in opposite
		// orders and deadlock.
		sort.Slice(lines, func(i, j int) bool { return lines[i].VariantID < lines[j].VariantID })

		var conflicts []LineConflict
		var subtotal int64
		for i := range lines {
			l := &lines[i]
			switch {
			case !l.Active:
				conflicts = append(conflicts, LineConflict{
					VariantID: l.VariantID, SKU: l.SKU, Reason: ReasonInactive})
				continue
			case l.CurrentPrice != l.SnapshotPrice:
				conflicts = append(conflicts, LineConflict{
					VariantID: l.VariantID, SKU: l.SKU, Reason: ReasonPriceChanged,
					CurrentPriceMinor: l.CurrentPrice})
				continue
			}
			// Where the units come from is decided once, here, and written
			// onto the line: a cancellation months later has to put them back
			// on the shelf they left, not on whichever shelf is default that
			// week.
			loc, err := pickLocation(ctx, tx, l.VariantID, l.Quantity)
			if err == nil {
				err = reserveStock(ctx, tx, l.VariantID, loc, l.Quantity)
			}
			if err != nil {
				if errors.Is(err, errInsufficientStock) {
					conflicts = append(conflicts, LineConflict{
						VariantID: l.VariantID, SKU: l.SKU, Reason: ReasonInsufficientStock,
						Available: l.Available, Requested: l.Quantity})
					continue
				}
				return err
			}
			l.LocationID = loc
			subtotal += l.CurrentPrice * int64(l.Quantity)
		}
		if len(conflicts) > 0 {
			return &conflictError{conflicts: conflicts}
		}

		shipping := s.app.cfg.FlatShippingMinor

		// The discount is decided here and nowhere else, under the lock that
		// just re-checked every price and every reservation. A code that
		// expired while the shopper was typing their address is refused by the
		// same mechanism that refuses a sold-out line.
		applied, err := s.app.discounts.applyTx(ctx, tx, discountRequest{
			Code: cartCode, Email: in.Email, Subtotal: subtotal,
		})
		if err != nil {
			return err
		}
		var discount int64
		if applied != nil {
			discount = applied.AmountMinor
			if applied.FreeShipping {
				shipping = 0
			}
		}
		// Tax comes after the discount, because tax is charged on what the
		// customer actually pays. The rates are looked up against the address
		// this order is going to, and what each line is charged is stored on
		// the line — see taxes.go.
		taxable := make([]taxableLine, len(lines))
		for i, l := range lines {
			taxable[i] = taxableLine{
				ProductID: l.ProductID,
				Total:     l.CurrentPrice * int64(l.Quantity),
				Taxable:   l.Taxable,
			}
		}
		inclusive := s.app.cfg.PricesIncludeTax
		lineTaxes, tax, err := s.app.taxes.computeTax(ctx, tx,
			in.Address.Country, in.Address.State, taxable, discount, inclusive)
		if err != nil {
			return err
		}

		// Inclusive prices already contain the tax, so it is reported rather
		// than added. Exclusive prices do not, so it is added. Getting this
		// backwards is the one bug in tax that a customer notices immediately.
		total := subtotal + shipping - discount
		if !inclusive {
			total += tax
		}

		if err := tx.QueryRowContext(ctx,
			`SELECT nextval(pg_get_serial_sequence('orders', 'id'))`).Scan(&orderID); err != nil {
			return err
		}
		number := fmt.Sprintf("%s%06d", s.app.cfg.OrderPrefix, orderID)
		accessToken, err := token()
		if err != nil {
			return err
		}
		addr, err := json.Marshal(in.Address)
		if err != nil {
			return err
		}
		meta, err := in.Metadata.value()
		if err != nil {
			return Validationf("metadata is not valid JSON: %v", err)
		}

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO orders (id, number, access_token, status, payment_status,
			                    payment_provider, currency, subtotal_minor, shipping_minor,
			                    discount_minor, tax_minor, tax_inclusive, total_minor,
			                    email, phone, name, address,
			                    lang, metadata, reservation_expires_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,
			        now() + make_interval(secs => $20))`,
			orderID, number, accessToken, OrderPending, PaymentPending, code, currency,
			subtotal, shipping, discount, tax, inclusive, total,
			strings.ToLower(in.Email), nullString(in.Phone),
			nullString(in.Name), addr, s.app.RequestLanguageValue(ctx), meta,
			s.app.cfg.OrderTTL.Seconds(),
		); err != nil {
			return err
		}

		// The snapshot, in the same transaction as the order it belongs to.
		if err := recordOrderDiscount(ctx, tx, orderID, applied); err != nil {
			return err
		}

		for i, l := range lines {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO order_lines (order_id, product_id, variant_id, sku, title,
				                         variant_label, quantity, unit_price_minor, total_minor,
				                         tax_minor, tax_rate_bp, tax_name, location_id)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
				orderID, l.ProductID, l.VariantID, l.SKU, l.Title, l.Label,
				l.Quantity, l.CurrentPrice, l.CurrentPrice*int64(l.Quantity),
				lineTaxes[i].AmountMinor, lineTaxes[i].RateBP, lineTaxes[i].Name,
				l.LocationID); err != nil {
				return err
			}
		}

		if _, err := tx.ExecContext(ctx,
			`UPDATE carts SET status = $2, updated_at = now() WHERE id = $1`,
			cartID, CartConverted); err != nil {
			return err
		}

		if key != "" {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO idempotency_keys (scope, key, request_hash, order_id)
				 VALUES ($1, $2, $3, $4)`, scope, key, hash, orderID); err != nil {
				if isUniqueViolation(err) {
					// Another request with this key won the race and is
					// creating the same order; ours must not also exist.
					return Conflictf("a checkout with this Idempotency-Key is already in progress")
				}
				return err
			}
		}

		o := &Order{ID: orderID, Number: number, Status: OrderPending,
			PaymentStatus: PaymentPending, PaymentProvider: code, Currency: currency,
			Total: money(total, currency), Email: in.Email, Phone: in.Phone,
			Name: in.Name, Language: s.app.RequestLanguageValue(ctx)}
		for _, l := range lines {
			o.Lines = append(o.Lines, OrderLine{
				SKU: l.SKU, Title: l.Title, VariantLabel: l.Label, Quantity: l.Quantity,
				UnitPrice: money(l.CurrentPrice, currency),
				Total:     money(l.CurrentPrice*int64(l.Quantity), currency),
			})
		}
		return s.app.outbox.write(ctx, tx, EventOrderCreated, AggregateOrder, orderID, s.eventPayload(o))
	})

	if err != nil {
		var ce *conflictError
		if errors.As(err, &ce) {
			// The transaction rolled back, so refresh the cart's snapshot
			// prices now: the shopper should see what things actually cost
			// when they look again.
			s.refreshCartPrices(ctx, in.CartID)
			return nil, Conflictf("the cart is no longer valid at these prices").
				WithDetails(ce.conflicts)
		}
		return nil, err
	}

	o, err := s.Get(ctx, orderID)
	if err != nil {
		return nil, err
	}
	// Returned once, to the shopper who just placed the order.
	if err := s.app.db.QueryRowContext(ctx,
		`SELECT access_token FROM orders WHERE id = $1`, orderID).Scan(&o.AccessToken); err != nil {
		return nil, err
	}
	return o, nil
}

// initiatePayment is phase B, outside any transaction.
func (s *Orders) initiatePayment(ctx context.Context, provider PaymentProvider, o *Order, in CheckoutInput, scope, key string) (*CheckoutResult, error) {
	intent, err := provider.Initiate(ctx, o, PayOptions{ReturnURL: in.ReturnURL, Data: in.PaymentData})
	if err != nil {
		// The order stands, unpaid. A retry with the same key resumes here
		// rather than reserving stock all over again.
		s.app.log.Error("payment initiation failed",
			"order", o.Number, "provider", provider.Code(), "error", err)
		return nil, Internalf(err, "could not start payment with %s", provider.Code())
	}
	intent.Provider = provider.Code()

	if intent.Reference != "" {
		if _, err := s.app.db.ExecContext(ctx,
			`UPDATE orders SET payment_reference = $2, updated_at = now() WHERE id = $1`,
			o.ID, intent.Reference); err != nil {
			return nil, err
		}
	}

	// "none" means the payment method needs nothing further — cash on
	// delivery. The sale is happening, so the order is confirmed and its
	// stock leaves the shelf; it simply is not paid for yet.
	if intent.Kind == IntentNone {
		confirmed, err := s.Confirm(ctx, o.ID)
		if err != nil {
			return nil, err
		}
		confirmed.AccessToken = o.AccessToken
		o = confirmed
	} else {
		refreshed, err := s.Get(ctx, o.ID)
		if err == nil {
			refreshed.AccessToken = o.AccessToken
			o = refreshed
		}
	}

	result := &CheckoutResult{Order: o, Payment: intent}
	if key != "" {
		if body, err := json.Marshal(result); err == nil {
			if _, err := s.app.db.ExecContext(ctx,
				`UPDATE idempotency_keys SET response = $3 WHERE scope = $1 AND key = $2`,
				scope, key, body); err != nil {
				s.app.log.Warn("could not store idempotent response", "error", err)
			}
		}
	}
	s.app.nudgeOutbox()
	return result, nil
}

// refreshCartPrices re-snapshots a cart to current prices after a conflict.
func (s *Orders) refreshCartPrices(ctx context.Context, cartToken string) {
	if _, err := s.app.db.ExecContext(ctx, `
		UPDATE cart_line_items l
		SET unit_price_minor = v.price_minor, updated_at = now()
		FROM variants v, carts c
		WHERE v.id = l.variant_id AND c.id = l.cart_id AND c.token = $1
		  AND l.unit_price_minor <> v.price_minor`, cartToken); err != nil {
		s.app.log.Warn("could not refresh cart prices", "error", err)
	}
}

// ------------------------------------------------------------------ helpers

type checkoutLine struct {
	VariantID     int64
	ProductID     int64
	SKU           string
	Title         string
	Label         string
	Quantity      int
	SnapshotPrice int64
	CurrentPrice  int64
	Available     int
	Active        bool
	Taxable       bool
	// LocationID is filled in when the line's stock is reserved, not when it is
	// read: it is the answer to "where did these come from", and there is no
	// answer until something has actually been taken.
	LocationID int64
}

func lockCartForCheckout(ctx context.Context, tx *sql.Tx, tok string) (int64, string, string, error) {
	var id int64
	var status, currency, discountCode string
	err := tx.QueryRowContext(ctx,
		`SELECT id, status, currency, discount_code FROM carts WHERE token = $1 FOR UPDATE`, tok).
		Scan(&id, &status, &currency, &discountCode)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", "", NotFoundf("cart not found")
	}
	if err != nil {
		return 0, "", "", err
	}
	if status != CartOpen {
		return 0, "", "", Conflictf("this cart has already been checked out")
	}
	return id, currency, discountCode, nil
}

func loadCheckoutLines(ctx context.Context, tx *sql.Tx, cartID int64) ([]checkoutLine, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT l.variant_id, v.product_id, v.sku, p.title, l.quantity,
		       l.unit_price_minor, v.price_minor, v.active, v.taxable,
		       CASE WHEN v.track_inventory AND NOT v.continue_selling
		            THEN coalesce((SELECT sum(vs.on_hand - vs.reserved) FROM variant_stock vs WHERE vs.variant_id = v.id), 0) ELSE -1 END,
		       coalesce((
		           SELECT string_agg(pov.value, ' / ' ORDER BY o.position, o.id)
		           FROM variant_option_values vov
		           JOIN product_option_values pov ON pov.id = vov.option_value_id
		           JOIN product_options o ON o.id = pov.option_id
		           WHERE vov.variant_id = v.id
		       ), '')
		FROM cart_line_items l
		JOIN variants v ON v.id = l.variant_id
		JOIN products p ON p.id = v.product_id
		WHERE l.cart_id = $1
		ORDER BY l.id`, cartID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var lines []checkoutLine
	for rows.Next() {
		var l checkoutLine
		if err := rows.Scan(&l.VariantID, &l.ProductID, &l.SKU, &l.Title, &l.Quantity,
			&l.SnapshotPrice, &l.CurrentPrice, &l.Active, &l.Taxable, &l.Available,
			&l.Label); err != nil {
			return nil, err
		}
		lines = append(lines, l)
	}
	return lines, rows.Err()
}

func validateCheckoutInput(in *CheckoutInput) error {
	in.Email = strings.TrimSpace(in.Email)
	if in.CartID == "" {
		return Validationf("cart_id is required")
	}
	if in.Email == "" || !strings.Contains(in.Email, "@") {
		return Validationf("a valid email is required")
	}
	return in.Address.Validate()
}

// requestHash fingerprints a checkout request so a repeated Idempotency-Key
// carrying a different body can be told apart from a genuine retry.
func requestHash(code string, in CheckoutInput) string {
	body, _ := json.Marshal(struct {
		Code string `json:"code"`
		In   CheckoutInput
	}{code, in})
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "23505")
}

// isForeignKeyViolation reports a 23503, which reaches the service layer as
// "you referenced something that is not there" — a validation error the caller
// can fix, not an internal one.
func isForeignKeyViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "23503")
}

// RequestLanguageValue is [App.RequestLanguage] for a bare context, used where
// the request is no longer in scope.
func (a *App) RequestLanguageValue(ctx context.Context) string {
	if lang := Language(ctx); lang != "" {
		return lang
	}
	return a.cfg.DefaultLanguage
}

// ---------------------------------------------------------- placing by hand

// NewOrderInput is an order an operator places on the customer's behalf — the
// phone order, the trade counter, the replacement for a parcel that never
// arrived.
type NewOrderInput struct {
	// PaymentMethod is the code of a registered method, "cod" by default. The
	// operator is standing in for the shopper, so they choose the same way the
	// shopper would.
	PaymentMethod string         `json:"payment_method"`
	Email         string         `json:"email"`
	Phone         string         `json:"phone"`
	Name          string         `json:"name"`
	Address       Address        `json:"address"`
	Lines         []NewOrderLine `json:"lines"`
	Metadata      Metadata       `json:"metadata"`
}

// NewOrderLine is one variant and how many of it.
type NewOrderLine struct {
	VariantID int64 `json:"variant_id"`
	Quantity  int   `json:"quantity"`
}

// Create places an order the way a shopper would, on their behalf.
//
// It builds a cart and checks it out rather than inserting an order, and that
// is the whole design. An order placed here reserves stock under the same lock,
// snapshots prices the same way, gets the same number, the same access token —
// so the customer can look it up without an account, which is D22's whole point
// — and emits the same `order.created` that every notifier and module already
// listens for. A second insert path would be a second definition of what an
// order is, and the two would drift the first time either changed.
//
// The import path in transfer.go looks similar and is not: it records orders
// that already happened somewhere else, so it must not move stock. This one is
// a sale being made now.
//
// The cart is real and is consumed by the checkout, exactly as a shopper's is.
// It is never handed out, so it needs no token of its own beyond the one the
// cart service mints.
func (s *Orders) Create(ctx context.Context, in NewOrderInput) (*CheckoutResult, error) {
	if len(in.Lines) == 0 {
		return nil, Validationf("an order needs at least one line")
	}
	code := strings.TrimSpace(in.PaymentMethod)
	if code == "" {
		code = CodeCOD
	}
	if _, ok := s.app.payments.provider(code); !ok {
		return nil, NotFoundf("no payment method named %q", code)
	}

	cart, err := s.app.carts.Create(ctx, in.Email)
	if err != nil {
		return nil, err
	}
	for _, l := range in.Lines {
		if l.Quantity <= 0 {
			return nil, Validationf("every line needs a quantity")
		}
		// AddLine is the shopper's own path, so an inactive variant or one the
		// shelf cannot cover is refused here in the same words the storefront
		// would use — before any order exists to be half-made.
		if _, err := s.app.carts.AddLine(ctx, cart.Token, l.VariantID, l.Quantity); err != nil {
			return nil, err
		}
	}

	// No idempotency key: the operator is a person clicking a button, and the
	// panel does not retry. A caller that wants replay protection can check out
	// the cart itself with a key.
	return s.Checkout(ctx, code, CheckoutInput{
		CartID:   cart.Token,
		Email:    in.Email,
		Phone:    in.Phone,
		Name:     in.Name,
		Address:  in.Address,
		Metadata: in.Metadata,
	}, "")
}

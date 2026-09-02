package gocommerce

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Cart statuses.
const (
	CartOpen      = "open"
	CartConverted = "converted"
	CartAbandoned = "abandoned"
)

// Cart is a guest's basket. Its token is the only credential involved —
// there is no account, and there never has to be, because guest checkout is a
// permanent guarantee rather than a stage this project grows out of.
type Cart struct {
	ID        int64      `json:"-"`
	Token     string     `json:"id"`
	Status    string     `json:"status"`
	Currency  string     `json:"currency"`
	Email     string     `json:"email,omitempty"`
	Lines     []CartLine `json:"line_items"`
	ItemCount int        `json:"item_count"`
	Subtotal  Money      `json:"subtotal"`
	Metadata  Metadata   `json:"metadata"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	ExpiresAt time.Time  `json:"expires_at"`
}

// CartLine is one variant in a cart, with the price as it was when added.
type CartLine struct {
	ID           int64  `json:"id"`
	VariantID    int64  `json:"variant_id"`
	ProductID    int64  `json:"product_id"`
	SKU          string `json:"sku"`
	Title        string `json:"title"`
	VariantLabel string `json:"variant_label,omitempty"`
	Quantity     int    `json:"quantity"`
	UnitPrice    Money  `json:"unit_price"`
	Total        Money  `json:"total"`

	// The live view of the line, so a storefront can warn before checkout
	// rather than surprising the shopper at the end.
	CurrentPrice Money `json:"current_price"`
	Available    int   `json:"available"`
	InStock      bool  `json:"in_stock"`
	PriceChanged bool  `json:"price_changed"`
}

// Carts owns baskets.
type Carts struct {
	app *App
}

// Cart returns the cart service.
func (a *App) Cart() *Carts { return a.carts }

// Create opens an empty cart and mints its token.
func (c *Carts) Create(ctx context.Context, email string) (*Cart, error) {
	tok, err := token()
	if err != nil {
		return nil, err
	}
	var id int64
	err = c.app.db.QueryRowContext(ctx, `
		INSERT INTO carts (token, currency, email, expires_at)
		VALUES ($1, $2, $3, now() + make_interval(secs => $4))
		RETURNING id`,
		tok, c.app.cfg.Currency, nullString(email), c.app.cfg.CartTTL.Seconds()).Scan(&id)
	if err != nil {
		return nil, err
	}
	return c.GetByToken(ctx, tok)
}

// GetByToken loads a cart. The token is unguessable, so possessing it is the
// authorisation.
func (c *Carts) GetByToken(ctx context.Context, tok string) (*Cart, error) {
	if tok == "" {
		return nil, Validationf("a cart id is required")
	}
	cart := &Cart{}
	var meta []byte
	var email sql.NullString
	err := c.app.db.QueryRowContext(ctx, `
		SELECT id, token, status, currency, email, metadata, created_at, updated_at, expires_at
		FROM carts WHERE token = $1`, tok,
	).Scan(&cart.ID, &cart.Token, &cart.Status, &cart.Currency, &email,
		&meta, &cart.CreatedAt, &cart.UpdatedAt, &cart.ExpiresAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, NotFoundf("cart not found")
		}
		return nil, err
	}
	cart.Email = email.String
	if err := scanMetadata(meta, &cart.Metadata); err != nil {
		return nil, err
	}
	if err := c.loadLines(ctx, cart); err != nil {
		return nil, err
	}
	return cart, nil
}

// loadLines fills in the cart's lines with both the snapshot price and the
// live one, in a single join.
func (c *Carts) loadLines(ctx context.Context, cart *Cart) error {
	rows, err := c.app.db.QueryContext(ctx, `
		SELECT l.id, l.variant_id, v.product_id, v.sku, p.title, l.quantity,
		       l.unit_price_minor, v.price_minor, v.track_inventory,
		       coalesce((SELECT sum(vs.on_hand - vs.reserved) FROM variant_stock vs WHERE vs.variant_id = v.id), 0), v.active
		FROM cart_line_items l
		JOIN variants v ON v.id = l.variant_id
		JOIN products p ON p.id = v.product_id
		WHERE l.cart_id = $1
		ORDER BY l.id`, cart.ID)
	if err != nil {
		return err
	}
	defer rows.Close()

	cart.Lines = []CartLine{}
	var subtotal int64
	var variantIDs []int64
	for rows.Next() {
		var l CartLine
		var currentPrice int64
		var tracks, active bool
		var available int
		if err := rows.Scan(&l.ID, &l.VariantID, &l.ProductID, &l.SKU, &l.Title,
			&l.Quantity, &l.UnitPrice.AmountMinor, &currentPrice, &tracks,
			&available, &active); err != nil {
			return err
		}
		cur := c.app.cfg.Currency
		l.UnitPrice.Currency = cur
		l.Total = money(l.UnitPrice.AmountMinor*int64(l.Quantity), cur)
		l.CurrentPrice = money(currentPrice, cur)
		l.PriceChanged = currentPrice != l.UnitPrice.AmountMinor
		l.Available = available
		if !tracks {
			l.Available = -1 // not tracked
		}
		l.InStock = active && (!tracks || available >= l.Quantity)
		subtotal += l.Total.AmountMinor
		cart.ItemCount += l.Quantity
		cart.Lines = append(cart.Lines, l)
		variantIDs = append(variantIDs, l.VariantID)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	cart.Subtotal = money(subtotal, c.app.cfg.Currency)

	// Attach each line's variant label ("M / Black") in one extra query.
	if len(variantIDs) > 0 {
		labels, err := c.variantLabels(ctx, variantIDs)
		if err != nil {
			return err
		}
		for i := range cart.Lines {
			cart.Lines[i].VariantLabel = labels[cart.Lines[i].VariantID]
		}
	}
	return nil
}

func (c *Carts) variantLabels(ctx context.Context, ids []int64) (map[int64]string, error) {
	rows, err := c.app.db.QueryContext(ctx, `
		SELECT vov.variant_id, string_agg(pov.value, ' / ' ORDER BY o.position, o.id)
		FROM variant_option_values vov
		JOIN product_option_values pov ON pov.id = vov.option_value_id
		JOIN product_options o ON o.id = pov.option_id
		WHERE vov.variant_id = ANY($1::bigint[])
		GROUP BY vov.variant_id`, int64Array(ids))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	labels := map[int64]string{}
	for rows.Next() {
		var id int64
		var label string
		if err := rows.Scan(&id, &label); err != nil {
			return nil, err
		}
		labels[id] = label
	}
	return labels, rows.Err()
}

// AddLine adds a variant, or increases its quantity if it is already there.
// Adding records the price now; checkout is what makes a price authoritative.
func (c *Carts) AddLine(ctx context.Context, tok string, variantID int64, qty int) (*Cart, error) {
	if qty <= 0 {
		return nil, Validationf("quantity must be at least 1")
	}
	err := InTx(ctx, c.app.db, func(tx *sql.Tx) error {
		cartID, err := c.openCartID(ctx, tx, tok)
		if err != nil {
			return err
		}

		var price int64
		var active, tracks, oversell bool
		var available int
		err = tx.QueryRowContext(ctx, `
			SELECT price_minor, active, track_inventory, continue_selling,
			       coalesce((SELECT sum(on_hand - reserved) FROM variant_stock WHERE variant_id = $1), 0)
			FROM variants WHERE id = $1`, variantID,
		).Scan(&price, &active, &tracks, &oversell, &available)
		if errors.Is(err, sql.ErrNoRows) {
			return NotFoundf("variant %d does not exist", variantID)
		}
		if err != nil {
			return err
		}
		if !active {
			return Conflictf("that variant is not available")
		}

		var existing int
		err = tx.QueryRowContext(ctx,
			`SELECT coalesce((SELECT quantity FROM cart_line_items WHERE cart_id = $1 AND variant_id = $2), 0)`,
			cartID, variantID).Scan(&existing)
		if err != nil {
			return err
		}
		if tracks && !oversell && available < existing+qty {
			return Conflictf("only %d left in stock", available)
		}

		_, err = tx.ExecContext(ctx, `
			INSERT INTO cart_line_items (cart_id, variant_id, quantity, unit_price_minor)
			VALUES ($1, $2, $3, $4)
			-- The price is set on insert and never touched again. Re-adding a
			-- variant to bump its quantity must not silently move the units
			-- already in the cart to today's price: the snapshot is what lets
			-- checkout notice a price change and make the shopper re-confirm,
			-- and overwriting it here destroys that evidence.
			ON CONFLICT (cart_id, variant_id) DO UPDATE
			SET quantity = cart_line_items.quantity + EXCLUDED.quantity,
			    updated_at = now()`,
			cartID, variantID, qty, price)
		if err != nil {
			return err
		}
		return touchCart(ctx, tx, cartID, c.app.cfg.CartTTL)
	})
	if err != nil {
		return nil, err
	}
	return c.GetByToken(ctx, tok)
}

// UpdateLine sets a line's quantity. Zero removes it, which is what a quantity
// stepper stepping down to nothing means.
func (c *Carts) UpdateLine(ctx context.Context, tok string, lineID int64, qty int) (*Cart, error) {
	if qty < 0 {
		return nil, Validationf("quantity must not be negative")
	}
	if qty == 0 {
		return c.RemoveLine(ctx, tok, lineID)
	}
	err := InTx(ctx, c.app.db, func(tx *sql.Tx) error {
		cartID, err := c.openCartID(ctx, tx, tok)
		if err != nil {
			return err
		}
		var variantID int64
		err = tx.QueryRowContext(ctx,
			`SELECT variant_id FROM cart_line_items WHERE id = $1 AND cart_id = $2`,
			lineID, cartID).Scan(&variantID)
		if errors.Is(err, sql.ErrNoRows) {
			return NotFoundf("line item %d is not in this cart", lineID)
		}
		if err != nil {
			return err
		}

		var tracks, oversell bool
		var available int
		if err := tx.QueryRowContext(ctx,
			`SELECT track_inventory, continue_selling, coalesce((SELECT sum(on_hand - reserved) FROM variant_stock WHERE variant_id = $1), 0)
			 FROM variants WHERE id = $1`,
			variantID).Scan(&tracks, &oversell, &available); err != nil {
			return err
		}
		if tracks && !oversell && available < qty {
			return Conflictf("only %d left in stock", available)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE cart_line_items SET quantity = $2, updated_at = now() WHERE id = $1`,
			lineID, qty); err != nil {
			return err
		}
		return touchCart(ctx, tx, cartID, c.app.cfg.CartTTL)
	})
	if err != nil {
		return nil, err
	}
	return c.GetByToken(ctx, tok)
}

// RemoveLine drops a line from the cart.
func (c *Carts) RemoveLine(ctx context.Context, tok string, lineID int64) (*Cart, error) {
	err := InTx(ctx, c.app.db, func(tx *sql.Tx) error {
		cartID, err := c.openCartID(ctx, tx, tok)
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx,
			`DELETE FROM cart_line_items WHERE id = $1 AND cart_id = $2`, lineID, cartID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return NotFoundf("line item %d is not in this cart", lineID)
		}
		return touchCart(ctx, tx, cartID, c.app.cfg.CartTTL)
	})
	if err != nil {
		return nil, err
	}
	return c.GetByToken(ctx, tok)
}

// SetEmail records the shopper's email on the cart, so an abandoned-cart
// consumer has something to work with.
func (c *Carts) SetEmail(ctx context.Context, tok, email string) (*Cart, error) {
	err := InTx(ctx, c.app.db, func(tx *sql.Tx) error {
		cartID, err := c.openCartID(ctx, tx, tok)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE carts SET email = $2, updated_at = now() WHERE id = $1`, cartID, nullString(email))
		return err
	})
	if err != nil {
		return nil, err
	}
	return c.GetByToken(ctx, tok)
}

// openCartID resolves a token to a cart that can still be modified.
func (c *Carts) openCartID(ctx context.Context, tx *sql.Tx, tok string) (int64, error) {
	var id int64
	var status string
	err := tx.QueryRowContext(ctx,
		`SELECT id, status FROM carts WHERE token = $1 FOR UPDATE`, tok).Scan(&id, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, NotFoundf("cart not found")
	}
	if err != nil {
		return 0, err
	}
	if status != CartOpen {
		return 0, Conflictf("this cart has already been checked out")
	}
	return id, nil
}

func touchCart(ctx context.Context, tx *sql.Tx, cartID int64, ttl time.Duration) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE carts
		SET updated_at = now(), expires_at = now() + make_interval(secs => $2)
		WHERE id = $1`, cartID, ttl.Seconds())
	return err
}

// SweepExpired deletes carts nobody has touched within the TTL. A public
// endpoint that creates rows needs something that removes them, or the table
// grows without limit for as long as the store is popular.
func (c *Carts) SweepExpired(ctx context.Context) (int64, error) {
	res, err := c.app.db.ExecContext(ctx,
		`DELETE FROM carts WHERE status = 'open' AND expires_at < now()`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

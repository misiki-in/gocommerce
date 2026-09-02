package gocommerce

import (
	"context"
	"database/sql"
	"errors"
)

// Inventory owns stock movements. Every change goes through here rather than
// through arbitrary SQL, because "set the stock to 7" is not a safe operation
// when another request may have sold one in between — the operations below are
// deltas evaluated under a row lock, not overwrites.
//
// A variant's sellable quantity is on_hand - reserved, summed over its
// locations. Checkout reserves; confirming a sale converts the reservation
// into a decrement of on_hand; cancelling releases or restocks depending on
// how far the order got.
//
// Since M17 those two numbers live per (variant, location) and the variant's
// totals are sums taken on the way out. Every movement therefore names a
// place; 0 means the default one, which is what a single-location store
// always passes and never has to think about.
type Inventory struct {
	app *App
}

// Stock returns the inventory service.
func (a *App) Stock() *Inventory { return a.inventory }

// The four stock movements, each against one location.
//
// Every one of them is a single statement, so the check and the change happen
// under the same row lock. That is what makes two concurrent checkouts for the
// last unit resolve rather than race: the second re-evaluates its condition
// after the first commits.
//
// `track_inventory` is on the variant and the movement is on the stock row, so
// each of these joins back to ask whether counting applies at all. A variant
// that does not track inventory succeeds and moves nothing.

// pickLocation chooses where a reservation comes from: the first active
// location, in priority order, that can cover the quantity — falling back to the
// default when nothing can, so that a variant which sells past zero still has a
// place to be short in.
//
// One query, because "which location" and "is there enough there" are the same
// question and answering them separately invites the stock to move in between.
func pickLocation(ctx context.Context, tx *sql.Tx, variantID int64, qty int) (int64, error) {
	var id int64
	err := tx.QueryRowContext(ctx, `
		SELECT coalesce(
		    (SELECT vs.location_id
		       FROM variant_stock vs
		       JOIN locations l ON l.id = vs.location_id
		       JOIN variants v ON v.id = vs.variant_id
		      WHERE vs.variant_id = $1 AND l.active
		        AND (NOT v.track_inventory OR v.continue_selling
		             OR vs.on_hand - vs.reserved >= $2)
		      ORDER BY l.priority, l.id
		      LIMIT 1),
		    (SELECT id FROM locations WHERE is_default))`,
		variantID, qty).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) || id == 0 {
		return 0, errInsufficientStock
	}
	return id, err
}

// reserveStock holds qty units at one location.
func reserveStock(ctx context.Context, tx *sql.Tx, variantID, locationID int64, qty int) error {
	res, err := tx.ExecContext(ctx, `
		UPDATE variant_stock vs
		SET reserved = vs.reserved + CASE WHEN v.track_inventory THEN $3 ELSE 0 END,
		    updated_at = now()
		FROM variants v
		WHERE v.id = vs.variant_id
		  AND vs.variant_id = $1 AND vs.location_id = $2
		  AND (NOT v.track_inventory OR v.continue_selling
		       OR vs.on_hand - vs.reserved >= $3)`,
		variantID, locationID, qty)
	if err != nil {
		return translateCatalogErr(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		// Either there is no stock row for this pair or there is not enough
		// left. The caller distinguishes them; from here both mean "cannot
		// sell this".
		return errInsufficientStock
	}
	return nil
}

// commitStock turns a reservation into a sale: the units leave both the
// reservation and the shelf they were held on.
func commitStock(ctx context.Context, tx *sql.Tx, variantID, locationID int64, qty int) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE variant_stock vs
		SET reserved = vs.reserved - CASE WHEN v.track_inventory THEN $3 ELSE 0 END,
		    on_hand  = vs.on_hand  - CASE WHEN v.track_inventory THEN $3 ELSE 0 END,
		    updated_at = now()
		FROM variants v
		WHERE v.id = vs.variant_id AND vs.variant_id = $1 AND vs.location_id = $2`,
		variantID, locationID, qty)
	return translateCatalogErr(err)
}

// releaseStock drops a reservation without selling: the units go back on sale
// where they were held.
func releaseStock(ctx context.Context, tx *sql.Tx, variantID, locationID int64, qty int) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE variant_stock vs
		SET reserved = greatest(0, vs.reserved - CASE WHEN v.track_inventory THEN $3 ELSE 0 END),
		    updated_at = now()
		FROM variants v
		WHERE v.id = vs.variant_id AND vs.variant_id = $1 AND vs.location_id = $2`,
		variantID, locationID, qty)
	return translateCatalogErr(err)
}

// restockStock returns already-sold units to the shelf they came off, for a
// cancellation after the sale was committed.
func restockStock(ctx context.Context, tx *sql.Tx, variantID, locationID int64, qty int) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE variant_stock vs
		SET on_hand = vs.on_hand + CASE WHEN v.track_inventory THEN $3 ELSE 0 END,
		    updated_at = now()
		FROM variants v
		WHERE v.id = vs.variant_id AND vs.variant_id = $1 AND vs.location_id = $2`,
		variantID, locationID, qty)
	return translateCatalogErr(err)
}

// ensureStockRow makes sure a (variant, location) pair exists before it is
// moved. A variant created before a location existed has no row for it, and a
// movement against a missing row is silently nothing.
func ensureStockRow(ctx context.Context, tx *sql.Tx, variantID, locationID int64) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO variant_stock (variant_id, location_id) VALUES ($1, $2)
		ON CONFLICT (variant_id, location_id) DO NOTHING`, variantID, locationID)
	return translateCatalogErr(err)
}

// defaultLocationID is where stock goes when nobody has said otherwise.
func defaultLocationID(ctx context.Context, tx *sql.Tx) (int64, error) {
	var id int64
	err := tx.QueryRowContext(ctx,
		`SELECT id FROM locations WHERE is_default`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, Conflictf("this store has no default location")
	}
	return id, err
}

var errInsufficientStock = errors.New("insufficient stock")

// resolveLocation turns the caller's location — or 0, meaning "wherever the
// store puts things" — into a real id, and refuses one that does not exist
// rather than moving stock into a row the operator did not mean.
func resolveLocation(ctx context.Context, tx *sql.Tx, locationID int64) (int64, error) {
	if locationID == 0 {
		return defaultLocationID(ctx, tx)
	}
	var ok bool
	err := tx.QueryRowContext(ctx,
		`SELECT true FROM locations WHERE id = $1`, locationID).Scan(&ok)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, NotFoundf("location %d does not exist", locationID)
	}
	return locationID, err
}

// Adjust moves a variant's on-hand quantity at one location by delta — a
// receipt of new stock, or a correction after a stock count. Pass 0 for the
// location to mean the default. The result may not go below what is already
// reserved *there*: those units are promised to orders that will be picked from
// that shelf, and no other shelf can answer for them.
func (i *Inventory) Adjust(ctx context.Context, variantID, locationID int64, delta int) (*Variant, error) {
	if delta == 0 {
		return i.app.catalog.GetVariant(ctx, variantID)
	}
	err := InTx(ctx, i.app.db, func(tx *sql.Tx) error {
		loc, err := i.prepare(ctx, tx, variantID, locationID)
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `
			UPDATE variant_stock
			SET on_hand = on_hand + $3, updated_at = now()
			WHERE variant_id = $1 AND location_id = $2 AND on_hand + $3 >= reserved`,
			variantID, loc, delta)
		if err != nil {
			return translateCatalogErr(err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return i.explainStockFailure(ctx, tx, variantID, loc)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return i.app.catalog.GetVariant(ctx, variantID)
}

// SetOnHand sets the absolute on-hand quantity at one location, for a stock
// take. It refuses to drop below the quantity reserved there, for Adjust's
// reason.
func (i *Inventory) SetOnHand(ctx context.Context, variantID, locationID int64, qty int) (*Variant, error) {
	if qty < 0 {
		return nil, Validationf("stock_on_hand must not be negative")
	}
	err := InTx(ctx, i.app.db, func(tx *sql.Tx) error {
		loc, err := i.prepare(ctx, tx, variantID, locationID)
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `
			UPDATE variant_stock SET on_hand = $3, updated_at = now()
			WHERE variant_id = $1 AND location_id = $2 AND $3 >= reserved`,
			variantID, loc, qty)
		if err != nil {
			return translateCatalogErr(err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return i.explainStockFailure(ctx, tx, variantID, loc)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return i.app.catalog.GetVariant(ctx, variantID)
}

// Move transfers units between two locations: one statement out, one in, in a
// single transaction, so the store's total never changes even for an instant.
//
// Reserved units do not travel. They are promised to orders that will be picked
// from where they are, and moving them would send a picker to the wrong shelf.
func (i *Inventory) Move(ctx context.Context, variantID, fromID, toID int64, qty int) (*Variant, error) {
	if qty <= 0 {
		return nil, Validationf("quantity must be positive")
	}
	err := InTx(ctx, i.app.db, func(tx *sql.Tx) error {
		from, err := resolveLocation(ctx, tx, fromID)
		if err != nil {
			return err
		}
		to, err := i.prepare(ctx, tx, variantID, toID)
		if err != nil {
			return err
		}
		if from == to {
			return Validationf("a transfer needs two different locations")
		}
		res, err := tx.ExecContext(ctx, `
			UPDATE variant_stock SET on_hand = on_hand - $3, updated_at = now()
			WHERE variant_id = $1 AND location_id = $2 AND on_hand - $3 >= reserved`,
			variantID, from, qty)
		if err != nil {
			return translateCatalogErr(err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return i.explainStockFailure(ctx, tx, variantID, from)
		}
		_, err = tx.ExecContext(ctx, `
			UPDATE variant_stock SET on_hand = on_hand + $3, updated_at = now()
			WHERE variant_id = $1 AND location_id = $2`, variantID, to, qty)
		return translateCatalogErr(err)
	})
	if err != nil {
		return nil, err
	}
	return i.app.catalog.GetVariant(ctx, variantID)
}

// ByLocation is where a variant's stock actually is. The variant's own totals
// answer "how many"; this answers "where", which is the question a picker, a
// shipping estimate and a stock take all really ask.
func (i *Inventory) ByLocation(ctx context.Context, variantID int64) ([]VariantStock, error) {
	if _, err := i.app.catalog.GetVariant(ctx, variantID); err != nil {
		return nil, err
	}
	// A left join from locations, not from variant_stock: a location holding
	// none of this variant is a real and useful answer — it is where an
	// operator would send a transfer — and an inner join would hide it.
	rows, err := i.app.db.QueryContext(ctx, `
		SELECT l.id, l.code, l.name, l.active,
		       coalesce(vs.on_hand, 0), coalesce(vs.reserved, 0)
		FROM locations l
		LEFT JOIN variant_stock vs ON vs.location_id = l.id AND vs.variant_id = $1
		ORDER BY l.priority, l.id`, variantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []VariantStock{}
	for rows.Next() {
		s := VariantStock{VariantID: variantID}
		if err := rows.Scan(&s.LocationID, &s.LocationCode, &s.LocationName,
			&s.Active, &s.OnHand, &s.Reserved); err != nil {
			return nil, err
		}
		s.Available = s.OnHand - s.Reserved
		out = append(out, s)
	}
	return out, rows.Err()
}

// prepare resolves the location and makes sure the variant has a row there, so
// that receiving stock at a location opened after the variant existed works
// without the operator having to create anything.
func (i *Inventory) prepare(ctx context.Context, tx *sql.Tx, variantID, locationID int64) (int64, error) {
	loc, err := resolveLocation(ctx, tx, locationID)
	if err != nil {
		return 0, err
	}
	var exists bool
	err = tx.QueryRowContext(ctx,
		`SELECT true FROM variants WHERE id = $1`, variantID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, NotFoundf("variant %d does not exist", variantID)
	}
	if err != nil {
		return 0, err
	}
	return loc, ensureStockRow(ctx, tx, variantID, loc)
}

// explainStockFailure turns "the update matched no rows" into a sentence naming
// the reservation that blocked it. The variant and the row are known to exist
// by the time this is reached — prepare made sure — so there is only one reason
// left.
func (i *Inventory) explainStockFailure(ctx context.Context, tx *sql.Tx, variantID, locationID int64) error {
	var reserved int
	err := tx.QueryRowContext(ctx,
		`SELECT reserved FROM variant_stock WHERE variant_id = $1 AND location_id = $2`,
		variantID, locationID).Scan(&reserved)
	if errors.Is(err, sql.ErrNoRows) {
		return NotFoundf("variant %d holds no stock at location %d", variantID, locationID)
	}
	if err != nil {
		return err
	}
	return Conflictf("stock cannot go below the %d unit(s) already reserved for open orders", reserved)
}

// LowStock lists variants at or below a threshold, so an operator — or an
// agent — can find what needs reordering. The threshold is against the store's
// total: a variant with one unit in each of five shops is not low, even though
// every individual shelf looks it.
func (i *Inventory) LowStock(ctx context.Context, threshold, limit, offset int) ([]*Variant, int, error) {
	if threshold < 0 {
		return nil, 0, Validationf("threshold must not be negative")
	}
	var total int
	if err := i.app.db.QueryRowContext(ctx, `
		SELECT count(*) FROM variants v
		WHERE v.track_inventory AND `+variantAvailable+` <= $1`, threshold).Scan(&total); err != nil {
		return nil, 0, err
	}
	if limit <= 0 {
		limit = DefaultLimit
	}
	variants, err := i.app.catalog.queryVariantsOrdered(ctx,
		`v.track_inventory AND `+variantAvailable+` <= $1`,
		variantAvailable+` ASC, v.id`, limit, offset, threshold)
	if err != nil {
		return nil, 0, err
	}
	return variants, total, nil
}

// sellStock takes units off a shelf that were never reserved.
//
// The case is an order that has already committed its stock being edited
// upward: its reservation is long gone, so there is nothing to convert and the
// units come straight off on-hand. The guard is reserveStock's, for the same
// reason — the check and the decrement have to happen under one row lock, or
// two operators editing two orders can both pass it.
func sellStock(ctx context.Context, tx *sql.Tx, variantID, locationID int64, qty int) error {
	res, err := tx.ExecContext(ctx, `
		UPDATE variant_stock vs
		SET on_hand = vs.on_hand - CASE WHEN v.track_inventory THEN $3 ELSE 0 END,
		    updated_at = now()
		FROM variants v
		WHERE v.id = vs.variant_id
		  AND vs.variant_id = $1 AND vs.location_id = $2
		  AND (NOT v.track_inventory OR v.continue_selling
		       OR vs.on_hand - vs.reserved >= $3)`,
		variantID, locationID, qty)
	if err != nil {
		return translateCatalogErr(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return errInsufficientStock
	}
	return nil
}

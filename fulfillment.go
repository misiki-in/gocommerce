package gocommerce

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strings"
)

// Fulfillments books shipments. The engine keeps the order's state machine and
// its events; a provider only talks to a carrier and reports what came back.
type Fulfillments struct {
	app       *App
	providers map[string]FulfillmentProvider
}

// Ship returns the fulfillment service.
func (a *App) Ship() *Fulfillments { return a.fulfillment }

// Providers lists the installed fulfillment codes.
func (f *Fulfillments) Providers() []string {
	codes := make([]string, 0, len(f.providers))
	for code := range f.providers {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	return codes
}

// Create books a shipment and moves the order to shipped.
//
// The carrier call happens before the transaction opens, never inside it: a
// carrier API having a bad minute must not hold write locks on the orders
// table. The transaction then re-checks the order's state, because between the
// two the order could have been cancelled by someone else.
func (f *Fulfillments) Create(ctx context.Context, orderID int64, providerCode string, req ShipRequest) (*Order, error) {
	if providerCode == "" {
		providerCode = ProviderManual
	}
	provider, ok := f.providers[providerCode]
	if !ok {
		return nil, NotFoundf("no fulfillment provider named %q", providerCode)
	}

	order, err := f.app.orders.Get(ctx, orderID)
	if err != nil {
		return nil, err
	}
	if err := shippableOrder(order); err != nil {
		return nil, err
	}

	shipment, err := provider.Ship(ctx, order, req)
	if err != nil {
		return nil, Internalf(err, "%s could not create the shipment", providerCode)
	}
	if shipment.Provider == "" {
		shipment.Provider = providerCode
	}

	return f.app.orders.transition(ctx, orderID, func(ctx context.Context, tx *sql.Tx, o *Order) (string, any, error) {
		// Re-check under the row lock: the world may have moved while the
		// carrier was thinking.
		if err := shippableOrder(o); err != nil {
			return "", nil, err
		}
		meta, err := json.Marshal(req.Meta)
		if err != nil {
			return "", nil, err
		}
		// Who is carrying it, worked out from the number, unless the provider
		// already knows — an integration that booked the shipment has been told
		// by the carrier and does not have to guess.
		carrier := shipment.Carrier
		if carrier == "" {
			if c, ok := DetectCarrier(shipment.Tracking); ok {
				carrier = c.Code
			}
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO fulfillments (order_id, provider, tracking, carrier, label_url, status, metadata)
			VALUES ($1, $2, $3, $4, $5, 'shipped', $6)`,
			o.ID, shipment.Provider, shipment.Tracking, carrier, shipment.LabelURL, meta); err != nil {
			return "", nil, err
		}
		if err := setOrderStatus(ctx, tx, o.ID, OrderShipped); err != nil {
			return "", nil, err
		}
		o.Status = OrderShipped

		payload := f.app.orders.eventPayload(o)
		payload.Tracking = shipment.Tracking
		payload.Extra = map[string]string{"provider": shipment.Provider}
		if shipment.LabelURL != "" {
			payload.Extra["label_url"] = shipment.LabelURL
		}
		return EventOrderShipped, payload, nil
	})
}

// FulfillmentPatch changes what was recorded about a shipment.
//
// Both fields are pointers because "" is a real value for each: clearing a
// tracking number that was typed against the wrong order is exactly the
// correction this exists for.
type FulfillmentPatch struct {
	Tracking *string `json:"tracking"`
	Carrier  *string `json:"carrier"`
}

// Update corrects a shipment's tracking number, and with it the carrier.
//
// Typing a tracking number is the one step of shipping a parcel that nobody
// else checks, so it is the one that gets mistyped, and until now the only fix
// was a row in the database. It does not touch the order's state: the parcel
// left either way, and correcting the number is not un-shipping it.
//
// Changing the number re-identifies the carrier, because the old one described
// the old number. An explicit carrier in the same patch wins — the operator can
// see the parcel and the engine is pattern-matching.
func (f *Fulfillments) Update(ctx context.Context, id int64, patch FulfillmentPatch) (*Fulfillment, error) {
	if patch.Tracking == nil && patch.Carrier == nil {
		return nil, Validationf("nothing to change")
	}
	if patch.Carrier != nil && *patch.Carrier != "" {
		if _, ok := CarrierByCode(*patch.Carrier, ""); !ok {
			return nil, Validationf("no carrier named %q", *patch.Carrier)
		}
	}

	var out Fulfillment
	err := InTx(ctx, f.app.db, func(tx *sql.Tx) error {
		var current Fulfillment
		var meta []byte
		if err := tx.QueryRowContext(ctx, `
			SELECT id, provider, tracking, carrier, label_url, status, metadata, created_at
			FROM fulfillments WHERE id = $1 FOR UPDATE`, id).Scan(
			&current.ID, &current.Provider, &current.Tracking, &current.Carrier,
			&current.LabelURL, &current.Status, &meta, &current.CreatedAt); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return NotFoundf("fulfillment not found")
			}
			return err
		}

		tracking, carrier := current.Tracking, current.Carrier
		if patch.Tracking != nil {
			tracking = strings.TrimSpace(*patch.Tracking)
			if tracking != current.Tracking {
				// The stored carrier described the old number. Re-read it, and
				// leave it empty when the new number identifies nobody rather
				// than keeping an answer to a question that has changed.
				carrier = ""
				if c, ok := DetectCarrier(tracking); ok {
					carrier = c.Code
				}
			}
		}
		if patch.Carrier != nil {
			carrier = *patch.Carrier
		}

		if err := tx.QueryRowContext(ctx, `
			UPDATE fulfillments SET tracking = $2, carrier = $3, updated_at = now()
			WHERE id = $1
			RETURNING id, provider, tracking, carrier, label_url, status, metadata, created_at`,
			id, tracking, carrier).Scan(
			&out.ID, &out.Provider, &out.Tracking, &out.Carrier,
			&out.LabelURL, &out.Status, &meta, &out.CreatedAt); err != nil {
			return err
		}
		return scanMetadata(meta, &out.Metadata)
	})
	if err != nil {
		return nil, err
	}
	out.decorate()
	return &out, nil
}

func shippableOrder(o *Order) error {
	switch o.Status {
	case OrderConfirmed:
		return nil
	case OrderPending:
		return Conflictf("order %s is not confirmed yet, so it cannot ship", o.Number)
	case OrderShipped, OrderDelivered:
		return Conflictf("order %s has already shipped", o.Number)
	default:
		return Conflictf("order %s is %s and cannot ship", o.Number, o.Status)
	}
}

// ------------------------------------------------------------------ manual

// ProviderManual is the built-in fulfillment method: an operator packs the box
// themselves and types in the tracking number. It is in core because it needs
// no third party, and because a store must be able to ship before it has
// integrated a carrier.
const ProviderManual = "manual"

type manualFulfillment struct{}

func (manualFulfillment) Code() string { return ProviderManual }

func (manualFulfillment) Ship(ctx context.Context, o *Order, req ShipRequest) (Shipment, error) {
	// The carrier travels through: an operator holding the parcel is a better
	// source than a pattern, and an empty one still leaves the engine to read
	// it off the number.
	return Shipment{Provider: ProviderManual, Tracking: req.Tracking, Carrier: req.Carrier}, nil
}

// Delete removes a shipment recorded in error.
//
// The order follows it. "Shipped" was true because that record existed, so
// removing the last one that says so puts the order back to confirmed — leaving
// it shipped with nothing shipping it would be a state no operation could
// explain and no operator could correct.
//
// It refuses on a delivered order. A parcel somebody received is not a record
// to erase, and undoing the delivery first is the operation that says so.
func (f *Fulfillments) Delete(ctx context.Context, id int64) (*Order, error) {
	var orderID int64
	if err := f.app.db.QueryRowContext(ctx,
		`SELECT order_id FROM fulfillments WHERE id = $1`, id).Scan(&orderID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, NotFoundf("fulfillment not found")
		}
		return nil, err
	}

	return f.app.orders.transition(ctx, orderID, func(ctx context.Context, tx *sql.Tx, o *Order) (string, any, error) {
		if o.Status == OrderDelivered {
			return "", nil, Conflictf(
				"order %s is delivered; undo the delivery before removing what shipped it", o.Number)
		}

		res, err := tx.ExecContext(ctx, `DELETE FROM fulfillments WHERE id = $1`, id)
		if err != nil {
			return "", nil, err
		}
		if n, err := res.RowsAffected(); err != nil {
			return "", nil, err
		} else if n == 0 {
			// Deleted by somebody else between the read and the lock.
			return "", nil, NotFoundf("fulfillment not found")
		}

		var left int
		if err := tx.QueryRowContext(ctx,
			`SELECT count(*) FROM fulfillments WHERE order_id = $1 AND status <> 'cancelled'`,
			o.ID).Scan(&left); err != nil {
			return "", nil, err
		}
		if left == 0 && o.Status == OrderShipped {
			if err := setOrderStatus(ctx, tx, o.ID, OrderConfirmed); err != nil {
				return "", nil, err
			}
			o.Status = OrderConfirmed
		}
		return EventOrderUnshipped, f.app.orders.eventPayload(o), nil
	})
}

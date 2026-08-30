package gocommerce

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"sort"
)

// Payments owns payment state. Every gateway, every admin action and every
// webhook funnels through MarkPaid and MarkFailed, so there is exactly one
// place where an order's money status changes — and exactly one place where
// the event announcing it is written.
type Payments struct {
	app       *App
	providers map[string]PaymentProvider
}

// Pay returns the payment service.
func (a *App) Pay() *Payments { return a.payments }

func (p *Payments) provider(code string) (PaymentProvider, bool) {
	pr, ok := p.providers[code]
	return pr, ok
}

// Methods lists the payment codes this store accepts.
func (p *Payments) Methods() []string {
	codes := make([]string, 0, len(p.providers))
	for code := range p.providers {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	return codes
}

// MarkPaid records that money arrived and confirms the order.
//
// It is idempotent by design, not by accident: a gateway will replay a webhook,
// and marking an already-paid order paid a second time must not commit its
// stock twice or emit a second order.paid.
//
// Confirming here is what makes a gateway order shippable — without it, a paid
// order would sit at "pending" forever waiting for a step nobody performs.
func (p *Payments) MarkPaid(ctx context.Context, orderID int64, reference string) (*Order, error) {
	return p.app.orders.transition(ctx, orderID, func(ctx context.Context, tx *sql.Tx, o *Order) (string, any, error) {
		if o.PaymentStatus == PaymentPaid {
			return "", nil, nil
		}
		if o.Status == OrderCancelled {
			// Money for a cancelled order is a refund problem, not a
			// confirmation problem, and silently reviving the order would
			// resurrect an inventory reservation nobody is holding.
			return "", nil, Conflictf("order %s was cancelled; this payment needs a refund, not a confirmation", o.Number)
		}

		if _, err := tx.ExecContext(ctx, `
			UPDATE orders
			SET payment_status = $2,
			    payment_reference = coalesce(nullif($3, ''), payment_reference),
			    updated_at = now()
			WHERE id = $1`, o.ID, PaymentPaid, reference); err != nil {
			return "", nil, err
		}
		o.PaymentStatus = PaymentPaid

		if o.Status == OrderPending {
			if err := commitOrderStock(ctx, tx, o); err != nil {
				return "", nil, err
			}
			if err := setOrderStatus(ctx, tx, o.ID, OrderConfirmed); err != nil {
				return "", nil, err
			}
			o.Status = OrderConfirmed
		}
		return EventOrderPaid, p.app.orders.eventPayload(o), nil
	})
}

// MarkUnpaid takes back a payment that was recorded by mistake.
//
// This is a correction, not a refund. A refund says money went out and is a
// fact about the world; this says the money never came in and somebody clicked
// the wrong row — so it leaves no refund behind and no trace but the event.
//
// It moves the payment and nothing else. The temptation is to also undo the
// confirmation MarkPaid performs, and that is wrong: a confirmed order awaiting
// payment is not a broken state, it is what every cash-on-delivery order in the
// store already is. Un-confirming would put the stock back on the shelf while
// the order is still live, which is the one outcome nobody wants — and for a
// COD order, whose checkout confirmed it long before anybody touched the
// payment, it would reverse a decision this never made.
//
// So the stock does not move. An operator who wants it back cancels the order,
// which is the operation that means that and already knows how.
func (p *Payments) MarkUnpaid(ctx context.Context, orderID int64) (*Order, error) {
	return p.app.orders.transition(ctx, orderID, func(ctx context.Context, tx *sql.Tx, o *Order) (string, any, error) {
		switch o.PaymentStatus {
		case PaymentPending, PaymentFailed:
			// Already not paid. Nothing to take back.
			return "", nil, nil
		case PaymentRefunded:
			return "", nil, Conflictf("order %s was refunded; that is a payment that happened and came back, not one to erase", o.Number)
		}
		if o.Status == OrderCancelled {
			return "", nil, Conflictf("order %s is cancelled; money on it is a refund, not a mistake to unrecord", o.Number)
		}

		// The reference goes with it. It described a payment that did not
		// happen, and leaving it behind would have the next person looking for
		// a transaction nobody can find.
		if _, err := tx.ExecContext(ctx, `
			UPDATE orders SET payment_status = $2, payment_reference = '', updated_at = now()
			WHERE id = $1`, o.ID, PaymentPending); err != nil {
			return "", nil, err
		}
		o.PaymentStatus = PaymentPending
		return EventOrderUnpaid, p.app.orders.eventPayload(o), nil
	})
}

// MarkFailed records that a payment attempt did not succeed. The order stays
// pending so the shopper can try again; if nobody does, the unpaid sweeper
// eventually cancels it and returns the stock.
func (p *Payments) MarkFailed(ctx context.Context, orderID int64, reason string) (*Order, error) {
	return p.app.orders.transition(ctx, orderID, func(ctx context.Context, tx *sql.Tx, o *Order) (string, any, error) {
		if o.PaymentStatus == PaymentPaid {
			return "", nil, Conflictf("order %s is already paid", o.Number)
		}
		if o.PaymentStatus == PaymentFailed {
			return "", nil, nil
		}
		_, err := tx.ExecContext(ctx,
			`UPDATE orders SET payment_status = $2, updated_at = now() WHERE id = $1`,
			o.ID, PaymentFailed)
		if err != nil {
			return "", nil, err
		}
		p.app.log.Info("payment failed", "order", o.Number, "reason", reason)
		return "", nil, nil
	})
}

// Refund returns money through the provider that took it. A provider that
// cannot refund — cash on delivery, for one — simply does not implement
// [Refunder], and this reports that plainly instead of pretending.
func (p *Payments) Refund(ctx context.Context, orderID int64, amountMinor int64) (*Order, error) {
	o, err := p.app.orders.Get(ctx, orderID)
	if err != nil {
		return nil, err
	}
	if o.PaymentStatus != PaymentPaid {
		return nil, Conflictf("order %s is not paid, so there is nothing to refund", o.Number)
	}
	if amountMinor <= 0 {
		amountMinor = o.Total.AmountMinor
	}
	if amountMinor > o.Total.AmountMinor {
		return nil, Validationf("refund of %d exceeds the order total of %d",
			amountMinor, o.Total.AmountMinor)
	}

	provider, ok := p.provider(o.PaymentProvider)
	if !ok {
		return nil, Conflictf("payment method %q is not installed in this build, so it cannot refund", o.PaymentProvider)
	}
	refunder, ok := provider.(Refunder)
	if !ok {
		return nil, Conflictf("payment method %q does not support refunds", o.PaymentProvider)
	}

	// The provider call happens outside any transaction: refunding is a
	// network round trip and must not hold core write locks.
	if err := refunder.Refund(ctx, o, amountMinor); err != nil {
		return nil, Internalf(err, "the refund was declined by %s", o.PaymentProvider)
	}

	return p.app.orders.transition(ctx, orderID, func(ctx context.Context, tx *sql.Tx, o *Order) (string, any, error) {
		if o.PaymentStatus == PaymentRefunded {
			return "", nil, nil
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE orders SET payment_status = $2, updated_at = now() WHERE id = $1`,
			o.ID, PaymentRefunded); err != nil {
			return "", nil, err
		}
		o.PaymentStatus = PaymentRefunded
		return "", nil, nil
	})
}

// ------------------------------------------------------------ cash on delivery

// codProvider is the built-in payment method: the one that needs no third
// party, which is why it can live in core without dragging an SDK behind it.
//
// Checkout with cash on delivery confirms the order immediately — the sale is
// happening and the stock leaves the shelf — while payment stays pending until
// an operator marks it paid on delivery.
type codProvider struct{}

// CodeCOD is the payment code of the built-in cash-on-delivery method.
const CodeCOD = "cod"

func (codProvider) Code() string { return CodeCOD }

func (codProvider) Initiate(ctx context.Context, o *Order, opts PayOptions) (PaymentIntent, error) {
	return PaymentIntent{Kind: IntentNone, Provider: CodeCOD}, nil
}

func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

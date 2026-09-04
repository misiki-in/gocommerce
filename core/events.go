package gocommerce

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// The frozen event taxonomy. These names are a public contract: a consumer
// written against them must keep working, so a name is added only when
// something real produces it and is never repurposed.
const (
	EventOrderCreated   = "order.created"
	EventOrderPaid      = "order.paid"
	EventOrderShipped   = "order.shipped"
	EventOrderDelivered = "order.delivered"
	EventOrderCancelled = "order.cancelled"
	EventOrderEdited    = "order.edited"

	// The two corrections. Each says a fact recorded earlier was not true —
	// somebody marked the wrong row — so they are their own names rather than
	// an `order.edited`, which means the order itself changed and carries the
	// amendment. A consumer that congratulated the customer on a payment is
	// exactly the consumer that needs to hear this.
	EventOrderUnpaid      = "order.unpaid"
	EventOrderUndelivered = "order.undelivered"
	EventOrderUnshipped   = "order.unshipped"
)

// Aggregate types name what an event is about.
const AggregateOrder = "order"

// Event is one thing that happened. It is created inside the transaction that
// caused it and delivered afterwards, so an event exists if and only if the
// change it describes was committed.
type Event struct {
	ID            string          `json:"id"`
	Name          string          `json:"name"`
	Version       int             `json:"v"`
	At            time.Time       `json:"at"`
	AggregateType string          `json:"aggregate_type"`
	AggregateID   int64           `json:"aggregate_id"`
	Data          json.RawMessage `json:"data"`
}

// Decode unmarshals the payload into v.
func (e Event) Decode(v any) error {
	if len(e.Data) == 0 {
		return nil
	}
	return json.Unmarshal(e.Data, v)
}

// EventHandler consumes an event.
//
// Delivery is at-least-once. A handler that is not idempotent will eventually
// do its work twice, because a crash between doing the work and recording the
// delivery is a normal event rather than a bug. Returning an error asks for
// redelivery with backoff.
type EventHandler func(ctx context.Context, e Event) error

// OrderEvent is the payload of every order.* event: enough for a consumer to
// act without reading the database, and stable enough to be a contract.
type OrderEvent struct {
	OrderID       int64            `json:"order_id"`
	Number        string           `json:"number"`
	Status        string           `json:"status"`
	PaymentStatus string           `json:"payment_status"`
	Provider      string           `json:"payment_provider"`
	Currency      string           `json:"currency"`
	TotalMinor    int64            `json:"total_minor"`
	Email         string           `json:"email"`
	Phone         string           `json:"phone,omitempty"`
	Name          string           `json:"name,omitempty"`
	Language      string           `json:"language"`
	Lines         []OrderEventLine `json:"lines"`
	Tracking      string           `json:"tracking,omitempty"`
	Reason        string           `json:"reason,omitempty"`
	// Change is set on order.edited: what the amendment did, and the totals
	// either side of it. It is the part of an edited order that the order
	// itself no longer says, because the order says what is agreed now.
	Change   *OrderChange      `json:"change,omitempty"`
	Metadata map[string]any    `json:"metadata,omitempty"`
	Extra    map[string]string `json:"extra,omitempty"`
}

// OrderEventLine is one line of an order, as it was at the time.
type OrderEventLine struct {
	SKU            string `json:"sku"`
	Title          string `json:"title"`
	VariantLabel   string `json:"variant_label,omitempty"`
	Quantity       int    `json:"quantity"`
	UnitPriceMinor int64  `json:"unit_price_minor"`
	TotalMinor     int64  `json:"total_minor"`
}

// ------------------------------------------------------------------ the bus

type subscription struct {
	pattern string
	owner   string
	handler EventHandler
}

// eventBus holds the in-process subscriptions. It is only the delivery
// mechanism: whether an event exists at all is the database's answer, not the
// bus's, which is what lets the dispatcher be replaced without touching
// correctness.
type eventBus struct {
	mu      sync.RWMutex
	subs    []subscription
	log     *slog.Logger
	timeout time.Duration
}

func (b *eventBus) subscribe(pattern, owner string, h EventHandler) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subs = append(b.subs, subscription{pattern: pattern, owner: owner, handler: h})
}

// dispatch delivers to every matching handler and reports whether any failed.
// Every handler runs even if an earlier one fails, so one broken consumer
// cannot starve the others; the aggregated error asks the outbox to redeliver.
func (b *eventBus) dispatch(ctx context.Context, e Event) error {
	b.mu.RLock()
	subs := make([]subscription, len(b.subs))
	copy(subs, b.subs)
	b.mu.RUnlock()

	var failures []error
	for _, sub := range subs {
		if !matchEventPattern(sub.pattern, e.Name) {
			continue
		}
		if err := b.runHandler(ctx, sub, e); err != nil {
			b.log.Error("event handler failed",
				"event", e.Name, "event_id", e.ID, "owner", sub.owner, "error", err)
			failures = append(failures, fmt.Errorf("%s: %w", sub.owner, err))
		}
	}
	return errors.Join(failures...)
}

// runHandler bounds one handler and converts a panic into an error, so a bug
// in a consumer cannot take down the dispatcher.
func (b *eventBus) runHandler(ctx context.Context, sub subscription, e Event) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("handler panicked: %v", p)
		}
	}()
	hctx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()
	return sub.handler(hctx, e)
}

// matchEventPattern supports an exact name, a "prefix.*" wildcard, and "*".
func matchEventPattern(pattern, name string) bool {
	switch {
	case pattern == "*":
		return true
	case strings.HasSuffix(pattern, ".*"):
		return strings.HasPrefix(name, strings.TrimSuffix(pattern, "*"))
	default:
		return pattern == name
	}
}

// newUUID returns a random (version 4) UUID. Event identity has to be globally
// unique and unguessable, and 16 bytes from crypto/rand is that, without
// taking a dependency for it.
func newUUID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate uuid: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

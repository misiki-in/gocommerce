# Events

Events are how a store's behaviour is extended without changing its core.
An invoice module, an email notifier and a search indexer all do their work by
reacting to events, and none of them appears anywhere in the checkout code.

That only works if events are trustworthy, so they are part of correctness
rather than a notification convenience.

## The guarantee

**An event exists if and only if the change it describes was committed.**

Both halves matter. The engine writes the event to an `outbox_events` row
inside the same transaction as the state change:

```
BEGIN
  create the order
  reserve the inventory
  INSERT INTO outbox_events ...
COMMIT
```

Either all of it happens or none of it does. A process that dies immediately
after the commit has still recorded the event; a transaction that rolls back
leaves no trace of one. The failure this rules out is the expensive one: an
order that is paid for and that nobody was ever told about.

A separate dispatcher then delivers what the outbox holds, claiming rows with
`FOR UPDATE SKIP LOCKED` so several application instances can run without ever
handing the same event to two workers at once.

## Delivery is at-least-once

A handler can run, and the process can die before the delivery is recorded. On
the next pass the event is delivered again. This is normal operation, not a
bug, and it is the price of never losing an event.

**Every handler must therefore be idempotent.** In practice that means one of:

- Make the work naturally repeatable — setting a status to `paid` twice is
  harmless.
- Guard with a unique constraint. The invoices module has `UNIQUE (order_id)`,
  so a second delivery inserts nothing.
- Record what you have seen. The Stripe module claims each event id in its own
  table before acting.

Every event carries a stable `id`. If nothing else fits, remember it.

Returning an error from a handler asks for redelivery with exponential
backoff. After twelve failures the row is marked dead rather than deleted: an
event nobody could deliver is evidence, and evidence should outlive the
incident.

## The taxonomy

These names are a public contract. A name is added when something real
produces it, and is never repurposed.

| Event | When | Notes |
|---|---|---|
| `order.created` | An order was created at checkout | Inventory is reserved, or committed for cash on delivery |
| `order.paid` | Money arrived | Also confirms a pending order, so it becomes shippable |
| `order.shipped` | A shipment was booked | Payload carries `tracking` |
| `order.delivered` | The customer received it | |
| `order.cancelled` | The order was voided | Stock has been returned; payload carries `reason` |

There is deliberately no `order.confirmed`. Confirmation always coincides with
either `order.created` (cash on delivery) or `order.paid` (everything else), so
a separate event would carry no information and would be one more name frozen
forever.

There is no `product.*` family either, until something consumes it.

## The payload

Every `order.*` event carries an `OrderEvent`:

```json
{
  "order_id": 42,
  "number": "GC-000042",
  "status": "confirmed",
  "payment_status": "paid",
  "payment_provider": "stripe",
  "currency": "USD",
  "total_minor": 5000,
  "email": "shopper@example.com",
  "phone": "",
  "name": "A Shopper",
  "language": "en",
  "lines": [
    {
      "sku": "TEE-001",
      "title": "Cotton tee",
      "variant_label": "M / Black",
      "quantity": 2,
      "unit_price_minor": 2500,
      "total_minor": 5000
    }
  ],
  "tracking": "",
  "reason": ""
}
```

It carries enough for a consumer to act without reading the database — a
notifier can write an email from this alone.

Amounts are integer minor units, as everywhere else. `language` is the
language the shopper checked out in, so a notifier can reply in it.

### Changing a payload

Adding an optional field is safe. Removing or repurposing one is not: a
consumer written last year is still running. If a payload has to change
incompatibly, publish it under a new event version — `Event.V` exists for
exactly that — and keep emitting the old one until consumers have moved.

## Subscribing

```go
func (m *Module) Register(app *gocommerce.App) error {
    app.Subscribe(gocommerce.EventOrderPaid, m.onPaid)
    return nil
}

func (m *Module) onPaid(ctx context.Context, e gocommerce.Event) error {
    var ev gocommerce.OrderEvent
    if err := e.Decode(&ev); err != nil {
        return err
    }
    // Idempotent: a unique constraint makes a redelivery a no-op.
    return m.issueInvoice(ctx, ev.OrderID)
}
```

Patterns may be an exact name, a prefix (`order.*`), or everything (`*`).

Handlers run under `Config.HandlerTimeout` (10 seconds by default). A handler
that hangs is failed and retried rather than being allowed to stall the
dispatcher.

## Who may publish

Core does. A module reacts to events and calls domain services; it does not
announce state changes itself, because an event that does not correspond to a
committed core transaction would break the guarantee this whole page rests on.

## Testing

`gctest` makes delivery synchronous so a test never sleeps:

```go
app := gctest.New(t, mymodule.New(cfg))
result := gctest.PlaceOrder(t, app, gocommerce.CodeCOD)
gctest.DrainOutbox(t, app)          // deliver everything pending
gctest.AssertOutboxEmpty(t, app)    // nothing failed or was parked
```

To prove your handler is idempotent, replay the events and assert the result
did not change:

```go
_, _ = app.DB().ExecContext(ctx,
    `UPDATE outbox_events SET published_at = NULL, available_at = now()`)
gctest.DrainOutbox(t, app)
```

---
name: events
description: Use when reacting to commerce state changes — subscribing a handler, making it idempotent, adding an event, or debugging the outbox and its dead letters.
---

# Events

The contract in [`docs/events.md`](../docs/events.md) is the public reference;
this is how to work with it. `outbox.go` and `events.go` are the code.

## The model

An event is written to `outbox_events` **inside the transaction that caused it**
and delivered afterwards by a separate dispatcher.

```
BEGIN
  UPDATE orders SET payment_status = 'paid' …
  commit the reserved stock
  INSERT INTO outbox_events (event_id, event_name, …, payload)
COMMIT                              -- both, or neither
```

Nothing else may write that table: `outbox.write(ctx, tx, name, aggregateType,
aggregateID, payload)` is the only path, and it takes a `*sql.Tx` because there
is no correct way to call it without one.

The dispatcher claims rows with `FOR UPDATE SKIP LOCKED`, so several
application instances can run without handing the same event to two workers.
`App.nudgeOutbox` wakes it after a request writes an event, so the common case
is delivered in milliseconds rather than at the next poll (`Config.OutboxPoll`,
1s; `Config.OutboxBatchSize`, 100).

```go
type Event struct {
    ID            string          `json:"id"`   // uuid v4, stable across redeliveries
    Name          string          `json:"name"`
    Version       int             `json:"v"`
    At            time.Time       `json:"at"`
    AggregateType string          `json:"aggregate_type"` // "order"
    AggregateID   int64           `json:"aggregate_id"`
    Data          json.RawMessage `json:"data"`
}
```

## The names

Five, all of them in `events.go`, all `AggregateOrder`:

| Constant | Name | Emitted by |
|---|---|---|
| `EventOrderCreated` | `order.created` | `Orders.Checkout` phase A; also `Transfer.ImportOrders` when `POST /api/admin/import/orders?fire_events=true` |
| `EventOrderPaid` | `order.paid` | `Payments.MarkPaid` |
| `EventOrderShipped` | `order.shipped` | `Fulfillments.Create` |
| `EventOrderDelivered` | `order.delivered` | `Orders.MarkDelivered` |
| `EventOrderCancelled` | `order.cancelled` | `Orders.Cancel`, including the unpaid sweeper |

Three transitions deliberately emit **nothing**: `Orders.Confirm` (the story is
already told by `order.created` or `order.paid`), `Payments.MarkFailed` (the
order stays `pending` and the shopper may still succeed), and `Payments.Refund`
(there is no `order.refunded`; add one only when something consumes it). A
no-op transition is silent too — marking a paid order paid announces nothing,
because nothing changed.

## The payload

Every `order.*` event carries the same `OrderEvent`, so a consumer can act
without reading the database:

```json
{"order_id":42,"number":"GC-000042","status":"confirmed","payment_status":"paid",
 "payment_provider":"stripe","currency":"USD","total_minor":5000,
 "email":"shopper@example.com","phone":"","name":"A Shopper","language":"en",
 "lines":[{"sku":"TEE-001","title":"Cotton tee","variant_label":"M / Black",
           "quantity":2,"unit_price_minor":2500,"total_minor":5000}],
 "tracking":"","reason":""}
```

Two fields are populated only where they mean something:

- `reason` — on `order.cancelled`, the cancellation reason (the sweeper's is
  `"payment not completed in time"`).
- `tracking` — on `order.shipped`, plus `extra` carrying `provider` and, when
  the carrier returned one, `label_url`.

Amounts are minor units and `language` is the language the shopper checked out
in, so a notifier can reply in it. `lines` is the order's snapshot at the time
of the event, not a live catalog read.

## Invariants

**An event exists if and only if the change it describes was committed.** The
failure this rules out is the expensive one: an order that is paid for and that
nobody was ever told about. Never write state and then emit — a crash between
the two is a lie the system never notices — and never publish from a handler to
announce a state change (AGENTS.md rule 4).

**Delivery is at-least-once, so every handler must be idempotent.** A handler
can run and the process can die before `published_at` is written; the next pass
delivers it again. Key on `Event.ID`, which is stable across redeliveries:

- Make the work naturally repeatable — setting a status to `paid` twice is
  harmless.
- Guard with a unique constraint. `ext/invoices` has `UNIQUE (order_id)` on
  `invoices_documents`, so a redelivery inserts nothing.
- Claim the id in your own table. `ext/payments-stripe` does exactly this with
  `INSERT … ON CONFLICT (id) DO NOTHING`.

Note the sharper version of this: **every** matching handler runs on every
delivery, and the dispatcher joins their errors. One failing consumer causes the
whole event to be redelivered, so the handlers that already succeeded run again.
Idempotency is not only about your own crashes.

**Failure is retried with backoff, then dead-lettered.** `attempts` increments
on claim, `available_at` moves out by `2^(attempts-1)` seconds capped at 15
minutes, and `last_error` records why. After **12** attempts the row is marked
`dead = true` rather than deleted — an event nobody could deliver is evidence,
and evidence should outlive the incident. A claimed row is also invisible for 60
seconds, so a process that dies mid-delivery releases its work automatically.

## How to subscribe

```go
func (m *Module) Register(app *gocommerce.App) error {
    app.Subscribe(gocommerce.EventOrderPaid, m.onPaid)  // exact
    app.Subscribe("order.*", m.audit)                   // prefix
    return nil
}

func (m *Module) onPaid(ctx context.Context, e gocommerce.Event) error {
    var ev gocommerce.OrderEvent
    if err := e.Decode(&ev); err != nil {
        return err // malformed payload: retrying will not help, but it is loud
    }
    return m.issueInvoice(ctx, ev.OrderID) // UNIQUE (order_id) makes a replay a no-op
}
```

Patterns are an exact name, a prefix (`order.*`), or `*`. Handlers run under
`Config.HandlerTimeout` (10s); a panic is recovered into an error rather than
taking down the dispatcher.

Return an error only for something a retry could fix — a vendor being briefly
unreachable. For a permanent rejection, log it and return nil, or you will spend
twelve attempts learning the same thing.

## How to test a handler

```go
app := gctest.New(t, mymodule.New(cfg))
result := gctest.PlaceOrder(t, app, gocommerce.CodeCOD)
gctest.DrainOutbox(t, app)       // synchronous delivery — never sleep
gctest.AssertOutboxEmpty(t, app) // nothing pending, nothing dead
```

Prove idempotency by replaying rather than by reading the code:

```go
_, _ = app.DB().ExecContext(ctx,
    `UPDATE outbox_events SET published_at = NULL, available_at = now()`)
gctest.DrainOutbox(t, app)       // assert the result did not change
```

## How to debug a stuck outbox

```go
pending, dead, err := app.PendingEvents(ctx)
```

`gocommerce doctor -json` reports the same thing and exits non-zero, so it can
gate work without being parsed. When `dead` is non-zero, read the rows:

```sql
SELECT event_name, aggregate_id, attempts, last_error
FROM outbox_events WHERE dead ORDER BY id DESC;
```

Fix the handler, then clear `dead` and `published_at` on the rows you want
redelivered. That is a repair, not a routine.

## Common mistakes

- **Emitting an event outside the transaction that made the change**, or from a
  module at all. Core publishes; modules react.
- **A handler that is not idempotent.** It will do its work twice, and the
  second time will not be a crash you caused.
- **Deduplicating on `order_id` when you meant `event_id`.** An order has five
  events; each is delivered at least once.
- **Doing slow work in a handler.** Ten seconds and it is failed and retried.
  Record the intent, do the work elsewhere.
- **Adding a name for a transition nothing consumes.** Names are frozen once
  shipped and never repurposed.
- **Changing a payload field's meaning.** Adding an optional field is safe;
  removing or repurposing one breaks a consumer written last year. Bump
  `Event.Version` and keep emitting the old shape until consumers have moved.

Related: [orders](orders.md), [payments](payments.md), [checkout](checkout.md),
[integrations](integrations.md).

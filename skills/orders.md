---
name: orders
description: Use when reading or changing order state — the confirm/ship/deliver/cancel transitions, order line snapshots, guest order lookup, or the unpaid sweeper.
---

# Orders

## The model

`Orders` owns the state machine. Every transition lives in `orders.go` — no
module, script or integration gets to invent its own version of confirming or
cancelling (AGENTS.md rule 3).

Two independent statuses, both `CHECK`-constrained in `schema.go`:

```
status:         pending → confirmed → shipped → delivered
                   ↓          ↓
                     cancelled
payment_status: pending → paid → refunded
                   ↓
                 failed
```

| Transition | Call | HTTP | Event |
|---|---|---|---|
| pending → confirmed | `Order().Confirm(ctx, id)` | *(no route — it happens via payment or COD checkout)* | none |
| payment → paid, and pending → confirmed | `Pay().MarkPaid(ctx, id, ref)` | `POST /api/admin/orders/{id}/mark-paid` | `order.paid` |
| payment → failed | `Pay().MarkFailed(ctx, id, reason)` | *(module-driven)* | none |
| confirmed → shipped | `Ship().Create(ctx, id, code, req)` | `POST /api/admin/create-fulfillment` | `order.shipped` |
| shipped → delivered | `Order().MarkDelivered(ctx, id)` | `POST /api/admin/orders/{id}/deliver` | `order.delivered` |
| pending\|confirmed → cancelled | `Order().Cancel(ctx, id, reason)` | `POST /api/admin/orders/{id}/cancel` | `order.cancelled` |
| paid → refunded | `Pay().Refund(ctx, id, amountMinor)` | `POST /api/admin/orders/{id}/refund` | none |

Every one of them runs through `Orders.transition`, which opens `InTx`, reads
the order `FOR UPDATE`, loads its lines, runs the callback, and writes the
callback's event to the outbox **in the same transaction**. That is what makes
the change and the event inseparable (AGENTS.md rule 4).

## Invariants

**A transition that changes nothing announces nothing.** The callback returns an
empty event name to stay quiet. Confirming an already-confirmed order, marking a
paid order paid, cancelling a cancelled order — all no-ops, all silent, because
gateways replay webhooks and an event per replay would be a lie about how many
times the world changed.

There is deliberately no `order.confirmed`. Confirmation always coincides with
`order.created` (cash on delivery) or `order.paid` (everything else), so a
separate name would carry no information and be frozen forever.

**Inventory movement is derived from status, never stored.** `stockCommitted`
reads `status`, so the two cannot disagree. Cancelling therefore does different
things depending on how far the order got: a `pending` order only ever
*reserved*, so `releaseStock` drops the reservation; a `confirmed` order already
took the units off the shelf, so `restockStock` puts them back — onto the shelf
the line records, not onto the default. Getting this
backwards silently invents or destroys inventory. An order that has shipped
cannot be cancelled at all — that is a return, and 409 says so.

**`order_lines` are snapshots with nullable foreign keys.** `product_id` and
`variant_id` are `REFERENCES … ON DELETE SET NULL`, while `sku`, `title`,
`variant_label`, `unit_price_minor` and `total_minor` are `NOT NULL` copies. A
two-year-old order stays readable — and legally meaningful — after the product
is deleted or renamed and the price has moved four times. The same applies to
`email`, `name`, `phone` and the `address` jsonb on `orders` itself.

The cost is real and worth knowing: stock movements skip lines whose
`variant_id` is NULL, because there is nothing left to move. Cancelling an order
whose variant was deleted restocks nothing, correctly.

**Guest checkout is permanent** (AGENTS.md rule 8). There is no `customer_id`.
An order is reachable by `orders.access_token`, returned once at checkout.
`GetForGuest` compares it in constant time and reports a mismatch as
**not found**, so the endpoint cannot be walked to discover which order numbers
exist.

**Money is `*_minor` integers plus a currency.** `Money{AmountMinor, Currency}`
serializes as `{"amount_minor": 2500, "currency": "USD"}` — never a formatted
string, because decimal places belong to the currency and symbols to the
reader's locale.

## How to read an order

```go
o, err := app.Order().Get(ctx, 42)                       // by id
o, err := app.Order().GetByNumber(ctx, "GC-000042")      // by human number
o, err := app.Order().GetForGuest(ctx, "GC-000042", tok) // by access token
```

```http
GET /api/orders/GC-000042?token=…            # the guest's own order
GET /api/admin/orders?status=confirmed&payment_status=paid&limit=50
GET /api/admin/orders/42
```

`OrderQuery` filters on `Status`, `PaymentStatus`, `Email` (case-insensitive),
`From`/`To` over `created_at`, plus `Limit`/`Offset`; `List` returns the page and
the total. `access_token` is `omitempty` and never populated by a listing.

## How to move an order forward

```go
// Settlement — the only path, for every provider. See payments.md.
order, err := app.Pay().MarkPaid(ctx, 42, "pi_3Ox…")

// Ship it. The carrier call happens before the transaction opens; the
// transaction then re-checks status, because the world may have moved.
order, err := app.Ship().Create(ctx, 42, gocommerce.ProviderManual,
    gocommerce.ShipRequest{Tracking: "1Z999AA1"})

order, err := app.Order().MarkDelivered(ctx, 42)
order, err := app.Order().Cancel(ctx, 42, "customer changed their mind")
```

```http
POST /api/admin/create-fulfillment
{"order_id":42,"provider":"manual","tracking":"1Z999AA1"}

POST /api/admin/orders/42/cancel
{"reason":"customer changed their mind"}
```

Out-of-order calls are refused with 409, not tolerated: delivering an order that
never shipped, shipping one that is still `pending`, cancelling one that has
shipped.

## How to reclaim abandoned inventory

`Orders.SweepUnpaid` cancels `pending`/`pending` orders whose
`reservation_expires_at` has passed, 200 at a time, and returns their stock. It
runs every five minutes from `App.runSweepers` alongside the cart sweeper, so
you rarely call it directly — but call it in a test that asserts a reservation
is released. Without it an abandoned redirect holds inventory out of sale
forever, which is invisible until the day it sells out a product that is
actually in stock.

## Common mistakes

- **`UPDATE orders SET status = …` from a module or a script.** Rule 3. The
  status changes, the stock does not move, and nothing is told. Use the service.
- **Adding a transition outside `Orders.transition`.** You lose the row lock,
  the same-transaction event, or both.
- **Emitting an event from an idempotent no-op.** Return `""`.
- **Reading `payment_status` to decide shippability.** `shippableOrder` reads
  `status`; a COD order ships while `payment_status` is still `pending`.
- **Assuming `cancelled` means unpaid.** `MarkPaid` on a cancelled order is a
  409 telling you it needs a refund, not a confirmation — reviving the order
  would resurrect a reservation nobody is holding.
- **Building a customer record on `email`.** Identity is a module's job through
  `Config.AdminAuth`; core stays guest-only.

Related: [checkout](checkout.md), [payments](payments.md), [events](events.md),
[inventory](inventory.md).

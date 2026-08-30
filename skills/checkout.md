---
name: checkout
description: Use when turning a cart into an order — changing the checkout path, handling the 409 re-validation conflict, or making a client's Idempotency-Key retries safe.
---

# Checkout

## The model

Checkout is `POST /api/checkout/{code}`, where `code` is a registered payment
method (`GET /api/checkout` lists the installed ones). One call, two phases,
and a result carrying both the order and what the client must do next.

```go
result, err := app.Order().Checkout(ctx, "stripe", gocommerce.CheckoutInput{
    CartID: cartToken, // the cart's opaque token — its JSON "id", not a number
    Email:  "shopper@example.com",
    Name:   "A Shopper",
    Address: gocommerce.Address{
        Line1: "1 Main St", City: "Austin", PostalCode: "78701", Country: "US",
    },
    ReturnURL:   "https://store.example/thanks", // redirect-flow gateways only
    PaymentData: map[string]string{"save_card": "true"},
}, idempotencyKey)
// result.Order *Order, result.Payment PaymentIntent
```

**Phase A**, `createOrderFromCart`, is one transaction with no network in it:
lock the cart `FOR UPDATE`, re-validate every line, reserve stock, insert
`orders` and `order_lines`, flip the cart to `converted`, claim the idempotency
key, write `order.created` to the outbox.

**Phase B**, `initiatePayment`, runs after that transaction has committed:
`provider.Initiate(ctx, order, PayOptions{ReturnURL, Data})`, then store
`orders.payment_reference` if the provider returned one.

## Invariants

**The commit happens before the gateway call.** This is AGENTS.md rule 5 in its
original form. `Initiate` is an HTTP round trip to somebody else's server;
holding the cart row lock and every reserved variant row across it means a
gateway having a slow day becomes the store having a slow day, and a gateway
that hangs becomes a store that cannot sell anything.

The consequence is that **phase B can fail with the order already real**. When
it does, nothing is rolled back: the order stands at `status=pending`,
`payment_status=pending`, stock reserved, `order.created` already committed to
the outbox, and the caller gets a 500. That is the correct outcome — the
alternative is un-reserving stock a shopper is about to pay for. The order's
`reservation_expires_at` (`Config.OrderTTL`, 24h by default) means an abandoned
attempt eventually returns its stock via `Orders.SweepUnpaid`.

**An `Idempotency-Key` retry resumes; it never re-orders.** The key is stored
in `idempotency_keys` under `scope = "checkout:" + code` with a `request_hash`
over the code and the whole body, and `UNIQUE (scope, key)` is what makes a
double-tapped submit one order instead of two. A retry has four outcomes:

| Stored state | What happens |
|---|---|
| No row | Fresh checkout |
| Row, different `request_hash` | 400 — same key, different body is a client bug |
| Row with `response` | The stored `CheckoutResult`, with the order re-read so it reflects current state |
| Row with `order_id`, no `response` | **Resume phase B** on that order — no second reservation |
| Concurrent insert loses the race | 409, "a checkout with this Idempotency-Key is already in progress" |

The resume row is exactly the phase-B failure above, which is why the two
designs are one design.

**Re-validation is all-or-nothing, and current price always wins.** Every line
is checked against the live variant, in `variant_id` order — a stable order,
because two concurrent checkouts sharing variants that locked rows in opposite
orders would deadlock. Three reasons refuse a line:

- `inactive` — the variant is no longer sellable
- `price_changed` — `cart_line_items.unit_price_minor` differs from
  `variants.price_minor`; the conflict carries `current_price_minor`
- `insufficient_stock` — `reserveStock` could not take the units; the conflict
  carries `available` and `requested`

One conflicted line refuses the whole checkout with 409 and a `details` array of
`LineConflict`. The transaction rolls back, so no partial reservation survives,
and `refreshCartPrices` then re-snapshots the cart to current prices.

Core never silently reprices a confirmed order. A shopper who saw $25 is not
charged $27 because the catalog moved while they typed an address; they are
shown the new price and asked again. That is also why the conflict returns the
current values rather than just a reason — the storefront can redisplay the cart
without a second round trip.

**Money is `*_minor` integers plus a currency.** `subtotal_minor`,
`shipping_minor`, `discount_minor`, `total_minor`, and the API returns
`{"amount_minor": 2500, "currency": "USD"}`. The total is
`subtotal + Config.FlatShippingMinor`; discount is always 0 at checkout.

## How to check out over HTTP

```http
POST /api/checkout/cod
Content-Type: application/json
Idempotency-Key: 0f9c2b1e-4d1a-4a2f-9c33-8e1b6a5d0f21

{"cart_id":"kQ8x…","email":"shopper@example.com","name":"A Shopper",
 "address":{"line1":"1 Main St","city":"Austin","postal_code":"78701","country":"US"}}
```

```json
{"data":{"order":{"id":42,"number":"GC-000042","status":"confirmed",
  "payment_status":"pending","payment_provider":"cod",
  "total":{"amount_minor":2500,"currency":"USD"},
  "access_token":"…","line_items":[…]},
  "payment":{"kind":"none","provider":"cod"}}}
```

201, and `access_token` appears **once** — it is the guest's only handle on the
order afterwards (`GET /api/orders/GC-000042?token=…`). Persist it client-side
at this moment or it is gone.

`payment.kind` decides what the client does: `none` (nothing — the order is
already confirmed and its stock committed), `client_action` (finish in the page
with `client_data`, e.g. Stripe's `client_secret`), or `redirect`.

## How to handle a 409 conflict

```json
{"error":{"code":"conflict","message":"the cart is no longer valid at these prices",
  "details":[{"variant_id":42,"sku":"TEE-M-BLK","reason":"price_changed",
              "current_price_minor":2499}]}}
```

Re-`GET /api/carts/{cartId}` — it has already been refreshed to current prices —
show the differences, and retry with a **new** `Idempotency-Key`. Reusing the
old one with a changed body earns a 400, deliberately.

## How to test a checkout

```go
app := gctest.New(t)
gctest.CreateProduct(t, app, "TEE-001", 2500, 10)
result := gctest.PlaceOrder(t, app, gocommerce.CodeCOD)
gctest.DrainOutbox(t, app)
gctest.AssertOutboxEmpty(t, app)
```

Postgres is required (AGENTS.md rule 13): the correctness here lives in
`FOR UPDATE`, the conditional `UPDATE variant_stock … WHERE on_hand -
reserved >= $3`, and `UNIQUE (scope, key)`. No fake reproduces any of it.

## Common mistakes

- **Calling a gateway inside `InTx`.** Every external call belongs after the
  commit, with an idempotency key as the retry path.
- **Treating a phase-B 500 as "nothing happened".** The order exists. Retry
  with the same key.
- **Generating a new `Idempotency-Key` per network retry.** The key identifies
  the shopper's *intent*, not the TCP attempt; a fresh key per retry is how you
  get two orders.
- **Repricing an order to "help".** Refuse with 409 and let the shopper decide.
- **Reserving lines in map or insertion order.** Sort by `variant_id`, or two
  concurrent checkouts will deadlock in production and never in your test.
- **Assuming a conflict means an empty cart.** The cart is still `open` and
  still owned by the shopper; only its snapshot prices moved.

Related: [carts](carts.md), [orders](orders.md), [payments](payments.md),
[events](events.md).

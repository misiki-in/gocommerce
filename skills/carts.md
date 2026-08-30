---
name: carts
description: Use when opening a guest cart, adding or changing line items, or explaining why a cart's prices are snapshots that checkout refuses to override.
---

# Carts

## The model

A cart is a guest's basket, and its token is the only credential involved. No
column here references a customer, and none ever will: guest checkout is a
permanent guarantee (D22), not a stage the project grows out of. Possessing the
token *is* the authorisation, which is why `token()` mints an unguessable one.

The JSON `id` of a cart is that token — `Cart.ID` (the row id) is tagged
`json:"-"` and never leaves the process. Everything else on the wire is
`status` (`open` / `converted` / `abandoned`), `currency`, `email`,
`line_items`, `item_count`, `subtotal`, `metadata` and the three timestamps.

`cart_line_items` is `(cart_id, variant_id)`-unique and stores `quantity` plus
`unit_price_minor` — the price as it was when the line was added. Reading a cart
joins the live variant, so each line reports both coordinates:

```
unit_price / total        the snapshot, and quantity × snapshot
current_price             what the variant costs right now
price_changed             the two disagree; checkout will refuse
available                 on_hand − reserved, or -1 when not tracked
in_stock                  variant is active and can cover this quantity
```

A storefront that renders `price_changed` and `in_stock` warns the shopper on
the cart page instead of surprising them at the end of checkout.

## Invariants

- **Prices are snapshotted at add-time, and [checkout](checkout.md) refuses to
  silently reprice.** If a line's `unit_price_minor` no longer matches the
  variant's `price_minor`, the whole checkout is rejected with `409` and
  per-line detail (`{"variant_id", "sku", "reason": "price_changed",
  "current_price_minor"}`). A shopper agrees to a total, not to a moving one —
  and the alternative, charging whatever the catalog says at the moment the
  button was pressed, is how a store quietly overcharges people. After the
  refusal, `refreshCartPrices` re-snapshots the cart to current prices so the
  next attempt succeeds against numbers the shopper can now see.
- **`cart_line_items.variant_id` is `ON DELETE CASCADE`; `order_lines.variant_id`
  is `ON DELETE SET NULL`.** The asymmetry is deliberate. A cart line for a
  deleted variant cannot be bought, so it should disappear with it — and
  `RESTRICT` would let one abandoned guest cart block an operator from ever
  removing a product. An order line is the opposite case: it is an immutable
  snapshot of a sale, carrying its own sku, title, label, quantity and price, and
  it must stay readable for accounting and support long after the catalog has
  moved on. So it loses the reference and keeps the record.
- **Only an `open` cart can be modified.** `openCartID` selects
  `FOR UPDATE` and rejects any other status with `409 — "this cart has already
  been checked out"`. The lock is what stops two tabs adding the last unit at
  once.
- **Money is minor units plus a currency code.** `subtotal`, `unit_price`,
  `total` and `current_price` are all `{"amount_minor": 2499, "currency":
  "USD"}`. The cart's currency comes from `Config.Currency`.
- **A cart expires.** `expires_at` is pushed forward by `touchCart` on every
  mutation (`Config.CartTTL`, 720h by default), and `SweepExpired` deletes open
  carts past it. `POST /api/carts` is a public row-creating endpoint; without a
  sweeper the table grows for exactly as long as the store is popular.

## How to open a cart and add lines

```go
cart, err := app.Cart().Create(ctx, "" /* or the shopper's email */)
cart, err = app.Cart().AddLine(ctx, cart.Token, variantID, 2)
```

```http
POST /api/carts                       → 201 {"data": {"id": "<token>", …}}
POST /api/carts/<token>/line-items    {"variant_id": 77, "quantity": 2}   → 200, the whole cart
```

The create body is optional — shopping starts before a shopper has told you
anything about themselves — and `quantity` defaults to 1 when omitted or zero.
Every line-item route returns the full refreshed cart, so a client never has to
stitch a response into local state.

`AddLine` on a variant already in the cart sums the quantities, and checks
availability against `existing + qty` before doing so:
`409 — "only %d left in stock"`. An inactive variant is
`409 — "that variant is not available"`.

## How to change or remove a line

```go
cart, err := app.Cart().UpdateLine(ctx, token, lineID, 3)
cart, err := app.Cart().RemoveLine(ctx, token, lineID)
```

```http
PATCH  /api/carts/<token>/line-items/12   {"quantity": 3}   → 200
PATCH  /api/carts/<token>/line-items/12   {"quantity": 0}   → 200, line removed
DELETE /api/carts/<token>/line-items/12                     → 200
```

Quantity `0` removes the line rather than erroring, because that is what a
quantity stepper stepping down to nothing means. A line id belonging to another
cart is `404 — "line item %d is not in this cart"`, never someone else's data.

## How to handle a price change before checkout

Read the cart and act on the flags rather than waiting for the 409:

```go
cart, err := app.Cart().GetByToken(ctx, token)
for _, l := range cart.Lines {
    if l.PriceChanged || !l.InStock {
        // show the line, its CurrentPrice and Available, and ask for confirmation
    }
}
```

If checkout has already refused, the cart has been re-snapshotted for you:
show the new totals, get confirmation, and post the same checkout again.

## Common mistakes

- **Treating the cart's `id` as a row id.** It is the token — an opaque string,
  and the only credential. Do not log it, and do not put it in a URL a third
  party will see in a `Referer` header.
- **Expecting an admin route for carts.** There is none. `mountCartRoutes`
  serves five public routes, all keyed by the token; an operator has no way to
  browse other people's baskets, which is the point.
- **Looking for an endpoint that sets the email on an existing cart.**
  `Carts.SetEmail` exists as a service method but is not mounted; over HTTP the
  email is supplied at `POST /api/carts` or at checkout.
- **Expecting `AddLine` to reprice the line.** It does not, deliberately: the
  `ON CONFLICT` clause adds to `quantity` and leaves `unit_price_minor` at the
  value the first add captured. It used to overwrite it, which quietly moved
  units already in the basket to today's price and erased the evidence
  [checkout](checkout.md) needs to notice a change at all. `UpdateLine` takes
  an absolute quantity and is the clearer call when you mean "make it three".
- **Assuming `GetByToken` enforces expiry.** It does not; an expired cart still
  reads until `SweepExpired` removes it. Check `expires_at` if that matters to
  the surface you are building.
- **Rendering `available: -1` as a quantity.** It means the variant does not
  track inventory — see [inventory](inventory.md).

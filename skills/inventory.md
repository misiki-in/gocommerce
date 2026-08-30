---
name: inventory
description: Use when receiving stock, running a stock take, moving stock between locations, chasing a low-stock report, or reasoning about how a reservation becomes a sale.
---

# Inventory

## The model

Stock belongs to a [variant](variants.md) *at a place*. The variant is the
sellable SKU; the place is where the units physically are. Since M17 the truth
is one `variant_stock` row per pair:

```
variant_stock (variant_id, location_id)
    on_hand    physically on that shelf, including units promised to open orders
    reserved   promised to orders in flight but not yet taken off that shelf
```

The variant still reports one number for each, and they mean exactly what they
always did — the store has this many:

```
stock_on_hand    = sum(on_hand)  over the variant's locations
stock_reserved   = sum(reserved) over the variant's locations
available        = stock_on_hand - stock_reserved   (derived, never stored)
```

A store with one location never has to think about any of this. Every store gets
a `default` location when its schema is created, opening stock lands there, and
every stock call takes `0` to mean it.

Four movements, all in `inventory.go`, all single UPDATE statements so the check
and the write happen under the same row lock, and all against one location:

| movement | effect | when |
| --- | --- | --- |
| `reserveStock` | `reserved += qty` | [checkout](checkout.md) creates the order |
| `commitStock` | `reserved -= qty`, `on_hand -= qty` | payment confirms the order |
| `releaseStock` | `reserved -= qty` | a pending order is cancelled or swept |
| `restockStock` | `on_hand += qty` | a *confirmed* order is cancelled |

Which of the last two is correct depends on how far the order got: a pending
order only ever reserved stock, while a confirmed one has already taken it off
the shelf. `Orders.Cancel` picks with `stockCommitted(o.Status)` — getting that
choice wrong double-counts inventory in one direction or the other.

Which *place* they act on is not a fresh decision. `pickLocation` chooses once,
at checkout, and the answer is written to `order_lines.location_id`; everything
afterwards reads it back through `lineLocation`. That is what makes a
cancellation put the units back on the shelf they left rather than on whichever
shelf is default that week.

The public service is `app.Stock()`, returning `*Inventory`, with `Adjust`,
`SetOnHand`, `Move`, `ByLocation` and `LowStock`. Places are `app.Places()`,
returning `*Locations`. The four movement functions above are unexported: they
only ever run inside the order transaction that justifies them.

## Invariants

- **The reserved-within-on-hand invariant is now the service's, not a CHECK's.**
  It used to be `variants_reserved_within_on_hand`. That constraint was
  conditioned on `continue_selling`, which lives on the variant and not on a
  stock row, and copying the flag onto every row to keep the CHECK would have
  been a stored duplicate — the same mistake this codebase refuses for category
  paths. What enforces it now is `reserveStock`'s conditional UPDATE, which was
  always doing the real work, plus a `doctor` check that counts any row where it
  has drifted. `variant_stock.reserved >= 0` is still a CHECK, because that one
  needs nothing from the variant.
- **Reservation is one statement, never read-then-write.** `reserveStock`'s
  `WHERE` carries `vs.on_hand - vs.reserved >= $3`, so two concurrent checkouts
  for the last unit cannot both pass: the second re-evaluates the condition after
  the first commits and matches zero rows, which becomes `errInsufficientStock`.
  A `SELECT` followed by an `UPDATE` would sell the same unit twice under load
  and pass every test that runs serially.
- **A line comes off one shelf.** `pickLocation` takes the first active location,
  in priority order, that can cover the whole quantity. It does not split a line
  across two places: doing so would promise a shopper one parcel and hand the
  warehouse two picks, which is a bigger commitment than this engine makes.
- **The floor is per location, not per store.** Five spare units in the warehouse
  do not entitle the shop to go below what is reserved there, because the order
  being picked from the shop cannot be filled from the warehouse.
- **A location holding stock cannot be closed.** Its units would still be counted
  in `stock_on_hand` while `pickLocation` skipped it, so the store would believe
  it could sell something it could not reach. `Locations.Update` and
  `Locations.Delete` both refuse, and the message names how many units to move.
- **`track_inventory = false` means unlimited, not zero.** Every movement is
  wrapped in `CASE WHEN track_inventory THEN $2 ELSE 0 END`, and `reserveStock`
  succeeds for such a variant while reserving nothing. That is how digital goods
  and made-to-order items work without a second code path.
- **Stock never moves through a patch.** `VariantPatch` has no stock field.
  "Set the stock to 7" is not a safe operation when another request may have
  sold one in between, so the API offers a delta (`adjust`) and a stock take
  (`set`) — both evaluated against the current row, not against what the caller
  last read.
- **A reservation has a deadline.** Checkout stamps
  `orders.reservation_expires_at` at `now() + Config.OrderTTL` (24h default);
  `Orders.SweepUnpaid` cancels what is past it and releases the stock. Without
  it an abandoned payment takes units out of sale forever — invisible until the
  day it sells out something that is actually on the shelf. `gocommerce doctor`
  reports stale reservations for exactly this reason.

## How to receive stock or run a stock take

`Adjust` moves the count by a delta; `SetOnHand` replaces it. Both return the
refreshed variant:

```go
v, err := app.Stock().Adjust(ctx, variantID, 0, 25)    // a delivery arrived
v, err := app.Stock().SetOnHand(ctx, variantID, 0, 18) // a shelf was counted
```

The third argument is the location. `0` means the default one, which is what a
single-location store always passes; a real id says which shelf.

Both refuse to drop that location's on-hand below what is reserved there, and
`explainStockFailure` turns the zero-row update into
`409 — "stock cannot go below the N unit(s) already reserved for open orders"`.
A location id that does not exist is a 404 rather than a silent fallback to the
default: putting stock somewhere the caller did not name is worse than telling
them they were wrong.

```http
POST /api/admin/variants/77/inventory
Authorization: Bearer <admin token>

{"adjust": 25}                       → 200, the variant
{"set": 18, "location_id": 3}        → 200, the variant
{"adjust": 5, "set": 18}             → 400, "send either adjust or set, not both"
```

Both keys omitted is also a 400. Receiving stock and counting a shelf are
different acts, and the endpoint makes you say which one you performed.

## How to work with more than one location

```go
places, err := app.Places().List(ctx)
rows, err := app.Stock().ByLocation(ctx, variantID)       // where it actually is
v, err := app.Stock().Move(ctx, variantID, fromID, toID, 10)
```

```http
GET  /api/admin/locations
POST /api/admin/locations              {"code": "shop", "name": "The shop", "priority": 1}
POST /api/admin/locations/3/default
GET  /api/admin/variants/77/stock
POST /api/admin/variants/77/stock/transfer
     {"from_location_id": 1, "to_location_id": 3, "quantity": 10}
```

`Move` is one statement out and one in, in a single transaction, so the store's
total never changes even for an instant. Reserved units do not travel: they are
promised to orders that will be picked from where they are, and moving them
would send a picker to the wrong shelf.

`priority` orders the search — lower is tried first. `from_location_id` has no
default, because emptying a shelf nobody named is not something anyone means to
do.

## How stock travels through a CSV

The product export carries one stock column per location, named by code:

```
product_slug,...,stock_on_hand:default,stock_on_hand:shop,...
tee,...,4,3,...
```

A store with one location gets the plain `stock_on_hand` column instead, exactly
as it did before locations existed — which is what keeps every file already
sitting in somebody's folder importing unchanged. On import the bare column
means the default location.

Three rules make the format safe to hand-edit:

- **An absent column says nothing about that location.** A file naming two of
  five locations leaves the other three alone. So does a blank cell: an empty
  cell is not a zero.
- **A misspelt code fails the whole file**, naming the code. It is one mistake in
  one header cell affecting every line, so it is worth one sentence rather than a
  row error per line burying it.
- **A count cannot drop below what is reserved there.** The import writes
  `greatest(count, reserved)`, because a count taken on the shop floor does not
  know about the order that arrived while it was being taken.

Mixing `stock_on_hand` and `stock_on_hand:<code>` in one file is refused: there
is no way to tell which one a row means.

## How to find what is running out

```go
variants, total, err := app.Stock().LowStock(ctx, 5 /*threshold*/, 50, 0)
```

```http
GET /api/admin/inventory/low-stock?threshold=3&limit=20&page=2
```

The threshold defaults to 5 and compares against *available*, not on-hand, so
units already promised to open orders count as gone. Only variants with
`track_inventory` are considered — an unlimited variant is never low. Results
are ordered by availability ascending, so the most urgent row is first.

Pagination is the engine's standard contract: `limit` with either `offset` or
`page`, and **`page` wins when both are sent**. The `meta` block carries
`total`, `limit`, `offset`, `page` and `total_pages`.

## How to make an item that never runs out

Set `track_inventory` to false on the variant; the quantity columns are then
ignored by every movement.

```go
tracks := false
v, err := app.Products().UpdateVariant(ctx, variantID,
    gocommerce.VariantPatch{TrackInventory: &tracks})
```

Cart lines for such a variant report `"available": -1`, which is the wire
signal for "not tracked" — a storefront must not render it as a quantity.

## Common mistakes

- **`UPDATE variant_stock SET on_hand = …` from a module or a script.** It skips
  the reserved-quantity check, the row lock and the event. Rule 3 in
  [`AGENTS.md`](../AGENTS.md): reading with SQL is fine, writing is not. Use
  `app.Stock()`. (`variants.stock_on_hand` is not a column at all any more — M17
  dropped it. A query naming it fails loudly, which is the right outcome.)
- **Treating `stock_on_hand` on a variant as a column.** It is a sum across
  locations, computed on the way out. Filtering or ordering on it in your own SQL
  means repeating the subquery — `variantOnHand` and `variantAvailable` in
  `catalog.go` are there to be reused so the two cannot drift.
- **Restocking to the default instead of to where the units came from.** The
  order line records its location for exactly this reason. `lineLocation` reads
  it back; a movement that hardcodes the default silently teleports stock between
  shelves.
- **Reporting `stock_on_hand` as "in stock" in a storefront.** On-hand includes
  units already promised. The sellable number is `available`, and
  `Variant.InStock(qty)` is the predicate that also handles the untracked case.
- **Adding stock by calling `SetOnHand` with what you think the new total is.**
  Between your read and your write, a sale may have committed. `Adjust` with the
  delta you actually received is the operation that survives concurrency.
- **Restocking a cancelled *pending* order.** It never left the shelf; only the
  reservation needs releasing. `Orders.Cancel` already picks correctly — do not
  "help" it with a manual adjustment afterwards.
- **Assuming a failed checkout leaves stock reserved.** The reservation is made
  inside the order transaction; a conflict rolls the whole thing back. Only a
  *created but unpaid* order holds stock, and the sweeper is what eventually
  frees it.

---
name: products
description: Use when creating, reading, updating or deleting products and their option axes — the merchandising layer that sits above the sellable variant.
---

# Products

## The model

A product is what a shopper browses. What they buy is a [variant](variants.md),
and every downstream reference — cart lines, order lines, stock — points at a
variant, never at a product. `products` therefore carries only merchandising:
slug, title, description, status, currency, metadata.

Statuses are CHECK-constrained to `draft`, `active`, `archived`. Only `active`
is reachable on the public routes, and `respondPublicProduct` answers 404 for
the other two rather than 403 — whether an unreleased product exists is not a
shopper's business.

`products.currency` is written from `Config.Currency`; `ProductInput` has no
currency field at all. One store, one settlement currency.

The service is `app.Products()`, which returns `*Catalog` — the same service
owns variants, because a product and its variants are created and read
together.

## Invariants

- **`slug` is unique store-wide.** Omit it and `normalizeInput` derives one with
  `slugify(title)`; if that yields nothing you get a validation error rather
  than a row with an empty slug, because the slug is a public URL. A collision
  is `409` — `translateCatalogErr` recognises `products_slug_key` and rewrites
  it into the API's vocabulary, so a client learns what it did wrong instead of
  reading a driver message.
- **A product always has at least one variant.** `CreateProduct` rejects input
  carrying neither `variants` nor the `sku` + `price_minor` shorthand, and
  `DeleteVariant` refuses to remove the last one. Nothing downstream is written
  to cope with a product that has nothing sellable.
- **A product with no options has exactly one variant.** Enforced twice on
  purpose: `normalizeInput` rejects more than one variant when `options` is
  empty, and the unique index `variants_product_option_key_idx` makes a second
  `option_key = ''` row on the same product impossible even if the service is
  bypassed. See [variants](variants.md) for why the key is the load-bearing part.
- **An option value must be unique across the whole product, not just its axis.**
  `product_option_values` has `UNIQUE (option_id, value)`, and `insertOptions`
  additionally rejects a value that appears on two axes in the same call. A
  variant names its selection by value (`["M", "Black"]`), so "Small" as both a
  size and a cup size would make that list ambiguous.
- **The product holds no price.** Money lives on the variant as `price_minor`,
  and the API returns `{"amount_minor": 1499, "currency": "USD"}` — never a
  formatted string, because decimal places belong to the currency and the symbol
  belongs to the reader's locale.
- **A nil patch field is left alone.** Every field of `ProductPatch` is a
  pointer, which is what separates "not mentioned" from "set to empty".
  `DecodeJSON` also rejects unknown fields, so a misspelled key is a 400 instead
  of a silently ignored edit.

## How to create a product

The single-variant case is the shorthand; the engine creates the default variant
so the simple case never becomes a special case:

```go
price := int64(1499)
stock := 12
p, err := app.Products().CreateProduct(ctx, gocommerce.ProductInput{
    Title:      "Enamel Mug",
    Status:     gocommerce.ProductActive,
    SKU:        "MUG-001",
    PriceMinor: &price,
    Stock:      &stock,
})
```

`Options` and `Variants` may be supplied in the same call, and all of it commits
in one transaction — an import creates a coherent product or none at all. See
[variants](variants.md) for that shape.

Over HTTP:

```http
POST /api/admin/products
Authorization: Bearer <admin token>
Content-Type: application/json

{"title": "Enamel Mug", "status": "active", "sku": "MUG-001", "price_minor": 1499, "stock": 12}
```

→ `201` with `{"data": {…product with its options and variants…}}`.

## How to list, search and page

```go
products, total, err := app.Products().ListProducts(ctx, gocommerce.ProductQuery{
    Search: "mug",
    Status: gocommerce.ProductActive,
    Limit:  20,
    Offset: 0,
})
```

`Search` is a case-insensitive `LIKE` over title and description. A `Limit` of
zero becomes `DefaultLimit` (50); the HTTP layer caps it at `MaxLimit` (200).
The public listing is forced to `active`; the admin listing accepts `?status=`:

```http
GET /api/products?q=mug&limit=20&page=2
GET /api/admin/products?status=draft&limit=20&offset=20
```

Both pagination coordinates work everywhere `Page(r)` is used, and **`page` wins
when a request sends both** — it is the more specific intent, and honouring the
offset instead would quietly serve a different window than the one asked for.
`meta` reports `total`, `limit`, `offset`, `page` and `total_pages`, the page
fields derived from the offset so the two can never disagree.

Direct lookups are `GetProduct(ctx, id)` and `GetProductBySlug(ctx, slug)`, or
`GET /api/products/{id}`, `/api/products/slug/{slug}` and
`/api/products/sku/{sku}` — the SKU route resolves a variant's SKU back to its
product, since SKU is already the catalog's stable key for import and export.

## Categories, and how they differ from collections

A **category** is where a product sits in a taxonomy: Apparel / Clothing /
Shirts. One product, one category, one parent per category. A **collection** is
a list somebody curated, and a product can be on six of them. The difference is
load-bearing rather than stylistic: a marketplace feed, a tax rule and a
shipping profile all need to ask "what kind of thing is this", and that question
has exactly one answer. A product filed under both Shirts and Trousers is a data
error, not a merchandising decision.

Hence `products.category_id` — a single nullable column, not a join table — and
`categories` as an adjacency list with a nullable `parent_id`. Adjacency rather
than a materialised path because the tree is small and edited by hand: a stored
path would have to be rewritten across a whole subtree on every rename, and the
first missed rewrite is a lie nothing ever finds. `full_name` and `depth` are
computed by a recursive CTE on read.

The service is `app.Categories()`.

```go
apparel, err := app.Categories().Create(ctx, gocommerce.CategoryInput{Title: "Apparel"})
shirts, err := app.Categories().Create(ctx, gocommerce.CategoryInput{
    Title: "Shirts", ParentID: &apparel.ID,
})
```

`Tree(ctx)` returns the roots with their children nested; `List(ctx)` returns
the same tree flattened depth-first, with `Children` omitted so the same rows
never appear twice in one response. Both return the whole table — a taxonomy is
tens of rows, a picker needs all of them to draw a tree, and a page of a tree is
a forest of stumps.

```http
GET /api/categories             → nested
GET /api/categories?flat=1      → depth-first, each row carrying full_name and depth
GET /api/categories/{slug}      → the category with a page of the active products under it
```

Admin CRUD is `GET|POST /api/admin/categories` and
`GET|PATCH|DELETE /api/admin/categories/{id}`.

### Invariants

- **The tree stays a tree.** `Update` refuses a move that would put a category
  inside itself or one of its own descendants, walking up from the proposed
  parent first; a one-node cycle is also caught by the
  `categories_not_own_parent` CHECK. A cycle would make every recursive query
  walk forever.
- **Nesting is capped at `MaxCategoryDepth` (8).** Checked on create and on
  move, and a move counts the height of the subtree being moved, not just the
  node. The limit is what stops a bad import from building a chain every
  recursive query then has to walk.
- **Delete is refused while anything points at the category**, with a count of
  what is in the way — subcategories, then products. Both foreign keys are
  `ON DELETE RESTRICT`. Cascading would be wrong in both directions: deleting
  "Apparel" would silently take every subcategory, and uncategorising forty
  products is a change nobody asked for and nobody would see.
- **Filtering by a category includes its descendants.** `ProductQuery.CategoryID`
  and `?category_id=` expand through a recursive CTE, because someone who picks
  "Apparel" means the shirts too — an exact-node filter shows an empty page for
  every branch.
- **`ProductPatch.CategoryID` is a `NullableID`, not a `*int64`.** An omitted
  field and an explicit `null` both decode to a nil pointer, so a plain pointer
  could file a product but never un-file it. `SetID(id)` files it, `ClearID()`
  uncategorises it, and the zero value leaves it alone.

## How to change status or delete

```go
status := gocommerce.ProductActive
p, err := app.Products().UpdateProduct(ctx, id, gocommerce.ProductPatch{Status: &status})
```

```http
PATCH /api/admin/products/42     {"status": "archived"}
DELETE /api/admin/products/42    → 204
```

Deleting cascades to options, option values and variants. Order lines survive:
their `product_id` is `ON DELETE SET NULL`, and they keep their own snapshot of
sku, title, label and price, so a historical order stays readable after the
catalog moves on. See [carts](carts.md) for why cart lines cascade instead.

## Common mistakes

- **Trying to set stock through `UpdateProduct` or `UpdateVariant`.** Stock is
  absent from both patches by design; it moves through
  [inventory](inventory.md) so every change is a delta under a row lock rather
  than an overwrite of a number another request just changed.
- **Expecting a per-product currency.** There is none. Change `Config.Currency`
  and every price re-renders in the new code — the stored minor units do not
  move, which is exactly why a currency switch is a business decision, not a
  migration.
- **Assuming a draft product 403s.** It 404s. A test asserting 403 is asserting
  an information leak.
- **Sending an unknown field in a patch.** `DecodeJSON` uses
  `DisallowUnknownFields`; `{"name": "..."}` on a product is a 400, not a
  no-op. The field is `title`.
- **Writing `products` or `variants` with SQL from a module.** Rule 3 in
  [`AGENTS.md`](../AGENTS.md): core owns the state machine and the event that
  describes each transition, and the two commit together. Go through
  `app.Products()`.

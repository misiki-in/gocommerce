---
name: variants
description: Use when adding option axes, creating or editing variants, or debugging a "variant with that combination of options already exists" conflict.
---

# Variants

## The model

The variant is the sellable unit. It carries the SKU, the price, the stock and
the weight, and it is what `cart_line_items` and `order_lines` reference. A
[product](products.md) is merchandising; a variant is the thing that is bought.

Three tables describe the choices: `product_options` (one axis, `Size`, unique by
`(product_id, name)`), `product_option_values` (one choice, `M`, unique by
`(option_id, value)`) and `variant_option_values` (what a variant selected). The
last references the value `ON DELETE RESTRICT`, so a value a variant depends on
cannot vanish underneath it.

The Go shape is `Variant`: `price` and `compare_at_price` as `Money`, `options`
(`["M","Black"]`), a display `label` (`"M / Black"`, joined in option position
order), `stock_on_hand`, `stock_reserved` and the derived `available`. The
two counts are sums across the variant's [locations](inventory.md);
`GET /api/admin/variants/{id}/stock` is the breakdown.
`Variant.InStock(qty)` is the sellability test, always true for a variant that
does not track inventory.

## Invariants

- **`variants.option_key` is what makes uniqueness a database guarantee.** The
  key is the variant's selected `option_value_id`s, sorted ascending and joined
  with commas (`optionKey` in `catalog.go`). Sorting is the point: it makes
  `{Black, M}` and `{M, Black}` the same string, so
  `CREATE UNIQUE INDEX variants_product_option_key_idx ON variants (product_id, option_key)`
  turns "no two variants may be Colour=Black + Size=M" into something PostgreSQL
  refuses, rather than something the service hopes it checked. Two concurrent
  requests creating the same combination cannot both win; the loser gets
  `409 conflict — "a variant with that combination of options already exists"`,
  translated from the index name by `translateCatalogErr`.
- **A product with no options has one variant whose key is `''`.** The same
  index then guarantees the single default variant, with no second rule and no
  special case. This is why `Product.DefaultVariant()` can just return the first
  active variant and be right.
- **`sku` is UNIQUE across the entire store**, not per product — which is what
  lets `GET /api/products/sku/{sku}` and the CSV import resolve a variant from a
  SKU alone. A duplicate is `409 — "that sku is already used by another
  variant"`.
- **Prices are `price_minor` / `compare_at_price_minor`, both CHECKed `>= 0`,**
  and reach the client as `{"amount_minor": 2499, "currency": "USD"}` — never a
  formatted string, and `*_minor` rather than `*_cents` because JPY has no
  decimals and KWD has three.
- **Stock is not patchable here.** `VariantPatch` deliberately has no stock
  field: a blind `SET stock = 7` races every concurrent sale. See
  [inventory](inventory.md).
- **A product must keep at least one variant.** `DeleteVariant` counts first and
  returns `409 — "a product must keep at least one variant"`. A product with
  nothing sellable is not a product, and every downstream path assumes one
  exists.

## How to add an axis and its variants

Options first, then the variants that select from them. `AddOption` appends at
the next free position:

```go
p, err := app.Products().AddOption(ctx, productID, gocommerce.OptionInput{
    Name:   "Size",
    Values: []string{"S", "M", "L"},
})
```

```go
v, err := app.Products().CreateVariant(ctx, productID, gocommerce.VariantInput{
    SKU:        "TEE-M-BLK",
    PriceMinor: 2499,
    Options:    []string{"M", "Black"}, // any order; matched by value, case-insensitively
})
```

`CreateVariant` refuses the two mismatches: options given for a product that has
none, and no options given for a product that has some. Over HTTP:

```http
POST /api/admin/products/42/options
{"name": "Size", "values": ["S", "M", "L"]}          → 201, the whole product

POST /api/admin/products/42/variants
{"sku": "TEE-M-BLK", "price_minor": 2499, "options": ["M", "Black"], "stock_on_hand": 20}
                                                      → 201, the variant
```

## How to change a variant

Every field is a pointer; nil means "leave it alone".

```go
price := int64(2199)
active := false
v, err := app.Products().UpdateVariant(ctx, variantID, gocommerce.VariantPatch{
    PriceMinor: &price,
    Active:     &active,
})
```

```http
PATCH /api/admin/variants/77    {"price_minor": 2199, "active": false}
DELETE /api/admin/variants/77   → 204
```

Note what changing `price_minor` does *not* do: carts already holding this
variant keep the price they snapshotted, and [checkout](checkout.md) refuses
rather than repricing them — intended behaviour, not a bug to fix here.

## How to look a variant up

```go
v, err := app.Products().GetVariant(ctx, id)
v, err := app.Products().GetVariantBySKU(ctx, "TEE-M-BLK")
vs, err := app.Products().ListVariants(ctx, productID)
```

```http
GET /api/variants/77
GET /api/variants?product_id=42
```

`product_id` is required on the collection route — variants are a top-level
collection rather than `/api/products/{id}/variants`, because that path would
collide with `/api/products/slug/{slug}` and `/api/products/sku/{sku}`, and
neither pattern is more specific than the other.

## Common mistakes

- **Reordering the options list and expecting a different variant.** `["Black",
  "M"]` and `["M", "Black"]` produce the same `option_key` and therefore the
  same conflict. That is the feature.
- **Assuming a variant must choose a value on every axis.** `resolveOptionValues`
  resolves whatever list it is handed; a variant selecting one of two axes gets
  a shorter — and distinct — `option_key`, and the database accepts it. If a
  full selection matters to your store, validate it before calling
  `CreateVariant`.
- **Adding an axis to a product that already has a default variant.** The
  existing variant keeps `option_key = ''` and no option values, while every new
  variant is now required to select values. Fix it deliberately: create the real
  variants, then delete the bare default (it is no longer the last one).
- **Adding an option value that already exists on another axis.** The
  cross-axis duplicate check in `insertOptions` only spans values inserted in
  the *same* call, so `AddOption` cannot see the axes already on the product.
  `optionValueLookup` keys by lowercased value, so a duplicate silently resolves
  to one of them. Keep value names distinct across a product's axes.
- **Treating `available` as a column.** It is `stock_on_hand - stock_reserved`,
  computed in `selectVariants` and in SQL where it is filtered on. There is
  nothing to update.
- **Deleting a variant to "hide" it.** Set `active: false`. Deleting also
  removes it from every cart that holds it (`ON DELETE CASCADE`), while order
  lines merely null their reference — see [carts](carts.md).

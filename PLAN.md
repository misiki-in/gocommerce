# GoCommerce — Product, Architecture & Execution Plan

> **Revision R3 — 2026-08-27**
>
> This revision changes the project from a "minimal SQLite ecommerce platform"
> into a **small, composable, production-grade commerce engine for Go**.
>
> The two architectural changes that drive R3 are deliberate:
>
> 1. **PostgreSQL is the production database.** SQLite is no longer the
>    architectural spine. Simplicity comes from the application model and
>    composition model, not from choosing an embedded database at the cost of
>    transactional headroom.
> 2. **Events use a transactional outbox.** A committed order/payment change
>    must not be able to disappear because the process crashed between the DB
>    commit and event publication.
>
> Product variants are also moved into the core model from day one. Variants
> are not an optional module because they affect carts, stock, prices, order
> snapshots, imports, and product identity.
>
> **R3.1 amendments (2026-08-28)** — four decisions taken after R3 was drafted:
>
> 1. **One Go module, no monorepo** (§22). Extensions with no third-party
>    dependency are packages under `ext/`; only dependency-carrying extensions
>    become satellite repos.
> 2. **Guest checkout is a permanent guarantee** (D22), not a v1 simplification.
> 3. **Default currency is `USD`, and currency is extensible** (D14) — money
>    columns are named `*_minor`, never `*_cents`, because JPY has 0 decimal
>    places and KWD has 3.
> 4. **Default language is `en`, and language is extensible** (D21) — core
>    negotiates a request language and ships a `Translator` seam; translations
>    themselves live in a module.
>
> The prior SQLite-based plan is archived at
> `docs/PLAN-r2-sqlite-archived.md` — useful for its operational detail
> (Windows notes, CSV specifics, route inventory), superseded on architecture.

---

## 0. The product thesis

### 0.1 What GoCommerce is

**GoCommerce is a small, composable commerce engine for Go.**

It provides the business primitives that every store eventually needs:

```text
Products / Variants
        ↓
      Cart
        ↓
    Checkout
        ↓
      Order
        ↓
    Payment
        ↓
  Fulfillment
        ↓
 Events / Automation
```

Everything around those primitives is replaceable:

```text
                 ┌──────── payments-stripe
                 ├──────── payments-razorpay
                 ├──────── fulfill-shiprocket
                 ├──────── notify-sendgrid
                 ├──────── notify-msg91
                 ├──────── search-meilisearch
                 ├──────── storage-s3
                 ├──────── invoices
                 ├──────── cms
                 └──────── mcp
                         │
                         ▼
                 ┌───────────────┐
                 │  gocommerce   │
                 │  Go + Postgres│
                 └───────────────┘
```

### 0.2 What it is NOT

GoCommerce is deliberately not:

- a Shopify clone;
- a hosted SaaS store builder;
- a CMS-first platform;
- a giant plugin framework;
- a low-code admin application;
- a replacement for every commerce concern on day one.

The operator is a developer. A store is a Go program that composes the engine
with the capabilities it needs.

### 0.3 The core promise

> **Own your commerce stack. Start small. Add capabilities without replacing the engine.**

A useful mental model is:

```go
app, err := gocommerce.New(
    gocommerce.Config{DBURL: os.Getenv("DATABASE_URL")},
    stripe.New(...),
    shiprocket.New(...),
    sendgrid.New(...),
)
```

The application author can inspect the complete composition from `main()`.
There is no runtime plugin registry, dependency injection container, reflection
magic, or configuration DSL.

### 0.4 The real differentiation

The differentiation is not "Go ecommerce" by itself.

The differentiation is the combination of:

1. **Commerce primitives that are complete enough for real stores.**
2. **Compile-time composition using ordinary Go packages.**
3. **PostgreSQL-backed transactional correctness.**
4. **Durable events via a transactional outbox.**
5. **First-class product variants instead of a flat toy catalog.**
6. **OpenAPI and MCP so the store is both application-friendly and agent-ready.**
7. **Litekart-compatible API shapes where compatibility provides immediate value.**

The product should be presented as a **commerce engine**, not merely an
"ecommerce framework."

---

## 1. Who should use it

### Primary customer

A Go developer or small engineering team building:

- a single-brand ecommerce site;
- a B2B ordering portal;
- a manufacturer storefront;
- a niche marketplace foundation;
- a headless commerce backend;
- an internal commerce or ordering system;
- a custom storefront where Shopify/WooCommerce becomes restrictive.

### Secondary customer

Teams already running a Go application that need commerce primitives without
adopting a large platform.

### Especially strong use case

A company wants:

```text
Svelte / React / native app
          │
          ▼
   gocommerce API
          │
     PostgreSQL
          │
    provider modules
```

The frontend is free to be replaced. The commerce state remains owned by the
engine.

### Explicit non-target

A non-technical merchant who wants to sign up, choose a theme, add products,
and never touch code should use a hosted storefront product instead.

GoCommerce can support an admin UI later, but the engine does not depend on
one.

---

## 2. Why PostgreSQL wins in R3

The previous design made SQLite the architectural center in order to maximize
single-binary simplicity. That was elegant, but it made several future needs
artificially expensive: stronger concurrency, durable event storage, richer
queries, variants at scale, and horizontal application scaling.

R3 therefore chooses:

> **PostgreSQL for production.**

The application remains simple because Postgres is treated as infrastructure,
not because the infrastructure is removed.

### 2.1 Benefits we gain immediately

- concurrent writes without a single application-level writer bottleneck;
- robust transaction semantics;
- row-level locking for stock operations;
- transactional outbox in the same database;
- reliable unique constraints for idempotency;
- JSONB for extensible metadata where appropriate;
- stronger indexing/query options for variants and orders;
- natural path to read replicas and multiple application instances;
- straightforward cloud/self-hosted deployment.

### 2.2 What we give up

- a completely dependency-free local database;
- the "copy one `.db` file" operational story;
- the ability to truthfully describe the engine as embedded.

That trade is intentional.

The new simplicity promise is:

> **One Go binary + one PostgreSQL database.**

That is still dramatically simpler than operating a large commerce platform
with Redis, Kafka, Elasticsearch, multiple workers, and a framework-specific
plugin runtime.

### 2.3 SQLite policy

SQLite is **not** part of the production architecture in v1.

A future `gocommerce-lite` compatibility target may be added only after the
PostgreSQL domain model is stable. It must not shape the core APIs or constrain
transactional behavior.

Do not build a database abstraction prematurely just to support SQLite.

---

## 3. Product positioning

### One-line

> **A small, composable commerce engine for Go.**

### Developer-facing pitch

> Build commerce directly into your Go application. Products, variants, carts,
> checkout, orders, payments, fulfillment and durable events live in one small
> engine. Integrations are ordinary Go modules you wire together in `main()`.

### Stronger technical pitch

> **Commerce primitives, not a commerce monolith.**
>
> PostgreSQL-backed, API-first, event-driven, variant-aware, and designed for
> developers who want to own the stack.

### AI-era pitch

> **A commerce engine your application — and your agents — can operate.**
>
> OpenAPI exposes the HTTP contract. MCP exposes controlled commerce actions to
> AI agents. Both sit above the same domain services, so there is only one
> business state machine.

### What not to say

Avoid claims such as:

- "the fastest ecommerce engine";
- "Shopify killer";
- "zero infrastructure";
- "infinitely scalable";
- "microservices without the complexity".

Those claims are hard to prove and distract from the real advantage.

---

## 4. Strategic principles

### S1 — Keep the domain small, not the product fake

The core should contain the concepts that change together:

- product;
- variant;
- inventory;
- cart;
- checkout;
- order;
- payment state;
- fulfillment state;
- durable domain events;
- basic administration/authentication;
- API contract.

A feature does not belong in a module merely because it is optional. If moving
it out would make the core's invariants fragmented, it stays in core.

### S2 — Modules integrate external capabilities

Modules own:

- external payment APIs;
- carriers;
- notification vendors;
- search indexes;
- object storage;
- tax engines;
- CMS;
- invoicing;
- AI/MCP adapters.

They do not own core commerce state transitions.

### S3 — Correctness before abstraction

Do not create interfaces for things that are not actually replaceable.

There should be clear ports where composition matters:

The ports that exist in `ports.go` as built:

```text
PaymentProvider       Code() + Initiate()
WebhookProvider       optional: a provider that receives gateway callbacks
Refunder              optional: a provider that can refund
FulfillmentProvider   Code() + Ship()
Notifier              one channel's delivery ("email" | "sms")
Translator            optional: localized catalog copy
```

`OpenAPIContributor` is the seventh extension point and lives in `openapi.go`
rather than `ports.go`, because it extends the served contract rather than the
domain.

Search and storage ports were considered and **not built**: search is `LIKE`
against PostgreSQL until a store outgrows it, and images are URLs (§17). The
event bus is not a port either — the outbox replaced it (§11), which is why
`Config` has no `Bus` field. Do not create interfaces for things that are not
actually replaceable.

The database itself is not abstracted in v1.

### S4 — One state machine

Whether an action is initiated from:

- REST;
- a payment webhook;
- MCP;
- an admin command;
- an import;
- a future queue consumer;

it must eventually invoke the same domain service.

No integration gets to invent its own version of `MarkPaid`, `Ship`, or
`Cancel`.

### S5 — Durable events are part of correctness

Events are not "nice-to-have notifications." They are the integration seam
for a modern commerce application.

If the database says an order is paid, an event describing that fact must be
recoverable after a process crash.

### S6 — Variants are core, not future debt

Product variants influence:

- SKU;
- price;
- stock;
- cart lines;
- order lines;
- images;
- imports/exports;
- fulfillment metadata.

Designing a flat-product schema first and retrofitting variants later creates
avoidable migration and API breakage.

### S7 — API compatibility is tactical, not architectural

Where Litekart compatibility is useful, preserve compatible routes and
payloads.

Do not distort the domain model merely to clone legacy behavior.

Compatibility is an adapter/contract concern, not the definition of the
engine.

---

## 5. Architecture decisions

| # | Decision | Choice | Rationale |
|---|---|---|---|
| D1 | Product model | Commerce engine, not full platform | Keeps the value proposition focused. |
| D2 | Database | PostgreSQL | Removes SQLite writer ceiling and enables durable outbox + real concurrency. |
| D3 | ORM/query layer | Prefer `database/sql` with explicit SQL in v1 | Keeps data ownership visible and avoids ORM behavior becoming architecture. |
| D4 | DB abstraction | None in v1 | Supporting multiple production databases would add interfaces before they are needed. |
| D5 | Extension mechanism | Explicit module wiring | Typed, searchable, debuggable, easy for Go developers. |
| D6 | Core shape | One public `gocommerce` package | Prevents cycles and keeps module integration simple. File discipline replaces premature package fragmentation. |
| D7 | HTTP | stdlib `net/http` + Go `ServeMux` | Routing and middleware requirements remain intentionally small. |
| D8 | Events | Transactional outbox + dispatcher | Event record is committed with the business transaction; no crash window between commit and publication. |
| D9 | Event delivery | At-least-once | Consumers must be idempotent; duplicates are acceptable, lost business events are not. |
| D10 | Async transport | Core outbox dispatcher; Redis Streams as an optional module/transport | Reliable by default, scalable when needed. |
| D11 | Variants | First-class core model | Avoids a later breaking redesign of cart/order/stock. |
| D12 | Inventory | Variant-level stock, optional product-level computed availability | Commerce quantity belongs to the sellable SKU. |
| D13 | Money | Integer minor units, in `*_minor` columns | Avoids floating-point money errors. Never `*_cents`: JPY has 0 decimals, KWD has 3, so a `_cents` column is a lie the moment currency becomes extensible. |
| D14 | Currency | One settlement currency per store, `Config.Currency`, **default `USD`**; snapshotted on every order; API returns `{amount_minor, currency}` and never a formatted string | Mixed-currency checkout is out of scope, but any ISO code works today and the order-level snapshot keeps history self-describing if a store changes currency. Formatting (symbol, decimal places) is the client's job — which is what makes new currencies need no core change. |
| D15 | Idempotency | Persisted request keys + provider event IDs | Prevent double orders and repeated external callbacks. |
| D16 | API contract | Unversioned Litekart-compatible overlap + explicit GoCommerce additions | Reuse existing storefronts without freezing every future design choice. |
| D17 | API docs | OpenAPI embedded/served by core | API remains discoverable without external tooling. |
| D18 | Agent interface | MCP module | Exposes controlled domain actions without duplicating business logic. |
| D19 | Authentication | Simple admin bearer token in core + replaceable middleware seam | Good for single-store bootstrap while leaving a path to accounts/RBAC. |
| D20 | Search | Out of core | Search infrastructure varies heavily by deployment. |
| D21 | Language | `Config.DefaultLanguage` **default `en`**, `Config.Languages` default `["en"]`. Core negotiates per request (`?lang=` → `Accept-Language` → default) with hand-rolled primary-subtag matching, snapshots the language on the order, and passes it to notifiers. Content translations are OUT of core: an optional `Translator` port supplies per-language field overrides, batched by id. | A single-language store must sell without paying for i18n, but extensibility was a requirement — so the seam is built, not speculated. `golang.org/x/text` is a dependency we decline for ~25 lines of matching. |
| D22 | Guest checkout | **Permanent core guarantee.** A shopper buys with a cart token and an email; no account, ever. A future identity module may ADD authenticated checkout and order history, but may never make an account required, and core carries no path that assumes a customer record exists. | Stating it as an invariant now constrains the identity module before it exists — the only time that constraint is free. It also keeps order snapshots (§10.4) authoritative rather than deferring to mutable customer rows. |
| D23 | Repo layout | **One Go module.** Core at the root; extensions that add zero third-party dependencies are packages under `ext/`; an extension needing an SDK (`queue-redis`, `search-meilisearch`, `storage-s3`) ships as its own satellite repo. MCP was expected to be one and is not — it speaks JSON-RPC over `encoding/json` instead of taking the official SDK, so it stayed in `ext/` (§22). No `go.work`, no nested modules, no per-extension tags. | Dependency isolation was the only thing multi-module bought, and most planned extensions are REST + HMAC with no SDK. Bundled extensions still see only core's exported API — Go package boundaries forbid reaching into unexported internals — so they remain a real proof of the seams while `go build ./...` covers the whole product. |
| D24 | Role rights | **Fixed roles, configurable sets.** The roles stay `owner`, `manager`, `staff`; what `manager` and `staff` may do is the store's to re-cut, stored in `role_rights` as the departure from the engine's defaults (a role with no rows keeps tracking them). Owner is not storable and always carries every right. Every role keeps `catalog.read`. Rights resolve on each authentication, never from a cached copy. This renames `RightsOf`/`Can` to `DefaultRightsOf`/`DefaultCan` and adds `(*Superuser).Has` — a breaking change to the exported API, taken deliberately. | D19 left the seam open for RBAC and rights.go was already a map so that the sets could move into a table without moving the decisions. Custom *roles* were declined: a role name is in the `superusers` CHECK, in every picker and in every invitation, and a store that wants "warehouse" almost always wants "staff, plus stock" — which re-cutting gives it. Resolving per request rather than caching means a withdrawn right is gone on the next call, with nobody signed out and no second server holding a stale copy of who may refund. |

---

## 6. Core domain model

### 6.1 Product hierarchy

The fundamental model is:

```text
Product
  ├── Option: Size
  │     ├── S
  │     ├── M
  │     └── L
  ├── Option: Color
  │     ├── Black
  │     └── White
  └── Variants
        ├── SKU-001 = M / Black
        ├── SKU-002 = M / White
        └── SKU-003 = L / Black
```

A product is the merchandising concept.

A variant is the sellable unit.

### 6.2 Product fields

Core product fields:

```text
id
slug
name
description
status
base_price_minor
currency
metadata
created_at
updated_at
```

`base_price_minor` is useful for simple products and presentation, but the
**variant price is authoritative at checkout** when variants exist.

### 6.3 Variant fields

```text
id
product_id
sku
barcode nullable
price_minor
compare_at_price_minor nullable
stock_on_hand
stock_reserved
active
weight_grams nullable
metadata
created_at
updated_at
```

`available_stock` is derived as:

```text
stock_on_hand - stock_reserved
```

Stock mutations happen through an inventory service, never through arbitrary
module SQL.

### 6.4 Option model

Core stores the definitions required to understand a variant:

```text
product_options
product_option_values
variant_option_values
```

Variant combinations must be unique within a product.

For example, a product cannot contain two active variants representing exactly
`Color=Black + Size=M`.

### 6.5 Single-variant products

A product that does not need options still has one default variant.

This keeps all cart/order/inventory logic variant-centric without making a
simple product feel complicated to the API client.

The client can treat:

```text
Product -> default variant
```

as a zero-configuration case.

---

## 7. Inventory model

Inventory belongs to variants.

### 7.1 Reservation flow

Checkout must not merely decrement stock blindly.

The transaction performs:

```text
available = on_hand - reserved

require available >= requested

reserved += requested
```

The reservation is tied to the order.

### 7.2 Order confirmation

On a successful paid/COD confirmation, the reservation is converted into a
committed sale:

```text
reserved -= quantity
on_hand  -= quantity
```

### 7.3 Cancellation

Before shipment:

```text
reserved -= quantity
```

If inventory was already committed to the sale, cancellation logic must issue
an explicit restock transaction according to the order state.

The state transition rules must be tested rather than left to callers.

### 7.4 Concurrency

Stock allocation uses PostgreSQL transactional locking.

Proof test:

> 100 concurrent checkouts against 1 available unit create exactly one
> successful reservation/order and 99 conflicts; inventory never becomes
> negative.

---

## 8. Cart design

**Guest checkout is a permanent guarantee (D22)** — a cart needs a token and
an email, never an account. Everything below holds with no identity module
installed, and must keep holding after one is.

Carts support:

- guest tokens;
- optional authenticated customer ID in the future (additive, never required);
- variant-based line items;
- quantity changes;
- item removal;
- price snapshots;
- inventory availability checks;
- cart expiration;
- cart metadata.

A cart line references a variant, not merely a product:

```json
{
  "variant_id": 42,
  "qty": 2
}
```

The API may return the parent product and option selections for convenience.

### Cart price policy

Adding an item records the current display price, but checkout is authoritative.

At checkout:

1. the variant must still exist and be active;
2. the requested quantity must still be available;
3. the current authoritative price is compared with the cart snapshot;
4. a changed price produces `409 conflict`;
5. the cart is refreshed with current values;
6. the shopper confirms again.

The engine never silently reprices a confirmed order.

---

## 9. Checkout design

Checkout is divided into three logical boundaries.

### Phase A — validate and create order

Inside one PostgreSQL transaction:

1. load cart;
2. validate every variant;
3. lock relevant inventory rows;
4. re-check prices and active state;
5. reserve inventory;
6. create immutable order + order lines;
7. store idempotency state;
8. consume/close cart;
9. append domain events to the outbox;
10. commit.

### Phase B — initiate external payment

After commit:

```text
PaymentProvider.Initiate(...)
```

never executes while a transaction is holding core write locks.

If initiation fails:

- the order remains valid;
- payment status remains `pending`;
- the stored idempotency key points to the same order;
- retry resumes payment initiation instead of creating another order.

### Phase C — payment confirmation

A provider webhook invokes the provider module.

The module:

1. validates the provider signature;
2. deduplicates the provider event;
3. calls `Payments.MarkPaid(...)`;
4. core performs the state transition;
5. core records the resulting event in the outbox.

The provider never updates `orders` directly.

---

## 10. Orders

### 10.1 Order status

```text
pending
  ↓
confirmed
  ↓
processing (optional future state)
  ↓
shipped
  ↓
delivered
```

Cancellation is a controlled transition, not a generic update.

### 10.2 Payment status

```text
pending → paid → refunded
       ↘ failed
```

Refunding a payment is an explicit operation.

### 10.3 Order lines

Order lines are immutable snapshots containing at minimum:

```text
product_id nullable
variant_id nullable
sku
title
variant_label
quantity
unit_price_minor
total_minor
metadata
```

Historical orders must remain understandable even when products or variants
are later deleted or changed.

### 10.4 Customer snapshot

Order stores the checkout-time customer information:

```text
email
phone
name
address snapshot
```

A future customer-account module can maintain the long-lived customer entity,
but historical orders never depend on mutable customer records for their legal
or operational meaning.

---

## 11. Transactional outbox — the major R3 upgrade

The outbox is now a **core correctness mechanism**.

### 11.1 The problem

The dangerous pattern is:

```text
BEGIN
change order
COMMIT
publish event
        ↑
      crash
```

A process failure in the gap loses the event even though the order change is
permanently committed.

That is unacceptable for events such as:

- `order.created`;
- `order.paid`;
- `order.shipped`;
- `order.delivered`;
- `order.cancelled`.

### 11.2 The R3 solution

The business transaction writes both state and an outbox record:

```text
BEGIN
  update orders
  update inventory
  INSERT outbox_events
COMMIT
```

Either everything commits, or none of it does.

### 11.3 Outbox schema

```sql
outbox_events (
    id              uuid primary key,
    event_name      text not null,
    event_version   integer not null,
    aggregate_type  text not null,
    aggregate_id    bigint not null,
    payload         jsonb not null,
    created_at      timestamptz not null,
    available_at    timestamptz not null,
    published_at    timestamptz null,
    attempts        integer not null default 0,
    last_error      text null
)
```

Indexes:

```text
(published_at, available_at, created_at)
(aggregate_type, aggregate_id, created_at)
```

### 11.4 Dispatcher

The built-in dispatcher continuously claims unpublished rows, delivers them
to registered handlers, and marks successful delivery.

PostgreSQL row locking prevents multiple application instances from processing
the same outbox row concurrently.

Conceptually:

```text
SELECT ...
FROM outbox_events
WHERE published_at IS NULL
  AND available_at <= now()
ORDER BY created_at
FOR UPDATE SKIP LOCKED
LIMIT N
```

Delivery is **at-least-once**.

A crash after a handler runs but before `published_at` is persisted can cause
a duplicate delivery. Handlers must therefore be idempotent.

### 11.5 Event identity

Every event has a stable UUID.

Consumers that perform non-idempotent work maintain a processed-event table or
provider-specific idempotency key.

### 11.6 Retry strategy

Failed delivery uses exponential backoff with a maximum retry delay and keeps:

```text
attempts
last_error
available_at
```

A dead-letter state is preferred over silently deleting a repeatedly failing
event.

### 11.7 Operational rule

The database is the source of truth for whether an event exists.

The bus is the delivery mechanism.

This separation makes the default synchronous dispatcher and future Redis/Kafka
transports implementation details rather than correctness dependencies.

---

## 12. Event contract

Initial frozen taxonomy:

```text
order.created
order.paid
order.shipped
order.delivered
order.cancelled
```

Event envelope:

```go
type Event struct {
    ID       string          `json:"id"`
    Name     string          `json:"name"`
    Version  int             `json:"v"`
    At       time.Time       `json:"at"`
    AggregateType string     `json:"aggregate_type"`
    AggregateID int64        `json:"aggregate_id"`
    Data     json.RawMessage `json:"data"`
}
```

### Event rules

1. Events are created inside the same transaction as the state change.
2. Event payloads are immutable after commit.
3. Payload schema changes require a new event version.
4. Consumers must tolerate duplicate delivery.
5. Consumers must not assume synchronous delivery.
6. Core owns event creation; modules never publish state-transition events by
   themselves.

Do not freeze speculative events such as `product.updated` until a real module
needs them.

---

## 13. Modules and extension mechanism

A module remains an ordinary Go value implementing:

```go
type Module interface {
    Name() string
    Migrations() []Migration
    Register(app *App) error
}
```

Modules are passed to `gocommerce.New(...)`.

### Allowed module capabilities

```go
RegisterPayment(...)
RegisterFulfillment(...)
RegisterNotifier(...)
RegisterTranslator(...)
Handle(...)            // validated against /x/<name>/
HandleAdmin(...)       // validated against /api/admin/x/<name>/, auth wrapped
Subscribe(...)
OnStart(...)
OnStop(...)
```

(There is no `RegisterSearch` or `RegisterStorage`; see §5's port list for why.)

### Hard rule

Modules may create and own their own PostgreSQL tables.

Modules **may not mutate core commerce tables directly**.

Instead:

```text
module
  ↓
core service
  ↓
transaction
  ↓
state change + outbox event
```

This is the invariant that keeps the system composable.

---

## 14. Payments

### Built-in COD

COD remains built into core because it is a valid payment method that requires
no external provider.

Behavior:

```text
checkout
  ↓
order confirmed
  ↓
payment_status = pending
  ↓
delivery
  ↓
admin marks paid
```

### Payment provider interface

```go
type PaymentProvider interface {
    Code() string
    Initiate(ctx context.Context, order *Order, opts PayOptions) (PaymentIntent, error)
}
```

Optional webhook capability:

```go
type WebhookProvider interface {
    Webhook() http.Handler
}
```

### First external provider

`payments-stripe`

Responsibilities:

- create PaymentIntent;
- return client secret/redirect data;
- verify webhook signatures;
- deduplicate webhook events;
- invoke `Payments.MarkPaid` / `Payments.MarkFailed`;
- implement refund operations.

`payments-razorpay` follows the same port.

---

## 15. Fulfillment

Core provides:

```text
manual fulfillment
```

External carriers are modules.

First carrier:

```text
fulfill-shiprocket
```

The provider receives an order plus a typed shipping request and returns:

```go
type Shipment struct {
    Provider string
    Tracking string
    LabelURL string
}
```

Carrier webhooks never update orders directly. They call core fulfillment
services.

---

## 16. Notifications

Core defines the notification abstraction and durable event triggers.

Modules implement actual delivery:

```text
notify-sendgrid
notify-resend
notify-msg91
notify-twilio
```

Templates remain outside core.

A provider failure must not corrupt an order transaction.

Notifications are therefore downstream consumers of committed events.

---

## 17. Search

Search remains outside core.

Core owns:

```text
catalog + variants + availability + canonical state
```

Search modules own:

```text
indexing + ranking + autocomplete + faceting
```

Initial likely modules:

```text
search-meilisearch
search-typesense
```

The event/outbox layer is the integration mechanism:

```text
product/variant change
        ↓
   durable event
        ↓
 search module
        ↓
 search index
```

This makes search rebuildable and independently scalable.

---

## 18. Imports and exports

CSV support remains, but the model must now be variant-aware.

### Products export

Suggested columns:

```text
product_id
product_slug
product_title
variant_id
sku
barcode
variant_options
price_minor
compare_at_price_minor
stock_on_hand
active
weight_grams
metadata
```

### Import behavior

Products/variants upsert by stable SKU where appropriate.

The import engine supports:

- dry-run;
- streaming;
- row-level error reporting;
- formula-injection protection on export;
- lossless export/edit/import round-trip;
- explicit option/variant combination validation.

### Orders export

Orders continue to export one row per order item because this is useful for
accounting and operational workflows.

Historical order snapshots remain authoritative during import/export: an
imported order line keeps its own sku, title and price with a nullable
`product_id`, so a migrated history stays readable even where the products no
longer exist.

**Order import fires no events by default.** Migrating five thousand historical
orders must not send five thousand confirmation emails — the events are opt-in
per request (`?fire_events=1`). This is the one place where suppressing a
durable event is correct, and it is a deliberate exception to §12 rather than
an oversight.

---

## 19. API strategy

### 19.1 Compatibility

Retain the useful Litekart-compatible overlap:

```text
/api/products
/api/products/{id}
/api/products/sku/{sku}
/api/carts
/api/carts/{cartId}
/api/carts/{cartId}/line-items
/api/checkout/{code}
/api/orders/{number}
/api/admin/...
```

Where GoCommerce needs a better domain shape, add a clean endpoint rather than
bending the data model to emulate legacy behavior.

### 19.2 Variants API

Example surface:

```text
GET  /api/products/{id}/variants
GET  /api/variants/{id}
GET  /api/products/sku/{sku}
POST /api/admin/products/{id}/variants
PATCH /api/admin/variants/{id}
DELETE /api/admin/variants/{id}
```

The exact route set is frozen after the first integration implementation, not
before.

### 19.3 Idempotency

Mutating checkout requests require:

```text
Idempotency-Key: <opaque-client-key>
```

The key is scoped to the relevant operation and persisted in PostgreSQL.

Same key + same request returns the original result.

Same key + conflicting request returns a validation error.

### 19.4 API response envelope

Maintain:

```json
{"data": ...}
```

and:

```json
{"error": {"code": "...", "message": "..."}}
```

Every list endpoint paginates.

### 19.5 OpenAPI

Core serves:

```text
GET /doc
GET /docs
```

The OpenAPI contract is generated or assembled from a maintained source of
truth and validated in CI.

The important rule is not the implementation technique; it is that the served
spec cannot silently drift from the actual routes.

---

## 20. Authentication and administration

Core accepts **two kinds of admin credential**, because scripts and people want
different things. Both arrive as `Authorization: Bearer <x>` and both satisfy
the same middleware, so no handler has to know which it got.

- **Static tokens** — `Config.AdminTokens` is a slice, not a string,
  specifically so a token can be rotated without downtime: add the new one,
  deploy, retire the old. They have no session and no expiry, which is right
  for CI and curl and wrong for a browser.
- **Superuser sessions** — an operator signs in with an email and a password
  and gets a token that expires. Passwords are PBKDF2-HMAC-SHA256 from the
  standard library (600k iterations, self-describing hash so the cost can be
  raised without a migration); session tokens are stored only as a SHA-256
  hash. The login endpoint returns the same error and takes the same time for
  a wrong password and an unknown account, and throttles on two counters —
  per (account, address) and per address — so neither lockout-by-proxy nor
  password spraying is free.

`superusers` is an **operator** table. Nothing in the commerce path reads it,
so guest checkout (D22) is untouched by its existence.

```go
Config.AdminAuth
```

remains the seam for replacing the whole scheme with:

- sessions;
- JWT/OIDC;
- RBAC;
- customer accounts;
- organization membership.

A future identity module owns identity. Core continues to own commerce state.

---

## 21. MCP / agent strategy

MCP is a strategic feature, but it is not allowed to duplicate the domain.

The MCP module exposes controlled tools over the core services.

Initial tools:

```text
list_products
get_product
update_product
list_low_stock_variants
list_orders
get_order
mark_paid
cancel_order
create_fulfillment
ship_order
```

Potential later tools:

```text
create_product
update_variant_inventory
create_discount
create_purchase_recommendation
reconcile_orders
```

The principle is:

```text
AI agent
   ↓
   MCP
   ↓
core service
   ↓
PostgreSQL transaction
   ↓
outbox
```

The agent never receives direct database access.

This becomes a meaningful differentiator because the same state machine is
usable from humans, applications and controlled AI agents.

---

## 22. Repository layout

As built:

```text
gocommerce/                      # ONE Go module: github.com/misiki/gocommerce
├── go.mod                       # pgx is the only production dependency
├── PLAN.md  README.md  AGENTS.md  CLAUDE.md
├── gocommerce.go                # App, Config, New, ListenAndServe
├── module.go  ports.go
├── events.go  outbox.go
├── db.go  migrate.go  schema.go
├── httpx.go  openapi.go  openapi.json
├── i18n.go                      # language negotiation + Translator application (D21)
├── catalog.go  catalog_http.go  # products, options AND variants: one service
├── inventory.go
├── cart.go  checkout.go  orders.go  commerce_http.go
├── payments.go  fulfillment.go  notify.go
├── superusers.go  superusers_http.go   # operator identity (email + password)
├── doctor.go                    # operational diagnostics → `gocommerce doctor`
├── transfer.go  transfer_http.go       # CSV import/export
├── types.go                     # Money, Address, Metadata
├── admin/                       # SvelteKit panel, embedded (admin_http.go)
├── skills/                      # procedural guides for humans and agents
├── .cursor/rules/               # editor-level guardrails
├── gctest/                      # module-author test kit
├── cmd/gocommerce/main.go       # serve | migrate | superuser | doctor | spec
├── docs/
│   ├── events.md  writing-a-module.md  operations.md  admin-panel.md
│   ├── litekart-openapi.json    # vendored api.litekart.in/doc — the D16 reference
│   └── PLAN-r2-sqlite-archived.md
├── examples/store/              # same module, no go.mod of its own
└── ext/                         # bundled extensions: ZERO third-party deps (D23)
    ├── payments-stripe/         # package stripe — REST + HMAC, no SDK
    ├── payments-razorpay/
    ├── notify-sendgrid/   notify-msg91/
    ├── fulfill-shiprocket/
    └── invoices/   cms/   mcp/
```

**Satellite repos** — the only extensions that leave, and only because they
carry a third-party dependency:

```text
gocommerce-redis/     queue-redis (Redis client)
gocommerce-search/    search-meilisearch / search-typesense
gocommerce-storage/   storage-s3 (AWS SDK)
```

MCP was expected to be one of these and is not: rather than take the official
Go SDK, it speaks JSON-RPC 2.0 over `encoding/json` in about as much code as
the SDK's wiring would have been. It therefore satisfies D23 and lives in
`ext/mcp` — a useful datapoint for how often a protocol actually requires its
reference implementation.

Nothing about the extension mechanism changes when a package moves out — a
`Module` is a `Module`. A CI check asserts core's `go.mod` holds only the
Postgres driver and that nothing under `ext/` imports a third-party package:
D23's rule, enforced rather than remembered.

---

## 23. Configuration

The configuration surface should stay intentionally small.

Conceptually:

```go
type Config struct {
    DBURL           string
    Addr            string
    Currency        string   // default "USD" (D14)
    DefaultLanguage string   // default "en"  (D21)
    Languages       []string // default ["en"]
    AdminTokens     []string
    AdminAuth       func(http.Handler) http.Handler
    HandlerTimeout  time.Duration
    CartTTL         time.Duration
    OrderTTL        time.Duration
    OrderPrefix     string
    OutboxBatchSize int
    OutboxPoll      time.Duration
    Dev             bool
}
```

Do not turn configuration into a second programming language.

Provider configuration belongs to provider modules.

---

## 24. Operations

### Background work

Three loops run inside the process, all started as ordinary lifecycle hooks so
they stop with it:

- **The outbox dispatcher** — claims batches with `FOR UPDATE SKIP LOCKED`,
  publishes, backs off exponentially on failure, and dead-letters after
  repeated attempts. This is the one whose silence is dangerous, which is why
  `gocommerce doctor` reports its backlog age.
- **The cart sweeper** — deletes carts past their TTL. `POST /api/carts` is
  unauthenticated, so without this it is an unbounded-growth vector.
- **The reservation sweeper** — cancels orders still unpaid past
  `reservation_expires_at` and releases the stock they were holding. An
  abandoned gateway redirect must not hold inventory forever, and a store
  silently "out of stock" with full shelves is a hard failure to diagnose
  from the outside.

Running several application nodes is safe: the dispatcher's claim query and the
migration advisory lock are what make concurrency the database's problem rather
than the operator's.

### Topology

The production architecture is:

```text
                 Internet
                    │
               reverse proxy
                    │
          ┌─────────┴─────────┐
          │ one or more Go    │
          │ application nodes │
          └─────────┬─────────┘
                    │
               PostgreSQL
```

Optional later:

```text
                  PostgreSQL
                 /           \
          primary             read replica
              │
       application nodes
```

Redis is optional.

It is introduced only when an external queue is operationally useful. The
commerce engine must remain correct without Redis.

### Backups

PostgreSQL backup/restore documentation is part of the initial production
release.

Later:

- point-in-time recovery;
- managed Postgres;
- continuous backup;
- replica promotion.

The operational philosophy is:

> **Start with one database and one binary. Scale components only when traffic or reliability requirements justify them.**

---

## 25. Reliability requirements

These are now first-class proof requirements rather than documentation notes.

### Must pass

1. Two concurrent checkouts cannot oversell one variant.
2. Reusing an idempotency key cannot create a second order.
3. A payment webhook replay cannot double-settle an order.
4. A transaction rollback creates no durable business event.
5. A committed transaction always leaves its outbox event recoverable.
6. A dispatcher crash produces at-least-once delivery, not event loss.
7. Duplicate event delivery does not create duplicate invoices or notifications
   in idempotent consumers.
8. Cancelling an eligible order restores/resolves inventory correctly.
9. Historical order lines remain correct after product/variant edits.
10. Variant combinations cannot duplicate the same option selection.
11. OpenAPI contains every public/admin core route.
12. A store can run with zero optional modules.
13. Adding Stripe does not change core order/payment code.
14. Replacing the sync dispatcher with Redis does not alter the purchase path.

---

## 26. Testing strategy

### Unit tests

Focus on:

- state transitions;
- price calculations;
- variant combination validation;
- inventory rules;
- idempotency semantics;
- event creation.

### Integration tests

Use a real PostgreSQL test instance rather than pretending PostgreSQL locking
behavior is equivalent to an in-memory mock.

Test:

```text
checkout concurrency
payment webhook replay
outbox retry
multiple application workers
variant imports
order cancellation
refunds
```

### End-to-end purchase-path test

The canonical test should be:

```text
create product
create variants
add variant to cart
checkout
payment initiate
payment webhook
order paid
notification emitted
fulfillment created
shipped
outbox drained
```

The same order state transitions must be executable from the API and MCP.

---

## 27. Module SDK

`gctest` remains useful, but its goal changes from testing a SQLite application
to making third-party module development easy against PostgreSQL.

Desired helpers:

```go
gctest.New(t, cfg, mods...)
gctest.AdminRequest(...)
gctest.CreateProduct(...)
gctest.CreateVariant(...)
gctest.WaitForOutbox(...)
gctest.RecordingNotifier(...)
```

Module authors should not need to understand the entire engine to write a
payment provider.

---

## 28. Initial module set

| Module | Priority | Purpose |
|---|---:|---|
| `payments-stripe` | P0 | First real online payment proof. |
| `notify-sendgrid` | P0 | First real notification consumer. |
| `fulfill-shiprocket` | P1 | Strong India deployment story. |
| `payments-razorpay` | P1 | Important India payment option. |
| `search-meilisearch` | P1 | Real catalog search without polluting core. |
| `mcp` | P1 | Strategic AI/agent differentiation. |
| `queue-redis` | P2 | External asynchronous transport when needed. |
| `invoices` | P2 | Accounting/transaction document extension. |
| `storage-s3` | P2 | Real media/object storage. |
| `cms` | P3 | Optional content layer. |
| `i18n` | P3 | Content translations via the `Translator` port (D21). |

---

## 29. Release strategy

Do not wait until the entire ecosystem exists.

### v0.1 — real commerce engine

Ship:

- PostgreSQL;
- products;
- variants;
- inventory;
- carts;
- checkout;
- orders;
- COD;
- manual fulfillment;
- transactional outbox;
- REST API;
- OpenAPI;
- CSV import/export;
- backup/operations docs;
- strong test suite.

This is the first public release.

### v0.2 — real integrations

Ship:

- Stripe;
- email notifier;
- refunds;
- webhook idempotency;
- module author test kit;
- example store.

### v0.3 — production extensions

Ship:

- Shiprocket;
- Razorpay;
- Meilisearch;
- richer inventory metadata;
- multi-instance deployment proof.

### v0.4 — agent-ready commerce

Ship:

- MCP;
- agent integration examples;
- safe mutation tools;
- audit logging for agent actions.

### v0.5 — ecosystem hardening

Ship only extensions demanded by users:

- tax;
- discounts;
- shipping rates;
- object storage;
- customer accounts;
- additional payment/carrier providers.

Do not build a speculative feature catalogue.

---

## 30. What should NOT enter core

**The membership test**, which generates the list below rather than being
justified by it:

> *Can a cash-on-delivery store, selling in one country, in one currency,
> complete a sale without this?*

If yes, it is not core. The test is deliberately harsh — it excludes things
most shops eventually want — because the alternative is a kernel that grows by
plausible increments until nobody can hold it in their head. Everything it
excludes is still buildable; it just builds as a module, against the same
exported API a third party would use.

Applied honestly it also rules *in*: variants pass (a product with two sizes is
one sale), the outbox passes (an order that silently fails to notify is a
broken sale), and guest checkout passes by definition.

Keep these outside unless evidence shows they fundamentally change the
commerce state machine:

- Stripe/Razorpay SDKs;
- carrier APIs;
- email/SMS vendor SDKs;
- Redis clients;
- search engines;
- object storage;
- CMS;
- invoice rendering;
- customer identity providers;
- tax providers;
- advanced promotions;
- recommendation engines;
- analytics;
- AI models;
- storefront UI.

Core owns the commerce truth. Modules own external capabilities.

---

## 31. Strategy for Litekart

Litekart compatibility remains one of the strongest tactical advantages for
this project.

The recommended relationship is:

```text
                 Litekart
                    │
          existing storefronts
                    │
                    ▼
             Litekart API shape
                    │
             ┌──────┴──────┐
             │             │
          existing      gocommerce
          backend         engine
```

Do not require Litekart to disappear.

Instead:

1. make GoCommerce implement the useful overlap of the Litekart API;
2. make Svelte-Commerce able to target GoCommerce with minimal changes;
3. use existing Litekart users as early technical adopters;
4. let GoCommerce become a separate product with its own identity.

This gives the project an unusually practical distribution path.

### Strategic advantage

You are not starting with:

```text
new engine → find users
```

You can start with:

```text
existing ecosystem → new engine → migration/compatibility story
```

That materially reduces the cold-start problem.

---

## 32. Go-to-market strategy

### Phase 1 — developer credibility

The objective is not revenue.

The objective is proof that developers understand the thesis.

Publish:

- architecture write-up;
- benchmark methodology;
- "why not a huge ecommerce framework" article;
- variant model explanation;
- transactional outbox explanation;
- "build a store in Go" tutorial;
- Litekart migration example;
- MCP demo.

### Phase 2 — reference store

Create one excellent reference implementation:

```text
Go
Postgres
gocommerce
Svelte-Commerce
Stripe
Shiprocket
SendGrid
MCP
```

It should be deployable without proprietary infrastructure.

### Phase 3 — migration story

Make it easy to answer:

> "I already have products/orders/customers — how do I get onto this?"

CSV import is useful here, but migration tooling should eventually include
provider-specific import adapters.

### Phase 4 — ecosystem

Invite Go developers to publish modules.

The project wins when the module directory begins to answer:

> "Can it integrate with my stack?"

without core changes.

---

## 33. Distribution and repository strategy

The GitHub repository should make the thesis obvious in the first screen.

README structure:

```text
hero
↓
what it is
↓
30-second architecture
↓
minimal example
↓
variant-aware commerce example
↓
module example
↓
transactional outbox explanation
↓
MCP example
↓
Litekart compatibility
↓
production deployment
↓
roadmap
```

The README should not start with a giant feature matrix.

Lead with the design decision:

> **Commerce primitives in Go, without a commerce monolith.**

---

## 34. Competitive positioning

The comparison should focus on architecture rather than pretending all
products solve exactly the same problem.

| Approach | Strength | GoCommerce position |
|---|---|---|
| Shopify | Excellent hosted merchant experience | Not competing on hosted SaaS. |
| WooCommerce | Huge ecosystem | Smaller, code-first, headless-friendly. |
| Medusa/Vendure | Mature composable commerce | Smaller Go-native engine with explicit composition. |
| Custom Go app | Maximum control | GoCommerce removes repeated commerce plumbing. |
| Large microservice stack | Scale/flexibility | GoCommerce keeps the first deployment much smaller. |
| SQLite embedded store | Operational simplicity | GoCommerce chooses Postgres for production correctness/concurrency. |

The claim is not that GoCommerce is universally better.

The claim is:

> **For Go teams that want ownership without rebuilding commerce primitives, it is a strong middle ground between a giant commerce platform and a custom implementation.**

---

## 35. Economic/product strategy

The open-source engine should remain useful by itself.

Potential commercial layers later:

```text
Open-source engine
       │
       ├── paid modules
       ├── migration services
       ├── hosted control plane
       ├── enterprise support
       └── managed deployment
```

Do not introduce a license that damages early developer adoption unless there is
a concrete reason.

License decision remains open between MIT and Apache-2.0.

---

## 36. Success metrics

Do not judge the project by GitHub stars alone.

### Technical

- successful concurrent checkout proof;
- no lost outbox events;
- zero duplicate settlement on webhook replay;
- migration/import correctness;
- predictable recovery after process crash;
- API compatibility coverage.

### Adoption

Within the first public iterations, look for:

```text
10 real stores/apps trying it
3 external module contributors
2 production deployments not controlled by the author
1 meaningful migration from an existing commerce backend
```

Those signals matter more than 10,000 stars from developers who never deploy it.

### Product-market signal

The strongest signal is:

> A developer chooses GoCommerce for a real project without being asked to.

---

## 37. What success looks like in 12 months

A successful first year does **not** require GoCommerce to replace Shopify.

A successful outcome looks like:

```text
                gocommerce
                    │
      ┌─────────────┼──────────────┐
      │             │              │
   small stores   B2B apps    custom storefronts
      │             │              │
      └─────────────┼──────────────┘
                    │
              module ecosystem
                    │
          payments / search / AI
```

The project becomes known among Go developers as the place to start when they
need commerce primitives without adopting an enormous platform.

---

## 38. Milestones

Each milestone must produce something usable.

### M0 — PostgreSQL kernel

- repository;
- PostgreSQL connection/lifecycle;
- migrations;
- `App`, `Config`, `Module`;
- request middleware;
- health/readiness;
- admin auth;
- OpenAPI skeleton;
- CI on Linux + Windows;
- integration-test PostgreSQL service.

**Proof:** clean build, database migration, health endpoints, valid OpenAPI.

### M1 — Products + variants

- products;
- options;
- option values;
- variants;
- SKU uniqueness;
- variant combination uniqueness;
- admin CRUD;
- public catalog;
- CSV import/export.

**Proof:** a product can be created and sold as both a simple single-variant
product and a multi-variant product.

### M2 — Inventory + cart

- variant inventory;
- reservation model;
- guest cart;
- line-item CRUD;
- TTL;
- price snapshots.

**Proof:** concurrent cart/stock behavior is deterministic.

### M3 — Sell

- orders;
- order snapshots;
- checkout;
- idempotency;
- COD;
- domain events;
- transactional outbox;
- outbox dispatcher.

**Proof:** commit + crash simulation never loses an event; concurrent checkout
never oversells.

### M4 — Operate

- manual fulfillment;
- deliver/cancel/mark-paid;
- backup/restore docs;
- order CSV import/export;
- observability;
- `gctest`;
- reference store.

**Release:** `v0.1.0`.

### M5 — Real payments

- Stripe module;
- webhook validation;
- webhook deduplication;
- refunds;
- email notifier;
- event retry/dead-letter behavior.

**Release:** `v0.2.0`.

### M6 — Production extensions

- Razorpay;
- Shiprocket;
- Meilisearch;
- multi-instance test;
- outbox throughput test.

**Release:** `v0.3.0`.

### M7 — AI-native commerce

- MCP module;
- read tools;
- safe mutation tools;
- agent audit records;
- scripted end-to-end agent flow.

**Release:** `v0.4.0`.

### M8+ — user-driven extensions

Only then evaluate:

- customer accounts;
- discounts;
- tax;
- shipping rates;
- media;
- additional queues;
- additional carriers;
- additional search engines;
- SQLite compatibility.

---

## 39. Deferred decisions

These should not block M0–M4:

1. customer identity model;
2. tax engine;
3. discount engine;
4. shipping-rate calculation;
5. multi-currency;
6. advanced promotion rules;
7. product bundles/kits;
8. subscriptions;
9. multi-tenant mode;
10. marketplace/vendor settlements;
11. SQLite compatibility;
12. Kafka transport.

They become priorities only when a real use case requires them.

---

## 39a. Amendment: media is stored, not only linked

R3 said image storage stays out of core — URLs only, with object storage as a
satellite module (§30). That is now amended, deliberately and with the cost
stated.

**Why it changed.** "Paste a URL" presumes the operator already runs somewhere
to host files. A small store does not, and telling someone to go set up a
bucket before they can add a photograph to a product is the kind of gap that
makes an admin panel feel unfinished rather than principled.

**What kept the original intent.** Core owns the *records* and a two-method
interface, never a storage SDK:

```go
type MediaStore interface {
    Put(ctx, filename, contentType string, r io.Reader) (key, url string, err error)
    Delete(ctx, key string) error
}
```

There is one implementation in core — the local filesystem, behind
`Config.MediaDir` — and `Config.MediaStore` replaces it wholesale. S3, GCS and
a CDN remain modules, and D23's rule that core carries one production
dependency is untouched. A store that sets neither still runs: the library
records media by URL and the panel says why upload is unavailable rather than
offering a button that fails.

**The costs, eyes open.** A directory that must be backed up alongside the
database and can fill a disk; a file-serving path that must never become a
traversal or a stored-XSS vector (hence: random flat keys, an extension
allow-list, `X-Content-Type-Options: nosniff`, and a handler that resolves
exactly one file in exactly one directory rather than using `http.FileServer`);
and orphaned files when a row insert fails after a write, which is the correct
direction to fail — wasted bytes a sweep can find beat a row pointing at
nothing.

Media is a **library**, not a per-product list, because the same photograph
belongs to several products and should be stored once. `product_media` joins
them with `ON DELETE RESTRICT`, so deleting a file six products still display
is refused rather than silently stripping them.

---

## 39b. AI-native developer experience

AI is treated as a first-class developer interface, in three layers. Each layer
answers a different question, and the split matters: rules constrain, skills
teach, and MCP acts.

**Rules — what must not be broken.** `AGENTS.md` is the canonical set of
architectural guardrails; `CLAUDE.md` and `.cursor/rules/gocommerce.mdc` point
at it and add only what is tool-specific. Each rule states the reason it
exists, because a rule whose reason is unstated gets optimised away by the next
person who finds it inconvenient.

**Skills — how to do the work.** `skills/` holds task-scoped procedural
knowledge: architecture, products, variants, carts, checkout, orders,
inventory, payments, events, integrations, infrastructure and development. Each
one carries frontmatter describing when to reach for it, so a reader — human or
agent — loads the relevant page instead of the whole codebase. They document
the invariants and *why* they hold, not the API surface, which `/doc` already
describes precisely.

**MCP — safe domain tools.** `ext/mcp` exposes tools that call the same
services REST does. It never exposes arbitrary SQL and never mutates core
tables directly, so an agent operating the store is bound by exactly the
invariants a human operator is (Rules 1 and 2 below). Every tool is a wrapper
over a service method; that is the contract, and a tool that needs to reach
past it is a missing service method, not a licence.

**Diagnostics.** `gocommerce doctor` runs the operational checks an operator
actually needs at 2am — database, migrations, admin reachability, outbox
backlog and dead letters, expired stock reservations, cart sweeping, unsellable
catalog entries, provider registration, and spec-versus-reality drift. It is a
core service (`App.Diagnose`) rather than a CLI feature, so the CLI, the MCP
`store_health` tool and any future panel screen all render the same report.
`-json` makes it machine-readable and a non-zero exit makes it gateable, so an
agent can act on it without parsing prose.

---

## 39c. Accepted tradeoffs (eyes open)

Every one of these is a real cost, accepted knowingly. They are listed so that
nobody has to rediscover them under pressure and mistake a decision for a bug.

1. **Compile-time composition excludes non-programmers.** There is no
   plugin-install button; every store is a Go build. Go's `plugin` package does
   not work on Windows and RPC plugins are ceremony a small team cannot afford.
   The operator is a developer, by design.
2. **PostgreSQL is a hard dependency.** No embedded mode, no SQLite fallback,
   nothing runs without a server. R2 traded this away for zero-install and R3
   bought it back deliberately (§2) — the outbox, `FOR UPDATE SKIP LOCKED` and
   advisory locks are the whole concurrency story and none of them survive the
   swap.
3. **No repository interfaces.** SQL lives in the services. Moving to another
   database is surgery, not configuration. Abstracting the datastore we chose on
   purpose would be ceremony pretending to be flexibility.
4. **One flat package.** File discipline is the only internal structure, so
   everything exported to modules is exported to everyone and the API-freeze
   burden lands early. `App` has to be guarded against god-object growth —
   every accessor request gets the membership test in §30.
5. **At-least-once event delivery.** The outbox guarantees an event is never
   lost, not that it arrives once. Every handler must be idempotent, and a
   single failing handler re-runs the ones beside it that already succeeded.
6. **JSON event payloads** trade compile-time typing for bus-swappability.
   Shared payload structs and an event version are convention, not proof.
7. **Unversioned `/api/`.** Litekart compatibility means route shapes are
   effectively frozen on first release; a breaking change needs a new path, not
   a `/v2/`.
8. **Forward-only migrations.** A bad migration in production needs a
   corrective forward migration. There is no rollback, because rollback is the
   thing that does not work under pressure.
9. **Table ownership is convention.** Route and migration namespacing are
   structurally enforced; table prefixes are review-enforced. A sloppy module
   *can* write `orders`. The discipline in §40 is the guardrail, not a sandbox.
10. **Hand-maintained OpenAPI.** The spec can drift from the code. The test
    that every served route appears in `/doc`, and `gocommerce doctor`'s
    re-check at runtime, are the honesty mechanisms — module fragments remain
    the module author's burden.
11. **The admin panel owns `/`.** One binary therefore cannot also serve a
    storefront there, and the panel's base path is fixed at build time.

---

## 40. Hard architectural rules

These rules are intentionally stronger than style guidelines.

### Rule 1
A module cannot mutate core commerce tables directly.

### Rule 2
Every business state transition goes through a core service.

### Rule 3
Every durable domain event is created inside the transaction that caused it.

### Rule 4
External network calls never execute while holding a core database transaction
unless a future design proves the call is both necessary and safely bounded.

### Rule 5
Consumers must be idempotent because event delivery is at-least-once.

### Rule 6
Variants are first-class sellable entities.

### Rule 7
The database is not abstracted merely for theoretical future portability.

### Rule 8
Litekart compatibility must not distort the domain model.

### Rule 9
MCP and REST call the same domain services.

### Rule 10
Every new core feature must answer:

> **Does this belong to the commerce state machine, or can it be a module?**

If it can be a module without weakening the invariants, it stays outside core.

---

## 41. Final product thesis

The project should now be understood as:

> **GoCommerce is a small, production-grade commerce engine for Go. It gives
> developers the hard parts of commerce — variants, inventory, carts,
> checkout, orders, payments, fulfillment and durable events — without forcing
> them into a monolithic platform. PostgreSQL provides the transactional spine;
> explicit Go modules provide the extension model; OpenAPI provides the public
> contract; MCP makes the same commerce engine operable by AI agents.**

The project wins by being **small enough to understand, complete enough to use,
and extensible enough to keep.**

That is the standard every architectural decision should be measured against.

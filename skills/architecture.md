---
name: architecture
description: Use when deciding where a new feature belongs, proposing a structural change, or trying to understand why the engine is shaped the way it is before arguing with it.
---

# Architecture

The authority is the decision table in [`PLAN.md`](../PLAN.md) §5 — D1 through
D25, each recorded with the argument that produced it. [`AGENTS.md`](../AGENTS.md)
is the operational summary. This file is the shape those decisions add up to,
with the D-numbers so you can go and read the full case before disagreeing.

## A store is its own Go program

GoCommerce is an engine, not an application (D1). There is no `gocommerce`
server you configure; there is a library you compose:

```go
app, err := gocommerce.New(cfg, stripe.New(...), sendgrid.New(...), mcp.New(...))
if err != nil { log.Fatal(err) }
return app.ListenAndServe()
```

[`examples/store/main.go`](../examples/store/main.go) is the canonical shape.
`cmd/gocommerce` is the reference binary with **no** modules installed — useful
for `migrate`, `doctor` and `spec`, and for proving the engine boots on its own.

The whole composition of a store is one screen of Go, and "go to definition"
works on every line of it. That is the payoff for the rules below, and it is why
there is no plugin registry, no DI container, no reflection and no configuration
DSL. Anything that would turn configuration into a second programming language
is a constant instead — `maxJSONBytes`, `httpWriteTimeout`, `shutdownGrace`,
the sweep interval — because no store has a good reason to want a 90-second
write timeout.

## One package, flat (D6)

`gocommerce.go`, `catalog.go`, `cart.go`, `checkout.go`, `orders.go`,
`inventory.go`, `payments.go`, `fulfillment.go`, `outbox.go`, `events.go`,
`httpx.go`, `doctor.go` are all `package gocommerce`, and since D25 they live
in [`core/`](../core/) rather than at the repo root — the directory moved, the
package clause did not. File discipline replaces package fragmentation.

Two reasons, and neither is taste:

- **Cycles.** A commerce domain is densely connected — checkout touches carts,
  inventory, orders, payments and the outbox in one transaction. Splitting that
  into packages produces import cycles, which are then broken with interfaces
  that exist only to break them (S3 forbids exactly that).
- **The module seam stays honest.** `ext/` packages are separate packages, so
  Go's own rules forbid them reaching into unexported internals. A bundled
  extension therefore proves the exported surface is sufficient, while
  `go build ./...` still covers the whole product (D23).

The domain services (`Catalog`, `Orders`, `Payments`, …) are concrete structs,
not interfaces. There is one implementation of each and it is not replaceable;
inventing an interface for it would buy indirection and nothing else.

## Modules are values, wired by hand (D5)

```go
type Module interface {
    Name() string
    Migrations() []Migration
    Register(app *App) error
}
```

That is the entire extension mechanism. `New` gives it a fixed, total ordering:

1. `Config.applyDefaults`, then module names validated (`[a-z0-9-]+`, unique).
2. Database opened, services built, built-in providers registered — through the
   same public surface a module uses, so the mechanism cannot rot unnoticed.
3. **Every** migration applied: core's first, then each module's in argument
   order. A module may therefore assume its own tables exist in `Register`.
4. Core routes mounted, before any module — so a core route always wins.
5. Each module's `Register`, in argument order.
6. The OpenAPI document merged, background work scheduled.

If module B needs module A's tables, the application author lists A first.
Explicit wiring extends to schema; there is no dependency resolver, and there
does not need to be one. See [integrations](integrations.md) for what `Register`
can actually do.

## Route namespacing is structural, not documented

A module may mount only `/x/<name>/…` (public) and `/api/admin/x/<name>/…`
(admin). The check in `module.go` runs against `App.current` — the module whose
`Register` is executing — not against a name the module passes in, so a module
cannot claim a core route or a neighbour's namespace even deliberately.

`HandleAdmin` wraps the handler in admin authentication as it mounts it.
Choosing the method *is* the authentication: there is no check to forget.

Wiring failures accumulate in `App.regErr` rather than panicking, and `New`
returns them, so a bad route is a clear startup error instead of a route that
silently is not served.

## The outbox, not a queue (D8, D9, D10)

Committing an order and then publishing its event are two operations. A process
that dies between them keeps the order and loses the event — a paid order
nobody was told about, and nothing in the system notices. So the event row is
written **inside the caller's transaction** (`outbox.write`, in the same
`InTx`), and a dispatcher publishes it after commit.

That buys durability without adding infrastructure. There is no Kafka, no
Redis, no separate worker to deploy: the queue is a table in the database that
already has to be up for the store to sell anything. Delivery is at-least-once,
which is why every handler must be idempotent (D9) — a redelivery after a crash
is normal operation, not an error.

Redis Streams remain an *optional transport module* (D10), never a prerequisite.
Mechanics, tuning and failure modes are in [infrastructure](infrastructure.md);
the event contract is in [`docs/events.md`](../docs/events.md).

Two rules follow and both are load-bearing:

- **Never publish a state change by calling the bus directly.** Write the row
  and the outbox row together, or the event is a lie waiting to happen.
- **Never hold a transaction across network I/O.** Checkout is two-phase for
  this reason: commit the order, *then* call the gateway. A gateway having a
  slow day must not become the database having a slow day. See
  [checkout](checkout.md).

## One repository, one Go module (D23)

`github.com/misiki/gocommerce` is the whole repo — that is still the module
path, though since D25 the engine package itself imports as
`github.com/misiki/gocommerce/core`. `github.com/jackc/pgx/v5` is
the only production dependency. There is no `go.work`, no nested `go.mod`, no
per-extension tags.

The rule that makes it possible: **`ext/` packages add zero third-party
dependencies.** Stripe, Razorpay, SendGrid, MSG91 and Shiprocket are all REST
over `net/http` with hand-written HMAC — that is the standard here, not a
hardship. MCP is JSON-RPC 2.0, which `encoding/json` speaks unaided.

Dependency isolation was the only thing a multi-module layout bought, and it is
bought more cheaply by not having the dependencies. **A module that genuinely
needs an SDK ships as its own repository** — a `Module` is a `Module`, and
nothing about the mechanism changes when it moves out.

This is enforced, not remembered: the `deps` job in
[`.github/workflows/ci.yml`](../.github/workflows/ci.yml) fails the build if
`go.mod` grows a second dependency or if anything under `ext/` imports a package
that is neither the standard library nor the engine.

## Where a new feature goes

PLAN §40 Rule 10 is the membership test, and it has exactly one question:

> **Does this belong to the commerce state machine, or can it be a module?**

If it can be a module without weakening a core invariant, it stays outside core.
Optionality is *not* the test — a feature does not belong in a module merely
because a store could live without it, and it does not belong in core merely
because most stores want it. What matters is whether moving it out fragments an
invariant.

In core because their invariants are entangled (S1): product, variant,
inventory, cart, checkout, order, payment state, fulfillment state, durable
events, admin authentication, the API contract.

Outside core, per PLAN §30: payment and carrier and vendor SDKs, Redis, search,
object storage, CMS, invoice rendering, customer identity, tax, promotions,
recommendations, analytics, AI models, storefront UI.

Two worked examples. `ext/cms` is a module because a store sells fine without
content pages and nothing in the order state machine cares about them. Variants
are core because moving them out would split SKU, price, stock, cart lines and
order lines across a boundary that a transaction has to cross (S6, D11) — see
[products](products.md).

Guest checkout is the standing constraint on this test (D22): core has no
customer concept and never will. An identity module may *add* authenticated
checkout; it may never make an account required. `superusers` is an operator
table, and nothing in the commerce path reads it.

## Common mistakes

- **Adding an interface for a domain service.** `Orders`, `Payments` and the
  rest are concrete on purpose (S3, D4). Ports exist only where composition
  actually happens — see `ports.go`.
- **Splitting core into packages to "tidy it up."** That is D6 in reverse, and
  it ends in cycles broken by interfaces nobody wanted.
- **Adding a dependency to `ext/`.** CI rejects it. The right answer is a
  hand-written REST client, or a satellite repo.
- **Writing a core table from a module or a script.** The state change and its
  event have to commit together; SQL that updates `orders` produces a state
  nothing was told about.
- **Emitting an event from a handler after the commit.** The crash window
  between the two is exactly what the outbox exists to close.
- **Growing `Config`.** Before adding a field, check the value is genuinely a
  store's decision rather than the engine's.
- **Treating `PLAN.md` as history.** It is the authority. A change that needs a
  decision broken records the new decision there rather than breaking it
  quietly.

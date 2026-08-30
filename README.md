# GoCommerce

**Commerce primitives in Go, without a commerce monolith.**

GoCommerce is a small, composable commerce engine. It owns the hard parts —
products and variants, inventory, carts, checkout, orders, payments,
fulfillment and durable events — over a PostgreSQL database. Integrations are
ordinary Go packages you wire together in `main()`.

> **Status: pre-1.0.** The engine and the modules below are implemented and
> tested. The API is unstable until `v0.1.0`; see [PLAN.md](PLAN.md) for the
> architecture and the road there.

## A store is a Go program

```go
app, err := gocommerce.New(
    gocommerce.Config{
        DBURL:       os.Getenv("DATABASE_URL"),
        Currency:    "USD",
        AdminTokens: []string{os.Getenv("GOCOMMERCE_ADMIN_TOKEN")},
    },
    stripe.New(stripe.Config{
        SecretKey:     os.Getenv("STRIPE_SECRET_KEY"),
        WebhookSecret: os.Getenv("STRIPE_WEBHOOK_SECRET"),
    }),
    sendgrid.New(sendgrid.Config{
        APIKey: os.Getenv("SENDGRID_API_KEY"),
        From:   "orders@example.com",
    }),
    invoices.New(invoices.Config{SellerName: "Example Ltd"}),
)
if err != nil {
    log.Fatal(err)
}
log.Fatal(app.ListenAndServe())
```

No plugin registry, no dependency-injection container, no reflection, no
configuration DSL. Everything a store runs is on that screen, and "go to
definition" works on all of it.

## The design in one page

**One binary, one PostgreSQL database.** That is the whole production
architecture. Redis, search and object storage are options you add when traffic
justifies them, never prerequisites.

**Durable events, not fire-and-forget.** A state change and the event
describing it are written in the same transaction, to an outbox table. A
process that dies between the commit and the publish loses nothing. A
dispatcher claims unsent rows with `FOR UPDATE SKIP LOCKED`, so several
instances never deliver the same event at once. Delivery is at-least-once, so
consumers are idempotent by contract — see [docs/events.md](docs/events.md).

**Variants are first class.** A variant is the sellable unit: SKU, price, stock
and order lines all hang off it. A product with no options still has one
default variant, so the simple case stays simple without a flat schema that has
to be torn up later. Two variants cannot claim the same option combination —
enforced by a unique index, not by hope.

**One state machine.** REST, a payment webhook, a CSV import, an MCP tool call
and an admin action all end up in the same domain service. No integration gets
to invent its own `MarkPaid`.

**Modules integrate; core decides.** A module owns its own tables, mounts
routes in its own namespace, and provides payments, fulfillment or
notifications. It may not write core commerce tables — it calls a service,
which performs the transition and writes the event.

**Guest checkout, permanently.** A shopper buys with a cart token and an email.
A future identity module may add accounts; it may never make one required.

## What is in the box

The engine, plus these modules — each an ordinary package under `ext/`, each
adding **zero third-party dependencies** (they talk REST over `net/http` and
verify HMACs with `crypto/hmac`, so no vendor SDK enters your dependency
graph):

| Module | Provides |
|---|---|
| `payments-stripe` | Card payments, signed webhooks, refunds |
| `payments-razorpay` | Cards and UPI, hosted or in-page |
| `notify-sendgrid` | Order email, templates you own |
| `notify-msg91` | Order SMS via DLT templates |
| `fulfill-shiprocket` | Booking shipments and waybills |
| `invoices` | Numbered, gapless invoices on payment |
| `cms` | Content pages, per language |
| `mcp` | The store as tools for an AI agent, with an audit trail |

Cash on delivery and manual fulfillment are built in, because they need no
third party — a store can sell and ship before it has integrated anything.

## API

Unversioned, JSON, and compatible with the useful overlap of the Litekart API
so existing storefronts can point at it with minimal changes.

```
GET  /health              GET /health/ready
GET  /doc                 the OpenAPI contract this build serves
GET  /docs                a browsable reference

GET  /api/products        /api/products/{id}  /api/products/slug/{slug}
                          /api/products/sku/{sku}
POST /api/carts           /api/carts/{cartId}/line-items
POST /api/checkout/{code} /api/checkout/{code}/webhook
GET  /api/orders/{number}?token=…

     /api/admin/products  /api/admin/orders  /api/admin/create-fulfillment
     /api/admin/import/…  /api/admin/export/…
```

Every response is `{"data": ...}` or `{"error": {"code", "message"}}` — including
the ones that miss, so a client decoding JSON never gets a plain-text 404
instead.

Every list paginates, either way you like: `?limit=20&offset=40` for a cursor,
`?limit=20&page=3` for a page number counting from 1. They describe the same
window, and when a request carries both, `page` wins. The `meta` block reports
both, so a UI can draw "3 of 12" without doing arithmetic:

```json
{ "data": [ ... ],
  "meta": { "total": 240, "limit": 20, "offset": 40, "page": 3, "total_pages": 12 } }
```

Money is always `{"amount_minor": 1999, "currency": "USD"}` — an integer and a
code, never a float and never a formatted string, because the decimal places
belong to the currency and the symbol belongs to the reader.

A test asserts that every route the engine mounts appears in `/doc`, so the
contract cannot quietly drift from the code.

## Quick start

Requires Go 1.23+ and PostgreSQL 16+.

```sh
createdb mystore
export DATABASE_URL=postgres://localhost/mystore
export GOCOMMERCE_ADMIN_TOKEN=$(openssl rand -hex 32)

go run ./cmd/gocommerce serve
```

Then sell something:

```sh
# Add a product
curl -X POST localhost:8080/api/admin/products \
  -H "Authorization: Bearer $GOCOMMERCE_ADMIN_TOKEN" \
  -d '{"title":"Cotton tee","status":"active","sku":"TEE-001","price_minor":2500,"stock":10}'

# Shop
CART=$(curl -sX POST localhost:8080/api/carts | jq -r .data.id)
curl -X POST localhost:8080/api/carts/$CART/line-items \
  -d '{"variant_id":1,"quantity":2}'

# Check out with cash on delivery
curl -X POST localhost:8080/api/checkout/cod \
  -H 'Idempotency-Key: demo-1' \
  -d '{"cart_id":"'$CART'","email":"buyer@example.com",
       "address":{"line1":"1 High St","city":"Town","postal_code":"12345","country":"US"}}'
```

## The admin panel

One executable serves both the API and a full admin panel. Run the binary,
open `http://localhost:8080/`, and you have a dashboard, product and order
management, inventory, CSV import/export and settings — with no separate
process, no Node.js on the server and no configuration beyond a database URL.

The panel owns the root, because the API is namespaced under `/api` (plus
`/health`, `/doc`, and a module's `/x/`) and nothing else wants that URL. The
consequence, stated plainly: this binary cannot also host a storefront at `/`.
A headless store's storefront is a separate application anyway — give it its
own origin, or put a proxy in front.

```powershell
.\scripts\build.ps1        # builds the panel, then embeds it in the binary
.\gocommerce.exe -db "$env:DATABASE_URL" -admin-token <token> serve
```

The panel is a SvelteKit single-page app compiled to static files and embedded
with `go:embed`. It is a client of the same public API as anything else: it has
no private endpoints, and everything it does you can do with curl. Sign-in is
the store's admin token, held in the browser and sent as a bearer token — there
is no session to expire, because there is no session.

Its design is a deliberate, close port of the
[PocketBase](https://github.com/pocketbase/pocketbase) dashboard: the same
IBM Plex Sans and Plex Mono, the same Remix Icon set, the same design tokens
(`#1055c9` accent, 5px and 15px radii, 45px controls, a 30/20 spacing scale),
and the same interaction grammar — hover moves one surface step at 150ms, a
press moves two at 70ms, panels arrive as a right-hand drawer, toasts rise from
the bottom and pause when you hover them. Fonts are self-hosted, so the panel
works on a machine that has never seen the internet.

Building `-tags no_admin` produces an API-only binary that carries no
JavaScript and leaves the root free.

## Running and testing a dev store

```powershell
.\scripts\dev.ps1 -Seed       # create the database, build, serve, load a demo catalog
.\scripts\dev.ps1 -Reset      # start again from empty
```

Then open `http://127.0.0.1:8080/` and sign in with the dev token
(`dev-token` unless you passed another).

Then pick whichever way of poking at it suits you:

**In a browser.** `http://127.0.0.1:8080/docs` renders the contract as a
browsable reference you can send requests from. `/doc` is the raw OpenAPI
document behind it.

**In your editor.** [`api.http`](api.http) is a working request collection —
open it in VS Code with the REST Client extension, or any JetBrains IDE, and
send requests inline. It threads ids from one response into the next, so the
cart and checkout blocks run in sequence, and it ends with a set of requests
that are *supposed* to fail so you can see the error shapes.

**As a script.** `.\scripts\smoke.ps1` walks a running store through a complete
sale and checks what each step should have changed — 36 assertions covering
variant uniqueness, stock movement, idempotent checkout, guest order access,
the refund refusal, CSV round-trip and contract coverage. It creates its own
product and removes it afterwards, so it is safe to re-run against a store with
data in it.

**With curl.**

```sh
curl localhost:8080/api/products

CART=$(curl -sX POST localhost:8080/api/carts | jq -r .data.id)
curl -X POST localhost:8080/api/carts/$CART/line-items -d '{"variant_id":2,"quantity":2}'

curl -X POST localhost:8080/api/checkout/cod -H 'Idempotency-Key: demo-1' \
  -d '{"cart_id":"'$CART'","email":"buyer@example.com",
       "address":{"line1":"1 High St","city":"Town","postal_code":"12345","country":"US"}}'

curl -H 'Authorization: Bearer dev-token' localhost:8080/api/admin/orders
```

## Development

```sh
go build ./...
go vet ./...
go test ./...        # database-backed tests skip without a test database
```

For the full suite, point `GOCOMMERCE_TEST_DB` at a scratch database. Each test
gets its own PostgreSQL schema, so packages running in parallel cannot tread on
each other; the database name must contain `test`:

```sh
createdb gocommerce_test
GOCOMMERCE_TEST_DB='postgres://localhost/gocommerce_test?sslmode=disable' \
  go test ./... -race -count=1
```

The suite includes the engine's reliability requirements as tests: concurrent
checkouts cannot oversell, a replayed webhook cannot double-settle, a
rolled-back transaction leaves no event, a failed consumer is retried rather
than losing one, and cancelling returns exactly the stock it should.

## Operational diagnostics

```sh
gocommerce doctor           # human-readable, exits non-zero if anything failed
gocommerce -json doctor     # the same report, for scripts and agents
```

Nine checks: the database and its pool, pending migrations, whether anyone can
still administer the store, outbox backlog and dead letters, stock held by
orders nobody will pay for, cart sweeping, catalog entries that cannot be
bought, provider registration, and whether the served routes match `/doc`.
Every failure names what to do about it.

It is a core service (`App.Diagnose`), so the CLI, the MCP `store_health` tool
and anything else render the same report.

## AI-native by design

AI is a first-class developer interface here, in three layers:

- **Rules** — [AGENTS.md](AGENTS.md) holds the architectural guardrails, with
  the reason each one exists. [CLAUDE.md](CLAUDE.md) and
  [`.cursor/rules`](.cursor/rules) point at it and add only tool-specifics.
- **Skills** — [`skills/`](skills/README.md) is task-scoped procedural
  knowledge, from checkout's two-phase transaction to the outbox's delivery
  guarantee, each page saying when to reach for it.
- **MCP** — [`ext/mcp`](ext/mcp) exposes domain tools that call the same
  services REST does. It never exposes arbitrary SQL and never mutates core
  tables directly, so an agent operating a store is bound by exactly the
  invariants a person is.

## Documentation

- [PLAN.md](PLAN.md) — architecture, decisions and milestones
- [AGENTS.md](AGENTS.md) — the rules any contributor works under
- [skills/](skills/README.md) — procedural guides, one per task area
- [docs/events.md](docs/events.md) — the event contract and the outbox guarantee
- [docs/writing-a-module.md](docs/writing-a-module.md) — building an extension
- [docs/admin-panel.md](docs/admin-panel.md) — the embedded admin UI
- [docs/operations.md](docs/operations.md) — running it in production
- [`examples/store`](examples/store) — the shape of a real store's `main()`

## License

Not yet chosen — MIT and Apache-2.0 are the candidates. Until then, all rights
are reserved; treat this as source-available for evaluation.

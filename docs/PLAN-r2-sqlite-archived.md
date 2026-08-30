# gocommerce — Project Plan

A minimal, extensible ecommerce platform in Go with SQLite. The core is an
extremely slim kernel — one Go package, one third-party dependency — that a
store extends by writing ordinary Go packages and wiring them in `main()`.

> **Revision R3 (2026-08-28):** the monorepo is gone — this repo is ONE Go
> module (D3). Guest checkout is a permanent guarantee (D14). Default currency
> is USD and default language `en`, both extensible (D9, D13).
>
> **Revision R2 (2026-08-27):** incorporates the adversarial review (30
> findings) plus two new requirements: OpenAPI docs served by core, and API
> routing kept compatible with the live Litekart API
> (`api.litekart.in/doc`).

**Design goals, in priority order:**

1. **Slim core.** Everything that can live outside the core, lives outside the
   core. Core's `go.mod` carries exactly one third-party dependency (the SQLite
   driver). The membership test for every feature: *"can a COD-only store in
   one country sell without it?"* If yes, it's out.
2. **Extensible without touching core.** Payments, queues, fulfillment
   carriers, notification providers, invoices, CMS, MCP — all are separate Go
   modules added by importing a package and passing one value to the app.
3. **Easy to develop.** `go build` works on a bare Windows machine with no C
   toolchain, no Docker, no external services. One flat core package, explicit
   wiring you can follow with go-to-definition, no reflection, no global
   registries, no config DSL. A new provider module is one small interface and
   one line in `main()`.

> **How this design was chosen:** three independent architecture proposals
> (a Caddy-style compile-time registry, an explicit-DI "mainboard", and a
> commerce-prior-art translation of Medusa/Vendure/Shopify ideas) were scored
> by three judges on slimness, extensibility, and pragmatics. Explicit DI won
> slimness (9/10) and pragmatics (8/10); the registry design lost slimness
> 4/10 as a "framework-in-disguise". This plan is the explicit-DI design plus
> the judges' graft list, fixes for blind spots all three proposals shared,
> and the R2 review fixes.

---

## 1. Architecture decisions

| # | Decision | Choice | Why |
|---|----------|--------|-----|
| D1 | Extension mechanism | Explicit wiring: `Module` interface, instances passed to `gocommerce.New(...)` in `main()`. No `init()` registration, no global registry, no config file. | One concept to learn; composition is greppable, typed, debuggable. The registry alternative adds a config DSL, capability discovery by type assertion (silent failures), and two composition paths — cost without payoff when the operator is already a Go developer. |
| D2 | Core package shape | ONE flat Go package `gocommerce` at the repo root. | Modules import exactly one package for every type they need, which makes import cycles impossible by construction. Splitting domain/store/http packages forces interface ceremony or cycles (the third proposal's drawn layout had a core↔products cycle). File discipline replaces package walls at ~5k LOC. |
| D3 | Repo layout | **ONE Go module, one repo.** Core at the root; bundled extensions are ordinary packages under `ext/`. No `go.work`, no nested modules, no per-extension tags. The rule that keeps it honest: **a package may live in `ext/` only if it adds ZERO third-party dependencies.** An extension that needs an SDK (`queue-redis`, `mcp`) ships as its own satellite repo instead. | Dependency isolation was the only thing multi-module bought, and only 2 of 9 planned extensions need it — Stripe, Razorpay, SendGrid, MSG91, Shiprocket, invoices and CMS are REST-over-`net/http` + HMAC with no SDK. Paying go.work/nested-tag/CI-matrix ceremony across seven packages to isolate two was a bad trade against goal 3. Bundled extensions still use only core's exported API (Go package boundaries forbid reaching into core's unexported internals), so they remain a real proof of the seams. |
| D4 | SQLite driver | `modernc.org/sqlite` (pure Go, driver name `"sqlite"`). | Zero CGO: builds on a stock Windows box, cross-compiles trivially. Rejected: `mattn/go-sqlite3` (CGO), `ncruces/go-sqlite3` (adds a wasm runtime), `zombiezen` (not database/sql). Cost accepted: ~1.5–2x slower than CGO SQLite — irrelevant at target scale. This is core's ONLY dependency. |
| D5 | HTTP router | stdlib `net/http` with Go 1.22+ `ServeMux` patterns (`GET /api/products/{id}`). | Method routing + path params is all a JSON API needs. Middleware is plain `func(http.Handler) http.Handler`. |
| D6 | Events | Synchronous in-process bus behind a `Bus` interface; payloads are JSON from birth; **publish only after the enclosing DB transaction commits**. | The interface is the async seam: `queue-redis` implements `Bus` and is swapped in via `Config.Bus` — zero handler changes. JSON payloads mean an out-of-process bus ships events unchanged. |
| D7 | Module namespacing | Route/migration namespacing is enforced structurally: while a module's `Register` runs, the App records the current module and rejects routes outside `/x/<name>/` (public) or `/api/admin/x/<name>/` (admin, auto-auth-wrapped); the exact paths `/x/<name>` and `/api/admin/x/<name>` (no trailing segment) also pass. Table `<name>_` prefixes ('-'→'_') are **convention**, review-enforced (structural enforcement would require DDL parsing). | "Documented, not enforced" was the winning proposal's admitted security gap (a forgotten admin-wrap is a structural bug). Current-module tracking closes it for routes without trusting the module — and module routes can never collide with core routes. |
| D8 | State ownership | Modules NEVER write core tables. Settlement is one core call: `app.Payments().MarkPaid(...)`. Providers translate external reality into core service calls; core owns every state transition and event emission. | Invariants live in one place. A Stripe webhook is ~30 lines because the state machine isn't its problem. |
| D9 | Money & currency | `INTEGER` **minor units** in `*_minor` columns (never `*_cents` — JPY has 0 decimals, KWD has 3); ONE settlement currency per store, `Config.Currency`, **default `"USD"`**; the code is snapshotted on every order so history stays self-describing if the store's currency ever changes. No per-product currency. The API returns `{amount_minor, currency}` and never a formatted string — the exponent and the `$` are the client's business, which is what makes any currency work without core changes. | Mixed-currency carts are an invariant nobody needs at this scale, and a currency column without a mixed-cart rule is a latent bug. Extensibility lives at the edges instead: any ISO code works today; presentment/multi-currency price lists are a future pricing module's job, and the order-level currency snapshot is the seam that makes that possible without a core migration. |
| D10 | Migrations | No library. Forward-only, per-module namespace `<module>/<id>`, tracked in `schema_migrations`, core first then the order modules are passed to `New`. `Migration{ID, SQL}` or `Migration{ID, Run: func(ctx, tx)}` for data fixes. | ~80 lines of core code. "If module B needs module A's tables, list A first" — explicit wiring extends to schema. |
| D11 | API routing | **Litekart-compatible**: unversioned `/api/…` public prefix, `/api/admin/…` admin prefix, `/health`, cart `line-items`, per-provider `POST /api/checkout/{code}` (+ `/webhook`), `/api/admin/export/…` and `/api/admin/import/…`. Deliberate divergences are listed in §8. | Existing Litekart storefronts and tooling hit these exact routes (`api.litekart.in/doc` is the reference). Compatibility is a requirement, not a taste. |
| D12 | OpenAPI | Hand-maintained `openapi.json` embedded via `go:embed`, served at `GET /doc`; a static Scalar HTML page (one CDN script tag, zero Go deps) served at `GET /docs`. Modules MAY implement the optional `interface{ OpenAPI() []byte }` returning a JSON fragment (`paths` + `components`) that core merges via `encoding/json` at startup. Spec linting (spectral) runs CI-side only. | Mirrors Litekart's own docs shape (`/doc` + Scalar at `/docs`). Codegen/runtime OpenAPI libraries would violate D4's one-dependency rule; JSON-is-valid-YAML plus `encoding/json` merging keeps the whole feature dependency-free. Use `{param}` style throughout — do not inherit Litekart's mixed `:param`/`{param}`. |
| D13 | Language | `Config.DefaultLanguage` **default `"en"`**, `Config.Languages` default `["en"]`. Core negotiates a language per request (`?lang=` wins, else `Accept-Language`, else the default) with hand-rolled primary-subtag matching (~25 lines; `golang.org/x/text` is a dependency we won't take), snapshots it on the order, and passes it to notifiers. Core stores content in ONE language. Translations are OUT of core: an optional `Translator` port lets an `i18n` module supply per-language field overrides, which core applies when serializing catalog content. | A single-language store must sell without paying for i18n (the membership test), but "extensible" was a stated requirement, so the seam is built, not speculated. Batch-by-ids in the port keeps a list response to one translator call, not N+1. |
| D14 | Guest checkout | **Permanent core guarantee**, not a v1 simplification: a shopper buys with a cart token and an email — no account, ever. A future accounts module may ADD identified checkout and order history; it may never make an account required to buy, and core carries no code path that assumes a customer record exists. | It's a hard requirement, and stating it as an invariant now constrains the accounts module's design before it exists — which is the only time such a constraint is free. |

---

## 2. Repository layout

```
gocommerce/                          # repo root
├── go.mod                           # module github.com/misiki/gocommerce — deps: modernc.org/sqlite. That's all.
├── PLAN.md                          # this file
├── gocommerce.go                    # App, Config, New, ListenAndServe, OnStart/OnStop
├── module.go                        # Module, Migration, namespace enforcement
├── ports.go                         # PaymentProvider, FulfillmentProvider, Notifier, Bus (+ Refunder at M5)
├── events.go                        # Event, SyncBus, event-name consts, payload structs
├── db.go                            # OpenDB → DB{R,W}, pragmas, WAL checkpointer, Windows path→URI normalization
├── migrate.go                       # migrator + schema_migrations
├── schema.go                        # core's own []Migration
├── httpx.go                         # JSON envelope, error taxonomy, adminAuth, pagination, body limits
├── openapi.go  openapi.json         # /doc (embedded spec + module-fragment merge) and /docs (Scalar page)
├── product.go   product_http.go
├── cart.go      cart_http.go        # incl. TTL sweeper
├── order.go     order_http.go       # incl. unpaid-order sweeper (see §3 Orders)
├── checkout.go                      # cart→order tx, commit-then-Initiate, idempotency keys
├── payment.go                       # Payments service (MarkPaid/MarkFailed; Refund at M5) + built-in COD
├── fulfillment.go                   # Shipments + built-in "manual" provider
├── notify.go                        # event→Notification dispatcher + LogNotifier
├── i18n.go                          # language negotiation + Translator application (D13)
├── transfer.go transfer_products.go transfer_orders.go   # CSV import/export
├── backup.go                        # VACUUM INTO
├── gctest/                          # module-author test kit (same core module)
│   └── gctest.go                    # gctest.New(t, cfg, mods...) → *App on temp-file DB; RecordingBus
├── cmd/gocommerce/main.go           # reference binary: serve|migrate|import|export|backup
├── docs/
│   ├── events.md                    # THE frozen event-name/payload contract
│   ├── writing-a-module.md
│   ├── operations.md                # backup/restore, Windows notes, deployment
│   └── litekart-openapi.json        # vendored snapshot of api.litekart.in/doc (the D11 reference)
├── examples/store/                  # the canonical wired main() — same module, no go.mod of its own
└── ext/                             # bundled extensions: ordinary packages, ZERO third-party deps (D3)
    ├── payments-stripe/             # package stripe — REST + HMAC, no SDK
    ├── payments-razorpay/
    ├── notify-sendgrid/   notify-msg91/
    ├── fulfill-shiprocket/
    └── invoices/   cms/   i18n/
```

**Satellite repos** — the only extensions that leave, and only because they
carry a third-party dependency:

```
gocommerce-redis/    # queue-redis: events.Bus on Redis Streams (needs a Redis client)
gocommerce-mcp/      # mcp: needs the official Go MCP SDK
```

They are `Module` implementations like any other — nothing about the extension
mechanism changes when a package moves out. If keeping one repo ever matters
more than the no-nested-modules rule, either can become a nested module
instead; that call belongs to M6/M7, not now.

Releases: ONE tag, `vX.Y.Z`, covering core and every bundled extension — they
version together, which is exactly right when they're maintained together.
Satellite repos pin a core version in their own `go.mod` and tag themselves.
CI: `windows-latest` + `ubuntu-latest` from M0, and `go build ./...` /
`go test ./...` really do cover the whole product. A CI check asserts core's
`go.mod` still lists exactly one requirement and that nothing under `ext/`
imports a third-party package — D3's rule, enforced rather than remembered.

---

## 3. Core scope

**IN the kernel:**

- **Products** — flat catalog: `sku` (unique, the import/export key), title,
  description, `price_cents`, stock, active flag, images (child table, URLs
  only), `metadata` JSON column (see §7, the module-data seam). Public lookup
  by id AND by sku (`GET /api/products/sku/{sku}` — Litekart parity, and sku
  is already the catalog key). No variants in v1 (declared later-core
  milestone — it touches order-line schema).
- **Carts** — guest carts keyed by a crypto/rand 128-bit URL-safe token (the
  token is the cart's public `{cartId}`); add/update/remove line items; price
  snapshot at add-time; TTL sweeper goroutine deletes stale carts
  (`Config.CartTTL`, default 30 days) — the public `POST /api/carts` endpoint
  must not be an unbounded-growth vector.
- **Checkout** — `POST /api/checkout/{code}` (provider selected by path, per
  Litekart). Two phases, explicitly:
  1. **The write transaction:** re-validate lines against current product
     state, snapshot lines, decrement stock, consume cart, create order
     (`status` per provider kind, `payment_status` "pending"), commit.
  2. **After commit:** `PaymentProvider.Initiate` runs OUTSIDE the
     transaction — a gateway network call must never hold the single writer
     connection. On Initiate failure the order stands, `payment_status`
     stays "pending", and the response is the error envelope plus the order
     reference; a retry with the same `Idempotency-Key` re-runs Initiate for
     that order instead of creating a new one.
  **Re-validation policy (customer-facing, fixed):** if any line's current
  price differs from its snapshot, or its product is inactive, or stock is
  insufficient, checkout fails `409 ErrConflict` with per-line details
  (`{product_id, reason: price_changed|inactive|insufficient_stock,
  current_price_cents}`) and the cart's snapshots are refreshed to current
  values — the client re-displays and the shopper re-confirms. Current price
  always wins; core never silently reprices a confirmed order.
  `Idempotency-Key` maps to the created order + stored response (including
  the PaymentIntent), so a mobile double-tap replays instead of
  double-ordering.
- **Orders** — `status`: pending → confirmed → shipped → delivered,
  cancellable from pending/confirmed (cancel restocks). **Transitions into
  `confirmed`:** COD auto-confirms at checkout; `Payments.MarkPaid`
  auto-confirms a pending order (this is the gateway path — without it a paid
  order could never ship). Cancelling a paid order does NOT auto-refund;
  refunds are explicit and arrive with the refund surface at M5. Separate
  `payment_status`: pending → paid | failed (| refunded from M5). Totals:
  `subtotal_cents + shipping_cents − discount_cents = total_cents` — the
  columns exist from day one. `shipping_cents` is fed by
  `Config.FlatShippingCents`; `discount_cents` is **always 0 in core v1** —
  the named invariant a future discounts module relaxes. Customer fields:
  `email`, `phone`, and a structured `address` JSON object with fixed keys
  `{name, phone, line1, line2, city, state, postal_code, country}` — typed
  now because Shiprocket (M6) needs fields, not a blob. **Unpaid-order
  sweeper:** orders still `status=pending` after `Config.UnpaidOrderTTL`
  (default 24h) are auto-cancelled and restocked (publishes
  `order.cancelled`) — an abandoned gateway redirect must not hold stock
  forever. COD orders are unaffected (they confirm instantly). Implemented
  with the first gateway at M5, specified now.
- **Payments** — `PaymentProvider` port + built-in **COD**: `Initiate`
  returns `Kind:"none"`, order auto-confirms, `payment_status` stays pending
  until admin `mark-paid` at delivery. `Payments.MarkPaid/MarkFailed` is the
  ONLY settlement path for everyone. **Refunds are deferred to M5** — the
  `Refunder` port, `Payments.Refund`, the `refunded` status, and the admin
  refund endpoint all land with `payments-stripe` (the first real Refunder),
  under M5's fix-core-pre-freeze rule. Core v1 ships nothing refund-shaped
  it cannot itself exercise.
- **Fulfillment** — `FulfillmentProvider` port + built-in **manual**: admin
  creates a fulfillment with a typed tracking number; deliver/cancel
  endpoints; core persists shipments, enforces the status machine, publishes
  events.
- **Notifications** — the ABSTRACTION only: a dispatcher subscribed to
  `order.*` builds channel-agnostic `Notification` values and fans out to
  per-channel notifiers. **Channels are a closed core-defined set in v1:
  `"email"` and `"sms"`** (the dispatcher must know which address field feeds
  `To`); anything else subscribes to the bus directly. Core ships a log-only
  notifier. Templates belong to notifier modules — core hands them flat
  data, never rendered copy.
- **Events** — `Bus` interface + synchronous in-process implementation + the
  frozen `order.*` taxonomy (see §6).
- **CSV import/export** — products and orders, stdlib `encoding/csv`, CLI and
  admin API, streaming both directions (see §8, §9).
- **OpenAPI** — embedded `openapi.json` served at `GET /doc`; Scalar docs
  page at `GET /docs`; optional module fragments merged at startup (D12).
  The spec grows with every milestone from M0's skeleton onward.
- **Admin auth** — static bearer token(s), constant-time compare; refuses to
  boot with no token unless `Config.Dev` (the `--dev` flag on the reference
  binary sets it). `Config.AdminAuth` accepts a replacement middleware — the
  seam a future accounts/RBAC module needs to exist in core or it can never
  be a module.
- **Ops** — `backup` command (`VACUUM INTO`), WAL checkpointer, slog logging,
  graceful shutdown, `GET /health` + `GET /health/ready` (Litekart parity —
  ready additionally checks the DB answers a trivial read).

**Config (the complete surface — nothing configurable exists outside it):**

```go
type Config struct {
	DBPath, Addr, Currency string
	AdminTokens []string                        // ≥1 required unless Dev
	AdminAuth   func(http.Handler) http.Handler // nil → built-in bearer middleware
	Bus         Bus                             // nil → built-in SyncBus
	FlatShippingCents int64
	HandlerTimeout    time.Duration // per event handler on SyncBus; default 10s
	CartTTL           time.Duration // default 30 days
	UnpaidOrderTTL    time.Duration // default 24h (see §3 Orders; active from M5)
	OrderPrefix       string        // human order numbers: prefix + sequence
	Dev               bool          // permit boot without AdminTokens
}
```

**OUT of the kernel** (module or non-goal): every real payment gateway, every
real email/SMS sender, every carrier API, async dispatch, invoices, CMS, MCP,
customer accounts, variants, tax/discount engines, shipping-rate calculation,
multi-currency, image storage (URLs only), search beyond `LIKE`, outbound
webhooks, storefront/admin UI, rate limiting, metrics beyond slog — and,
until M5, refunds.

---

## 4. The extension mechanism

One mechanism for everything. A module is a value implementing:

```go
type Module interface {
	// Name is unique per app (duplicate names are a startup error), lowercase
	// [a-z0-9-]. With '-'→'_' it is the conventional prefix for the module's
	// tables; it namespaces its migration IDs ("stripe/0001_events") and its
	// routes (/x/<name>/).
	Name() string
	// Migrations run after core's, in the order modules are passed to New.
	Migrations() []Migration
	// Register wires the module: providers, routes, subscriptions.
	// Called once, after ALL migrations have been applied.
	Register(app *App) error
}
```

Optional capabilities, detected by type assertion at startup (each is logged
when found):

```go
interface{ OpenAPI() []byte }  // JSON fragment {paths, components} merged into /doc (D12)
```

The composition surface a module uses inside `Register`:

```go
func (a *App) RegisterPayment(p PaymentProvider)           // duplicate Code() of a builtin replaces it; of another module = startup error
func (a *App) RegisterFulfillment(f FulfillmentProvider)   // same collision rule
func (a *App) RegisterNotifier(channel string, n Notifier) // "email"|"sms" (closed set, §3); appends — all notifiers on a channel receive; LogNotifier only fires on channels with none
func (a *App) Handle(pattern string, h http.Handler)       // full pattern incl. method and absolute path ("POST /x/stripe/callback"); core VALIDATES the /x/<name>/ prefix — it does not rewrite
func (a *App) HandleAdmin(pattern string, h http.Handler)  // same contract under /api/admin/x/<name>/, auth auto-wrapped
func (a *App) Bus() Bus                                    // Subscribe during Register; Publish anytime
func (a *App) DB() *DB                                     // DB{R,W} for the module's own tables
func (a *App) Log() *slog.Logger                           // pre-scoped: With("module", name)
func (a *App) OnStart(fn func(context.Context) error)      // long-runners, after migrate, before serve
func (a *App) OnStop(fn func(context.Context) error)       // reverse order
// Core services — modules act on the domain through these, never via SQL on core tables:
func (a *App) Products() *Products
func (a *App) Orders()   *Orders
func (a *App) Carts()    *Carts
func (a *App) Payments() *Payments   // MarkPaid / MarkFailed (Refund from M5)
```

The app author's ENTIRE `main()`:

```go
func main() {
	app, err := gocommerce.New(gocommerce.Config{
		DBPath: "store.db", Addr: ":8080", Currency: "INR",
		AdminTokens: []string{os.Getenv("GC_ADMIN_TOKEN")},
		// Bus: nil → built-in SyncBus; or redisqueue.New(...) to go async
	},
		stripe.New(stripe.Config{SecretKey: os.Getenv("STRIPE_SK"), WebhookSecret: os.Getenv("STRIPE_WHS")}),
		sendgrid.New(sendgrid.Config{APIKey: os.Getenv("SENDGRID_KEY"), From: "orders@shop.example"}),
	)
	if err != nil {
		log.Fatal(err)
	}
	if err := app.ListenAndServe(); err != nil { // returns nil on clean signal shutdown
		log.Fatal(err)
	}
}
```

**Lifecycle order** (fixed, in `New` + `ListenAndServe`): open DB → core
migrations → module migrations in the order modules are passed to `New` →
wire core (services, dispatcher subscriptions — plain code, readable top to
bottom) → `Register` each module in order (namespace-enforced; startup log
prints every wiring: `module stripe provides payment method "stripe"`) →
merge module OpenAPI fragments → `OnStart` hooks → serve → on signal,
`OnStop` in reverse, close DB.

**The bus is a `Config` field, not a module**, because it must exist before
any `Register` subscribes. If the supplied Bus implements the optional
`interface{ Start(context.Context) error; Stop(context.Context) error }`, the
App runs it — this answers "who starts the Redis consumer."

**Module-to-module composition is hand-wired in `main()`** — if the MCP module
should expose invoice tools, the author passes them in:
`mcp.New(mcpCfg, mcp.WithTools(invoices.Tools()...))`. No discovery, no
registry; honest and greppable. The same rule covers alternate run modes: the
mcp module's stdio mode is `mcp.ServeStdio(app)` called from `main()` in
place of `app.ListenAndServe()` — modules cannot add CLI subcommands.

---

## 5. Ports

```go
// ---- payment ----
type PaymentProvider interface {
	Code() string // "cod", "stripe", "razorpay" — POST /api/checkout/{code} selects by this
	// Initiate runs AFTER the checkout transaction has committed (§3) —
	// never on the writer connection. opts carries client pass-through so
	// gateways get what they need without core body changes.
	Initiate(ctx context.Context, o *Order, opts PayOptions) (PaymentIntent, error)
}
// Optional capability: a provider that receives gateway webhooks. Core routes
// POST /api/checkout/{code}/webhook to it with the RAW body (Litekart-
// compatible path; signature verification is the handler's job).
// interface{ Webhook() http.Handler }
type PayOptions struct {
	ReturnURL string            // storefront return for redirect flows
	Data      map[string]string // checkout body's payment_data, passed through verbatim
}
type PaymentIntent struct {
	Kind       string            // "none" (COD) | "client_action" | "redirect"
	Ref        string            // provider's id; persisted on the order
	ClientData map[string]string // {"client_secret": ...} or {"url": ...}
}
// Refunder is added to ports.go at M5 with payments-stripe (§3 Payments):
//   type Refunder interface { Refund(ctx, o *Order, amountCents int64) error }

// ---- fulfillment ----
type FulfillmentProvider interface {
	Code() string // "manual", "shiprocket"
	// Ship backs POST /api/admin/create-fulfillment. Core persists the
	// Shipment, sets status=shipped, publishes order.shipped.
	Ship(ctx context.Context, o *Order, req ShipRequest) (Shipment, error)
}
type ShipRequest struct {
	Tracking string            // manual: admin-typed
	Meta     map[string]string // carrier options pass-through (courier id, pickup slot…)
}
type Shipment struct{ Provider, Tracking, LabelURL string }

// ---- notifications ----
type Notifier interface {
	Notify(ctx context.Context, n Notification) error
}
type Notification struct {
	Event   string            // "order.paid"
	Channel string            // "email" | "sms" — closed set in v1 (§3)
	To      string            // address or phone for the channel
	Data    map[string]string // flat: order_number, total, customer_name, items_summary…
}

// ---- events ----
type Event struct {
	ID   string          `json:"id"` // random; at-least-once consumers dedup on it
	Name string          `json:"name"`
	V    int             `json:"v"`  // payload schema version, starts at 1
	At   time.Time       `json:"at"`
	Data json.RawMessage `json:"data"`
}
type Handler func(ctx context.Context, e Event) error
type Bus interface {
	Publish(ctx context.Context, e Event) error
	Subscribe(pattern string, h Handler) // exact, "order.*", or "*"; composition-time only
}

// ---- migrations ----
type Migration struct {
	ID  string                                      // "0001_init"; recorded as "<module>/<id>"
	SQL string                                      // applied in one tx; OR:
	Run func(ctx context.Context, tx *sql.Tx) error // Go data fixes; exactly one of SQL/Run
}
```

---

## 6. Events

**Taxonomy (frozen at M5 in `docs/events.md`):** `order.created`,
`order.paid`, `order.shipped`, `order.delivered`, `order.cancelled`. Payload
structs live in `events.go` and are shared by producers and subscribers.
The pending→confirmed transition deliberately carries **no separate event**:
COD confirmation coincides with `order.created`, gateway confirmation with
`order.paid` — a subscriber that cares about "confirmed" listens to those
two. Nothing else is frozen until a consumer exists — no speculative
`product.*` contract surface.

**Delivery contract (documented, load-bearing):**

- Core publishes **after the enclosing transaction commits** — an event never
  describes a state that rolled back.
- Sync bus: handlers run inline in registration order; handler errors are
  logged, never propagated (a dead notifier must not fail a checkout); each
  handler runs under a deadline (`Config.HandlerTimeout`, default 10s) so a
  hanging SendGrid call can't pin a checkout request for 30s.
- Delivery is **at-most-once** on the sync bus (crash between commit and
  publish loses the event — no outbox in core, deliberately) and
  **at-least-once** on queue buses. Therefore all handlers must be idempotent
  from day one, keyed on `Event.ID` where needed. Consumer modules that need
  certainty (invoices) reconcile against core tables on start.

---

## 7. Data layer

**Connections** — the proven single-writer pattern:

```go
// dbURI normalizes the path for a file: URI — forward slashes, and
// percent-escapes '?', '#', '%' — so D:\data\store.db works (Windows-first!).
base := "file:" + dbURI(path) + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)" +
	"&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"
w, _ := sql.Open("sqlite", base+"&_txlock=immediate") // BEGIN IMMEDIATE: no lock-upgrade deadlocks
w.SetMaxOpenConns(1)                                  // THE writer — writes serialize in Go, fairly
r, _ := sql.Open("sqlite", base+"&mode=ro")           // reader pool physically cannot write
r.SetMaxOpenConns(max(4, runtime.NumCPU()))
r.SetConnMaxIdleTime(time.Minute)                     // fd hygiene (read-marks are held only during read txs)
```

All mutations go through `DB.W` in transactions. `busy_timeout` exists for the
legitimate second process: the CLI importing CSV against a live server's file.
A background ticker runs `PRAGMA wal_checkpoint(TRUNCATE)` hourly and **logs
the result** — TRUNCATE can return busy under constant read load and simply
retries next tick; the goal is that an always-busy reader pool can't starve
checkpointing indefinitely and grow the `-wal` file without bound.

**Schema conventions** — INTEGER primary keys; opaque random tokens where the
public needs a handle (cart token, per-order guest access token); orders get a
human `number` (`Config.OrderPrefix` + sequence) and a nullable UNIQUE
`external_ref` (the CSV platform-migration key, §9); money INTEGER minor
units; timestamps TEXT RFC3339 UTC; the order `address` is a JSON object with
the fixed keys in §3.

**Core tables:** `products`, `product_images`, `carts`, `cart_items`,
`orders`, `order_items`, `shipments`, `checkout_keys`, `schema_migrations`.
`order_items` are snapshots (sku text, title, price copied at checkout) with
a nullable `product_id` — imported historical orders may reference products
that no longer exist.

**The module-data seam:** `products.metadata` and `orders.metadata` are JSON
TEXT columns (default `{}`). Modules read/write them through core services
(`Products.SetMetadata(id, key, value)`-style, key-namespaced by module name)
— this is how Shiprocket stores weight/dims and invoices stores GST/HSN
without core schema changes, and CSV export/import carries a `metadata` column
so module data survives the spreadsheet round-trip. Modules with real
relational needs create their own `<name>_` tables; they may store core IDs
but MUST NOT declare cross-boundary FOREIGN KEYs — a module can be added or
removed without core schema entanglement (orphaned tables and
`schema_migrations` rows are inert, documented).

**Module removal semantics** (documented): orders whose `payment_provider`
refers to a no-longer-compiled module remain fully manageable — status
transitions, `mark-paid`, cancel — because core owns the state machine;
only new `Initiate` calls for that code fail (400 with a clear error).

---

## 8. HTTP API

**Routing contract (D11):** Litekart-compatible — unversioned `/api/`,
admin under `/api/admin/`, reference spec `api.litekart.in/doc`. Deliberate
divergences (documented in the served spec): guest order access uses
`?token=`; `deliver`/`cancel`/`mark-paid` are explicit action routes (Litekart
spreads these across `PATCH /api/admin/orders/{id}` and `post-sale/*`); order
CSV import is `POST /api/admin/import/orders` (Litekart has no order import).

JSON envelope everywhere: `{"data": ...}` / `{"error": {"code": "...",
"message": "..."}}`, mapped from a small error taxonomy (`ErrNotFound`,
`ErrConflict`, `ErrValidation`). All list endpoints paginate:
`?limit=` (default 50, max 200) `&offset=`, response carries `total`.
`http.MaxBytesReader` on every core body (1MB JSON, 32MB CSV) — **from M0's
skeleton, not retrofitted**. Server Read/Write/Idle timeouts set from M0.
Slog request logging, panic-recover → JSON 500.

**Public:**

```
GET  /health                             GET /health/ready
GET  /doc                                OpenAPI 3.0 (embedded + module fragments)
GET  /docs                               Scalar reference UI
GET  /api/products                       ?q=&limit=&offset=   (active only)
GET  /api/products/{id}
GET  /api/products/sku/{sku}
POST /api/carts                          → {cart_id}          (the opaque token)
GET  /api/carts/{cartId}
POST /api/carts/{cartId}/line-items      {product_id, qty}
PATCH|DELETE /api/carts/{cartId}/line-items/{lineItemId}
POST /api/checkout/{code}                Idempotency-Key header; code = "cod" | module codes
     {cart_id, email, phone, name, address: {name,phone,line1,line2,city,state,postal_code,country},
      payment_data: {...}}
     → {order: {number, access_token, totals...}, payment: PaymentIntent}
POST /api/checkout/{code}/webhook        → provider's optional Webhook() handler, RAW body
GET  /api/orders/{number}?token=…        guest order status via access token
```

**Admin** (bearer token; multiple tokens supported for rotation):

```
GET|POST /api/admin/products             GET|PATCH|DELETE /api/admin/products/{id}
GET  /api/admin/orders                   ?status=&from=&to=&limit=&offset=
GET  /api/admin/orders/{id}
POST /api/admin/create-fulfillment       {order_id, provider: "manual", tracking: "...", meta: {...}}
POST /api/admin/orders/{id}/deliver | /cancel | /mark-paid
GET  /api/admin/export/admin-products    POST /api/admin/import/products
GET  /api/admin/export/admin-orders      POST /api/admin/import/orders
     (import/export accept ?dry_run=1; order import accepts ?fire_events=1 — §9)
```

**Modules** (namespace enforced at Register, see D7):

```
/x/<name>/…                    public  (GET /x/cms/pages/{slug})
/api/admin/x/<name>/…          admin, auth auto-wrapped
/api/checkout/{code}/webhook   core-owned dispatch to a payment provider's Webhook() — the
                               Litekart-compatible gateway webhook path (D11)
```

Webhook note (the first thing the Stripe module hits): the dispatched handler
gets the raw body — core applies no body-consuming middleware and no
MaxBytesReader to `/x/` routes or webhook dispatch (modules set their own
limits; `docs/writing-a-module.md` covers signature verification +
body-size limits).

---

## 9. CSV import/export

Core files `transfer*.go`, stdlib `encoding/csv`, streaming both directions,
same code behind CLI and admin API. Imports batch into 500-row writer
transactions (a big file never pins the single writer end-to-end); row errors
report `{line, msg}` and never abort the stream; dry-run supported
(`--dry-run` / `?dry_run=1`). Export guards against spreadsheet formula
injection by prefixing cells beginning with `=`, `+`, `-`, `@` with a single
`'`; **import strips exactly one leading `'`** — the escape is reversible, so
export → edit → import round-trips losslessly (the proof test in §11 asserts
equality after unescaping).

- **Products:** columns `sku,title,description,price_cents,stock,active,images,metadata`
  (images pipe-separated). Import **upserts by sku** — export → edit in Excel →
  import is the catalog-management loop and is tested as such.
- **Orders:** export is one row per order item (order columns repeated) — the
  shape accountants actually want. Columns:
  `number,external_ref,created_at,status,payment_status,payment_provider,`
  `email,phone,name,address_line1,address_line2,city,state,postal_code,country,`
  `subtotal_cents,shipping_cents,discount_cents,total_cents,`
  `item_sku,item_title,item_qty,item_price_cents,metadata`.
  Import groups rows by `external_ref` when present, else by `number`;
  native exports are re-importable (that's the round-trip test). Imported
  numbers are kept if supplied and non-colliding, else assigned from the
  sequence. Item skus are stored as snapshot text with `product_id` linked
  when the sku exists (§7). Orders are created in their stated status and
  **fire NO events by default** (`--fire-events` / `?fire_events=1` to opt
  in) — migrating 5k historical orders must not send 5k confirmation emails.

```
gocommerce -db store.db serve
gocommerce -db store.db import products products.csv [--dry-run]
gocommerce -db store.db export products > products.csv
gocommerce -db store.db import orders legacy.csv [--fire-events]
gocommerce -db store.db export orders --from 2026-01-01 > orders.csv
gocommerce -db store.db backup -o backup-2026-08-27.db
```

(Note: `--fire-events` on the reference CLI dispatches only to what that
binary wires — core alone means LogNotifier. A store whose modules should
see the events imports through the admin API of its own running binary.)

---

## 10. Operations (docs/operations.md ships at M4)

- **Backup:** `gocommerce backup` runs `VACUUM INTO` (safe on a live WAL DB) —
  the .db file IS the business. Docs cover restore, a scheduled-task example
  (Windows Task Scheduler / cron), and point at Litestream for continuous
  replication.
- **Windows realities:** keep the DB out of OneDrive/Dropbox-synced and
  AV-hot-scanned folders (sync clients take file locks that masquerade as
  SQLITE_BUSY/corruption); run as a service via NSSM or `sc.exe`; CI runs
  `windows-latest` from M0 so none of this is theoretical.
- **Retention:** cart TTL sweeper + unpaid-order sweeper (core); idempotency
  tables (`checkout_keys`, module webhook-event tables) get documented
  pruning guidance.

---

## 11. Testing

- Core runs with **zero modules**; the full purchase path is an integration
  test over `httptest` + a temp-file DB. (Never `:memory:` — each pooled
  connection gets a private in-memory DB; temp files match production.)
- `gctest` package (part of core — the module-author kit, so every third-party
  author doesn't re-derive the harness): `gctest.New(t, cfg, mods...)` boots an
  App on a temp DB; `gctest.RecordingBus` asserts event emission; helpers for
  admin-authed requests.
- Named proof tests: concurrent checkout against 1 unit of stock oversells
  nothing (the serialized writer proves itself); a failed `Initiate` leaves a
  pending order that an Idempotency-Key retry resumes; Stripe webhook replay
  through `/api/checkout/stripe/webhook` is idempotent; the purchase-path
  suite runs green under SyncBus AND queue-redis with zero handler changes
  (M6's headline assertion); CSV round-trip is lossless after unescaping,
  including metadata; `/doc` validates as OpenAPI 3.0 in CI (spectral) and
  contains every registered core route.

---

## 12. Planned extension modules

| Module | Mechanism it uses | One-line design |
|---|---|---|
| `payments-stripe` | `RegisterPayment` + `Webhook()` capability + own table | `Initiate` creates a PaymentIntent (metadata carries order id); webhook arrives via core's `POST /api/checkout/stripe/webhook` dispatch, verifies signature, `INSERT OR IGNORE` into `stripe_events` for idempotency, then `Payments().MarkPaid` — never touches `orders`. Implements `Refunder` (the port lands in core at M5 with this module). ~200 lines. |
| `payments-razorpay` | same | Checkout-JS flow: `PaymentIntent{Kind:"client_action"}` with order id in `ClientData`; `PayOptions.ReturnURL` for the hosted/redirect variant (`Kind:"redirect"`). |
| `notify-sendgrid` | `RegisterNotifier("email", …)` | One HTTPS POST to SendGrid v3 — zero third-party deps; owns its templates, renders from `Notification.Data`. |
| `notify-msg91` | `RegisterNotifier("sms", …)` | Same shape, SMS channel. Postmark/Resend are the same module pattern. |
| `queue-redis` | `Config.Bus` + optional Start/Stop | `Bus` on Redis Streams; consumer groups give at-least-once + retry; `Event.ID` dedup. No subscriber anywhere changes. |
| `fulfill-shiprocket` | `RegisterFulfillment` | `Ship` books via API using `ShipRequest.Meta` (courier, pickup) + product weight/dims from `metadata`; status webhooks land on its own `/x/shiprocket/` routes → core service calls. |
| `invoices` | `Bus().Subscribe("order.paid")` + own tables + admin route | Numbered invoices (`invoices_invoices`, `invoices_counters`), GST/HSN from order metadata, HTML render served at `/api/admin/x/invoices/{orderID}`; reconciles missed events against orders on start. |
| `cms` | own tables + routes | `cms_pages`, admin CRUD, public `GET /x/cms/pages/{slug}`. |
| `mcp` | core services + `mcp.WithTools(...)` | Official Go MCP SDK (dependency stays in its go.mod); streamable-HTTP endpoint via `HandleAdmin` at `/api/admin/x/mcp` (admin auth comes free); stdio mode is `mcp.ServeStdio(app)` called from `main()` (§4); tools: list/update products, stock, orders, ship, mark-paid — an AI-agent-operable store. Other modules contribute tools via explicit `WithTools` wiring in main(). Contributes an `OpenAPI()` fragment. |

---

## 13. Milestones

Each milestone is a working, tested, shippable binary. Windows + Linux CI from M0.

- **M0 — Boot.** Repo + `go.work`; `db.go` (modernc, WAL, single-writer,
  checkpointer, Windows path→URI); migrator; `App`/`Config`/`Module`/ports
  compile; namespace enforcement; admin auth middleware (+ `Config.Dev`);
  HTTP skeleton WITH hardening (server timeouts, MaxBytesReader, panic
  recovery); `/health` + `/health/ready`; `/doc` (skeleton spec) + `/docs`;
  `cmd/gocommerce serve|migrate`; CI. *Proof: clean `go build` on a
  toolchain-free Windows box; a throwaway test module registers a migration +
  route and both exist; a namespace violation fails startup; `/doc` serves
  valid OpenAPI.*
- **M1 — Catalog.** Products schema/service, public read API + pagination +
  sku lookup, admin CRUD, product CSV round-trip (CLI + API, upsert-by-sku,
  dry-run). Spec covers every route it ships (rule holds for all milestones).
- **M2 — Cart.** Token carts, line items with price snapshots, totals,
  stock-aware validation, TTL sweeper.
- **M3 — Sell.** Built in two reviewable halves: (a) orders + lifecycle +
  SyncBus + `order.*` events (publish-after-commit) + notify dispatcher +
  LogNotifier; then (b) checkout transaction + commit-then-Initiate +
  re-validation policy + idempotency keys + COD provider + guest order
  lookup. *Proof: concurrent checkout can't oversell; failed-Initiate retry
  resumes the order; full COD purchase path green.*
- **M4 — Operate.** Manual fulfillment (create-fulfillment /
  deliver / cancel / mark-paid + status machine, MarkPaid auto-confirm);
  order CSV export/import (fire-events off, round-trip proof); `backup`
  command; reversible CSV sanitization; `docs/operations.md`. **Tag core
  `v0.1.0`** — a real COD store runs on core alone.
- **M5 — Prove the seams.** Build `payments-stripe` + `notify-sendgrid` for
  real; webhook dispatch via `/api/checkout/{code}/webhook`; **refund surface
  lands in core** (`Refunder`, `Payments.Refund`, `refunded` status,
  `POST /api/admin/orders/{id}/refund`); **unpaid-order sweeper activates**;
  module `OpenAPI()` fragments proven; `examples/store`; `gctest`;
  `docs/writing-a-module.md` + `docs/events.md` (freeze the event contract).
  Rule: any friction found is fixed by changing CORE now, pre-freeze.
  Released-core CI matrix leg starts here. **Tag core `v0.2.0`** with a
  compatibility promise for `Module` + ports (additive-only for v0.x).
- **M6 — Swap the spine.** `queue-redis` (kill-worker/redeliver test) +
  `fulfill-shiprocket`. *Proof: the purchase-path suite passes under both
  buses with a zero-line handler diff.*
- **M7 — Outer ring.** `invoices`, `cms`, `mcp` — each independently tagged;
  the MCP module's integration test is a scripted agent flow (list products,
  place COD order, ship it).

**Backlog (post-M7, explicitly not designed yet):** `payments-razorpay`,
`notify-msg91`/postmark/resend, customer accounts module (via the
`Config.AdminAuth` seam), product variants (core milestone — will be a
breaking pre-1.0 change), shipping-rate port, discounts (relaxes the
`discount_cents = 0` invariant), tax, outbound webhook-bridge module,
media/upload module, admin UI, kafka bus.

---

## 14. Accepted tradeoffs (eyes open)

1. **Compile-time composition excludes non-programmers** — no plugin-install
   button; every store is a Go build. Go's `plugin` package doesn't work on
   Windows; RPC plugins are ceremony a solo team can't afford. The operator is
   a developer, by design.
2. **modernc.org/sqlite** is ~1.5–2x slower than CGO SQLite and is a huge
   transpiled codebase — the price of toolchain-free Windows builds.
3. **Single-writer SQLite** — a write burst queues on one connection; no
   horizontal scaling. There are also **no repository interfaces**: SQL lives
   in services; swapping to Postgres later is surgery. Abstracting the
   datastore we swore was the only datastore would be enterprise cosplay.
4. **At-most-once sync events, no outbox** — a crash between commit and
   publish loses the event. Consumers that care reconcile; adopting a queue
   changes delivery timing (in-request → later) and module authors must not
   assume either.
5. **Single-package core** — file discipline is the only internal structure;
   everything exported to modules is exported to everyone, so the API-freeze
   burden lands early (M5) and `App` must be guarded against god-object
   growth (every accessor request gets the core-membership test).
6. **JSON event payloads** trade compile-time typing for bus-swappability;
   shared payload structs + `Event.V` are convention, not proof.
7. **Multi-module monorepo tagging** (`modules/x/v1.2.3`) is genuinely
   annoying; CI matrix keeps version skew honest.
8. **Forward-only migrations** — a bad migration in production needs a
   corrective forward migration, not a rollback.
9. **Table-prefix ownership is convention** (route/migration-ID namespacing is
   enforced; table naming is review-enforced) — a sloppy module CAN write
   `orders`; the `mode=ro` reader pool and "settlement is one core call"
   discipline are the guardrails, not a sandbox.
10. **Litekart route compatibility + unversioned `/api/`** — no version
    segment means route shapes are effectively frozen on first release;
    breaking route changes need new paths, not `/v2/`. The compatibility
    surface is pinned to `api.litekart.in/doc` as observed at R2; divergences
    are documented in §8, not accumulated silently.
11. **Hand-maintained OpenAPI** — the spec can drift from the code; the CI
    test that every registered core route appears in `/doc` (§11) is the
    honesty check, and module fragments are the module author's burden.

---

## 15. Open questions (answer before M0)

1. **Module path** — `github.com/misiki/gocommerce` assumed; confirm the org.
2. **Default currency** for examples/docs — INR assumed.
3. **License** — MIT/Apache-2.0 if open source; affects whether `modules/`
   third-party contributions are expected.
4. **Litekart compatibility scope** — §8 pins the overlap for core's surface;
   confirm no OTHER Litekart routes (coupons, wishlists, auth, …) are
   expected of core v1 — everything outside §3's scope stays module/backlog.

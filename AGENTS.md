# Working on GoCommerce

Guardrails for anyone changing this repository — human or agent. They are not
style preferences; each one exists because violating it breaks something that
is expensive to find later.

If a change requires breaking a rule here, that is a design decision: say so
explicitly and record it in `PLAN.md`, rather than breaking it quietly.

Deeper procedural guides live in [`skills/`](skills/). Start here.

---

## Orientation

GoCommerce is a commerce **engine**, not an application: a store is its own Go
program that composes the engine with the modules it needs in `main()`. Read
`PLAN.md` §5 for the decision table (D1–D25); it is the authority, and this
file is its operational summary.

```
core/                the engine — still `package gocommerce`
  gocommerce.go      App, Config, New, lifecycle
  module.go          Module interface, route namespace enforcement
  schema.go          core migrations (append-only)
  catalog.go cart.go checkout.go orders.go inventory.go payments.go fulfillment.go
  outbox.go events.go  transactional outbox + event bus
  superusers.go      admin identity (email+password sessions)
  doctor.go          operational diagnostics behind `gocommerce doctor`
  httpx.go           JSON envelope, error taxonomy, admin auth, pagination
ext/                 bundled modules — see rule 2
admin/               SvelteKit admin panel, embedded with go:embed
```

Build and test: see [`skills/development.md`](skills/development.md). Postgres
is required for tests; there is no mock.

---

## The rules

### 1. One Go module. One production dependency.

`github.com/misiki/gocommerce` is the whole repo. The engine is one package,
`gocommerce`, living in `core/` and imported as
`github.com/misiki/gocommerce/core` (D25). The only third-party
production dependency is `github.com/jackc/pgx/v5`. Adding a second is a
decision that belongs in `PLAN.md`, not in a commit.

### 2. `ext/` packages add ZERO third-party dependencies

This is what makes one module possible (D23). Stripe, Razorpay, SendGrid,
MSG91 and Shiprocket are all REST-over-`net/http` with hand-written HMAC —
that is the standard here, not a hardship. **A module that genuinely needs an
SDK ships as its own repository**, never as an `ext/` package.

### 3. Modules never write core tables

Every business state transition goes through a core service:
`app.Pay().MarkPaid(...)`, `app.Order().Cancel(...)`, `app.Stock().Adjust(...)`.
A module may create and own its own `<name>_` tables through `app.DB()`.

The reason is not tidiness. Core owns the state machine *and* the event that
describes each transition, and the two commit together. SQL that updates
`orders` directly produces a state nothing was told about.

### 4. A state change and its event commit in the same transaction

That is the whole point of the outbox. Write the row and the `outbox_events`
row in one `InTx`; the dispatcher publishes after commit, at least once.

Never publish from inside a handler by calling the bus directly for a state
change. Never write state and then emit an event afterwards — a crash between
the two is a lie the system never notices.

### 5. Never hold a transaction across network I/O

Checkout is two-phase for this reason: commit the order, *then* call the
payment gateway. A gateway that takes four seconds must never hold a row lock
for four seconds. If a new flow needs an external call, the call goes after the
commit, and the retry path is an idempotency key.

### 6. Money is integer minor units plus a currency code

Columns are `*_minor` (never `*_cents` — JPY has 0 decimals, KWD has 3). The
API returns `{"amount_minor": 2500, "currency": "USD"}` and **never** a
formatted string: how many decimals a currency has and where the symbol goes
are the client's business. No floats anywhere near money.

### 7. Migrations are forward-only and append-only

Once a migration ID has shipped, its SQL is frozen. A correction is a new
migration. Editing a released migration leaves every existing database in a
state no future version can reason about.

### 8. Guest checkout is permanent

Core has no customer or account concept, and D22 says it never will. An order
is reachable by its access token; a cart is reachable by its token. Do not add
a `customer_id` to core. Identity is a module's job through `Config.AdminAuth`.

`superusers` is an **operator** table — the people who administer the store —
and nothing in the commerce path reads it.

### 9. Route namespacing is enforced, not documented

A module may mount only under `/x/<name>/` (public) and `/api/admin/x/<name>/`
(admin, auth wrapped automatically). The App checks this while the module's
`Register` runs, so a module cannot claim a core route even by lying about its
name. Choosing `HandleAdmin` *is* the authentication — there is no way to
forget it.

### 10. Every response is JSON, including the ones you did not write

The envelope is `{"data": ...}` or `{"error": {...}}`, from the taxonomy in
`httpx.go` (`ErrNotFound`, `ErrValidation`, `ErrConflict`, `ErrUnauthorized`).
404s and 405s from the router are converted too — a client decoding JSON must
never receive Go's plain-text default.

### 11. Every served route appears in `core/openapi.json`

A test enforces it, and `gocommerce doctor` re-checks it at runtime. Add the
path when you add the route. Panel routes are marked `Route.UI` and excluded —
a spec describing a file server is noise.

### 12. The admin panel's CSS is PocketBase's, verbatim

`admin/src/lib/styles/*.css` are copied files. **Do not hand-edit them**; to
take an upstream change, re-copy from a PocketBase checkout so the diff stays
readable. `gocommerce.css` and `fonts-inter.css` are the only sheets we own,
and the deliberate deviations are listed at the top of each.

Use PocketBase's class vocabulary and nothing else — `.label` not `.badge`,
plain `.btn` for the primary action, `.page-table-wrapper` not
`.table-wrapper`. Grep the stylesheets before inventing a class. See
[`skills/development.md`](skills/development.md).

### 13. Tests run against a real PostgreSQL

`GOCOMMERCE_TEST_DB` points at it; each test gets its own schema. There is no
in-memory substitute and no mock database — the engine's correctness lives in
`FOR UPDATE SKIP LOCKED`, advisory locks and CHECK constraints, none of which a
fake reproduces.

### 14. Comments explain why, never what

The code says what it does. A comment earns its place by explaining a
constraint, a trade-off, or a bug that is not visible from the surrounding
lines. Do not narrate.

---

## Before you finish

```powershell
gofmt -l .                      # must print nothing
go vet ./...
go test ./... -count=1          # needs GOCOMMERCE_TEST_DB
go build -tags no_admin ./...
go test -tags no_admin ./core -count=1
.\scripts\check-docs.ps1        # links resolve; skills still match the code
.\scripts\smoke.ps1             # against a running store
.\gocommerce.exe doctor         # operational sanity
```

`check-docs.ps1` exists because prose cannot be compiled. If you rename a route
or a service method, it tells you which guide still names the old one — a
document that sends a reader somewhere that no longer exists is worse than no
document, because they blame themselves.

Changing anything under `admin/src` also needs `.\scripts\build.ps1` — the
built panel in `admin/build` is committed so `go build` works without Node.

---

## For AI agents specifically

- **MCP is the safe surface.** `ext/mcp` exposes domain tools that call the
  same services REST does. It never exposes arbitrary SQL and never mutates
  core tables directly. Keep it that way: a new tool wraps a service method.
- **`gocommerce doctor -json`** is the machine-readable health check. It exits
  non-zero when something is wrong, so it can gate work without being parsed.
- **Prefer the API over the database.** Reading with SQL to understand
  something is fine. Writing with SQL is rule 3.
- **Do not "fix" the deliberate deviations** listed in `gocommerce.css` and
  `docs/admin-panel.md`. They are requested choices, not drift.

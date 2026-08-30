---
name: integrations
description: Use when writing or changing a module — a payment gateway, carrier, notifier, translator, MCP tool or anything else that plugs into the engine through a port.
---

# Integrations

[`docs/writing-a-module.md`](../docs/writing-a-module.md) is the tutorial: the
smallest module, a payment method, a notifier, a webhook. Read it first. This
file covers what it does not — the exact lifecycle order, every port and its
implementations, and how the MCP surface is extended without widening it.

## The interface, and when each method runs

```go
type Module interface {
    Name() string                 // [a-z0-9-]+, unique within an app
    Migrations() []Migration      // applied after core's, in argument order
    Register(app *App) error      // called once, after every migration
}
```

`New` fixes the order and it is total:

1. Defaults applied, module names validated (`[a-z0-9-]+`, unique).
2. Database opened, services built, built-in providers registered.
3. **Every** migration applied — core's, then each module's in argument order.
4. Core routes mounted, so a core route always wins a conflict.
5. Each module's `Register`, in argument order. **Your tables already exist.**
6. OpenAPI fragments merged; background work scheduled.
7. `ListenAndServe` runs `OnStart` hooks, then accepts traffic.
8. Shutdown runs `OnStop` hooks in **reverse** order, then closes the pool.

`Register` is where everything is wired. Store `app.DB()`, `app.Log()` and the
bits of `app.Config()` you need on your struct.

Startup failures are surfaced, never swallowed. `Handle`, `RegisterPayment` and
friends cannot return an error, so they record one in `App.regErr` and `New`
returns it — a bad route or a colliding provider code fails the boot with a
message naming the module.

## The ports

Everything in `ports.go`. Nothing else in the engine is abstracted.

| Port | Method(s) | Registered with | Implemented by |
|---|---|---|---|
| `PaymentProvider` | `Code`, `Initiate` | `RegisterPayment` | built-in `cod`; `ext/payments-stripe`, `ext/payments-razorpay` |
| `WebhookProvider` | `Webhook` | *optional, detected* | stripe, razorpay |
| `Refunder` | `Refund` | *optional, detected* | stripe, razorpay — **not** `cod` |
| `FulfillmentProvider` | `Code`, `Ship` | `RegisterFulfillment` | built-in `manual`; `ext/fulfill-shiprocket` |
| `Notifier` | `Notify` | `RegisterNotifier(channel, n)` | built-in log notifier; `ext/notify-sendgrid` (email), `ext/notify-msg91` (SMS) |
| `Translator` | `Translate` | `RegisterTranslator` | nothing in this repo yet — the seam is built, not speculated (D21) |

One more optional capability lives in `openapi.go`: implement
`OpenAPI() []byte` and your fragment's paths and component schemas are merged
into the document served at `/doc`. A duplicate path or schema name is a startup
error rather than a silent overwrite.

Registration rules worth knowing before they bite:

- **Codes collide loudly.** A module may replace a built-in by reusing its
  code; two *modules* claiming one code is a startup error, because silently
  picking a winner would make it depend on argument order.
- **Notifiers append**, so a store can send email and mirror it to an audit
  sink; the built-in log notifier stands down for any channel that gets a real
  one. Channels are a closed set (`ChannelEmail`, `ChannelSMS`) because the
  dispatcher has to know which recipient field feeds one — a novel channel
  subscribes to the bus directly instead.
- **Only one translator.** Merging overlapping translations from several
  sources has no obviously correct answer, so the engine declines to guess.
- **`Subscribe` patterns** are an exact name (`order.paid`), a prefix
  (`order.*`) or `*`. Handlers must be idempotent: delivery is at-least-once.

## Worked example: `ext/cms`

[`ext/cms/cms.go`](../ext/cms/cms.go) is the whole shape in one file — a table,
public routes, admin routes, and not one line that touches commerce state.

**Own a table, prefixed with the module name.** `Migrations()` returns one
`Migration{ID: "0001_pages", SQL: "CREATE TABLE cms_pages (…)"}` — `ID` matching
`[a-z0-9_]+`, recorded in the ledger as `cms/0001_pages`, exactly one of `SQL`
or `Run` set, and frozen once it ships.

**Wire it, and let the mount decide the auth.**

```go
func (m *Module) Register(app *gocommerce.App) error {
    m.db = app.DB()
    m.defaultLang = app.Config().DefaultLanguage

    app.HandleFunc("GET /x/cms/pages/{slug}", m.handleGetPublic)
    app.HandleAdminFunc("POST /api/admin/x/cms/pages", m.handleCreate)
    // ...
    return nil
}
```

The pattern is a complete `net/http` ServeMux pattern including the method. The
engine validates the prefix and never rewrites the path, so what you write is
what gets served.

**Use the engine's HTTP vocabulary, not your own.** Every response is the JSON
envelope, including errors:

```go
limit, offset, err := gocommerce.Page(r)          // ?limit / ?offset / ?page
gocommerce.RespondList(w, pages, gocommerce.ListMeta{Total: total, Limit: limit, Offset: offset})
gocommerce.Respond(w, http.StatusCreated, page)
gocommerce.RespondError(w, r, gocommerce.NotFoundf("no page at %q", slug))
```

`NotFoundf`, `Validationf`, `Conflictf` and `Internalf` build `*APIError` values
with the taxonomy's status and code; `DecodeJSON` applies the body limit and
returns a validation error for malformed input. Never write a bare
`http.Error` — a client decoding JSON must never get Go's plain-text default.
Read the negotiated language with `gocommerce.Language(r.Context())` rather
than parsing `Accept-Language` again.

## Where a module lives, and what it may not do

A module that adds **no third-party dependency** belongs in `ext/`. Every
provider here talks REST over `net/http` and verifies HMACs with `crypto/hmac`;
that is the standard, and CI enforces it (see [architecture](architecture.md)
on D23). A module that genuinely needs an SDK ships as its own repository.

The hard rule is unchanged either way: **modules never write core tables.** Go
through the service — `app.Pay().MarkPaid(...)`, `app.Order().Cancel(...)`,
`app.Stock().Adjust(...)`, `app.Ship().Create(...)`. The service performs the
transition, enforces the invariants and writes the outbox event, all in one
transaction. You get `app.DB()` for **your** tables.

## MCP: the same state machine, a different door

`ext/mcp` mounts `POST /api/admin/x/mcp` through `HandleAdmin`, so the store's
admin credential is the agent's credential and the module writes no
authentication of its own. `mcp.ServeStdio(app, m)` runs the same dispatcher
over stdin/stdout for a desktop agent — called from `main()` in place of
`ListenAndServe`, because which mode a binary runs in is the application
author's decision, not a module's.

The safety property is structural, not a policy document: **every tool in
`tools.go` calls a domain service.** `mark_order_paid` is `app.Pay().MarkPaid`,
`cancel_order` is `app.Order().Cancel`, `update_variant_inventory` is
`app.Stock().Adjust` or `SetOnHand`. There is no SQL tool, no "run this query",
no direct table access. An agent therefore cannot reach a state a person could
not, and cannot skip a rule by coming in through a different door (PLAN §40
Rule 9).

A tool is a struct in `builtinTools()` — a name, a description, a JSON Schema
built with the `object`/`str`/`integer`/`enumStr` helpers, a `Mutates` flag, and
a `Call` that decodes its arguments and calls one service method:

```go
Mutates: true,
Call: func(ctx context.Context, raw json.RawMessage) (any, error) {
    var args struct {
        OrderID   int64  `json:"order_id"`
        Reference string `json:"reference"`
    }
    if err := decode(raw, &args); err != nil {
        return nil, err
    }
    return m.app.Pay().MarkPaid(ctx, args.OrderID, args.Reference)
},
```

Five things to get right:

1. **Wrap a service method.** If the operation you want has no service, the
   change belongs in core first, not in a tool.
2. **Set `Mutates: true` for anything that changes state.** It is what
   `Config.ReadOnly` withholds and what gets written to `mcp_audit` — "which
   agent cancelled that order, and when" has to stay answerable.
3. **Summarise lists, return records whole.** An agent reading fifty orders does
   not need every field of each; see `summarizeOrders`.
4. **Strip credentials.** `get_order` clears `order.AccessToken` — that is the
   shopper's credential, not something an agent needs to read an order.
5. **Return a domain error, not a transport error.** A failing `Call` becomes
   tool content with `isError` set: "that order is already shipped" is
   information the agent can act on.

Other modules contribute tools explicitly — `mcp.New(mcp.Config{Tools:
invoices.Tools()})`. There is no discovery: if you want a module's tools
exposed, you say so in `main()`.

## Common mistakes

- **Mounting outside your namespace.** `/api/admin/cms/pages` is a startup
  error; the admin path is `/api/admin/x/cms/pages`.
- **Writing an auth check.** `HandleAdmin` already did it. A hand-rolled one is
  the beginning of a second auth scheme.
- **Editing a shipped migration.** Every existing database already ran it.
- **Returning an error from a notifier for a permanent failure.** That buys
  twelve delivery attempts learning the same thing. Retry transient vendor
  failures; log and return nil for a malformed address.
- **Forgetting the webhook is a raw-body route.** The engine applies no
  body-consuming middleware there precisely so you can verify the signature
  against the exact bytes — and then deduplicate on the gateway's event id.
- **Adding a route and not the spec.** A test and `gocommerce doctor` both
  check every served route is documented; add the path to `openapi.json` or to
  your `OpenAPI()` fragment.
- **Assuming the dispatcher runs in a test.** It starts from an `OnStart` hook,
  which only `ListenAndServe` runs. Use `gctest.DrainOutbox` — see
  [development](development.md).

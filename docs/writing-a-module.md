# Writing a module

A module is an ordinary Go value. There is no registry to enrol in, no
interface hierarchy, no lifecycle framework — you implement three methods and
pass an instance to `gocommerce.New`.

```go
type Module interface {
    Name() string
    Migrations() []Migration
    Register(app *App) error
}
```

## The smallest useful module

```go
package hello

import (
    "net/http"
    "github.com/misiki/gocommerce/core"
)

type Module struct{}

func New() *Module { return &Module{} }

func (m *Module) Name() string                        { return "hello" }
func (m *Module) Migrations() []gocommerce.Migration  { return nil }

func (m *Module) Register(app *gocommerce.App) error {
    app.HandleFunc("GET /x/hello/greeting", func(w http.ResponseWriter, r *http.Request) {
        gocommerce.Respond(w, http.StatusOK, map[string]string{"hello": "world"})
    })
    return nil
}
```

The import path is `github.com/misiki/gocommerce/core`, but the package is
still named `gocommerce` — code refers to it as `gocommerce.New`, as above.

Install it by adding one argument:

```go
app, err := gocommerce.New(cfg, hello.New())
```

## The rules

There are two, and they are what make modules composable rather than merely
possible.

**A module may not write core commerce tables.** Call a domain service
instead — `app.Pay().MarkPaid(...)`, `app.Order().Cancel(...)`,
`app.Stock().Adjust(...)`. The service performs the transition, enforces the
invariants and writes the event, all in one transaction. A module that
`UPDATE`s `orders` directly skips every one of those and eventually produces an
order whose state nothing else understands.

**A module lives in its own namespace.** Public routes go under
`/x/<name>/`, admin routes under `/api/admin/x/<name>/`, and tables are
prefixed with the module's name. The route fence is enforced against the module
currently registering, so it cannot be talked around; a violation fails startup
rather than silently shadowing a core route.

## What Register can do

```go
app.RegisterPayment(p)                  // add a payment method
app.RegisterFulfillment(f)              // add a shipping backend
app.RegisterNotifier(channel, n)        // deliver email or SMS
app.RegisterTranslator(t)               // supply catalog translations
app.Subscribe(pattern, handler)         // react to events
app.Handle(pattern, h)                  // public route, under /x/<name>/
app.HandleAdmin(pattern, h)             // admin route, authentication included
app.OnStart(fn)  app.OnStop(fn)         // long-running work
app.DB()  app.Log()  app.Config()       // your own tables, a scoped logger

app.Products()  app.Cart()  app.Order()
app.Pay()       app.Ship()  app.Stock() // the domain services
```

Choosing `HandleAdmin` *is* the authentication. You never write an auth check,
and you cannot forget one.

## Owning tables

Return migrations from `Migrations()`. They run after core's, in the order the
modules were passed to `New`, each in its own transaction.

```go
func (m *Module) Migrations() []gocommerce.Migration {
    return []gocommerce.Migration{{
        ID: "0001_init",
        SQL: `CREATE TABLE hello_greetings (
                  id   bigserial PRIMARY KEY,
                  text text NOT NULL
              )`,
    }}
}
```

Migrations are forward-only and append-only. **Once an ID has shipped, its SQL
is frozen** — every existing database has already run it, and editing it leaves
those databases in a state no future version can reason about. Correct a
mistake with a new migration.

Use `Run: func(ctx, *sql.Tx) error` instead of `SQL` for a data fix that SQL
alone cannot express.

If your module needs another module's tables, the application author lists that
module first. Explicit wiring extends to schema; there is no dependency
resolver, and there does not need to be one.

## Adding a payment method

```go
func (m *Module) Code() string { return "acme" }

func (m *Module) Initiate(ctx context.Context, o *gocommerce.Order,
    opts gocommerce.PayOptions) (gocommerce.PaymentIntent, error) {

    intent, err := m.createRemotePayment(ctx, o.Total.AmountMinor, o.Total.Currency, o.ID)
    if err != nil {
        return gocommerce.PaymentIntent{}, err
    }
    return gocommerce.PaymentIntent{
        Kind:       gocommerce.IntentClientAction,
        Reference:  intent.ID,
        ClientData: map[string]string{"client_secret": intent.Secret},
    }, nil
}
```

`Initiate` runs **after** the checkout transaction has committed. That is
deliberate: a gateway having a slow day must not become the store having a slow
day. If it fails, the order stands unpaid and a retry with the same
`Idempotency-Key` resumes payment rather than reserving stock all over again.

Shoppers then complete payment, and the gateway tells you. Implement the
optional webhook capability and the engine serves
`POST /api/checkout/<code>/webhook` for you, with the raw body:

```go
func (m *Module) Webhook() http.Handler { return http.HandlerFunc(m.handle) }
```

Three things a webhook handler must do:

1. **Verify the signature** against the raw bytes. The engine applies no
   body-consuming middleware to webhook routes precisely so you can.
2. **Deduplicate.** Gateways retry. Claim the event id in your own table with
   `INSERT ... ON CONFLICT DO NOTHING` and return 200 if the claim finds it
   already taken.
3. **Call `app.Pay().MarkPaid(...)`**, never `UPDATE orders`.

Implement `Refund(ctx, order, amountMinor) error` if the gateway can refund.
Not implementing it is a valid answer — cash on delivery does not, and the
engine reports that plainly rather than pretending.

## Adding a notifier

```go
func (m *Module) Notify(ctx context.Context, n gocommerce.Notification) error {
    if n.Channel != gocommerce.ChannelEmail || n.To == "" {
        return nil
    }
    subject, body := m.render(n.Event, n.Language, n.Data)
    return m.send(ctx, n.To, subject, body)
}
```

Templates belong to your module. The engine sends flat data — order number,
total, item summary, customer name — and never rendered copy, because wording
and branding are the store's business.

Returning an error asks the outbox to retry, which is right for a vendor being
briefly unreachable. For a permanent rejection — a malformed address — log it
and return nil, or you will spend twelve delivery attempts learning the same
thing.

## Contributing to the API contract

Implement `OpenAPI() []byte` and your paths are merged into the document served
at `/doc`:

```go
func (m *Module) OpenAPI() []byte {
    return []byte(`{"paths":{"/x/hello/greeting":{"get":{
        "summary":"Say hello","responses":{"200":{"description":"ok"}}}}}}`)
}
```

## Testing

`gctest` boots the engine on an isolated PostgreSQL schema, so tests in
different packages cannot tread on each other.

```go
func TestMyModule(t *testing.T) {
    stub := gctest.StubHTTP(t, fakeVendorHandler)      // never call the real vendor
    app := gctest.New(t, mymodule.New(mymodule.Config{BaseURL: stub.URL}))

    result := gctest.PlaceOrder(t, app, "acme")
    gctest.DrainOutbox(t, app)

    order, err := app.Order().Get(context.Background(), result.Order.ID)
    // ...assert
}
```

Set `GOCOMMERCE_TEST_DB` to a database whose name contains `test`; tests skip
themselves when it is unset, so a contributor without PostgreSQL can still run
the rest of your suite.

Worth testing specifically:

- **Signature verification**, including a tampered body and a replayed old one.
- **Redelivery.** Send the same webhook three times and assert the order was
  settled once.
- **Vendor failure.** Assert a 500 retries and a 400 does not.

## Where a module lives

A module that adds **no third-party dependency** belongs in `ext/` in this
repository, alongside the others. Every provider here talks REST over
`net/http` and verifies HMACs with `crypto/hmac`, so a vendor SDK is usually
avoidable — and avoiding it keeps that SDK's transitive tree out of the
dependency graph of every store that installs the module.

A module that genuinely needs an SDK ships as its own repository. Nothing about
the extension mechanism changes when it does; a `Module` is a `Module`.

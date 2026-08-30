# Operations

The production architecture is one Go binary and one PostgreSQL database.
Everything else — Redis, search, object storage — is a choice you make later
when traffic or reliability justifies it, never a prerequisite.

```
        Internet
           │
      reverse proxy (TLS)
           │
   one or more Go processes
           │
       PostgreSQL
```

## Configuration

Only `DBURL` and one admin token are required.

| Setting | Default | Notes |
|---|---|---|
| `DBURL` | — | PostgreSQL connection string. Required. |
| `Addr` | `:8080` | Listen address. |
| `Currency` | `USD` | One settlement currency per store, ISO 4217. |
| `DefaultLanguage` / `Languages` | `en` / `["en"]` | Negotiated per request. |
| `AdminTokens` | — | At least one, unless `Dev`. Several allow rotation. |
| `AdminAuth` | bearer tokens | Replace to add sessions, OIDC or RBAC. |
| `CartTTL` | 720h | How long an untouched cart survives. |
| `OrderTTL` | 24h | How long an unpaid order holds its stock. |
| `HandlerTimeout` | 10s | Bounds one event handler. |
| `OutboxBatchSize` / `OutboxPoll` | 100 / 1s | Dispatcher tuning. |
| `FlatShippingMinor` | 0 | v1's entire shipping calculation. |
| `OrderPrefix` | `GC-` | Prefixes human order numbers. |

Generate an admin token with real entropy:

```sh
export GOCOMMERCE_ADMIN_TOKEN=$(openssl rand -hex 32)
```

Rotating one is a two-step deploy: add the new token to `AdminTokens`
alongside the old, move clients over, then remove the old. There is no window
in which nothing works.

## Deploying

```sh
go build -o gocommerce ./cmd/gocommerce        # or your own main()
./gocommerce -db "$DATABASE_URL" migrate       # apply the schema
./gocommerce -db "$DATABASE_URL" serve
```

`New` applies pending migrations on start, so `migrate` is only needed if you
prefer schema changes as a separate deployment step. Either way it is safe to
run several instances at once: migrations take a PostgreSQL advisory lock, so
the first instance applies them and the others wait rather than racing.

Shutdown is graceful. On SIGINT or SIGTERM the server stops accepting
connections, in-flight requests finish (up to 20 seconds), `OnStop` hooks run
in reverse order, and the database pool closes. `ListenAndServe` returns nil
after a clean shutdown, so any non-nil error is genuinely fatal.

### Running several instances

Nothing has to change. The outbox dispatcher claims rows with `FOR UPDATE SKIP
LOCKED`, so each process takes work no other process holds. The sweepers are
idempotent. There is no leader election to configure because there is no leader.

## Health checks

| Endpoint | Meaning | Use for |
|---|---|---|
| `GET /health` | The process is up. Touches nothing. | Liveness |
| `GET /health/ready` | The database answers. | Readiness |

Point liveness at `/health`, not `/health/ready`. A database hiccup should take
a process out of the load balancer, not restart it.

## Backups

The database is the business. Everything else can be rebuilt.

```sh
pg_dump --format=custom "$DATABASE_URL" > gocommerce-$(date +%F).dump
pg_restore --clean --if-exists --dbname "$DATABASE_URL" gocommerce-2026-08-28.dump
```

Test the restore. An untested backup is a hope, not a backup — restore into a
scratch database on a schedule and check that the last order is there.

For anything with real revenue, move to point-in-time recovery: continuous WAL
archiving, or a managed PostgreSQL that does it for you. The difference matters
in the case that actually happens — not "the server died" but "someone ran the
wrong `DELETE` an hour ago".

## What to watch

**Outbox depth.** The one metric specific to this engine:

```sql
SELECT count(*) FILTER (WHERE published_at IS NULL AND NOT dead) AS pending,
       count(*) FILTER (WHERE dead)                              AS dead
FROM outbox_events;
```

Pending should hover near zero. A rising number means a consumer is failing or
a vendor is down. Anything `dead` is an event nobody could deliver after twelve
attempts — read `last_error`, fix the cause, and requeue:

```sql
UPDATE outbox_events
SET dead = false, attempts = 0, available_at = now()
WHERE dead AND event_name = 'order.paid';
```

`GET /health/ready` covers the database. Beyond that, watch what you would for
any Go service: latency, error rate, connection-pool saturation.

**Reserved stock that never resolves.** The unpaid-order sweeper cancels
pending orders past `OrderTTL` and returns their inventory. If reserved
quantities climb anyway, look for orders stuck `pending` with a payment status
nobody ever settled.

## Housekeeping

The engine sweeps expired carts and unpaid orders every five minutes on its
own. Two tables grow forever and are yours to prune:

```sql
-- Delivered events, once you no longer need the audit trail.
DELETE FROM outbox_events WHERE published_at < now() - interval '90 days';

-- Webhook idempotency records, well past any gateway's retry window.
DELETE FROM payments_stripe_events WHERE received_at < now() - interval '30 days';
```

Keep them longer than you think you need. They are how you answer "did we
actually send that?" three weeks later.

## Data in and out

```sh
gocommerce -db "$DATABASE_URL" export products > products.csv
gocommerce -db "$DATABASE_URL" import products products.csv --dry-run
```

Or over the API, which is the same code:

```sh
curl -H "Authorization: Bearer $TOKEN" \
     "$STORE/api/admin/export/admin-products" > products.csv

curl -X POST -H "Authorization: Bearer $TOKEN" -H "Content-Type: text/csv" \
     --data-binary @products.csv \
     "$STORE/api/admin/import/products?dry_run=1"
```

Always dry-run first: it validates the whole file and rolls back, reporting
what it would have done.

Importing historical orders from another platform fires **no** events unless
you pass `fire_events`. Importing five thousand orders should not send five
thousand confirmation emails to people who bought something last year.

Exports prefix cells beginning with `=`, `+`, `-` or `@` with an apostrophe,
because a spreadsheet executes those when the file is opened. Import strips
exactly one back, so the round trip is lossless.

## Windows

Development on Windows is first class — there is no cgo anywhere in the engine,
so `go build` works with no C toolchain. For production, prefer Linux;
if you do run Windows, install the binary as a service with NSSM or `sc.exe`
rather than leaving a console window open.

## Security

- Terminate TLS at the proxy. The engine speaks plain HTTP.
- Admin tokens are compared in constant time, and there is no window where a
  wrong token takes measurably longer than a right one.
- Guest order lookup needs the order's access token, and a wrong token returns
  "not found" rather than "forbidden", so the endpoint cannot be used to
  discover which order numbers exist.
- Webhook secrets are mandatory in every payment module. A webhook endpoint
  without signature verification is an endpoint that lets anyone mark orders
  paid.
- Set `Dev: false` in production. It exists to let the engine boot without an
  admin token, which is exactly what you do not want.

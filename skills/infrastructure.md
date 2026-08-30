---
name: infrastructure
description: Use when running, deploying, migrating or diagnosing a store — the database, the migrator, the outbox dispatcher, the sweepers, `gocommerce doctor`, and building the binary.
---

# Infrastructure

[`docs/operations.md`](../docs/operations.md) is the operator's manual:
configuration table, deployment, health checks, backups, housekeeping SQL, CSV
import/export, security. Read it for those. This file is the machinery behind
them — the numbers, the failure modes, and what each diagnostic actually means.

Production is one Go binary and one PostgreSQL database. Everything else is a
choice you make later, never a prerequisite.

## PostgreSQL and the pool

PostgreSQL is the production database and the only one (D2, D4). There is no
database abstraction and no SQLite fallback: the engine's correctness lives in
`FOR UPDATE SKIP LOCKED`, advisory locks and CHECK constraints, and an
abstraction thin enough to hide them would hide the thing that works.

`OpenDB` (in `db.go`) opens `sql.DB` over pgx's `database/sql` driver and pings
it with a 10-second timeout before returning, so a bad DSN fails at boot rather
than on the first request. The pool is fixed at 25 open / 5 idle connections,
a 1-hour connection lifetime and a 5-minute idle timeout — constants, not
configuration. Twenty-five per process is a working ceiling for a commerce
workload; if you run many processes behind PgBouncer, size the pooler, not
this. There is no application-level write serialization anywhere: concurrency
control is the database's job, done with row locks inside transactions.

`InTx(ctx, db, fn)` is the only sanctioned way to write core state. It commits
on success, rolls back on error, and re-raises a panic *after* rolling back so
a bug never leaves a transaction open. `InTxOpts` takes explicit
`sql.TxOptions` for the rare caller needing stricter isolation.

## Migrations

Forward-only and append-only (D-rule; `migrate.go`). There are no down
migrations, because a down migration is the thing that never works under
pressure.

- The ledger is `gocommerce_migrations (owner, id, applied_at)`, deliberately
  **not** `schema_migrations`: the engine may share a database with the host
  application's own migration tooling, and a generic name would eventually
  collide.
- A row is keyed `<owner>/<id>` — `core/0003_orders_and_outbox`,
  `cms/0001_pages`. Owners namespace each other, so two modules may both have
  an `0001_init`.
- The whole run holds `pg_advisory_lock(0x676F636F6D6D6572)` on one dedicated
  connection. Rolling out several nodes at once is safe: the first applies, the
  rest wait.
- Each migration runs in **its own transaction**. PostgreSQL DDL is
  transactional, so a failure leaves no partial schema.
- `ID` must match `[a-z0-9_]+` and be unique within its owner; exactly one of
  `SQL` or `Run` must be set. Violations fail before anything is applied.

`New` applies pending migrations on start. `gocommerce migrate` does the same
and exits, for deployments that prefer schema as a separate step.

**A bad migration that has shipped is corrected by a new migration.** Never
edit the SQL of an ID that exists in any database you do not control — every
one of them already ran it, and editing it leaves them in a state no future
version can reason about. If it has *not* shipped (still local, no other
database has seen it), edit it freely and drop your dev database.

If a migration fails halfway through a rollout: the transaction rolled back, so
the schema is unchanged and the ledger has no row. Fix the SQL, redeploy. The
advisory lock means the other nodes never got past it either.

## The outbox dispatcher

`outbox.go`. Started as an ordinary `OnStart` hook, so it runs with the server
and stops with it — and a test that never serves is never surprised by one.

| Knob | Default | Where |
|---|---|---|
| Batch size | 100 | `Config.OutboxBatchSize` |
| Idle poll | 1s | `Config.OutboxPoll` |
| Claim visibility | 60s | constant |
| Max attempts before dead | 12 | constant |
| Backoff | `2^(attempts-1)` seconds, capped at 15m | constant |

One pass claims a batch with a single `UPDATE … FROM (SELECT … FOR UPDATE SKIP
LOCKED LIMIT n)`, incrementing `attempts` and pushing `available_at` 60 seconds
out. That claim is what makes multiple application instances safe with no
coordination and no leader election: each worker takes rows no other worker
holds. If a process dies mid-delivery, the row reappears after the visibility
window — the at-least-once guarantee doing its job.

When a pass finds work it comes straight back for more rather than sleeping, so
a backlog drains at full speed; a request that just wrote an event nudges the
dispatcher, so the common case is delivered in milliseconds. Handler failure
schedules a retry with exponential backoff and records `last_error`. At 12
attempts the row is parked `dead = true` — dead-lettering beats deleting,
because an event nobody could deliver is evidence.

**Telling a stuck dispatcher from a busy one.** Depth alone is not the signal;
*age* is:

```sql
SELECT count(*) FILTER (WHERE published_at IS NULL AND NOT dead) AS pending,
       count(*) FILTER (WHERE dead)                              AS dead,
       min(created_at) FILTER (WHERE published_at IS NULL AND NOT dead) AS oldest
FROM outbox_events;
```

The dispatcher polls every second, so minutes of backlog means it is failing,
not busy. `gocommerce doctor` encodes exactly that: over 30 seconds is a warn,
over 5 minutes is a fail. Read `last_error` on the stuck rows, fix the cause,
then requeue with the `UPDATE … SET dead = false, attempts = 0` in
[`docs/operations.md`](../docs/operations.md).

`App.DrainOutbox(ctx)` delivers everything due, synchronously, and returns the
count. Tests and CLI flows use it instead of waiting on the poll interval.

## The sweepers

One goroutine, started by the same `OnStart` hook, running every **5 minutes**
and once immediately at boot. It reclaims what abandoned traffic leaves behind:

- `carts.SweepExpired` — carts past `Config.CartTTL` (default 720h). `POST
  /api/carts` is unauthenticated, so unswept carts are an unbounded-growth
  vector, not merely untidy.
- `orders.SweepUnpaid` — unpaid orders past `Config.OrderTTL` (default 24h) are
  cancelled and their stock reservation released. A reservation outliving its
  order is how a store silently goes out of stock while the shelves are full.

Both are idempotent, so running several instances needs no coordination. If
reserved quantities climb anyway, the sweeper is not running — check that the
process actually called `ListenAndServe`.

## `gocommerce doctor`

`Diagnose` (in `doctor.go`) is a core service, not a CLI feature: the CLI
renders it, an MCP tool can call it, a panel screen could show it. It never
returns an error — a check that cannot run is itself a finding, and an operator
asking "what is wrong" should not be answered with one problem when there are
six. `-json` prints the full `Report`; either form **exits non-zero when any
check fails**, so it gates CI or an agent without being parsed.

Nine checks, in order:

| Check | Warn | Fail |
|---|---|---|
| `database` | every pooled connection checked out — look for long transactions | cannot reach PostgreSQL |
| `migrations` | — | something is unapplied; run `gocommerce migrate` |
| `admin access` | no superusers but tokens configured (the panel needs one), or `Dev` is on | no superusers **and** no admin tokens — nobody can administer the store |
| `outbox` | oldest unpublished > 30s, or any dead-lettered rows | oldest unpublished > 5m, or the table is unreadable |
| `stock reservations` | unpaid orders past their reservation window still holding units | — |
| `carts` | more than 1000 expired-but-open carts — the sweeper is not running | — |
| `catalog` | active products with no sellable variant (invisible to shoppers, fine in the admin list) | a `variant_stock` row with `reserved > on_hand` on a variant that does not sell past zero |
| `providers` | — | no payment providers at all — checkout is impossible |
| `api contract` | served routes missing from `/doc` | — |

Two deserve reading twice. **`catalog` failing on oversold stock should be
impossible** — the CHECK constraint `variants_reserved_within_on_hand` forbids
it, so a hit means the constraint is gone (a hand-edited schema, or a restore
from a dump that dropped it); verify the constraint before touching the data.
**`providers` failing** means core wiring did not run at all: `cod` and
`manual` are registered by the engine itself.

## Building

```powershell
.\scripts\build.ps1                 # panel + binary for this machine
.\scripts\build.ps1 -SkipPanel      # reuse the committed admin/build
.\scripts\build.ps1 -NoPanel        # API only, -tags no_admin
.\scripts\build.ps1 -All            # every platform, into dist/
.\scripts\build.ps1 -Version v0.1.0 # stamps main.version via -ldflags
```

Cross-compilation is unconditional because there is no cgo anywhere:
`-All` sets `CGO_ENABLED=0` with `GOOS`/`GOARCH` and produces
windows/linux/darwin on amd64 and arm64 from any host. Binaries are stripped
(`-s -w`) — roughly 13 MB with the panel, 12 MB without.

`admin/build` is committed, so plain `go build ./cmd/gocommerce` works on a
machine with no Node.js. Details in [development](development.md) and
[`docs/admin-panel.md`](../docs/admin-panel.md).

The reference binary's commands: `serve` (default), `migrate`, `superuser
create|update|list`, `doctor` (with `-json`), `spec`, `version`. Flags: `-db`,
`-addr`, `-admin-token`, `-currency`, `-languages`, `-dev`, `-v`, `-json`.
`DATABASE_URL`, `GOCOMMERCE_ADMIN_TOKEN`, `GOCOMMERCE_ADMIN_EMAIL` and
`GOCOMMERCE_ADMIN_PASSWORD` are the environment equivalents.

## Common mistakes

- **Pointing liveness at `/health/ready`.** A database hiccup should take a
  process out of the load balancer, not restart it. `/health` touches nothing.
- **Editing a released migration.** See above; it is the single most expensive
  mistake available in this repo.
- **Deleting dead outbox rows to clear an alert.** They are the evidence. Read
  `last_error`, fix the handler, requeue.
- **Treating pending outbox depth as the alarm.** A spike during a sale is
  normal; age is the signal.
- **Raising the pool ceiling because it looks saturated.** The `database` check
  warns on saturation because it is usually a leaked transaction, and a bigger
  pool just delays the diagnosis.
- **Running `doctor` against the wrong database.** A healthy report on an empty
  dev database says nothing about production.
- **Setting `Dev: true` in production.** It permits booting with no admin
  token, which `doctor` warns about for exactly that reason.

# Skills

Task-scoped procedural knowledge for working on GoCommerce. Each page carries
frontmatter saying when to reach for it, so you can load the one that matches
the job instead of reading the codebase end to end.

These document **invariants and the reasoning behind them**. For the exact API
surface, read the OpenAPI document the running store serves at `/doc` — it is
generated from the same contract the tests enforce, so it cannot drift the way
prose can.

The binding rules are in [`../AGENTS.md`](../AGENTS.md). Read that first; these
pages assume it.

## The domain

| Skill | Use when |
|---|---|
| [products](products.md) | Adding or changing catalog entries, options, or the public product API |
| [variants](variants.md) | Working with the sellable unit — option combinations, SKUs, uniqueness |
| [inventory](inventory.md) | Touching stock: on-hand, reservations, adjustments, low-stock |
| [carts](carts.md) | Guest carts, line items, price snapshots, TTL |
| [checkout](checkout.md) | The two-phase transaction, re-validation, idempotency keys |
| [orders](orders.md) | The order state machine, cancellation, fulfillment, refunds |
| [payments](payments.md) | Settlement, `MarkPaid`, or writing a new payment provider |
| [events](events.md) | The transactional outbox, event names and payloads, subscribing |

## The platform

| Skill | Use when |
|---|---|
| [architecture](architecture.md) | Deciding where something belongs, or why the system is shaped this way |
| [integrations](integrations.md) | Writing a module, implementing a port, or adding an MCP tool |
| [infrastructure](infrastructure.md) | Migrations, the dispatcher, sweepers, `doctor`, deployment |
| [development](development.md) | The local loop: build, seed, test, smoke, and the admin panel |
| [team](team.md) | Roles, rights, gating a route, invitations, and how access is lost |

## Keeping these honest

A skill that has drifted from the code is worse than no skill, because it is
believed. When you change behaviour these pages describe, change the page in
the same commit — and prefer linking to the source over restating it, so there
is less to drift.

---
name: team
description: Use when adding a right, gating a route, changing what a role may do, inviting somebody to the panel, or reasoning about how an operator gets and loses access.
---

# The team: roles, rights and access

## The model

One enumerated list of rights, three fixed roles drawn from it, and one place
that decides — [`rights.go`](../rights.go). **Nothing outside that file may
invent a right.**

```
catalog.read      products, variants, categories, collections, media, stock levels
catalog.write     editing any of them
orders.read       seeing orders and customers' purchases
orders.write      fulfilling, editing, placing
orders.refund     sending money back out — separate for exactly that reason
inventory.write   stock takes and adjustments
customers.read    orders grouped by who placed them, which is personal data
settings.write    the store's configuration, the team, and import/export
```

| Role | Carries |
|---|---|
| `owner` | everything, including deciding who else can |
| `manager` | the catalog, orders, refunds, stock, customers — not settings or the team |
| `staff` | sees the shop and moves orders along; no money out, no prices, no access |

The rights are coarse on purpose. A right per button is a permission system
nobody can hold in their head, and the store has no use for "may edit a barcode
but not a weight".

`roleRights` is a map rather than a set of conditionals so that operator-defined
roles later mean storing sets in a table and changing one lookup — not finding
every place a permission is decided.

## Gating a route

```go
a.HandleAdminFunc("POST /api/admin/tax-rates", a.handleCreateTaxRate, RightSettingsWrite)
```

The rights are **variadic**, so forgetting them is silent: the route mounts,
authentication still runs, and every signed-in operator reaches it whatever
their role. That is how the discount and tax routes once shipped ungated, where
a staff account — deliberately denied `catalog.write` — could have created a
hundred-percent-off code. `TestEveryAdminRouteDeclaresRights` is the guard, and
its exemption list is named rather than pattern-matched so that adding one is a
decision somebody writes down.

`requireRights` runs after authentication and refuses by name
(`403 — "your role (staff) does not carry orders.refund"`), because an operator
told only "forbidden" has to guess, and whoever sets roles needs to know what to
grant.

**A static admin token carries every right.** It is the bootstrap credential and
the one scripts use; narrowing what a script may do is a decision about who holds
the token, not about the route.

## How somebody joins

Invite them. `POST /api/admin/invitations` returns a `token` and an `accept_url`
**once** — only the SHA-256 is stored, exactly as with a session — and the
invitee sets their own password at `/accept-invite/<token>`, which signs them in.

The alternative, which this engine did until M18 and still supports for the cases
invitations cannot serve, is an owner choosing somebody else's password and then
telling it to them. That password is known to two people from the moment it
exists and is almost never changed.

- Re-inviting an address **replaces** the outstanding invitation, so the previous
  link stops working. A partial unique index enforces one open invitation per
  address; two live links means revoking the one you can see does not close the
  door.
- Accepting is one transaction and claims the invitation with a conditional
  `UPDATE`, so two people opening one link cannot both become operators.
- An address already on the team is refused, pointing at the role endpoint —
  which cannot be used to hand out a fresh password.

## The two lockouts

Both are the same failure: nobody left who carries `settings.write`, which is
the only right that can grant rights. The survivors cannot promote anybody,
including themselves, and the way back in is a database client.

- **Demoting the last owner** — `Superusers.SetRole` refuses. The owner rows are
  locked and then counted (PostgreSQL refuses `count(*) … FOR UPDATE`), so two
  requests each demoting one of the last two owners serialise.
- **Deleting the last owner** — `Superusers.Delete` refuses, and separately
  refuses to delete the last superuser at all. Deleting is not gentler than
  demoting; it is the same door, and it was open until M18.

## Losing access

A role is read from the row on every request, so **a demotion takes effect on the
operator's next request** — no waiting for a session to expire. A session has to
be ended explicitly:

```http
POST /api/admin/superusers/{id}/revoke-sessions   # settings.write — before removing somebody
POST /api/admin/me/revoke-sessions                # your own, including this browser
```

Deleting an operator cascades their sessions away with them.

## Changing your own password

`PATCH /api/admin/me` — and deliberately **not** behind `settings.write`, which
is also the right to change everybody's role. Gate it there and a staff member
who suspects their password is known must ask an owner to choose a new one for
them, which is the practice invitations exist to end.

`current_password` is required for either change: a session left open on an
unlocked laptop should not be enough to take the account over. Changing the
password ends every **other** session and keeps the caller's — signing somebody
out of the browser they are typing in, as a reward for improving their password,
teaches them not to.

## Common mistakes

- **Adding a right anywhere but `rights.go`.** `TestRouteRightsExist` catches a
  route asking for one that no role can hold, which would otherwise be a route
  nobody can reach and nobody finds out about until somebody tries.
- **Gating a self-service route.** If it acts on the caller and nobody else, the
  session already identifies them; a right would only decide *which* people may
  administer *themselves*, which is not a real question.
- **Assuming the panel's `can()` is enforcement.** It hides nav items that would
  only lead to a 403. The engine is what refuses.
- **Reading a role from a stored session record.** `Resolve` re-reads it per
  request for a reason — see the demotion note above.

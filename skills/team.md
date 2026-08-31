---
name: team
description: Use when adding a right, gating a route, changing what a role may do, inviting somebody to the panel, or reasoning about how an operator gets and loses access.
---

# The team: roles, rights and access

## The model

One enumerated list of rights, three fixed roles drawn from it, and one place
that decides — [`rights.go`](../rights.go). **Nothing outside that file may
invent a right.** What each role *carries* is the store's to change
([`roles.go`](../roles.go)); the list itself is not.

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

The table above is the **default**. `roleRights` being a map rather than a set
of conditionals is what let M19 make the sets configurable by changing one
lookup instead of finding every place a permission is decided.

## Re-cutting a role

A store may widen or narrow `manager` and `staff` — Settings → Roles in the
panel, or:

```http
GET    /api/admin/roles           # the matrix: every role, the catalogue, the floor
PUT    /api/admin/roles/{role}    # the whole set the role should carry
DELETE /api/admin/roles/{role}    # drop the override; the role tracks defaults again
```

In Go it is `app.Roles()` — `Matrix`, `Of`, `Set`, `Reset`.

Four rules, all enforced in `RoleRights.Set`:

- **Owner is fixed** and always carries every right. It is the way back into a
  store that has been configured into a corner, so it is not storable at all —
  the `role_rights` CHECK does not accept it.
- **Every role keeps `catalog.read`** (`RequiredRights`). A role stripped past
  the floor is not a narrower role; it is an account that can sign in and see
  nothing, and removing the person says that honestly.
- **A set equal to the default is stored as no rows**, so the role goes on
  tracking a default that a later release may widen. `Reset` is therefore not
  the same as saving the defaults back — though it lands in the same state.
- **You cannot remove `settings.write` from your own role.** Owner is immune
  because it is not configurable; the case that bites is a `manager` who was
  handed `settings.write`. A static admin token has no role, so it is exempt —
  and it is what undoes this kind of mistake.

It is deliberately **not** an escalation guard: `settings.write` already means
"the right that can grant rights", and whoever holds it can promote themselves
through the team screen anyway. Handing it out is the decision.

`role_rights` has no foreign key to the rights — they live in Go — so a right
dropped in a later release leaves rows that resolve to nothing. The lookup
intersects with `AllRights`, which is the safe direction to be wrong in.

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

`requireRights` checks the operator's **resolved** set, not the role's defaults:
authentication already applied the store's matrix, so the middleware costs no
query and cannot disagree with what the panel was told. It runs after
authentication and refuses by name
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

A role is read from the row on every request, and the rights that role carries
are resolved with it, so **a demotion or a narrowed role takes effect on the
operator's next request** — no waiting for a session to expire, and nothing to
revoke. Nothing is cached anywhere, which is the point: a second server holding
last week's answer to "who may refund" is not a cache, it is a hole. A session has to
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
- **Calling `DefaultRightsOf` or `DefaultCan` when you mean what an operator may
  do.** They answer for the engine's defaults, which in a store that has re-cut
  the role is the wrong answer. The names are long on purpose; use
  `(*Superuser).Has` for a request, or `app.Roles().Of` for a role.

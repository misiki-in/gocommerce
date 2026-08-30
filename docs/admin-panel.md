# The admin panel

One executable serves the API and the dashboard. Run it, open
`http://localhost:8080/`, and there is nothing else to install or configure.

## What it is

A SvelteKit single-page app, built to static files and embedded in the Go
binary with `go:embed`. It talks to the same public API as any other client:
no private endpoints, no server-side rendering, no backend of its own.

That constraint is deliberate. If the panel needed an endpoint the API did not
have, the API would be incomplete — and the next person to automate something
would find the gap the hard way.

## Design

The look is not *modelled on* the [PocketBase](https://github.com/pocketbase/pocketbase)
dashboard — it **is** PocketBase's stylesheets. `admin/src/lib/styles/` holds
`vars.css`, `base.css`, `form.css`, `layout.css`, `table.css`, `modal.css`,
`toast.css`, `list.css`, `grid.css`, `dropdown.css`, `accordion.css`,
`bulkbar.css`, `tabs.css`, `tooltip.css` and `animations.css` copied verbatim
from PocketBase's `ui/src/css`, imported in PocketBase's own order.

Do not hand-edit those files. To take an upstream change, re-copy them from a
PocketBase checkout, so the diff stays readable. Exactly one stylesheet in the
panel is ours — `gocommerce.css` — and it covers only what PocketBase has no
counterpart to: dashboard stat cards, a section heading, and money columns. It
is built from PocketBase's own tokens, so it introduces no new colour, radius
or duration.

The markup follows suit. Pages own their `.page` wrapper, the primary
navigation is horizontal in the accent header (`.app-main-nav`), a page title
is `<nav class="breadcrumbs">` rather than a heading, and secondary navigation
is a `.page-sidebar` of `<details class="nav-group">` groups — which is why
Settings has a sidebar and the list screens do not.

A consequence worth knowing: the class vocabulary is PocketBase's and nothing
else. There is no `.badge` (`.label`, with four colour variants), no
`.btn.accent` (plain `.btn` *is* the accent), no `.table-wrapper`
(`.page-table-wrapper`), and no `.panel`. When adding a screen, grep the
stylesheets before inventing a class.

Everything below is therefore a description of PocketBase's design, not of a
reimplementation of it.

**Typography.** **Inter** for text, IBM Plex Mono for SKUs, order numbers and
code — Inter has no monospace companion, and a column of order numbers has to
align. Body text is 14px on a 22px line. Headings are *sized*, not bolded:
weight comes from `<strong>` and from the few components that ask for it.

Inter replaces PocketBase's IBM Plex Sans, so two things follow. PocketBase
sets `letter-spacing: 0.004em` on the body and its own comment says why — it
normalises *Plex Sans* between Chrome and Firefox; `gocommerce.css` resets it
to `normal`, because Inter ships with its own metrics and the extra tracking
reads loose. And the faces are self-hosted in `admin/static/fonts/inter/`
rather than linked from Google, because the panel runs under a CSP with
`font-src 'self'` — and a dashboard should not phone a third party to render
its own text.

Four files, not eight: Google serves Inter as a variable font, so one file per
subset carries the whole weight axis (the 400 and 600 downloads were
byte-identical). With `unicode-range` splitting latin from latin-ext, an
English page fetches about 100KB of the 277KB total. Dropping the now-unused
Plex Sans binaries made the panel *smaller* than it was with Plex.

**Icons.** Remix Icon 4.6.0 as an icon font, so an icon inherits the colour and
size of whatever it sits in. 3,058 of them are available; `<i class="ri-truck-line">`
is the whole API.

**Colour.** One accent, four semantic colours, and a five-step surface ramp
from `#fff` to `#ccd4db`. Almost every other shade is derived with
`color-mix()` rather than hand-picked, which is why the dark theme needed only
the ramp redefined. Components reference roles (`--surfaceAlt2Color`,
`--inputFocusColor`), never raw hex — that is what lets the header be an accent
"surface" while reusing every component unchanged.

The accent is **olive** (`#89a24c`), not PocketBase's blue. That is not a
one-token swap, and the reason is worth knowing before anyone changes it again.

PocketBase's blue is dark enough to carry white text, so `vars.css` hardcodes
`#fff` for everything sitting on the accent. White on `#89a24c` measures 2.9:1
— below the 4.5:1 floor — so `gocommerce.css` inverts the accent's *foreground*
instead: `--accentTxtColor` and the header's `--surfaceTxtColor` become a dark
olive, and `--selectionColor` follows, because a focus ring derived from a
lightened green disappears into the bar.

The hint step needs naming rather than deriving. `vars.css` produces it by
fading the text toward transparent, which works on a light accent and fails on
a mid-tone one: 40% of dark text over this green lands near 2.6:1, and the idle
nav items become decoration. An explicit `--accentTxtHintColor` keeps it at
4.6:1.

`.txt-accent` has the opposite problem — the accent as *foreground* on white is
also 2.9:1 — so light mode uses a deepened moss while dark mode can use the
accent itself.

Measured in the browser: 5.6:1 active nav, 4.6:1 idle nav, 5.7:1 accent text on
white and 6.0:1 on the dark surface. If you change the accent again, re-measure
rather than assuming — the two failure directions are independent, and a
mid-tone hue fails both.

The header wordmark is `logo_header.svg` — named for its role rather than its
colour, since it is dark on the accent and `logo_white.svg` would have lied.

**Motion.** The rule the whole panel follows: hover and focus move one surface
step over 150ms; a press moves two steps over 70ms. The asymmetry is most of
why it feels crisp — a click answers immediately while a hover glides. There is
no ripple and nothing scales under the cursor: a control that moves when you
aim at it makes double-clicks miss.

Specific motions:

| Element | Behaviour |
|---|---|
| Drawer | Full-height, right-anchored, slides 30px in over 200ms |
| Confirmation | Centred, scales 0.98 to 1 — it belongs to the button you pressed |
| Toast | Rises 10px from the bottom centre; hovering pauses its timer |
| Dropdown | Falls 3px with a fade |
| Tooltip | Fades only, no movement |
| Row actions | Hidden until row hover, sliding 5px in — always visible on touch |
| Loading button | Label fades, spinner takes its place, width never changes |

**Fields.** The distinctive PocketBase input: a filled box with no border and
the label *inside* it. Focus darkens the whole field one step rather than
drawing a ring, and an error re-tints the field instead of adding red furniture
around it.

Route changes animate the `.page-content`, not the `.page`: moving between two
Settings screens re-mounts an identical sidebar, and animating the whole page
would make that stationary element flicker while the part that actually changed
sat still. The motion is PocketBase's own `slideTop` — 3px and a fade over
150ms.

PocketBase's stylesheets carry no `prefers-reduced-motion` handling, so
`gocommerce.css` adds it for the motion that travels: page transitions, row
fades, toasts and the drawer slide. The loading spinner is exempt, because a
frozen spinner reads as a hung page.

## Authentication

Two kinds of credential exist, because scripts and people want different
things. Both arrive as `Authorization: Bearer <x>` and both satisfy the same
middleware, so no handler has to care which one it got.

**A superuser signs in with an email and a password**, as in PocketBase. The
server issues a session token, the panel keeps it in `localStorage`, and it
expires after 14 days. This is what a person uses.

**A static admin token** from `Config.AdminTokens` is what a script uses. It
has no session and no expiry, which is exactly right for CI and curl and
exactly wrong for a browser. Several can be configured at once, so one can be
rotated without downtime.

Static tokens are checked first, because that check is a memory compare while a
session costs a query — an unauthenticated flood therefore never reaches the
database.

### Creating the first operator

A fresh database has none, and the panel asks before it renders a login form
nobody could satisfy (`GET /api/admin/auth-state`). Three ways to create one:

```powershell
# 1. From the panel. Open it on a fresh database and it offers the form.

# 2. From the CLI.
gocommerce superuser create you@example.com "a good password"
gocommerce superuser update you@example.com "a new password"
gocommerce superuser list

# 3. From the environment, for an unattended deploy.
$env:GOCOMMERCE_ADMIN_EMAIL = 'you@example.com'
$env:GOCOMMERCE_ADMIN_PASSWORD = '...'
gocommerce serve
```

The environment path is create-only: if an operator already exists, a stale
variable must not silently reset their password.

### How passwords are stored

PBKDF2-HMAC-SHA256 at 600,000 iterations with a per-user salt, from the
standard library's `crypto/pbkdf2` — chosen over bcrypt because the engine's
claim to slimness is that it has exactly one production dependency.

The stored hash is self-describing (`pbkdf2-sha256$<iterations>$<salt>$<key>`),
so the cost can be raised later without a migration and without a flag day: an
old hash keeps verifying against its own recorded parameters.

Session tokens are stored as a SHA-256 hash, never in the clear, so a leaked
database yields no usable session.

### What the login endpoint refuses to tell you

A wrong password and an unknown account return the identical error, and take
the same time — the unknown-account path deliberately performs a real
verification against an existing hash so that the *cost* matches too. Between
them, the endpoint cannot be used to discover who has an account.

Repeated failures are throttled with exponential backoff on two counters: one
per (account, address) pair, so nobody can lock a real operator out by failing
on their behalf; and one per address with a larger allowance, so a single host
spraying one password across many accounts is still slowed. The throttle is
in-process and therefore per-replica; it raises the cost of online guessing,
which is what it is for, and is not a substitute for a strong password.

Changing a password ends every session that operator holds. If they are
changing their own, the response carries a replacement token, so the security
property holds without kicking them out of the flow they are in.

`Config.AdminAuth` still replaces the whole middleware, so an identity module
can take over from either scheme.

## How it is served

Mounted at the **root**. The API lives entirely under `/api`, `/health`, `/doc`
and a module's `/x/`, so nothing competes for `/` — and the store's address is
the dashboard's address.

- `GET /{path...}` serves the embedded files. Go's `ServeMux` prefers the most
  specific pattern, so every real API route still wins over this catch-all.
- An unknown path with no file extension returns `index.html`, so refreshing on
  `/orders` works. A missing *asset* returns 404 — answering a missing `.js`
  with HTML would turn a build problem into a baffling syntax error.
- **An unmatched path under an API namespace returns the JSON 404**, not the
  panel. This is the one guard the root mount makes necessary: without it,
  `GET /api/typo` would answer with an index.html, and a client decoding JSON
  would report a syntax error instead of reading the message. It is covered by
  its own test.
- `/_` and `/_/…` — the panel's previous home, matching PocketBase — redirect
  to `/`, so an old bookmark still works.
- Hashed assets under `_app/immutable` are cached for a year; `index.html` is
  never cached, or a deploy would not reach anyone.
- A strict CSP locks the panel to its own origin.

Panel routes are marked `UI` in the route table so the OpenAPI coverage test
skips them: a spec describing a file server would be noise.

### The trade

Owning the root means this binary cannot also serve a storefront there. That is
the right call for a headless engine — the storefront is a separate application
with its own deployment — but it is a real constraint. If you need both on one
origin, put a reverse proxy in front, or rebuild the panel with a different
`paths.base` in `admin/svelte.config.js` and change `AdminPanelPath` to match.
The base is fixed at build time, which is why it is not a runtime setting.

## Building

```powershell
.\scripts\build.ps1              # build the panel, embed it, compile
.\scripts\build.ps1 -SkipPanel   # reuse the committed panel build
.\scripts\build.ps1 -NoPanel     # API only
.\scripts\build.ps1 -All         # every platform, into dist/
```

`admin/build` is **committed**, so `go build` and `go install` work on a machine
with no Node.js — the same trade PocketBase makes, for the same reason. Rebuild
it whenever you change anything under `admin/src`.

Cross-compilation works everywhere because there is no cgo: `CGO_ENABLED=0`
with `GOOS`/`GOARCH` produces a Linux binary from Windows and vice versa.

Sizes: about 13 MB stripped with the panel, 12 MB without. The panel itself is
roughly 830 KB, most of it the icon font and the four Plex faces.

## Developing the panel

```powershell
.\scripts\dev.ps1               # the API on :8080
cd admin; npm run dev           # the panel on :5173, proxying to :8080
```

Vite's dev server gives hot reload and proxies `/api`, `/health` and `/doc` to
the running store, so the panel talks to real data. In production both are the
same binary and no proxy exists.

## What it does not do yet

Honest gaps, rather than a roadmap:

- **An option axis cannot be removed or renamed** once it exists, and a
  variant's option combination cannot be changed — the API has no route for
  either, so the drawer shows them read-only and tells you the remedy (add the
  combination you want, delete the one you don't). SKU, price and active are
  editable.
- **Variants in the drawer are unpaginated.** They arrive embedded in the
  product, which has no limit; the paginated route
  (`GET /api/variants?product_id=`) returns variants without the product, so
  using it would mean reconciling two sources. A product with hundreds of
  variants renders all of them.
- **A new variant starts at zero on hand** and is unsellable until someone
  visits Inventory. The form says so. Stock deliberately stays there, so every
  movement is a transactional adjustment rather than an overwrite.
- **Not exposed in the variant form:** barcode, compare-at price,
  `track_inventory`, weight, position. All patchable through the API.
- **Duplicate option values across axes** (a `Size: Small` and a
  `Cup: Small` on one product) are caught in the panel, not the engine:
  `insertOptions` compares values only within a single call, and the value
  lookup keys on the lowercased string, so the engine would resolve the
  collision silently. That is an engine gap the panel is papering over.
- **No collection browser.** PocketBase's dashboard is generic over
  user-defined collections; this one is specific to a commerce domain that
  already knows what a product and an order are.
- **No log viewer or SQL console.**
- **No bulk actions** on the list screens.

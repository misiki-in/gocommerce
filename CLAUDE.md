# CLAUDE.md

The architectural guardrails for this repository live in
**[AGENTS.md](AGENTS.md)** — read it first, and treat its fourteen rules as
binding. This file adds only what is specific to working here with Claude Code.

Procedural guides are in [`skills/`](skills/README.md). Load the one that
matches the task rather than reading the whole codebase.

## The five that get broken most

Full reasoning is in AGENTS.md; these are the ones worth having in front of you:

1. **Never write core tables from a module or a script.** Go through the
   service — `app.Pay().MarkPaid(...)`, not `UPDATE orders`. The state change
   and its event have to commit together.
2. **`ext/` packages add zero third-party dependencies.** If the work needs an
   SDK, it is a separate repository.
3. **Money is `*_minor` integers plus a currency code.** No floats, and the API
   never returns a formatted string.
4. **Migrations are append-only.** A shipped migration is frozen; corrections
   are new migrations.
5. **Do not hand-edit `admin/src/lib/styles/*.css`.** They are PocketBase's
   files, verbatim. Only `gocommerce.css` and `fonts-inter.css` are ours.

## Environment on this machine

Go is not on the system PATH:

```powershell
$env:Path += ';C:\Users\LENOVO\go-sdk\go\bin'
$env:GOCOMMERCE_TEST_DB = 'postgres://gocommerce@127.0.0.1:5433/gocommerce_test?sslmode=disable'
```

PostgreSQL runs on **port 5433** (a trust-auth cluster created for this
project; the default 5432 cluster uses scram and its password is not known
here). Tests need it — there is no mock.

There is no cgo toolchain, so `-race` is unavailable locally; CI covers it.

## Verifying a change

```powershell
gofmt -l .                                  # must print nothing
go vet ./...
go test ./... -count=1
go test -tags no_admin . -count=1           # the API-only build
.\scripts\check-docs.ps1                    # skills and links still match the code
.\scripts\build.ps1                         # required after any admin/src change
.\scripts\smoke.ps1                         # walks a whole sale; exits non-zero on any failure
.\gocommerce.exe doctor                     # 9 operational checks
```

`scripts/dev.ps1 -Seed` starts a store on :8080 with demo data. It signs in as
`admin@example.com` / `devpassword`; `dev-token` is the static admin token for
scripts.

## Verifying the admin panel

The panel cannot be checked by reading code — a page can return 200 for every
asset and still render nothing (it did, once: a CSP header blocked SvelteKit's
inline bootstrap). **Claude-in-Chrome is not on this host** and its requests
never reach the local server.

Use Playwright. A harness lives in the session scratchpad under `verify/`:
`check3.mjs` signs in with a real password, walks every screen, captures
console and page errors and failed requests, reads computed styles, and
screenshots desktop, dark and mobile. Run it more than once — the worst bug
found this way was intermittent.

## Working style

- Read `PLAN.md` §1 before proposing anything structural. The decisions have
  numbers (D1–D23) and reasons; disagree with the reason, not the conclusion.
- Comments explain **why**. The code already says what.
- Match the surrounding prose voice in docs — plain, specific, no filler.
- When something is genuinely ambiguous, say so and state the assumption you
  proceeded under, rather than picking silently.

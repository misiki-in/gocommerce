package gocommerce

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Operational diagnostics.
//
// This exists because the questions an operator asks at 2am — "is the outbox
// stuck?", "can anyone still sign in?", "is stock pinned by orders nobody is
// going to pay for?" — are answerable from the database, but only if you know
// which nine queries to run. Diagnose knows them.
//
// It is a core service rather than a CLI feature so that everything can reach
// it: `gocommerce doctor` renders it, an MCP tool can call it, and a future
// panel screen can show it. The CLI is a client, like everything else.

// Status is a diagnostic's verdict.
const (
	// StatusOK means the check passed and needs no attention.
	StatusOK = "ok"
	// StatusWarn means something is worth looking at but the store is serving.
	StatusWarn = "warn"
	// StatusFail means something is broken or about to be.
	StatusFail = "fail"
)

// Diagnostic is one check's result.
type Diagnostic struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
	// Hint says what to do about it. Every failing check has one — a
	// diagnostic that reports a problem without naming a next step just moves
	// the puzzle.
	Hint string `json:"hint,omitempty"`
}

// Report is the whole health picture.
type Report struct {
	Version string       `json:"version"`
	At      time.Time    `json:"at"`
	OK      bool         `json:"ok"`
	Checks  []Diagnostic `json:"checks"`
}

// Failed returns the checks that did not pass, worst first.
func (r Report) Failed() []Diagnostic {
	var out []Diagnostic
	for _, c := range r.Checks {
		if c.Status != StatusOK {
			out = append(out, c)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Status == StatusFail && out[j].Status != StatusFail
	})
	return out
}

// Diagnose runs every check and reports what it found.
//
// It never returns an error: a check that cannot run is itself a finding, and
// an operator asking "what is wrong" should never be answered with one problem
// when there might be six.
func (a *App) Diagnose(ctx context.Context) Report {
	rep := Report{Version: Version, At: time.Now().UTC()}
	add := func(d Diagnostic) { rep.Checks = append(rep.Checks, d) }

	add(a.checkDatabase(ctx))
	add(a.checkMigrations(ctx))
	add(a.checkAdminAccess(ctx))
	add(a.checkOutbox(ctx))
	add(a.checkReservations(ctx))
	add(a.checkCarts(ctx))
	add(a.checkCatalog(ctx))
	add(a.checkProviders())
	add(a.checkContract())

	rep.OK = true
	for _, c := range rep.Checks {
		if c.Status == StatusFail {
			rep.OK = false
		}
	}
	return rep
}

func (a *App) checkDatabase(ctx context.Context) Diagnostic {
	d := Diagnostic{Name: "database"}
	var version string
	if err := a.db.QueryRowContext(ctx, `SHOW server_version`).Scan(&version); err != nil {
		d.Status, d.Detail = StatusFail, "cannot reach PostgreSQL: "+err.Error()
		d.Hint = "check the connection string and that the server is accepting connections"
		return d
	}
	stats := a.db.Stats()
	d.Status = StatusOK
	d.Detail = fmt.Sprintf("PostgreSQL %s, %d/%d connections in use", version, stats.InUse, stats.MaxOpenConnections)
	// A pool pinned at its ceiling is the shape of a leak or of genuine
	// saturation, and both show up here before they show up as timeouts.
	if stats.MaxOpenConnections > 0 && stats.InUse >= stats.MaxOpenConnections {
		d.Status = StatusWarn
		d.Hint = "every pooled connection is checked out; look for long transactions"
	}
	return d
}

func (a *App) checkMigrations(ctx context.Context) Diagnostic {
	d := Diagnostic{Name: "migrations"}

	applied := map[string]bool{}
	rows, err := a.db.QueryContext(ctx, `SELECT owner, id FROM `+migrationsTable)
	if err != nil {
		d.Status, d.Detail = StatusFail, "cannot read the migration ledger: "+err.Error()
		d.Hint = "run `gocommerce migrate`"
		return d
	}
	for rows.Next() {
		var owner, id string
		if err := rows.Scan(&owner, &id); err == nil {
			applied[owner+"/"+id] = true
		}
	}
	rows.Close()

	var pending []string
	for _, set := range a.migrationSets() {
		for _, m := range set.Migrations {
			if key := set.Owner + "/" + m.ID; !applied[key] {
				pending = append(pending, key)
			}
		}
	}
	if len(pending) > 0 {
		d.Status = StatusFail
		d.Detail = fmt.Sprintf("%d migration(s) not applied: %s", len(pending), strings.Join(pending, ", "))
		d.Hint = "run `gocommerce migrate`"
		return d
	}
	d.Status = StatusOK
	d.Detail = fmt.Sprintf("%d applied, none pending", len(applied))
	return d
}

// checkAdminAccess answers the question a locked-out operator is actually
// asking. Either credential is enough on its own; having neither means the
// admin API is unreachable by anyone, which no amount of uptime compensates
// for.
func (a *App) checkAdminAccess(ctx context.Context) Diagnostic {
	d := Diagnostic{Name: "admin access"}

	tokens := len(a.cfg.AdminTokens)
	supers, err := a.superusers.Count(ctx)
	if err != nil {
		d.Status, d.Detail = StatusWarn, "cannot count superusers: "+err.Error()
		return d
	}

	switch {
	case supers == 0 && tokens == 0:
		d.Status = StatusFail
		d.Detail = "no superusers and no admin tokens — nobody can administer this store"
		d.Hint = "run `gocommerce superuser create <email> <password>`, or start with -admin-token"
	case supers == 0:
		d.Status = StatusWarn
		d.Detail = fmt.Sprintf("no superusers; %d admin token(s) configured", tokens)
		d.Hint = "the panel needs a superuser: `gocommerce superuser create <email> <password>`"
	default:
		d.Status = StatusOK
		d.Detail = fmt.Sprintf("%d superuser(s), %d admin token(s)", supers, tokens)
	}
	// Config.Dev is deliberately not reported here. It is only dangerous while
	// serving, and the CLI sets it for its own offline commands — including
	// this one — so a warning keyed on it would fire on every doctor run and
	// mean nothing. ListenAndServe is where that belongs.
	return d
}

// checkOutbox is the most load-bearing check here. The outbox is what makes
// events durable, and its failure mode is silent: deliveries stop, state keeps
// committing, and nothing surfaces until someone notices the emails stopped.
func (a *App) checkOutbox(ctx context.Context) Diagnostic {
	d := Diagnostic{Name: "outbox"}

	pending, dead, err := a.PendingEvents(ctx)
	if err != nil {
		d.Status, d.Detail = StatusFail, "cannot read the outbox: "+err.Error()
		return d
	}

	var oldest sql.NullTime
	_ = a.db.QueryRowContext(ctx, `
		SELECT min(created_at) FROM outbox_events
		WHERE published_at IS NULL AND NOT dead`).Scan(&oldest)

	parts := []string{fmt.Sprintf("%d pending, %d dead-lettered", pending, dead)}
	d.Status = StatusOK

	if oldest.Valid {
		age := time.Since(oldest.Time)
		parts = append(parts, fmt.Sprintf("oldest unpublished %s", age.Round(time.Second)))
		// The dispatcher polls every second and backs off exponentially on
		// failure. Minutes of backlog means it is failing, not busy.
		if age > 5*time.Minute {
			d.Status = StatusFail
			d.Hint = "the dispatcher is not draining; check handler errors in the log"
		} else if age > 30*time.Second {
			d.Status = StatusWarn
			d.Hint = "backlog is growing; check whether a handler is slow or erroring"
		}
	}
	if dead > 0 && d.Status == StatusOK {
		d.Status = StatusWarn
		d.Hint = "dead-lettered events exhausted their retries and will never be delivered; inspect outbox_events.last_error"
	}
	d.Detail = strings.Join(parts, ", ")
	return d
}

// checkReservations looks for stock held by orders that will never be paid.
// A reservation outliving its order is how a store silently goes out of stock
// while the shelves are full.
func (a *App) checkReservations(ctx context.Context) Diagnostic {
	d := Diagnostic{Name: "stock reservations"}

	var stale int
	var units sql.NullInt64
	err := a.db.QueryRowContext(ctx, `
		SELECT count(DISTINCT o.id), sum(ol.quantity)
		FROM orders o
		JOIN order_lines ol ON ol.order_id = o.id
		WHERE o.status = 'pending'
		  AND o.payment_status = 'pending'
		  AND o.reservation_expires_at IS NOT NULL
		  AND o.reservation_expires_at < now()`).Scan(&stale, &units)
	if err != nil {
		d.Status, d.Detail = StatusWarn, "cannot inspect reservations: "+err.Error()
		return d
	}

	if stale == 0 {
		d.Status, d.Detail = StatusOK, "no expired reservations"
		return d
	}
	d.Status = StatusWarn
	d.Detail = fmt.Sprintf("%d unpaid order(s) past their reservation window holding %d unit(s)", stale, units.Int64)
	d.Hint = "the sweeper releases these on its next pass; if the count keeps growing, check that background work is running"
	return d
}

func (a *App) checkCarts(ctx context.Context) Diagnostic {
	d := Diagnostic{Name: "carts"}

	var open, expired int
	err := a.db.QueryRowContext(ctx, `
		SELECT count(*) FILTER (WHERE status = 'open'),
		       count(*) FILTER (WHERE status = 'open' AND expires_at < now())
		FROM carts`).Scan(&open, &expired)
	if err != nil {
		d.Status, d.Detail = StatusWarn, "cannot inspect carts: "+err.Error()
		return d
	}

	d.Status, d.Detail = StatusOK, fmt.Sprintf("%d open, %d past TTL", open, expired)
	// POST /api/carts is unauthenticated, so unswept carts are an
	// unbounded-growth vector rather than merely untidy.
	if expired > 1000 {
		d.Status = StatusWarn
		d.Hint = "expired carts are not being swept; check that background work is running"
	}
	return d
}

func (a *App) checkCatalog(ctx context.Context) Diagnostic {
	d := Diagnostic{Name: "catalog"}

	var products, active, orphans, oversold int
	err := a.db.QueryRowContext(ctx, `
		SELECT
			(SELECT count(*) FROM products),
			(SELECT count(*) FROM products WHERE status = 'active'),
			-- An active product with no sellable variant is invisible to
			-- shoppers while looking fine in the admin list.
			(SELECT count(*) FROM products p WHERE p.status = 'active'
			   AND NOT EXISTS (SELECT 1 FROM variants v WHERE v.product_id = p.id AND v.active)),
			-- Mirroring variants_reserved_within_on_hand, which M12 conditioned
			-- on continue_selling: a variant that sells past zero is *meant* to
			-- go negative, and counting it here reported a supported state as
			-- impossible and sent the operator to check a constraint that was
			-- working.
			--
			-- M17 moved the count onto (variant, location) rows and the CHECK
			-- could not follow it: continue_selling is on the variant, and
			-- copying the flag onto every stock row to keep a conditional CHECK
			-- would be a stored duplicate. reserveStock's conditional UPDATE
			-- does the enforcing now, and this is where a drift from it shows.
			(SELECT count(*) FROM variant_stock vs
			   JOIN variants v ON v.id = vs.variant_id
			  WHERE NOT v.continue_selling AND vs.reserved > vs.on_hand)
	`).Scan(&products, &active, &orphans, &oversold)
	if err != nil {
		d.Status, d.Detail = StatusWarn, "cannot inspect the catalog: "+err.Error()
		return d
	}

	d.Status = StatusOK
	d.Detail = fmt.Sprintf("%d product(s), %d active", products, active)

	if orphans > 0 {
		d.Status = StatusWarn
		d.Detail += fmt.Sprintf(", %d active with no sellable variant", orphans)
		d.Hint = "those products cannot be bought; add an active variant or set the product to draft"
	}
	// The database has a CHECK constraint for this, so a hit here means the
	// constraint is missing — a hand-edited schema, or a restore from a dump
	// that dropped it.
	if oversold > 0 {
		d.Status = StatusFail
		d.Detail += fmt.Sprintf(", %d variant(s) reserved beyond stock", oversold)
		d.Hint = "this should be impossible: verify variants_reserved_within_on_hand still exists"
	}
	return d
}

func (a *App) checkProviders() Diagnostic {
	d := Diagnostic{Name: "providers"}

	pay := make([]string, 0, len(a.payments.providers))
	for code := range a.payments.providers {
		pay = append(pay, code)
	}
	sort.Strings(pay)

	ship := make([]string, 0, len(a.fulfillment.providers))
	for code := range a.fulfillment.providers {
		ship = append(ship, code)
	}
	sort.Strings(ship)

	d.Status = StatusOK
	d.Detail = fmt.Sprintf("payment: %s; fulfillment: %s; modules: %d",
		strings.Join(pay, ", "), strings.Join(ship, ", "), len(a.modules))
	if len(pay) == 0 {
		d.Status = StatusFail
		d.Detail = "no payment providers registered — checkout is impossible"
		d.Hint = "cash on delivery is built in; if it is missing, core wiring did not run"
	}
	return d
}

// checkContract compares what is served against what is documented. Drift here
// is invisible in production and only bites an integrator, which is exactly the
// kind of problem worth having a machine notice.
func (a *App) checkContract() Diagnostic {
	d := Diagnostic{Name: "api contract"}

	documented, err := a.SpecPaths()
	if err != nil {
		d.Status, d.Detail = StatusWarn, "cannot read the OpenAPI document: "+err.Error()
		return d
	}
	have := make(map[string]bool, len(documented))
	for _, p := range documented {
		have[p] = true
	}

	var missing []string
	for _, r := range a.Routes() {
		if r.UI {
			continue // the panel's files are not an API surface
		}
		if !have[r.Path] {
			missing = append(missing, r.Method+" "+r.Path)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		d.Status = StatusWarn
		d.Detail = fmt.Sprintf("%d served route(s) missing from /doc: %s", len(missing), strings.Join(missing, ", "))
		d.Hint = "add them to openapi.json, or to the module's OpenAPI() fragment"
		return d
	}
	d.Status = StatusOK
	d.Detail = fmt.Sprintf("%d documented path(s) cover every served route", len(documented))
	return d
}

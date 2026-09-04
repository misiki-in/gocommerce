// Package invoices issues a numbered invoice for every paid order.
//
// An order is not an invoice: an order is what a shopper asked for, while an
// invoice is an accounting document with its own gapless sequence and its own
// retention rules. Keeping them apart is why this is a module and not a column
// on the order.
//
//	app, err := gocommerce.New(cfg,
//	    invoices.New(invoices.Config{
//	        SellerName: "Example Ltd",
//	        NumberFormat: "INV-{year}-{seq:05}",
//	    }),
//	)
package invoices

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/misiki/gocommerce/core"
)

// Config configures the module.
type Config struct {
	// SellerName and SellerAddress head the document. Required.
	SellerName    string
	SellerAddress string
	// TaxID is printed alongside the seller, for jurisdictions that need it —
	// a GSTIN, a VAT number, whatever applies.
	TaxID string
	// NumberFormat builds the invoice number. {year} is the issuing year and
	// {seq} the sequence within it; {seq:05} pads to five digits.
	// Defaults to "INV-{year}-{seq:05}".
	NumberFormat string
	// Footer is printed at the bottom of every invoice.
	Footer string
}

// Module issues invoices.
type Module struct {
	cfg    Config
	db     *sql.DB
	log    *slog.Logger
	orders *gocommerce.Orders
	tpl    *template.Template
}

// New constructs the module.
func New(cfg Config) *Module { return &Module{cfg: cfg} }

// Name implements gocommerce.Module.
func (m *Module) Name() string { return "invoices" }

// Migrations implements gocommerce.Module.
func (m *Module) Migrations() []gocommerce.Migration {
	return []gocommerce.Migration{{
		ID: "0001_invoices",
		SQL: `
			CREATE TABLE invoices_documents (
			    id         bigserial   PRIMARY KEY,
			    order_id   bigint      NOT NULL UNIQUE,
			    number     text        NOT NULL UNIQUE,
			    year       integer     NOT NULL,
			    sequence   integer     NOT NULL,
			    issued_at  timestamptz NOT NULL DEFAULT now(),
			    total_minor bigint     NOT NULL,
			    currency   text        NOT NULL,
			    snapshot   jsonb       NOT NULL,
			    UNIQUE (year, sequence)
			);
			CREATE INDEX invoices_documents_issued_idx ON invoices_documents (issued_at DESC);

			-- The sequence lives in a row rather than a PostgreSQL sequence
			-- because an invoice sequence must have no gaps: a rolled-back
			-- transaction has to give its number back, and a sequence would
			-- not.
			CREATE TABLE invoices_counters (
			    year integer PRIMARY KEY,
			    next integer NOT NULL
			);`,
	}}
}

// Register implements gocommerce.Module.
func (m *Module) Register(app *gocommerce.App) error {
	if strings.TrimSpace(m.cfg.SellerName) == "" {
		return errors.New("invoices: SellerName is required")
	}
	if m.cfg.NumberFormat == "" {
		m.cfg.NumberFormat = "INV-{year}-{seq:05}"
	}
	m.db = app.DB()
	m.log = app.Log()
	m.orders = app.Order()

	tpl, err := template.New("invoice").Parse(invoiceHTML)
	if err != nil {
		return fmt.Errorf("invoices: parse template: %w", err)
	}
	m.tpl = tpl

	app.Subscribe(gocommerce.EventOrderPaid, m.onOrderPaid)

	app.HandleAdminFunc("GET /api/admin/x/invoices", m.handleList)
	app.HandleAdminFunc("GET /api/admin/x/invoices/{orderId}", m.handleGet)

	// Delivery is at-least-once but not guaranteed to have happened before a
	// crash, and an invoice that was never issued is an accounting hole. So
	// the module also reconciles against the orders table at startup rather
	// than trusting that it saw every event.
	app.OnStart(m.reconcile)
	return nil
}

// onOrderPaid issues the invoice. It is idempotent because event delivery is
// at-least-once: the unique constraint on order_id is what makes a redelivery
// a no-op rather than a duplicate document.
func (m *Module) onOrderPaid(ctx context.Context, e gocommerce.Event) error {
	var ev gocommerce.OrderEvent
	if err := e.Decode(&ev); err != nil {
		return err
	}
	return m.issue(ctx, ev.OrderID)
}

func (m *Module) issue(ctx context.Context, orderID int64) error {
	var exists bool
	if err := m.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM invoices_documents WHERE order_id = $1)`, orderID).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}

	order, err := m.orders.Get(ctx, orderID)
	if err != nil {
		return err
	}
	snapshot, err := json.Marshal(order)
	if err != nil {
		return err
	}

	return gocommerce.InTx(ctx, m.db, func(tx *sql.Tx) error {
		year := time.Now().UTC().Year()

		// Take the next number under a row lock. The UPDATE locks the year's
		// counter row, so two concurrent deliveries queue rather than both
		// being handed the same number — and because it is inside this
		// transaction, a rollback gives the number back and the sequence
		// stays gapless.
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO invoices_counters (year, next) VALUES ($1, 1)
			 ON CONFLICT (year) DO NOTHING`, year); err != nil {
			return err
		}
		var seq int
		if err := tx.QueryRowContext(ctx,
			`UPDATE invoices_counters SET next = next + 1 WHERE year = $1 RETURNING next - 1`,
			year).Scan(&seq); err != nil {
			return err
		}

		number := formatNumber(m.cfg.NumberFormat, year, seq)
		_, err = tx.ExecContext(ctx, `
			INSERT INTO invoices_documents (order_id, number, year, sequence, total_minor, currency, snapshot)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (order_id) DO NOTHING`,
			orderID, number, year, seq, order.Total.AmountMinor, order.Currency, snapshot)
		if err != nil {
			return err
		}
		m.log.Info("invoice issued", "invoice", number, "order", order.Number)
		return nil
	})
}

// reconcile issues invoices for paid orders that have none — the safety net
// under at-least-once delivery, because an event that was lost before this
// module ever ran would otherwise leave a permanent gap.
func (m *Module) reconcile(ctx context.Context) error {
	rows, err := m.db.QueryContext(ctx, `
		SELECT o.id FROM orders o
		LEFT JOIN invoices_documents i ON i.order_id = o.id
		WHERE o.payment_status = 'paid' AND i.id IS NULL
		ORDER BY o.id LIMIT 500`)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, id := range ids {
		if err := m.issue(ctx, id); err != nil {
			m.log.Error("could not issue a missing invoice", "order_id", id, "error", err)
		}
	}
	if len(ids) > 0 {
		m.log.Info("issued invoices missing for paid orders", "count", len(ids))
	}
	return nil
}

// formatNumber expands {year} and {seq}, with optional zero padding.
func formatNumber(format string, year, seq int) string {
	out := strings.ReplaceAll(format, "{year}", strconv.Itoa(year))
	if i := strings.Index(out, "{seq:"); i >= 0 {
		if j := strings.Index(out[i:], "}"); j > 0 {
			spec := out[i+len("{seq:") : i+j]
			width, err := strconv.Atoi(spec)
			if err == nil {
				out = out[:i] + fmt.Sprintf("%0*d", width, seq) + out[i+j+1:]
			}
		}
	}
	return strings.ReplaceAll(out, "{seq}", strconv.Itoa(seq))
}

// ---------------------------------------------------------------- endpoints

func (m *Module) handleList(w http.ResponseWriter, r *http.Request) {
	limit, offset, err := gocommerce.Page(r)
	if err != nil {
		gocommerce.RespondError(w, r, err)
		return
	}
	var total int
	if err := m.db.QueryRowContext(r.Context(),
		`SELECT count(*) FROM invoices_documents`).Scan(&total); err != nil {
		gocommerce.RespondError(w, r, err)
		return
	}
	rows, err := m.db.QueryContext(r.Context(), `
		SELECT id, order_id, number, issued_at, total_minor, currency
		FROM invoices_documents ORDER BY id DESC LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		gocommerce.RespondError(w, r, err)
		return
	}
	defer rows.Close()

	type row struct {
		ID       int64            `json:"id"`
		OrderID  int64            `json:"order_id"`
		Number   string           `json:"number"`
		IssuedAt time.Time        `json:"issued_at"`
		Total    gocommerce.Money `json:"total"`
	}
	list := []row{}
	for rows.Next() {
		var v row
		if err := rows.Scan(&v.ID, &v.OrderID, &v.Number, &v.IssuedAt,
			&v.Total.AmountMinor, &v.Total.Currency); err != nil {
			gocommerce.RespondError(w, r, err)
			return
		}
		list = append(list, v)
	}
	gocommerce.RespondList(w, list, gocommerce.ListMeta{Total: total, Limit: limit, Offset: offset})
}

func (m *Module) handleGet(w http.ResponseWriter, r *http.Request) {
	orderID, err := strconv.ParseInt(r.PathValue("orderId"), 10, 64)
	if err != nil || orderID <= 0 {
		gocommerce.RespondError(w, r, gocommerce.Validationf("orderId must be a positive integer"))
		return
	}

	var number string
	var issuedAt time.Time
	var snapshot []byte
	err = m.db.QueryRowContext(r.Context(),
		`SELECT number, issued_at, snapshot FROM invoices_documents WHERE order_id = $1`,
		orderID).Scan(&number, &issuedAt, &snapshot)
	if errors.Is(err, sql.ErrNoRows) {
		gocommerce.RespondError(w, r, gocommerce.NotFoundf("no invoice has been issued for order %d", orderID))
		return
	}
	if err != nil {
		gocommerce.RespondError(w, r, err)
		return
	}

	var order gocommerce.Order
	if err := json.Unmarshal(snapshot, &order); err != nil {
		gocommerce.RespondError(w, r, err)
		return
	}

	// JSON for a client that wants the data, HTML for a human who wants to
	// print it.
	if strings.Contains(r.Header.Get("Accept"), "application/json") {
		gocommerce.Respond(w, http.StatusOK, map[string]any{
			"number": number, "issued_at": issuedAt, "order": order,
		})
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := m.tpl.Execute(w, map[string]any{
		"Number": number, "IssuedAt": issuedAt.UTC().Format("2 January 2006"),
		"Order": order, "Config": m.cfg,
	}); err != nil {
		m.log.Error("could not render the invoice", "order_id", orderID, "error", err)
	}
}

const invoiceHTML = `<!doctype html>
<html><head><meta charset="utf-8"><title>Invoice {{.Number}}</title>
<style>
 body{font-family:system-ui,-apple-system,"Segoe UI",sans-serif;max-width:46rem;margin:3rem auto;padding:0 1.5rem;color:#111;line-height:1.5}
 header{display:flex;justify-content:space-between;align-items:flex-start;gap:2rem;border-bottom:2px solid #111;padding-bottom:1rem}
 h1{font-size:1.5rem;margin:0 0 .25rem}
 .muted{color:#555;font-size:.875rem}
 table{width:100%;border-collapse:collapse;margin:2rem 0}
 th,td{text-align:left;padding:.5rem 0;border-bottom:1px solid #ddd}
 td.num,th.num{text-align:right;font-variant-numeric:tabular-nums}
 tfoot td{border:0;padding-top:.4rem}
 tfoot tr:last-child td{font-weight:700;border-top:2px solid #111;padding-top:.6rem}
 footer{margin-top:3rem;color:#555;font-size:.8125rem}
</style></head>
<body>
<header>
  <div>
    <h1>{{.Config.SellerName}}</h1>
    <div class="muted">{{.Config.SellerAddress}}{{if .Config.TaxID}}<br>{{.Config.TaxID}}{{end}}</div>
  </div>
  <div class="muted" style="text-align:right">
    <strong>Invoice {{.Number}}</strong><br>
    Issued {{.IssuedAt}}<br>
    Order {{.Order.Number}}
  </div>
</header>

<p><strong>Billed to</strong><br>
{{with .Order}}{{.Name}}<br>{{.Email}}<br>
{{.Address.Line1}}{{if .Address.Line2}}, {{.Address.Line2}}{{end}}<br>
{{.Address.City}} {{.Address.PostalCode}}<br>{{.Address.Country}}{{end}}</p>

<table>
  <thead><tr><th>Item</th><th class="num">Qty</th><th class="num">Unit</th><th class="num">Total</th></tr></thead>
  <tbody>
  {{range .Order.Lines}}
    <tr>
      <td>{{.Title}}{{if .VariantLabel}} <span class="muted">({{.VariantLabel}})</span>{{end}}<br><span class="muted">{{.SKU}}</span></td>
      <td class="num">{{.Quantity}}</td>
      <td class="num">{{.UnitPrice.Currency}} {{.UnitPrice.AmountMinor}}</td>
      <td class="num">{{.Total.Currency}} {{.Total.AmountMinor}}</td>
    </tr>
  {{end}}
  </tbody>
  <tfoot>
    <tr><td colspan="3" class="num">Subtotal</td><td class="num">{{.Order.Subtotal.Currency}} {{.Order.Subtotal.AmountMinor}}</td></tr>
    {{if .Order.Shipping.AmountMinor}}<tr><td colspan="3" class="num">Shipping</td><td class="num">{{.Order.Shipping.Currency}} {{.Order.Shipping.AmountMinor}}</td></tr>{{end}}
    {{if .Order.Discount.AmountMinor}}<tr><td colspan="3" class="num">Discount</td><td class="num">-{{.Order.Discount.Currency}} {{.Order.Discount.AmountMinor}}</td></tr>{{end}}
    <tr><td colspan="3" class="num">Total</td><td class="num">{{.Order.Total.Currency}} {{.Order.Total.AmountMinor}}</td></tr>
  </tfoot>
</table>

<p class="muted">Amounts are shown in minor units of {{.Order.Currency}}.</p>
{{if .Config.Footer}}<footer>{{.Config.Footer}}</footer>{{end}}
</body></html>
`

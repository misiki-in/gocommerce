package gocommerce

import (
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// CSV import and export. Spreadsheets are the actual tool most operators reach
// for, so the format is denormalised — one row per variant, one row per order
// line — rather than shaped for a machine that could have used the API.

// productCSVHeader is the export format, and the set of columns import
// understands. Versioning it by column name means a file written by an older
// release still imports as long as the columns it has are still meaningful.
//
// The stock column is the one that is not fixed. A spreadsheet has one cell per
// variant per column and stock has a place now, so the header carries the place:
// see stockColumnPrefix.
var productCSVHeader = []string{
	"product_slug", "product_title", "product_description", "product_status",
	"sku", "barcode", "variant_options", "price_minor", "compare_at_price_minor",
	"stock_on_hand", "track_inventory", "continue_selling", "active",
	"weight_grams", "origin_country", "hs_code", "metadata",
}

// stockColumnPrefix names a location inside a column heading:
// `stock_on_hand:warehouse` is the count at the location whose code is
// `warehouse`.
//
// The bare `stock_on_hand` still means the default location, so every file ever
// written by this exporter, and every file an operator has in a folder
// somewhere, still imports and still means what it meant. A store with one
// location — which is most of them — never sees a suffix at all.
//
// The location is named by code rather than by id because a CSV is a document
// people edit and mail to each other. `warehouse` survives being re-imported
// into a different store; `3` silently means something else there.
const stockColumnPrefix = "stock_on_hand:"

// stockColumn ties one CSV column to one location.
type stockColumn struct {
	name       string
	code       string
	locationID int64
}

// productStockColumns is the stock part of the export header.
//
// One column per location, always — not one per location that currently holds
// something. The header would otherwise change shape as stock moved, which
// breaks anyone diffing two exports or scripting against the columns, and it
// would leave no cell to type into to receive stock somewhere empty.
func (t *Transfer) productStockColumns(ctx context.Context) ([]stockColumn, error) {
	rows, err := t.app.db.QueryContext(ctx,
		`SELECT id, code, is_default FROM locations ORDER BY priority, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var cols []stockColumn
	var single stockColumn
	n := 0
	for rows.Next() {
		var c stockColumn
		var isDefault bool
		if err := rows.Scan(&c.locationID, &c.code, &isDefault); err != nil {
			return nil, err
		}
		c.name = stockColumnPrefix + c.code
		cols = append(cols, c)
		if isDefault {
			single = c
		}
		n++
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if n == 1 {
		// A single-location store gets exactly the file it got before locations
		// existed, byte for byte.
		single.name = "stock_on_hand"
		return []stockColumn{single}, nil
	}
	return cols, nil
}

// resolveStockColumns works out which locations a file's stock columns name.
//
// It runs once, against the header, rather than per row. A misspelt location
// code is one mistake in one header cell and it affects every line in the file,
// so it fails the import with a sentence naming the code — the same treatment a
// missing required column already gets. Reporting it a thousand times, once per
// row, would bury the one fact the operator needs.
func (t *Transfer) resolveStockColumns(ctx context.Context, header []string) ([]stockColumn, error) {
	var named []string
	plain := false
	for _, raw := range header {
		name := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(raw, "\ufeff")))
		switch {
		case name == "stock_on_hand":
			plain = true
		case strings.HasPrefix(name, stockColumnPrefix):
			named = append(named, strings.TrimPrefix(name, stockColumnPrefix))
		}
	}

	if plain && len(named) > 0 {
		return nil, Validationf(
			"the CSV has both %q and per-location stock columns; use one form or the other, "+
				"because there is no way to tell which one a row means",
			"stock_on_hand")
	}
	if !plain && len(named) == 0 {
		// No stock columns at all is fine and common — a price list, say. The
		// import simply does not touch stock.
		return nil, nil
	}

	if plain {
		var c stockColumn
		err := t.app.db.QueryRowContext(ctx,
			`SELECT id, code FROM locations WHERE is_default`).Scan(&c.locationID, &c.code)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, Conflictf("this store has no default location")
		}
		if err != nil {
			return nil, err
		}
		c.name = "stock_on_hand"
		return []stockColumn{c}, nil
	}

	cols := make([]stockColumn, 0, len(named))
	for _, code := range named {
		var c stockColumn
		err := t.app.db.QueryRowContext(ctx,
			`SELECT id, code FROM locations WHERE code = $1`, code).Scan(&c.locationID, &c.code)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, Validationf(
				"the CSV column %q names a location that does not exist; "+
					"open it first, or fix the code",
				stockColumnPrefix+code)
		}
		if err != nil {
			return nil, err
		}
		c.name = stockColumnPrefix + c.code
		cols = append(cols, c)
	}
	return cols, nil
}

// productHeaderFor is productCSVHeader with the stock column expanded.
func productHeaderFor(cols []stockColumn) []string {
	out := make([]string, 0, len(productCSVHeader)+len(cols)-1)
	for _, name := range productCSVHeader {
		if name != "stock_on_hand" {
			out = append(out, name)
			continue
		}
		for _, c := range cols {
			out = append(out, c.name)
		}
	}
	return out
}

var orderCSVHeader = []string{
	"number", "created_at", "status", "payment_status", "payment_provider",
	"currency", "email", "phone", "name",
	"address_line1", "address_line2", "city", "state", "postal_code", "country",
	"subtotal_minor", "shipping_minor", "discount_minor", "total_minor", "language",
	"sku", "title", "variant_label", "quantity", "unit_price_minor", "line_total_minor",
}

// ImportResult reports what an import did. Row errors never abort the file:
// one bad line in a thousand should not cost the other 999.
type ImportResult struct {
	Created  int        `json:"created"`
	Updated  int        `json:"updated"`
	Skipped  int        `json:"skipped"`
	Errors   []RowError `json:"errors"`
	DryRun   bool       `json:"dry_run"`
	Duration string     `json:"duration,omitempty"`
}

// RowError names the line so an operator can go and fix it.
type RowError struct {
	Line    int    `json:"line"`
	Message string `json:"message"`
}

// Transfer owns CSV import and export.
type Transfer struct {
	app *App
}

// Data returns the import/export service.
func (a *App) Data() *Transfer { return a.transfer }

// ------------------------------------------------------------------ export

// ExportProducts streams the catalog as CSV, one row per variant.
func (t *Transfer) ExportProducts(ctx context.Context, out io.Writer) error {
	w := csv.NewWriter(out)
	defer w.Flush()

	stockCols, err := t.productStockColumns(ctx)
	if err != nil {
		return err
	}
	if err := w.Write(productHeaderFor(stockCols)); err != nil {
		return err
	}

	// One scalar subquery per location rather than a join or a pre-loaded map:
	// the export streams, and holding a row per variant per location in memory
	// would cost about what the catalog itself costs.
	stockSelect := ""
	args := make([]any, 0, len(stockCols))
	for _, c := range stockCols {
		args = append(args, c.locationID)
		stockSelect += fmt.Sprintf(
			"coalesce((SELECT vs.on_hand FROM variant_stock vs"+
				" WHERE vs.variant_id = v.id AND vs.location_id = $%d), 0), ", len(args))
	}

	rows, err := t.app.db.QueryContext(ctx, `
		SELECT p.slug, p.title, p.description, p.status,
		       v.sku, coalesce(v.barcode, ''),
		       coalesce((
		           SELECT string_agg(o.name || '=' || pov.value, '|' ORDER BY o.position, o.id)
		           FROM variant_option_values vov
		           JOIN product_option_values pov ON pov.id = vov.option_value_id
		           JOIN product_options o ON o.id = pov.option_id
		           WHERE vov.variant_id = v.id
		       ), ''),
		       v.price_minor, v.compare_at_price_minor, `+stockSelect+`
		       v.track_inventory, v.continue_selling, v.active,
		       v.weight_grams, v.origin_country, v.hs_code, v.metadata
		FROM variants v
		JOIN products p ON p.id = v.product_id
		ORDER BY p.id, v.position, v.id`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var slug, title, desc, status, sku, barcode, options string
		var price int64
		var compareAt, weight sql.NullInt64
		stock := make([]int, len(stockCols))
		var tracks, oversell, active bool
		var origin, hs string
		var meta []byte
		dest := []any{&slug, &title, &desc, &status, &sku, &barcode, &options,
			&price, &compareAt}
		for i := range stock {
			dest = append(dest, &stock[i])
		}
		dest = append(dest, &tracks, &oversell, &active, &weight, &origin, &hs, &meta)
		if err := rows.Scan(dest...); err != nil {
			return err
		}
		record := []string{
			slug, title, desc, status, sku, barcode, options,
			strconv.FormatInt(price, 10), nullIntString(compareAt),
		}
		for _, n := range stock {
			record = append(record, strconv.Itoa(n))
		}
		record = append(record,
			strconv.FormatBool(tracks), strconv.FormatBool(oversell),
			strconv.FormatBool(active),
			nullIntString(weight), origin, hs, string(meta),
		)
		if err := w.Write(escapeRecord(record)); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	w.Flush()
	return w.Error()
}

// ExportOrders streams orders as CSV, one row per order line, with the order's
// own columns repeated. That shape is what an accountant's pivot table wants.
func (t *Transfer) ExportOrders(ctx context.Context, out io.Writer, q OrderQuery) error {
	w := csv.NewWriter(out)
	defer w.Flush()
	if err := w.Write(orderCSVHeader); err != nil {
		return err
	}

	where, args := []string{"1 = 1"}, []any{}
	add := func(expr string, v any) {
		args = append(args, v)
		where = append(where, fmt.Sprintf(expr, len(args)))
	}
	if q.Status != "" {
		add("o.status = $%d", q.Status)
	}
	if q.From != nil {
		add("o.created_at >= $%d", *q.From)
	}
	if q.To != nil {
		add("o.created_at < $%d", *q.To)
	}

	rows, err := t.app.db.QueryContext(ctx, `
		SELECT o.number, o.created_at, o.status, o.payment_status, o.payment_provider,
		       o.currency, o.email, coalesce(o.phone, ''), coalesce(o.name, ''), o.address,
		       o.subtotal_minor, o.shipping_minor, o.discount_minor, o.total_minor, o.lang,
		       l.sku, l.title, l.variant_label, l.quantity, l.unit_price_minor, l.total_minor
		FROM orders o
		JOIN order_lines l ON l.order_id = o.id
		WHERE `+strings.Join(where, " AND ")+`
		ORDER BY o.id, l.id`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var number, status, payStatus, provider, currency, email, phone, name, lang string
		var createdAt time.Time
		var addrRaw []byte
		var subtotal, shipping, discount, total, unitPrice, lineTotal int64
		var sku, title, label string
		var qty int
		if err := rows.Scan(&number, &createdAt, &status, &payStatus, &provider,
			&currency, &email, &phone, &name, &addrRaw,
			&subtotal, &shipping, &discount, &total, &lang,
			&sku, &title, &label, &qty, &unitPrice, &lineTotal); err != nil {
			return err
		}
		var addr Address
		_ = json.Unmarshal(addrRaw, &addr)

		record := []string{
			number, createdAt.UTC().Format(time.RFC3339), status, payStatus, provider,
			currency, email, phone, name,
			addr.Line1, addr.Line2, addr.City, addr.State, addr.PostalCode, addr.Country,
			strconv.FormatInt(subtotal, 10), strconv.FormatInt(shipping, 10),
			strconv.FormatInt(discount, 10), strconv.FormatInt(total, 10), lang,
			sku, title, label, strconv.Itoa(qty),
			strconv.FormatInt(unitPrice, 10), strconv.FormatInt(lineTotal, 10),
		}
		if err := w.Write(escapeRecord(record)); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	w.Flush()
	return w.Error()
}

// ------------------------------------------------------------------ import

// ImportProducts upserts products and variants from CSV, keyed on SKU.
//
// Rows for one product must be contiguous, which is what export produces and
// what lets this stream a large file instead of holding it in memory. Each
// product is one transaction, so a failure leaves neither a half-built product
// nor a poisoned import.
func (t *Transfer) ImportProducts(ctx context.Context, in io.Reader, dryRun bool) (*ImportResult, error) {
	start := time.Now()
	r := csv.NewReader(in)
	r.FieldsPerRecord = -1

	header, err := r.Read()
	if err != nil {
		return nil, Validationf("could not read the CSV header: %v", err)
	}
	cols := indexColumns(header)
	for _, required := range []string{"product_slug", "sku", "price_minor"} {
		if _, ok := cols[required]; !ok {
			return nil, Validationf("the CSV is missing the required column %q", required)
		}
	}
	stockCols, err := t.resolveStockColumns(ctx, header)
	if err != nil {
		return nil, err
	}

	result := &ImportResult{DryRun: dryRun}
	var group []csvRow
	var groupSlug string

	flush := func() {
		if len(group) == 0 {
			return
		}
		t.importProductGroup(ctx, group, stockCols, dryRun, result)
		group = nil
	}

	line := 1
	for {
		record, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		line++
		if err != nil {
			result.Errors = append(result.Errors, RowError{Line: line, Message: err.Error()})
			continue
		}
		row := csvRow{line: line, cols: cols, values: unescapeRecord(record)}
		slug := row.get("product_slug")
		if slug == "" {
			result.Errors = append(result.Errors, RowError{Line: line, Message: "product_slug is empty"})
			continue
		}
		if slug != groupSlug {
			flush()
			groupSlug = slug
		}
		group = append(group, row)
	}
	flush()

	result.Duration = time.Since(start).String()
	return result, nil
}

// importProductGroup writes one product and its variants in one transaction.
func (t *Transfer) importProductGroup(ctx context.Context, rows []csvRow, stockCols []stockColumn, dryRun bool, result *ImportResult) {
	first := rows[0]
	slug := first.get("product_slug")
	var created bool

	err := InTx(ctx, t.app.db, func(tx *sql.Tx) error {
		var productID int64
		created = false
		err := tx.QueryRowContext(ctx, `SELECT id FROM products WHERE slug = $1`, slug).Scan(&productID)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			title := first.get("product_title")
			if title == "" {
				title = slug
			}
			status := first.get("product_status")
			if !validProductStatus(status) {
				status = ProductDraft
			}
			if err := tx.QueryRowContext(ctx, `
				INSERT INTO products (slug, title, description, status, currency)
				VALUES ($1, $2, $3, $4, $5) RETURNING id`,
				slug, title, first.get("product_description"), status,
				t.app.cfg.Currency).Scan(&productID); err != nil {
				return translateCatalogErr(err)
			}
			created = true
		case err != nil:
			return err
		default:
			sets, args := []string{}, []any{}
			if v := first.get("product_title"); v != "" {
				args = append(args, v)
				sets = append(sets, fmt.Sprintf("title = $%d", len(args)))
			}
			if v := first.get("product_description"); v != "" {
				args = append(args, v)
				sets = append(sets, fmt.Sprintf("description = $%d", len(args)))
			}
			if v := first.get("product_status"); validProductStatus(v) {
				args = append(args, v)
				sets = append(sets, fmt.Sprintf("status = $%d", len(args)))
			}
			if len(sets) > 0 {
				args = append(args, productID)
				if _, err := tx.ExecContext(ctx,
					"UPDATE products SET "+strings.Join(sets, ", ")+", updated_at = now()"+
						fmt.Sprintf(" WHERE id = $%d", len(args)), args...); err != nil {
					return translateCatalogErr(err)
				}
			}
		}

		for _, row := range rows {
			if err := t.importVariantRow(ctx, tx, productID, row, stockCols, result); err != nil {
				return err
			}
		}
		if dryRun {
			// Everything above proved the file would apply; rolling back is
			// what makes it a rehearsal rather than a change.
			return errDryRun
		}
		return nil
	})

	// Counters are updated only after the transaction resolves, so the report
	// describes what is in the database rather than what was attempted.
	switch {
	case err == nil, errors.Is(err, errDryRun):
		if created {
			result.Created++
		} else {
			result.Updated++
		}
	default:
		result.Errors = append(result.Errors, RowError{Line: first.line, Message: err.Error()})
		result.Skipped += len(rows)
	}
}

var errDryRun = errors.New("dry run")

// importVariantRow upserts one variant, creating any option values it names.
func (t *Transfer) importVariantRow(ctx context.Context, tx *sql.Tx, productID int64, row csvRow, stockCols []stockColumn, result *ImportResult) error {
	sku := row.get("sku")
	if sku == "" {
		return fmt.Errorf("line %d: sku is empty", row.line)
	}
	price, err := row.int64("price_minor")
	if err != nil {
		return fmt.Errorf("line %d: %v", row.line, err)
	}

	valueIDs, err := t.ensureOptions(ctx, tx, productID, row.get("variant_options"))
	if err != nil {
		return fmt.Errorf("line %d: %v", row.line, err)
	}
	key := optionKey(valueIDs)

	tracks := row.boolDefault("track_inventory", true)
	oversell := row.boolDefault("continue_selling", false)
	active := row.boolDefault("active", true)
	compareAt := row.nullInt64("compare_at_price_minor")
	weight := row.nullInt64("weight_grams")
	// Validated here rather than left to the CHECK, so a mistyped cell is a row
	// error naming its line instead of a constraint violation that takes the
	// whole file down.
	origin, err := normalizeOriginCountry(row.get("origin_country"))
	if err != nil {
		return fmt.Errorf("line %d: %v", row.line, err)
	}
	hs, err := normalizeHSCode(row.get("hs_code"))
	if err != nil {
		return fmt.Errorf("line %d: %v", row.line, err)
	}
	meta := row.get("metadata")
	if strings.TrimSpace(meta) == "" {
		meta = "{}"
	}

	var variantID int64
	err = tx.QueryRowContext(ctx, `
		INSERT INTO variants (product_id, sku, barcode, price_minor, compare_at_price_minor,
		                      track_inventory, continue_selling, active,
		                      weight_grams, origin_country, hs_code, option_key, metadata)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		ON CONFLICT (sku) DO UPDATE SET
		    product_id = EXCLUDED.product_id,
		    barcode = EXCLUDED.barcode,
		    price_minor = EXCLUDED.price_minor,
		    compare_at_price_minor = EXCLUDED.compare_at_price_minor,
		    track_inventory = EXCLUDED.track_inventory,
		    continue_selling = EXCLUDED.continue_selling,
		    active = EXCLUDED.active,
		    weight_grams = EXCLUDED.weight_grams,
		    origin_country = EXCLUDED.origin_country,
		    hs_code = EXCLUDED.hs_code,
		    option_key = EXCLUDED.option_key,
		    metadata = EXCLUDED.metadata,
		    updated_at = now()
		RETURNING id`,
		productID, sku, nullString(row.get("barcode")), price, compareAt,
		tracks, oversell, active, weight, origin, hs, key, meta).Scan(&variantID)
	if err != nil {
		return translateCatalogErr(err)
	}

	// The variant needs somewhere to be even when the file says nothing about
	// stock, so that ByLocation and the reservation picker have a row to find.
	def, err := defaultLocationID(ctx, tx)
	if err != nil {
		return err
	}
	if err := ensureStockRow(ctx, tx, variantID, def); err != nil {
		return err
	}

	for _, c := range stockCols {
		// An empty cell is not a zero. Stock is only set where the file
		// actually says a number, because an import that reran must not
		// silently undo sales that happened since it was exported — and because
		// a file listing two of five locations is saying nothing about the
		// other three.
		if !row.has(c.name) {
			continue
		}
		qty, err := row.intDefault(c.name, 0)
		if err != nil {
			return fmt.Errorf("line %d: %v", row.line, err)
		}
		if err := ensureStockRow(ctx, tx, variantID, c.locationID); err != nil {
			return err
		}
		// The floor is reserved, for the reason SetOnHand refuses outright: a
		// count taken on the shop floor does not know about the order that came
		// in while it was being taken, and dropping below what is promised would
		// oversell it. The file loses, the reservation wins.
		//
		// Except for a variant that sells past zero, where a negative count is
		// not a mistake — it is a debt to a customer who has already ordered.
		// Flooring it would let an export-edit-import round trip quietly write
		// that debt off, which is the one thing a round trip must never do. The
		// condition is reserveStock's, and M12's, deliberately.
		if _, err := tx.ExecContext(ctx,
			`UPDATE variant_stock vs
			 SET on_hand = CASE WHEN v.continue_selling
			                    THEN $3 ELSE greatest($3, vs.reserved) END,
			     updated_at = now()
			 FROM variants v
			 WHERE v.id = vs.variant_id
			   AND vs.variant_id = $1 AND vs.location_id = $2`,
			variantID, c.locationID, qty); err != nil {
			return translateCatalogErr(err)
		}
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM variant_option_values WHERE variant_id = $1`, variantID); err != nil {
		return err
	}
	for _, id := range valueIDs {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO variant_option_values (variant_id, option_value_id) VALUES ($1, $2)`,
			variantID, id); err != nil {
			return err
		}
	}
	return nil
}

// ensureOptions parses "Size=M|Color=Black", creating any option or value the
// product does not have yet, and returns the value ids.
func (t *Transfer) ensureOptions(ctx context.Context, tx *sql.Tx, productID int64, spec string) ([]int64, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, nil
	}
	var ids []int64
	for _, pair := range strings.Split(spec, "|") {
		name, value, ok := strings.Cut(pair, "=")
		name, value = strings.TrimSpace(name), strings.TrimSpace(value)
		if !ok || name == "" || value == "" {
			return nil, fmt.Errorf("variant_options entry %q must look like Name=Value", pair)
		}

		var optionID int64
		err := tx.QueryRowContext(ctx,
			`SELECT id FROM product_options WHERE product_id = $1 AND name = $2`,
			productID, name).Scan(&optionID)
		if errors.Is(err, sql.ErrNoRows) {
			if err := tx.QueryRowContext(ctx, `
				INSERT INTO product_options (product_id, name, position)
				VALUES ($1, $2, (SELECT coalesce(max(position) + 1, 0)
				                 FROM product_options WHERE product_id = $1))
				RETURNING id`, productID, name).Scan(&optionID); err != nil {
				return nil, translateCatalogErr(err)
			}
		} else if err != nil {
			return nil, err
		}

		var valueID int64
		err = tx.QueryRowContext(ctx,
			`SELECT id FROM product_option_values WHERE option_id = $1 AND value = $2`,
			optionID, value).Scan(&valueID)
		if errors.Is(err, sql.ErrNoRows) {
			if err := tx.QueryRowContext(ctx, `
				INSERT INTO product_option_values (option_id, value, position)
				VALUES ($1, $2, (SELECT coalesce(max(position) + 1, 0)
				                 FROM product_option_values WHERE option_id = $1))
				RETURNING id`, optionID, value).Scan(&valueID); err != nil {
				return nil, translateCatalogErr(err)
			}
		} else if err != nil {
			return nil, err
		}
		ids = append(ids, valueID)
	}
	return ids, nil
}

// ImportOrders loads historical orders, for a migration from another platform.
//
// It does not touch inventory — the stock movements happened on the old system
// — and it fires no events unless asked, because importing five thousand
// orders must not send five thousand confirmation emails.
func (t *Transfer) ImportOrders(ctx context.Context, in io.Reader, dryRun, fireEvents bool) (*ImportResult, error) {
	start := time.Now()
	r := csv.NewReader(in)
	r.FieldsPerRecord = -1

	header, err := r.Read()
	if err != nil {
		return nil, Validationf("could not read the CSV header: %v", err)
	}
	cols := indexColumns(header)
	for _, required := range []string{"number", "email", "sku", "quantity", "unit_price_minor"} {
		if _, ok := cols[required]; !ok {
			return nil, Validationf("the CSV is missing the required column %q", required)
		}
	}

	result := &ImportResult{DryRun: dryRun}
	var group []csvRow
	var groupNumber string

	flush := func() {
		if len(group) == 0 {
			return
		}
		t.importOrderGroup(ctx, group, dryRun, fireEvents, result)
		group = nil
	}

	line := 1
	for {
		record, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		line++
		if err != nil {
			result.Errors = append(result.Errors, RowError{Line: line, Message: err.Error()})
			continue
		}
		row := csvRow{line: line, cols: cols, values: unescapeRecord(record)}
		number := row.get("number")
		if number == "" {
			result.Errors = append(result.Errors, RowError{Line: line, Message: "number is empty"})
			continue
		}
		if number != groupNumber {
			flush()
			groupNumber = number
		}
		group = append(group, row)
	}
	flush()

	result.Duration = time.Since(start).String()
	return result, nil
}

func (t *Transfer) importOrderGroup(ctx context.Context, rows []csvRow, dryRun, fireEvents bool, result *ImportResult) {
	first := rows[0]
	number := first.get("number")

	err := InTx(ctx, t.app.db, func(tx *sql.Tx) error {
		var exists bool
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM orders WHERE number = $1)`, number).Scan(&exists); err != nil {
			return err
		}
		if exists {
			// Re-importing a file the store itself exported is a no-op rather
			// than a duplicate, which makes the round trip safe to repeat.
			result.Skipped++
			return nil
		}

		status := first.get("status")
		if status == "" {
			status = OrderConfirmed
		}
		payStatus := first.get("payment_status")
		if payStatus == "" {
			payStatus = PaymentPaid
		}
		provider := first.get("payment_provider")
		if provider == "" {
			provider = "imported"
		}
		lang := first.get("language")
		if lang == "" {
			lang = t.app.cfg.DefaultLanguage
		}
		createdAt := time.Now().UTC()
		if v := first.get("created_at"); v != "" {
			if parsed, err := time.Parse(time.RFC3339, v); err == nil {
				createdAt = parsed
			}
		}
		addr, _ := json.Marshal(Address{
			Line1: first.get("address_line1"), Line2: first.get("address_line2"),
			City: first.get("city"), State: first.get("state"),
			PostalCode: first.get("postal_code"), Country: first.get("country"),
		})

		var subtotal int64
		type line struct {
			sku, title, label string
			qty               int
			unit, total       int64
		}
		var lines []line
		for _, row := range rows {
			qty, err := row.intDefault("quantity", 0)
			if err != nil || qty <= 0 {
				return fmt.Errorf("line %d: quantity must be a positive integer", row.line)
			}
			unit, err := row.int64("unit_price_minor")
			if err != nil {
				return fmt.Errorf("line %d: %v", row.line, err)
			}
			total := unit * int64(qty)
			subtotal += total
			lines = append(lines, line{
				sku: row.get("sku"), title: firstNonEmpty(row.get("title"), row.get("sku")),
				label: row.get("variant_label"), qty: qty, unit: unit, total: total,
			})
		}
		shipping, _ := first.int64Default("shipping_minor", 0)
		discount, _ := first.int64Default("discount_minor", 0)
		total, _ := first.int64Default("total_minor", subtotal+shipping-discount)

		accessToken, err := token()
		if err != nil {
			return err
		}
		var orderID int64
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO orders (number, access_token, status, payment_status, payment_provider,
			                    currency, subtotal_minor, shipping_minor, discount_minor,
			                    total_minor, email, phone, name, address, lang, created_at, updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$16)
			RETURNING id`,
			number, accessToken, status, payStatus, provider,
			firstNonEmpty(first.get("currency"), t.app.cfg.Currency),
			subtotal, shipping, discount, total,
			strings.ToLower(first.get("email")), nullString(first.get("phone")),
			nullString(first.get("name")), addr, lang, createdAt).Scan(&orderID); err != nil {
			return err
		}

		for _, l := range lines {
			// Link to a live variant when the SKU still exists, so reporting
			// can join; the snapshot columns stand on their own when it does
			// not, which is the point of snapshotting them.
			var variantID, productID sql.NullInt64
			_ = tx.QueryRowContext(ctx,
				`SELECT id, product_id FROM variants WHERE sku = $1`, l.sku).
				Scan(&variantID, &productID)
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO order_lines (order_id, product_id, variant_id, sku, title,
				                         variant_label, quantity, unit_price_minor, total_minor)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
				orderID, productID, variantID, l.sku, l.title, l.label,
				l.qty, l.unit, l.total); err != nil {
				return err
			}
		}

		if fireEvents {
			o := &Order{ID: orderID, Number: number, Status: status, PaymentStatus: payStatus,
				PaymentProvider: provider, Currency: t.app.cfg.Currency,
				Total: money(total, t.app.cfg.Currency), Email: first.get("email"), Language: lang}
			if err := t.app.outbox.write(ctx, tx, EventOrderCreated, AggregateOrder, orderID,
				t.app.orders.eventPayload(o)); err != nil {
				return err
			}
		}

		result.Created++
		if dryRun {
			return errDryRun
		}
		return nil
	})

	if err != nil && !errors.Is(err, errDryRun) {
		result.Errors = append(result.Errors, RowError{Line: first.line, Message: err.Error()})
	}
}

// ------------------------------------------------------------------ csv row

type csvRow struct {
	line   int
	cols   map[string]int
	values []string
}

func (r csvRow) has(name string) bool {
	i, ok := r.cols[name]
	return ok && i < len(r.values) && strings.TrimSpace(r.values[i]) != ""
}

func (r csvRow) get(name string) string {
	i, ok := r.cols[name]
	if !ok || i >= len(r.values) {
		return ""
	}
	return strings.TrimSpace(r.values[i])
}

func (r csvRow) int64(name string) (int64, error) {
	v := r.get(name)
	if v == "" {
		return 0, fmt.Errorf("%s is required", name)
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be a whole number, got %q", name, v)
	}
	return n, nil
}

func (r csvRow) int64Default(name string, def int64) (int64, error) {
	if !r.has(name) {
		return def, nil
	}
	return r.int64(name)
}

func (r csvRow) intDefault(name string, def int) (int, error) {
	if !r.has(name) {
		return def, nil
	}
	n, err := r.int64(name)
	return int(n), err
}

func (r csvRow) nullInt64(name string) any {
	if !r.has(name) {
		return nil
	}
	n, err := r.int64(name)
	if err != nil {
		return nil
	}
	return n
}

func (r csvRow) boolDefault(name string, def bool) bool {
	if !r.has(name) {
		return def
	}
	switch strings.ToLower(r.get(name)) {
	case "true", "t", "yes", "y", "1":
		return true
	case "false", "f", "no", "n", "0":
		return false
	}
	return def
}

func indexColumns(header []string) map[string]int {
	cols := make(map[string]int, len(header))
	for i, name := range header {
		// A spreadsheet saving as UTF-8 often prepends a byte-order mark, which
		// would otherwise make the first column's name unrecognisable.
		cols[strings.ToLower(strings.TrimSpace(strings.TrimPrefix(name, "\ufeff")))] = i
	}
	return cols
}

// escapeRecord defuses spreadsheet formula injection. A cell beginning with
// =, +, - or @ is executed by Excel and Sheets when the file is opened, so a
// product titled "=cmd|..." would be a live attack on whoever opens the
// export. Prefixing an apostrophe is the standard defence, and import strips
// exactly one back — so the round trip stays lossless.
func escapeRecord(record []string) []string {
	out := make([]string, len(record))
	for i, cell := range record {
		if cell != "" && strings.ContainsRune("=+-@", rune(cell[0])) {
			cell = "'" + cell
		}
		out[i] = cell
	}
	return out
}

func unescapeRecord(record []string) []string {
	out := make([]string, len(record))
	for i, cell := range record {
		out[i] = strings.TrimPrefix(cell, "'")
	}
	return out
}

func nullIntString(v sql.NullInt64) string {
	if !v.Valid {
		return ""
	}
	return strconv.FormatInt(v.Int64, 10)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

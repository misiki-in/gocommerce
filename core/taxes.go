package gocommerce

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Tax: a rate is a rule about a place and a kind of thing, and every line of an
// order is charged by the most specific rule that fits it.
//
// Two things here are worth knowing before reading the rest.
//
// The first is that a rate reaches down a category tree. A rule written against
// "Apparel" applies to "Apparel / Shirts" beneath it, and a rule written against
// "Apparel / Shirts" beats it for shirts. That is what makes a handful of rules
// cover a catalog of fourteen thousand categories.
//
// The second is that what a line was charged is stored on the line, not
// recomputed. An invoice must print the same figures next year as it did today,
// and a rate that has since changed would otherwise quietly rewrite history.
type Taxes struct {
	app *App
}

// Taxes returns the tax service.
func (a *App) Taxes() *Taxes { return a.taxes }

// TaxRate is a rule an operator maintains.
type TaxRate struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	RateBP int    `json:"rate_bp"`
	// Country and State are where it applies. Empty country is the fallback
	// every other rule is more specific than.
	Country string `json:"country,omitempty"`
	State   string `json:"state,omitempty"`
	// CategoryID is what it applies to, reaching the whole subtree beneath it.
	// Nil is everything.
	CategoryID   *int64 `json:"category_id,omitempty"`
	CategoryName string `json:"category_name,omitempty"`

	Active    bool      `json:"active"`
	Metadata  Metadata  `json:"metadata"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// TaxRateInput creates one.
type TaxRateInput struct {
	Name       string   `json:"name"`
	RateBP     int      `json:"rate_bp"`
	Country    string   `json:"country"`
	State      string   `json:"state"`
	CategoryID *int64   `json:"category_id"`
	Active     *bool    `json:"active"`
	Metadata   Metadata `json:"metadata"`
}

// TaxRatePatch updates one.
type TaxRatePatch struct {
	Name       *string    `json:"name"`
	RateBP     *int       `json:"rate_bp"`
	Country    *string    `json:"country"`
	State      *string    `json:"state"`
	CategoryID NullableID `json:"category_id"`
	Active     *bool      `json:"active"`
	Metadata   *Metadata  `json:"metadata"`
}

// LineTax is what one line was charged.
type LineTax struct {
	Name        string `json:"name,omitempty"`
	RateBP      int    `json:"rate_bp"`
	AmountMinor int64  `json:"amount_minor"`
}

const taxRateColumns = `t.id, t.name, t.rate_bp, t.country, t.state, t.category_id,
	t.active, t.metadata, t.created_at, t.updated_at`

func scanTaxRate(row interface{ Scan(...any) error }) (*TaxRate, error) {
	var t TaxRate
	var meta []byte
	if err := row.Scan(&t.ID, &t.Name, &t.RateBP, &t.Country, &t.State, &t.CategoryID,
		&t.Active, &meta, &t.CreatedAt, &t.UpdatedAt); err != nil {
		return nil, err
	}
	if err := scanMetadata(meta, &t.Metadata); err != nil {
		return nil, err
	}
	return &t, nil
}

// ---------------------------------------------------------------------- CRUD

// Create adds a rate.
func (s *Taxes) Create(ctx context.Context, in TaxRateInput) (*TaxRate, error) {
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		return nil, Validationf("a rate needs a name; it is what appears on the invoice")
	}
	if in.RateBP < 0 || in.RateBP > 10000 {
		return nil, Validationf("a rate is between 0 and 10000 basis points (100 is 1%%)")
	}
	if in.State != "" && in.Country == "" {
		return nil, Validationf("a state needs the country it is in")
	}
	meta, err := in.Metadata.value()
	if err != nil {
		return nil, Validationf("metadata is not valid JSON: %v", err)
	}
	active := true
	if in.Active != nil {
		active = *in.Active
	}

	t, err := scanTaxRate(s.app.db.QueryRowContext(ctx, `
		INSERT INTO tax_rates (name, rate_bp, country, state, category_id, active, metadata)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		RETURNING `+strings.ReplaceAll(taxRateColumns, "t.", ""),
		in.Name, in.RateBP, strings.ToUpper(strings.TrimSpace(in.Country)),
		strings.ToUpper(strings.TrimSpace(in.State)), in.CategoryID, active, meta))
	if err != nil {
		return nil, translateTaxErr(err)
	}
	return t, nil
}

// Get loads one.
func (s *Taxes) Get(ctx context.Context, id int64) (*TaxRate, error) {
	t, err := scanTaxRate(s.app.db.QueryRowContext(ctx,
		`SELECT `+strings.ReplaceAll(taxRateColumns, "t.", "")+` FROM tax_rates WHERE id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, NotFoundf("tax rate not found")
	}
	return t, err
}

// List returns every rate, most specific first, so the order they are read in
// is the order they are applied in.
func (s *Taxes) List(ctx context.Context) ([]*TaxRate, error) {
	rows, err := s.app.db.QueryContext(ctx, `
		SELECT `+taxRateColumns+`, coalesce(c.title, '')
		FROM tax_rates t
		LEFT JOIN categories c ON c.id = t.category_id
		ORDER BY (t.category_id IS NOT NULL) DESC, t.country DESC, t.state DESC, t.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []*TaxRate{}
	for rows.Next() {
		var t TaxRate
		var meta []byte
		if err := rows.Scan(&t.ID, &t.Name, &t.RateBP, &t.Country, &t.State, &t.CategoryID,
			&t.Active, &meta, &t.CreatedAt, &t.UpdatedAt, &t.CategoryName); err != nil {
			return nil, err
		}
		if err := scanMetadata(meta, &t.Metadata); err != nil {
			return nil, err
		}
		out = append(out, &t)
	}
	return out, rows.Err()
}

// Update changes a rate. It does not touch orders already placed: what they were
// charged is on their lines.
func (s *Taxes) Update(ctx context.Context, id int64, patch TaxRatePatch) (*TaxRate, error) {
	set, args := []string{}, []any{id}
	add := func(col string, v any) {
		args = append(args, v)
		set = append(set, fmt.Sprintf("%s = $%d", col, len(args)))
	}
	if patch.Name != nil {
		name := strings.TrimSpace(*patch.Name)
		if name == "" {
			return nil, Validationf("a rate needs a name")
		}
		add("name", name)
	}
	if patch.RateBP != nil {
		if *patch.RateBP < 0 || *patch.RateBP > 10000 {
			return nil, Validationf("a rate is between 0 and 10000 basis points")
		}
		add("rate_bp", *patch.RateBP)
	}
	if patch.Country != nil {
		add("country", strings.ToUpper(strings.TrimSpace(*patch.Country)))
	}
	if patch.State != nil {
		add("state", strings.ToUpper(strings.TrimSpace(*patch.State)))
	}
	if patch.CategoryID.Present {
		add("category_id", patch.CategoryID.Value)
	}
	if patch.Active != nil {
		add("active", *patch.Active)
	}
	if patch.Metadata != nil {
		meta, err := patch.Metadata.value()
		if err != nil {
			return nil, Validationf("metadata is not valid JSON: %v", err)
		}
		add("metadata", meta)
	}
	if len(set) == 0 {
		return nil, Validationf("nothing to change")
	}

	t, err := scanTaxRate(s.app.db.QueryRowContext(ctx,
		`UPDATE tax_rates SET `+strings.Join(set, ", ")+`, updated_at = now()
		 WHERE id = $1 RETURNING `+strings.ReplaceAll(taxRateColumns, "t.", ""), args...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, NotFoundf("tax rate not found")
	}
	if err != nil {
		return nil, translateTaxErr(err)
	}
	return t, nil
}

// Delete removes a rate. Orders keep what they were charged.
func (s *Taxes) Delete(ctx context.Context, id int64) error {
	res, err := s.app.db.ExecContext(ctx, `DELETE FROM tax_rates WHERE id = $1`, id)
	if err != nil {
		return translateTaxErr(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return NotFoundf("tax rate not found")
	}
	return nil
}

// ------------------------------------------------------------------ resolving

// resolvedRate is the rule that won for one product.
type resolvedRate struct {
	Name   string
	RateBP int
}

// ratesForProducts finds, for each product, the most specific active rate that
// fits the destination.
//
// Specificity is scored rather than branched: a category match is worth more
// than a country, a country more than a state alone, and within category
// matches the shallower ancestor loses to the deeper one. Doing it in one query
// keeps a checkout at one round trip no matter how many lines it has, and keeps
// the precedence in one readable place instead of spread across Go conditionals.
func (s *Taxes) ratesForProducts(ctx context.Context, tx *sql.Tx, country, state string, productIDs []int64) (map[int64]resolvedRate, error) {
	out := map[int64]resolvedRate{}
	if len(productIDs) == 0 {
		return out, nil
	}
	country = strings.ToUpper(strings.TrimSpace(country))
	state = strings.ToUpper(strings.TrimSpace(state))

	rows, err := tx.QueryContext(ctx, `
		WITH RECURSIVE up AS (
		    -- Every product's own category, then every ancestor above it. depth
		    -- 0 is the product's own, which is the most specific.
		    SELECT p.id AS product_id, c.id AS category_id, c.parent_id, 0 AS depth
		    FROM products p JOIN categories c ON c.id = p.category_id
		    WHERE p.id = ANY($1::bigint[])
		  UNION ALL
		    SELECT up.product_id, c.id, c.parent_id, up.depth + 1
		    FROM up JOIN categories c ON c.id = up.parent_id
		    WHERE up.depth < $4
		),
		candidates AS (
		    SELECT up.product_id, t.id, t.name, t.rate_bp, up.depth,
		           4 + (CASE WHEN t.country <> '' THEN 2 ELSE 0 END)
		             + (CASE WHEN t.state   <> '' THEN 1 ELSE 0 END) AS score
		    FROM up
		    JOIN tax_rates t ON t.category_id = up.category_id
		    WHERE t.active
		      AND (t.country = '' OR t.country = $2)
		      AND (t.state   = '' OR t.state   = $3)
		  UNION ALL
		    -- A rule with no category applies to every product in the basket.
		    SELECT p.id, t.id, t.name, t.rate_bp, 1000000 AS depth,
		           (CASE WHEN t.country <> '' THEN 2 ELSE 0 END)
		         + (CASE WHEN t.state   <> '' THEN 1 ELSE 0 END) AS score
		    FROM products p
		    CROSS JOIN tax_rates t
		    WHERE p.id = ANY($1::bigint[]) AND t.category_id IS NULL AND t.active
		      AND (t.country = '' OR t.country = $2)
		      AND (t.state   = '' OR t.state   = $3)
		)
		SELECT DISTINCT ON (product_id) product_id, name, rate_bp
		FROM candidates
		ORDER BY product_id, score DESC, depth ASC, id`,
		int64Array(productIDs), country, state, MaxCategoryDepth)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var productID int64
		var r resolvedRate
		if err := rows.Scan(&productID, &r.Name, &r.RateBP); err != nil {
			return nil, err
		}
		out[productID] = r
	}
	return out, rows.Err()
}

// ----------------------------------------------------------------- arithmetic

// taxableLine is what the arithmetic needs to know about one line.
type taxableLine struct {
	ProductID int64
	Total     int64
	Taxable   bool
}

// allocateDiscount splits an order-level discount across the lines it came off,
// in proportion to what each line contributed.
//
// The remainders are handed out largest-first so the parts sum to the whole
// exactly. Any other rounding leaves a minor unit unaccounted for, and a tax
// figure computed from a base that does not add up is a figure an accountant
// will find.
//
// This is not stored (D25). It exists so tax is charged on what the customer
// actually paid for each line, which is what every jurisdiction asks for, and
// the durable result of it — the tax itself — is what goes on the line.
func allocateDiscount(lines []taxableLine, discount int64) []int64 {
	out := make([]int64, len(lines))
	if discount <= 0 {
		return out
	}
	var gross int64
	for _, l := range lines {
		gross += l.Total
	}
	if gross <= 0 {
		return out
	}
	if discount > gross {
		discount = gross
	}

	type rem struct {
		i   int
		rem int64
	}
	var assigned int64
	rems := make([]rem, 0, len(lines))
	for i, l := range lines {
		exact := l.Total * discount
		share := exact / gross
		out[i] = share
		assigned += share
		rems = append(rems, rem{i: i, rem: exact % gross})
	}
	// Largest remainder first; ties by position, so the result is stable.
	for left := discount - assigned; left > 0; left-- {
		best := -1
		for _, r := range rems {
			if out[r.i] >= lines[r.i].Total {
				continue // this line is already fully discounted
			}
			if best == -1 || r.rem > rems[best].rem {
				for k := range rems {
					if rems[k].i == r.i {
						best = k
						break
					}
				}
			}
		}
		if best == -1 {
			break
		}
		out[rems[best].i]++
		rems[best].rem = -1
	}
	return out
}

// taxOn computes the tax for one line's base.
//
// Exclusive: the base has no tax in it and the tax is added.
// Inclusive: the base already contains it, so the tax is what is left after
// dividing the base back out — which is why it is not simply base × rate.
func taxOn(base int64, rateBP int, inclusive bool) int64 {
	if base <= 0 || rateBP <= 0 {
		return 0
	}
	if inclusive {
		net := (base*10000 + int64(10000+rateBP)/2) / int64(10000+rateBP)
		return base - net
	}
	return (base*int64(rateBP) + 5000) / 10000
}

// computeTax charges every line and returns the per-line tax and the total.
//
// Tax is charged on what was paid, so the order-level discount is allocated
// across the lines first. Shipping is not taxed: it is one flat number from
// configuration today, and taxing a figure that is not yet a real shipping rate
// would be inventing a liability.
func (s *Taxes) computeTax(ctx context.Context, tx *sql.Tx, country, state string,
	lines []taxableLine, discount int64, inclusive bool) ([]LineTax, int64, error) {

	result := make([]LineTax, len(lines))
	ids := make([]int64, 0, len(lines))
	for _, l := range lines {
		if l.Taxable {
			ids = append(ids, l.ProductID)
		}
	}
	if len(ids) == 0 {
		return result, 0, nil
	}

	rates, err := s.ratesForProducts(ctx, tx, country, state, ids)
	if err != nil {
		return nil, 0, err
	}
	if len(rates) == 0 {
		return result, 0, nil
	}

	allocated := allocateDiscount(lines, discount)
	var total int64
	for i, l := range lines {
		if !l.Taxable {
			continue
		}
		r, ok := rates[l.ProductID]
		if !ok || r.RateBP == 0 {
			continue
		}
		base := l.Total - allocated[i]
		amount := taxOn(base, r.RateBP, inclusive)
		result[i] = LineTax{Name: r.Name, RateBP: r.RateBP, AmountMinor: amount}
		total += amount
	}
	return result, total, nil
}

func translateTaxErr(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "tax_rates_unique_target"):
		return Conflictf("a rate already covers that country, state and category")
	case strings.Contains(msg, "tax_rates_category_id_fkey"):
		return Conflictf("that category is used by a tax rate")
	}
	return err
}

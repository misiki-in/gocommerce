package gocommerce

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Discounts are the one place a store takes money off a basket.
//
// A discount is a rule; what an order got is a snapshot of applying it. The two
// are separate tables and separate types here for the same reason an order line
// keeps its own price: a promotion that ended may be deleted, and every order it
// touched must still say what it was given.
//
// The arithmetic is deliberately dull. One rounding for the whole basket rather
// than one per line, integers throughout, and a discount that can never exceed
// what there is to discount — the interesting part of a promotion is the rule,
// and an operator reconciling a day's takings should never have to think about
// how it was computed.
type Discounts struct {
	app *App
}

// Discounts returns the discount service.
func (a *App) Discounts() *Discounts { return a.discounts }

// Discount kinds.
const (
	DiscountPercentage   = "percentage"
	DiscountFixed        = "fixed"
	DiscountFreeShipping = "free_shipping"
)

// Discount scopes. Only DiscountScopeOrder is evaluated today; the others are
// stored and refused at checkout rather than silently applying to everything.
const (
	DiscountScopeOrder       = "order"
	DiscountScopeProducts    = "products"
	DiscountScopeCollections = "collections"
	DiscountScopeCategories  = "categories"
)

// Discount is a rule an operator maintains.
type Discount struct {
	ID int64 `json:"id"`
	// Code is what a shopper types. Empty means automatic — reserved, and not
	// evaluated at checkout yet.
	Code  string `json:"code,omitempty"`
	Title string `json:"title"`
	Kind  string `json:"kind"`
	// ValueBP is basis points for a percentage: 1000 is 10.00%.
	ValueBP int `json:"value_bp,omitempty"`
	// ValueMinor is the amount for a fixed discount, in minor units.
	ValueMinor int64 `json:"value_minor,omitempty"`

	Scope string `json:"scope"`
	// MinSubtotalMinor is the basket a discount needs before it applies. Nil is
	// no minimum, which is not the same as zero.
	MinSubtotalMinor *int64 `json:"min_subtotal_minor,omitempty"`

	StartsAt *time.Time `json:"starts_at,omitempty"`
	EndsAt   *time.Time `json:"ends_at,omitempty"`

	UsageLimit   *int `json:"usage_limit,omitempty"`
	UsedCount    int  `json:"used_count"`
	OncePerEmail bool `json:"once_per_email"`

	Active    bool      `json:"active"`
	Metadata  Metadata  `json:"metadata"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// AppliedDiscount is what a basket actually got: the snapshot, before it is
// written to an order.
type AppliedDiscount struct {
	DiscountID  int64  `json:"discount_id"`
	Code        string `json:"code,omitempty"`
	Title       string `json:"title"`
	Kind        string `json:"kind"`
	AmountMinor int64  `json:"amount_minor"`
	// FreeShipping is set instead of an amount, because shipping is not part of
	// the subtotal a discount comes off.
	FreeShipping bool `json:"free_shipping,omitempty"`
}

// DiscountInput creates one.
type DiscountInput struct {
	Code             string     `json:"code"`
	Title            string     `json:"title"`
	Kind             string     `json:"kind"`
	ValueBP          int        `json:"value_bp"`
	ValueMinor       int64      `json:"value_minor"`
	Scope            string     `json:"scope"`
	MinSubtotalMinor *int64     `json:"min_subtotal_minor"`
	StartsAt         *time.Time `json:"starts_at"`
	EndsAt           *time.Time `json:"ends_at"`
	UsageLimit       *int       `json:"usage_limit"`
	OncePerEmail     bool       `json:"once_per_email"`
	Active           *bool      `json:"active"`
	Metadata         Metadata   `json:"metadata"`
}

// DiscountPatch updates one. Only the keys present are written.
//
// `used_count` is absent on purpose: it is a fact about what happened, not a
// setting, and letting an operator type over it would let a limited promotion be
// silently reopened.
type DiscountPatch struct {
	Code             *string       `json:"code"`
	Title            *string       `json:"title"`
	Kind             *string       `json:"kind"`
	ValueBP          *int          `json:"value_bp"`
	ValueMinor       *int64        `json:"value_minor"`
	Scope            *string       `json:"scope"`
	MinSubtotalMinor NullableInt64 `json:"min_subtotal_minor"`
	StartsAt         *time.Time    `json:"starts_at"`
	EndsAt           *time.Time    `json:"ends_at"`
	UsageLimit       *int          `json:"usage_limit"`
	OncePerEmail     *bool         `json:"once_per_email"`
	Active           *bool         `json:"active"`
	Metadata         *Metadata     `json:"metadata"`
}

// DiscountQuery filters a listing.
type DiscountQuery struct {
	Search string
	Active *bool
	Limit  int
	Offset int
}

const discountColumns = `id, coalesce(code, ''), title, kind,
	coalesce(value_bp, 0), coalesce(value_minor, 0), scope, min_subtotal_minor,
	starts_at, ends_at, usage_limit, used_count, once_per_email, active,
	metadata, created_at, updated_at`

func scanDiscount(row interface{ Scan(...any) error }) (*Discount, error) {
	var d Discount
	var meta []byte
	if err := row.Scan(&d.ID, &d.Code, &d.Title, &d.Kind, &d.ValueBP, &d.ValueMinor,
		&d.Scope, &d.MinSubtotalMinor, &d.StartsAt, &d.EndsAt, &d.UsageLimit,
		&d.UsedCount, &d.OncePerEmail, &d.Active, &meta, &d.CreatedAt, &d.UpdatedAt); err != nil {
		return nil, err
	}
	if err := scanMetadata(meta, &d.Metadata); err != nil {
		return nil, err
	}
	return &d, nil
}

// ------------------------------------------------------------------ validation

// validate checks a rule against itself: the value has to match the kind, and a
// window has to be a window. The database enforces all of this too — these
// messages exist so an operator is told what is wrong rather than shown a
// constraint name.
func validateDiscount(kind, scope string, valueBP int, valueMinor int64) error {
	switch kind {
	case DiscountPercentage:
		if valueBP <= 0 || valueBP > 10000 {
			return Validationf("a percentage discount is between 1 and 10000 basis points (100 is 1%%)")
		}
		if valueMinor != 0 {
			return Validationf("a percentage discount has no fixed amount")
		}
	case DiscountFixed:
		if valueMinor <= 0 {
			return Validationf("a fixed discount needs an amount above zero")
		}
		if valueBP != 0 {
			return Validationf("a fixed discount has no percentage")
		}
	case DiscountFreeShipping:
		if valueBP != 0 || valueMinor != 0 {
			return Validationf("free shipping carries no value of its own")
		}
	default:
		return Validationf("kind must be percentage, fixed or free_shipping")
	}

	switch scope {
	case "", DiscountScopeOrder, DiscountScopeProducts,
		DiscountScopeCollections, DiscountScopeCategories:
	default:
		return Validationf("scope must be order, products, collections or categories")
	}
	return nil
}

// ---------------------------------------------------------------------- CRUD

// Create adds a discount.
func (s *Discounts) Create(ctx context.Context, in DiscountInput) (*Discount, error) {
	in.Title = strings.TrimSpace(in.Title)
	if in.Title == "" {
		return nil, Validationf("title is required")
	}
	if err := validateDiscount(in.Kind, in.Scope, in.ValueBP, in.ValueMinor); err != nil {
		return nil, err
	}
	if in.Scope == "" {
		in.Scope = DiscountScopeOrder
	}
	if in.EndsAt != nil && in.StartsAt != nil && !in.EndsAt.After(*in.StartsAt) {
		return nil, Validationf("a discount cannot end before it starts")
	}
	if in.UsageLimit != nil && *in.UsageLimit <= 0 {
		return nil, Validationf("a usage limit is a count above zero; leave it out for no limit")
	}
	meta, err := in.Metadata.value()
	if err != nil {
		return nil, Validationf("metadata is not valid JSON: %v", err)
	}
	active := true
	if in.Active != nil {
		active = *in.Active
	}

	d, err := scanDiscount(s.app.db.QueryRowContext(ctx, `
		INSERT INTO discounts (code, title, kind, value_bp, value_minor, scope,
		                       min_subtotal_minor, starts_at, ends_at, usage_limit,
		                       once_per_email, active, metadata)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		RETURNING `+discountColumns,
		nullString(strings.TrimSpace(in.Code)), in.Title, in.Kind,
		nullInt(in.ValueBP), nullInt64(in.ValueMinor), in.Scope,
		in.MinSubtotalMinor, in.StartsAt, in.EndsAt, in.UsageLimit,
		in.OncePerEmail, active, meta))
	if err != nil {
		return nil, translateDiscountErr(err)
	}
	return d, nil
}

// Get loads one by id.
func (s *Discounts) Get(ctx context.Context, id int64) (*Discount, error) {
	d, err := scanDiscount(s.app.db.QueryRowContext(ctx,
		`SELECT `+discountColumns+` FROM discounts WHERE id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, NotFoundf("discount not found")
	}
	return d, err
}

// GetByCode looks one up the way a shopper does: case-insensitively.
//
// The folding is PostgreSQL's, matching the unique index, and only
// PostgreSQL's. See the migration for why there is exactly one implementation.
func (s *Discounts) GetByCode(ctx context.Context, code string) (*Discount, error) {
	code = strings.TrimSpace(code)
	if code == "" {
		return nil, NotFoundf("discount not found")
	}
	d, err := scanDiscount(s.app.db.QueryRowContext(ctx,
		`SELECT `+discountColumns+` FROM discounts WHERE lower(code) = lower($1)`, code))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, NotFoundf("no discount with that code")
	}
	return d, err
}

// List returns a page, newest first.
func (s *Discounts) List(ctx context.Context, q DiscountQuery) ([]*Discount, int, error) {
	where, args := []string{"true"}, []any{}
	if term := strings.TrimSpace(q.Search); term != "" {
		args = append(args, "%"+term+"%")
		where = append(where, fmt.Sprintf(
			"(title ILIKE $%d OR coalesce(code, '') ILIKE $%d)", len(args), len(args)))
	}
	if q.Active != nil {
		args = append(args, *q.Active)
		where = append(where, fmt.Sprintf("active = $%d", len(args)))
	}
	clause := strings.Join(where, " AND ")

	var total int
	if err := s.app.db.QueryRowContext(ctx,
		`SELECT count(*) FROM discounts WHERE `+clause, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	limit, offset := q.Limit, q.Offset
	if limit <= 0 {
		limit = 50
	}
	args = append(args, limit, offset)
	rows, err := s.app.db.QueryContext(ctx,
		`SELECT `+discountColumns+` FROM discounts WHERE `+clause+
			fmt.Sprintf(" ORDER BY id DESC LIMIT $%d OFFSET $%d", len(args)-1, len(args)), args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	out := []*Discount{}
	for rows.Next() {
		d, err := scanDiscount(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, d)
	}
	return out, total, rows.Err()
}

// Update changes a rule.
func (s *Discounts) Update(ctx context.Context, id int64, patch DiscountPatch) (*Discount, error) {
	current, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}

	kind, scope := current.Kind, current.Scope
	valueBP, valueMinor := current.ValueBP, current.ValueMinor
	if patch.Kind != nil {
		kind = *patch.Kind
	}
	if patch.Scope != nil {
		scope = *patch.Scope
	}
	if patch.ValueBP != nil {
		valueBP = *patch.ValueBP
	}
	if patch.ValueMinor != nil {
		valueMinor = *patch.ValueMinor
	}
	// Changing the kind without the value is the mistake this catches: a
	// percentage rule given a fixed kind would otherwise take one basis point
	// off the basket.
	if err := validateDiscount(kind, scope, valueBP, valueMinor); err != nil {
		return nil, err
	}

	set, args := []string{}, []any{id}
	add := func(col string, v any) {
		args = append(args, v)
		set = append(set, fmt.Sprintf("%s = $%d", col, len(args)))
	}
	if patch.Code != nil {
		add("code", nullString(strings.TrimSpace(*patch.Code)))
	}
	if patch.Title != nil {
		title := strings.TrimSpace(*patch.Title)
		if title == "" {
			return nil, Validationf("title is required")
		}
		add("title", title)
	}
	if patch.Kind != nil || patch.ValueBP != nil || patch.ValueMinor != nil {
		add("kind", kind)
		add("value_bp", nullInt(valueBP))
		add("value_minor", nullInt64(valueMinor))
	}
	if patch.Scope != nil {
		add("scope", scope)
	}
	if patch.MinSubtotalMinor.Present {
		add("min_subtotal_minor", patch.MinSubtotalMinor.Value)
	}
	if patch.StartsAt != nil {
		add("starts_at", *patch.StartsAt)
	}
	if patch.EndsAt != nil {
		add("ends_at", *patch.EndsAt)
	}
	if patch.UsageLimit != nil {
		if *patch.UsageLimit <= 0 {
			return nil, Validationf("a usage limit is a count above zero")
		}
		add("usage_limit", *patch.UsageLimit)
	}
	if patch.OncePerEmail != nil {
		add("once_per_email", *patch.OncePerEmail)
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

	d, err := scanDiscount(s.app.db.QueryRowContext(ctx,
		`UPDATE discounts SET `+strings.Join(set, ", ")+`, updated_at = now()
		 WHERE id = $1 RETURNING `+discountColumns, args...))
	if err != nil {
		return nil, translateDiscountErr(err)
	}
	return d, nil
}

// Delete removes a discount. Orders that used it keep their snapshot.
func (s *Discounts) Delete(ctx context.Context, id int64) error {
	res, err := s.app.db.ExecContext(ctx, `DELETE FROM discounts WHERE id = $1`, id)
	if err != nil {
		return translateDiscountErr(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return NotFoundf("discount not found")
	}
	return nil
}

// ------------------------------------------------------------------ applying

// discountRequest is what a basket knows about itself when it asks.
type discountRequest struct {
	Code     string
	Email    string
	Subtotal int64
}

// Preview reports what a basket would get, without consuming anything.
//
// The storefront calls this to show a figure before anybody commits. It is
// deliberately not the same code path as checkout — this one takes no locks and
// counts no usage — but it answers with the same arithmetic, so the number a
// shopper is shown is the number they are charged.
func (s *Discounts) Preview(ctx context.Context, code, email string, subtotal int64) (*AppliedDiscount, error) {
	d, err := s.GetByCode(ctx, code)
	if err != nil {
		return nil, err
	}
	if err := s.eligible(ctx, nil, d, discountRequest{Email: email, Subtotal: subtotal}); err != nil {
		return nil, err
	}
	return applyDiscount(d, subtotal), nil
}

// eligible reports whether a discount may be used for this basket, or says why
// not. tx may be nil for a preview, which skips the once-per-email lookup's
// need for the checkout's own view of the world.
func (s *Discounts) eligible(ctx context.Context, tx *sql.Tx, d *Discount, req discountRequest) error {
	if !d.Active {
		return Validationf("that discount is not active")
	}
	now := time.Now()
	if d.StartsAt != nil && now.Before(*d.StartsAt) {
		return Validationf("that discount has not started yet")
	}
	if d.EndsAt != nil && !now.Before(*d.EndsAt) {
		return Validationf("that discount has expired")
	}
	if d.UsageLimit != nil && d.UsedCount >= *d.UsageLimit {
		return Validationf("that discount has been fully used")
	}
	if d.MinSubtotalMinor != nil && req.Subtotal < *d.MinSubtotalMinor {
		return Validationf("that discount needs a basket of at least %d", *d.MinSubtotalMinor)
	}
	// Only order-wide rules are evaluated. A scoped one is stored and refused
	// rather than quietly applied to the whole basket, which is the failure
	// that would cost a store money without anybody noticing.
	if d.Scope != DiscountScopeOrder {
		return Validationf("scoped discounts are not applied yet")
	}
	if d.OncePerEmail && strings.TrimSpace(req.Email) != "" {
		used, err := s.emailHasUsed(ctx, tx, d.ID, req.Email)
		if err != nil {
			return err
		}
		if used {
			return Validationf("that discount has already been used with this email address")
		}
	}
	return nil
}

// emailHasUsed reports whether this address already has an order carrying this
// discount. It is a deterrent, not a control — a second address defeats it —
// and D26 says so out loud rather than implying otherwise.
func (s *Discounts) emailHasUsed(ctx context.Context, tx *sql.Tx, discountID int64, email string) (bool, error) {
	const q = `
		SELECT EXISTS (
		    SELECT 1 FROM order_discounts od
		    JOIN orders o ON o.id = od.order_id
		    WHERE od.discount_id = $1 AND lower(o.email) = lower($2)
		      AND o.status <> 'cancelled'
		)`
	var used bool
	var err error
	if tx != nil {
		err = tx.QueryRowContext(ctx, q, discountID, email).Scan(&used)
	} else {
		err = s.app.db.QueryRowContext(ctx, q, discountID, email).Scan(&used)
	}
	return used, err
}

// applyDiscount is the arithmetic, and all of it.
//
// One rounding for the whole basket rather than one per line: per-line rounding
// drifts by a minor unit per line and leaves a total nobody can reconcile
// against the lines above it. Half up, because a shopper who is told "10% off"
// should not lose a paisa to banker's rounding.
//
// The result can never exceed the subtotal. A fixed discount larger than the
// basket takes the basket — the alternative is a negative total, which the
// order's own CHECK would refuse anyway, at a point far from the cause.
func applyDiscount(d *Discount, subtotal int64) *AppliedDiscount {
	out := &AppliedDiscount{
		DiscountID: d.ID, Code: d.Code, Title: d.Title, Kind: d.Kind,
	}
	switch d.Kind {
	case DiscountPercentage:
		out.AmountMinor = (subtotal*int64(d.ValueBP) + 5000) / 10000
	case DiscountFixed:
		out.AmountMinor = d.ValueMinor
	case DiscountFreeShipping:
		out.FreeShipping = true
	}
	if out.AmountMinor > subtotal {
		out.AmountMinor = subtotal
	}
	if out.AmountMinor < 0 {
		out.AmountMinor = 0
	}
	return out
}

// applyTx is the checkout path: eligibility, arithmetic, and the usage claim,
// all inside the transaction that is creating the order.
//
// The claim is a conditional UPDATE rather than a read followed by a write. Two
// checkouts racing for the last use of a code is the one contention that
// matters here, and it is the database's to resolve — zero rows affected means
// somebody else took it.
func (s *Discounts) applyTx(ctx context.Context, tx *sql.Tx, req discountRequest) (*AppliedDiscount, error) {
	code := strings.TrimSpace(req.Code)
	if code == "" {
		return nil, nil
	}

	d, err := scanDiscount(tx.QueryRowContext(ctx,
		`SELECT `+discountColumns+` FROM discounts WHERE lower(code) = lower($1) FOR UPDATE`, code))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, Validationf("no discount with that code")
	}
	if err != nil {
		return nil, err
	}
	if err := s.eligible(ctx, tx, d, req); err != nil {
		return nil, err
	}

	res, err := tx.ExecContext(ctx, `
		UPDATE discounts SET used_count = used_count + 1, updated_at = now()
		WHERE id = $1 AND (usage_limit IS NULL OR used_count < usage_limit)`, d.ID)
	if err != nil {
		return nil, err
	}
	if n, err := res.RowsAffected(); err != nil {
		return nil, err
	} else if n == 0 {
		return nil, Validationf("that discount has been fully used")
	}

	return applyDiscount(d, req.Subtotal), nil
}

// recordOrderDiscount writes the snapshot beside the order that got it.
func recordOrderDiscount(ctx context.Context, tx *sql.Tx, orderID int64, a *AppliedDiscount) error {
	if a == nil {
		return nil
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO order_discounts (order_id, discount_id, code, title, kind, amount_minor)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		orderID, nullInt64(a.DiscountID), a.Code, a.Title, a.Kind, a.AmountMinor)
	return err
}

// loadOrderDiscounts attaches the snapshots to a page of orders in one query.
func (s *Orders) loadOrderDiscounts(ctx context.Context, byID map[int64]*Order, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	rows, err := s.app.db.QueryContext(ctx, `
		SELECT order_id, coalesce(discount_id, 0), code, title, kind, amount_minor
		FROM order_discounts WHERE order_id = ANY($1::bigint[]) ORDER BY id`, int64Array(ids))
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var orderID int64
		var a AppliedDiscount
		if err := rows.Scan(&orderID, &a.DiscountID, &a.Code, &a.Title, &a.Kind, &a.AmountMinor); err != nil {
			return err
		}
		if o := byID[orderID]; o != nil {
			o.Discounts = append(o.Discounts, a)
		}
	}
	return rows.Err()
}

func translateDiscountErr(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "discounts_code_key"):
		return Conflictf("that code is already in use")
	case strings.Contains(msg, "discounts_value_matches_kind"):
		return Validationf("the value does not match the kind of discount")
	case strings.Contains(msg, "discounts_window"):
		return Validationf("a discount cannot end before it starts")
	}
	return err
}

// nullInt and nullInt64 render a zero as SQL NULL, for the columns where zero
// is not a value the CHECK constraints permit.
func nullInt(v int) any {
	if v == 0 {
		return nil
	}
	return v
}

func nullInt64(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

// recomputeOrderDiscount is D24: what happens to a discount when the basket it
// came off changes.
//
// A fixed amount survives — it was never a function of the basket. A percentage
// is taken again, because the basket it was a percentage of no longer exists
// and keeping the old figure would leave an order whose arithmetic does not add
// up. An order that has fallen below the minimum its discount required is
// refused: it no longer qualifies for the promotion, and choosing between
// removing the discount and refusing the edit is an operator's call, not a
// silent one.
//
// The stored snapshot moves with it, so the order and its discount rows never
// disagree.
func recomputeOrderDiscount(ctx context.Context, tx *sql.Tx, orderID, subtotal, current int64) (int64, error) {
	var (
		rowID    int64
		kind     string
		discount sql.NullInt64
	)
	err := tx.QueryRowContext(ctx, `
		SELECT id, kind, discount_id FROM order_discounts
		WHERE order_id = $1 ORDER BY id LIMIT 1`, orderID).Scan(&rowID, &kind, &discount)
	if errors.Is(err, sql.ErrNoRows) {
		// No snapshot: either the order predates discounts or it never had one.
		// Whatever is on the order stays, clamped to what there is to discount.
		if current > subtotal {
			return subtotal, nil
		}
		return current, nil
	}
	if err != nil {
		return 0, err
	}

	amount := current
	if kind == DiscountPercentage {
		if !discount.Valid {
			// The rule was deleted. Its percentage is unknowable, so the amount
			// stands as recorded rather than being guessed at.
			if current > subtotal {
				amount = subtotal
			}
		} else {
			var valueBP int
			var minSubtotal sql.NullInt64
			if err := tx.QueryRowContext(ctx,
				`SELECT coalesce(value_bp, 0), min_subtotal_minor FROM discounts WHERE id = $1`,
				discount.Int64).Scan(&valueBP, &minSubtotal); err != nil {
				return 0, err
			}
			if minSubtotal.Valid && subtotal < minSubtotal.Int64 {
				return 0, Conflictf(
					"this order would fall below the %d its discount needs; remove the discount first",
					minSubtotal.Int64)
			}
			amount = (subtotal*int64(valueBP) + 5000) / 10000
		}
	}
	if amount > subtotal {
		amount = subtotal
	}
	if amount != current {
		if _, err := tx.ExecContext(ctx,
			`UPDATE order_discounts SET amount_minor = $2 WHERE id = $1`, rowID, amount); err != nil {
			return 0, err
		}
	}
	return amount, nil
}

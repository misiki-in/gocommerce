package gocommerce

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Locations are the places stock physically is: a shop, a warehouse, a shelf in
// somebody's garage. Every store has at least one and most have exactly one,
// which is the case this service is shaped around — a single-location store
// should never have to think about locations, and the default one exists so it
// does not have to.
//
// What a location is *for* is answering two questions the single number could
// not: can I ship this today from somewhere near the buyer, and which box does
// this order come out of. Both are answered at reservation time, once, and
// recorded on the order line — so a cancellation puts the units back where they
// came from rather than wherever the default happens to be that week.
type Locations struct {
	app *App
}

// Places returns the locations service.
func (a *App) Places() *Locations { return a.locations }

// Location is one place stock can be.
type Location struct {
	ID   int64  `json:"id"`
	Code string `json:"code"`
	Name string `json:"name"`
	// Address is where it is, when that matters — a pickup point a shopper is
	// sent to, or the origin on a customs form. A stockroom nobody visits can
	// leave it empty.
	Address *Address `json:"address,omitempty"`
	// Priority orders the search for stock to reserve: lower is preferred. Two
	// locations at the same priority are tried oldest first, which is arbitrary
	// but stable, and stability is what makes a reservation reproducible.
	Priority int `json:"priority"`
	// Active is whether new orders may be filled from here. Stock is not
	// allowed to sit at an inactive location — see Update — so deactivating is
	// a statement that the place is empty, not a way to hide what is in it.
	Active    bool     `json:"active"`
	IsDefault bool     `json:"is_default"`
	Metadata  Metadata `json:"metadata"`
	// OnHand and Reserved are what this location is holding across every
	// variant. Summed on the way out rather than stored, for the same reason
	// the variant totals are.
	OnHand    int       `json:"on_hand"`
	Reserved  int       `json:"reserved"`
	SKUs      int       `json:"skus"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// LocationInput creates a location.
type LocationInput struct {
	Code     string   `json:"code"`
	Name     string   `json:"name"`
	Address  *Address `json:"address"`
	Priority *int     `json:"priority"`
	Active   *bool    `json:"active"`
	Metadata Metadata `json:"metadata"`
}

// LocationPatch changes one. Every field is optional; an omitted field is left
// alone, which is what lets the panel send only what the operator touched.
type LocationPatch struct {
	Name     *string  `json:"name"`
	Address  *Address `json:"address"`
	Priority *int     `json:"priority"`
	Active   *bool    `json:"active"`
	Metadata Metadata `json:"metadata"`
}

// VariantStock is one variant's holding at one location.
type VariantStock struct {
	VariantID    int64  `json:"variant_id"`
	LocationID   int64  `json:"location_id"`
	LocationCode string `json:"location_code"`
	LocationName string `json:"location_name"`
	Active       bool   `json:"active"`
	OnHand       int    `json:"on_hand"`
	Reserved     int    `json:"reserved"`
	Available    int    `json:"available"`
}

const locationColumns = `l.id, l.code, l.name, l.address, l.priority, l.active,
	l.is_default, l.metadata, l.created_at, l.updated_at,
	coalesce((SELECT sum(vs.on_hand)  FROM variant_stock vs WHERE vs.location_id = l.id), 0),
	coalesce((SELECT sum(vs.reserved) FROM variant_stock vs WHERE vs.location_id = l.id), 0),
	(SELECT count(*) FROM variant_stock vs WHERE vs.location_id = l.id AND vs.on_hand <> 0)`

// List returns every location in the order stock is drawn from them.
func (s *Locations) List(ctx context.Context) ([]*Location, error) {
	rows, err := s.app.db.QueryContext(ctx,
		`SELECT `+locationColumns+` FROM locations l ORDER BY l.priority, l.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Location{}
	for rows.Next() {
		l, err := scanLocation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// Get returns one location.
func (s *Locations) Get(ctx context.Context, id int64) (*Location, error) {
	row := s.app.db.QueryRowContext(ctx,
		`SELECT `+locationColumns+` FROM locations l WHERE l.id = $1`, id)
	l, err := scanLocation(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, NotFoundf("location %d does not exist", id)
	}
	return l, err
}

type scanner interface{ Scan(dest ...any) error }

func scanLocation(row scanner) (*Location, error) {
	l := &Location{}
	var addr, meta []byte
	if err := row.Scan(&l.ID, &l.Code, &l.Name, &addr, &l.Priority, &l.Active,
		&l.IsDefault, &meta, &l.CreatedAt, &l.UpdatedAt,
		&l.OnHand, &l.Reserved, &l.SKUs); err != nil {
		return nil, err
	}
	if err := scanMetadata(meta, &l.Metadata); err != nil {
		return nil, err
	}
	// An empty object and a null both mean "no address"; neither should turn
	// into a struct full of empty strings that a client has to test field by
	// field.
	var a Address
	if len(addr) > 0 {
		if err := json.Unmarshal(addr, &a); err != nil {
			return nil, err
		}
		if a != (Address{}) {
			l.Address = &a
		}
	}
	return l, nil
}

// Create adds a location. It is not made default — that is SetDefault's job,
// and doing it here would silently redirect every reservation in the store as a
// side effect of adding a shelf.
func (s *Locations) Create(ctx context.Context, in LocationInput) (*Location, error) {
	code := strings.ToLower(strings.TrimSpace(in.Code))
	name := strings.TrimSpace(in.Name)
	if code == "" {
		return nil, Validationf("code is required")
	}
	if name == "" {
		return nil, Validationf("name is required")
	}
	meta, err := in.Metadata.value()
	if err != nil {
		return nil, Validationf("location metadata is not valid JSON: %v", err)
	}
	addr, err := marshalAddress(in.Address)
	if err != nil {
		return nil, err
	}
	priority := 0
	if in.Priority != nil {
		priority = *in.Priority
	}

	var id int64
	err = s.app.db.QueryRowContext(ctx, `
		INSERT INTO locations (code, name, address, priority, active, metadata)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
		code, name, addr, priority, boolOr(in.Active, true), meta).Scan(&id)
	if err != nil {
		return nil, translateLocationErr(err, code)
	}
	return s.Get(ctx, id)
}

// Update changes a location.
//
// Deactivating one is the only interesting case. An inactive location that
// still holds stock would report units as available that nothing can reserve —
// the totals count them, the picker skips them — so the stock has to go
// somewhere first. Refusing here is what keeps `available` meaning what it says.
func (s *Locations) Update(ctx context.Context, id int64, patch LocationPatch) (*Location, error) {
	sets := []string{"updated_at = now()"}
	args := []any{id}
	add := func(col string, v any) {
		args = append(args, v)
		sets = append(sets, fmt.Sprintf("%s = $%d", col, len(args)))
	}
	if patch.Name != nil {
		if strings.TrimSpace(*patch.Name) == "" {
			return nil, Validationf("name must not be empty")
		}
		add("name", strings.TrimSpace(*patch.Name))
	}
	if patch.Address != nil {
		addr, err := marshalAddress(patch.Address)
		if err != nil {
			return nil, err
		}
		add("address", addr)
	}
	if patch.Priority != nil {
		add("priority", *patch.Priority)
	}
	if patch.Metadata != nil {
		meta, err := patch.Metadata.value()
		if err != nil {
			return nil, Validationf("location metadata is not valid JSON: %v", err)
		}
		add("metadata", meta)
	}
	if patch.Active != nil {
		if !*patch.Active {
			if err := s.refuseIfHolding(ctx, id, "deactivated"); err != nil {
				return nil, err
			}
		}
		add("active", *patch.Active)
	}
	if len(sets) == 1 {
		return s.Get(ctx, id)
	}
	res, err := s.app.db.ExecContext(ctx,
		`UPDATE locations SET `+strings.Join(sets, ", ")+` WHERE id = $1`, args...)
	if err != nil {
		return nil, translateLocationErr(err, "")
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, NotFoundf("location %d does not exist", id)
	}
	return s.Get(ctx, id)
}

// SetDefault moves the default, which is where stock lands when nobody says
// otherwise. Two statements in one transaction, because the unique index allows
// exactly one default row and the old one has to stand down before the new one
// can stand up.
func (s *Locations) SetDefault(ctx context.Context, id int64) (*Location, error) {
	err := InTx(ctx, s.app.db, func(tx *sql.Tx) error {
		var active bool
		err := tx.QueryRowContext(ctx,
			`SELECT active FROM locations WHERE id = $1`, id).Scan(&active)
		if errors.Is(err, sql.ErrNoRows) {
			return NotFoundf("location %d does not exist", id)
		}
		if err != nil {
			return err
		}
		if !active {
			return Conflictf("an inactive location cannot be the default")
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE locations SET is_default = false, updated_at = now() WHERE is_default`); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE locations SET is_default = true, updated_at = now() WHERE id = $1`, id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, id)
}

// Delete removes a location. The default cannot go — something has to be the
// answer to "where does this land" — and neither can one that still holds
// stock, which the foreign key would refuse anyway in a sentence about a
// constraint rather than about the shelf.
func (s *Locations) Delete(ctx context.Context, id int64) error {
	var isDefault bool
	err := s.app.db.QueryRowContext(ctx,
		`SELECT is_default FROM locations WHERE id = $1`, id).Scan(&isDefault)
	if errors.Is(err, sql.ErrNoRows) {
		return NotFoundf("location %d does not exist", id)
	}
	if err != nil {
		return err
	}
	if isDefault {
		return Conflictf("this is the default location; make another one default first")
	}
	if err := s.refuseIfHolding(ctx, id, "deleted"); err != nil {
		return err
	}
	// Rows at zero are bookkeeping, not stock, and holding up a deletion for
	// them would make the refusal above unclearable.
	if _, err := s.app.db.ExecContext(ctx,
		`DELETE FROM variant_stock WHERE location_id = $1`, id); err != nil {
		return err
	}
	res, err := s.app.db.ExecContext(ctx, `DELETE FROM locations WHERE id = $1`, id)
	if err != nil {
		return translateLocationErr(err, "")
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return NotFoundf("location %d does not exist", id)
	}
	return nil
}

// refuseIfHolding blocks a change that would strand stock, and names the amount
// so the operator knows how much to move rather than having to go and count.
func (s *Locations) refuseIfHolding(ctx context.Context, id int64, verb string) error {
	var units, skus int
	if err := s.app.db.QueryRowContext(ctx, `
		SELECT coalesce(sum(on_hand + reserved), 0), count(*) FILTER (WHERE on_hand <> 0 OR reserved <> 0)
		FROM variant_stock WHERE location_id = $1`, id).Scan(&units, &skus); err != nil {
		return err
	}
	if units != 0 || skus != 0 {
		return Conflictf(
			"this location still holds %d unit(s) across %d SKU(s); move them before it is %s",
			units, skus, verb)
	}
	return nil
}

func marshalAddress(a *Address) ([]byte, error) {
	if a == nil {
		return []byte(`{}`), nil
	}
	b, err := json.Marshal(a)
	if err != nil {
		return nil, Validationf("address is not valid: %v", err)
	}
	return b, nil
}

func translateLocationErr(err error, code string) error {
	if err == nil {
		return nil
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "locations_code_key"):
		return Conflictf("a location with the code %q already exists", code)
	case strings.Contains(msg, "locations_code_check"):
		return Validationf("code must not be empty")
	case strings.Contains(msg, "locations_name_check"):
		return Validationf("name must not be empty")
	case strings.Contains(msg, "variant_stock_location_id_fkey"):
		return Conflictf("this location still holds stock; move it before the location goes")
	}
	return err
}

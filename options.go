package gocommerce

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

// Editing the option matrix.
//
// The existing AddOption appends one axis and nothing can change it afterwards,
// which is fine for an API and useless for an editor: a person renaming "Size"
// or dropping "XL" is doing one thing, and doing it as four calls leaves the
// product briefly incoherent between them.
//
// SetOptions takes the whole matrix and reconciles the variants to match, in
// one transaction. That is the shape the admin UI actually needs, and it is
// also the honest one — the option set and the variants that depend on it are a
// single fact, so they should change together or not at all.

// OptionSpec is one axis in a desired matrix.
//
// ID is what makes a rename possible. Matched by name alone, renaming "Size"
// to "Größe" is indistinguishable from deleting one axis and adding another —
// and the engine would dutifully strip every variant of its size and collapse
// them all onto the same empty combination. Sending the id says "this is the
// same axis, under a new name"; omitting it says "this one is new".
type OptionSpec struct {
	ID     *int64   `json:"id"`
	Name   string   `json:"name"`
	Values []string `json:"values"`
}

// OptionSet is the desired option matrix for a product.
type OptionSet struct {
	Options []OptionSpec `json:"options"`
	// GenerateVariants creates a variant for every combination that does not
	// have one yet. Off by default: adding an axis to a catalog of 40 products
	// should not quietly mint 200 sellable SKUs at a guessed price.
	GenerateVariants bool `json:"generate_variants"`
	// PriceMinor is the price for generated variants. Zero means "copy the
	// product's existing default", which is nearly always what was meant.
	PriceMinor *int64 `json:"price_minor"`
}

// OptionChange reports what SetOptions did, so an operator can be told rather
// than left to discover it.
type OptionChange struct {
	AxesAdded       []string `json:"axes_added"`
	AxesRemoved     []string `json:"axes_removed"`
	AxesRenamed     []string `json:"axes_renamed"`
	ValuesAdded     []string `json:"values_added"`
	ValuesRemoved   []string `json:"values_removed"`
	VariantsCreated []string `json:"variants_created"`
	VariantsRemoved []string `json:"variants_removed"`
}

// SetOptions replaces a product's option axes and reconciles its variants.
//
// The rules, in the order they matter:
//
//   - A variant whose combination still exists is left completely alone. Its
//     price, SKU and stock survive a rename of the axis above it, because
//     nothing about the thing being sold changed.
//   - A variant whose combination no longer exists is deleted. Order lines keep
//     their own snapshot and merely lose the reference, so history stays
//     readable; cart lines cascade, because a line nobody can buy should not
//     block the operator.
//   - New combinations are created only when asked for.
func (c *Catalog) SetOptions(ctx context.Context, productID int64, in OptionSet) (*Product, *OptionChange, error) {
	for i := range in.Options {
		in.Options[i].Name = strings.TrimSpace(in.Options[i].Name)
		if in.Options[i].Name == "" {
			return nil, nil, Validationf("every option needs a name")
		}
		in.Options[i].Values = normalizeOptionValues(in.Options[i].Values)
		if len(in.Options[i].Values) == 0 {
			return nil, nil, Validationf("option %q has no values", in.Options[i].Name)
		}
	}
	if err := checkAxisNames(in.Options); err != nil {
		return nil, nil, err
	}
	// The engine resolves a variant's options by value, so the same value on
	// two axes is ambiguous by construction — "Small" as both a Size and a Cup
	// cannot be told apart. Refusing is the only honest answer until the
	// resolver keys on (axis, value).
	if err := checkValuesUniqueAcrossAxes(in.Options); err != nil {
		return nil, nil, err
	}

	change := &OptionChange{}
	err := InTx(ctx, c.app.db, func(tx *sql.Tx) error {
		before, err := loadOptionMatrix(ctx, tx, productID)
		if err != nil {
			return err
		}
		if before == nil {
			return NotFoundf("product %d not found", productID)
		}

		// Every existing variant's combination, keyed by the *axis id* it came
		// from. Names change; ids do not, which is what lets a rename keep its
		// variants.
		existing, err := loadVariantCombinations(ctx, tx, productID)
		if err != nil {
			return err
		}

		// wanted maps each surviving axis id to the values it keeps. An axis
		// with no id is new and has no variants pointing at it yet.
		wanted := map[int64][]string{}
		order := []string{}
		for _, o := range in.Options {
			order = append(order, o.Name)
			if o.ID == nil {
				change.AxesAdded = append(change.AxesAdded, o.Name)
				continue
			}
			prev, known := before[*o.ID]
			if !known {
				return Validationf("this product has no option %d", *o.ID)
			}
			wanted[*o.ID] = o.Values
			if !strings.EqualFold(prev.Name, o.Name) {
				change.AxesRenamed = append(change.AxesRenamed, prev.Name+" → "+o.Name)
			}
			added, removed := diffValues(prev.Values, o.Values)
			for _, v := range added {
				change.ValuesAdded = append(change.ValuesAdded, o.Name+": "+v)
			}
			for _, v := range removed {
				change.ValuesRemoved = append(change.ValuesRemoved, o.Name+": "+v)
			}
		}
		for id, prev := range before {
			if _, keep := wanted[id]; !keep {
				change.AxesRemoved = append(change.AxesRemoved, prev.Name)
			}
		}

		// Order matters here, and the database enforces it:
		// variant_option_values references option values with ON DELETE
		// RESTRICT, so the axes cannot be dropped while any variant still
		// points at them. Retire the doomed variants, release the survivors'
		// references, and only then rebuild.

		// 1. Decide each existing variant's fate by whether its combination
		//    still exists in the new matrix.
		//
		//    Dropping an axis can also collapse two survivors onto each other:
		//    take Colour away from Size x Colour and both S/Red and S/Blue
		//    become plain S. They cannot both be kept — a combination is unique
		//    per product, in the unique index as much as in the shop — so the
		//    first in the catalog's own order keeps its price, SKU and stock
		//    and the rest are removed, reported like any other variant this
		//    edit takes with it. Merging beats refusing: otherwise an axis
		//    could never be deleted from a product that has variants, which is
		//    every product an axis is worth deleting from.
		var keep []variantCombination
		claimed := map[string]bool{}
		for _, v := range existing {
			key := survivingKey(v, wanted)
			if combinationSurvives(v, wanted) && !claimed[key] {
				claimed[key] = true
				keep = append(keep, v)
				continue
			}
			change.VariantsRemoved = append(change.VariantsRemoved, v.SKU)
			if _, err := tx.ExecContext(ctx, `DELETE FROM variants WHERE id = $1`, v.ID); err != nil {
				return Internalf(err, "remove variant %s", v.SKU)
			}
		}

		// 2. Release the survivors' references so the old values are free.
		for _, v := range keep {
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM variant_option_values WHERE variant_id = $1`, v.ID); err != nil {
				return Internalf(err, "clear variant options")
			}
		}

		// 3. Now the axes can go, and the new matrix take their place.
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM product_options WHERE product_id = $1`, productID); err != nil {
			return Internalf(err, "clear options")
		}
		valueIDs, err := insertOptionMatrix(ctx, tx, productID, in.Options)
		if err != nil {
			return err
		}

		// 4. Re-point the survivors at the newly inserted value rows. Their
		//    price, SKU and stock were never touched — only the ids beneath
		//    them moved.
		for _, v := range keep {
			var ids []int64
			for axisID, value := range v.ByAxis {
				if _, still := wanted[axisID]; !still {
					continue
				}
				// The axis kept its identity but its rows are new, so the
				// value is looked up under whatever the axis is called now.
				ids = append(ids, valueIDs[valueKey(nameOf(in.Options, axisID), value)])
			}
			for _, id := range ids {
				if _, err := tx.ExecContext(ctx,
					`INSERT INTO variant_option_values (variant_id, option_value_id) VALUES ($1, $2)`,
					v.ID, id); err != nil {
					return Internalf(err, "re-link variant options")
				}
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE variants SET option_key = $2, updated_at = now() WHERE id = $1`,
				v.ID, optionKeyFor(ids)); err != nil {
				return Internalf(err, "update option key")
			}
		}

		if in.GenerateVariants {
			created, err := generateMissingVariants(ctx, tx, productID, in, order, valueIDs)
			if err != nil {
				return err
			}
			change.VariantsCreated = created
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}

	p, err := c.GetProduct(ctx, productID)
	if err != nil {
		return nil, nil, err
	}
	return p, change, nil
}

// ------------------------------------------------------------------ helpers

type variantCombination struct {
	ID    int64
	SKU   string
	Price int64
	// Keyed by option (axis) id, because names are what a rename changes.
	ByAxis map[int64]string
}

// axisState is an axis as it exists now.
type axisState struct {
	Name   string
	Values []string
}

// nameOf returns the new name of an axis, for building the value lookup after
// the matrix has been reinserted.
func nameOf(specs []OptionSpec, axisID int64) string {
	for _, s := range specs {
		if s.ID != nil && *s.ID == axisID {
			return s.Name
		}
	}
	return ""
}

func normalizeOptionValues(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if k := strings.ToLower(v); !seen[k] {
			seen[k] = true
			out = append(out, v)
		}
	}
	return out
}

func checkAxisNames(opts []OptionSpec) error {
	seen := map[string]bool{}
	for _, o := range opts {
		k := strings.ToLower(o.Name)
		if seen[k] {
			return Conflictf("two options are both called %q", o.Name)
		}
		seen[k] = true
	}
	return nil
}

// checkValuesUniqueAcrossAxes is the guard for a real hole in the resolver:
// variant options are matched by value alone, so "Small" on two axes resolves
// ambiguously and silently. Until that is keyed on (axis, value), the only
// correct behaviour is to refuse the input rather than accept it and be wrong.
func checkValuesUniqueAcrossAxes(opts []OptionSpec) error {
	owner := map[string]string{}
	for _, o := range opts {
		for _, v := range o.Values {
			k := strings.ToLower(v)
			if prev, clash := owner[k]; clash && prev != o.Name {
				return Conflictf(
					"%q is a value on both %q and %q; a variant's options are matched by value, so the two could not be told apart",
					v, prev, o.Name)
			}
			owner[k] = o.Name
		}
	}
	return nil
}

func diffValues(before, after []string) (added, removed []string) {
	had := map[string]bool{}
	for _, v := range before {
		had[strings.ToLower(v)] = true
	}
	want := map[string]bool{}
	for _, v := range after {
		want[strings.ToLower(v)] = true
		if !had[strings.ToLower(v)] {
			added = append(added, v)
		}
	}
	for _, v := range before {
		if !want[strings.ToLower(v)] {
			removed = append(removed, v)
		}
	}
	return added, removed
}

func combinationSurvives(v variantCombination, wanted map[int64][]string) bool {
	for axisID, value := range v.ByAxis {
		values, still := wanted[axisID]
		if !still {
			// The axis is gone. The variant survives on its remaining axes;
			// whether that leaves it identical to another survivor is
			// survivingKey's question, not this one's.
			continue
		}
		found := false
		for _, candidate := range values {
			if strings.EqualFold(candidate, value) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// survivingKey is the combination a variant will hold once the axes that are
// going have gone: its value on each axis that stays, keyed by axis id.
//
// Case-insensitive, because the rebuilt matrix is. normalizeOptionValues folds
// "Red" and "red" into one value row, so two variants holding them separately
// end up pointing at the same row and the same option_key — which is exactly
// what this key has to predict.
func survivingKey(v variantCombination, wanted map[int64][]string) string {
	axes := make([]int64, 0, len(v.ByAxis))
	for axisID := range v.ByAxis {
		if _, still := wanted[axisID]; still {
			axes = append(axes, axisID)
		}
	}
	sort.Slice(axes, func(i, j int) bool { return axes[i] < axes[j] })
	parts := make([]string, len(axes))
	for i, axisID := range axes {
		parts[i] = fmt.Sprintf("%d=%s", axisID, strings.ToLower(v.ByAxis[axisID]))
	}
	return strings.Join(parts, ",")
}

func loadOptionMatrix(ctx context.Context, tx *sql.Tx, productID int64) (map[int64]*axisState, error) {
	var exists bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM products WHERE id = $1)`, productID).Scan(&exists); err != nil {
		return nil, Internalf(err, "check product")
	}
	if !exists {
		return nil, nil
	}

	rows, err := tx.QueryContext(ctx, `
		SELECT o.id, o.name, v.value
		FROM product_options o
		LEFT JOIN product_option_values v ON v.option_id = o.id
		WHERE o.product_id = $1
		ORDER BY o.position, o.id, v.position, v.id`, productID)
	if err != nil {
		return nil, Internalf(err, "read options")
	}
	defer rows.Close()

	out := map[int64]*axisState{}
	for rows.Next() {
		var id int64
		var name string
		var value sql.NullString
		if err := rows.Scan(&id, &name, &value); err != nil {
			return nil, Internalf(err, "scan option")
		}
		if _, ok := out[id]; !ok {
			out[id] = &axisState{Name: name}
		}
		if value.Valid {
			out[id].Values = append(out[id].Values, value.String)
		}
	}
	return out, rows.Err()
}

func loadVariantCombinations(ctx context.Context, tx *sql.Tx, productID int64) ([]variantCombination, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT v.id, v.sku, v.price_minor, o.id, ov.value
		FROM variants v
		LEFT JOIN variant_option_values vov ON vov.variant_id = v.id
		LEFT JOIN product_option_values ov ON ov.id = vov.option_value_id
		LEFT JOIN product_options o ON o.id = ov.option_id
		WHERE v.product_id = $1
		ORDER BY v.position, v.id`, productID)
	if err != nil {
		return nil, Internalf(err, "read variants")
	}
	defer rows.Close()

	byID := map[int64]*variantCombination{}
	var order []int64
	for rows.Next() {
		var (
			id     int64
			sku    string
			price  int64
			axisID sql.NullInt64
			value  sql.NullString
		)
		if err := rows.Scan(&id, &sku, &price, &axisID, &value); err != nil {
			return nil, Internalf(err, "scan variant")
		}
		v, ok := byID[id]
		if !ok {
			v = &variantCombination{ID: id, SKU: sku, Price: price, ByAxis: map[int64]string{}}
			byID[id] = v
			order = append(order, id)
		}
		if axisID.Valid && value.Valid {
			v.ByAxis[axisID.Int64] = value.String
		}
	}
	out := make([]variantCombination, 0, len(order))
	for _, id := range order {
		out = append(out, *byID[id])
	}
	return out, rows.Err()
}

// insertOptionMatrix writes the axes and returns a lookup from
// "axis\x00value" to the new option_value id.
func insertOptionMatrix(ctx context.Context, tx *sql.Tx, productID int64, opts []OptionSpec) (map[string]int64, error) {
	ids := map[string]int64{}
	for i, o := range opts {
		var optionID int64
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO product_options (product_id, name, position)
			VALUES ($1, $2, $3) RETURNING id`, productID, o.Name, i).Scan(&optionID); err != nil {
			if isUniqueViolation(err) {
				return nil, Conflictf("two options are both called %q", o.Name)
			}
			return nil, Internalf(err, "create option %s", o.Name)
		}
		for j, v := range o.Values {
			var valueID int64
			if err := tx.QueryRowContext(ctx, `
				INSERT INTO product_option_values (option_id, value, position)
				VALUES ($1, $2, $3) RETURNING id`, optionID, v, j).Scan(&valueID); err != nil {
				return nil, Internalf(err, "create option value %s", v)
			}
			ids[valueKey(o.Name, v)] = valueID
		}
	}
	return ids, nil
}

// valueKey is how the value-id lookup is keyed: axis name and value, folded.
//
// Folded because the survivors are re-linked by the value they already hold,
// and the operator may have just retyped it in a different case. Matching that
// exactly would miss, hand back a zero id, and break the foreign key — the
// same edit the rest of this file treats as a no-op.
func valueKey(axisName, value string) string {
	return strings.ToLower(axisName) + "\x00" + strings.ToLower(value)
}

// generateMissingVariants mints the combinations that have no variant yet.
func generateMissingVariants(
	ctx context.Context, tx *sql.Tx, productID int64, in OptionSet,
	order []string, valueIDs map[string]int64,
) ([]string, error) {
	// Keyed by the axis's *current* name, which is what valueIDs is keyed by
	// too — ids matter for identity, names for lookup after the rebuild.
	byName := map[string][]string{}
	for _, o := range in.Options {
		byName[o.Name] = o.Values
	}
	price := int64(0)
	if in.PriceMinor != nil {
		price = *in.PriceMinor
	} else {
		// Copy whatever the product already sells for; a generated variant at
		// zero is a variant that can be bought for nothing.
		_ = tx.QueryRowContext(ctx,
			`SELECT price_minor FROM variants WHERE product_id = $1 ORDER BY position, id LIMIT 1`,
			productID).Scan(&price)
	}

	var baseSKU string
	if err := tx.QueryRowContext(ctx,
		`SELECT slug FROM products WHERE id = $1`, productID).Scan(&baseSKU); err != nil {
		return nil, Internalf(err, "read product slug")
	}

	var created []string
	for _, combo := range cartesian(order, byName) {
		var ids []int64
		var parts []string
		for _, axis := range order {
			ids = append(ids, valueIDs[valueKey(axis, combo[axis])])
			parts = append(parts, combo[axis])
		}
		key := optionKeyFor(ids)

		var taken bool
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM variants WHERE product_id = $1 AND option_key = $2)`,
			productID, key).Scan(&taken); err != nil {
			return nil, Internalf(err, "check combination")
		}
		if taken {
			continue
		}

		sku := strings.ToUpper(baseSKU + "-" + strings.Join(parts, "-"))
		sku = strings.ReplaceAll(sku, " ", "-")
		var variantID int64
		err := tx.QueryRowContext(ctx, `
			INSERT INTO variants (product_id, sku, price_minor, option_key, position)
			VALUES ($1, $2, $3, $4, (SELECT coalesce(max(position), -1) + 1 FROM variants WHERE product_id = $1))
			RETURNING id`, productID, sku, price, key).Scan(&variantID)
		if err != nil {
			if isUniqueViolation(err) {
				// A SKU collision across the catalog is the operator's to
				// resolve; skipping silently would leave a hole in the matrix
				// they never asked about.
				return nil, Conflictf("cannot generate variant %q: that sku is already used", sku)
			}
			return nil, Internalf(err, "create variant %s", sku)
		}
		for _, id := range ids {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO variant_option_values (variant_id, option_value_id) VALUES ($1, $2)`,
				variantID, id); err != nil {
				return nil, Internalf(err, "link generated variant")
			}
		}
		created = append(created, sku)
	}
	return created, nil
}

// cartesian expands the axes into every combination, in axis order.
func cartesian(order []string, values map[string][]string) []map[string]string {
	out := []map[string]string{{}}
	for _, axis := range order {
		var next []map[string]string
		for _, base := range out {
			for _, v := range values[axis] {
				combo := make(map[string]string, len(base)+1)
				for k, existing := range base {
					combo[k] = existing
				}
				combo[axis] = v
				next = append(next, combo)
			}
		}
		out = next
	}
	return out
}

// optionKeyFor builds the sorted, joined key the unique index enforces
// combination uniqueness on.
func optionKeyFor(ids []int64) string {
	sorted := append([]int64(nil), ids...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	parts := make([]string, len(sorted))
	for i, id := range sorted {
		parts[i] = fmt.Sprint(id)
	}
	return strings.Join(parts, ",")
}

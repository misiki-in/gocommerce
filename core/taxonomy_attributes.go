package gocommerce

import (
	"bufio"
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"io"
	"strings"
)

// The fields Shopify's taxonomy asks of a product, per category.
//
// Built from the same release as shopify-categories.txt, out of the two other
// files Shopify publishes with it. Those two are 95MB between them and nearly
// all of it is repetition: every category carries the full text of every
// attribute it uses, and every attribute the full text of its values. Here each
// is written once, which is the same information in a twentieth of the space.
//
// Like the tree, it ships embedded rather than downloaded — an import that
// reaches the network fails in an air-gapped deployment and drifts between two
// installs of the same version — and nothing imports it automatically.

//go:embed taxonomy/shopify-category-attributes.txt
var shopifyCategoryAttributes string

// ShopifyCategoryAttributes returns the embedded attribute file, in two
// sections: the attribute dictionary, then one line per category.
func ShopifyCategoryAttributes() string { return shopifyCategoryAttributes }

// CategoryAttribute is one field a category asks of a product.
//
// Key and Label are the category's own, out of its metadata. Choices comes from
// the shared table and is filled in on the way out — see attachAttributes. No
// choices is a free-text field rather than a broken one: a store may declare a
// field nobody has published a value list for.
type CategoryAttribute struct {
	Key     string   `json:"key"`
	Label   string   `json:"label"`
	Choices []string `json:"choices"`
}

// TaxonomyAttributeImport reports what an attribute import did.
type TaxonomyAttributeImport struct {
	// Attributes counts rows written to the shared dictionary. Categories
	// counts categories matched by taxonomy id and given their field list.
	// Unmatched counts lines naming a category this store does not have, which
	// is the ordinary case when the tree came from a different release or only
	// part of it was kept.
	Attributes int `json:"attributes"`
	Categories int `json:"categories"`
	Unmatched  int `json:"unmatched"`
	Skipped    int `json:"skipped"`
}

type attributeDef struct {
	Handle  string   `json:"handle"`
	Label   string   `json:"label"`
	Choices []string `json:"choices"`
}

// parseCategoryAttributes reads the two-section format:
//
//	handle = Label : value | value | ...
//	gid://shopify/TaxonomyCategory/x : handle , handle , ...
//
// Which kind a line is comes from which separator it has, not from which
// section it appears in, so a stray blank line cannot silently reinterpret the
// rest of the file. A malformed line is skipped and counted rather than fatal,
// for the reason the category parser gives: the file is data, and one bad row
// should not cost the operator the other fourteen thousand.
func parseCategoryAttributes(r io.Reader) ([]attributeDef, map[string][]string, int, error) {
	var (
		defs    []attributeDef
		seen    = map[string]int{}
		byGID   = map[string][]string{}
		skipped int
	)
	scanner := bufio.NewScanner(r)
	// One attribute's values run to a few kilobytes. A megabyte is generous,
	// and set explicitly because the failure it prevents — a truncated line —
	// would otherwise be silent.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		if handle, rest, ok := strings.Cut(line, " = "); ok {
			label, values, _ := strings.Cut(rest, " : ")
			handle, label = strings.TrimSpace(handle), strings.TrimSpace(label)
			if handle == "" || label == "" {
				skipped++
				continue
			}
			def := attributeDef{Handle: handle, Label: label, Choices: []string{}}
			for _, v := range strings.Split(values, "|") {
				if v = strings.TrimSpace(v); v != "" {
					def.Choices = append(def.Choices, v)
				}
			}
			// Later wins, and the earlier row is replaced rather than appended.
			// The upsert below cannot touch one row twice in a statement, so a
			// file with a repeated handle would fail the whole import.
			if at, dup := seen[handle]; dup {
				defs[at] = def
			} else {
				seen[handle] = len(defs)
				defs = append(defs, def)
			}
			continue
		}

		gid, rest, ok := strings.Cut(line, " : ")
		if !ok {
			skipped++
			continue
		}
		handles := []string{}
		for _, h := range strings.Split(rest, ",") {
			if h = strings.TrimSpace(h); h != "" {
				handles = append(handles, h)
			}
		}
		if gid = strings.TrimSpace(gid); gid == "" || len(handles) == 0 {
			skipped++
			continue
		}
		byGID[gid] = handles
	}
	if err := scanner.Err(); err != nil {
		return nil, nil, skipped, Internalf(err, "read the attribute file")
	}
	return defs, byGID, skipped, nil
}

// ImportCategoryAttributes loads the field definitions and attaches them to
// categories already in the tree.
//
// It matches on `metadata.taxonomy_gid`, which ImportTaxonomy writes on every
// leaf it creates, so this runs after that one and touches only categories that
// came from the same source. A category somebody typed by hand has no taxonomy
// id and is left alone, metadata included — that is the operator's own.
//
// One transaction, like the tree import and for the same reason: half a set of
// definitions is worse than none, because nothing in the result would say which
// half is missing.
func (s *Categories) ImportCategoryAttributes(ctx context.Context, r io.Reader) (TaxonomyAttributeImport, error) {
	defs, byGID, skipped, err := parseCategoryAttributes(r)
	if err != nil {
		return TaxonomyAttributeImport{}, err
	}
	result := TaxonomyAttributeImport{Skipped: skipped}
	if len(defs) == 0 {
		return result, Validationf("that file contains no attribute definitions")
	}

	// The label written onto a category comes from the dictionary, so the two
	// can never disagree about what a field is called.
	label := make(map[string]string, len(defs))
	for _, d := range defs {
		label[d.Handle] = d.Label
	}

	encodedDefs, err := json.Marshal(defs)
	if err != nil {
		return TaxonomyAttributeImport{}, Internalf(err, "encode the attribute definitions")
	}

	// What a category stores is only which fields it asks for, not what may be
	// answered: choices belong to the attribute. Its own type rather than
	// CategoryAttribute with an empty list, so the stored JSON has no `choices`
	// key at all and nothing later mistakes a null for "no choices".
	type storedField struct {
		Key   string `json:"key"`
		Label string `json:"label"`
	}
	type categoryFields struct {
		GID    string        `json:"gid"`
		Fields []storedField `json:"fields"`
	}
	batch := make([]categoryFields, 0, len(byGID))
	for gid, handles := range byGID {
		fields := make([]storedField, 0, len(handles))
		for _, h := range handles {
			name, ok := label[h]
			if !ok {
				// A category naming a field the dictionary does not define.
				// There is nothing to show, so it is left out rather than
				// rendered as a field with a blank label.
				result.Skipped++
				continue
			}
			// Writing the choices here instead would put 29MB of repeated text
			// in the table and 400KB of it in every page of a listing.
			fields = append(fields, storedField{Key: h, Label: name})
		}
		if len(fields) > 0 {
			batch = append(batch, categoryFields{GID: gid, Fields: fields})
		}
	}
	encodedBatch, err := json.Marshal(batch)
	if err != nil {
		return TaxonomyAttributeImport{}, Internalf(err, "encode the field lists")
	}

	err = InTx(ctx, s.app.db, func(tx *sql.Tx) error {
		// JSON in, arrays out — the same round trip tags take, so there is no
		// hand-written PostgreSQL array quoting anywhere to get wrong.
		//
		// Upsert rather than replace: a store may have written attribute rows
		// of its own, and re-importing the published set should not take them
		// away.
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO taxonomy_attributes (handle, label, choices)
			SELECT d->>'handle', d->>'label',
			       ARRAY(SELECT jsonb_array_elements_text(d->'choices'))
			FROM jsonb_array_elements($1::jsonb) AS d
			ON CONFLICT (handle) DO UPDATE
			SET label = excluded.label,
			    choices = excluded.choices,
			    updated_at = now()`, encodedDefs); err != nil {
			return Internalf(err, "write the attribute definitions")
		}
		result.Attributes = len(defs)

		var touched int
		if err := tx.QueryRowContext(ctx, `
			WITH incoming AS (
			    SELECT d->>'gid' AS gid, d->'fields' AS fields
			    FROM jsonb_array_elements($1::jsonb) AS d
			), updated AS (
			    UPDATE categories c
			    SET metadata = jsonb_set(c.metadata, '{attributes}', i.fields, true)
			    FROM incoming i
			    WHERE c.metadata->>'taxonomy_gid' = i.gid
			    RETURNING 1
			)
			SELECT count(*) FROM updated`, encodedBatch).Scan(&touched); err != nil {
			return Internalf(err, "attach the field lists")
		}
		result.Categories = touched
		result.Unmatched = len(batch) - touched
		return nil
	})
	if err != nil {
		return TaxonomyAttributeImport{}, err
	}
	return result, nil
}

// attachAttributes fills in each category's Attributes: the fields its own
// metadata declares, with the choices the shared table holds for them.
//
// The metadata itself is left exactly as it was read. A client that shows a
// category and writes it back should not find that merely reading it had grown
// the row by every value list it mentions.
func attachAttributes(ctx context.Context, db *sql.DB, cats []*Category) error {
	type decl struct {
		Key   string `json:"key"`
		Label string `json:"label"`
	}
	declared := make([][]decl, len(cats))
	seen := map[string]bool{}
	handles := []string{}

	for i, c := range cats {
		raw, ok := c.Metadata["attributes"]
		if !ok {
			continue
		}
		encoded, err := json.Marshal(raw)
		if err != nil {
			continue
		}
		var list []decl
		if err := json.Unmarshal(encoded, &list); err != nil {
			// Metadata is the operator's own and may hold anything under this
			// key. Something that is not a field list is not an error; it is
			// simply not a field list.
			continue
		}
		declared[i] = list
		for _, d := range list {
			if d.Key != "" && !seen[d.Key] {
				seen[d.Key] = true
				handles = append(handles, d.Key)
			}
		}
	}
	if len(handles) == 0 {
		return nil
	}

	encoded, err := json.Marshal(handles)
	if err != nil {
		return Internalf(err, "encode the attribute handles")
	}
	rows, err := db.QueryContext(ctx, `
		SELECT handle, to_jsonb(choices) FROM taxonomy_attributes
		WHERE handle = ANY(ARRAY(SELECT jsonb_array_elements_text($1::jsonb)))`, encoded)
	if err != nil {
		return Internalf(err, "read the attribute choices")
	}
	defer rows.Close()

	choices := map[string][]string{}
	for rows.Next() {
		var handle string
		var raw []byte
		if err := rows.Scan(&handle, &raw); err != nil {
			return Internalf(err, "scan the attribute choices")
		}
		var values []string
		if err := scanTags(raw, &values); err != nil {
			return Internalf(err, "decode the attribute choices")
		}
		choices[handle] = values
	}
	if err := rows.Err(); err != nil {
		return Internalf(err, "read the attribute choices")
	}

	for i, c := range cats {
		for _, d := range declared[i] {
			if d.Key == "" {
				continue
			}
			label := d.Label
			if label == "" {
				label = d.Key
			}
			values := choices[d.Key]
			if values == nil {
				values = []string{}
			}
			c.Attributes = append(c.Attributes, CategoryAttribute{
				Key: d.Key, Label: label, Choices: values,
			})
		}
	}
	return nil
}

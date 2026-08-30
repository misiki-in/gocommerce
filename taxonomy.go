package gocommerce

import (
	"bufio"
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"io"
	"strings"
)

// Shopify's Standard Product Taxonomy, as a starting tree.
//
// It ships embedded rather than downloaded, because an import that reaches the
// network is an import that fails in an air-gapped deployment and drifts
// between two installs of the same version. The file is ~2MB of text; the
// binary carries it once.
//
// Nothing imports it automatically. Categories are the operator's own data, and
// dropping 14,000 rows into a store that wanted six of its own is not a default
// anybody would choose — so it is a command they run.

//go:embed taxonomy/shopify-categories.txt
var shopifyTaxonomy string

// ShopifyTaxonomy returns the embedded category list, in the published format:
//
//	gid://shopify/TaxonomyCategory/ap-2-1 : Animals & Pet Supplies > Pet Supplies > Bird Supplies
func ShopifyTaxonomy() string { return shopifyTaxonomy }

// TaxonomyImport reports what an import did.
type TaxonomyImport struct {
	// Created counts rows written. Matched counts categories that already
	// existed under the same parent with the same title and were left alone,
	// which is what makes a second run a no-op rather than a duplicate tree.
	Created int `json:"created"`
	Matched int `json:"matched"`
	Skipped int `json:"skipped"`
}

// pathSep joins a trail of names into the key both sides match on.
//
// Unit separator, not NUL: PostgreSQL text may hold any byte except 0x00, so a
// NUL separator makes the query that builds the same key server-side fail
// outright — which is exactly how this was found. 0x1F cannot appear in a
// category title anybody typed.
const pathSep = "\x1f"

// taxonomyLine is one parsed row: the trail of names from the root down.
type taxonomyLine struct {
	gid  string
	path []string
}

// parseTaxonomy reads the published format, ignoring comments and blanks.
//
// A malformed line is skipped rather than fatal. The file is data, and one bad
// row in fourteen thousand should not cost an operator the other 13,999 — the
// count comes back in the report so a silent loss is still a visible one.
func parseTaxonomy(r io.Reader) ([]taxonomyLine, int, error) {
	var (
		out     []taxonomyLine
		skipped int
	)
	scanner := bufio.NewScanner(r)
	// Some paths are long; the default 64KB token is plenty, but a line-length
	// failure would be silent truncation, so the buffer is set explicitly.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		gid, trail, ok := strings.Cut(line, " : ")
		if !ok {
			skipped++
			continue
		}
		parts := strings.Split(trail, ">")
		path := make([]string, 0, len(parts))
		for _, p := range parts {
			if name := strings.TrimSpace(p); name != "" {
				path = append(path, name)
			}
		}
		if len(path) == 0 {
			skipped++
			continue
		}
		out = append(out, taxonomyLine{gid: strings.TrimSpace(gid), path: path})
	}
	if err := scanner.Err(); err != nil {
		return nil, skipped, Internalf(err, "read the taxonomy")
	}
	return out, skipped, nil
}

// ImportTaxonomy loads a category tree from the published Shopify format.
//
// It is idempotent on (parent, title): a category that already sits in the
// right place is matched and left alone, so running it twice, or running it
// over a tree an operator has already started, adds only what is missing. That
// natural key is used rather than a stored taxonomy id because it needs no
// column of its own and it also matches categories somebody typed by hand.
//
// The whole import is one transaction. Half a taxonomy is worse than none: the
// tree would be missing branches with no way to tell which, and a re-run would
// look like it had nothing to do.
func (s *Categories) ImportTaxonomy(ctx context.Context, r io.Reader) (TaxonomyImport, error) {
	lines, skipped, err := parseTaxonomy(r)
	if err != nil {
		return TaxonomyImport{}, err
	}
	result := TaxonomyImport{Skipped: skipped}
	if len(lines) == 0 {
		return result, Validationf("that file contains no categories")
	}

	err = InTx(ctx, s.app.db, func(tx *sql.Tx) error {
		// byPath maps a full trail to the row id standing at it, so a child
		// resolves its parent without a query. The whole existing tree is read
		// in once for the same reason: 14,000 lookups is 14,000 round trips.
		byPath, err := existingPaths(ctx, tx)
		if err != nil {
			return err
		}

		insert, err := tx.PrepareContext(ctx, `
			INSERT INTO categories (parent_id, slug, title, position, metadata)
			VALUES ($1, $2, $3, $4, $5) RETURNING id`)
		if err != nil {
			return Internalf(err, "prepare the insert")
		}
		defer insert.Close()

		taken, err := existingSlugs(ctx, tx)
		if err != nil {
			return err
		}

		for _, line := range lines {
			if len(line.path) > MaxCategoryDepth {
				// The published file is eight deep and the limit is eight, so
				// this is unreachable today. It stays because the file is data
				// that can change under us, and the alternative is a tree that
				// silently loses its deepest branches.
				result.Skipped++
				continue
			}
			var parentID *int64
			for depth, name := range line.path {
				key := strings.ToLower(strings.Join(line.path[:depth+1], pathSep))
				if id, ok := byPath[key]; ok {
					if depth == len(line.path)-1 {
						result.Matched++
					}
					parentID = &id
					continue
				}

				slug := uniqueSlug(name, line.path[:depth+1], taken)
				meta := []byte(`{}`)
				if depth == len(line.path)-1 {
					meta = []byte(fmt.Sprintf(`{"taxonomy_gid":%q}`, line.gid))
				}
				var id int64
				if err := insert.QueryRowContext(ctx,
					parentID, slug, name, depth, meta).Scan(&id); err != nil {
					return translateCategoryErr(err)
				}
				byPath[key] = id
				parentID = &id
				result.Created++
			}
		}
		return nil
	})
	if err != nil {
		return TaxonomyImport{}, err
	}
	return result, nil
}

// existingPaths reads the whole category tree as trail -> id.
//
// The case folding happens here rather than in SQL, and that is the whole point
// of the comment. PostgreSQL's `lower()` follows the database's collation: on
// this cluster it leaves "É" alone, while Go's strings.ToLower folds it to "é".
// Lower-casing on both sides therefore produced two different keys for the same
// category — and a re-import that had matched fourteen thousand rows created a
// second "Éclairs" beside the first. One implementation of case folding, in one
// language, is the only version of this that stays true.
func existingPaths(ctx context.Context, tx *sql.Tx) (map[string]int64, error) {
	rows, err := tx.QueryContext(ctx, `
		WITH RECURSIVE down AS (
		    SELECT id, parent_id, title::text AS path
		    FROM categories WHERE parent_id IS NULL
		  UNION ALL
		    SELECT c.id, c.parent_id, d.path || chr(31) || c.title
		    FROM categories c JOIN down d ON d.id = c.parent_id
		)
		SELECT path, id FROM down`)
	if err != nil {
		return nil, Internalf(err, "read the existing tree")
	}
	defer rows.Close()

	out := map[string]int64{}
	for rows.Next() {
		var path string
		var id int64
		if err := rows.Scan(&path, &id); err != nil {
			return nil, Internalf(err, "scan the existing tree")
		}
		out[strings.ToLower(path)] = id
	}
	return out, rows.Err()
}

func existingSlugs(ctx context.Context, tx *sql.Tx) (map[string]bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT slug FROM categories`)
	if err != nil {
		return nil, Internalf(err, "read existing slugs")
	}
	defer rows.Close()

	out := map[string]bool{}
	for rows.Next() {
		var slug string
		if err := rows.Scan(&slug); err != nil {
			return nil, Internalf(err, "scan slugs")
		}
		out[slug] = true
	}
	return out, rows.Err()
}

// uniqueSlug picks a free slug for a category, claiming it in taken.
//
// The leaf name first, because "bird-cage-accessories" is what belongs in a
// URL. Names repeat across branches — there is more than one "Accessories" in
// this taxonomy — so a collision falls back to the ancestry, and only then to a
// counter. The slug column is globally unique, and a name is not.
func uniqueSlug(name string, path []string, taken map[string]bool) string {
	candidates := []string{slugify(name)}
	if len(path) > 1 {
		candidates = append(candidates, slugify(strings.Join(path[len(path)-2:], " ")))
		candidates = append(candidates, slugify(strings.Join(path, " ")))
	}
	for _, c := range candidates {
		if c != "" && !taken[c] {
			taken[c] = true
			return c
		}
	}
	base := candidates[0]
	if base == "" {
		base = "category"
	}
	for i := 2; ; i++ {
		c := fmt.Sprintf("%s-%d", base, i)
		if !taken[c] {
			taken[c] = true
			return c
		}
	}
}

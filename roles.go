package gocommerce

import (
	"context"
	"database/sql"
	"slices"
	"sort"
	"strings"
)

// What a role may do, as this store decided it.
//
// rights.go declares the rights and the default set each role carries. This is
// the store's departure from those defaults: a table of grants, one row per
// right, and a resolution step every authenticated request goes through.
//
// The shape is deliberate in three ways.
//
// A role with no rows tracks the defaults rather than freezing a copy of them,
// so a right added to `manager` in a later release reaches every store that
// never touched the matrix. Saving a set that matches the default therefore
// stores nothing at all.
//
// Owner is not storable. It is the recovery path — a store configured into a
// corner still has somebody who can configure it back out — and the way back
// must not depend on configuration being sane.
//
// Nothing is cached. Rights are read on each authentication, which is one small
// query on a table holding at most a dozen rows, and in exchange a change takes
// effect on the affected operator's next request without anybody being signed
// out and without a second server holding a stale copy of who may spend money.
type RoleRights struct {
	app *App
}

// Roles returns the role-rights service.
func (a *App) Roles() *RoleRights { return a.roles }

// roleRightsLockKey is the first half of the two-int advisory lock key that
// serialises writes to one role ("role" in ASCII). The second half is the role
// name, so two roles are edited independently.
const roleRightsLockKey int32 = 0x726F6C65

// RoleSet is one row of the matrix: what a role may do here, what it would do
// untouched, and whether those differ.
type RoleSet struct {
	Role   string  `json:"role"`
	Rights []Right `json:"rights"`
	// Default is what the engine ships for this role. The screen that edits a
	// set needs to show what "reset" would go back to.
	Default []Right `json:"default"`
	// Customized is whether this store has departed from Default — which is not
	// the same as having a row, since a set equal to the default is stored as
	// nothing at all.
	Customized   bool `json:"customized"`
	Configurable bool `json:"configurable"`
}

// RoleMatrix is the whole model in one response: every role, the closed list of
// rights, and the floor. A client renders the grid without keeping its own copy
// of either axis — the same reason a superuser record carries its rights.
type RoleMatrix struct {
	Roles []RoleSet `json:"roles"`
	// AllRights is the catalogue, in the engine's display order.
	AllRights []Right `json:"all_rights"`
	// Required is what no role can be stripped of; the panel renders these
	// checked and locked rather than letting somebody save a set the API will
	// only refuse.
	Required []Right `json:"required"`
}

// Matrix returns the store's effective role/right matrix.
func (r *RoleRights) Matrix(ctx context.Context) (*RoleMatrix, error) {
	stored, err := r.overrides(ctx)
	if err != nil {
		return nil, err
	}
	out := &RoleMatrix{
		AllRights: append([]Right(nil), AllRights...),
		Required:  append([]Right(nil), RequiredRights...),
	}
	for _, role := range Roles {
		def := DefaultRightsOf(role)
		row := RoleSet{
			Role: role, Rights: def, Default: def,
			Configurable: RoleConfigurable(role),
		}
		if set, ok := stored[role]; ok && row.Configurable {
			row.Rights = set
			row.Customized = !sameRights(set, def)
		}
		out.Roles = append(out.Roles, row)
	}
	return out, nil
}

// Of resolves what a role may actually do in this store.
//
// Every path that hands an operator their rights goes through here rather than
// through DefaultRightsOf, which is what makes a change to the matrix take
// effect on the next request.
func (r *RoleRights) Of(ctx context.Context, role string) ([]Right, error) {
	// The one lookup that needs no table at all. Owner is not storable, and
	// resolving it without reading anything means a store whose role_rights is
	// unreadable can still be signed into by the person who can fix it.
	if !RoleConfigurable(role) {
		return DefaultRightsOf(role), nil
	}
	all, err := r.All(ctx)
	if err != nil {
		return nil, err
	}
	return all[role], nil
}

// All resolves every role in one query, for the paths that scan more than one
// operator and would otherwise ask the same question per row.
func (r *RoleRights) All(ctx context.Context) (map[string][]Right, error) {
	stored, err := r.overrides(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string][]Right, len(Roles))
	for _, role := range Roles {
		if set, ok := stored[role]; ok && RoleConfigurable(role) {
			out[role] = set
			continue
		}
		out[role] = DefaultRightsOf(role)
	}
	return out, nil
}

// Set replaces what a role may do.
//
// The guards, in order: owner is fixed; only rights this engine has are
// accepted; the floor is kept; and nobody removes settings.write from their own
// role. The last one is the same rule the panel draws locked — an operator who
// saves their way out of the roles screen has no way back in short of a
// database client, and the mistake is one keystroke away from being made by
// somebody who was only tidying up.
//
// It is *not* an escalation guard: a manager granted settings.write can widen
// their own role, or make themselves an owner outright through the team screen.
// That is what settings.write already means — "the right that can grant rights"
// — and pretending otherwise here would be a check that reads like a boundary
// without being one. Handing it out is the decision; this is not the place to
// second-guess it.
//
// by is the operator making the change, or nil for a static admin token, which
// has no role to lock itself out of.
func (r *RoleRights) Set(ctx context.Context, role string, rights []Right, by *Superuser) (*RoleSet, error) {
	if !ValidRole(role) {
		return nil, Validationf("%q is not a role; the roles are %s", role, strings.Join(Roles, ", "))
	}
	if !RoleConfigurable(role) {
		return nil, Forbiddenf("the owner role always carries every right and cannot be changed")
	}

	clean := make([]Right, 0, len(rights))
	for _, right := range rights {
		if !slices.Contains(AllRights, right) {
			return nil, Validationf("%q is not a right this engine has", right)
		}
		if !slices.Contains(clean, right) {
			clean = append(clean, right)
		}
	}
	for _, required := range RequiredRights {
		if !slices.Contains(clean, required) {
			return nil, Validationf(
				"every role keeps %s: a role without it can sign in and see nothing, "+
					"which is a removed operator rather than a narrower one", required)
		}
	}
	if by != nil && by.Role == role && !slices.Contains(clean, RightSettingsWrite) {
		return nil, Forbiddenf(
			"this is your own role, and removing %s from it would lock you out of this screen",
			RightSettingsWrite)
	}
	sort.Slice(clean, func(i, j int) bool { return clean[i] < clean[j] })

	// Stored as the store's departure from the defaults, so a set that matches
	// them is stored as no rows and the role goes back to tracking.
	def := DefaultRightsOf(role)
	var grantedBy *int64
	if by != nil {
		grantedBy = &by.ID
	}
	err := InTx(ctx, r.app.db, func(tx *sql.Tx) error {
		// Delete-then-insert is not serialisable on its own: two owners saving
		// the same role at once each delete the rows they can see and then
		// insert, and the store ends up with the union of two sets that nobody
		// chose. There is no row to lock when a role has no override yet, so
		// the lock is on the role's name. It is released at commit.
		if _, err := tx.ExecContext(ctx,
			`SELECT pg_advisory_xact_lock($1, hashtext($2))`,
			roleRightsLockKey, role); err != nil {
			return Internalf(err, "lock the role")
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM role_rights WHERE role = $1`, role); err != nil {
			return Internalf(err, "clear role rights")
		}
		if sameRights(clean, def) {
			return nil
		}
		for _, right := range clean {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO role_rights (role, right_name, granted_by)
				VALUES ($1, $2, $3)`, role, string(right), grantedBy); err != nil {
				return Internalf(err, "grant %s to %s", right, role)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &RoleSet{
		Role: role, Rights: clean, Default: def,
		Customized: !sameRights(clean, def), Configurable: true,
	}, nil
}

// Reset returns a role to the rights the engine ships for it, by removing the
// store's override rather than by writing the defaults down — so the role
// tracks them again from here on.
func (r *RoleRights) Reset(ctx context.Context, role string) (*RoleSet, error) {
	if !ValidRole(role) {
		return nil, Validationf("%q is not a role; the roles are %s", role, strings.Join(Roles, ", "))
	}
	if !RoleConfigurable(role) {
		return nil, Forbiddenf("the owner role always carries every right and cannot be changed")
	}
	err := InTx(ctx, r.app.db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			`SELECT pg_advisory_xact_lock($1, hashtext($2))`,
			roleRightsLockKey, role); err != nil {
			return Internalf(err, "lock the role")
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM role_rights WHERE role = $1`, role); err != nil {
			return Internalf(err, "reset role rights")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	def := DefaultRightsOf(role)
	return &RoleSet{Role: role, Rights: def, Default: def, Configurable: true}, nil
}

// overrides loads every stored set, keyed by role and sorted.
//
// Rights the engine no longer has are dropped on the way out. The table has no
// foreign key to hold them to the list — the rights live in Go — so a right
// removed in a later release leaves rows behind, and the intersection is what
// keeps them from resolving into something nobody can name.
func (r *RoleRights) overrides(ctx context.Context) (map[string][]Right, error) {
	rows, err := r.app.db.QueryContext(ctx,
		`SELECT role, right_name FROM role_rights ORDER BY role, right_name`)
	if err != nil {
		return nil, Internalf(err, "read role rights")
	}
	defer rows.Close()

	out := map[string][]Right{}
	for rows.Next() {
		var role, name string
		if err := rows.Scan(&role, &name); err != nil {
			return nil, Internalf(err, "scan role right")
		}
		right := Right(name)
		if !slices.Contains(AllRights, right) {
			continue
		}
		out[role] = append(out[role], right)
	}
	if err := rows.Err(); err != nil {
		return nil, Internalf(err, "read role rights")
	}
	return out, nil
}

// sameRights compares two sets. Both sides are sorted by the time they get
// here, so this is an equality test and not a set comparison.
func sameRights(a, b []Right) bool {
	return slices.Equal(a, b)
}

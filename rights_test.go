package gocommerce

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// The permission model is data, so the first thing to pin is the data: every
// role's rights are drawn from the one enumerated list, and the list is what
// custom roles will later be stored against.
func TestRolesDrawFromTheOneList(t *testing.T) {
	known := map[Right]bool{}
	for _, r := range AllRights {
		if known[r] {
			t.Errorf("%s appears twice in AllRights", r)
		}
		known[r] = true
	}

	for _, role := range Roles {
		if !ValidRole(role) {
			t.Errorf("%q is listed in Roles but ValidRole says no", role)
		}
		rights := RightsOf(role)
		if len(rights) == 0 {
			t.Errorf("role %q carries nothing", role)
		}
		for _, r := range rights {
			if !known[r] {
				t.Errorf("role %q carries %q, which is not in AllRights", role, r)
			}
		}
	}

	// An owner can do everything there is; that is what makes them the role
	// that can grant roles.
	if len(RightsOf(RoleOwner)) != len(AllRights) {
		t.Errorf("owner has %d rights, want all %d", len(RightsOf(RoleOwner)), len(AllRights))
	}
	// And the ones that separate the roles, spelled out so a change to the
	// table has to be deliberate.
	cases := []struct {
		role  string
		right Right
		want  bool
	}{
		{RoleManager, RightCatalogWrite, true},
		{RoleManager, RightOrdersRefund, true},
		{RoleManager, RightSettingsWrite, false},
		{RoleStaff, RightOrdersWrite, true},
		{RoleStaff, RightOrdersRefund, false},
		{RoleStaff, RightCatalogWrite, false},
		{RoleStaff, RightInventoryWrite, false},
		{"nonsense", RightCatalogRead, false},
	}
	for _, c := range cases {
		if got := Can(c.role, c.right); got != c.want {
			t.Errorf("Can(%q, %q) = %v, want %v", c.role, c.right, got, c.want)
		}
	}
}

// The rights are enforced where they are declared: on the route.
func TestRightsAreEnforcedOnAdminRoutes(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	signIn := func(email, role string) string {
		t.Helper()
		if _, err := app.Superusers().Create(ctx, email, "a-long-enough-password", role); err != nil {
			t.Fatalf("create %s: %v", role, err)
		}
		_, session, err := app.Superusers().Authenticate(ctx, email, "a-long-enough-password", "127.0.0.1")
		if err != nil {
			t.Fatalf("sign in %s: %v", role, err)
		}
		return session.Token
	}
	as := func(token string) func(*http.Request) {
		return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) }
	}

	staff := signIn("staff@example.com", RoleStaff)
	manager := signIn("manager@example.com", RoleManager)
	owner := signIn("owner@example.com", RoleOwner)

	cases := []struct {
		name   string
		token  string
		method string
		path   string
		want   int
	}{
		{"staff may read the catalog", staff, "GET", "/api/admin/products", 200},
		{"staff may read orders", staff, "GET", "/api/admin/orders", 200},
		{"staff may not write the catalog", staff, "POST", "/api/admin/products", 403},
		{"staff may not see the team", staff, "GET", "/api/admin/superusers", 403},
		{"manager may write the catalog", manager, "POST", "/api/admin/products", 400},
		{"manager may not see the team", manager, "GET", "/api/admin/superusers", 403},
		{"owner may see the team", owner, "GET", "/api/admin/superusers", 200},
		{"a static token may do anything", testAdminToken, "GET", "/api/admin/superusers", 200},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := do(t, app, c.method, c.path, as(c.token))
			if rec.Code != c.want {
				t.Errorf("%s %s = %d, want %d: %s", c.method, c.path, rec.Code, c.want, rec.Body)
			}
			if c.want == 403 && !strings.Contains(rec.Body.String(), "does not carry") {
				t.Errorf("the refusal does not name the missing right: %s", rec.Body)
			}
		})
	}
}

// A store keeps at least one owner, because owner is the only role that can
// hand out roles.
func TestTheLastOwnerCannotBeDemoted(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	only, err := app.Superusers().Create(ctx, "solo@example.com", "a-long-enough-password", RoleOwner)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := app.Superusers().SetRole(ctx, only.ID, RoleStaff); err == nil {
		t.Fatal("demoted the only owner, locking the store out of its own team screen")
	} else if !strings.Contains(err.Error(), "only owner") {
		t.Errorf("refusal = %q, which does not explain why", err.Error())
	}

	// With a second owner, the first may step down.
	second, err := app.Superusers().Create(ctx, "second@example.com", "a-long-enough-password", RoleOwner)
	if err != nil {
		t.Fatalf("create second: %v", err)
	}
	demoted, err := app.Superusers().SetRole(ctx, only.ID, RoleManager)
	if err != nil {
		t.Fatalf("demote with a second owner present: %v", err)
	}
	if demoted.Role != RoleManager {
		t.Errorf("role = %q, want manager", demoted.Role)
	}
	if len(demoted.Rights) != len(RightsOf(RoleManager)) {
		t.Errorf("the record's rights did not follow the role: %v", demoted.Rights)
	}

	// And now the second one is the last.
	if _, err := app.Superusers().SetRole(ctx, second.ID, RoleStaff); err == nil {
		t.Error("demoted the last remaining owner")
	}

	// An unknown role is refused rather than stored, since RightsOf gives one
	// nothing and the row would lock somebody out silently.
	if _, err := app.Superusers().SetRole(ctx, second.ID, "wizard"); err == nil {
		t.Error("stored a role the engine has never heard of")
	}
}

// Every operator that existed before roles did is an owner: the migration
// defaults the column, so an upgrade changes nobody's access.
func TestExistingOperatorsBecomeOwners(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	su, _, err := app.Superusers().Bootstrap(ctx, "first@example.com", "a-long-enough-password")
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if su.Role != RoleOwner {
		t.Errorf("the first operator is %q, want owner", su.Role)
	}
	if !Can(su.Role, RightSettingsWrite) {
		t.Error("the first operator cannot manage the team")
	}
}

// TestEveryAdminRouteDeclaresRights is the guard the roles system was missing.
//
// Rights are variadic on HandleAdminFunc, so forgetting them is silent: the
// route mounts, authentication still runs, and every signed-in operator can
// reach it whatever their role. That is exactly how the discount and tax routes
// shipped ungated — a staff account, deliberately denied catalog.write, could
// have created a hundred-percent-off code.
//
// The exemptions are named rather than pattern-matched, so adding one is a
// decision somebody writes down.
func TestEveryAdminRouteDeclaresRights(t *testing.T) {
	app := newTestApp(t)

	// Refreshing and ending your own session cannot require a right: they are
	// how an operator with no rights at all still signs out.
	//
	// The /me routes are exempt for the same reason turned around. They act on
	// the caller and on nobody else, and the only right that could gate them is
	// settings.write — which is also the right to change everybody's role. Put
	// them behind it and a staff member cannot change their own password without
	// asking an owner to choose one for them, which is the practice invitations
	// exist to end. The handlers read the operator from the session, so there is
	// no id to tamper with.
	exempt := map[string]bool{
		"POST /api/admin/auth-refresh":       true,
		"POST /api/admin/auth-logout":        true,
		"GET /api/admin/me":                  true,
		"PATCH /api/admin/me":                true,
		"POST /api/admin/me/revoke-sessions": true,
	}

	var ungated []string
	for _, r := range app.Routes() {
		if !r.Admin || len(r.Rights) > 0 {
			continue
		}
		if exempt[r.Method+" "+r.Path] {
			continue
		}
		ungated = append(ungated, r.Method+" "+r.Path)
	}
	if len(ungated) > 0 {
		t.Errorf("admin routes with no rights, reachable by any role: %v", ungated)
	}
}

// A right a route asks for has to be one a role can actually hold, or the route
// is unreachable by anybody and nobody finds out until somebody tries.
func TestRouteRightsExist(t *testing.T) {
	app := newTestApp(t)

	known := map[Right]bool{}
	for _, r := range AllRights {
		known[r] = true
	}
	for _, route := range app.Routes() {
		for _, right := range route.Rights {
			if !known[right] {
				t.Errorf("%s %s wants %q, which is not a right this engine has",
					route.Method, route.Path, right)
			}
		}
	}
}

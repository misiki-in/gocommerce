package gocommerce

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

// signInAs creates an operator in a role and returns a live session token, so a
// test can prove what that person may do rather than what the table says.
func signInAs(t *testing.T, app *App, email, role string) string {
	t.Helper()
	ctx := context.Background()
	if _, err := app.Superusers().Create(ctx, email, "a-long-enough-password", role); err != nil {
		t.Fatalf("create %s: %v", role, err)
	}
	_, session, err := app.Superusers().Authenticate(ctx, email, "a-long-enough-password", "127.0.0.1")
	if err != nil {
		t.Fatalf("sign in %s: %v", role, err)
	}
	return session.Token
}

func bearer(token string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) }
}

// doBody is `do` with a JSON body, which the roles endpoints need and no other
// test so far did.
func doBody(t *testing.T, app *App, method, target, body string, opts ...func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for _, o := range opts {
		o(req)
	}
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	return rec
}

// An untouched store answers with the engine's defaults, and says so: nothing
// is customised, and owner is not something the store may re-cut.
func TestMatrixStartsOnTheDefaults(t *testing.T) {
	app := newTestApp(t)

	matrix, err := app.Roles().Matrix(context.Background())
	if err != nil {
		t.Fatalf("Matrix: %v", err)
	}
	if len(matrix.Roles) != len(Roles) {
		t.Fatalf("matrix has %d roles, want %d", len(matrix.Roles), len(Roles))
	}
	if !slices.Equal(matrix.AllRights, AllRights) {
		t.Errorf("the catalogue is not the engine's: %v", matrix.AllRights)
	}
	for _, row := range matrix.Roles {
		if row.Customized {
			t.Errorf("%s is customised in a store that has changed nothing", row.Role)
		}
		if !slices.Equal(row.Rights, DefaultRightsOf(row.Role)) {
			t.Errorf("%s carries %v, want the default %v", row.Role, row.Rights, DefaultRightsOf(row.Role))
		}
		if want := row.Role != RoleOwner; row.Configurable != want {
			t.Errorf("%s configurable = %v, want %v", row.Role, row.Configurable, want)
		}
	}
}

// The point of the feature: what a role may do is what the store said, and it
// is enforced on the route rather than only shown in the panel.
func TestNarrowingARoleTakesEffectOnTheNextRequest(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	manager := signInAs(t, app, "manager@example.com", RoleManager)

	// A manager refunds by default. 404 rather than 403: the right is carried,
	// so the request gets as far as looking for an order that does not exist.
	if rec := do(t, app, "POST", "/api/admin/orders/9999/refund", bearer(manager)); rec.Code == http.StatusForbidden {
		t.Fatalf("a manager was refused a refund before anything was changed: %s", rec.Body)
	}

	narrowed := []Right{RightCatalogRead, RightCatalogWrite, RightOrdersRead, RightOrdersWrite,
		RightInventoryWrite, RightCustomersRead}
	set, err := app.Roles().Set(ctx, RoleManager, narrowed, nil)
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	if !set.Customized {
		t.Error("a narrowed role does not report itself customised")
	}

	// The same session token, with no sign-out in between: rights are resolved
	// per request, which is the whole reason nothing has to be revoked.
	rec := do(t, app, "POST", "/api/admin/orders/9999/refund", bearer(manager))
	if rec.Code != http.StatusForbidden {
		t.Errorf("refund after the right was withdrawn = %d, want 403", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), string(RightOrdersRefund)) {
		t.Errorf("the refusal does not name the missing right: %s", rec.Body)
	}
}

// And the other direction, which the defaults alone could never produce: a
// staff account that may adjust stock.
func TestWideningARoleGrantsWhatTheDefaultDidNot(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	staff := signInAs(t, app, "staff@example.com", RoleStaff)
	if rec := do(t, app, "POST", "/api/admin/variants/1/inventory", bearer(staff)); rec.Code != http.StatusForbidden {
		t.Fatalf("staff adjusted stock by default = %d, want 403", rec.Code)
	}

	widened := append(DefaultRightsOf(RoleStaff), RightInventoryWrite)
	if _, err := app.Roles().Set(ctx, RoleStaff, widened, nil); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if rec := do(t, app, "POST", "/api/admin/variants/1/inventory", bearer(staff)); rec.Code == http.StatusForbidden {
		t.Errorf("staff was still refused stock after being granted it: %s", rec.Body)
	}

	// The record the panel renders from has to agree with what the engine will
	// enforce, or the screen offers buttons that 403.
	su, ok := app.Superusers().Resolve(ctx, staff)
	if !ok {
		t.Fatal("the session stopped resolving")
	}
	if !su.Has(RightInventoryWrite) {
		t.Errorf("the operator's record does not carry the new right: %v", su.Rights)
	}
}

// A set equal to the default is stored as no override at all, so the role goes
// on tracking a default that a later release may widen.
func TestSavingTheDefaultStoresNoOverride(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	set, err := app.Roles().Set(ctx, RoleStaff, DefaultRightsOf(RoleStaff), nil)
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	if set.Customized {
		t.Error("saving the defaults reports the role as customised")
	}

	var rows int
	if err := app.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM role_rights WHERE role = $1`, RoleStaff).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 0 {
		t.Errorf("%d rows stored for a role that matches the default; it has stopped tracking it", rows)
	}
}

// Reset removes the override rather than writing the defaults down.
func TestResetReturnsARoleToTheDefaults(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	if _, err := app.Roles().Set(ctx, RoleManager, []Right{RightCatalogRead}, nil); err != nil {
		t.Fatalf("Set: %v", err)
	}
	set, err := app.Roles().Reset(ctx, RoleManager)
	if err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if set.Customized || !slices.Equal(set.Rights, DefaultRightsOf(RoleManager)) {
		t.Errorf("after a reset the role carries %v, want the default", set.Rights)
	}

	rights, err := app.Roles().Of(ctx, RoleManager)
	if err != nil {
		t.Fatalf("Of: %v", err)
	}
	if !slices.Equal(rights, DefaultRightsOf(RoleManager)) {
		t.Errorf("resolution still sees %v", rights)
	}
}

func TestSetRefusesWhatWouldBreakTheStore(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	// The owner is the way back into a store configured into a corner.
	if _, err := app.Roles().Set(ctx, RoleOwner, []Right{RightCatalogRead}, nil); err == nil {
		t.Error("re-cut the owner role")
	} else if !strings.Contains(err.Error(), "owner") {
		t.Errorf("refusal = %q, which does not explain why", err)
	}

	if _, err := app.Roles().Set(ctx, RoleStaff, []Right{RightCatalogRead, "catalog.destroy"}, nil); err == nil {
		t.Error("granted a right this engine does not have")
	}

	// A role without the floor is a person who can sign in and see nothing.
	if _, err := app.Roles().Set(ctx, RoleStaff, []Right{RightOrdersRead}, nil); err == nil {
		t.Error("stripped a role past catalog.read")
	}

	if _, err := app.Roles().Set(ctx, "wizard", []Right{RightCatalogRead}, nil); err == nil {
		t.Error("configured a role the engine has never heard of")
	}

	// Nothing above stored anything.
	var rows int
	if err := app.DB().QueryRowContext(ctx, `SELECT count(*) FROM role_rights`).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 0 {
		t.Errorf("a refused change left %d rows behind", rows)
	}
}

// The self-lockout guard: an operator may not save away the right that got them
// to this screen. Owner is unaffected — it is not configurable at all — so the
// case that matters is a manager who was handed settings.write.
func TestYouCannotRemoveYourOwnWayBackIn(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	if _, err := app.Roles().Set(ctx, RoleManager,
		append(DefaultRightsOf(RoleManager), RightSettingsWrite), nil); err != nil {
		t.Fatalf("grant settings.write to manager: %v", err)
	}
	if _, err := app.Superusers().Create(ctx, "manager@example.com", "a-long-enough-password", RoleManager); err != nil {
		t.Fatalf("create manager: %v", err)
	}
	// Resolved from a session rather than from Create, so the operator carries
	// the rights the store gave the role — which is what Set is handed.
	manager, ok := app.Superusers().Resolve(ctx, mustSession(t, app, "manager@example.com"))
	if !ok {
		t.Fatal("the manager's session did not resolve")
	}

	_, err := app.Roles().Set(ctx, RoleManager, DefaultRightsOf(RoleManager), manager)
	if err == nil {
		t.Fatal("an operator saved away their own access to the roles screen")
	}
	if !strings.Contains(err.Error(), "your own role") {
		t.Errorf("refusal = %q, which does not say what happened", err)
	}

	// Somebody else's role is not their own, so the same edit is allowed there.
	if _, err := app.Roles().Set(ctx, RoleStaff, DefaultRightsOf(RoleStaff), manager); err != nil {
		t.Errorf("editing another role was refused: %v", err)
	}

	// And a static admin token has no role to lock itself out of. It is the
	// credential that undoes exactly this kind of mistake.
	if _, err := app.Roles().Set(ctx, RoleManager, DefaultRightsOf(RoleManager), nil); err != nil {
		t.Errorf("a token-authenticated change was refused: %v", err)
	}
}

func mustSession(t *testing.T, app *App, email string) string {
	t.Helper()
	_, session, err := app.Superusers().Authenticate(context.Background(), email, "a-long-enough-password", "127.0.0.1")
	if err != nil {
		t.Fatalf("sign in %s: %v", email, err)
	}
	return session.Token
}

// A right the engine no longer has leaves rows nothing reads. The table has no
// foreign key to hold it — the rights live in Go — so the lookup intersects
// with the catalogue, and this writes the row a future release would leave
// behind to prove it.
func TestRightsTheEngineNoLongerHasAreIgnored(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	if _, err := app.Roles().Set(ctx, RoleStaff, []Right{RightCatalogRead, RightOrdersRead}, nil); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if _, err := app.DB().ExecContext(ctx,
		`INSERT INTO role_rights (role, right_name) VALUES ($1, $2)`,
		RoleStaff, "warehouse.teleport"); err != nil {
		t.Fatalf("insert a stale grant: %v", err)
	}

	rights, err := app.Roles().Of(ctx, RoleStaff)
	if err != nil {
		t.Fatalf("Of: %v", err)
	}
	for _, r := range rights {
		if !slices.Contains(AllRights, r) {
			t.Errorf("resolution returned %q, which is not a right this engine has", r)
		}
	}
}

// The endpoints, including who may reach them.
func TestRoleRoutes(t *testing.T) {
	app := newTestApp(t)

	staff := signInAs(t, app, "staff@example.com", RoleStaff)
	if rec := do(t, app, "GET", "/api/admin/roles", bearer(staff)); rec.Code != http.StatusForbidden {
		t.Errorf("staff reading the matrix = %d, want 403", rec.Code)
	}

	rec := do(t, app, "GET", "/api/admin/roles", withAdmin)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/admin/roles = %d: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"all_rights"`) {
		t.Errorf("the matrix does not carry the catalogue: %s", rec.Body)
	}

	rec = doBody(t, app, "PUT", "/api/admin/roles/staff",
		`{"rights":["catalog.read","orders.read","orders.refund"]}`, withAdmin)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT /api/admin/roles/staff = %d: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"customized":true`) {
		t.Errorf("the saved role does not report itself customised: %s", rec.Body)
	}

	rec = doBody(t, app, "PUT", "/api/admin/roles/owner", `{"rights":["catalog.read"]}`, withAdmin)
	if rec.Code != http.StatusForbidden {
		t.Errorf("PUT on the owner role = %d, want 403", rec.Code)
	}

	if rec := do(t, app, "DELETE", "/api/admin/roles/staff", withAdmin); rec.Code != http.StatusOK {
		t.Errorf("DELETE /api/admin/roles/staff = %d: %s", rec.Code, rec.Body)
	}
	rights, err := app.Roles().Of(context.Background(), RoleStaff)
	if err != nil {
		t.Fatalf("Of: %v", err)
	}
	if !slices.Equal(rights, DefaultRightsOf(RoleStaff)) {
		t.Errorf("after DELETE the role carries %v, want the default", rights)
	}
}

// An invitation shows what the invitee is actually getting, which in a store
// that has re-cut the role is not what the engine ships for it.
func TestAnInvitationShowsTheStoresRights(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	if _, err := app.Roles().Set(ctx, RoleStaff, []Right{RightCatalogRead, RightOrdersRead}, nil); err != nil {
		t.Fatalf("Set: %v", err)
	}
	inv, err := app.Team().Invite(ctx, "new@example.com", RoleStaff, nil)
	if err != nil {
		t.Fatalf("Invite: %v", err)
	}
	if !slices.Equal(inv.Rights, []Right{RightCatalogRead, RightOrdersRead}) {
		t.Errorf("the invitation offers %v, which is not what this store gives staff", inv.Rights)
	}
}

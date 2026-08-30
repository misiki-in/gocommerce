package gocommerce

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// The doctor's value is entirely in whether it notices things. A check that
// always says "ok" is worse than no check, because it converts an unknown into
// a false assurance — so each test below breaks something real and asserts the
// report changes.

func diagnostic(t *testing.T, rep Report, name string) Diagnostic {
	t.Helper()
	for _, c := range rep.Checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no %q check in the report; got %d checks", name, len(rep.Checks))
	return Diagnostic{}
}

func TestDiagnoseHealthyStore(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()
	newSuperuser(t, app, "ops@example.com", "a good password")

	rep := app.Diagnose(ctx)

	if !rep.OK {
		t.Errorf("a freshly migrated store is not healthy: %+v", rep.Failed())
	}
	if rep.Version == "" {
		t.Error("report carries no version")
	}
	if len(rep.Checks) < 8 {
		t.Errorf("only %d checks ran; the doctor is thinner than it looks", len(rep.Checks))
	}

	// Every check must be one of the three known statuses — a typo in a status
	// string would otherwise silently read as "not ok" forever.
	for _, c := range rep.Checks {
		switch c.Status {
		case StatusOK, StatusWarn, StatusFail:
		default:
			t.Errorf("check %q has unknown status %q", c.Name, c.Status)
		}
		if c.Detail == "" {
			t.Errorf("check %q reports nothing", c.Name)
		}
	}

	if got := diagnostic(t, rep, "migrations"); !strings.Contains(got.Detail, "none pending") {
		t.Errorf("migrations = %q, want it to report nothing pending", got.Detail)
	}
	// COD is built in, so a store with no modules still has a way to sell.
	if got := diagnostic(t, rep, "providers"); !strings.Contains(got.Detail, "cod") {
		t.Errorf("providers = %q, want cash on delivery listed", got.Detail)
	}
}

// A store nobody can administer is the one failure an operator cannot recover
// from through the UI, so it must be a hard fail rather than a warning.
func TestDiagnoseFlagsUnreachableAdmin(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	// The test config supplies a token, so drop it to reach the real hazard.
	app.cfg.AdminTokens = nil

	got := diagnostic(t, app.Diagnose(ctx), "admin access")
	if got.Status != StatusFail {
		t.Errorf("admin access = %q, want %q when there are no superusers and no tokens", got.Status, StatusFail)
	}
	if got.Hint == "" {
		t.Error("the check reports the problem but not what to do about it")
	}

	// One superuser is enough to make the store administrable again.
	newSuperuser(t, app, "ops@example.com", "a good password")
	if got := diagnostic(t, app.Diagnose(ctx), "admin access"); got.Status == StatusFail {
		t.Errorf("admin access still %q after creating a superuser: %s", got.Status, got.Detail)
	}
}

func TestDiagnoseFlagsPendingMigrations(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	// Forget one applied migration: the same shape as a database restored from
	// a dump taken before the last deploy.
	if _, err := app.db.ExecContext(ctx,
		`DELETE FROM `+migrationsTable+` WHERE id = $1`, "0005_superusers"); err != nil {
		t.Fatalf("drop a migration record: %v", err)
	}

	rep := app.Diagnose(ctx)
	got := diagnostic(t, rep, "migrations")
	if got.Status != StatusFail {
		t.Errorf("migrations = %q, want %q", got.Status, StatusFail)
	}
	if !strings.Contains(got.Detail, "0005_superusers") {
		t.Errorf("detail = %q, want it to name the missing migration", got.Detail)
	}
	if rep.OK {
		t.Error("report is OK despite a failing check")
	}
}

// An active product with no sellable variant is invisible to shoppers while
// looking perfectly fine in the admin list — exactly the kind of thing a person
// does not notice and a check should.
func TestDiagnoseFlagsUnsellableProducts(t *testing.T) {
	app := newTestApp(t)
	ctx := context.Background()

	p := simpleProduct(t, app, "GHOST-1", 1000, 5)
	if _, err := app.db.ExecContext(ctx,
		`UPDATE variants SET active = false WHERE product_id = $1`, p.ID); err != nil {
		t.Fatalf("deactivate variants: %v", err)
	}
	if _, err := app.db.ExecContext(ctx,
		`UPDATE products SET status = 'active' WHERE id = $1`, p.ID); err != nil {
		t.Fatalf("activate product: %v", err)
	}

	got := diagnostic(t, app.Diagnose(ctx), "catalog")
	if got.Status != StatusWarn {
		t.Errorf("catalog = %q, want %q", got.Status, StatusWarn)
	}
	if !strings.Contains(got.Detail, "no sellable variant") {
		t.Errorf("detail = %q, want it to mention the unsellable product", got.Detail)
	}
}

// The report is the machine-readable half of `gocommerce doctor`, so it has to
// survive the round trip an agent will actually put it through.
func TestReportMarshalsForAgents(t *testing.T) {
	app := newTestApp(t)
	rep := app.Diagnose(context.Background())

	encoded, err := json.Marshal(rep)
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	var back Report
	if err := json.Unmarshal(encoded, &back); err != nil {
		t.Fatalf("unmarshal report: %v", err)
	}
	if len(back.Checks) != len(rep.Checks) || back.OK != rep.OK {
		t.Errorf("round trip lost data: %d/%v vs %d/%v",
			len(back.Checks), back.OK, len(rep.Checks), rep.OK)
	}
	if strings.Contains(string(encoded), `"hint":""`) {
		t.Error("empty hints should be omitted, not serialized")
	}
}

// Failed sorts hard failures ahead of warnings, because an operator reading a
// long report top-down should meet the thing that is actually broken first.
func TestFailedPutsFailuresFirst(t *testing.T) {
	rep := Report{Checks: []Diagnostic{
		{Name: "a", Status: StatusOK},
		{Name: "b", Status: StatusWarn},
		{Name: "c", Status: StatusFail},
		{Name: "d", Status: StatusWarn},
	}}
	got := rep.Failed()
	if len(got) != 3 {
		t.Fatalf("Failed() returned %d checks, want 3", len(got))
	}
	if got[0].Name != "c" {
		t.Errorf("first failure is %q, want the hard failure %q", got[0].Name, "c")
	}
}

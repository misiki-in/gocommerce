package gocommerce

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

func TestSplitPattern(t *testing.T) {
	t.Parallel()

	tests := []struct {
		pattern    string
		wantMethod string
		wantPath   string
		wantErr    string
	}{
		{pattern: "GET /api/products", wantMethod: "GET", wantPath: "/api/products"},
		{pattern: "POST /x/stripe/callback", wantMethod: "POST", wantPath: "/x/stripe/callback"},
		{pattern: "/health", wantPath: "/health"},
		{pattern: "GET /api/products/{id}", wantMethod: "GET", wantPath: "/api/products/{id}"},
		{pattern: "", wantErr: "empty route pattern"},
		{pattern: "FETCH /api/products", wantErr: "unknown method"},
		{pattern: "GET example.com/api", wantErr: "must be absolute"},
	}

	for _, tc := range tests {
		method, path, err := splitPattern(tc.pattern)
		switch {
		case tc.wantErr != "":
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("splitPattern(%q) error = %v, want it to contain %q", tc.pattern, err, tc.wantErr)
			}
		case err != nil:
			t.Errorf("splitPattern(%q) unexpected error: %v", tc.pattern, err)
		case method != tc.wantMethod || path != tc.wantPath:
			t.Errorf("splitPattern(%q) = (%q, %q), want (%q, %q)",
				tc.pattern, method, path, tc.wantMethod, tc.wantPath)
		}
	}
}

func TestCheckNamespace(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		current string
		path    string
		admin   bool
		wantErr bool
	}{
		{name: "core may mount anywhere", current: "", path: "/api/products"},
		{name: "module public route in namespace", current: "cms", path: "/x/cms/pages/{slug}"},
		{name: "module namespace root itself", current: "cms", path: "/x/cms"},
		{name: "module admin route in namespace", current: "cms", path: "/api/admin/x/cms/pages", admin: true},
		{name: "module cannot squat a core route", current: "cms", path: "/api/products", wantErr: true},
		{name: "module cannot claim another namespace", current: "cms", path: "/x/invoices/all", wantErr: true},
		{
			name: "a prefix that merely starts the same is not the namespace",
			// "/x/cms-evil" must not pass the "/x/cms" fence.
			current: "cms", path: "/x/cms-evil/pages", wantErr: true,
		},
		{
			name:    "public path in the admin scope is rejected",
			current: "cms", path: "/x/cms/pages", admin: true, wantErr: true,
		},
		{
			name:    "admin path mounted publicly is rejected",
			current: "cms", path: "/api/admin/x/cms/pages", wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a := &App{current: tc.current}
			err := a.checkNamespace(tc.path, tc.admin)
			if (err != nil) != tc.wantErr {
				t.Errorf("checkNamespace(%q, admin=%v) error = %v, wantErr = %v",
					tc.path, tc.admin, err, tc.wantErr)
			}
		})
	}
}

func TestValidateModules(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mods    []Module
		wantErr string
	}{
		{name: "none", mods: nil},
		{name: "valid names", mods: []Module{&testModule{name: "stripe"}, &testModule{name: "notify-sendgrid"}}},
		{name: "duplicate", mods: []Module{&testModule{name: "cms"}, &testModule{name: "cms"}}, wantErr: "duplicate"},
		{name: "uppercase", mods: []Module{&testModule{name: "Stripe"}}, wantErr: "invalid module name"},
		{name: "underscore", mods: []Module{&testModule{name: "notify_sendgrid"}}, wantErr: "invalid module name"},
		{name: "trailing dash", mods: []Module{&testModule{name: "cms-"}}, wantErr: "invalid module name"},
		{name: "empty", mods: []Module{&testModule{name: ""}}, wantErr: "invalid module name"},
		{name: "nil module", mods: []Module{nil}, wantErr: "is nil"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateModules(tc.mods)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("unexpected error: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Errorf("error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestValidateMigrations(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		set     migrationSet
		wantErr string
	}{
		{
			name: "valid",
			set:  migrationSet{Owner: "core", Migrations: []Migration{{ID: "0001_init", SQL: "SELECT 1"}}},
		},
		{
			name:    "empty id",
			set:     migrationSet{Owner: "core", Migrations: []Migration{{SQL: "SELECT 1"}}},
			wantErr: "empty ID",
		},
		{
			name:    "bad id characters",
			set:     migrationSet{Owner: "core", Migrations: []Migration{{ID: "0001-Init", SQL: "SELECT 1"}}},
			wantErr: "must match",
		},
		{
			name: "duplicate id",
			set: migrationSet{Owner: "core", Migrations: []Migration{
				{ID: "a", SQL: "SELECT 1"}, {ID: "a", SQL: "SELECT 2"},
			}},
			wantErr: "duplicate ID",
		},
		{
			name:    "neither sql nor run",
			set:     migrationSet{Owner: "core", Migrations: []Migration{{ID: "a"}}},
			wantErr: "neither SQL nor Run",
		},
		{
			name: "both sql and run",
			set: migrationSet{Owner: "core", Migrations: []Migration{{
				ID: "a", SQL: "SELECT 1", Run: func(context.Context, *sql.Tx) error { return nil },
			}}},
			wantErr: "exactly one",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateMigrations([]migrationSet{tc.set})
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("unexpected error: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Errorf("error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

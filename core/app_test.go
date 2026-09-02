package gocommerce

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestBootAndHealth is M0's headline proof: the engine boots against a real
// PostgreSQL database and reports itself alive and ready.
func TestBootAndHealth(t *testing.T) {
	app := newTestApp(t)

	t.Run("liveness", func(t *testing.T) {
		rec := do(t, app, http.MethodGet, "/health")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
		}
		var got struct {
			Data struct {
				Status  string `json:"status"`
				Version string `json:"version"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.Data.Status != "ok" || got.Data.Version != Version {
			t.Errorf("health = %+v, want status ok and version %s", got.Data, Version)
		}
	})

	t.Run("readiness reflects configuration", func(t *testing.T) {
		rec := do(t, app, http.MethodGet, "/health/ready")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
		}
		body := rec.Body.String()
		// The defaults the project committed to: USD and en.
		for _, want := range []string{`"currency":"USD"`, `"language":"en"`, `"status":"ready"`} {
			if !strings.Contains(body, want) {
				t.Errorf("readiness body %s missing %q", body, want)
			}
		}
	})
}

// TestModuleExtension proves the extension mechanism end to end: a module
// brings its own table, mounts a public and an admin route, and contributes
// to the API contract — without the engine knowing anything about it.
func TestModuleExtension(t *testing.T) {
	mod := &testModule{
		name: "widgets",
		migration: &Migration{
			ID:  "0001_init",
			SQL: `CREATE TABLE widgets_items (id bigserial PRIMARY KEY, label text NOT NULL)`,
		},
		routes: []string{"GET /x/widgets/items"},
		admin:  []string{"GET /api/admin/x/widgets/items"},
		spec: []byte(`{"paths":{"/x/widgets/items":{"get":{"summary":"List widgets",` +
			`"responses":{"200":{"description":"ok"}}}}}}`),
	}
	app := newTestApp(t, mod)

	t.Run("module table exists", func(t *testing.T) {
		// to_regclass resolves through the connection's search path, so this
		// asks "is the table visible to the engine" rather than assuming
		// which schema it landed in.
		var exists bool
		err := app.DB().QueryRowContext(context.Background(),
			`SELECT to_regclass('widgets_items') IS NOT NULL`).Scan(&exists)
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		if !exists {
			t.Error("the module's migration did not create widgets_items")
		}
	})

	t.Run("migration is recorded under the module's name", func(t *testing.T) {
		var count int
		err := app.DB().QueryRowContext(context.Background(),
			`SELECT count(*) FROM `+migrationsTable+` WHERE owner = $1 AND id = $2`,
			"widgets", "0001_init").Scan(&count)
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		if count != 1 {
			t.Errorf("migration rows = %d, want 1", count)
		}
	})

	t.Run("public route is served", func(t *testing.T) {
		rec := do(t, app, http.MethodGet, "/x/widgets/items")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body)
		}
		if !strings.Contains(rec.Body.String(), `"module":"widgets"`) {
			t.Errorf("body = %s, want it to come from the module", rec.Body)
		}
	})

	t.Run("admin route requires a token", func(t *testing.T) {
		// Choosing HandleAdmin is the authentication: the module never wrote
		// a line of auth code.
		rec := do(t, app, http.MethodGet, "/api/admin/x/widgets/items")
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("unauthenticated status = %d, want 401", rec.Code)
		}
		if got := rec.Header().Get("WWW-Authenticate"); !strings.Contains(got, "Bearer") {
			t.Errorf("WWW-Authenticate = %q, want a Bearer challenge", got)
		}

		rec = do(t, app, http.MethodGet, "/api/admin/x/widgets/items", withAdmin)
		if rec.Code != http.StatusOK {
			t.Errorf("authenticated status = %d, want 200: %s", rec.Code, rec.Body)
		}

		rec = do(t, app, http.MethodGet, "/api/admin/x/widgets/items",
			header("Authorization", "Bearer wrong-token"))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("bad-token status = %d, want 401", rec.Code)
		}
	})

	t.Run("module contributes to the served contract", func(t *testing.T) {
		paths, err := app.SpecPaths()
		if err != nil {
			t.Fatalf("SpecPaths: %v", err)
		}
		if !contains(paths, "/x/widgets/items") {
			t.Errorf("spec paths = %v, want the module's path merged in", paths)
		}
	})

	t.Run("stop hooks run on close", func(t *testing.T) {
		if err := app.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if !mod.stopped {
			t.Error("the module's OnStop hook did not run")
		}
	})
}

// TestNamespaceViolationFailsStartup proves the route fence is structural: a
// module that reaches outside its namespace does not start, rather than
// quietly shadowing a core route or skipping admin authentication.
func TestNamespaceViolationFailsStartup(t *testing.T) {
	dsn := requireDB(t)
	resetSchema(t, dsn)

	tests := []struct {
		name string
		mod  *testModule
	}{
		{
			name: "public route outside the namespace",
			mod:  &testModule{name: "rogue", routes: []string{"GET /api/products"}},
		},
		{
			name: "admin route in another module's namespace",
			mod:  &testModule{name: "rogue", admin: []string{"GET /api/admin/x/cms/pages"}},
		},
		{
			name: "unauthenticated mount of an admin path",
			mod:  &testModule{name: "rogue", routes: []string{"GET /api/admin/x/rogue/secret"}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			app, err := New(testConfig(dsn), tc.mod)
			if err == nil {
				_ = app.Close()
				t.Fatal("expected startup to fail")
			}
			if !strings.Contains(err.Error(), "outside its namespace") {
				t.Errorf("error = %v, want it to name the namespace violation", err)
			}
		})
	}
}

// TestSpecCoversEveryCoreRoute is the honesty check on a hand-maintained
// contract: the served spec cannot silently drift from the routes the engine
// actually mounts.
func TestSpecCoversEveryCoreRoute(t *testing.T) {
	app := newTestApp(t)

	paths, err := app.SpecPaths()
	if err != nil {
		t.Fatalf("SpecPaths: %v", err)
	}
	for _, r := range app.Routes() {
		if r.Owner != coreMigrationOwner {
			continue
		}
		if r.UI {
			// The admin panel serves static files; documenting them in an API
			// contract would describe a file server, not an endpoint.
			continue
		}
		if !contains(paths, r.Path) {
			t.Errorf("route %s %s is served but absent from openapi.json", r.Method, r.Path)
		}
	}
}

func TestSpecIsValidJSONDocument(t *testing.T) {
	app := newTestApp(t)

	var doc struct {
		OpenAPI string         `json:"openapi"`
		Info    map[string]any `json:"info"`
		Paths   map[string]any `json:"paths"`
	}
	if err := json.Unmarshal(app.Spec(), &doc); err != nil {
		t.Fatalf("served spec is not valid JSON: %v", err)
	}
	if !strings.HasPrefix(doc.OpenAPI, "3.") {
		t.Errorf("openapi = %q, want a 3.x document", doc.OpenAPI)
	}
	if len(doc.Paths) == 0 {
		t.Error("spec documents no paths")
	}
}

// TestMigrationsAreIdempotent proves a restart is safe: the second boot
// applies nothing and leaves exactly one record per migration.
func TestMigrationsAreIdempotent(t *testing.T) {
	dsn := requireDB(t)
	resetSchema(t, dsn)

	newMod := func() *testModule {
		return &testModule{
			name: "widgets",
			migration: &Migration{
				ID:  "0001_init",
				SQL: `CREATE TABLE widgets_items (id bigserial PRIMARY KEY)`,
			},
		}
	}

	first, err := New(testConfig(dsn), newMod())
	if err != nil {
		t.Fatalf("first boot: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close first: %v", err)
	}

	// A second boot must not re-run a migration whose CREATE TABLE would fail.
	second, err := New(testConfig(dsn), newMod())
	if err != nil {
		t.Fatalf("second boot: %v", err)
	}
	defer second.Close()

	var moduleCount int
	if err := second.DB().QueryRowContext(context.Background(),
		`SELECT count(*) FROM `+migrationsTable+` WHERE owner = 'widgets'`).Scan(&moduleCount); err != nil {
		t.Fatalf("query: %v", err)
	}
	if moduleCount != 1 {
		t.Errorf("module migration rows = %d, want exactly 1", moduleCount)
	}

	// Core's own migrations are equally once-only.
	var coreCount int
	if err := second.DB().QueryRowContext(context.Background(),
		`SELECT count(*) FROM `+migrationsTable+` WHERE owner = $1`, coreMigrationOwner).Scan(&coreCount); err != nil {
		t.Fatalf("query: %v", err)
	}
	if want := len(coreMigrations()); coreCount != want {
		t.Errorf("core migration rows = %d, want %d", coreCount, want)
	}
}

// TestLanguageNegotiationOverHTTP checks the whole request path, not just the
// matcher: the resolved language reaches the handler and the response
// declares it.
func TestLanguageNegotiationOverHTTP(t *testing.T) {
	dsn := requireDB(t)
	resetSchema(t, dsn)

	cfg := testConfig(dsn)
	cfg.Languages = []string{"en", "fr"}

	var seen string
	mod := &testModule{name: "probe"}
	app, err := New(cfg, mod)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer app.Close()

	// Mount a probe through the engine's own (unrestricted) wiring.
	app.HandleFunc("GET /probe/language", func(w http.ResponseWriter, r *http.Request) {
		seen = app.RequestLanguage(r)
		Respond(w, http.StatusOK, map[string]string{"language": seen})
	})

	tests := []struct {
		name   string
		target string
		accept string
		want   string
	}{
		{name: "default", target: "/probe/language", want: "en"},
		{name: "header", target: "/probe/language", accept: "fr", want: "fr"},
		{name: "regional header", target: "/probe/language", accept: "fr-CA", want: "fr"},
		{name: "query overrides header", target: "/probe/language?lang=en", accept: "fr", want: "en"},
		{name: "unsupported falls back", target: "/probe/language", accept: "ja", want: "en"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var opts []func(*http.Request)
			if tc.accept != "" {
				opts = append(opts, header("Accept-Language", tc.accept))
			}
			rec := do(t, app, http.MethodGet, tc.target, opts...)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", rec.Code, rec.Body)
			}
			if seen != tc.want {
				t.Errorf("handler saw language %q, want %q", seen, tc.want)
			}
			if got := rec.Header().Get("Content-Language"); got != tc.want {
				t.Errorf("Content-Language = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestNewRejectsBadConfig(t *testing.T) {
	t.Parallel()

	t.Run("no database url", func(t *testing.T) {
		if _, err := New(Config{AdminTokens: []string{"t"}}); err == nil {
			t.Error("expected an error when DBURL is empty")
		}
	})

	t.Run("no admin token outside dev", func(t *testing.T) {
		_, err := New(Config{DBURL: "postgres://localhost/x"})
		if err == nil || !strings.Contains(err.Error(), "AdminTokens") {
			t.Errorf("error = %v, want it to demand an admin token", err)
		}
	})

	t.Run("duplicate module names", func(t *testing.T) {
		_, err := New(Config{DBURL: "postgres://localhost/x", Dev: true},
			&testModule{name: "dup"}, &testModule{name: "dup"})
		if err == nil || !strings.Contains(err.Error(), "duplicate") {
			t.Errorf("error = %v, want a duplicate-name error", err)
		}
	})
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

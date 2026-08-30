package gocommerce

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
)

// testDSNEnv names the environment variable holding the PostgreSQL URL used
// by integration tests. Tests that need a database skip without it, so
// `go test ./...` still runs the pure-logic suite on a machine with no
// PostgreSQL — but CI always sets it, so the database path is never untested
// where it counts.
const testDSNEnv = "GOCOMMERCE_TEST_DB"

const testAdminToken = "test-admin-token"

// requireDB returns a connection string pointing at a PostgreSQL schema
// created for this test alone, dropped when the test finishes.
//
// Isolation is per test rather than "empty the database", because `go test
// ./...` runs each package as its own binary, concurrently: a shared database
// that every test wipes on entry means one package deleting another's tables
// halfway through its run.
func requireDB(t *testing.T) string {
	t.Helper()

	base := os.Getenv(testDSNEnv)
	if base == "" {
		t.Skipf("set %s to a PostgreSQL URL to run integration tests", testDSNEnv)
	}
	if !strings.Contains(strings.ToLower(dsnDatabase(base)), "test") {
		t.Fatalf("%s database name must contain \"test\" (refusing to use %q)",
			testDSNEnv, dsnDatabase(base))
	}

	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatalf("generate a schema name: %v", err)
	}
	schema := "gctest_" + hex.EncodeToString(suffix[:])

	db, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	defer db.Close()
	if _, err := db.ExecContext(context.Background(), `CREATE SCHEMA `+schema); err != nil {
		t.Fatalf("create test schema: %v", err)
	}
	t.Cleanup(func() {
		cleanup, err := sql.Open("pgx", base)
		if err != nil {
			return
		}
		defer cleanup.Close()
		if _, err := cleanup.ExecContext(context.Background(),
			`DROP SCHEMA IF EXISTS `+schema+` CASCADE`); err != nil {
			t.Logf("could not drop the test schema %s: %v", schema, err)
		}
	})

	// libpq's `options` applies per connection, so every connection in the
	// pool lands in the same schema — which a bare SET search_path would not
	// guarantee.
	separator := "?"
	if strings.Contains(base, "?") {
		separator = "&"
	}
	return base + separator + "options=" + url.QueryEscape("-csearch_path="+schema)
}

// resetSchema is a no-op now that every test gets its own schema. It remains
// so the call sites read the same as before.
func resetSchema(t *testing.T, dsn string) { t.Helper() }

// dsnDatabase extracts the database name from a URL- or keyword-style DSN,
// well enough for the safety guard above.
func dsnDatabase(dsn string) string {
	if i := strings.Index(dsn, "dbname="); i >= 0 {
		rest := dsn[i+len("dbname="):]
		if j := strings.IndexAny(rest, " \t"); j >= 0 {
			return rest[:j]
		}
		return rest
	}
	s := dsn
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[i+1:]
		if j := strings.IndexAny(s, "?"); j >= 0 {
			s = s[:j]
		}
		return s
	}
	return ""
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func testConfig(dsn string) Config {
	return Config{
		DBURL:       dsn,
		AdminTokens: []string{testAdminToken},
		Logger:      quietLogger(),
	}
}

// newTestApp boots an engine against a freshly emptied test database.
func newTestApp(t *testing.T, mods ...Module) *App {
	t.Helper()
	dsn := requireDB(t)
	resetSchema(t, dsn)

	app, err := New(testConfig(dsn), mods...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = app.Close() })
	return app
}

// do runs a request through the engine's full middleware chain.
func do(t *testing.T, app *App, method, target string, opts ...func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	for _, o := range opts {
		o(req)
	}
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	return rec
}

func withAdmin(r *http.Request) { r.Header.Set("Authorization", "Bearer "+testAdminToken) }

func header(k, v string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set(k, v) }
}

// ---------------------------------------------------------------- test module

// testModule is a minimal module used to prove the extension mechanism: it
// owns a table, mounts a public and an admin route, contributes to the API
// contract, and records its lifecycle hooks.
type testModule struct {
	name      string
	migration *Migration
	routes    []string
	admin     []string
	spec      []byte

	started, stopped bool
	registerErr      error
}

func (m *testModule) Name() string { return m.name }

func (m *testModule) Migrations() []Migration {
	if m.migration == nil {
		return nil
	}
	return []Migration{*m.migration}
}

func (m *testModule) Register(app *App) error {
	if m.registerErr != nil {
		return m.registerErr
	}
	for _, p := range m.routes {
		app.HandleFunc(p, func(w http.ResponseWriter, r *http.Request) {
			Respond(w, http.StatusOK, map[string]string{"module": m.name})
		})
	}
	for _, p := range m.admin {
		app.HandleAdminFunc(p, func(w http.ResponseWriter, r *http.Request) {
			Respond(w, http.StatusOK, map[string]string{"module": m.name, "scope": "admin"})
		})
	}
	app.OnStart(func(context.Context) error { m.started = true; return nil })
	app.OnStop(func(context.Context) error { m.stopped = true; return nil })
	return nil
}

func (m *testModule) OpenAPI() []byte { return m.spec }

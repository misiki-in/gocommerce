// Package gctest is the module author's test kit.
//
// Writing a payment provider should not require understanding the whole
// engine, and it should certainly not require every author to re-derive the
// same harness: boot an app on a scratch database, place an order, drain the
// outbox, assert on what was delivered. That harness lives here.
//
//	func TestMyProvider(t *testing.T) {
//	    app := gctest.New(t, mymodule.New(mymodule.Config{...}))
//	    order := gctest.PlaceOrder(t, app, "mycode")
//	    gctest.DrainOutbox(t, app)
//	    // ...assert
//	}
//
// Tests skip themselves when GOCOMMERCE_TEST_DB is unset, so a contributor
// without PostgreSQL can still run the rest of a module's suite.
package gctest

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/misiki/gocommerce"
)

// DSNEnv names the environment variable holding the PostgreSQL URL used by
// integration tests.
const DSNEnv = "GOCOMMERCE_TEST_DB"

// AdminToken is the admin credential in a test app.
const AdminToken = "gctest-admin-token"

// New boots an engine with the given modules against an empty scratch
// database, and closes it when the test finishes.
func New(t *testing.T, mods ...gocommerce.Module) *gocommerce.App {
	t.Helper()
	return NewWithConfig(t, gocommerce.Config{}, mods...)
}

// NewWithConfig is New with configuration of your own. DBURL, AdminTokens and
// Logger are filled in if you leave them empty.
func NewWithConfig(t *testing.T, cfg gocommerce.Config, mods ...gocommerce.Module) *gocommerce.App {
	t.Helper()

	if cfg.DBURL == "" {
		cfg.DBURL = IsolatedDSN(t)
	}
	if len(cfg.AdminTokens) == 0 {
		cfg.AdminTokens = []string{AdminToken}
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	}

	app, err := gocommerce.New(cfg, mods...)
	if err != nil {
		t.Fatalf("gctest: boot the engine: %v", err)
	}
	t.Cleanup(func() { _ = app.Close() })
	return app
}

// IsolatedDSN returns a connection string pointing at a PostgreSQL schema
// created for this test alone, and drops it when the test finishes.
//
// Isolation is per test rather than "empty the database", because `go test
// ./...` runs each package as its own binary, concurrently. A shared database
// that every test wipes on entry means one package deleting another package's
// tables halfway through its run — which looks like a flaky engine and is
// actually a flaky harness.
//
// It refuses any database whose name does not contain "test": the helper
// creates and drops schemas, and that guard is what makes it safe to hand
// someone the environment variable.
func IsolatedDSN(t *testing.T) string {
	t.Helper()

	base := os.Getenv(DSNEnv)
	if base == "" {
		t.Skipf("set %s to a PostgreSQL URL to run integration tests", DSNEnv)
	}
	if !strings.Contains(strings.ToLower(databaseName(base)), "test") {
		t.Fatalf("gctest: refusing to use %q — the %s database name must contain \"test\"",
			databaseName(base), DSNEnv)
	}

	schema := "gctest_" + randomSuffix(t)
	admin, err := gocommerce.OpenDB(context.Background(), base)
	if err != nil {
		t.Fatalf("gctest: open the test database: %v", err)
	}
	defer admin.Close()

	if _, err := admin.ExecContext(context.Background(),
		`CREATE SCHEMA `+schema); err != nil {
		t.Fatalf("gctest: create the test schema: %v", err)
	}
	t.Cleanup(func() {
		db, err := gocommerce.OpenDB(context.Background(), base)
		if err != nil {
			return
		}
		defer db.Close()
		if _, err := db.ExecContext(context.Background(),
			`DROP SCHEMA IF EXISTS `+schema+` CASCADE`); err != nil {
			t.Logf("gctest: could not drop the test schema %s: %v", schema, err)
		}
	})

	return withSearchPath(base, schema)
}

// withSearchPath points a DSN at one schema. libpq's `options` is applied per
// connection, so every connection in the pool lands in the same place — which
// a bare `SET search_path` would not guarantee.
func withSearchPath(dsn, schema string) string {
	separator := "?"
	if strings.Contains(dsn, "?") {
		separator = "&"
	}
	return dsn + separator + "options=" + url.QueryEscape("-csearch_path="+schema)
}

func randomSuffix(t *testing.T) string {
	t.Helper()
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("gctest: generate a schema name: %v", err)
	}
	return hex.EncodeToString(b)
}

func databaseName(dsn string) string {
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
		if j := strings.IndexByte(s, '?'); j >= 0 {
			s = s[:j]
		}
		return s
	}
	return ""
}

// ------------------------------------------------------------------ requests

// Request sends a request through the engine's full middleware chain.
func Request(t *testing.T, app *gocommerce.App, method, target string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return request(t, app, method, target, body, "")
}

// AdminRequest is Request with a valid admin token attached.
func AdminRequest(t *testing.T, app *gocommerce.App, method, target string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return request(t, app, method, target, body, AdminToken)
}

func request(t *testing.T, app *gocommerce.App, method, target string, body any, token string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("gctest: encode request body: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}
	req := httptest.NewRequest(method, target, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	return rec
}

// DecodeData unmarshals the "data" member of a JSON envelope.
func DecodeData(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	var envelope struct {
		Data  json.RawMessage `json:"data"`
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("gctest: decode response: %v (body: %s)", err, rec.Body)
	}
	if envelope.Error != nil {
		t.Fatalf("gctest: request failed: %s: %s", envelope.Error.Code, envelope.Error.Message)
	}
	if err := json.Unmarshal(envelope.Data, v); err != nil {
		t.Fatalf("gctest: decode data: %v", err)
	}
}

// ------------------------------------------------------------------ fixtures

// CreateProduct creates an active single-variant product and returns it.
func CreateProduct(t *testing.T, app *gocommerce.App, sku string, priceMinor int64, stock int) *gocommerce.Product {
	t.Helper()
	p, err := app.Products().CreateProduct(context.Background(), gocommerce.ProductInput{
		Title:  "gctest " + sku,
		Status: "active",
		SKU:    sku, PriceMinor: &priceMinor, Stock: &stock,
	})
	if err != nil {
		t.Fatalf("gctest: create product %s: %v", sku, err)
	}
	return p
}

// PlaceOrder runs a complete purchase — product, cart, checkout — with the
// given payment method, and returns the result.
func PlaceOrder(t *testing.T, app *gocommerce.App, paymentCode string) *gocommerce.CheckoutResult {
	t.Helper()
	ctx := context.Background()

	product := CreateProduct(t, app, "GCTEST-"+paymentCode, 2500, 10)
	variant := product.DefaultVariant()
	if variant == nil {
		t.Fatal("gctest: the product has no sellable variant")
	}

	cart, err := app.Cart().Create(ctx, "")
	if err != nil {
		t.Fatalf("gctest: create cart: %v", err)
	}
	if _, err := app.Cart().AddLine(ctx, cart.Token, variant.ID, 1); err != nil {
		t.Fatalf("gctest: add to cart: %v", err)
	}

	result, err := app.Order().Checkout(ctx, paymentCode, gocommerce.CheckoutInput{
		CartID: cart.Token,
		Email:  "gctest@example.com",
		Name:   "GC Test",
		Address: gocommerce.Address{
			Line1: "1 Test Street", City: "Testville",
			PostalCode: "12345", Country: "US",
		},
	}, "")
	if err != nil {
		t.Fatalf("gctest: checkout with %s: %v", paymentCode, err)
	}
	return result
}

// DrainOutbox delivers every pending event, so a test can assert on what
// consumers received without waiting on the background dispatcher.
func DrainOutbox(t *testing.T, app *gocommerce.App) int {
	t.Helper()
	n, err := app.DrainOutbox(context.Background())
	if err != nil {
		t.Fatalf("gctest: drain the outbox: %v", err)
	}
	return n
}

// AssertOutboxEmpty fails if any event is still waiting or was parked as
// undeliverable.
func AssertOutboxEmpty(t *testing.T, app *gocommerce.App) {
	t.Helper()
	pending, dead, err := app.PendingEvents(context.Background())
	if err != nil {
		t.Fatalf("gctest: count pending events: %v", err)
	}
	if pending != 0 || dead != 0 {
		t.Errorf("gctest: outbox is not empty: %d pending, %d dead", pending, dead)
	}
}

// ------------------------------------------------------------------ doubles

// RecordingNotifier captures notifications instead of delivering them.
type RecordingNotifier struct {
	mu   sync.Mutex
	sent []gocommerce.Notification
}

// Notify implements gocommerce.Notifier.
func (n *RecordingNotifier) Notify(ctx context.Context, note gocommerce.Notification) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.sent = append(n.sent, note)
	return nil
}

// All returns everything captured so far.
func (n *RecordingNotifier) All() []gocommerce.Notification {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]gocommerce.Notification, len(n.sent))
	copy(out, n.sent)
	return out
}

// Count returns how many notifications were sent for an event.
func (n *RecordingNotifier) Count(event string) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	c := 0
	for _, s := range n.sent {
		if s.Event == event {
			c++
		}
	}
	return c
}

// RecordingEvents captures events from the bus.
type RecordingEvents struct {
	mu     sync.Mutex
	events []gocommerce.Event
}

// Handler returns a subscriber to pass to App.Subscribe.
func (r *RecordingEvents) Handler() gocommerce.EventHandler {
	return func(ctx context.Context, e gocommerce.Event) error {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.events = append(r.events, e)
		return nil
	}
}

// Names returns the event names seen, in order.
func (r *RecordingEvents) Names() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	names := make([]string, len(r.events))
	for i, e := range r.events {
		names[i] = e.Name
	}
	return names
}

// StubHTTP starts a test HTTP server, so a provider module can be tested
// against a fake vendor rather than the real one.
func StubHTTP(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

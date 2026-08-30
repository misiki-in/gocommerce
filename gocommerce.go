// Package gocommerce is a small, composable commerce engine for Go.
//
// It provides the commerce primitives a store needs — products, variants,
// inventory, carts, checkout, orders, payments, fulfillment and durable
// events — over a PostgreSQL database. Integrations are ordinary Go values
// implementing [Module], composed explicitly in main():
//
//	app, err := gocommerce.New(
//	    gocommerce.Config{DBURL: os.Getenv("DATABASE_URL")},
//	    stripe.New(...),
//	    sendgrid.New(...),
//	)
//	if err != nil { log.Fatal(err) }
//	if err := app.ListenAndServe(); err != nil { log.Fatal(err) }
//
// There is no plugin registry, no dependency-injection container, no
// reflection and no configuration DSL: the complete composition of a store is
// readable from its main().
package gocommerce

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// Version is the engine version. Milestone M0 is the PostgreSQL kernel: it
// boots, migrates, serves health and the OpenAPI contract, and hosts modules.
// It deliberately contains no commerce domain yet.
const Version = "0.0.0-m0"

// Defaults applied by [Config] when a field is left zero.
const (
	DefaultCurrency = "USD" // D14: one settlement currency per store
	DefaultLanguage = "en"  // D21: negotiated per request, extensible
	DefaultAddr     = ":8080"
)

// Fixed HTTP limits. These are constants rather than configuration on
// purpose: configuration should not become a second programming language,
// and no store has a good reason to want a 90-second write timeout.
const (
	maxJSONBytes     = 1 << 20  // 1 MiB
	maxUploadBytes   = 32 << 20 // 32 MiB, for CSV import (M4)
	httpReadTimeout  = 15 * time.Second
	httpWriteTimeout = 60 * time.Second
	httpIdleTimeout  = 120 * time.Second
	shutdownGrace    = 20 * time.Second
)

// Config is the complete configuration surface of the engine. Provider
// configuration belongs to provider modules, not here.
type Config struct {
	// DBURL is the PostgreSQL connection string. Required.
	DBURL string
	// Addr is the listen address. Defaults to ":8080".
	Addr string

	// Currency is the store's single settlement currency as an ISO 4217 code.
	// Defaults to "USD". Money is always stored and returned as integer minor
	// units alongside this code; formatting is the client's job, which is what
	// lets any currency work without an engine change.
	Currency string
	// DefaultLanguage is the BCP 47 tag used when a request expresses no
	// preference. Defaults to "en".
	DefaultLanguage string
	// Languages is the set of tags the store serves. Defaults to
	// []string{DefaultLanguage}. The default language is always included.
	Languages []string

	// AdminTokens are accepted as "Authorization: Bearer <token>" on admin
	// routes. At least one is required unless Dev is set. Supplying several
	// allows rotation without downtime.
	AdminTokens []string
	// AdminAuth replaces the built-in bearer-token middleware wholesale. This
	// is the seam a future identity module uses to add sessions, JWT/OIDC or
	// RBAC without the engine knowing about identity.
	AdminAuth func(http.Handler) http.Handler

	// MediaDir is a directory uploaded media is written to, and served from at
	// MediaURLPrefix. Leaving it empty disables uploads: the media library
	// still records files by URL, and the panel says so rather than offering a
	// button that cannot work.
	//
	// This is the plan's one amendment on storage (§30 said URLs only). Core
	// owns the records and the MediaStore interface, never an SDK — object
	// storage remains a module that replaces MediaStore.
	MediaDir string
	// MediaURLPrefix is the path MediaDir is served at. Defaults to "/media".
	MediaURLPrefix string
	// MediaStore replaces the built-in local-disk store wholesale — the seam an
	// S3 or GCS module uses. When set, MediaDir is ignored.
	MediaStore MediaStore

	// HandlerTimeout bounds a single event handler. Defaults to 10s.
	HandlerTimeout time.Duration
	// CartTTL is how long an untouched cart survives. Defaults to 720h.
	CartTTL time.Duration
	// OrderTTL is how long an unpaid order holds its inventory reservation
	// before it is cancelled and the stock released. Defaults to 24h.
	OrderTTL time.Duration
	// OrderPrefix prefixes human order numbers. Defaults to "GC-".
	OrderPrefix string
	// PricesIncludeTax says whether the prices in the catalog already contain
	// tax. It changes what every figure on an order means, so it is snapshotted
	// onto each order as it is placed: switching a live store must not rewrite
	// what last month's orders were saying.
	//
	// False — tax added at checkout — is the default because it is what a store
	// with no rates configured already does, so turning tax on cannot silently
	// change any total.
	PricesIncludeTax bool

	// FlatShippingMinor is added to every order, in minor units. It is the
	// whole of v1's shipping calculation: a rate engine is a module's job, but
	// a store that charges one flat fee should not need one.
	FlatShippingMinor int64

	// OutboxBatchSize is how many outbox rows one dispatcher pass claims.
	// Defaults to 100.
	OutboxBatchSize int
	// OutboxPoll is the dispatcher's idle poll interval. Defaults to 1s.
	OutboxPoll time.Duration

	// Dev permits booting with no admin token and serves friendlier errors.
	// Never set it in production.
	Dev bool
	// Logger defaults to slog.Default().
	Logger *slog.Logger
}

func (c *Config) applyDefaults() error {
	if c.DBURL == "" {
		return errors.New("gocommerce: Config.DBURL is required")
	}
	if c.Addr == "" {
		c.Addr = DefaultAddr
	}
	if c.Currency == "" {
		c.Currency = DefaultCurrency
	}
	if c.DefaultLanguage == "" {
		c.DefaultLanguage = DefaultLanguage
	}
	if len(c.Languages) == 0 {
		c.Languages = []string{c.DefaultLanguage}
	} else if !containsFold(c.Languages, c.DefaultLanguage) {
		c.Languages = append([]string{c.DefaultLanguage}, c.Languages...)
	}
	if c.HandlerTimeout <= 0 {
		c.HandlerTimeout = 10 * time.Second
	}
	if c.CartTTL <= 0 {
		c.CartTTL = 30 * 24 * time.Hour
	}
	if c.OrderTTL <= 0 {
		c.OrderTTL = 24 * time.Hour
	}
	if c.OrderPrefix == "" {
		c.OrderPrefix = "GC-"
	}
	if c.OutboxBatchSize <= 0 {
		c.OutboxBatchSize = 100
	}
	if c.OutboxPoll <= 0 {
		c.OutboxPoll = time.Second
	}
	if c.MediaURLPrefix == "" {
		c.MediaURLPrefix = "/media"
	}
	if !strings.HasPrefix(c.MediaURLPrefix, "/") {
		return errors.New("gocommerce: Config.MediaURLPrefix must start with /")
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if len(c.AdminTokens) == 0 && !c.Dev {
		return errors.New("gocommerce: at least one Config.AdminTokens entry is required (or set Dev)")
	}
	for _, t := range c.AdminTokens {
		if t == "" {
			return errors.New("gocommerce: Config.AdminTokens contains an empty token")
		}
	}
	return nil
}

// App is the composed engine: configuration, database, HTTP surface and the
// modules wired into it.
type App struct {
	cfg Config
	db  *sql.DB
	log *slog.Logger
	mux *http.ServeMux

	modules []Module
	// current is the module whose Register is executing, or "" for the
	// engine's own wiring. Route namespacing is enforced against it, so a
	// module cannot claim a route outside its namespace even by lying about
	// its name.
	current string

	// Domain services. They are concrete types rather than interfaces: they
	// are not replaceable, and inventing an interface for something with one
	// implementation buys nothing but indirection.
	catalog     *Catalog
	inventory   *Inventory
	carts       *Carts
	orders      *Orders
	payments    *Payments
	fulfillment *Fulfillments
	transfer    *Transfer
	superusers  *Superusers
	media       *Media
	discounts   *Discounts
	locations   *Locations
	invitations *Invitations
	taxes       *Taxes
	// mediaStore is nil when the store has nowhere to put uploads, which is a
	// supported configuration: the library still records media by URL.
	mediaStore MediaStore

	bus      *eventBus
	outbox   *outbox
	notifier *notifierSet

	translator      Translator
	translatorOwner string

	// Which module owns each provider code, so a collision names the culprit.
	paymentOwners     map[string]string
	fulfillmentOwners map[string]string

	routes  []Route
	onStart []hook
	onStop  []hook

	// regErr accumulates wiring failures reported by methods that cannot
	// return an error (Handle and friends). New surfaces it, so a bad route
	// fails startup with a clear message instead of panicking or, worse,
	// silently not being served.
	regErr error

	// spec is the merged OpenAPI document, built once at startup.
	spec []byte

	srv *http.Server
}

type hook struct {
	owner string
	fn    func(context.Context) error
}

// Route is a mounted HTTP route, recorded so that the OpenAPI contract can be
// checked against reality rather than trusted.
type Route struct {
	Method  string
	Path    string
	Admin   bool
	Owner   string // "core" or a module name
	Pattern string
	// Rights the route requires. Empty means any authenticated admin — see
	// HandleAdmin.
	Rights []Right
	// UI marks a route that serves the admin panel's own files rather than
	// part of the API. Those are excluded from the OpenAPI contract: a spec
	// describing a static file server would be noise, and the coverage test
	// needs to tell "undocumented endpoint" apart from "not an endpoint".
	UI bool
}

// New opens the database, applies core and module migrations, wires the
// engine's own routes, and then registers each module in the order given.
//
// Ordering is fixed and total: every migration has been applied before any
// Register runs, so a module may rely on its own tables existing; and no
// module has been registered before the engine's own routes are mounted, so
// core routes always win a conflict.
func New(cfg Config, mods ...Module) (*App, error) {
	if err := cfg.applyDefaults(); err != nil {
		return nil, err
	}
	if err := validateModules(mods); err != nil {
		return nil, err
	}

	db, err := OpenDB(context.Background(), cfg.DBURL)
	if err != nil {
		return nil, err
	}

	a := &App{
		cfg:               cfg,
		db:                db,
		log:               cfg.Logger,
		mux:               http.NewServeMux(),
		modules:           mods,
		paymentOwners:     map[string]string{},
		fulfillmentOwners: map[string]string{},
	}
	a.buildServices()

	if err := a.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}

	// Core wiring first, in plain code, readable top to bottom — and mounted
	// before any module, so a core route always exists before something else
	// could try to shadow it.
	a.subscribeNotifications()
	a.mountCoreRoutes()
	if a.regErr != nil {
		db.Close()
		return nil, fmt.Errorf("gocommerce: wiring core routes: %w", a.regErr)
	}

	for _, m := range mods {
		a.current = m.Name()
		err := m.Register(a)
		a.current = ""
		if err != nil {
			db.Close()
			return nil, fmt.Errorf("gocommerce: module %q: Register: %w", m.Name(), err)
		}
		if a.regErr != nil {
			db.Close()
			return nil, fmt.Errorf("gocommerce: module %q: %w", m.Name(), a.regErr)
		}
		a.log.Info("module registered", "module", m.Name(), "routes", a.routeCount(m.Name()))
	}

	if err := a.buildSpec(); err != nil {
		db.Close()
		return nil, fmt.Errorf("gocommerce: %w", err)
	}
	a.startBackgroundWork()

	return a, nil
}

// buildServices constructs the domain services and the built-in providers.
//
// The built-ins are registered through exactly the same surface a module uses.
// The engine eats its own extension mechanism, so the mechanism cannot rot
// unnoticed: if registering a payment method broke, cash on delivery would
// break with it.
func (a *App) buildServices() {
	a.bus = &eventBus{log: a.log, timeout: a.cfg.HandlerTimeout}
	a.outbox = &outbox{
		db: a.db, bus: a.bus, log: a.log,
		batchSize: a.cfg.OutboxBatchSize, poll: a.cfg.OutboxPoll,
		delivered: make(chan struct{}, 1),
		wake:      make(chan struct{}, 1),
	}
	a.notifier = &notifierSet{log: a.log}

	a.catalog = &Catalog{app: a}
	a.inventory = &Inventory{app: a}
	a.carts = &Carts{app: a}
	a.orders = &Orders{app: a}
	a.payments = &Payments{app: a, providers: map[string]PaymentProvider{}}
	a.fulfillment = &Fulfillments{app: a, providers: map[string]FulfillmentProvider{}}
	a.transfer = &Transfer{app: a}
	a.superusers = newSuperusers(a.db)
	a.media = &Media{app: a}
	a.discounts = &Discounts{app: a}
	a.locations = &Locations{app: a}
	a.invitations = &Invitations{app: a}
	a.taxes = &Taxes{app: a}

	// An explicit store wins; otherwise a directory gets the built-in one; with
	// neither, uploads are simply unavailable and the library is URL-only.
	switch {
	case a.cfg.MediaStore != nil:
		a.mediaStore = a.cfg.MediaStore
	case a.cfg.MediaDir != "":
		a.mediaStore = &LocalMediaStore{Dir: a.cfg.MediaDir, Prefix: a.cfg.MediaURLPrefix}
	}

	a.RegisterPayment(codProvider{})
	a.RegisterFulfillment(manualFulfillment{})
	a.RegisterNotifier(ChannelEmail, logNotifier{log: a.log})
	a.RegisterNotifier(ChannelSMS, logNotifier{log: a.log})
}

// startBackgroundWork schedules the engine's own long-running jobs. They are
// registered as ordinary lifecycle hooks, so they start and stop with the
// server and a test that never serves is never surprised by one.
func (a *App) startBackgroundWork() {
	a.OnStart(func(ctx context.Context) error {
		go a.outbox.run(ctx)
		go a.runSweepers(ctx)
		return nil
	})
}

// runSweepers reclaims what abandoned traffic leaves behind: carts nobody came
// back to, and inventory held by orders whose payment never arrived.
func (a *App) runSweepers(ctx context.Context) {
	const interval = 5 * time.Minute
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	sweep := func() {
		if n, err := a.carts.SweepExpired(ctx); err != nil {
			a.log.Error("cart sweep failed", "error", err)
		} else if n > 0 {
			a.log.Info("expired carts removed", "carts", n)
		}
		if _, err := a.orders.SweepUnpaid(ctx); err != nil {
			a.log.Error("unpaid order sweep failed", "error", err)
		}
	}

	sweep()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweep()
		}
	}
}

// nudgeOutbox asks the dispatcher to look now rather than at its next poll, so
// an event written during a request is usually delivered within milliseconds.
func (a *App) nudgeOutbox() {
	if a.outbox != nil {
		a.outbox.nudge()
	}
}

// translateProducts applies a registered translator to a page of products.
// With no translator, or when the request is in the store's own language, it
// does nothing at all — a single-language store pays nothing for i18n.
func (a *App) translateProducts(r *http.Request, products []*Product) {
	if a.translator == nil || len(products) == 0 {
		return
	}
	lang := a.RequestLanguage(r)
	if lang == a.cfg.DefaultLanguage {
		return
	}
	ids := make([]int64, 0, len(products))
	for _, p := range products {
		ids = append(ids, p.ID)
	}
	overrides, err := a.translator.Translate(r.Context(), lang, KindProduct, ids)
	if err != nil {
		// A translation failure degrades to the default language rather than
		// failing the request: a shopper would rather read English than an
		// error page.
		a.log.Error("translator failed", "language", lang, "error", err)
		return
	}
	for _, p := range products {
		fields := overrides[p.ID]
		if v, ok := fields["title"]; ok && v != "" {
			p.Title = v
		}
		if v, ok := fields["description"]; ok && v != "" {
			p.Description = v
		}
	}
}

func (a *App) routeCount(owner string) int {
	n := 0
	for _, r := range a.routes {
		if r.Owner == owner {
			n++
		}
	}
	return n
}

// migrate applies core migrations first, then each module's in the order the
// modules were passed to New. If module B needs module A's tables, the
// application author lists A first: explicit wiring extends to schema.
func (a *App) migrate(ctx context.Context) error {
	return runMigrations(ctx, a.db, a.log, a.migrationSets())
}

// migrationSets is the full migration plan, in application order. It is shared
// with Diagnose so that "what should be applied" has exactly one definition —
// a doctor that computed the plan separately could disagree with the migrator
// and report a healthy schema that is not.
func (a *App) migrationSets() []migrationSet {
	plan := []migrationSet{{Owner: coreMigrationOwner, Migrations: coreMigrations()}}
	for _, m := range a.modules {
		if ms := m.Migrations(); len(ms) > 0 {
			plan = append(plan, migrationSet{Owner: m.Name(), Migrations: ms})
		}
	}
	return plan
}

// DB returns the engine's database handle. Modules may create and own their
// own tables through it; they may not mutate core commerce tables directly —
// every business state transition goes through a core service so that the
// state change and its durable event commit together.
func (a *App) DB() *sql.DB { return a.db }

// Log returns a logger scoped to the calling module.
func (a *App) Log() *slog.Logger {
	if a.current == "" {
		return a.log
	}
	return a.log.With("module", a.current)
}

// Config returns a copy of the effective configuration.
func (a *App) Config() Config { return a.cfg }

// Routes returns every mounted route. The OpenAPI coverage test uses this to
// prove the served contract has not drifted from the code.
func (a *App) Routes() []Route {
	out := make([]Route, len(a.routes))
	copy(out, a.routes)
	return out
}

// Handler returns the fully wrapped HTTP handler: panic recovery, request
// logging and language negotiation, outermost first.
//
// Request body limits are deliberately not middleware. A single outer
// MaxBytesReader cannot be widened again further in, which would cap CSV
// import and module webhooks at the JSON limit; instead each handler applies
// its own limit at the point it reads the body (see [DecodeJSON]).
func (a *App) Handler() http.Handler {
	var h http.Handler = a.mux
	h = a.fallbackJSON(h)
	h = a.languageMW(h)
	h = a.logMW(h)
	h = a.recoverMW(h)
	return h
}

// ListenAndServe runs OnStart hooks, serves until SIGINT/SIGTERM, then runs
// OnStop hooks in reverse order and closes the database. It returns nil after
// a clean shutdown, so a caller may treat any non-nil error as fatal.
func (a *App) ListenAndServe() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	for _, h := range a.onStart {
		if err := h.fn(ctx); err != nil {
			return fmt.Errorf("gocommerce: OnStart (%s): %w", h.owner, err)
		}
	}

	a.srv = &http.Server{
		Addr:         a.cfg.Addr,
		Handler:      a.Handler(),
		ReadTimeout:  httpReadTimeout,
		WriteTimeout: httpWriteTimeout,
		IdleTimeout:  httpIdleTimeout,
	}

	// Serving with no admin token is the one configuration that silently leaves
	// the admin API open, and it is reachable by forgetting a flag. Say so
	// loudly here — this is the moment it starts to matter.
	if a.cfg.Dev && len(a.cfg.AdminTokens) == 0 {
		a.log.Warn("serving in dev mode with no admin token: the admin API is unprotected",
			"fix", "pass -admin-token, or create a superuser and sign in to the panel")
	}

	errc := make(chan error, 1)
	go func() {
		a.log.Info("gocommerce listening",
			"addr", a.cfg.Addr, "version", Version,
			"currency", a.cfg.Currency, "language", a.cfg.DefaultLanguage,
			"modules", len(a.modules), "routes", len(a.routes), "dev", a.cfg.Dev)
		if err := a.srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
			return
		}
		errc <- nil
	}()

	select {
	case err := <-errc:
		a.shutdownHooks(context.Background())
		a.db.Close()
		return err
	case <-ctx.Done():
		a.log.Info("shutdown signal received")
	}

	sctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()

	err := a.srv.Shutdown(sctx)
	a.shutdownHooks(sctx)
	if cerr := a.db.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		a.log.Info("shutdown complete")
	}
	return err
}

func (a *App) shutdownHooks(ctx context.Context) {
	for i := len(a.onStop) - 1; i >= 0; i-- {
		if err := a.onStop[i].fn(ctx); err != nil {
			a.log.Error("OnStop hook failed", "owner", a.onStop[i].owner, "error", err)
		}
	}
}

// Close releases resources without serving. Tests use it; ListenAndServe does
// its own cleanup.
func (a *App) Close() error {
	a.shutdownHooks(context.Background())
	return a.db.Close()
}

package gocommerce

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

// Module is the single unit of extension. A module is an ordinary Go value,
// constructed with its own configuration and passed to [New]:
//
//	app, err := gocommerce.New(cfg, stripe.New(stripe.Config{...}))
//
// Modules integrate external capabilities: payment gateways, carriers,
// notification vendors, search indexes, object storage, invoicing, agents.
// They do not own core commerce state transitions.
type Module interface {
	// Name is unique within an app and matches [a-z0-9-]+. It namespaces the
	// module's migrations, its tables (by convention, "<name>_" with dashes
	// as underscores) and its routes (enforced, see [App.Handle]).
	Name() string
	// Migrations returns the module's schema migrations, applied after core's
	// and in the order the modules were passed to New. Return nil if the
	// module owns no tables.
	Migrations() []Migration
	// Register wires the module: routes, providers, subscriptions, lifecycle
	// hooks. It is called exactly once, after every migration has been
	// applied, so a module may assume its own tables exist.
	Register(app *App) error
}

var moduleNameRE = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

func validateModules(mods []Module) error {
	seen := make(map[string]bool, len(mods))
	for i, m := range mods {
		if m == nil {
			return fmt.Errorf("gocommerce: module at index %d is nil", i)
		}
		name := m.Name()
		if !moduleNameRE.MatchString(name) {
			return fmt.Errorf("gocommerce: invalid module name %q: want lowercase letters, digits and single dashes", name)
		}
		if seen[name] {
			return fmt.Errorf("gocommerce: duplicate module name %q", name)
		}
		seen[name] = true
	}
	return nil
}

// Handle mounts a public route. Modules are confined to their own namespace:
// the path must be "/x/<module>" or start with "/x/<module>/". The engine
// enforces this against the module currently being registered rather than
// against a name the module passes in, so a module cannot squat on a core
// route or on another module's namespace even deliberately.
//
// The pattern is a complete net/http ServeMux pattern including the method,
// for example "POST /x/stripe/callback". The engine validates the prefix; it
// never rewrites the path, so what a module writes is what gets served.
func (a *App) Handle(pattern string, h http.Handler) {
	a.mount(pattern, h, false)
}

// HandleAdmin mounts an admin route under "/api/admin/x/<module>/", wrapped
// in the admin authentication middleware. A module never has to remember to
// authenticate: choosing this method is the authentication.
// A route may name the rights it needs. Naming none means "any authenticated
// admin", which is what every route meant before roles existed and what a
// module gets unless it says otherwise.
func (a *App) HandleAdmin(pattern string, h http.Handler, rights ...Right) {
	a.mount(pattern, h, true, rights...)
}

// HandleFunc is Handle for a plain function.
func (a *App) HandleFunc(pattern string, h http.HandlerFunc) { a.Handle(pattern, h) }

// HandleAdminFunc is HandleAdmin for a plain function.
func (a *App) HandleAdminFunc(pattern string, h http.HandlerFunc, rights ...Right) {
	a.HandleAdmin(pattern, h, rights...)
}

func (a *App) mount(pattern string, h http.Handler, admin bool, rights ...Right) {
	owner := a.current
	if owner == "" {
		owner = coreMigrationOwner
	}

	method, path, err := splitPattern(pattern)
	if err != nil {
		a.regErrf("%s: %w", owner, err)
		return
	}
	if err := a.checkNamespace(path, admin); err != nil {
		a.regErrf("%w", err)
		return
	}

	if admin {
		// Rights first in the chain as written, which means they are checked
		// second: the wrapper closest to the handler runs last. Authentication
		// has to answer "who" before anything can ask "may they".
		if len(rights) > 0 {
			h = requireRights(rights...)(h)
		}
		h = a.adminAuth()(h)
	}
	a.mux.Handle(pattern, h)
	a.routes = append(a.routes, Route{
		Method:  method,
		Path:    path,
		Admin:   admin,
		Owner:   owner,
		Pattern: pattern,
		Rights:  rights,
	})
}

// checkNamespace enforces the module route fence. Core wiring (current == "")
// is unrestricted; it runs first, so core routes always exist before any
// module can conflict with one.
func (a *App) checkNamespace(path string, admin bool) error {
	if a.current == "" {
		return nil
	}
	want := "/x/" + a.current
	if admin {
		want = "/api/admin/x/" + a.current
	}
	if path == want || strings.HasPrefix(path, want+"/") {
		return nil
	}
	kind := "public"
	if admin {
		kind = "admin"
	}
	return fmt.Errorf("module %q mounted %s route %q outside its namespace %q/", a.current, kind, path, want)
}

var knownMethods = map[string]bool{
	http.MethodGet: true, http.MethodHead: true, http.MethodPost: true,
	http.MethodPut: true, http.MethodPatch: true, http.MethodDelete: true,
	http.MethodOptions: true,
}

// splitPattern splits a ServeMux pattern into its method and path. Host
// patterns are rejected: a commerce API served on several hostnames should
// resolve that at the proxy, and allowing them here would make the namespace
// fence unenforceable.
func splitPattern(pattern string) (method, path string, err error) {
	p := strings.TrimSpace(pattern)
	if p == "" {
		return "", "", fmt.Errorf("empty route pattern")
	}
	if i := strings.IndexByte(p, ' '); i >= 0 {
		method, path = p[:i], strings.TrimSpace(p[i+1:])
		if !knownMethods[method] {
			return "", "", fmt.Errorf("route pattern %q: unknown method %q", pattern, method)
		}
	} else {
		path = p
	}
	if !strings.HasPrefix(path, "/") {
		return "", "", fmt.Errorf("route pattern %q: path must be absolute (host patterns are not supported)", pattern)
	}
	return method, path, nil
}

// OnStart registers a hook run after migrations and before the listener
// accepts traffic — the place for a module's long-running goroutines, such as
// a queue consumer.
func (a *App) OnStart(fn func(context.Context) error) {
	a.onStart = append(a.onStart, hook{owner: a.ownerName(), fn: fn})
}

// OnStop registers a shutdown hook. Hooks run in reverse registration order.
func (a *App) OnStop(fn func(context.Context) error) {
	a.onStop = append(a.onStop, hook{owner: a.ownerName(), fn: fn})
}

func (a *App) ownerName() string {
	if a.current == "" {
		return coreMigrationOwner
	}
	return a.current
}

func (a *App) regErrf(format string, args ...any) {
	err := fmt.Errorf(format, args...)
	if a.regErr == nil {
		a.regErr = err
		return
	}
	a.regErr = fmt.Errorf("%w; %w", a.regErr, err)
}

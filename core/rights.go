package gocommerce

import (
	"net/http"
	"sort"
)

// Roles and rights.
//
// D19 keeps core on a simple admin token behind a replaceable middleware seam,
// and names RBAC as the thing that seam was left open for. This is the smallest
// version of it that is honestly useful: three fixed roles over one enumerated
// list of rights.
//
// The list is the point. Every right this engine knows about is declared here
// and nowhere else, and a role is a set drawn from it. That is what let M19 make
// the sets configurable (roles.go) by changing one lookup rather than by finding
// every place a permission is decided. Nothing outside this file may invent a
// right, and roles.go may re-cut the sets but never widen the list.

// Right is one thing an operator may be allowed to do.
type Right string

// The rights, grouped by the part of the store they govern.
//
// They are coarse — one per area a person could plausibly be kept out of, not
// one per button — but no coarser than that, and the test of "coarse enough"
// is whether a real store would ever want the line drawn there. It would: a
// merchandiser who runs discounts has no business editing tax rates, and the
// person who invites operators is not necessarily the person who decides what
// a role means.
//
// The first cut of this list had eight rights and drew none of those lines.
// Discounts were read with catalog.read and written with catalog.write; tax
// rates were read with catalog.read but written with settings.write; and
// settings.write alone covered the team, the roles matrix, the locations, the
// tax rates and the export of the whole database. That is not a permission
// system, it is four unrelated jobs sharing one key.
const (
	// ------------------------------------------------------------- catalog

	// RightCatalogRead covers products, variants, categories, collections and
	// media. Reading the catalog is the floor: an operator who cannot see it
	// cannot do anything else either.
	RightCatalogRead  Right = "catalog.read"
	RightCatalogWrite Right = "catalog.write"

	// ----------------------------------------------------------- inventory

	// RightInventoryRead is what is on the shelf: stock levels and the
	// low-stock report. Apart from the catalog because the people who count
	// stock and the people who write listings are rarely the same people.
	RightInventoryRead Right = "inventory.read"
	// RightInventoryWrite is stock takes, adjustments and transfers — the
	// numbers, not the listings.
	RightInventoryWrite Right = "inventory.write"

	// ------------------------------------------------------- merchandising

	// RightDiscountsRead and RightDiscountsWrite are their own pair rather
	// than part of the catalog. A discount is money off, and the account that
	// writes product copy is not automatically the account that may invent a
	// hundred-percent-off code — which is the exact mistake the roles system
	// was introduced to make expressible.
	RightDiscountsRead  Right = "discounts.read"
	RightDiscountsWrite Right = "discounts.write"

	// ------------------------------------------------------- configuration

	// RightTaxesRead and RightTaxesWrite are the tax rates. Writing one
	// changes what every future order collects, which is why it is not filed
	// under the catalog with the prices.
	RightTaxesRead  Right = "taxes.read"
	RightTaxesWrite Right = "taxes.write"

	// RightLocationsRead and RightLocationsWrite are the places stock lives.
	// Writing includes choosing the default, which silently changes where new
	// stock lands.
	RightLocationsRead  Right = "locations.read"
	RightLocationsWrite Right = "locations.write"

	// -------------------------------------------------------------- orders

	// RightOrdersRead and RightOrdersWrite split at the point where an order
	// changes: editing, placing, cancelling and settling payment are writes.
	RightOrdersRead  Right = "orders.read"
	RightOrdersWrite Right = "orders.write"
	// RightOrdersFulfill is moving the goods — creating and amending
	// fulfillments. A warehouse account needs this and nothing else on an
	// order.
	RightOrdersFulfill Right = "orders.fulfill"
	// RightOrdersRefund is separate from the rest of an order's lifecycle
	// because it is the one that sends money back out of the store.
	RightOrdersRefund Right = "orders.refund"

	// RightCustomersRead is the orders grouped by who placed them, which is
	// personal data and so is named separately from the orders themselves.
	RightCustomersRead Right = "customers.read"

	// -------------------------------------------------------------- access

	// RightTeamRead is who is on the team, and who has been invited and has
	// not arrived yet.
	RightTeamRead Right = "team.read"
	// RightTeamWrite is inviting, creating, editing and removing operators,
	// changing their role and ending their sessions. It can hand somebody the
	// owner role, so it can hand somebody everything.
	RightTeamWrite Right = "team.write"
	// RightRolesWrite is the matrix itself — not who holds a role, but what
	// holding it means. Apart from team.write because they are different
	// powers: one staffs the shop, the other rewrites the rules it is staffed
	// under.
	RightRolesWrite Right = "roles.write"

	// ---------------------------------------------------------------- data

	// RightDataExport is the whole catalog, or every order, as a file, out of
	// the building. A read right with the reach of a database dump.
	RightDataExport Right = "data.export"
	// RightDataImport is the same door inwards: one bad file changes every
	// price faster than any screen could.
	RightDataImport Right = "data.import"
)

// AllRights is every right, in the order the panel renders them. Used by the
// roles below, by the panel when it asks what it may do, and by the tests that
// keep the two honest.
var AllRights = []Right{
	RightCatalogRead, RightCatalogWrite,
	RightInventoryRead, RightInventoryWrite,
	RightDiscountsRead, RightDiscountsWrite,
	RightTaxesRead, RightTaxesWrite,
	RightLocationsRead, RightLocationsWrite,
	RightOrdersRead, RightOrdersWrite, RightOrdersFulfill, RightOrdersRefund,
	RightCustomersRead,
	RightTeamRead, RightTeamWrite, RightRolesWrite,
	RightDataExport, RightDataImport,
}

// The roles. Fixed, and few: a store with three people does not need a
// permission editor, and a store that does needs one designed rather than
// grown. What each of them *carries* is the store's to change — see roles.go.
const (
	// RoleOwner can do everything, including deciding who else can. Every
	// existing operator is one, because until now everyone was.
	RoleOwner = "owner"
	// RoleManager runs the shop: the catalog, the orders, the money going back
	// out. They cannot change the store's configuration or the team, which is
	// what separates running the shop from owning it.
	RoleManager = "manager"
	// RoleStaff works the orders: they can see what is being sold and move an
	// order along, and they cannot send money out, change prices, or alter who
	// has access.
	RoleStaff = "staff"
)

// Roles is every role, in the order they are worth showing: most able first.
var Roles = []string{RoleOwner, RoleManager, RoleStaff}

// roleRights is the default permission model: what each role carries in a store
// that has never said otherwise.
//
// It is no longer the last word. A store may re-cut manager and staff through
// `role_rights` (roles.go), and the effective set is what every request is
// judged against. This map stays the seed: a role with no stored override
// tracks it, so widening a default reaches every store that never touched it.
//
// The sets below are what the eight-right version granted, spelled out against
// the finer list — growing the list changed nobody's access. Staff can still
// see tax rates, locations and discount codes because staff could always see
// them; the difference is that a store can now say otherwise, and the defaults
// do not assume it wants to.
var roleRights = map[string][]Right{
	RoleOwner: AllRights,
	RoleManager: {
		RightCatalogRead, RightCatalogWrite,
		RightInventoryRead, RightInventoryWrite,
		RightDiscountsRead, RightDiscountsWrite,
		RightTaxesRead,
		RightLocationsRead,
		RightOrdersRead, RightOrdersWrite, RightOrdersFulfill, RightOrdersRefund,
		RightCustomersRead,
	},
	RoleStaff: {
		RightCatalogRead,
		RightInventoryRead,
		RightDiscountsRead,
		RightTaxesRead,
		RightLocationsRead,
		RightOrdersRead, RightOrdersWrite, RightOrdersFulfill,
		RightCustomersRead,
	},
}

// ValidRole reports whether a role is one this engine knows.
func ValidRole(role string) bool {
	_, ok := roleRights[role]
	return ok
}

// DefaultRightsOf returns what a role carries before the store has changed
// anything, sorted so the answer is stable enough to compare and to show. An
// unknown role gets nothing rather than everything, which is the safe direction
// to be wrong in.
//
// This is the default and not the answer. What an operator may actually do is
// `app.Roles().Of(ctx, role)`, which applies the store's overrides — the name
// here is long precisely so that reaching for it by accident is hard.
func DefaultRightsOf(role string) []Right {
	rights := append([]Right(nil), roleRights[role]...)
	sort.Slice(rights, func(i, j int) bool { return rights[i] < rights[j] })
	return rights
}

// DefaultCan reports whether a role carries a right by default. To ask what a
// signed-in operator may do, use their resolved rights: (*Superuser).Has.
func DefaultCan(role string, right Right) bool {
	for _, r := range roleRights[role] {
		if r == right {
			return true
		}
	}
	return false
}

// RoleConfigurable reports whether a store may re-cut a role.
//
// Owner cannot be, and that is the whole of the safety net under this feature:
// an owner always holds every right, so a store that configures itself into a
// corner still has somebody who can configure it back out. `role_rights` will
// not even store a row for it.
func RoleConfigurable(role string) bool {
	return ValidRole(role) && role != RoleOwner
}

// RequiredRights is the floor: what every role keeps, however it is cut.
//
// Only catalog.read, and for the reason already given above — an operator who
// cannot see the catalog cannot do anything else either, so a role stripped
// past this point is not a narrower role, it is an account that can sign in and
// see nothing. Removing the person says that honestly; a role that grants
// nothing says it by accident.
//
// It also makes the storage unambiguous: no rows for a role means "tracking the
// defaults" and can never also mean "customised down to nothing".
var RequiredRights = []Right{RightCatalogRead}

// ------------------------------------------------------------- enforcement

// requireRights refuses a request whose operator does not carry every right the
// route asked for.
//
// A static admin token carries all of them. It is the bootstrap credential and
// the one scripts use — seed.ps1, smoke.ps1, a cron job — and it has no person
// behind it to hold a role. Narrowing what a script may do is a matter of not
// giving it the token, which is a decision about the token rather than about
// the route.
//
// It runs after authentication, so a request that reaches it has already been
// identified. No superuser on the context therefore means the static token
// authenticated it.
func requireRights(rights ...Right) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			su := SuperuserFrom(r.Context())
			if su == nil {
				next.ServeHTTP(w, r)
				return
			}
			for _, right := range rights {
				// The operator's own resolved set, not the role's defaults:
				// authentication already applied the store's overrides, so this
				// costs nothing and cannot disagree with what the panel was told.
				if !su.Has(right) {
					// Which right, by name: an operator told only "forbidden"
					// has to guess, and the person who can fix it — whoever set
					// the role — needs to know what to grant.
					RespondError(w, r, Forbiddenf(
						"your role (%s) does not carry %s", su.Role, right))
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

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
// and nowhere else, and a role is a set drawn from it — so adding operator-
// defined roles later is a matter of storing sets somewhere instead of reading
// them from `roleRights`, not of finding every place a permission is decided.
// Nothing outside this file may invent a right.

// Right is one thing an operator may be allowed to do.
type Right string

// The rights, one per thing the panel gates. They are coarse on purpose: a
// right per button is a permission system nobody can hold in their head, and
// the store has no use for "may edit a barcode but not a weight".
const (
	// RightCatalogRead covers products, variants, categories, collections and
	// media. Reading the catalog is the floor: an operator who cannot see it
	// cannot do anything else either.
	RightCatalogRead  Right = "catalog.read"
	RightCatalogWrite Right = "catalog.write"

	// RightOrdersRead and RightOrdersWrite split at the point where an order
	// changes: fulfilling, editing and placing are writes.
	RightOrdersRead  Right = "orders.read"
	RightOrdersWrite Right = "orders.write"
	// RightOrdersRefund is separate from the rest of an order's lifecycle
	// because it is the one that sends money back out of the store.
	RightOrdersRefund Right = "orders.refund"

	// RightInventoryWrite is stock takes and adjustments. Reading stock comes
	// with the catalog, since it is on the variant.
	RightInventoryWrite Right = "inventory.write"

	// RightCustomersRead is the orders grouped by who placed them, which is
	// personal data and so is named separately from the orders themselves.
	RightCustomersRead Right = "customers.read"

	// RightSettingsWrite is the store's own configuration, the team, and the
	// import and export of everything. It is the right that can grant rights.
	RightSettingsWrite Right = "settings.write"
)

// AllRights is every right, in a stable order. Used by the roles below, by the
// panel when it asks what it may do, and by the tests that keep the two honest.
var AllRights = []Right{
	RightCatalogRead, RightCatalogWrite,
	RightOrdersRead, RightOrdersWrite, RightOrdersRefund,
	RightInventoryWrite,
	RightCustomersRead,
	RightSettingsWrite,
}

// The roles. Fixed, and few: a store with three people does not need a
// permission editor, and a store that does needs one designed rather than
// grown.
const (
	// RoleOwner can do everything, including deciding who else can. Every
	// existing operator is one, because until now everyone was.
	RoleOwner = "owner"
	// RoleManager runs the shop: the catalog, the orders, the money going back
	// out. They cannot change the store's settings or the team, which is what
	// separates running the shop from owning it.
	RoleManager = "manager"
	// RoleStaff works the orders: they can see what is being sold and move an
	// order along, and they cannot send money out, change prices, or alter who
	// has access.
	RoleStaff = "staff"
)

// Roles is every role, in the order they are worth showing: most able first.
var Roles = []string{RoleOwner, RoleManager, RoleStaff}

// roleRights is the whole permission model.
//
// When custom roles arrive, this map becomes the seed of a table and the lookup
// below reads that table instead. Nothing else has to move, which is the reason
// it is expressed as data rather than as conditionals.
var roleRights = map[string][]Right{
	RoleOwner: AllRights,
	RoleManager: {
		RightCatalogRead, RightCatalogWrite,
		RightOrdersRead, RightOrdersWrite, RightOrdersRefund,
		RightInventoryWrite,
		RightCustomersRead,
	},
	RoleStaff: {
		RightCatalogRead,
		RightOrdersRead, RightOrdersWrite,
		RightCustomersRead,
	},
}

// ValidRole reports whether a role is one this engine knows.
func ValidRole(role string) bool {
	_, ok := roleRights[role]
	return ok
}

// RightsOf returns what a role may do, sorted so the answer is stable enough to
// compare and to show. An unknown role gets nothing rather than everything,
// which is the safe direction to be wrong in.
func RightsOf(role string) []Right {
	rights := append([]Right(nil), roleRights[role]...)
	sort.Slice(rights, func(i, j int) bool { return rights[i] < rights[j] })
	return rights
}

// Can reports whether a role carries a right.
func Can(role string, right Right) bool {
	for _, r := range roleRights[role] {
		if r == right {
			return true
		}
	}
	return false
}

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
				if !Can(su.Role, right) {
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

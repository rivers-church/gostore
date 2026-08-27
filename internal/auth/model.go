package auth

import (
	"fmt"
	"slices"
	"time"
)

// Role is what an administrator is allowed to do, as one word on their row.
//
// The four follow Stripe's dashboard split — Super Administrator, Administrator,
// Support Specialist, View only — reduced to the surface this store actually has.
// A capability set per role, rather than per user, is what keeps the admin UI to
// one select box instead of a permissions matrix nobody maintains.
type Role string

const (
	// RoleOwner is RoleAdmin plus one property: it is the role the
	// last-account guard protects, so an owner cannot be disabled or demoted
	// while they are the only enabled one. Splitting it out makes "who can
	// never be locked out" a visible fact about the row rather than something
	// emergent from counting.
	RoleOwner Role = "owner"
	// RoleAdmin can do everything, including managing other accounts.
	RoleAdmin Role = "admin"
	// RoleManager runs the shop — catalog, files, orders, entitlements — and
	// cannot reach the accounts pages. WooCommerce's shop_manager.
	RoleManager Role = "manager"
	// RoleViewer can read every admin page and change nothing. Stripe's
	// "View only".
	RoleViewer Role = "viewer"
)

// Roles is every role, in descending order of privilege — which is the order a
// select box should offer them in, so the list and the UI cannot disagree.
var Roles = []Role{RoleOwner, RoleAdmin, RoleManager, RoleViewer}

// Valid reports whether r is one of the four. The schema's CHECK constraint says
// the same thing, but a form should answer with a message on the field rather
// than a constraint violation turned into a 500.
func (r Role) Valid() bool {
	return slices.Contains(Roles, r)
}

// Label is the role's name for a human, since the stored value is lower case and
// a select box should not be.
func (r Role) Label() string {
	switch r {
	case RoleOwner:
		return "Owner"
	case RoleAdmin:
		return "Administrator"
	case RoleManager:
		return "Manager"
	case RoleViewer:
		return "Viewer"
	default:
		return string(r)
	}
}

// Permission is one thing a role may do. Handlers name the permission a route
// needs on the line that registers it, so authorisation travels with the route
// the way authentication already does.
type Permission string

const (
	// PermRead is every admin page that only displays. Every role holds it, so
	// a route naming it is saying "any signed-in administrator".
	PermRead Permission = "read"
	// PermCatalogWrite covers products, variants, categories, images and the
	// downloadable files attached to a product.
	PermCatalogWrite Permission = "catalog.write"
	// PermOrdersWrite is revoking and restoring entitlements. Orders
	// themselves stay read-only for everyone: an order records something that
	// happened, and only an authenticated gateway notification may change one.
	PermOrdersWrite Permission = "orders.write"
	// PermUsersWrite is the accounts pages — creating administrators,
	// disabling them, changing roles and resetting passwords.
	PermUsersWrite Permission = "users.write"
)

// permissions is the whole authorisation model: a static map, not a table.
//
// A permissions table would put a join on every request to buy configurability
// nobody has asked for, and would let a deployment invent a role this code has
// never heard of. Adding a permission here is a compile-time change reviewed with
// the routes it gates.
var permissions = map[Role]map[Permission]bool{
	RoleOwner: {
		PermRead: true, PermCatalogWrite: true, PermOrdersWrite: true, PermUsersWrite: true,
	},
	RoleAdmin: {
		PermRead: true, PermCatalogWrite: true, PermOrdersWrite: true, PermUsersWrite: true,
	},
	RoleManager: {
		PermRead: true, PermCatalogWrite: true, PermOrdersWrite: true,
	},
	RoleViewer: {
		PermRead: true,
	},
}

// Can reports whether the role holds a permission. An unknown role holds
// nothing, so a row that somehow escaped the CHECK constraint fails closed.
func (r Role) Can(p Permission) bool { return permissions[r][p] }

// User is one administrator account.
//
// PasswordHash is on the struct because Authenticate and the store need it, and
// leaving it off would mean a second type for the same row. It is never rendered:
// no template reads it, and the admin pages that show a user show Email, Name,
// Role and status.
type User struct {
	ID           string
	Email        string
	Name         string
	PasswordHash string
	Role         Role
	// Disabled accounts keep their history. Deleting the row would erase who did
	// what, so the admin offers disable and never delete.
	Disabled bool
	// MustChangePassword is set when somebody else reset this password, and
	// forces the next sign-in through the change form before any other page
	// opens. Setting your own password never sets it.
	MustChangePassword bool
	CreatedAt          time.Time
	// LastLoginAt is the zero time for an account that has never signed in,
	// which the admin list renders as "Never" rather than as a date in 1970.
	LastLoginAt time.Time
}

// Can is the user's role's answer, with one addition: a disabled account holds
// no permissions at all. Sessions are checked against the account on every
// request, so this is belt and braces rather than the mechanism — but a
// permission check that ignored `disabled` would be the wrong default to leave
// lying around.
func (u User) Can(p Permission) bool { return !u.Disabled && u.Role.Can(p) }

// Display is the name to show, falling back to the address for an account
// created without one.
func (u User) Display() string {
	if u.Name != "" {
		return u.Name
	}
	return u.Email
}

// Session is one live sign-in. Token is never stored: only its sha256 is, so a
// leaked database backup does not hand over anyone's live sessions.
type Session struct {
	UserID    string
	CreatedAt time.Time
	ExpiresAt time.Time
}

// ErrInvalidRole is returned rather than letting the schema's CHECK constraint
// answer, so a bad role reaches a form as a field error.
type ErrInvalidRole struct{ Role Role }

func (e *ErrInvalidRole) Error() string {
	return fmt.Sprintf("auth: %q is not a role", string(e.Role))
}

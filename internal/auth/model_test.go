package auth

import (
	"slices"
	"testing"
)

// Permissions is what tells a template's string from a typo, so a permission
// that exists in the role map but not in that list would be rejected as a
// misspelling wherever it was named.
func TestPermissions_ListsEveryPermissionTheRolesHold(t *testing.T) {
	for role, held := range permissions {
		for perm := range held {
			if !slices.Contains(Permissions, perm) {
				t.Errorf("%s holds %q, which is missing from Permissions", role, perm)
			}
		}
	}
	for _, perm := range Permissions {
		if !slices.ContainsFunc(Roles, func(r Role) bool { return r.Can(perm) }) {
			t.Errorf("%q is held by no role, so no route naming it can ever be reached", perm)
		}
	}
}

func TestRoleCan(t *testing.T) {
	for _, tc := range []struct {
		role Role
		perm Permission
		want bool
	}{
		{RoleOwner, PermUsersWrite, true},
		{RoleAdmin, PermUsersWrite, true},
		{RoleManager, PermUsersWrite, false},
		{RoleManager, PermCatalogWrite, true},
		{RoleViewer, PermCatalogWrite, false},
		{RoleViewer, PermRead, true},
		// A role the CHECK constraint would have rejected, and a permission
		// nothing defines: both hold nothing rather than everything.
		{Role("root"), PermRead, false},
		{RoleOwner, Permission("catalog.destroy"), false},
	} {
		if got := tc.role.Can(tc.perm); got != tc.want {
			t.Errorf("%s.Can(%q) = %v, want %v", tc.role, tc.perm, got, tc.want)
		}
	}

	// A disabled account holds nothing, whatever its role says.
	if (User{Role: RoleOwner, Disabled: true}).Can(PermRead) {
		t.Error("a disabled owner still holds read")
	}
}

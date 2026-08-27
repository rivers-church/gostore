package handler

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/17xande-dev/gostore/internal/auth"
	"github.com/17xande-dev/gostore/internal/catalog"
)

// The account pages. The guards are the subject: what an administrator may do to
// somebody else's account, and the shorter list of what they may do to their own.

const newPassword = "a different long password"

func TestAdminUsers_ListsAccounts(t *testing.T) {
	s := setupShop(t)
	mustAccount(t, s, "manager@example.com", testPassword, auth.RoleManager)
	off := mustAccount(t, s, "gone@example.com", testPassword, auth.RoleViewer)
	if err := s.users.SetDisabled(t.Context(), off.ID, true); err != nil {
		t.Fatalf("SetDisabled: %v", err)
	}

	res, body := get(t, s.srv, "/admin/users")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /admin/users = %d", res.StatusCode)
	}
	for _, want := range []string{
		testEmail, "manager@example.com", "Manager", "Viewer",
		// A disabled account is listed, because this page is also how it is
		// switched back on.
		"gone@example.com", "Disabled",
		// The owner signed in during this test has, so only the others are Never.
		"Never",
		"/admin/users/" + off.ID + "/edit",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the list is missing %q", want)
		}
	}
}

func TestAdminUsers_CreateStoresTheAccountAndForcesAPasswordChange(t *testing.T) {
	s := setupShop(t)

	res, body := post(t, s.srv, "/admin/users", url.Values{
		// Display-name form and mixed case: normalised before validation, or the
		// address check rejects the space and the account is never created.
		"email":            {"Ada Lovelace <Ada@Example.COM>"},
		"name":             {"Ada"},
		"role":             {"manager"},
		"password":         {newPassword},
		"password_confirm": {newPassword},
	})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("create = %d %s", res.StatusCode, body)
	}
	if got := res.Header.Get("Location"); got != "/admin/users?notice=created" {
		t.Errorf("Location = %q", got)
	}

	u, err := s.users.GetByEmail(t.Context(), "ada@example.com")
	if err != nil {
		t.Fatalf("the account was not created: %v", err)
	}
	if u.Email != "Ada@Example.COM" && u.Email != "ada@example.com" {
		t.Errorf("stored email = %q, want the addr-spec without the display name", u.Email)
	}
	if u.Role != auth.RoleManager {
		t.Errorf("role = %q", u.Role)
	}
	// Somebody else chose this password and knows it, so it is a temporary one.
	if !u.MustChangePassword {
		t.Error("a created account is not asked to choose its own password")
	}

	// And the notice is the fixed text for the code, not the code.
	_, body = get(t, s.srv, "/admin/users?notice=created")
	if !strings.Contains(body, userNotices["created"]) {
		t.Error("the created notice does not render")
	}
	// Anything else in the parameter renders nothing at all.
	_, body = get(t, s.srv, "/admin/users?notice=%3Cb%3Ehello%3C/b%3E")
	if strings.Contains(body, "hello") {
		t.Error("the notice parameter is echoed back into the page")
	}
}

func TestAdminUsers_CreateRefusesBadInput(t *testing.T) {
	s := setupShop(t)
	mustAccount(t, s, "taken@example.com", testPassword, auth.RoleViewer)

	for _, tc := range []struct {
		name  string
		form  url.Values
		field string
	}{
		{"duplicate address", url.Values{
			"email": {"TAKEN@example.com"}, "role": {"viewer"},
			"password": {newPassword}, "password_confirm": {newPassword},
		}, "already exists"},
		{"short password", url.Values{
			"email": {"new@example.com"}, "role": {"viewer"},
			"password": {"short"}, "password_confirm": {"short"},
		}, "at least"},
		{"mismatched passwords", url.Values{
			"email": {"new@example.com"}, "role": {"viewer"},
			"password": {newPassword}, "password_confirm": {newPassword + "!"},
		}, "do not match"},
		{"unknown role", url.Values{
			"email": {"new@example.com"}, "role": {"root"},
			"password": {newPassword}, "password_confirm": {newPassword},
		}, "one of the roles"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, body := post(t, s.srv, "/admin/users", tc.form)
			if res.StatusCode != http.StatusUnprocessableEntity {
				t.Fatalf("create = %d, want 422", res.StatusCode)
			}
			if !strings.Contains(body, tc.field) {
				t.Errorf("the form does not say %q", tc.field)
			}
			// Whatever else it carries back, never the password.
			if strings.Contains(body, newPassword) {
				t.Error("the rejected form carries the password back into the page")
			}
		})
	}

	if n, err := s.users.List(t.Context()); err != nil || len(n) != 2 {
		t.Errorf("List = %d accounts, want the owner and the taken one", len(n))
	}
}

func TestAdminUsers_RoleChangeEndsTheirSessions(t *testing.T) {
	s := setupShop(t)
	target := mustAccount(t, s, "manager@example.com", testPassword, auth.RoleManager)
	if _, _, err := s.users.IssueSession(t.Context(), target.ID, time.Hour); err != nil {
		t.Fatalf("IssueSession: %v", err)
	}

	res, body := post(t, s.srv, "/admin/users/"+target.ID+"/role", url.Values{"role": {"viewer"}})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("role change = %d %s", res.StatusCode, body)
	}
	if got, want := res.Header.Get("Location"), "/admin/users/"+target.ID+"/edit?notice=role_changed"; got != want {
		t.Errorf("Location = %q, want %q", got, want)
	}

	after, err := s.users.Get(t.Context(), target.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if after.Role != auth.RoleViewer {
		t.Errorf("role = %q, want viewer", after.Role)
	}
	// A privilege change signs them out, so the new role is what they come back
	// with rather than something that takes effect whenever they reload.
	if n, err := s.users.CountSessionsForUser(t.Context(), target.ID); err != nil || n != 0 {
		t.Errorf("sessions after a role change = %d, want 0", n)
	}
}

func TestAdminUsers_RoleChangeRefusesAnUnknownRoleAndReportsANoOp(t *testing.T) {
	s := setupShop(t)
	target := mustAccount(t, s, "manager@example.com", testPassword, auth.RoleManager)
	if _, _, err := s.users.IssueSession(t.Context(), target.ID, time.Hour); err != nil {
		t.Fatalf("IssueSession: %v", err)
	}

	// Not a role: a bad submission, so 422 with the message on the field, the
	// same answer the create form gives — not the 409 the guards use.
	res, body := post(t, s.srv, "/admin/users/"+target.ID+"/role", url.Values{"role": {"root"}})
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("role=root = %d, want 422", res.StatusCode)
	}
	if !strings.Contains(body, "one of the roles listed") {
		t.Error("the refusal does not name the field")
	}

	// The role they already have. The store does not end their sessions for a
	// no-op, so the page must not claim it did.
	res, _ = post(t, s.srv, "/admin/users/"+target.ID+"/role", url.Values{"role": {"manager"}})
	if got, want := res.Header.Get("Location"), "/admin/users/"+target.ID+"/edit?notice=role_unchanged"; got != want {
		t.Errorf("Location = %q, want %q", got, want)
	}
	if n, _ := s.users.CountSessionsForUser(t.Context(), target.ID); n != 1 {
		t.Errorf("sessions after a no-op role change = %d, want the one they had", n)
	}
	_, body = get(t, s.srv, res.Header.Get("Location"))
	if !strings.Contains(body, userNotices["role_unchanged"]) {
		t.Error("the page does not say that nothing changed")
	}
}

// An id that is not a uuid reaches Postgres as a syntax error, not as "no rows".
// It has to arrive as a 404 rather than a 500 on every one of these routes.
func TestAdminUsers_UnknownAccountIsNotFound(t *testing.T) {
	s := setupShop(t)

	const missing = "3f2504e0-4f89-41d3-9a0c-0305e82c3301"
	for _, id := range []string{"x", missing} {
		if res, _ := get(t, s.srv, "/admin/users/"+id+"/edit"); res.StatusCode != http.StatusNotFound {
			t.Errorf("GET /admin/users/%s/edit = %d, want 404", id, res.StatusCode)
		}
		for path, form := range map[string]url.Values{
			"/role":     {"role": {"viewer"}},
			"/disabled": {"disabled": {"1"}},
			"/password": {"password": {newPassword}, "password_confirm": {newPassword}},
		} {
			res, _ := post(t, s.srv, "/admin/users/"+id+path, form)
			if res.StatusCode != http.StatusNotFound {
				t.Errorf("POST /admin/users/%s%s = %d, want 404", id, path, res.StatusCode)
			}
		}
	}
}

func TestAdminUsers_DisableAndEnable(t *testing.T) {
	s := setupShop(t)
	target := mustAccount(t, s, "manager@example.com", testPassword, auth.RoleManager)
	if _, _, err := s.users.IssueSession(t.Context(), target.ID, time.Hour); err != nil {
		t.Fatalf("IssueSession: %v", err)
	}

	res, body := post(t, s.srv, "/admin/users/"+target.ID+"/disabled", url.Values{"disabled": {"1"}})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("disable = %d %s", res.StatusCode, body)
	}
	after, err := s.users.Get(t.Context(), target.ID)
	if err != nil || !after.Disabled {
		t.Fatalf("account after disable = %+v, %v", after, err)
	}
	if n, _ := s.users.CountSessionsForUser(t.Context(), target.ID); n != 0 {
		t.Errorf("sessions after a disable = %d, want 0", n)
	}
	// The account is still there. Disabling is not deleting.
	if _, err := s.users.GetByEmail(t.Context(), "manager@example.com"); err != nil {
		t.Errorf("the disabled account is gone: %v", err)
	}

	res, _ = post(t, s.srv, "/admin/users/"+target.ID+"/disabled", url.Values{"disabled": {"0"}})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("enable = %d", res.StatusCode)
	}
	if after, _ = s.users.Get(t.Context(), target.ID); after.Disabled {
		t.Error("the account is still disabled after being enabled")
	}
}

// A toggle that read `value == "1"` would make an absent field mean "enable", so
// an empty POST would quietly perform the direction no guard covers.
func TestAdminUsers_DisableNeedsAnExplicitState(t *testing.T) {
	s := setupShop(t)
	target := mustAccount(t, s, "manager@example.com", testPassword, auth.RoleManager)

	for _, form := range []url.Values{{}, {"disabled": {""}}, {"disabled": {"true"}}} {
		res, _ := post(t, s.srv, "/admin/users/"+target.ID+"/disabled", form)
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("POST disabled=%v = %d, want 400", form["disabled"], res.StatusCode)
		}
	}
	if after, _ := s.users.Get(t.Context(), target.ID); after.Disabled {
		t.Error("an unreadable toggle disabled the account anyway")
	}
}

func TestAdminUsers_PasswordResetForcesAChangeAndEndsSessions(t *testing.T) {
	s := setupShop(t)
	target := mustAccount(t, s, "manager@example.com", testPassword, auth.RoleManager)
	if _, _, err := s.users.IssueSession(t.Context(), target.ID, time.Hour); err != nil {
		t.Fatalf("IssueSession: %v", err)
	}

	res, body := post(t, s.srv, "/admin/users/"+target.ID+"/password", url.Values{
		"password": {newPassword}, "password_confirm": {newPassword},
	})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("reset = %d %s", res.StatusCode, body)
	}
	after, err := s.users.Get(t.Context(), target.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !after.MustChangePassword {
		t.Error("a reset password is not marked as one the account must change")
	}
	if n, _ := s.users.CountSessionsForUser(t.Context(), target.ID); n != 0 {
		t.Errorf("sessions after a reset = %d, want 0", n)
	}
	if _, err := s.users.Authenticate(t.Context(), "manager@example.com", newPassword); err != nil {
		t.Errorf("the new password does not work: %v", err)
	}
}

// The three things an administrator may not do to their own account, each for its
// own reason: a reset skips the current-password check, and an account that can
// change its own role or switch itself off is not held by either.
func TestAdminUsers_RefusesChangesToYourOwnAccount(t *testing.T) {
	s := setupShop(t)
	me := s.owner

	for _, tc := range []struct {
		name, path string
		form       url.Values
	}{
		{"role", "/role", url.Values{"role": {"viewer"}}},
		{"disable", "/disabled", url.Values{"disabled": {"1"}}},
		{"password reset", "/password", url.Values{
			"password": {newPassword}, "password_confirm": {newPassword},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, body := post(t, s.srv, "/admin/users/"+me.ID+tc.path, tc.form)
			if res.StatusCode != http.StatusConflict {
				t.Fatalf("%s on self = %d, want 409", tc.name, res.StatusCode)
			}
			if !strings.Contains(body, "your own") {
				t.Errorf("the refusal does not say why: %.200s", body)
			}
		})
	}

	after, err := s.users.Get(t.Context(), me.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if after.Role != auth.RoleOwner || after.Disabled {
		t.Errorf("the account changed anyway: %+v", after)
	}
	// And the page does not offer any of it in the first place.
	_, body := get(t, s.srv, "/admin/users/"+me.ID+"/edit")
	for _, absent := range []string{
		`action="/admin/users/` + me.ID + `/role"`,
		`action="/admin/users/` + me.ID + `/disabled"`,
		`action="/admin/users/` + me.ID + `/password"`,
	} {
		if strings.Contains(body, absent) {
			t.Errorf("your own account page offers %s", absent)
		}
	}
	if !strings.Contains(body, "/admin/password") {
		t.Error("your own account page does not point at the password form")
	}
}

// The last owner who can still sign in. The store refuses both ways of removing
// them; these pages leave the controls out rather than showing a button that
// answers 409.
func TestAdminUsers_RefusesToRemoveTheLastOwner(t *testing.T) {
	s := setupShop(t)
	// A second administrator to act, so the refusal is the last-owner guard
	// rather than the self guard.
	other := mustAccount(t, s, "admin@example.com", testPassword, auth.RoleAdmin)
	signInAs(t, s.srv, "admin@example.com", testPassword)
	owner := s.owner

	res, body := post(t, s.srv, "/admin/users/"+owner.ID+"/role", url.Values{"role": {"manager"}})
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("demote the last owner = %d %s", res.StatusCode, body)
	}
	if !strings.Contains(body, "last owner") {
		t.Error("the refusal does not explain itself")
	}

	res, body = post(t, s.srv, "/admin/users/"+owner.ID+"/disabled", url.Values{"disabled": {"1"}})
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("disable the last owner = %d %s", res.StatusCode, body)
	}

	if after, _ := s.users.Get(t.Context(), owner.ID); after.Role != auth.RoleOwner || after.Disabled {
		t.Errorf("the last owner changed anyway: %+v", after)
	}

	// The page offers neither control while they are the last one...
	_, body = get(t, s.srv, "/admin/users/"+owner.ID+"/edit")
	if strings.Contains(body, `action="/admin/users/`+owner.ID+`/disabled"`) {
		t.Error("the last owner's page offers to disable them")
	}
	// ...and offers both again once there is a second owner.
	if err := s.users.SetRole(t.Context(), other.ID, auth.RoleOwner); err != nil {
		t.Fatalf("SetRole: %v", err)
	}
	signInAs(t, s.srv, "admin@example.com", testPassword)
	_, body = get(t, s.srv, "/admin/users/"+owner.ID+"/edit")
	if !strings.Contains(body, `action="/admin/users/`+owner.ID+`/disabled"`) {
		t.Error("the controls are still hidden once a second owner exists")
	}
}

func TestAdminUsers_NavLinkFollowsThePermission(t *testing.T) {
	s := setupShop(t)
	if _, body := get(t, s.srv, "/admin/products"); !strings.Contains(body, `href="/admin/users"`) {
		t.Error("an owner has no Users link")
	}

	mustAccount(t, s, "manager@example.com", testPassword, auth.RoleManager)
	signInAs(t, s.srv, "manager@example.com", testPassword)
	if _, body := get(t, s.srv, "/admin/products"); strings.Contains(body, `href="/admin/users"`) {
		t.Error("a manager is offered a Users link they cannot open")
	}
	// And the link being absent is not the restriction.
	if res, _ := get(t, s.srv, "/admin/users"); res.StatusCode != http.StatusForbidden {
		t.Errorf("GET /admin/users as a manager = %d, want 403", res.StatusCode)
	}
}

// The catalog and order pages grew roles too: a control a role cannot use is
// left out rather than rendered as a link to a 403.
func TestAdminUsers_ViewerSeesNoWriteControls(t *testing.T) {
	s := setupShop(t)
	p, err := s.catalog.Create(t.Context(), catalog.Product{Slug: "tee", Title: "Sample Tee", Active: true})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := s.catalog.CreateCategory(t.Context(), catalog.Category{Slug: "apparel", Name: "Apparel"}); err != nil {
		t.Fatalf("CreateCategory: %v", err)
	}

	// As the owner, both pages offer the write controls...
	_, products := get(t, s.srv, "/admin/products")
	_, categories := get(t, s.srv, "/admin/categories")
	for _, want := range []string{"New product", "/admin/products/" + p.ID + "/edit"} {
		if !strings.Contains(products, want) {
			t.Errorf("the owner's product list is missing %q", want)
		}
	}
	if !strings.Contains(categories, "New category") {
		t.Error("the owner's category list is missing the New category link")
	}

	// ...and as a viewer, neither does.
	mustAccount(t, s, "viewer@example.com", testPassword, auth.RoleViewer)
	signInAs(t, s.srv, "viewer@example.com", testPassword)
	_, products = get(t, s.srv, "/admin/products")
	_, categories = get(t, s.srv, "/admin/categories")
	for _, absent := range []string{"New product", "/edit"} {
		if strings.Contains(products, absent) {
			t.Errorf("a viewer's product list still offers %q", absent)
		}
	}
	if strings.Contains(categories, "New category") {
		t.Error("a viewer's category list still offers to create one")
	}
	// The product itself is still listed: a viewer may read the catalog.
	if !strings.Contains(products, "Sample Tee") {
		t.Error("a viewer cannot see the products at all")
	}
}

func TestAdminPassword_ChangeYourOwn(t *testing.T) {
	s := setupShop(t)

	// The current password is asked for, and a wrong one is refused even though
	// the session is authenticated and the CSRF token is good.
	res, body := post(t, s.srv, "/admin/password", url.Values{
		"current_password": {"not my password"},
		"password":         {newPassword},
		"password_confirm": {newPassword},
	})
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("wrong current password = %d, want 422", res.StatusCode)
	}
	if !strings.Contains(body, "not your current password") {
		t.Error("the form does not say what was wrong")
	}
	if strings.Contains(body, newPassword) {
		t.Error("the rejected form carries the new password back into the page")
	}

	res, body = post(t, s.srv, "/admin/password", url.Values{
		"current_password": {testPassword},
		"password":         {newPassword},
		"password_confirm": {newPassword},
	})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("change = %d %s", res.StatusCode, body)
	}
	if got := res.Header.Get("Location"); got != "/admin/login?notice=password_changed" {
		t.Errorf("Location = %q", got)
	}

	// Every session, including this one: the admin is closed until they sign in
	// again with the new password.
	if n, _ := s.users.CountSessionsForUser(t.Context(), s.owner.ID); n != 0 {
		t.Errorf("sessions after changing your own password = %d, want 0", n)
	}
	if res, _ := get(t, s.srv, "/admin/products"); res.StatusCode != http.StatusSeeOther {
		t.Errorf("still signed in after a password change: %d", res.StatusCode)
	}
	after, err := s.users.Get(t.Context(), s.owner.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	// Never must-change: this one they chose themselves.
	if after.MustChangePassword {
		t.Error("choosing your own password marked it as one you must change")
	}
	// The login page explains why they are there, from the code rather than from
	// the query text — checked while signed out, which is when it is shown.
	_, body = get(t, s.srv, "/admin/login?notice=password_changed")
	if !strings.Contains(body, loginNotices["password_changed"]) {
		t.Error("the login page does not say the password was changed")
	}
	signInAs(t, s.srv, testEmail, newPassword)
}

// The whole forced-change path, from one administrator resetting another's
// password to that account getting back in.
func TestAdminPassword_ForcedChangeAfterAReset(t *testing.T) {
	s := setupShop(t)
	target := mustAccount(t, s, "manager@example.com", testPassword, auth.RoleManager)

	res, _ := post(t, s.srv, "/admin/users/"+target.ID+"/password", url.Values{
		"password": {newPassword}, "password_confirm": {newPassword},
	})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("reset = %d", res.StatusCode)
	}

	signInAs(t, s.srv, "manager@example.com", newPassword)

	// Nothing else opens until they have chosen one.
	res, _ = get(t, s.srv, "/admin/products")
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != passwordPath {
		t.Fatalf("GET /admin/products = %d %q, want a bounce to %s",
			res.StatusCode, res.Header.Get("Location"), passwordPath)
	}
	res, body := get(t, s.srv, passwordPath)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d", passwordPath, res.StatusCode)
	}
	if !strings.Contains(body, "set by another administrator") {
		t.Error("the forced change form does not say why it is being shown")
	}

	const chosen = "a password of their own choosing"
	res, body = post(t, s.srv, "/admin/password", url.Values{
		"current_password": {newPassword},
		"password":         {chosen},
		"password_confirm": {chosen},
	})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("change = %d %s", res.StatusCode, body)
	}

	signInAs(t, s.srv, "manager@example.com", chosen)
	if res, _ := get(t, s.srv, "/admin/products"); res.StatusCode != http.StatusOK {
		t.Errorf("GET /admin/products after choosing a password = %d, want 200", res.StatusCode)
	}
}

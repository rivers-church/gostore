package handler

import (
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/17xande-dev/gostore/internal/auth"
)

// The route sweep and the role matrix both start from what RegisterAdmin
// actually registered, not from a list written beside it. The hand-maintained
// version of this test listed ten of the thirty routes, which is the failure
// mode: a route added later was protected only if somebody remembered two
// places.

func TestAdminRoutes_EveryProtectedRouteRefusesAnonymous(t *testing.T) {
	s := newStore(t)
	routes := s.handler.AdminProtectedRoutes()

	// A derived list that came back empty would let this pass while testing
	// nothing at all. The number is a floor, not a count to keep updated.
	if len(routes) < 25 {
		t.Fatalf("AdminProtectedRoutes returned %d routes; the admin has more than that", len(routes))
	}

	for _, route := range routes {
		path := route.TestPath()
		var res *http.Response
		if route.Method == http.MethodGet {
			res, _ = get(t, s.srv, path)
		} else {
			// A real token, because nosurf's own 403 for a missing one looks
			// exactly like a refusal to an assertion that only checks "not 200"
			// — and would pass with the session check removed entirely.
			res, _ = post(t, s.srv, path, url.Values{"title": {"Sneaky"}})
		}
		if res.StatusCode != http.StatusSeeOther {
			t.Errorf("%s %s = %d, want 303", route.Method, path, res.StatusCode)
			continue
		}
		if got := res.Header.Get("Location"); !strings.HasPrefix(got, "/admin/login") {
			t.Errorf("%s %s redirected to %q, want the login form", route.Method, path, got)
		}
	}
}

// TestAdminRoutes_RolesGetTheirPermissions is the authorisation matrix: one
// representative route per permission, tried as each of the four roles.
//
// An anonymous sweep says nothing about roles — every one of those routes
// refuses everybody — so this is the half that proves the permission named on
// the registration line is the one enforced.
func TestAdminRoutes_RolesGetTheirPermissions(t *testing.T) {
	type permCase struct {
		perm         auth.Permission
		method, path string
		form         url.Values
	}

	cases := []permCase{
		{auth.PermRead, http.MethodGet, "/admin/products", nil},
		{auth.PermCatalogWrite, http.MethodPost, "/admin/products", url.Values{"title": {"A Product"}}},
		{auth.PermCatalogWrite, http.MethodGet, "/admin/products/new", nil},
		{
			auth.PermOrdersWrite, http.MethodPost,
			"/admin/orders/3f2504e0-4f89-41d3-9a0c-0305e82c3301/entitlements/9f2504e0-4f89-41d3-9a0c-0305e82c3301/revoke",
			url.Values{},
		},
	}

	// Nothing may be enforced that this matrix does not cover: a permission
	// introduced with a route and no case here would otherwise be authorised by
	// nobody's assertion.
	s := newStore(t)
	for _, route := range s.handler.AdminProtectedRoutes() {
		if !slices.ContainsFunc(cases, func(c permCase) bool { return c.perm == route.Perm }) {
			t.Errorf("%s %s needs %q, which no case in this matrix exercises", route.Method, route.Pattern, route.Perm)
		}
	}

	for _, role := range auth.Roles {
		t.Run(string(role), func(t *testing.T) {
			s := newStore(t)
			email := string(role) + "-role@example.com"
			mustAccount(t, s, email, testPassword, role)
			signInAs(t, s.srv, email, testPassword)

			for _, tc := range cases {
				var res *http.Response
				if tc.method == http.MethodGet {
					res, _ = get(t, s.srv, tc.path)
				} else {
					res, _ = post(t, s.srv, tc.path, tc.form)
				}

				forbidden := res.StatusCode == http.StatusForbidden
				if want := !role.Can(tc.perm); forbidden != want {
					t.Errorf("%s %s as %s = %d; forbidden = %v, want %v",
						tc.method, tc.path, role, res.StatusCode, forbidden, want)
				}
			}
		})
	}
}

// TestAdminRoutes_UnprotectedRoutesArePinned fixes the routes RegisterAdmin
// mounts outside the closure, because those are the ones with neither a session
// nor a permission in front of them.
//
// A source-text test rather than a behavioural one: what it is guarding against
// is a route added by the other mounting path, which by definition would not
// appear in AdminProtectedRoutes and so cannot be found by asking the handler.
func TestAdminRoutes_UnprotectedRoutesArePinned(t *testing.T) {
	src, err := os.ReadFile("admin.go")
	if err != nil {
		t.Fatalf("read admin.go: %v", err)
	}
	body := registerAdminBody(t, string(src))

	want := []string{
		"GET /admin/login",
		"POST /admin/login",
		"POST /admin/logout",
		"GET /admin/setup",
		"POST /admin/setup",
	}
	var got []string
	for _, m := range regexp.MustCompile(`mux\.Handle(?:Func)?\("([^"]+)"`).FindAllStringSubmatch(body, -1) {
		got = append(got, m[1])
	}
	if !slices.Equal(got, want) {
		t.Errorf("RegisterAdmin mounts %q directly on the mux; the routes outside the\n"+
			"permission closure are meant to be exactly %q. A new one belongs in the\n"+
			"closure with the permission it needs, or in this list with a reason.", got, want)
	}
}

// registerAdminBody is the text of RegisterAdmin, from its signature to the
// closing brace in the first column.
func registerAdminBody(t *testing.T, src string) string {
	t.Helper()

	const sig = "func (h *Handler) RegisterAdmin("
	i := strings.Index(src, sig)
	if i < 0 {
		t.Fatalf("no %s in admin.go", sig)
	}
	rest := src[i:]
	end := strings.Index(rest, "\n}\n")
	if end < 0 {
		t.Fatal("RegisterAdmin has no closing brace in the first column")
	}
	return rest[:end]
}

func TestAdminRoutes_MustChangePasswordBouncesEverything(t *testing.T) {
	s := newStore(t)

	const email = "reset@example.com"
	if _, err := s.users.Create(t.Context(), email, "Reset", cheapHash(t, testPassword), auth.RoleAdmin, true); err != nil {
		t.Fatalf("create the account: %v", err)
	}
	// Taken before signing in, because every page that would carry one is about
	// to redirect to a form that does not exist yet. nosurf validates a token
	// against the client's cookie, which this jar keeps across the sign-in.
	token := csrfToken(t, s.srv)
	signInAs(t, s.srv, email, testPassword)

	// Every route, not a sample: a forced change that let through the pages
	// nobody thought of is decorative.
	for _, route := range s.handler.AdminProtectedRoutes() {
		path := route.TestPath()
		var res *http.Response
		if route.Method == http.MethodGet {
			res, _ = get(t, s.srv, path)
		} else {
			res, _ = post(t, s.srv, path, url.Values{"title": {"Sneaky"}, "csrf_token": {token}})
		}
		if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != passwordPath {
			t.Errorf("%s %s = %d %q, want 303 to %s",
				route.Method, path, res.StatusCode, res.Header.Get("Location"), passwordPath)
		}
	}

	// And signing out still works, or an account in this state could only be
	// left by closing the browser.
	res, _ := post(t, s.srv, "/admin/logout", url.Values{"csrf_token": {token}})
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/admin/login" {
		t.Errorf("sign out = %d %q", res.StatusCode, res.Header.Get("Location"))
	}
}

func TestPageCan(t *testing.T) {
	p := page{User: auth.User{Role: auth.RoleManager}}

	if can, err := p.Can("catalog.write"); err != nil || !can {
		t.Errorf(`Can("catalog.write") as a manager = %v, %v`, can, err)
	}
	if can, err := p.Can("users.write"); err != nil || can {
		t.Errorf(`Can("users.write") as a manager = %v, %v`, can, err)
	}
	// The zero page is every public page: nobody is signed in, so nothing is
	// permitted.
	if can, _ := (page{}).Can("read"); can {
		t.Error(`Can("read") on a page with no user is true`)
	}
	// A misspelling is an error rather than a quiet false, which would hide the
	// thing it guards from everybody and look deliberate.
	if _, err := p.Can("catalog-write"); err == nil {
		t.Error(`Can("catalog-write") returned no error`)
	}
}

func TestAdminRoute_TestPath(t *testing.T) {
	for _, tc := range []struct{ pattern, want string }{
		{"/admin/{$}", "/admin/"},
		{"/admin/products", "/admin/products"},
		{"/admin/users/{id}/edit", "/admin/users/x/edit"},
		{"/admin/products/{id}/files/{fileID}/delete", "/admin/products/x/files/x/delete"},
	} {
		if got := (AdminRoute{Pattern: tc.pattern}).TestPath(); got != tc.want {
			t.Errorf("TestPath(%q) = %q, want %q", tc.pattern, got, tc.want)
		}
	}
}

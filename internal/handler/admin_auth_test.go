package handler

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/17xande-dev/gostore/internal/auth"
)

func TestAdminAuth_RedirectsUnauthenticated(t *testing.T) {
	s := newStore(t)

	// Every protected route, not a sample of them: an unprotected route is the
	// kind of mistake that only shows up when someone finds it.
	//
	// Phase 3 replaces this hand-maintained list with one derived from the route
	// registration itself, so a new route cannot be added without appearing here.
	paths := []struct {
		method, path string
	}{
		{http.MethodGet, "/admin/"},
		{http.MethodGet, "/admin/products"},
		{http.MethodGet, "/admin/products/new"},
		{http.MethodPost, "/admin/products"},
		{http.MethodGet, "/admin/products/3f2504e0-4f89-41d3-9a0c-0305e82c3301/edit"},
		{http.MethodPost, "/admin/products/3f2504e0-4f89-41d3-9a0c-0305e82c3301"},
		{http.MethodPost, "/admin/products/3f2504e0-4f89-41d3-9a0c-0305e82c3301/delete"},
		{http.MethodPost, "/admin/products/3f2504e0-4f89-41d3-9a0c-0305e82c3301/variants"},
		{http.MethodPost, "/admin/products/3f2504e0-4f89-41d3-9a0c-0305e82c3301/variants/9f2504e0-4f89-41d3-9a0c-0305e82c3301"},
		{http.MethodPost, "/admin/products/3f2504e0-4f89-41d3-9a0c-0305e82c3301/variants/9f2504e0-4f89-41d3-9a0c-0305e82c3301/delete"},
	}

	for _, tc := range paths {
		var res *http.Response
		if tc.method == http.MethodGet {
			res, _ = get(t, s.srv, tc.path)
		} else {
			res, _ = post(t, s.srv, tc.path, url.Values{"title": {"Sneaky"}})
		}
		if res.StatusCode != http.StatusSeeOther {
			t.Errorf("%s %s = %d, want 303", tc.method, tc.path, res.StatusCode)
			continue
		}
		if got := res.Header.Get("Location"); !strings.HasPrefix(got, "/admin/login") {
			t.Errorf("%s %s redirected to %q, want the login form", tc.method, tc.path, got)
		}
	}
}

func TestAdminAuth_SignsInWithEmailAndPassword(t *testing.T) {
	s := newStore(t)
	owner := s.owner

	res, body := get(t, s.srv, "/admin/login")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /admin/login = %d", res.StatusCode)
	}
	for _, want := range []string{`name="email"`, `name="password"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the login page has no %s field", want)
		}
	}

	// Mixed case and surrounding space, because both are what a person types and
	// neither should stop them signing in: the address is matched case
	// insensitively by the unique index the store looks it up through.
	res, body = post(t, s.srv, "/admin/login", url.Values{
		"email":    {"  Owner@Example.COM  "},
		"password": {testPassword},
	})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("sign in = %d %s", res.StatusCode, body)
	}
	if got := res.Header.Get("Location"); got != "/admin/products" {
		t.Errorf("Location = %q, want /admin/products", got)
	}

	cookie := sessionCookieFrom(t, res)
	if !cookie.HttpOnly {
		t.Error("the session cookie is not HttpOnly")
	}
	if cookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want Lax", cookie.SameSite)
	}
	// Scoped to /admin, so it is never sent with the cookie-free, embeddable
	// storefront fragments.
	if cookie.Path != "/admin" {
		t.Errorf("Path = %q, want /admin", cookie.Path)
	}

	// The cookie is a key to a row, and the row is what the store knows about.
	sess, user, err := s.users.Session(t.Context(), cookie.Value)
	if err != nil {
		t.Fatalf("the issued token does not resolve: %v", err)
	}
	if sess.UserID != owner.ID || user.Email != testEmail {
		t.Errorf("the session belongs to %+v, want the owner", user)
	}
	// Only the hash is stored, which is the whole reason a leaked backup is not a
	// set of live sessions.
	if n, err := s.users.CountSessionsForUser(t.Context(), owner.ID); err != nil || n != 1 {
		t.Errorf("sessions for the owner = %d (err %v), want 1", n, err)
	}

	// The jar now holds the session, so the protected pages open.
	if res, _ := get(t, s.srv, "/admin/products"); res.StatusCode != http.StatusOK {
		t.Errorf("GET /admin/products after signing in = %d, want 200", res.StatusCode)
	}
}

func TestAdminAuth_RejectsBadCredentials(t *testing.T) {
	s := newStore(t)
	disabled := mustAccount(t, s, "gone@example.com", testPassword, auth.RoleManager)
	if err := s.users.SetDisabled(t.Context(), disabled.ID, true); err != nil {
		t.Fatalf("SetDisabled: %v", err)
	}

	cases := map[string]url.Values{
		"no password":      {"email": {testEmail}},
		"wrong password":   {"email": {testEmail}, "password": {"wrong"}},
		"case-shifted":     {"email": {testEmail}, "password": {strings.ToUpper(testPassword)}},
		"unknown address":  {"email": {"nobody@example.com"}, "password": {testPassword}},
		"no email":         {"password": {testPassword}},
		"disabled account": {"email": {"gone@example.com"}, "password": {testPassword}},
	}
	for name, form := range cases {
		res, body := post(t, s.srv, "/admin/login", form)
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s = %d, want 401", name, res.StatusCode)
		}
		// One message for all six. Which part was wrong is what an attacker
		// enumerating addresses wants to be told, and a disabled account must not
		// answer differently from a nonexistent one.
		if !strings.Contains(body, "That email and password do not match an account.") {
			t.Errorf("%s: no error message on the re-rendered form", name)
		}
		for _, c := range res.Cookies() {
			if c.Name == auth.CookieName && c.Value != "" {
				t.Errorf("%s issued a session cookie", name)
			}
		}
	}

	// And a failed login leaves the admin closed.
	if res, _ := get(t, s.srv, "/admin/products"); res.StatusCode != http.StatusSeeOther {
		t.Errorf("GET /admin/products after failed logins = %d, want 303", res.StatusCode)
	}
}

// A rejected form comes back carrying the address so it does not have to be
// retyped, and never the password.
func TestAdminAuth_RejectedFormKeepsEmailNotPassword(t *testing.T) {
	s := newStore(t)

	_, body := post(t, s.srv, "/admin/login", url.Values{
		"email":    {testEmail},
		"password": {"the wrong password entirely"},
	})
	if !strings.Contains(body, `value="`+testEmail+`"`) {
		t.Error("the rejected form lost the email address")
	}
	if strings.Contains(body, "the wrong password entirely") {
		t.Error("the rejected form carries the password back into the page")
	}
}

func TestAdminAuth_LogoutDeletesTheSessionRow(t *testing.T) {
	s := setupShop(t)
	owner := s.owner

	if res, _ := get(t, s.srv, "/admin/products"); res.StatusCode != http.StatusOK {
		t.Fatal("not signed in at the start of the test")
	}

	res, body := post(t, s.srv, "/admin/logout", nil)
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("logout = %d %s", res.StatusCode, body)
	}
	if got := res.Header.Get("Location"); got != "/admin/login" {
		t.Errorf("Location = %q, want /admin/login", got)
	}
	cookie := sessionCookieFrom(t, res)
	if cookie.Value != "" || cookie.MaxAge >= 0 {
		t.Errorf("logout cookie is %+v, want an empty value and a negative MaxAge", cookie)
	}

	// The half that matters: the row is gone, so a copy of the cookie taken from
	// a debugger or left in a stale tab opens nothing.
	if n, err := s.users.CountSessionsForUser(t.Context(), owner.ID); err != nil || n != 0 {
		t.Errorf("sessions after logout = %d (err %v), want 0", n, err)
	}
	if res, _ := get(t, s.srv, "/admin/products"); res.StatusCode != http.StatusSeeOther {
		t.Errorf("GET /admin/products after logout = %d, want 303", res.StatusCode)
	}
}

// Signing in replaces whatever session the browser arrived with, rather than
// leaving it live alongside the new one. That is the session-fixation case: an
// attacker who can set a cookie plants a token and waits for the victim to sign
// in with it.
func TestAdminAuth_SignInReplacesAnExistingSession(t *testing.T) {
	s := newStore(t)
	owner := s.owner

	planted, _, err := s.users.IssueSession(t.Context(), owner.ID, time.Hour)
	if err != nil {
		t.Fatalf("IssueSession: %v", err)
	}
	req := newRequest(t, s.srv, http.MethodPost, "/admin/login", url.Values{
		"email":    {testEmail},
		"password": {testPassword},
	})
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: planted})
	res, body := do(t, s.srv, req)
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("sign in = %d %s", res.StatusCode, body)
	}

	issued := sessionCookieFrom(t, res)
	if issued.Value == planted {
		t.Fatal("the planted token was kept as the session")
	}
	if _, _, err := s.users.Session(t.Context(), planted); err == nil {
		t.Error("the planted session is still live after the sign-in")
	}
	if _, _, err := s.users.Session(t.Context(), issued.Value); err != nil {
		t.Errorf("the newly issued session does not resolve: %v", err)
	}
}

// A live session stops working on the very next request when the account behind
// it is disabled — which is the entire reason the session moved into a table.
func TestAdminAuth_DisablingAnAccountEndsItsSession(t *testing.T) {
	s := newStore(t)
	manager := mustAccount(t, s, "manager@example.com", testPassword, auth.RoleManager)
	signInAs(t, s.srv, "manager@example.com", testPassword)

	if res, _ := get(t, s.srv, "/admin/products"); res.StatusCode != http.StatusOK {
		t.Fatal("the manager is not signed in at the start of the test")
	}

	if err := s.users.SetDisabled(t.Context(), manager.ID, true); err != nil {
		t.Fatalf("SetDisabled: %v", err)
	}
	if res, _ := get(t, s.srv, "/admin/products"); res.StatusCode != http.StatusSeeOther {
		t.Errorf("GET /admin/products after being disabled = %d, want 303", res.StatusCode)
	}
}

// Likewise for a password change, which is what somebody does after a scare.
func TestAdminAuth_PasswordChangeEndsTheSession(t *testing.T) {
	s := setupShop(t)
	owner := s.owner

	if err := s.users.SetPassword(t.Context(), owner.ID, cheapHash(t, "a different long passphrase"), true); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	if res, _ := get(t, s.srv, "/admin/products"); res.StatusCode != http.StatusSeeOther {
		t.Errorf("GET /admin/products after a password change = %d, want 303", res.StatusCode)
	}
}

func TestAdminAuth_LoginPageSkippedWhenSignedIn(t *testing.T) {
	srv, _ := setup(t)

	res, _ := get(t, srv, "/admin/login")
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("GET /admin/login while signed in = %d, want 303", res.StatusCode)
	}
	if got := res.Header.Get("Location"); got != "/admin/products" {
		t.Errorf("Location = %q, want /admin/products", got)
	}
}

func TestAdminAuth_ExpiredSessionIsRejected(t *testing.T) {
	s := newStore(t)
	owner := s.owner

	// Genuinely issued by this deployment, and over by the time the request
	// arrives. Expiry is enforced in the lookup's own predicate rather than by the
	// hourly sweep, which is what makes this a 303 rather than a session that
	// works until the sweep next runs.
	token, _, err := s.users.IssueSession(t.Context(), owner.ID, time.Nanosecond)
	if err != nil {
		t.Fatalf("IssueSession: %v", err)
	}

	req := newRequest(t, s.srv, http.MethodGet, "/admin/products", nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: token})
	if res, _ := do(t, s.srv, req); res.StatusCode != http.StatusSeeOther {
		t.Errorf("expired session = %d, want 303", res.StatusCode)
	}
}

// next resumes where an unauthenticated request was going, and cannot be used to
// send somebody off the site — which is what makes a redirect parameter a
// phishing tool rather than a convenience.
func TestAdminAuth_NextCannotLeaveTheSite(t *testing.T) {
	s := newStore(t)

	// The honest case first: the login page carries it in a hidden field, and the
	// sign-in lands there.
	res, body := get(t, s.srv, "/admin/login?next=%2Fadmin%2Fcategories")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /admin/login = %d", res.StatusCode)
	}
	if !strings.Contains(body, `name="next" value="/admin/categories"`) {
		t.Error("the login form does not carry a safe next value")
	}
	res, body = post(t, s.srv, "/admin/login", url.Values{
		"email":    {testEmail},
		"password": {testPassword},
		"next":     {"/admin/categories"},
	})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("sign in = %d %s", res.StatusCode, body)
	}
	if got := res.Header.Get("Location"); got != "/admin/categories" {
		t.Errorf("Location = %q, want /admin/categories", got)
	}

	for _, next := range []string{
		"https://evil.example/admin/",
		"//evil.example/admin/",
		"/admin//evil.example",
		"/admin/x/../../evil",
		"/products",
		"/admin/x\r\nSet-Cookie: a=b",
		"javascript:alert(1)",
		"\\\\evil.example",
	} {
		s := newStore(t)

		res, _ := get(t, s.srv, "/admin/login?next="+url.QueryEscape(next))
		if res.StatusCode != http.StatusOK {
			t.Errorf("next %q: GET /admin/login = %d", next, res.StatusCode)
			continue
		}
		res, body := post(t, s.srv, "/admin/login", url.Values{
			"email":    {testEmail},
			"password": {testPassword},
			"next":     {next},
		})
		if res.StatusCode != http.StatusSeeOther {
			t.Errorf("next %q: sign in = %d %s", next, res.StatusCode, body)
			continue
		}
		if got := res.Header.Get("Location"); got != "/admin/products" {
			t.Errorf("next %q sent the browser to %q, want /admin/products", next, got)
		}
	}
}

func sessionCookieFrom(t *testing.T, res *http.Response) *http.Cookie {
	t.Helper()
	for _, c := range res.Cookies() {
		if c.Name == auth.CookieName {
			return c
		}
	}
	t.Fatalf("no %s cookie in the response", auth.CookieName)
	return nil
}

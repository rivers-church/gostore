package handler

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/17xande-dev/gostore/internal/auth"
)

// setupToken puts an unclaimed setup token in the store, as main.go's boot path
// does on a fresh database, and returns it.
func setupToken(t *testing.T, s *shop) string {
	t.Helper()

	token, err := auth.NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	stored, err := s.users.CreateSetupToken(t.Context(), token)
	if err != nil || !stored {
		t.Fatalf("CreateSetupToken = %v, %v", stored, err)
	}
	return token
}

const claimPassword = "a first owner's long passphrase"

func TestAdminSetup_ClaimsTheFirstAccount(t *testing.T) {
	s := newUnclaimedStore(t)
	token := setupToken(t, s)

	// While nobody has claimed the store the login form is a dead end, so it
	// points at the page that is not.
	res, _ := get(t, s.srv, "/admin/login")
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("GET /admin/login before any account = %d, want 303", res.StatusCode)
	}
	if got := res.Header.Get("Location"); got != "/admin/setup" {
		t.Errorf("Location = %q, want /admin/setup", got)
	}

	res, body := get(t, s.srv, "/admin/setup")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /admin/setup = %d %s", res.StatusCode, body)
	}
	for _, want := range []string{`name="token"`, `name="email"`, `name="password"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the setup page has no %s field", want)
		}
	}

	res, body = post(t, s.srv, "/admin/setup", url.Values{
		"token":            {token},
		"email":            {"first@example.com"},
		"name":             {"The First Owner"},
		"password":         {claimPassword},
		"password_confirm": {claimPassword},
	})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("claim = %d %s", res.StatusCode, body)
	}
	if got := res.Header.Get("Location"); got != "/admin/products" {
		t.Errorf("Location = %q, want /admin/products", got)
	}

	// The account is an owner — the role the last-account guard protects — and it
	// is not forced through a password change, because it chose its own password.
	user, err := s.users.GetByEmail(t.Context(), "first@example.com")
	if err != nil {
		t.Fatalf("GetByEmail: %v", err)
	}
	if user.Role != auth.RoleOwner {
		t.Errorf("role = %q, want owner", user.Role)
	}
	if user.MustChangePassword {
		t.Error("the claimed account must change its password, which it just set")
	}
	if user.Name != "The First Owner" {
		t.Errorf("name = %q", user.Name)
	}

	// Signed in by the claim: they hold the token and have just chosen the
	// password, which is more than the login form asks for.
	if res, _ := get(t, s.srv, "/admin/products"); res.StatusCode != http.StatusOK {
		t.Errorf("GET /admin/products after claiming = %d, want 200", res.StatusCode)
	}
	// And the password really is the one they typed.
	if _, err := s.users.Authenticate(t.Context(), "first@example.com", claimPassword); err != nil {
		t.Errorf("the claimed account cannot sign in with its password: %v", err)
	}
}

// Setup locks permanently. It is not a page that closes for a while: the consumed
// timestamp is never cleared, so a restart does not reopen it and neither does
// disabling every account.
func TestAdminSetup_LocksAfterOneClaim(t *testing.T) {
	s := newUnclaimedStore(t)
	token := setupToken(t, s)

	res, body := post(t, s.srv, "/admin/setup", url.Values{
		"token":            {token},
		"email":            {"first@example.com"},
		"password":         {claimPassword},
		"password_confirm": {claimPassword},
	})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("claim = %d %s", res.StatusCode, body)
	}

	// A 404 rather than a redirect or a message: on a claimed store the page does
	// not exist, and nothing about a live deployment should advertise the shape of
	// its bootstrap.
	if res, _ := get(t, s.srv, "/admin/setup"); res.StatusCode != http.StatusNotFound {
		t.Errorf("GET /admin/setup after the claim = %d, want 404", res.StatusCode)
	}
	res, _ = post(t, s.srv, "/admin/setup", url.Values{
		"token":            {token},
		"email":            {"second@example.com"},
		"password":         {claimPassword},
		"password_confirm": {claimPassword},
	})
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("a second claim with the same token = %d, want 404", res.StatusCode)
	}
	if _, err := s.users.GetByEmail(t.Context(), "second@example.com"); err == nil {
		t.Error("the second claim created an account")
	}

	// And the login form stops pointing at setup. This jar is signed in from the
	// claim, so it skips the form for the admin itself; TestAdminAuth_SignsIn…
	// covers the form rendering for a jar that is not.
	res, _ = get(t, s.srv, "/admin/login")
	if got := res.Header.Get("Location"); got != "/admin/products" {
		t.Errorf("GET /admin/login on a claimed store → %q, want /admin/products", got)
	}
}

// A store with accounts has no setup page at all, whether or not a token was ever
// issued — which is the state every deployment past its first day is in.
func TestAdminSetup_AbsentOnceAnAdminExists(t *testing.T) {
	s := newStore(t)

	if res, _ := get(t, s.srv, "/admin/setup"); res.StatusCode != http.StatusNotFound {
		t.Errorf("GET /admin/setup = %d, want 404", res.StatusCode)
	}
	res, _ := post(t, s.srv, "/admin/setup", url.Values{
		"token":            {"anything at all"},
		"email":            {"sneaky@example.com"},
		"password":         {claimPassword},
		"password_confirm": {claimPassword},
	})
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("POST /admin/setup = %d, want 404", res.StatusCode)
	}
	if _, err := s.users.GetByEmail(t.Context(), "sneaky@example.com"); err == nil {
		t.Error("a claim on a store that already has an owner created an account")
	}
}

func TestAdminSetup_RefusesAWrongToken(t *testing.T) {
	s := newUnclaimedStore(t)
	token := setupToken(t, s)

	res, body := post(t, s.srv, "/admin/setup", url.Values{
		"token":            {token + "x"},
		"email":            {"first@example.com"},
		"password":         {claimPassword},
		"password_confirm": {claimPassword},
	})
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong token = %d, want 401", res.StatusCode)
	}
	if !strings.Contains(body, "That setup token is not right.") {
		t.Error("no message on the token field")
	}
	// The token is a credential, so a rejected form does not hand it back to the
	// browser's form history or to a screenshot of the page.
	if strings.Contains(body, token) {
		t.Error("the rejected form carries the token back into the page")
	}
	if n, err := s.users.Count(t.Context()); err != nil || n != 0 {
		t.Errorf("accounts after a refused claim = %d (err %v), want 0", n, err)
	}

	// A refusal does not spend the token, so the operator can try again with the
	// one they meant.
	res, body = post(t, s.srv, "/admin/setup", url.Values{
		"token":            {token},
		"email":            {"first@example.com"},
		"password":         {claimPassword},
		"password_confirm": {claimPassword},
	})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("claim after a refusal = %d %s", res.StatusCode, body)
	}
}

func TestAdminSetup_ValidatesTheForm(t *testing.T) {
	cases := map[string]struct {
		form  url.Values
		field string
	}{
		"no token": {url.Values{
			"email": {"first@example.com"}, "password": {claimPassword},
			"password_confirm": {claimPassword},
		}, "token"},
		"no email": {url.Values{
			"password": {claimPassword}, "password_confirm": {claimPassword},
		}, "email"},
		"bad email": {url.Values{
			"email": {"not an address"}, "password": {claimPassword},
			"password_confirm": {claimPassword},
		}, "email"},
		"short password": {url.Values{
			"email": {"first@example.com"}, "password": {"short"},
			"password_confirm": {"short"},
		}, "password"},
		"mismatched confirmation": {url.Values{
			"email": {"first@example.com"}, "password": {claimPassword},
			"password_confirm": {claimPassword + "x"},
		}, "password_confirm"},
	}

	for name, tc := range cases {
		s := newUnclaimedStore(t)
		token := setupToken(t, s)
		form := tc.form
		if _, set := form["token"]; !set && tc.field != "token" {
			form.Set("token", token)
		}

		res, _ := post(t, s.srv, "/admin/setup", form)
		if res.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("%s = %d, want 422", name, res.StatusCode)
		}
		if n, err := s.users.Count(t.Context()); err != nil || n != 0 {
			t.Errorf("%s: accounts = %d (err %v), want 0", name, n, err)
		}
		// The token survives a rejected form, so a typo in the email does not cost
		// an operator their one chance at the bootstrap.
		if pending, err := s.users.SetupPending(t.Context()); err != nil || !pending {
			t.Errorf("%s: setup is no longer pending (err %v)", name, err)
		}
	}
}

// The display-name form of an address must be reduced to the addr-spec before
// validation, not after: gostore's own address check rejects the space in
// `Alex <a@b.com>`, so a claim in that form would be refused rather than stored
// as a second, permanently unusable account for one mailbox.
func TestAdminSetup_NormalizesTheAddress(t *testing.T) {
	s := newUnclaimedStore(t)
	token := setupToken(t, s)

	res, body := post(t, s.srv, "/admin/setup", url.Values{
		"token":            {token},
		"email":            {"The First Owner <first@example.com>"},
		"password":         {claimPassword},
		"password_confirm": {claimPassword},
	})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("claim = %d %s", res.StatusCode, body)
	}
	user, err := s.users.GetByEmail(t.Context(), "first@example.com")
	if err != nil {
		t.Fatalf("GetByEmail: %v", err)
	}
	if user.Email != "first@example.com" {
		t.Errorf("stored email = %q, want the addr-spec", user.Email)
	}
}

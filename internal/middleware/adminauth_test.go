package middleware

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/17xande-dev/gostore/internal/auth"
	"github.com/17xande-dev/gostore/internal/dbtest"
)

// cheapHash keeps these tests fast: they are about the middleware, not about how
// expensive an argon2id hash is.
func cheapHash(t *testing.T, password string) string {
	t.Helper()
	h, err := auth.HashPassword(password,
		auth.Params{Memory: 64, Time: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32})
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	return h
}

// store returns a real auth.Store with one enabled owner in it. RequireAdmin
// looks a session up on every request, so there is no seam here worth faking —
// and a fake would not have the behaviour the middleware depends on, which is
// that a revoked session stops resolving.
func store(t *testing.T) (*auth.Store, auth.User, context.Context) {
	t.Helper()
	s := auth.NewStore(dbtest.Pool(t))
	ctx := t.Context()
	u, err := s.Create(ctx, "owner@example.com", "The Owner",
		cheapHash(t, "correct horse battery"), auth.RoleOwner, false)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return s, u, ctx
}

// protected wraps a handler in RequireAdmin, and reports both whether the handler
// was reached and which user reached it.
func protected(t *testing.T, s *auth.Store) (http.Handler, *auth.User) {
	t.Helper()

	var got auth.User
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, ok := AdminUser(r)
		if !ok {
			t.Error("the protected handler has no user in its context")
		}
		got = u
		w.Write([]byte("secret"))
	})
	return RequireAdmin(s, slog.New(slog.DiscardHandler))(next), &got
}

func request(t *testing.T, h http.Handler, method, target, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, target, nil)
	if token != "" {
		req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: token})
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestRequireAdmin_AllowsAndCarriesTheUser(t *testing.T) {
	s, owner, ctx := store(t)
	h, got := protected(t, s)

	token, _, err := s.IssueSession(ctx, owner.ID, time.Hour)
	if err != nil {
		t.Fatalf("IssueSession: %v", err)
	}

	w := request(t, h, http.MethodGet, "/admin/products", token)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	// The account, not just a yes: phase 3's permission checks and every template
	// that hides what a role cannot do read it from here.
	if got.ID != owner.ID || got.Role != auth.RoleOwner {
		t.Errorf("context user = %+v, want the owner", *got)
	}
}

func TestRequireAdmin_RedirectsWithoutALiveSession(t *testing.T) {
	s, owner, ctx := store(t)

	live, _, err := s.IssueSession(ctx, owner.ID, time.Hour)
	if err != nil {
		t.Fatalf("IssueSession: %v", err)
	}
	revoked, _, err := s.IssueSession(ctx, owner.ID, time.Hour)
	if err != nil {
		t.Fatalf("IssueSession: %v", err)
	}
	if err := s.DeleteSession(ctx, revoked); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}

	cases := map[string]string{
		"no cookie": "",
		"forged":    "bm93LmZha2U",
		// The whole reason the session moved into a table: a token that was
		// genuinely issued stops working the moment its row is gone.
		"revoked": revoked,
	}
	for name, token := range cases {
		h, _ := protected(t, s)
		w := request(t, h, http.MethodGet, "/admin/products", token)

		if w.Code != http.StatusSeeOther {
			t.Errorf("%s: status = %d, want 303", name, w.Code)
		}
		if got := w.Header().Get("Location"); got != "/admin/login?next=%2Fadmin%2Fproducts" {
			t.Errorf("%s: Location = %q", name, got)
		}
		if w.Body.String() == "secret" {
			t.Errorf("%s: the protected handler ran anyway", name)
		}
	}

	h, _ := protected(t, s)
	if w := request(t, h, http.MethodGet, "/admin/products", live); w.Code != http.StatusOK {
		t.Errorf("the live session was rejected too: %d", w.Code)
	}
}

// A disabled account's sessions are deleted in the same transaction that disables
// it, so this is the belt-and-braces path: a row that somehow outlives the
// disable authenticates nobody.
func TestRequireAdmin_RefusesADisabledAccount(t *testing.T) {
	s, _, ctx := store(t)
	other, err := s.Create(ctx, "manager@example.com", "A Manager",
		cheapHash(t, "correct horse battery"), auth.RoleManager, false)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	token, _, err := s.IssueSession(ctx, other.ID, time.Hour)
	if err != nil {
		t.Fatalf("IssueSession: %v", err)
	}
	h, _ := protected(t, s)
	if w := request(t, h, http.MethodGet, "/admin/products", token); w.Code != http.StatusOK {
		t.Fatalf("not signed in at the start of the test: %d", w.Code)
	}

	// Disabling deletes the sessions; put one back to reach the branch under test.
	if err := s.SetDisabled(ctx, other.ID, true); err != nil {
		t.Fatalf("SetDisabled: %v", err)
	}
	token, _, err = s.IssueSession(ctx, other.ID, time.Hour)
	if err != nil {
		t.Fatalf("IssueSession: %v", err)
	}

	h, _ = protected(t, s)
	w := request(t, h, http.MethodGet, "/admin/products", token)
	if w.Code != http.StatusSeeOther {
		t.Errorf("status = %d, want 303", w.Code)
	}
	if w.Body.String() == "secret" {
		t.Error("a disabled account reached the protected handler")
	}
}

func TestRequireAdmin_HTMXGets401(t *testing.T) {
	s, _, _ := store(t)
	h, _ := protected(t, s)

	// A fragment request must not be answered with a login page: htmx would
	// swap it into the middle of the current document.
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/admin/products", nil)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
	if got := w.Header().Get("HX-Refresh"); got != "true" {
		t.Errorf("HX-Refresh = %q, want true", got)
	}
}

// A store that cannot answer is a 500, not a redirect. The alternative sends an
// operator round a login loop that cannot complete during a database outage, with
// the outage reported as an authentication problem.
func TestRequireAdmin_StoreErrorIs500(t *testing.T) {
	pool := dbtest.Pool(t)
	s := auth.NewStore(pool)
	// Closing the pool is the bluntest available outage, and it is the one shape
	// this has to survive: every query from here on fails.
	pool.Close()

	h, _ := protected(t, s)
	w := request(t, h, http.MethodGet, "/admin/products", "any-token-at-all")
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

func TestRequireAdmin_NextOnlyForSafeGETs(t *testing.T) {
	s, _, _ := store(t)

	cases := []struct{ method, target, want string }{
		{http.MethodGet, "/admin/products", "/admin/login?next=%2Fadmin%2Fproducts"},
		{http.MethodGet, "/admin/orders?state=paid", "/admin/login?next=%2Fadmin%2Forders%3Fstate%3Dpaid"},
		// A POST is not resumable: signing in and replaying a form submission the
		// person cannot see is worse than landing them on the admin's front page.
		{http.MethodPost, "/admin/products", "/admin/login"},
	}
	for _, tc := range cases {
		h, _ := protected(t, s)
		w := request(t, h, tc.method, tc.target, "")
		if got := w.Header().Get("Location"); got != tc.want {
			t.Errorf("%s %s: Location = %q, want %q", tc.method, tc.target, got, tc.want)
		}
	}
}

func TestAdminUser_AbsentOutsideRequireAdmin(t *testing.T) {
	// Nothing outside this package can put a User into a request context, so a
	// handler mounted without RequireAdmin gets a false rather than somebody
	// else's account.
	req := httptest.NewRequest(http.MethodGet, "/admin/products", nil)
	if u, ok := AdminUser(req); ok {
		t.Errorf("AdminUser on a bare request returned %+v", u)
	}
}

func TestChain_AppliesOutermostFirst(t *testing.T) {
	var order []string
	mw := func(name string) Middleware {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				order = append(order, name)
				next.ServeHTTP(w, r)
			})
		}
	}
	final := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { order = append(order, "handler") })

	Chain(final, mw("first"), mw("second")).ServeHTTP(
		httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	want := []string{"first", "second", "handler"}
	if len(order) != len(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}

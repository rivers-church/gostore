// Package middleware holds the HTTP middleware the server wraps routes in.
package middleware

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/17xande-dev/gostore/internal/auth"
)

// Middleware is the shape every middleware in this package returns, so routes
// can be composed with chain().
type Middleware func(http.Handler) http.Handler

// userKey is the context key the signed-in administrator travels under. Its type
// is unexported and its only value is here, so nothing outside this package can
// put a User into a request context — a handler either got one from RequireAdmin
// or it is not behind RequireAdmin.
type userKey struct{}

// AdminUser returns the administrator this request is authenticated as.
//
// The second return is false for a request that did not come through
// RequireAdmin, which is a programming error at every call site that reads it —
// so the value is worth checking rather than assuming, and the zero User holds no
// permissions if somebody does not.
func AdminUser(r *http.Request) (auth.User, bool) {
	u, ok := r.Context().Value(userKey{}).(auth.User)
	return u, ok
}

// withAdminUser is the writing half, kept next to the reading half so the two
// cannot disagree about the key.
func withAdminUser(r *http.Request, u auth.User) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), userKey{}, u))
}

// RequireAdmin rejects requests without a live admin session and hands the
// account it belongs to down to the handler in the request context.
//
// The session is looked up in the database on every request, which is the point:
// a password change, a disable or a demotion deletes the rows, so the next
// request from that browser is anonymous rather than authenticated until its
// cookie happens to expire.
//
// Three failure behaviours, and the first is the one worth reading twice:
//
//   - A store error is a 500, not a redirect. Answering "please sign in" when the
//     database is unreachable sends an operator round a login loop that cannot
//     complete, with nothing in the logs to say why — the outage is reported as an
//     authentication problem.
//   - An htmx request gets 401 and HX-Refresh. Swapping a login page into a
//     fragment of the current page would produce a broken hybrid.
//   - Anything else is a redirect to the login form, carrying where they were
//     going so signing in resumes it.
func RequireAdmin(users *auth.Store, log *slog.Logger) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			cookie, err := r.Cookie(auth.CookieName)
			if err == nil {
				_, user, err := users.Session(r.Context(), cookie.Value)
				switch {
				case err == nil && !user.Disabled:
					next.ServeHTTP(w, withAdminUser(r, user))
					return
				case err == nil:
					// Belt and braces: disabling an account deletes its sessions
					// in the same transaction, so a live session for a disabled
					// account should not exist. If one does, it does not work.
					log.Warn("session for a disabled account", "user", user.ID, "path", r.URL.Path)
				case errors.Is(err, auth.ErrNotFound):
					// Expired, revoked, or never issued. Routine, and not worth a
					// log line per request from a browser holding a stale cookie.
				default:
					log.Error("cannot read the admin session", "path", r.URL.Path, "error", err)
					http.Error(w, "internal server error", http.StatusInternalServerError)
					return
				}
			}

			if r.Header.Get("HX-Request") == "true" {
				w.Header().Set("HX-Refresh", "true")
				http.Error(w, "unauthorised", http.StatusUnauthorized)
				return
			}
			http.Redirect(w, r, loginURL(r), http.StatusSeeOther)
		})
	}
}

// loginURL is the login form, told where the request was going.
//
// Only for a GET: resuming a POST after a sign-in would replay a form submission
// the person cannot see, and "next" is a link the login page follows, not a
// request it re-issues. The value is still sanitised on the way back out, by
// handler.safeNext — this side building it from r.URL is not what makes it safe.
func loginURL(r *http.Request) string {
	const login = "/admin/login"
	if r.Method != http.MethodGet {
		return login
	}
	next := r.URL.EscapedPath()
	if r.URL.RawQuery != "" {
		next += "?" + r.URL.RawQuery
	}
	if !strings.HasPrefix(next, "/admin/") {
		return login
	}
	return login + "?next=" + url.QueryEscape(next)
}

// Chain wraps h in every middleware, applied in reverse so that call sites read
// outermost-first: Chain(h, a, b) serves a(b(h)).
func Chain(h http.Handler, mw ...Middleware) http.Handler {
	for _, m := range slices.Backward(mw) {
		h = m(h)
	}
	return h
}

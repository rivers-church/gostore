package handler

import (
	"errors"
	"net/http"
	"strings"

	"github.com/17xande-dev/gostore/internal/auth"
	"github.com/17xande-dev/gostore/internal/middleware"
)

// AdminRoute is one route under /admin and the permission it needs, recorded as
// it is registered.
//
// The record exists so that "every protected route" is something the tests can
// ask for rather than a list somebody maintains by hand next to the real one. A
// hand-written list is only ever as complete as the last person to add a route
// remembered to make it.
//
// Method is part of the record because a POST route answers a GET with 405
// before any middleware runs: a sweep that only issued GETs would prove nothing
// about the routes that actually change things.
type AdminRoute struct {
	Method  string
	Pattern string
	Perm    auth.Permission
}

// TestPath turns a registration pattern into a path that will match it —
// "/admin/users/{id}/edit" into "/admin/users/x/edit" — so a sweep can request
// every recorded route without knowing what any of them mean.
//
// The values are deliberately not valid ids. What is being tested here is the
// layer in front of the handler, which answers before anything parses them; a
// test that needed real rows would be testing the handlers instead.
func (r AdminRoute) TestPath() string {
	segments := strings.Split(r.Pattern, "/")
	for i, s := range segments {
		switch {
		case s == "{$}":
			// The end-of-path anchor is not a segment: "/admin/{$}" is the
			// pattern for exactly "/admin/".
			segments[i] = ""
		case strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}"):
			segments[i] = "x"
		}
	}
	return strings.Join(segments, "/")
}

// AdminProtectedRoutes is every route registered behind a session, with the
// permission each one names. The slice is a copy: a caller iterating it cannot
// change what the server enforces.
func (h *Handler) AdminProtectedRoutes() []AdminRoute {
	out := make([]AdminRoute, len(h.adminRoutes))
	copy(out, h.adminRoutes)
	return out
}

// passwordPath is the one protected page an administrator who must change their
// password may still reach. Signing out is the other, and it is not behind a
// session at all.
//
// The page itself arrives with the rest of the account management; until then
// nothing sets must_change_password, so the bounce below has nowhere to send
// anybody and nobody to send.
const passwordPath = "/admin/password"

// requirePerm is the authorisation half of the admin routes, applied inside
// RequireAdmin's authentication.
//
// Every route names its permission on the line that registers it, so
// authorisation travels with the route the way authentication already does.
// Templates hide what a role cannot do, but that is presentation: this is the
// enforcement, and a button that is merely absent from a page is not a
// restriction on anybody who types the address.
func (h *Handler) requirePerm(perm auth.Permission, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, ok := middleware.AdminUser(r)
		if !ok {
			// Registered outside protect, which is a wiring mistake rather than
			// anything the request did. Failing closed with a 500 is the only
			// safe reading: the alternative is serving an admin page to whoever
			// asked.
			h.serverError(w, r, errNoAdminUser)
			return
		}
		if !h.passwordIsCurrent(w, r, user) {
			return
		}
		if !user.Can(perm) {
			h.logger(r).Warn("refused an admin request the role cannot make",
				"user", user.ID, "role", user.Role, "permission", perm,
				"method", r.Method, "path", r.URL.Path)
			h.forbidden(w, r)
			return
		}
		next(w, r)
	}
}

// errNoAdminUser is the wiring mistake requirePerm cannot recover from.
var errNoAdminUser = errors.New("handler: an admin route is registered without RequireAdmin in front of it")

// passwordIsCurrent bounces an administrator whose password was reset by somebody
// else to the change form, and reports whether the caller may carry on.
//
// Every route, not just the interesting ones: a forced password change that
// covered the pages an operator was likely to visit and not the rest would be
// decorative. The exception is the change form itself, which they would
// otherwise be redirected to from.
func (h *Handler) passwordIsCurrent(w http.ResponseWriter, r *http.Request, user auth.User) bool {
	if !user.MustChangePassword || r.URL.Path == passwordPath {
		return true
	}

	// htmx would swap the redirect's target into a fragment of the page it is
	// refusing to serve, so it is told to navigate instead.
	if isHTMX(r) {
		w.Header().Set("HX-Redirect", passwordPath)
		w.WriteHeader(http.StatusNoContent)
		return false
	}
	http.Redirect(w, r, passwordPath, http.StatusSeeOther)
	return false
}

// forbidden answers a request from a signed-in administrator whose role does not
// cover it. Deliberately not a 404: the page exists, they are simply not allowed
// it, and pretending otherwise would send somebody hunting for a broken link.
func (h *Handler) forbidden(w http.ResponseWriter, r *http.Request) {
	h.clientError(w, r, http.StatusForbidden, "Not for your account",
		"Your role does not cover this. If you need it, ask an administrator to change your role.")
}

package handler

import (
	"errors"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/17xande-dev/gostore/internal/auth"
	"github.com/17xande-dev/gostore/internal/validate"
)

type loginPage struct {
	page

	// Email is carried back into a rejected form; the password never is.
	Email string
	Error string

	// Next is where to go after signing in, already sanitised. It is rendered as
	// a hidden field rather than kept in the URL so the POST carries it too.
	Next string

	// Notice is why they are back here, looked up from a fixed map by code — a
	// changed password ends every session, and arriving at a login form with no
	// explanation looks like the store signed you out for no reason.
	Notice string
}

// loginNotices are the reasons the login page can give for being where it is.
// Codes, not text: nothing from the query string is rendered.
var loginNotices = map[string]string{
	"password_changed": "Password changed. Sign in with the new one.",
}

type setupPage struct {
	page

	// Token is not carried back into a rejected form. It is a credential, and a
	// value that survives a failed submission is one that ends up in a browser's
	// form history and in a screenshot of the page.
	Email  string
	Name   string
	Errors validate.FormErrors
}

func (h *Handler) adminLoginForm(w http.ResponseWriter, r *http.Request) {
	// Nobody has claimed this store yet: the login form is a dead end, and the
	// claim page is where an operator needs to be. Checked before the cookie,
	// because a session cannot exist while there are no accounts.
	pending, ok := h.setupPending(w, r)
	if !ok {
		return
	}
	if pending {
		http.Redirect(w, r, "/admin/setup", http.StatusSeeOther)
		return
	}

	// Already signed in? Skip the form rather than inviting a second login.
	if _, ok := h.currentSession(r); ok {
		http.Redirect(w, r, "/admin/products", http.StatusSeeOther)
		return
	}
	h.render(w, r, http.StatusOK, "admin_login", loginPage{
		page:   h.newPage(r, "Sign in"),
		Next:   safeNext(r.URL.Query().Get("next")),
		Notice: noticeFor(r, loginNotices),
	})
}

func (h *Handler) adminLogin(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.badForm(w, r)
		return
	}

	email := validate.NormalizeEmail(r.PostFormValue("email"))
	next := safeNext(r.PostFormValue("next"))

	user, err := h.users.Authenticate(r.Context(), email, r.PostFormValue("password"))
	if err != nil {
		if !errors.Is(err, auth.ErrBadCredentials) {
			h.serverError(w, r, err)
			return
		}
		// One message for every failure — unknown address, wrong password,
		// disabled account. Which one it was is exactly what an attacker
		// enumerating addresses wants told, and the store already spends the same
		// argon2 work on all three so the timing does not say either.
		h.logger(r).Warn("failed admin login", "email", email)
		h.render(w, r, http.StatusUnauthorized, "admin_login", loginPage{
			page:  h.newPage(r, "Sign in"),
			Email: r.PostFormValue("email"),
			Next:  next,
			Error: "That email and password do not match an account.",
		})
		return
	}

	if !h.startSession(w, r, user) {
		return
	}
	h.logger(r).Info("admin signed in", "user", user.ID, "email", user.Email, "role", user.Role)
	if next == "" {
		next = "/admin/products"
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// startSession issues a session for user, replacing whatever the request arrived
// with, and sets the cookie. It reports whether the caller may carry on; a false
// has already answered the request.
//
// The old session is deleted rather than left to expire: a token that was in the
// browser before the sign-in must not still open the admin afterwards, which is
// the session-fixation case — an attacker who can set a cookie on the victim's
// browser plants a token, waits for them to sign in, and holds a session that is
// now authenticated as them.
func (h *Handler) startSession(w http.ResponseWriter, r *http.Request, user auth.User) bool {
	if old, err := r.Cookie(auth.CookieName); err == nil {
		if err := h.users.DeleteSession(r.Context(), old.Value); err != nil {
			h.serverError(w, r, err)
			return false
		}
	}

	token, _, err := h.users.IssueSession(r.Context(), user.ID, h.cfg.SessionTTL)
	if err != nil {
		h.serverError(w, r, err)
		return false
	}
	http.SetCookie(w, h.sessionCookie(token, h.cfg.SessionTTL))
	return true
}

func (h *Handler) adminLogout(w http.ResponseWriter, r *http.Request) {
	// The row is the session and the cookie is a copy of the key to it, so both
	// go. Deleting the row is the half that matters: a cookie a browser keeps
	// after this — a stale tab, a copy taken from a debugger — opens nothing.
	if cookie, err := r.Cookie(auth.CookieName); err == nil {
		if err := h.users.DeleteSession(r.Context(), cookie.Value); err != nil {
			// Logged, not shown. Somebody who has asked to sign out gets signed
			// out of this browser either way, and an error page in place of the
			// login form would suggest they are still in.
			h.logger(r).Error("could not delete the session row on sign-out", "error", err)
		}
	}
	http.SetCookie(w, h.sessionCookie("", -time.Hour))
	http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
}

// adminSetupForm offers the first account, and 404s once there is one.
//
// A 404 rather than a redirect, and the same for the POST: setup is not a page
// that is "closed", it is a page that does not exist on a store that has been
// claimed. Nothing about a live deployment should advertise the shape of its
// bootstrap.
func (h *Handler) adminSetupForm(w http.ResponseWriter, r *http.Request) {
	pending, ok := h.setupPending(w, r)
	if !ok {
		return
	}
	if !pending {
		h.notFound(w, r)
		return
	}
	h.render(w, r, http.StatusOK, "admin_setup", setupPage{page: h.newPage(r, "Set up gostore")})
}

// adminSetupClaim creates the first administrator, given the setup token.
//
// It is rate limited under the same limiter as the login POST, because it is the
// same kind of request: it verifies a secret, and an unlimited one is a token
// worth guessing at.
func (h *Handler) adminSetupClaim(w http.ResponseWriter, r *http.Request) {
	pending, ok := h.setupPending(w, r)
	if !ok {
		return
	}
	if !pending {
		h.notFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		h.badForm(w, r)
		return
	}

	// Normalised BEFORE validation, not after: gostore's address check rejects
	// spaces, so `Alex <a@example.com>` would never survive validation intact to
	// be normalised on the way to the store. See validate.NormalizeEmail.
	email := validate.NormalizeEmail(r.PostFormValue("email"))
	name := strings.TrimSpace(r.PostFormValue("name"))
	token := r.PostFormValue("token")
	password := r.PostFormValue("password")

	errs := validate.AdminUser(email, name)
	validate.Password(errs, password, r.PostFormValue("password_confirm"))
	if token == "" {
		errs.Add("token", "Required.")
	}
	if errs.Any() {
		h.renderSetup(w, r, http.StatusUnprocessableEntity, email, name, errs)
		return
	}

	hash, err := auth.HashPassword(password, auth.DefaultParams)
	if err != nil {
		h.serverError(w, r, err)
		return
	}

	user, err := h.users.ClaimSetup(r.Context(), token, email, name, hash)
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrBadSetupToken):
			h.logger(r).Warn("wrong setup token offered")
			errs.Add("token", "That setup token is not right.")
			h.renderSetup(w, r, http.StatusUnauthorized, email, name, errs)
		case errors.Is(err, auth.ErrSetupClosed):
			// Somebody claimed the store between the check above and here.
			h.notFound(w, r)
		case errors.Is(err, auth.ErrEmailTaken):
			errs.Add("email", "An administrator with that email already exists.")
			h.renderSetup(w, r, http.StatusUnprocessableEntity, email, name, errs)
		default:
			h.serverError(w, r, err)
		}
		return
	}

	// Signed straight in. They hold the setup token and have just chosen this
	// account's password, which is more than the login form would ask for.
	if !h.startSession(w, r, user) {
		return
	}
	h.logger(r).Info("first administrator claimed the store", "user", user.ID, "email", user.Email)
	http.Redirect(w, r, "/admin/products", http.StatusSeeOther)
}

func (h *Handler) renderSetup(w http.ResponseWriter, r *http.Request, status int, email, name string, errs validate.FormErrors) {
	h.render(w, r, status, "admin_setup", setupPage{
		page:   h.newPage(r, "Set up gostore"),
		Email:  email,
		Name:   name,
		Errors: errs,
	})
}

// setupPending answers whether the store is still claimable. The second return is
// false when it could not find out, in which case the request has already been
// answered with a 500 and the caller must return without writing anything more.
//
// Two returns rather than one, because "not pending" and "cannot tell" need
// different responses and a single bool collapses them into the wrong one: every
// caller would go on to write a page after the error page, which is a 500 with a
// second document appended to it and a superfluous WriteHeader in the log.
//
// Failing closed on an error would 404 the setup page during a database blip and
// leave an operator convinced the store was already claimed; failing open would
// offer a claim form on a store that has administrators. Neither is a guess worth
// making, so an error is an error.
func (h *Handler) setupPending(w http.ResponseWriter, r *http.Request) (pending, ok bool) {
	pending, err := h.users.SetupPending(r.Context())
	if err != nil {
		h.serverError(w, r, err)
		return false, false
	}
	return pending, true
}

// currentSession reports whether this request carries a live session, for the
// login form's "you are already signed in" branch. Everything behind
// RequireAdmin reads middleware.AdminUser instead.
func (h *Handler) currentSession(r *http.Request) (auth.User, bool) {
	cookie, err := r.Cookie(auth.CookieName)
	if err != nil {
		return auth.User{}, false
	}
	_, user, err := h.users.Session(r.Context(), cookie.Value)
	if err != nil || user.Disabled {
		return auth.User{}, false
	}
	return user, true
}

// safeNext reduces a "where was I going" parameter to something safe to redirect
// to, or to "" — which every caller reads as the default landing page.
//
// An allowlist rather than a blocklist: it must be a path under /admin/, which
// rules out an absolute URL, a protocol-relative //evil.example (a host, not a
// path, to a browser), and a header-splitting CR or LF, without needing to have
// thought of each of them. A redirect the store hands out is a redirect somebody
// will use to make a phishing link look like it came from here.
func safeNext(next string) string {
	if next == "" || !strings.HasPrefix(next, "/admin/") || strings.HasPrefix(next, "/admin//") {
		return ""
	}
	if strings.ContainsAny(next, "\r\n\\") {
		return ""
	}
	// Parseable, and still relative once parsed.
	u, err := url.Parse(next)
	if err != nil || u.Scheme != "" || u.Host != "" || u.Opaque != "" {
		return ""
	}
	// And it must be the path it appears to be: "/admin/x/../../evil" starts
	// "/admin/" as far as HasPrefix is concerned, and a browser resolves it
	// somewhere else entirely before asking for it.
	if path.Clean(u.Path) != u.Path {
		return ""
	}
	if u.EscapedPath() != next && u.RequestURI() != next {
		return ""
	}
	return next
}

// sessionCookie builds the admin cookie, or its removal when value is empty.
// Path is /admin, so the cookie is never sent with a storefront request and
// cannot leak into the embeddable, deliberately cookie-free catalog fragments.
func (h *Handler) sessionCookie(value string, ttl time.Duration) *http.Cookie {
	c := &http.Cookie{
		Name:     auth.CookieName,
		Value:    value,
		Path:     "/admin",
		HttpOnly: true,
		Secure:   h.cfg.CookieSecure,
		// Hard-coded, not configurable. Strict would sign an operator out of
		// every link that arrives from anywhere else — including the ones in the
		// order notification emails this store sends itself — and None would
		// send the admin session with a cross-site request, which is the whole
		// thing CSRF protection exists to stop needing to worry about.
		SameSite: http.SameSiteLaxMode,
	}
	if ttl > 0 {
		c.Expires = time.Now().Add(ttl)
		c.MaxAge = int(ttl.Seconds())
	} else {
		c.MaxAge = -1
	}
	return c
}

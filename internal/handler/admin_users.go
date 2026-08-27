package handler

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/17xande-dev/gostore/internal/auth"
	"github.com/17xande-dev/gostore/internal/middleware"
	"github.com/17xande-dev/gostore/internal/validate"
)

// The administrator accounts. Plain POST-redirect-GET throughout, no htmx: these
// pages are used a handful of times in a store's life, and a full page load after
// each change is the clearest possible statement of what the account now is.
//
// Two rules run through the whole file:
//
//   - **Accounts are disabled, never deleted.** A removed row erases who did
//     what, and the orders and products an administrator touched outlive their
//     employment.
//   - **A refused action is absent, not broken.** Every guard below is mirrored
//     in the template, so the button for something the server would turn down is
//     not rendered at all. The handler is still what enforces it — a hidden form
//     is a restriction on the mouse and nothing else.

type usersPage struct {
	page
	Users []auth.User
	// Notice is the outcome of whatever redirected here, looked up from a fixed
	// map by code. The query string never reaches the page as text: a notice
	// built from `?notice=<whatever>` is a way to put chosen words on our own
	// page, under our own domain, in front of somebody who trusts both.
	Notice string
}

type userFormPage struct {
	page
	Form   userForm
	Roles  []auth.Role
	Errors validate.FormErrors
}

// userForm is the create form's raw input, so a rejected submission comes back
// with what was typed. The password is deliberately not part of it — a password
// that survives a failed submission ends up in the browser's form history and in
// a screenshot of the page.
type userForm struct {
	Email string
	Name  string
	Role  auth.Role
}

type userEditPage struct {
	page
	User   auth.User
	Roles  []auth.Role
	Notice string
	Errors validate.FormErrors

	// Self is set when this is the account of the person reading the page. It is
	// what removes the role, disable and reset controls: all three are things
	// only somebody else may do to you.
	Self bool

	// LastOwner is set when disabling or demoting this account would leave the
	// admin area with nobody who can open it. The store refuses that whatever
	// this says; here it decides whether the controls appear.
	LastOwner bool
}

type passwordPage struct {
	page
	Errors validate.FormErrors

	// Forced is set when this page was reached by being bounced to it, so it can
	// say why rather than looking like a page the browser wandered onto.
	Forced bool
}

// userNotices are the outcomes the account pages can report, by code. A fixed
// map, so the only strings that can be rendered are these.
var userNotices = map[string]string{
	"created":        "Administrator created. They will choose their own password the first time they sign in.",
	"role_changed":   "Role changed. Their sessions have ended, so they will sign in again with the new one.",
	"role_unchanged": "That is already their role. Nothing changed, and their sessions are untouched.",
	"disabled":       "Account disabled and its sessions ended. Nothing they did has been removed.",
	"enabled":        "Account enabled. They can sign in again.",
	"password_reset": "Password reset and sessions ended. They must choose a new one when they next sign in.",
}

// noticeFor looks a code up in a table, and answers "" for anything else.
func noticeFor(r *http.Request, table map[string]string) string {
	return table[r.URL.Query().Get("notice")]
}

func (h *Handler) adminUserList(w http.ResponseWriter, r *http.Request) {
	users, err := h.users.List(r.Context())
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	h.render(w, r, http.StatusOK, "admin_users", usersPage{
		page:   h.newPage(r, "Administrators"),
		Users:  users,
		Notice: noticeFor(r, userNotices),
	})
}

func (h *Handler) adminUserNew(w http.ResponseWriter, r *http.Request) {
	h.renderUserForm(w, r, http.StatusOK, userForm{Role: auth.RoleManager}, nil)
}

// adminUserCreate adds an administrator.
//
// The password is set by whoever creates the account, so must_change_password
// goes with it: a credential its owner did not choose, and somebody else knows,
// is a temporary one.
func (h *Handler) adminUserCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.badForm(w, r)
		return
	}

	// Normalised BEFORE validation, not after: gostore's address check rejects
	// spaces, so `Alex <a@example.com>` would never survive validation intact to
	// be normalised on the way to the store. See validate.NormalizeEmail.
	form := userForm{
		Email: validate.NormalizeEmail(r.PostFormValue("email")),
		Name:  strings.TrimSpace(r.PostFormValue("name")),
		Role:  auth.Role(r.PostFormValue("role")),
	}
	password := r.PostFormValue("password")

	errs := validate.AdminUser(form.Email, form.Name)
	validate.Password(errs, password, r.PostFormValue("password_confirm"))
	if !form.Role.Valid() {
		errs.Add("role", "Choose one of the roles listed.")
	}
	if errs.Any() {
		h.renderUserForm(w, r, http.StatusUnprocessableEntity, form, errs)
		return
	}

	hash, err := auth.HashPassword(password, auth.DefaultParams)
	if err != nil {
		h.serverError(w, r, err)
		return
	}

	user, err := h.users.Create(r.Context(), form.Email, form.Name, hash, form.Role, true)
	if err != nil {
		if errors.Is(err, auth.ErrEmailTaken) {
			errs.Add("email", "An administrator with that email already exists.")
			h.renderUserForm(w, r, http.StatusUnprocessableEntity, form, errs)
			return
		}
		h.serverError(w, r, err)
		return
	}

	h.logger(r).Info("administrator created", "user", user.ID, "email", user.Email,
		"role", user.Role, "by", h.actorID(r))
	http.Redirect(w, r, "/admin/users?notice=created", http.StatusSeeOther)
}

func (h *Handler) adminUserEdit(w http.ResponseWriter, r *http.Request) {
	user, ok := h.managedUser(w, r)
	if !ok {
		return
	}
	h.renderUserEdit(w, r, http.StatusOK, user, noticeFor(r, userNotices), nil)
}

// adminUserRole changes somebody else's role.
//
// Never your own, and not because of the last-owner guard: an administrator who
// can promote themselves is not held by their role at all, and one who can demote
// themselves can take away the permission they would need to undo it.
func (h *Handler) adminUserRole(w http.ResponseWriter, r *http.Request) {
	user, ok := h.managedUser(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		h.badForm(w, r)
		return
	}
	if h.isSelf(r, user) {
		h.refuseUserChange(w, r, user, "You cannot change your own role. Ask another administrator to do it.")
		return
	}

	// A role that is not one of the four is a bad submission rather than a
	// state this account is in, so it is 422 with the message on the field —
	// the same answer the create form gives — and not the 409 the guards use.
	role := auth.Role(r.PostFormValue("role"))
	if !role.Valid() {
		errs := validate.FormErrors{}
		errs.Add("role", "Choose one of the roles listed.")
		h.renderUserEdit(w, r, http.StatusUnprocessableEntity, user, "", errs)
		return
	}

	// Saving the form without touching the select changes nothing, and the store
	// deliberately does not end their sessions for a no-op. The notice has to
	// agree with that: "their sessions have ended" would be a lie told by the
	// page that just failed to end them.
	notice := "role_changed"
	if role == user.Role {
		notice = "role_unchanged"
	}

	if err := h.users.SetRole(r.Context(), user.ID, role); err != nil {
		h.userChangeError(w, r, user, err,
			"This is the last owner who can still sign in, so their role cannot be lowered. "+
				"Make somebody else an owner first.")
		return
	}
	h.logger(r).Info("administrator role changed", "user", user.ID, "role", role, "by", h.actorID(r))
	http.Redirect(w, r, "/admin/users/"+user.ID+"/edit?notice="+notice, http.StatusSeeOther)
}

// adminUserDisabled switches an account off or back on.
//
// The desired state is read from the form rather than inferred by flipping what
// was rendered. Two administrators looking at the same stale list would otherwise
// each get the opposite of what they clicked, and a toggle is the one control
// where that is silent.
func (h *Handler) adminUserDisabled(w http.ResponseWriter, r *http.Request) {
	user, ok := h.managedUser(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		h.badForm(w, r)
		return
	}

	// Strictly "1" or "0". Reading it as `value == "1"` would make an absent
	// field mean *enable*, so an empty POST would perform the unguarded
	// direction of a control nobody submitted.
	var disabled bool
	switch r.PostFormValue("disabled") {
	case "1":
		disabled = true
	case "0":
		disabled = false
	default:
		h.clientError(w, r, http.StatusBadRequest, "That request could not be read",
			"The form did not say whether to disable or enable the account.")
		return
	}

	if h.isSelf(r, user) {
		h.refuseUserChange(w, r, user,
			"You cannot disable your own account. Ask another administrator to do it.")
		return
	}

	if err := h.users.SetDisabled(r.Context(), user.ID, disabled); err != nil {
		h.userChangeError(w, r, user, err,
			"This is the last owner who can still sign in, so they cannot be disabled. "+
				"Make somebody else an owner first.")
		return
	}

	notice := "enabled"
	if disabled {
		notice = "disabled"
	}
	h.logger(r).Info("administrator account switched", "user", user.ID,
		"disabled", disabled, "by", h.actorID(r))
	http.Redirect(w, r, "/admin/users/"+user.ID+"/edit?notice="+notice, http.StatusSeeOther)
}

// adminUserPasswordReset sets somebody else's password.
//
// Never your own, even though you are allowed to change your own password on
// /admin/password: a reset here does not ask for the current password, so
// allowing it against yourself would make an unattended screen enough to take an
// account over for good. The route that does ask is the one you use on yourself.
func (h *Handler) adminUserPasswordReset(w http.ResponseWriter, r *http.Request) {
	user, ok := h.managedUser(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		h.badForm(w, r)
		return
	}
	if h.isSelf(r, user) {
		h.refuseUserChange(w, r, user,
			"Change your own password from your account page, which asks for the current one.")
		return
	}

	password := r.PostFormValue("password")
	errs := validate.FormErrors{}
	validate.Password(errs, password, r.PostFormValue("password_confirm"))
	if errs.Any() {
		h.renderUserEdit(w, r, http.StatusUnprocessableEntity, user, "", errs)
		return
	}

	hash, err := auth.HashPassword(password, auth.DefaultParams)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	// mustChange, because somebody else chose it and knows it.
	if err := h.users.SetPassword(r.Context(), user.ID, hash, true); err != nil {
		h.userChangeError(w, r, user, err, "")
		return
	}
	h.logger(r).Info("administrator password reset", "user", user.ID, "by", h.actorID(r))
	http.Redirect(w, r, "/admin/users/"+user.ID+"/edit?notice=password_reset", http.StatusSeeOther)
}

// adminPasswordForm is your own password, and the one admin page every role can
// reach — including an account that has been bounced here and can reach nothing
// else.
func (h *Handler) adminPasswordForm(w http.ResponseWriter, r *http.Request) {
	user, _ := middleware.AdminUser(r)
	h.render(w, r, http.StatusOK, "admin_password", passwordPage{
		page:   h.newPage(r, "Your password"),
		Forced: user.MustChangePassword,
	})
}

// adminPasswordChange sets your own password.
//
// It asks for the current one. A CSRF token proves the request came from our
// form; it does not prove the person at the keyboard is the account's owner, and
// an unattended session is the case this closes.
//
// Every session ends, this one included — the whole point of changing a password
// after a scare is that whoever prompted it stops being signed in — so it
// finishes at the login form rather than back in the admin.
func (h *Handler) adminPasswordChange(w http.ResponseWriter, r *http.Request) {
	user, ok := middleware.AdminUser(r)
	if !ok {
		h.serverError(w, r, errNoAdminUser)
		return
	}
	if err := r.ParseForm(); err != nil {
		h.badForm(w, r)
		return
	}

	current := r.PostFormValue("current_password")
	password := r.PostFormValue("password")

	errs := validate.FormErrors{}
	if current == "" {
		errs.Add("current_password", "Required.")
	}
	validate.Password(errs, password, r.PostFormValue("password_confirm"))
	if errs.Any() {
		h.renderPassword(w, r, http.StatusUnprocessableEntity, user, errs)
		return
	}

	switch ok, err := h.users.CheckPasswordFor(r.Context(), user.ID, current); {
	case err != nil:
		h.serverError(w, r, err)
		return
	case !ok:
		h.logger(r).Warn("wrong current password on a password change", "user", user.ID)
		errs.Add("current_password", "That is not your current password.")
		h.renderPassword(w, r, http.StatusUnprocessableEntity, user, errs)
		return
	}

	hash, err := auth.HashPassword(password, auth.DefaultParams)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	// Never must-change: this password is one its owner chose.
	if err := h.users.SetPassword(r.Context(), user.ID, hash, false); err != nil {
		h.serverError(w, r, err)
		return
	}

	// The row behind this cookie is gone, so the cookie is already inert; it is
	// cleared anyway rather than left in the browser to be sent with every
	// request until it expires.
	http.SetCookie(w, h.sessionCookie("", -time.Hour))
	h.logger(r).Info("administrator changed their own password", "user", user.ID)
	http.Redirect(w, r, "/admin/login?notice=password_changed", http.StatusSeeOther)
}

// managedUser loads the account a /admin/users/{id}/… route is about. A false
// second return means the request has been answered.
func (h *Handler) managedUser(w http.ResponseWriter, r *http.Request) (auth.User, bool) {
	user, err := h.users.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, auth.ErrNotFound) {
			h.notFound(w, r)
			return auth.User{}, false
		}
		h.serverError(w, r, err)
		return auth.User{}, false
	}
	return user, true
}

// isSelf reports whether the account being managed is the one making the request.
func (h *Handler) isSelf(r *http.Request, user auth.User) bool {
	actor, ok := middleware.AdminUser(r)
	return ok && actor.ID == user.ID
}

// actorID is who is doing this, for the log lines. Every change to an account is
// somebody's doing, and an audit line naming only the account changed answers
// half the question.
func (h *Handler) actorID(r *http.Request) string {
	actor, _ := middleware.AdminUser(r)
	return actor.ID
}

// refuseUserChange answers a guard this handler enforces itself: 409, because the
// request was well formed and understood, and it is the account's state — not the
// submission — that makes it impossible.
func (h *Handler) refuseUserChange(w http.ResponseWriter, r *http.Request, user auth.User, why string) {
	h.logger(r).Warn("refused a change to an administrator account",
		"user", user.ID, "by", h.actorID(r), "reason", why)
	errs := validate.FormErrors{}
	errs.Add("form", why)
	h.renderUserEdit(w, r, http.StatusConflict, user, "", errs)
}

// userChangeError turns a store failure into the page it belongs on: the
// last-owner refusal is a 409 with lastOwner as the explanation, and anything
// else is ours.
func (h *Handler) userChangeError(w http.ResponseWriter, r *http.Request, user auth.User, err error, lastOwner string) {
	switch {
	case errors.Is(err, auth.ErrLastOwner) && lastOwner != "":
		h.refuseUserChange(w, r, user, lastOwner)
	case errors.Is(err, auth.ErrNotFound):
		// Deleted between the load and the write. Nobody deletes accounts here,
		// but a 404 is still the honest answer to "change this row".
		h.notFound(w, r)
	default:
		h.serverError(w, r, err)
	}
}

func (h *Handler) renderUserForm(w http.ResponseWriter, r *http.Request, status int, form userForm, errs validate.FormErrors) {
	h.render(w, r, status, "admin_user_form", userFormPage{
		page:   h.newPage(r, "New administrator"),
		Form:   form,
		Roles:  auth.Roles,
		Errors: errs,
	})
}

func (h *Handler) renderUserEdit(w http.ResponseWriter, r *http.Request, status int, user auth.User, notice string, errs validate.FormErrors) {
	owners, err := h.users.CountEnabledOwners(r.Context())
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	h.render(w, r, status, "admin_user_edit", userEditPage{
		page:      h.newPage(r, user.Display()),
		User:      user,
		Roles:     auth.Roles,
		Notice:    notice,
		Errors:    errs,
		Self:      h.isSelf(r, user),
		LastOwner: user.Role == auth.RoleOwner && !user.Disabled && owners <= 1,
	})
}

func (h *Handler) renderPassword(w http.ResponseWriter, r *http.Request, status int, user auth.User, errs validate.FormErrors) {
	h.render(w, r, status, "admin_password", passwordPage{
		page:   h.newPage(r, "Your password"),
		Errors: errs,
		Forced: user.MustChangePassword,
	})
}

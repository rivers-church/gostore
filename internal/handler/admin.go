package handler

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/17xande-dev/gostore/internal/auth"
	"github.com/17xande-dev/gostore/internal/blob"
	"github.com/17xande-dev/gostore/internal/cart"
	"github.com/17xande-dev/gostore/internal/catalog"
	"github.com/17xande-dev/gostore/internal/config"
	"github.com/17xande-dev/gostore/internal/downloads"
	"github.com/17xande-dev/gostore/internal/middleware"
	"github.com/17xande-dev/gostore/internal/orders"
	"github.com/17xande-dev/gostore/internal/payment"
	"github.com/17xande-dev/gostore/internal/validate"
	"github.com/17xande-dev/mailer"
	"github.com/justinas/nosurf"
)

// Handler holds everything the HTTP layer needs. It is created once at startup
// and is safe for concurrent use.
type Handler struct {
	cfg     config.Config
	log     *slog.Logger
	tmpl    *Templates
	cat     *catalog.Store
	cart    *cart.Store
	orders  *orders.Store
	grants  *downloads.Store
	gateway payment.Gateway
	mail    mailer.Sender
	// blob is the PUBLIC image store and files is the PRIVATE download store.
	// They are never interchangeable: putting a purchased file through blob would
	// publish it, and it is worth the two fields being named differently enough
	// that the mistake looks wrong at the call site.
	blob  blob.Storage
	files blob.Downloads

	// users is the administrator accounts and their sessions. The handler needs it
	// for the login, sign-out and setup-claim routes; RequireAdmin holds the same
	// store for the lookup it does on every protected request.
	users *auth.Store

	// adminRoutes is every protected route and the permission it names, recorded
	// by RegisterAdmin as it wires them. Written once at startup, read-only
	// afterwards, so it needs no lock.
	adminRoutes []AdminRoute

	// limits are built here from cfg rather than passed in, so that a rate limit is
	// applied on the line that registers the route it protects — the same reasoning
	// as RequireAdmin. A limiter wrapped around a prefix by the caller is one
	// refactor away from silently not covering a new route.
	limits limiters
}

// limiters are the per-surface rate limits. A zero limit means the surface is
// unlimited, which is what the test configuration uses and what an operator gets
// by setting RATE_LIMIT_*_PER_MINUTE=0.
type limiters struct {
	login    middleware.Middleware
	checkout middleware.Middleware
	callback middleware.Middleware
	download middleware.Middleware
}

// perMinute builds a limiter allowing n requests a minute, or a pass-through when
// n is zero. Burst is a third of the allowance, minimum two, so a shopper who
// double-clicks is never the one who trips it.
func perMinute(name string, n int, trustProxy bool, log *slog.Logger, exceeded http.Handler) middleware.Middleware {
	if n <= 0 {
		log.Warn("rate limiting is disabled for a surface that has one available", "limiter", name)
		return func(next http.Handler) http.Handler { return next }
	}
	return middleware.RateLimit(middleware.RateLimitConfig{
		Name:     name,
		Every:    time.Minute / time.Duration(n),
		Burst:    max(2, n/3),
		Exceeded: exceeded,
	}, trustProxy, log)
}

// rateLimited is the page a throttled person sees. The payment callback does not
// use it — a gateway wants a status and a Retry-After, not HTML — which is why
// this is passed per limiter rather than built into the middleware.
func (h *Handler) rateLimited(w http.ResponseWriter, r *http.Request) {
	h.clientError(w, r, http.StatusTooManyRequests, "Too many requests",
		"That came through faster than we allow. Wait a moment and try again — the "+
			"Retry-After header on this response says how long.")
}

// Deps is everything a Handler needs, by name.
//
// It replaced a positional constructor when the twelfth argument arrived. The
// argument for that is the one this project already made about sqlc and the
// orders table: `images` and `downloads` are both blob interfaces, `cat` and
// `carts` are both stores, and a positional call is one careless edit away from
// putting the public image bucket where the private download store belongs —
// which would compile, pass most tests, and publish every purchased file.
type Deps struct {
	Config  config.Config
	Log     *slog.Logger
	Tmpl    *Templates
	Catalog *catalog.Store
	Carts   *cart.Store
	Orders  *orders.Store
	Grants  *downloads.Store
	Gateway payment.Gateway
	Mail    mailer.Sender
	Images  blob.Storage
	Files   blob.Downloads
	Users   *auth.Store
}

func New(d Deps) *Handler {
	cfg, log := d.Config, d.Log
	h := &Handler{
		cfg: cfg, log: log, tmpl: d.Tmpl, cat: d.Catalog, cart: d.Carts,
		orders: d.Orders, grants: d.Grants, gateway: d.Gateway, mail: d.Mail,
		blob: d.Images, files: d.Files, users: d.Users,
	}
	// Both storage backends are optional and both must be non-nil, so that a
	// caller omitting one gets a refusal with a message rather than a nil panic on
	// the first upload.
	if h.blob == nil {
		h.blob = blob.Unconfigured{}
	}
	if h.files == nil {
		h.files = blob.NoDownloads{}
	}
	page := http.HandlerFunc(h.rateLimited)
	h.limits = limiters{
		login:    perMinute("admin login", cfg.RateLimits.LoginPerMinute, cfg.TrustProxyIP, log, page),
		checkout: perMinute("checkout", cfg.RateLimits.CheckoutPerMinute, cfg.TrustProxyIP, log, page),
		// No page for the gateway: it is a machine, and the plain status with
		// Retry-After is exactly what it acts on.
		callback: perMinute("payment callback", cfg.RateLimits.CallbackPerMinute, cfg.TrustProxyIP, log, nil),
		// Signed URLs are cheap to mint, so a loop over one valid token should not
		// be able to produce them without bound. The allowance is generous: a buyer
		// clicking through a conference recording's twenty files in a minute is
		// ordinary, and a limit that fires on that would be turned off.
		download: perMinute("downloads", cfg.RateLimits.DownloadPerMinute, cfg.TrustProxyIP, log, page),
	}
	return h
}

// FirstPartyHandler returns everything that changes state — the admin, the cart
// and the checkout — behind CSRF protection. The admin routes additionally require
// a session; the cart and checkout are anonymous.
//
// CSRF is scoped to these routes rather than wrapped around the server's whole
// mux, because nosurf sets a token cookie on every response it handles and the
// embeddable catalog reads must stay cookie-free to be droppable into another
// origin's page. Scoping by group is also what makes the payment callback
// CSRF-exempt: it is not in this group at all, rather than being excused by an
// exempt-path string that has to keep matching the route.
func (h *Handler) FirstPartyHandler(protect middleware.Middleware) http.Handler {
	mux := http.NewServeMux()
	h.RegisterAdmin(mux, protect)
	h.registerCart(mux)
	h.registerCheckout(mux)
	// This mux is reached only for paths the outer one handed over — /admin/… and
	// /cart/… — so its own catch-all is what stops /cart/nonsense falling back to
	// Go's plain 404 while every other unknown URL gets the page.
	mux.HandleFunc("/", h.notFoundFor(mux))
	return h.withCSRF(mux)
}

// withCSRF wraps a handler in nosurf. It is one function so that every
// CSRF-protected group — the admin, the cart, and the first-party catalog pages
// that carry an add-to-cart form — shares a single configuration and a single
// token pool.
func (h *Handler) withCSRF(next http.Handler) http.Handler {
	csrf := nosurf.New(next)
	csrf.SetBaseCookie(http.Cookie{
		// Path "/" because the protected routes span /admin, /cart, /cart/checkout
		// and the first-party catalog pages, and a cookie has only one path.
		Path:     "/",
		HttpOnly: true,
		Secure:   h.cfg.CookieSecure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   nosurf.MaxAge,
	})
	csrf.SetFailureHandler(http.HandlerFunc(h.csrfFailed))

	// nosurf compares a request's Origin against one it builds from the Host
	// header, and assumes https unless told otherwise — so without this every
	// form on a plain-HTTP deployment is rejected as cross-origin. BaseURL is
	// the right signal rather than r.TLS: behind a TLS-terminating proxy the
	// connection is plain HTTP but the browser's origin is https.
	csrf.SetIsTLSFunc(func(*http.Request) bool { return h.cfg.CookieSecure })
	return csrf
}

// csrfFailed answers a request whose CSRF token was missing or wrong. It is a
// 403 and nothing else: the request was either forged or made with a stale form,
// and neither case should be retried silently.
func (h *Handler) csrfFailed(w http.ResponseWriter, r *http.Request) {
	h.logger(r).Warn("rejected request with a bad CSRF token",
		"method", r.Method, "path", r.URL.Path, "reason", nosurf.Reason(r))

	// For htmx, a reload rather than a page: the token is stale, and reloading is
	// both the explanation and the fix. HX-Refresh is honoured whatever the status,
	// because htmx handles it before it decides whether to swap anything.
	if isHTMX(r) {
		w.Header().Set("HX-Refresh", "true")
		http.Error(w, "the form you submitted has expired; reload the page and try again", http.StatusForbidden)
		return
	}
	h.clientError(w, r, http.StatusForbidden, "That form has expired",
		"Forms are only good for a while, and this one has run out. Reload the page you were on and try again.")
}

// RegisterAdmin wires the admin routes: the login form and the sign-out
// endpoint reachable by anyone, everything else behind protect and the
// permission it names.
//
// Protection is applied here, route by route, rather than by the caller. A
// middleware wrapped around a prefix somewhere else is one refactor away from
// silently no longer covering a new route; a handler registered without protect
// in this list is visible on the line that registers it. Authorisation rides
// along the same way: the permission is an argument to the registration, so
// there is no second place where a route and its role could disagree.
func (h *Handler) RegisterAdmin(mux *http.ServeMux, protect middleware.Middleware) {
	mux.HandleFunc("GET /admin/login", h.adminLoginForm)
	// Rate limited, and only the POST: the form itself is harmless, and limiting a
	// GET would lock an operator out of the page they need to read the message on.
	// argon2id's cost already makes each attempt expensive, but cost is not a limit.
	mux.Handle("POST /admin/login", h.limits.login(http.HandlerFunc(h.adminLogin)))
	mux.HandleFunc("POST /admin/logout", h.adminLogout)
	// The first-account claim, unprotected because there is no account to
	// authenticate as yet — the setup token is the credential. Both routes answer
	// 404 the moment an administrator exists, and the POST shares the login
	// limiter because it too verifies a secret.
	mux.HandleFunc("GET /admin/setup", h.adminSetupForm)
	mux.Handle("POST /admin/setup", h.limits.login(http.HandlerFunc(h.adminSetupClaim)))

	// Registering twice would otherwise record every route twice; the mux would
	// panic first, but a handler mounted on two muxes in a test would not.
	h.adminRoutes = nil

	// admin registers a route behind a session and the permission it needs, and
	// records the pair. The permission is a required argument rather than
	// something a route can leave out: a new route has to say what it is for, and
	// auth.PermRead is how it says "any signed-in administrator".
	admin := func(pattern string, perm auth.Permission, handler http.HandlerFunc) {
		method, path, ok := strings.Cut(pattern, " ")
		if !ok {
			panic("admin route pattern must be \"METHOD /path\": " + pattern)
		}
		h.adminRoutes = append(h.adminRoutes, AdminRoute{Method: method, Pattern: path, Perm: perm})
		mux.Handle(pattern, protect(h.requirePerm(perm, handler)))
	}
	admin("GET /admin/{$}", auth.PermRead, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin/products", http.StatusSeeOther)
	})
	admin("GET /admin/products", auth.PermRead, h.adminProductList)
	// The new-product form is catalog.write rather than read: it is not a view of
	// anything, it exists only to create, and offering it to a role that cannot
	// submit it would be a page whose one button is a 403. The edit page stays
	// read, because it is also where a viewer sees what a product is.
	admin("GET /admin/products/new", auth.PermCatalogWrite, h.adminProductNew)
	admin("POST /admin/products", auth.PermCatalogWrite, h.adminProductCreate)
	admin("GET /admin/products/{id}/edit", auth.PermRead, h.adminProductEdit)
	admin("GET /admin/products/{id}/downloads", auth.PermRead, h.adminProductDownloads)
	admin("POST /admin/products/{id}/files", auth.PermCatalogWrite, h.adminProductFileUpload)
	admin("POST /admin/products/{id}/files/{fileID}", auth.PermCatalogWrite, h.adminProductFileUpdate)
	admin("POST /admin/products/{id}/files/{fileID}/delete", auth.PermCatalogWrite, h.adminProductFileDelete)
	admin("POST /admin/products/{id}", auth.PermCatalogWrite, h.adminProductUpdate)
	admin("POST /admin/products/{id}/delete", auth.PermCatalogWrite, h.adminProductDelete)
	admin("POST /admin/products/{id}/variants", auth.PermCatalogWrite, h.adminVariantCreate)
	admin("POST /admin/products/{id}/variants/{variantID}", auth.PermCatalogWrite, h.adminVariantUpdate)
	admin("POST /admin/products/{id}/variants/{variantID}/delete", auth.PermCatalogWrite, h.adminVariantDelete)
	admin("POST /admin/products/{id}/image", auth.PermCatalogWrite, h.adminProductImageUpload)
	admin("POST /admin/products/{id}/image/delete", auth.PermCatalogWrite, h.adminProductImageDelete)
	// Categories. Deleting one unlinks it from its products and never deletes them
	// — see internal/handler/admin_categories.go.
	admin("GET /admin/categories", auth.PermRead, h.adminCategoryList)
	admin("GET /admin/categories/new", auth.PermCatalogWrite, h.adminCategoryNew)
	admin("POST /admin/categories", auth.PermCatalogWrite, h.adminCategoryCreate)
	admin("GET /admin/categories/{id}/edit", auth.PermRead, h.adminCategoryEdit)
	admin("POST /admin/categories/{id}", auth.PermCatalogWrite, h.adminCategoryUpdate)
	admin("POST /admin/categories/{id}/delete", auth.PermCatalogWrite, h.adminCategoryDelete)
	// Read-only on purpose: only an authenticated gateway notification may change
	// an order. See internal/handler/admin_orders.go.
	admin("GET /admin/orders", auth.PermRead, h.adminOrderList)
	admin("GET /admin/orders/{id}", auth.PermRead, h.adminOrderShow)
	// The only mutating routes under /admin/orders. See adminEntitlementRevoke for
	// why they do not break the read-only rule the order pages otherwise keep.
	admin("POST /admin/orders/{id}/entitlements/{entitlementID}/revoke", auth.PermOrdersWrite, h.adminEntitlementRevoke)
	admin("POST /admin/orders/{id}/entitlements/{entitlementID}/restore", auth.PermOrdersWrite, h.adminEntitlementRestore)
	// Administrator accounts. See internal/handler/admin_users.go — accounts are
	// disabled, never deleted, and nobody may change their own role, disable
	// themselves, or reset their own password from these pages.
	admin("GET /admin/users", auth.PermUsersWrite, h.adminUserList)
	admin("GET /admin/users/new", auth.PermUsersWrite, h.adminUserNew)
	admin("POST /admin/users", auth.PermUsersWrite, h.adminUserCreate)
	admin("GET /admin/users/{id}/edit", auth.PermUsersWrite, h.adminUserEdit)
	admin("POST /admin/users/{id}/role", auth.PermUsersWrite, h.adminUserRole)
	admin("POST /admin/users/{id}/disabled", auth.PermUsersWrite, h.adminUserDisabled)
	admin("POST /admin/users/{id}/password", auth.PermUsersWrite, h.adminUserPasswordReset)
	// Your own password: PermRead, because every role has one, and written as
	// passwordPath because requirePerm exempts exactly this path from the
	// forced-change bounce. Two strings that had to match would eventually not.
	admin("GET "+passwordPath, auth.PermRead, h.adminPasswordForm)
	// Rate limited for the same reason the login POST is: it verifies a secret.
	// Inside the session check rather than outside it, so the allowance is spent
	// by signed-in administrators rather than by anyone who can reach the door.
	admin("POST "+passwordPath, auth.PermRead, rateLimited(h.limits.login, h.adminPasswordChange))
}

// rateLimited puts a limiter in front of one handler, in the shape the route
// closure takes. The limiters are middleware because most of them wrap a route
// registered directly on the mux; this is the adapter for the ones registered
// through admin().
func rateLimited(limit middleware.Middleware, next http.HandlerFunc) http.HandlerFunc {
	return limit(next).ServeHTTP
}

// page is what every rendered page needs regardless of what it shows. It is
// embedded rather than repeated so that adding something universal — the CSRF
// token was exactly this — is one change, not one per page.
type page struct {
	Title     string
	StoreName string
	Currency  string
	CSRFToken string

	// BaseURL is the store's own address. Templates need it only where a link has
	// to work from somewhere else — the embedded catalog fragment renders inside
	// another origin's page, where a relative href would point at that origin.
	BaseURL string

	// FontCSSURL is a hosted font service's stylesheet, when one is configured, for
	// the default layout to link. Empty renders no link, which is the default: the
	// bundled theme uses the system font stack. Its origin is in the CSP's style-src
	// by the time it reaches here — config refuses to boot otherwise.
	FontCSSURL string

	// User is the signed-in administrator, and the zero User on every public page.
	// Templates ask Can rather than reading Role, so the answer comes from the
	// same map the routes are gated by.
	User auth.User
}

// Can reports whether the signed-in administrator holds a permission, so a
// template can leave out what their role could not do anyway.
//
// Presentation only. requirePerm is what actually refuses the request, and a
// page that hid a form from somebody who could still post to it would be a
// restriction on nothing but the mouse.
//
// An unknown permission is an error rather than a false, which is the whole
// reason for the second return: a mistyped name in a template would otherwise
// hide a button from everybody, for good, and look like a design decision.
func (p page) Can(perm string) (bool, error) {
	if !auth.Permission(perm).Valid() {
		return false, fmt.Errorf("page.Can: %q is not a permission", perm)
	}
	return p.User.Can(auth.Permission(perm)), nil
}

func (h *Handler) newPage(r *http.Request, title string) page {
	user, _ := middleware.AdminUser(r)
	// No template needs the hash, and every admin page would otherwise carry the
	// signed-in operator's argon2 hash in its render data — one careless
	// {{printf "%+v" .}} away from being on the page.
	user.PasswordHash = ""
	return page{
		Title:     title,
		StoreName: h.cfg.StoreName,
		Currency:  h.cfg.Currency,
		CSRFToken: nosurf.Token(r),
		BaseURL:   h.cfg.BaseURL,

		FontCSSURL: h.cfg.FontCSSURL,
		// Absent on every page outside RequireAdmin, which is the zero User: it
		// holds no permissions, so a public template asking Can gets false.
		User: user,
	}
}

type productsPage struct {
	page
	Products []catalog.Product
}

type productFormPage struct {
	page

	IsNew   bool
	Product catalog.Product
	Errors  validate.FormErrors

	// Categories is the whole taxonomy, so the form can offer every category as a
	// checkbox; Product.Categories decides which are ticked.
	Categories []catalog.Category

	// Variant form state. VariantErrorID names the existing variant whose edit
	// failed, so the message renders on that row; when it is empty the errors
	// belong to the add-a-variant form.
	VariantForm    variantForm
	VariantErrors  validate.FormErrors
	VariantErrorID string

	// Image upload state. UploadsEnabled is false when no object storage is
	// configured, in which case the page offers a pasted URL and says so rather
	// than showing a form that could only fail.
	UploadsEnabled bool
	AcceptTypes    string
	MaxUploadMB    int64

	// Download file state, rendered only for a digital product.
	// DownloadsEnabled is false when no private download store is configured, in
	// which case the page says so rather than offering an upload that could only
	// fail.
	DownloadsEnabled bool
	DownloadMaxLabel string
	Files            []catalog.File

	// KindLocked is set when this product's kind can no longer be changed, with
	// KindLockReason saying why. The form then shows the kind as text rather than
	// a select — a disabled control the server also refuses is two mechanisms
	// where one will do, and only the server's is load-bearing.
	KindLocked     bool
	KindLockReason string
}

// variantForm is the add-variant form's raw input, kept as typed so a
// rejected submission comes back with what was actually entered rather than a
// reformatted guess at it.
type variantForm struct {
	SKU string
	// Options are the variant's values in slot order, always OptionSlots long so a
	// re-rendered form can index them against the product's names without a
	// bounds check on every row.
	Options  [catalog.OptionSlots]string
	Price    string
	StockQty string
	Active   bool
}

func (h *Handler) adminProductList(w http.ResponseWriter, r *http.Request) {
	products, err := h.cat.List(r.Context())
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	h.render(w, r, http.StatusOK, "admin_products", productsPage{
		page:     h.newPage(r, "Products"),
		Products: products,
	})
}

func (h *Handler) adminProductNew(w http.ResponseWriter, r *http.Request) {
	cats, ok := h.categories(w, r)
	if !ok {
		return
	}
	h.render(w, r, http.StatusOK, "admin_product_form", h.productForm(r, catalog.Product{Active: true}, cats, true, nil))
}

func (h *Handler) adminProductCreate(w http.ResponseWriter, r *http.Request) {
	cats, ok := h.categories(w, r)
	if !ok {
		return
	}
	p, errs, ok := h.parseProduct(w, r, cats)
	if !ok {
		return
	}

	for field, msg := range validate.Product(p) {
		errs.Add(field, msg)
	}
	if errs.Any() {
		h.render(w, r, http.StatusUnprocessableEntity, "admin_product_form", h.productForm(r, p, cats, true, errs))
		return
	}

	created, err := h.cat.Create(r.Context(), p)
	if err != nil {
		if conflict, ok := errors.AsType[*catalog.ConflictError](err); ok {
			errs := validate.FormErrors{}
			errs.Add(conflict.Field, "Already used by another product.")
			h.render(w, r, http.StatusUnprocessableEntity, "admin_product_form", h.productForm(r, p, cats, true, errs))
			return
		}
		h.serverError(w, r, err)
		return
	}

	// Straight to the edit page: a product without variants cannot be bought,
	// so the next step is always adding one.
	http.Redirect(w, r, "/admin/products/"+created.ID+"/edit", http.StatusSeeOther)
}

func (h *Handler) adminProductEdit(w http.ResponseWriter, r *http.Request) {
	cats, ok := h.categories(w, r)
	if !ok {
		return
	}
	p, err := h.cat.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		h.storeError(w, r, err)
		return
	}
	form := h.productForm(r, p, cats, false, nil)
	h.attachFiles(w, r, &form)
	h.render(w, r, http.StatusOK, "admin_product_form", form)
}

func (h *Handler) adminProductUpdate(w http.ResponseWriter, r *http.Request) {
	cats, ok := h.categories(w, r)
	if !ok {
		return
	}
	p, errs, ok := h.parseProduct(w, r, cats)
	if !ok {
		return
	}
	p.ID = r.PathValue("id")

	// Nothing here has to defend the image any more: UpdateProduct does not write
	// either image column, so the form cannot touch the picture whatever it submits.
	// That replaced a read-then-preserve dance in this function.
	for field, msg := range validate.Product(p) {
		errs.Add(field, msg)
	}
	if errs.Any() {
		h.renderProductForm(w, r, http.StatusUnprocessableEntity, p, cats, errs)
		return
	}

	if _, err := h.cat.Update(r.Context(), p); err != nil {
		if conflict, ok := errors.AsType[*catalog.ConflictError](err); ok {
			errs := validate.FormErrors{}
			errs.Add(conflict.Field, "Already used by another product.")
			h.renderProductForm(w, r, http.StatusUnprocessableEntity, p, cats, errs)
			return
		}
		// The kind is frozen. The form usually renders it as text rather than a
		// select in this state, so reaching here means either a hand-crafted
		// request or a page rendered before the product was ordered — both of which
		// want the same explanation on the same form.
		if locked, ok := errors.AsType[*catalog.KindLockedError](err); ok {
			errs := validate.FormErrors{}
			if locked.Ordered {
				errs.Add("kind", "This product has been ordered, so its kind is fixed. "+
					"Deactivate it and create a new one instead.")
			} else {
				errs.Add("kind", fmt.Sprintf("Remove the %d attached file(s) first. Switching to a "+
					"physical product would leave them in storage with nothing listing them.", locked.Files))
			}
			// The submitted kind is refused, so the form must show the stored one —
			// otherwise the page argues with itself.
			if stored, err := h.cat.Get(r.Context(), p.ID); err == nil {
				p.Kind = stored.Kind
			}
			h.renderProductForm(w, r, http.StatusConflict, p, cats, errs)
			return
		}
		h.storeError(w, r, err)
		return
	}
	http.Redirect(w, r, "/admin/products/"+p.ID+"/edit", http.StatusSeeOther)
}

func (h *Handler) adminProductDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	err := h.cat.Delete(r.Context(), id)
	switch {
	case err == nil:
		http.Redirect(w, r, "/admin/products", http.StatusSeeOther)
	case errors.Is(err, catalog.ErrInUse):
		// The product has been ordered. Deleting it would rewrite history, so
		// the form says so instead of failing opaquely.
		cats, ok := h.categories(w, r)
		if !ok {
			return
		}
		p, getErr := h.cat.Get(r.Context(), id)
		if getErr != nil {
			h.storeError(w, r, getErr)
			return
		}
		errs := validate.FormErrors{}
		errs.Add("delete", "This product has been ordered and cannot be deleted. Deactivate it instead.")
		h.render(w, r, http.StatusConflict, "admin_product_form", h.productForm(r, p, cats, false, errs))
	default:
		h.storeError(w, r, err)
	}
}

func (h *Handler) adminVariantCreate(w http.ResponseWriter, r *http.Request) {
	productID := r.PathValue("id")
	v, form, errs, ok := h.parseVariant(w, r)
	if !ok {
		return
	}
	v.ProductID = productID

	if errs.Any() {
		h.renderVariantErrors(w, r, http.StatusUnprocessableEntity, productID, form, "", errs)
		return
	}

	if _, err := h.cat.CreateVariant(r.Context(), v); err != nil {
		if conflict, ok := errors.AsType[*catalog.ConflictError](err); ok {
			errs.Add(conflict.Field, conflictMessage(conflict.Field))
			h.renderVariantErrors(w, r, http.StatusUnprocessableEntity, productID, form, "", errs)
			return
		}
		h.storeError(w, r, err)
		return
	}
	http.Redirect(w, r, "/admin/products/"+productID+"/edit", http.StatusSeeOther)
}

func (h *Handler) adminVariantUpdate(w http.ResponseWriter, r *http.Request) {
	productID, variantID := r.PathValue("id"), r.PathValue("variantID")
	v, form, errs, ok := h.parseVariant(w, r)
	if !ok {
		return
	}
	v.ID, v.ProductID = variantID, productID

	if errs.Any() {
		h.renderVariantErrors(w, r, http.StatusUnprocessableEntity, productID, form, variantID, errs)
		return
	}

	if _, err := h.cat.UpdateVariant(r.Context(), v); err != nil {
		if conflict, ok := errors.AsType[*catalog.ConflictError](err); ok {
			errs.Add(conflict.Field, conflictMessage(conflict.Field))
			h.renderVariantErrors(w, r, http.StatusUnprocessableEntity, productID, form, variantID, errs)
			return
		}
		h.storeError(w, r, err)
		return
	}
	http.Redirect(w, r, "/admin/products/"+productID+"/edit", http.StatusSeeOther)
}

func (h *Handler) adminVariantDelete(w http.ResponseWriter, r *http.Request) {
	productID, variantID := r.PathValue("id"), r.PathValue("variantID")
	err := h.cat.DeleteVariant(r.Context(), productID, variantID)
	switch {
	case err == nil:
		http.Redirect(w, r, "/admin/products/"+productID+"/edit", http.StatusSeeOther)
	case errors.Is(err, catalog.ErrInUse):
		errs := validate.FormErrors{}
		errs.Add("sku", "This variant has been ordered and cannot be deleted. Deactivate it instead.")
		h.renderVariantErrors(w, r, http.StatusConflict, productID, variantForm{Active: true}, variantID, errs)
	default:
		h.storeError(w, r, err)
	}
}

// parseProduct reads the product form. A blank slug is derived from the title,
// because a slug is a detail of the URL, not a decision the operator has to
// make on every product.
//
// known is the taxonomy the form was rendered from; the submitted category ids
// are resolved against it, so an id that names nothing is a message on the form
// rather than a foreign key violation with no field attached.
func (h *Handler) parseProduct(w http.ResponseWriter, r *http.Request, known []catalog.Category) (catalog.Product, validate.FormErrors, bool) {
	if err := r.ParseForm(); err != nil {
		h.badForm(w, r)
		return catalog.Product{}, nil, false
	}
	p := catalog.Product{
		Slug:        strings.TrimSpace(r.PostFormValue("slug")),
		Title:       strings.TrimSpace(r.PostFormValue("title")),
		Description: strings.TrimSpace(r.PostFormValue("description")),
		// No image_url: the form does not offer one, and reading it here would be a
		// way to set it by hand-crafting a request. Images arrive by upload only.
		Active:      r.PostFormValue("active") != "",
		Kind:        catalog.Kind(strings.TrimSpace(r.PostFormValue("kind"))),
		Option1Name: strings.TrimSpace(r.PostFormValue("option1_name")),
		Option2Name: strings.TrimSpace(r.PostFormValue("option2_name")),
		Option3Name: strings.TrimSpace(r.PostFormValue("option3_name")),
	}
	if p.Slug == "" {
		p.Slug = catalog.Slugify(p.Title)
	}

	// A repeated field rather than one comma-separated value, because that is what
	// a checkbox list submits natively — no JavaScript, and no parsing of a format
	// somebody has to get right.
	chosen, errs := validate.ProductCategories(r.PostForm["category"], known)
	p.Categories = chosen
	return p, errs, true
}

// categories loads the taxonomy for a form that has to render it, answering the
// request itself if the read fails.
func (h *Handler) categories(w http.ResponseWriter, r *http.Request) ([]catalog.Category, bool) {
	cats, err := h.cat.Categories(r.Context())
	if err != nil {
		h.serverError(w, r, err)
		return nil, false
	}
	return cats, true
}

// parseVariant reads a variant form, returning both the parsed variant and the
// raw input, so a rejected form can be re-rendered exactly as it was typed.
func (h *Handler) parseVariant(w http.ResponseWriter, r *http.Request) (catalog.Variant, variantForm, validate.FormErrors, bool) {
	if err := r.ParseForm(); err != nil {
		h.badForm(w, r)
		return catalog.Variant{}, variantForm{}, nil, false
	}

	form := variantForm{
		SKU:      strings.TrimSpace(r.PostFormValue("sku")),
		Price:    strings.TrimSpace(r.PostFormValue("price")),
		StockQty: strings.TrimSpace(r.PostFormValue("stock_qty")),
		Active:   r.PostFormValue("active") != "",
	}
	for i := range form.Options {
		form.Options[i] = strings.TrimSpace(r.PostFormValue(fmt.Sprintf("option%d", i+1)))
	}
	v := catalog.Variant{
		SKU:     form.SKU,
		Option1: form.Options[0],
		Option2: form.Options[1],
		Option3: form.Options[2],
		Active:  form.Active,
	}

	errs := validate.FormErrors{}
	cents, err := catalog.ParsePrice(form.Price)
	if err != nil {
		errs.Add("price", "Enter an amount like 149.99.")
	}
	v.PriceCents = cents

	stock, err := strconv.Atoi(form.StockQty)
	if err != nil || stock < 0 {
		errs.Add("stock_qty", "Enter a whole number of items, 0 or more.")
	} else {
		v.StockQty = stock
	}

	for field, msg := range validate.Variant(v) {
		errs.Add(field, msg)
	}
	return v, form, errs, true
}

func (h *Handler) productForm(r *http.Request, p catalog.Product, cats []catalog.Category, isNew bool, errs validate.FormErrors) productFormPage {
	title := "Edit product"
	if isNew {
		title = "New product"
	}
	return productFormPage{
		page:           h.newPage(r, title),
		IsNew:          isNew,
		Product:        p,
		Categories:     cats,
		Errors:         errs,
		VariantForm:    variantForm{Active: true},
		UploadsEnabled: h.cfg.ImagesEnabled(),
		AcceptTypes:    strings.Join(blob.SupportedTypes(), ","),
		MaxUploadMB:    blob.MaxUploadBytes >> 20,

		DownloadsEnabled: h.cfg.DownloadsEnabled(),
		DownloadMaxLabel: catalog.HumanBytes(h.downloadMaxBytes()),
	}
}

// attachFiles loads a digital product's files onto the form, and works out
// whether its kind is still changeable.
//
// Both are reads the form needs and neither belongs in productForm, which is also
// used for a product that does not exist yet. A failure here is logged rather than
// answered: the form is usually being rendered *because* something else already
// went wrong, and replacing that message with a different one would lose it.
func (h *Handler) attachFiles(w http.ResponseWriter, r *http.Request, form *productFormPage) {
	if form.IsNew || form.Product.ID == "" {
		return
	}
	if form.Product.Digital() {
		files, err := h.cat.Files(r.Context(), form.Product.ID)
		if err != nil {
			h.logger(r).Error("could not read a product's files", "product", form.Product.ID, "error", err)
		}
		form.Files = files
	}
	form.KindLocked, form.KindLockReason = h.kindLock(r, form.Product)
}

// kindLock reports whether a product's kind is frozen, and why.
//
// The reasons are shown before the operator tries, because a select that silently
// refuses on submit is worse than a sentence saying it cannot be changed. The
// server refuses either way — catalog.Update checks the same two facts against the
// stored row — so this is presentation and not the guard.
func (h *Handler) kindLock(r *http.Request, p catalog.Product) (bool, string) {
	ordered, files, err := h.cat.KindChangeBlockers(r.Context(), p.ID)
	if err != nil {
		h.logger(r).Error("could not check whether a kind may change", "product", p.ID, "error", err)
		return false, ""
	}
	switch {
	case ordered > 0:
		return true, "This product has been ordered, so its kind is fixed. An order records how " +
			"something was actually fulfilled, and changing that afterwards would rewrite it. " +
			"Deactivate this product and create a new one instead."
	case p.Digital() && files > 0:
		return true, fmt.Sprintf("Remove the %d attached file(s) before changing this to a physical "+
			"product. Switching would leave them in storage with nothing listing them.", files)
	}
	return false, ""
}

// renderProductForm re-renders the edit page for a rejected product form,
// keeping the submitted values but showing the variants as stored.
func (h *Handler) renderProductForm(w http.ResponseWriter, r *http.Request, status int, p catalog.Product, cats []catalog.Category, errs validate.FormErrors) {
	variants, err := h.cat.Variants(r.Context(), p.ID)
	if err != nil {
		h.storeError(w, r, err)
		return
	}
	p.Variants = variants
	form := h.productForm(r, p, cats, false, errs)
	h.attachFiles(w, r, &form)
	h.render(w, r, status, "admin_product_form", form)
}

// renderVariantErrors re-renders the edit page after a variant form was
// rejected. The product is re-read, so everything except the offending form is
// shown as stored.
func (h *Handler) renderVariantErrors(w http.ResponseWriter, r *http.Request, status int, productID string, form variantForm, variantID string, errs validate.FormErrors) {
	cats, ok := h.categories(w, r)
	if !ok {
		return
	}
	p, err := h.cat.Get(r.Context(), productID)
	if err != nil {
		h.storeError(w, r, err)
		return
	}
	page := h.productForm(r, p, cats, false, nil)
	page.VariantForm = form
	page.VariantErrors = errs
	page.VariantErrorID = variantID
	h.attachFiles(w, r, &page)
	h.render(w, r, status, "admin_product_form", page)
}

func conflictMessage(field string) string {
	if field == "options" {
		return "Another variant of this product already has those options."
	}
	return "Already used by another variant."
}

// render writes a page, and turns a template that will not execute into a 500.
//
// It used to log the failure and return, which left the response at 200 with an
// empty body — the comment said the status line might already have been written,
// and that was not true: Templates.Execute renders to bytes and writes nothing, so
// at this point the status is still ours to choose. An adopter's broken override
// is now a server error that says so, rather than a blank page that claims success.
func (h *Handler) render(w http.ResponseWriter, r *http.Request, status int, name string, data any) {
	body, err := h.tmpl.Execute(name, data)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if _, err := w.Write(body); err != nil {
		h.logger(r).Error("writing a page failed", "template", name, "path", r.URL.Path, "error", err)
	}
}

// storeError maps a catalog error onto a response: missing rows are 404s, and
// anything else is a genuine server fault.
func (h *Handler) storeError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, catalog.ErrNotFound) {
		h.notFound(w, r)
		return
	}
	h.serverError(w, r, err)
}

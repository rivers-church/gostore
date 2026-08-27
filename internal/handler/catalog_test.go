package handler

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/17xande-dev/gostore/internal/auth"
	"github.com/17xande-dev/gostore/internal/blob"
	"github.com/17xande-dev/gostore/internal/cart"
	"github.com/17xande-dev/gostore/internal/catalog"
	"github.com/17xande-dev/gostore/internal/config"
	"github.com/17xande-dev/gostore/internal/dbtest"
	"github.com/17xande-dev/gostore/internal/downloads"
	"github.com/17xande-dev/gostore/internal/email"
	"github.com/17xande-dev/gostore/internal/middleware"
	"github.com/17xande-dev/gostore/internal/orders"
	"github.com/17xande-dev/gostore/internal/payment"
)

// newStorefront mirrors how main.go mounts the public routes: security headers
// around everything, CORS from config on the catalog itself, and no session or
// CSRF layer, because nothing here changes state.
func newStorefront(t *testing.T, cfg config.Config, templateDir string) (*httptest.Server, *catalog.Store) {
	t.Helper()

	pool := dbtest.Pool(t)
	store := catalog.NewStore(pool)
	images := blob.NewFake()
	tmpl, err := ParseTemplates(templateDir, images)
	if err != nil {
		t.Fatalf("ParseTemplates: %v", err)
	}
	gateway := payment.NewFake()
	h := New(Deps{
		Config: cfg, Log: slog.New(slog.DiscardHandler), Tmpl: tmpl,
		Catalog: store, Carts: cart.NewStore(pool), Orders: orders.NewStore(pool),
		Grants:  downloads.NewStore(pool, store),
		Gateway: gateway, Mail: email.NewFake(), Images: images,
		Files: blob.NewFakeDownloads(), Users: auth.NewStore(pool),
	})

	mux := http.NewServeMux()
	h.RegisterStorefront(mux)

	srv := httptest.NewServer(middleware.Chain(mux, middleware.SecurityHeaders(middleware.Policy{
		FrameAncestors: cfg.EmbedOrigins,
		FormActions:    []string{gateway.FormActionOrigin()},
	})))
	srv.Client().CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	t.Cleanup(srv.Close)
	return srv, store
}

// stock puts one buyable product in the catalog, plus things that must not
// appear on the storefront.
func stock(t *testing.T, store *catalog.Store) catalog.Product {
	t.Helper()
	ctx := t.Context()

	p, err := store.Create(ctx, catalog.Product{
		Slug: "sample-tee", Title: "Sample Tee",
		Description: "A demo garment.", Active: true,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for _, v := range []catalog.Variant{
		{ProductID: p.ID, SKU: "TEE-S", Option1: "S", PriceCents: 29900, StockQty: 4, Active: true},
		{ProductID: p.ID, SKU: "TEE-M", Option1: "M", PriceCents: 31900, StockQty: 0, Active: true},
		{ProductID: p.ID, SKU: "TEE-L", Option1: "L", PriceCents: 99900, StockQty: 9, Active: false},
	} {
		if _, err := store.CreateVariant(ctx, v); err != nil {
			t.Fatalf("CreateVariant: %v", err)
		}
	}

	hidden, err := store.Create(ctx, catalog.Product{Slug: "unpublished", Title: "Unpublished Draft", Active: false})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := store.CreateVariant(ctx, catalog.Variant{ProductID: hidden.ID, SKU: "DRAFT-1", PriceCents: 100, StockQty: 1, Active: true}); err != nil {
		t.Fatalf("CreateVariant: %v", err)
	}
	return p
}

func TestStorefront_ProductsPage(t *testing.T) {
	srv, store := newStorefront(t, testConfig(), "")

	res, body := get(t, srv, "/products")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /products = %d", res.StatusCode)
	}
	if !strings.Contains(body, "Nothing for sale yet") {
		t.Error("an empty catalog does not say so")
	}

	stock(t, store)
	_, body = get(t, srv, "/products")

	for _, want := range []string{
		"Sample Tee",
		`href="/products/sample-tee"`,
		"ZAR 299.00 – 319.00", // the range spans the active variants only
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the catalog page is missing %q", want)
		}
	}
	// An inactive product is not merely unlinked, it is absent.
	if strings.Contains(body, "Unpublished Draft") || strings.Contains(body, "unpublished") {
		t.Error("an inactive product is listed on the storefront")
	}
	// 999.00 is the inactive variant's price; if it appears, the range is wrong.
	if strings.Contains(body, "999.00") {
		t.Error("an inactive variant is priced into the listing")
	}
}

func TestStorefront_ProductDetail(t *testing.T) {
	srv, store := newStorefront(t, testConfig(), "")
	stock(t, store)

	res, body := get(t, srv, "/products/sample-tee")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /products/sample-tee = %d", res.StatusCode)
	}
	for _, want := range []string{"Sample Tee", "A demo garment.", "ZAR 299.00", "In stock", "Sold out"} {
		if !strings.Contains(body, want) {
			t.Errorf("the detail page is missing %q", want)
		}
	}
	if strings.Contains(body, "TEE-L") || strings.Contains(body, "999.00") {
		t.Error("the inactive variant is shown")
	}

	// The first-party page carries the add-to-cart form...
	if !strings.Contains(body, `action="/cart/items"`) {
		t.Error("the product page has no add-to-cart form")
	}
	// ...and the fragment served to an embedder does not. A cart form on another
	// origin could not work anyway: SameSite=Lax withholds the cookie and the
	// CSRF origin check would refuse the post.
	fragment := getWith(t, srv, "/products/sample-tee", http.Header{"HX-Request": {"true"}})
	if strings.Contains(fragment, "<form") {
		t.Errorf("the cross-origin detail fragment contains a form: %s", fragment)
	}
	if !strings.Contains(fragment, "Sample Tee") {
		t.Error("the fragment lost its content")
	}
}

func TestStorefront_UnknownOrHiddenProductIs404(t *testing.T) {
	srv, store := newStorefront(t, testConfig(), "")
	stock(t, store)

	for _, slug := range []string{"no-such-product", "unpublished"} {
		res, _ := get(t, srv, "/products/"+slug)
		if res.StatusCode != http.StatusNotFound {
			t.Errorf("GET /products/%s = %d, want 404", slug, res.StatusCode)
		}
	}
}

func TestStorefront_HTMXGetsAFragment(t *testing.T) {
	srv, store := newStorefront(t, testConfig(), "")
	stock(t, store)

	for _, path := range []string{"/products", "/products/sample-tee"} {
		full := getWith(t, srv, path, nil)
		if !strings.Contains(full, "<html") {
			t.Errorf("GET %s: a plain request did not get a whole document", path)
		}

		fragment := getWith(t, srv, path, http.Header{"HX-Request": {"true"}})
		if strings.Contains(fragment, "<html") || strings.Contains(fragment, "<head") {
			t.Errorf("GET %s with HX-Request returned a whole document", path)
		}
		if !strings.Contains(fragment, "Sample Tee") {
			t.Errorf("GET %s fragment is missing the content: %s", path, fragment)
		}

		// A boosted link is a navigation: the browser is replacing the document,
		// so it needs the whole thing back.
		boosted := getWith(t, srv, path, http.Header{"HX-Request": {"true"}, "HX-Boosted": {"true"}})
		if !strings.Contains(boosted, "<html") {
			t.Errorf("GET %s with HX-Boosted did not get a whole document", path)
		}
	}
}

func TestStorefront_EmbeddedRequestsSetNoCookies(t *testing.T) {
	// The whole embedding design rests on this: a catalog fetched from another
	// origin's page must set no cookie of any kind, so the response is cacheable
	// and carries nothing into the embedder's context.
	cfg := testConfig()
	cfg.EmbedOrigins = []string{"https://cms.example"}
	srv, store := newStorefront(t, cfg, "")
	stock(t, store)

	embedded := http.Header{"Origin": {"https://cms.example"}, "HX-Request": {"true"}}
	for _, path := range []string{"/products", "/products/sample-tee", "/static/htmx.min.js"} {
		res := getResponse(t, srv, path, embedded)
		if cookies := res.Cookies(); len(cookies) != 0 {
			t.Errorf("embedded GET %s set cookies: %v", path, cookies)
		}
	}
}

func TestStorefront_FirstPartyPagesCarryOnlyACSRFCookie(t *testing.T) {
	// A first-party visit does go through the CSRF layer, because the product
	// page carries an add-to-cart form and a form needs a token. What it must
	// never pick up here is a cart cookie: carts begin when something is added.
	srv, store := newStorefront(t, testConfig(), "")
	stock(t, store)

	for _, path := range []string{"/products", "/products/sample-tee"} {
		res, _ := get(t, srv, path)
		for _, c := range res.Cookies() {
			switch c.Name {
			case "csrf_token": // expected
			case CartCookieName:
				t.Errorf("GET %s issued a cart cookie", path)
			default:
				t.Errorf("GET %s set an unexpected cookie %q", path, c.Name)
			}
		}
	}

	// Static assets are outside all of it: nothing to protect, nothing to set.
	res, _ := get(t, srv, "/static/htmx.min.js")
	if cookies := res.Cookies(); len(cookies) != 0 {
		t.Errorf("the vendored asset set cookies: %v", cookies)
	}
}

func TestStorefront_CORSFollowsConfig(t *testing.T) {
	cfg := testConfig()
	cfg.EmbedOrigins = []string{"https://cms.example"}
	srv, store := newStorefront(t, cfg, "")
	stock(t, store)

	res := getResponse(t, srv, "/products", http.Header{"Origin": {"https://cms.example"}})
	if got := res.Header.Get("Access-Control-Allow-Origin"); got != "https://cms.example" {
		t.Errorf("allowed origin: Access-Control-Allow-Origin = %q", got)
	}
	// And the CSP has to permit framing by the same origin, or an iframe embed
	// is blocked even though the fetch is allowed.
	if csp := res.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors https://cms.example") {
		t.Errorf("CSP does not allow framing by the embed origin: %s", csp)
	}

	res = getResponse(t, srv, "/products", http.Header{"Origin": {"https://evil.example"}})
	if got := res.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("unlisted origin got Access-Control-Allow-Origin = %q", got)
	}
}

func TestStorefront_EmbeddingOffByDefault(t *testing.T) {
	srv, store := newStorefront(t, testConfig(), "") // no EmbedOrigins
	stock(t, store)

	res := getResponse(t, srv, "/products", http.Header{"Origin": {"https://cms.example"}})
	if got := res.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Access-Control-Allow-Origin = %q with embedding unconfigured", got)
	}
	if csp := res.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Errorf("CSP allows framing by default: %s", csp)
	}
}

func TestStorefront_TemplateOverride(t *testing.T) {
	// Theming and embedding are validated together, because they are the two
	// things an adopter does to this layer before anything else.
	dir := t.TempDir()
	writeOverride(t, dir, "pages/products.gohtml",
		`{{define "content"}}{{template "products_list" .}}{{end}}`+
			`{{define "products_list"}}<p>OUR OWN CATALOG: {{len .Products}} item(s)</p>{{end}}`)

	srv, store := newStorefront(t, testConfig(), dir)
	stock(t, store)

	// The overridden fragment replaces the default...
	fragment := getWith(t, srv, "/products", http.Header{"HX-Request": {"true"}})
	if !strings.Contains(fragment, "OUR OWN CATALOG: 1 item(s)") {
		t.Errorf("the override did not take effect: %s", fragment)
	}
	// ...including inside the full page, which still comes from the defaults.
	full := getWith(t, srv, "/products", nil)
	if !strings.Contains(full, "OUR OWN CATALOG") || !strings.Contains(full, "<html") {
		t.Errorf("the full page did not compose the override: %s", full)
	}
	// A template the override says nothing about is untouched.
	if detail := getWith(t, srv, "/products/sample-tee", nil); !strings.Contains(detail, "Sample Tee") {
		t.Error("the un-overridden detail page broke")
	}
}

func TestStorefront_ServesVendoredHTMX(t *testing.T) {
	srv, _ := newStorefront(t, testConfig(), "")

	res, body := get(t, srv, "/static/htmx.min.js")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /static/htmx.min.js = %d", res.StatusCode)
	}
	if !strings.Contains(res.Header.Get("Content-Type"), "javascript") {
		t.Errorf("Content-Type = %q", res.Header.Get("Content-Type"))
	}
	if !strings.Contains(body, "htmx") {
		t.Error("the served file does not look like htmx")
	}
	etag := res.Header.Get("ETag")
	if etag == "" {
		t.Fatal("no ETag, so the immutable Cache-Control below is a lie")
	}
	if !strings.Contains(res.Header.Get("Cache-Control"), "immutable") {
		t.Errorf("Cache-Control = %q", res.Header.Get("Cache-Control"))
	}

	// The page must reference the content-addressed URL, or the immutable
	// caching would pin an old copy forever.
	_, page := get(t, srv, "/products")
	if !strings.Contains(page, "/static/htmx.min.js?v=") {
		t.Error("the page does not reference the hashed asset URL")
	}

	// A conditional request is answered 304, which is the point of the ETag.
	res = getResponse(t, srv, "/static/htmx.min.js", http.Header{"If-None-Match": {etag}})
	if res.StatusCode != http.StatusNotModified {
		t.Errorf("conditional GET = %d, want 304", res.StatusCode)
	}

	// Files in the static directory that are not on the served list stay
	// unreachable, so leaving notes there cannot publish them.
	if res, _ := get(t, srv, "/static/README.md"); res.StatusCode != http.StatusNotFound {
		t.Errorf("GET /static/README.md = %d, want 404", res.StatusCode)
	}
}

// getWith returns the body of a GET carrying extra headers.
func getWith(t *testing.T, srv *httptest.Server, path string, header http.Header) string {
	t.Helper()
	_, body := doGet(t, srv, path, header)
	return body
}

func getResponse(t *testing.T, srv *httptest.Server, path string, header http.Header) *http.Response {
	t.Helper()
	res, _ := doGet(t, srv, path, header)
	return res
}

func doGet(t *testing.T, srv *httptest.Server, path string, header http.Header) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+path, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	return do(t, srv, req)
}

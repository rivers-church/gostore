package handler

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/17xande-dev/gostore/internal/auth"
	"github.com/17xande-dev/gostore/internal/blob"
	"github.com/17xande-dev/gostore/internal/cart"
	"github.com/17xande-dev/gostore/internal/catalog"
	"github.com/17xande-dev/gostore/internal/config"
	"github.com/17xande-dev/gostore/internal/dbtest"
	"github.com/17xande-dev/gostore/internal/downloads"
	"github.com/17xande-dev/gostore/internal/middleware"
	"github.com/17xande-dev/gostore/internal/orders"
	"github.com/17xande-dev/gostore/internal/payment"
	"github.com/17xande-dev/mailer"
)

// The invariant these tests defend: an error response either fills the target it
// was aimed at, or replaces the document. htmx is configured to swap error
// responses — without that, every refusal the store sends is discarded — and the
// price of that is that a whole page must never be sent to a fragment's target
// without saying so.

func TestHTMXError_RefusalFillsItsOwnTarget(t *testing.T) {
	// The bug this was written for: the cart answers "sold out" as a 409 carrying
	// the fragment the page asked for. htmx's default is to swap nothing on a 4xx,
	// so the message was sent and silently dropped — the shopper clicked add-to-cart
	// and watched nothing happen.
	//
	// This half asserts the server's side: the response is the fragment, aimed at
	// the target the request named, and it does *not* retarget the document.
	srv, _, variants := shopper(t)

	res, body := postHTMX(t, srv, "/cart/items",
		url.Values{"variant_id": {variants["M"].ID}, "quantity": {"2"}})
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("adding 2 of 1 = %d, want 409", res.StatusCode)
	}
	if !strings.Contains(body, "Only 1 of that option left") {
		t.Errorf("the refusal is missing its message: %s", excerpt(body))
	}
	if strings.Contains(body, "<html") {
		t.Error("the refusal is a whole document, which would replace the page")
	}
	if got := res.Header.Get("HX-Retarget"); got != "" {
		t.Errorf("HX-Retarget = %q; a refusal belongs in the target that asked for it", got)
	}
}

func TestHTMXError_PageReplacesTheDocument(t *testing.T) {
	// The other half. A full error page sent to an htmx request must say "replace
	// the document", or — now that error responses swap — it would be pasted into
	// whatever small target the request named, like the cart-count span.
	srv, _ := newStorefront(t, testConfig(), "")

	req, err := http.NewRequest("GET", srv.URL+"/nope", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("HX-Request", "true")
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("GET /nope: %v", err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /nope = %d", res.StatusCode)
	}
	if got := res.Header.Get("HX-Retarget"); got != "body" {
		t.Errorf("HX-Retarget = %q, want body", got)
	}
	if got := res.Header.Get("HX-Reswap"); got != "innerHTML" {
		t.Errorf("HX-Reswap = %q, want innerHTML", got)
	}
}

func TestHTMXConfig_SwapsErrorResponses(t *testing.T) {
	// The configuration half of the same invariant. Without this entry htmx drops
	// every 4xx body, and both the tests above would pass while the shopper still
	// saw nothing.
	srv, _ := newStorefront(t, testConfig(), "")

	_, body := get(t, srv, "/products")
	if !strings.Contains(body, "responseHandling") {
		t.Fatal("the htmx config does not mention responseHandling")
	}
	if !strings.Contains(body, `{"code":"[45]..","swap":true`) {
		t.Errorf("htmx is not configured to swap error responses:\n%s",
			between(t, body, `<meta name="htmx-config"`, ">"))
	}
}

func TestErrorPages_StatusByStatus(t *testing.T) {
	srv, store := newStorefront(t, testConfig(), "")
	stock(t, store)

	t.Run("405 names the methods that do work", func(t *testing.T) {
		req, err := http.NewRequest("POST", srv.URL+"/products", nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		res, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("POST /products: %v", err)
		}
		defer res.Body.Close()

		if res.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("POST /products = %d, want 405", res.StatusCode)
		}
		allow := res.Header.Get("Allow")
		for _, want := range []string{"GET", "HEAD", "OPTIONS"} {
			if !strings.Contains(allow, want) {
				t.Errorf("Allow = %q, missing %s", allow, want)
			}
		}
		if strings.Contains(allow, "POST") {
			t.Errorf("Allow = %q names the method that was just refused", allow)
		}
	})

	t.Run("an unknown path is still 404, not 405", func(t *testing.T) {
		// The failure mode of asking the mux which methods a path allows: if the
		// probe counted the catch-all itself, every 404 would become a 405.
		res, body := get(t, srv, "/no/such/path")
		if res.StatusCode != http.StatusNotFound {
			t.Errorf("GET /no/such/path = %d, want 404", res.StatusCode)
		}
		if !strings.Contains(body, "Page not found") {
			t.Error("an unknown path did not get the 404 page")
		}
	})
}

func TestErrorPages_CSRFFailureIsAPage(t *testing.T) {
	srv, _ := newServer(t)

	// An empty token, which is what post() treats as "control it yourself".
	res, body := post(t, srv, "/cart/items",
		url.Values{"csrf_token": {""}, "variant_id": {"x"}})

	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("a tokenless post = %d, want 403", res.StatusCode)
	}
	if !strings.Contains(body, "That form has expired") {
		t.Errorf("the 403 is not the error page:\n%s", excerpt(body))
	}
}

func TestErrorPages_DetailShownInDevAndHiddenInProduction(t *testing.T) {
	// Both directions, because asserting only the development case would pass just
	// as well with the flag ignored entirely.
	cases := []struct {
		name    string
		baseURL string
		want    bool
	}{
		{"development", "http://localhost:8080", true},
		{"production", "https://shop.example", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.BaseURL = tc.baseURL
			cfg.CookieSecure = strings.HasPrefix(tc.baseURL, "https://")
			cfg.ShowErrorDetail = !cfg.CookieSecure

			// A closed pool, so any catalog read is a real fault and reaches
			// serverError rather than a validation path.
			srv := brokenStorefront(t, cfg)
			res, body := get(t, srv, "/products")
			if res.StatusCode != http.StatusInternalServerError {
				t.Fatalf("a broken read = %d, want 500", res.StatusCode)
			}

			hasDetail := strings.Contains(body, "Development detail")
			if hasDetail != tc.want {
				t.Errorf("detail shown = %v, want %v (BASE_URL %s)", hasDetail, tc.want, tc.baseURL)
			}
			// The reference is shown either way: it is what makes a production error
			// reportable at all.
			if !strings.Contains(body, "Reference") {
				t.Error("the error page carries no reference")
			}
		})
	}
}

func TestConfig_ShowErrorDetailFollowsBaseURL(t *testing.T) {
	// The derivation itself, without a server in the way.
	t.Setenv("DATABASE_URL", "postgres://x/y")
	t.Setenv("PAYFAST_MERCHANT_ID", "10000100")
	t.Setenv("PAYFAST_MERCHANT_KEY", "46f0cd694581a")
	// Images and mail are required, and this test is about neither — it just has
	// to get past Load.
	t.Setenv("IMAGE_DIR", t.TempDir())
	t.Setenv("SMTP_HOST", "localhost")
	t.Setenv("EMAIL_FROM", "orders@example.com")

	for baseURL, wantDetail := range map[string]bool{
		"http://localhost:8080": true,
		"https://shop.example":  false,
	} {
		t.Setenv("BASE_URL", baseURL)
		cfg, err := config.Load()
		if err != nil {
			t.Fatalf("Load with BASE_URL=%s: %v", baseURL, err)
		}
		if cfg.ShowErrorDetail != wantDetail {
			t.Errorf("BASE_URL=%s: ShowErrorDetail = %v, want %v", baseURL, cfg.ShowErrorDetail, wantDetail)
		}
		// The same signal, so they must never disagree.
		if cfg.ShowErrorDetail == cfg.CookieSecure {
			t.Errorf("BASE_URL=%s: ShowErrorDetail and CookieSecure agree; they are opposites", baseURL)
		}
	}
}

func TestRequestID_IsEchoedAndShownOnTheErrorPage(t *testing.T) {
	srv := brokenStorefront(t, testConfig())

	res, body := get(t, srv, "/products")
	if res.StatusCode != http.StatusInternalServerError {
		t.Fatalf("a broken read = %d, want 500", res.StatusCode)
	}

	id := res.Header.Get(middleware.Header)
	if id == "" {
		t.Fatal("no request id was echoed")
	}
	// The page and the header must name the same request, or the reference a
	// customer quotes leads nowhere.
	if !strings.Contains(body, id) {
		t.Errorf("the page does not carry the id %q it was served under", id)
	}
}

func TestRequestID_AdoptsAnIncomingID(t *testing.T) {
	srv := brokenStorefront(t, testConfig())

	cases := map[string]struct{ header, value, want string }{
		"cloud trace": {"X-Cloud-Trace-Context", "abc123def456/9876;o=1", "abc123def456"},
		"request id":  {"X-Request-Id", "from-the-proxy", "from-the-proxy"},
		"sanitised":   {"X-Request-Id", "bad id; with <junk>", "badidwithjunk"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			req, err := http.NewRequest("GET", srv.URL+"/products", nil)
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			req.Header.Set(tc.header, tc.value)
			res, err := srv.Client().Do(req)
			if err != nil {
				t.Fatalf("GET /products: %v", err)
			}
			defer res.Body.Close()

			if got := res.Header.Get(middleware.Header); got != tc.want {
				t.Errorf("request id = %q, want %q", got, tc.want)
			}
		})
	}

	// With neither header, one is minted rather than left empty.
	res, _ := get(t, srv, "/products")
	if got := res.Header.Get(middleware.Header); len(got) != 16 {
		t.Errorf("generated id = %q, want 16 hex characters", got)
	}
}

// brokenStorefront is a storefront whose database is closed, so every catalog
// read is a genuine fault. It is the only honest way to exercise the 500 path:
// the handlers have no other way to fail on demand, and a fake that returned an
// error would test the fake.
//
// Request IDs are wired here as main.go wires them, because the reference on the
// error page is half of what these tests are about.
func brokenStorefront(t *testing.T, cfg config.Config) *httptest.Server {
	t.Helper()

	pool := dbtest.Pool(t)
	store := catalog.NewStore(pool)
	images := blob.NewFake()
	tmpl, err := ParseTemplates("", images)
	if err != nil {
		t.Fatalf("ParseTemplates: %v", err)
	}
	gateway := payment.NewFake()
	h := New(Deps{
		Config: cfg, Log: slog.New(slog.DiscardHandler), Tmpl: tmpl,
		Catalog: store, Carts: cart.NewStore(pool), Orders: orders.NewStore(pool),
		Grants:  downloads.NewStore(pool, store),
		Gateway: gateway, Mail: mailer.NewFake(), Images: images,
		Files: blob.NewFakeDownloads(), Users: auth.NewStore(pool),
	})

	mux := http.NewServeMux()
	h.RegisterStorefront(mux)
	srv := httptest.NewServer(middleware.Chain(mux, middleware.RequestID))
	srv.Client().CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	t.Cleanup(srv.Close)

	// Closed after the server is built, so construction succeeds and only the
	// queries fail — which is what a database that goes away mid-life looks like.
	pool.Close()
	return srv
}

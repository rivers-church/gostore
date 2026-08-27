package handler

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/17xande-dev/gostore/internal/config"
	"github.com/17xande-dev/gostore/internal/payment"
)

// The hardening phase's assertions about the running server, as opposed to the
// units underneath it. Every one of these is something that would be silently
// wrong rather than obviously broken.

func TestHardening_LoginIsRateLimited(t *testing.T) {
	// Two a minute, so the burst is the floor of 2.
	s := newStore(t, func(c *config.Config) { c.RateLimits.LoginPerMinute = 2 })

	var limited bool
	for range 6 {
		res, _ := post(t, s.srv, "/admin/login", url.Values{"password": {"wrong"}})
		switch res.StatusCode {
		case http.StatusUnauthorized: // a refused attempt, as expected
		case http.StatusTooManyRequests:
			limited = true
			if res.Header.Get("Retry-After") == "" {
				t.Error("a rate-limited login has no Retry-After")
			}
		default:
			t.Fatalf("login attempt = %d", res.StatusCode)
		}
	}
	if !limited {
		t.Error("six login attempts against a limit of two a minute were all served")
	}

	// The form itself keeps working: limiting the GET would lock an operator out of
	// the page they need to read the message on.
	if res, _ := get(t, s.srv, "/admin/login"); res.StatusCode != http.StatusOK {
		t.Errorf("GET /admin/login = %d after the POST limit was hit, want 200", res.StatusCode)
	}
}

func TestHardening_CheckoutIsRateLimited(t *testing.T) {
	s := newCheckoutShop(t, func(c *config.Config) { c.RateLimits.CheckoutPerMinute = 2 })
	addToCart(t, s.srv, s.variants["S"].ID, 1)

	var limited bool
	for range 6 {
		res, _ := post(t, s.srv, "/cart/checkout", validCheckoutForm())
		if res.StatusCode == http.StatusTooManyRequests {
			limited = true
		}
	}
	if !limited {
		t.Error("six checkouts against a limit of two a minute were all served")
	}
}

func TestHardening_CallbackIsRateLimitedAndAsksForARetry(t *testing.T) {
	// This is the surface the limiter exists for: unauthenticated, and every accepted
	// request makes the store POST to the gateway to validate it.
	s := newCheckoutShop(t, func(c *config.Config) { c.RateLimits.CallbackPerMinute = 2 })
	order := placeOrder(t, s, "S", 1)
	body := payment.FakeCallbackBody(order.ID, "1089250", "PENDING", order.TotalCents)

	var limited bool
	for range 8 {
		res := callback(t, s.srv, "fake", body)
		if res.StatusCode == http.StatusTooManyRequests {
			limited = true
			// A throttled notification *must* be retried, so it cannot be answered
			// 200 — that would tell the gateway it had been processed. This is the
			// one place the handler's always-200 rule does not apply, because the
			// request was never read.
			if res.Header.Get("Retry-After") == "" {
				t.Error("a throttled callback has no Retry-After, so the gateway must guess")
			}
		}
	}
	if !limited {
		t.Fatal("eight callbacks against a limit of two a minute were all served")
	}
}

func TestHardening_UnlimitedByDefaultInTests(t *testing.T) {
	// The rest of the suite depends on this: a zero limit means the surface is
	// unlimited, so no existing test is quietly throttled into failing.
	s := newStore(t)
	for i := range 12 {
		res, _ := post(t, s.srv, "/admin/login", url.Values{"password": {"wrong"}})
		if res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d with limits unset, want 401", i, res.StatusCode)
		}
	}
}

func TestHardening_OversoldOrderIsFlaggedInTheAdmin(t *testing.T) {
	// Until this phase the oversell existed only in the logs and in the owner's
	// email — an email is read once and a log is not read at all.
	s := newCheckoutShop(t)
	order := placeOrder(t, s, "S", 2)

	v := s.variants["S"]
	v.StockQty = 1
	if _, err := s.catalog.UpdateVariant(t.Context(), v); err != nil {
		t.Fatalf("UpdateVariant: %v", err)
	}
	callback(t, s.srv, "fake", payment.FakeCallbackBody(order.ID, "1089250", "paid", order.TotalCents))

	stored := s.reload(t, order.ID)
	if !stored.Paid() {
		t.Fatal("the order was not recorded paid")
	}
	if !stored.Oversold {
		t.Fatal("the order is not flagged oversold")
	}

	signIn(t, s.srv)
	// The flag is matched without regard to case: it is a state badge, and its
	// casing is a styling decision that has already changed once. The prose below
	// it is matched as written, because that is the explanation an operator acts
	// on rather than a label.
	_, list := get(t, s.srv, "/admin/orders")
	if !strings.Contains(strings.ToLower(list), "oversold") {
		t.Errorf("the order list does not flag it: %s", list)
	}
	_, page := get(t, s.srv, "/admin/orders/"+order.ID)
	if !strings.Contains(strings.ToLower(page), "oversold") {
		t.Errorf("the order page does not flag it: %s", page)
	}
	for _, want := range []string{"not enough stock", "fulfil this late or refund"} {
		if !strings.Contains(page, want) {
			t.Errorf("the order page is missing %q", want)
		}
	}
}

func TestHardening_OrdinaryOrderIsNotFlagged(t *testing.T) {
	s := newCheckoutShop(t)
	order := placeOrder(t, s, "S", 2)
	callback(t, s.srv, "fake", payment.FakeCallbackBody(order.ID, "1089250", "paid", order.TotalCents))

	if s.reload(t, order.ID).Oversold {
		t.Error("an order with stock to spare was flagged oversold")
	}
	signIn(t, s.srv)
	if _, list := get(t, s.srv, "/admin/orders"); strings.Contains(list, "OVERSOLD") {
		t.Error("the order list flags an order that is fine")
	}
}

func TestHardening_PaymentCallbackIsOutsideCSRFAndSetsNoCookie(t *testing.T) {
	// The plan asked for this to be verified explicitly rather than assumed. A
	// gateway cannot carry a CSRF token, and it is not a browser, so it should be
	// issued nothing.
	s := newCheckoutShop(t)
	order := placeOrder(t, s, "S", 1)

	res := callback(t, s.srv, "fake",
		payment.FakeCallbackBody(order.ID, "1089250", "paid", order.TotalCents))

	if res.StatusCode == http.StatusForbidden {
		t.Fatal("the callback is inside the CSRF group, so no gateway could reach it")
	}
	if cookies := res.Cookies(); len(cookies) != 0 {
		t.Errorf("the callback response set cookies: %v", cookies)
	}
	// nosurf's token cookie in particular: its presence would mean the route had
	// been moved inside the group.
	for _, c := range res.Cookies() {
		if c.Name == "csrf_token" {
			t.Error("the callback picked up a CSRF cookie")
		}
	}
	if !s.reload(t, order.ID).Paid() {
		t.Error("a notification with no CSRF token was not applied")
	}
}

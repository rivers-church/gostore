package handler

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/17xande-dev/gostore/internal/payment"
)

// paidOrder runs a whole checkout and pays it, so the admin tests read an order
// that came into existence the way real ones do rather than one inserted by hand.
func paidOrder(t *testing.T, s *shop) string {
	t.Helper()
	order := placeOrder(t, s, "S", 2)
	callback(t, s.srv, "fake", payment.FakeCallbackBody(order.ID, "1089250", "paid", order.TotalCents))
	return order.ID
}

func TestAdminOrders_RequiresASession(t *testing.T) {
	s := newCheckoutShop(t)
	id := paidOrder(t, s)

	// The order pages carry a customer's name, address and phone number, so an
	// unauthenticated request must not see any of it.
	for _, path := range []string{"/admin/orders", "/admin/orders/" + id} {
		res, body := get(t, s.srv, path)
		if res.StatusCode != http.StatusSeeOther {
			t.Errorf("GET %s unauthenticated = %d, want 303", path, res.StatusCode)
		}
		if got := res.Header.Get("Location"); got != "/admin/login" {
			t.Errorf("GET %s redirected to %q, want /admin/login", path, got)
		}
		if strings.Contains(body, "Jane Doe") || strings.Contains(body, "1 Example Road") {
			t.Errorf("GET %s leaked customer details to an unauthenticated request", path)
		}
	}
}

func TestAdminOrders_ListsOrders(t *testing.T) {
	s := newCheckoutShop(t)

	signIn(t, s.srv)
	if _, body := get(t, s.srv, "/admin/orders"); !strings.Contains(body, "No orders yet") {
		t.Errorf("an empty order list does not say so: %s", body)
	}

	id := paidOrder(t, s)
	order := s.reload(t, id)

	res, body := get(t, s.srv, "/admin/orders")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /admin/orders = %d", res.StatusCode)
	}
	for _, want := range []string{
		order.Reference(),
		"Jane Doe",
		"jane@example.com",
		"ZAR 598.00",
		"paid",
		"/admin/orders/" + id,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the order list is missing %q: %s", want, body)
		}
	}
	// The confirmation went out, so the list says so — that column is how an
	// operator notices a mail problem without reading logs.
	if !strings.Contains(body, "<td>yes</td>") {
		t.Errorf("the list does not report the confirmation as sent: %s", body)
	}
}

func TestAdminOrders_ShowsWhatToPackAndWhatTheGatewaySaid(t *testing.T) {
	s := newCheckoutShop(t)
	id := paidOrder(t, s)
	signIn(t, s.srv)

	order := s.reload(t, id)
	res, body := get(t, s.srv, "/admin/orders/"+id)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /admin/orders/%s = %d", id, res.StatusCode)
	}

	for _, want := range []string{
		order.Reference(),
		"Sample Tee",                    // what to pack
		"<td>S</td>",                    // which variant
		"ZAR 299.00",                    // at what price
		"ZAR 598.00",                    // for how much
		"Jane Doe",                      // to whom
		"1 Example Road<br>Exampletown", // where, with its line breaks intact
		"jane@example.com",
		// html/template escapes the leading + of a phone number to &#43;, which is
		// correct and is why this asserts on the digits.
		"27 11 555 0100",
		"1089250", // and what the gateway called it
		"fake",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the order page is missing %q: %s", want, body)
		}
	}

	// The raw notification body is kept for the day a customer and a bank disagree.
	if !strings.Contains(body, "Raw gateway notification") {
		t.Error("the order page does not show the raw notification")
	}

	// Read-only: an order records something that happened, and only an
	// authenticated gateway notification may change one. A form here would be a way
	// to record money that never arrived.
	if strings.Contains(body, `<form method="post" action="/admin/orders`) {
		t.Error("the order page has a form that could alter the order")
	}
}

func TestAdminOrders_PendingOrderIsNotShownAsPaid(t *testing.T) {
	s := newCheckoutShop(t)
	order := placeOrder(t, s, "S", 1) // no callback, so still pending
	signIn(t, s.srv)

	_, body := get(t, s.srv, "/admin/orders/"+order.ID)
	if strings.Contains(body, "<strong>Paid</strong>") {
		t.Error("a pending order is shown as paid")
	}
	if !strings.Contains(body, "pending") {
		t.Errorf("the page does not report the status: %s", body)
	}
	// Matched without regard to case: this is a state badge, and its casing is a
	// styling decision that has already changed once. What matters is that the
	// page says no confirmation went out, not how it is typeset.
	if !strings.Contains(strings.ToLower(body), "confirmation email not sent") {
		t.Errorf("the page does not report that no confirmation went out: %s", body)
	}
}

func TestAdminOrders_UnknownOrderIs404(t *testing.T) {
	s := newCheckoutShop(t)
	signIn(t, s.srv)

	// A real-looking id that names nothing, and one that could never be an id at
	// all: both are 404, because from outside they are the same answer.
	for _, id := range []string{"3f2504e0-4f89-41d3-9a0c-0305e82c3301", "not-a-uuid"} {
		res, _ := get(t, s.srv, "/admin/orders/"+id)
		if res.StatusCode != http.StatusNotFound {
			t.Errorf("GET /admin/orders/%s = %d, want 404", id, res.StatusCode)
		}
	}
}

func TestAdminOrders_SnapshotSurvivesACatalogEdit(t *testing.T) {
	// The reason order_items copies the title and price in: an order is a record of
	// what somebody bought, and editing the catalog afterwards must not rewrite it.
	s := newCheckoutShop(t)
	id := paidOrder(t, s)
	signIn(t, s.srv)

	v := s.variants["S"]
	v.PriceCents = 99900
	v.Option1 = "RENAMED"
	if _, err := s.catalog.UpdateVariant(t.Context(), v); err != nil {
		t.Fatalf("UpdateVariant: %v", err)
	}
	p, err := s.catalog.GetBySlug(t.Context(), "tee")
	if err != nil {
		t.Fatalf("GetBySlug: %v", err)
	}
	p.Title = "Renamed Product"
	if _, err := s.catalog.Update(t.Context(), p); err != nil {
		t.Fatalf("Update: %v", err)
	}

	_, body := get(t, s.srv, "/admin/orders/"+id)
	if !strings.Contains(body, "Sample Tee") || !strings.Contains(body, "ZAR 299.00") {
		t.Errorf("the order no longer shows what was bought: %s", body)
	}
	if strings.Contains(body, "Renamed Product") || strings.Contains(body, "999.00") {
		t.Error("a catalog edit rewrote purchase history")
	}
	// The option label is a snapshot for the same reason the title is. It is stored
	// rendered rather than joined from the variant at read time, so renaming an
	// option value — or the product's option *names* — cannot relabel a sale.
	if strings.Contains(body, "RENAMED") {
		t.Error("a renamed variant option rewrote the order's line label")
	}
}

func TestAdminOrders_SnapshotSurvivesRenamedOptionNames(t *testing.T) {
	// The half the previous test cannot reach: renaming the product's option
	// *headings* rather than a variant's value. Both would relabel the order if the
	// label were joined at read time instead of snapshotted.
	s := newCheckoutShop(t)
	id := paidOrder(t, s)
	signIn(t, s.srv)

	// The whole page, minus the per-request CSRF token, which is the only thing
	// that legitimately differs between two reads.
	csrf := regexp.MustCompile(`value="[^"]*"`)
	orderPage := func() string {
		_, body := get(t, s.srv, "/admin/orders/"+id)
		return csrf.ReplaceAllString(body, `value="..."`)
	}
	before := orderPage()

	p, err := s.catalog.GetBySlug(t.Context(), "tee")
	if err != nil {
		t.Fatalf("GetBySlug: %v", err)
	}
	p.Option1Name = "Chest measurement"
	if _, err := s.catalog.Update(t.Context(), p); err != nil {
		t.Fatalf("Update: %v", err)
	}

	if after := orderPage(); after != before {
		t.Error("renaming an option heading changed a recorded order")
	}
}

func TestAdminOrders_NavLinksToOrders(t *testing.T) {
	srv, _ := setup(t)

	// Reachable without typing a URL, or nobody will find it.
	if _, body := get(t, srv, "/admin/products"); !strings.Contains(body, `href="/admin/orders"`) {
		t.Errorf("the admin nav has no link to orders: %s", body)
	}
}

func TestAdminOrders_NoWriteRoutesExist(t *testing.T) {
	s := newCheckoutShop(t)
	id := paidOrder(t, s)
	signIn(t, s.srv)

	// Nothing accepts a POST under /admin/orders. If a write route is ever added,
	// this fails and whoever added it has to think about what "editing an order"
	// means.
	for _, path := range []string{
		"/admin/orders",
		"/admin/orders/" + id,
		"/admin/orders/" + id + "/delete",
	} {
		res, _ := post(t, s.srv, path, url.Values{})
		if res.StatusCode != http.StatusMethodNotAllowed && res.StatusCode != http.StatusNotFound {
			t.Errorf("POST %s = %d, want 404 or 405 — orders are read-only", path, res.StatusCode)
		}
	}
}

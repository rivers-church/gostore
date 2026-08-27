package validate

import (
	"strings"
	"testing"

	"github.com/17xande-dev/gostore/internal/catalog"
	"github.com/17xande-dev/gostore/internal/orders"
)

func TestFormErrors(t *testing.T) {
	e := FormErrors{}
	if e.Any() {
		t.Error("an empty FormErrors reports errors")
	}

	e.Add("title", "Required.")
	e.Add("title", "Something else.")
	if got := e["title"]; got != "Required." {
		t.Errorf("title = %q, want the first message to win", got)
	}

	e.Add("slug", "Bad.")
	if got, want := e.String(), "slug: Bad.; title: Required."; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

func TestProduct(t *testing.T) {
	valid := catalog.Product{
		Slug: "a-book", Title: "A Book", Description: "Fine.",
	}
	if errs := Product(valid); errs.Any() {
		t.Errorf("a valid product was rejected: %s", errs)
	}

	cases := []struct {
		name  string
		mut   func(*catalog.Product)
		field string
	}{
		{"no title", func(p *catalog.Product) { p.Title = "  " }, "title"},
		{"category slug not a slug", func(p *catalog.Product) { p.CategorySlugs = []string{"Gift Cards"} }, "categories"},
		{"no slug", func(p *catalog.Product) { p.Slug = "" }, "slug"},
		{"slug with spaces", func(p *catalog.Product) { p.Slug = "a book" }, "slug"},
		{"slug in capitals", func(p *catalog.Product) { p.Slug = "A-Book" }, "slug"},
		{"long title", func(p *catalog.Product) { p.Title = strings.Repeat("x", 201) }, "title"},
	}
	for _, tc := range cases {
		p := valid
		tc.mut(&p)
		errs := Product(p)
		if _, ok := errs[tc.field]; !ok {
			t.Errorf("%s: no error on %q, got %s", tc.name, tc.field, errs)
		}
	}

	// The image is deliberately not validated here any more, and there is no longer
	// even a URL to validate: a product stores only the storage key of something it
	// uploaded, and the key is generated rather than typed.
	for _, key := range []string{"products/a/b.jpg", "", "anything at all"} {
		p := valid
		p.ImageKey = key
		if errs := Product(p); errs.Any() {
			t.Errorf("ImageKey %q produced form errors %s", key, errs)
		}
	}
}

func TestVariant(t *testing.T) {
	valid := catalog.Variant{SKU: "TEE-M", Option1: "M", Option2: "Black", PriceCents: 100, StockQty: 1}
	if errs := Variant(valid); errs.Any() {
		t.Errorf("a valid variant was rejected: %s", errs)
	}

	// A variant with no options at all is the normal case for a book, not an
	// error: exactly one such variant is what makes cart and stock code
	// branch-free.
	plain := catalog.Variant{SKU: "BOOK-1", PriceCents: 24900}
	if errs := Variant(plain); errs.Any() {
		t.Errorf("an optionless variant was rejected: %s", errs)
	}

	cases := []struct {
		name  string
		mut   func(*catalog.Variant)
		field string
	}{
		{"no sku", func(v *catalog.Variant) { v.SKU = "" }, "sku"},
		{"negative price", func(v *catalog.Variant) { v.PriceCents = -1 }, "price"},
		{"negative stock", func(v *catalog.Variant) { v.StockQty = -1 }, "stock_qty"},
		{"overlong option 1", func(v *catalog.Variant) { v.Option1 = strings.Repeat("x", 101) }, "option1"},
		{"overlong option 3", func(v *catalog.Variant) { v.Option3 = strings.Repeat("x", 101) }, "option3"},
	}
	for _, tc := range cases {
		v := valid
		tc.mut(&v)
		errs := Variant(v)
		if _, ok := errs[tc.field]; !ok {
			t.Errorf("%s: no error on %q, got %s", tc.name, tc.field, errs)
		}
	}
}

func TestProductOptionNames(t *testing.T) {
	base := catalog.Product{Slug: "tee", Title: "Tee"}

	ok := []struct {
		name    string
		options [3]string
	}{
		{"none at all", [3]string{}},
		{"one", [3]string{"Format", "", ""}},
		{"two", [3]string{"Size", "Colour", ""}},
		{"all three", [3]string{"Size", "Colour", "Material"}},
	}
	for _, tc := range ok {
		p := base
		p.Option1Name, p.Option2Name, p.Option3Name = tc.options[0], tc.options[1], tc.options[2]
		if errs := Product(p); errs.Any() {
			t.Errorf("%s: valid option names were rejected: %s", tc.name, errs)
		}
	}

	bad := []struct {
		name    string
		options [3]string
		field   string
	}{
		// Values are matched to names by position, so a hole would leave every
		// variant's first value with no heading above it.
		{"gap in slot 1", [3]string{"", "Colour", ""}, "option2_name"},
		{"gap in slot 2", [3]string{"Size", "", "Material"}, "option3_name"},
		// Two headings reading the same makes the variants under them
		// indistinguishable, and the comparison is deliberately case-insensitive.
		{"duplicate", [3]string{"Size", "Size", ""}, "option2_name"},
		{"duplicate in another case", [3]string{"Size", "SIZE", ""}, "option2_name"},
		{"overlong", [3]string{strings.Repeat("x", 101), "", ""}, "option1_name"},
	}
	for _, tc := range bad {
		p := base
		p.Option1Name, p.Option2Name, p.Option3Name = tc.options[0], tc.options[1], tc.options[2]
		errs := Product(p)
		if _, ok := errs[tc.field]; !ok {
			t.Errorf("%s: no error on %q, got %s", tc.name, tc.field, errs)
		}
	}
}

func TestCustomer_AddressDependsOnWhetherAnythingShips(t *testing.T) {
	// Asking a buyer for their street address to receive an mp3 is friction for
	// them and personal data the shop has no use for. A mixed basket still needs
	// one: a single parcel among the downloads has to go somewhere.
	base := orders.Customer{Name: "Jane Doe", Email: "jane@example.com"}

	if errs := Customer(base, false); errs.Any() {
		t.Errorf("a downloads-only order was refused for having no address: %s", errs)
	}
	errs := Customer(base, true)
	if _, ok := errs["address"]; !ok {
		t.Errorf("an order that ships was accepted with no address: %s", errs)
	}

	// With one supplied, both agree.
	withAddress := base
	withAddress.Address = "1 Example Road"
	for _, ships := range []bool{true, false} {
		if errs := Customer(withAddress, ships); errs.Any() {
			t.Errorf("ships=%v: a complete form was refused: %s", ships, errs)
		}
	}

	// The length cap still applies to a downloads-only order: not required is not
	// the same as unchecked, and an unbounded field is a free way to write two
	// kilobytes of anything into the orders table.
	long := base
	long.Address = strings.Repeat("x", 2_001)
	if errs := Customer(long, false); !errs.Any() {
		t.Error("an oversized address was accepted because none was required")
	}
}

func TestProduct_RefusesAnUnknownKind(t *testing.T) {
	base := catalog.Product{Slug: "tee", Title: "Tee"}

	// Empty is the physical default, which is what a form with no such field
	// submits and what every seed file relies on.
	for _, kind := range []catalog.Kind{"", catalog.KindPhysical, catalog.KindDigital} {
		p := base
		p.Kind = kind
		if errs := Product(p); errs.Any() {
			t.Errorf("kind %q was refused: %s", kind, errs)
		}
	}

	// Anything else is a hand-crafted request. Without this it would reach the
	// CHECK constraint, which arrives as a 500 with no field to hang a message on.
	p := base
	p.Kind = "subscription"
	errs := Product(p)
	if _, ok := errs["kind"]; !ok {
		t.Errorf("an invented kind was accepted: %s", errs)
	}
}

func TestPassword(t *testing.T) {
	cases := []struct {
		name, password, confirm string
		wantFields              []string
	}{
		{"good", "correct horse battery", "correct horse battery", nil},
		// Twelve runes exactly, so the boundary is inclusive.
		{"exactly the minimum", "abcdefghijkl", "abcdefghijkl", nil},
		{"one short", "abcdefghijk", "abcdefghijk", []string{"password"}},
		{"empty", "", "", []string{"password"}},
		{"mismatch", "correct horse battery", "correct horse batery", []string{"password_confirm"}},
		// A blank pair must not produce two errors saying the same thing: the
		// mismatch is only worth reporting once the password itself is acceptable.
		{"empty and mismatched", "", "something", []string{"password"}},
		// Counted in runes, not bytes: this is 12 characters and 36 bytes, so a
		// byte count would wrongly accept a shorter passphrase in this script.
		{"twelve non-Latin runes", "日本語日本語日本語日本語", "日本語日本語日本語日本語", nil},
		{"eleven non-Latin runes", "日本語日本語日本語日本", "日本語日本語日本語日本", []string{"password"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := FormErrors{}
			Password(e, c.password, c.confirm)
			if len(e) != len(c.wantFields) {
				t.Fatalf("Password(%q, %q) = %v, want errors on %v", c.password, c.confirm, e, c.wantFields)
			}
			for _, field := range c.wantFields {
				if _, ok := e[field]; !ok {
					t.Errorf("no error on %q; got %v", field, e)
				}
			}
		})
	}
}

func TestNormalizeEmail(t *testing.T) {
	cases := []struct{ in, want string }{
		{"alex@example.com", "alex@example.com"},
		{"  alex@example.com  ", "alex@example.com"},
		// The case this exists for. Stored whole, the display-name form would be a
		// second account for one mailbox: it does not collide with the plain
		// address under admin_users' lower(email) index, and it could never sign
		// in, because the login form's type=email input will not accept it back.
		{"Alex <alex@example.com>", "alex@example.com"},
		{`"Alex F" <alex@example.com>`, "alex@example.com"},
		// Unparseable input comes back trimmed but otherwise untouched, so the
		// validator gets to be the one that reports it.
		{"not an address", "not an address"},
		{"", ""},
	}
	for _, c := range cases {
		if got := NormalizeEmail(c.in); got != c.want {
			t.Errorf("NormalizeEmail(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestAdminUser(t *testing.T) {
	if e := AdminUser("alex@example.com", "Alex"); e.Any() {
		t.Errorf("a valid account reported %v", e)
	}
	if e := AdminUser("", "Alex"); !e.Any() {
		t.Error("a missing email was accepted")
	}
	if e := AdminUser("not an address", "Alex"); !e.Any() {
		t.Error("a malformed email was accepted")
	}
	// A name is optional: the display falls back to the address, and demanding
	// one is a field to argue with rather than a property anything depends on.
	if e := AdminUser("alex@example.com", ""); e.Any() {
		t.Errorf("a missing name reported %v", e)
	}
	// The display-name form only reaches validation after NormalizeEmail, which
	// is the order the handler uses; on its own it has a space and is refused.
	if e := AdminUser(NormalizeEmail("Alex <alex@example.com>"), "Alex"); e.Any() {
		t.Errorf("a normalised display-name address reported %v", e)
	}
}

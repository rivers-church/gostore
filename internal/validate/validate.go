// Package validate holds form validation: a field-keyed error map plus one
// function per form, so handlers stay about HTTP and templates can render an
// error next to the input that caused it.
package validate

import (
	"fmt"
	"net/mail"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/17xande-dev/gostore/internal/catalog"
	"github.com/17xande-dev/gostore/internal/orders"
)

// FormErrors maps a form field name to a single message. One message per field
// is deliberate: a form that lists three complaints about the same input is
// harder to act on than the first one.
type FormErrors map[string]string

// Add records a message unless the field already has one.
func (e FormErrors) Add(field, msg string) {
	if _, seen := e[field]; !seen {
		e[field] = msg
	}
}

// Any reports whether the form failed validation.
func (e FormErrors) Any() bool { return len(e) > 0 }

// String renders every message in field order, for logs and command-line tools
// that have no form to render them into.
func (e FormErrors) String() string {
	fields := make([]string, 0, len(e))
	for field := range e {
		fields = append(fields, field)
	}
	slices.Sort(fields)

	var b strings.Builder
	for i, field := range fields {
		if i > 0 {
			b.WriteString("; ")
		}
		b.WriteString(field)
		b.WriteString(": ")
		b.WriteString(e[field])
	}
	return b.String()
}

// Product validates a product as submitted by the admin form. The slug is
// checked strictly because it is a public URL, and changing it later breaks
// links that already exist.
func Product(p catalog.Product) FormErrors {
	e := FormErrors{}

	required(e, "title", p.Title)
	maxLen(e, "title", p.Title, 200)
	maxLen(e, "description", p.Description, 10_000)

	switch {
	case p.Slug == "":
		e.Add("slug", "Required.")
	case p.Slug != catalog.Slugify(p.Slug):
		e.Add("slug", "Use lowercase letters, numbers and hyphens only.")
	default:
		maxLen(e, "slug", p.Slug, 200)
	}

	// CategorySlugs is only ever set by a seed file — the admin form submits ids,
	// which ProductCategories resolves — so this is a no-op for a form and the one
	// check a fixture gets. A slug that is not already in slug form would create a
	// category whose public URL is not the string the file wrote.
	for _, slug := range p.CategorySlugs {
		if slug == "" || slug != catalog.Slugify(slug) {
			e.Add("categories", "Category slugs must be lowercase letters, numbers and hyphens.")
			break
		}
	}

	// An empty kind is the physical default, which is what a form with no such
	// field submits. Anything else that is not one of the two is a hand-crafted
	// request: without this it would reach the CHECK constraint, which arrives as
	// a 500 with no field to hang a message on.
	if p.Kind != "" && !p.Kind.Valid() {
		e.Add("kind", "Choose either a physical product or a download.")
	}

	productOptions(e, p)

	// There is deliberately nothing here about the image. The product form does not
	// carry one: an image arrives by upload and its URL is whatever storage says it
	// is, so there is no user input to validate.
	return e
}

// productOptions checks the option *names* a product declares. Two rules, both
// enforced here rather than in the schema so the admin gets a message on the
// field instead of a constraint violation.
//
// No gaps: a name in slot 2 with slot 1 empty would leave every variant's first
// value unlabelled, since values and names are matched by position. Filling slots
// in order is the invariant that lets Product.OptionsFor skip by name alone.
//
// No duplicates: two slots both called "Size" makes a selector with two identical
// headings, and the variant it produces is indistinguishable from another.
func productOptions(e FormErrors, p catalog.Product) {
	names := p.OptionNames()
	seen := make(map[string]bool, len(names))
	gap := false

	for i, name := range names {
		field := fmt.Sprintf("option%d_name", i+1)
		if name == "" {
			gap = true
			continue
		}
		if gap {
			e.Add(field, "Fill the earlier option in first.")
			continue
		}
		maxLen(e, field, name, 100)

		key := strings.ToLower(name)
		if seen[key] {
			e.Add(field, "Already used by another option.")
		}
		seen[key] = true
	}
}

// ProductCategories resolves the category ids a product form submitted against
// the ones the form was rendered from, returning the chosen categories and an
// error for anything that is not among them.
//
// Checking against the rendered list rather than letting the database refuse the
// link is what makes the failure legible: a foreign key violation would arrive as
// a 500 with no field attached, and a hand-crafted id would silently do nothing.
// Categories are optional — a shop that does not use them submits none, which is
// not an error.
func ProductCategories(ids []string, known []catalog.Category) ([]catalog.Category, FormErrors) {
	e := FormErrors{}
	byID := make(map[string]catalog.Category, len(known))
	for _, c := range known {
		byID[c.ID] = c
	}

	chosen := make([]catalog.Category, 0, len(ids))
	for _, id := range ids {
		c, ok := byID[id]
		if !ok {
			e.Add("categories", "One of the categories no longer exists. Reload the page and try again.")
			continue
		}
		chosen = append(chosen, c)
	}
	return chosen, e
}

// Category validates a category as submitted by the admin form. The slug is
// checked as strictly as a product's, and for the same reason: it is a public URL
// parameter, and changing it later breaks links that already exist.
func Category(c catalog.Category) FormErrors {
	e := FormErrors{}

	required(e, "name", c.Name)
	maxLen(e, "name", c.Name, 100)

	switch {
	case c.Slug == "":
		e.Add("slug", "Required.")
	case c.Slug != catalog.Slugify(c.Slug):
		e.Add("slug", "Use lowercase letters, numbers and hyphens only.")
	default:
		maxLen(e, "slug", c.Slug, 100)
	}
	return e
}

// Variant validates a variant as submitted by the admin form. Price and stock
// arrive as text, so their parse failures are reported by the caller against
// the same field names used here.
func Variant(v catalog.Variant) FormErrors {
	e := FormErrors{}

	required(e, "sku", v.SKU)
	maxLen(e, "sku", v.SKU, 100)
	for i, o := range v.Options() {
		maxLen(e, fmt.Sprintf("option%d", i+1), o, 100)
	}

	if v.PriceCents < 0 {
		e.Add("price", "Cannot be negative.")
	}
	if v.StockQty < 0 {
		e.Add("stock_qty", "Cannot be negative.")
	}
	return e
}

// Customer validates the checkout form.
//
// It is deliberately forgiving about everything except the email address. A name
// or an address is whatever the customer says it is — this code has no business
// having opinions about either, and a shop that refuses an address because it has
// no comma in it loses a sale to no purpose. The email address is the exception
// because it is the only way the confirmation reaches anybody, and a typo there is
// silent.
//
// needsShipping is false for a cart of downloads only, which then asks for no
// address at all. Collecting a street address in order to send somebody an mp3 is
// friction for the shopper and personal data the shop has no use for. A mixed
// cart still needs one: a single parcel among the downloads has to go somewhere.
func Customer(c orders.Customer, needsShipping bool) FormErrors {
	e := FormErrors{}

	required(e, "name", c.Name)
	maxLen(e, "name", c.Name, 200)
	maxLen(e, "phone", c.Phone, 50)

	if needsShipping {
		required(e, "address", c.Address)
	}
	maxLen(e, "address", c.Address, 2_000)

	switch {
	case strings.TrimSpace(c.Email) == "":
		e.Add("email", "Required.")
	case !isEmail(c.Email):
		e.Add("email", "Does not look like an email address.")
	default:
		maxLen(e, "email", c.Email, 320)
	}
	return e
}

// isEmail is the weakest useful check: one @ with something either side, and no
// spaces. Anything stricter rejects addresses that are perfectly valid — the
// grammar in RFC 5322 permits far more than most validators believe — and the only
// real test of an address is whether mail to it arrives.
func isEmail(s string) bool {
	s = strings.TrimSpace(s)
	if strings.ContainsAny(s, " \t\r\n") {
		return false
	}
	local, domain, found := strings.Cut(s, "@")
	return found && local != "" && domain != "" &&
		!strings.Contains(domain, "@") && strings.Contains(domain, ".")
}

func required(e FormErrors, field, value string) {
	if strings.TrimSpace(value) == "" {
		e.Add(field, "Required.")
	}
}

func maxLen(e FormErrors, field, value string, max int) {
	if utf8.RuneCountInString(value) > max {
		e.Add(field, "Too long.")
	}
}

// MinPasswordLength is the shortest admin password accepted.
//
// Length is the only rule. Composition requirements ("one digit, one symbol")
// push people towards short mangled words, while argon2id already makes an
// offline guess expensive. Twelve characters admits a short passphrase, which is
// what we would rather people chose.
//
// It is counted in runes, so a passphrase in a non-Latin script is measured the
// way its writer would measure it rather than in UTF-8 bytes.
const MinPasswordLength = 12

// MaxPasswordLength bounds the other end. Nothing about a password needs to be
// longer than this, and the input is fed straight to argon2id, which allocates
// its 64 MiB and hashes whatever it is given: an unbounded field is a way to
// spend a server's memory and CPU from a form, and the login rate limit counts
// requests rather than bytes. Every other field in this package is bounded for
// less reason than this one.
const MaxPasswordLength = 1024

// Password checks a new password and its confirmation, writing any problems into
// e under "password" and "password_confirm".
//
// It takes the error map rather than returning its own so a caller validating a
// whole form — email, name and password together — reports every problem in one
// pass instead of making somebody fix them one page load at a time.
func Password(e FormErrors, password, confirm string) {
	switch {
	case password == "":
		e.Add("password", "Required.")
	case utf8.RuneCountInString(password) < MinPasswordLength:
		e.Add("password", fmt.Sprintf("Use at least %d characters.", MinPasswordLength))
	case utf8.RuneCountInString(password) > MaxPasswordLength:
		e.Add("password", fmt.Sprintf("Use at most %d characters.", MaxPasswordLength))
	}
	// Only worth reporting once the password itself is acceptable — otherwise a
	// blank pair produces two errors saying the same thing.
	if _, bad := e["password"]; !bad && password != confirm {
		e.Add("password_confirm", "The two passwords do not match.")
	}
}

// AdminUser validates the account half of the new-administrator form. The
// password is validated separately by Password, because resetting a password
// reuses that half on its own.
func AdminUser(email, name string) FormErrors {
	e := FormErrors{}
	required(e, "email", email)
	if email != "" && !isEmail(email) {
		e.Add("email", "Enter a valid email address.")
	}
	maxLen(e, "email", email, 320)
	maxLen(e, "name", name, 200)
	return e
}

// NormalizeEmail reduces an address to its addr-spec, so `Alex <a@example.com>`
// is stored as `a@example.com`. It returns s trimmed but otherwise unchanged if
// it does not parse.
//
// This is not cosmetic. mail.ParseAddress accepts RFC 5322's display-name form,
// and an account stored under the whole string would be a second account for one
// mailbox: it would not collide with the plain address under admin_users'
// lower(email) unique index, so neither that nor auth.ErrEmailTaken would catch
// it — and it could never sign in, because the login form's type=email input will
// not accept the string back.
func NormalizeEmail(s string) string {
	s = strings.TrimSpace(s)
	addr, err := mail.ParseAddress(s)
	if err != nil {
		return s
	}
	return addr.Address
}

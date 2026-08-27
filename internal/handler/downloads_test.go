package handler

import (
	"bytes"
	"encoding/hex"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/17xande-dev/gostore/internal/auth"
	"github.com/17xande-dev/gostore/internal/catalog"
	"github.com/17xande-dev/gostore/internal/config"
	"github.com/17xande-dev/gostore/internal/downloads"
	"github.com/17xande-dev/gostore/internal/orders"
	"github.com/17xande-dev/gostore/internal/payment"
)

// newSession gives the server's client a fresh cookie jar, which is what makes a
// test "a different person": new cart, new order, and no admin session.
func newSession(t *testing.T, srv *httptest.Server) {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar: %v", err)
	}
	srv.Client().Jar = jar
}

// digitalShop builds the case the feature was designed around: a conference
// recording sold as an audio set and a video set, with one bonus PDF that both
// include.
//
// The shared file is the point. Files hang off the product and variant_files says
// which variants include each one, so a bundle costs a row rather than a second
// upload — and a test where every file belonged to exactly one variant would not
// prove that at all.
type digitalShop struct {
	*shop
	product catalog.Product
	audio   catalog.Variant
	video   catalog.Variant
	mp3     catalog.File
	mp4     catalog.File
	notes   catalog.File // in both variants
}

func newDigitalShop(t *testing.T) *digitalShop {
	t.Helper()
	s := newStore(t)
	ctx := t.Context()

	p, err := s.catalog.Create(ctx, catalog.Product{
		Slug: "conference-2026", Title: "Conference 2026", Active: true,
		Kind: catalog.KindDigital, Option1Name: "Format",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	mk := func(sku, format string, cents int64) catalog.Variant {
		v, err := s.catalog.CreateVariant(ctx, catalog.Variant{
			ProductID: p.ID, SKU: sku, Option1: format, PriceCents: cents, Active: true,
		})
		if err != nil {
			t.Fatalf("CreateVariant %s: %v", sku, err)
		}
		return v
	}
	audio := mk("CONF-AUDIO", "Audio", 15000)
	video := mk("CONF-VIDEO", "Video", 40000)

	file := func(title, body string, variants ...string) catalog.File {
		key := "downloads/" + p.ID + "/" + title
		if err := s.files.Put(ctx, key, strings.NewReader(body), int64(len(body)), "application/octet-stream"); err != nil {
			t.Fatalf("Put %s: %v", title, err)
		}
		f, err := s.catalog.AddFile(ctx, catalog.File{
			ProductID: p.ID, Title: title, ObjectKey: key,
			OriginalFilename: title, ContentType: "application/octet-stream",
			SizeBytes: int64(len(body)),
		}, variants)
		if err != nil {
			t.Fatalf("AddFile %s: %v", title, err)
		}
		return f
	}

	// Reload so Variants is populated for anything that needs it.
	p, err = s.catalog.Get(ctx, p.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	return &digitalShop{
		shop:    s,
		product: p,
		audio:   audio,
		video:   video,
		mp3:     file("session-one.mp3", "AUDIO BYTES", audio.ID),
		mp4:     file("session-one.mp4", "VIDEO BYTES", video.ID),
		notes:   file("notes.pdf", "PDF BYTES", audio.ID, video.ID),
	}
}

// buy runs a whole checkout and pays it, returning the download token the
// confirmation email carried.
//
// The token is read out of the email rather than out of the database, because the
// database does not have it: only the SHA-256 hash is stored. Reading it from the
// mail is therefore not a shortcut, it is the only way — and it doubles as proof
// that the email is where a buyer actually gets their link.
func (d *digitalShop) buy(t *testing.T, variantID string) string {
	t.Helper()

	addToCart(t, d.srv, variantID, 1)
	if res, body := post(t, d.srv, "/cart/checkout", validCheckoutForm()); res.StatusCode != http.StatusOK {
		t.Fatalf("checkout = %d %s", res.StatusCode, body)
	}
	order, err := d.orders.LatestForCart(t.Context(), cartTokenOf(t, d.srv))
	if err != nil {
		t.Fatalf("LatestForCart: %v", err)
	}
	callback(t, d.srv, "fake", payment.FakeCallbackBody(order.ID, "pf-"+order.ID[:8], "paid", order.TotalCents))

	sent := d.mail.To("jane@example.com")
	if len(sent) == 0 {
		t.Fatal("no confirmation email, so no download link")
	}
	return tokenFromEmail(t, sent[len(sent)-1].Text)
}

func tokenFromEmail(t *testing.T, body string) string {
	t.Helper()
	for _, line := range strings.Fields(body) {
		if _, after, ok := strings.Cut(line, "/downloads/"); ok {
			return strings.TrimSpace(after)
		}
	}
	t.Fatalf("no download link in the email:\n%s", body)
	return ""
}

func TestDownloads_TheRequirement_RevokingOneBuyerLeavesTheOther(t *testing.T) {
	// The whole reason the feature exists, stated as one test: person A and person
	// B buy the same thing, A is cut off, B is not. Anything that gave both buyers
	// the same link would pass every other test in this file and fail this one.
	d := newDigitalShop(t)

	alice := d.buy(t, d.audio.ID)
	// A fresh cookie jar is a different person: same product, same variant, its own
	// cart and its own order.
	newSession(t, d.srv)
	bob := d.buy(t, d.audio.ID)

	if alice == bob {
		t.Fatal("two buyers of the same variant were given the same token")
	}

	for _, tok := range []string{alice, bob} {
		if res, _ := get(t, d.srv, "/downloads/"+tok+"/"+strconv.FormatInt(d.mp3.ID, 10)); res.StatusCode != http.StatusFound {
			t.Fatalf("download before revoking = %d, want 302", res.StatusCode)
		}
	}

	// Revoke Alice's, through the admin, the way an operator would.
	signIn(t, d.srv)
	aliceOrder := d.orderFor(t, alice)
	grants, err := d.grants.ForOrder(t.Context(), aliceOrder)
	if err != nil || len(grants) != 1 {
		t.Fatalf("ForOrder = %v, %v", grants, err)
	}
	res, body := post(t, d.srv, "/admin/orders/"+aliceOrder+"/entitlements/"+grants[0].ID+"/revoke", url.Values{})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("revoke = %d %s", res.StatusCode, body)
	}

	if res, _ := get(t, d.srv, "/downloads/"+alice+"/"+strconv.FormatInt(d.mp3.ID, 10)); res.StatusCode != http.StatusForbidden {
		t.Errorf("the revoked buyer's download = %d, want 403", res.StatusCode)
	}
	if res, _ := get(t, d.srv, "/downloads/"+bob+"/"+strconv.FormatInt(d.mp3.ID, 10)); res.StatusCode != http.StatusFound {
		t.Errorf("the other buyer's download = %d, want 302 — revoking one cut off both", res.StatusCode)
	}

	// And it is reversible, because a shop owner acting on a suspicion should be
	// able to change their mind.
	if res, _ := post(t, d.srv, "/admin/orders/"+aliceOrder+"/entitlements/"+grants[0].ID+"/restore", url.Values{}); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("restore = %d", res.StatusCode)
	}
	if res, _ := get(t, d.srv, "/downloads/"+alice+"/"+strconv.FormatInt(d.mp3.ID, 10)); res.StatusCode != http.StatusFound {
		t.Errorf("after restoring = %d, want 302", res.StatusCode)
	}

	// The page those two POSTs come from, which nothing else renders with an
	// entitlement on it: the buttons are there for a role that may use them and
	// absent for one that may not.
	res, body = get(t, d.srv, "/admin/orders/"+aliceOrder)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET the order = %d %s", res.StatusCode, body)
	}
	if !strings.Contains(body, "/entitlements/"+grants[0].ID+"/revoke") {
		t.Error("the order page offers no way to revoke an entitlement")
	}

	mustAccount(t, d.shop, "viewer@example.com", testPassword, auth.RoleViewer)
	signInAs(t, d.srv, "viewer@example.com", testPassword)
	res, body = get(t, d.srv, "/admin/orders/"+aliceOrder)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET the order as a viewer = %d", res.StatusCode)
	}
	if strings.Contains(body, "/entitlements/") {
		t.Error("a viewer is offered the revoke and restore buttons")
	}
	// Read-only, not shut out: the order itself is still theirs to look at.
	if !strings.Contains(body, grants[0].ID) && !strings.Contains(body, "Conference 2026") {
		t.Error("a viewer cannot see the order's contents")
	}
}

// orderFor finds the order a token belongs to, for a test that needs to drive the
// admin as well as the buyer.
func (d *digitalShop) orderFor(t *testing.T, token string) string {
	t.Helper()
	e, err := d.grants.Lookup(t.Context(), token)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	return e.OrderID
}

func TestDownloads_TokenResolvesButItsHashDoesNot(t *testing.T) {
	// The token the buyer holds is not what the database stores — see
	// orders.NewToken for the property itself. What this adds is that the lookup
	// really does hash before querying: if it compared the column against the raw
	// value, the *hash* would be the working credential and the token would not.
	d := newDigitalShop(t)
	token := d.buy(t, d.audio.ID)

	if _, err := d.grants.Lookup(t.Context(), token); err != nil {
		t.Fatalf("the buyer's token does not resolve: %v", err)
	}
	stored := hex.EncodeToString(orders.HashToken(token))
	if _, err := d.grants.Lookup(t.Context(), stored); !errors.Is(err, downloads.ErrNotFound) {
		t.Errorf("the stored hash works as a download credential: %v", err)
	}
}

func TestDownloads_RefusesEverythingButTheRightToken(t *testing.T) {
	d := newDigitalShop(t)
	token := d.buy(t, d.audio.ID)
	mp3 := strconv.FormatInt(d.mp3.ID, 10)

	// One character changed. Worth its own case because a lookup that compared a
	// prefix, or that fell back to some "first match", would pass a test using a
	// wholly different string.
	altered := []byte(token)
	if altered[0] == 'A' {
		altered[0] = 'B'
	} else {
		altered[0] = 'A'
	}

	// An empty token is deliberately absent: /downloads//1 is a path ServeMux
	// cleans and redirects, so it never reaches the handler and testing it would
	// be testing the mux.
	for name, tok := range map[string]string{
		"one character changed": string(altered),
		"invented":              "not-a-real-token-at-all",
		"truncated":             token[:len(token)-1],
		"another token's hash":  hex.EncodeToString(orders.HashToken(token)),
	} {
		res, _ := get(t, d.srv, "/downloads/"+tok+"/"+mp3)
		if res.StatusCode != http.StatusNotFound {
			t.Errorf("%s: download = %d, want 404", name, res.StatusCode)
		}
	}
}

func TestDownloads_AFileFromAnotherVariantIsRefused(t *testing.T) {
	// The audio buyer knows the video file's id — it is a small integer, and the
	// admin page shows them — so the check that matters is membership of *their*
	// variant, done in SQL rather than by trusting the URL.
	d := newDigitalShop(t)
	token := d.buy(t, d.audio.ID)

	res, _ := get(t, d.srv, "/downloads/"+token+"/"+strconv.FormatInt(d.mp4.ID, 10))
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("a file from the other variant = %d, want 404", res.StatusCode)
	}
	// A file id that does not exist at all is the same answer, so the response is
	// not an oracle for which ids are real.
	if res, _ := get(t, d.srv, "/downloads/"+token+"/999999"); res.StatusCode != http.StatusNotFound {
		t.Errorf("a nonexistent file = %d, want 404", res.StatusCode)
	}

	// The shared file is in both variants, so the audio buyer does get it. Without
	// this the test above would also pass if the join simply refused everything.
	if res, _ := get(t, d.srv, "/downloads/"+token+"/"+strconv.FormatInt(d.notes.ID, 10)); res.StatusCode != http.StatusFound {
		t.Errorf("the shared file = %d, want 302", res.StatusCode)
	}
}

func TestDownloads_IndexListsOnlyTheVariantsFiles(t *testing.T) {
	d := newDigitalShop(t)
	token := d.buy(t, d.audio.ID)

	res, body := get(t, d.srv, "/downloads/"+token)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET the download page = %d", res.StatusCode)
	}
	if !strings.Contains(body, "session-one.mp3") || !strings.Contains(body, "notes.pdf") {
		t.Errorf("the page is missing the audio variant's files: %s", body)
	}
	if strings.Contains(body, "session-one.mp4") {
		t.Error("the page lists a file belonging to the other variant")
	}
}

func TestDownloads_RedirectsToAShortLivedURL(t *testing.T) {
	// The bucket ending. What matters is that the redirect target is signed, is not
	// this server, carries an attachment disposition, and is minted per click
	// rather than reused.
	d := newDigitalShop(t)
	token := d.buy(t, d.audio.ID)
	path := "/downloads/" + token + "/" + strconv.FormatInt(d.mp3.ID, 10)

	res, _ := get(t, d.srv, path)
	if res.StatusCode != http.StatusFound {
		t.Fatalf("download = %d, want 302", res.StatusCode)
	}
	link := res.Header.Get("Location")
	if strings.HasPrefix(link, d.srv.URL) || !strings.Contains(link, "downloads.example") {
		t.Errorf("Location %q does not point at the bucket", link)
	}
	if !strings.Contains(link, "attachment") || !strings.Contains(link, "session-one.mp3") {
		t.Errorf("Location %q carries no attachment disposition", link)
	}
	if !strings.Contains(link, "expires=300") {
		t.Errorf("Location %q does not expire in the configured five minutes", link)
	}

	// A second click mints a second URL. Handing out the same one twice would mean
	// a link's lifetime was not what the first response implied.
	get(t, d.srv, path)
	if minted := d.files.Presigned(); len(minted) != 2 {
		t.Fatalf("%d URLs minted for two clicks, want 2", len(minted))
	}
}

func TestDownloads_StreamsWhenTheBackendCannotSign(t *testing.T) {
	// The disk ending, which no amount of testing the bucket one would reach. Same
	// URL, same authorisation, same recording — different last step.
	d := newDigitalShop(t)
	d.files.Presign = false
	token := d.buy(t, d.audio.ID)

	res, body := get(t, d.srv, "/downloads/"+token+"/"+strconv.FormatInt(d.mp3.ID, 10))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("download = %d, want 200", res.StatusCode)
	}
	if body != "AUDIO BYTES" {
		t.Errorf("streamed %q, want the stored bytes", body)
	}
	if got := res.Header.Get("Content-Disposition"); !strings.Contains(got, `filename="session-one.mp3"`) {
		t.Errorf("Content-Disposition = %q", got)
	}
	// A shared cache holding this would serve somebody else's purchase.
	if got := res.Header.Get("Cache-Control"); !strings.Contains(got, "no-store") {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if got := res.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q", got)
	}
}

func TestDownloads_AreCountedAndAttributed(t *testing.T) {
	// The stats the shop owner asked for, and the reason they are recorded here
	// rather than read back from the bucket: a presigned URL is anonymous to the
	// bucket, so only this store can say *who* downloaded.
	d := newDigitalShop(t)
	alice := d.buy(t, d.audio.ID)
	newSession(t, d.srv)
	bob := d.buy(t, d.video.ID)

	mp3 := strconv.FormatInt(d.mp3.ID, 10)
	for range 3 {
		get(t, d.srv, "/downloads/"+alice+"/"+mp3)
	}
	get(t, d.srv, "/downloads/"+bob+"/"+strconv.FormatInt(d.mp4.ID, 10))
	get(t, d.srv, "/downloads/"+bob+"/"+strconv.FormatInt(d.notes.ID, 10))

	stats, err := d.grants.Stats(t.Context(), d.product.ID)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.TotalDownloads != 5 {
		t.Errorf("TotalDownloads = %d, want 5", stats.TotalDownloads)
	}
	if stats.UniqueBuyers != 2 {
		t.Errorf("UniqueBuyers = %d, want 2", stats.UniqueBuyers)
	}
	if stats.EntitlementsIssued != 2 {
		t.Errorf("EntitlementsIssued = %d, want 2", stats.EntitlementsIssued)
	}
	if stats.LastDownload == nil {
		t.Error("LastDownload is unset after five downloads")
	}

	byTitle := map[string]int64{}
	for _, f := range stats.Files {
		byTitle[f.Title] = f.DownloadCount
	}
	if byTitle["session-one.mp3"] != 3 || byTitle["session-one.mp4"] != 1 || byTitle["notes.pdf"] != 1 {
		t.Errorf("per-file counts are %v", byTitle)
	}

	// A refused click is not a download. Without this, the count would include
	// attempts, which is a different number wearing the same label.
	get(t, d.srv, "/downloads/"+alice+"/"+strconv.FormatInt(d.mp4.ID, 10))
	after, err := d.grants.Stats(t.Context(), d.product.ID)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if after.TotalDownloads != 5 {
		t.Errorf("a refused download was counted: %d", after.TotalDownloads)
	}

	// And the admin page reports them.
	signIn(t, d.srv)
	_, body := get(t, d.srv, "/admin/products/"+d.product.ID+"/downloads")
	for _, want := range []string{"session-one.mp3", "jane@example.com", "authorises the click"} {
		if !strings.Contains(body, want) {
			t.Errorf("the stats page is missing %q", want)
		}
	}
}

func TestDownloads_DigitalLinesTakeNoStockAndAreNeverOversold(t *testing.T) {
	// A download cannot run out. Without the skip in MarkPaid, every digital sale
	// would find stock_qty at 0, count as oversold, flag the order and email the
	// owner a warning about a file.
	d := newDigitalShop(t)
	d.buy(t, d.audio.ID)

	order, err := d.orders.LatestForCart(t.Context(), cartTokenOf(t, d.srv))
	if err != nil {
		t.Fatalf("LatestForCart: %v", err)
	}
	if !order.Paid() {
		t.Fatal("the order is not paid")
	}
	if order.Oversold {
		t.Error("a digital order was flagged oversold")
	}

	v, err := d.catalog.Variants(t.Context(), d.product.ID)
	if err != nil {
		t.Fatalf("Variants: %v", err)
	}
	for _, variant := range v {
		if variant.StockQty != 0 {
			t.Errorf("%s stock moved to %d", variant.SKU, variant.StockQty)
		}
	}
	// The owner's warning is the thing that would actually reach a human, so it is
	// worth asserting it did not.
	for _, m := range d.mail.Sent() {
		if strings.HasPrefix(m.Subject, "OVERSOLD") {
			t.Errorf("an oversold warning went out for a download: %q", m.Subject)
		}
	}
}

func TestDownloads_CheckoutOfDownloadsOnlyNeedsNoAddress(t *testing.T) {
	d := newDigitalShop(t)
	addToCart(t, d.srv, d.audio.ID, 1)

	_, body := get(t, d.srv, "/cart/checkout")
	if strings.Contains(body, `name="address"`) {
		t.Error("a basket of downloads still asks for a delivery address")
	}
	if !strings.Contains(body, "Nothing here needs posting") {
		t.Errorf("the page does not explain why there is no address field: %s", body)
	}

	// And the server agrees, so a submission without one is accepted rather than
	// merely un-asked-for.
	form := validCheckoutForm()
	form.Del("address")
	if res, body := post(t, d.srv, "/cart/checkout", form); res.StatusCode != http.StatusOK {
		t.Fatalf("checkout without an address = %d %s", res.StatusCode, body)
	}
}

func TestDownloads_MixedCartStillNeedsAnAddress(t *testing.T) {
	// One parcel among the downloads still has to go somewhere.
	d := newDigitalShop(t)
	d.variants = stockCart(t, d.catalog)

	addToCart(t, d.srv, d.audio.ID, 1)
	addToCart(t, d.srv, d.variants["S"].ID, 1)

	_, body := get(t, d.srv, "/cart/checkout")
	if !strings.Contains(body, `name="address"`) {
		t.Error("a mixed basket does not ask for a delivery address")
	}

	form := validCheckoutForm()
	form.Del("address")
	res, body := post(t, d.srv, "/cart/checkout", form)
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("mixed checkout without an address = %d, want 422", res.StatusCode)
	}
	if !strings.Contains(body, "Required.") {
		t.Errorf("no message on the address field: %s", body)
	}
}

func TestDownloads_EmailCarriesTheLinkAndTheOwnerCopyDoesNot(t *testing.T) {
	// The customer's receipt is where the token lives. Putting a working credential
	// in the owner's inbox as well is the kind of thing that is obvious only after
	// it has happened.
	d := newDigitalShop(t)
	s := newStore(t, func(c *config.Config) { c.OrderNotifyEmail = "shop@example.com" })
	_ = s

	token := d.buy(t, d.audio.ID)
	for _, m := range d.mail.To("jane@example.com") {
		if strings.Contains(m.Text, token) {
			goto found
		}
	}
	t.Fatal("the customer's email does not carry the download link")
found:
	for _, m := range d.mail.Sent() {
		if m.To != "jane@example.com" && strings.Contains(m.Text, token) {
			t.Errorf("a download token reached %s", m.To)
		}
	}
}

func TestDownloads_SuccessPageOffersTheFilesWithoutTheToken(t *testing.T) {
	// The buyer should not have to wait for mail. The emailed token cannot be shown
	// here — it is gone — so these links are authorised by the cart cookie, which is
	// the same credential the page already uses to show the order at all.
	d := newDigitalShop(t)
	token := d.buy(t, d.audio.ID)

	res, body := get(t, d.srv, "/cart/checkout/success")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("success page = %d", res.StatusCode)
	}
	if !strings.Contains(body, "session-one.mp3") {
		t.Errorf("the success page does not offer the files: %s", body)
	}
	if strings.Contains(body, token) {
		t.Error("the success page leaks the emailed token into HTML that is not the email")
	}

	// The links work, and go through the cart-authorised route.
	e, err := d.grants.Lookup(t.Context(), token)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	path := "/cart/checkout/downloads/" + e.ID + "/" + strconv.FormatInt(d.mp3.ID, 10)
	if !strings.Contains(body, path) {
		t.Errorf("the success page does not link %s: %s", path, body)
	}
	if res, _ := get(t, d.srv, path); res.StatusCode != http.StatusFound {
		t.Errorf("the success-page download = %d, want 302", res.StatusCode)
	}

	// And somebody else's cookie does not reach it.
	newSession(t, d.srv)
	if res, _ := get(t, d.srv, path); res.StatusCode != http.StatusNotFound {
		t.Errorf("another shopper reached this order's download: %d", res.StatusCode)
	}
}

func TestDownloads_UploadStoresTheObjectAndLinksTheVariants(t *testing.T) {
	d := newDigitalShop(t)
	signIn(t, d.srv)

	res, out := uploadFile(t, d.shop, d.product.ID, "keynote.mp4",
		[]byte("\x00\x00\x00\x20ftypisom KEYNOTE"),
		map[string]string{"title": "Closing keynote", "variant": d.video.ID})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("upload = %d %s", res.StatusCode, out)
	}

	files, err := d.catalog.Files(t.Context(), d.product.ID)
	if err != nil {
		t.Fatalf("Files: %v", err)
	}
	var added *catalog.File
	for i := range files {
		if files[i].Title == "Closing keynote" {
			added = &files[i]
		}
	}
	if added == nil {
		t.Fatalf("the file was not recorded: %+v", files)
	}
	if added.OriginalFilename != "keynote.mp4" || added.SizeBytes == 0 {
		t.Errorf("the recorded file is %+v", added)
	}
	// The key is the store's own, never the uploaded name — a filename is
	// client-controlled and this one names bytes somebody will pay for.
	if strings.Contains(added.ObjectKey, "keynote") {
		t.Errorf("the object key %q was built from the uploaded filename", added.ObjectKey)
	}
	if _, ok := d.files.Get(added.ObjectKey); !ok {
		t.Error("no object was stored under the recorded key")
	}
	// Ticked against the video variant only.
	if !added.InVariant(d.video.ID) || added.InVariant(d.audio.ID) {
		t.Errorf("variant links are %v", added.VariantIDs)
	}

	// And it is immediately downloadable by a buyer of that variant.
	newSession(t, d.srv)
	token := d.buy(t, d.video.ID)
	if res, _ := get(t, d.srv, "/downloads/"+token+"/"+strconv.FormatInt(added.ID, 10)); res.StatusCode != http.StatusFound {
		t.Errorf("the newly uploaded file = %d, want 302", res.StatusCode)
	}
}

func TestDownloads_FilesCannotBeAttachedToAPhysicalProduct(t *testing.T) {
	// Server-side, because a hand-crafted POST is the whole reason to check: rows
	// on a physical product would be read by nothing and deleted by nothing.
	d := newDigitalShop(t)
	signIn(t, d.srv)

	p, err := d.catalog.Create(t.Context(), catalog.Product{Slug: "tee", Title: "Tee", Active: true})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	res, _ := uploadFile(t, d.shop, p.ID, "x.mp3", []byte("ID3 audio"), nil)
	if res.StatusCode != http.StatusConflict {
		t.Errorf("uploading to a physical product = %d, want 409", res.StatusCode)
	}
}

func TestDownloads_KindIsFrozenOnceOrdered(t *testing.T) {
	d := newDigitalShop(t)
	d.buy(t, d.audio.ID)
	signIn(t, d.srv)

	_, body := get(t, d.srv, "/admin/products/"+d.product.ID+"/edit")
	if strings.Contains(body, `<select name="kind"`) {
		t.Error("the form still offers to change the kind of an ordered product")
	}
	if !strings.Contains(body, "has been ordered") {
		t.Errorf("the form does not say why the kind is fixed: %s", body)
	}

	// The form not offering it is presentation. The refusal that matters is the
	// server's, against a request that never saw the form.
	res, _ := post(t, d.srv, "/admin/products/"+d.product.ID, url.Values{
		"title": {"Conference 2026"}, "slug": {"conference-2026"},
		"active": {"1"}, "kind": {"physical"}, "option1_name": {"Format"},
	})
	if res.StatusCode == http.StatusSeeOther {
		t.Error("a hand-crafted request changed the kind of an ordered product")
	}
	p, err := d.catalog.Get(t.Context(), d.product.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !p.Digital() {
		t.Error("the product is no longer digital")
	}
}

func TestDownloads_DigitalToPhysicalIsRefusedWhileFilesRemain(t *testing.T) {
	// Unordered, so the ordered rule does not apply — this is the other guard.
	// Switching would leave objects in storage with nothing listing them, and
	// deleting them as a side effect of a dropdown is worse than refusing.
	d := newDigitalShop(t)
	signIn(t, d.srv)

	res, body := post(t, d.srv, "/admin/products/"+d.product.ID, url.Values{
		"title": {"Conference 2026"}, "slug": {"conference-2026"},
		"active": {"1"}, "kind": {"physical"}, "option1_name": {"Format"},
	})
	if res.StatusCode == http.StatusSeeOther {
		t.Fatal("the kind changed with files still attached")
	}
	if !strings.Contains(body, "Remove the") {
		t.Errorf("no message telling the operator what to do: %s", body)
	}
}

// uploadFile posts a download file the way the admin form does: multipart, with a
// CSRF token and the variant checkboxes.
func uploadFile(t *testing.T, s *shop, productID, filename string, content []byte, fields map[string]string) (*http.Response, string) {
	t.Helper()

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("csrf_token", csrfToken(t, s.srv)); err != nil {
		t.Fatalf("write field: %v", err)
	}
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			t.Fatalf("write field %s: %v", k, err)
		}
	}
	part, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatalf("create part: %v", err)
	}
	if _, err := part.Write(content); err != nil {
		t.Fatalf("write part: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		s.srv.URL+"/admin/products/"+productID+"/files", &buf)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Origin", s.srv.URL)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	return do(t, s.srv, req)
}

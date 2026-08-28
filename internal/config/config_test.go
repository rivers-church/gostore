package config

import (
	"strings"
	"testing"
	"time"
)

// setRequired sets every required var to something valid, so each test can
// break exactly one of them and know which failure it is asserting on.
func setRequired(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://u:p@localhost:5432/gostore")
	// PayFast's own published sandbox credentials — see .env.example.
	t.Setenv("PAYFAST_MERCHANT_ID", "10000100")
	t.Setenv("PAYFAST_MERCHANT_KEY", "46f0cd694581a")
	// Images and mail are required too. IMAGE_DIR is the cheapest of the two image
	// backends and needs nothing running, which is why it is the one used here.
	t.Setenv("IMAGE_DIR", t.TempDir())
	t.Setenv("SMTP_HOST", "localhost")
	t.Setenv("EMAIL_FROM", "orders@example.com")
}

func TestLoad_RequiresSecrets(t *testing.T) {
	// Each required var, named in the error, so a misconfigured deployment says
	// what is missing instead of failing later and less clearly.
	for _, key := range []string{
		"DATABASE_URL", "PAYFAST_MERCHANT_ID", "PAYFAST_MERCHANT_KEY",
		"SMTP_HOST", "EMAIL_FROM",
	} {
		setRequired(t)
		t.Setenv(key, "")

		_, err := Load()
		if err == nil {
			t.Errorf("%s unset: expected an error, got nil", key)
			continue
		}
		if !strings.Contains(err.Error(), key) {
			t.Errorf("%s unset: error %q does not name it", key, err)
		}
	}
}

func TestLoad_Defaults(t *testing.T) {
	setRequired(t)

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Port != "8080" {
		t.Errorf("Port = %q, want 8080", c.Port)
	}
	if c.Currency != "ZAR" {
		t.Errorf("Currency = %q, want ZAR", c.Currency)
	}
	if c.BaseURL != "http://localhost:8080" {
		t.Errorf("BaseURL = %q, want http://localhost:8080", c.BaseURL)
	}
	if c.SessionTTL != 24*time.Hour {
		t.Errorf("SessionTTL = %s, want 24h", c.SessionTTL)
	}
	// No admin credential in the environment at all: the first account is claimed
	// at /admin/setup, and there is nothing to sign a cookie with because a
	// session is a row.
	if c.SetupToken != "" {
		t.Errorf("SetupToken = %q with SETUP_TOKEN unset, want empty", c.SetupToken)
	}
	// A plain-HTTP BaseURL cannot use Secure cookies, or local development
	// could never sign in.
	if c.CookieSecure {
		t.Error("CookieSecure is set for an http:// BaseURL")
	}
}

func TestLoad_TrimsTrailingSlashFromBaseURL(t *testing.T) {
	setRequired(t)
	t.Setenv("BASE_URL", "https://store.example.com/")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.BaseURL != "https://store.example.com" {
		t.Errorf("BaseURL = %q, want https://store.example.com", c.BaseURL)
	}
	// HTTPS deployments always want Secure cookies, so this is derived rather
	// than being one more thing to forget.
	if !c.CookieSecure {
		t.Error("CookieSecure is not set for an https:// BaseURL")
	}
}

func TestLoad_RejectsBadLogLevel(t *testing.T) {
	setRequired(t)
	t.Setenv("LOG_LEVEL", "verbose")

	if _, err := Load(); err == nil {
		t.Fatal("expected an error for an unknown LOG_LEVEL, got nil")
	}
}

func TestLoad_SetupToken(t *testing.T) {
	setRequired(t)

	// Long enough is passed through verbatim: it is compared against a hash, not
	// decoded, so the server must not reinterpret what an operator supplied.
	token := strings.Repeat("t", MinSetupTokenLen)
	t.Setenv("SETUP_TOKEN", "  "+token+"  ")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.SetupToken != token {
		t.Errorf("SetupToken = %q, want the trimmed %q", c.SetupToken, token)
	}

	// A short one is refused rather than accepted: while it is unclaimed it is
	// the credential for the whole admin area, and a guessable one is worse than
	// no automated bootstrap at all.
	t.Setenv("SETUP_TOKEN", strings.Repeat("t", MinSetupTokenLen-1))
	if _, err := Load(); err == nil {
		t.Error("a SETUP_TOKEN one character short of the minimum was accepted")
	} else if !strings.Contains(err.Error(), "SETUP_TOKEN") {
		t.Errorf("error %q does not name SETUP_TOKEN", err)
	}
}

func TestLoad_EmbedOrigins(t *testing.T) {
	setRequired(t)

	// Unset means no embedding, which is the right default for a store only ever
	// browsed on its own domain.
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.AllowsEmbedding() {
		t.Error("embedding is allowed with EMBED_ORIGINS unset")
	}

	t.Setenv("EMBED_ORIGINS", " https://cms.example , https://www.cms.example:8443 ")
	c, err = Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []string{"https://cms.example", "https://www.cms.example:8443"}
	if len(c.EmbedOrigins) != len(want) {
		t.Fatalf("EmbedOrigins = %v, want %v", c.EmbedOrigins, want)
	}
	for i := range want {
		if c.EmbedOrigins[i] != want[i] {
			t.Errorf("EmbedOrigins[%d] = %q, want %q", i, c.EmbedOrigins[i], want[i])
		}
	}
	if !c.AllowsEmbedding() {
		t.Error("AllowsEmbedding() is false with two origins configured")
	}

	t.Setenv("EMBED_ORIGINS", "*")
	if c, err = Load(); err != nil || len(c.EmbedOrigins) != 1 || c.EmbedOrigins[0] != "*" {
		t.Errorf("wildcard: %v, %v", c.EmbedOrigins, err)
	}

	// These are compared literally against an Origin header, so anything that
	// could never match one is a misconfiguration worth failing on now.
	for _, bad := range []string{"cms.example", "https://cms.example/", "https://cms.example/embed", "://nope"} {
		setRequired(t)
		t.Setenv("EMBED_ORIGINS", bad)
		if _, err := Load(); err == nil {
			t.Errorf("EMBED_ORIGINS %q was accepted", bad)
		}
	}
}

func TestLoad_FontOrigins(t *testing.T) {
	setRequired(t)

	// Unset is the default and the closed position: the bundled theme uses the
	// system font stack, so no origin needs allowing and no <link> is rendered.
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.FontOrigins) != 0 || c.FontCSSURL != "" {
		t.Errorf("fonts are configured by default: %v, %q", c.FontOrigins, c.FontCSSURL)
	}

	// The Typekit shape: two hosts, because the kit's stylesheet and the font files
	// it points at are served from different ones.
	t.Setenv("FONT_ORIGINS", " https://use.typekit.net , https://p.typekit.net ")
	t.Setenv("FONT_CSS_URL", "https://use.typekit.net/abc1def.css")
	c, err = Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []string{"https://use.typekit.net", "https://p.typekit.net"}
	if len(c.FontOrigins) != len(want) {
		t.Fatalf("FontOrigins = %v, want %v", c.FontOrigins, want)
	}
	for i := range want {
		if c.FontOrigins[i] != want[i] {
			t.Errorf("FontOrigins[%d] = %q, want %q", i, c.FontOrigins[i], want[i])
		}
	}
	if c.FontCSSURL != "https://use.typekit.net/abc1def.css" {
		t.Errorf("FontCSSURL = %q", c.FontCSSURL)
	}

	// A stylesheet the CSP would block is refused at boot. In a browser the only
	// symptom is a console warning and a page in the fallback font, which is a
	// genuinely slow thing to work out.
	setRequired(t)
	t.Setenv("FONT_ORIGINS", "https://p.typekit.net")
	t.Setenv("FONT_CSS_URL", "https://use.typekit.net/abc1def.css")
	if _, err := Load(); err == nil {
		t.Error("FONT_CSS_URL on an origin FONT_ORIGINS does not list was accepted")
	}

	// A wildcard font source would let any origin serve a stylesheet to every page
	// including the checkout. The embed list may say "*"; this one may not.
	setRequired(t)
	t.Setenv("FONT_CSS_URL", "")
	t.Setenv("FONT_ORIGINS", "*")
	if _, err := Load(); err == nil {
		t.Error(`FONT_ORIGINS="*" was accepted`)
	}

	// Same literal-comparison rule as the embed origins: a CSP source carrying a
	// path matches that path exactly, so a font list entry with one is a mistake.
	for _, bad := range []string{"use.typekit.net", "https://use.typekit.net/", "https://use.typekit.net/abc1def.css"} {
		setRequired(t)
		t.Setenv("FONT_CSS_URL", "")
		t.Setenv("FONT_ORIGINS", bad)
		if _, err := Load(); err == nil {
			t.Errorf("FONT_ORIGINS %q was accepted", bad)
		}
	}

	// And a relative stylesheet URL has no origin to check against the policy.
	setRequired(t)
	t.Setenv("FONT_ORIGINS", "https://use.typekit.net")
	t.Setenv("FONT_CSS_URL", "/fonts/kit.css")
	if _, err := Load(); err == nil {
		t.Error("a relative FONT_CSS_URL was accepted")
	}
}

func TestLoad_PayFast(t *testing.T) {
	setRequired(t)

	// Sandbox is on unless explicitly turned off: the wrong default here takes
	// real money during somebody's first afternoon with the project.
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.PayFast.Sandbox {
		t.Error("PayFast.Sandbox is false by default")
	}
	if c.PayFast.AllowAnySourceIP {
		t.Error("the notification source-IP check is disabled by default")
	}
	if c.PayFast.AllowedCIDRs != nil {
		t.Errorf("AllowedCIDRs = %v with the var unset, want nil so the gateway's defaults apply", c.PayFast.AllowedCIDRs)
	}

	t.Setenv("PAYFAST_SANDBOX", "false")
	// Real credentials alongside it: turning the sandbox off while still using
	// PayFast's published demo merchant id is refused. See
	// TestLoad_RefusesRealPaymentsWithDemoCredentials.
	t.Setenv("PAYFAST_MERCHANT_ID", "20000200")
	t.Setenv("PAYFAST_ALLOWED_CIDRS", " 10.0.0.0/8 , 192.168.0.0/16 ")
	c, err = Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.PayFast.Sandbox {
		t.Error("PAYFAST_SANDBOX=false did not turn the sandbox off")
	}
	want := []string{"10.0.0.0/8", "192.168.0.0/16"}
	if len(c.PayFast.AllowedCIDRs) != len(want) {
		t.Fatalf("AllowedCIDRs = %v, want %v", c.PayFast.AllowedCIDRs, want)
	}
	for i := range want {
		if c.PayFast.AllowedCIDRs[i] != want[i] {
			t.Errorf("AllowedCIDRs[%d] = %q, want %q", i, c.PayFast.AllowedCIDRs[i], want[i])
		}
	}

	// Disabling the check is spelled out, so it cannot happen by leaving something
	// blank.
	t.Setenv("PAYFAST_ALLOWED_CIDRS", "any")
	if c, err = Load(); err != nil || !c.PayFast.AllowAnySourceIP {
		t.Errorf("PAYFAST_ALLOWED_CIDRS=any: AllowAnySourceIP = %v, err = %v", c.PayFast.AllowAnySourceIP, err)
	}

	// The notify URL is the one PayFast's servers must reach, so a relative one
	// is a misconfiguration that would only show up as a missing notification.
	for _, bad := range []string{"/payments/payfast/callback", "notify.example.com"} {
		setRequired(t)
		t.Setenv("PAYFAST_NOTIFY_URL", bad)
		if _, err := Load(); err == nil {
			t.Errorf("PAYFAST_NOTIFY_URL %q was accepted", bad)
		}
	}
}

func TestLoad_Blob(t *testing.T) {
	setRequired(t)

	// Unconfigured is a complete, working store that pastes image URLs — the same
	// way the catalog worked for five phases.
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Blob.Configured() {
		t.Error("object storage is configured with no BLOB_ENDPOINT")
	}

	blobEnv := func(t *testing.T) {
		t.Helper()
		setRequired(t)
		// The two image backends are mutually exclusive and setRequired picks the
		// directory, so a test about object storage has to put that down first.
		t.Setenv("IMAGE_DIR", "")
		t.Setenv("BLOB_ENDPOINT", "localhost:9000")
		t.Setenv("BLOB_BUCKET", "gostore-images")
		t.Setenv("BLOB_ACCESS_KEY_ID", "key")
		t.Setenv("BLOB_SECRET_ACCESS_KEY", "secret")
		t.Setenv("BLOB_PUBLIC_BASE_URL", "http://localhost:9000/gostore-images")
	}

	blobEnv(t)
	c, err = Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.Blob.Configured() {
		t.Fatal("a full BLOB_* set was not recognised")
	}
	// "auto" is what R2 wants, and GCS and MinIO ignore it.
	if c.Blob.Region != "auto" {
		t.Errorf("Region = %q, want auto", c.Blob.Region)
	}
	if !c.Blob.UseTLS {
		t.Error("TLS is off by default; it should have to be turned off deliberately")
	}
	// A trailing slash must not survive into image URLs.
	t.Setenv("BLOB_PUBLIC_BASE_URL", "https://images.example/")
	if c, err = Load(); err != nil || c.Blob.PublicBaseURL != "https://images.example" {
		t.Errorf("PublicBaseURL = %q, %v", c.Blob.PublicBaseURL, err)
	}

	// A partial configuration fails at boot, not at the first upload — and the error
	// names what is missing.
	for _, key := range []string{
		"BLOB_BUCKET", "BLOB_ACCESS_KEY_ID", "BLOB_SECRET_ACCESS_KEY", "BLOB_PUBLIC_BASE_URL",
	} {
		blobEnv(t)
		t.Setenv(key, "")

		_, err := Load()
		if err == nil {
			t.Errorf("%s unset was accepted", key)
			continue
		}
		if !strings.Contains(err.Error(), key) {
			t.Errorf("%s unset: error %q does not name it", key, err)
		}
	}

	// And the other direction: credentials with no endpoint means uploads are off
	// while somebody clearly intended them on.
	setRequired(t)
	t.Setenv("BLOB_BUCKET", "gostore-images")
	t.Setenv("BLOB_ACCESS_KEY_ID", "key")
	if _, err := Load(); err == nil {
		t.Error("BLOB_* without BLOB_ENDPOINT was accepted, so uploads would be silently off")
	}

	blobEnv(t)
	t.Setenv("BLOB_PUBLIC_BASE_URL", "/images")
	if _, err := Load(); err == nil {
		t.Error("a relative BLOB_PUBLIC_BASE_URL was accepted")
	}
}

func TestBlob_PublicOrigin(t *testing.T) {
	// The CSP source has to be the origin alone. A source carrying a path matches
	// that path *exactly*, so listing the bucket base would permit the bucket root
	// and refuse every image beneath it — which is invisible outside a browser,
	// because the image itself still returns 200.
	cases := map[string]string{
		"http://localhost:9000/gostore-images": "http://localhost:9000",
		"https://images.example":               "https://images.example",
		"https://images.example/":              "https://images.example",
		"https://pub-abc.r2.dev/bucket/nested": "https://pub-abc.r2.dev",
		"not a url":                            "",
		"":                                     "",
	}
	for base, want := range cases {
		got := Blob{PublicBaseURL: base}.PublicOrigin()
		if got != want {
			t.Errorf("Blob{%q}.PublicOrigin() = %q, want %q", base, got, want)
		}
		if strings.Count(got, "/") > 2 {
			t.Errorf("PublicOrigin() = %q still carries a path", got)
		}
	}
}

func TestLoad_TrustProxyIP(t *testing.T) {
	setRequired(t)

	// False by default: believing X-Forwarded-For without a proxy in front lets a
	// client claim any source IP, which is one of the checks on the payment
	// callback.
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.TrustProxyIP {
		t.Error("TrustProxyIP is true by default")
	}

	t.Setenv("TRUST_PROXY_IP", "true")
	if c, err = Load(); err != nil || !c.TrustProxyIP {
		t.Errorf("TRUST_PROXY_IP=true: %v, %v", c.TrustProxyIP, err)
	}

	// Anything that is not plainly true is false, so a typo cannot quietly turn
	// it on.
	t.Setenv("TRUST_PROXY_IP", "ture")
	if c, err = Load(); err != nil || c.TrustProxyIP {
		t.Errorf("TRUST_PROXY_IP=ture was treated as true")
	}
}

func TestLoad_SessionTTL(t *testing.T) {
	setRequired(t)
	t.Setenv("SESSION_TTL_HOURS", "8")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.SessionTTL != 8*time.Hour {
		t.Errorf("SessionTTL = %s, want 8h", c.SessionTTL)
	}

	for _, bad := range []string{"0", "-1", "eight", ""} {
		setRequired(t)
		t.Setenv("SESSION_TTL_HOURS", bad)
		if _, err := Load(); err == nil {
			t.Errorf("SESSION_TTL_HOURS %q was accepted", bad)
		}
	}
}

// LoadTool is what cmd/seed and the server's migration modes use. The point of
// it is what it does *not* require: a migration job gets the database URL and
// none of the secrets, so these assertions are the security property, not a
// convenience.
func TestLoadTool_NeedsOnlyDatabaseURL(t *testing.T) {
	for _, key := range []string{
		"SETUP_TOKEN", "PAYFAST_MERCHANT_ID", "PAYFAST_MERCHANT_KEY",
	} {
		t.Setenv(key, "")
	}
	t.Setenv("DATABASE_URL", "postgres://u:p@localhost:5432/gostore")

	c, err := LoadTool()
	if err != nil {
		t.Fatalf("LoadTool with only DATABASE_URL: %v", err)
	}
	if c.DatabaseURL == "" {
		t.Error("DatabaseURL is empty")
	}
	if c.LogLevel != "info" {
		t.Errorf("LogLevel = %q, want the info default", c.LogLevel)
	}
}

func TestLoadTool_RequiresDatabaseURL(t *testing.T) {
	t.Setenv("DATABASE_URL", "")

	_, err := LoadTool()
	if err == nil {
		t.Fatal("DATABASE_URL unset: expected an error, got nil")
	}
	if !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Errorf("error %q does not name DATABASE_URL", err)
	}
}

// A bad LOG_LEVEL is rejected on both paths. It was once checked only in Load,
// which meant a typo'd level was a startup failure for the server and silently
// accepted by every tool.
func TestLoadTool_RejectsBadLogLevel(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://u:p@localhost:5432/gostore")
	t.Setenv("LOG_LEVEL", "verbose")

	if _, err := LoadTool(); err == nil {
		t.Error("LOG_LEVEL=verbose was accepted")
	}
}

func TestLoad_RefusesRealPaymentsWithDemoCredentials(t *testing.T) {
	// The mistake this catches is the second half of a two-step one: a store is
	// found to have been quietly running against the sandbox, somebody sets
	// PAYFAST_SANDBOX=false, and does not notice that the merchant id came from
	// .env.example too. Every payment would then be signed with a key printed in
	// PayFast's own documentation.
	setRequired(t) // leaves PAYFAST_MERCHANT_ID at the published sandbox id
	t.Setenv("PAYFAST_SANDBOX", "false")

	_, err := Load()
	if err == nil {
		t.Fatal("real payments were accepted with the demo merchant id")
	}
	// The message has to name the variable and the value, because the person
	// reading it has just been told their deployment will not start.
	for _, want := range []string{"PAYFAST_SANDBOX", "PAYFAST_MERCHANT_ID", "10000100"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %s: %v", want, err)
		}
	}

	// The same credentials are fine while the sandbox is on — that is the whole
	// point of shipping them.
	t.Setenv("PAYFAST_SANDBOX", "true")
	if _, err := Load(); err != nil {
		t.Errorf("the demo credentials were refused in sandbox mode: %v", err)
	}

	// And real credentials with the sandbox off are the ordinary production case.
	t.Setenv("PAYFAST_SANDBOX", "false")
	t.Setenv("PAYFAST_MERCHANT_ID", "20000200")
	if _, err := Load(); err != nil {
		t.Errorf("real credentials with the sandbox off were refused: %v", err)
	}
}

func TestLoad_DownloadStorageIsAllOrNothing(t *testing.T) {
	// The same stance the image bucket takes: a partial configuration would fail at
	// the first upload with whichever piece is missing, which is a worse place to
	// find out than at boot.
	for _, key := range []string{
		"DOWNLOAD_BUCKET", "DOWNLOAD_ACCESS_KEY_ID", "DOWNLOAD_SECRET_ACCESS_KEY",
	} {
		setRequired(t)
		t.Setenv("DOWNLOAD_ENDPOINT", "storage.googleapis.com")
		t.Setenv("DOWNLOAD_BUCKET", "gostore-downloads")
		t.Setenv("DOWNLOAD_ACCESS_KEY_ID", "key")
		t.Setenv("DOWNLOAD_SECRET_ACCESS_KEY", "secret")
		t.Setenv(key, "")

		_, err := Load()
		if err == nil {
			t.Errorf("%s unset: expected an error", key)
			continue
		}
		if !strings.Contains(err.Error(), key) {
			t.Errorf("%s unset: error %q does not name it", key, err)
		}
	}

	// And the other way round: settings that would do nothing without an endpoint
	// are a mistake worth naming, not a silent no-op that leaves uploads off.
	setRequired(t)
	t.Setenv("DOWNLOAD_BUCKET", "gostore-downloads")
	if _, err := Load(); err == nil {
		t.Error("DOWNLOAD_BUCKET without DOWNLOAD_ENDPOINT was accepted")
	}
}

func TestLoad_RefusesTwoDownloadBackends(t *testing.T) {
	setRequired(t)
	t.Setenv("DOWNLOAD_DIR", t.TempDir())
	t.Setenv("DOWNLOAD_ENDPOINT", "storage.googleapis.com")
	t.Setenv("DOWNLOAD_BUCKET", "gostore-downloads")
	t.Setenv("DOWNLOAD_ACCESS_KEY_ID", "key")
	t.Setenv("DOWNLOAD_SECRET_ACCESS_KEY", "secret")

	_, err := Load()
	if err == nil {
		t.Fatal("both download backends configured was accepted")
	}
	if !strings.Contains(err.Error(), "one or the other") {
		t.Errorf("error %q does not explain the exclusion", err)
	}
}

func TestLoad_RefusesADownloadDirInsideTheImageDir(t *testing.T) {
	// The check that matters most in this whole feature. Everything under IMAGE_DIR
	// is served publicly and unauthenticated at /images/, so an overlap would
	// publish every purchased file to anybody who guessed a key — the exact failure
	// the private store exists to prevent, arrived at by configuration rather than
	// by a bug. Boot is the only place to catch it.
	images := t.TempDir()

	cases := map[string]string{
		"download dir inside the image dir": images + "/files",
		"the same directory":                images,
	}
	for name, downloads := range cases {
		setRequired(t)
		t.Setenv("IMAGE_DIR", images)
		t.Setenv("DOWNLOAD_DIR", downloads)

		_, err := Load()
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if !strings.Contains(err.Error(), "/images/") {
			t.Errorf("%s: error %q does not explain that IMAGE_DIR is public", name, err)
		}
	}

	// The reverse containment too, which is just as bad and easier to miss.
	setRequired(t)
	downloads := t.TempDir()
	t.Setenv("DOWNLOAD_DIR", downloads)
	t.Setenv("IMAGE_DIR", downloads+"/pictures")
	if _, err := Load(); err == nil {
		t.Error("an image dir inside the download dir was accepted")
	}

	// Two separate directories are fine, or the check would be useless.
	setRequired(t)
	t.Setenv("IMAGE_DIR", t.TempDir())
	t.Setenv("DOWNLOAD_DIR", t.TempDir())
	if _, err := Load(); err != nil {
		t.Errorf("two separate directories were refused: %v", err)
	}
}

func TestLoad_RefusesTheImageBucketForDownloads(t *testing.T) {
	// The bucket half of the same mistake. The image bucket is publicly readable by
	// design, so pointing downloads at it would make every purchased file
	// anonymously fetchable — and nothing downstream would notice.
	setRequired(t)
	t.Setenv("IMAGE_DIR", "")
	t.Setenv("BLOB_ENDPOINT", "storage.googleapis.com")
	t.Setenv("BLOB_BUCKET", "gostore-assets")
	t.Setenv("BLOB_ACCESS_KEY_ID", "key")
	t.Setenv("BLOB_SECRET_ACCESS_KEY", "secret")
	t.Setenv("BLOB_PUBLIC_BASE_URL", "https://cdn.example/gostore-assets")
	t.Setenv("DOWNLOAD_ENDPOINT", "storage.googleapis.com")
	t.Setenv("DOWNLOAD_BUCKET", "GOSTORE-ASSETS") // case-insensitively the same
	t.Setenv("DOWNLOAD_ACCESS_KEY_ID", "key")
	t.Setenv("DOWNLOAD_SECRET_ACCESS_KEY", "secret")

	_, err := Load()
	if err == nil {
		t.Fatal("the public image bucket was accepted as the download bucket")
	}
	if !strings.Contains(err.Error(), "publicly readable") {
		t.Errorf("error %q does not explain why", err)
	}

	// A different bucket on the same endpoint is the normal setup and must pass.
	t.Setenv("DOWNLOAD_BUCKET", "gostore-downloads")
	if _, err := Load(); err != nil {
		t.Errorf("a separate download bucket was refused: %v", err)
	}
}

func TestBytesEnv(t *testing.T) {
	const def = int64(2 << 30)
	cases := map[string]int64{
		"":            def,
		"1024":        1024,
		"5MB":         5_000_000,
		"5MiB":        5 << 20,
		"2GiB":        2 << 30,
		"2G":          2 << 30,
		"  512 KiB  ": 512 << 10,
		// A malformed or absurd value falls back rather than failing the boot: this
		// is a cap on an admin's own upload, and refusing to start a shop over it
		// would be out of proportion.
		"abc":                   def,
		"-1":                    def,
		"0":                     def,
		"99999999999999999 GiB": def,
	}
	for in, want := range cases {
		t.Setenv("TEST_BYTES", in)
		if got := bytesEnv("TEST_BYTES", def); got != want {
			t.Errorf("bytesEnv(%q) = %d, want %d", in, got, want)
		}
	}
}

// setOAuth configures a complete app registration, so each case below can break
// exactly one part of it.
func setOAuth(t *testing.T) {
	t.Helper()
	t.Setenv("SMTP_USERNAME", "orders@example.com")
	t.Setenv("SMTP_OAUTH_TENANT_ID", "tenant-id")
	t.Setenv("SMTP_OAUTH_CLIENT_ID", "client-id")
	t.Setenv("SMTP_OAUTH_CLIENT_SECRET", "client-secret")
}

func TestLoad_SMTPOAuth(t *testing.T) {
	setRequired(t)
	setOAuth(t)

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.SMTP.OAuth.Configured() {
		t.Fatalf("OAuth = %+v, want it configured", c.SMTP.OAuth)
	}
	if c.SMTP.OAuth.TenantID != "tenant-id" || c.SMTP.OAuth.ClientID != "client-id" ||
		c.SMTP.OAuth.ClientSecret != "client-secret" {
		t.Errorf("OAuth = %+v, want the configured registration", c.SMTP.OAuth)
	}

	// Absent entirely is the ordinary case, not an error: a relay reached with a
	// password, or none at all, is still supported.
	t.Setenv("SMTP_OAUTH_TENANT_ID", "")
	t.Setenv("SMTP_OAUTH_CLIENT_ID", "")
	t.Setenv("SMTP_OAUTH_CLIENT_SECRET", "")
	c, err = Load()
	if err != nil {
		t.Fatalf("Load with no OAuth: %v", err)
	}
	if c.SMTP.OAuth.Configured() {
		t.Error("an empty environment produced a configured registration")
	}
}

// A half-configured registration is a boot failure. The alternative is a store
// that starts, takes an order, and only then discovers it cannot authenticate —
// with the buyer's download link in the message it failed to send.
func TestLoad_SMTPOAuthMustBeComplete(t *testing.T) {
	for _, missing := range []string{
		"SMTP_OAUTH_TENANT_ID", "SMTP_OAUTH_CLIENT_ID", "SMTP_OAUTH_CLIENT_SECRET",
	} {
		t.Run(missing, func(t *testing.T) {
			setRequired(t)
			setOAuth(t)
			t.Setenv(missing, "")

			_, err := Load()
			if err == nil {
				t.Fatalf("a registration missing %s was accepted", missing)
			}
			if !strings.Contains(err.Error(), "must be set together") {
				t.Errorf("error = %v, want it to say the three go together", err)
			}
		})
	}
}

func TestLoad_SMTPOAuthNeedsAUsername(t *testing.T) {
	setRequired(t)
	setOAuth(t)
	t.Setenv("SMTP_USERNAME", "")

	_, err := Load()
	if err == nil {
		t.Fatal("OAuth with no username was accepted")
	}
	if !strings.Contains(err.Error(), "SMTP_USERNAME") {
		t.Errorf("error = %v, want it to name SMTP_USERNAME", err)
	}
}

// Refused rather than resolved by precedence: both being set means somebody has
// a belief about which one is in use, and a silent winner would leave a stale
// secret in the environment looking live.
func TestLoad_SMTPOAuthAndPasswordConflict(t *testing.T) {
	setRequired(t)
	setOAuth(t)
	t.Setenv("SMTP_PASSWORD", "hunter2")

	_, err := Load()
	if err == nil {
		t.Fatal("a password and an app registration were both accepted")
	}
	if !strings.Contains(err.Error(), "SMTP_PASSWORD") {
		t.Errorf("error = %v, want it to name SMTP_PASSWORD", err)
	}
}

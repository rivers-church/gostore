// Package config loads all runtime configuration from the environment.
//
// Everything the server needs comes from env vars, so the same binary and image
// run unchanged on a VM, in Compose, or on a managed container platform.
package config

import (
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/17xande-dev/gostore/internal/blob"
	"github.com/17xande-dev/gostore/internal/email"
)

// Config is the fully resolved configuration for one server process.
type Config struct {
	// Server
	Port            string
	BaseURL         string
	ShutdownTimeout time.Duration

	// Storage
	DatabaseURL string

	// Store presentation. No organisation-specific defaults live in the code;
	// adopters set these.
	StoreName string
	Currency  string

	// TemplateDir, when set, overlays same-named templates from disk over the
	// embedded defaults, so adopters can restyle without forking.
	TemplateDir string

	// StaticDir is TemplateDir's counterpart for assets: a file there shadows a
	// bundled one of the same name, and a new name is served too. Replacing the logo
	// is dropping a logo.svg into it — restyling without a way to supply an image
	// would be half a feature.
	StaticDir string

	// ThemeReload re-reads TemplateDir and StaticDir on every request instead of
	// once at startup, so editing a theme file and refreshing the page is enough to
	// see it. It is for writing a theme and nothing else: it costs a parse of every
	// template and a read of every asset per request, and it turns a typo in a
	// template into a 500 at request time rather than a boot failure. Off unless
	// THEME_RELOAD says otherwise; the compose stack sets it.
	ThemeReload bool

	// SessionTTL is how long a sign-in lasts. A session is a row in
	// admin_sessions and a cookie carrying a random token; there is no secret to
	// configure, because there is nothing signed to verify — see internal/auth.
	SessionTTL time.Duration

	// SetupToken is the one-time token that may claim the first administrator
	// account, supplied rather than generated.
	//
	// It exists for an automated deploy, which cannot read a token out of a log
	// line and paste it into a form. Empty is the ordinary case: the server
	// generates one on first boot against an empty admin_users and prints it.
	// Either way it is stored only as a hash and is spent by the first claim.
	SetupToken string

	// PayFast is the payment gateway's configuration. It is a flat struct here
	// rather than the gateway package's own Config so that config depends on no
	// gateway: main assembles the two, which is also where a second gateway
	// would be chosen.
	PayFast PayFast

	// SMTP is how transactional mail leaves. It is optional: a store with no mail
	// server still takes orders correctly, and refusing to boot over it would
	// trade a working shop for a missing receipt. An unconfigured deployment logs
	// loudly at startup and again for every message it drops.
	SMTP SMTP

	// OrderNotifyEmail is where a copy of each paid order goes — whoever packs the
	// parcel. Empty means the customer's confirmation is the only mail sent, and
	// the operator finds orders in /admin/orders instead.
	OrderNotifyEmail string

	// Blob is object storage for product images.
	Blob Blob

	// ImageDir stores product images in a local directory served by this server,
	// for a shop that wants no object storage at all. Mutually exclusive with Blob:
	// two configured backends would leave "which one wins" to be guessed.
	//
	// A product image is only ever a bucket object or a file here. Pasting a URL
	// from the general internet used to be allowed and no longer is: those bytes
	// belong to somebody else, who can change or delete them, and a product page
	// with a broken image is worse than one with none.
	ImageDir string

	// Downloads is PRIVATE object storage for the files a digital product is made
	// of. Separate from Blob, and it has to be: the image bucket is publicly
	// readable by design, so a paid file in it would be one URL guess away from
	// everybody. A private prefix inside a public bucket is not an option either,
	// because public access on GCS and R2 is a whole-bucket toggle.
	Downloads Downloads

	// DownloadDir is the local-directory alternative, on the same terms as
	// ImageDir. Mutually exclusive with Downloads, and it must not be the
	// directory images are served from — that one is published at /images/, which
	// would hand every purchased file to anybody who guessed a key.
	DownloadDir string

	// DownloadMaxBytes caps an uploaded file. Audio and video, so the default is
	// gigabytes rather than the megabytes an image gets. The upload streams
	// straight to storage, so this is about what a shop should store and how long
	// a request may hold open, not about memory.
	DownloadMaxBytes int64

	// RateLimits are the per-IP limits on the three surfaces worth protecting.
	// Defaults are deliberately loose enough that no real shopper or operator
	// meets one — a limit that fires on ordinary use gets turned off.
	RateLimits RateLimits

	// CartTTLDays is how long an untouched cart survives before the cleanup job
	// removes it, keeping the carts table bounded.
	CartTTLDays int

	// TrustProxyIP makes the server believe X-Forwarded-For. It must be false
	// unless something in front of the server is actually setting that header,
	// because a client can otherwise claim any IP it likes — and the payment
	// callback's source-IP check is one of the things that would then be
	// trivially bypassed.
	TrustProxyIP bool

	// CookieSecure is derived from BaseURL rather than configured separately:
	// an HTTPS deployment always wants Secure cookies, and localhost
	// development cannot use them.
	CookieSecure bool

	// ShowErrorDetail puts the underlying error on the error page, instead of only
	// a reference to find in the logs. Derived from the same signal as
	// CookieSecure, deliberately: "is this production" should have one answer
	// rather than three, and this is already the question HSTS and the CSRF
	// cookie's TLS mode are decided by.
	//
	// The detail is the Go error string, never a stack trace. That string names
	// tables, columns and constraints, which is reconnaissance for anybody probing
	// the store, so it is off the moment BaseURL is https — and an http deployment
	// is one this project already treats as not-production, since it gets neither
	// Secure cookies nor HSTS.
	ShowErrorDetail bool

	// EmbedOrigins are the origins allowed to fetch the read-only catalog
	// fragments cross-origin, for dropping the catalog into a page hosted
	// elsewhere. Empty means no CORS headers at all, which is the right default
	// for a store that is only ever browsed on its own domain.
	EmbedOrigins []string

	// FontOrigins are the origins a web font may be loaded from besides this one.
	// Empty — the default — means the CSP stays closed and the theme uses the
	// system font stack.
	//
	// This opens *two* CSP directives, which is worth knowing before setting it: a
	// hosted font service serves a stylesheet that declares the fonts and then the
	// font files that stylesheet points at, so the origins land in both style-src
	// and font-src. Allowing the fonts without the stylesheet declaring them
	// half-works, which is a slow thing to diagnose. It does not open script-src:
	// a font service still cannot run JavaScript on the checkout page, which is
	// the property that makes this a narrow widening rather than a general one.
	FontOrigins []string

	// FontCSSURL is a stylesheet the default layout links from its <head>, for the
	// hosted-font case where the service gives you a CSS URL — an Adobe Fonts kit,
	// typically. Empty means no such link is rendered at all.
	//
	// Its origin must appear in FontOrigins, checked at boot: a link the CSP then
	// blocks fails silently apart from a console warning, and the page renders in
	// the fallback font with nothing to suggest why.
	//
	// Deliberately a CSS URL and not a script: a font service's JavaScript loader
	// needs script-src widened, connect-src opened for its config fetch, and a
	// nonce for the inline snippet and the inline <style> it injects. That is a far
	// larger concession than this one, and this project does not offer it.
	FontCSSURL string

	// LogLevel is one of debug, info, warn, error.
	LogLevel string

	// LogFormat is "json" or "gcp". Both are JSON on stdout; "gcp" renames two
	// keys — level to severity, msg to message — because that is what Google Cloud
	// Logging reads. Without it every line files as DEFAULT severity, so
	// `severity>=ERROR` matches nothing and alerting on the error rate silently
	// never fires.
	//
	// Opt-in rather than automatic: the rename is a Google convention, and this
	// project does not assume anybody's platform.
	LogFormat string
}

// payFastSandboxMerchantID is the merchant id PayFast publishes in its own
// documentation for testing, and therefore the one every copy of this project's
// .env.example and compose.yaml carries. It is a constant here so that "still on
// the demo credentials" is something the config can recognise.
// MinSetupTokenLen is the shortest SETUP_TOKEN accepted. The token is, for as
// long as it is unclaimed, the credential for the whole admin area, so it is held
// to the length 32 random bytes reach in base64 rather than to anything a person
// would type.
const MinSetupTokenLen = 32

const payFastSandboxMerchantID = "10000100"

// PayFast is what the PayFast gateway needs from the environment. The merchant
// id and key are required: a store that cannot take a payment is not a store, and
// discovering that at the first checkout is worse than discovering it at boot.
//
// Notification URLs are derived from BaseURL rather than configured, with one
// override — NotifyURL — because that is the one PayFast's own servers have to
// reach, which on a development machine means a tunnel's hostname and not
// localhost.
type PayFast struct {
	MerchantID  string
	MerchantKey string
	Passphrase  string
	Sandbox     bool

	NotifyURL string

	// AllowedCIDRs overrides PayFast's published source ranges. It is
	// configuration rather than a constant because PayFast has changed its ranges
	// before, and adding one should not need a release of this project.
	AllowedCIDRs []string
	// AllowAnySourceIP disables the source-IP check entirely. See
	// PAYFAST_ALLOWED_CIDRS=any in .env.example: it is for testing against the
	// sandbox and never right in production.
	AllowAnySourceIP bool
}

// Blob is object storage for product images, against anything speaking the S3
// API — Cloudflare R2, Google Cloud Storage in interoperability mode, or MinIO.
//
// PublicBaseURL is separate from Endpoint and cannot be derived from it: the
// address a bucket is written through and the address it is read from are
// routinely different — R2 writes to <account>.r2.cloudflarestorage.com and reads
// from a custom domain — and only the operator knows the second one.
type Blob struct {
	Endpoint  string
	Bucket    string
	AccessKey string
	SecretKey string

	// Region is "auto" for R2. GCS and MinIO ignore it.
	Region string
	UseTLS bool

	PublicBaseURL string
}

// Configured reports whether object storage is set up.
func (b Blob) Configured() bool { return b.Endpoint != "" }

// PublicOrigin is the scheme and host images are served from, with any path
// stripped — which is what a Content-Security-Policy source has to be.
//
// This is not fussiness. A CSP source whose path does not end in "/" must match a
// URL's path *exactly*, so listing "http://host:9000/bucket" permits that one URL
// and refuses "http://host:9000/bucket/products/x.jpg" — every actual image. MinIO
// and any path-style bucket URL hit this, and the failure is invisible outside a
// browser: the image returns 200 to curl, the markup is correct, and the page shows
// a broken image with a console warning nothing else surfaces.
func (b Blob) PublicOrigin() string {
	u, err := url.Parse(b.PublicBaseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		// Load has already rejected this, so reaching here means the value changed
		// underneath us. An empty source is safe: it permits nothing.
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// Downloads is private object storage for purchased files.
//
// It has no PublicBaseURL, and the absence is the design. These objects are never
// addressed publicly: a buyer's link points at this server, which checks the
// entitlement, records the click and only then produces a short-lived signed URL.
// A base URL here would be a standing invitation to build a permanent one.
type Downloads struct {
	Endpoint  string
	Bucket    string
	AccessKey string
	SecretKey string

	Region string
	UseTLS bool

	// PublicEndpoint is the address a *browser* reaches this bucket at, when that
	// differs from Endpoint. Empty means they are the same, which is the case in
	// every real deployment; it exists for a development stack where the server
	// and the browser are on different sides of a container network.
	//
	// A presigned URL's signature covers the Host header, so a URL signed for one
	// address cannot be rewritten to another — it has to be signed for the
	// browser's address from the start. Same idea as Blob.PublicBaseURL, one layer
	// down.
	PublicEndpoint string
}

// Configured reports whether download storage is set up.
func (d Downloads) Configured() bool { return d.Endpoint != "" }

// DownloadsEnabled reports whether this shop can sell digital products at all.
// With neither backend configured the admin says so rather than offering an
// upload form that could only fail.
func (c Config) DownloadsEnabled() bool { return c.Downloads.Configured() || c.DownloadDir != "" }

// ImagesEnabled reports whether a product can have an image at all — by upload to
// object storage or to a local directory. With neither, the admin says so rather
// than offering a form that could only fail.
func (c Config) ImagesEnabled() bool { return c.Blob.Configured() || c.ImageDir != "" }

// RateLimits holds the per-IP limits. Each is a number of requests per minute
// with a burst; see middleware.RateLimit for what the two mean together.
type RateLimits struct {
	// LoginPerMinute guards the admin password against brute force. Low, because
	// an operator signs in once.
	LoginPerMinute int
	// CheckoutPerMinute guards order creation. Loose, because refusing a real
	// shopper costs a sale and double-clicking is normal.
	CheckoutPerMinute int
	// CallbackPerMinute guards the payment callback, which is unauthenticated and
	// makes the store POST to the gateway for every request it accepts. Generous:
	// a throttled notification is retried, but throttling a busy shop's genuine
	// traffic delays real payments.
	CallbackPerMinute int
	// DownloadPerMinute guards the buyer's download links, which mint a signed URL
	// per click. Generous on purpose: somebody working through a conference
	// recording's twenty files is ordinary use, and a limit that fires on it would
	// be switched off rather than tuned.
	DownloadPerMinute int
}

// SMTP is the mail relay's configuration. Username and Password may be empty, for
// a relay that authenticates by network address — mailpit in development being the
// case that matters here.
type SMTP struct {
	Host     string
	Port     int
	Username string
	Password string

	// From is the sender address. A relay usually rejects a From it does not
	// consider itself responsible for, so this has to be on a domain it accepts.
	From    string
	ReplyTo string

	// TLS is "starttls" (the default, correct for port 587), "tls" (implicit, for
	// 465) or "none" (development only).
	TLS string
}

// Configured reports whether mail can actually be sent. Both a host and a From
// address are needed: a relay with no sender is not a working configuration, and
// half-configured is the case worth catching at startup.
func (s SMTP) Configured() bool { return s.Host != "" && s.From != "" }

// AllowsEmbedding reports whether any origin may fetch the catalog fragments.
func (c Config) AllowsEmbedding() bool { return len(c.EmbedOrigins) > 0 }

// Load reads configuration from the environment, applying defaults and
// returning an error listing every missing or malformed required value.
func Load() (Config, error) {
	c := Config{
		Port:            env("PORT", "8080"),
		BaseURL:         strings.TrimRight(env("BASE_URL", "http://localhost:8080"), "/"),
		DatabaseURL:     os.Getenv("DATABASE_URL"),
		StoreName:       env("STORE_NAME", "gostore"),
		Currency:        env("CURRENCY", "ZAR"),
		TemplateDir:     os.Getenv("TEMPLATE_DIR"),
		StaticDir:       strings.TrimSpace(os.Getenv("STATIC_DIR")),
		ThemeReload:     boolEnv("THEME_RELOAD", false),
		LogLevel:        env("LOG_LEVEL", "info"),
		LogFormat:       env("LOG_FORMAT", "json"),
		SetupToken:      strings.TrimSpace(os.Getenv("SETUP_TOKEN")),
		SessionTTL:      24 * time.Hour,
		ShutdownTimeout: 15 * time.Second,
		TrustProxyIP:    boolEnv("TRUST_PROXY_IP", false),
		CartTTLDays:     60,
		RateLimits: RateLimits{
			LoginPerMinute:    10,
			CheckoutPerMinute: 20,
			CallbackPerMinute: 120,
			DownloadPerMinute: 60,
		},
		OrderNotifyEmail: strings.TrimSpace(os.Getenv("ORDER_NOTIFY_EMAIL")),
		ImageDir:         strings.TrimSpace(os.Getenv("IMAGE_DIR")),
		Blob: Blob{
			Endpoint:      strings.TrimSpace(os.Getenv("BLOB_ENDPOINT")),
			Bucket:        strings.TrimSpace(os.Getenv("BLOB_BUCKET")),
			AccessKey:     os.Getenv("BLOB_ACCESS_KEY_ID"),
			SecretKey:     os.Getenv("BLOB_SECRET_ACCESS_KEY"),
			Region:        env("BLOB_REGION", "auto"),
			UseTLS:        boolEnv("BLOB_USE_TLS", true),
			PublicBaseURL: strings.TrimRight(strings.TrimSpace(os.Getenv("BLOB_PUBLIC_BASE_URL")), "/"),
		},
		DownloadDir:      strings.TrimSpace(os.Getenv("DOWNLOAD_DIR")),
		DownloadMaxBytes: bytesEnv("DOWNLOAD_MAX_BYTES", blob.DefaultMaxDownloadBytes),
		Downloads: Downloads{
			Endpoint:       strings.TrimSpace(os.Getenv("DOWNLOAD_ENDPOINT")),
			Bucket:         strings.TrimSpace(os.Getenv("DOWNLOAD_BUCKET")),
			AccessKey:      os.Getenv("DOWNLOAD_ACCESS_KEY_ID"),
			SecretKey:      os.Getenv("DOWNLOAD_SECRET_ACCESS_KEY"),
			Region:         env("DOWNLOAD_REGION", "auto"),
			UseTLS:         boolEnv("DOWNLOAD_USE_TLS", true),
			PublicEndpoint: strings.TrimSpace(os.Getenv("DOWNLOAD_PUBLIC_ENDPOINT")),
		},
		SMTP: SMTP{
			Host:     strings.TrimSpace(os.Getenv("SMTP_HOST")),
			Username: os.Getenv("SMTP_USERNAME"),
			Password: os.Getenv("SMTP_PASSWORD"),
			From:     strings.TrimSpace(os.Getenv("EMAIL_FROM")),
			ReplyTo:  strings.TrimSpace(os.Getenv("EMAIL_REPLY_TO")),
			TLS:      env("SMTP_TLS", "starttls"),
		},
		PayFast: PayFast{
			MerchantID:  os.Getenv("PAYFAST_MERCHANT_ID"),
			MerchantKey: os.Getenv("PAYFAST_MERCHANT_KEY"),
			Passphrase:  os.Getenv("PAYFAST_PASSPHRASE"),
			// Sandbox defaults to true: the wrong default here takes real money
			// from a real card during somebody's first afternoon with the project.
			//
			// The cost of that default is the mirror failure — a production
			// deployment that never sets it takes no money and looks fine — so
			// anything that deploys this is expected to set it explicitly, and
			// infra/terraform requires it. See the check further down for the
			// half-done version of turning it off.
			Sandbox:   boolEnv("PAYFAST_SANDBOX", true),
			NotifyURL: strings.TrimSpace(os.Getenv("PAYFAST_NOTIFY_URL")),
		},
	}
	c.CookieSecure = strings.HasPrefix(c.BaseURL, "https://")
	// One signal, three uses: Secure cookies, HSTS, and whether an error page may
	// say what actually went wrong.
	c.ShowErrorDetail = !c.CookieSecure

	var missing []string
	if c.DatabaseURL == "" {
		missing = append(missing, "DATABASE_URL")
	}
	if c.PayFast.MerchantID == "" {
		missing = append(missing, "PAYFAST_MERCHANT_ID")
	}
	if c.PayFast.MerchantKey == "" {
		missing = append(missing, "PAYFAST_MERCHANT_KEY")
	}
	if len(missing) > 0 {
		return Config{}, fmt.Errorf("config: required env vars not set: %s", strings.Join(missing, ", "))
	}

	// There is no admin credential in the environment any more: accounts live in
	// admin_users and the first one is claimed at /admin/setup. What can be
	// supplied is the token that authorises that claim, and a short one is a
	// guessable credential for the whole admin area — so it is bounded here,
	// where the message can say so, rather than accepted and regretted.
	if c.SetupToken != "" && len(c.SetupToken) < MinSetupTokenLen {
		return Config{}, fmt.Errorf("config: SETUP_TOKEN is %d characters, want at least %d "+
			"(generate one with `openssl rand -base64 32`)", len(c.SetupToken), MinSetupTokenLen)
	}

	if h, ok := os.LookupEnv("SESSION_TTL_HOURS"); ok {
		n, err := strconv.Atoi(h)
		if err != nil || n <= 0 {
			return Config{}, fmt.Errorf("config: SESSION_TTL_HOURS must be a positive integer, got %q", h)
		}
		c.SessionTTL = time.Duration(n) * time.Hour
	}

	if d, ok := os.LookupEnv("SHUTDOWN_TIMEOUT_SECONDS"); ok {
		n, err := strconv.Atoi(d)
		if err != nil || n < 0 {
			return Config{}, fmt.Errorf("config: SHUTDOWN_TIMEOUT_SECONDS must be a non-negative integer, got %q", d)
		}
		c.ShutdownTimeout = time.Duration(n) * time.Second
	}

	if err := checkLogLevel(c.LogLevel); err != nil {
		return Config{}, err
	}
	if err := checkLogFormat(c.LogFormat); err != nil {
		return Config{}, err
	}

	// Taking real money with the demo credentials is a contradiction, and it is
	// the mistake that follows naturally from the other one: somebody discovers
	// their store has been quietly running against the sandbox, sets
	// PAYFAST_SANDBOX=false, and does not realise the merchant id came from
	// .env.example too. Every payment would then be signed with a key published
	// in PayFast's own documentation.
	//
	// Refused at boot, where it costs one message, rather than at the first
	// checkout, where it costs a customer.
	if !c.PayFast.Sandbox && c.PayFast.MerchantID == payFastSandboxMerchantID {
		return Config{}, fmt.Errorf(
			"config: PAYFAST_SANDBOX is false but PAYFAST_MERCHANT_ID is still %s, "+
				"which is PayFast's published sandbox merchant id — set your own "+
				"credentials, or leave PAYFAST_SANDBOX=true", payFastSandboxMerchantID)
	}

	var err error
	// "*" is allowed here and nowhere else: the fragments these origins may fetch
	// are cookie-free and read-only, so a permissive list cannot become a way to
	// act as somebody.
	c.EmbedOrigins, err = parseOrigins("EMBED_ORIGINS", true)
	if err != nil {
		return Config{}, err
	}

	// Whereas a wildcard font source would let any origin serve a stylesheet to
	// every page of the store, the checkout included — the opposite of what this
	// directive is for. So: list them.
	c.FontOrigins, err = parseOrigins("FONT_ORIGINS", false)
	if err != nil {
		return Config{}, err
	}

	if c.FontCSSURL = strings.TrimSpace(os.Getenv("FONT_CSS_URL")); c.FontCSSURL != "" {
		u, err := url.Parse(c.FontCSSURL)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return Config{}, fmt.Errorf("config: FONT_CSS_URL %q must be an absolute URL", c.FontCSSURL)
		}
		// The stylesheet's own origin has to be allowed to serve it. Refused at boot
		// rather than in the browser, where the only symptom is a console warning and
		// a page rendered in the fallback font.
		origin := u.Scheme + "://" + u.Host
		if !slices.Contains(c.FontOrigins, origin) {
			return Config{}, fmt.Errorf(
				"config: FONT_CSS_URL is on %s, which FONT_ORIGINS does not list — "+
					"the CSP would block the stylesheet; add %s to FONT_ORIGINS", origin, origin)
		}
	}

	// The limits are configurable because the right number depends on a shop's
	// traffic, and 0 means "no limit on this surface" — spelled out rather than
	// implied by an empty value, since switching a protection off should be
	// something an operator typed.
	for _, l := range []struct {
		key string
		dst *int
	}{
		{"RATE_LIMIT_LOGIN_PER_MINUTE", &c.RateLimits.LoginPerMinute},
		{"RATE_LIMIT_CHECKOUT_PER_MINUTE", &c.RateLimits.CheckoutPerMinute},
		{"RATE_LIMIT_CALLBACK_PER_MINUTE", &c.RateLimits.CallbackPerMinute},
		{"RATE_LIMIT_DOWNLOAD_PER_MINUTE", &c.RateLimits.DownloadPerMinute},
	} {
		if v, ok := os.LookupEnv(l.key); ok {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				return Config{}, fmt.Errorf("config: %s must be a non-negative integer, got %q", l.key, v)
			}
			*l.dst = n
		}
	}

	if v, ok := os.LookupEnv("CART_TTL_DAYS"); ok {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return Config{}, fmt.Errorf("config: CART_TTL_DAYS must be a positive integer, got %q", v)
		}
		c.CartTTLDays = n
	}

	// Mail is validated whenever any of it is set, so a half-configured relay is a
	// boot failure rather than a receipt that silently never arrives.
	c.SMTP.Port = 587
	if p, ok := os.LookupEnv("SMTP_PORT"); ok {
		n, err := strconv.Atoi(p)
		if err != nil || n <= 0 || n > 65535 {
			return Config{}, fmt.Errorf("config: SMTP_PORT must be a port number, got %q", p)
		}
		c.SMTP.Port = n
	}
	if _, err := email.ParseTLSPolicy(c.SMTP.TLS); err != nil {
		return Config{}, fmt.Errorf("config: SMTP_TLS: %w", err)
	}
	if (c.SMTP.Host == "") != (c.SMTP.From == "") {
		return Config{}, fmt.Errorf(
			"config: SMTP_HOST and EMAIL_FROM must be set together; got host %q and from %q",
			c.SMTP.Host, c.SMTP.From)
	}
	// Mail is REQUIRED, and this reverses an earlier decision that it was optional.
	// The reason it changed is a fact that did not exist when it was made: a digital
	// download reaches its buyer as a link in the confirmation email, and only the
	// token's hash is stored — so a shop with no mail server does not merely drop a
	// receipt, it takes money for a file the buyer can then never reach, with
	// nothing recoverable afterwards.
	//
	// The old argument — "a shop's job is to take an order and record it, and that
	// does not depend on a mail server" — was right about a shop selling parcels
	// and is wrong about one that can sell downloads. Since any deployment *might*
	// sell one, the requirement is unconditional rather than tied to whether a
	// digital product happens to exist today.
	if !c.SMTP.Configured() {
		return Config{}, fmt.Errorf(
			"config: SMTP_HOST and EMAIL_FROM are required — a store must be able to send " +
				"a receipt, and a digital download's link exists nowhere else")
	}
	if c.OrderNotifyEmail != "" && !c.SMTP.Configured() {
		return Config{}, fmt.Errorf(
			"config: ORDER_NOTIFY_EMAIL is set but SMTP is not, so the notification could never be sent")
	}

	// One image backend at most. Both configured would leave which one wins to be
	// guessed, and the guess would be wrong half the time.
	if c.Blob.Configured() && c.ImageDir != "" {
		return Config{}, fmt.Errorf(
			"config: BLOB_ENDPOINT and IMAGE_DIR are both set; product images come from one or the other")
	}
	// And at least one, which also reverses an earlier decision. A catalog whose
	// products cannot have pictures is not a shop anybody would buy from, and the
	// admin's upload form has to either exist or explain itself on every product
	// page — a half-feature carried by every deployment that forgot a variable.
	//
	// The bar is deliberately low: IMAGE_DIR is one path and needs nothing running.
	// Refusing to boot over a setting that cheap costs an adopter a line of config
	// and saves them a storefront that looks broken.
	if !c.ImagesEnabled() {
		return Config{}, fmt.Errorf(
			"config: product images are required — set IMAGE_DIR for a local directory, " +
				"or the BLOB_* variables for object storage")
	}

	// Object storage is all-or-nothing: a partial configuration would fail at the
	// first upload with whichever piece is missing, which is a worse place to find
	// out than at boot.
	if c.Blob.Configured() {
		var missing []string
		for _, f := range []struct{ name, value string }{
			{"BLOB_BUCKET", c.Blob.Bucket},
			{"BLOB_ACCESS_KEY_ID", c.Blob.AccessKey},
			{"BLOB_SECRET_ACCESS_KEY", c.Blob.SecretKey},
			{"BLOB_PUBLIC_BASE_URL", c.Blob.PublicBaseURL},
		} {
			if strings.TrimSpace(f.value) == "" {
				missing = append(missing, f.name)
			}
		}
		if len(missing) > 0 {
			return Config{}, fmt.Errorf(
				"config: BLOB_ENDPOINT is set, so these are required too: %s", strings.Join(missing, ", "))
		}
		u, err := url.Parse(c.Blob.PublicBaseURL)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return Config{}, fmt.Errorf(
				"config: BLOB_PUBLIC_BASE_URL %q must be an absolute URL", c.Blob.PublicBaseURL)
		}
	} else if c.Blob.Bucket != "" || c.Blob.AccessKey != "" || c.Blob.PublicBaseURL != "" {
		return Config{}, fmt.Errorf(
			"config: BLOB_* variables are set but BLOB_ENDPOINT is not, so uploads would be off")
	}

	if err := c.checkDownloads(); err != nil {
		return Config{}, err
	}

	// "any" is spelled out rather than being an empty list, so disabling a
	// security check is something an operator typed on purpose.
	switch cidrs := strings.TrimSpace(os.Getenv("PAYFAST_ALLOWED_CIDRS")); cidrs {
	case "":
	case "any":
		c.PayFast.AllowAnySourceIP = true
	default:
		for _, cidr := range strings.Split(cidrs, ",") {
			if cidr = strings.TrimSpace(cidr); cidr != "" {
				c.PayFast.AllowedCIDRs = append(c.PayFast.AllowedCIDRs, cidr)
			}
		}
	}

	if c.PayFast.NotifyURL != "" {
		u, err := url.Parse(c.PayFast.NotifyURL)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return Config{}, fmt.Errorf("config: PAYFAST_NOTIFY_URL %q must be an absolute URL", c.PayFast.NotifyURL)
		}
	}

	for _, d := range []struct{ key, path string }{
		{"TEMPLATE_DIR", c.TemplateDir},
		{"STATIC_DIR", c.StaticDir},
	} {
		if d.path == "" {
			continue
		}
		if fi, err := os.Stat(d.path); err != nil {
			return Config{}, fmt.Errorf("config: %s %q: %w", d.key, d.path, err)
		} else if !fi.IsDir() {
			return Config{}, fmt.Errorf("config: %s %q is not a directory", d.key, d.path)
		}
	}

	return c, nil
}

// parseOrigins reads a comma-separated origin list from the environment.
//
// Both lists it serves — embed origins and font origins — are compared literally
// by the browser, against the Origin header in one case and as a CSP source
// expression in the other. Neither tolerates a trailing slash or a path, so both
// are refused here where the message can say why, rather than in a browser where
// the symptom is a request that simply does not happen.
func parseOrigins(key string, allowWildcard bool) ([]string, error) {
	var out []string
	for _, origin := range strings.Split(os.Getenv(key), ",") {
		origin = strings.TrimSpace(origin)
		if origin == "" {
			continue
		}
		if origin == "*" {
			if !allowWildcard {
				return nil, fmt.Errorf("config: %s does not accept \"*\" — list the origins", key)
			}
			out = append(out, origin)
			continue
		}
		u, err := url.Parse(origin)
		if err != nil || u.Scheme == "" || u.Host == "" || u.Path != "" {
			return nil, fmt.Errorf("config: %s entry %q must be scheme://host[:port] with no path", key, origin)
		}
		out = append(out, origin)
	}
	return out, nil
}

// LoadTool loads what a database-only operation needs, which is just
// DATABASE_URL. cmd/seed and the server's own -migrate and -migrate-status
// modes serve no HTTP and hold no session, so requiring the admin and payment
// secrets before they will touch the schema would be an obstacle with nothing
// behind it — and worse, it would mean handing a deploy pipeline's migration
// step the live merchant key to run an ALTER TABLE.
//
// The tradeoff is that a deployment whose payment or session config is broken
// no longer discovers it when migrations run. Run the binary with -check-config
// for that, which is the same check made deliberate.
func LoadTool() (Config, error) {
	c := Config{
		DatabaseURL: os.Getenv("DATABASE_URL"),
		LogLevel:    env("LOG_LEVEL", "info"),
		LogFormat:   env("LOG_FORMAT", "json"),

		// Download storage, because a seed fixture can attach files to a digital
		// product and has to put the bytes somewhere. Everything else the server
		// requires stays out: needing an admin password hash and a merchant key to
		// load a JSON file is an obstacle with nothing behind it.
		//
		// Still optional here, and unset is not an error — a fixture with no files
		// needs none of it. cmd/seed refuses only when a fixture asks for something
		// this cannot deliver.
		DownloadDir:      strings.TrimSpace(os.Getenv("DOWNLOAD_DIR")),
		DownloadMaxBytes: bytesEnv("DOWNLOAD_MAX_BYTES", blob.DefaultMaxDownloadBytes),
		Downloads: Downloads{
			Endpoint:       strings.TrimSpace(os.Getenv("DOWNLOAD_ENDPOINT")),
			Bucket:         strings.TrimSpace(os.Getenv("DOWNLOAD_BUCKET")),
			AccessKey:      os.Getenv("DOWNLOAD_ACCESS_KEY_ID"),
			SecretKey:      os.Getenv("DOWNLOAD_SECRET_ACCESS_KEY"),
			Region:         env("DOWNLOAD_REGION", "auto"),
			UseTLS:         boolEnv("DOWNLOAD_USE_TLS", true),
			PublicEndpoint: strings.TrimSpace(os.Getenv("DOWNLOAD_PUBLIC_ENDPOINT")),
		},
	}
	if c.DatabaseURL == "" {
		return Config{}, fmt.Errorf("config: required env vars not set: DATABASE_URL")
	}
	// The same all-or-nothing and mutual-exclusion rules the server applies, so a
	// half-configured backend fails here rather than at the first file.
	if err := c.checkDownloads(); err != nil {
		return Config{}, err
	}
	if err := checkLogLevel(c.LogLevel); err != nil {
		return Config{}, err
	}
	if err := checkLogFormat(c.LogFormat); err != nil {
		return Config{}, err
	}
	return c, nil
}

func checkLogFormat(format string) error {
	switch format {
	case "json", "gcp":
		return nil
	default:
		return fmt.Errorf("config: LOG_FORMAT must be json or gcp; got %q", format)
	}
}

func checkLogLevel(level string) error {
	switch level {
	case "debug", "info", "warn", "error":
		return nil
	default:
		return fmt.Errorf("config: LOG_LEVEL must be one of debug, info, warn, error; got %q", level)
	}
}

// boolEnv reads a flag. Only the obvious spellings count as true, and anything
// else is false — a typo turning a safety default off silently is worse than a
// typo being ignored.
func boolEnv(key string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "":
		return def
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// checkDownloads validates the private download store, on the same all-or-nothing
// terms as the image bucket: a partial configuration would fail at the first
// upload with whichever piece is missing, which is a worse place to find out.
//
// The overlap check is the one that matters most and is unique to this store.
func (c Config) checkDownloads() error {
	if c.Downloads.Configured() && c.DownloadDir != "" {
		return fmt.Errorf(
			"config: DOWNLOAD_ENDPOINT and DOWNLOAD_DIR are both set; purchased files come from one or the other")
	}

	// The download directory must not be, or contain, or sit inside, the directory
	// images are served from. /images/ is public and unauthenticated, so an overlap
	// would publish every purchased file to anybody who guessed a key — the exact
	// failure this whole feature exists to prevent, arrived at by a configuration
	// mistake rather than a bug. Refusing at boot is the only place to catch it.
	if c.DownloadDir != "" && c.ImageDir != "" {
		dl, err1 := filepath.Abs(c.DownloadDir)
		img, err2 := filepath.Abs(c.ImageDir)
		if err1 == nil && err2 == nil && overlaps(dl, img) {
			return fmt.Errorf(
				"config: DOWNLOAD_DIR %q overlaps IMAGE_DIR %q; everything under IMAGE_DIR is served publicly at /images/, so purchased files must not live there",
				dl, img)
		}
	}

	if c.Downloads.Configured() {
		var missing []string
		for _, f := range []struct{ name, value string }{
			{"DOWNLOAD_BUCKET", c.Downloads.Bucket},
			{"DOWNLOAD_ACCESS_KEY_ID", c.Downloads.AccessKey},
			{"DOWNLOAD_SECRET_ACCESS_KEY", c.Downloads.SecretKey},
		} {
			if strings.TrimSpace(f.value) == "" {
				missing = append(missing, f.name)
			}
		}
		if len(missing) > 0 {
			return fmt.Errorf(
				"config: DOWNLOAD_ENDPOINT is set, so these are required too: %s", strings.Join(missing, ", "))
		}

		// Same endpoint and same bucket as the images means the download store is
		// the public one. Nothing downstream would notice, and every purchased file
		// would be anonymously fetchable.
		if c.Blob.Configured() &&
			strings.EqualFold(c.Downloads.Endpoint, c.Blob.Endpoint) &&
			strings.EqualFold(c.Downloads.Bucket, c.Blob.Bucket) {
			return fmt.Errorf(
				"config: DOWNLOAD_BUCKET %q is the same bucket as BLOB_BUCKET, which is publicly readable; purchased files need a private bucket of their own",
				c.Downloads.Bucket)
		}
	} else if c.Downloads.Bucket != "" || c.Downloads.AccessKey != "" {
		return fmt.Errorf(
			"config: DOWNLOAD_* variables are set but DOWNLOAD_ENDPOINT is not, so digital products would be off")
	}
	return nil
}

// overlaps reports whether either path is the other, or is inside it.
func overlaps(a, b string) bool {
	if a == b {
		return true
	}
	return strings.HasPrefix(a, b+string(filepath.Separator)) ||
		strings.HasPrefix(b, a+string(filepath.Separator))
}

// bytesEnv reads a byte count, accepting a plain number of bytes or a suffix —
// "2GiB", "500MB", "2G". Written because the alternative is an operator typing
// 2147483648 and getting a digit wrong, and because a size limit expressed in raw
// bytes in a .env file is unreadable to the person who has to change it.
//
// A malformed or non-positive value falls back to the default rather than failing
// the boot: this is a cap on an admin's own upload, and refusing to start a shop
// over it would be out of proportion. Load logs nothing here, so the check that
// catches a typo is the admin form saying a smaller number than expected.
func bytesEnv(key string, def int64) int64 {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}

	mult := int64(1)
	upper := strings.ToUpper(raw)
	for _, s := range []struct {
		suffix string
		factor int64
	}{
		{"KIB", 1 << 10}, {"MIB", 1 << 20}, {"GIB", 1 << 30},
		{"KB", 1000}, {"MB", 1000 * 1000}, {"GB", 1000 * 1000 * 1000},
		{"K", 1 << 10}, {"M", 1 << 20}, {"G", 1 << 30},
		{"B", 1},
	} {
		if strings.HasSuffix(upper, s.suffix) {
			mult = s.factor
			upper = strings.TrimSpace(strings.TrimSuffix(upper, s.suffix))
			break
		}
	}

	n, err := strconv.ParseInt(upper, 10, 64)
	if err != nil || n <= 0 {
		return def
	}
	// Overflow would turn a huge number into a negative cap, which reads as "no
	// limit" to a caller comparing against it.
	if n > math.MaxInt64/mult {
		return def
	}
	return n * mult
}

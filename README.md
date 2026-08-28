# gostore

A small, self-hostable online store written in Go: `html/template` fragments for an
htmx frontend, PostgreSQL for storage, and [PayFast](https://payfast.io) for payments.
Stdlib-first, with a deliberately tiny dependency surface.

> **Status: early.** The skeleton (config, migrations, container stack, health check), the
> catalog (products, variants, seed command, admin CRUD), administrator accounts with roles,
> the storefront, the cart, checkout against PayFast, order emails, the admin's order views,
> product image uploads, the hardening pass, categories, and catalog search with filtering
> and pagination all work. What remains is the publishing checklist — see the build order
> below.
>
> There is no default admin password. `make up` prints a one-time setup token, and
> `/admin/setup` exchanges it for the first administrator account — see
> [Admin](#admin). The compose stack does ship PayFast's **published sandbox
> credentials**, so the checkout works out of the box; replace them before anyone else
> can reach the deployment — see [Payments](#payments).

## Why

Go has no maintained open-source store, and no PayFast integration at all. This aims to
be honest and good at one thing: a small catalog of physical goods — books and apparel —
sold in ZAR, with variants, stock, and a spare admin UI. It is not a general commerce
platform.

## Quickstart

```sh
git clone https://github.com/17xande-dev/gostore
cd gostore
make up
curl localhost:8080/healthz   # -> ok
make seed                     # load the demo catalog
open http://localhost:8080/admin      # sign in with the development password: gostore
```

`make up` starts Postgres, [mailpit](http://localhost:8025) (captures outgoing email),
[MinIO](http://localhost:9001) (S3-compatible object storage) and the server. Migrations
are applied automatically on boot. It also mounts [`theme/`](theme) into the server with
reloading on, so a stylesheet or template dropped in there takes effect on the next page
refresh — see [Theming](#theming).

Other useful targets:

| Target | Does |
|---|---|
| `make down` | Stop the stack (`make down ARGS=-v` also deletes the data volumes) |
| `make run` | Run the server on the host against the compose Postgres |
| `make seed` | Load a products JSON file (`SEED_FILE=...`, default `testdata/products.json`) |
| `make test` | Run every test, including the database-backed ones |
| `make hashpw` | Prompt for a password and print an argon2id hash — a lockout-recovery path, not part of setup |
| `make psql` | Open a `psql` shell on the compose database |
| `make logs` | Follow the server logs |
| `make migrate` | Apply pending migrations without starting the server |
| `make migrate-status` | Show which migrations have been applied |
| `make check-config` | Validate the full server configuration and exit |
| `make sqlc` | Regenerate the stores' query code after editing SQL (`make sqlc-install` first) |

## Configuration

Everything comes from the environment; see [`.env.example`](.env.example) for the full
list with defaults.

| Var | Required | Default | Purpose |
|---|---|---|---|
| `DATABASE_URL` | **yes** | — | Postgres connection string |
| `SETUP_TOKEN` | no | generated | The one-time token that claims the first account. 32+ characters. Generated and logged on first boot if unset |
| `SESSION_TTL_HOURS` | no | `24` | How long a sign-in lasts |
| `PAYFAST_MERCHANT_ID` | **yes** | — | From the PayFast dashboard |
| `PAYFAST_MERCHANT_KEY` | **yes** | — | From the PayFast dashboard |
| `PAYFAST_PASSPHRASE` | no | — | The account's salt passphrase; must match the dashboard exactly |
| `PAYFAST_SANDBOX` | no | `true` | `false` takes real money. Set it explicitly when deploying — see [Going live](#going-live) |
| `PAYFAST_NOTIFY_URL` | no | derived | Override when PayFast cannot reach `BASE_URL` (a tunnel) |
| `PAYFAST_ALLOWED_CIDRS` | no | published ranges | Override the source ranges; `any` disables the check |
| `TRUST_PROXY_IP` | no | `false` | Believe `X-Forwarded-For`; only with a proxy that replaces it |
| `PORT` | no | `8080` | Listen port |
| `BASE_URL` | no | `http://localhost:8080` | Public origin, for absolute URLs |
| `STORE_NAME` | no | `gostore` | Displayed store name |
| `CURRENCY` | no | `ZAR` | Currency code (PayFast requires `ZAR`) |
| `EMBED_ORIGINS` | no | — | Origins allowed to fetch and frame the catalog fragments |
| `FONT_ORIGINS` | no | — | Origins a web font may be loaded from. Widens the CSP's `font-src` **and** `style-src`. See [Web fonts](#web-fonts) |
| `FONT_CSS_URL` | no | — | A hosted font service's stylesheet, linked from the default layout. Its origin must be in `FONT_ORIGINS` |
| `TEMPLATE_DIR` | no | — | Directory of templates that override the embedded defaults |
| `STATIC_DIR` | no | — | Directory of assets that override the bundled ones (logo, placeholder, CSS) |
| `THEME_RELOAD` | no | `false` | Re-read those two directories on every request, so a theme edit needs a refresh and not a restart. Development only |
| `LOG_LEVEL` | no | `info` | `debug`, `info`, `warn` or `error` |
| `LOG_FORMAT` | no | `json` | `gcp` renames `level`/`msg` to `severity`/`message` for Cloud Logging. See [Logging](#logging) |
| `SHUTDOWN_TIMEOUT_SECONDS` | no | `15` | Grace period for in-flight requests |
| `SMTP_HOST` | **yes**¹ | — | Mail relay |
| `EMAIL_FROM` | **yes**¹ | — | Sender address |
| `SMTP_PORT` | no | `587` | `465` with `SMTP_TLS=tls`, `1025` for mailpit |
| `SMTP_TLS` | no | `starttls` | `starttls`, `tls` (implicit) or `none` (development only) |
| `SMTP_USERNAME` / `SMTP_PASSWORD` | no | — | Omit both for a relay that authenticates by address |
| `SMTP_OAUTH_TENANT_ID` / `SMTP_OAUTH_CLIENT_ID` / `SMTP_OAUTH_CLIENT_SECRET` | no⁵ | — | Authenticate with XOAUTH2 instead of a password. See [Microsoft Exchange Online](#microsoft-exchange-online) |
| `EMAIL_REPLY_TO` | no | — | When replies should not go to `EMAIL_FROM` |
| `ORDER_NOTIFY_EMAIL` | no | — | Sends a copy of each paid order to whoever packs it |
| `IMAGE_DIR` | **yes**³ | — | Store images in this directory, served by this server |
| `BLOB_ENDPOINT` | **yes**³ | — | Object storage host[:port], no scheme |
| `BLOB_BUCKET` | no² | — | Bucket name |
| `BLOB_ACCESS_KEY_ID` / `BLOB_SECRET_ACCESS_KEY` | no² | — | Credentials |
| `BLOB_PUBLIC_BASE_URL` | no² | — | Where images are **read** from — not where they are written |
| `BLOB_REGION` | no | `auto` | What R2 wants; GCS and MinIO ignore it |
| `BLOB_USE_TLS` | no | `true` | `false` only for a MinIO on the same machine |
| `DOWNLOAD_DIR` | no⁴ | — | Store purchased files in this directory — **never** served publicly |
| `DOWNLOAD_ENDPOINT` | no⁴ | — | Private bucket host[:port], no scheme |
| `DOWNLOAD_BUCKET` | no⁴ | — | Bucket name; must not be `BLOB_BUCKET` |
| `DOWNLOAD_ACCESS_KEY_ID` / `DOWNLOAD_SECRET_ACCESS_KEY` | no⁴ | — | Credentials |
| `DOWNLOAD_PUBLIC_ENDPOINT` | no | — | The address a *browser* reaches the bucket at, when it differs from `DOWNLOAD_ENDPOINT`. Development stacks only |
| `DOWNLOAD_REGION` | no | `auto` | As `BLOB_REGION` |
| `DOWNLOAD_USE_TLS` | no | `true` | As `BLOB_USE_TLS` |
| `DOWNLOAD_MAX_BYTES` | no | `2GiB` | Cap on one uploaded file; accepts `500MB`, `2G`, or plain bytes |
| `RATE_LIMIT_LOGIN_PER_MINUTE` | no | `10` | Per client IP; `0` disables |
| `RATE_LIMIT_CHECKOUT_PER_MINUTE` | no | `20` | Per client IP; `0` disables |
| `RATE_LIMIT_CALLBACK_PER_MINUTE` | no | `120` | Per client IP; `0` disables |
| `RATE_LIMIT_DOWNLOAD_PER_MINUTE` | no | `60` | Download links per IP; each click mints a signed URL |
| `CART_TTL_DAYS` | no | `60` | How long an untouched cart survives |

¹ **Mail is required**, and both must be set. This reverses an earlier position that a store
with no mail server should still boot and drop receipts loudly. What changed is a fact rather
than an opinion: a digital download's link lives in the confirmation email and **only its hash
is stored**, so an unconfigured relay does not lose a receipt — it takes money for a file the
buyer can then never reach, unrecoverably. The old argument was right about a shop selling
parcels and wrong about one that *can* sell downloads, and any deployment might.

² The `BLOB_*` set is all-or-nothing for the same reason: `BLOB_ENDPOINT` with any of the
others missing refuses to boot rather than failing at the first upload.

³ **One image backend is required**, and the two are mutually exclusive — both set refuses to
boot because which one wins would otherwise be a guess, and neither set refuses because a
catalog whose products cannot have pictures is not a shop anybody buys from. The bar is
deliberately low: `IMAGE_DIR` is one path and needs nothing running.

⁴ The `DOWNLOAD_*` set is the **private** store for purchased files, and is separate from the
image one in every respect. `DOWNLOAD_DIR` and `DOWNLOAD_ENDPOINT` are mutually exclusive;
the bucket set is all-or-nothing; with none of it, the shop sells no digital products and
the admin says so. Two overlaps refuse to boot, and both would otherwise publish files
somebody paid for: a `DOWNLOAD_DIR` that is, contains, or sits inside `IMAGE_DIR` — which is
served publicly at `/images/` — and a `DOWNLOAD_BUCKET` equal to `BLOB_BUCKET`, which is
anonymously readable.

⁵ **All three or none.** A half-configured app registration refuses to boot, because the
alternative is a store that starts, takes an order, and only then finds it cannot
authenticate — with the buyer's download link in the message it failed to send. Setting them
alongside `SMTP_PASSWORD` also refuses: which one authenticates would be a guess, and the
loser would sit in the environment looking live. `SMTP_USERNAME` becomes required, because
XOAUTH2 authenticates as a named mailbox.

## Admin

The admin lives at `/admin`. Accounts are rows in `admin_users`, not a credential in the
environment: there is no `ADMIN_PASSWORD_HASH` and no default password to forget to change.

**First run.** With no administrator in the database the server generates a one-time setup
token and prints it:

```sh
docker compose logs server | grep setup_token
```

`/admin/setup` exchanges that token for the first account, which is an `owner`, and
`/admin/login` redirects there while no account exists. The token is spent by the claim and
the page stops existing — permanently: the consumed timestamp is never cleared, so a restart
does not reopen it and neither does disabling every account. A deploy with nobody reading
logs can supply `SETUP_TOKEN` instead, in which case nothing is printed; the Terraform stack
generates one into Secret Manager and reads it with
`gcloud secrets versions access latest --secret=gostore-setup-token`.

The alternatives are both worse, and both common. A fixed default credential is a CVE class,
especially in a project published for others to copy. An unguarded first-run wizard is a race
that whoever finds the deployment first — before its operator does — wins.

**Sessions are rows, not signed cookies.** A sign-in inserts into `admin_sessions` and the
cookie carries 32 bytes from `crypto/rand`. Only `sha256(token)` is stored, so a leaked
backup hands over no live session, and there is no signing secret to configure or rotate.

The row is what makes a session revocable, which is the whole reason for the table: changing
an account's password, disabling it, or changing its role deletes every session it holds in
the same transaction, so the next request from that browser is anonymous rather than valid
until the cookie happens to expire. Expiry is enforced in the lookup's own SQL predicate; the
hourly sweep of expired rows keeps the table bounded and is explicitly not what makes it
correct.

The costs, stated plainly: one indexed lookup per admin request, and a session that outlives
a `DELETE FROM admin_sessions` does not exist. Both are the price of being able to end one.

If nobody can sign in at all — every owner disabled, or the only password lost — `make hashpw`
prints a hash to set by hand. See [`cmd/hashpw`](cmd/hashpw/main.go) for the `UPDATE`, and for
the `DELETE FROM admin_sessions` that has to go with it.

Notes:

- The cookie is `HttpOnly`, `SameSite=Lax`, scoped to `/admin`, and `Secure` whenever
  `BASE_URL` is `https://` — so it is never sent with the cookie-free embeddable catalog
  fragments.
- Signing in replaces whatever session the browser arrived with, rather than adopting it.
  That is session fixation: an attacker who can set a cookie plants a token and waits.
- A redirect back to where you were going (`?next=`) is reduced by an allowlist — a path
  under `/admin/`, no `//`, no CR/LF, no `..` — so it cannot send anybody off the site.
- A session lookup that *fails* is a `500`, not a redirect to the login form. Answering
  "please sign in" during a database outage sends an operator round a loop that cannot
  complete, with the outage reported as an authentication problem.
- htmx requests that have lost their session get `401` and `HX-Refresh: true` instead of a
  redirect, because swapping a login page into a fragment produces a broken hybrid.
- Login, the setup claim and the change-your-own-password form share one rate limit of 10
  attempts a minute per IP; see [Hardening](#hardening). All three verify a secret, and each
  one spends 64 MiB on argon2 doing it.
- What each account may actually *do* is [Roles](#roles), below.

## Roles

Four roles, following Stripe's dashboard split reduced to the surface this store has. Read
access to the catalog and the orders comes with having a session at all, so the table is
really about who may write:

| Role | Catalog | Orders & entitlements | Accounts | |
|---|---|---|---|---|
| `owner` | write | write | write | Cannot be disabled or demoted while it is the last enabled owner |
| `admin` | write | write | write | |
| `manager` | write | write | — | The shop-runner role |
| `viewer` | read | read | — | Looks, changes nothing |

`owner` and `admin` are identical in capability. `owner` exists to be the account the
last-account guard protects, which makes "who can never be locked out" a visible fact about
a row rather than something that emerges from counting.

**Permissions are a static map in Go** — `auth.Role` to a set of `auth.Permission` — not a
table. A permissions table would put a join on every request to buy configurability nobody
asked for, and would let a deployment invent a role the code has never heard of.

**Every route names the permission it needs on the line that registers it**, so
authorization travels with the route the way authentication already does:

```go
admin("GET  /admin/products", auth.PermRead,         h.adminProductList)
admin("POST /admin/products", auth.PermCatalogWrite, h.adminProductCreate)
admin("POST /admin/users",    auth.PermUsersWrite,   h.adminUserCreate)
```

The registration records the pair, so `AdminProtectedRoutes()` is what the tests sweep
rather than a list maintained beside the real one. Templates ask `.Can "catalog.write"` and
leave out what a role could not use — a button that is merely absent is not a restriction on
anybody who types the address, so that is presentation and `requirePerm` is the enforcement.

### Managing accounts

`/admin/users` is `users.write` only. What it will not do is as much of the design as what
it will:

- **Accounts are disabled, never deleted.** A removed row erases who did what, and the
  products and orders an administrator touched outlive their employment. Disabling ends
  their sessions in the same transaction.
- **Nobody may change their own role, disable themselves, or reset their own password from
  these pages.** A reset skips the current-password check, so allowing it against yourself
  would make an unattended screen enough to take an account over for good; an administrator
  who can change their own role is not held by it. All three answer `409`, and the controls
  are absent rather than present-and-refusing.
- **The last owner who can still sign in cannot be disabled or demoted**, from either
  direction — both guards take the same advisory lock, because otherwise two administrators
  removing two different owners each pass a count nobody re-reads.
- **A password somebody else chose is temporary.** Creating an account, or resetting its
  password, sets `must_change_password`; every route except the change form itself then
  bounces there until a new one is chosen.
- **Changing your own password asks for the current one.** A CSRF token proves the request
  came from our form, not that the person at the keyboard owns the account. It ends every
  session including the one doing it, and lands on the login form.

### A first run, end to end

```sh
make up
docker compose logs server | grep setup_token
```

1. Open `/admin` — with no account it redirects to `/admin/setup`.
2. Paste the token, choose an address and a password: that account is the `owner`.
3. `/admin/users/new` creates the rest. Give each one the least role that covers their job;
   `manager` is the usual answer for somebody running the shop.
4. Hand over the starting password however you like. They will be asked to replace it before
   any other admin page opens.
5. When somebody leaves, disable them. Their sessions stop on their next request, and the
   record of what they did stays.

## CSRF

Every state-changing request — admin or cart — needs a
[nosurf](https://github.com/justinas/nosurf) token, submitted as a `csrf_token` form field
(the `{{template "csrf" .CSRFToken}}` partial) or an `X-CSRF-Token` header. A request without
one gets `403`.

CSRF is mounted over groups of routes rather than the whole server, for a reason worth
knowing before adding any: nosurf sets a token cookie on every response it handles, and a
catalog fetched from another origin must stay cookie-free. So `/admin`, `/cart` and
*first-party* catalog pages go through it — the last of those because the product page
carries an add-to-cart form — while the same catalog pages fetched cross-origin do not.
Grouping also means the payment callback, which cannot carry a token and must never require
one, will be exempt by not being in a group at all, rather than by an exempt-path string that
has to keep matching the route.

The trap this design walks into, if you extend it: **a page rendered outside the CSRF layer
gets an empty `.CSRFToken`**, and every form on it is then refused with a `403`. Any new page
carrying a form has to be inside one of these groups.

nosurf also requires that an unsafe request identify its origin, via `Sec-Fetch-Site`,
`Origin` or `Referer`. Browsers always send at least one; **`curl` does not**, so manual
testing needs `-H "Origin: http://localhost:8080"` or the request is rejected before the
token is even examined.

The origin nosurf compares against is built from the request's `Host` and a scheme it
assumes is `https` unless told otherwise, so the scheme is taken from **`BASE_URL`** rather
than from the connection: behind a TLS-terminating proxy the connection is plain HTTP while
the browser's origin is `https`. Getting `BASE_URL` wrong therefore breaks every admin form
with a `403`, not just absolute links.

## Hardening

### Rate limits

Per client IP, on three surfaces, with a token bucket from
[`golang.org/x/time/rate`](https://pkg.go.dev/golang.org/x/time/rate) and the keying and
eviction written here — the algorithm is the part with the clock edge cases already found
in it, and a bucket per client with bounded memory is where the decisions are.

| Route | Default | Why |
|---|---|---|
| `POST /admin/login` | 10/min | Brute force. argon2id's cost makes each attempt expensive, but cost is not a limit |
| `POST /cart/checkout` | 20/min | Order-row spam, loose enough that double-clicking never trips it |
| `POST /payments/{gw}/callback` | 120/min | **The reason the limiter exists**: unauthenticated, and every accepted request makes the store POST to the gateway — an amplifier |

The burst is a third of the allowance (minimum 2), so `10/min` means three attempts
immediately and then one every six seconds. A refusal is `429` with `Retry-After`. Limits
are applied on the line that registers each route, not wrapped around a prefix, for the
same reason `RequireAdmin` is: a prefix wrapper is one refactor away from silently not
covering a new route.

**The callback's `429` is not a contradiction of the always-`200` rule.** `200` means
*read and decided*, so a gateway does not retry a forgery. A throttled request has not
been read, and a retry is exactly what should happen — hence the limiter sits in front of
the handler and answers `429`, which PayFast reads and honours.

Only the POST on `/admin/login` is limited. Limiting the GET would lock an operator out of
the page carrying the message explaining why.

Idle buckets are evicted on a lazy sweep during an ordinary request, so there is no
goroutine to own and shut down for a map that is usually tiny, and the map cannot grow
without bound as an attacker cycles addresses.

**Catalog search is deliberately not limited**, and it is worth being explicit since it is the
one read that costs more than a primary-key lookup: every surface above is a `POST`, and
`GET /products` is none of them. A search is a bounded index scan over a small table, it holds
no lock and writes nothing, and the page it returns is cacheable — so the limiter would mostly
be throttling a crawler doing something harmless. That reasoning depends on the catalog staying
small; a store whose search starts showing up in slow-query logs should give `/products` its
own bucket, which is one line where the route is registered.

### Password hashing

argon2id, via `x/crypto/argon2` — no new dependency, since `x/crypto` was already here for
bcrypt. Parameters are RFC 9106's second recommendation: 64 MiB, three passes, four lanes,
encoded into the hash as a standard PHC string:

```
$argon2id$v=19$m=65536,t=3,p=4$<salt>$<hash>
```

Because the parameters live in the hash, raising them later needs no migration: existing
hashes keep verifying at their own settings, and the next password set through the admin —
or through `make hashpw`, which is now only the lockout-recovery path — is written with the
new ones.

**A bcrypt hash still verifies.** `CheckPassword` dispatches on the prefix, so a hash
carried over from an older deployment keeps working and moves to argon2id the next time that
password is set. New hashes are argon2id only.

Two details that are defence rather than decoration:

- Verification **caps the memory a stored hash may request** at 1 GiB. Without that, a
  hand-edited hash claiming `m=4194304` would try to allocate four gibibytes on the first
  login attempt — a denial of service delivered by a typo.
- A hash is **parsed on the way in**, by every store method that takes one, so a malformed
  one is refused where the caller can say so rather than becoming an account that can never
  sign in with nothing in the logs to explain it.

### Response headers

```
Content-Security-Policy: default-src 'self'; img-src 'self' <bucket>;
  font-src 'self' <font origins>; style-src 'self' <font origins>; script-src 'self';
  form-action 'self' <gateway>; base-uri 'none'; object-src 'none';
  frame-ancestors <embed origins or 'none'>
Permissions-Policy: geolocation=(), camera=(), microphone=(), payment=()
Referrer-Policy: strict-origin-when-cross-origin
X-Content-Type-Options: nosniff
Strict-Transport-Security: max-age=63072000; includeSubDomains   (https deployments only)
```

**Digital downloads add no directive**, which is worth stating because the download bucket
looks like something that ought to be named here. A buyer's download is a top-level
navigation to a signed URL, and no fetch directive governs one — `connect-src` would only
come into it if the browser uploaded straight to the bucket, which is exactly the design
that was declined.

`img-src` is `'self'` plus the bucket and nothing else, because a product image is always
bytes this store holds. The angle-bracketed placeholders are the only external origins any
directive gets, each one named by a deployment: the bucket, the payment gateway, the embedders,
and a font service. There is no `'unsafe-inline'` anywhere — which is worth knowing before
writing a theme:

- **`font-src` and `style-src`** — both `'self'` unless `FONT_ORIGINS` is set, which is the
  only knob that opens two directives at once. A hosted font is two fetches: the stylesheet
  declaring the fonts, then the files it names. See [Web fonts](#web-fonts).
- **`style-src 'self'`** — it used to carry `'unsafe-inline'`, because restyling through
  `TEMPLATE_DIR` had no other legal way to apply CSS: there was no stylesheet and no way for
  an adopter to add one. Both exist now — a bundled `styles.css` and `STATIC_DIR` to replace
  it — so the concession is gone. **An overriding template cannot use `style` attributes or
  `<style>` blocks**; put the CSS in a stylesheet in `STATIC_DIR`. (Email bodies still use
  inline styles, because no CSP has ever applied to them.)
- **`script-src 'self'`** — same rule for scripts. A theme's JavaScript is a `.js` file in
  `STATIC_DIR`, referenced with `{{asset "yours.js"}}`; an inline `<script>` is simply not
  run.

HSTS is sent only when `BASE_URL` is `https://`: browsers ignore it over plain HTTP, and
sending it from a development server would pin a rule making the next plain-HTTP project
on that port unreachable. There is deliberately no `preload` directive — that list has a
slow exit, and it is the operator's decision rather than this project's.

### Overselling

Stock is taken at payment, never reserved at checkout, so two shoppers can both reach a
payment page for the last item and both pay. The second order is still recorded **paid** —
the money was taken, and refusing to record it would lose the sale *and* still be oversold
— and `orders.oversold` is set in the same transaction, so an order is never
paid-but-unflagged.

It shows as `OVERSOLD` in `/admin/orders` and as a prominent block on the order page, as
well as in the owner's notification email. Before this phase it existed only in the logs
and that email, which is the wrong place for something needing reconciliation: an email is
read once and a log is not read at all.

Nothing here refunds anything. Refunds happen in the gateway's dashboard, because this
schema models a forward payment only — see the plan's note on when to reconsider
off-the-shelf.

### Cart cleanup

Carts untouched for `CART_TTL_DAYS` are deleted on boot and then daily, by a goroutine in
the server process. It runs in every instance rather than being elected to one, and that is
fine because the work is a single idempotent `DELETE`: two instances produce the same end
state as one, and the second finds nothing to do. Electing a leader would need coordination
this store has no other use for, and a cron container would break the one-binary
deployment story.

A failed sweep is logged and retried at the next tick. A cleanup that fails is a table that
grows a little longer, which is not worth waking anyone for.

## Catalog

A **product** is a catalog entry; a **variant** is what a customer actually buys, and
carries the price and the stock count. A product with no options — a single-edition book —
still has exactly one variant, with every option left blank. That is deliberate: cart,
order and stock code then never branches on "has options versus not".

A product is either **physical** or a **digital download**, set by its *kind*. A download
has no stock, needs no delivery address, and is delivered by a per-buyer link rather than a
parcel — see [Digital downloads](#digital-downloads).

Prices are **integer cents** everywhere in the code and the database. The decimal point
exists only in forms and rendered pages, because a float total rounded differently from a
payment gateway's amount string is a real and hard-to-find class of bug.

Manage the catalog at `/admin/products`.

### Variant options

A variant is told apart from its siblings by up to **three options, named per product**. A
t-shirt declares `Size` and `Colour`; a book declares `Cover`; a conference recording
declares `Format`. The names live on the product and the values on each variant, so the
storefront's selector is headed in the shop's own words rather than in ours.

Names fill in order, and two slots may not share a name. Both are refused on the form
rather than by a constraint, so the message lands on the field.

**The names are not attached to the category, deliberately.** Categories here are
many-to-many precisely so that a book can also be a gift — so option structure hanging off
them would give that product two contradictory answers, and adding a "Sale" category for a
promotion would change what a variant *is*. Shopify and WooCommerce declare options per
product exactly like this; Magento, Saleor and Sylius add a reusable template on top and
every one of them keeps that template separate from the browsing taxonomy. A template layer
here would be strictly additive later, because the values live in these columns either way.

### Categories

**A category is a row, not a string on the product.** Two tables: `categories`, and a
`product_categories` join.

| Column | Is |
|---|---|
| `slug` | The public parameter — `/products?category=books`. Unique |
| `name` | What a shopper reads |
| `position` | The display order. Sorting by `name` would put "Apparel" ahead of "Books" for ever, and a shop owner wants their own order |

**A product may be in several categories**, which is why there is a join table rather than a
column. A book that is also a gift belongs in both, and making a shop owner choose is a
decision the store has no business making for them. The cost is one extra query wherever
categories are read — paid in the admin, and deliberately not on the storefront cards, which
do not show them.

**Deleting a category unlinks its products; it never deletes them.** The cascade is on the
join table alone. This is the same stance as refusing to delete a product an order references:
a taxonomy edit must not be able to remove things people bought. The cost is that deleting an
unused category looks like it did nothing, so the admin says how many links it removed.

Manage them at `/admin/categories`.

### Product images

**A product image is always bytes this store holds.** There is no way to point a product at
a URL on somebody else's server: those bytes can change or vanish without warning, and a
product page with a broken picture is worse than one with none. The admin has no URL field,
and hand-crafting the parameter does nothing — `UpdateProduct` does not write either image
column.

Two backends, mutually exclusive, both behind `blob.Storage`:

| | Set | Image URL | Suits |
|---|---|---|---|
| **Object storage** | `BLOB_*` | the bucket's public hostname | anything scaled out; R2, GCS interop, MinIO |
| **A local directory** | `IMAGE_DIR` | `/images/...`, served by this server | one instance with a persistent volume |
| **Neither** | — | products have no images, and the admin says so | a catalog that does not need pictures |

`IMAGE_DIR` is the simpler shape: one binary, one directory, no object storage to run. Its
limitation is worth stating plainly because it is the thing that will bite — **two instances
do not share a directory.** Behind a load balancer, or on a platform that scales to zero and
restarts elsewhere, an image uploaded by one instance is a 404 from the other. Use a bucket
there.

With a bucket, **images are served straight from it and never proxied through the store**,
so the bucket must be publicly readable at `BLOB_PUBLIC_BASE_URL` and a CDN in front of it
does the work. With `IMAGE_DIR` the server serves them itself, from a same-origin path — which
is why `img-src` can be `'self'` with no external origin allowed at all.

**`products.image_key` is the only thing stored** — the URL is computed when a page is
rendered, by resolving the key against whichever backend is running. That is what makes the
same row work on a development machine serving from a directory and in production serving
from R2: **switching backends needs no data migration.** Storing the URL as well would bake
one deployment's answer into every row.

A product with no image gets a bundled placeholder rather than a gap, so a catalog of mixed
products keeps its shape while photographs are still being taken.

Two consequences worth knowing:

- **Uploads are validated on their sniffed magic bytes**, not the filename and not the
  browser's `Content-Type`, and the stored extension comes from the sniffed type too. A
  publicly readable bucket that will serve `evil.html` because somebody named their upload
  that is a cross-site scripting hole on a hostname you own — and the same is true of a
  directory this server serves. JPEG, PNG, GIF and WebP; 5 MB.
- **Replacing an image writes a new key**, so the new photograph is visible immediately.
  A stable key would need a CDN purge on every replacement — an operation this store has
  no credentials for and no way to verify — and until it happened the old picture would
  keep being served.

With a bucket, `BLOB_ENDPOINT` and `BLOB_PUBLIC_BASE_URL` are separate values because the
address a bucket is *written* through and the address it is *read* from are routinely
different:
R2 writes to `<account>.r2.cloudflarestorage.com` and reads from a custom domain, and in
the compose stack the server writes to `minio:9000` while your browser reads from
`localhost:9000`. Only the operator knows the second one, so it is not derived.

**Why minio-go and not the AWS SDK.** Since `aws-sdk-go-v2/service/s3` v1.73.0 every
`PutObject` carries a CRC32 checksum by default, which
[broke R2, GCS interop and older MinIO](https://github.com/aws/aws-sdk-go-v2/discussions/2960) —
the three stores this targets — while being correct for the one it is least likely to be
pointed at. minio-go speaks the conservative subset all of them agree on.

The upload order is chosen so no failure leaves a product pointing at nothing: store the
new object, point the product at it, and only then delete the old one. A failure part-way
leaves the previous image working, or at worst an orphaned object that costs a few
kilobytes and is logged.

**How images load** is the browser's own lazy loading and nothing else — no
`IntersectionObserver`, no JavaScript, no CSP directive involved. The catalog grid marks
everything after the first row `loading="lazy"`; the first four cards are left eager, because
lazy-loading an image that is already on screen defers exactly the picture that decides how
fast the page *feels* loaded. Four is a deliberate guess: the grid asks for as many columns as
fit, so the real count is a CSS decision the server cannot see, and four over-fetches slightly
on a phone and under-fetches on a wide monitor. A product page's own photograph is eager and
`fetchpriority="high"`, being the one image that page is about.

There are **no `width` and `height` attributes**, and that is not an oversight. The fixed-ratio
frame (`aspect-ratio: 4 / 5`) already reserves the space before any bytes arrive, so there is
no layout shift left to prevent — and the store never records a photograph's real dimensions,
so any numbers put there would be a guess about an image that is going to be cropped to the
frame anyway.

### Digital downloads

A product whose kind is **digital** is delivered as files rather than as a parcel. It has no
stock, needs no delivery address, and each buyer gets a link that can be withdrawn without
touching anybody else's.

**Files belong to the product; variants say which files they grant.** A conference recording
sold as an audio set and a video set is one product, two variants, and a tick list saying
which files each includes — so an "Audio + Video" bundle costs a row rather than a second
upload of the same two gigabytes.

**Private storage, always.** `DOWNLOAD_DIR` or the `DOWNLOAD_*` bucket, configured separately
from images and never the same place. The image bucket is anonymously readable by design —
that is what lets a CDN serve product photographs — so a purchased file in it would be one URL
guess away from everybody, and public access on GCS and R2 is a whole-bucket toggle rather
than something a prefix can carry. The server **refuses to boot** if the download directory
overlaps `IMAGE_DIR`, or if the download bucket is the image bucket.

#### How a buyer gets their file

```
paid order
  └─ entitlement per digital line, with a 32-byte token
       └─ emailed as  {BASE_URL}/downloads/{token}

GET /downloads/{token}            the files this entitlement grants
GET /downloads/{token}/{fileID}   check it is not revoked, check the file
                                  belongs to this variant, record the download,
                                  then 302 to a signed URL that expires in five
                                  minutes — or stream, on the disk backend
```

The token in the URL is the whole credential: there is no account and no login. **Only its
SHA-256 hash is stored**, so a dump of the entitlements table is a list of hashes rather than
a set of working links — and the consequence, stated plainly, is that a confirmation email
that never arrives cannot be recovered from. Issue a fresh entitlement in that case.

The link never points at the bucket. Authorising and recording happen before any bytes move,
which is what makes revocation take effect on the next click and makes the counts trustworthy.
The signed URL is minted per click, so one forwarded to a friend is already expired.

A presigned URL's signature covers the `Host` header, so it must be signed for the address the
*browser* will use rather than the one the server connects through. Those are the same
everywhere except a container stack, which is what `DOWNLOAD_PUBLIC_ENDPOINT` is for — compose
sets it, because the server reaches MinIO at `minio:9000` and a browser reaches it at
`localhost:9000`.

#### Revoking

`/admin/orders/{id}` lists an order's downloads with a **Revoke** button and how many times
each has been taken. Revoking stops that buyer and nobody else, takes effect on their next
click, and is reversible. These are the only forms on the order page — everything else there
is read-only, because an order records what happened and a button that changed it would be a
way to record money that never arrived. Revoking changes no financial fact.

#### Statistics

`/admin/products/{id}/downloads` reports total downloads, how many distinct buyers took
something, per-file counts, and the most recent downloads with the buyer against each.

**A count is an authorised click, not a completed transfer**, and the page says so. With a
signed URL the bytes never come through the store, so it cannot know how a transfer ended; a
connection that dropped at 80% and was started again is two.

Asking the bucket instead does not work, and not for a reason that might be fixed later:
neither GCS nor R2 exposes per-object read counts, and a presigned URL is *anonymous* to the
bucket — it has no idea which buyer, order or entitlement. That mapping exists only here.

#### Uploads

Through the server, streamed to storage. The request is spooled to a temporary file and
streamed on from there, so memory does not track the file size — measured, a 477 MB upload
grew the server by 75 MB and a 1.43 GB upload by 65 MB. What it does cost is temporary disk
(the file exists twice for the length of the request) and a held-open connection.

Uploading straight from the browser to the bucket would avoid both, and is deliberately not
built: it needs a CORS policy on the bucket, a widened `connect-src`, a JavaScript uploader
and an orphan sweep, and uploads here are rare and done by one operator watching them.

`DOWNLOAD_MAX_BYTES` caps one file, default 2 GiB. Unlike images there is no allow-list of
types: the bucket is private, every read is authorised, and the response carries the stored
`Content-Type`, `nosniff` and an attachment disposition — so refusing an unusual format would
only stop a shop selling what it sells.

#### Changing a product's kind

Frozen once the product has been **ordered**, and while a digital product still has **files
attached**. Neither protects purchase history — `order_items` snapshots the kind, so a
completed sale is already safe. What they protect is live state: flipping to digital would
leave a stock count nothing decrements, and flipping the other way would leave objects in
storage with nothing listing them. The second is a step rather than a dead end: remove the
files and the kind becomes changeable.

### Seeding

`cmd/seed` loads a plain products JSON file:

```json
[
  {
    "categories": ["books"],
    "slug": "the-quiet-machine",
    "title": "The Quiet Machine",
    "description": "Paperback, 248 pages.",
    "active": true,
    "option1_name": "Cover",
    "variants": [
      { "sku": "BOOK-TQM-PB", "option1": "Paperback", "price_cents": 24900, "stock_qty": 12, "active": true },
      { "sku": "BOOK-TQM-HC", "option1": "Hardcover", "price_cents": 34900, "stock_qty": 3, "active": true }
    ]
  }
]
```

`option1_name` through `option3_name` name the product's variant options; `option1` through
`option3` are each variant's values for them. Omit them all for a product with only one
version of itself.

#### Seeding a digital product

`kind: "digital"` plus a `files` list, which is the one thing a fixture can say that the
domain type deliberately cannot — an object key is storage's to choose, and a size and a
content type are facts about bytes:

```json
{
  "slug": "a-quiet-hour",
  "kind": "digital",
  "option1_name": "Format",
  "variants": [
    { "sku": "QH-AUDIO",            "option1": "Audio",              "price_cents": 8000 },
    { "sku": "QH-AUDIO-TRANSCRIPT", "option1": "Audio + transcript", "price_cents": 12000 }
  ],
  "files": [
    { "path": "downloads/sample-recording.wav",  "title": "A Quiet Hour — recording",
      "variants": ["QH-AUDIO", "QH-AUDIO-TRANSCRIPT"] },
    { "path": "downloads/sample-transcript.pdf", "title": "Transcript",
      "variants": ["QH-AUDIO-TRANSCRIPT"] }
  ]
}
```

`path` is **relative to the seed file's own directory**, and anything absolute or climbing
out of it is refused — a seed file is data, and data that can name any path on the machine
and have it uploaded to a bucket is a way to exfiltrate a private key by editing JSON.
`variants` are SKUs, the same natural key the variants themselves match on. `title` defaults
to the filename. The content type is **sniffed**, not taken from the extension.

Seeding files needs somewhere private to put them — `DOWNLOAD_DIR` or the `DOWNLOAD_*`
bucket. Nothing else the server requires is needed to seed. A fixture with files and no
storage configured is refused rather than half-loaded, along with files on a physical
product, a SKU that is not one of that product's variants, and a file that is not there —
all before a single row is written.

**Files match on the name they were seeded from**, so a second run retitles and re-links
rather than uploading a second copy. That is not only tidiness: replacing the row would mint
a new file id, and a buyer holding an entitlement would find that what they paid for had
quietly become something else.

The shipped fixture demonstrates the part that matters — the recording is granted by both
variants and the transcript by only one, which is what a per-variant file list is *for*. Both
sample files in `testdata/downloads/` are real and playable/readable, not renamed
placeholders.

`slug` may be omitted and is then derived from the title. Seeding is rerunnable: products
match on `slug` and variants on `sku`, so a second run updates titles and prices rather
than duplicating rows — and it leaves `stock_qty` on rows that already exist alone, since a
fixture is a starting point and not the truth about inventory. Variants missing from the
file are not deleted.

`categories` is a list of category slugs, and any that do not exist yet are created, named by
title-casing the slug — `gift-cards` becomes "Gift Cards". That keeps a fixture
self-contained — seeding a fresh database needs no prior trip to the admin — at the cost of a
typo becoming a new category rather than an error. A category that already exists is left
exactly as it is, name and position included, so re-seeding never undoes an edit.
Give it a proper name in the admin afterwards — products stay linked through that rename,
because the link is by id. Changing the *slug* is the one to think about, since that is what
filter URLs carry.

**There is no `image_url` field**, and an unknown field is an error rather than being
ignored — so a file carrying one is rejected with the field named. A fixture cannot upload
bytes, so the only thing it could set is a URL to somebody else's server, which is exactly
what is no longer allowed. Re-seeding therefore never disturbs an uploaded image.

```sh
make seed                              # testdata/products.json
make seed SEED_FILE=my-catalog.json
```

`testdata/products.json` is generic sample data: fictional titles, no real contact details.

## Storefront

| Route | Serves |
|---|---|
| `GET /` | The index: the store name, a line to replace, and the newest few products |
| `GET /products?q=…&category=…&page=…` | The catalog, optionally searched, filtered and paged |
| `GET /products/{slug}` | One product, with its variants |

### The index page

Deliberately almost empty, and meant to be replaced. It exists so that a fresh
deployment has a working front door rather than a `404`, and it carries the least
that is still a page: the store name from `STORE_NAME`, one plain line, the four
newest products, and a link to the catalog.

The products are **example content, not a feature**. They are the newest four —
there is no `featured` flag, no extra column and nothing to tick in the admin,
because a front page that needs curating before it works is a front page that
ships empty. They share the catalog's card grid (`product_grid`), so restyling a
card changes both pages rather than one of them.

Replacing the whole thing is one `pages/index.gohtml` in `TEMPLATE_DIR` defining
`content`. See [Theming](#theming).

One thing to know before adding to it: `/` is served outside the CSRF layer,
because it carries no form and therefore sets no cookie — the only HTML page in
the store that sets none at all. **A form added to the index would be refused with
a `403`.** Link to a page that has one instead.

### When a page is not found

Every HTML surface answers a missing page with the same rendered `404`: an unknown
URL, a withdrawn or misspelled product, a `?page=` past the end of the catalog, an
order id that is not one. It says what happened and offers a search box and a way
back, because the one page a visitor reaches by accident should not be a dead end.

Two deliberate exceptions:

- **Byte endpoints stay plain.** A missing `/static/…` or `/images/…` answers with
  Go's one-line `404`, since nothing reads an HTML page out of an `<img>` tag.
- **A broken theme still answers `404`.** If an overridden `not_found` fails to
  render, the plain one is sent rather than an empty `200` — the status is what a
  browser and a crawler act on.

Installing it costs a `"/"` pattern on the mux, which is how a custom not-found
handler is done in Go: `ServeMux` has no `NotFoundHandler` to set. One consequence
is that a known path requested with an unregistered method now answers `404` rather
than `405`, because a pattern that matches beats one that would have matched under
a different method.

The two catalog routes below are read-only, and both answer twice over: a full page for an ordinary visit, and a
bare fragment when htmx asks (`HX-Request: true`, unless `HX-Boosted` says the browser is
replacing the whole document). One URL serves the store and an embedder, so there is no
second API to keep in step.

**A request from another origin gets no cookies at all** — that is what makes the catalog
droppable into someone else's page. A *first-party* visit does pass through the CSRF layer,
because the product page carries an add-to-cart form and a form needs a token; it never picks
up a cart cookie there. The two chains serve identical HTML.

Only active products with at least one active variant appear; an inactive product is a `404`,
not an unlinked page. Sold-out variants are *shown* and marked unavailable rather than
hidden, because a size vanishing from a selector reads as a bug to whoever is looking for it.

### Search and filtering

Three parameters on the one catalog route, in any combination: `q` searches, `category`
narrows, `page` pages. A bare `/products` still means everything, so nothing about the plain
catalog changed.

**Search matches words and spellings both, because neither alone is enough.** Postgres
full-text handles the first: a generated `tsvector` column with the title weighted above the
description, queried through `websearch_to_tsquery`, so "books" finds a title containing
"book". It cannot survive a typo. `pg_trgm` handles the second — trigram similarity finds
"quiet machnie" — and has no idea "books" and "book" are the same word. Each covers the other's
blind spot, and results are ordered by whichever of the two scores is higher.

The costs, stated because they are the ones an adopter will meet:

- **`pg_trgm` must exist.** It is a core contrib extension, present in the Postgres image this
  repo runs, and *trusted* since PostgreSQL 13, so the database owner can create it without
  superuser. Every managed host worth naming permits it. The migration creates it — see the
  fixed-schema rule under [Migrations](#migrations).
- **A query under two characters is treated as empty**, because a trigram index cannot help
  below three and a one-letter search returns the whole catalog regardless.
- **English is hard-coded** in the `to_tsvector` configuration. Stemming is
  language-specific, and a store selling in another language wants that word changed.

**Selecting several categories widens the results, it does not narrow them.** So
`?category=books&category=apparel` returns both. The opposite reading — products that are
simultaneously a book and apparel — is
almost always empty, because these are kinds of thing rather than facets like size and colour.
The filter list itself always shows every category in its configured order, whether or not the
current search hits it: a list that reshapes itself as you type moves the option you were
reaching for.

**Pagination is `LIMIT`/`OFFSET`, 24 to a page**, and the total is counted in the same query as
the page rather than a second one that could disagree with it. A page past the end is a `404`,
for the same reason an inactive product is: it stops `?page=900` from being a silent success
that a crawler will happily index. The cost of offset is that a deep page scans and discards
rows on the way to its window — cheap at the size of catalog this store is for, and the reason
a cursor was not worth the complexity here.

**None of it needs JavaScript.** The filter is an ordinary GET form whose checkboxes share the
name `category`, which is how one form produces repeated parameters without help; the page
links are ordinary links. htmx then upgrades both to swap just the results list and push the
URL, so the address bar always describes what is on screen and a search is a shareable link.

### Embedding the catalog elsewhere

Set `EMBED_ORIGINS` to the origins allowed to fetch the fragments, and they can be dropped
into a page on another domain:

```html
<div hx-get="https://store.example.com/products" hx-trigger="load"></div>
```

That is the whole integration. It works because the fragments need no cookie: `EMBED_ORIGINS`
controls both the CORS allowance and the CSP's `frame-ancestors`, and no credentialed CORS
header is ever sent, so a permissive origin list cannot become a way to act as somebody.

Everything from "add to cart" onward stays first-party on the store's own domain. That keeps
the cart cookie first-party and sidesteps `SameSite=None`, third-party cookie blocking and
iframe checkout entirely — the split is a feature of the design, not a limitation of it.

Concretely: the embedded fragment carries **no** add-to-cart form, and links to the store's
own product page instead. A cart form on another origin could not work anyway — `SameSite=Lax`
withholds the cookie on a cross-site post, and the CSRF origin check would refuse it.

**The embedded fragment carries no search box, filter or page links either**, for a different
reason: those controls push the URL they navigate to, and inside somebody else's page that
would rewrite *their* address bar. An embedder gets the first page and a link through to the
full catalog on the store's own domain, which is where searching belongs. That link matters —
an embedded fragment silently showing 24 products out of 200 would look like the whole shop.
Searching and filtering are first-party, on the same reads-anywhere, writes-first-party line
everything else here follows.

## Cart

| Route | Does |
|---|---|
| `GET /cart` | The cart page (or its body, for htmx) |
| `GET /cart/status` | The "N items in your cart" fragment |
| `POST /cart/items` | Add a variant |
| `POST /cart/items/{variantID}` | Set a quantity; **0 removes the line** |
| `DELETE /cart/items/{variantID}` | Remove a line (what htmx sends) |

A cart is a database row keyed by an opaque 24-byte random token that is also the cookie
value — `HttpOnly`, `SameSite=Lax`, 30 days, scoped to `/cart`. Not a signed cart carried in
the cookie: prices and stock are live server-side truth that has to be re-read on every
render anyway, so reading the cart from the database is not extra work, it is the same work.
The token is unguessable rather than signed, because holding one grants nothing beyond one
anonymous basket.

Consequences worth knowing before changing any of it:

- **The cart holds quantities, not prices.** Every render prices the lines from the catalog
  as it stands, so a price change or a sell-out shows up next time the cart is looked at.
  Snapshotting happens when the order is created, not before.
- **Withdrawn or sold-out lines stay visible** and are marked unavailable, with the reason,
  and they block checkout. A line vanishing between page loads reads as a bug — or worse, as
  a silent change to the total.
- **Stock is checked against the resulting total**, so two adds of three cannot smuggle six
  past a limit of four. A refusal says how many are actually left.
- **A cart row is created on the first add**, not on the first visit, so browsing leaves no
  trail of empty carts.
- **A stale cookie starts a fresh cart** rather than an error page, for shoppers returning
  after the cleanup job has been through.
- **Without JavaScript everything still works**: forms post and redirect, and the remove
  button submits quantity 0. With htmx, adding swaps a small status block so the shopper
  keeps their place, and quantity changes swap the cart body.
- Deleting a variant in the admin **empties it from carts** (`ON DELETE CASCADE`), because an
  abandoned cart must not stop the shop owner editing the catalog. `order_items` deliberately
  does the opposite: purchase history is not rewritable.

## Checkout

| Route | Does |
|---|---|
| `GET /cart/checkout` | The shipping form, alongside what is being bought |
| `POST /cart/checkout` | Creates a **pending** order and hands over to the gateway |
| `GET /cart/checkout/success` | The gateway's `return_url` — **informational only** |
| `GET /cart/checkout/cancel` | The gateway's `cancel_url` |
| `POST /payments/{gateway}/callback` | The only thing that can mark an order paid |

**Checkout lives under `/cart`, not at `/checkout`.** The cart cookie is scoped to `/cart`
so the catalog pages stay genuinely cookie-free and embeddable, and a page at `/checkout`
would therefore never be sent the token identifying the basket it is meant to be checking
out. Nesting it costs a URL segment; the alternatives were giving the catalog a cookie back
or issuing a second one.

The order of events matters more than the routes do:

- **An order is a snapshot.** A cart holds quantities and prices everything live; an order
  copies the title, options and unit price in as they were. A later price rise, rename or
  withdrawal cannot rewrite what somebody bought.
- **The total is computed from the catalog inside the transaction that creates the order**,
  never from the figure the submitted page happened to be showing. That total is what the
  gateway is asked for and what its notification is checked against, so it has to be a number
  the database agrees with.
- **Stock does not move at checkout.** It moves when the money arrives. An abandoned checkout
  therefore holds no inventory, which is the right trade for a small shop: two people can
  reach a payment page for the last item, and the second one is refunded rather than everyone
  being blocked by carts nobody will pay for.
- **The cart survives checkout** and is emptied when payment succeeds, so backing out of the
  gateway's page leaves the basket intact.
- **`/cart/checkout/success` grants nothing.** A shopper can navigate there without paying, so
  it says the payment is being confirmed rather than that it succeeded. It names the order —
  the cart cookie identifies it, and a reference is what a customer needs to quote.

The hand-over to the gateway is a real cross-origin form post, not a redirect, which has two
consequences worth knowing before touching the CSP: the gateway's origin must be in
`form-action`, and the submit-on-load script is a **file** (`/static/redirect.js`) because
`script-src 'self'` forbids the inline script that would otherwise do it. Without JavaScript
the form's button is the whole mechanism, and it says so.

## Payments

PayFast is the only gateway, behind a small `payment.Gateway` interface so adding another is
code and no migration — the order's `gateway_*` columns are deliberately gateway-neutral.

### Setting it up

1. Get a merchant id and key from the [PayFast dashboard](https://sandbox.payfast.co.za) —
   the sandbox's have no relationship to a live account's.
2. Set a **salt passphrase** in the dashboard and put the same value in
   `PAYFAST_PASSPHRASE`. Set on one side only, every signature fails.
3. Leave `PAYFAST_SANDBOX=true` until a full sandbox payment has worked end to end.
4. Make sure PayFast's servers can reach the callback. `notify_url` is derived from
   `BASE_URL`, which on a laptop is `localhost` and unreachable from the internet — so local
   testing needs a tunnel, and `PAYFAST_NOTIFY_URL` is where its hostname goes.

Then, in order: place an order, pay it on the sandbox, and check that the order is `paid` and
stock has moved. Replaying the captured notification body with `curl` must not move stock a
second time.

### Going live

**`PAYFAST_SANDBOX` defaults to `true` on purpose**, so that nobody's first afternoon with
this project charges a real card. The cost of that default is the mirror mistake: a
deployment that never sets it takes no money and looks like it works. Set it explicitly
wherever you deploy — [`infra/terraform`](infra/terraform) requires it as a variable with no
default for exactly this reason.

Switching it off is two changes, not one. The merchant credentials must be your own: the
server **refuses to start** with `PAYFAST_SANDBOX=false` and PayFast's published sandbox
merchant id, because that combination signs every payment with a key printed in PayFast's
documentation.

One more, if you deploy behind any proxy or managed platform: **`TRUST_PROXY_IP` must be
`true`**, or the source-IP check below compares PayFast's ranges against your load
balancer's address and rejects every genuine notification — money taken, nothing recorded.
It must stay `false` when nothing in front of the server sets `X-Forwarded-For`, since a
client could then claim any address it liked.

### How a notification is authenticated

The customer's browser returning to `return_url` proves nothing. The **ITN** — PayFast's
form-encoded POST to `notify_url` — is the only statement about a payment this store trusts,
and it passes four independent checks before anything happens:

1. **The signature recomputes** over the fields exactly as received, in the order received.
2. **The source IP** is one of PayFast's published ranges.
3. **PayFast confirms it**, when the exact bytes received are posted back to
   `/eng/query/validate`.
4. **The merchant id** is ours.

None of them is sufficient alone. The signature can be produced by anyone holding the
passphrase, an IP can be spoofed or shared with whoever else is behind the same proxy, and the
server-to-server check proves the data is PayFast's but not that it was meant for this store.

Then the handler does what only it can: find the order, check the amount against the order's
own total, and stop a replay from decrementing stock twice.

**The callback always answers `200`.** A gateway retries anything else, and a notification
that fails validation is not "try again later" — it is forged or broken, and neither improves
on the third attempt. Rejections are logged in full, naming the check that failed, and
dropped. It is also outside the CSRF group by *not being in it* rather than by an exempt-path
string that has to keep matching the route.

### The signature, and why it is spelled out in code

Three details account for nearly every PayFast integration failure, and
[`internal/payment/payfast`](internal/payment/payfast) says so in its package comment:

- **The field order is the order they were submitted in, not alphabetical.** Sorting produces
  a signature PayFast rejects. This is why `payment.Field` is a slice and never a map anywhere
  near a signature.
- **`urlencode` is PHP's**, which every reference implementation uses. Go's `url.QueryEscape`
  is nearly the same and differs over `~`, and one character is a failed signature with no
  diagnostic beyond "mismatch".
- **Outgoing and incoming disagree about blank fields.** They are excluded when building the
  redirect form and *included* when verifying a notification, because that is what PayFast's
  own code does in each direction. Building the form sidesteps it by not submitting blanks at
  all.

`TestPayFast_SignatureMatchesKnownVector` pins both the parameter string and its digest.
**Put that string through [PayFast's signature tool](https://developers.payfast.co.za) before
taking real money** — no test suite can do that step, and the cost of skipping it is every
payment being rejected.

## Orders and email

Two pages, both read-only:

| Route | Shows |
|---|---|
| `GET /admin/orders` | Recent orders, newest first |
| `GET /admin/orders/{id}` | What to pack, where it goes, and what the gateway said |

**There are no buttons on either page, deliberately.** An order records something that
happened, and the only thing allowed to change one is an authenticated gateway notification.
A "mark as paid" button in the admin would be a way to record money that never arrived. A test
asserts that no route under `/admin/orders` accepts a `POST`, so adding one is a decision
somebody has to make on purpose.

The order page shows the **snapshot** — the title, options and unit price as they were when
the order was placed — so renaming, repricing or withdrawing a product afterwards does not
rewrite what somebody bought. It also shows the raw gateway notification, for the day a
customer and a bank disagree about what happened.

The list is capped at the 200 most recent. Products are a small fixed set; orders accumulate
forever, so "the catalog is small" does not carry over to this table.

### What gets sent, and when

When an authenticated notification says an order is paid, two emails go out — and both go out
**after** the order is recorded paid. That ordering is the whole point: a mail server having a
bad afternoon must never be able to lose a sale. Nothing in the mail path can fail the payment
callback.

- **The customer** gets a receipt, once. `orders.emailed` records it, so a replayed gateway
  notification does not send a second copy. Both a plain-text and an HTML part are sent; the
  plain-text one is not optional, because a receipt has to arrive readable in a client that
  refuses HTML.
- **`ORDER_NOTIFY_EMAIL`**, if set, gets a work order: what to pack, where it goes, and a link
  to the admin page. It also carries the **oversell warning**, which otherwise exists only in
  the logs — the person who has to tell a customer their item is gone should not have to find
  that in a log aggregator.

They are two separate sends rather than one message with two recipients: a receipt and a work
order say different things, and one of them failing should not suppress the other.

**Mail is required and the server will not start without it.** That reverses what this
section used to say. The old reasoning — the shop's job is to take an order and record it,
which does not depend on a mail server — held while every product was a parcel. It stopped
holding when a product could be a download: that link is emailed and nowhere else, because
only its hash is stored. `mailer.Discard` still exists for tests and for an adopter assembling
their own `main`, but the shipped binary no longer reaches it.

Sending itself lives in [`github.com/17xande-dev/mailer`](https://github.com/17xande-dev/mailer),
which is shared with another application rather than kept here.

### Microsoft Exchange Online

Basic Auth for SMTP client submission is going away, so an Exchange mailbox is reached with
**XOAUTH2**: the password becomes an OAuth2 access token, fetched per send and cached until
shortly before it expires.

```bash
SMTP_HOST=smtp.office365.com
SMTP_PORT=587
SMTP_TLS=starttls
SMTP_USERNAME=orders@example.com   # the mailbox; XOAUTH2 authenticates as a named one
EMAIL_FROM=orders@example.com
SMTP_OAUTH_TENANT_ID=...
SMTP_OAUTH_CLIENT_ID=...
SMTP_OAUTH_CLIENT_SECRET=...
# and no SMTP_PASSWORD — setting both refuses to boot
```

**The tenant-side setup is the part that bites**, and none of it is visible from here: a
tenant that has not been set up hands out a token perfectly happily and the mail server then
refuses it. You need an Entra ID app registration with the **`SMTP.SendAsApp`** application
permission (Office 365 Exchange Online) and admin consent, a service principal for it
registered in Exchange Online, **Send As** on the mailbox, and **SMTP AUTH enabled for that
mailbox** — it is disabled tenant-wide by default. Check each against current Microsoft
documentation; these requirements move.

Worth weighing before choosing this at all: Exchange Online is a mailbox service rather than
a transactional relay, and it throttles accordingly. A dropped confirmation costs a buyer
their download link, since it exists nowhere else. A dedicated transactional provider over
ordinary SMTP is the lower-risk option for a storefront, and needs none of the above — just
`SMTP_USERNAME` and `SMTP_PASSWORD`.

### Email templates

`mail/email_order_paid.txt`, `mail/email_order_paid.gohtml` and `mail/email_order_notify.txt`,
overridable from `TEMPLATE_DIR` like any other template. The `.txt` files go through
**`text/template`** and the rest through `html/template`, which is not a detail: running a
receipt through the HTML escaper puts `&amp;` in front of a customer.

They are the one thing under `TEMPLATE_DIR` that no layout wraps: a message is not a page,
and giving it the store's `<head>` and site nav would be actively wrong. They still see
`partials/`, so `{{template "csrf" …}}` would resolve in an email — which is meaningless
and harmless, and cheaper than a second partials set to prevent it.

The HTML part is deliberately primitive — table layout, inline styles, no external CSS and no
images. Mail clients are twenty years behind browsers, and a receipt that renders everywhere
beats one that looks better in three clients and breaks in the rest.

### Overselling

Two people can pay for the last item, because stock is only taken at payment. When a
decrement would go negative the order is still recorded paid — the money has been taken, and
refusing to record it would lose the sale *and* still be oversold — and the event is logged at
error level. Surfacing it in the admin's order view is part of the hardening phase.

### Money

Integer cents everywhere in Go and in the database; a decimal string only at the gateway
boundary and in rendered pages. A float total rounded differently from a gateway's amount
string is a real and hard-to-find class of bug, and the amount comparison in the callback is
exactly where it would bite.

### Theming

A theme is two directories: templates that override the embedded ones by name, and assets
that override the bundled ones by name. Nothing is forked, nothing is rebuilt, and anything
you do not override keeps coming from the binary.

| | Points at | Overrides |
|---|---|---|
| `TEMPLATE_DIR` | a directory shaped like [`internal/handler/templates/`](internal/handler/templates) | templates, by the path of the file — `pages/cart.gohtml` replaces `pages/cart.gohtml` |
| `STATIC_DIR` | a flat directory of assets | bundled files, by filename |

`make up` and `make run` already set both, at [`theme/templates`](theme) and
[`theme/static`](theme), with **`THEME_RELOAD=true`** — so writing a theme is editing a file
and refreshing the page. Both directories are empty on a clean checkout, which is why the
store looks the same until you put something in one.

#### Writing one

Start from a default rather than from nothing — the defaults are meant to be read:

```sh
cp internal/handler/static/styles.css theme/static/styles.css     # restyle
mkdir -p theme/templates/pages                                    # the path is the contract
cp internal/handler/templates/pages/products.gohtml theme/templates/pages/
```

Then edit and refresh. Deleting your copy puts the default back, also without a restart.
Keep the subdirectory: an override is found at the same path it has in the defaults, and a
`products.gohtml` at the top of `TEMPLATE_DIR` is a file nothing looks for.

Most themes never need a template at all. **The CSS is the intended level to work at**:
every colour, size and spacing value in the default theme is a custom property in one
`:root` block at the top of `styles.css`, and no colour literal appears anywhere below it.
Rebranding is editing a dozen values.

```css
/* theme/static/styles.css — the whole theme, for many stores */
:root {
  --paper: #fffdf8;  --paper-sunk: #f4efe4;
  --ink: #241f18;    --ink-soft: #5c5348;  --ink-faint: #8d8375;
  --rule: #e2d9c8;
  --accent: #7a2e1f; --accent-ink: #fffdf8;  /* buttons, links, prices */
  --warn: #8a2f1d;
  --font: "Iowan Old Style", Georgia, serif;
  --radius: 0;       --page: 1000px;         /* square corners, narrower column */
}
```

The full set is `--paper`, `--paper-sunk`, `--ink`, `--ink-soft`, `--ink-faint`, `--rule`,
`--accent`, `--accent-ink`, `--warn`; `--font`, `--font-mono`, `--text`, `--text-small`,
`--text-large`, `--title`, `--title-page`; `--gap-xs` through `--gap-xl`; `--radius`,
`--radius-lg`, `--measure`, `--page`. Overriding just these in a file that then `@import`s
nothing means you are also replacing the rest of the stylesheet — so either copy the whole
default and edit its `:root`, or write your own from scratch. There is no cascade between
the bundled file and yours: same name, one wins.

#### Web fonts

**No web font in the default theme.** The system font stack is what every operating system
already has, it loads instantly, and it puts no third-party request on the page. Two ways to
change that.

**Self-host.** Drop the `.woff2` into `STATIC_DIR` — that is one of the reasons new names are
served — and reference it from your stylesheet with a `/static/...` URL. Nothing else changes:
the file is served from this origin, so the CSP already allows it and no config is involved.
Fonts under an open licence, which is most of Google's, can be used this way; `.woff2` alone
is enough for every browser this project supports.

**Use a hosted service.** Adobe Fonts requires this — its licence has no self-host tier — and
it needs two variables:

```sh
FONT_ORIGINS=https://use.typekit.net,https://p.typekit.net
FONT_CSS_URL=https://use.typekit.net/abc1def.css
```

`FONT_CSS_URL` is the stylesheet the default layout links from its `<head>`. `FONT_ORIGINS`
widens the CSP, and it widens **two** directives, because a hosted font is two fetches: the
browser fetches the stylesheet declaring the fonts (`style-src`) and then the font files that
stylesheet names (`font-src`). Typekit splits those across two hosts, which is why both are
listed — a kit that loads and *still* renders in the fallback font almost always means
`p.typekit.net` is missing. Google Fonts is the same shape, with
`fonts.googleapis.com,fonts.gstatic.com`.

Set both or neither. `FONT_ORIGINS` alone allows a font nothing asks for; `FONT_CSS_URL` alone
is a boot failure, because the CSP would block the stylesheet and the only symptom in a browser
is a console warning.

Widening the CSP and linking the kit makes the family *available* — it does not apply it. Set
`--font` in a `styles.css` under `STATIC_DIR` to use it.

Three things worth knowing before choosing the hosted route:

- **Only the `<link>` embed is supported.** Adobe's Web Project panel offers a JavaScript
  loader by default; pick the CSS embed instead. The loader needs `script-src` widened,
  `connect-src` opened for its config fetch, and a nonce for both the inline `<script>` it
  gives you and the inline `<style>` it injects. See
  [Response headers](#response-headers) for why that answer is no.
- **`script-src` stays `'self'` either way.** A font origin cannot run JavaScript on any page
  of this store, the checkout included. That is what makes this a narrow widening.
- **It puts a third-party request on every page, including the checkout**, and the font CDN
  sees your visitors' IP addresses. That is a real consideration under the GDPR — a German
  court has ruled against a site for exactly this with Google Fonts — and a reason to prefer
  self-hosting where the licence allows it.

#### Overriding templates

Overriding is per *path*: a file at `pages/index.gohtml` under `TEMPLATE_DIR` replaces the
definitions in the embedded `pages/index.gohtml` and leaves everything else alone. Put it
in the wrong subdirectory and it is simply not found — nothing warns, because a directory
of files the server has never heard of is a normal thing for a theme directory to contain.

The directory decides two things, so it is worth knowing before copying anything:

| Directory | Holds | Wrapped in |
|---|---|---|
| `layouts/` | `public.gohtml`, `admin.gohtml` — each defining `layout` | — |
| `partials/` | pieces parsed into **every** page: `document_head`, `csrf`, `err`, `product_grid`, `error_reference` | — |
| `pages/` | the storefront, and the admin sign-in page | `layouts/public.gohtml` |
| `admin/` | everything behind the admin login | `layouts/admin.gohtml` |
| `mail/` | `email_order_paid.gohtml` and the two `.txt` bodies | nothing — a message is not a page |

**Each page is parsed into a set of its own**, which is the thing to understand before
writing a theme. A definition in `pages/products.gohtml` reaches the catalog and no other
page, so a page can fill in a block, restyle a partial for itself, or define a fragment,
without any of it leaking into the cart. The cost is the other side of the same coin: a
page can only call a partial, its own layout, or something it defines itself — a call to
anything else renders as a `500` on that page, because Go resolves template names when a
template runs and not when it is parsed. Anything two pages need belongs in `partials/`.

**A page file defines `content`**, and the layout renders it. Two blocks exist:

| Block | Filled by | Is |
|---|---|---|
| `content` | every page, always | The page itself. A page that defines none refuses the boot |
| `nav_extra` | `pages/products.gohtml`, if you want it | An addition to the site nav. Empty by default: the catalog's filter form is rendered above the grid instead, and two copies of one search form on one page is two search boxes |

Moving the filter form into the header is the two-definition case the blocks exist for. A
`pages/products.gohtml` in `TEMPLATE_DIR` holding nothing but this does it:

```html
{{define "content"}}
<h1>All products</h1>
<div id="products">{{template "products_list" .}}</div>
{{end}}

{{define "nav_extra"}}{{template "products_filters" .}}{{end}}
```

Nothing else in the catalog is touched: `products_filters`, `products_list` and
`products_pager` keep coming from the default file, since an override replaces the
definitions it names and no others.

The pages, and the fragments each one owns — a fragment is what an htmx swap renders, so
overriding one changes both the full page and the swap, which is what keeps the two
consistent:

| File | Also defines | Is |
|---|---|---|
| `pages/index.gohtml` | | The front page. Small on purpose — this is the one most shops replace outright |
| `pages/products.gohtml` | `products_list`, `products_filters`, `products_pager` | The catalog, the results inside it, the search and category form, and the page links |
| `pages/product.gohtml` | `product_detail`, `add_to_cart` | The product page, its body, and the variant/quantity form |
| `pages/cart.gohtml` | `cart_items`, `cart_status` | The cart, the lines htmx swaps, and the count a product page shows after an add |
| `pages/checkout.gohtml` | `checkout_form` | The checkout, and the form htmx swaps back with its errors |
| `pages/checkout_redirect.gohtml`, `pages/checkout_success.gohtml`, `pages/checkout_cancel.gohtml` | | The hand-over to the gateway, and the two pages a shopper comes back to |
| `pages/downloads.gohtml` | | The page a buyer reaches from their confirmation email |
| `pages/not_found.gohtml` | | The 404, which every mistyped URL and withdrawn product lands on |
| `pages/error_client.gohtml`, `pages/error_server.gohtml` | | The 4xx and 5xx pages. See [When something goes wrong](#when-something-goes-wrong) |
| `pages/admin_login.gohtml` | | The sign-in page. In `pages/` deliberately: it uses the public layout, so it cannot offer to sign out of a session it does not have |
| `admin/admin_*.gohtml` | `variant_errors`, `product_image`, `product_files` | The admin. The files keep their `admin_` prefix because a page's file name is the name it is rendered by, and `admin/products.gohtml` would collide with the catalog |
| `partials/product_grid.gohtml` | | The card grid, **shared by the catalog and the index**. Override this to restyle a product card everywhere it appears — overriding `products_list` alone changes the catalog only |
| `partials/document.gohtml` | | Everything from the doctype to `</head>`, on every page of both layouts. Two htmx settings in it are load-bearing; keep them |
| `mail/*` | | See [Email templates](#email-templates) |

Four things to know before writing one:

- **Class names are the contract** between the templates and the stylesheet. They describe
  what a thing is — `.product-card`, `.site-header`, `.price`, `.field` — so that overriding
  one side and not the other keeps working. Change the markup and keep the names, or change
  both together.
- **Every form needs `{{template "csrf" .CSRFToken}}`**, or it gets a `403`. See
  [CSRF](#csrf).
- **Templates get exactly the data the handler passes.** Every page embeds `.Title`,
  `.StoreName`, `.Currency` and `.CSRFToken`, plus its own — `.Products`, `.Product`,
  `.Cart`, `.Order`. The functions available are `money` (cents → a displayed amount),
  `asset` (a bundled or overridden file → its hashed URL), `image` (a product's image key →
  where it is served from) and `linebreaks`.
- **A field or a template name that does not exist is a `500` on that page**, not a refused
  boot: Go checks both when a template *runs*, not when it is parsed. This is the reason
  `nav_extra` is filled by the catalog's own file rather than called from the layout —
  `{{template "products_filters" .}}` in the site nav renders on the catalog and breaks
  every other page, because none of their data carries `.Search` or `.Facets`. Render each
  page you have touched before shipping the theme.

#### Reloading, and not reloading

`THEME_RELOAD=true` re-reads both directories on **every request**. It is for writing a
theme and for nothing else, and a deployment must leave it off:

- It reparses every template and re-reads every asset per request.
- It moves a *later* mistake — a file saved half-written while the server is up — from
  impossible to a `500` on whichever page uses it. A theme that is already broken at startup
  still fails the boot either way: both directories are validated once before anything
  serves, and a template that does not parse or a `STATIC_DIR` that cannot be read
  **refuses to start**. That is the behaviour you want in front of customers, where the
  alternative is finding out from the first shopper.

Either way the theme is read from disk at runtime, so shipping a change is replacing files
and restarting — never a rebuild. The server logs a warning at startup whenever reloading is
on, so a deployment that has it by accident says so.

One thing does not need reloading to appear: **asset URLs carry a hash of the file's
contents** (`/static/styles.css?v=fc1508f97297`), so a replaced stylesheet is a different
URL and no cache can serve the old one. That is what makes a refresh enough rather than a
hard reload.

### Bundled assets

Four things ship inside the binary: htmx, `redirect.js`, a `logo.svg` and a
`placeholder.svg`. A store with no configuration at all therefore has a mark in its header
and a picture on every product card.

These are **not** product images. A product image is uploaded, keyed and deleted by the
application; these are replaced by an operator. Keeping them apart means a sweep over
uploaded objects can never consider a logo an orphan.

**Override any of them with `STATIC_DIR`**, which is to assets what `TEMPLATE_DIR` is to
templates: a file there shadows a bundled one of the same name, and a new name is served
too — so an overridden template can reference its own `hero.png`. Read at startup, so a
change needs a restart and never a rebuild — or no restart either, under
[`THEME_RELOAD`](#reloading-and-not-reloading). Rebranding is dropping a `logo.svg` into a
directory.

The defaults are deliberately generic: the logo has no text, because the store's name comes
from `STORE_NAME` and a name baked into a logo would be wrong for every adopter but one.

Everything is served from this origin, which is why the CSP needs no allowance beyond
`'self'` for any of it. htmx in particular is vendored rather than loaded from a CDN, so the
store works offline and no third-party origin sits near the payment path. `redirect.js` is a
file for the same reason: with no `'unsafe-inline'`, an inline script would simply be
blocked, leaving the shopper on a page waiting for something the browser refused to run.

Asset URLs carry a hash of the contents (`/static/logo.svg?v=fc1508f97297`) and are served
`immutable`, so a replacement appears immediately rather than after a cache expires.

**Only extensions in the content-type map are served** — images, CSS, JS and fonts. That is
what keeps a note left in either directory from becoming a URL, and it applies to
`STATIC_DIR` too, so an `.html` or a `.php` dropped there is not published. See
[`internal/handler/static/README.md`](internal/handler/static/README.md).

## Dependencies

The objection this project has is to **frameworks**, not libraries: something that owns the
shape of the application, dictates its architecture and ages on someone else's schedule
would defeat the point of a stdlib-shaped `net/http` and `html/template` design. A small,
single-purpose, widely-reviewed library that does one thing is a different proposition, and
is preferred over hand-rolling anything security-sensitive or fiddly. The standard mechanics
of a website — password hashing, CSRF tokens, session signing, migrations — are solved
problems, and a local reimplementation read by one person is not an improvement on one read
by thousands.

The counterweight is the Go idiom that a little copying beats a little dependency. Where a
package is a thin wrapper over something the stdlib already does, writing or copying those
few dozen lines avoids inheriting a release cadence and a transitive graph. The deciding
question is the depth of the problem, not the size of the dependency.

| Dependency | For |
|---|---|
| [`jackc/pgx/v5`](https://github.com/jackc/pgx) | Postgres driver and pool. No cgo, so the binary stays static |
| [`pressly/goose/v3`](https://github.com/pressly/goose) | Migrations: advisory locking, `NO TRANSACTION` support, and a CLI for the day one needs hand-holding |
| [`justinas/nosurf`](https://github.com/justinas/nosurf) | CSRF tokens and origin checks |
| [`golang.org/x/crypto`](https://pkg.go.dev/golang.org/x/crypto) | `argon2` for admin password hashing, `bcrypt` to keep older hashes verifying |
| [`golang.org/x/time`](https://pkg.go.dev/golang.org/x/time/rate) | The token bucket behind the rate limits |
| [`17xande-dev/mailer`](https://github.com/17xande-dev/mailer) | Sending email, behind one `Sender` interface — the SMTP transport, the XOAUTH2 token dance for Exchange, and the fake the handler tests inject. Shared with another application, which is why it is a module rather than an `internal/` package. It pulls in [`wneessen/go-mail`](https://github.com/wneessen/go-mail) for MIME, RFC 2047 subjects, quoted-printable, STARTTLS and implicit TLS |
| [`minio/minio-go/v7`](https://github.com/minio/minio-go) | Object storage over the S3 API — R2, GCS interop, MinIO |

Everything else is stdlib so far, by decision rather than by rule. Notably **not** taken:
a router (`ServeMux` does method and wildcard patterns), a validation library (struct tags
fight the per-field messages these forms need), and a decimal type (money is
integer cents). A UUID library is not needed *directly* — the database generates ids — and
`google/uuid` now arrives indirectly with minio-go, which is fine.

### Build-time tools

Separate from the table above, because nothing here links into the binary:

| Tool | For |
|---|---|
| [`sqlc`](https://sqlc.dev) | Generates the stores' row structs and scan code from the SQL |

sqlc is pinned in the **Makefile**, not as a `go tool` directive in `go.mod`. That is a
deliberate trade: `go tool` would pin the version alongside the code, but it also puts about
forty indirect modules — a MySQL driver, antlr, cel-go — into the file this README points at
as the project's dependency statement, and grows `go.sum` from 62 lines to 165. For something
that never reaches the binary, keeping `go.mod` a truthful description of what the server
depends on is worth more than the convenience. `make sqlc-install` installs the pinned
version; CI runs it before anything else.

### Decisions still open

Recorded here so they are decided deliberately rather than by default:

| Decision | Candidate | When |
|---|---|---|
| Server-side sessions | [`alexedwards/scs`](https://github.com/alexedwards/scs) | **Closed: not taken.** The trigger arrived and the answer was a hand-rolled `admin_sessions` table. scs keys an opaque blob by token with no user column, so "end every session belonging to this account" — the one operation that reopened the question — cannot be expressed against its schema without querying around the abstraction. Its other features (flash data, idle-vs-absolute timeouts) go unused here, and dropping the signed cookie removed a dependency rather than adding one |
| `AND` category filtering | a second parameter, or a toggle in the filter form | When a shop's categories overlap enough that widening the results is the wrong default |
| Accent-insensitive search | `unaccent`, behind an `IMMUTABLE` wrapper so it can be indexed | When a catalog carries accented titles and "cafe" failing to find "café" starts costing sales |
| Keyset pagination | a cursor on the ranking and title | When a catalog is deep enough that discarding rows to reach a late page is measurable |
| Tuned trigram thresholds | `AfterConnect` on the pgx pool | When the defaults visibly over- or under-match; they are session settings, so they belong on the connection, not in a query |

## When something goes wrong

Every HTML surface answers a failure with a rendered page rather than a line of plain
text: an unknown URL, a withdrawn product, a form whose token has expired, a request
that came too fast, a fault on the server. Three pages cover it — `pages/not_found.gohtml`
for 404, `pages/error_client.gohtml` for the rest of the 4xx range,
`pages/error_server.gohtml` for 5xx — and all three are overridable from `TEMPLATE_DIR`.
They are three files rather than one because they say genuinely different things, and a
shop should be able to reword one without touching the others; the reference block they
share is `partials/error_reference.gohtml`.

**In development the page says what broke; in production it does not.** The signal is
`BASE_URL`: an `https://` origin is production and the page shows only a reference, and
anything else shows the Go error as well. That is the same signal `Secure` cookies and
HSTS already use, so "is this production" has one answer rather than three. The error
string names tables, columns and constraints, which is reconnaissance in front of a
stranger and a first diagnosis in front of whoever is writing the code.

**Every response carries a request id**, echoed as `X-Request-Id` and printed on the
error page as a reference. The same id is on every log line for that request, so "I got
an error and it said 7f3a9c2e" is enough to find the one line that says why. An id
arriving in `X-Cloud-Trace-Context` (Cloud Run sets one on every request) or
`X-Request-Id` is adopted rather than replaced, so the store's logs and the platform's
name the same request.

Two deliberate exceptions to all of the above:

- **Byte endpoints stay plain.** A missing `/static/…` or `/images/…` answers with Go's
  one-line 404, because nothing reads an HTML page out of an `<img>` tag.
- **A broken theme still answers correctly.** If an overridden error template will not
  render, the plain text is sent with the same status — a 500 that fails to render must
  not become an empty 200.

### Errors and htmx

htmx does not swap `4xx`/`5xx` responses by default, which quietly discarded every
refusal this store sends: the cart answers "Only 2 of that option left" as a `409`
carrying the fragment the page asked for, and the browser dropped it, so a shopper
clicked *Add to cart* and watched nothing happen. `partials/document.gohtml` therefore
configures `responseHandling` to swap errors too, in the one `<head>` both layouts use.

That makes one rule load-bearing, and it is worth knowing before adding a handler: **an
error response either fills the target it was aimed at, or replaces the document.** A
refusal that belongs in its target renders a fragment; a whole error page sends
`HX-Retarget: body` so it is not pasted into the cart-count span. `HX-Refresh` is the
third option, used where a reload is both the explanation and the fix — an expired CSRF
token, or an admin session that has ended.

## Logging

**JSON to stdout, and nothing else.** No log file, no log table, no agent. That is the
interface every container platform already reads: `docker compose logs`, `make logs`, or
Cloud Logging on Cloud Run with no configuration at all.

Errors are deliberately **not** written to the database. It is the most likely thing to
be broken during an incident, so a logger that needs it fails exactly when it is wanted;
an error storm becomes a write storm; and retention becomes somebody's job. What the
schema does keep is durable *facts* that need acting on — `orders.oversold`,
`orders.gateway_payload` — which is a different thing from diagnostics.

`LOG_FORMAT=gcp` renames `level` and `msg` to `severity` and `message`. On Google Cloud
that is the difference between a working `severity>=ERROR` filter and one that silently
matches nothing, because every line files under `DEFAULT` without it. It is opt-in
because the convention is Google's, and the default output is unchanged.

What is deliberately not here yet: metrics, tracing, and alerting. The volume worth
watching is small — a healthy store logs a dozen lines at boot, one a day for the cart
sweep, and then only per-event lines — and the `ERROR` stream is already high-signal:
an unrecorded payment, an oversold order, a receipt that did not send.

## Deploying

The binary is static and the image is distroless, so it runs anywhere a container does.
Four things are all that separate "runs on a managed platform" from "runs on a VM behind
a reverse proxy", and the server does all four: it reads `PORT` from the environment,
takes a single `DATABASE_URL`, logs JSON to stdout, and serves `GET /healthz`.

Migrations run on boot, before the server accepts traffic, guarded by a Postgres advisory
lock so several instances starting at once cannot race. Where you would rather migrate as
its own deploy step, run the same image with `-migrate` first and start the server after
it exits; `-migrate-status` prints what has been applied.

`-migrate` and `-migrate-status` read `DATABASE_URL` and nothing else. A schema change has
no payment gateway and no session, so a migration job — a CI step, an init container, a
release command — should not have to be trusted with the live merchant key and the session
secret in order to run an `ALTER TABLE`. Give that step the database URL alone.

The cost of that is real and worth knowing: a deployment whose payment or admin config is
broken no longer finds out when migrations run, but when the server starts, with the schema
already moved. `-check-config` is that check made deliberate — it validates the whole
environment, touches nothing, and exits. Run it *before* `-migrate` in a deploy and a
missing `PAYFAST_MERCHANT_KEY` fails while the database is still untouched:

```sh
gostore -check-config && gostore -migrate && exec gostore
```

Terraform for a Google Cloud deployment (Cloud Run, private-IP Cloud SQL, Secret
Manager, Artifact Registry) lives in [`infra/terraform`](infra/terraform/README.md).

## Development

```sh
make test          # TEST_DATABASE_URL defaults to the compose database
go test ./...      # database-backed tests skip when TEST_DATABASE_URL is unset
```

Database tests create a dedicated schema per test and drop it on cleanup, so they never
interfere with each other or with development data.

### The store layer

Queries live in `internal/db/queries/*.sql`; [sqlc](https://sqlc.dev) reads them **against the
migrations** and generates `internal/db/gen`. The stores in `internal/{catalog,cart,orders}`
call the generated methods and map rows onto the domain types.

```
internal/db/migrations/*.sql   the schema (goose runs these; sqlc reads them)
internal/db/queries/*.sql      the queries, annotated with -- name: X :one
internal/db/gen/               generated. Do not edit; `make sqlc` rewrites it
internal/{catalog,cart,orders} the stores: mapping, error translation, transactions
```

**Add or change a query:** edit the `.sql` file, run `make sqlc`, then use the new method.
CI runs `sqlc diff` and fails if the checked-in generated code is stale, so a query edited
without regenerating cannot reach `main` as code that quietly runs the old statement.

What this buys, and it is one specific thing: **a column can no longer land in the wrong
field.** The mapping is by name, checked at compile time, instead of a positional `Scan`
against a hand-maintained column list. `orders` has seven consecutive `TEXT` columns —
`customer_name`, `customer_email`, `customer_phone`, `shipping_address` and three
`gateway_*` — where a reordering used to compile, run, and file the phone number as the
address.

What it does not buy: the interesting parts are still hand-written, and deliberately so.
Transactions (`orders.MarkPaid`, `catalog.Upsert`) are orchestrated in Go, because the
control flow — lock, check, loop, decide — is the logic. Error translation is hand-written,
because turning `23505` into "that slug is taken, and here is the field to highlight" is
domain knowledge no generator has.

Three things worth knowing before touching `sqlc.yaml`:

- **`uuid` is overridden to `string`**, so ids are one type everywhere: in a URL path, in a
  form field, in a template. The cost is that a malformed id reaches Postgres and returns
  error `22P02`, which each store's `translate()` maps to `ErrNotFound`. That replaced a
  hand-written `isUUID` check.
- **`SELECT *` is deliberate** on single-table queries. sqlc expands it against the real
  schema at generation time, so the column list cannot drift from the table.
- **A cast can be load-bearing.** `(v.active AND p.active)::bool` looks redundant — neither
  column is nullable — but sqlc cannot prove that and would otherwise hand the store a
  `*bool`, implying a third state that does not exist.

### Migrations

The migrations are also sqlc's idea of the schema, so a migration and the generated code
change together: add a column, run `make sqlc`.

Numbered `.sql` files in `internal/db/migrations`, managed by
[goose](https://github.com/pressly/goose), embedded into the binary and applied in one
transaction each.

To add one, create `NNNN_name.sql` with a version above every existing file and a
`-- +goose Up` section:

```sql
-- +goose Up
ALTER TABLE products ADD COLUMN subtitle TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE products DROP COLUMN subtitle;
```

Four rules, each with teeth:

- **Never edit a migration that has been applied anywhere.** goose records versions, not
  checksums, so an edited file is silently skipped and the schema quietly diverges from
  the one in the repository. Add a new migration instead.
- **Number above every existing file.** Out-of-order migrations are rejected, because a
  file numbered below one already applied would produce a different schema depending on
  when you first ran it — unacceptable in a project other people run against their data.
- **Down sections are for local development.** Production is forward-only: rolling a
  schema back over live orders loses money, not just columns.
- **Create extensions in a named schema.** `CREATE EXTENSION IF NOT EXISTS pg_trgm SCHEMA
  public`, never the bare form. An extension's *objects* are schema-scoped but its *name* is
  database-global, so the bare statement installs into whatever the `search_path` leads with
  and then does nothing on the next database whose path leads elsewhere — where the operators
  are simply missing. The database-backed tests are exactly that case: each one runs in its own
  schema, so the bare form lands in whichever test happened to run first and disappears with
  it. The symptom is `operator does not exist: text <% text` in tests that pass individually
  and fail as a suite.

Statements that cannot run inside a transaction — `CREATE INDEX CONCURRENTLY`, most
notably — need `-- +goose NO TRANSACTION` at the top of the file.

The first rule has been broken exactly once, deliberately: `0001_init.sql` was rewritten and
four follow-on migrations folded into it, before the project was published and while its only
database was a development one. If you are reading this in a released version, that window is
closed — the rule is absolute now, and a database from before the rewrite is recreated with
`make down ARGS=-v && make up && make seed`.

The files are ordinary goose migrations, so the `goose` CLI works against this directory
unchanged when a migration needs to be inspected or applied by hand.

## Build order

1. **Skeleton** — config, migration runner, compose, Dockerfile, `/healthz`, CI ← *done*
2. **Catalog** — products and variants, seed command, admin CRUD ← *done*
3. **Admin auth** — `RequireAdmin`, `cmd/hashpw`; the signed cookie it started with was replaced in 11.6 ← *done*
4. **Storefront reads** — `/products` pages and fragments, vendored htmx, CORS ← *done*
5. **Cart** — cookie-keyed server-side cart, add/update/remove ← *done*
6. **Checkout + PayFast** — orders, signature, ITN validation ← *done*
7. **Order emails + admin orders** — go-mail, receipts, `/admin/orders` ← *done*
8. **Images** — `blob` package, admin upload to R2/GCS/MinIO ← *done*
9. **Hardening** — rate limits, argon2id, CSP review, oversell flagging, cart cleanup ← *done*
10. **Categories** — schema reset, `categories` + join table, `kind` retired, CRUD ← *done*
11. **Search and filtering** — full-text plus trigram, category filters, pagination, images ← *done*
11.5. **Administrator accounts** — `admin_users`, roles, the last-owner guards ← *done*
11.6. **Sessions and setup** — `admin_sessions`, the `/admin/setup` claim, no credential in the environment ← *done*
11.7. **Authorization** — a permission named on every route registration, `must_change_password` ← *done*
11.8. **Account management** — `/admin/users`, own-password change ← *done*
12. Publish

## Licence

[MIT](LICENSE).

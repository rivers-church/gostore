-- +goose Up

-- Trigram matching for the catalog search. In `public` explicitly, not into
-- whatever search_path happens to lead with: extension objects are schema-scoped
-- but extension *names* are database-global, so a bare CREATE EXTENSION lands in
-- the first schema to run it and is invisible from every other one. That failure
-- is order-dependent, which makes it a test suite that passes one at a time and
-- fails together.
CREATE EXTENSION IF NOT EXISTS pg_trgm SCHEMA public;

CREATE TABLE products (
    id          UUID PRIMARY KEY,
    slug        TEXT NOT NULL UNIQUE,
    title       TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    -- image_key names the object in blob storage that this product's image *is*.
    -- The URL is not stored: it is this key resolved against whichever backend is
    -- configured, at render time. Storing it as well would bake one deployment's
    -- answer into every row, so moving a store from a local directory to a bucket —
    -- or putting a custom domain in front of one — would need an UPDATE across the
    -- table before any image loaded again.
    image_key   TEXT NOT NULL DEFAULT '',
    -- How this product reaches the buyer: a parcel, or a file they download.
    --
    -- 'kind' held 'book'/'apparel' in an earlier schema and became `categories`;
    -- the CHECK is what makes the new meaning unambiguous. It is not called
    -- `fulfillment` because in Shopify, Magento and Saleor a "fulfillment" is the
    -- shipment record with a tracking number, and colliding with that term would
    -- bite the first time this store grows anything shipment-shaped. The enum
    -- shape follows Magento's type_id and BigCommerce's type; a boolean
    -- (`requires_shipping`) is what WooCommerce grew into two interacting flags
    -- nobody can remember.
    --
    -- Read by: stock decrementing (a download cannot run out), the checkout's
    -- shipping address, the admin product form, and whether payment mints
    -- entitlements.
    kind        TEXT NOT NULL DEFAULT 'physical'
                CHECK (kind IN ('physical', 'digital')),
    -- The NAMES of this product's variant options; the values live on the variants.
    -- Declared per product, which is what Shopify and WooCommerce do, so a t-shirt
    -- says 'Size'/'Colour', a book says 'Cover' and a recording says 'Format'.
    --
    -- Deliberately NOT hung off the category. Categories here are many-to-many
    -- precisely so a book can also be a gift, and option structure riding on them
    -- would give that product two contradictory answers — while adding a 'Sale'
    -- category for a promotion would change what a variant *is*. Magento, Saleor
    -- and Sylius all have a reusable template for this and all keep it separate
    -- from the browsing taxonomy for the same reason.
    --
    -- Empty means the slot is unused. Slots fill in order: a name in slot 2 with
    -- slot 1 empty is refused by validate.ProductOptions, not by the schema, so the
    -- admin gets a message on the field rather than a constraint violation.
    option1_name TEXT NOT NULL DEFAULT '',
    option2_name TEXT NOT NULL DEFAULT '',
    option3_name TEXT NOT NULL DEFAULT '',
    active      BOOLEAN NOT NULL DEFAULT TRUE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Generated rather than maintained by a trigger, so it cannot fall out of step
    -- with the row. to_tsvector's two-argument form is load-bearing: with a literal
    -- config it is immutable, which is what makes GENERATED ... STORED legal at all.
    -- setweight puts a title match above the same word buried in a description.
    search      tsvector GENERATED ALWAYS AS (
                    setweight(to_tsvector('english', title), 'A') ||
                    setweight(to_tsvector('english', description), 'B')
                ) STORED
);
-- Full-text finds words, trigram finds spellings: websearch_to_tsquery stems, so
-- "running" reaches "run", but it dies on a typo; trigram survives the typo and has
-- no idea the two words are related. Each covers the other's blind spot, which is
-- why both indexes are here rather than one.
CREATE INDEX products_search_idx     ON products USING GIN (search);
CREATE INDEX products_title_trgm_idx ON products USING GIN (title gin_trgm_ops);

-- A category is a row, not a string on the product. slug is the public URL
-- parameter; position is the display order, because ordering by name would put
-- "Apparel" ahead of "Books" for ever and a shop owner wants their own order.
CREATE TABLE categories (
    id       UUID PRIMARY KEY,
    slug     TEXT NOT NULL UNIQUE,
    name     TEXT NOT NULL,
    position INTEGER NOT NULL DEFAULT 0
);

-- A join table rather than a column on products: a book that is also a gift belongs
-- in both, and forcing that choice on a shop owner is a decision the store should
-- not make for them. Cascades unlink a deleted category from its products; it never
-- deletes the products themselves.
CREATE TABLE product_categories (
    product_id  UUID NOT NULL REFERENCES products(id) ON DELETE CASCADE,
    category_id UUID NOT NULL REFERENCES categories(id) ON DELETE CASCADE,
    PRIMARY KEY (product_id, category_id)
);
CREATE INDEX ON product_categories (category_id);

-- The variant is the purchasable, priced, stocked unit. A single-edition book
-- still gets exactly one row (every option ''), so cart/order/stock logic
-- never branches on "has options vs not" — one code path everywhere.
--
-- option1..3 are the VALUES for the names declared on the product: 'L'/'Navy' for
-- apparel, 'Hardcover' for a book, 'Audio' for a recording. Three slots because
-- that is where Shopify landed after a decade, and because a fourth column is free
-- before publication and a migration after it.
CREATE TABLE product_variants (
    id          UUID PRIMARY KEY,
    product_id  UUID NOT NULL REFERENCES products(id) ON DELETE CASCADE,
    sku         TEXT NOT NULL UNIQUE,
    option1     TEXT NOT NULL DEFAULT '',
    option2     TEXT NOT NULL DEFAULT '',
    option3     TEXT NOT NULL DEFAULT '',
    price_cents BIGINT NOT NULL CHECK (price_cents >= 0),
    stock_qty   INTEGER NOT NULL DEFAULT 0 CHECK (stock_qty >= 0),
    active      BOOLEAN NOT NULL DEFAULT TRUE,
    -- Named rather than left to Postgres, because catalog.translate() matches on
    -- the constraint name to turn a violation into "options" on the admin form.
    -- An auto-generated name would change the moment a column is added.
    CONSTRAINT product_variants_options_key UNIQUE (product_id, option1, option2, option3)
);
CREATE INDEX ON product_variants (product_id);

-- Anonymous carts, keyed by an opaque random token that is also the cookie value.
CREATE TABLE carts (
    id         TEXT PRIMARY KEY,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- The cleanup job deletes carts by age.
CREATE INDEX ON carts (updated_at);

-- The variant reference cascades here and deliberately restricts in order_items.
-- The two tables look alike and want opposite behaviour: a cart is ephemeral, so a
-- variant sitting in somebody's abandoned cart must not stop the shop owner from
-- deleting it. An order is a record of what was actually bought, and deleting a
-- variant must never quietly rewrite purchase history.
CREATE TABLE cart_items (
    id         BIGSERIAL PRIMARY KEY,
    cart_id    TEXT NOT NULL REFERENCES carts(id) ON DELETE CASCADE,
    variant_id UUID NOT NULL REFERENCES product_variants(id) ON DELETE CASCADE,
    quantity   INTEGER NOT NULL CHECK (quantity > 0),
    UNIQUE (cart_id, variant_id)
);

CREATE TABLE orders (
    id                UUID PRIMARY KEY,           -- sent to the gateway as its reference
    cart_id           TEXT REFERENCES carts(id) ON DELETE SET NULL,
    customer_name     TEXT NOT NULL,
    customer_email    TEXT NOT NULL,
    customer_phone    TEXT NOT NULL DEFAULT '',
    shipping_address  TEXT NOT NULL DEFAULT '',
    total_cents       BIGINT NOT NULL,
    currency          TEXT NOT NULL,              -- from config; ZAR for PayFast
    status            TEXT NOT NULL DEFAULT 'pending',  -- pending|paid|failed|cancelled
    -- gateway-agnostic payment columns, so a second gateway needs no migration
    gateway           TEXT NOT NULL DEFAULT '',   -- 'payfast'
    gateway_ref       TEXT,                       -- e.g. PayFast pf_payment_id
    gateway_status    TEXT NOT NULL DEFAULT '',   -- e.g. 'COMPLETE'
    gateway_amount    TEXT NOT NULL DEFAULT '',   -- as received, for audit
    gateway_payload   TEXT NOT NULL DEFAULT '',   -- raw callback body, for disputes
    emailed           BOOLEAN NOT NULL DEFAULT FALSE,
    -- A paid order whose stock could not be decremented, because there was not
    -- enough left by the time the payment arrived. Stock is taken at payment, not
    -- reserved at checkout, so two shoppers can both reach a payment page for the
    -- last item and both pay. The second order is still recorded paid — the money has
    -- been taken, and refusing to record it would lose the sale *and* still be
    -- oversold — and this flag is how a human finds out. It has to live in the
    -- schema, not only in a log and a notification email: an email is read once and a
    -- log is not read at all.
    oversold          BOOLEAN NOT NULL DEFAULT FALSE,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    paid_at           TIMESTAMPTZ
);
CREATE INDEX ON orders (status);
CREATE UNIQUE INDEX ON orders (gateway, gateway_ref) WHERE gateway_ref IS NOT NULL;
-- Partial, because the only interesting query is "which orders need attention" and
-- almost none of them do.
CREATE INDEX ON orders (created_at DESC) WHERE oversold;

CREATE TABLE order_items (
    id               BIGSERIAL PRIMARY KEY,
    order_id         UUID NOT NULL REFERENCES orders(id) ON DELETE CASCADE,
    variant_id       UUID NOT NULL REFERENCES product_variants(id),
    -- snapshots: later catalog edits must never rewrite purchase history
    title            TEXT NOT NULL,
    -- The variant's options as they read at purchase — 'L / Navy', 'Hardcover' —
    -- rather than the three values in their own columns. This snapshot is used for
    -- display only, which is all size/color ever were here. Structured reporting
    -- ("how many hardcovers sold") is still reachable, because variant_id
    -- deliberately RESTRICTS rather than cascades, so the variant row is guaranteed
    -- to still exist to join against.
    variant_label    TEXT NOT NULL DEFAULT '',
    -- Snapshotted for the same reason as the title: a product flipped from
    -- physical to digital afterwards must not relabel how a completed sale was
    -- fulfilled. It is also what MarkPaid reads to decide whether to decrement
    -- stock, so a line's behaviour is fixed at the moment it was bought.
    kind             TEXT NOT NULL DEFAULT 'physical',
    unit_price_cents BIGINT NOT NULL,
    quantity         INTEGER NOT NULL CHECK (quantity > 0)
);
CREATE INDEX ON order_items (order_id);

-- The files a digital product is made of. They hang off the PRODUCT rather than
-- a variant, with variant_files below saying which variants include each one:
-- a conference recording sold as an audio set and a video set has two disjoint
-- sets, but an "Audio + Video" bundle variant must not mean uploading the same
-- 2 GB file a second time.
--
-- object_key names an object in the PRIVATE download store — never the public
-- image bucket, where anything is one URL guess away from everybody. As with
-- image_key, only the key is stored and the URL is resolved against whichever
-- backend is configured, at download time.
CREATE TABLE product_files (
    id                BIGSERIAL PRIMARY KEY,
    product_id        UUID NOT NULL REFERENCES products(id) ON DELETE CASCADE,
    position          INTEGER NOT NULL DEFAULT 0,
    -- What the buyer sees. Defaults to the uploaded filename, but a shop owner
    -- should be able to say "Session 1 — Opening" rather than "REC_0042.mp4".
    title             TEXT NOT NULL,
    object_key        TEXT NOT NULL,
    -- Used for Content-Disposition so the file saves under a sensible name. It is
    -- never used to build a key: a filename is client-controlled, and the key is
    -- this store's to choose.
    original_filename TEXT NOT NULL,
    content_type      TEXT NOT NULL,
    size_bytes        BIGINT NOT NULL CHECK (size_bytes >= 0),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX ON product_files (product_id, position);

CREATE TABLE variant_files (
    variant_id UUID   NOT NULL REFERENCES product_variants(id) ON DELETE CASCADE,
    file_id    BIGINT NOT NULL REFERENCES product_files(id)    ON DELETE CASCADE,
    PRIMARY KEY (variant_id, file_id)
);
CREATE INDEX ON variant_files (file_id);

-- One row per purchased digital line, and the whole point of the feature: this
-- is what makes person A's download revocable without touching person B's.
--
-- token_hash is SHA-256 of the token, never the token. The URL is a bearer
-- credential to paid goods — there is no account and no login — so a database
-- leak must not also be a licence to download the catalogue. Lookup is by hash,
-- so the index still does its job.
CREATE TABLE entitlements (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    order_id      UUID   NOT NULL REFERENCES orders(id) ON DELETE CASCADE,
    order_item_id BIGINT NOT NULL REFERENCES order_items(id) ON DELETE CASCADE,
    variant_id    UUID   NOT NULL REFERENCES product_variants(id),
    token_hash    BYTEA  NOT NULL UNIQUE,
    -- Unlimited and never expiring by default: a buyer who lost a file on a new
    -- phone should not have to email the shop. Revoking is the one lever, and it
    -- is per-buyer by construction.
    revoked_at    TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX ON entitlements (order_id);

-- One row per authorised download click. Recorded by the store rather than read
-- back from the bucket, for two reasons that are not about API limitations:
-- neither GCS nor R2 exposes per-object read counts at all, and a presigned URL
-- is anonymous to the bucket — it has no idea which buyer, order or entitlement,
-- and that mapping exists only here.
--
-- What this counts is the authorisation, not the completed transfer. A
-- connection that drops at 80% is one row. Counting real bytes would mean
-- proxying every video through Go, which is the cost the redirect exists to
-- avoid, and the admin says so rather than implying otherwise.
CREATE TABLE download_events (
    id             BIGSERIAL PRIMARY KEY,
    entitlement_id UUID   NOT NULL REFERENCES entitlements(id) ON DELETE CASCADE,
    file_id        BIGINT NOT NULL REFERENCES product_files(id) ON DELETE CASCADE,
    ip             TEXT NOT NULL DEFAULT '',
    user_agent     TEXT NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX ON download_events (file_id, created_at DESC);
CREATE INDEX ON download_events (entitlement_id);

-- Named administrator accounts, replacing the single ADMIN_PASSWORD_HASH the
-- server used to boot with. The trigger for this was the one the original design
-- documented: a second operator with different permissions, and the ability to
-- revoke one session without signing everybody out.
CREATE TABLE admin_users (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    email         TEXT NOT NULL,
    name          TEXT NOT NULL DEFAULT '',
    -- An argon2id PHC string, or a bcrypt hash from an older deployment. Never a
    -- password: see internal/auth/password.go.
    password_hash TEXT NOT NULL,
    -- The four roles, following Stripe's dashboard split reduced to the surface
    -- this store actually has. 'owner' and 'admin' are identical in capability;
    -- 'owner' exists so that "the account that must not be locked out" is a
    -- visible property of the row rather than something emergent from a count.
    --
    -- TEXT with a CHECK rather than an enum type: a fifth role is then an
    -- ordinary migration instead of ALTER TYPE, and the Go side owns the
    -- enumeration either way (internal/auth/model.go).
    role          TEXT NOT NULL
                  CHECK (role IN ('owner', 'admin', 'manager', 'viewer')),
    -- Accounts are disabled, never deleted. A removed row would erase who did
    -- what, and orders and entitlements are records of things that happened.
    disabled      BOOLEAN NOT NULL DEFAULT FALSE,
    -- Set when an administrator resets someone else's password, so the next
    -- sign-in is forced through the change form before anything else opens.
    -- Never set when somebody sets their own.
    must_change_password BOOLEAN NOT NULL DEFAULT FALSE,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- NULL means "never signed in", which is a different fact from the zero time
    -- and is rendered differently in the admin list.
    last_login_at TIMESTAMPTZ
);
-- Case-insensitive identity, enforced by the index rather than by remembering to
-- lower() at every call site. Addresses are also normalised to their addr-spec
-- before they reach here (validate.NormalizeEmail), because `Alex <a@b.com>`
-- would *not* collide with `a@b.com` under this index and would become a second,
-- permanently unusable account for one mailbox.
CREATE UNIQUE INDEX admin_users_email_key ON admin_users (lower(email));

-- A session is a row, not a signed cookie. The signed cookie it replaces could
-- not support any of disabling an account, ending sessions on a password change,
-- or revoking one login — each of which needs a lookup per request anyway, so the
-- table is the mechanism rather than the cost.
CREATE TABLE admin_sessions (
    -- Only the sha256 of the token the browser holds. There is nothing to
    -- brute-force in 32 bytes of uniform randomness, so a slow KDF here would buy
    -- nothing and be paid on every request.
    token_hash BYTEA PRIMARY KEY,
    user_id    UUID NOT NULL REFERENCES admin_users(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL
);
-- For "end every session belonging to this user", which runs on a password
-- change, a disable and a role change.
CREATE INDEX admin_sessions_user_idx ON admin_sessions (user_id);
-- Expiry is enforced on read; this index is for the sweep that stops the table
-- growing without bound.
CREATE INDEX admin_sessions_expires_idx ON admin_sessions (expires_at);

-- One row, holding the hash of the token that may claim the first account.
--
-- The store boots with no administrator at all rather than a default password: a
-- fixed default credential is a well-worn way to hand over an instance that was
-- reachable for the few minutes before someone signed in, and this is a project
-- published as an example others copy. The token closes the claim race that an
-- unguarded first-run wizard leaves open.
CREATE TABLE admin_setup (
    -- Single-row table: the primary key can only ever hold TRUE, so a second
    -- INSERT conflicts rather than creating a second live token.
    id          BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (id),
    token_hash  BYTEA NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Non-NULL once the token has been spent. It is never cleared, so setup locks
    -- permanently and survives a restart.
    consumed_at TIMESTAMPTZ
);

-- +goose Down
-- Down exists for local development resets only. Production is forward-only:
-- rolling a schema back over live orders loses money, not just columns.
--
-- pg_trgm is deliberately not dropped. An extension is database-wide and may have
-- other users, so tearing one schema down must not remove it from under them.
DROP TABLE admin_setup;
DROP TABLE admin_sessions;
DROP TABLE admin_users;
DROP TABLE download_events;
DROP TABLE entitlements;
DROP TABLE variant_files;
DROP TABLE product_files;
DROP TABLE order_items;
DROP TABLE orders;
DROP TABLE cart_items;
DROP TABLE carts;
DROP TABLE product_variants;
DROP TABLE product_categories;
DROP TABLE categories;
DROP TABLE products;

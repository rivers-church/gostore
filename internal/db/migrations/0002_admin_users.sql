-- +goose Up

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
-- Down exists for local development resets only. Production is forward-only.
DROP TABLE admin_setup;
DROP TABLE admin_sessions;
DROP TABLE admin_users;

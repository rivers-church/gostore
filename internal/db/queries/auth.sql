-- Queries behind internal/auth. See sqlc.yaml; regenerate with `make sqlc`.
--
-- `SELECT *` is deliberate on the single-table queries: sqlc expands it against
-- the real schema at generation time, so the column list in the generated code
-- cannot drift from the table the way a hand-maintained one could.

-- name: CountAdminUsers :one
-- The boot check behind the setup flow: zero means nobody has claimed the store.
SELECT count(*) FROM admin_users;

-- name: ListAdminUsers :many
SELECT * FROM admin_users ORDER BY lower(email);

-- name: GetAdminUser :one
SELECT * FROM admin_users WHERE id = $1;

-- Case-insensitively, matching the unique index, so an address typed with a
-- capital still finds the account it belongs to.
-- name: GetAdminUserByEmail :one
SELECT * FROM admin_users WHERE lower(email) = lower(@email::text);

-- The insert *is* the duplicate check. A find-then-insert would be two round
-- trips with a race between them, and the unique index has to be consulted
-- anyway; ON CONFLICT DO NOTHING turns "the address is taken" into an empty
-- result rather than a driver error to match on by message text.
--
-- The database generates the id, which is why no UUID library reaches the binary.
-- name: CreateAdminUser :one
INSERT INTO admin_users (email, name, password_hash, role, must_change_password)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (lower(email)) DO NOTHING
RETURNING *;

-- name: SetAdminUserPassword :execrows
UPDATE admin_users
SET password_hash = @password_hash, must_change_password = @must_change_password
WHERE id = @id;

-- Recorded on a successful sign-in. A failure here never fails the login: the
-- operator is who they said they were, and bookkeeping is not worth turning that
-- into a refusal.
-- name: TouchAdminUserLogin :exec
UPDATE admin_users SET last_login_at = now() WHERE id = $1;

-- Serialises the two owner-count guards below against each other.
--
-- The count subquery in those guards is NOT sufficient on its own, and the
-- earlier comment here claiming it was is wrong — it was carried over from a
-- SQLite codebase, where every write serialises anyway. Postgres under READ
-- COMMITTED evaluates the subquery against each transaction's own snapshot, and
-- the row lock only covers the row being updated. Two transactions disabling
-- *different* owners therefore never block each other, both count two, and both
-- commit — leaving zero enabled owners, the exact state the guard exists to
-- prevent. That is reproducible in a psql session, not theoretical.
--
-- A transaction-scoped advisory lock is the fix rather than SERIALIZABLE, which
-- would need retry logic on serialisation failure at every call site, or
-- SELECT ... FOR UPDATE over the owner rows, which needs a consistent lock order
-- to avoid deadlocking. These operations happen a handful of times in a store's
-- life, so serialising them globally costs nothing. The same reasoning and shape
-- as db.go's migration lock.
-- name: LockOwnerGuard :exec
SELECT pg_advisory_xact_lock($1);

-- Disabling the last enabled owner would leave an admin area nobody can sign in
-- to, repairable only with shell access. The count below is the guard; holding
-- LockOwnerGuard for the transaction is what makes the count trustworthy.
--
-- Four ways past the guard, in the order they are cheapest to think about:
-- enabling is never dangerous; an already-disabled row is a no-op, so a
-- double-submit reports success rather than a refusal; a non-owner is not the
-- protected case at all; and otherwise there must be another enabled owner left
-- behind. RowsAffected() = 0 is the refusal.
-- name: SetAdminUserDisabledUnlessLastOwner :execrows
UPDATE admin_users u SET disabled = @disabled
WHERE u.id = @id
  AND (@disabled::boolean = FALSE
       OR u.disabled = TRUE
       OR u.role <> 'owner'
       OR (SELECT count(*) FROM admin_users o WHERE o.role = 'owner' AND NOT o.disabled) > 1);

-- The same guard, for the other way of removing the last owner. Promoting *to*
-- owner never reduces the count; neither does demoting an owner who is already
-- disabled, since they were not among the enabled ones being counted.
--
-- This takes LockOwnerGuard too, and must: a disable of owner X racing a demotion
-- of owner Y would otherwise pass both guards independently and still leave zero.
-- name: SetAdminUserRoleUnlessLastOwner :execrows
UPDATE admin_users u SET role = @role
WHERE u.id = @id
  AND (@role::text = 'owner'
       OR u.role <> 'owner'
       OR u.disabled = TRUE
       OR (SELECT count(*) FROM admin_users o WHERE o.role = 'owner' AND NOT o.disabled) > 1);

-- name: CreateAdminSession :exec
INSERT INTO admin_sessions (token_hash, user_id, expires_at)
VALUES ($1, $2, $3);

-- Joined to the user because every caller needs both, and because the user's
-- current `disabled` is what makes a session revocable at all: a signed cookie
-- would still say "signed in" long after the account was switched off.
--
-- Expiry is enforced HERE, in the predicate, rather than left to the caller to
-- remember to compare ExpiresAt after the fact. The sweep that deletes expired
-- rows is housekeeping against unbounded growth and is explicitly not what makes
-- this correct — between two sweeps the table is full of expired sessions, and a
-- lookup that returned them would authenticate every one.
-- name: GetAdminSession :one
SELECT sqlc.embed(s), sqlc.embed(u)
FROM admin_sessions s
JOIN admin_users u ON u.id = s.user_id
WHERE s.token_hash = $1
  AND s.expires_at > now();

-- name: DeleteAdminSession :exec
DELETE FROM admin_sessions WHERE token_hash = $1;

-- Runs inside the same transaction as a password change, so there is no window
-- in which the old password's sessions outlive it.
-- name: DeleteAdminSessionsForUser :execrows
DELETE FROM admin_sessions WHERE user_id = $1;

-- Housekeeping only: expiry is enforced on read, so this exists to stop the table
-- growing without bound rather than to make anything correct.
-- name: DeleteExpiredAdminSessions :execrows
DELETE FROM admin_sessions WHERE expires_at < now();

-- name: CreateSetupToken :execrows
INSERT INTO admin_setup (id, token_hash) VALUES (TRUE, $1)
ON CONFLICT (id) DO NOTHING;

-- name: GetSetupToken :one
SELECT * FROM admin_setup WHERE id = TRUE;

-- Whether the store is still claimable, by exactly the conditions ConsumeSetupToken
-- enforces. Asking "is consumed_at NULL" alone is not the same question: if a first
-- account ever arrives by some path other than the claim flow — a seed command, a
-- manual INSERT, a recovery CLI — the token stays unconsumed for ever, and a setup
-- page driven off consumed_at would go on offering a claim whose every submission
-- fails. Keeping the two in one place is what stops them disagreeing.
-- name: SetupPending :one
SELECT EXISTS (
    SELECT 1 FROM admin_setup
    WHERE id = TRUE
      AND consumed_at IS NULL
      AND NOT EXISTS (SELECT 1 FROM admin_users)
);

-- Consuming is conditional on not already being consumed, so two simultaneous
-- claims cannot both spend the same token — the loser affects no rows. The
-- NOT EXISTS is the same guard from the other side: it closes the window between
-- one request creating the first account and another checking whether any exist.
-- name: ConsumeSetupToken :execrows
UPDATE admin_setup SET consumed_at = now()
WHERE id = TRUE
  AND consumed_at IS NULL
  AND NOT EXISTS (SELECT 1 FROM admin_users);

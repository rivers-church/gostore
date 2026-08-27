package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/17xande-dev/gostore/internal/db/gen"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned for an account or session that does not exist, so
// handlers can answer 404 without inspecting driver errors.
var ErrNotFound = errors.New("auth: not found")

// ErrBadCredentials is the single answer to every failed sign-in — unknown
// address, wrong password, disabled account. They are deliberately
// indistinguishable: telling the three apart turns the login form into a way to
// discover which addresses have accounts.
var ErrBadCredentials = errors.New("auth: email or password is not right")

// ErrEmailTaken means an account already exists for that address, case
// insensitively. It exists so a handler can render the message on the email
// field rather than answering 500 or matching on driver text.
var ErrEmailTaken = errors.New("auth: an administrator with that email already exists")

// ErrLastOwner refuses the change that would leave no enabled owner behind: an
// admin area nobody can sign in to, repairable only with database access.
var ErrLastOwner = errors.New("auth: this is the last enabled owner")

// ownerGuardLockID is the key for the transaction advisory lock that serialises
// the last-owner guards. Arbitrary but fixed, and deliberately distinct from
// db.go's migration lock, which is the other advisory lock in this codebase —
// two unrelated operations sharing a key would block each other for no reason.
//
// See LockOwnerGuard in internal/db/queries/auth.sql for why the guards' own
// count subquery is not sufficient without it.
const ownerGuardLockID int64 = 8_675_309_002

// dummyHash makes a sign-in for an address with no account cost what a wrong
// password costs. Without it, the unknown case returns without doing any argon2
// work at all, and the difference is measurable from outside.
//
// It is computed once, on first use rather than at package init, so that every
// binary importing this package does not pay 64 MiB and ~100 ms to start —
// notably the test binaries, which import it constantly. The one request that
// triggers the computation is slower than its peers, which is the harmless
// direction: it makes an unknown address look *more* expensive, not less.
var dummyHash = sync.OnceValue(func() string {
	h, err := HashPassword("not a real password", DefaultParams)
	if err != nil {
		// Unreachable: DefaultParams is a constant this package controls.
		panic("auth: cannot hash the dummy password: " + err.Error())
	}
	return h
})

// Store is the administrator accounts' persistence: the users themselves, their
// live sessions, and the one-shot token that lets the first account be claimed.
//
// The SQL lives in internal/db/queries/auth.sql and the scanning is generated
// from it by sqlc; what remains here is the part that is genuinely this package's
// own — turning the driver's vocabulary into the domain's, and the handful of
// operations that must be one transaction to be correct.
type Store struct {
	pool *pgxpool.Pool
	q    *gen.Queries
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool, q: gen.New(pool)}
}

// Authenticate verifies an address and password, returning the account.
//
// Every failure is ErrBadCredentials. The order of the checks is load-bearing:
// `disabled` is examined *after* the password, because refusing a disabled
// account before verifying would answer faster for a wrong password than a right
// one, making the account's state an oracle for the password being correct.
func (s *Store) Authenticate(ctx context.Context, email, password string) (User, error) {
	row, err := s.q.GetAdminUserByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Spend the same work an existing account would have cost, and
			// discard the answer.
			CheckPassword(dummyHash(), password)
			return User{}, ErrBadCredentials
		}
		return User{}, translate(fmt.Errorf("auth: user by email: %w", err))
	}

	if !CheckPassword(row.PasswordHash, password) {
		return User{}, ErrBadCredentials
	}
	if row.Disabled {
		return User{}, ErrBadCredentials
	}

	u := userOf(row)
	// Bookkeeping, and never a reason to refuse a sign-in that has already
	// succeeded: the operator is who they said they were whether or not this
	// UPDATE lands.
	if err := s.q.TouchAdminUserLogin(ctx, u.ID); err == nil {
		u.LastLoginAt = time.Now()
	}
	return u, nil
}

// CheckPasswordFor verifies a password against one account without recording a
// sign-in, for the "change my password" form that asks for the current one.
//
// It is separate from Authenticate for exactly that reason: a password check is
// not a login, and letting it move last_login_at would make the admin's "last
// signed in" column quietly wrong. A CSRF token proves a request came from our
// form, not that the person at the keyboard owns the account, which is why the
// form asks at all.
func (s *Store) CheckPasswordFor(ctx context.Context, id, password string) (bool, error) {
	row, err := s.q.GetAdminUser(ctx, id)
	if err != nil {
		return false, translate(fmt.Errorf("auth: user: %w", err))
	}
	return CheckPassword(row.PasswordHash, password), nil
}

// Create stores a new administrator. passwordHash is a hash, never a password:
// this package does not decide cost parameters on a caller's behalf.
//
// The duplicate check is the insert itself. A find-then-insert would be two round
// trips with a race between them, and the unique index has to be consulted
// anyway.
func (s *Store) Create(ctx context.Context, email, name, passwordHash string, role Role, mustChange bool) (User, error) {
	if !role.Valid() {
		return User{}, &ErrInvalidRole{Role: role}
	}
	// Validated on the way in, so a malformed hash is refused here rather than
	// becoming an account that can never sign in and nothing to say why.
	if err := ParsePasswordHash(passwordHash); err != nil {
		return User{}, err
	}

	row, err := s.q.CreateAdminUser(ctx, gen.CreateAdminUserParams{
		Email:              email,
		Name:               name,
		PasswordHash:       passwordHash,
		Role:               string(role),
		MustChangePassword: mustChange,
	})
	if err != nil {
		// ON CONFLICT DO NOTHING makes a taken address an empty result rather
		// than a unique violation, so pgx reports "no rows".
		if errors.Is(err, pgx.ErrNoRows) {
			return User{}, ErrEmailTaken
		}
		return User{}, translate(fmt.Errorf("auth: create user: %w", err))
	}
	return userOf(row), nil
}

// Get returns one account by id.
func (s *Store) Get(ctx context.Context, id string) (User, error) {
	row, err := s.q.GetAdminUser(ctx, id)
	if err != nil {
		return User{}, translate(fmt.Errorf("auth: user: %w", err))
	}
	return userOf(row), nil
}

// GetByEmail returns one account by address, case insensitively.
func (s *Store) GetByEmail(ctx context.Context, email string) (User, error) {
	row, err := s.q.GetAdminUserByEmail(ctx, email)
	if err != nil {
		return User{}, translate(fmt.Errorf("auth: user by email: %w", err))
	}
	return userOf(row), nil
}

// List returns every account, disabled ones included, ordered by address. The
// admin list is the caller, and it shows disabled accounts precisely so they can
// be enabled again.
func (s *Store) List(ctx context.Context) ([]User, error) {
	rows, err := s.q.ListAdminUsers(ctx)
	if err != nil {
		return nil, translate(fmt.Errorf("auth: list users: %w", err))
	}
	users := make([]User, 0, len(rows))
	for _, row := range rows {
		users = append(users, userOf(row))
	}
	return users, nil
}

// Count is the boot check behind the setup flow: zero means nobody has claimed
// this store yet.
func (s *Store) Count(ctx context.Context) (int, error) {
	n, err := s.q.CountAdminUsers(ctx)
	if err != nil {
		return 0, translate(fmt.Errorf("auth: count users: %w", err))
	}
	return int(n), nil
}

// SetPassword replaces an account's password and ends every session it has,
// in one transaction.
//
// The two halves must not be separable. A password changed without dropping the
// sessions it authorised leaves whoever prompted the change still signed in,
// which is the one thing changing a password after a scare is meant to fix.
//
// mustChange is set when an administrator resets somebody else's password and
// left false when somebody sets their own.
func (s *Store) SetPassword(ctx context.Context, id, passwordHash string, mustChange bool) error {
	if err := ParsePasswordHash(passwordHash); err != nil {
		return err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("auth: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	q := s.q.WithTx(tx)
	rows, err := q.SetAdminUserPassword(ctx, gen.SetAdminUserPasswordParams{
		ID: id, PasswordHash: passwordHash, MustChangePassword: mustChange,
	})
	if err != nil {
		return translate(fmt.Errorf("auth: set password: %w", err))
	}
	if rows == 0 {
		return ErrNotFound
	}
	if _, err := q.DeleteAdminSessionsForUser(ctx, id); err != nil {
		return translate(fmt.Errorf("auth: end sessions: %w", err))
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("auth: commit: %w", err)
	}
	return nil
}

// SetDisabled switches an account on or off, refusing to disable the last
// enabled owner.
//
// Disabling also ends the account's live sessions, in the same transaction, so
// somebody switched off mid-session stops on their next request rather than
// whenever their cookie happens to expire. Enabling has no sessions to drop.
//
// Disabling an already-disabled account is a no-op that reports success: a
// double-submitted form is not a refusal worth explaining.
func (s *Store) SetDisabled(ctx context.Context, id string, disabled bool) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("auth: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	q := s.q.WithTx(tx)
	if err := q.LockOwnerGuard(ctx, ownerGuardLockID); err != nil {
		return fmt.Errorf("auth: lock owner guard: %w", err)
	}
	rows, err := q.SetAdminUserDisabledUnlessLastOwner(ctx,
		gen.SetAdminUserDisabledUnlessLastOwnerParams{ID: id, Disabled: disabled})
	if err != nil {
		return translate(fmt.Errorf("auth: set disabled: %w", err))
	}
	if rows == 0 {
		return s.refusal(ctx, q, id)
	}
	if disabled {
		if _, err := q.DeleteAdminSessionsForUser(ctx, id); err != nil {
			return translate(fmt.Errorf("auth: end sessions: %w", err))
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("auth: commit: %w", err)
	}
	return nil
}

// SetRole changes an account's role, refusing to demote the last enabled owner.
//
// It ends the account's sessions too. A role is read from the session's user on
// every request, so this is not strictly required for the new role to take
// effect — but a demotion is a privilege change, and making the person sign in
// again is the honest way to mark one.
func (s *Store) SetRole(ctx context.Context, id string, role Role) error {
	if !role.Valid() {
		return &ErrInvalidRole{Role: role}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("auth: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	q := s.q.WithTx(tx)
	if err := q.LockOwnerGuard(ctx, ownerGuardLockID); err != nil {
		return fmt.Errorf("auth: lock owner guard: %w", err)
	}

	// Read the current role inside the transaction, so that "did this actually
	// change anything" is answered against the same snapshot the UPDATE runs in.
	before, err := q.GetAdminUser(ctx, id)
	if err != nil {
		return translate(fmt.Errorf("auth: get user: %w", err))
	}

	rows, err := q.SetAdminUserRoleUnlessLastOwner(ctx,
		gen.SetAdminUserRoleUnlessLastOwnerParams{ID: id, Role: string(role)})
	if err != nil {
		return translate(fmt.Errorf("auth: set role: %w", err))
	}
	if rows == 0 {
		return s.refusal(ctx, q, id)
	}
	// Only when the role really moved. Saving the account form without touching
	// the role select would otherwise sign that administrator out of every
	// device — including, if they are editing themselves, the session they are
	// doing it from.
	if Role(before.Role) != role {
		if _, err := q.DeleteAdminSessionsForUser(ctx, id); err != nil {
			return translate(fmt.Errorf("auth: end sessions: %w", err))
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("auth: commit: %w", err)
	}
	return nil
}

// refusal tells the two reasons a guarded UPDATE can affect no rows apart: the
// account is not there, or the guard turned it down. Neither is inferable from
// the row count alone, and a handler needs to answer 404 for one and explain the
// other.
//
// It takes the caller's transaction-bound querier rather than reaching for
// s.q. Using the pool here would acquire a *second* connection while the
// caller's transaction still holds the first: enough concurrent guarded writes
// to exhaust the pool and every one of them waits for a connection only another
// of them can release, until the context expires. It would also read outside the
// transaction's snapshot, which is the wrong answer to the question being asked.
func (s *Store) refusal(ctx context.Context, q *gen.Queries, id string) error {
	if _, err := q.GetAdminUser(ctx, id); err != nil {
		return translate(fmt.Errorf("auth: get user: %w", err))
	}
	return ErrLastOwner
}

// DeleteSessionsForUser ends every session an account holds. SetPassword,
// SetDisabled and SetRole each do this inside their own transaction; this is the
// standalone form, for a handler that wants to sign somebody out and change
// nothing else.
func (s *Store) DeleteSessionsForUser(ctx context.Context, id string) error {
	if _, err := s.q.DeleteAdminSessionsForUser(ctx, id); err != nil {
		return translate(fmt.Errorf("auth: end sessions: %w", err))
	}
	return nil
}

// CountSessionsForUser is for tests and for an admin page that wants to say how
// many places an account is signed in.
func (s *Store) CountSessionsForUser(ctx context.Context, id string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx,
		"SELECT count(*) FROM admin_sessions WHERE user_id = $1", id).Scan(&n)
	if err != nil {
		return 0, translate(fmt.Errorf("auth: count sessions: %w", err))
	}
	return n, nil
}

// DeleteExpiredSessions is housekeeping, swept periodically. Expiry is enforced
// on read, so this stops the table growing without bound rather than making
// anything correct.
func (s *Store) DeleteExpiredSessions(ctx context.Context) (int64, error) {
	n, err := s.q.DeleteExpiredAdminSessions(ctx)
	if err != nil {
		return 0, translate(fmt.Errorf("auth: sweep sessions: %w", err))
	}
	return n, nil
}

// CreateSetupToken records the hash of the token that may claim the first
// account, and reports whether it stored this one.
//
// False means a token was already there: admin_setup holds a single row, so the
// second caller conflicts rather than replacing a live token with a new one. That
// is what makes the boot path safe to run on every start.
func (s *Store) CreateSetupToken(ctx context.Context, token string) (bool, error) {
	rows, err := s.q.CreateSetupToken(ctx, hashToken(token))
	if err != nil {
		return false, translate(fmt.Errorf("auth: create setup token: %w", err))
	}
	return rows > 0, nil
}

// SetupPending reports whether a setup token exists and has not been spent.
func (s *Store) SetupPending(ctx context.Context) (bool, error) {
	pending, err := s.q.SetupPending(ctx)
	if err != nil {
		return false, translate(fmt.Errorf("auth: setup pending: %w", err))
	}
	return pending, nil
}

// CheckSetupToken reports whether token is the unspent setup token.
//
// The comparison is against a stored hash, so the token itself only ever exists
// in the log line that printed it and in the operator's clipboard.
func (s *Store) CheckSetupToken(ctx context.Context, token string) (bool, error) {
	row, err := s.q.GetSetupToken(ctx)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, translate(fmt.Errorf("auth: setup token: %w", err))
	}
	if row.ConsumedAt != nil {
		return false, nil
	}
	return constantTimeEqual(row.TokenHash, hashToken(token)), nil
}

// ClaimSetup spends the setup token and creates the first account, in one
// transaction.
//
// Both guards are in SQL rather than in a preceding check, because both are
// races: ConsumeSetupToken updates only an unconsumed row that no account exists
// alongside, so of two requests arriving with the same token at the same moment
// exactly one gets a row and the other is refused. A read-then-write would let
// both through and create two owners from one token.
func (s *Store) ClaimSetup(ctx context.Context, token, email, name, passwordHash string) (User, error) {
	if err := ParsePasswordHash(passwordHash); err != nil {
		return User{}, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return User{}, fmt.Errorf("auth: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	q := s.q.WithTx(tx)
	row, err := q.GetSetupToken(ctx)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return User{}, ErrSetupClosed
		}
		return User{}, translate(fmt.Errorf("auth: setup token: %w", err))
	}
	if row.ConsumedAt != nil {
		return User{}, ErrSetupClosed
	}
	if !constantTimeEqual(row.TokenHash, hashToken(token)) {
		return User{}, ErrBadSetupToken
	}

	rows, err := q.ConsumeSetupToken(ctx)
	if err != nil {
		return User{}, translate(fmt.Errorf("auth: consume setup token: %w", err))
	}
	if rows == 0 {
		// Somebody else claimed the store between the read above and here.
		return User{}, ErrSetupClosed
	}

	created, err := q.CreateAdminUser(ctx, gen.CreateAdminUserParams{
		Email: email, Name: name, PasswordHash: passwordHash,
		Role: string(RoleOwner), MustChangePassword: false,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return User{}, ErrEmailTaken
		}
		return User{}, translate(fmt.Errorf("auth: create first user: %w", err))
	}

	if err := tx.Commit(ctx); err != nil {
		return User{}, fmt.Errorf("auth: commit: %w", err)
	}
	return userOf(created), nil
}

// ErrSetupClosed means the store has already been claimed. It is permanent: the
// consumed timestamp is never cleared, so setup does not reopen on a restart or
// if every account is later disabled.
var ErrSetupClosed = errors.New("auth: setup has already been completed")

// ErrBadSetupToken means the token offered does not match the one issued.
var ErrBadSetupToken = errors.New("auth: setup token is not right")

// hashToken is sha256 and deliberately not a KDF. A setup or session token is 32
// bytes of uniform randomness with nothing to guess, so a slow hash would buy no
// resistance and be paid on every request that carries one.
func hashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// constantTimeEqual compares two hashes without an early return. The hashes are
// not secret in the way a key is, but the comparison is against a value derived
// from one, and a length-prefixed early exit is a habit worth not forming.
func constantTimeEqual(a, b []byte) bool {
	return subtle.ConstantTimeCompare(a, b) == 1
}

// userOf maps a row to the domain's view of it. The nullable last_login_at
// becomes the zero time, which is the same fact said in the vocabulary the rest
// of the code already uses for "never".
func userOf(row gen.AdminUser) User {
	u := User{
		ID:                 row.ID,
		Email:              row.Email,
		Name:               row.Name,
		PasswordHash:       row.PasswordHash,
		Role:               Role(row.Role),
		Disabled:           row.Disabled,
		MustChangePassword: row.MustChangePassword,
		CreatedAt:          row.CreatedAt,
	}
	if row.LastLoginAt != nil {
		u.LastLoginAt = *row.LastLoginAt
	}
	return u
}

// translate turns the driver's vocabulary into this package's.
func translate(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok {
		switch pgErr.Code {
		case "22P02": // invalid input syntax, i.e. not a UUID
			return ErrNotFound
		case "23505": // unique violation
			if strings.Contains(pgErr.ConstraintName, "email") {
				return ErrEmailTaken
			}
		}
	}
	return err
}

package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/17xande-dev/gostore/internal/dbtest"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// hash produces a password hash at the cheap parameters, because the real ones
// cost 64 MiB and ~100 ms each and these tests make dozens.
func hash(t *testing.T, password string) string {
	t.Helper()
	h, err := HashPassword(password, cheapParams)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	return h
}

// randomLabel gives each inserted session a distinct token, since token_hash is
// the primary key and two sessions for one user must not collide.
func randomLabel(t *testing.T) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return hex.EncodeToString(b)
}

func newStore(t *testing.T) (*Store, *pgxpool.Pool, context.Context) {
	t.Helper()
	pool := dbtest.Pool(t)
	return NewStore(pool), pool, t.Context()
}

// mustCreate makes an account and fails the test if it cannot, for the setup
// half of tests whose subject is something else.
func mustCreate(t *testing.T, s *Store, ctx context.Context, email, password string, role Role) User {
	t.Helper()
	u, err := s.Create(ctx, email, "Test User", hash(t, password), role, false)
	if err != nil {
		t.Fatalf("Create(%s, %s): %v", email, role, err)
	}
	return u
}

// session inserts a live session directly, so tests about ending sessions do not
// depend on the session-issuing API that arrives with the handler work.
func session(t *testing.T, pool *pgxpool.Pool, ctx context.Context, userID string) {
	t.Helper()
	_, err := pool.Exec(ctx,
		`INSERT INTO admin_sessions (token_hash, user_id, expires_at)
		 VALUES ($1, $2, now() + interval '1 hour')`, hashToken(randomLabel(t)), userID)
	if err != nil {
		t.Fatalf("insert session: %v", err)
	}
}

func TestCreateAndGet(t *testing.T) {
	s, _, ctx := newStore(t)

	created, err := s.Create(ctx, "owner@example.com", "The Owner", hash(t, "correct horse battery"), RoleOwner, false)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.ID == "" {
		t.Fatal("Create returned an empty id")
	}
	if created.Role != RoleOwner {
		t.Errorf("Role = %q, want %q", created.Role, RoleOwner)
	}
	// A brand new account has never signed in, and the admin list renders that
	// as "Never" rather than as a date — so the zero time, not 1970.
	if !created.LastLoginAt.IsZero() {
		t.Errorf("LastLoginAt = %v, want the zero time for an account that has never signed in", created.LastLoginAt)
	}

	got, err := s.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Email != "owner@example.com" || got.Name != "The Owner" || got.Disabled {
		t.Errorf("Get returned %+v", got)
	}

	if _, err := s.Get(ctx, "not-a-uuid"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get(malformed id) = %v, want ErrNotFound", err)
	}
}

func TestCreateRejectsDuplicateEmailCaseInsensitively(t *testing.T) {
	s, _, ctx := newStore(t)
	mustCreate(t, s, ctx, "alex@example.com", "correct horse battery", RoleOwner)

	// The unique index is on lower(email), so a capital does not buy a second
	// account for the same mailbox.
	_, err := s.Create(ctx, "ALEX@example.com", "Impostor", hash(t, "another password"), RoleAdmin, false)
	if !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("Create(differing only in case) = %v, want ErrEmailTaken", err)
	}
}

func TestCreateRejectsBadRoleAndBadHash(t *testing.T) {
	s, _, ctx := newStore(t)

	var invalid *ErrInvalidRole
	_, err := s.Create(ctx, "a@example.com", "", hash(t, "correct horse battery"), Role("superuser"), false)
	if !errors.As(err, &invalid) {
		t.Errorf("Create(unknown role) = %v, want ErrInvalidRole", err)
	}

	// Validated on the way in, so a malformed hash cannot become an account that
	// can never sign in with nothing to say why.
	if _, err := s.Create(ctx, "b@example.com", "", "not-a-hash", RoleAdmin, false); !errors.Is(err, ErrPasswordFormat) {
		t.Errorf("Create(malformed hash) = %v, want ErrPasswordFormat", err)
	}
}

// The three ways a sign-in can fail must be indistinguishable from outside, or
// the login form becomes a way to discover which addresses have accounts.
func TestAuthenticateFailuresAreIndistinguishable(t *testing.T) {
	s, _, ctx := newStore(t)
	const password = "correct horse battery"
	disabled := mustCreate(t, s, ctx, "disabled@example.com", password, RoleAdmin)
	if err := s.SetDisabled(ctx, disabled.ID, true); err != nil {
		t.Fatalf("SetDisabled: %v", err)
	}
	mustCreate(t, s, ctx, "live@example.com", password, RoleAdmin)

	cases := []struct {
		name, email, password string
	}{
		{"unknown address", "nobody@example.com", password},
		{"wrong password", "live@example.com", "not the password"},
		{"disabled account, right password", "disabled@example.com", password},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := s.Authenticate(ctx, c.email, c.password)
			if !errors.Is(err, ErrBadCredentials) {
				t.Errorf("Authenticate = %v, want ErrBadCredentials", err)
			}
		})
	}
}

func TestAuthenticateSucceedsAndRecordsTheLogin(t *testing.T) {
	s, _, ctx := newStore(t)
	const password = "correct horse battery"
	created := mustCreate(t, s, ctx, "Alex@Example.com", password, RoleManager)

	// Case-insensitively, matching the unique index: an address typed with a
	// different capitalisation still finds its account.
	u, err := s.Authenticate(ctx, "alex@example.com", password)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if u.ID != created.ID || u.Role != RoleManager {
		t.Errorf("Authenticate returned %+v", u)
	}

	stored, err := s.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.LastLoginAt.IsZero() {
		t.Error("a successful sign-in did not record last_login_at")
	}
}

// A password check is not a login: letting the "change my password" form move
// last_login_at would make the admin's "last signed in" column quietly wrong.
func TestCheckPasswordForDoesNotRecordALogin(t *testing.T) {
	s, _, ctx := newStore(t)
	const password = "correct horse battery"
	u := mustCreate(t, s, ctx, "alex@example.com", password, RoleAdmin)

	ok, err := s.CheckPasswordFor(ctx, u.ID, password)
	if err != nil || !ok {
		t.Fatalf("CheckPasswordFor = %v, %v; want true, nil", ok, err)
	}
	if bad, _ := s.CheckPasswordFor(ctx, u.ID, "wrong"); bad {
		t.Error("CheckPasswordFor accepted the wrong password")
	}

	stored, _ := s.Get(ctx, u.ID)
	if !stored.LastLoginAt.IsZero() {
		t.Error("CheckPasswordFor recorded a sign-in")
	}
}

// A password changed without dropping the sessions it authorised leaves whoever
// prompted the change still signed in, which is the one thing changing a password
// after a scare is meant to fix.
func TestSetPasswordEndsEverySession(t *testing.T) {
	s, pool, ctx := newStore(t)
	u := mustCreate(t, s, ctx, "alex@example.com", "correct horse battery", RoleAdmin)
	other := mustCreate(t, s, ctx, "other@example.com", "correct horse battery", RoleAdmin)
	session(t, pool, ctx, u.ID)
	session(t, pool, ctx, u.ID)
	session(t, pool, ctx, other.ID)

	if err := s.SetPassword(ctx, u.ID, hash(t, "a different password entirely"), true); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}

	if n, _ := s.CountSessionsForUser(ctx, u.ID); n != 0 {
		t.Errorf("%d sessions survived the password change, want 0", n)
	}
	// Somebody else's sessions are not collateral damage.
	if n, _ := s.CountSessionsForUser(ctx, other.ID); n != 1 {
		t.Errorf("another account lost sessions: %d, want 1", n)
	}

	if _, err := s.Authenticate(ctx, "alex@example.com", "correct horse battery"); !errors.Is(err, ErrBadCredentials) {
		t.Error("the old password still signs in")
	}
	got, err := s.Authenticate(ctx, "alex@example.com", "a different password entirely")
	if err != nil {
		t.Fatalf("Authenticate with the new password: %v", err)
	}
	if !got.MustChangePassword {
		t.Error("a password reset by somebody else did not set must_change_password")
	}
}

func TestSetPasswordRejectsBadHashAndUnknownUser(t *testing.T) {
	s, _, ctx := newStore(t)
	u := mustCreate(t, s, ctx, "alex@example.com", "correct horse battery", RoleAdmin)

	if err := s.SetPassword(ctx, u.ID, "not-a-hash", false); !errors.Is(err, ErrPasswordFormat) {
		t.Errorf("SetPassword(malformed hash) = %v, want ErrPasswordFormat", err)
	}
	// The account must still work: a refused write must not have half-applied.
	if _, err := s.Authenticate(ctx, "alex@example.com", "correct horse battery"); err != nil {
		t.Errorf("the account stopped working after a refused password change: %v", err)
	}

	const otherID = "00000000-0000-0000-0000-000000000000"
	if err := s.SetPassword(ctx, otherID, hash(t, "correct horse battery"), false); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetPassword(unknown user) = %v, want ErrNotFound", err)
	}
}

// Two owners disabling each other at the same moment would each read "2 enabled",
// each pass a separate check, and between them leave an admin area nobody can
// sign in to. The guard is in the UPDATE's WHERE clause for that reason.
func TestCannotDisableTheLastEnabledOwner(t *testing.T) {
	s, pool, ctx := newStore(t)
	only := mustCreate(t, s, ctx, "owner@example.com", "correct horse battery", RoleOwner)
	session(t, pool, ctx, only.ID)

	if err := s.SetDisabled(ctx, only.ID, true); !errors.Is(err, ErrLastOwner) {
		t.Fatalf("SetDisabled(last owner) = %v, want ErrLastOwner", err)
	}
	stored, _ := s.Get(ctx, only.ID)
	if stored.Disabled {
		t.Error("the last owner was disabled anyway")
	}
	// A refusal must not have the side effects of a success.
	if n, _ := s.CountSessionsForUser(ctx, only.ID); n != 1 {
		t.Errorf("a refused disable left %d sessions, want the 1 it started with", n)
	}

	// A second owner makes the first one ordinary.
	second := mustCreate(t, s, ctx, "second@example.com", "correct horse battery", RoleOwner)
	if err := s.SetDisabled(ctx, only.ID, true); err != nil {
		t.Fatalf("SetDisabled with a second owner present: %v", err)
	}
	if n, _ := s.CountSessionsForUser(ctx, only.ID); n != 0 {
		t.Errorf("disabling left %d sessions live, want 0", n)
	}

	// And now the second one is the last, so it is protected in turn.
	if err := s.SetDisabled(ctx, second.ID, true); !errors.Is(err, ErrLastOwner) {
		t.Errorf("SetDisabled(the new last owner) = %v, want ErrLastOwner", err)
	}
}

func TestDisableIsIdempotentAndEnablingIsNeverGuarded(t *testing.T) {
	s, _, ctx := newStore(t)
	mustCreate(t, s, ctx, "owner@example.com", "correct horse battery", RoleOwner)
	victim := mustCreate(t, s, ctx, "manager@example.com", "correct horse battery", RoleManager)

	if err := s.SetDisabled(ctx, victim.ID, true); err != nil {
		t.Fatalf("SetDisabled: %v", err)
	}
	// A double-submitted form is not a refusal worth explaining.
	if err := s.SetDisabled(ctx, victim.ID, true); err != nil {
		t.Errorf("disabling an already-disabled account = %v, want success", err)
	}
	if err := s.SetDisabled(ctx, victim.ID, false); err != nil {
		t.Errorf("enabling = %v, want success", err)
	}
	stored, _ := s.Get(ctx, victim.ID)
	if stored.Disabled {
		t.Error("the account is still disabled after being enabled")
	}
}

// Demotion is the other way to remove the last owner, and needs the same guard.
func TestCannotDemoteTheLastEnabledOwner(t *testing.T) {
	s, pool, ctx := newStore(t)
	only := mustCreate(t, s, ctx, "owner@example.com", "correct horse battery", RoleOwner)
	session(t, pool, ctx, only.ID)

	if err := s.SetRole(ctx, only.ID, RoleViewer); !errors.Is(err, ErrLastOwner) {
		t.Fatalf("SetRole(last owner -> viewer) = %v, want ErrLastOwner", err)
	}
	stored, _ := s.Get(ctx, only.ID)
	if stored.Role != RoleOwner {
		t.Errorf("Role = %q after a refused demotion, want %q", stored.Role, RoleOwner)
	}
	if n, _ := s.CountSessionsForUser(ctx, only.ID); n != 1 {
		t.Error("a refused role change dropped the account's sessions")
	}

	// Setting the same role again is not a demotion and must be allowed, or the
	// admin form refuses a submission that changes nothing.
	if err := s.SetRole(ctx, only.ID, RoleOwner); err != nil {
		t.Errorf("SetRole(owner -> owner) = %v, want success", err)
	}

	mustCreate(t, s, ctx, "second@example.com", "correct horse battery", RoleOwner)
	if err := s.SetRole(ctx, only.ID, RoleManager); err != nil {
		t.Fatalf("SetRole with a second owner present: %v", err)
	}
	// A privilege change is worth making somebody sign in again for.
	if n, _ := s.CountSessionsForUser(ctx, only.ID); n != 0 {
		t.Errorf("a role change left %d sessions live, want 0", n)
	}
}

// A disabled owner is not among the enabled ones being counted, so demoting one
// cannot be what removes the last.
func TestDemotingADisabledOwnerIsAllowed(t *testing.T) {
	s, _, ctx := newStore(t)
	mustCreate(t, s, ctx, "owner@example.com", "correct horse battery", RoleOwner)
	spare := mustCreate(t, s, ctx, "spare@example.com", "correct horse battery", RoleOwner)

	if err := s.SetDisabled(ctx, spare.ID, true); err != nil {
		t.Fatalf("SetDisabled: %v", err)
	}
	if err := s.SetRole(ctx, spare.ID, RoleViewer); err != nil {
		t.Errorf("SetRole(disabled owner) = %v, want success", err)
	}
}

func TestGuardedWritesReportAMissingAccountAsNotFound(t *testing.T) {
	s, _, ctx := newStore(t)
	const missing = "00000000-0000-0000-0000-000000000000"

	// The two reasons a guarded UPDATE affects no rows are not inferable from the
	// row count, and a handler answers 404 for one and explains the other.
	if err := s.SetDisabled(ctx, missing, true); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetDisabled(unknown) = %v, want ErrNotFound", err)
	}
	if err := s.SetRole(ctx, missing, RoleViewer); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetRole(unknown) = %v, want ErrNotFound", err)
	}
	var invalid *ErrInvalidRole
	if err := s.SetRole(ctx, missing, Role("wizard")); !errors.As(err, &invalid) {
		t.Errorf("SetRole(unknown role) = %v, want ErrInvalidRole", err)
	}
}

func TestListAndCount(t *testing.T) {
	s, _, ctx := newStore(t)
	if n, err := s.Count(ctx); err != nil || n != 0 {
		t.Fatalf("Count on an unclaimed store = %d, %v; want 0, nil", n, err)
	}

	mustCreate(t, s, ctx, "zoe@example.com", "correct horse battery", RoleViewer)
	mustCreate(t, s, ctx, "adam@example.com", "correct horse battery", RoleOwner)

	users, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(users) != 2 {
		t.Fatalf("List returned %d users, want 2", len(users))
	}
	if users[0].Email != "adam@example.com" {
		t.Errorf("List is not ordered by address: %s first", users[0].Email)
	}
	if n, _ := s.Count(ctx); n != 2 {
		t.Errorf("Count = %d, want 2", n)
	}
}

func TestSessionSweepRemovesOnlyExpiredRows(t *testing.T) {
	s, pool, ctx := newStore(t)
	u := mustCreate(t, s, ctx, "alex@example.com", "correct horse battery", RoleAdmin)
	session(t, pool, ctx, u.ID)
	_, err := pool.Exec(ctx,
		`INSERT INTO admin_sessions (token_hash, user_id, expires_at)
		 VALUES ($1, $2, now() - interval '1 minute')`, hashToken(randomLabel(t)), u.ID)
	if err != nil {
		t.Fatalf("insert expired session: %v", err)
	}

	n, err := s.DeleteExpiredSessions(ctx)
	if err != nil {
		t.Fatalf("DeleteExpiredSessions: %v", err)
	}
	if n != 1 {
		t.Errorf("swept %d sessions, want 1", n)
	}
	if live, _ := s.CountSessionsForUser(ctx, u.ID); live != 1 {
		t.Errorf("%d sessions left, want the unexpired one", live)
	}
}

func TestSetupTokenLifecycle(t *testing.T) {
	s, _, ctx := newStore(t)

	if pending, err := s.SetupPending(ctx); err != nil || pending {
		t.Fatalf("SetupPending before a token = %v, %v; want false, nil", pending, err)
	}

	stored, err := s.CreateSetupToken(ctx, "the-setup-token")
	if err != nil || !stored {
		t.Fatalf("CreateSetupToken = %v, %v; want true, nil", stored, err)
	}
	// Running the boot path again must not replace a live token with a new one,
	// or every restart would invalidate the token the operator is holding.
	again, err := s.CreateSetupToken(ctx, "a-different-token")
	if err != nil || again {
		t.Fatalf("CreateSetupToken a second time = %v, %v; want false, nil", again, err)
	}

	if pending, _ := s.SetupPending(ctx); !pending {
		t.Error("SetupPending is false with an unspent token")
	}
	if ok, _ := s.CheckSetupToken(ctx, "a-different-token"); ok {
		t.Error("CheckSetupToken accepted the wrong token")
	}
	if ok, err := s.CheckSetupToken(ctx, "the-setup-token"); err != nil || !ok {
		t.Errorf("CheckSetupToken(right token) = %v, %v; want true, nil", ok, err)
	}

	// Only the hash is stored, so a database backup does not hand over the token.
	var tokenHash []byte
	if err := s.pool.QueryRow(ctx, "SELECT token_hash FROM admin_setup").Scan(&tokenHash); err != nil {
		t.Fatalf("read token_hash: %v", err)
	}
	if string(tokenHash) == "the-setup-token" {
		t.Error("the setup token is stored in the clear")
	}
}

func TestClaimSetup(t *testing.T) {
	s, _, ctx := newStore(t)
	if _, err := s.CreateSetupToken(ctx, "the-setup-token"); err != nil {
		t.Fatalf("CreateSetupToken: %v", err)
	}

	pw := hash(t, "correct horse battery")
	if _, err := s.ClaimSetup(ctx, "wrong-token", "alex@example.com", "Alex", pw); !errors.Is(err, ErrBadSetupToken) {
		t.Fatalf("ClaimSetup(wrong token) = %v, want ErrBadSetupToken", err)
	}
	// A refused claim must leave the token spendable, or one typo locks the
	// operator out of their own store.
	if n, _ := s.Count(ctx); n != 0 {
		t.Fatalf("a refused claim created %d accounts", n)
	}

	owner, err := s.ClaimSetup(ctx, "the-setup-token", "alex@example.com", "Alex", pw)
	if err != nil {
		t.Fatalf("ClaimSetup: %v", err)
	}
	if owner.Role != RoleOwner {
		t.Errorf("the first account is %q, want %q", owner.Role, RoleOwner)
	}
	if owner.MustChangePassword {
		t.Error("the claimer chose their own password, so must_change_password should be false")
	}

	// Setup locks permanently: the consumed timestamp is never cleared, so it
	// does not reopen on a restart or if every account is later disabled.
	if pending, _ := s.SetupPending(ctx); pending {
		t.Error("SetupPending is still true after a successful claim")
	}
	if _, err := s.ClaimSetup(ctx, "the-setup-token", "impostor@example.com", "", pw); !errors.Is(err, ErrSetupClosed) {
		t.Errorf("a second ClaimSetup = %v, want ErrSetupClosed", err)
	}
	if n, _ := s.Count(ctx); n != 1 {
		t.Errorf("Count = %d after the second claim, want 1", n)
	}
}

func TestClaimSetupWithNoTokenIsClosed(t *testing.T) {
	s, _, ctx := newStore(t)
	_, err := s.ClaimSetup(ctx, "anything", "alex@example.com", "", hash(t, "correct horse battery"))
	if !errors.Is(err, ErrSetupClosed) {
		t.Errorf("ClaimSetup with no token issued = %v, want ErrSetupClosed", err)
	}
}

// Authenticate must not be slower for an unknown address than for a known one
// with a wrong password, or response time tells an attacker which addresses have
// accounts. This is a coarse check — it catches the "returns immediately with no
// argon2 work at all" regression, which is the one that actually happens.
func TestUnknownAddressCostsRoughlyWhatAWrongPasswordCosts(t *testing.T) {
	s, _, ctx := newStore(t)
	mustCreate(t, s, ctx, "live@example.com", "correct horse battery", RoleAdmin)
	// Warm the dummy hash, which is computed on first use.
	_, _ = s.Authenticate(ctx, "nobody@example.com", "x")

	known := timeCall(func() { _, _ = s.Authenticate(ctx, "live@example.com", "wrong") })
	unknown := timeCall(func() { _, _ = s.Authenticate(ctx, "nobody@example.com", "wrong") })

	if unknown < known/4 {
		t.Errorf("an unknown address answered in %s against %s for a wrong password: "+
			"the dummy-hash verification is not happening", unknown, known)
	}
}

func timeCall(fn func()) time.Duration {
	start := time.Now()
	fn()
	return time.Since(start)
}

// TestOwnerGuardSerialisesConcurrentWriters pins the fix for a race the guard's
// count subquery does not close on its own.
//
// Two transactions removing *different* owners touch different rows, so they take
// no lock in common, and under READ COMMITTED each evaluates the count against
// its own snapshot: both see two enabled owners, both pass, both commit, and the
// store is left with none. That is reproducible in a psql session in seconds.
//
// The interleaving is driven explicitly rather than by racing two goroutines and
// hoping. A timing-based version of this test passed just as happily with the
// advisory lock removed, which makes it worse than no test at all: what is
// asserted here is that the second writer *blocks* until the first commits, and
// is then refused by a count it can finally trust.
func TestOwnerGuardSerialisesConcurrentWriters(t *testing.T) {
	s, pool, ctx := newStore(t)
	a := mustCreate(t, s, ctx, "a@example.com", "correct horse battery", RoleOwner)
	b := mustCreate(t, s, ctx, "b@example.com", "correct horse battery", RoleOwner)

	// Two dedicated connections, so the two transactions are genuinely separate.
	connA, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire A: %v", err)
	}
	defer connA.Release()
	connB, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire B: %v", err)
	}
	defer connB.Release()

	const guard = `UPDATE admin_users u SET disabled = TRUE
		WHERE u.id = $1
		  AND (u.disabled = TRUE
		       OR u.role <> 'owner'
		       OR (SELECT count(*) FROM admin_users o
		           WHERE o.role = 'owner' AND NOT o.disabled) > 1)`

	txA, err := connA.Begin(ctx)
	if err != nil {
		t.Fatalf("begin A: %v", err)
	}
	defer txA.Rollback(ctx)
	if _, err := txA.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, ownerGuardLockID); err != nil {
		t.Fatalf("lock A: %v", err)
	}
	tagA, err := txA.Exec(ctx, guard, a.ID)
	if err != nil {
		t.Fatalf("guard A: %v", err)
	}
	if tagA.RowsAffected() != 1 {
		t.Fatalf("the first writer was refused with two owners enabled")
	}

	// B now tries to take the same lock while A holds it. It must not get through.
	blocked := make(chan error, 1)
	go func() {
		txB, err := connB.Begin(ctx)
		if err != nil {
			blocked <- err
			return
		}
		defer txB.Rollback(ctx)
		if _, err := txB.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, ownerGuardLockID); err != nil {
			blocked <- err
			return
		}
		tagB, err := txB.Exec(ctx, guard, b.ID)
		if err != nil {
			blocked <- err
			return
		}
		if tagB.RowsAffected() != 0 {
			blocked <- errors.New("the second writer removed the last enabled owner")
			return
		}
		blocked <- txB.Commit(ctx)
	}()

	select {
	case err := <-blocked:
		t.Fatalf("the second writer did not wait for the first: %v", err)
	case <-time.After(250 * time.Millisecond):
		// Still waiting on the lock, which is the point.
	}

	if err := txA.Commit(ctx); err != nil {
		t.Fatalf("commit A: %v", err)
	}
	select {
	case err := <-blocked:
		if err != nil {
			t.Fatalf("the second writer: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the second writer never proceeded after the first committed")
	}

	// The property all of this exists for.
	users, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	enabled := 0
	for _, u := range users {
		if u.Role == RoleOwner && !u.Disabled {
			enabled++
		}
	}
	if enabled != 1 {
		t.Errorf("%d enabled owners remain, want 1", enabled)
	}
}

// TestSetRoleToTheSameRoleKeepsSessions guards against an account form that
// signs somebody out for saving it without touching the role select — including,
// when they are editing themselves, out of the session they are using.
func TestSetRoleToTheSameRoleKeepsSessions(t *testing.T) {
	s, pool, ctx := newStore(t)
	mustCreate(t, s, ctx, "owner@example.com", "correct horse battery", RoleOwner)
	u := mustCreate(t, s, ctx, "manager@example.com", "correct horse battery", RoleManager)
	session(t, pool, ctx, u.ID)

	if err := s.SetRole(ctx, u.ID, RoleManager); err != nil {
		t.Fatalf("SetRole to the role it already has: %v", err)
	}
	if n, _ := s.CountSessionsForUser(ctx, u.ID); n != 1 {
		t.Errorf("a no-op role save left %d sessions, want the 1 it started with", n)
	}

	// A real change still ends them, which is the behaviour this must not break.
	if err := s.SetRole(ctx, u.ID, RoleViewer); err != nil {
		t.Fatalf("SetRole: %v", err)
	}
	if n, _ := s.CountSessionsForUser(ctx, u.ID); n != 0 {
		t.Errorf("a real demotion left %d sessions live, want 0", n)
	}
}

// TestExpiredSessionsAreNotReturnedByTheLookup pins expiry to the lookup query
// itself, rather than to whichever handler Phase 2 wires onto it remembering to
// compare ExpiresAt afterwards. The sweep is not the backstop: it is housekeeping
// against unbounded growth, and between two sweeps the table is full of expired
// rows that a predicate-less lookup would happily authenticate.
//
// This goes through the generated query directly because the Store method that
// will wrap it does not exist yet; the behaviour being pinned is the query's.
func TestExpiredSessionsAreNotReturnedByTheLookup(t *testing.T) {
	s, pool, ctx := newStore(t)
	u := mustCreate(t, s, ctx, "owner@example.com", "correct horse battery", RoleOwner)

	live, expired := randomLabel(t), randomLabel(t)
	if _, err := pool.Exec(ctx,
		`INSERT INTO admin_sessions (token_hash, user_id, expires_at)
		 VALUES ($1, $2, now() + interval '1 hour'),
		        ($3, $2, now() - interval '1 second')`,
		hashToken(live), u.ID, hashToken(expired)); err != nil {
		t.Fatalf("insert sessions: %v", err)
	}

	if _, err := s.q.GetAdminSession(ctx, hashToken(expired)); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("an expired session was returned by the lookup (err = %v), want no rows", err)
	}
	// The live one still resolving is what stops the predicate being trivially
	// satisfied by returning nothing at all.
	if _, err := s.q.GetAdminSession(ctx, hashToken(live)); err != nil {
		t.Errorf("the live session did not resolve: %v", err)
	}
}

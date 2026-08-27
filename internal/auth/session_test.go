package auth

import (
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestNewToken_IsRandomAndURLSafe(t *testing.T) {
	seen := make(map[string]bool, 64)
	for range 64 {
		token, err := NewToken()
		if err != nil {
			t.Fatalf("NewToken: %v", err)
		}
		if seen[token] {
			t.Fatalf("NewToken returned %q twice", token)
		}
		seen[token] = true

		// base64 of 32 bytes, unpadded. The cookie carries this verbatim, so a
		// character needing escaping would be a bug that only shows up in a
		// browser.
		if len(token) != 43 {
			t.Errorf("token %q is %d characters, want 43", token, len(token))
		}
		if strings.ContainsAny(token, "+/=") {
			t.Errorf("token %q is not URL-safe", token)
		}
	}
}

func TestIssueSession_StoresOnlyTheHash(t *testing.T) {
	s, pool, ctx := newStore(t)
	u := mustCreate(t, s, ctx, "owner@example.com", "correct horse battery", RoleOwner)

	token, sess, err := s.IssueSession(ctx, u.ID, time.Hour)
	if err != nil {
		t.Fatalf("IssueSession: %v", err)
	}
	if token == "" {
		t.Fatal("IssueSession returned an empty token")
	}
	if sess.UserID != u.ID {
		t.Errorf("session UserID = %q, want %q", sess.UserID, u.ID)
	}
	if time.Until(sess.ExpiresAt) < 55*time.Minute {
		t.Errorf("ExpiresAt = %s, want about an hour out", sess.ExpiresAt)
	}

	// The point of the whole design: the token is not in the database. A leaked
	// backup must not hand over anybody's live session.
	var stored []byte
	if err := pool.QueryRow(ctx, `SELECT token_hash FROM admin_sessions`).Scan(&stored); err != nil {
		t.Fatalf("read token_hash: %v", err)
	}
	if strings.Contains(string(stored), token) {
		t.Error("the plain token is in the admin_sessions row")
	}
	want := sha256.Sum256([]byte(token))
	if string(stored) != string(want[:]) {
		t.Errorf("token_hash = %x, want sha256(token) = %x", stored, want)
	}
}

func TestSession_RoundTrip(t *testing.T) {
	s, _, ctx := newStore(t)
	u := mustCreate(t, s, ctx, "owner@example.com", "correct horse battery", RoleOwner)

	token, _, err := s.IssueSession(ctx, u.ID, time.Hour)
	if err != nil {
		t.Fatalf("IssueSession: %v", err)
	}

	sess, got, err := s.Session(ctx, token)
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if sess.UserID != u.ID {
		t.Errorf("session UserID = %q, want %q", sess.UserID, u.ID)
	}
	// The account travels with the session, because every caller needs both and
	// because the user's current state is what makes a session revocable.
	if got.ID != u.ID || got.Email != u.Email || got.Role != RoleOwner {
		t.Errorf("user = %+v, want the owner %q", got, u.Email)
	}
}

func TestSession_RejectsUnknownEmptyAndExpired(t *testing.T) {
	s, pool, ctx := newStore(t)
	u := mustCreate(t, s, ctx, "owner@example.com", "correct horse battery", RoleOwner)

	live, _, err := s.IssueSession(ctx, u.ID, time.Hour)
	if err != nil {
		t.Fatalf("IssueSession: %v", err)
	}
	expired, _, err := s.IssueSession(ctx, u.ID, time.Hour)
	if err != nil {
		t.Fatalf("IssueSession: %v", err)
	}
	// Aged past its expiry rather than issued with a nanosecond TTL, so the test
	// does not depend on how fast the machine running it is.
	if _, err := pool.Exec(ctx,
		`UPDATE admin_sessions SET expires_at = now() - interval '1 second' WHERE token_hash = $1`,
		hashToken(expired)); err != nil {
		t.Fatalf("age the session: %v", err)
	}

	cases := map[string]string{
		"empty":   "",
		"unknown": "not-a-token-anybody-issued",
		// Expiry is enforced in the lookup's predicate, not by the sweep: between
		// two sweeps the table is full of expired rows, and a lookup that returned
		// them would authenticate every one.
		"expired": expired,
	}
	for name, token := range cases {
		if _, _, err := s.Session(ctx, token); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: Session error = %v, want ErrNotFound", name, err)
		}
	}

	// And the live one still works, so the cases above failed for their own
	// reasons rather than because nothing works.
	if _, _, err := s.Session(ctx, live); err != nil {
		t.Errorf("the live session broke too: %v", err)
	}
}

func TestIssueSession_RefusesNonPositiveTTL(t *testing.T) {
	s, _, ctx := newStore(t)
	u := mustCreate(t, s, ctx, "owner@example.com", "correct horse battery", RoleOwner)

	for _, ttl := range []time.Duration{0, -time.Hour} {
		if _, _, err := s.IssueSession(ctx, u.ID, ttl); err == nil {
			t.Errorf("IssueSession with ttl %s was accepted", ttl)
		}
	}
	// Refused rather than clamped, and refused before writing: a session created
	// already expired is a sign-in that silently does not work.
	if n, err := s.CountSessionsForUser(ctx, u.ID); err != nil || n != 0 {
		t.Errorf("sessions = %d (err %v), want none written", n, err)
	}
}

func TestDeleteSession(t *testing.T) {
	s, _, ctx := newStore(t)
	u := mustCreate(t, s, ctx, "owner@example.com", "correct horse battery", RoleOwner)

	token, _, err := s.IssueSession(ctx, u.ID, time.Hour)
	if err != nil {
		t.Fatalf("IssueSession: %v", err)
	}
	other, _, err := s.IssueSession(ctx, u.ID, time.Hour)
	if err != nil {
		t.Fatalf("IssueSession: %v", err)
	}

	if err := s.DeleteSession(ctx, token); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if _, _, err := s.Session(ctx, token); !errors.Is(err, ErrNotFound) {
		t.Errorf("the deleted session still resolves: %v", err)
	}
	// Signing out of one browser must not sign the account out of the others.
	if _, _, err := s.Session(ctx, other); err != nil {
		t.Errorf("the other session died too: %v", err)
	}

	// Idempotent: a double-submitted sign-out and one carrying an already-expired
	// cookie both arrive here, and neither is a problem worth reporting to
	// somebody who has just left.
	if err := s.DeleteSession(ctx, token); err != nil {
		t.Errorf("deleting a gone session: %v", err)
	}
	if err := s.DeleteSession(ctx, ""); err != nil {
		t.Errorf("deleting an empty token: %v", err)
	}
}

func TestSetPassword_EndsSessionsIssuedByIssueSession(t *testing.T) {
	// The store tests already cover "SetPassword deletes the rows"; this covers
	// the pairing that matters at runtime — a token handed to a browser stops
	// resolving the moment the password behind it changes.
	s, _, ctx := newStore(t)
	u := mustCreate(t, s, ctx, "owner@example.com", "correct horse battery", RoleOwner)

	token, _, err := s.IssueSession(ctx, u.ID, time.Hour)
	if err != nil {
		t.Fatalf("IssueSession: %v", err)
	}
	if err := s.SetPassword(ctx, u.ID, hash(t, "a different long passphrase"), true); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	if _, _, err := s.Session(ctx, token); !errors.Is(err, ErrNotFound) {
		t.Errorf("the session survived a password change: %v", err)
	}
}

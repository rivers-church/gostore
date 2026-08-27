// Package auth holds the administrator accounts, their roles, and the passwords
// and sessions that get them in.
//
// The single-operator design this package started as — one ADMIN_PASSWORD_HASH
// in the environment and a signed, self-describing cookie carrying nothing but
// an expiry — reached the trigger it always documented for itself: a second
// administrator with different permissions, and per-session revocation. Named
// accounts, roles and an admin_sessions table are what replace it, in
// model.go and store.go.
//
// This file is the tail of that older design and is still what authenticates a
// request. It goes when the handlers move onto Store's session methods; until
// then both exist, and the securecookie path is the live one. Nothing new should
// be built on it.
package auth

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/gorilla/securecookie"
)

// CookieName is the admin session cookie. It is scoped to /admin by the
// handler that sets it, so it is never sent with storefront requests. It is
// also part of the signed payload, so a value lifted from another cookie will
// not verify here.
const CookieName = "admin_session"

// MinSecretLen is the shortest session secret accepted. HMAC-SHA256 gains
// nothing from a key longer than its 32-byte block, and loses meaningfully to
// one shorter.
const MinSecretLen = 32

// ErrExpired distinguishes a session that was genuine but has run out from one
// that was never valid, so a caller can log the second and ignore the first.
var ErrExpired = errors.New("auth: session expired")

// Sessions issues and verifies admin session cookies.
//
// It holds one codec per accepted key: the first signs, and any of them may
// verify. That is the whole mechanism behind rotating SESSION_SECRET without
// signing the operator out — move the old value to SESSION_SECRET_PREVIOUS,
// deploy, and remove it once the old sessions have expired.
type Sessions struct {
	codecs []securecookie.Codec
	ttl    time.Duration
}

// NewSessions builds the session codecs. previous may be nil.
func NewSessions(secret, previous []byte, ttl time.Duration) (*Sessions, error) {
	if len(secret) < MinSecretLen {
		return nil, fmt.Errorf("auth: session secret is %d bytes, want at least %d", len(secret), MinSecretLen)
	}
	if previous != nil && len(previous) < MinSecretLen {
		return nil, fmt.Errorf("auth: previous session secret is %d bytes, want at least %d", len(previous), MinSecretLen)
	}
	if ttl <= 0 {
		return nil, fmt.Errorf("auth: session ttl must be positive, got %s", ttl)
	}

	// nil block keys: the payload is an expiry timestamp, which needs to be
	// unforgeable, not secret. Signing without encrypting keeps the cookie
	// readable in a debugger and one fewer key to manage.
	keyPairs := [][]byte{secret, nil}
	if previous != nil {
		keyPairs = append(keyPairs, previous, nil)
	}

	codecs := securecookie.CodecsFromPairs(keyPairs...)
	for _, c := range codecs {
		if sc, ok := c.(*securecookie.SecureCookie); ok {
			// securecookie's own timestamp check, in addition to the expiry we
			// sign into the payload below. Two independent bounds on the same
			// thing, because a session that outlives its welcome is the failure
			// that matters here.
			sc.MaxAge(int(ttl.Seconds()))
		}
	}
	return &Sessions{codecs: codecs, ttl: ttl}, nil
}

// Issue returns a cookie value that proves the holder authenticated and says
// when that stops being true.
//
// The expiry travels inside the signed payload rather than relying only on the
// cookie's Expires attribute, which a client controls and can simply not
// honour.
func (s *Sessions) Issue(now time.Time) (string, error) {
	payload := strconv.FormatInt(now.Add(s.ttl).Unix(), 10)
	value, err := securecookie.EncodeMulti(CookieName, payload, s.codecs...)
	if err != nil {
		return "", fmt.Errorf("auth: issue session: %w", err)
	}
	return value, nil
}

// TTL is how long an issued session lasts, for setting the cookie's own
// lifetime to match.
func (s *Sessions) TTL() time.Duration { return s.ttl }

// Verify checks a cookie value's signature and expiry, returning when it
// expires so a caller can log or refresh it.
func (s *Sessions) Verify(value string, now time.Time) (time.Time, error) {
	var payload string
	if err := securecookie.DecodeMulti(CookieName, value, &payload, s.codecs...); err != nil {
		// securecookie rejects a stale timestamp as a decode error, which is
		// indistinguishable from tampering without matching on its message. The
		// signed expiry below is what tells the two apart, so treat anything
		// that fails to decode as not genuine and let the caller log it.
		return time.Time{}, fmt.Errorf("auth: session does not verify: %w", err)
	}

	unix, err := strconv.ParseInt(payload, 10, 64)
	if err != nil {
		return time.Time{}, errors.New("auth: malformed session expiry")
	}
	expiry := time.Unix(unix, 0)
	if now.After(expiry) {
		return expiry, ErrExpired
	}
	return expiry, nil
}

// Password hashing lives in password.go.

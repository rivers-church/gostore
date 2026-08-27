package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/17xande-dev/gostore/internal/auth"
	"github.com/17xande-dev/gostore/internal/dbtest"
)

// captureLogger records what the boot path logged, because for the generated
// token the log line *is* the delivery mechanism: an operator who cannot find it
// cannot claim the store.
func captureLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewJSONHandler(&buf, nil)), &buf
}

// loggedToken pulls the setup_token attribute out of the captured JSON lines.
func loggedToken(t *testing.T, buf *bytes.Buffer) string {
	t.Helper()

	for line := range strings.SplitSeq(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line is not JSON: %s", line)
		}
		if token, ok := rec["setup_token"].(string); ok {
			return token
		}
	}
	return ""
}

func TestEnsureSetupToken_GeneratesAndLogsOnAnEmptyStore(t *testing.T) {
	users := auth.NewStore(dbtest.Pool(t))
	ctx := t.Context()
	log, buf := captureLogger()

	if err := ensureSetupToken(ctx, users, "", log); err != nil {
		t.Fatalf("ensureSetupToken: %v", err)
	}

	token := loggedToken(t, buf)
	if token == "" {
		t.Fatalf("no setup token in the log: %s", buf.String())
	}
	// The logged token is the one the claim flow will accept, and only its hash is
	// stored — so this is the single place it ever exists in plain text.
	ok, err := users.CheckSetupToken(ctx, token)
	if err != nil || !ok {
		t.Errorf("CheckSetupToken(logged token) = %v, %v", ok, err)
	}
	if pending, err := users.SetupPending(ctx); err != nil || !pending {
		t.Errorf("SetupPending = %v, %v, want true", pending, err)
	}
}

func TestEnsureSetupToken_NeverLogsASuppliedToken(t *testing.T) {
	users := auth.NewStore(dbtest.Pool(t))
	ctx := t.Context()
	log, buf := captureLogger()
	supplied := strings.Repeat("s", 43)

	if err := ensureSetupToken(ctx, users, supplied, log); err != nil {
		t.Fatalf("ensureSetupToken: %v", err)
	}

	// It is already wherever the deploy keeps its secrets; a log line is a worse
	// second place for it to be.
	if strings.Contains(buf.String(), supplied) {
		t.Errorf("SETUP_TOKEN was written to the log: %s", buf.String())
	}
	if ok, err := users.CheckSetupToken(ctx, supplied); err != nil || !ok {
		t.Errorf("the supplied token was not stored: %v, %v", ok, err)
	}
}

// A restart must not replace a token that is still unclaimed — the second call
// leaves the first token working, because a browser may already be holding it on
// a half-finished setup page.
func TestEnsureSetupToken_KeepsAnUnclaimedToken(t *testing.T) {
	users := auth.NewStore(dbtest.Pool(t))
	ctx := t.Context()

	log, buf := captureLogger()
	if err := ensureSetupToken(ctx, users, "", log); err != nil {
		t.Fatalf("first boot: %v", err)
	}
	first := loggedToken(t, buf)

	log, buf = captureLogger()
	if err := ensureSetupToken(ctx, users, "", log); err != nil {
		t.Fatalf("second boot: %v", err)
	}

	if ok, err := users.CheckSetupToken(ctx, first); err != nil || !ok {
		t.Errorf("the first token stopped working after a restart: %v, %v", ok, err)
	}
	if again := loggedToken(t, buf); again != "" {
		t.Errorf("the second boot logged a token (%q); only its hash is stored, so it cannot be a working one", again)
	}
	// It says so instead, and points at where the working one was printed.
	if !strings.Contains(buf.String(), "already been issued") {
		t.Errorf("the second boot says nothing about the token already issued: %s", buf.String())
	}
}

// A claimed store issues nothing, on this boot or any later one. That is what
// makes the bootstrap a one-time event rather than a door that reopens.
func TestEnsureSetupToken_IssuesNothingOnAClaimedStore(t *testing.T) {
	users := auth.NewStore(dbtest.Pool(t))
	ctx := t.Context()

	if _, err := users.Create(ctx, "owner@example.com", "The Owner",
		mustHash(t, "correct horse battery"), auth.RoleOwner, false); err != nil {
		t.Fatalf("Create: %v", err)
	}

	log, buf := captureLogger()
	if err := ensureSetupToken(ctx, users, "", log); err != nil {
		t.Fatalf("ensureSetupToken: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("a claimed store logged %s", buf.String())
	}
	if pending, err := users.SetupPending(ctx); err != nil || pending {
		t.Errorf("SetupPending = %v, %v, want false", pending, err)
	}
}

// Command hashpw turns a password into an argon2id hash.
//
// It is a recovery path and not part of setting the store up. Administrators are
// created at /admin/users, and the first one claims the store at /admin/setup with
// the one-time token the server prints on its first boot — no password hash goes
// into the environment any more.
//
// What is left for this command is the case nothing in the admin can repair: every
// enabled owner locked out, or the only account's password lost. With database
// access, hash a new password here and set it:
//
//	read -rs P && printf %s "$P" | go run ./cmd/hashpw
//	UPDATE admin_users SET password_hash = '<hash>', disabled = false,
//	    must_change_password = true WHERE lower(email) = 'you@example.com';
//	DELETE FROM admin_sessions WHERE user_id = (SELECT id FROM admin_users
//	    WHERE lower(email) = 'you@example.com');
//
// The DELETE is the half worth remembering: changing the hash by hand skips
// everything Store.SetPassword does around it, and a session issued under the old
// password stays live otherwise.
//
// The password is read from stdin, never from a command-line argument, so it does
// not end up in shell history or in another user's `ps` output.
package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/17xande-dev/gostore/internal/auth"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	password, err := readPassword(os.Stdin)
	if err != nil {
		return err
	}

	hash, err := auth.HashPassword(password, auth.DefaultParams)
	if err != nil {
		return err
	}

	// The hash alone, on stdout, with nothing around it: it is going into a SQL
	// statement, and a KEY=value line would only have to be edited back out.
	fmt.Println(hash)
	return nil
}

// readPassword takes everything up to the first newline, so a piped
// `printf %s` and an interactively typed line both work.
func readPassword(r io.Reader) (string, error) {
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && err != io.EOF {
		return "", fmt.Errorf("hashpw: read password: %w", err)
	}
	password := strings.TrimRight(line, "\r\n")
	if password == "" {
		return "", errors.New("hashpw: no password on stdin; pipe one in, e.g. " +
			`read -rs P && printf %s "$P" | go run ./cmd/hashpw`)
	}
	return password, nil
}

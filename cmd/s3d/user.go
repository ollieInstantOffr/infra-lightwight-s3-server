package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"golang.org/x/term"

	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/config"
	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/db"
)

// The user subcommands are the way back in when nobody can sign in.
//
// Authentication is by password and there is no emailed reset link, so a
// forgotten password has no self-service recovery. This is the recovery: it
// runs on the host, needs no session, and works when every administrator is
// locked out. It is also how a fresh deployment gets started, since
// EnsureAdmin creates the bootstrap administrator but cannot invent a password
// for them.

const userUsage = `Usage:
  s3d user list                    list console users
  s3d user set-password <email>    set a user's password
  s3d user promote <email>         make a user an administrator

set-password reads the new password from the terminal without echoing it. When
stdin is not a terminal it reads one line, so it can be piped:

  echo 'the-new-password' | s3d user set-password admin@example.com
`

// runUserCommand handles "s3d user ...". It returns false if the arguments are
// not a user command, so the caller can carry on and start the server.
func runUserCommand(args []string) (handled bool, err error) {
	if len(args) == 0 || args[0] != "user" {
		return false, nil
	}
	if len(args) < 2 {
		return true, fmt.Errorf("%s", userUsage)
	}

	cfg, err := config.Load()
	if err != nil {
		return true, err
	}

	ctx := context.Background()
	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return true, err
	}
	defer pool.Close()

	switch args[1] {
	case "list":
		return true, listConsoleUsers(ctx, pool)

	case "set-password":
		if len(args) < 3 {
			return true, fmt.Errorf("set-password needs an email address\n\n%s", userUsage)
		}
		return true, setUserPassword(ctx, pool, args[2])

	case "promote":
		if len(args) < 3 {
			return true, fmt.Errorf("promote needs an email address\n\n%s", userUsage)
		}
		return true, promoteUser(ctx, pool, args[2])

	default:
		return true, fmt.Errorf("unknown user command %q\n\n%s", args[1], userUsage)
	}
}

func listConsoleUsers(ctx context.Context, pool *db.Pool) error {
	users, err := db.ListUsers(ctx, pool)
	if err != nil {
		return err
	}
	if len(users) == 0 {
		fmt.Println("No users yet.")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "EMAIL\tROLE\tPASSWORD\tLAST SIGNED IN")
	for _, user := range users {
		// Whether a password is set is the thing an operator running this is
		// most likely trying to find out: an account without one cannot sign
		// in, and that is the state every account is in after upgrading.
		password := "set"
		if present, err := db.HasPassword(ctx, pool, user.Email); err == nil && !present {
			password = "NOT SET"
		}
		lastLogin := "never"
		if user.LastLoginAt != nil {
			lastLogin = user.LastLoginAt.Format("2006-01-02 15:04")
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", user.Email, user.Role, password, lastLogin)
	}
	return w.Flush()
}

func setUserPassword(ctx context.Context, pool *db.Pool, email string) error {
	// Checked before prompting, so a typo in the address is reported before
	// the operator types a password twice.
	if _, err := db.GetUserByEmail(ctx, pool, email); err != nil {
		if errors.Is(err, db.ErrUserNotFound) {
			return fmt.Errorf("no user with the address %q. `s3d user list` shows who exists", email)
		}
		return err
	}

	password, err := readNewPassword()
	if err != nil {
		return err
	}

	// mustChange is false: whoever runs this has host access, so there is no
	// separation to enforce between the person setting the password and the
	// person using it. Forcing a change here would only add a step to the
	// recovery path this command exists to be.
	if err := db.SetPasswordByEmail(ctx, pool, email, password, false); err != nil {
		return err
	}

	// Sessions opened with the old password are no longer trusted. If the
	// reason for running this is that someone else had it, leaving their
	// session alive defeats the point.
	user, err := db.GetUserByEmail(ctx, pool, email)
	if err != nil {
		return err
	}
	if err := db.RevokeUserSessions(ctx, pool, user.ID); err != nil {
		return fmt.Errorf("password was set, but existing sessions could not be revoked: %w", err)
	}

	fmt.Printf("Password set for %s. Any existing sessions have been signed out.\n", email)
	return nil
}

func promoteUser(ctx context.Context, pool *db.Pool, email string) error {
	user, err := db.GetUserByEmail(ctx, pool, email)
	if err != nil {
		if errors.Is(err, db.ErrUserNotFound) {
			return fmt.Errorf("no user with the address %q", email)
		}
		return err
	}
	if user.IsAdmin() {
		fmt.Printf("%s is already an administrator.\n", email)
		return nil
	}
	if err := db.SetUserRole(ctx, pool, user.ID, db.RoleAdmin); err != nil {
		return err
	}
	fmt.Printf("%s is now an administrator.\n", email)
	return nil
}

// readNewPassword collects a password without putting it in the shell history
// or the process list, where any other user on the host could read it.
//
// From a terminal it prompts twice with echo off, because a mistyped password
// that nobody can see is a lockout. From a pipe it reads a single line and
// does not confirm, so the command stays scriptable.
func readNewPassword() (string, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", fmt.Errorf("read password from stdin: %w", err)
		}
		password := strings.TrimRight(line, "\r\n")
		if err := db.ValidatePassword(password); err != nil {
			return "", err
		}
		return password, nil
	}

	fmt.Print("New password: ")
	first, err := term.ReadPassword(fd)
	fmt.Println()
	if err != nil {
		return "", fmt.Errorf("read password: %w", err)
	}

	// Validated before asking again, so a too-short password is reported after
	// one attempt rather than two.
	if err := db.ValidatePassword(string(first)); err != nil {
		return "", err
	}

	fmt.Print("Repeat password: ")
	second, err := term.ReadPassword(fd)
	fmt.Println()
	if err != nil {
		return "", fmt.Errorf("read password: %w", err)
	}

	if string(first) != string(second) {
		return "", errors.New("the two passwords do not match; nothing was changed")
	}
	return string(first), nil
}

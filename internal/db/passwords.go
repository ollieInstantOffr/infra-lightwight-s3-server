package db

import (
	"context"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"
)

var (
	// ErrPasswordIncorrect means the address exists but the password does not
	// match, or the address does not exist at all. Callers must not tell those
	// apart to the outside world — see VerifyPassword.
	ErrPasswordIncorrect = errors.New("email or password is incorrect")
	// ErrPasswordUnset means the account has no password yet, so nobody can
	// sign in as it until an administrator or the CLI sets one.
	ErrPasswordUnset = errors.New("no password is set for this account")
	// ErrPasswordTooShort is returned before hashing, so a password that cannot
	// be stored is rejected cheaply.
	ErrPasswordTooShort = errors.New("password is too short")
	// ErrPasswordTooLong guards bcrypt's own limit, which silently truncates
	// rather than failing — two different long passwords sharing a 72-byte
	// prefix would otherwise be interchangeable.
	ErrPasswordTooLong = errors.New("password is too long")
)

const (
	// MinPasswordLength is deliberately a length floor and nothing else.
	// Composition rules push people towards predictable substitutions and a
	// written-down password; length is the property that actually costs an
	// attacker work.
	MinPasswordLength = 12

	// MaxPasswordLength is bcrypt's hard limit. Anything beyond 72 bytes is
	// ignored by the algorithm, so accepting it would be a lie.
	MaxPasswordLength = 72

	// bcryptCost is the work factor. Each increment doubles the time to verify.
	// 12 puts a single verification in the low hundreds of milliseconds on
	// current hardware: slow enough to make offline guessing expensive, fast
	// enough that a sign-in does not feel broken.
	bcryptCost = 12
)

// dummyHash is verified against when no account exists, so a miss costs the
// same time as a hit.
//
// Without it, an unknown address returns in microseconds while a known one
// takes the full bcrypt cost, and the difference is measurable over the
// network. That turns the sign-in endpoint into an account enumerator, which
// is the exact thing the magic-link endpoint went out of its way to avoid.
var dummyHash []byte

func init() {
	// Cost must match the real one, or the timing still differs.
	h, err := bcrypt.GenerateFromPassword([]byte("timing-equalisation-placeholder"), bcryptCost)
	if err != nil {
		panic("bcrypt is unusable: " + err.Error())
	}
	dummyHash = h
}

// ValidatePassword reports whether a password may be stored, without hashing
// it. Length is measured in bytes because that is what bcrypt truncates on;
// a 30-character password of multi-byte runes can exceed 72 bytes.
func ValidatePassword(password string) error {
	if utf8.RuneCountInString(password) < MinPasswordLength {
		return fmt.Errorf("%w: use at least %d characters", ErrPasswordTooShort, MinPasswordLength)
	}
	if len(password) > MaxPasswordLength {
		return fmt.Errorf("%w: %d bytes is over the %d-byte limit",
			ErrPasswordTooLong, len(password), MaxPasswordLength)
	}
	return nil
}

// HashPassword produces a storable hash, rejecting anything that cannot be
// stored faithfully.
func HashPassword(password string) ([]byte, error) {
	if err := ValidatePassword(password); err != nil {
		return nil, err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return nil, fmt.Errorf("hash password: %w", err)
	}
	return hash, nil
}

// SetPassword stores a new password for a user.
//
// mustChange marks a password somebody else chose. An administrator setting a
// starting password knows it, so it is a shared secret until the owner
// replaces it; the flag is what forces that to happen.
func SetPassword(ctx context.Context, q Querier, userID, password string, mustChange bool) error {
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	tag, err := q.Exec(ctx, `
		UPDATE users
		SET password_hash = $2, password_set_at = now(), must_change_password = $3
		WHERE id = $1`, userID, hash, mustChange)
	if err != nil {
		return fmt.Errorf("set password: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrUserNotFound
	}
	return nil
}

// SetPasswordByEmail is the same, addressed the way the CLI addresses users.
func SetPasswordByEmail(ctx context.Context, q Querier, email, password string, mustChange bool) error {
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	tag, err := q.Exec(ctx, `
		UPDATE users
		SET password_hash = $2, password_set_at = now(), must_change_password = $3
		WHERE email = $1`, NormalizeEmail(email), hash, mustChange)
	if err != nil {
		return fmt.Errorf("set password: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrUserNotFound
	}
	return nil
}

// VerifyPassword checks a password and returns the account it belongs to.
//
// Every failure returns ErrPasswordIncorrect regardless of cause, and every
// call performs a bcrypt comparison — including one for an address with no
// account. Callers must pass that error through unchanged: distinguishing
// "no such user" from "wrong password" tells an unauthenticated caller which
// addresses have accounts.
//
// ErrPasswordUnset is the one exception, and is only safe because it is
// reported to an operator reading the log, never to the caller.
func VerifyPassword(ctx context.Context, q Querier, email, password string) (*User, error) {
	user := &User{}
	var hash []byte
	err := q.QueryRow(ctx, `
		SELECT id::text, email, role, created_at, last_login_at,
		       password_hash, must_change_password
		FROM users WHERE email = $1`, NormalizeEmail(email),
	).Scan(&user.ID, &user.Email, &user.Role, &user.CreatedAt, &user.LastLoginAt,
		&hash, &user.MustChangePassword)

	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// Spend the same time as a real comparison before failing.
		_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
		return nil, ErrPasswordIncorrect
	case err != nil:
		return nil, fmt.Errorf("look up user for sign-in: %w", err)
	}

	if len(hash) == 0 {
		_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
		return nil, ErrPasswordUnset
	}
	if err := bcrypt.CompareHashAndPassword(hash, []byte(password)); err != nil {
		return nil, ErrPasswordIncorrect
	}
	return user, nil
}

// HasPassword reports whether an account can sign in at all. Used at startup to
// warn that the bootstrap administrator still needs one.
func HasPassword(ctx context.Context, q Querier, email string) (bool, error) {
	var present bool
	err := q.QueryRow(ctx,
		`SELECT password_hash IS NOT NULL FROM users WHERE email = $1`,
		NormalizeEmail(email)).Scan(&present)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrUserNotFound
	}
	if err != nil {
		return false, fmt.Errorf("check password presence: %w", err)
	}
	return present, nil
}

// ClearMustChangePassword lifts the forced-change flag. Separate from
// SetPassword so the flag can be cleared without touching the hash.
func ClearMustChangePassword(ctx context.Context, q Querier, userID string) error {
	if _, err := q.Exec(ctx,
		`UPDATE users SET must_change_password = false WHERE id = $1`, userID); err != nil {
		return fmt.Errorf("clear forced password change: %w", err)
	}
	return nil
}

// RecordLoginAttempt notes an attempt for throttling and for the audit trail.
func RecordLoginAttempt(ctx context.Context, q Querier, email, ip string, successful bool) error {
	if _, err := q.Exec(ctx,
		`INSERT INTO login_attempts (email, ip, successful) VALUES ($1, $2, $3)`,
		NormalizeEmail(email), nullableIP(ip), successful); err != nil {
		return fmt.Errorf("record login attempt: %w", err)
	}
	return nil
}

// CountRecentFailures reports failed attempts against an address and from an IP
// inside a window.
//
// Both are returned from one round trip because the sign-in path is the one
// place a second query costs a user waiting. They are counted separately: an
// attacker guessing at one account must not be able to lock its owner out by
// tripping a shared counter.
func CountRecentFailures(ctx context.Context, q Querier, email, ip string, window time.Duration) (byEmail, byIP int, err error) {
	err = q.QueryRow(ctx, `
		SELECT
			count(*) FILTER (WHERE email = $1),
			count(*) FILTER (WHERE ip = $2::inet AND $2 IS NOT NULL)
		FROM login_attempts
		WHERE NOT successful AND at > now() - $3::interval`,
		NormalizeEmail(email), nullableIP(ip), window.String(),
	).Scan(&byEmail, &byIP)
	if err != nil {
		return 0, 0, fmt.Errorf("count recent sign-in failures: %w", err)
	}
	return byEmail, byIP, nil
}

// ClearLoginFailures forgets an address's failures, so a successful sign-in
// does not leave the account part-way to a lockout.
func ClearLoginFailures(ctx context.Context, q Querier, email string) error {
	if _, err := q.Exec(ctx,
		`DELETE FROM login_attempts WHERE email = $1 AND NOT successful`,
		NormalizeEmail(email)); err != nil {
		return fmt.Errorf("clear sign-in failures: %w", err)
	}
	return nil
}

// PurgeLoginAttempts drops attempts older than the retention window. Without
// it the table grows one row per sign-in forever.
func PurgeLoginAttempts(ctx context.Context, q Querier, retain time.Duration) error {
	if _, err := q.Exec(ctx,
		`DELETE FROM login_attempts WHERE at < now() - $1::interval`, retain.String()); err != nil {
		return fmt.Errorf("purge login attempts: %w", err)
	}
	return nil
}

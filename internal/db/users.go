package db

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

var (
	// ErrUserNotFound means no account exists for that address.
	ErrUserNotFound = errors.New("user not found")
	// ErrLastAdmin means the operation would leave the console with no admin,
	// which would lock everyone out of user management permanently.
	ErrLastAdmin = errors.New("cannot remove or demote the last admin")
)

// Roles a console user can hold.
const (
	RoleAdmin  = "ADMIN"
	RoleMember = "MEMBER"
)

// User is a console operator. There is no password column: authentication is
// entirely by emailed link, so there is no password to leak.
type User struct {
	ID          string
	Email       string
	Role        string
	CreatedAt   time.Time
	LastLoginAt *time.Time
}

// IsAdmin reports whether the user may manage other users and credentials.
func (u *User) IsAdmin() bool { return u.Role == RoleAdmin }

// NormalizeEmail lowercases and trims an address.
//
// Addresses are stored lowercased and compared that way throughout. Without
// this, Ollie@example.com and ollie@example.com would be two accounts, and an
// invite sent to one would not admit the other.
func NormalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// GetUserByEmail looks up an account.
func GetUserByEmail(ctx context.Context, q Querier, email string) (*User, error) {
	user := &User{}
	err := q.QueryRow(ctx, `
		SELECT id::text, email, role, created_at, last_login_at
		FROM users WHERE email = $1`, NormalizeEmail(email),
	).Scan(&user.ID, &user.Email, &user.Role, &user.CreatedAt, &user.LastLoginAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get user: %w", err)
	}
	return user, nil
}

// GetUserByID looks up an account by its identifier.
func GetUserByID(ctx context.Context, q Querier, id string) (*User, error) {
	user := &User{}
	err := q.QueryRow(ctx, `
		SELECT id::text, email, role, created_at, last_login_at
		FROM users WHERE id = $1`, id,
	).Scan(&user.ID, &user.Email, &user.Role, &user.CreatedAt, &user.LastLoginAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get user: %w", err)
	}
	return user, nil
}

// EnsureAdmin creates or promotes the bootstrap administrator.
//
// Called on every startup with ADMIN_EMAIL. Promoting on each boot rather than
// only on creation is deliberate: it is the documented way back in if the last
// admin is demoted or removed by accident.
func EnsureAdmin(ctx context.Context, q Querier, email string) (*User, error) {
	user := &User{}
	err := q.QueryRow(ctx, `
		INSERT INTO users (email, role) VALUES ($1, $2)
		ON CONFLICT (email) DO UPDATE SET role = $2
		RETURNING id::text, email, role, created_at, last_login_at`,
		NormalizeEmail(email), RoleAdmin,
	).Scan(&user.ID, &user.Email, &user.Role, &user.CreatedAt, &user.LastLoginAt)
	if err != nil {
		return nil, fmt.Errorf("ensure bootstrap admin: %w", err)
	}
	return user, nil
}

// CreateUser adds an account.
func CreateUser(ctx context.Context, q Querier, email, role string) (*User, error) {
	user := &User{}
	err := q.QueryRow(ctx, `
		INSERT INTO users (email, role) VALUES ($1, $2)
		ON CONFLICT (email) DO UPDATE SET email = EXCLUDED.email
		RETURNING id::text, email, role, created_at, last_login_at`,
		NormalizeEmail(email), role,
	).Scan(&user.ID, &user.Email, &user.Role, &user.CreatedAt, &user.LastLoginAt)
	if err != nil {
		return nil, fmt.Errorf("create user: %w", err)
	}
	return user, nil
}

// ListUsers returns every account, oldest first.
func ListUsers(ctx context.Context, q Querier) ([]User, error) {
	rows, err := q.Query(ctx, `
		SELECT id::text, email, role, created_at, last_login_at
		FROM users ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()

	var out []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Email, &u.Role, &u.CreatedAt, &u.LastLoginAt); err != nil {
			return nil, fmt.Errorf("scan user: %w", err)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// RecordLogin stamps a successful sign-in.
func RecordLogin(ctx context.Context, q Querier, userID string) error {
	if _, err := q.Exec(ctx, `UPDATE users SET last_login_at = now() WHERE id = $1`, userID); err != nil {
		return fmt.Errorf("record login: %w", err)
	}
	return nil
}

// SetUserRole changes a user's role, refusing to demote the last admin.
//
// The count and the update happen in one statement so two concurrent demotions
// cannot each see a second admin that the other is removing.
func SetUserRole(ctx context.Context, q Querier, userID, role string) error {
	if role != RoleAdmin && role != RoleMember {
		return fmt.Errorf("unknown role %q", role)
	}

	var updated bool
	err := q.QueryRow(ctx, `
		WITH changed AS (
			UPDATE users SET role = $2
			WHERE id = $1
			  AND ($2 = 'ADMIN' OR role <> 'ADMIN'
			       OR (SELECT count(*) FROM users WHERE role = 'ADMIN') > 1)
			RETURNING id
		)
		SELECT EXISTS (SELECT 1 FROM changed)`, userID, role).Scan(&updated)
	if err != nil {
		return fmt.Errorf("set user role: %w", err)
	}
	if updated {
		return nil
	}
	if _, err := GetUserByID(ctx, q, userID); err != nil {
		return err
	}
	return ErrLastAdmin
}

// DeleteUser removes an account, refusing to remove the last admin.
func DeleteUser(ctx context.Context, q Querier, userID string) error {
	var deleted bool
	err := q.QueryRow(ctx, `
		WITH removed AS (
			DELETE FROM users
			WHERE id = $1
			  AND (role <> 'ADMIN' OR (SELECT count(*) FROM users WHERE role = 'ADMIN') > 1)
			RETURNING id
		)
		SELECT EXISTS (SELECT 1 FROM removed)`, userID).Scan(&deleted)
	if err != nil {
		return fmt.Errorf("delete user: %w", err)
	}
	if deleted {
		return nil
	}
	if _, err := GetUserByID(ctx, q, userID); err != nil {
		return err
	}
	return ErrLastAdmin
}

// Token generation and hashing.
//
// Every emailed or cookie-borne token is stored only as a SHA-256 hash. The
// plaintext exists in the email or the cookie and nowhere else, so a database
// leak cannot be replayed into a session. SHA-256 rather than a password hash
// is right here: these are 256-bit random values, so there is no guessing
// attack for a work factor to slow down.

// tokenBytes is the entropy in each token: 256 bits.
const tokenBytes = 32

// NewToken returns a URL-safe random token and its hash.
func NewToken() (token string, hash []byte, err error) {
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", nil, fmt.Errorf("generate token: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(buf)
	return token, HashToken(token), nil
}

// HashToken hashes a token for storage and lookup.
func HashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

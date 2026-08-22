package db

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/jackc/pgx/v5"
)

var (
	// ErrTokenInvalid means the token is unknown, already used, or expired.
	// The three are deliberately not distinguished: telling a caller which
	// would confirm that a token once existed.
	ErrTokenInvalid = errors.New("token is invalid or has expired")
	// ErrSessionInvalid means the session is unknown, revoked or expired.
	ErrSessionInvalid = errors.New("session is invalid or has expired")
	// ErrInviteInvalid means the invite is unknown, revoked, used or expired.
	ErrInviteInvalid = errors.New("invite is invalid or has expired")
)

// CreateMagicLink records a login token for an address.
func CreateMagicLink(ctx context.Context, q Querier, email string, hash []byte, ttl time.Duration, ip string) error {
	_, err := q.Exec(ctx, `
		INSERT INTO magic_links (email, token_hash, expires_at, request_ip)
		VALUES ($1, $2, now() + $3::interval, $4)`,
		NormalizeEmail(email), hash, ttl.String(), nullableIP(ip))
	if err != nil {
		return fmt.Errorf("create magic link: %w", err)
	}
	return nil
}

// ConsumeMagicLink redeems a login token and returns the address it was issued
// for.
//
// The lookup and the consumption are one statement: a token that two requests
// redeem simultaneously must succeed exactly once, and a select followed by an
// update would let both through.
func ConsumeMagicLink(ctx context.Context, q Querier, hash []byte) (email string, err error) {
	err = q.QueryRow(ctx, `
		UPDATE magic_links SET consumed_at = now()
		WHERE token_hash = $1 AND consumed_at IS NULL AND expires_at > now()
		RETURNING email`, hash).Scan(&email)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrTokenInvalid
	}
	if err != nil {
		return "", fmt.Errorf("consume magic link: %w", err)
	}
	return email, nil
}

// CountRecentMagicLinks reports how many links were issued for an address
// inside a window, which is what bounds mailbox flooding.
func CountRecentMagicLinks(ctx context.Context, q Querier, email string, window time.Duration) (int, error) {
	var count int
	err := q.QueryRow(ctx, `
		SELECT count(*) FROM magic_links
		WHERE email = $1 AND created_at > now() - $2::interval`,
		NormalizeEmail(email), window.String()).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count recent magic links: %w", err)
	}
	return count, nil
}

// Session is an authenticated console session.
type Session struct {
	ID                string
	UserID            string
	IdleExpiresAt     time.Time
	AbsoluteExpiresAt time.Time
}

// CreateSession opens a session for a user.
//
// Two expiries are kept. Idle expiry slides forward on each use so an active
// operator is not logged out mid-task; absolute expiry never moves, so a stolen
// cookie cannot be kept alive indefinitely by using it.
func CreateSession(ctx context.Context, q Querier, userID string, hash []byte, idle, absolute time.Duration, userAgent, ip string) (*Session, error) {
	session := &Session{UserID: userID}
	err := q.QueryRow(ctx, `
		INSERT INTO sessions (user_id, token_hash, idle_expires_at, absolute_expires_at, user_agent, ip)
		VALUES ($1, $2, now() + $3::interval, now() + $4::interval, $5, $6)
		RETURNING id::text, idle_expires_at, absolute_expires_at`,
		userID, hash, idle.String(), absolute.String(), truncate(userAgent, 512), nullableIP(ip),
	).Scan(&session.ID, &session.IdleExpiresAt, &session.AbsoluteExpiresAt)
	if err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}
	return session, nil
}

// TouchSession validates a session and slides its idle expiry forward,
// returning the user it belongs to.
//
// Validation and renewal are one statement so a session cannot be renewed on
// the strength of a check that has already gone stale.
func TouchSession(ctx context.Context, q Querier, hash []byte, idle time.Duration) (*User, error) {
	user := &User{}
	err := q.QueryRow(ctx, `
		WITH renewed AS (
			UPDATE sessions
			SET last_seen_at = now(),
			    idle_expires_at = LEAST(now() + $2::interval, absolute_expires_at)
			WHERE token_hash = $1
			  AND revoked_at IS NULL
			  AND idle_expires_at > now()
			  AND absolute_expires_at > now()
			RETURNING user_id
		)
		SELECT u.id::text, u.email, u.role, u.created_at, u.last_login_at
		FROM users u JOIN renewed ON renewed.user_id = u.id`,
		hash, idle.String(),
	).Scan(&user.ID, &user.Email, &user.Role, &user.CreatedAt, &user.LastLoginAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrSessionInvalid
	}
	if err != nil {
		return nil, fmt.Errorf("touch session: %w", err)
	}
	return user, nil
}

// RevokeSession ends a session immediately.
func RevokeSession(ctx context.Context, q Querier, hash []byte) error {
	if _, err := q.Exec(ctx,
		`UPDATE sessions SET revoked_at = now() WHERE token_hash = $1 AND revoked_at IS NULL`,
		hash); err != nil {
		return fmt.Errorf("revoke session: %w", err)
	}
	return nil
}

// RevokeUserSessions ends every session a user holds, which is what must happen
// when their account is removed.
func RevokeUserSessions(ctx context.Context, q Querier, userID string) error {
	if _, err := q.Exec(ctx,
		`UPDATE sessions SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`,
		userID); err != nil {
		return fmt.Errorf("revoke user sessions: %w", err)
	}
	return nil
}

// Invite is a pending invitation.
type Invite struct {
	ID        string
	Email     string
	Role      string
	InvitedBy *string
	CreatedAt time.Time
	ExpiresAt time.Time
}

// CreateInvite issues an invitation, replacing any pending one for the address.
//
// Replacing rather than accumulating matters: a partial unique index allows one
// live invite per address, so re-inviting someone does not leave several working
// tokens in several inboxes.
func CreateInvite(ctx context.Context, q Querier, email, role string, invitedBy *string, hash []byte, ttl time.Duration) (*Invite, error) {
	normalized := NormalizeEmail(email)

	// Withdraw any existing pending invite so the new one is the only way in.
	if _, err := q.Exec(ctx,
		`UPDATE invites SET revoked_at = now()
		 WHERE email = $1 AND accepted_at IS NULL AND revoked_at IS NULL`,
		normalized); err != nil {
		return nil, fmt.Errorf("withdraw previous invite: %w", err)
	}

	invite := &Invite{Email: normalized, Role: role, InvitedBy: invitedBy}
	err := q.QueryRow(ctx, `
		INSERT INTO invites (email, token_hash, role, invited_by, expires_at)
		VALUES ($1, $2, $3, $4, now() + $5::interval)
		RETURNING id::text, created_at, expires_at`,
		normalized, hash, role, invitedBy, ttl.String(),
	).Scan(&invite.ID, &invite.CreatedAt, &invite.ExpiresAt)
	if err != nil {
		return nil, fmt.Errorf("create invite: %w", err)
	}
	return invite, nil
}

// AcceptInvite redeems an invitation and returns the address and role it
// grants.
func AcceptInvite(ctx context.Context, q Querier, hash []byte) (email, role string, err error) {
	err = q.QueryRow(ctx, `
		UPDATE invites SET accepted_at = now()
		WHERE token_hash = $1 AND accepted_at IS NULL AND revoked_at IS NULL AND expires_at > now()
		RETURNING email, role`, hash).Scan(&email, &role)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrInviteInvalid
	}
	if err != nil {
		return "", "", fmt.Errorf("accept invite: %w", err)
	}
	return email, role, nil
}

// HasPendingInvite reports whether an address has a live invitation. The login
// endpoint uses it to decide whether an unknown address may still sign in.
func HasPendingInvite(ctx context.Context, q Querier, email string) (bool, error) {
	var exists bool
	err := q.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM invites
			WHERE email = $1 AND accepted_at IS NULL AND revoked_at IS NULL AND expires_at > now()
		)`, NormalizeEmail(email)).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check pending invite: %w", err)
	}
	return exists, nil
}

// ListPendingInvites returns invitations that have not been used or withdrawn.
func ListPendingInvites(ctx context.Context, q Querier) ([]Invite, error) {
	rows, err := q.Query(ctx, `
		SELECT id::text, email, role, invited_by::text, created_at, expires_at
		FROM invites
		WHERE accepted_at IS NULL AND revoked_at IS NULL AND expires_at > now()
		ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list invites: %w", err)
	}
	defer rows.Close()

	var out []Invite
	for rows.Next() {
		var i Invite
		if err := rows.Scan(&i.ID, &i.Email, &i.Role, &i.InvitedBy, &i.CreatedAt, &i.ExpiresAt); err != nil {
			return nil, fmt.Errorf("scan invite: %w", err)
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// RevokeInvite withdraws a pending invitation.
func RevokeInvite(ctx context.Context, q Querier, inviteID string) error {
	tag, err := q.Exec(ctx,
		`UPDATE invites SET revoked_at = now()
		 WHERE id = $1 AND accepted_at IS NULL AND revoked_at IS NULL`, inviteID)
	if err != nil {
		return fmt.Errorf("revoke invite: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrInviteInvalid
	}
	return nil
}

// PurgeExpiredAuthTokens clears spent and expired login artefacts.
//
// These accumulate one row per login attempt forever otherwise. Consumed magic
// links are kept briefly rather than deleted at once, so a user who clicks a
// link twice sees "already used" rather than a puzzling "invalid".
func PurgeExpiredAuthTokens(ctx context.Context, q Querier, retain time.Duration) error {
	if _, err := q.Exec(ctx,
		`DELETE FROM magic_links WHERE expires_at < now() - $1::interval`, retain.String()); err != nil {
		return fmt.Errorf("purge magic links: %w", err)
	}
	if _, err := q.Exec(ctx,
		`DELETE FROM sessions WHERE absolute_expires_at < now() - $1::interval`, retain.String()); err != nil {
		return fmt.Errorf("purge sessions: %w", err)
	}
	return nil
}

// nullableIP converts an address string to something Postgres INET accepts,
// returning nil when it cannot be parsed rather than failing the write. A
// login must not fail because a proxy sent an address in an odd form.
func nullableIP(raw string) *string {
	if raw == "" {
		return nil
	}
	if _, err := netip.ParseAddr(raw); err != nil {
		return nil
	}
	return &raw
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}

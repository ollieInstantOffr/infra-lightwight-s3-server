package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/secrets"
)

var (
	// ErrCredentialNotFound means no such access key id exists. S3 reports this
	// as InvalidAccessKeyId.
	ErrCredentialNotFound = errors.New("credential not found")
	// ErrCredentialRevoked means the key existed but has been revoked. Kept
	// distinct from not-found for logging; both surface to the client as
	// InvalidAccessKeyId so a revoked key is not distinguishable from a
	// fictional one.
	ErrCredentialRevoked = errors.New("credential revoked")
)

// Credential is an S3 access key pair.
type Credential struct {
	ID          string
	AccessKeyID string
	// SecretKey is populated only by LookupCredential and CreateCredential. It
	// is never returned to the console after creation.
	SecretKey   string
	Description string
	OwnerUserID *string
	// Scope is what this key may do. Keys created before scoping existed are
	// unrestricted, and so is a key created without one being supplied.
	Scope      Grant
	CreatedAt  time.Time
	LastUsedAt *time.Time
	RevokedAt  *time.Time
}

// Revoked reports whether the credential has been revoked.
func (c *Credential) Revoked() bool { return c.RevokedAt != nil }

// CreateCredential generates a new access key pair, stores the secret
// encrypted, and returns the credential with the plaintext secret populated.
// That plaintext is the only copy the caller will ever see: it is shown once in
// the console and never recoverable from the API afterwards.
func CreateCredential(
	ctx context.Context,
	q Querier,
	cipher *secrets.Cipher,
	description string,
	ownerUserID *string,
	scope Grant,
) (*Credential, error) {
	if err := scope.Validate(); err != nil {
		return nil, fmt.Errorf("access scope: %w", err)
	}
	accessKeyID, err := secrets.GenerateAccessKeyID()
	if err != nil {
		return nil, err
	}
	secretKey, err := secrets.GenerateSecretKey()
	if err != nil {
		return nil, err
	}
	ciphertext, nonce, err := cipher.Encrypt(secretKey, accessKeyID)
	if err != nil {
		return nil, fmt.Errorf("encrypt credential secret: %w", err)
	}

	encodedScope, err := marshalGrant(scope)
	if err != nil {
		return nil, err
	}

	cred := &Credential{
		AccessKeyID: accessKeyID,
		SecretKey:   secretKey,
		Description: description,
		OwnerUserID: ownerUserID,
		Scope:       scope,
	}
	err = q.QueryRow(ctx, `
		INSERT INTO credentials
		    (access_key_id, secret_ciphertext, secret_nonce, description, owner_user_id, access_scope)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id::text, created_at`,
		accessKeyID, ciphertext, nonce, description, ownerUserID, encodedScope,
	).Scan(&cred.ID, &cred.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("insert credential: %w", err)
	}
	return cred, nil
}

// LookupCredential fetches a credential and decrypts its secret, which SigV4
// needs in order to re-derive the signing key.
func LookupCredential(
	ctx context.Context,
	q Querier,
	cipher *secrets.Cipher,
	accessKeyID string,
) (*Credential, error) {
	var (
		cred         Credential
		ciphertext   []byte
		nonce        []byte
		encodedScope []byte
	)
	err := q.QueryRow(ctx, `
		SELECT id::text, access_key_id, secret_ciphertext, secret_nonce,
		       description, owner_user_id::text, access_scope,
		       created_at, last_used_at, revoked_at
		FROM credentials
		WHERE access_key_id = $1`,
		accessKeyID,
	).Scan(&cred.ID, &cred.AccessKeyID, &ciphertext, &nonce,
		&cred.Description, &cred.OwnerUserID, &encodedScope,
		&cred.CreatedAt, &cred.LastUsedAt, &cred.RevokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrCredentialNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("look up credential: %w", err)
	}
	if cred.Revoked() {
		return nil, ErrCredentialRevoked
	}

	// A scope that cannot be decoded fails the lookup rather than defaulting to
	// anything. Defaulting to unrestricted would turn a corrupted row into a
	// silent privilege escalation; defaulting to no-access would look like a
	// mysterious permission bug. An error says what actually happened.
	cred.Scope, err = unmarshalGrant(encodedScope)
	if err != nil {
		return nil, fmt.Errorf("credential %s: %w", accessKeyID, err)
	}

	// A failure here means CREDENTIALS_KEY has almost certainly changed. It
	// propagates as secrets.ErrUndecryptable so the caller can say that plainly
	// rather than reporting an inscrutable signature mismatch.
	cred.SecretKey, err = cipher.Decrypt(ciphertext, nonce, cred.AccessKeyID)
	if err != nil {
		return nil, fmt.Errorf("credential %s: %w", accessKeyID, err)
	}
	return &cred, nil
}

// touchInterval throttles last_used_at updates. Writing on every request would
// add a database write to every object GET for information that is only ever
// read by a human glancing at the console.
const touchInterval = time.Minute

// TouchCredential records that a credential was used, at most once per
// touchInterval. Best-effort by design: a failure here must never fail an
// otherwise valid S3 request.
func TouchCredential(ctx context.Context, q Querier, accessKeyID string) error {
	_, err := q.Exec(ctx, `
		UPDATE credentials
		SET last_used_at = now()
		WHERE access_key_id = $1
		  AND (last_used_at IS NULL OR last_used_at < now() - $2::interval)`,
		accessKeyID, touchInterval.String())
	if err != nil {
		return fmt.Errorf("touch credential %s: %w", accessKeyID, err)
	}
	return nil
}

// ListCredentials returns every credential without secrets, newest first.
func ListCredentials(ctx context.Context, q Querier) ([]Credential, error) {
	rows, err := q.Query(ctx, `
		SELECT id::text, access_key_id, description, owner_user_id::text,
		       access_scope, created_at, last_used_at, revoked_at
		FROM credentials
		ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list credentials: %w", err)
	}
	defer rows.Close()

	var out []Credential
	for rows.Next() {
		var (
			c            Credential
			encodedScope []byte
		)
		if err := rows.Scan(&c.ID, &c.AccessKeyID, &c.Description, &c.OwnerUserID,
			&encodedScope, &c.CreatedAt, &c.LastUsedAt, &c.RevokedAt); err != nil {
			return nil, fmt.Errorf("scan credential: %w", err)
		}
		if c.Scope, err = unmarshalGrant(encodedScope); err != nil {
			return nil, fmt.Errorf("credential %s: %w", c.AccessKeyID, err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// RevokeCredential marks a credential unusable. It takes effect on the very
// next S3 request, since every request looks the credential up.
func RevokeCredential(ctx context.Context, q Querier, accessKeyID string) error {
	tag, err := q.Exec(ctx, `
		UPDATE credentials SET revoked_at = now()
		WHERE access_key_id = $1 AND revoked_at IS NULL`, accessKeyID)
	if err != nil {
		return fmt.Errorf("revoke credential %s: %w", accessKeyID, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrCredentialNotFound
	}
	return nil
}

// SetCredentialScope replaces what a key is allowed to do.
//
// Narrowing a key that turned out to be too wide is the realistic response to
// discovering the problem, and it has to be possible without reissuing the
// secret — reissuing means coordinating a change with whoever holds it, which
// is exactly the friction that leaves an over-broad key in place.
//
// It takes effect on the next request, since every request looks the credential
// up. That cuts both ways: widening a key is a privilege escalation that is
// live immediately, which is why the console records it in the audit log.
func SetCredentialScope(ctx context.Context, q Querier, accessKeyID string, scope Grant) error {
	if err := scope.Validate(); err != nil {
		return fmt.Errorf("access scope: %w", err)
	}
	encoded, err := marshalGrant(scope)
	if err != nil {
		return err
	}
	tag, err := q.Exec(ctx, `
		UPDATE credentials SET access_scope = $2
		WHERE access_key_id = $1 AND revoked_at IS NULL`, accessKeyID, encoded)
	if err != nil {
		return fmt.Errorf("set scope on credential %s: %w", accessKeyID, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrCredentialNotFound
	}
	return nil
}

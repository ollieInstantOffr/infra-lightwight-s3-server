package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/secrets"
)

// alertEmailKeyAAD binds the stored key to its purpose. The cipher takes the
// access key id as additional data for S3 secrets; there is no such id here, so
// a fixed label serves the same role — a ciphertext lifted from the credentials
// table will not decrypt in this one.
const alertEmailKeyAAD = "app_settings:resend_api_key"

// AlertEmailSettings is how alert notifications are delivered.
//
// It lives in the database rather than the environment so it can be changed
// without a restart, and so an operator can fix a bad API key from the console
// at the moment alerts stop arriving — which is exactly when nobody wants to be
// editing .env and redeploying.
type AlertEmailSettings struct {
	Enabled bool
	From    string
	// APIKey is decrypted on read. It is never returned to a browser; the
	// console reports only whether one is present.
	APIKey    string
	UpdatedAt time.Time
	UpdatedBy *string
}

// Configured reports whether email can actually be sent.
func (s AlertEmailSettings) Configured() bool {
	return s.Enabled && s.APIKey != "" && s.From != ""
}

// GetAlertEmailSettings reads the single settings row.
//
// A missing row is not an error: it means nothing has been configured, which
// is the state a fresh deployment is in and is reported as disabled.
func GetAlertEmailSettings(ctx context.Context, q Querier, cipher *secrets.Cipher) (AlertEmailSettings, error) {
	var (
		settings  AlertEmailSettings
		encrypted []byte
		nonce     []byte
	)
	err := q.QueryRow(ctx, `
		SELECT resend_enabled, resend_from, resend_api_key, resend_api_key_nonce,
		       updated_at, updated_by::text
		FROM app_settings WHERE id = true`,
	).Scan(&settings.Enabled, &settings.From, &encrypted, &nonce,
		&settings.UpdatedAt, &settings.UpdatedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return AlertEmailSettings{}, nil
	}
	if err != nil {
		return AlertEmailSettings{}, fmt.Errorf("read alert email settings: %w", err)
	}

	if len(encrypted) > 0 {
		key, err := cipher.Decrypt(encrypted, nonce, alertEmailKeyAAD)
		if err != nil {
			// A key that cannot be decrypted means CREDENTIALS_KEY has changed.
			// Reporting it is better than silently behaving as if no key were
			// set, which would look like alerts quietly not being configured.
			return settings, fmt.Errorf("decrypt the stored API key "+
				"(has CREDENTIALS_KEY changed?): %w", err)
		}
		settings.APIKey = key
	}
	return settings, nil
}

// SaveAlertEmailSettings writes the settings row.
//
// An empty apiKey leaves the stored one untouched, so the console can save a
// change to the from-address or the enabled flag without ever having to send
// the key back to the browser and receive it again.
func SaveAlertEmailSettings(ctx context.Context, q Querier, cipher *secrets.Cipher,
	enabled bool, from, apiKey string, updatedBy string) error {

	if apiKey == "" {
		_, err := q.Exec(ctx, `
			INSERT INTO app_settings (id, resend_enabled, resend_from, updated_at, updated_by)
			VALUES (true, $1, $2, now(), $3)
			ON CONFLICT (id) DO UPDATE
			SET resend_enabled = $1, resend_from = $2, updated_at = now(), updated_by = $3`,
			enabled, from, nullableUUID(updatedBy))
		if err != nil {
			return fmt.Errorf("save alert email settings: %w", err)
		}
		return nil
	}

	encrypted, nonce, err := cipher.Encrypt(apiKey, alertEmailKeyAAD)
	if err != nil {
		return fmt.Errorf("encrypt the API key: %w", err)
	}
	_, err = q.Exec(ctx, `
		INSERT INTO app_settings (id, resend_enabled, resend_from, resend_api_key,
		                          resend_api_key_nonce, updated_at, updated_by)
		VALUES (true, $1, $2, $3, $4, now(), $5)
		ON CONFLICT (id) DO UPDATE
		SET resend_enabled = $1, resend_from = $2, resend_api_key = $3,
		    resend_api_key_nonce = $4, updated_at = now(), updated_by = $5`,
		enabled, from, encrypted, nonce, nullableUUID(updatedBy))
	if err != nil {
		return fmt.Errorf("save alert email settings: %w", err)
	}
	return nil
}

// ClearAlertEmailKey removes the stored key, which is the only way to stop
// holding one at all.
func ClearAlertEmailKey(ctx context.Context, q Querier) error {
	if _, err := q.Exec(ctx,
		`UPDATE app_settings
		 SET resend_api_key = NULL, resend_api_key_nonce = NULL,
		     resend_enabled = false, updated_at = now()
		 WHERE id = true`); err != nil {
		return fmt.Errorf("clear the alert email key: %w", err)
	}
	return nil
}

// nullableUUID keeps an empty actor out of a UUID column, which would
// otherwise be a type error rather than a null.
func nullableUUID(id string) *string {
	if id == "" {
		return nil
	}
	return &id
}

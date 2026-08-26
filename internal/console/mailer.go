// Package console serves the admin UI and the session-authenticated API behind
// it.
//
// It is deliberately separate from the S3 API: the console authenticates with a
// session cookie, the S3 API with SigV4, and they listen on different ports so
// bucket paths can never collide with console routes.
package console

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/db"
	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/secrets"
)

// Mailer sends the console's outbound email.
type Mailer interface {
	Send(ctx context.Context, to, subject, textBody, htmlBody string) error
}

const (
	resendEndpoint = "https://api.resend.com/emails"
	// resendTimeout bounds an outbound send. A login must not hang on a slow
	// third party; failing quickly lets the handler report a real error.
	resendTimeout = 15 * time.Second
)

// ResendMailer sends through Resend.
type ResendMailer struct {
	APIKey string
	From   string
	Client *http.Client
}

// NewResendMailer builds a mailer with a bounded HTTP client.
func NewResendMailer(apiKey, from string) *ResendMailer {
	return &ResendMailer{
		APIKey: apiKey,
		From:   from,
		Client: &http.Client{Timeout: resendTimeout},
	}
}

type resendRequest struct {
	From    string   `json:"from"`
	To      []string `json:"to"`
	Subject string   `json:"subject"`
	Text    string   `json:"text"`
	HTML    string   `json:"html"`
}

// Send delivers one message.
func (m *ResendMailer) Send(ctx context.Context, to, subject, textBody, htmlBody string) error {
	payload, err := json.Marshal(resendRequest{
		From: m.From, To: []string{to}, Subject: subject, Text: textBody, HTML: htmlBody,
	})
	if err != nil {
		return fmt.Errorf("encode email: %w", err)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, resendEndpoint, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("build email request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+m.APIKey)
	request.Header.Set("Content-Type", "application/json")

	response, err := m.Client.Do(request)
	if err != nil {
		return fmt.Errorf("send email: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode >= 300 {
		// Resend's own error text is the only useful diagnosis for a rejected
		// send — a wrong from-address or an unverified domain, typically — so
		// it is carried through to the operator's log rather than swallowed.
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4<<10))
		return fmt.Errorf("resend rejected the message (%d): %s", response.StatusCode, body)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
	return nil
}

// LogMailer prints messages instead of sending them.
//
// This is what makes local development possible before a Resend key exists: the
// login link goes to the log, so the flow can be exercised end to end without
// any third party involved. It is only ever selected when RESEND_API_KEY is
// unset, and startup warns when that happens outside development.
type LogMailer struct {
	Log *slog.Logger
}

func (m *LogMailer) Send(_ context.Context, to, subject, textBody, _ string) error {
	m.Log.Warn("email not sent: no RESEND_API_KEY configured, printing instead",
		"to", to, "subject", subject, "body", textBody)
	return nil
}

// SettingsMailer resolves its configuration from the database on every send.
//
// The mailer it replaced was built once at startup from RESEND_API_KEY, so
// correcting a bad key meant editing .env and redeploying — at exactly the
// moment alerts had stopped arriving. Reading per send costs one indexed query
// on a path that only runs when an alert fires, and makes the console's
// settings screen take effect immediately.
type SettingsMailer struct {
	DB     *db.Pool
	Cipher *secrets.Cipher
	Log    *slog.Logger

	// envAPIKey and envFrom are the initial values from the environment. They
	// apply only until something is saved in the console, so an existing
	// deployment keeps working across the upgrade without being edited.
	envAPIKey string
	envFrom   string
}

// NewSettingsMailer builds a mailer backed by the settings table.
func NewSettingsMailer(pool *db.Pool, cipher *secrets.Cipher, log *slog.Logger, envAPIKey, envFrom string) *SettingsMailer {
	return &SettingsMailer{DB: pool, Cipher: cipher, Log: log, envAPIKey: envAPIKey, envFrom: envFrom}
}

// ErrMailNotConfigured means nothing has been set up to send with. It is a
// distinct error because the alert engine treats it as "nowhere to deliver"
// rather than a delivery failure worth retrying.
var ErrMailNotConfigured = errors.New("no email provider is configured")

// Resolve returns the effective settings, falling back to the environment
// while nothing has been saved.
func (m *SettingsMailer) Resolve(ctx context.Context) (db.AlertEmailSettings, error) {
	settings, err := db.GetAlertEmailSettings(ctx, m.DB, m.Cipher)
	if err != nil {
		return db.AlertEmailSettings{}, err
	}
	if settings.APIKey == "" && m.envAPIKey != "" {
		// Nothing saved yet: the environment is the initial value. Enabled is
		// implied, because a deployment that set RESEND_API_KEY was already
		// sending mail and must not stop on upgrade.
		settings.APIKey = m.envAPIKey
		settings.Enabled = true
		if settings.From == "" {
			settings.From = m.envFrom
		}
	}
	return settings, nil
}

// Send delivers a message using whatever is currently configured.
func (m *SettingsMailer) Send(ctx context.Context, to, subject, textBody, htmlBody string) error {
	settings, err := m.Resolve(ctx)
	if err != nil {
		return err
	}
	if !settings.Configured() {
		return ErrMailNotConfigured
	}
	sender := &ResendMailer{
		APIKey: settings.APIKey,
		From:   settings.From,
		Client: &http.Client{Timeout: resendTimeout},
	}
	return sender.Send(ctx, to, subject, textBody, htmlBody)
}

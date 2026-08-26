package console

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/db"
)

// handleGetBucketSettings returns a bucket's configuration, plus the numbers an
// operator needs before changing it.
func (s *Server) handleGetBucketSettings(w http.ResponseWriter, r *http.Request) {
	bucket, ok := s.requireBucket(w, r)
	if !ok {
		return
	}

	settings, err := db.GetBucketSettings(r.Context(), s.DB, bucket.ID)
	if err != nil {
		s.internalError(w, r, "read bucket settings", err)
		return
	}

	// How much space old versions are holding. Turning versioning on is easy;
	// noticing months later that nothing has been reclaimed is not, so the
	// number is shown next to the switch.
	versionBytes, versionCount, err := db.VersionedSpace(r.Context(), s.DB, bucket.ID)
	if err != nil {
		s.internalError(w, r, "measure versioned space", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"bucket":     bucket.Name,
		"publicRead": settings.PublicRead,
		// The console shows a switch; the state behind it is reported too, so
		// the difference between suspended and never-enabled is visible to
		// anyone who needs it without complicating the control.
		"versioning":        settings.Versioning.Versioned(),
		"versioningState":   string(settings.Versioning),
		"corsRules":         settings.CORSRules,
		"lifecycleRules":    settings.Lifecycle,
		"updatedAt":         settings.UpdatedAt,
		"versionedBytes":    versionBytes,
		"versionCount":      versionCount,
		"publicReadWarning": "Anyone who knows an object's URL can read it, with no credentials. Anonymous writes are never permitted.",
	})
}

type bucketSettingsRequest struct {
	PublicRead bool `json:"publicRead"`
	// Versioning stays a boolean on the console's wire format. The three-state
	// distinction exists for S3 clients, and asking someone clicking a toggle
	// to understand the difference between suspended and never-enabled would be
	// exposing an implementation detail of the API rather than a choice.
	// Turning it off suspends, which is the only thing S3 permits once it has
	// been on.
	Versioning     bool               `json:"versioning"`
	CORSRules      []db.CORSRule      `json:"corsRules"`
	LifecycleRules []db.LifecycleRule `json:"lifecycleRules"`
}

// handleSaveBucketSettings updates a bucket's configuration.
func (s *Server) handleSaveBucketSettings(w http.ResponseWriter, r *http.Request) {
	bucket, ok := s.requireBucket(w, r)
	if !ok {
		return
	}

	var request bucketSettingsRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "Send a JSON body with the bucket's settings.")
		return
	}

	if message := validateCORS(request.CORSRules); message != "" {
		writeError(w, http.StatusBadRequest, message)
		return
	}
	if message := validateLifecycle(request.LifecycleRules); message != "" {
		writeError(w, http.StatusBadRequest, message)
		return
	}

	previous, err := db.GetBucketSettings(r.Context(), s.DB, bucket.ID)
	if err != nil {
		s.internalError(w, r, "read bucket settings", err)
		return
	}

	versioning := previous.Versioning
	if request.Versioning {
		versioning = db.VersioningEnabled
	} else if previous.Versioning.Configured() {
		// Never back to unversioned: S3 does not allow it, and a bucket that
		// claimed never to have had versions while holding version ids already
		// handed out would be lying to its clients.
		versioning = db.VersioningSuspended
	}

	settings := &db.BucketSettings{
		BucketID:   bucket.ID,
		PublicRead: request.PublicRead,
		Versioning: versioning,
		CORSRules:  request.CORSRules,
		Lifecycle:  request.LifecycleRules,
	}
	if settings.CORSRules == nil {
		settings.CORSRules = []db.CORSRule{}
	}
	if settings.Lifecycle == nil {
		settings.Lifecycle = []db.LifecycleRule{}
	}

	if err := db.SaveBucketSettings(r.Context(), s.DB, settings); err != nil {
		s.internalError(w, r, "save bucket settings", err)
		return
	}

	// Recorded with before-and-after, because "who made this bucket public"
	// is exactly the question an audit log exists to answer.
	s.audit(r, db.ActionBucketSettings, "bucket", bucket.Name, map[string]any{
		"publicRead": map[string]bool{"from": previous.PublicRead, "to": settings.PublicRead},
		"versioning": map[string]string{
			"from": string(previous.Versioning), "to": string(settings.Versioning)},
		"corsRules": len(settings.CORSRules),
		"lifecycle": len(settings.Lifecycle),
	})

	writeJSON(w, http.StatusOK, map[string]any{"message": "Settings saved."})
}

// validateCORS rejects rules that would not do what they appear to.
func validateCORS(rules []db.CORSRule) string {
	for i, rule := range rules {
		if len(rule.AllowedOrigins) == 0 {
			return "Every CORS rule needs at least one allowed origin."
		}
		if len(rule.AllowedMethods) == 0 {
			return "Every CORS rule needs at least one allowed method."
		}
		for _, origin := range rule.AllowedOrigins {
			if origin != "*" && !strings.HasPrefix(origin, "http://") && !strings.HasPrefix(origin, "https://") {
				return "Origins must be a full scheme and host, such as https://app.example.com, or * for any."
			}
		}
		for _, method := range rule.AllowedMethods {
			switch strings.ToUpper(method) {
			case http.MethodGet, http.MethodHead, http.MethodPut, http.MethodPost, http.MethodDelete:
			default:
				return "CORS methods must be GET, HEAD, PUT, POST or DELETE."
			}
		}
		if rule.MaxAgeSeconds < 0 {
			return "A CORS rule's max age cannot be negative."
		}
		_ = i
	}
	return ""
}

// validateLifecycle rejects rules that would delete more than intended.
func validateLifecycle(rules []db.LifecycleRule) string {
	for _, rule := range rules {
		if rule.ExpireDays <= 0 {
			return "A lifecycle rule must expire objects after at least one day."
		}
		// A rule is allowed to cover the whole bucket, but it has to be typed
		// deliberately rather than arrived at by leaving a field blank.
		if rule.ID == "" {
			return "Every lifecycle rule needs a name."
		}
	}
	return ""
}

// Alert email settings.
//
// The provider API key is write-only over this API. It goes in, and the only
// thing that ever comes back out is whether one is present — a settings screen
// that redisplays a secret turns every "view settings" into a way to read it.

type alertEmailRequest struct {
	Enabled bool   `json:"enabled"`
	From    string `json:"from"`
	// APIKey empty means "leave the stored one alone", which is what lets the
	// screen save a change to the other fields without ever holding the key.
	APIKey string `json:"apiKey"`
}

// handleGetAlertEmailSettings reports the current configuration.
func (s *Server) handleGetAlertEmailSettings(w http.ResponseWriter, r *http.Request) {
	settings, err := s.resolveAlertEmail(r.Context())
	if err != nil {
		s.internalError(w, r, "read alert email settings", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled": settings.Enabled,
		"from":    settings.From,
		// Presence, never the value.
		"hasApiKey": settings.APIKey != "",
		"updatedAt": settings.UpdatedAt,
	})
}

// handleSaveAlertEmailSettings stores the configuration.
func (s *Server) handleSaveAlertEmailSettings(w http.ResponseWriter, r *http.Request) {
	var request alertEmailRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "Send a JSON body with enabled, from and optionally apiKey.")
		return
	}

	request.From = strings.TrimSpace(request.From)
	request.APIKey = strings.TrimSpace(request.APIKey)

	ctx := r.Context()
	// Enabling without something to send with would report success and then
	// quietly fail on the first alert, which is the failure mode this screen
	// exists to prevent.
	if request.Enabled {
		if !looksLikeEmail(db.NormalizeEmail(request.From)) {
			writeError(w, http.StatusBadRequest,
				"A from-address is needed to send alerts, and it must be one Resend has verified.")
			return
		}
		if request.APIKey == "" {
			current, err := s.resolveAlertEmail(ctx)
			if err != nil {
				s.internalError(w, r, "read alert email settings", err)
				return
			}
			if current.APIKey == "" {
				writeError(w, http.StatusBadRequest, "An API key is needed before alert email can be enabled.")
				return
			}
		}
	}

	actor, _ := UserFrom(ctx)
	if err := db.SaveAlertEmailSettings(ctx, s.DB, s.Cipher,
		request.Enabled, request.From, request.APIKey, actor.ID); err != nil {
		s.internalError(w, r, "save alert email settings", err)
		return
	}

	s.Log.Info("alert email settings changed",
		"by", actor.Email, "enabled", request.Enabled, "keyReplaced", request.APIKey != "")
	s.audit(r, db.ActionAlertEmailSettings, "settings", "alert-email",
		map[string]any{"enabled": request.Enabled, "keyReplaced": request.APIKey != ""})

	writeJSON(w, http.StatusOK, map[string]string{"message": "Saved."})
}

// handleTestAlertEmail sends a message to the caller's own address.
//
// A mail configuration that is only exercised when something is already broken
// is a mail configuration nobody trusts. Sending to the caller rather than an
// arbitrary address keeps this from being a way to mail strangers.
func (s *Server) handleTestAlertEmail(w http.ResponseWriter, r *http.Request) {
	actor, _ := UserFrom(r.Context())

	subject := "Pail test message"
	text := "This is a test from Pail's alert email settings.\n\n" +
		"If you are reading it, alert notifications will reach you.\n"
	htmlBody := "<p>This is a test from Pail's alert email settings.</p>" +
		"<p>If you are reading it, alert notifications will reach you.</p>"

	if err := s.Mailer.Send(r.Context(), actor.Email, subject, text, htmlBody); err != nil {
		if errors.Is(err, ErrMailNotConfigured) {
			writeError(w, http.StatusBadRequest,
				"Nothing is configured to send with. Save an API key and a from-address first.")
			return
		}
		// The provider's own message names the actual problem — an unverified
		// sender, a rejected key — far better than a generic failure would.
		s.Log.Error("test email failed", "to", actor.Email, "error", err)
		writeError(w, http.StatusBadGateway, "The provider rejected it: "+err.Error())
		return
	}

	s.Log.Info("test email sent", "to", actor.Email)
	writeJSON(w, http.StatusOK, map[string]string{
		"message": "Sent to " + actor.Email + ". If it does not arrive, check the spam folder and the sender domain.",
	})
}

// resolveAlertEmail reads the effective settings, including the environment
// fallback the SettingsMailer applies.
func (s *Server) resolveAlertEmail(ctx context.Context) (db.AlertEmailSettings, error) {
	if resolver, ok := s.Mailer.(*SettingsMailer); ok {
		return resolver.Resolve(ctx)
	}
	return db.GetAlertEmailSettings(ctx, s.DB, s.Cipher)
}

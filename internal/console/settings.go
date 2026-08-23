package console

import (
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

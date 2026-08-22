package console

import (
	"errors"
	"net/http"
	"strings"

	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/db"
	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/s3api"
)

// handleListSessions returns the signed-in user's active sessions.
func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	user, _ := UserFrom(r.Context())

	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "Not signed in.")
		return
	}

	sessions, err := db.ListSessions(r.Context(), s.DB, user.ID, db.HashToken(cookie.Value))
	if err != nil {
		s.internalError(w, r, "list sessions", err)
		return
	}

	entries := make([]map[string]any, 0, len(sessions))
	for _, session := range sessions {
		entries = append(entries, map[string]any{
			"id":         session.ID,
			"device":     describeUserAgent(session.UserAgent),
			"userAgent":  session.UserAgent,
			"ip":         session.IP,
			"createdAt":  session.CreatedAt,
			"lastSeenAt": session.LastSeenAt,
			"expiresAt":  session.IdleExpires,
			"current":    session.Current,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": entries})
}

// handleRevokeSession ends one of the user's own sessions.
func (s *Server) handleRevokeSession(w http.ResponseWriter, r *http.Request) {
	user, _ := UserFrom(r.Context())

	switch err := db.RevokeSessionByID(r.Context(), s.DB, user.ID, r.PathValue("id")); {
	case err == nil:
		s.audit(r, db.ActionSessionRevoke, "session", r.PathValue("id"), nil)
		writeJSON(w, http.StatusOK, map[string]string{
			"message": "Signed that device out. It stops working on its next request.",
		})
	case errors.Is(err, db.ErrSessionInvalid):
		writeError(w, http.StatusNotFound, "That session has already ended.")
	default:
		s.internalError(w, r, "revoke session", err)
	}
}

// handleRevokeOtherSessions ends every session except the current one.
//
// This is the control that matters after a lost laptop, so it deliberately
// leaves the caller signed in: being logged out by your own security action
// makes people hesitate to use it.
func (s *Server) handleRevokeOtherSessions(w http.ResponseWriter, r *http.Request) {
	user, _ := UserFrom(r.Context())

	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "Not signed in.")
		return
	}

	revoked, err := db.RevokeOtherSessions(r.Context(), s.DB, user.ID, db.HashToken(cookie.Value))
	if err != nil {
		s.internalError(w, r, "revoke other sessions", err)
		return
	}
	s.audit(r, db.ActionSessionRevoke, "session", "all-others",
		map[string]any{"count": revoked})

	writeJSON(w, http.StatusOK, map[string]any{
		"revoked": revoked,
		"message": "Signed out everywhere else. This device stays signed in.",
	})
}

// describeUserAgent turns a user-agent string into something a person can
// recognise their own device by. Deliberately coarse: the point is "was that
// me", not a fingerprint.
func describeUserAgent(raw *string) string {
	if raw == nil || *raw == "" {
		return "Unknown device"
	}
	agent := *raw

	var browser string
	switch {
	case strings.Contains(agent, "Edg/"):
		browser = "Edge"
	case strings.Contains(agent, "OPR/"):
		browser = "Opera"
	case strings.Contains(agent, "Firefox/"):
		browser = "Firefox"
	case strings.Contains(agent, "Chrome/"):
		browser = "Chrome"
	case strings.Contains(agent, "Safari/"):
		browser = "Safari"
	default:
		browser = "Browser"
	}

	var platform string
	switch {
	case strings.Contains(agent, "iPhone"):
		platform = "iPhone"
	case strings.Contains(agent, "iPad"):
		platform = "iPad"
	case strings.Contains(agent, "Android"):
		platform = "Android"
	case strings.Contains(agent, "Mac OS X"), strings.Contains(agent, "Macintosh"):
		platform = "macOS"
	case strings.Contains(agent, "Windows"):
		platform = "Windows"
	case strings.Contains(agent, "Linux"):
		platform = "Linux"
	default:
		return browser
	}
	return browser + " on " + platform
}

// handleSetupState reports whether the console has ever been signed in to.
//
// Unauthenticated by necessity: it is what decides between the first-run screen
// and an ordinary sign-in form. It reveals only a boolean and the bootstrap
// address, which is already in the operator's own .env — and showing it is the
// point, since a fresh install otherwise offers a login form with no indication
// of which address will work.
func (s *Server) handleSetupState(w http.ResponseWriter, r *http.Request) {
	var everSignedIn bool
	if err := s.DB.QueryRow(r.Context(),
		`SELECT EXISTS (SELECT 1 FROM users WHERE last_login_at IS NOT NULL)`).Scan(&everSignedIn); err != nil {
		s.internalError(w, r, "read setup state", err)
		return
	}

	var credentials int
	_ = s.DB.QueryRow(r.Context(),
		`SELECT count(*) FROM credentials WHERE revoked_at IS NULL`).Scan(&credentials)

	writeJSON(w, http.StatusOK, map[string]any{
		"configured":      everSignedIn,
		"adminEmail":      s.AdminEmail,
		"emailConfigured": s.System.ResendConfigured,
		"hasCredentials":  credentials > 0,
		"consoleURL":      s.PublicURL,
		"s3URL":           s.PublicS3URL,
	})
}

// handleCreateFolder creates an empty folder.
//
// Object stores have no folders: a folder is a common prefix that exists only
// because objects sit under it. Creating one therefore writes a zero-byte
// object whose key ends in a slash, which the delimiter listing then groups as
// a folder. It disappears when it is deleted, and an empty one is the only
// thing keeping the folder visible — which the response says, because the
// alternative is a folder that mysteriously vanishes.
func (s *Server) handleCreateFolder(w http.ResponseWriter, r *http.Request) {
	bucket, ok := s.requireBucket(w, r)
	if !ok {
		return
	}

	var request struct {
		Prefix string `json:"prefix"`
	}
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "Send a JSON body with a folder path.")
		return
	}

	key := strings.TrimPrefix(request.Prefix, "/")
	if key == "" {
		writeError(w, http.StatusBadRequest, "A folder needs a name.")
		return
	}
	if !strings.HasSuffix(key, "/") {
		key += "/"
	}
	if err := s3api.ValidateObjectKey(key); err != nil {
		writeError(w, http.StatusBadRequest, s3api.AsAPIError(err).Message)
		return
	}

	// A zero-byte blob. Content addressing means every folder marker in the
	// system shares one file, so this costs a row rather than an inode.
	blob, err := s.Blobs.Put(r.Context(), strings.NewReader(""))
	if err != nil {
		s.internalError(w, r, "create folder marker", err)
		return
	}

	object := &db.Object{
		BucketID:    bucket.ID,
		Key:         key,
		BlobDigest:  blob.Digest,
		Size:        0,
		ETag:        blob.ETag,
		ContentType: "application/x-directory",
	}
	if err := db.PutObject(r.Context(), s.DB, object, s.writeOptions(r.Context(), bucket)); err != nil {
		s.internalError(w, r, "create folder", err)
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"prefix": key,
		"note":   "Folders are not real in an object store. This is a zero-byte marker, and deleting it removes the folder.",
	})
}

package console

import (
	"errors"
	"net/http"

	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/db"
)

// handleListVersions returns version history, either for one key or across a
// prefix.
func (s *Server) handleListVersions(w http.ResponseWriter, r *http.Request) {
	bucket, ok := s.requireBucket(w, r)
	if !ok {
		return
	}

	query := r.URL.Query()
	var versions []db.ObjectVersion
	var err error

	if key := query.Get("key"); key != "" {
		versions, err = db.ListVersions(r.Context(), s.DB, bucket.ID, key,
			intParam(query.Get("limit"), 100))
	} else {
		versions, err = db.ListBucketVersions(r.Context(), s.DB, bucket.ID,
			query.Get("prefix"), intParam(query.Get("limit"), 200))
	}
	if err != nil {
		s.internalError(w, r, "list versions", err)
		return
	}

	// The live object is marked so the history reads as "this one is current"
	// rather than leaving the reader to infer it from timestamps.
	live := map[string]string{}
	if key := query.Get("key"); key != "" {
		if object, err := db.GetObject(r.Context(), s.DB, bucket.ID, key); err == nil {
			live[key] = object.BlobDigest
		}
	}

	entries := make([]map[string]any, 0, len(versions))
	for _, version := range versions {
		digest := ""
		if version.BlobDigest != nil {
			digest = *version.BlobDigest
		}
		entries = append(entries, map[string]any{
			"versionId":      version.VersionID,
			"key":            version.Key,
			"size":           version.Size,
			"etag":           version.ETag,
			"contentType":    version.ContentType,
			"isDeleteMarker": version.IsDeleteMarker,
			"createdAt":      version.CreatedAt,
			"createdBy":      version.CreatedBy,
			"isCurrent":      digest != "" && live[version.Key] == digest,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{"bucket": bucket.Name, "versions": entries})
}

type versionRequest struct {
	Key       string `json:"key"`
	VersionID string `json:"versionId"`
}

// handleRestoreVersion makes an old version current again.
//
// This is also how an accidental delete is undone: the delete left the previous
// state as a version, so restoring it brings the object back.
func (s *Server) handleRestoreVersion(w http.ResponseWriter, r *http.Request) {
	bucket, ok := s.requireBucket(w, r)
	if !ok {
		return
	}

	var request versionRequest
	if err := decodeJSON(r, &request); err != nil || request.Key == "" || request.VersionID == "" {
		writeError(w, http.StatusBadRequest, "Send a JSON body with a key and a version id.")
		return
	}

	user, _ := UserFrom(r.Context())
	object, err := db.RestoreVersion(r.Context(), s.DB, bucket.ID, request.Key, request.VersionID, user.Email)
	if err != nil {
		if errors.Is(err, db.ErrVersionNotFound) {
			writeError(w, http.StatusNotFound, "That version no longer exists.")
			return
		}
		s.internalError(w, r, "restore version", err)
		return
	}

	s.audit(r, db.ActionObjectRestore, "object", bucket.Name+"/"+request.Key,
		map[string]any{"versionId": request.VersionID})

	writeJSON(w, http.StatusOK, map[string]any{
		"message": "Restored. The restore is itself the newest version, so it can be undone the same way.",
		"key":     object.Key,
		"size":    object.Size,
	})
}

type purgeRequest struct {
	Key string `json:"key"`
	// VersionID purges one version; leaving it empty purges the key's whole
	// history, which is what actually reclaims the space.
	VersionID string `json:"versionId"`
}

// handlePurgeVersions permanently removes history.
func (s *Server) handlePurgeVersions(w http.ResponseWriter, r *http.Request) {
	bucket, ok := s.requireBucket(w, r)
	if !ok {
		return
	}

	var request purgeRequest
	if err := decodeJSON(r, &request); err != nil || request.Key == "" {
		writeError(w, http.StatusBadRequest, "Send a JSON body with a key.")
		return
	}

	if request.VersionID != "" {
		if err := db.PurgeVersion(r.Context(), s.DB, bucket.ID, request.Key, request.VersionID); err != nil {
			if errors.Is(err, db.ErrVersionNotFound) {
				writeError(w, http.StatusNotFound, "That version no longer exists.")
				return
			}
			s.internalError(w, r, "purge version", err)
			return
		}
		s.audit(r, db.ActionObjectPurge, "object", bucket.Name+"/"+request.Key,
			map[string]any{"versionId": request.VersionID})
		writeJSON(w, http.StatusOK, map[string]any{"purged": 1})
		return
	}

	purged, err := db.PurgeKeyVersions(r.Context(), s.DB, bucket.ID, request.Key)
	if err != nil {
		s.internalError(w, r, "purge versions", err)
		return
	}
	s.audit(r, db.ActionObjectPurge, "object", bucket.Name+"/"+request.Key,
		map[string]any{"versions": purged})

	writeJSON(w, http.StatusOK, map[string]any{
		"purged":  purged,
		"message": "Purged. The space is reclaimed by the next sweep.",
	})
}

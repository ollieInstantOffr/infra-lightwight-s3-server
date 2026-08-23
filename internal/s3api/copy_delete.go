package s3api

import (
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/db"
)

const (
	// maxDeleteKeys is S3's limit for one batch delete.
	maxDeleteKeys = 1000

	// maxDeleteBodySize bounds the request document: 1000 keys of up to 1024
	// bytes each, plus XML overhead.
	maxDeleteBodySize = 2 << 20

	// metadataDirectiveReplace means the copy takes its metadata from the
	// request rather than from the source object.
	metadataDirectiveCopy    = "COPY"
	metadataDirectiveReplace = "REPLACE"
)

// DeleteObjectsRequest is the batch delete document.
type DeleteObjectsRequest struct {
	XMLName xml.Name          `xml:"Delete"`
	Quiet   bool              `xml:"Quiet"`
	Objects []DeleteObjectXML `xml:"Object"`
}

// DeleteObjectXML names one key to delete, and optionally which version of it.
type DeleteObjectXML struct {
	Key string `xml:"Key"`
	// VersionID removes that specific version permanently, rather than writing
	// a delete marker. Batch delete carries it per key because a client
	// cleaning up a versioned bucket has to name versions, and doing so one
	// request at a time defeats the point of the batch call.
	VersionID string `xml:"VersionId"`
}

// DeleteResult is the batch delete response.
type DeleteResult struct {
	XMLName xml.Name        `xml:"DeleteResult"`
	Xmlns   string          `xml:"xmlns,attr"`
	Deleted []DeletedEntry  `xml:"Deleted"`
	Errors  []DeleteErroXML `xml:"Error"`
}

// DeletedEntry is one successfully deleted key.
type DeletedEntry struct {
	Key       string `xml:"Key"`
	VersionID string `xml:"VersionId,omitempty"`
	// Set when the delete wrote a marker rather than removing anything, with
	// the marker's own id — which is what a client needs to undo it.
	DeleteMarker          bool   `xml:"DeleteMarker,omitempty"`
	DeleteMarkerVersionID string `xml:"DeleteMarkerVersionId,omitempty"`
}

// DeleteErroXML is one key that could not be deleted.
type DeleteErroXML struct {
	Key       string `xml:"Key"`
	VersionID string `xml:"VersionId,omitempty"`
	Code      string `xml:"Code"`
	Message   string `xml:"Message"`
}

// CopyObjectResult is the CopyObject response body.
type CopyObjectResult struct {
	XMLName      xml.Name `xml:"CopyObjectResult"`
	Xmlns        string   `xml:"xmlns,attr"`
	ETag         string   `xml:"ETag"`
	LastModified string   `xml:"LastModified"`
}

// handleDeleteObjects implements the batch DeleteObjects call, which is what
// `aws s3 rm --recursive` and the console's multi-select delete both use. Doing
// it one request per key would make deleting a thousand objects a thousand
// round trips.
func (s *Server) handleDeleteObjects(w http.ResponseWriter, r *http.Request, bucket *db.Bucket) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxDeleteBodySize))
	if err != nil {
		WriteError(w, r, ErrIncompleteBody)
		return
	}
	var request DeleteObjectsRequest
	if err := xml.Unmarshal(body, &request); err != nil {
		WriteError(w, r, ErrMalformedXML)
		return
	}
	if len(request.Objects) == 0 {
		WriteError(w, r, ErrMalformedXML.WithMessage("The delete request listed no keys."))
		return
	}
	if len(request.Objects) > maxDeleteKeys {
		WriteError(w, r, ErrMalformedXML.WithMessage(
			"A delete request may name at most %d keys, got %d.", maxDeleteKeys, len(request.Objects)))
		return
	}

	options := s.writeOptions(r, bucket)
	grant, authenticated := grantFor(r)
	result := DeleteResult{Xmlns: s3Namespace}
	for _, target := range request.Objects {
		// Authorized per key, not per request. The router only established that
		// this key may delete something in this bucket; a prefix-scoped key
		// naming a thousand keys must be refused for each one outside its
		// prefix and obeyed for the rest.
		//
		// Refused keys come back in the per-key error list, which is how S3
		// reports partial failure and how a client learns which of its keys
		// were rejected. Failing the whole request instead would tell it far
		// less.
		if authenticated && !grant.Allows(bucket.Name, target.Key, db.PermissionDelete) {
			noteFailure(r.Context(), ErrAccessDenied.Code,
				"the access key is not permitted to delete "+bucket.Name+"/"+target.Key)
			result.Errors = append(result.Errors, DeleteErroXML{
				Key:     target.Key,
				Code:    ErrAccessDenied.Code,
				Message: ErrAccessDenied.Message,
			})
			continue
		}
		if target.VersionID != "" {
			deletion, err := db.DeleteObjectVersion(r.Context(), s.DB, bucket.ID, target.Key, target.VersionID)
			if err != nil {
				code, message := ErrInternalError.Code, ErrInternalError.Message
				if errors.Is(err, db.ErrVersionNotFound) {
					code, message = ErrNoSuchVersion.Code, ErrNoSuchVersion.Message
				} else {
					s.Log.Error("batch delete failed for one version",
						"request_id", RequestIDFrom(r.Context()),
						"bucket", bucket.Name, "key", target.Key,
						"version_id", target.VersionID, "error", err)
				}
				result.Errors = append(result.Errors, DeleteErroXML{
					Key: target.Key, VersionID: target.VersionID, Code: code, Message: message,
				})
				continue
			}
			if !request.Quiet {
				result.Deleted = append(result.Deleted, DeletedEntry{
					Key:                   target.Key,
					VersionID:             target.VersionID,
					DeleteMarker:          deletion.DeleteMarker,
					DeleteMarkerVersionID: deletion.MarkerVersionID,
				})
			}
			continue
		}

		// One key failing must not abandon the rest: S3 reports per-key
		// outcomes so a client can retry only what actually failed.
		deletion, err := db.DeleteObject(r.Context(), s.DB, bucket.ID, target.Key, options)
		if err != nil {
			s.Log.Error("batch delete failed for one key",
				"request_id", RequestIDFrom(r.Context()),
				"bucket", bucket.Name, "key", target.Key, "error", err)
			result.Errors = append(result.Errors, DeleteErroXML{
				Key:     target.Key,
				Code:    ErrInternalError.Code,
				Message: ErrInternalError.Message,
			})
			continue
		}
		// Deleting an absent key is a success, matching single-key DELETE.
		if !request.Quiet {
			result.Deleted = append(result.Deleted, DeletedEntry{
				Key:                   target.Key,
				DeleteMarker:          deletion.DeleteMarker,
				DeleteMarkerVersionID: deletion.MarkerVersionID,
			})
		}
	}
	writeXML(w, r, http.StatusOK, result)
}

// handleCopyObject implements server-side copy, signalled by x-amz-copy-source
// on a PUT.
//
// No bytes move. Blobs are content-addressed, so the copy is a second object
// row referencing the same blob — a copy of a 100 GB object costs one database
// row and takes microseconds.
func (s *Server) handleCopyObject(w http.ResponseWriter, r *http.Request, destination *db.Bucket, copySource string) {
	destinationKey := keyOf(r)
	if err := ValidateObjectKey(destinationKey); err != nil {
		WriteError(w, r, err)
		return
	}

	sourceBucketName, sourceKey, sourceVersion, err := parseCopySource(copySource)
	if err != nil {
		WriteError(w, r, err)
		return
	}

	sourceBucket, err := db.GetBucket(r.Context(), s.DB, sourceBucketName)
	if errors.Is(err, db.ErrBucketNotFound) {
		WriteError(w, r, ErrNoSuchBucket.WithMessage(
			"The source bucket %q does not exist.", sourceBucketName))
		return
	}
	if err != nil {
		s.internal(w, r, "get copy source bucket", err)
		return
	}

	// Naming a version copies that one rather than whatever is current. This
	// is how a version is restored through the API — copy it over its own key
	// and it becomes current again, with the old state kept as history.
	var source *db.Object
	if sourceVersion != "" {
		source, err = db.GetObjectVersion(r.Context(), s.DB, sourceBucket.ID, sourceKey, sourceVersion)
		switch {
		case errors.Is(err, db.ErrIsDeleteMarker):
			WriteError(w, r, ErrInvalidArgument.WithMessage(
				"The source version is a delete marker and has no contents to copy."))
			return
		case errors.Is(err, db.ErrVersionNotFound), errors.Is(err, db.ErrObjectNotFound):
			WriteError(w, r, ErrNoSuchVersion)
			return
		case err != nil:
			s.internal(w, r, "get copy source version", err)
			return
		}
	} else {
		source, err = db.GetObject(r.Context(), s.DB, sourceBucket.ID, sourceKey)
		if errors.Is(err, db.ErrObjectNotFound) {
			WriteError(w, r, ErrNoSuchKey.WithMessage(
				"The source key %q does not exist.", sourceKey))
			return
		}
		if err != nil {
			s.internal(w, r, "get copy source object", err)
			return
		}
	}

	// Copying an object onto itself with no change is rejected by S3, because
	// it is almost always a mistake rather than an intent to no-op.
	directive := strings.ToUpper(r.Header.Get("x-amz-metadata-directive"))
	if directive == "" {
		directive = metadataDirectiveCopy
	}
	if sourceBucket.ID == destination.ID && sourceKey == destinationKey &&
		directive == metadataDirectiveCopy && sourceVersion == "" {
		WriteError(w, r, ErrInvalidArgument.WithMessage(
			"This copy request is illegal because it is trying to copy an object to itself "+
				"without changing the object's metadata."))
		return
	}

	contentType := source.ContentType
	metadata := source.Metadata
	if directive == metadataDirectiveReplace {
		contentType = contentTypeOf(r)
		if metadata, err = userMetadata(r.Header); err != nil {
			WriteError(w, r, err)
			return
		}
	}

	object := &db.Object{
		BucketID:    destination.ID,
		Key:         destinationKey,
		BlobDigest:  source.BlobDigest,
		Size:        source.Size,
		ETag:        source.ETag,
		ContentType: contentType,
		Metadata:    metadata,
	}
	if err := db.PutObject(r.Context(), s.DB, object, s.writeOptions(r, destination)); err != nil {
		s.internal(w, r, "copy object", err)
		return
	}

	// The destination's own version, not the source's. A client copying to
	// restore needs the id of what it just created.
	writeVersionHeaders(w, object.VersionID)
	if sourceVersion != "" {
		w.Header().Set("x-amz-copy-source-version-id", sourceVersion)
	}
	writeXML(w, r, http.StatusOK, CopyObjectResult{
		Xmlns:        s3Namespace,
		ETag:         quoteETag(object.ETag),
		LastModified: formatXMLTime(object.UpdatedAt),
	})
}

// parseCopySource splits x-amz-copy-source into bucket and key.
//
// The header is "/bucket/key" or "bucket/key", percent-encoded. Decoding
// happens after the split so an encoded slash inside the key stays part of the
// key rather than becoming the separator.
func parseCopySource(raw string) (bucket, key, version string, err error) {
	trimmed := strings.TrimPrefix(raw, "/")
	// A version id suffix names which version to copy from, which is the
	// ordinary way to restore an old version through the API: copy it over the
	// key it came from and it becomes current again.
	if idx := strings.Index(trimmed, "?versionId="); idx >= 0 {
		version = trimmed[idx+len("?versionId="):]
		trimmed = trimmed[:idx]
	}

	rawBucket, rawKey, found := strings.Cut(trimmed, "/")
	if !found || rawBucket == "" || rawKey == "" {
		return "", "", "", ErrInvalidArgument.WithMessage(
			"The x-amz-copy-source header must name a bucket and key, got %q.", raw)
	}
	if bucket, err = percentDecode(rawBucket); err != nil {
		return "", "", "", ErrInvalidArgument.WithMessage("The x-amz-copy-source header is not valid.")
	}
	if key, err = percentDecode(rawKey); err != nil {
		return "", "", "", ErrInvalidArgument.WithMessage("The x-amz-copy-source header is not valid.")
	}
	return bucket, key, version, nil
}

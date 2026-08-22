package s3api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/db"
	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/storage"
)

const (
	// userMetadataPrefix is how clients attach arbitrary metadata to an object.
	userMetadataPrefix = "x-amz-meta-"

	// defaultContentType is what S3 assumes when a client sends none.
	defaultContentType = "binary/octet-stream"

	// maxUserMetadataBytes bounds the combined size of user metadata, matching
	// S3's 2 KB limit. Metadata is returned on every GET and HEAD, so unbounded
	// metadata would be a cheap way to make every read expensive.
	maxUserMetadataBytes = 2 << 10
)

// handlePutObject implements PUT Object.
//
// The body is streamed straight into the blob store. Nothing is buffered, so
// object size is bounded by disk rather than by memory.
func (s *Server) handlePutObject(w http.ResponseWriter, r *http.Request) {
	bucket, err := s.requireBucket(w, r, bucketOf(r))
	if err != nil {
		return
	}
	key := keyOf(r)
	if err := ValidateObjectKey(key); err != nil {
		WriteError(w, r, err)
		return
	}

	metadata, err := userMetadata(r.Header)
	if err != nil {
		WriteError(w, r, err)
		return
	}

	identity, ok := IdentityFrom(r.Context())
	if !ok {
		// Unreachable behind the authentication middleware, but a missing
		// identity here would mean streaming an unauthenticated body to disk.
		WriteError(w, r, ErrAccessDenied)
		return
	}

	// Body() strips aws-chunked framing and enforces the declared payload hash
	// as the bytes flow past.
	body := s.Verifier.Body(r, identity)
	defer body.Close()

	blob, err := s.Blobs.Put(r.Context(), body)
	if err != nil {
		// A hash or chunk-signature failure surfaces here, mid-stream, because
		// it cannot be known until the last byte. The blob that was written is
		// left unreferenced and the sweeper reclaims it.
		if apiErr := AsAPIError(err); apiErr != ErrInternalError {
			WriteError(w, r, err)
			return
		}
		s.internal(w, r, "write object data", err)
		return
	}

	object := &db.Object{
		BucketID:    bucket.ID,
		Key:         key,
		BlobDigest:  blob.Digest,
		Size:        blob.Size,
		ETag:        blob.ETag,
		ContentType: contentTypeOf(r),
		Metadata:    metadata,
	}
	if err := db.PutObject(r.Context(), s.DB, object); err != nil {
		s.internal(w, r, "write object metadata", err)
		return
	}

	w.Header().Set("ETag", quoteETag(blob.ETag))
	w.WriteHeader(http.StatusOK)
}

// handleGetObject implements GET Object.
func (s *Server) handleGetObject(w http.ResponseWriter, r *http.Request) {
	object, ok := s.lookupObject(w, r)
	if !ok {
		return
	}

	if status := checkPreconditions(r, object); status != 0 {
		writeConditionalResponse(w, r, status, object)
		return
	}

	file, err := s.Blobs.Open(object.BlobDigest)
	if err != nil {
		// Metadata without bytes means the store and the database have
		// diverged, which is an operator problem rather than a client one.
		if errors.Is(err, storage.ErrNotFound) {
			s.internal(w, r, "open object data", fmt.Errorf(
				"object %q references missing blob %s", object.Key, object.BlobDigest))
			return
		}
		s.internal(w, r, "open object data", err)
		return
	}
	defer file.Close()

	writeObjectHeaders(w, object)

	// ServeContent handles Range and conditional requests, and sets
	// Content-Length and status. It needs a ReadSeeker, which is exactly why
	// the blob store hands back a seekable file.
	http.ServeContent(w, r, object.Key, object.UpdatedAt, file)
}

// handleHeadObject implements HEAD Object: the same headers as GET, no body.
func (s *Server) handleHeadObject(w http.ResponseWriter, r *http.Request) {
	object, ok := s.lookupObject(w, r)
	if !ok {
		return
	}

	if status := checkPreconditions(r, object); status != 0 {
		writeConditionalResponse(w, r, status, object)
		return
	}

	writeObjectHeaders(w, object)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", object.Size))
	// Advertised so clients know Range requests will work.
	w.Header().Set("Accept-Ranges", "bytes")
	w.WriteHeader(http.StatusOK)
}

// handleDeleteObject implements DELETE Object.
//
// S3 makes this idempotent: deleting a key that does not exist returns 204, the
// same as deleting one that did. Clients rely on it when cleaning up.
func (s *Server) handleDeleteObject(w http.ResponseWriter, r *http.Request) {
	bucket, err := s.requireBucket(w, r, bucketOf(r))
	if err != nil {
		return
	}
	key := keyOf(r)

	if _, err := db.DeleteObject(r.Context(), s.DB, bucket.ID, key); err != nil {
		s.internal(w, r, "delete object", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// lookupObject resolves the bucket and object named by the request, writing the
// appropriate error and returning false if either is missing.
func (s *Server) lookupObject(w http.ResponseWriter, r *http.Request) (*db.Object, bool) {
	bucket, err := s.requireBucket(w, r, bucketOf(r))
	if err != nil {
		return nil, false
	}
	key := keyOf(r)
	if err := ValidateObjectKey(key); err != nil {
		WriteError(w, r, err)
		return nil, false
	}

	object, err := db.GetObject(r.Context(), s.DB, bucket.ID, key)
	if errors.Is(err, db.ErrObjectNotFound) {
		WriteError(w, r, ErrNoSuchKey)
		return nil, false
	}
	if err != nil {
		s.internal(w, r, "get object", err)
		return nil, false
	}
	return object, true
}

// writeObjectHeaders sets the headers common to GET and HEAD.
func writeObjectHeaders(w http.ResponseWriter, object *db.Object) {
	w.Header().Set("ETag", quoteETag(object.ETag))
	w.Header().Set("Content-Type", object.ContentType)
	w.Header().Set("Last-Modified", formatHTTPTime(object.UpdatedAt))
	w.Header().Set("Accept-Ranges", "bytes")
	for name, value := range object.Metadata {
		w.Header().Set(userMetadataPrefix+name, value)
	}
}

// userMetadata collects x-amz-meta-* headers.
//
// Names are lowercased because HTTP header names are case-insensitive but the
// JSON keys they become are not; without normalising, the same logical metadata
// could round-trip under two different names.
func userMetadata(header http.Header) (map[string]string, error) {
	var metadata map[string]string
	total := 0

	for name, values := range header {
		lower := strings.ToLower(name)
		if !strings.HasPrefix(lower, userMetadataPrefix) {
			continue
		}
		suffix := strings.TrimPrefix(lower, userMetadataPrefix)
		if suffix == "" {
			continue
		}
		value := strings.Join(values, ",")
		total += len(suffix) + len(value)
		if total > maxUserMetadataBytes {
			return nil, ErrInvalidArgument.WithMessage(
				"Your metadata headers exceed the maximum allowed metadata size of %d bytes.",
				maxUserMetadataBytes)
		}
		if metadata == nil {
			metadata = make(map[string]string)
		}
		metadata[suffix] = value
	}
	return metadata, nil
}

// contentTypeOf returns the declared content type, or S3's default.
func contentTypeOf(r *http.Request) string {
	if ct := r.Header.Get("Content-Type"); ct != "" {
		return ct
	}
	return defaultContentType
}

// quoteETag wraps a hex digest in the double quotes S3 uses. Clients compare
// the quoted form verbatim, so the quotes are part of the value rather than
// decoration.
func quoteETag(etag string) string {
	if strings.HasPrefix(etag, `"`) {
		return etag
	}
	return `"` + etag + `"`
}

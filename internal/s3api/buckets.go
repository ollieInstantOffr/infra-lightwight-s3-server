package s3api

import (
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/db"
)

// maxCreateBucketBody bounds the CreateBucketConfiguration document. It is a
// handful of bytes in practice; the limit stops an unbounded read on a request
// whose body is supposed to be tiny.
const maxCreateBucketBody = 4 << 10

// handleListBuckets implements ListBuckets: GET on the service root.
func (s *Server) handleListBuckets(w http.ResponseWriter, r *http.Request) {
	buckets, err := db.ListBuckets(r.Context(), s.DB)
	if err != nil {
		s.internal(w, r, "list buckets", err)
		return
	}

	result := ListAllMyBucketsResult{
		Xmlns: s3Namespace,
		Owner: defaultOwner(),
	}
	// Marshalled as an empty <Buckets/> element rather than omitted, since
	// clients expect the element to exist even with no buckets.
	result.Buckets.Bucket = make([]BucketEntry, 0, len(buckets))
	for _, b := range buckets {
		result.Buckets.Bucket = append(result.Buckets.Bucket, BucketEntry{
			Name:         b.Name,
			CreationDate: formatXMLTime(b.CreatedAt),
		})
	}
	writeXML(w, r, http.StatusOK, result)
}

// handleCreateBucket implements CreateBucket.
//
// Creating a bucket that the caller already owns succeeds in the sense that
// nothing is broken, but S3 still reports BucketAlreadyOwnedByYou outside
// us-east-1, and the SDKs expect that. Since this server has one tenant, an
// existing bucket is always one the caller owns.
func (s *Server) handleCreateBucket(w http.ResponseWriter, r *http.Request) {
	name := bucketOf(r)
	if err := ValidateBucketName(name); err != nil {
		WriteError(w, r, err)
		return
	}

	// The body, when present, names a region. Read it so the connection stays
	// usable, and reject a region that disagrees with this server's.
	if err := s.checkLocationConstraint(r); err != nil {
		WriteError(w, r, err)
		return
	}

	identity, _ := IdentityFrom(r.Context())
	_ = identity // bucket ownership is per-server, not per-credential

	if _, err := db.CreateBucket(r.Context(), s.DB, name, nil); err != nil {
		if errors.Is(err, db.ErrBucketExists) {
			WriteError(w, r, ErrBucketAlreadyOwnedByYou)
			return
		}
		s.internal(w, r, "create bucket", err)
		return
	}

	// S3 returns the bucket's path in Location, which some clients follow.
	w.Header().Set("Location", "/"+name)
	w.WriteHeader(http.StatusOK)
}

// checkLocationConstraint validates the optional CreateBucketConfiguration.
func (s *Server) checkLocationConstraint(r *http.Request) error {
	if r.Body == nil || r.ContentLength == 0 {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxCreateBucketBody))
	if err != nil {
		return ErrIncompleteBody
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return nil
	}

	var cfg CreateBucketConfiguration
	if err := xml.Unmarshal(body, &cfg); err != nil {
		return ErrMalformedXML
	}
	// An empty constraint means us-east-1, which is how S3 encodes its default.
	if cfg.LocationConstraint == "" || cfg.LocationConstraint == s.Region {
		return nil
	}
	return ErrInvalidArgument.WithMessage(
		"The specified location constraint %q is not valid. This server serves region %q.",
		cfg.LocationConstraint, s.Region)
}

// handleGetBucket dispatches GET on a bucket.
//
// S3 overloads this verb heavily through query subresources: ?location,
// ?versioning, ?acl and so on all arrive as a GET on the bucket. Anything not
// handled must report NotImplemented rather than being mistaken for a listing.
func (s *Server) handleGetBucket(w http.ResponseWriter, r *http.Request) {
	name := bucketOf(r)
	query := r.URL.Query()

	bucket, err := s.requireBucket(w, r, name)
	if err != nil {
		return
	}

	switch {
	case query.Has("location"):
		// us-east-1 is reported as an empty constraint, matching S3.
		value := s.Region
		if value == "us-east-1" {
			value = ""
		}
		writeXML(w, r, http.StatusOK, LocationConstraint{Xmlns: s3Namespace, Value: value})
	case query.Has("uploads"):
		s.handleListMultipartUploads(w, r, bucket)
	default:
		// A plain GET on a bucket is a listing, in both v1 and v2 form.
		s.handleListObjects(w, r, bucket)
	}
}

// handleHeadBucket implements HeadBucket, the existence check.
func (s *Server) handleHeadBucket(w http.ResponseWriter, r *http.Request) {
	if _, err := s.requireBucket(w, r, bucketOf(r)); err != nil {
		return
	}
	w.Header().Set("x-amz-bucket-region", s.Region)
	w.WriteHeader(http.StatusOK)
}

// handleDeleteBucket implements DeleteBucket, which refuses a bucket that still
// has contents.
func (s *Server) handleDeleteBucket(w http.ResponseWriter, r *http.Request) {
	name := bucketOf(r)
	if err := ValidateBucketName(name); err != nil {
		WriteError(w, r, err)
		return
	}

	switch err := db.DeleteBucket(r.Context(), s.DB, name); {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, db.ErrBucketNotFound):
		WriteError(w, r, ErrNoSuchBucket)
	case errors.Is(err, db.ErrBucketNotEmpty):
		WriteError(w, r, ErrBucketNotEmpty)
	default:
		s.internal(w, r, "delete bucket", err)
	}
}

// requireBucket resolves a bucket, writing the appropriate error and returning
// non-nil if it could not be found. Callers return immediately on error.
func (s *Server) requireBucket(w http.ResponseWriter, r *http.Request, name string) (*db.Bucket, error) {
	if err := ValidateBucketName(name); err != nil {
		WriteError(w, r, err)
		return nil, err
	}
	bucket, err := db.GetBucket(r.Context(), s.DB, name)
	if errors.Is(err, db.ErrBucketNotFound) {
		WriteError(w, r, ErrNoSuchBucket)
		return nil, err
	}
	if err != nil {
		s.internal(w, r, "get bucket", err)
		return nil, err
	}
	return bucket, nil
}

// internal logs an unexpected failure in full and tells the client only that
// something went wrong, so implementation detail stays out of the response.
func (s *Server) internal(w http.ResponseWriter, r *http.Request, operation string, err error) {
	s.Log.Error("s3 request failed",
		"request_id", RequestIDFrom(r.Context()),
		"operation", operation,
		"method", r.Method,
		"path", r.URL.Path,
		"error", err,
	)
	WriteError(w, r, ErrInternalError)
}

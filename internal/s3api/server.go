package s3api

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/db"
	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/storage"
)

// Server serves the S3 API.
type Server struct {
	DB       *db.Pool
	Blobs    *storage.Store
	Verifier *Verifier
	Log      *slog.Logger
	Region   string
	// PublicURL is the address clients reach this server on, which is not the
	// address it binds to: TLS terminates at the reverse proxy. It appears in
	// the Location of a completed multipart upload.
	PublicURL string
	// S3Domain enables virtual-host style addressing when set: a request to
	// bucket.s3.example.com names that bucket. Empty means path-style only.
	S3Domain string
	// Metrics counts requests for the console's overview. Optional: nil simply
	// means nothing is counted.
	Metrics RequestCounter
	// Scrape is optional. Nil simply means nothing is exporting metrics, which
	// is the case in most tests.
	Scrape RequestObserver
	// Logs receives completed requests for the console's log viewer. Optional.
	Logs RequestRecorder
}

// RequestCounter accumulates request counts. The metrics collector satisfies
// it; taking an interface keeps this package independent of it.
// RequestObserver records a completed request for scraping. The metrics
// registry satisfies it.
type RequestObserver interface {
	Observe(surface, operation string, status int, duration time.Duration, bytesIn, bytesOut int64)
}

type RequestCounter interface {
	Record(status int, bytesIn, bytesOut int64)
}

// Handler builds the routed, authenticated handler for the S3 listener.
//
// Middleware order is deliberate: request ids first so even a rejected request
// is traceable, then access logging so rejections are logged too, then
// authentication, and only then routing.
func (s *Server) Handler() http.Handler {
	var handler http.Handler = http.HandlerFunc(s.route)
	handler = s.Authenticate(s.Log, handler)
	handler = WithAccessLog(s.Log, s.Logs, s.Verifier.Proxies, handler)
	if s.Metrics != nil {
		handler = WithMetrics(s.Metrics, handler)
	}
	if s.Scrape != nil {
		handler = WithScrapeMetrics(s.Scrape, handler)
	}
	// Outside everything that reads it, so the access log and the metrics both
	// see the operation the router recorded.
	handler = WithRequestInfo(handler)
	return WithRequestID(handler)
}

// route dispatches a request to the right handler.
//
// http.ServeMux is deliberately not used. It cleans the request path before
// matching, so a key containing a "." or ".." segment — "logs/../old.txt", say —
// is answered with a redirect to a different key. S3 explicitly does not
// normalize object keys, and treats them as opaque byte strings, so a mux that
// rewrites them cannot serve them. Routing here is simple enough that reading
// the raw path directly is both shorter and correct.
func (s *Server) route(w http.ResponseWriter, r *http.Request) {
	bucket, key, err := s.resolveAddressing(r)
	if err != nil {
		WriteError(w, r, ErrInvalidArgument.WithMessage("The request URI is not valid."))
		return
	}

	ctx := context.WithValue(r.Context(), pathKey{}, pathParts{bucket: bucket, key: key})
	r = r.WithContext(ctx)
	noteTarget(ctx, bucket, key)

	switch {
	case bucket == "":
		s.routeService(w, r)
	case key == "":
		s.routeBucket(w, r)
	default:
		s.routeObject(w, r)
	}
}

func (s *Server) routeService(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		noteOperation(r.Context(), "ListBuckets")
		s.handleListBuckets(w, r)
		return
	}
	s.unsupported(w, r)
}

func (s *Server) routeBucket(w http.ResponseWriter, r *http.Request) {
	bucket := bucketOf(r)

	switch {
	case r.Method == http.MethodPost && r.URL.Query().Has("delete"):
		// Batch delete is a POST on the bucket with a ?delete subresource,
		// which is how `aws s3 rm --recursive` removes a thousand keys in one
		// request rather than a thousand.
		//
		// Gated loosely here and strictly per key inside the handler: a request
		// naming a thousand keys is not one decision. The check here only
		// rejects a key with no delete permission anywhere in the bucket, which
		// saves parsing a body that cannot succeed.
		if !s.permit(w, r, bucket, "", access{db.PermissionDelete, reachesSomewhere}) {
			return
		}
		noteOperation(r.Context(), "DeleteObjects")
		s.withBucket(w, r, s.handleDeleteObjects)
		return
	}

	// ?versioning is a PUT on the bucket that configures it rather than
	// creating it, so it is dispatched before the create path — and it is a
	// configuration change, which needs write across the whole bucket rather
	// than write on some prefix of it.
	if r.Method == http.MethodPut && r.URL.Query().Has("versioning") {
		if !s.permit(w, r, bucket, "", access{db.PermissionWrite, reachesEverything}) {
			return
		}
		noteOperation(r.Context(), "PutBucketVersioning")
		s.withBucket(w, r, s.handlePutBucketVersioning)
		return
	}

	switch r.Method {
	case http.MethodPut:
		if !s.permit(w, r, bucket, "", createBucket) {
			return
		}
		noteOperation(r.Context(), "CreateBucket")
		s.handleCreateBucket(w, r)
	case http.MethodGet:
		// Listing a bucket, and listing its multipart uploads, are both reads
		// of the bucket. What a prefix-scoped key actually sees is narrowed by
		// the handler rather than refused here.
		if !s.permit(w, r, bucket, "", readBucket) {
			return
		}
		noteOperation(r.Context(), "GetBucket")
		s.handleGetBucket(w, r)
	case http.MethodHead:
		if !s.permit(w, r, bucket, "", readBucket) {
			return
		}
		noteOperation(r.Context(), "HeadBucket")
		s.handleHeadBucket(w, r)
	case http.MethodDelete:
		if !s.permit(w, r, bucket, "", deleteBucket) {
			return
		}
		noteOperation(r.Context(), "DeleteBucket")
		s.handleDeleteBucket(w, r)
	default:
		s.unsupported(w, r)
	}
}

func (s *Server) routeObject(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()

	// Multipart is expressed through query subresources on the object path
	// rather than through distinct routes, so it is dispatched before the
	// plain verbs. uploadId identifies an in-progress upload; ?uploads with no
	// id starts one.
	bucket, key := bucketOf(r), keyOf(r)

	// Every multipart step is part of a write. See the note in authorize.go on
	// why abort is a write rather than a delete.
	uploadID := query.Get("uploadId")
	switch {
	case r.Method == http.MethodPost && query.Has("uploads"):
		if !s.permit(w, r, bucket, key, writeObject) {
			return
		}
		noteOperation(r.Context(), "CreateMultipartUpload")
		s.withBucket(w, r, s.handleCreateMultipartUpload)
		return
	case r.Method == http.MethodPut && uploadID != "":
		if !s.permit(w, r, bucket, key, writeObject) {
			return
		}
		noteOperation(r.Context(), "UploadPart")
		s.withUpload(w, r, uploadID, s.handleUploadPart)
		return
	case r.Method == http.MethodPost && uploadID != "":
		if !s.permit(w, r, bucket, key, writeObject) {
			return
		}
		noteOperation(r.Context(), "CompleteMultipartUpload")
		s.withUpload(w, r, uploadID, s.handleCompleteMultipartUpload)
		return
	case r.Method == http.MethodDelete && uploadID != "":
		if !s.permit(w, r, bucket, key, writeObject) {
			return
		}
		noteOperation(r.Context(), "AbortMultipartUpload")
		s.withUpload(w, r, uploadID, s.handleAbortMultipartUpload)
		return
	case r.Method == http.MethodGet && uploadID != "":
		if !s.permit(w, r, bucket, key, writeObject) {
			return
		}
		noteOperation(r.Context(), "ListParts")
		s.withUpload(w, r, uploadID, s.handleListParts)
		return
	}

	// A PUT carrying x-amz-copy-source is a server-side copy, not an upload.
	// It touches two objects, so it needs two checks: the destination as a
	// write, and the source as a read. Copying is how a key with read on one
	// bucket and write on another would otherwise move data it cannot read.
	if r.Method == http.MethodPut && r.Header.Get("x-amz-copy-source") != "" {
		source := r.Header.Get("x-amz-copy-source")
		if !s.permit(w, r, bucket, key, writeObject) {
			return
		}
		sourceBucket, sourceKey, _, err := parseCopySource(source)
		if err != nil {
			WriteError(w, r, err)
			return
		}
		if !s.permit(w, r, sourceBucket, sourceKey, readObject) {
			return
		}
		noteOperation(r.Context(), "CopyObject")
		s.withBucket(w, r, func(w http.ResponseWriter, r *http.Request, bucket *db.Bucket) {
			s.handleCopyObject(w, r, bucket, source)
		})
		return
	}

	switch r.Method {
	case http.MethodPut:
		if !s.permit(w, r, bucket, key, writeObject) {
			return
		}
		noteOperation(r.Context(), "PutObject")
		s.handlePutObject(w, r)
	case http.MethodGet:
		if !s.permit(w, r, bucket, key, readObject) {
			return
		}
		noteOperation(r.Context(), "GetObject")
		s.handleGetObject(w, r)
	case http.MethodHead:
		if !s.permit(w, r, bucket, key, readObject) {
			return
		}
		noteOperation(r.Context(), "HeadObject")
		s.handleHeadObject(w, r)
	case http.MethodDelete:
		if !s.permit(w, r, bucket, key, deleteObject) {
			return
		}
		noteOperation(r.Context(), "DeleteObject")
		s.handleDeleteObject(w, r)
	default:
		s.unsupported(w, r)
	}
}

// withBucket resolves the request's bucket before calling handler.
func (s *Server) withBucket(w http.ResponseWriter, r *http.Request, handler func(http.ResponseWriter, *http.Request, *db.Bucket)) {
	bucket, err := s.requireBucket(w, r, bucketOf(r))
	if err != nil {
		return
	}
	handler(w, r, bucket)
}

// withUpload resolves the bucket and passes the upload id through.
func (s *Server) withUpload(w http.ResponseWriter, r *http.Request, uploadID string, handler func(http.ResponseWriter, *http.Request, *db.Bucket, string)) {
	bucket, err := s.requireBucket(w, r, bucketOf(r))
	if err != nil {
		return
	}
	handler(w, r, bucket, uploadID)
}

// unsupported answers anything the router did not match. NotImplemented rather
// than a bare 404 tells a client the difference between "this object is
// missing" and "this server does not do that".
func (s *Server) unsupported(w http.ResponseWriter, r *http.Request) {
	WriteError(w, r, ErrNotImplemented.WithMessage(
		"%s %s is not supported by this server.", r.Method, r.URL.Path))
}

// pathParts carries the routed bucket and key through the request context.
type pathParts struct{ bucket, key string }

type pathKey struct{}

// bucketOf returns the bucket named by the request path.
func bucketOf(r *http.Request) string {
	parts, _ := r.Context().Value(pathKey{}).(pathParts)
	return parts.bucket
}

// keyOf returns the object key named by the request path, decoded but not
// normalized.
func keyOf(r *http.Request) string {
	parts, _ := r.Context().Value(pathKey{}).(pathParts)
	return parts.key
}

// splitPath splits an escaped request path into bucket and key.
//
// The escaped form is used because net/http has already decoded URL.Path, and
// decoding is not reversible: a key containing a literal "/" arrives as "%2F"
// and must stay a single key rather than becoming a path separator. Splitting
// before decoding preserves that distinction.
func splitPath(escapedPath string) (bucket, key string, err error) {
	trimmed := strings.TrimPrefix(escapedPath, "/")
	rawBucket, rawKey, _ := strings.Cut(trimmed, "/")

	if bucket, err = percentDecode(rawBucket); err != nil {
		return "", "", err
	}
	if key, err = percentDecode(rawKey); err != nil {
		return "", "", err
	}
	return bucket, key, nil
}

// writeOptions resolves the per-bucket behaviour a write depends on.
//
// Settings are read per request rather than cached. A cache would need
// invalidating from the console process, and the query is a single indexed
// lookup against a table with one row per bucket — cheaper than the bug.
func (s *Server) writeOptions(r *http.Request, bucket *db.Bucket) db.WriteOptions {
	settings, err := db.GetBucketSettings(r.Context(), s.DB, bucket.ID)
	if err != nil {
		// Failing closed here would reject writes because of a settings read.
		// Versioning off is the safe default: the write still succeeds, and
		// the worst case is a missing history entry, which is logged.
		s.Log.Warn("could not read bucket settings; proceeding without versioning",
			"bucket", bucket.Name, "error", err)
		return db.WriteOptions{}
	}

	actor := "s3"
	if identity, ok := IdentityFrom(r.Context()); ok {
		actor = identity.AccessKeyID
	}
	return db.WriteOptions{Versioning: settings.Versioning, Actor: actor}
}

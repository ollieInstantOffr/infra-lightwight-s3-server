package s3api

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

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
}

// Handler builds the routed, authenticated handler for the S3 listener.
//
// Middleware order is deliberate: request ids first so even a rejected request
// is traceable, then access logging so rejections are logged too, then
// authentication, and only then routing.
func (s *Server) Handler() http.Handler {
	return WithRequestID(WithAccessLog(s.Log,
		s.Verifier.Authenticate(s.Log, http.HandlerFunc(s.route))))
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
	bucket, key, err := splitPath(r.URL.EscapedPath())
	if err != nil {
		WriteError(w, r, ErrInvalidArgument.WithMessage("The request URI is not valid."))
		return
	}

	ctx := context.WithValue(r.Context(), pathKey{}, pathParts{bucket: bucket, key: key})
	r = r.WithContext(ctx)

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
		s.handleListBuckets(w, r)
		return
	}
	s.unsupported(w, r)
}

func (s *Server) routeBucket(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPut:
		s.handleCreateBucket(w, r)
	case http.MethodGet:
		s.handleGetBucket(w, r)
	case http.MethodHead:
		s.handleHeadBucket(w, r)
	case http.MethodDelete:
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
	uploadID := query.Get("uploadId")
	switch {
	case r.Method == http.MethodPost && query.Has("uploads"):
		s.withBucket(w, r, s.handleCreateMultipartUpload)
		return
	case r.Method == http.MethodPut && uploadID != "":
		s.withUpload(w, r, uploadID, s.handleUploadPart)
		return
	case r.Method == http.MethodPost && uploadID != "":
		s.withUpload(w, r, uploadID, s.handleCompleteMultipartUpload)
		return
	case r.Method == http.MethodDelete && uploadID != "":
		s.withUpload(w, r, uploadID, s.handleAbortMultipartUpload)
		return
	case r.Method == http.MethodGet && uploadID != "":
		s.withUpload(w, r, uploadID, s.handleListParts)
		return
	}

	switch r.Method {
	case http.MethodPut:
		s.handlePutObject(w, r)
	case http.MethodGet:
		s.handleGetObject(w, r)
	case http.MethodHead:
		s.handleHeadObject(w, r)
	case http.MethodDelete:
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

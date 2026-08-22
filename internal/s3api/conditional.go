package s3api

import (
	"net/http"
	"strings"
	"time"

	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/db"
)

// Conditional requests are what let a client re-fetch cheaply ("only if it
// changed") and write safely ("only if nobody else has touched it").
//
// http.ServeContent already handles If-None-Match and If-Modified-Since, but it
// does not know about If-Match or If-Unmodified-Since, and it compares ETags
// only once the response headers are set. These are checked up front so a
// failed precondition never begins streaming an object body.

// checkPreconditions evaluates the conditional headers against an object.
//
// It reports the status to return, or 0 to proceed. RFC 9110 fixes the
// evaluation order, and it matters: If-Match is checked before
// If-Unmodified-Since, and the ETag conditions take precedence over the
// date-based ones because ETags are exact where timestamps are only
// second-resolution.
func checkPreconditions(r *http.Request, object *db.Object) int {
	etag := quoteETag(object.ETag)
	modified := object.UpdatedAt.Truncate(time.Second)

	// If-Match: proceed only if the object still has one of these ETags.
	// Used for "replace only if unchanged".
	if header := r.Header.Get("If-Match"); header != "" {
		if !etagMatches(header, etag, false) {
			return http.StatusPreconditionFailed
		}
	} else if header := r.Header.Get("If-Unmodified-Since"); header != "" {
		if since, err := http.ParseTime(header); err == nil && modified.After(since) {
			return http.StatusPreconditionFailed
		}
	}

	// If-None-Match: proceed only if the object has none of these ETags.
	// A read gets 304, anything else gets 412.
	if header := r.Header.Get("If-None-Match"); header != "" {
		if etagMatches(header, etag, true) {
			if r.Method == http.MethodGet || r.Method == http.MethodHead {
				return http.StatusNotModified
			}
			return http.StatusPreconditionFailed
		}
	} else if header := r.Header.Get("If-Modified-Since"); header != "" {
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			if since, err := http.ParseTime(header); err == nil && !modified.After(since) {
				return http.StatusNotModified
			}
		}
	}

	return 0
}

// etagMatches reports whether candidate satisfies a comma-separated ETag list.
//
// "*" matches any existing object. allowWeak governs whether the weak
// comparison is used, which If-None-Match permits and If-Match does not: a weak
// ETag asserts semantic equivalence rather than byte equality, which is enough
// to skip a re-fetch but not enough to make a write safe.
func etagMatches(header, candidate string, allowWeak bool) bool {
	header = strings.TrimSpace(header)
	if header == "*" {
		return true
	}
	for _, raw := range strings.Split(header, ",") {
		entry := strings.TrimSpace(raw)
		weak := strings.HasPrefix(entry, "W/")
		if weak {
			if !allowWeak {
				continue
			}
			entry = strings.TrimPrefix(entry, "W/")
		}
		if entry == candidate {
			return true
		}
	}
	return false
}

// writeConditionalResponse sends a precondition outcome.
//
// A 304 must carry the validators the client will send next time, and must not
// carry a body — a body on a 304 is a protocol violation that some clients
// mis-parse as content.
func writeConditionalResponse(w http.ResponseWriter, r *http.Request, status int, object *db.Object) {
	if status == http.StatusNotModified {
		w.Header().Set("ETag", quoteETag(object.ETag))
		w.Header().Set("Last-Modified", formatHTTPTime(object.UpdatedAt))
		w.WriteHeader(http.StatusNotModified)
		return
	}
	WriteError(w, r, ErrPreconditionFailed)
}

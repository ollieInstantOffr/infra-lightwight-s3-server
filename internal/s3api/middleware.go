package s3api

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/db"
	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/httpx"
	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/logs"
)

// WithRequestID stamps every request with an identifier, echoes it back, and
// puts it in the context for logs and error bodies.
//
// This is the first middleware in the chain so that even a request rejected
// during authentication carries an id an operator can search for.
func WithRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := NewRequestID()
		w.Header().Set("x-amz-request-id", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id)))
	})
}

// responseRecorder captures what was actually sent, so the access log reports
// the real status and byte count rather than what the handler intended.
type responseRecorder struct {
	http.ResponseWriter
	status  int
	written int64
}

func (w *responseRecorder) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
		w.ResponseWriter.WriteHeader(status)
	}
}

func (w *responseRecorder) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(p)
	w.written += int64(n)
	return n, err
}

// Unwrap lets http.ResponseController reach the underlying writer, which
// matters for flushing and for hijacking a connection.
func (w *responseRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// RequestRecorder receives completed requests for the console's log. The logs
// sink satisfies it; taking an interface keeps this package independent of it.
type RequestRecorder interface {
	RecordRequest(entry db.RequestLog)
}

// WithAccessLog logs one line per request once it completes, to stdout and —
// when a recorder is supplied — to the console's queryable log.
//
// The access key id is included where authentication succeeded, which is what
// turns "someone is filling the disk" into "this credential is filling the
// disk". Secrets, signatures and presigned query parameters are never logged:
// a presigned URL in a log file is a working credential.
func WithAccessLog(log *slog.Logger, recorder RequestRecorder, proxies *httpx.ProxyTrust, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &responseRecorder{ResponseWriter: w}

		// The holder is attached further out, so both this and the metrics
		// middleware read the same one.
		info := infoFrom(r.Context())
		next.ServeHTTP(rec, r)

		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		attrs := []any{
			"request_id", RequestIDFrom(r.Context()),
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"bytes", rec.written,
			"duration_ms", time.Since(start).Milliseconds(),
		}
		if q := r.URL.RawQuery; q != "" {
			attrs = append(attrs, "query", q)
		}
		if id, ok := IdentityFrom(r.Context()); ok {
			attrs = append(attrs, "access_key_id", id.AccessKeyID)
		}

		// Client mistakes are not server problems; only 5xx deserves the
		// operator's attention at error level.
		// Marked request-scoped so these do not also land in the server event
		// log: they are already in the request log, with the reason attached.
		attrs = append(attrs, logs.Skip())
		switch {
		case rec.status >= 500:
			log.Error("request failed", attrs...)
		case rec.status >= 400:
			log.Warn("request rejected", attrs...)
		default:
			log.Info("request", attrs...)
		}

		if recorder == nil {
			return
		}

		bucket, key, code, reason := info.snapshot()
		if bucket == "" {
			// Authentication runs before routing, so a rejected request never
			// reached the code that knows its bucket. Deriving it here is what
			// makes the bucket filter work on exactly the requests someone
			// wants to filter.
			bucket, key, _ = splitPath(r.URL.EscapedPath())
		}
		bytesIn := r.ContentLength
		if bytesIn < 0 {
			// Unknown for a chunked upload until it has been read. Counting it
			// as zero understates rather than inventing a number.
			bytesIn = 0
		}
		accessKeyID := ""
		if id, ok := IdentityFrom(r.Context()); ok {
			accessKeyID = id.AccessKeyID
		}

		recorder.RecordRequest(db.RequestLog{
			At:        start,
			RequestID: RequestIDFrom(r.Context()),
			Surface:   "s3",
			Method:    r.Method,
			Bucket:    bucket,
			Key:       key,
			// The path only. The query string is dropped wholesale because a
			// presigned request carries its signature there.
			Path:        r.URL.Path,
			Status:      rec.status,
			ErrorCode:   code,
			Reason:      reason,
			BytesIn:     bytesIn,
			BytesOut:    rec.written,
			DurationMS:  int(time.Since(start).Milliseconds()),
			AccessKeyID: accessKeyID,
			ClientIP:    proxies.ClientIP(r),
			UserAgent:   r.UserAgent(),
		})
	})
}

// identityKey is the context key for the authenticated caller.
type identityKey struct{}

// WithIdentity stores the authenticated caller in the context.
func WithIdentity(ctx context.Context, id *Identity) context.Context {
	return context.WithValue(ctx, identityKey{}, id)
}

// IdentityFrom returns the authenticated caller, if the request got that far.
func IdentityFrom(ctx context.Context) (*Identity, bool) {
	id, ok := ctx.Value(identityKey{}).(*Identity)
	return id, ok
}

// Authenticate verifies the signature on every request and rejects anything
// that does not check out, in the S3 error dialect.
func (s *Server) Authenticate(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// CORS is applied before authentication because a preflight carries no
		// signature by definition: the browser sends OPTIONS with no
		// credentials, and rejecting it would block every cross-origin request
		// before the real one is ever attempted.
		if s.applyCORS(w, r) {
			return
		}

		id, err := s.Verifier.Verify(r.Context(), r)
		if err != nil {
			// A public bucket serves anonymous reads. Checked only after
			// verification has failed, so the cost falls on anonymous traffic
			// rather than on every signed request.
			if s.allowAnonymous(r) {
				next.ServeHTTP(w, r)
				return
			}

			// Logged in full here, but reported to the client only as the
			// mapped S3 code: the detail is useful to an operator and useful to
			// an attacker probing for valid access key ids.
			log.Warn("authentication failed",
				"request_id", RequestIDFrom(r.Context()),
				"method", r.Method,
				"path", r.URL.Path,
				"error", err,
				logs.Skip(),
			)
			// Captured explicitly: the mapped code deliberately hides the
			// cause from the client, and this is the one place that knows it.
			noteFailure(r.Context(), AsAPIError(err).Code, err.Error())
			WriteError(w, r, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(WithIdentity(r.Context(), id)))
	})
}

// WithMetrics counts requests for the console's overview.
//
// The counter accumulates in memory and is flushed periodically, so this adds
// a mutex and two increments to the request path rather than a database write.
func WithMetrics(counter RequestCounter, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder := &responseRecorder{ResponseWriter: w}
		next.ServeHTTP(recorder, r)

		status := recorder.status
		if status == 0 {
			status = http.StatusOK
		}
		// ContentLength is -1 for a chunked upload, where the size is not known
		// until it has been read; counting it as zero understates bytes in
		// rather than inventing a number.
		bytesIn := r.ContentLength
		if bytesIn < 0 {
			bytesIn = 0
		}
		counter.Record(status, bytesIn, recorder.written)
	})
}

// WithScrapeMetrics feeds the counters a Prometheus scrape reads.
//
// Separate from WithMetrics, which rolls counts into hourly cells for the
// console's chart. A scraper needs monotonic counters and a duration
// histogram; deriving those from an hourly rollup would hand it a counter that
// resets every hour, which is the one shape a counter must never have.
func WithScrapeMetrics(observer RequestObserver, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		recorder := &responseRecorder{ResponseWriter: w}
		next.ServeHTTP(recorder, r)

		status := recorder.status
		if status == 0 {
			status = http.StatusOK
		}
		bytesIn := r.ContentLength
		if bytesIn < 0 {
			bytesIn = 0
		}
		// The operation is whatever the router settled on. A request rejected
		// before routing — a bad signature, a denied scope — has none, and is
		// counted under Unknown rather than being dropped: those are exactly
		// the requests someone watching an error rate wants to see.
		observer.Observe("s3", Operation(r.Context()), status,
			time.Since(started), bytesIn, recorder.written)
	})
}

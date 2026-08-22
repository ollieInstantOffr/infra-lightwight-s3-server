package s3api

import (
	"context"
	"log/slog"
	"net/http"
	"time"
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

// WithAccessLog logs one line per request once it completes.
//
// The access key id is included where authentication succeeded, which is what
// turns "someone is filling the disk" into "this credential is filling the
// disk". Secrets and signatures are never logged.
func WithAccessLog(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &responseRecorder{ResponseWriter: w}

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
		switch {
		case rec.status >= 500:
			log.Error("request failed", attrs...)
		case rec.status >= 400:
			log.Warn("request rejected", attrs...)
		default:
			log.Info("request", attrs...)
		}
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
func (v *Verifier) Authenticate(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, err := v.Verify(r.Context(), r)
		if err != nil {
			// Logged in full here, but reported to the client only as the
			// mapped S3 code: the detail is useful to an operator and useful to
			// an attacker probing for valid access key ids.
			log.Warn("authentication failed",
				"request_id", RequestIDFrom(r.Context()),
				"method", r.Method,
				"path", r.URL.Path,
				"error", err,
			)
			WriteError(w, r, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(WithIdentity(r.Context(), id)))
	})
}

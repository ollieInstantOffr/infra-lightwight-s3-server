package s3api

import (
	"context"
	"net/http"
	"sync"
)

// What a request turned out to be is discovered in pieces, and by handlers
// deeper in the stack than the middleware that has to log it. Routing knows the
// bucket and key; the error writer knows why it failed.
//
// A context value cannot carry that back outward — contexts flow down. So the
// outermost middleware puts a mutable holder in the context and inner layers
// fill it in.

type requestInfo struct {
	mu        sync.Mutex
	bucket    string
	key       string
	errorCode string
	// reason is the server's own explanation, and is deliberately never sent
	// to the client. A prober is told InvalidAccessKeyId; an operator needs to
	// know the credential was revoked.
	reason string
	// operation is the S3 call name the router settled on, which the metrics
	// and the request log both want and neither can work out for itself.
	operation string
}

type requestInfoKey struct{}

// withRequestInfo attaches a holder for handlers to fill in.
func withRequestInfo(r *http.Request) (*http.Request, *requestInfo) {
	info := &requestInfo{}
	return r.WithContext(context.WithValue(r.Context(), requestInfoKey{}, info)), info
}

// WithRequestInfo attaches the holder that inner layers fill in.
//
// Outermost of the middleware that reads it, because a context flows down: a
// layer that attaches the holder itself is invisible to everything wrapping it.
// The access log used to create it, which meant the metrics middleware outside
// the access log saw an empty context and recorded every request as Unknown —
// wrong in the quietest possible way, since the metric existed and had
// plausible numbers in it.
func WithRequestInfo(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r, _ = withRequestInfo(r)
		next.ServeHTTP(w, r)
	})
}

func infoFrom(ctx context.Context) *requestInfo {
	info, _ := ctx.Value(requestInfoKey{}).(*requestInfo)
	return info
}

// noteTarget records which bucket and key a request was for.
func noteTarget(ctx context.Context, bucket, key string) {
	info := infoFrom(ctx)
	if info == nil {
		return
	}
	info.mu.Lock()
	defer info.mu.Unlock()
	info.bucket, info.key = bucket, key
}

// noteFailure records the client-visible code and the internal reason.
//
// The first failure wins. A handler that writes an error and then unwinds
// through another should not have the specific reason replaced by a generic
// one on the way out.
func noteFailure(ctx context.Context, code, reason string) {
	info := infoFrom(ctx)
	if info == nil {
		return
	}
	info.mu.Lock()
	defer info.mu.Unlock()
	if info.errorCode == "" {
		info.errorCode, info.reason = code, reason
	}
}

func (i *requestInfo) snapshot() (bucket, key, code, reason string) {
	if i == nil {
		return "", "", "", ""
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.bucket, i.key, i.errorCode, i.reason
}

// noteOperation records which S3 call this turned out to be.
//
// Recorded rather than derived after the fact, because working out that a POST
// with ?uploads is CreateMultipartUpload means reimplementing the router's own
// decision — and the two would drift. The router already knows; it just has to
// say so.
//
// The name is the S3 operation name, from a fixed set. That is what bounds the
// cardinality of the metric it feeds: a label drawn from a list this package
// controls cannot grow with traffic the way a bucket or key label would.
func noteOperation(ctx context.Context, operation string) {
	info := infoFrom(ctx)
	if info == nil {
		return
	}
	info.mu.Lock()
	defer info.mu.Unlock()
	info.operation = operation
}

// Operation returns the S3 call a request was routed to, if the router got
// far enough to decide.
func Operation(ctx context.Context) string {
	info := infoFrom(ctx)
	if info == nil {
		return ""
	}
	info.mu.Lock()
	defer info.mu.Unlock()
	return info.operation
}

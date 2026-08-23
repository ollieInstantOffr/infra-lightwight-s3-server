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
}

type requestInfoKey struct{}

// withRequestInfo attaches a holder for handlers to fill in.
func withRequestInfo(r *http.Request) (*http.Request, *requestInfo) {
	info := &requestInfo{}
	return r.WithContext(context.WithValue(r.Context(), requestInfoKey{}, info)), info
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

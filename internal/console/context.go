package console

import (
	"context"
	"net/http"
	"time"
)

// contextWithTimeout bounds a handler's work without outliving the request.
func contextWithTimeout(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), d)
}

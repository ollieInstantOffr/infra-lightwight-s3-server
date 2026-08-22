package console

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/db"
	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/httpx"
	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/secrets"
	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/storage"
)

// Server serves the console API and the embedded single-page app.
type Server struct {
	DB      *db.Pool
	Blobs   *storage.Store
	Cipher  *secrets.Cipher
	Mailer  Mailer
	Proxies *httpx.ProxyTrust
	Log     *slog.Logger

	// PublicURL is where the console is reached, used to build the absolute
	// links that go into emails. TLS terminates at the proxy, so this cannot be
	// derived from the request alone.
	PublicURL string
	// PublicS3URL is where the S3 API is reached, used for presigned links and
	// shown in the credentials screen's connection snippets.
	PublicS3URL string
	Region      string

	// SessionSecret signs the session cookie.
	SessionSecret string
	// Assets serves the built SPA. Nil until the frontend is embedded, in which
	// case the API still works and the root returns a plain message.
	Assets http.Handler

	// Now is injectable for tests.
	Now func() time.Time
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Handler builds the console's routes.
//
// Everything under /api requires a session except the authentication endpoints
// themselves. The SPA is served from everything else, with unknown paths
// falling back to index.html so client-side routing works on a hard refresh.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Health probes are unauthenticated so an orchestrator can reach them.
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)

	// Authentication. Open by necessity: these are how a session is obtained.
	mux.HandleFunc("POST /api/auth/magic-link", s.handleRequestMagicLink)
	mux.HandleFunc("GET /api/auth/callback", s.handleCallback)
	mux.HandleFunc("POST /api/auth/logout", s.handleLogout)
	mux.HandleFunc("GET /api/auth/me", s.requireSession(s.handleMe))

	// Users and invitations.
	mux.HandleFunc("GET /api/users", s.requireAdmin(s.handleListUsers))
	mux.HandleFunc("POST /api/users/invite", s.requireAdmin(s.handleInvite))
	mux.HandleFunc("DELETE /api/users/{id}", s.requireAdmin(s.handleDeleteUser))
	mux.HandleFunc("PUT /api/users/{id}/role", s.requireAdmin(s.handleSetRole))
	mux.HandleFunc("GET /api/invites", s.requireAdmin(s.handleListInvites))
	mux.HandleFunc("DELETE /api/invites/{id}", s.requireAdmin(s.handleRevokeInvite))

	// S3 credentials.
	mux.HandleFunc("GET /api/credentials", s.requireAdmin(s.handleListCredentials))
	mux.HandleFunc("POST /api/credentials", s.requireAdmin(s.handleCreateCredential))
	mux.HandleFunc("DELETE /api/credentials/{accessKeyId}", s.requireAdmin(s.handleRevokeCredential))

	// Buckets and objects.
	mux.HandleFunc("GET /api/buckets", s.requireSession(s.handleListBuckets))
	mux.HandleFunc("POST /api/buckets", s.requireSession(s.handleCreateBucket))
	mux.HandleFunc("DELETE /api/buckets/{bucket}", s.requireSession(s.handleDeleteBucket))
	mux.HandleFunc("GET /api/buckets/{bucket}/objects", s.requireSession(s.handleListObjects))
	mux.HandleFunc("POST /api/buckets/{bucket}/objects", s.requireSession(s.handleUploadObject))
	mux.HandleFunc("POST /api/buckets/{bucket}/objects/delete", s.requireSession(s.handleDeleteObjects))
	mux.HandleFunc("GET /api/buckets/{bucket}/object", s.requireSession(s.handleDownloadObject))
	mux.HandleFunc("POST /api/buckets/{bucket}/share", s.requireSession(s.handleShareObject))
	mux.HandleFunc("GET /api/dashboard", s.requireSession(s.handleDashboard))

	mux.HandleFunc("/", s.serveApp)

	return s.withRequestLog(mux)
}

// serveApp hands unmatched paths to the SPA.
func (s *Server) serveApp(w http.ResponseWriter, r *http.Request) {
	if s.Assets != nil {
		s.Assets.ServeHTTP(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("The console API is running. The web interface is not built into this binary.\n"))
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok\n"))
}

// handleReadyz reports whether the server can actually serve, which is not the
// same as being alive: a process with an unreachable database answers /healthz
// perfectly well while failing every request.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	checks := map[string]string{}
	ready := true

	ctx, cancel := contextWithTimeout(r, 3*time.Second)
	defer cancel()

	if err := s.DB.Ping(ctx); err != nil {
		checks["database"] = err.Error()
		ready = false
	} else {
		checks["database"] = "ok"
	}

	if _, err := s.Blobs.Usage(); err != nil {
		checks["storage"] = err.Error()
		ready = false
	} else {
		checks["storage"] = "ok"
	}

	status := http.StatusOK
	if !ready {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, map[string]any{"ready": ready, "checks": checks})
}

// withRequestLog logs one line per console request.
func (s *Server) withRequestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := s.now()
		recorder := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(recorder, r)

		if recorder.status == 0 {
			recorder.status = http.StatusOK
		}
		// Health probes would otherwise dominate the log.
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			return
		}
		s.Log.Info("console request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", recorder.status,
			"duration_ms", time.Since(start).Milliseconds(),
			"ip", s.Proxies.ClientIP(r),
		)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (w *statusRecorder) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
		w.ResponseWriter.WriteHeader(status)
	}
}

func (w *statusRecorder) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(p)
}

func (w *statusRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// writeJSON sends a JSON response.
func writeJSON(w http.ResponseWriter, status int, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		http.Error(w, `{"error":"could not encode response"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// writeError sends a JSON error with a message intended for a person.
func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

// internalError logs the real cause and tells the client only that something
// failed, so implementation detail stays out of the browser.
func (s *Server) internalError(w http.ResponseWriter, r *http.Request, operation string, err error) {
	s.Log.Error("console request failed",
		"operation", operation, "method", r.Method, "path", r.URL.Path, "error", err)
	writeError(w, http.StatusInternalServerError, "Something went wrong. Please try again.")
}

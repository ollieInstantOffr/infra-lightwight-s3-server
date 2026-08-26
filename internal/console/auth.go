package console

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/db"
)

// Authentication is by email and password, exchanged for a session cookie.
//
// It was previously by emailed link, which meant the console could not let
// anyone in at all when the mail provider was unreachable or unconfigured.
// Passwords remove that dependency; the cost is that a forgotten one has no
// self-service recovery, which `s3d user set-password` covers from the host.

const (
	// sessionIdleTTL logs out an inactive browser; sessionAbsoluteTTL is the
	// hard ceiling a session can never be extended past, no matter how active.
	sessionIdleTTL     = 12 * time.Hour
	sessionAbsoluteTTL = 30 * 24 * time.Hour

	sessionCookieName = "s3d_session"

	// loginFailureWindow is how far back failed attempts are counted. A lockout
	// therefore expires on its own rather than needing an administrator.
	loginFailureWindow = 15 * time.Minute

	// The two limits are deliberately different. The per-address one is tight,
	// because a real person mistyping their own password does not reach ten.
	// The per-IP one is looser, because an office behind one NAT address is a
	// legitimate source of many sign-ins and must not lock itself out.
	loginFailureLimitPerEmail = 10
	loginFailureLimitPerIP    = 50

	maxAuthBodySize = 4 << 10
)

// contextKey scopes values this package stores on a request context.
type contextKey struct{ name string }

var userContextKey = &contextKey{"user"}

// UserFrom returns the authenticated user, if the request carries a session.
func UserFrom(ctx context.Context) (*db.User, bool) {
	user, ok := ctx.Value(userContextKey).(*db.User)
	return user, ok
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// signInFailed is the only thing a failed sign-in ever says.
//
// It covers an unknown address, a wrong password, and an account that has no
// password set. Distinguishing them would tell an unauthenticated caller which
// addresses have accounts, which is the same reason the magic-link endpoint it
// replaced answered identically whether or not the address was known.
const signInFailed = "That email address or password is not correct."

// handleLogin exchanges an email and password for a session.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	// Sign-in is unauthenticated, so it sits outside requireSession and does
	// not inherit its CSRF check. A cross-site post here could log someone into
	// an account the attacker controls, which is a real attack: everything they
	// then do in the console is attributed to them.
	if !s.originAllowed(r) {
		s.Log.Warn("rejected a cross-origin sign-in", "origin", r.Header.Get("Origin"))
		writeError(w, http.StatusForbidden, "This request did not come from the console.")
		return
	}

	var request loginRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "Send a JSON body with an email address and a password.")
		return
	}

	ctx := r.Context()
	email := db.NormalizeEmail(request.Email)
	ip := s.Proxies.ClientIP(r)

	byEmail, byIP, err := db.CountRecentFailures(ctx, s.DB, email, ip, loginFailureWindow)
	if err != nil {
		s.internalError(w, r, "count sign-in failures", err)
		return
	}
	if byEmail >= loginFailureLimitPerEmail || byIP >= loginFailureLimitPerIP {
		s.Log.Warn("sign-in throttled",
			"email", email, "ip", ip, "byEmail", byEmail, "byIP", byIP)
		// Recorded so a sustained attack keeps the lockout alive rather than
		// letting it lapse while the attacker keeps trying.
		s.recordAttempt(ctx, email, ip, false)
		writeError(w, http.StatusTooManyRequests,
			"Too many sign-in attempts. Please wait a few minutes and try again.")
		return
	}

	user, err := db.VerifyPassword(ctx, s.DB, email, request.Password)
	if err != nil {
		s.recordAttempt(ctx, email, ip, false)
		switch {
		case errors.Is(err, db.ErrPasswordUnset):
			// Worth an operator seeing: it means an account exists that nobody
			// can use, which on a fresh deployment is the bootstrap admin and
			// is fixed by one command. The caller still learns nothing.
			s.Log.Warn("sign-in attempted for an account with no password set",
				"email", email, "ip", ip, "fix", "s3d user set-password "+email)
		case errors.Is(err, db.ErrPasswordIncorrect):
			s.Log.Info("failed sign-in", "email", email, "ip", ip)
		default:
			s.internalError(w, r, "verify password", err)
			return
		}
		writeError(w, http.StatusUnauthorized, signInFailed)
		return
	}

	s.recordAttempt(ctx, email, ip, true)
	// A successful sign-in clears the count, so someone who mistypes twice and
	// then succeeds is not left part-way to a lockout for the rest of the window.
	if err := db.ClearLoginFailures(ctx, s.DB, email); err != nil {
		s.Log.Warn("could not clear sign-in failures", "email", email, "error", err)
	}

	if err := s.startSession(w, r, user); err != nil {
		s.internalError(w, r, "start session", err)
		return
	}
	if err := db.RecordLogin(ctx, s.DB, user.ID); err != nil {
		// Cosmetic; never worth failing a sign-in over.
		s.Log.Warn("could not record login", "user", user.Email, "error", err)
	}

	s.Log.Info("signed in", "email", user.Email, "ip", ip)
	s.auditFor(ctx, user, db.ActionSignIn, "user", user.Email, ip, r.UserAgent(), nil)
	writeJSON(w, http.StatusOK, userResponse(user))
}

// recordAttempt writes an attempt for throttling. A failure to record is
// logged rather than returned: it must not turn a wrong password into a 500,
// and it must not stop a correct one from signing in.
func (s *Server) recordAttempt(ctx context.Context, email, ip string, successful bool) {
	if err := db.RecordLoginAttempt(ctx, s.DB, email, ip, successful); err != nil {
		s.Log.Warn("could not record sign-in attempt", "email", email, "error", err)
	}
}

// startSession issues a session cookie.
func (s *Server) startSession(w http.ResponseWriter, r *http.Request, user *db.User) error {
	token, hash, err := db.NewToken()
	if err != nil {
		return err
	}
	if _, err := db.CreateSession(r.Context(), s.DB, user.ID, hash,
		sessionIdleTTL, sessionAbsoluteTTL, r.UserAgent(), s.Proxies.ClientIP(r)); err != nil {
		return err
	}

	http.SetCookie(w, &http.Cookie{
		Name:  sessionCookieName,
		Value: token,
		Path:  "/",
		// HttpOnly keeps the cookie out of reach of any script on the page, so
		// a cross-site scripting bug cannot exfiltrate a session.
		HttpOnly: true,
		// Lax rather than Strict: the sign-in link is followed from an email
		// client, which is a cross-site navigation, and Strict would drop the
		// cookie on exactly that first request.
		SameSite: http.SameSiteLaxMode,
		// Set from the scheme the client actually used, which behind the proxy
		// is only visible in X-Forwarded-Proto.
		Secure:  s.Proxies.Scheme(r) == "https",
		Expires: s.now().Add(sessionAbsoluteTTL),
	})
	return nil
}

// handleLogout ends the session server-side as well as clearing the cookie.
// Clearing the cookie alone would leave a copied session token valid.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	// Logout is deliberately not behind requireSession: signing out with an
	// already-expired session must still clear the cookie rather than return a
	// 401 at the moment the user is trying to leave. The actor is therefore
	// resolved here, before the session is revoked and while it is still known.
	if cookie, err := r.Cookie(sessionCookieName); err == nil {
		hash := db.HashToken(cookie.Value)
		if user, err := db.TouchSession(r.Context(), s.DB, hash, sessionIdleTTL); err == nil {
			s.auditFor(r.Context(), user, db.ActionSignOut, "user", user.Email,
				s.Proxies.ClientIP(r), r.UserAgent(), nil)
		}
		if err := db.RevokeSession(r.Context(), s.DB, hash); err != nil {
			s.Log.Warn("could not revoke session", "error", err)
		}
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Value: "", Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode,
		Secure: s.Proxies.Scheme(r) == "https",
		MaxAge: -1,
	})
	writeJSON(w, http.StatusOK, map[string]string{"message": "Signed out."})
}

// handleMe reports the signed-in user, which is how the app decides what to
// render before making any other call.
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	user, _ := UserFrom(r.Context())
	writeJSON(w, http.StatusOK, userResponse(user))
}

// requireSession rejects unauthenticated requests.
func (s *Server) requireSession(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(sessionCookieName)
		if err != nil || cookie.Value == "" {
			writeError(w, http.StatusUnauthorized, "Not signed in.")
			return
		}

		user, err := db.TouchSession(r.Context(), s.DB, db.HashToken(cookie.Value), sessionIdleTTL)
		if err != nil {
			if errors.Is(err, db.ErrSessionInvalid) {
				writeError(w, http.StatusUnauthorized, "Your session has expired. Please sign in again.")
				return
			}
			s.internalError(w, r, "validate session", err)
			return
		}

		// A user whose password was chosen by someone else may do exactly two
		// things: change it, or leave. Enforcing it here rather than in the app
		// is what makes it unavoidable — a forced change that can be skipped by
		// navigating elsewhere, or by calling the API directly, is not forced.
		if user.MustChangePassword && !isPasswordChangeRequest(r) {
			writeJSON(w, http.StatusForbidden, map[string]string{
				"error": "Choose a new password before continuing.",
				// The app routes on this rather than matching the message.
				"code": codePasswordChangeRequired,
			})
			return
		}

		// Every state-changing request must also prove it did not originate
		// from another site.
		if isStateChanging(r) && !s.originAllowed(r) {
			s.Log.Warn("rejected a cross-origin state-changing request",
				"origin", r.Header.Get("Origin"), "path", r.URL.Path, "user", user.Email)
			writeError(w, http.StatusForbidden, "This request did not come from the console.")
			return
		}

		next(w, r.WithContext(context.WithValue(r.Context(), userContextKey, user)))
	}
}

// codePasswordChangeRequired is returned alongside the message so the app can
// route to the change screen without string-matching prose that may be reworded.
const codePasswordChangeRequired = "password_change_required"

// isPasswordChangeRequest reports whether a request is one of the two things a
// user with a forced password change is still allowed to do.
//
// /api/auth/me is included because the app calls it to discover it is in this
// state at all; refusing it would leave the console unable to explain itself.
func isPasswordChangeRequest(r *http.Request) bool {
	switch r.URL.Path {
	case "/api/account/password", "/api/auth/logout", "/api/auth/me":
		return true
	}
	return false
}

// requireAdmin additionally demands the admin role.
func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return s.requireSession(func(w http.ResponseWriter, r *http.Request) {
		user, _ := UserFrom(r.Context())
		if !user.IsAdmin() {
			writeError(w, http.StatusForbidden, "This action requires an administrator.")
			return
		}
		next(w, r)
	})
}

// isStateChanging reports whether a request can alter anything.
func isStateChanging(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	return true
}

// originAllowed checks the Origin header against the console's own address.
//
// This is the CSRF defence. SameSite=Lax already blocks cross-site form posts,
// but it is a browser-side control with uneven history across versions, and the
// cost of also checking here is one string comparison.
//
// A request with no Origin is allowed: non-browser clients such as curl send
// none, and they are not subject to CSRF in the first place.
func (s *Server) originAllowed(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	if origin == s.PublicURL {
		return true
	}
	// Also accept the address the request actually arrived on, which covers
	// local development where PublicURL is a placeholder.
	return origin == s.Proxies.Scheme(r)+"://"+s.Proxies.Host(r)
}

// redirectWithError sends the browser back to the app with a reason it can
// render, rather than showing a bare JSON error at an API URL.
func (s *Server) redirectWithError(w http.ResponseWriter, r *http.Request, reason string) {
	http.Redirect(w, r, s.PublicURL+"/sign-in?error="+url.QueryEscape(reason), http.StatusSeeOther)
}

// decodeJSON reads a bounded JSON body and refuses unknown fields, so a typo in
// a field name is reported rather than silently ignored.
func decodeJSON(r *http.Request, target any) error {
	decoder := json.NewDecoder(io.LimitReader(r.Body, maxAuthBodySize))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}

// looksLikeEmail is a loose shape check. Real validation is delivery.
func looksLikeEmail(email string) bool {
	at := strings.IndexByte(email, '@')
	if at <= 0 || at == len(email)-1 {
		return false
	}
	domain := email[at+1:]
	return !strings.ContainsAny(email, " \t") && strings.Contains(domain, ".") &&
		!strings.HasPrefix(domain, ".") && !strings.HasSuffix(domain, ".")
}

// userResponse is the shape the app receives. It never includes anything that
// is not already visible to the person it describes.
func userResponse(user *db.User) map[string]any {
	return map[string]any{
		"id":          user.ID,
		"email":       user.Email,
		"role":        user.Role,
		"isAdmin":     user.IsAdmin(),
		"createdAt":   user.CreatedAt,
		"lastLoginAt": user.LastLoginAt,
		// The app needs this to route to the change screen; it describes the
		// person receiving it and tells them nothing they do not already know.
		"mustChangePassword": user.MustChangePassword,
	}
}

// withOptionalSession attaches the caller's identity when they have one, and
// lets the request through when they do not.
//
// For endpoints that accept more than one kind of credential and have to decide
// for themselves — /metrics takes either a scraper's bearer token or an
// administrator's session, and requireSession would reject the scraper before
// the handler ever saw its token.
//
// It grants nothing on its own. A handler behind this is responsible for its
// own refusal, which is a sharper edge than requireSession and the reason it is
// used in exactly one place.
func (s *Server) withOptionalSession(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(sessionCookieName)
		if err != nil || cookie.Value == "" {
			next(w, r)
			return
		}
		user, err := db.TouchSession(r.Context(), s.DB, db.HashToken(cookie.Value), sessionIdleTTL)
		if err != nil {
			next(w, r)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), userContextKey, user)))
	}
}

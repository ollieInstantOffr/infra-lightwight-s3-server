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

// Authentication is passwordless. A user asks for a link, receives one by
// email, and clicking it exchanges a single-use token for a session cookie.
// There is no password to choose, forget, reuse or leak.

const (
	// magicLinkTTL is short on purpose. The link is a bearer credential sitting
	// in an inbox; fifteen minutes is long enough to fetch an email and short
	// enough that an old one in a synced mailbox is inert.
	magicLinkTTL = 15 * time.Minute

	// inviteTTL is longer because an invitation may sit unread over a weekend.
	inviteTTL = 7 * 24 * time.Hour

	// sessionIdleTTL logs out an inactive browser; sessionAbsoluteTTL is the
	// hard ceiling a session can never be extended past, no matter how active.
	sessionIdleTTL     = 12 * time.Hour
	sessionAbsoluteTTL = 30 * 24 * time.Hour

	sessionCookieName = "s3d_session"

	// magicLinkRateWindow and magicLinkRateLimit bound how often a single
	// address can be mailed. Without this the endpoint is an open relay for
	// flooding someone's inbox, since anyone can name any address.
	magicLinkRateWindow = 15 * time.Minute
	magicLinkRateLimit  = 5

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

type magicLinkRequest struct {
	Email string `json:"email"`
}

// handleRequestMagicLink issues a sign-in link.
//
// The response is identical whether or not the address is known. Anything else
// turns this endpoint into a way to test which addresses have accounts, and it
// is unauthenticated by necessity.
func (s *Server) handleRequestMagicLink(w http.ResponseWriter, r *http.Request) {
	var request magicLinkRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "Send a JSON body with an email address.")
		return
	}

	email := db.NormalizeEmail(request.Email)
	// Deliberately vague, and the same shape as the success case.
	const accepted = "If that address can sign in, a link is on its way."

	if !looksLikeEmail(email) {
		writeJSON(w, http.StatusOK, map[string]string{"message": accepted})
		return
	}

	ctx := r.Context()
	allowed, err := s.maySignIn(ctx, email)
	if err != nil {
		s.internalError(w, r, "check sign-in eligibility", err)
		return
	}
	if !allowed {
		// Logged so an operator can see attempts, but the caller learns nothing.
		s.Log.Info("sign-in requested for an address that cannot sign in",
			"email", email, "ip", s.Proxies.ClientIP(r))
		writeJSON(w, http.StatusOK, map[string]string{"message": accepted})
		return
	}

	recent, err := db.CountRecentMagicLinks(ctx, s.DB, email, magicLinkRateWindow)
	if err != nil {
		s.internalError(w, r, "count recent magic links", err)
		return
	}
	if recent >= magicLinkRateLimit {
		s.Log.Warn("magic link rate limit reached",
			"email", email, "ip", s.Proxies.ClientIP(r), "window", magicLinkRateWindow)
		// Still the same response: revealing the limit would reveal the address
		// exists.
		writeJSON(w, http.StatusOK, map[string]string{"message": accepted})
		return
	}

	token, hash, err := db.NewToken()
	if err != nil {
		s.internalError(w, r, "generate token", err)
		return
	}
	if err := db.CreateMagicLink(ctx, s.DB, email, hash, magicLinkTTL, s.Proxies.ClientIP(r)); err != nil {
		s.internalError(w, r, "create magic link", err)
		return
	}

	link := s.PublicURL + "/api/auth/callback?token=" + url.QueryEscape(token)
	subject, text, htmlBody := magicLinkEmail(link, magicLinkTTL)
	if err := s.Mailer.Send(ctx, email, subject, text, htmlBody); err != nil {
		// A send failure is a real error worth reporting: the user would
		// otherwise wait for an email that is never coming.
		s.Log.Error("could not send sign-in email", "email", email, "error", err)
		writeError(w, http.StatusBadGateway, "The sign-in email could not be sent. Please try again shortly.")
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"message": accepted})
}

// maySignIn reports whether an address is entitled to a link: an existing user,
// or someone holding a live invitation.
func (s *Server) maySignIn(ctx context.Context, email string) (bool, error) {
	if _, err := db.GetUserByEmail(ctx, s.DB, email); err == nil {
		return true, nil
	} else if !errors.Is(err, db.ErrUserNotFound) {
		return false, err
	}
	return db.HasPendingInvite(ctx, s.DB, email)
}

// handleCallback exchanges a token for a session and redirects into the app.
func (s *Server) handleCallback(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if token == "" {
		s.redirectWithError(w, r, "missing")
		return
	}
	ctx := r.Context()

	email, err := db.ConsumeMagicLink(ctx, s.DB, db.HashToken(token))
	if err != nil {
		if errors.Is(err, db.ErrTokenInvalid) {
			// Covers unknown, expired and already-used alike. A user who clicks
			// twice sees the same thing as one with a stale link, which is both
			// simpler to explain and safer.
			s.redirectWithError(w, r, "expired")
			return
		}
		s.internalError(w, r, "consume magic link", err)
		return
	}

	user, err := s.resolveUser(ctx, email)
	if err != nil {
		if errors.Is(err, db.ErrUserNotFound) {
			s.redirectWithError(w, r, "not-invited")
			return
		}
		s.internalError(w, r, "resolve user", err)
		return
	}

	if err := s.startSession(w, r, user); err != nil {
		s.internalError(w, r, "start session", err)
		return
	}
	if err := db.RecordLogin(ctx, s.DB, user.ID); err != nil {
		// Cosmetic; never worth failing a login over.
		s.Log.Warn("could not record login", "user", user.Email, "error", err)
	}

	s.Log.Info("signed in", "email", user.Email, "ip", s.Proxies.ClientIP(r))
	s.auditFor(r.Context(), user, db.ActionSignIn, "user", user.Email,
		s.Proxies.ClientIP(r), r.UserAgent(), nil)
	http.Redirect(w, r, s.PublicURL+"/", http.StatusSeeOther)
}

// resolveUser finds the account for a verified address, creating it if the
// address holds an unredeemed invitation. Accepting the invite here rather than
// on a separate screen means a single click both admits and signs in.
func (s *Server) resolveUser(ctx context.Context, email string) (*db.User, error) {
	user, err := db.GetUserByEmail(ctx, s.DB, email)
	if err == nil {
		return user, nil
	}
	if !errors.Is(err, db.ErrUserNotFound) {
		return nil, err
	}

	pending, err := db.HasPendingInvite(ctx, s.DB, email)
	if err != nil {
		return nil, err
	}
	if !pending {
		return nil, db.ErrUserNotFound
	}
	// The invitation's role is applied, so an admin can invite another admin.
	return db.CreateUser(ctx, s.DB, email, db.RoleMember)
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
	}
}

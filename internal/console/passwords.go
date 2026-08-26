package console

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/db"
)

// Setting and changing passwords.
//
// There is no emailed reset link, by design: the console does not depend on a
// mail provider to let anyone in. That leaves three ways a password is set —
// the owner changes their own, an administrator sets a starting one, or
// `s3d user set-password` sets one from the host when nobody can sign in at
// all — and the last of those is the reason the other two can be this simple.

type changePasswordRequest struct {
	CurrentPassword string `json:"currentPassword"`
	NewPassword     string `json:"newPassword"`
}

// handleChangePassword lets a user replace their own password.
//
// The current password is required even though the caller already holds a
// session. Without it, a stolen session could change the password and lock the
// real owner out of their own account; with it, the thief has to know the
// secret they were trying to replace.
func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	user, _ := UserFrom(r.Context())

	var request changePasswordRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "Send a JSON body with the current and new passwords.")
		return
	}

	ctx := r.Context()
	if _, err := db.VerifyPassword(ctx, s.DB, user.Email, request.CurrentPassword); err != nil {
		if errors.Is(err, db.ErrPasswordIncorrect) || errors.Is(err, db.ErrPasswordUnset) {
			s.Log.Info("failed password change: wrong current password",
				"user", user.Email, "ip", s.Proxies.ClientIP(r))
			writeError(w, http.StatusUnauthorized, "Your current password is not correct.")
			return
		}
		s.internalError(w, r, "verify current password", err)
		return
	}

	// Rejecting a no-op change matters most in the forced-change case, where
	// re-entering the administrator's password would clear the flag while
	// leaving the shared secret in place.
	if request.NewPassword == request.CurrentPassword {
		writeError(w, http.StatusBadRequest, "The new password must be different from the current one.")
		return
	}
	if err := db.ValidatePassword(request.NewPassword); err != nil {
		writeError(w, http.StatusBadRequest, passwordRuleMessage(err))
		return
	}

	if err := db.SetPassword(ctx, s.DB, user.ID, request.NewPassword, false); err != nil {
		s.internalError(w, r, "set password", err)
		return
	}

	// Every other session is ended. If the reason for changing the password is
	// that somebody else had it, leaving their session alive defeats the point.
	cookie, err := r.Cookie(sessionCookieName)
	if err == nil {
		if _, err := db.RevokeOtherSessions(ctx, s.DB, user.ID, db.HashToken(cookie.Value)); err != nil {
			s.Log.Warn("could not revoke other sessions after a password change",
				"user", user.Email, "error", err)
		}
	}

	s.Log.Info("password changed", "user", user.Email, "ip", s.Proxies.ClientIP(r))
	s.audit(r, db.ActionPasswordChange, "user", user.Email, nil)
	writeJSON(w, http.StatusOK, map[string]string{
		"message": "Password changed. Any other sessions have been signed out.",
	})
}

type setPasswordRequest struct {
	// Password is optional. Omitted, the server generates one, which is the
	// better default: an administrator inventing passwords for other people
	// tends to invent weak and reused ones.
	Password string `json:"password"`
}

// handleResetPassword lets an administrator set someone else's password.
//
// The result always comes back flagged for change. The administrator now knows
// it, so it is a shared secret until its owner replaces it.
func (s *Server) handleResetPassword(w http.ResponseWriter, r *http.Request) {
	actor, _ := UserFrom(r.Context())
	ctx := r.Context()

	target, err := db.GetUserByID(ctx, s.DB, r.PathValue("id"))
	if err != nil {
		if errors.Is(err, db.ErrUserNotFound) {
			writeError(w, http.StatusNotFound, "No such user.")
			return
		}
		s.internalError(w, r, "look up user", err)
		return
	}

	var request setPasswordRequest
	if r.ContentLength > 0 {
		if err := decodeJSON(r, &request); err != nil {
			writeError(w, http.StatusBadRequest, "Send a JSON body with a password, or no body at all.")
			return
		}
	}

	password := request.Password
	generated := password == ""
	if generated {
		if password, err = generatePassword(); err != nil {
			s.internalError(w, r, "generate password", err)
			return
		}
	} else if err := db.ValidatePassword(password); err != nil {
		writeError(w, http.StatusBadRequest, passwordRuleMessage(err))
		return
	}

	if err := db.SetPassword(ctx, s.DB, target.ID, password, true); err != nil {
		s.internalError(w, r, "set password", err)
		return
	}
	// The old password is no longer trusted, so neither is anything signed in
	// with it.
	if err := db.RevokeUserSessions(ctx, s.DB, target.ID); err != nil {
		s.Log.Warn("could not revoke sessions after a password reset",
			"user", target.Email, "error", err)
	}

	s.Log.Info("password reset by an administrator",
		"user", target.Email, "by", actor.Email, "generated", generated)
	s.audit(r, db.ActionPasswordReset, "user", target.Email, map[string]any{"generated": generated})

	writeJSON(w, http.StatusOK, map[string]any{
		"email": target.Email,
		// Returned once and never retrievable again: only the hash is stored.
		"password": password,
		"message":  "Give this to the user. It is not shown again, and they must change it when they sign in.",
	})
}

type createUserRequest struct {
	Email    string `json:"email"`
	Role     string `json:"role"`
	Password string `json:"password"`
}

// handleCreateUser adds an account with a starting password.
//
// This replaces inviting by email. Without a mail provider there is nothing to
// send an invitation to, so the administrator creates the account and passes
// the password on however they already talk to that person.
func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var request createUserRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "Send a JSON body with an email address.")
		return
	}

	email := db.NormalizeEmail(request.Email)
	if !looksLikeEmail(email) {
		writeError(w, http.StatusBadRequest, "That does not look like an email address.")
		return
	}
	role := request.Role
	if role == "" {
		role = db.RoleMember
	}
	if role != db.RoleAdmin && role != db.RoleMember {
		writeError(w, http.StatusBadRequest, "Role must be ADMIN or MEMBER.")
		return
	}

	ctx := r.Context()
	if _, err := db.GetUserByEmail(ctx, s.DB, email); err == nil {
		writeError(w, http.StatusConflict, "That address already has an account.")
		return
	} else if !errors.Is(err, db.ErrUserNotFound) {
		s.internalError(w, r, "check existing user", err)
		return
	}

	password := request.Password
	generated := password == ""
	var err error
	if generated {
		if password, err = generatePassword(); err != nil {
			s.internalError(w, r, "generate password", err)
			return
		}
	} else if err := db.ValidatePassword(password); err != nil {
		writeError(w, http.StatusBadRequest, passwordRuleMessage(err))
		return
	}

	user, err := db.CreateUser(ctx, s.DB, email, role)
	if err != nil {
		s.internalError(w, r, "create user", err)
		return
	}
	if err := db.SetPassword(ctx, s.DB, user.ID, password, true); err != nil {
		// The account exists but cannot be signed in to. Saying so is better
		// than reporting success and leaving an unusable account behind.
		s.internalError(w, r, "set initial password", err)
		return
	}

	actor, _ := UserFrom(ctx)
	s.Log.Info("created a user", "email", email, "role", role, "by", actor.Email)
	s.audit(r, db.ActionUserCreate, "user", email, map[string]any{"role": role})

	writeJSON(w, http.StatusCreated, map[string]any{
		"user":     userResponse(user),
		"password": password,
		"message":  "Give this to the user. It is not shown again, and they must change it when they sign in.",
	})
}

// passwordRuleMessage turns a validation error into something worth reading.
// The rule is a length floor and nothing else, so saying so is more useful than
// a generic refusal.
func passwordRuleMessage(err error) string {
	switch {
	case errors.Is(err, db.ErrPasswordTooShort):
		return fmt.Sprintf("Use at least %d characters. Length is the only rule; a passphrase is fine.",
			db.MinPasswordLength)
	case errors.Is(err, db.ErrPasswordTooLong):
		return fmt.Sprintf("That is over the %d-byte limit. Anything longer would be silently truncated.",
			db.MaxPasswordLength)
	default:
		return "That password cannot be used."
	}
}

// generatePassword produces a starting password.
//
// Base64 of 18 random bytes: 144 bits of entropy, comfortably long enough that
// the length floor is irrelevant, and safe to read aloud or paste. Padding is
// stripped because a trailing '=' invites someone to drop it.
func generatePassword() (string, error) {
	raw := make([]byte, 18)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	return strings.TrimRight(base64.URLEncoding.EncodeToString(raw), "="), nil
}

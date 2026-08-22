package console

import (
	"errors"
	"net/http"
	"net/url"

	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/db"
)

// handleListUsers returns the console's members.
func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := db.ListUsers(r.Context(), s.DB)
	if err != nil {
		s.internalError(w, r, "list users", err)
		return
	}
	out := make([]map[string]any, 0, len(users))
	for i := range users {
		out = append(out, userResponse(&users[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": out})
}

type inviteRequest struct {
	Email string `json:"email"`
	Role  string `json:"role"`
}

// handleInvite sends an invitation.
//
// Unlike the sign-in endpoint, this one reports exactly what happened: the
// caller is already an authenticated admin, so there is nothing to conceal and
// a vague response would only make the screen harder to use.
func (s *Server) handleInvite(w http.ResponseWriter, r *http.Request) {
	var request inviteRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "Send a JSON body with an email address and a role.")
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

	inviter, _ := UserFrom(ctx)
	token, hash, err := db.NewToken()
	if err != nil {
		s.internalError(w, r, "generate invite token", err)
		return
	}
	invite, err := db.CreateInvite(ctx, s.DB, email, role, &inviter.ID, hash, inviteTTL)
	if err != nil {
		s.internalError(w, r, "create invite", err)
		return
	}

	// The invitation link goes through the same callback as a sign-in link, so
	// accepting an invitation and signing in are one click rather than two.
	link := s.PublicURL + "/api/auth/callback?token=" + url.QueryEscape(token)
	subject, text, htmlBody := inviteEmail(link, inviteTTL)
	if err := s.Mailer.Send(ctx, email, subject, text, htmlBody); err != nil {
		s.Log.Error("could not send invitation", "email", email, "error", err)
		writeError(w, http.StatusBadGateway,
			"The invitation was created but the email could not be sent. Check the Resend configuration.")
		return
	}

	s.Log.Info("invited a user", "email", email, "role", role, "by", inviter.Email)
	writeJSON(w, http.StatusCreated, inviteResponse(invite))
}

// handleListInvites returns pending invitations.
func (s *Server) handleListInvites(w http.ResponseWriter, r *http.Request) {
	invites, err := db.ListPendingInvites(r.Context(), s.DB)
	if err != nil {
		s.internalError(w, r, "list invites", err)
		return
	}
	out := make([]map[string]any, 0, len(invites))
	for i := range invites {
		out = append(out, inviteResponse(&invites[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"invites": out})
}

// handleRevokeInvite withdraws a pending invitation.
func (s *Server) handleRevokeInvite(w http.ResponseWriter, r *http.Request) {
	switch err := db.RevokeInvite(r.Context(), s.DB, r.PathValue("id")); {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]string{"message": "Invitation withdrawn."})
	case errors.Is(err, db.ErrInviteInvalid):
		writeError(w, http.StatusNotFound, "That invitation no longer exists.")
	default:
		s.internalError(w, r, "revoke invite", err)
	}
}

// handleDeleteUser removes a member and ends their sessions.
func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	targetID := r.PathValue("id")
	actor, _ := UserFrom(r.Context())

	// Removing yourself would sign you out mid-action and, if you were the only
	// admin, lock everyone out. The last-admin rule covers the second case; this
	// covers the merely confusing one.
	if targetID == actor.ID {
		writeError(w, http.StatusBadRequest, "You cannot remove your own account.")
		return
	}

	// Sessions are revoked first. Deleting the row cascades them away, but only
	// once the delete succeeds — and it can fail on the last-admin rule, in
	// which case revoking early is harmless.
	if err := db.RevokeUserSessions(r.Context(), s.DB, targetID); err != nil {
		s.internalError(w, r, "revoke user sessions", err)
		return
	}

	switch err := db.DeleteUser(r.Context(), s.DB, targetID); {
	case err == nil:
		s.Log.Info("removed a user", "id", targetID, "by", actor.Email)
		writeJSON(w, http.StatusOK, map[string]string{"message": "Member removed."})
	case errors.Is(err, db.ErrUserNotFound):
		writeError(w, http.StatusNotFound, "That member no longer exists.")
	case errors.Is(err, db.ErrLastAdmin):
		writeError(w, http.StatusConflict,
			"That is the only administrator. Promote someone else first.")
	default:
		s.internalError(w, r, "delete user", err)
	}
}

type roleRequest struct {
	Role string `json:"role"`
}

// handleSetRole promotes or demotes a member.
func (s *Server) handleSetRole(w http.ResponseWriter, r *http.Request) {
	var request roleRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "Send a JSON body with a role.")
		return
	}
	if request.Role != db.RoleAdmin && request.Role != db.RoleMember {
		writeError(w, http.StatusBadRequest, "Role must be ADMIN or MEMBER.")
		return
	}

	actor, _ := UserFrom(r.Context())
	switch err := db.SetUserRole(r.Context(), s.DB, r.PathValue("id"), request.Role); {
	case err == nil:
		s.Log.Info("changed a user's role", "id", r.PathValue("id"), "role", request.Role, "by", actor.Email)
		writeJSON(w, http.StatusOK, map[string]string{"message": "Role updated."})
	case errors.Is(err, db.ErrUserNotFound):
		writeError(w, http.StatusNotFound, "That member no longer exists.")
	case errors.Is(err, db.ErrLastAdmin):
		writeError(w, http.StatusConflict,
			"That is the only administrator. Promote someone else before demoting them.")
	default:
		s.internalError(w, r, "set user role", err)
	}
}

func inviteResponse(invite *db.Invite) map[string]any {
	return map[string]any{
		"id":        invite.ID,
		"email":     invite.Email,
		"role":      invite.Role,
		"createdAt": invite.CreatedAt,
		"expiresAt": invite.ExpiresAt,
	}
}

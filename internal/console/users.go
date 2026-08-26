package console

import (
	"errors"
	"net/http"

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
		s.audit(r, db.ActionUserRemove, "user", targetID, nil)
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
		s.audit(r, db.ActionUserRole, "user", r.PathValue("id"), map[string]any{"role": request.Role})
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

package console

import (
	"context"
	"net/http"

	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/db"
)

// audit records a console action against the person who took it.
//
// Failures are logged and swallowed. An audit write must never fail the action
// it describes: refusing a bucket deletion because the log was unwritable would
// be a worse outcome than an incomplete log, and the failure is loud in the
// server's own log either way.
func (s *Server) audit(r *http.Request, action, subjectType, subject string, detail map[string]any) {
	user, ok := UserFrom(r.Context())
	if !ok {
		return
	}

	ip := s.Proxies.ClientIP(r)
	userAgent := r.UserAgent()
	event := db.AuditEvent{
		ActorEmail:  user.Email,
		Action:      action,
		SubjectType: subjectType,
		Subject:     subject,
		Detail:      detail,
		IP:          &ip,
		UserAgent:   &userAgent,
	}

	// Written on the request's own context. If the client disconnects
	// mid-action the entry may be lost, which is preferable to holding a
	// connection open past the request that caused it.
	if err := db.RecordAudit(r.Context(), s.DB, event, &user.ID); err != nil {
		s.Log.Warn("could not record audit event",
			"action", action, "actor", user.Email, "error", err)
	}
}

// auditFor records an action for a known user outside a handler's own session,
// which sign-in needs: the session does not exist yet when the event happens.
func (s *Server) auditFor(ctx context.Context, user *db.User, action, subjectType, subject, ip, userAgent string, detail map[string]any) {
	event := db.AuditEvent{
		ActorEmail:  user.Email,
		Action:      action,
		SubjectType: subjectType,
		Subject:     subject,
		Detail:      detail,
		IP:          &ip,
		UserAgent:   &userAgent,
	}
	if err := db.RecordAudit(ctx, s.DB, event, &user.ID); err != nil {
		s.Log.Warn("could not record audit event", "action", action, "actor", user.Email, "error", err)
	}
}

// handleAuditLog returns a page of the audit log.
func (s *Server) handleAuditLog(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()

	filter := db.AuditFilter{
		Actor:  query.Get("actor"),
		Action: query.Get("action"),
		Limit:  intParam(query.Get("limit"), 50),
		Before: int64Param(query.Get("before")),
	}

	events, err := db.ListAuditEvents(r.Context(), s.DB, filter)
	if err != nil {
		s.internalError(w, r, "list audit events", err)
		return
	}
	actors, err := db.DistinctAuditActors(r.Context(), s.DB)
	if err != nil {
		s.internalError(w, r, "list audit actors", err)
		return
	}

	entries := make([]map[string]any, 0, len(events))
	for _, event := range events {
		entries = append(entries, map[string]any{
			"id":          event.ID,
			"actor":       event.ActorEmail,
			"action":      event.Action,
			"subjectType": event.SubjectType,
			"subject":     event.Subject,
			"detail":      event.Detail,
			"ip":          event.IP,
			"userAgent":   event.UserAgent,
			"createdAt":   event.CreatedAt,
		})
	}

	// The cursor is the last id returned, so the next page continues from
	// exactly where this one ended even as new events arrive at the top.
	var nextBefore any
	if len(events) == filter.Limit && len(events) > 0 {
		nextBefore = events[len(events)-1].ID
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"events":     entries,
		"actors":     actors,
		"actions":    auditActions(),
		"nextBefore": nextBefore,
	})
}

// auditActions is the fixed list the filter control offers.
func auditActions() []map[string]string {
	return []map[string]string{
		{"value": db.ActionSignIn, "label": "Signed in"},
		{"value": db.ActionSignOut, "label": "Signed out"},
		{"value": db.ActionBucketCreate, "label": "Bucket created"},
		{"value": db.ActionBucketDelete, "label": "Bucket deleted"},
		{"value": db.ActionBucketSettings, "label": "Bucket settings changed"},
		{"value": db.ActionObjectUpload, "label": "Object uploaded"},
		{"value": db.ActionObjectDelete, "label": "Object deleted"},
		{"value": db.ActionObjectRestore, "label": "Version restored"},
		{"value": db.ActionObjectPurge, "label": "Versions purged"},
		{"value": db.ActionShareLink, "label": "Share link created"},
		{"value": db.ActionCredentialCreate, "label": "Access key created"},
		{"value": db.ActionCredentialRevoke, "label": "Access key revoked"},
		{"value": db.ActionUserCreate, "label": "User created"},
		{"value": db.ActionPasswordChange, "label": "Password changed"},
		{"value": db.ActionPasswordReset, "label": "Password reset"},
		{"value": db.ActionUserRemove, "label": "User removed"},
		{"value": db.ActionUserRole, "label": "Role changed"},
		// Invitations are gone, but entries recorded before that release are
		// still in the log and would render as a bare action string without a
		// label to match them.
		{"value": db.ActionUserInvite, "label": "User invited (before 0008)"},
		{"value": db.ActionInviteRevoke, "label": "Invitation withdrawn (before 0008)"},
		{"value": db.ActionSessionRevoke, "label": "Session revoked"},
	}
}

package db

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// The audit log answers "who did that, and when". It is written from the
// console, where actions are attributable to a person; the S3 API is
// attributable only to a credential, and that belongs in the request log
// rather than here.
//
// It never records object contents, secrets, or tokens.

// Audit actions. Constants rather than free strings so the filter UI can offer
// a fixed list and a typo cannot silently create a new category.
const (
	ActionSignIn           = "auth.sign_in"
	ActionSignOut          = "auth.sign_out"
	ActionBucketCreate     = "bucket.create"
	ActionBucketDelete     = "bucket.delete"
	ActionBucketSettings   = "bucket.settings"
	ActionObjectUpload     = "object.upload"
	ActionObjectDelete     = "object.delete"
	ActionObjectRestore    = "object.restore"
	ActionObjectPurge      = "object.purge"
	ActionShareLink        = "object.share"
	ActionCredentialCreate = "credential.create"
	ActionCredentialRevoke = "credential.revoke"
	ActionUserInvite       = "user.invite"
	ActionUserRemove       = "user.remove"
	ActionUserRole         = "user.role"
	ActionInviteRevoke     = "invite.revoke"
	ActionSessionRevoke    = "session.revoke"
)

// AuditEvent is one recorded action.
type AuditEvent struct {
	ID          int64
	ActorEmail  string
	Action      string
	SubjectType string
	Subject     string
	Detail      map[string]any
	IP          *string
	UserAgent   *string
	CreatedAt   time.Time
}

// RecordAudit writes an audit entry.
//
// actorEmail is stored alongside the user reference so the log still reads
// correctly after the account is deleted. An audit trail that forgets who acted
// once they leave is not an audit trail.
func RecordAudit(ctx context.Context, q Querier, event AuditEvent, actorID *string) error {
	detail, err := json.Marshal(event.Detail)
	if err != nil {
		return fmt.Errorf("encode audit detail: %w", err)
	}
	_, err = q.Exec(ctx, `
		INSERT INTO audit_events
			(actor_id, actor_email, action, subject_type, subject, detail, ip, user_agent)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		actorID, event.ActorEmail, event.Action, event.SubjectType, event.Subject,
		detail, nullableIPPtr(event.IP), event.UserAgent)
	if err != nil {
		return fmt.Errorf("record audit event: %w", err)
	}
	return nil
}

// AuditFilter narrows an audit listing.
type AuditFilter struct {
	Actor  string
	Action string
	// Before paginates: only events older than this id are returned. An id
	// cursor rather than an offset, so a busy log does not shift rows between
	// pages while they are being read.
	Before int64
	Limit  int
}

// ListAuditEvents returns a page of the log, newest first.
func ListAuditEvents(ctx context.Context, q Querier, filter AuditFilter) ([]AuditEvent, error) {
	limit := filter.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}

	query := `
		SELECT id, actor_email, action, subject_type, subject, detail, host(ip), user_agent, created_at
		FROM audit_events
		WHERE ($1 = '' OR actor_email = $1)
		  AND ($2 = '' OR action = $2)
		  AND ($3 = 0 OR id < $3)
		ORDER BY id DESC
		LIMIT $4`

	rows, err := q.Query(ctx, query, filter.Actor, filter.Action, filter.Before, limit)
	if err != nil {
		return nil, fmt.Errorf("list audit events: %w", err)
	}
	defer rows.Close()

	var out []AuditEvent
	for rows.Next() {
		var event AuditEvent
		var detail []byte
		if err := rows.Scan(&event.ID, &event.ActorEmail, &event.Action, &event.SubjectType,
			&event.Subject, &detail, &event.IP, &event.UserAgent, &event.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan audit event: %w", err)
		}
		if len(detail) > 0 {
			_ = json.Unmarshal(detail, &event.Detail)
		}
		out = append(out, event)
	}
	return out, rows.Err()
}

// DistinctAuditActors lists who appears in the log, for the filter control.
func DistinctAuditActors(ctx context.Context, q Querier) ([]string, error) {
	rows, err := q.Query(ctx, `SELECT DISTINCT actor_email FROM audit_events ORDER BY actor_email`)
	if err != nil {
		return nil, fmt.Errorf("list audit actors: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var actor string
		if err := rows.Scan(&actor); err != nil {
			return nil, fmt.Errorf("scan audit actor: %w", err)
		}
		out = append(out, actor)
	}
	return out, rows.Err()
}

// PurgeAuditEvents drops entries past the retention window.
func PurgeAuditEvents(ctx context.Context, q Querier, retain time.Duration) (int64, error) {
	tag, err := q.Exec(ctx,
		`DELETE FROM audit_events WHERE created_at < now() - $1::interval`, retain.String())
	if err != nil {
		return 0, fmt.Errorf("purge audit events: %w", err)
	}
	return tag.RowsAffected(), nil
}

func nullableIPPtr(ip *string) *string {
	if ip == nil {
		return nil
	}
	return nullableIP(*ip)
}

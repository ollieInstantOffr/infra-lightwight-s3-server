package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrAlertNotFound means no such live alert exists.
var ErrAlertNotFound = errors.New("alert not found")

// Alert states.
const (
	AlertFiring       = "firing"
	AlertAcknowledged = "acknowledged"
	AlertResolved     = "resolved"
)

// Alert severities.
const (
	SeverityInfo     = "info"
	SeverityWarning  = "warning"
	SeverityCritical = "critical"
)

// AlertRule is a condition worth telling someone about.
type AlertRule struct {
	ID          string
	Name        string
	Description string
	Enabled     bool
	Severity    string
	Settings    map[string]any
	UpdatedAt   time.Time
}

// Alert is an occurrence of a rule's condition.
type Alert struct {
	ID       int64
	RuleID   string
	RuleName string
	State    string
	Severity string
	Summary  string
	// Guidance says what to do about it. An alert that reports a problem
	// without suggesting an action is one that gets muted.
	Guidance       string
	Detail         map[string]any
	FiredAt        time.Time
	LastSeenAt     time.Time
	AcknowledgedAt *time.Time
	AcknowledgedBy *string
	ResolvedAt     *time.Time
	NotifiedAt     *time.Time
}

// Live reports whether the alert still needs attention.
func (a *Alert) Live() bool { return a.State != AlertResolved }

// UpsertAlertRule stores a rule, preserving any operator changes to enablement
// and thresholds. Seeding on startup must not silently re-enable a rule someone
// deliberately turned off.
func UpsertAlertRule(ctx context.Context, q Querier, rule AlertRule) error {
	settings, err := json.Marshal(rule.Settings)
	if err != nil {
		return fmt.Errorf("encode alert settings: %w", err)
	}
	_, err = q.Exec(ctx, `
		INSERT INTO alert_rules (id, name, description, enabled, severity, settings)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (id) DO UPDATE SET
			name = EXCLUDED.name,
			description = EXCLUDED.description`,
		rule.ID, rule.Name, rule.Description, rule.Enabled, rule.Severity, settings)
	if err != nil {
		return fmt.Errorf("upsert alert rule: %w", err)
	}
	return nil
}

// ListAlertRules returns every rule.
func ListAlertRules(ctx context.Context, q Querier) ([]AlertRule, error) {
	rows, err := q.Query(ctx, `
		SELECT id, name, description, enabled, severity, settings, updated_at
		FROM alert_rules ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list alert rules: %w", err)
	}
	defer rows.Close()

	var out []AlertRule
	for rows.Next() {
		var rule AlertRule
		var settings []byte
		if err := rows.Scan(&rule.ID, &rule.Name, &rule.Description, &rule.Enabled,
			&rule.Severity, &settings, &rule.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan alert rule: %w", err)
		}
		if len(settings) > 0 {
			_ = json.Unmarshal(settings, &rule.Settings)
		}
		out = append(out, rule)
	}
	return out, rows.Err()
}

// UpdateAlertRule changes enablement and thresholds.
func UpdateAlertRule(ctx context.Context, q Querier, id string, enabled bool, severity string, settings map[string]any) error {
	encoded, err := json.Marshal(settings)
	if err != nil {
		return fmt.Errorf("encode alert settings: %w", err)
	}
	tag, err := q.Exec(ctx, `
		UPDATE alert_rules SET enabled = $2, severity = $3, settings = $4, updated_at = now()
		WHERE id = $1`, id, enabled, severity, encoded)
	if err != nil {
		return fmt.Errorf("update alert rule: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrAlertNotFound
	}
	return nil
}

// RaiseAlert records that a condition is true.
//
// One live alert per rule, enforced by a partial unique index. A condition that
// stays true updates the existing row rather than creating one per evaluation,
// which is what stops a full disk producing an alert every minute for a week.
//
// An acknowledged alert stays acknowledged while the condition persists. Being
// re-raised into firing would defeat the purpose of acknowledging it.
func RaiseAlert(ctx context.Context, q Querier, alert Alert) (raised bool, err error) {
	detail, err := json.Marshal(alert.Detail)
	if err != nil {
		return false, fmt.Errorf("encode alert detail: %w", err)
	}

	var inserted bool
	err = q.QueryRow(ctx, `
		INSERT INTO alerts (rule_id, state, severity, summary, guidance, detail)
		VALUES ($1, 'firing', $2, $3, $4, $5)
		ON CONFLICT (rule_id) WHERE state <> 'resolved'
		DO UPDATE SET
			last_seen_at = now(),
			summary = EXCLUDED.summary,
			detail = EXCLUDED.detail,
			severity = EXCLUDED.severity
		RETURNING (xmax = 0)`,
		alert.RuleID, alert.Severity, alert.Summary, alert.Guidance, detail).Scan(&inserted)
	if err != nil {
		return false, fmt.Errorf("raise alert: %w", err)
	}
	// xmax = 0 distinguishes an insert from an update, which is how the caller
	// knows whether this is new rather than ongoing.
	return inserted, nil
}

// ResolveAlert closes any live alert for a rule, because the condition cleared.
func ResolveAlert(ctx context.Context, q Querier, ruleID string) (resolved bool, err error) {
	tag, err := q.Exec(ctx, `
		UPDATE alerts SET state = 'resolved', resolved_at = now()
		WHERE rule_id = $1 AND state <> 'resolved'`, ruleID)
	if err != nil {
		return false, fmt.Errorf("resolve alert: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// AcknowledgeAlert marks an alert as seen, which stops it notifying without
// hiding it.
func AcknowledgeAlert(ctx context.Context, q Querier, id int64, who string) error {
	tag, err := q.Exec(ctx, `
		UPDATE alerts SET state = 'acknowledged', acknowledged_at = now(), acknowledged_by = $2
		WHERE id = $1 AND state = 'firing'`, id, who)
	if err != nil {
		return fmt.Errorf("acknowledge alert: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrAlertNotFound
	}
	return nil
}

// ListAlerts returns live alerts, and optionally recent resolved ones.
func ListAlerts(ctx context.Context, q Querier, includeResolved bool, limit int) ([]Alert, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := q.Query(ctx, `
		SELECT a.id, a.rule_id, coalesce(r.name, a.rule_id), a.state, a.severity,
		       a.summary, a.guidance, a.detail, a.fired_at, a.last_seen_at,
		       a.acknowledged_at, a.acknowledged_by, a.resolved_at, a.notified_at
		FROM alerts a LEFT JOIN alert_rules r ON r.id = a.rule_id
		WHERE $1 OR a.state <> 'resolved'
		ORDER BY (a.state <> 'resolved') DESC, a.fired_at DESC
		LIMIT $2`, includeResolved, limit)
	if err != nil {
		return nil, fmt.Errorf("list alerts: %w", err)
	}
	defer rows.Close()

	var out []Alert
	for rows.Next() {
		var a Alert
		var detail []byte
		if err := rows.Scan(&a.ID, &a.RuleID, &a.RuleName, &a.State, &a.Severity,
			&a.Summary, &a.Guidance, &detail, &a.FiredAt, &a.LastSeenAt,
			&a.AcknowledgedAt, &a.AcknowledgedBy, &a.ResolvedAt, &a.NotifiedAt); err != nil {
			return nil, fmt.Errorf("scan alert: %w", err)
		}
		if len(detail) > 0 {
			_ = json.Unmarshal(detail, &a.Detail)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// AlertsAwaitingNotification returns firing alerts that have not been notified
// recently, so a flapping condition sends one message rather than hundreds.
func AlertsAwaitingNotification(ctx context.Context, q Querier, quiet time.Duration) ([]Alert, error) {
	rows, err := q.Query(ctx, `
		SELECT a.id, a.rule_id, coalesce(r.name, a.rule_id), a.state, a.severity,
		       a.summary, a.guidance, a.detail, a.fired_at, a.last_seen_at,
		       a.acknowledged_at, a.acknowledged_by, a.resolved_at, a.notified_at
		FROM alerts a LEFT JOIN alert_rules r ON r.id = a.rule_id
		WHERE a.state = 'firing'
		  AND (a.notified_at IS NULL OR a.notified_at < now() - $1::interval)`,
		quiet.String())
	if err != nil {
		return nil, fmt.Errorf("find alerts to notify: %w", err)
	}
	defer rows.Close()

	var out []Alert
	for rows.Next() {
		var a Alert
		var detail []byte
		if err := rows.Scan(&a.ID, &a.RuleID, &a.RuleName, &a.State, &a.Severity,
			&a.Summary, &a.Guidance, &detail, &a.FiredAt, &a.LastSeenAt,
			&a.AcknowledgedAt, &a.AcknowledgedBy, &a.ResolvedAt, &a.NotifiedAt); err != nil {
			return nil, fmt.Errorf("scan alert: %w", err)
		}
		if len(detail) > 0 {
			_ = json.Unmarshal(detail, &a.Detail)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// MarkAlertNotified records that a message went out.
func MarkAlertNotified(ctx context.Context, q Querier, id int64) error {
	if _, err := q.Exec(ctx, `UPDATE alerts SET notified_at = now() WHERE id = $1`, id); err != nil {
		return fmt.Errorf("mark alert notified: %w", err)
	}
	return nil
}

// CountLiveAlerts returns how many alerts need attention, for the sidebar badge.
func CountLiveAlerts(ctx context.Context, q Querier) (firing, acknowledged int, err error) {
	err = q.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE state = 'firing'),
		       count(*) FILTER (WHERE state = 'acknowledged')
		FROM alerts WHERE state <> 'resolved'`).Scan(&firing, &acknowledged)
	if err != nil {
		return 0, 0, fmt.Errorf("count alerts: %w", err)
	}
	return firing, acknowledged, nil
}

// ErrorBreakdown is one grouped failure pattern.
type ErrorBreakdown struct {
	ErrorCode   string
	Reason      string
	Bucket      string
	AccessKeyID string
	ClientIP    string
	Count       int64
	LastSeen    time.Time
}

// GroupErrors aggregates recent failures, because a list of a thousand 403s is
// not a diagnosis and a count of them by cause is.
func GroupErrors(ctx context.Context, q Querier, window time.Duration, limit int) ([]ErrorBreakdown, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := q.Query(ctx, `
		SELECT error_code,
		       -- The reasons within a group differ only in their specifics, so
		       -- one representative is more useful than a list of near-duplicates.
		       (array_agg(reason ORDER BY occurred_at DESC))[1],
		       coalesce(nullif(bucket, ''), ''),
		       coalesce(nullif(access_key_id, ''), ''),
		       coalesce(host(max(client_ip)), ''),
		       count(*), max(occurred_at)
		FROM request_logs
		WHERE status >= 400 AND occurred_at > now() - $1::interval
		GROUP BY error_code, bucket, access_key_id
		ORDER BY count(*) DESC
		LIMIT $2`, window.String(), limit)
	if err != nil {
		return nil, fmt.Errorf("group errors: %w", err)
	}
	defer rows.Close()

	var out []ErrorBreakdown
	for rows.Next() {
		var e ErrorBreakdown
		if err := rows.Scan(&e.ErrorCode, &e.Reason, &e.Bucket, &e.AccessKeyID,
			&e.ClientIP, &e.Count, &e.LastSeen); err != nil {
			return nil, fmt.Errorf("scan error group: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// CountAuthFailures reports repeated rejections from one source, which is
// either a misconfigured client or someone probing.
func CountAuthFailures(ctx context.Context, q Querier, window time.Duration) (source string, count int64, err error) {
	err = q.QueryRow(ctx, `
		SELECT coalesce(nullif(access_key_id, ''), host(client_ip), 'unknown'), count(*)
		FROM request_logs
		WHERE status = 403 AND occurred_at > now() - $1::interval
		GROUP BY 1
		ORDER BY count(*) DESC
		LIMIT 1`, window.String()).Scan(&source, &count)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", 0, nil
	}
	if err != nil {
		return "", 0, fmt.Errorf("count auth failures: %w", err)
	}
	return source, count, nil
}

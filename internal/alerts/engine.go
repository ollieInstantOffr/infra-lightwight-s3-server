// Package alerts evaluates conditions worth telling an operator about, and
// keeps their state.
//
// The design constraint that matters: an alert that spams gets muted, and a
// muted alert is worse than none. So a condition that stays true updates one
// row rather than raising a new alert per evaluation, acknowledging stops
// notification without hiding the alert, and a resolved condition closes itself
// rather than waiting to be dismissed.
package alerts

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/db"
	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/storage"
)

const (
	// EvaluationInterval is how often conditions are checked. Frequent enough
	// to be useful, rare enough that the queries are free.
	EvaluationInterval = time.Minute

	// NotifyQuiet is how long before a still-firing alert notifies again.
	NotifyQuiet = 6 * time.Hour
)

// Rule identifiers, stable because operators change their thresholds and those
// settings are keyed by id.
const (
	RuleErrorRate     = "error_rate"
	RuleDiskSpace     = "disk_space"
	RuleAuthFailures  = "auth_failures"
	RuleWriteFailures = "write_failures"
	RuleNoCredentials = "no_credentials"
)

// Notifier delivers an alert. The console's mailer satisfies it.
type Notifier interface {
	NotifyAlert(ctx context.Context, alert db.Alert) error
}

// Engine evaluates rules on a schedule.
type Engine struct {
	Pool     *db.Pool
	Blobs    *storage.Store
	Log      *slog.Logger
	Notifier Notifier
}

// DefaultRules are seeded on startup. Enablement and thresholds are preserved
// across restarts, so a rule an operator turned off stays off.
func DefaultRules() []db.AlertRule {
	return []db.AlertRule{
		{
			ID: RuleErrorRate, Name: "Error rate", Severity: db.SeverityWarning, Enabled: true,
			Description: "Too many requests are failing.",
			Settings:    map[string]any{"threshold_percent": 5.0, "window_minutes": 15.0, "minimum_requests": 20.0},
		},
		{
			ID: RuleDiskSpace, Name: "Disk space", Severity: db.SeverityCritical, Enabled: true,
			Description: "The volume holding objects is filling up.",
			Settings:    map[string]any{"warn_below_percent": 15.0, "critical_below_percent": 5.0},
		},
		{
			ID: RuleAuthFailures, Name: "Repeated authentication failures", Severity: db.SeverityWarning, Enabled: true,
			Description: "One credential or address is being rejected repeatedly, which is either a misconfigured client or probing.",
			Settings:    map[string]any{"threshold": 50.0, "window_minutes": 15.0},
		},
		{
			ID: RuleWriteFailures, Name: "Upload failures", Severity: db.SeverityCritical, Enabled: true,
			Description: "Uploads are failing, which usually means disk or a proxy limit.",
			Settings:    map[string]any{"threshold": 5.0, "window_minutes": 15.0},
		},
		{
			ID: RuleNoCredentials, Name: "No S3 credentials", Severity: db.SeverityInfo, Enabled: true,
			Description: "No active access keys exist, so the S3 API cannot be used.",
			Settings:    map[string]any{},
		},
	}
}

// SeedRules ensures the default rules exist.
func SeedRules(ctx context.Context, pool *db.Pool) error {
	for _, rule := range DefaultRules() {
		if err := db.UpsertAlertRule(ctx, pool, rule); err != nil {
			return err
		}
	}
	return nil
}

// Run evaluates on a ticker until ctx is cancelled.
func (e *Engine) Run(ctx context.Context) {
	ticker := time.NewTicker(EvaluationInterval)
	defer ticker.Stop()

	// Evaluated once at startup so a server that comes up into a bad state
	// says so immediately rather than a minute later.
	e.EvaluateOnce(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.EvaluateOnce(ctx)
		}
	}
}

// EvaluateOnce runs every enabled rule and then delivers notifications.
func (e *Engine) EvaluateOnce(ctx context.Context) {
	rules, err := db.ListAlertRules(ctx, e.Pool)
	if err != nil {
		e.Log.Warn("could not read alert rules", "error", err)
		return
	}

	for _, rule := range rules {
		if !rule.Enabled {
			// A disabled rule resolves anything it left behind, so turning a
			// noisy rule off actually clears it rather than leaving a stuck
			// alert nobody can dismiss.
			if _, err := db.ResolveAlert(ctx, e.Pool, rule.ID); err != nil {
				e.Log.Warn("could not resolve alert for disabled rule", "rule", rule.ID, "error", err)
			}
			continue
		}
		e.evaluate(ctx, rule)
	}

	e.notify(ctx)
}

func (e *Engine) evaluate(ctx context.Context, rule db.AlertRule) {
	var alert *db.Alert
	var err error

	switch rule.ID {
	case RuleErrorRate:
		alert, err = e.checkErrorRate(ctx, rule)
	case RuleDiskSpace:
		alert, err = e.checkDiskSpace(ctx, rule)
	case RuleAuthFailures:
		alert, err = e.checkAuthFailures(ctx, rule)
	case RuleWriteFailures:
		alert, err = e.checkWriteFailures(ctx, rule)
	case RuleNoCredentials:
		alert, err = e.checkNoCredentials(ctx, rule)
	default:
		return
	}

	if err != nil {
		e.Log.Warn("could not evaluate alert rule", "rule", rule.ID, "error", err)
		return
	}

	if alert == nil {
		if resolved, err := db.ResolveAlert(ctx, e.Pool, rule.ID); err != nil {
			e.Log.Warn("could not resolve alert", "rule", rule.ID, "error", err)
		} else if resolved {
			e.Log.Info("alert resolved", "rule", rule.ID)
		}
		return
	}

	alert.RuleID = rule.ID
	raised, err := db.RaiseAlert(ctx, e.Pool, *alert)
	if err != nil {
		e.Log.Warn("could not raise alert", "rule", rule.ID, "error", err)
		return
	}
	if raised {
		e.Log.Warn("alert raised", "rule", rule.ID, "summary", alert.Summary)
	}
}

func (e *Engine) checkErrorRate(ctx context.Context, rule db.AlertRule) (*db.Alert, error) {
	threshold := setting(rule, "threshold_percent", 5)
	window := time.Duration(setting(rule, "window_minutes", 15)) * time.Minute
	minimum := int64(setting(rule, "minimum_requests", 20))

	var total, failed int64
	err := e.Pool.QueryRow(ctx, `
		SELECT count(*), count(*) FILTER (WHERE status >= 400)
		FROM request_logs WHERE occurred_at > now() - $1::interval`, window.String()).
		Scan(&total, &failed)
	if err != nil {
		return nil, err
	}

	// Below the minimum the percentage is noise: two failures out of three
	// requests on an idle server is 66% and means nothing.
	if total < minimum || total == 0 {
		return nil, nil
	}

	rate := float64(failed) / float64(total) * 100
	if rate < threshold {
		return nil, nil
	}

	return &db.Alert{
		Severity: rule.Severity,
		Summary:  fmt.Sprintf("%.1f%% of requests failed in the last %d minutes (%d of %d)", rate, int(window.Minutes()), failed, total),
		Guidance: "Open the log, filtered to errors, to see which requests and why. Grouped failures usually name the cause: a revoked key, a client clock outside the 15 minute window, or a proxy not forwarding the host.",
		Detail:   map[string]any{"rate": rate, "failed": failed, "total": total},
	}, nil
}

func (e *Engine) checkDiskSpace(ctx context.Context, rule db.AlertRule) (*db.Alert, error) {
	usage, err := e.Blobs.Usage()
	if err != nil {
		return nil, err
	}
	if usage.TotalBytes == 0 {
		return nil, nil
	}

	freePercent := float64(usage.FreeBytes) / float64(usage.TotalBytes) * 100
	warnBelow := setting(rule, "warn_below_percent", 15)
	criticalBelow := setting(rule, "critical_below_percent", 5)

	if freePercent >= warnBelow {
		return nil, nil
	}

	severity := db.SeverityWarning
	guidance := "Free space or add capacity. Deleting objects reclaims space on the next sweep; if versioning is on, old versions must be purged before their bytes are released."
	if freePercent < criticalBelow {
		severity = db.SeverityCritical
		guidance = "Uploads will begin failing when the volume is full. " + guidance
	}

	return &db.Alert{
		Severity: severity,
		Summary:  fmt.Sprintf("Only %.1f%% of the volume is free", freePercent),
		Guidance: guidance,
		Detail:   map[string]any{"free_percent": freePercent, "free_bytes": usage.FreeBytes, "total_bytes": usage.TotalBytes},
	}, nil
}

func (e *Engine) checkAuthFailures(ctx context.Context, rule db.AlertRule) (*db.Alert, error) {
	threshold := int64(setting(rule, "threshold", 50))
	window := time.Duration(setting(rule, "window_minutes", 15)) * time.Minute

	source, count, err := db.CountAuthFailures(ctx, e.Pool, window)
	if err != nil || count < threshold {
		return nil, err
	}

	return &db.Alert{
		Severity: rule.Severity,
		Summary:  fmt.Sprintf("%d rejected requests from %s in %d minutes", count, source, int(window.Minutes())),
		Guidance: "Filter the log by that credential or address. A steady rate is usually a misconfigured client — a revoked key, a wrong secret, or a clock outside the permitted window. A burst from an unknown address is worth treating as probing.",
		Detail:   map[string]any{"source": source, "count": count},
	}, nil
}

func (e *Engine) checkWriteFailures(ctx context.Context, rule db.AlertRule) (*db.Alert, error) {
	threshold := int64(setting(rule, "threshold", 5))
	window := time.Duration(setting(rule, "window_minutes", 15)) * time.Minute

	var count int64
	err := e.Pool.QueryRow(ctx, `
		SELECT count(*) FROM request_logs
		WHERE status >= 500 AND method IN ('PUT', 'POST') AND occurred_at > now() - $1::interval`,
		window.String()).Scan(&count)
	if err != nil || count < threshold {
		return nil, err
	}

	return &db.Alert{
		Severity: rule.Severity,
		Summary:  fmt.Sprintf("%d uploads failed with a server error in %d minutes", count, int(window.Minutes())),
		Guidance: "Check disk space first, then the log for the underlying reason. A proxy body-size limit produces a 413 rather than a 5xx, so this is more likely storage or the database.",
		Detail:   map[string]any{"count": count},
	}, nil
}

func (e *Engine) checkNoCredentials(ctx context.Context, rule db.AlertRule) (*db.Alert, error) {
	var active int
	if err := e.Pool.QueryRow(ctx,
		`SELECT count(*) FROM credentials WHERE revoked_at IS NULL`).Scan(&active); err != nil {
		return nil, err
	}
	if active > 0 {
		return nil, nil
	}
	return &db.Alert{
		Severity: rule.Severity,
		Summary:  "No active S3 access keys exist",
		Guidance: "The S3 API cannot be used until a key is created. Make one on the Access keys screen.",
		Detail:   map[string]any{},
	}, nil
}

// notify delivers firing alerts that have not been notified recently.
func (e *Engine) notify(ctx context.Context) {
	if e.Notifier == nil {
		return
	}

	pending, err := db.AlertsAwaitingNotification(ctx, e.Pool, NotifyQuiet)
	if err != nil {
		e.Log.Warn("could not find alerts to notify", "error", err)
		return
	}

	for _, alert := range pending {
		// Info is visible in the console but not worth an email. Waking someone
		// for "no access keys yet" would teach them to ignore the sender.
		if alert.Severity == db.SeverityInfo {
			continue
		}
		if err := e.Notifier.NotifyAlert(ctx, alert); err != nil {
			e.Log.Warn("could not send alert notification", "rule", alert.RuleID, "error", err)
			continue
		}
		if err := db.MarkAlertNotified(ctx, e.Pool, alert.ID); err != nil {
			e.Log.Warn("could not record alert notification", "id", alert.ID, "error", err)
		}
	}
}

// setting reads a numeric threshold, falling back when absent or the wrong
// shape — a malformed setting should not disable a rule silently.
func setting(rule db.AlertRule, key string, fallback float64) float64 {
	if rule.Settings == nil {
		return fallback
	}
	if value, ok := rule.Settings[key].(float64); ok {
		return value
	}
	return fallback
}

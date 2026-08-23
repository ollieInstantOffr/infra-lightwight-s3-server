package console

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/db"
)

// handleListAlerts returns live alerts, and recent resolved ones on request.
func (s *Server) handleListAlerts(w http.ResponseWriter, r *http.Request) {
	includeResolved := r.URL.Query().Get("resolved") == "1"

	alerts, err := db.ListAlerts(r.Context(), s.DB, includeResolved,
		intParam(r.URL.Query().Get("limit"), 50))
	if err != nil {
		s.internalError(w, r, "list alerts", err)
		return
	}
	firing, acknowledged, err := db.CountLiveAlerts(r.Context(), s.DB)
	if err != nil {
		s.internalError(w, r, "count alerts", err)
		return
	}

	out := make([]map[string]any, 0, len(alerts))
	for _, a := range alerts {
		out = append(out, map[string]any{
			"id": a.ID, "rule": a.RuleID, "ruleName": a.RuleName,
			"state": a.State, "severity": a.Severity,
			"summary": a.Summary, "guidance": a.Guidance, "detail": a.Detail,
			"firedAt": a.FiredAt, "lastSeenAt": a.LastSeenAt,
			"acknowledgedAt": a.AcknowledgedAt, "acknowledgedBy": a.AcknowledgedBy,
			"resolvedAt": a.ResolvedAt,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"alerts": out, "firing": firing, "acknowledged": acknowledged,
	})
}

// handleAcknowledgeAlert marks an alert as seen. It stops notification without
// hiding the alert, which is the distinction that keeps people using it rather
// than muting the whole system.
func (s *Server) handleAcknowledgeAlert(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "That is not an alert id.")
		return
	}
	user, _ := UserFrom(r.Context())

	switch err := db.AcknowledgeAlert(r.Context(), s.DB, id, user.Email); {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]string{
			"message": "Acknowledged. It stays visible until the condition clears, but will not notify again.",
		})
	case errors.Is(err, db.ErrAlertNotFound):
		writeError(w, http.StatusNotFound, "That alert is no longer firing.")
	default:
		s.internalError(w, r, "acknowledge alert", err)
	}
}

// handleResolveAlert closes an alert by hand.
//
// Alerts resolve themselves when their condition clears, so this is for the
// case where an operator knows better than the rule — and the next evaluation
// will simply raise it again if the condition is in fact still true.
func (s *Server) handleResolveAlert(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "That is not an alert id.")
		return
	}

	var ruleID string
	if err := s.DB.QueryRow(r.Context(),
		`SELECT rule_id FROM alerts WHERE id = $1`, id).Scan(&ruleID); err != nil {
		writeError(w, http.StatusNotFound, "No such alert.")
		return
	}
	if _, err := db.ResolveAlert(r.Context(), s.DB, ruleID); err != nil {
		s.internalError(w, r, "resolve alert", err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"message": "Resolved. It will return on the next evaluation if the condition still holds.",
	})
}

// handleListAlertRules returns the rules and their thresholds.
func (s *Server) handleListAlertRules(w http.ResponseWriter, r *http.Request) {
	rules, err := db.ListAlertRules(r.Context(), s.DB)
	if err != nil {
		s.internalError(w, r, "list alert rules", err)
		return
	}
	out := make([]map[string]any, 0, len(rules))
	for _, rule := range rules {
		out = append(out, map[string]any{
			"id": rule.ID, "name": rule.Name, "description": rule.Description,
			"enabled": rule.Enabled, "severity": rule.Severity,
			"settings": rule.Settings, "updatedAt": rule.UpdatedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"rules": out})
}

type alertRuleRequest struct {
	Enabled  bool               `json:"enabled"`
	Severity string             `json:"severity"`
	Settings map[string]float64 `json:"settings"`
}

func (s *Server) handleUpdateAlertRule(w http.ResponseWriter, r *http.Request) {
	var request alertRuleRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "Send a JSON body with the rule's settings.")
		return
	}
	switch request.Severity {
	case db.SeverityInfo, db.SeverityWarning, db.SeverityCritical:
	default:
		writeError(w, http.StatusBadRequest, "Severity must be info, warning or critical.")
		return
	}

	settings := make(map[string]any, len(request.Settings))
	for key, value := range request.Settings {
		settings[key] = value
	}

	switch err := db.UpdateAlertRule(r.Context(), s.DB, r.PathValue("id"),
		request.Enabled, request.Severity, settings); {
	case err == nil:
		user, _ := UserFrom(r.Context())
		s.Log.Info("alert rule changed", "rule", r.PathValue("id"), "by", user.Email)
		writeJSON(w, http.StatusOK, map[string]string{"message": "Rule updated."})
	case errors.Is(err, db.ErrAlertNotFound):
		writeError(w, http.StatusNotFound, "No such alert rule.")
	default:
		s.internalError(w, r, "update alert rule", err)
	}
}

// NotifyAlert emails administrators about a firing alert.
//
// Delivered through the existing mailer, so a deployment with no email provider
// simply logs it — the console remains the source of truth either way.
func (s *Server) NotifyAlert(ctx context.Context, alert db.Alert) error {
	users, err := db.ListUsers(ctx, s.DB)
	if err != nil {
		return fmt.Errorf("list recipients: %w", err)
	}

	subject := fmt.Sprintf("[%s] %s", alert.Severity, alert.RuleName)
	text := fmt.Sprintf(`%s

%s

%s

Open the console to acknowledge or investigate:
%s/alerts
`, alert.RuleName, alert.Summary, alert.Guidance, s.PublicURL)

	htmlBody := fmt.Sprintf(`<!doctype html>
<html><body style="font-family: system-ui, -apple-system, sans-serif; line-height: 1.6; color: #12211d;">
  <h2 style="margin:0 0 6px; font-weight:600;">%s</h2>
  <p style="margin:0 0 14px; font-size:15px;">%s</p>
  <p style="margin:0 0 18px; color:#5f736d; font-size:14px;">%s</p>
  <p><a href="%s/alerts" style="display:inline-block; padding:10px 18px; background:#12211d; color:#fff; text-decoration:none; border-radius:8px;">Open the console</a></p>
</body></html>`, alert.RuleName, alert.Summary, alert.Guidance, s.PublicURL)

	// Administrators only. A member cannot act on most of these, and an alert
	// sent to someone who cannot fix it is training them to ignore alerts.
	sent := 0
	for _, user := range users {
		if !user.IsAdmin() {
			continue
		}
		if err := s.Mailer.Send(ctx, user.Email, subject, text, htmlBody); err != nil {
			return fmt.Errorf("send to %s: %w", user.Email, err)
		}
		sent++
	}
	if sent == 0 {
		return errors.New("no administrators to notify")
	}
	return nil
}

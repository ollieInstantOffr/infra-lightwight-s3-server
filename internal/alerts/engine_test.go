package alerts

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/db"
	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/storage"
)

func testEngine(t *testing.T) *Engine {
	t.Helper()
	pool := testPool(t)
	blobs, err := storage.New(t.TempDir())
	if err != nil {
		t.Fatalf("storage.New: %v", err)
	}
	ctx := context.Background()
	if err := SeedRules(ctx, pool); err != nil {
		t.Fatalf("SeedRules: %v", err)
	}
	return &Engine{Pool: pool, Blobs: blobs, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func liveAlert(t *testing.T, e *Engine, ruleID string) *db.Alert {
	t.Helper()
	alerts, err := db.ListAlerts(context.Background(), e.Pool, false, 50)
	if err != nil {
		t.Fatalf("ListAlerts: %v", err)
	}
	for i := range alerts {
		if alerts[i].RuleID == ruleID {
			return &alerts[i]
		}
	}
	return nil
}

// A condition that stays true must update one alert, not raise a new one every
// minute. An alert that spams is one that gets muted.
func TestFiringConditionRaisesExactlyOneAlert(t *testing.T) {
	e := testEngine(t)
	ctx := context.Background()

	// No credentials exist, so that rule fires.
	for range 5 {
		e.EvaluateOnce(ctx)
	}

	alerts, err := db.ListAlerts(ctx, e.Pool, true, 100)
	if err != nil {
		t.Fatalf("ListAlerts: %v", err)
	}
	count := 0
	for _, a := range alerts {
		if a.RuleID == RuleNoCredentials {
			count++
		}
	}
	if count != 1 {
		t.Errorf("five evaluations produced %d alerts for one condition, want 1", count)
	}
}

// The condition clearing must close the alert without anyone dismissing it.
func TestAlertResolvesItselfWhenTheConditionClears(t *testing.T) {
	e := testEngine(t)
	ctx := context.Background()

	e.EvaluateOnce(ctx)
	if liveAlert(t, e, RuleNoCredentials) == nil {
		t.Fatal("expected the no-credentials alert to fire")
	}

	if _, err := e.Pool.Exec(ctx, `
		INSERT INTO credentials (access_key_id, secret_ciphertext, secret_nonce)
		VALUES ('AKIATEST', '\x00', '\x00')`); err != nil {
		t.Fatalf("insert credential: %v", err)
	}

	e.EvaluateOnce(ctx)
	if alert := liveAlert(t, e, RuleNoCredentials); alert != nil {
		t.Errorf("alert still live after the condition cleared: %+v", alert)
	}
}

// Acknowledging must survive further evaluations. Being re-raised into firing
// would defeat the point of acknowledging it.
func TestAcknowledgedAlertStaysAcknowledged(t *testing.T) {
	e := testEngine(t)
	ctx := context.Background()

	e.EvaluateOnce(ctx)
	alert := liveAlert(t, e, RuleNoCredentials)
	if alert == nil {
		t.Fatal("expected an alert to acknowledge")
	}
	if err := db.AcknowledgeAlert(ctx, e.Pool, alert.ID, "ollie@example.com"); err != nil {
		t.Fatalf("AcknowledgeAlert: %v", err)
	}

	e.EvaluateOnce(ctx)

	after := liveAlert(t, e, RuleNoCredentials)
	if after == nil {
		t.Fatal("alert disappeared after being acknowledged")
	}
	if after.State != db.AlertAcknowledged {
		t.Errorf("state = %q after re-evaluation, want acknowledged", after.State)
	}
	if after.ID != alert.ID {
		t.Errorf("a second alert was raised (%d then %d) rather than the first being updated", alert.ID, after.ID)
	}
}

// Disabling a noisy rule must clear what it left behind, rather than stranding
// an alert nobody can dismiss.
func TestDisablingARuleResolvesItsAlert(t *testing.T) {
	e := testEngine(t)
	ctx := context.Background()

	e.EvaluateOnce(ctx)
	if liveAlert(t, e, RuleNoCredentials) == nil {
		t.Fatal("expected an alert")
	}

	if err := db.UpdateAlertRule(ctx, e.Pool, RuleNoCredentials, false, db.SeverityInfo, nil); err != nil {
		t.Fatalf("UpdateAlertRule: %v", err)
	}
	e.EvaluateOnce(ctx)

	if alert := liveAlert(t, e, RuleNoCredentials); alert != nil {
		t.Errorf("disabling the rule left its alert live: %+v", alert)
	}
}

// An idle server must not alarm on a tiny sample. Two failures out of three
// requests is 66% and means nothing.
func TestErrorRateIgnoresTinySamples(t *testing.T) {
	e := testEngine(t)
	ctx := context.Background()

	if err := db.InsertRequestLogs(ctx, e.Pool, []db.RequestLog{
		{At: time.Now(), Status: 500}, {At: time.Now(), Status: 500},
	}); err != nil {
		t.Fatalf("InsertRequestLogs: %v", err)
	}

	e.EvaluateOnce(ctx)
	if alert := liveAlert(t, e, RuleErrorRate); alert != nil {
		t.Errorf("two failed requests raised an error-rate alert: %s", alert.Summary)
	}
}

// Above the minimum it must fire, and say something useful.
func TestErrorRateFiresAboveTheThreshold(t *testing.T) {
	e := testEngine(t)
	ctx := context.Background()

	entries := make([]db.RequestLog, 0, 100)
	for i := range 100 {
		status := 200
		if i < 40 {
			status = 403
		}
		entries = append(entries, db.RequestLog{At: time.Now(), Status: status})
	}
	if err := db.InsertRequestLogs(ctx, e.Pool, entries); err != nil {
		t.Fatalf("InsertRequestLogs: %v", err)
	}

	e.EvaluateOnce(ctx)

	alert := liveAlert(t, e, RuleErrorRate)
	if alert == nil {
		t.Fatal("40% failures did not raise an alert")
	}
	if alert.Guidance == "" {
		t.Error("the alert says what is wrong but not what to do about it")
	}
}

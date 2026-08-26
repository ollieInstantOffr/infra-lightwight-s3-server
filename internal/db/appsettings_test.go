package db

import (
	"context"
	"strings"
	"testing"

	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/secrets"
)

func TestAlertEmailSettingsRoundTrip(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	cipher := testCipher(t)

	if err := SaveAlertEmailSettings(ctx, pool, cipher, true,
		"alerts@example.com", "re_test_key_value", ""); err != nil {
		t.Fatalf("SaveAlertEmailSettings: %v", err)
	}

	got, err := GetAlertEmailSettings(ctx, pool, cipher)
	if err != nil {
		t.Fatalf("GetAlertEmailSettings: %v", err)
	}
	if !got.Enabled || got.From != "alerts@example.com" || got.APIKey != "re_test_key_value" {
		t.Errorf("round-trip gave %+v", got)
	}
	if !got.Configured() {
		t.Error("settings with a key, a from-address and enabled report as not configured")
	}
}

// The key is a provider credential. A database dump that carries it in plain
// text is a dump away from being someone else's mail account.
func TestTheAPIKeyIsNotStoredInPlainText(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	cipher := testCipher(t)

	const key = "re_a_very_recognisable_secret"
	if err := SaveAlertEmailSettings(ctx, pool, cipher, true, "alerts@example.com", key, ""); err != nil {
		t.Fatalf("SaveAlertEmailSettings: %v", err)
	}

	var stored []byte
	if err := pool.QueryRow(ctx,
		`SELECT resend_api_key FROM app_settings WHERE id = true`).Scan(&stored); err != nil {
		t.Fatalf("read the raw column: %v", err)
	}
	if strings.Contains(string(stored), key) {
		t.Error("the API key is readable in the database column")
	}
	if len(stored) == 0 {
		t.Error("nothing was stored at all")
	}
}

// Saving without a key must keep the stored one. It is what lets the settings
// screen change the from-address without ever having the key in the browser.
func TestSavingWithoutAKeyKeepsTheStoredOne(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	cipher := testCipher(t)

	if err := SaveAlertEmailSettings(ctx, pool, cipher, true,
		"first@example.com", "re_original_key", ""); err != nil {
		t.Fatalf("initial save: %v", err)
	}
	if err := SaveAlertEmailSettings(ctx, pool, cipher, false,
		"second@example.com", "", ""); err != nil {
		t.Fatalf("second save: %v", err)
	}

	got, err := GetAlertEmailSettings(ctx, pool, cipher)
	if err != nil {
		t.Fatalf("GetAlertEmailSettings: %v", err)
	}
	if got.APIKey != "re_original_key" {
		t.Errorf("the stored key was lost: %q", got.APIKey)
	}
	if got.From != "second@example.com" || got.Enabled {
		t.Errorf("the other fields did not save: %+v", got)
	}
}

// A key encrypted under one CREDENTIALS_KEY must not silently decrypt under
// another. Reporting an error beats behaving as if nothing were configured,
// which would look like alerts quietly having been turned off.
func TestAKeyFromADifferentCipherIsReportedRatherThanIgnored(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	if err := SaveAlertEmailSettings(ctx, pool, testCipher(t), true,
		"alerts@example.com", "re_key", ""); err != nil {
		t.Fatalf("SaveAlertEmailSettings: %v", err)
	}

	other, err := secrets.NewCipher("a-completely-different-key-32-chars-ok!!")
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	if _, err := GetAlertEmailSettings(ctx, pool, other); err == nil {
		t.Error("a key encrypted under a different CREDENTIALS_KEY decrypted, or was silently ignored")
	}
}

func TestClearAlertEmailKey(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	cipher := testCipher(t)

	if err := SaveAlertEmailSettings(ctx, pool, cipher, true,
		"alerts@example.com", "re_key", ""); err != nil {
		t.Fatalf("SaveAlertEmailSettings: %v", err)
	}
	if err := ClearAlertEmailKey(ctx, pool); err != nil {
		t.Fatalf("ClearAlertEmailKey: %v", err)
	}

	got, err := GetAlertEmailSettings(ctx, pool, cipher)
	if err != nil {
		t.Fatalf("GetAlertEmailSettings: %v", err)
	}
	if got.APIKey != "" {
		t.Error("the key survived being cleared")
	}
	if got.Enabled {
		t.Error("clearing the key left the channel enabled, so alerts would fail on every send")
	}
}

// Nothing configured is the state a fresh deployment is in, and must read as
// disabled rather than as an error.
func TestUnconfiguredSettingsAreNotAnError(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	got, err := GetAlertEmailSettings(ctx, pool, testCipher(t))
	if err != nil {
		t.Fatalf("GetAlertEmailSettings on a fresh deployment: %v", err)
	}
	if got.Configured() {
		t.Error("an unconfigured deployment reports as configured")
	}
}

func TestConfiguredNeedsEveryPart(t *testing.T) {
	cases := map[string]struct {
		settings AlertEmailSettings
		want     bool
	}{
		"everything":     {AlertEmailSettings{Enabled: true, From: "a@b.com", APIKey: "k"}, true},
		"disabled":       {AlertEmailSettings{Enabled: false, From: "a@b.com", APIKey: "k"}, false},
		"no key":         {AlertEmailSettings{Enabled: true, From: "a@b.com"}, false},
		"no from":        {AlertEmailSettings{Enabled: true, APIKey: "k"}, false},
		"nothing at all": {AlertEmailSettings{}, false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := c.settings.Configured(); got != c.want {
				t.Errorf("Configured() = %v, want %v", got, c.want)
			}
		})
	}
}

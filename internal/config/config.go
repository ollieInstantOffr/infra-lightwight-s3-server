// Package config loads and validates the server's runtime configuration from
// the environment. Everything the process needs is resolved once at startup so
// that a misconfigured deployment fails immediately and loudly rather than at
// the first request that happens to need the missing value.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Default ports. Both serve plain HTTP: TLS terminates at the reverse proxy,
// so despite 8443 and 8444 reading as TLS ports by convention, nothing here
// listens for a handshake. Point the proxy at them over http://.
const (
	defaultS3Port      = 8443
	defaultConsolePort = 8444
)

// Environment names the deployment mode. Development relaxes a few checks that
// would otherwise make local work tedious, such as requiring a real Resend key.
type Environment string

const (
	Development Environment = "development"
	Production  Environment = "production"
)

// Config is the fully resolved configuration for both listeners.
type Config struct {
	Env Environment

	// Listeners. The S3 API and the console are deliberately separate ports so
	// that bucket paths can never collide with console routes, and so each can
	// be mapped to its own hostname in the reverse proxy.
	S3Port      int
	ConsolePort int

	// Storage.
	DataDir     string
	DatabaseURL string

	// Public identity. These are the URLs clients actually reach us on, which
	// is not the same as what we bind to: TLS terminates at the reverse proxy.
	PublicS3URL      string
	PublicConsoleURL string

	// S3Domain, when set, enables virtual-host style addressing: a request to
	// bucket.s3.example.com is treated as a request for that bucket. Empty
	// means path-style only.
	S3Domain string
	S3Region string

	// Console authentication.
	AdminEmail    string
	SessionSecret string
	// CredentialsKey encrypts S3 secret keys at rest. Unlike a password, an S3
	// secret cannot be hashed: SigV4 verification re-derives the signing key
	// from the secret itself, so the server must be able to recover it.
	CredentialsKey string
	ResendAPIKey   string
	ResendFrom     string

	// TrustedProxies lists the CIDRs allowed to set X-Forwarded-* headers.
	// Without this an outside caller could spoof the public hostname and defeat
	// signature verification.
	TrustedProxies []string

	LogLevel slog.Level
	// LogSampleRate is the fraction of successful requests kept in the
	// queryable log. Failures and slow requests are always kept regardless.
	LogSampleRate float64
	// LogSlowRequestMS retains any request at least this slow, whatever its
	// status — a slow success is often the more interesting event.
	LogSlowRequestMS int
}

// ResendConfigured reports whether outbound email can actually be sent. When it
// is false the magic-link flow logs the login URL instead, which keeps local
// development usable before an API key exists.
func (c *Config) ResendConfigured() bool { return c.ResendAPIKey != "" }

// Load reads configuration from the environment and validates it. All problems
// are collected and reported together, so a fresh deployment does not have to
// be fixed one restart at a time.
func Load() (*Config, error) {
	var problems []string
	fail := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	env := Environment(strings.ToLower(envStr("ENV", string(Production))))
	if env != Development && env != Production {
		fail("ENV must be %q or %q, got %q", Development, Production, env)
	}

	cfg := &Config{
		Env:              env,
		S3Port:           envInt("S3_PORT", defaultS3Port, fail),
		ConsolePort:      envInt("CONSOLE_PORT", defaultConsolePort, fail),
		DataDir:          envStr("DATA_DIR", "/data"),
		DatabaseURL:      envStr("DATABASE_URL", ""),
		PublicS3URL:      strings.TrimRight(envStr("PUBLIC_S3_URL", ""), "/"),
		PublicConsoleURL: strings.TrimRight(envStr("PUBLIC_CONSOLE_URL", ""), "/"),
		S3Domain:         strings.ToLower(strings.TrimSpace(envStr("S3_DOMAIN", ""))),
		S3Region:         envStr("S3_REGION", "us-east-1"),
		AdminEmail:       strings.ToLower(strings.TrimSpace(envStr("ADMIN_EMAIL", ""))),
		SessionSecret:    envStr("SESSION_SECRET", ""),
		CredentialsKey:   envStr("CREDENTIALS_KEY", ""),
		ResendAPIKey:     envStr("RESEND_API_KEY", ""),
		ResendFrom:       envStr("RESEND_FROM", ""),
		TrustedProxies:   envList("TRUSTED_PROXIES", "127.0.0.1/32,::1/128,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16"),
		LogSampleRate:    envFloat("LOG_SAMPLE_RATE", 0.01, fail),
		LogSlowRequestMS: envInt("LOG_SLOW_REQUEST_MS", 3000, fail),
	}

	level, err := parseLogLevel(envStr("LOG_LEVEL", "info"))
	if err != nil {
		fail("LOG_LEVEL: %v", err)
	}
	cfg.LogLevel = level

	if cfg.LogSampleRate < 0 || cfg.LogSampleRate > 1 {
		fail("LOG_SAMPLE_RATE must be between 0 and 1, got %v", cfg.LogSampleRate)
	}

	validatePort("S3_PORT", cfg.S3Port, fail)
	validatePort("CONSOLE_PORT", cfg.ConsolePort, fail)
	if cfg.S3Port == cfg.ConsolePort {
		fail("S3_PORT and CONSOLE_PORT must differ, both are %d", cfg.S3Port)
	}

	if cfg.DatabaseURL == "" {
		fail("DATABASE_URL is required (postgres connection string)")
	}

	if cfg.DataDir == "" {
		fail("DATA_DIR is required")
	} else {
		abs, err := filepath.Abs(cfg.DataDir)
		if err != nil {
			fail("DATA_DIR %q is not a usable path: %v", cfg.DataDir, err)
		} else {
			cfg.DataDir = abs
		}
	}

	if cfg.AdminEmail == "" {
		fail("ADMIN_EMAIL is required; it bootstraps the first console admin")
	} else if !looksLikeEmail(cfg.AdminEmail) {
		fail("ADMIN_EMAIL %q is not a valid email address", cfg.AdminEmail)
	}

	validatePublicURL("PUBLIC_S3_URL", cfg.PublicS3URL, fail)
	validatePublicURL("PUBLIC_CONSOLE_URL", cfg.PublicConsoleURL, fail)

	// Secrets and outbound email are only strictly required in production. In
	// development we substitute safe stand-ins so a bare `go run` works.
	switch env {
	case Production:
		if cfg.PublicConsoleURL == "" {
			fail("PUBLIC_CONSOLE_URL is required in production; magic-link emails need an absolute URL")
		}
		if len(cfg.SessionSecret) < 32 {
			fail("SESSION_SECRET must be at least 32 characters in production (got %d)", len(cfg.SessionSecret))
		}
		if len(cfg.CredentialsKey) < 32 {
			fail("CREDENTIALS_KEY must be at least 32 characters in production (got %d); "+
				"changing it makes every existing S3 credential undecryptable", len(cfg.CredentialsKey))
		}
		if cfg.ResendConfigured() && cfg.ResendFrom == "" {
			fail("RESEND_FROM is required when RESEND_API_KEY is set")
		}
	case Development:
		if cfg.PublicS3URL == "" {
			cfg.PublicS3URL = fmt.Sprintf("http://localhost:%d", cfg.S3Port)
		}
		if cfg.PublicConsoleURL == "" {
			cfg.PublicConsoleURL = fmt.Sprintf("http://localhost:%d", cfg.ConsolePort)
		}
		if cfg.SessionSecret == "" {
			secret, err := randomSecret()
			if err != nil {
				fail("could not generate a development SESSION_SECRET: %v", err)
			}
			cfg.SessionSecret = secret
		}
		if cfg.CredentialsKey == "" {
			// Ephemeral, so credentials created in one dev run cannot be
			// decrypted by the next. That is the intended local behaviour.
			key, err := randomSecret()
			if err != nil {
				fail("could not generate a development CREDENTIALS_KEY: %v", err)
			}
			cfg.CredentialsKey = key
		}
		if cfg.ResendFrom == "" {
			cfg.ResendFrom = "s3d <onboarding@resend.dev>"
		}
	}

	if len(problems) > 0 {
		return nil, fmt.Errorf("invalid configuration:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return cfg, nil
}

// LogValue implements slog.LogValuer so the config can be logged at startup
// without leaking secrets into the log stream.
func (c *Config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("env", string(c.Env)),
		slog.Int("s3_port", c.S3Port),
		slog.Int("console_port", c.ConsolePort),
		slog.String("data_dir", c.DataDir),
		slog.String("database_url", redactDSN(c.DatabaseURL)),
		slog.String("public_s3_url", c.PublicS3URL),
		slog.String("public_console_url", c.PublicConsoleURL),
		slog.String("s3_domain", orNone(c.S3Domain)),
		slog.String("s3_region", c.S3Region),
		slog.String("admin_email", c.AdminEmail),
		slog.Bool("resend_configured", c.ResendConfigured()),
		slog.Int("trusted_proxies", len(c.TrustedProxies)),
		slog.String("log_level", c.LogLevel.String()),
		slog.Float64("log_sample_rate", c.LogSampleRate),
	)
}

func envStr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envInt(key string, def int, fail func(string, ...any)) int {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		fail("%s must be an integer, got %q", key, raw)
		return def
	}
	return n
}

func envFloat(key string, def float64, fail func(string, ...any)) float64 {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return def
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		fail("%s must be a number, got %q", key, raw)
		return def
	}
	return value
}

func envList(key, def string) []string {
	raw := envStr(key, def)
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func validatePort(key string, port int, fail func(string, ...any)) {
	if port < 1 || port > 65535 {
		fail("%s must be between 1 and 65535, got %d", key, port)
	}
}

func validatePublicURL(key, raw string, fail func(string, ...any)) {
	if raw == "" {
		return
	}
	u, err := url.Parse(raw)
	if err != nil {
		fail("%s is not a valid URL: %v", key, err)
		return
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		fail("%s must be an absolute http or https URL, got %q", key, raw)
	}
	if u.Host == "" {
		fail("%s must include a host, got %q", key, raw)
	}
}

func parseLogLevel(raw string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, errors.New("must be one of debug, info, warn, error")
	}
}

// looksLikeEmail is a deliberately loose check. Real validation is delivery:
// the magic-link flow proves the address works by sending to it.
func looksLikeEmail(s string) bool {
	at := strings.IndexByte(s, '@')
	if at <= 0 || at == len(s)-1 {
		return false
	}
	domain := s[at+1:]
	return !strings.ContainsAny(s, " \t") && strings.Contains(domain, ".") &&
		!strings.HasPrefix(domain, ".") && !strings.HasSuffix(domain, ".")
}

// redactDSN keeps the shape of a connection string visible while removing the
// password, so a startup log stays useful for debugging without being a leak.
func redactDSN(dsn string) string {
	if dsn == "" {
		return ""
	}
	u, err := url.Parse(dsn)
	if err != nil || u.User == nil {
		return dsn
	}
	if _, hasPassword := u.User.Password(); hasPassword {
		u.User = url.UserPassword(u.User.Username(), "xxxxx")
	}
	return u.String()
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// Command s3d is a lightweight, single-node S3-compatible object server.
//
// It runs two HTTP listeners: the S3 API, which speaks the AWS S3 wire protocol
// and authenticates with SigV4, and the admin console, which serves the React
// SPA and its session-authenticated API. They are separate ports so that bucket
// paths can never collide with console routes.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/alerts"
	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/config"
	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/console"
	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/db"
	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/httpx"
	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/logs"
	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/metrics"
	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/s3api"
	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/secrets"
	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/storage"
)

// version is stamped at build time with -ldflags "-X main.version=...".
var version = "dev"

// shutdownGrace bounds how long in-flight requests get to finish. Object
// transfers can be large, so this is generous by HTTP standards.
const shutdownGrace = 30 * time.Second

// startupTimeout bounds database connection and migration. A container stuck
// here should fail and be restarted rather than hang indefinitely.
const startupTimeout = 60 * time.Second

func main() {
	// Subcommands run and exit without starting the listeners. Ordered so the
	// ones that must work on a misconfigured host come first.
	args := os.Args[1:]
	if helpCommand(args) || versionCommand(args) || runHealthcheck(args) {
		return
	}

	if handled, err := runCredentialCommand(args); handled {
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			os.Exit(1)
		}
		return
	}

	if err := run(); err != nil {
		// The logger may not exist yet if configuration itself failed, so this
		// path deliberately writes plainly to stderr.
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	// The sink is created before the logger, because the logger tees warnings
	// and errors into it. Nothing is written until the database is reachable
	// and the flusher starts; entries buffer in the meantime.
	logSink := logs.New(nodeName(), logs.Policy{
		SampleRate:    cfg.LogSampleRate,
		SlowThreshold: time.Duration(cfg.LogSlowRequestMS) * time.Millisecond,
	})

	log := newLogger(cfg, logSink)
	slog.SetDefault(log)

	log.Info("starting s3d",
		"version", version,
		"go", runtime.Version(),
		"config", cfg,
	)

	// New() creates the layout, proves it is writable, and clears any partial
	// uploads a previous crash left behind.
	blobs, err := storage.New(cfg.DataDir)
	if err != nil {
		return err
	}
	usage, err := blobs.Usage()
	if err != nil {
		return err
	}
	log.Info("blob store ready",
		"root", blobs.Root(),
		"free_bytes", usage.FreeBytes,
		"total_bytes", usage.TotalBytes,
	)

	// Connecting and migrating happen before the listeners bind, so a container
	// that cannot reach its database never accepts a request it would only fail.
	startupCtx, cancelStartup := context.WithTimeout(context.Background(), startupTimeout)
	defer cancelStartup()

	pool, err := db.Connect(startupCtx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	log.Info("connected to postgres")

	if err := db.Migrate(startupCtx, pool, log); err != nil {
		return fmt.Errorf("migrate database: %w", err)
	}

	cipher, err := secrets.NewCipher(cfg.CredentialsKey)
	if err != nil {
		return fmt.Errorf("credentials key: %w", err)
	}
	proxies, err := httpx.NewProxyTrust(cfg.TrustedProxies)
	if err != nil {
		return err
	}

	collector := metrics.New()
	// Three resolutions of the same request, each for a different reader: the
	// collector rolls counts into hourly cells for the console's chart, the
	// registry holds the monotonic counters a Prometheus scrape reads, and
	// liveWindow keeps the last minute at one-second resolution for the
	// Performance page's Live mode.
	registry := metrics.NewRegistry(version)
	liveWindow := metrics.NewLiveWindow()
	inFlight := s3api.NewInFlight()

	s3Server := &s3api.Server{
		DB:        pool,
		Blobs:     blobs,
		Log:       log,
		Region:    cfg.S3Region,
		PublicURL: cfg.PublicS3URL,
		S3Domain:  cfg.S3Domain,
		Metrics:   collector,
		Live:      liveWindow,
		InFlight:  inFlight,
		Scrape:    registry,
		Logs:      logSink,
		Verifier: &s3api.Verifier{
			Region:  cfg.S3Region,
			Proxies: proxies,
			// Looked up per request so a revoked credential stops working
			// immediately rather than at the end of some cache lifetime.
			Lookup: func(ctx context.Context, accessKeyID string) (s3api.KeyMaterial, error) {
				cred, err := db.LookupCredential(ctx, pool, cipher, accessKeyID)
				if err != nil {
					return s3api.KeyMaterial{}, err
				}
				// Best-effort, and throttled to once a minute in the query, so
				// a failure here must not fail an otherwise valid request.
				if err := db.TouchCredential(ctx, pool, accessKeyID); err != nil {
					log.Warn("could not record credential use", "access_key_id", accessKeyID, "error", err)
				}
				// The scope comes back with the secret, so what a request is
				// authorized against is always the same version of the key that
				// its signature was checked against — and a key narrowed or
				// revoked a moment ago takes effect on the very next request,
				// including one using a presigned URL signed before the change.
				return s3api.KeyMaterial{SecretKey: cred.SecretKey, Grant: cred.Scope}, nil
			},
		},
	}

	// The bootstrap admin is created or promoted on every start, which is the
	// documented way back in if the last admin is removed by accident.
	admin, err := db.EnsureAdmin(startupCtx, pool, cfg.AdminEmail)
	if err != nil {
		return err
	}
	log.Info("bootstrap admin ready", "email", admin.Email)

	var mailer console.Mailer
	if cfg.ResendConfigured() {
		mailer = console.NewResendMailer(cfg.ResendAPIKey, cfg.ResendFrom)
	} else {
		// Without a key the sign-in link is logged instead of sent, which keeps
		// local development usable. In production that is a misconfiguration.
		mailer = &console.LogMailer{Log: log}
		if cfg.Env == config.Production {
			log.Warn("RESEND_API_KEY is not set: sign-in links will be written to this log instead of emailed")
		}
	}

	consoleServer := &console.Server{
		DB:            pool,
		Blobs:         blobs,
		Cipher:        cipher,
		Mailer:        mailer,
		Proxies:       proxies,
		Log:           log,
		AdminEmail:    cfg.AdminEmail,
		PublicURL:     cfg.PublicConsoleURL,
		PublicS3URL:   cfg.PublicS3URL,
		Region:        cfg.S3Region,
		SessionSecret: cfg.SessionSecret,
		Assets:        consoleAssets(log),
		Logs:          logSink,
		Sink:          logSink,
		Registry:      registry,
		MetricsToken:  cfg.MetricsToken,
		Live:          liveWindow,
		InFlight:      inFlight,
		System: console.SystemInfo{
			Version:           version,
			NodeName:          nodeName(),
			StartedAt:         time.Now(),
			DataDir:           cfg.DataDir,
			S3Domain:          cfg.S3Domain,
			TrustedProxyCount: len(cfg.TrustedProxies),
			ResendConfigured:  cfg.ResendConfigured(),
			Environment:       string(cfg.Env),
		},
	}

	// Alert rules are seeded on every start, preserving any thresholds or
	// enablement an operator has changed.
	if err := alerts.SeedRules(startupCtx, pool); err != nil {
		return fmt.Errorf("seed alert rules: %w", err)
	}

	warnIfNoCredentials(startupCtx, pool, log)

	// Signal handling is installed before the listeners so a Ctrl-C during
	// startup is still honoured rather than killing the process outright.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	servers := []*namedServer{
		{
			name: "s3",
			server: &http.Server{
				Addr:    fmt.Sprintf(":%d", cfg.S3Port),
				Handler: s3Server.Handler(),
				// No WriteTimeout: object downloads are unbounded in duration
				// and a write deadline would truncate large transfers.
				ReadHeaderTimeout: 15 * time.Second,
				IdleTimeout:       120 * time.Second,
				ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
			},
		},
		{
			name: "console",
			server: &http.Server{
				Addr:              fmt.Sprintf(":%d", cfg.ConsolePort),
				Handler:           consoleServer.Handler(),
				ReadHeaderTimeout: 15 * time.Second,
				IdleTimeout:       120 * time.Second,
				ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
			},
		},
	}

	// Background reclamation runs for the life of the process and stops with
	// the shutdown signal.
	go runMaintenance(ctx, pool, blobs, log)
	// Request counts are accumulated in memory and flushed on a ticker, so the
	// request path never waits on the database to record a graph.
	go collector.Run(ctx, pool, log)
	// Request logs and captured server events flush on their own ticker, so
	// neither the request path nor a log call waits on the database.
	go logSink.Run(ctx, pool, log)

	// Alerts evaluate against the metrics and logs already being collected, so
	// this adds queries on a one-minute ticker rather than any new bookkeeping.
	alertEngine := &alerts.Engine{
		Pool: pool, Blobs: blobs, Log: log, Notifier: consoleServer,
	}
	go alertEngine.Run(ctx)

	// serveErr carries the first listener failure. It is buffered so a failing
	// goroutine never blocks on a shutdown that is already under way.
	serveErr := make(chan error, len(servers))
	var wg sync.WaitGroup

	for _, s := range servers {
		wg.Add(1)
		go func(s *namedServer) {
			defer wg.Done()
			log.Info("listening", "listener", s.name, "addr", s.server.Addr)
			if err := s.server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				serveErr <- fmt.Errorf("%s listener: %w", s.name, err)
			}
		}(s)
	}

	// Wait for either an operator signal or a listener that could not start.
	var runErr error
	select {
	case <-ctx.Done():
		log.Info("shutdown signal received, draining connections", "grace", shutdownGrace)
	case runErr = <-serveErr:
		log.Error("listener failed, shutting down", "error", runErr)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()

	for _, s := range servers {
		if err := s.server.Shutdown(shutdownCtx); err != nil {
			log.Warn("listener did not drain cleanly", "listener", s.name, "error", err)
			// Force the remaining connections closed so the process can exit
			// rather than hanging past the grace period.
			_ = s.server.Close()
		}
	}

	wg.Wait()
	log.Info("stopped")
	return runErr
}

// nodeName identifies this server on the system screen. The hostname is what
// an operator recognises, and inside a container it is the container id, which
// is exactly what they would paste into `docker logs`.
func nodeName() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		return "s3d"
	}
	return host
}

type namedServer struct {
	name   string
	server *http.Server
}

func newLogger(cfg *config.Config, sink *logs.Sink) *slog.Logger {
	opts := &slog.HandlerOptions{Level: cfg.LogLevel}

	// Human-readable text locally, JSON in production where logs are collected.
	var handler slog.Handler
	if cfg.Env == config.Development {
		handler = slog.NewTextHandler(os.Stdout, opts)
	} else {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	}

	// Warnings and errors are also persisted, so the console can explain the
	// things that are not requests: a failed email send, a blob that could not
	// be reclaimed. Stdout keeps everything either way.
	return slog.New(logs.NewHandler(handler, sink))
}

// warnIfNoCredentials points out a server nobody can actually use. A fresh
// deployment has no S3 credentials until one is created in the console, and the
// resulting InvalidAccessKeyId is otherwise a confusing first experience.
func warnIfNoCredentials(ctx context.Context, pool *db.Pool, log *slog.Logger) {
	var count int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM credentials WHERE revoked_at IS NULL`).Scan(&count); err != nil {
		log.Warn("could not count credentials", "error", err)
		return
	}
	if count == 0 {
		log.Warn("no S3 credentials exist yet; create one in the console before using the S3 API")
	}
}

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

	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/config"
	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/db"
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

	log := newLogger(cfg)
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

	// Signal handling is installed before the listeners so a Ctrl-C during
	// startup is still honoured rather than killing the process outright.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	servers := []*namedServer{
		{
			name: "s3",
			server: &http.Server{
				Addr:    fmt.Sprintf(":%d", cfg.S3Port),
				Handler: placeholderHandler("s3", version),
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
				Handler:           placeholderHandler("console", version),
				ReadHeaderTimeout: 15 * time.Second,
				IdleTimeout:       120 * time.Second,
				ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
			},
		},
	}

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

type namedServer struct {
	name   string
	server *http.Server
}

func newLogger(cfg *config.Config) *slog.Logger {
	opts := &slog.HandlerOptions{Level: cfg.LogLevel}
	// Human-readable text locally, JSON in production where logs are collected.
	if cfg.Env == config.Development {
		return slog.New(slog.NewTextHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, opts))
}

// placeholderHandler stands in until the S3 API and console routers land in
// their own issues. It answers health probes so the container is orchestratable
// from day one, and reports 501 for everything else.
func placeholderHandler(listener, version string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusNotImplemented)
		fmt.Fprintf(w, "s3d %s: the %s listener is not implemented yet\n", version, listener)
	})
	return mux
}

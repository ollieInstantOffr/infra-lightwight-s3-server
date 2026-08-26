package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/config"
)

// The runtime image is distroless: no shell, no curl, no wget. A container
// healthcheck therefore has to be the binary itself, which is also more honest
// — it checks the same readiness endpoint an orchestrator would, using the port
// the process actually bound.

const healthcheckTimeout = 5 * time.Second

// runHealthcheck handles "s3d healthcheck", exiting non-zero if the server is
// not ready to serve.
func runHealthcheck(args []string) (handled bool) {
	if len(args) == 0 || args[0] != "healthcheck" {
		return false
	}

	// The port is read from the environment rather than the full config, so a
	// healthcheck still works when something else in the configuration is
	// wrong — which is exactly when it is being consulted.
	port := os.Getenv("CONSOLE_PORT")
	if port == "" {
		port = "8444"
	}

	client := &http.Client{Timeout: healthcheckTimeout}
	response, err := client.Get(fmt.Sprintf("http://127.0.0.1:%s/readyz", port))
	if err != nil {
		fmt.Fprintf(os.Stderr, "not ready: %v\n", err)
		os.Exit(1)
	}
	defer response.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(response.Body, 4<<10))
	if response.StatusCode != http.StatusOK {
		// The body names which dependency failed, which is what makes a
		// failing healthcheck actionable from `docker inspect` alone.
		fmt.Fprintf(os.Stderr, "not ready (%d): %s\n", response.StatusCode, body)
		os.Exit(1)
	}

	fmt.Println("ready")
	return true
}

// versionCommand handles "s3d version".
func versionCommand(args []string) (handled bool) {
	if len(args) == 0 || (args[0] != "version" && args[0] != "-v" && args[0] != "--version") {
		return false
	}
	fmt.Printf("s3d %s\n", version)
	return true
}

// helpCommand prints usage.
func helpCommand(args []string) (handled bool) {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "help", "-h", "--help":
	default:
		return false
	}

	fmt.Print(`s3d — a lightweight, single-node S3-compatible object server.

Usage:
  s3d                              run the server
  s3d healthcheck                  exit 0 if the server is ready to serve
  s3d version                      print the version
  s3d credential create [note]     create an S3 access key pair
  s3d credential list              list credentials
  s3d credential revoke <key-id>   revoke a credential
  s3d user list                    list console users
  s3d user set-password <email>    set a user's password
  s3d user promote <email>         make a user an administrator
  s3d selftest                     measure throughput over loopback, no proxy

Configuration is read from the environment. See .env.example for every
variable, or the README for a walk-through.
`)
	// Config is loaded last so `s3d help` works on a misconfigured host.
	if _, err := config.Load(); err != nil {
		fmt.Fprintf(os.Stderr, "\nNote: the current configuration is not valid:\n%v\n", err)
	}
	return true
}

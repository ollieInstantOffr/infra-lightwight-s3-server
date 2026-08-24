package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/db"
	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/httpx"
	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/s3api"
	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/secrets"
	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/storage"
)

func TestParseSelftestFlagsDefaults(t *testing.T) {
	sizeMB, concurrency, err := parseSelftestFlags(nil)
	if err != nil {
		t.Fatalf("parseSelftestFlags(nil): %v", err)
	}
	if sizeMB != defaultSelftestSizeMB || concurrency != defaultSelftestConcurrency {
		t.Errorf("defaults = (%d, %d), want (%d, %d)",
			sizeMB, concurrency, defaultSelftestSizeMB, defaultSelftestConcurrency)
	}
}

func TestParseSelftestFlags(t *testing.T) {
	cases := map[string]struct {
		args            []string
		wantSize        int
		wantConcurrency int
		wantErr         bool
	}{
		"both set":          {args: []string{"--size", "10", "--concurrency", "4"}, wantSize: 10, wantConcurrency: 4},
		"size only":         {args: []string{"--size", "50"}, wantSize: 50, wantConcurrency: defaultSelftestConcurrency},
		"concurrency only":  {args: []string{"--concurrency", "2"}, wantSize: defaultSelftestSizeMB, wantConcurrency: 2},
		"unknown option":    {args: []string{"--turbo"}, wantErr: true},
		"missing value":     {args: []string{"--size"}, wantErr: true},
		"not a number":      {args: []string{"--size", "big"}, wantErr: true},
		"zero rejected":     {args: []string{"--size", "0"}, wantErr: true},
		"negative rejected": {args: []string{"--concurrency", "-4"}, wantErr: true},
		// A mistyped size must not be allowed to fill the data volume, and a
		// mistyped concurrency must not fork thousands of transfers.
		"size capped":        {args: []string{"--size", "999999"}, wantErr: true},
		"concurrency capped": {args: []string{"--concurrency", "100000"}, wantErr: true},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			sizeMB, concurrency, err := parseSelftestFlags(c.args)
			if c.wantErr {
				if err == nil {
					t.Fatalf("parseSelftestFlags(%v) succeeded, want an error", c.args)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseSelftestFlags(%v): %v", c.args, err)
			}
			if sizeMB != c.wantSize || concurrency != c.wantConcurrency {
				t.Errorf("got (%d, %d), want (%d, %d)", sizeMB, concurrency, c.wantSize, c.wantConcurrency)
			}
		})
	}
}

// The reader has to deliver exactly the promised number of bytes: it is what
// Content-Length is set from, so a reader that is even one byte short turns
// every measurement into a hung or rejected request.
func TestPatternReaderYieldsExactlyTheRequestedSize(t *testing.T) {
	for _, size := range []int64{0, 1, 4096, 256 << 10, (256 << 10) + 1, 3 << 20} {
		t.Run(fmt.Sprintf("%d bytes", size), func(t *testing.T) {
			n, err := io.Copy(io.Discard, newPatternReader(size))
			if err != nil {
				t.Fatalf("Copy: %v", err)
			}
			if n != size {
				t.Errorf("read %d bytes, want %d", n, size)
			}
		})
	}
}

// Reading through a buffer smaller than the internal block exercises the
// offset bookkeeping, where an off-by-one would silently repeat or skip bytes.
func TestPatternReaderHandlesSmallBuffers(t *testing.T) {
	const size = 700 << 10
	reader := newPatternReader(size)

	var total int64
	buf := make([]byte, 999) // deliberately not a divisor of the block size
	for {
		n, err := reader.Read(buf)
		total += int64(n)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if n == 0 {
			t.Fatal("Read returned 0 bytes without EOF, which would spin forever")
		}
	}
	if total != size {
		t.Errorf("read %d bytes, want %d", total, size)
	}
}

// Zeroes would let a compressing or hole-punching filesystem make the write
// vanish, reporting a throughput the server cannot actually sustain on real
// data.
func TestPatternReaderIsNotAllZeroes(t *testing.T) {
	data, err := io.ReadAll(newPatternReader(64 << 10))
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if bytes.Equal(data, make([]byte, len(data))) {
		t.Error("pattern is entirely zeroes; a compressing filesystem would flatter the result")
	}
}

func TestThroughput(t *testing.T) {
	if got := throughput(100<<20, 2*time.Second); got != 50 {
		t.Errorf("throughput(100MB, 2s) = %v, want 50", got)
	}
	// A measurement so fast the clock did not move must not divide by zero.
	if got := throughput(1<<20, 0); got != 0 {
		t.Errorf("throughput with zero elapsed = %v, want 0", got)
	}
}

// The whole point of the command is that it talks to a real server with real
// signatures. This drives it against one, which is what proves the hand-rolled
// signing in selftestClient is actually accepted rather than silently failing
// into a misleading error.
func TestSelftestRunsAgainstARealServer(t *testing.T) {
	endpoint, cleanup := newSelftestServer(t)
	defer cleanup()

	client := &selftestClient{
		endpoint:    endpoint.url,
		accessKeyID: endpoint.accessKeyID,
		secretKey:   endpoint.secretKey,
		region:      "us-east-1",
		http:        &http.Client{Timeout: 60 * time.Second},
	}

	// Small and low-concurrency: this is proving it works, not measuring a
	// number, and the test suite should not write hundreds of megabytes.
	if err := runSelftestAgainst(context.Background(), client, 4, 3); err != nil {
		t.Fatalf("runSelftestAgainst: %v", err)
	}
}

// Every bucket and object the run created must be gone afterwards, including
// the ones from the concurrent phase. A diagnostic that leaves test data in a
// production deployment is worse than no diagnostic.
func TestSelftestCleansUpAfterItself(t *testing.T) {
	endpoint, cleanup := newSelftestServer(t)
	defer cleanup()

	client := &selftestClient{
		endpoint:    endpoint.url,
		accessKeyID: endpoint.accessKeyID,
		secretKey:   endpoint.secretKey,
		region:      "us-east-1",
		http:        &http.Client{Timeout: 60 * time.Second},
	}
	if err := runSelftestAgainst(context.Background(), client, 4, 3); err != nil {
		t.Fatalf("runSelftestAgainst: %v", err)
	}

	var buckets int
	if err := endpoint.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM buckets`).Scan(&buckets); err != nil {
		t.Fatalf("count buckets: %v", err)
	}
	if buckets != 0 {
		t.Errorf("%d buckets left behind, want 0", buckets)
	}

	var objects int
	if err := endpoint.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM objects`).Scan(&objects); err != nil {
		t.Fatalf("count objects: %v", err)
	}
	if objects != 0 {
		t.Errorf("%d objects left behind, want 0", objects)
	}
}

// A wrong secret must surface as a clear failure rather than a zero
// measurement that reads as "the server is infinitely fast".
func TestSelftestFailsLoudlyOnABadSignature(t *testing.T) {
	endpoint, cleanup := newSelftestServer(t)
	defer cleanup()

	client := &selftestClient{
		endpoint:    endpoint.url,
		accessKeyID: endpoint.accessKeyID,
		secretKey:   "this-is-not-the-right-secret-key-at-all",
		region:      "us-east-1",
		http:        &http.Client{Timeout: 30 * time.Second},
	}
	if err := runSelftestAgainst(context.Background(), client, 1, 2); err == nil {
		t.Fatal("runSelftestAgainst succeeded with a wrong secret, want an error")
	}
}

type selftestEndpoint struct {
	url         string
	accessKeyID string
	secretKey   string
	pool        *db.Pool
}

// newSelftestServer stands up the real S3 server against real Postgres and a
// real blob store, which is the only setup that can prove the client's
// signatures are accepted.
func newSelftestServer(t *testing.T) (*selftestEndpoint, func()) {
	t.Helper()

	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping database tests")
	}

	ctx := context.Background()
	const schema = "test_cmd_selftest"

	admin, err := db.Connect(ctx, base)
	if err != nil {
		t.Fatalf("connect to create test schema: %v", err)
	}
	if _, err := admin.Exec(ctx, fmt.Sprintf(`CREATE SCHEMA IF NOT EXISTS %q`, schema)); err != nil {
		admin.Close()
		t.Fatalf("create schema: %v", err)
	}
	admin.Close()

	parsed, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse TEST_DATABASE_URL: %v", err)
	}
	q := parsed.Query()
	q.Set("search_path", schema)
	parsed.RawQuery = q.Encode()

	pool, err := db.Connect(ctx, parsed.String())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := db.Migrate(ctx, pool, quiet); err != nil {
		pool.Close()
		t.Fatalf("migrate: %v", err)
	}
	for _, stmt := range []string{`DELETE FROM buckets`, `DELETE FROM blobs`, `DELETE FROM credentials`} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			pool.Close()
			t.Fatalf("reset (%s): %v", stmt, err)
		}
	}

	cipher, err := secrets.NewCipher("selftest-command-credentials-key-32chars")
	if err != nil {
		pool.Close()
		t.Fatalf("NewCipher: %v", err)
	}
	cred, err := db.CreateCredential(ctx, pool, cipher, "selftest command test", nil, db.UnrestrictedGrant())
	if err != nil {
		pool.Close()
		t.Fatalf("CreateCredential: %v", err)
	}

	blobs, err := storage.New(t.TempDir())
	if err != nil {
		pool.Close()
		t.Fatalf("storage.New: %v", err)
	}
	trust, _ := httpx.NewProxyTrust(nil)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		pool.Close()
		t.Fatalf("listen: %v", err)
	}
	endpointURL := fmt.Sprintf("http://127.0.0.1:%d", listener.Addr().(*net.TCPAddr).Port)

	server := &s3api.Server{
		DB: pool, Blobs: blobs, Log: quiet, Region: "us-east-1",
		PublicURL: endpointURL,
		Verifier: &s3api.Verifier{
			Region: "us-east-1", Proxies: trust,
			Lookup: func(ctx context.Context, accessKeyID string) (s3api.KeyMaterial, error) {
				c, err := db.LookupCredential(ctx, pool, cipher, accessKeyID)
				if err != nil {
					return s3api.KeyMaterial{}, err
				}
				return s3api.KeyMaterial{SecretKey: c.SecretKey, Grant: c.Scope}, nil
			},
		},
	}

	httpSrv := &http.Server{Handler: server.Handler()}
	go func() { _ = httpSrv.Serve(listener) }()

	cleanup := func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
		pool.Close()
	}

	return &selftestEndpoint{
		url: endpointURL, accessKeyID: cred.AccessKeyID, secretKey: cred.SecretKey, pool: pool,
	}, cleanup
}

package s3api

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/db"
	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/httpx"
	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/secrets"
	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/storage"
)

// newIntegrationServer stands up the real S3 server — real Postgres, real blob
// store, real signature verification — and returns an AWS SDK client pointed at
// it. Everything below this line exercises the same code paths a deployed
// container would.
func newIntegrationServer(t *testing.T) (*s3.Client, *db.Pool) {
	t.Helper()

	dsn := testDSN(t, "test_s3api_pkg")
	ctx := context.Background()

	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := db.Migrate(ctx, pool, quiet); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// Objects cascade from buckets; blobs and credentials are cleared
	// separately so each test starts from a known state.
	for _, stmt := range []string{`DELETE FROM buckets`, `DELETE FROM blobs`, `DELETE FROM credentials`} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("reset (%s): %v", stmt, err)
		}
	}

	cipher, err := secrets.NewCipher("integration-test-credentials-key-32-chars")
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	cred, err := db.CreateCredential(ctx, pool, cipher, "integration test", nil)
	if err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}

	blobs, err := storage.New(t.TempDir())
	if err != nil {
		t.Fatalf("storage.New: %v", err)
	}
	trust, err := httpx.NewProxyTrust(nil)
	if err != nil {
		t.Fatalf("NewProxyTrust: %v", err)
	}

	server := &Server{
		DB:    pool,
		Blobs: blobs,
		Log:   quiet,
		Verifier: &Verifier{
			Region:  "us-east-1",
			Proxies: trust,
			Lookup: func(ctx context.Context, accessKeyID string) (string, error) {
				c, err := db.LookupCredential(ctx, pool, cipher, accessKeyID)
				if err != nil {
					return "", err
				}
				return c.SecretKey, nil
			},
		},
		Region: "us-east-1",
	}

	httpSrv := httptest.NewServer(server.Handler())
	t.Cleanup(httpSrv.Close)

	client := s3.New(s3.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(httpSrv.URL),
		UsePathStyle: true,
		Credentials: credentials.NewStaticCredentialsProvider(
			cred.AccessKeyID, cred.SecretKey, ""),
	})
	return client, pool
}

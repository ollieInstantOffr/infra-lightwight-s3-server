package s3api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
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

// virtualHostServer serves a bucket addressed through its hostname.
//
// Testing this needs a client that both *signs* and *sends* a Host the DNS
// system would never resolve, so the transport dials the test listener directly
// while everything above it believes it is talking to bucket.s3.example.com.
// Signing the real hostname is the point: it proves bucket resolution and
// signature verification agree on which host the request was for.
type virtualHostServer struct {
	client *http.Client
	creds  aws.Credentials
	region string
}

func newVirtualHostServer(t *testing.T, s3Domain string) *virtualHostServer {
	t.Helper()

	dsn := testDSN(t, "test_s3api_vhost")
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
	// The schema outlives the run, so it is reset here as well. Without this
	// the seed below fails the second time the suite runs against the same
	// database, which is exactly what happens locally.
	for _, stmt := range []string{`DELETE FROM buckets`, `DELETE FROM blobs`, `DELETE FROM credentials`} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("reset (%s): %v", stmt, err)
		}
	}

	cipher, err := secrets.NewCipher("integration-test-credentials-key-32-chars")
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	cred, err := db.CreateCredential(ctx, pool, cipher, "vhost test", nil)
	if err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}

	blobs, err := storage.New(t.TempDir())
	if err != nil {
		t.Fatalf("storage.New: %v", err)
	}
	trust, _ := httpx.NewProxyTrust(nil)

	server := &Server{
		DB: pool, Blobs: blobs, Log: quiet, Region: "us-east-1", S3Domain: s3Domain,
		Verifier: &Verifier{
			Region: "us-east-1", Proxies: trust,
			Lookup: func(ctx context.Context, accessKeyID string) (string, error) {
				c, err := db.LookupCredential(ctx, pool, cipher, accessKeyID)
				if err != nil {
					return "", err
				}
				return c.SecretKey, nil
			},
		},
	}

	httpSrv := httptest.NewServer(server.Handler())
	t.Cleanup(httpSrv.Close)
	listenAddr := strings.TrimPrefix(httpSrv.URL, "http://")

	// Seed the same bucket and object the path-style server has, since this is
	// a separate schema.
	pathClient := s3.New(s3.Options{
		Region: "us-east-1", BaseEndpoint: aws.String(httpSrv.URL), UsePathStyle: true,
		Credentials: credentials.NewStaticCredentialsProvider(cred.AccessKeyID, cred.SecretKey, ""),
	})
	if _, err := pathClient.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("vhost")}); err != nil {
		t.Fatalf("seed bucket: %v", err)
	}
	if _, err := pathClient.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("vhost"), Key: aws.String("nested/file.txt"),
		Body: strings.NewReader("reached through a virtual host"),
	}); err != nil {
		t.Fatalf("seed object: %v", err)
	}

	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, listenAddr)
		},
	}
	return &virtualHostServer{
		client: &http.Client{Transport: transport},
		creds:  aws.Credentials{AccessKeyID: cred.AccessKeyID, SecretAccessKey: cred.SecretKey},
		region: "us-east-1",
	}
}

// get signs and sends a GET to a virtual-host style URL.
func (v *virtualHostServer) get(t *testing.T, host, path string) (string, int) {
	t.Helper()

	request, err := http.NewRequest(http.MethodGet, "http://"+host+path, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	// Signed with the AWS SDK's own signer, so the canonical request is the
	// SDK's rather than a reimplementation of it.
	hash := sha256.Sum256(nil)
	request.Header.Set("X-Amz-Content-Sha256", hex.EncodeToString(hash[:]))
	if err := v4.NewSigner().SignHTTP(t.Context(), v.creds, request,
		hex.EncodeToString(hash[:]), "s3", v.region, time.Now()); err != nil {
		t.Fatalf("sign request: %v", err)
	}

	resp, err := v.client.Do(request)
	if err != nil {
		t.Fatalf("send request: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return string(body), resp.StatusCode
}

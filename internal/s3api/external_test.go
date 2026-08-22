package s3api

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/httpx"
)

// These tests drive the real aws-cli and boto3 against the verifier, in
// containers, because neither is installed on a typical development machine.
// They are the acceptance criteria for signature support: an implementation can
// satisfy every unit test and still be rejected by the tools people actually
// use.
//
// Set S3D_EXTERNAL_TESTS=1 to run them. They need a working Docker daemon and
// pull images on first use, so they are opt-in rather than part of the default
// `go test ./...`.

func skipUnlessExternal(t *testing.T) {
	t.Helper()
	if os.Getenv("S3D_EXTERNAL_TESTS") == "" {
		t.Skip("set S3D_EXTERNAL_TESTS=1 to run container-based client tests")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
}

// externalServer listens on all interfaces so a container can reach it, and
// records every verification outcome.
type externalServer struct {
	port int

	mu       sync.Mutex
	attempts []error
}

func (e *externalServer) record(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.attempts = append(e.attempts, err)
}

func (e *externalServer) results() []error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]error(nil), e.attempts...)
}

func startExternalServer(t *testing.T) *externalServer {
	t.Helper()

	trust, err := httpx.NewProxyTrust(nil)
	if err != nil {
		t.Fatalf("NewProxyTrust: %v", err)
	}
	verifier := &Verifier{
		Region:  "us-east-1",
		Proxies: trust,
		Lookup: func(_ context.Context, accessKeyID string) (string, error) {
			if accessKeyID != sdkAccessKeyID {
				return "", errNoSuchKey
			}
			return sdkSecretKey, nil
		},
	}

	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &externalServer{port: listener.Addr().(*net.TCPAddr).Port}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, err := verifier.Verify(r.Context(), r)
		if err != nil {
			srv.record(err)
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>SignatureDoesNotMatch</Code><Message>`+err.Error()+`</Message></Error>`)
			return
		}
		// Draining the body proves the framing decoded, not just that the
		// headers verified.
		if _, err := io.Copy(io.Discard, verifier.Body(r, id)); err != nil {
			srv.record(fmt.Errorf("body: %w", err))
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		srv.record(nil)
		writeMinimalResponse(w, r)
	})

	httpSrv := &http.Server{Handler: handler}
	go func() { _ = httpSrv.Serve(listener) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(ctx)
	})
	return srv
}

// assertAllVerified fails with the first rejection, which is far more useful
// than the client's own opaque "signature does not match".
func (e *externalServer) assertAllVerified(t *testing.T, minAttempts int) {
	t.Helper()
	results := e.results()
	if len(results) < minAttempts {
		t.Fatalf("server saw %d requests, expected at least %d; the client may not have reached it",
			len(results), minAttempts)
	}
	for i, err := range results {
		if err != nil {
			t.Fatalf("request %d of %d was rejected: %v", i+1, len(results), err)
		}
	}
}

func TestAWSCLIAuthenticates(t *testing.T) {
	skipUnlessExternal(t)
	srv := startExternalServer(t)

	endpoint := fmt.Sprintf("http://host.docker.internal:%d", srv.port)
	script := strings.Join([]string{
		"echo 'hello from aws-cli' > /tmp/object.txt",
		"aws --endpoint-url " + endpoint + " s3api put-object --bucket bucket --key cli/object.txt --body /tmp/object.txt",
		"aws --endpoint-url " + endpoint + " s3api head-object --bucket bucket --key cli/object.txt",
		"aws --endpoint-url " + endpoint + " s3api list-objects-v2 --bucket bucket --prefix cli/",
	}, " && ")

	cmd := exec.CommandContext(t.Context(), "docker", "run", "--rm",
		"--add-host=host.docker.internal:host-gateway",
		"-e", "AWS_ACCESS_KEY_ID="+sdkAccessKeyID,
		"-e", "AWS_SECRET_ACCESS_KEY="+sdkSecretKey,
		"-e", "AWS_DEFAULT_REGION=us-east-1",
		"--entrypoint", "/bin/sh",
		"amazon/aws-cli:latest", "-c", script)

	out, err := cmd.CombinedOutput()
	t.Logf("aws-cli output:\n%s", out)
	if err != nil {
		t.Fatalf("aws-cli failed: %v", err)
	}
	srv.assertAllVerified(t, 3)
}

func TestBoto3Authenticates(t *testing.T) {
	skipUnlessExternal(t)
	srv := startExternalServer(t)

	endpoint := fmt.Sprintf("http://host.docker.internal:%d", srv.port)
	script := fmt.Sprintf(`
pip install --quiet boto3 2>/dev/null
python3 - <<'PY'
import boto3
from botocore.config import Config

s3 = boto3.client(
    "s3",
    endpoint_url=%q,
    aws_access_key_id=%q,
    aws_secret_access_key=%q,
    region_name="us-east-1",
    config=Config(s3={"addressing_style": "path"}),
)
s3.put_object(Bucket="bucket", Key="boto3/object.txt", Body=b"hello from boto3")
s3.head_object(Bucket="bucket", Key="boto3/object.txt")
s3.list_objects_v2(Bucket="bucket", Prefix="boto3/")
s3.put_object(Bucket="bucket", Key="boto3/awkward key $with,chars.txt", Body=b"x" * 100000)
print("boto3 ok")
PY`, endpoint, sdkAccessKeyID, sdkSecretKey)

	cmd := exec.CommandContext(t.Context(), "docker", "run", "--rm",
		"--add-host=host.docker.internal:host-gateway",
		"python:3.12-slim", "/bin/sh", "-c", script)

	out, err := cmd.CombinedOutput()
	t.Logf("boto3 output:\n%s", out)
	if err != nil {
		t.Fatalf("boto3 failed: %v", err)
	}
	srv.assertAllVerified(t, 4)
}

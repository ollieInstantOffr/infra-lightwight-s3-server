package s3api

import (
	"bytes"
	"context"
	"errors"
	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/db"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/httpx"
)

// These tests are the ones that actually matter. Matching AWS's published
// vectors proves the algorithm; letting a real SDK sign real requests proves
// the reading of net/http, the header handling and the body framing, which is
// where an implementation that passes the vectors still tends to fail.

const (
	sdkAccessKeyID = "AKIATESTTESTTESTTEST"
	sdkSecretKey   = "testSecretKeyThatIsFortyCharsLong1234567"
)

// captured records what a verified request turned out to be, so assertions can
// be made after the SDK call returns.
type captured struct {
	mu          sync.Mutex
	verifyErr   error
	accessKeyID string
	payloadHash string
	body        []byte
	bodyErr     error
	method      string
	path        string
	query       string
}

func (c *captured) snapshot() captured {
	c.mu.Lock()
	defer c.mu.Unlock()
	return captured{
		verifyErr: c.verifyErr, accessKeyID: c.accessKeyID, payloadHash: c.payloadHash,
		body: c.body, bodyErr: c.bodyErr, method: c.method, path: c.path, query: c.query,
	}
}

// newSDKTestServer stands up a server that verifies every request and records
// the outcome, then returns an S3 client pointed at it.
func newSDKTestServer(t *testing.T) (*s3.Client, *captured, string) {
	t.Helper()

	trust, err := httpx.NewProxyTrust(nil)
	if err != nil {
		t.Fatalf("NewProxyTrust: %v", err)
	}
	verifier := &Verifier{
		Region:  "us-east-1",
		Proxies: trust,
		Lookup: func(_ context.Context, accessKeyID string) (KeyMaterial, error) {
			if accessKeyID != sdkAccessKeyID {
				return KeyMaterial{}, errNoSuchKey
			}
			return KeyMaterial{SecretKey: sdkSecretKey, Grant: db.UnrestrictedGrant()}, nil
		},
	}

	rec := &captured{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, err := verifier.Verify(r.Context(), r)

		rec.mu.Lock()
		rec.verifyErr = err
		rec.method, rec.path, rec.query = r.Method, r.URL.Path, r.URL.RawQuery
		if err == nil {
			rec.accessKeyID, rec.payloadHash = id.AccessKeyID, id.PayloadHash
		}
		rec.mu.Unlock()

		if err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}

		body, readErr := io.ReadAll(verifier.Body(r, id))
		rec.mu.Lock()
		rec.body, rec.bodyErr = body, readErr
		rec.mu.Unlock()

		if readErr != nil {
			http.Error(w, readErr.Error(), http.StatusBadRequest)
			return
		}
		writeMinimalResponse(w, r)
	}))
	t.Cleanup(srv.Close)

	client := s3.New(s3.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(srv.URL),
		UsePathStyle: true,
		Credentials: credentials.NewStaticCredentialsProvider(
			sdkAccessKeyID, sdkSecretKey, ""),
	})
	return client, rec, srv.URL
}

// writeMinimalResponse returns just enough XML for the SDK to parse a result.
// The handlers themselves belong to later issues; this only has to satisfy the
// client so the signing path can be exercised.
func writeMinimalResponse(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/xml")
	switch {
	case r.Method == http.MethodGet && strings.Contains(r.URL.RawQuery, "list-type=2"):
		io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?>
<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
<Name>bucket</Name><KeyCount>0</KeyCount><MaxKeys>1000</MaxKeys><IsTruncated>false</IsTruncated>
</ListBucketResult>`)
	case r.Method == http.MethodGet:
		w.Header().Set("ETag", `"d41d8cd98f00b204e9800998ecf8427e"`)
		io.WriteString(w, "object contents")
	default:
		w.Header().Set("ETag", `"d41d8cd98f00b204e9800998ecf8427e"`)
	}
}

func TestAWSSDKPutObjectVerifies(t *testing.T) {
	client, rec, _ := newSDKTestServer(t)
	payload := []byte("the AWS SDK signs this body, and the server must decode it back byte for byte")

	_, err := client.PutObject(t.Context(), &s3.PutObjectInput{
		Bucket: aws.String("bucket"),
		Key:    aws.String("some/key.txt"),
		Body:   bytes.NewReader(payload),
	})
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	got := rec.snapshot()
	if got.verifyErr != nil {
		t.Fatalf("server rejected an SDK-signed request: %v", got.verifyErr)
	}
	if got.accessKeyID != sdkAccessKeyID {
		t.Errorf("access key id = %q, want %q", got.accessKeyID, sdkAccessKeyID)
	}
	if got.bodyErr != nil {
		t.Fatalf("reading the body failed: %v", got.bodyErr)
	}
	if !bytes.Equal(got.body, payload) {
		t.Errorf("body round-trip mismatch.\n got: %q\nwant: %q", got.body, payload)
	}
	t.Logf("SDK sent x-amz-content-sha256: %s", got.payloadHash)
}

// A body large enough that the SDK chunks it, which is where framing bugs show.
func TestAWSSDKPutLargeObjectVerifies(t *testing.T) {
	client, rec, _ := newSDKTestServer(t)
	payload := bytes.Repeat([]byte("0123456789abcdef"), 1<<16) // 1 MiB

	_, err := client.PutObject(t.Context(), &s3.PutObjectInput{
		Bucket: aws.String("bucket"),
		Key:    aws.String("large.bin"),
		Body:   bytes.NewReader(payload),
	})
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	got := rec.snapshot()
	if got.verifyErr != nil {
		t.Fatalf("server rejected an SDK-signed request: %v", got.verifyErr)
	}
	if got.bodyErr != nil {
		t.Fatalf("reading the body failed: %v", got.bodyErr)
	}
	if !bytes.Equal(got.body, payload) {
		t.Errorf("body round-trip mismatch: got %d bytes, want %d", len(got.body), len(payload))
	}
}

func TestAWSSDKGetObjectVerifies(t *testing.T) {
	client, rec, _ := newSDKTestServer(t)

	out, err := client.GetObject(t.Context(), &s3.GetObjectInput{
		Bucket: aws.String("bucket"),
		Key:    aws.String("some/key.txt"),
	})
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	defer out.Body.Close()

	if got := rec.snapshot(); got.verifyErr != nil {
		t.Fatalf("server rejected an SDK-signed request: %v", got.verifyErr)
	}
}

// Query parameters must canonicalise identically to the SDK's own ordering and
// encoding, or listing breaks while simple gets keep working.
func TestAWSSDKListObjectsVerifies(t *testing.T) {
	client, rec, _ := newSDKTestServer(t)

	_, err := client.ListObjectsV2(t.Context(), &s3.ListObjectsV2Input{
		Bucket:    aws.String("bucket"),
		Prefix:    aws.String("some/prefix with spaces/"),
		Delimiter: aws.String("/"),
		MaxKeys:   aws.Int32(42),
	})
	if err != nil {
		t.Fatalf("ListObjectsV2: %v", err)
	}

	got := rec.snapshot()
	if got.verifyErr != nil {
		t.Fatalf("server rejected an SDK-signed list request: %v", got.verifyErr)
	}
	t.Logf("query canonicalised from: %s", got.query)
}

// Keys containing characters that percent-encode differently between Go's url
// package and AWS's rules are exactly where canonical URI handling breaks.
func TestAWSSDKAwkwardKeysVerify(t *testing.T) {
	keys := []string{
		"plain.txt",
		"with space.txt",
		"with+plus.txt",
		"with$dollar.txt",
		"with,comma.txt",
		"with=equals.txt",
		"with&ampersand.txt",
		"nested/deeply/inside/a/prefix.txt",
		"unicode-ünïcødé-文字.txt",
		"percent%encoded.txt",
		"question?mark.txt",
		"hash#symbol.txt",
		"tilde~and-dash_dot.txt",
	}

	for _, key := range keys {
		t.Run(key, func(t *testing.T) {
			client, rec, _ := newSDKTestServer(t)
			payload := []byte("contents of " + key)

			_, err := client.PutObject(t.Context(), &s3.PutObjectInput{
				Bucket: aws.String("bucket"),
				Key:    aws.String(key),
				Body:   bytes.NewReader(payload),
			})
			if err != nil {
				t.Fatalf("PutObject(%q): %v", key, err)
			}

			got := rec.snapshot()
			if got.verifyErr != nil {
				t.Fatalf("server rejected key %q: %v", key, got.verifyErr)
			}
			if !bytes.Equal(got.body, payload) {
				t.Errorf("body mismatch for key %q", key)
			}
		})
	}
}

func TestAWSSDKRejectsWrongSecret(t *testing.T) {
	_, rec, url := newSDKTestServer(t)

	bad := s3.New(s3.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(url),
		UsePathStyle: true,
		Credentials: credentials.NewStaticCredentialsProvider(
			sdkAccessKeyID, "wrongSecretKeyOfExactlyFortyCharacters12", ""),
	})
	if _, err := bad.GetObject(t.Context(), &s3.GetObjectInput{
		Bucket: aws.String("bucket"),
		Key:    aws.String("key"),
	}); err == nil {
		t.Fatal("a request signed with the wrong secret was accepted")
	}
	if got := rec.snapshot(); !errors.Is(got.verifyErr, ErrSignatureMismatch) {
		t.Errorf("verify error = %v, want ErrSignatureMismatch", got.verifyErr)
	}
}

// Over plain HTTP the SDK signs the payload with a real digest. Over TLS it
// switches to aws-chunked framing with a trailing checksum, which is the mode
// every production deployment will actually see — and the mode a server without
// a chunk decoder silently corrupts. This exercises that path with the real SDK
// rather than with hand-built framing.
func newTLSSDKTestServer(t *testing.T) (*s3.Client, *captured) {
	t.Helper()

	trust, err := httpx.NewProxyTrust(nil)
	if err != nil {
		t.Fatalf("NewProxyTrust: %v", err)
	}
	verifier := &Verifier{
		Region:  "us-east-1",
		Proxies: trust,
		Lookup: func(_ context.Context, accessKeyID string) (KeyMaterial, error) {
			if accessKeyID != sdkAccessKeyID {
				return KeyMaterial{}, errNoSuchKey
			}
			return KeyMaterial{SecretKey: sdkSecretKey, Grant: db.UnrestrictedGrant()}, nil
		},
	}

	rec := &captured{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, err := verifier.Verify(r.Context(), r)

		rec.mu.Lock()
		rec.verifyErr = err
		if err == nil {
			rec.accessKeyID, rec.payloadHash = id.AccessKeyID, id.PayloadHash
		}
		rec.mu.Unlock()

		if err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}

		body, readErr := io.ReadAll(verifier.Body(r, id))
		rec.mu.Lock()
		rec.body, rec.bodyErr = body, readErr
		rec.mu.Unlock()

		if readErr != nil {
			http.Error(w, readErr.Error(), http.StatusBadRequest)
			return
		}
		writeMinimalResponse(w, r)
	}))
	t.Cleanup(srv.Close)

	client := s3.New(s3.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(srv.URL),
		UsePathStyle: true,
		HTTPClient:   srv.Client(),
		Credentials: credentials.NewStaticCredentialsProvider(
			sdkAccessKeyID, sdkSecretKey, ""),
	})
	return client, rec
}

func TestAWSSDKOverTLS(t *testing.T) {
	client, rec := newTLSSDKTestServer(t)
	payload := bytes.Repeat([]byte("chunked payload "), 1<<16) // 1 MiB

	_, err := client.PutObject(t.Context(), &s3.PutObjectInput{
		Bucket: aws.String("bucket"),
		Key:    aws.String("streamed.bin"),
		Body:   bytes.NewReader(payload),
	})
	if err != nil {
		t.Fatalf("PutObject over TLS: %v", err)
	}

	got := rec.snapshot()
	if got.verifyErr != nil {
		t.Fatalf("server rejected an SDK-signed TLS request: %v", got.verifyErr)
	}
	if got.bodyErr != nil {
		t.Fatalf("decoding the body failed: %v", got.bodyErr)
	}
	if !bytes.Equal(got.body, payload) {
		t.Fatalf("body mismatch: decoded %d bytes, want %d", len(got.body), len(payload))
	}
	// Over TLS the SDK trusts the transport and skips payload signing entirely.
	if got.payloadHash != UnsignedPayload {
		t.Logf("SDK over TLS sent x-amz-content-sha256: %s", got.payloadHash)
	}
}

// Requesting a flexible checksum is what makes the SDK reach for aws-chunked
// framing with a trailing header. This is the STREAMING-UNSIGNED-PAYLOAD-TRAILER
// path, driven by the real SDK rather than by hand-built framing.
func TestAWSSDKChecksumTriggersChunkedFraming(t *testing.T) {
	for _, tc := range []struct {
		name string
		tls  bool
	}{{"over HTTP", false}, {"over TLS", true}} {
		t.Run(tc.name, func(t *testing.T) {
			var client *s3.Client
			var rec *captured
			if tc.tls {
				client, rec = newTLSSDKTestServer(t)
			} else {
				client, rec, _ = newSDKTestServer(t)
			}

			payload := bytes.Repeat([]byte("trailer checksum payload "), 1<<15)

			_, err := client.PutObject(t.Context(), &s3.PutObjectInput{
				Bucket:            aws.String("bucket"),
				Key:               aws.String("checksummed.bin"),
				Body:              bytes.NewReader(payload),
				ChecksumAlgorithm: types.ChecksumAlgorithmCrc32,
			})
			if err != nil {
				t.Fatalf("PutObject with CRC32 checksum: %v", err)
			}

			got := rec.snapshot()
			if got.verifyErr != nil {
				t.Fatalf("server rejected the request: %v", got.verifyErr)
			}
			if got.bodyErr != nil {
				t.Fatalf("decoding the body failed: %v", got.bodyErr)
			}
			if !bytes.Equal(got.body, payload) {
				t.Fatalf("body mismatch: decoded %d bytes, want %d; framing or trailer leaked into the payload",
					len(got.body), len(payload))
			}
			t.Logf("x-amz-content-sha256: %s (streaming=%v)", got.payloadHash, IsStreaming(got.payloadHash))
		})
	}
}

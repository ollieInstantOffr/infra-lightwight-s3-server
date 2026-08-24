package main

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/config"
	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/db"
	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/s3api"
	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/secrets"
)

// "The uploads are slow" has two causes that look identical from a client: the
// server is slow, or everything between the client and the server is. Only one
// of them is fixable here, and guessing wrong costs days.
//
// selftest answers it in one number. It drives the running server over
// loopback, so the reverse proxy, TLS termination and the network link are all
// out of the path. Run it on the server host and compare: if it reports many
// times what a remote client sees, the server is not the bottleneck and there
// is no point optimising it further.
//
// Bodies are sent as UNSIGNED-PAYLOAD deliberately. Hashing a few hundred
// megabytes client-side would put the test's own CPU cost inside the
// measurement; the server still hashes every byte it stores either way, so the
// work being measured is the server's, not this command's.

const selftestUsage = `Usage:
  s3d selftest [--size MB] [--concurrency N]

Measures the running server's throughput over loopback, with no reverse proxy
in the path. Creates a temporary bucket and access key, and removes both when
it finishes.

  --size MB        object size for the single-stream tests (default 200)
  --concurrency N  parallel uploads for the concurrent test (default 16)
`

const (
	defaultSelftestSizeMB      = 200
	defaultSelftestConcurrency = 16

	// Bounded so a mistyped flag cannot fill the data volume or fork thousands
	// of transfers at a server that is presumably already struggling.
	maxSelftestSizeMB      = 4096
	maxSelftestConcurrency = 256

	selftestTimeout = 30 * time.Minute
)

// runSelftest handles "s3d selftest". It returns false if the arguments are
// not a selftest command, so the caller can carry on and start the server.
func runSelftest(args []string) (handled bool, err error) {
	if len(args) == 0 || args[0] != "selftest" {
		return false, nil
	}

	sizeMB, concurrency, err := parseSelftestFlags(args[1:])
	if err != nil {
		return true, err
	}

	cfg, err := config.Load()
	if err != nil {
		return true, err
	}
	cipher, err := secrets.NewCipher(cfg.CredentialsKey)
	if err != nil {
		return true, fmt.Errorf("credentials key: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), selftestTimeout)
	defer cancel()

	pool, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return true, err
	}
	defer pool.Close()

	// A real credential is the only way to exercise the real signing path. It
	// is revoked on the way out, including when a transfer fails partway.
	cred, err := db.CreateCredential(ctx, pool, cipher, "s3d selftest (temporary)", nil, db.UnrestrictedGrant())
	if err != nil {
		return true, fmt.Errorf("create temporary credential: %w", err)
	}
	defer func() {
		if revokeErr := db.RevokeCredential(context.Background(), pool, cred.AccessKeyID); revokeErr != nil {
			fmt.Fprintf(os.Stderr, "warning: could not revoke the temporary credential %s: %v\n",
				cred.AccessKeyID, revokeErr)
		}
	}()

	client := &selftestClient{
		endpoint:    fmt.Sprintf("http://127.0.0.1:%d", cfg.S3Port),
		accessKeyID: cred.AccessKeyID,
		secretKey:   cred.SecretKey,
		region:      cfg.S3Region,
		http: &http.Client{
			// No timeout: a large object on slow storage is the case being
			// measured, and cutting it off would report a failure instead of a
			// slow number. The context bounds the run overall.
			Transport: &http.Transport{
				MaxIdleConnsPerHost: maxSelftestConcurrency,
				DisableCompression:  true,
			},
		},
	}

	return true, runSelftestAgainst(ctx, client, sizeMB, concurrency)
}

// parseSelftestFlags reads the two options by hand rather than through the flag
// package, which would otherwise claim the process's global flag set.
func parseSelftestFlags(args []string) (sizeMB, concurrency int, err error) {
	sizeMB = defaultSelftestSizeMB
	concurrency = defaultSelftestConcurrency

	for i := 0; i < len(args); i++ {
		var target *int
		var name string
		switch args[i] {
		case "--size":
			target, name = &sizeMB, "--size"
		case "--concurrency":
			target, name = &concurrency, "--concurrency"
		default:
			return 0, 0, fmt.Errorf("unknown option %q\n\n%s", args[i], selftestUsage)
		}

		i++
		if i >= len(args) {
			return 0, 0, fmt.Errorf("%s needs a value\n\n%s", name, selftestUsage)
		}
		value, convErr := strconv.Atoi(args[i])
		if convErr != nil || value < 1 {
			return 0, 0, fmt.Errorf("%s needs a positive whole number, got %q", name, args[i])
		}
		*target = value
	}

	if sizeMB > maxSelftestSizeMB {
		return 0, 0, fmt.Errorf("--size is capped at %d MB", maxSelftestSizeMB)
	}
	if concurrency > maxSelftestConcurrency {
		return 0, 0, fmt.Errorf("--concurrency is capped at %d", maxSelftestConcurrency)
	}
	return sizeMB, concurrency, nil
}

// runSelftestAgainst is the body of the command, separated from credential and
// configuration handling so it can be driven against a test server.
func runSelftestAgainst(ctx context.Context, c *selftestClient, sizeMB, concurrency int) error {
	bucket, err := selftestBucketName()
	if err != nil {
		return err
	}

	fmt.Printf("Measuring %s over loopback — no reverse proxy in the path.\n\n", c.endpoint)

	if err := c.createBucket(ctx, bucket); err != nil {
		return fmt.Errorf("create the temporary bucket: %w", err)
	}
	// Cleanup runs even when a measurement fails, so a failed run does not
	// leave a bucket full of test objects behind.
	keys := []string{}
	defer func() {
		for _, key := range keys {
			if err := c.deleteObject(context.Background(), bucket, key); err != nil {
				fmt.Fprintf(os.Stderr, "warning: could not delete %s/%s: %v\n", bucket, key, err)
			}
		}
		if err := c.deleteBucket(context.Background(), bucket); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not delete the bucket %s: %v\n", bucket, err)
		}
	}()

	size := int64(sizeMB) << 20

	// Single-stream upload.
	key := "single.bin"
	keys = append(keys, key)
	start := time.Now()
	if err := c.putObject(ctx, bucket, key, size); err != nil {
		return fmt.Errorf("upload: %w", err)
	}
	putRate := throughput(size, time.Since(start))
	fmt.Printf("  upload,   single stream   %7.1f MB/s   (%d MB)\n", putRate, sizeMB)

	// Single-stream download.
	start = time.Now()
	read, err := c.getObject(ctx, bucket, key)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	if read != size {
		return fmt.Errorf("download returned %d bytes, expected %d", read, size)
	}
	getRate := throughput(size, time.Since(start))
	fmt.Printf("  download, single stream   %7.1f MB/s   (%d MB)\n", getRate, sizeMB)

	// Concurrent upload. Each stream is smaller so the whole test stays within
	// a sensible amount of disk and time; the aggregate is what matters.
	concurrentSize := size / int64(concurrency)
	if concurrentSize < 1<<20 {
		concurrentSize = 1 << 20
	}
	concurrentKeys := make([]string, concurrency)
	for i := range concurrentKeys {
		concurrentKeys[i] = fmt.Sprintf("concurrent-%d.bin", i)
	}
	keys = append(keys, concurrentKeys...)

	start = time.Now()
	errs := make([]error, concurrency)
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = c.putObject(ctx, bucket, concurrentKeys[i], concurrentSize)
		}(i)
	}
	wg.Wait()
	elapsed := time.Since(start)

	failed := 0
	var firstErr error
	for _, err := range errs {
		if err != nil {
			failed++
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	aggregate := throughput(concurrentSize*int64(concurrency-failed), elapsed)
	fmt.Printf("  upload,   %2d streams      %7.1f MB/s   (%d x %d MB aggregate)\n",
		concurrency, aggregate, concurrency, concurrentSize>>20)

	if failed > 0 {
		fmt.Printf("\n  %d of %d concurrent uploads failed. First failure: %v\n",
			failed, concurrency, firstErr)
		fmt.Println("  Failures under concurrency are a server-side problem — that is worth chasing.")
		return fmt.Errorf("%d of %d concurrent uploads failed", failed, concurrency)
	}

	printSelftestVerdict(putRate, getRate)
	return nil
}

// printSelftestVerdict turns the numbers into the comparison the operator
// actually came for, rather than leaving them to judge it.
func printSelftestVerdict(putRate, getRate float64) {
	slowest := putRate
	if getRate < slowest {
		slowest = getRate
	}
	fmt.Printf(`
This is what the server can do with nothing in front of it.

Compare it with what a real client sees against the public endpoint. If that
number is far below %.0f MB/s, the difference is the reverse proxy, TLS
termination or the network link — not the server, and not something a change
to this codebase will fix.

  Same test from the client's machine:
    aws --endpoint-url <public-url> s3 cp <a large file> s3://<bucket>/

  If that is slow but this was fast, look at (in order): the link's actual
  bandwidth, the proxy host's CPU during a transfer, and the proxy's buffering
  and timeout settings — docs/reverse-proxy.md covers the last of those.
`, slowest)
}

func throughput(bytes int64, elapsed time.Duration) float64 {
	if elapsed <= 0 {
		return 0
	}
	return (float64(bytes) / (1 << 20)) / elapsed.Seconds()
}

// selftestBucketName returns a name no real bucket is likely to collide with,
// so a crashed earlier run cannot make this one fail.
func selftestBucketName() (string, error) {
	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		return "", fmt.Errorf("generate a bucket name: %w", err)
	}
	return fmt.Sprintf("s3d-selftest-%x", suffix), nil
}

// selftestClient is a minimal signed S3 client. It uses the server's own SigV4
// implementation rather than an SDK, which keeps the production binary from
// carrying an SDK purely for a diagnostic.
type selftestClient struct {
	endpoint    string
	accessKeyID string
	secretKey   string
	region      string
	http        *http.Client
}

func (c *selftestClient) createBucket(ctx context.Context, bucket string) error {
	return c.do(ctx, http.MethodPut, "/"+bucket, nil, 0)
}

func (c *selftestClient) deleteBucket(ctx context.Context, bucket string) error {
	return c.do(ctx, http.MethodDelete, "/"+bucket, nil, 0)
}

func (c *selftestClient) deleteObject(ctx context.Context, bucket, key string) error {
	return c.do(ctx, http.MethodDelete, "/"+bucket+"/"+key, nil, 0)
}

func (c *selftestClient) putObject(ctx context.Context, bucket, key string, size int64) error {
	return c.do(ctx, http.MethodPut, "/"+bucket+"/"+key, newPatternReader(size), size)
}

// getObject streams the body to io.Discard and reports how many bytes arrived,
// so the measurement covers the transfer rather than the parsing.
func (c *selftestClient) getObject(ctx context.Context, bucket, key string) (int64, error) {
	request, err := c.sign(ctx, http.MethodGet, "/"+bucket+"/"+key, nil, 0)
	if err != nil {
		return 0, err
	}
	response, err := c.http.Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return 0, statusError(response)
	}
	return io.Copy(io.Discard, response.Body)
}

func (c *selftestClient) do(ctx context.Context, method, path string, body io.Reader, size int64) error {
	request, err := c.sign(ctx, method, path, body, size)
	if err != nil {
		return err
	}
	response, err := c.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	// Draining matters: an undrained body prevents the connection being reused,
	// which would quietly turn the concurrent test into a connection-setup test.
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))

	if response.StatusCode < 200 || response.StatusCode > 299 {
		return statusError(response)
	}
	return nil
}

// sign builds a request carrying a valid SigV4 Authorization header.
func (c *selftestClient) sign(ctx context.Context, method, path string, body io.Reader, size int64) (*http.Request, error) {
	request, err := http.NewRequestWithContext(ctx, method, c.endpoint+path, body)
	if err != nil {
		return nil, err
	}
	// Set explicitly so the body is sent with a Content-Length rather than
	// chunked transfer encoding, which is what a real client does.
	request.ContentLength = size

	now := time.Now().UTC()
	scope := s3api.Scope{
		Date:    now.Format("20060102"),
		Region:  c.region,
		Service: "s3",
	}
	timestamp := now.Format("20060102T150405Z")

	request.Header.Set("X-Amz-Date", timestamp)
	request.Header.Set("X-Amz-Content-Sha256", s3api.UnsignedPayload)

	signedHeaders := []string{"host", "x-amz-content-sha256", "x-amz-date"}
	canonical := s3api.CanonicalRequest(
		method, path, "", request.Header, request.Host, signedHeaders, s3api.UnsignedPayload)
	signature := s3api.Sign(s3api.SigningKey(c.secretKey, scope), s3api.StringToSign(now, scope, canonical))

	request.Header.Set("Authorization", fmt.Sprintf(
		"%s Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		s3api.Algorithm, c.accessKeyID, scope.String(), strings.Join(signedHeaders, ";"), signature))
	return request, nil
}

// statusError includes the server's own error body, which names the actual
// problem far better than the status code alone.
func statusError(response *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(response.Body, 2<<10))
	detail := strings.TrimSpace(string(body))
	if detail == "" {
		return fmt.Errorf("%s", response.Status)
	}
	return fmt.Errorf("%s: %s", response.Status, detail)
}

// patternReader yields a fixed number of bytes from a small random block,
// repeated. Generating the whole payload up front would cost memory
// proportional to the test size, and reading from /dev/urandom for every byte
// would measure the random number generator instead of the server.
//
// The block is random rather than zeroes so a filesystem with compression or
// hole-punching cannot make the write disappear and flatter the result.
type patternReader struct {
	block     []byte
	remaining int64
	offset    int
}

func newPatternReader(size int64) *patternReader {
	block := make([]byte, 256<<10)
	// A failure here is not worth aborting a diagnostic for; the zero block
	// still measures the transport, and rand.Read does not fail in practice.
	_, _ = rand.Read(block)
	return &patternReader{block: block, remaining: size}
}

func (p *patternReader) Read(dst []byte) (int, error) {
	if p.remaining <= 0 {
		return 0, io.EOF
	}
	if int64(len(dst)) > p.remaining {
		dst = dst[:p.remaining]
	}

	written := 0
	for written < len(dst) {
		n := copy(dst[written:], p.block[p.offset:])
		written += n
		p.offset += n
		if p.offset >= len(p.block) {
			p.offset = 0
		}
	}
	p.remaining -= int64(written)
	return written, nil
}

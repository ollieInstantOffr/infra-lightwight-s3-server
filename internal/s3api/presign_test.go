package s3api

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// The SDK's own presigner is the reference: if a URL it produces verifies here,
// the query-string signing scheme matches.
func TestPresignedGetFromSDK(t *testing.T) {
	client, _ := newIntegrationServer(t)
	ctx := t.Context()
	makeBucket(t, client, "presigned")

	const contents = "the contents behind a share link"
	if _, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("presigned"), Key: aws.String("shared/file.txt"),
		Body: strings.NewReader(contents),
	}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	presigner := s3.NewPresignClient(client)
	signed, err := presigner.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String("presigned"), Key: aws.String("shared/file.txt"),
	}, s3.WithPresignExpires(10*time.Minute))
	if err != nil {
		t.Fatalf("PresignGetObject: %v", err)
	}

	// Fetched with a plain HTTP client holding no credentials at all — which is
	// the whole point.
	resp, err := http.Get(signed.URL)
	if err != nil {
		t.Fatalf("GET presigned URL: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("presigned GET returned %d: %s", resp.StatusCode, body)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != contents {
		t.Errorf("body = %q, want %q", body, contents)
	}
}

// Direct browser upload: the console hands out a signed PUT so bytes go
// straight to the S3 port instead of through the console process.
func TestPresignedPutFromSDK(t *testing.T) {
	client, _ := newIntegrationServer(t)
	ctx := t.Context()
	makeBucket(t, client, "presigned")

	presigner := s3.NewPresignClient(client)
	signed, err := presigner.PresignPutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("presigned"), Key: aws.String("uploaded.txt"),
	}, s3.WithPresignExpires(10*time.Minute))
	if err != nil {
		t.Fatalf("PresignPutObject: %v", err)
	}

	const contents = "uploaded straight from a browser"
	request, err := http.NewRequest(http.MethodPut, signed.URL, strings.NewReader(contents))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("PUT presigned URL: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("presigned PUT returned %d: %s", resp.StatusCode, body)
	}

	get, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String("presigned"), Key: aws.String("uploaded.txt"),
	})
	if err != nil {
		t.Fatalf("GetObject after presigned upload: %v", err)
	}
	defer get.Body.Close()
	body, _ := io.ReadAll(get.Body)
	if string(body) != contents {
		t.Errorf("stored body = %q, want %q", body, contents)
	}
}

// The signature covers the path, so a recipient cannot edit the link to reach
// a different object.
func TestPresignedURLCannotBeRetargeted(t *testing.T) {
	client, _ := newIntegrationServer(t)
	ctx := t.Context()
	makeBucket(t, client, "presigned")
	seedKeys(t, client, "presigned", "public.txt", "secret.txt")

	presigner := s3.NewPresignClient(client)
	signed, err := presigner.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String("presigned"), Key: aws.String("public.txt"),
	}, s3.WithPresignExpires(10*time.Minute))
	if err != nil {
		t.Fatalf("PresignGetObject: %v", err)
	}

	tampered := strings.Replace(signed.URL, "public.txt", "secret.txt", 1)
	resp, err := http.Get(tampered)
	if err != nil {
		t.Fatalf("GET tampered URL: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatal("a presigned link edited to point at a different key was honoured")
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
}

func TestPresignedURLExpires(t *testing.T) {
	client, _ := newIntegrationServer(t)
	ctx := t.Context()
	makeBucket(t, client, "presigned")
	seedKeys(t, client, "presigned", "file.txt")

	presigner := s3.NewPresignClient(client)
	signed, err := presigner.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String("presigned"), Key: aws.String("file.txt"),
	}, s3.WithPresignExpires(time.Second))
	if err != nil {
		t.Fatalf("PresignGetObject: %v", err)
	}

	// Valid now.
	resp, err := http.Get(signed.URL)
	if err != nil {
		t.Fatalf("GET presigned URL: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("presigned GET returned %d before expiry", resp.StatusCode)
	}

	time.Sleep(2 * time.Second)

	resp, err = http.Get(signed.URL)
	if err != nil {
		t.Fatalf("GET expired URL: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatal("an expired presigned URL was still honoured")
	}
}

// The server's own Presign helper is what the console will call, so it has to
// produce URLs this same server accepts.
func TestServerSidePresignRoundTrips(t *testing.T) {
	client, _ := newIntegrationServer(t)
	ctx := t.Context()
	makeBucket(t, client, "presigned")

	const contents = "generated by the server's own presigner"
	if _, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("presigned"), Key: aws.String("share me/file (1).txt"),
		Body: strings.NewReader(contents),
	}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	endpoint := client.Options().BaseEndpoint
	creds, err := client.Options().Credentials.Retrieve(ctx)
	if err != nil {
		t.Fatalf("retrieve credentials: %v", err)
	}

	url, err := Presign(http.MethodGet, aws.ToString(endpoint),
		"presigned", "share me/file (1).txt", nil,
		creds.AccessKeyID, creds.SecretAccessKey, "us-east-1",
		time.Now(), 5*time.Minute)
	if err != nil {
		t.Fatalf("Presign: %v", err)
	}

	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET server-presigned URL: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("server-presigned GET returned %d: %s\nurl: %s", resp.StatusCode, body, url)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != contents {
		t.Errorf("body = %q, want %q", body, contents)
	}
}

func TestPresignRejectsUnreasonableExpiry(t *testing.T) {
	for _, expiry := range []time.Duration{0, -time.Minute, 8 * 24 * time.Hour} {
		if _, err := Presign(http.MethodGet, "https://s3.example.com", "b", "k", nil,
			"AKIA1", "secret", "us-east-1", time.Now(), expiry); err == nil {
			t.Errorf("Presign accepted an expiry of %s", expiry)
		}
	}
}

// Virtual-host style addressing: mybucket.s3.example.com/key.
func TestBucketFromHost(t *testing.T) {
	cases := []struct {
		host, domain, want string
		ok                 bool
	}{
		{"mybucket.s3.example.com", "s3.example.com", "mybucket", true},
		{"mybucket.s3.example.com:8443", "s3.example.com", "mybucket", true},
		{"MyBucket.S3.Example.Com", "s3.example.com", "mybucket", true},
		{"mybucket.s3.example.com.", "s3.example.com", "mybucket", true},
		// The bare domain is not a bucket; it is the service endpoint.
		{"s3.example.com", "s3.example.com", "", false},
		// A deeper subdomain is not a bucket: each dot is a DNS label, and a
		// wildcard certificate covers only one level.
		{"a.b.s3.example.com", "s3.example.com", "", false},
		// Not a legal bucket name, so not a bucket.
		{"AB.s3.example.com", "s3.example.com", "", false},
		{"my_bucket.s3.example.com", "s3.example.com", "", false},
		// A different domain entirely.
		{"mybucket.s3.other.com", "s3.example.com", "", false},
		// Virtual-host addressing is off unless a domain is configured.
		{"mybucket.s3.example.com", "", "", false},
	}
	for _, tc := range cases {
		got, ok := bucketFromHost(tc.host, tc.domain)
		if got != tc.want || ok != tc.ok {
			t.Errorf("bucketFromHost(%q, %q) = %q, %v; want %q, %v",
				tc.host, tc.domain, got, ok, tc.want, tc.ok)
		}
	}
}

// With a domain configured, a request whose Host names the bucket must resolve
// the same object as the path-style equivalent.
func TestVirtualHostStyleAddressing(t *testing.T) {
	client, _ := newIntegrationServer(t)
	ctx := t.Context()
	makeBucket(t, client, "vhost")

	const contents = "reached through a virtual host"
	if _, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("vhost"), Key: aws.String("nested/file.txt"),
		Body: strings.NewReader(contents),
	}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	// The same server, addressed virtual-host style. The SDK signs the Host it
	// sends, so this also proves signature verification uses the same host the
	// bucket was resolved from.
	vhost := newVirtualHostServer(t, "s3.example.com")
	body, status := vhost.get(t, "vhost.s3.example.com", "/nested/file.txt")
	if status != http.StatusOK {
		t.Fatalf("virtual-host GET returned %d: %s", status, body)
	}
	if body != contents {
		t.Errorf("body = %q, want %q", body, contents)
	}
}

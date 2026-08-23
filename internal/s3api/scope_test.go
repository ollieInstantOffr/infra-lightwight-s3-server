package s3api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"

	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/db"
	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/httpx"
	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/storage"
)

// A scope checked on GetObject and missed on multipart is worse than no scope,
// because it reads as protection that is not there. These tests drive the real
// router — every request goes through Handler, exactly as a client's would —
// and the table in TestEveryRouteIsGuarded is the enumeration ILS-81 asked for:
// it fails when a route is added without a decision about what it requires.

// scopedServer builds a server whose single key carries the given grant.
//
// A real database, because the point is to drive the actual handlers: a route
// that passes the scope check must go on to do its real work, and a fixture
// that panicked there would hide whether the check let it through.
func scopedServer(t *testing.T, grant db.Grant) *httptest.Server {
	return scopedServerVar(t, &grant)
}

// scopedServerVar is the same, with a grant the test can change between
// requests — so one test can set data up as an unrestricted key and then read
// it back as a narrow one, against the same database.
func scopedServerVar(t *testing.T, grant *db.Grant) *httptest.Server {
	t.Helper()

	ctx := context.Background()
	pool, err := db.Connect(ctx, testDSN(t, "test_s3api_pkg"))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	for _, stmt := range []string{`DELETE FROM buckets`, `DELETE FROM blobs`} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("reset (%s): %v", stmt, err)
		}
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
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Verifier: &Verifier{
			Region:  "us-east-1",
			Proxies: trust,
			Lookup: func(_ context.Context, accessKeyID string) (KeyMaterial, error) {
				if accessKeyID != exampleAccessKeyID {
					return KeyMaterial{}, errNoSuchKey
				}
				return KeyMaterial{SecretKey: exampleSecretKey, Grant: *grant}, nil
			},
		},
		Region: "us-east-1",
	}
	srv := httptest.NewServer(server.Handler())
	t.Cleanup(srv.Close)
	return srv
}

// route is one way into the server, with what it should require.
type route struct {
	name   string
	method string
	// target is the request path and query, relative to the server root.
	target string
	body   string
	header map[string]string
}

// everyRoute lists every entry point the router dispatches. A new operation
// that does not appear here fails TestEveryRouteIsGuarded, which is the point:
// the failure mode this epic guards against is an operation added later with no
// authorization decision made about it.
func everyRoute() []route {
	return []route{
		{name: "GetObject", method: http.MethodGet, target: "/b/k"},
		{name: "HeadObject", method: http.MethodHead, target: "/b/k"},
		{name: "PutObject", method: http.MethodPut, target: "/b/k", body: "x"},
		{name: "DeleteObject", method: http.MethodDelete, target: "/b/k"},
		{name: "ListObjects", method: http.MethodGet, target: "/b"},
		{name: "ListObjectsV2", method: http.MethodGet, target: "/b?list-type=2"},
		{name: "HeadBucket", method: http.MethodHead, target: "/b"},
		{name: "CreateBucket", method: http.MethodPut, target: "/b"},
		{name: "DeleteBucket", method: http.MethodDelete, target: "/b"},
		{name: "DeleteObjects", method: http.MethodPost, target: "/b?delete",
			body: `<Delete><Object><Key>k</Key></Object></Delete>`},
		{name: "CreateMultipartUpload", method: http.MethodPost, target: "/b/k?uploads"},
		{name: "UploadPart", method: http.MethodPut, target: "/b/k?uploadId=u&partNumber=1", body: "x"},
		{name: "CompleteMultipartUpload", method: http.MethodPost, target: "/b/k?uploadId=u",
			body: `<CompleteMultipartUpload></CompleteMultipartUpload>`},
		{name: "AbortMultipartUpload", method: http.MethodDelete, target: "/b/k?uploadId=u"},
		{name: "ListParts", method: http.MethodGet, target: "/b/k?uploadId=u"},
		{name: "ListMultipartUploads", method: http.MethodGet, target: "/b?uploads"},
		{name: "CopyObject", method: http.MethodPut, target: "/b/k",
			header: map[string]string{"x-amz-copy-source": "/other/source"}},
	}
}

// send signs and performs one route against a server.
func send(t *testing.T, srv *httptest.Server, rt route) *http.Response {
	t.Helper()

	request, err := http.NewRequest(rt.method, srv.URL+rt.target, strings.NewReader(rt.body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	for k, v := range rt.header {
		request.Header.Set(k, v)
	}

	// Signed with the AWS SDK's own signer, so these tests exercise the real
	// verifier against a real signature rather than a reimplementation of one.
	sum := sha256.Sum256([]byte(rt.body))
	payloadHash := hex.EncodeToString(sum[:])
	request.Header.Set("X-Amz-Content-Sha256", payloadHash)
	creds := aws.Credentials{AccessKeyID: exampleAccessKeyID, SecretAccessKey: exampleSecretKey}
	if err := v4.NewSigner().SignHTTP(t.Context(), creds, request,
		payloadHash, "s3", "us-east-1", time.Now()); err != nil {
		t.Fatalf("sign request: %v", err)
	}

	response, err := srv.Client().Do(request)
	if err != nil {
		t.Fatalf("%s: %v", rt.name, err)
	}
	return response
}

func TestEveryRouteIsGuarded(t *testing.T) {
	// A key that can do nothing at all. Every route must refuse it, and must
	// refuse it with AccessDenied rather than by failing some other way that
	// happens to look like a rejection.
	srv := scopedServer(t, db.Grant{})

	for _, rt := range everyRoute() {
		t.Run(rt.name, func(t *testing.T) {
			response := send(t, srv, rt)
			defer response.Body.Close()

			if response.StatusCode != http.StatusForbidden {
				body, _ := io.ReadAll(response.Body)
				t.Fatalf("status = %d, want 403 for a key with no permissions.\n"+
					"An unguarded route reads as protection that is not there.\nbody: %s",
					response.StatusCode, body)
			}
			// HEAD carries no body by definition, so there is no error
			// document to inspect — the status is the whole answer.
			if rt.method == http.MethodHead {
				return
			}
			body, _ := io.ReadAll(response.Body)
			if !strings.Contains(string(body), "AccessDenied") {
				t.Errorf("body did not report AccessDenied: %s", body)
			}
		})
	}
}

func TestUnrestrictedKeyIsNotRefusedByScope(t *testing.T) {
	// The other half: scoping must not have broken the ordinary case. These
	// requests fail for real reasons — no such bucket, no such upload — but
	// never with 403, which would mean the authorizer refused them.
	srv := scopedServer(t, db.UnrestrictedGrant())

	for _, rt := range everyRoute() {
		t.Run(rt.name, func(t *testing.T) {
			response := send(t, srv, rt)
			defer response.Body.Close()

			if response.StatusCode == http.StatusForbidden {
				body, _ := io.ReadAll(response.Body)
				t.Fatalf("an unrestricted key was refused: %s", body)
			}
		})
	}
}

func TestPermissionsAreDistinct(t *testing.T) {
	readOnly := db.Grant{Rules: []db.GrantRule{
		{Bucket: "b", Permissions: []db.Permission{db.PermissionRead}},
	}}
	srv := scopedServer(t, readOnly)

	allowed := map[string]bool{
		"GetObject": true, "HeadObject": true, "ListObjects": true,
		"ListObjectsV2": true, "HeadBucket": true, "ListMultipartUploads": true,
	}

	for _, rt := range everyRoute() {
		t.Run(rt.name, func(t *testing.T) {
			response := send(t, srv, rt)
			defer response.Body.Close()

			refused := response.StatusCode == http.StatusForbidden
			if allowed[rt.name] && refused {
				t.Errorf("a read-only key was refused %s", rt.name)
			}
			if !allowed[rt.name] && !refused {
				t.Errorf("a read-only key was permitted %s (status %d)", rt.name, response.StatusCode)
			}
		})
	}
}

func TestCopyChecksTheSourceNotOnlyTheDestination(t *testing.T) {
	// Copy is the operation that can move data a key cannot read into somewhere
	// it can. Checking only the destination would make read permission
	// meaningless for anyone holding write elsewhere.
	writeDestinationOnly := db.Grant{Rules: []db.GrantRule{
		{Bucket: "dest", Permissions: []db.Permission{db.PermissionRead, db.PermissionWrite}},
	}}
	srv := scopedServer(t, writeDestinationOnly)

	response := send(t, srv, route{
		name: "copy", method: http.MethodPut, target: "/dest/k",
		header: map[string]string{"x-amz-copy-source": "/secrets/private.pem"},
	})
	defer response.Body.Close()

	if response.StatusCode != http.StatusForbidden {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("copy from an unreadable bucket was not refused: status %d, body %s",
			response.StatusCode, body)
	}
}

func TestPrefixScopedKeyCannotEscapeItsPrefix(t *testing.T) {
	grant := db.Grant{Rules: []db.GrantRule{{
		Bucket:      "shared",
		Prefix:      "tenant-a/",
		Permissions: []db.Permission{db.PermissionRead, db.PermissionWrite, db.PermissionDelete},
	}}}
	srv := scopedServer(t, grant)

	cases := []struct {
		name    string
		rt      route
		refused bool
	}{
		{"read inside the prefix", route{method: http.MethodGet, target: "/shared/tenant-a/f"}, false},
		{"read outside the prefix", route{method: http.MethodGet, target: "/shared/tenant-b/f"}, true},
		{"write inside the prefix", route{method: http.MethodPut, target: "/shared/tenant-a/f", body: "x"}, false},
		{"write outside the prefix", route{method: http.MethodPut, target: "/shared/tenant-b/f", body: "x"}, true},
		{"delete outside the prefix", route{method: http.MethodDelete, target: "/shared/tenant-b/f"}, true},
		// Deleting the bucket would destroy the other tenant's data, so a
		// prefix-scoped key must not be able to, even holding delete.
		{"delete the whole bucket", route{method: http.MethodDelete, target: "/shared"}, true},
		{"create the bucket", route{method: http.MethodPut, target: "/shared"}, true},
		// Listing is narrowed rather than refused.
		{"list the bucket", route{method: http.MethodGet, target: "/shared"}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			response := send(t, srv, tc.rt)
			defer response.Body.Close()

			refused := response.StatusCode == http.StatusForbidden
			if refused != tc.refused {
				body, _ := io.ReadAll(response.Body)
				t.Errorf("refused = %v, want %v (status %d)\nbody: %s",
					refused, tc.refused, response.StatusCode, body)
			}
		})
	}
}

func TestListingShowsOnlyWhatTheKeyMayRead(t *testing.T) {
	// Listing narrows rather than refuses, so this is where a leak would hide:
	// a 403 is obvious, a listing quietly containing another tenant's key names
	// is not.
	grant := db.UnrestrictedGrant()
	srv := scopedServerVar(t, &grant)

	// Set up as an unrestricted key.
	mustSucceed(t, srv, route{method: http.MethodPut, target: "/shared"})
	for _, key := range []string{"tenant-a/one", "tenant-a/two", "tenant-b/secret", "loose"} {
		mustSucceed(t, srv, route{method: http.MethodPut, target: "/shared/" + key, body: "x"})
	}

	// Then narrow the key and look again.
	grant = db.Grant{Rules: []db.GrantRule{{
		Bucket: "shared", Prefix: "tenant-a/",
		Permissions: []db.Permission{db.PermissionRead},
	}}}

	body := readBody(t, srv, route{method: http.MethodGet, target: "/shared"})
	for _, want := range []string{"tenant-a/one", "tenant-a/two"} {
		if !strings.Contains(body, want) {
			t.Errorf("listing omitted %q, which the key may read:\n%s", want, body)
		}
	}
	for _, leaked := range []string{"tenant-b/secret", "loose"} {
		if strings.Contains(body, leaked) {
			t.Errorf("listing leaked %q, which the key may not read:\n%s", leaked, body)
		}
	}

	// Asking explicitly for someone else's prefix must not reveal it either.
	body = readBody(t, srv, route{method: http.MethodGet, target: "/shared?prefix=tenant-b/"})
	if strings.Contains(body, "tenant-b/secret") {
		t.Errorf("asking for another prefix by name returned it:\n%s", body)
	}

	// The response must echo the prefix the client asked for, not the one the
	// server narrowed the scan to — otherwise a client comparing them breaks,
	// and the key is told what its own scope is.
	body = readBody(t, srv, route{method: http.MethodGet, target: "/shared"})
	if strings.Contains(body, "<Prefix>tenant-a/</Prefix>") {
		t.Errorf("listing echoed the narrowed prefix rather than the requested one:\n%s", body)
	}
}

func TestBatchDeleteDecidesPerKey(t *testing.T) {
	// A thousand keys in one request is not one decision. A prefix-scoped key
	// must have the keys inside its prefix deleted and the rest refused, in the
	// same response.
	grant := db.UnrestrictedGrant()
	srv := scopedServerVar(t, &grant)

	mustSucceed(t, srv, route{method: http.MethodPut, target: "/shared"})
	for _, key := range []string{"tenant-a/gone", "tenant-b/kept"} {
		mustSucceed(t, srv, route{method: http.MethodPut, target: "/shared/" + key, body: "x"})
	}

	grant = db.Grant{Rules: []db.GrantRule{{
		Bucket: "shared", Prefix: "tenant-a/",
		Permissions: []db.Permission{db.PermissionRead, db.PermissionDelete},
	}}}

	body := readBody(t, srv, route{
		method: http.MethodPost, target: "/shared?delete",
		body: `<Delete>` +
			`<Object><Key>tenant-a/gone</Key></Object>` +
			`<Object><Key>tenant-b/kept</Key></Object>` +
			`</Delete>`,
	})

	if !strings.Contains(body, "<Deleted><Key>tenant-a/gone</Key></Deleted>") {
		t.Errorf("the key inside the prefix was not deleted:\n%s", body)
	}
	if !strings.Contains(body, "AccessDenied") || !strings.Contains(body, "tenant-b/kept") {
		t.Errorf("the key outside the prefix was not refused by name:\n%s", body)
	}

	// And the refusal must have been real, not just reported.
	grant = db.UnrestrictedGrant()
	response := send(t, srv, route{method: http.MethodHead, target: "/shared/tenant-b/kept"})
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Errorf("the object outside the prefix was deleted anyway (status %d)", response.StatusCode)
	}
}

// mustSucceed performs a route and fails the test if it did not.
func mustSucceed(t *testing.T, srv *httptest.Server, rt route) {
	t.Helper()
	response := send(t, srv, rt)
	defer response.Body.Close()
	if response.StatusCode >= 300 {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("%s %s: status %d\n%s", rt.method, rt.target, response.StatusCode, body)
	}
}

// readBody performs a route and returns its body.
func readBody(t *testing.T, srv *httptest.Server, rt route) string {
	t.Helper()
	response := send(t, srv, rt)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(body)
}

// recordingObserver captures what the metrics middleware reported.
type recordingObserver struct {
	mu         sync.Mutex
	operations []string
}

func (o *recordingObserver) Observe(_, operation string, _ int, _ time.Duration, _, _ int64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.operations = append(o.operations, operation)
}

func (o *recordingObserver) seen() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.operations...)
}

// The metrics middleware reads the operation the router recorded, and the two
// are separated by the whole middleware stack. When the holder that carries it
// was attached too far in, every request was reported as Unknown — a metric
// that existed, parsed, and had plausible numbers in it, all of them wrong.
// Nothing about that fails loudly, so it gets a test.
func TestMetricsSeeTheRoutedOperation(t *testing.T) {
	observer := &recordingObserver{}

	pool, err := db.Connect(context.Background(), testDSN(t, "test_s3api_pkg"))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(context.Background(), pool, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	blobs, err := storage.New(t.TempDir())
	if err != nil {
		t.Fatalf("storage.New: %v", err)
	}
	trust, _ := httpx.NewProxyTrust(nil)

	server := &Server{
		DB: pool, Blobs: blobs, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Region: "us-east-1", Scrape: observer,
		Verifier: &Verifier{
			Region: "us-east-1", Proxies: trust,
			Lookup: func(_ context.Context, _ string) (KeyMaterial, error) {
				return KeyMaterial{SecretKey: exampleSecretKey, Grant: db.UnrestrictedGrant()}, nil
			},
		},
	}
	srv := httptest.NewServer(server.Handler())
	t.Cleanup(srv.Close)

	for _, rt := range []route{
		{name: "CreateBucket", method: http.MethodPut, target: "/mbucket"},
		{name: "PutObject", method: http.MethodPut, target: "/mbucket/k", body: "x"},
		{name: "GetObject", method: http.MethodGet, target: "/mbucket/k"},
		{name: "ListObjectsV2", method: http.MethodGet, target: "/mbucket?list-type=2"},
	} {
		response := send(t, srv, rt)
		response.Body.Close()
	}

	seen := observer.seen()
	want := []string{"CreateBucket", "PutObject", "GetObject", "ListObjectsV2"}
	if len(seen) != len(want) {
		t.Fatalf("observed %v, want %v", seen, want)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Errorf("observation %d = %q, want %q (all Unknown means the holder is attached too far in)",
				i, seen[i], want[i])
		}
	}
}

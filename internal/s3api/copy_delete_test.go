package s3api

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

func TestBatchDeleteObjects(t *testing.T) {
	client, _ := newIntegrationServer(t)
	ctx := t.Context()
	makeBucket(t, client, "batch")
	seedKeys(t, client, "batch", "a.txt", "b.txt", "c.txt", "keep.txt")

	out, err := client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
		Bucket: aws.String("batch"),
		Delete: &types.Delete{Objects: []types.ObjectIdentifier{
			{Key: aws.String("a.txt")},
			{Key: aws.String("b.txt")},
			{Key: aws.String("c.txt")},
			// Deleting an absent key succeeds, matching single-key DELETE.
			{Key: aws.String("never-existed.txt")},
		}},
	})
	if err != nil {
		t.Fatalf("DeleteObjects: %v", err)
	}
	if len(out.Errors) != 0 {
		t.Errorf("got %d per-key errors, want none: %+v", len(out.Errors), out.Errors)
	}
	if len(out.Deleted) != 4 {
		t.Errorf("got %d deleted entries, want 4", len(out.Deleted))
	}

	list, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String("batch")})
	if err != nil {
		t.Fatalf("ListObjectsV2: %v", err)
	}
	if got, want := listedKeys(list), []string{"keep.txt"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("remaining keys = %v, want %v", got, want)
	}
}

// Quiet mode suppresses the per-key success entries, which is what makes
// deleting a thousand keys produce a small response instead of a large one.
func TestBatchDeleteQuiet(t *testing.T) {
	client, _ := newIntegrationServer(t)
	makeBucket(t, client, "batch")
	seedKeys(t, client, "batch", "a.txt", "b.txt")

	out, err := client.DeleteObjects(t.Context(), &s3.DeleteObjectsInput{
		Bucket: aws.String("batch"),
		Delete: &types.Delete{
			Quiet:   aws.Bool(true),
			Objects: []types.ObjectIdentifier{{Key: aws.String("a.txt")}, {Key: aws.String("b.txt")}},
		},
	})
	if err != nil {
		t.Fatalf("DeleteObjects: %v", err)
	}
	if len(out.Deleted) != 0 {
		t.Errorf("quiet mode returned %d Deleted entries, want none", len(out.Deleted))
	}
}

// aws s3 rm --recursive is the command this exists for.
func TestBatchDeleteEmptiesBucketForRecursiveRemove(t *testing.T) {
	client, _ := newIntegrationServer(t)
	ctx := t.Context()
	makeBucket(t, client, "batch")

	var keys []string
	for i := range 120 {
		keys = append(keys, fmt.Sprintf("bulk/%03d.txt", i))
	}
	seedKeys(t, client, "batch", keys...)

	identifiers := make([]types.ObjectIdentifier, 0, len(keys))
	for _, key := range keys {
		identifiers = append(identifiers, types.ObjectIdentifier{Key: aws.String(key)})
	}
	if _, err := client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
		Bucket: aws.String("batch"),
		Delete: &types.Delete{Objects: identifiers},
	}); err != nil {
		t.Fatalf("DeleteObjects: %v", err)
	}

	// The bucket must now be genuinely empty, so DeleteBucket succeeds.
	if _, err := client.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String("batch")}); err != nil {
		t.Fatalf("DeleteBucket after emptying: %v", err)
	}
}

// Copying is metadata-only: blobs are content-addressed, so the copy points at
// the same bytes rather than duplicating them.
func TestCopyObjectSharesTheBlob(t *testing.T) {
	client, pool := newIntegrationServer(t)
	ctx := t.Context()
	makeBucket(t, client, "copies")

	payload := strings.Repeat("copy me ", 100000) // ~800 KB
	if _, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("copies"), Key: aws.String("original.txt"),
		Body: strings.NewReader(payload), ContentType: aws.String("text/plain"),
	}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	out, err := client.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket: aws.String("copies"), Key: aws.String("duplicate.txt"),
		CopySource: aws.String("copies/original.txt"),
	})
	if err != nil {
		t.Fatalf("CopyObject: %v", err)
	}
	if out.CopyObjectResult == nil || out.CopyObjectResult.ETag == nil {
		t.Fatal("CopyObject response has no ETag")
	}

	var blobCount, refcount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*), coalesce(max(refcount), 0) FROM blobs`).Scan(&blobCount, &refcount); err != nil {
		t.Fatalf("count blobs: %v", err)
	}
	if blobCount != 1 || refcount != 2 {
		t.Errorf("got %d blobs with max refcount %d, want 1 blob referenced twice; "+
			"a copy must not duplicate bytes", blobCount, refcount)
	}

	get, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String("copies"), Key: aws.String("duplicate.txt"),
	})
	if err != nil {
		t.Fatalf("GetObject on the copy: %v", err)
	}
	defer get.Body.Close()
	body, _ := io.ReadAll(get.Body)
	if string(body) != payload {
		t.Error("the copy's contents differ from the original")
	}
	// COPY is the default directive, so content type comes from the source.
	if ct := aws.ToString(get.ContentType); ct != "text/plain" {
		t.Errorf("ContentType = %q, want text/plain carried over from the source", ct)
	}
}

func TestCopyObjectBetweenBuckets(t *testing.T) {
	client, _ := newIntegrationServer(t)
	ctx := t.Context()
	makeBucket(t, client, "source-bucket")
	makeBucket(t, client, "target-bucket")
	seedKeys(t, client, "source-bucket", "file.txt")

	if _, err := client.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket: aws.String("target-bucket"), Key: aws.String("moved.txt"),
		CopySource: aws.String("source-bucket/file.txt"),
	}); err != nil {
		t.Fatalf("CopyObject across buckets: %v", err)
	}

	if _, err := client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String("target-bucket"), Key: aws.String("moved.txt"),
	}); err != nil {
		t.Fatalf("the copy is not readable in the target bucket: %v", err)
	}
}

func TestCopyObjectReplacingMetadata(t *testing.T) {
	client, _ := newIntegrationServer(t)
	ctx := t.Context()
	makeBucket(t, client, "copies")

	if _, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("copies"), Key: aws.String("original.txt"),
		Body: strings.NewReader("x"), ContentType: aws.String("text/plain"),
		Metadata: map[string]string{"stage": "draft"},
	}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	if _, err := client.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket: aws.String("copies"), Key: aws.String("published.txt"),
		CopySource:        aws.String("copies/original.txt"),
		MetadataDirective: types.MetadataDirectiveReplace,
		ContentType:       aws.String("application/json"),
		Metadata:          map[string]string{"stage": "final"},
	}); err != nil {
		t.Fatalf("CopyObject with REPLACE: %v", err)
	}

	head, err := client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String("copies"), Key: aws.String("published.txt"),
	})
	if err != nil {
		t.Fatalf("HeadObject: %v", err)
	}
	if got := head.Metadata["stage"]; got != "final" {
		t.Errorf("metadata stage = %q, want final", got)
	}
	if ct := aws.ToString(head.ContentType); ct != "application/json" {
		t.Errorf("ContentType = %q, want application/json", ct)
	}
}

// Copying an object onto itself without changing anything is almost always a
// mistake, and S3 rejects it rather than silently doing nothing.
func TestCopyObjectOntoItself(t *testing.T) {
	client, _ := newIntegrationServer(t)
	makeBucket(t, client, "copies")
	seedKeys(t, client, "copies", "same.txt")

	_, err := client.CopyObject(t.Context(), &s3.CopyObjectInput{
		Bucket: aws.String("copies"), Key: aws.String("same.txt"),
		CopySource: aws.String("copies/same.txt"),
	})
	if err == nil {
		t.Fatal("copying an object onto itself with no change was accepted")
	}
	if code := apiErrorCode(err); code != "InvalidArgument" {
		t.Errorf("error code = %q, want InvalidArgument", code)
	}
}

func TestCopyObjectFromMissingSource(t *testing.T) {
	client, _ := newIntegrationServer(t)
	makeBucket(t, client, "copies")

	_, err := client.CopyObject(t.Context(), &s3.CopyObjectInput{
		Bucket: aws.String("copies"), Key: aws.String("target.txt"),
		CopySource: aws.String("copies/absent.txt"),
	})
	if err == nil {
		t.Fatal("CopyObject succeeded from a source that does not exist")
	}
	if code := apiErrorCode(err); code != "NoSuchKey" {
		t.Errorf("error code = %q, want NoSuchKey", code)
	}
}

// A key containing a slash or a space must survive the copy-source header,
// which is where percent-decoding order matters.
func TestCopyObjectWithAwkwardSourceKey(t *testing.T) {
	client, _ := newIntegrationServer(t)
	ctx := t.Context()
	makeBucket(t, client, "copies")

	const source = "deep/nested path/file with spaces.txt"
	seedKeys(t, client, "copies", source)

	if _, err := client.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket: aws.String("copies"), Key: aws.String("copied.txt"),
		CopySource: aws.String("copies/" + source),
	}); err != nil {
		t.Fatalf("CopyObject with an awkward source key: %v", err)
	}
	if _, err := client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String("copies"), Key: aws.String("copied.txt"),
	}); err != nil {
		t.Fatalf("the copy is missing: %v", err)
	}
}

// Conditional reads are how a client avoids re-downloading something it already
// has.
func TestConditionalGet(t *testing.T) {
	client, _ := newIntegrationServer(t)
	ctx := t.Context()
	makeBucket(t, client, "conditional")

	put, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("conditional"), Key: aws.String("doc.txt"),
		Body: strings.NewReader("contents"),
	})
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	etag := aws.ToString(put.ETag)

	// If-None-Match with the current ETag means "I already have this".
	_, err = client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String("conditional"), Key: aws.String("doc.txt"),
		IfNoneMatch: aws.String(etag),
	})
	if err == nil {
		t.Error("If-None-Match with the current ETag returned a body; want 304 Not Modified")
	} else if status := httpStatusOf(err); status != http.StatusNotModified {
		t.Errorf("If-None-Match status = %d, want %d", status, http.StatusNotModified)
	}

	// A different ETag means the client's copy is stale, so serve it.
	out, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String("conditional"), Key: aws.String("doc.txt"),
		IfNoneMatch: aws.String(`"00000000000000000000000000000000"`),
	})
	if err != nil {
		t.Fatalf("If-None-Match with a stale ETag should serve the object: %v", err)
	}
	out.Body.Close()

	// If-Match with the current ETag proceeds.
	out, err = client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String("conditional"), Key: aws.String("doc.txt"),
		IfMatch: aws.String(etag),
	})
	if err != nil {
		t.Fatalf("If-Match with the current ETag should serve the object: %v", err)
	}
	out.Body.Close()

	// If-Match with a different ETag means the object changed underneath.
	_, err = client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String("conditional"), Key: aws.String("doc.txt"),
		IfMatch: aws.String(`"00000000000000000000000000000000"`),
	})
	if err == nil {
		t.Error("If-Match with a mismatched ETag was served; want 412 Precondition Failed")
	} else if status := httpStatusOf(err); status != http.StatusPreconditionFailed {
		t.Errorf("If-Match status = %d, want %d", status, http.StatusPreconditionFailed)
	}
}

func TestConditionalGetByModificationTime(t *testing.T) {
	client, _ := newIntegrationServer(t)
	ctx := t.Context()
	makeBucket(t, client, "conditional")
	seedKeys(t, client, "conditional", "doc.txt")

	head, err := client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String("conditional"), Key: aws.String("doc.txt"),
	})
	if err != nil {
		t.Fatalf("HeadObject: %v", err)
	}
	lastModified := aws.ToTime(head.LastModified)

	// Not modified since its own timestamp.
	_, err = client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String("conditional"), Key: aws.String("doc.txt"),
		IfModifiedSince: aws.Time(lastModified),
	})
	if err == nil {
		t.Error("If-Modified-Since at the object's own timestamp returned a body; want 304")
	} else if status := httpStatusOf(err); status != http.StatusNotModified {
		t.Errorf("If-Modified-Since status = %d, want %d", status, http.StatusNotModified)
	}

	// Modified since an hour earlier.
	out, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String("conditional"), Key: aws.String("doc.txt"),
		IfModifiedSince: aws.Time(lastModified.Add(-time.Hour)),
	})
	if err != nil {
		t.Fatalf("If-Modified-Since an hour ago should serve the object: %v", err)
	}
	out.Body.Close()
}

// The wildcard means "if the object exists at all", which is how a client
// writes only-if-absent or only-if-present logic.
func TestEtagWildcardMatching(t *testing.T) {
	cases := []struct {
		header, candidate string
		allowWeak, want   bool
	}{
		{`*`, `"abc"`, false, true},
		{`"abc"`, `"abc"`, false, true},
		{`"abc"`, `"def"`, false, false},
		{`"abc", "def"`, `"def"`, false, true},
		{`W/"abc"`, `"abc"`, true, true},
		// A weak ETag asserts semantic equivalence, not byte equality, so it is
		// not strong enough to make a write safe.
		{`W/"abc"`, `"abc"`, false, false},
	}
	for _, tc := range cases {
		if got := etagMatches(tc.header, tc.candidate, tc.allowWeak); got != tc.want {
			t.Errorf("etagMatches(%q, %q, weak=%v) = %v, want %v",
				tc.header, tc.candidate, tc.allowWeak, got, tc.want)
		}
	}
}

func TestParseCopySource(t *testing.T) {
	cases := map[string]struct{ bucket, key, version string }{
		"/bucket/key.txt":             {"bucket", "key.txt", ""},
		"bucket/key.txt":              {"bucket", "key.txt", ""},
		"/bucket/deep/nested/key.txt": {"bucket", "deep/nested/key.txt", ""},
		"/bucket/with%20space.txt":    {"bucket", "with space.txt", ""},
		// The version id is no longer discarded: it names which version to copy,
		// which is how a version is restored through the API.
		"/bucket/key.txt?versionId=x": {"bucket", "key.txt", "x"},
	}
	for raw, want := range cases {
		bucket, key, version, err := parseCopySource(raw)
		if err != nil {
			t.Errorf("parseCopySource(%q) = error %v", raw, err)
			continue
		}
		if bucket != want.bucket || key != want.key || version != want.version {
			t.Errorf("parseCopySource(%q) = %q, %q, %q; want %q, %q, %q",
				raw, bucket, key, version, want.bucket, want.key, want.version)
		}
	}

	for _, bad := range []string{"", "/", "bucket", "/bucket/", "/bucket"} {
		if _, _, _, err := parseCopySource(bad); err == nil {
			t.Errorf("parseCopySource(%q) accepted a malformed source", bad)
		}
	}
}

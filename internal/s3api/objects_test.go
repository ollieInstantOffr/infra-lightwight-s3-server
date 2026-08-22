package s3api

import (
	"bytes"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func makeBucket(t *testing.T, client *s3.Client, name string) {
	t.Helper()
	if _, err := client.CreateBucket(t.Context(), &s3.CreateBucketInput{
		Bucket: aws.String(name),
	}); err != nil {
		t.Fatalf("CreateBucket(%q): %v", name, err)
	}
}

func TestObjectRoundTrip(t *testing.T) {
	client, _ := newIntegrationServer(t)
	ctx := t.Context()
	makeBucket(t, client, "objects")

	payload := []byte("the contents of the object, which must survive exactly")

	put, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String("objects"),
		Key:         aws.String("dir/sub/file.txt"),
		Body:        bytes.NewReader(payload),
		ContentType: aws.String("text/plain"),
	})
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	// The ETag must be the MD5 of the contents, quoted, because that is what
	// clients compare against to detect a corrupted transfer.
	wantETag := md5.Sum(payload)
	if got := strings.Trim(aws.ToString(put.ETag), `"`); got != hex.EncodeToString(wantETag[:]) {
		t.Errorf("ETag = %s, want %s", got, hex.EncodeToString(wantETag[:]))
	}

	get, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String("objects"),
		Key:    aws.String("dir/sub/file.txt"),
	})
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	defer get.Body.Close()

	body, err := io.ReadAll(get.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !bytes.Equal(body, payload) {
		t.Errorf("body = %q, want %q", body, payload)
	}
	if ct := aws.ToString(get.ContentType); ct != "text/plain" {
		t.Errorf("ContentType = %q, want text/plain", ct)
	}
	if aws.ToInt64(get.ContentLength) != int64(len(payload)) {
		t.Errorf("ContentLength = %d, want %d", aws.ToInt64(get.ContentLength), len(payload))
	}
	if get.LastModified == nil {
		t.Error("LastModified is nil")
	}
}

func TestHeadObject(t *testing.T) {
	client, _ := newIntegrationServer(t)
	ctx := t.Context()
	makeBucket(t, client, "objects")

	payload := bytes.Repeat([]byte("x"), 5000)
	if _, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("objects"), Key: aws.String("sized.bin"),
		Body: bytes.NewReader(payload),
	}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	head, err := client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String("objects"), Key: aws.String("sized.bin"),
	})
	if err != nil {
		t.Fatalf("HeadObject: %v", err)
	}
	if aws.ToInt64(head.ContentLength) != int64(len(payload)) {
		t.Errorf("ContentLength = %d, want %d", aws.ToInt64(head.ContentLength), len(payload))
	}
	if head.ETag == nil {
		t.Error("ETag missing from HEAD response")
	}
}

func TestUserMetadataRoundTrips(t *testing.T) {
	client, _ := newIntegrationServer(t)
	ctx := t.Context()
	makeBucket(t, client, "objects")

	metadata := map[string]string{
		"author":     "ollie",
		"project-id": "ILS-7",
	}
	if _, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("objects"), Key: aws.String("annotated.txt"),
		Body: strings.NewReader("body"), Metadata: metadata,
	}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	head, err := client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String("objects"), Key: aws.String("annotated.txt"),
	})
	if err != nil {
		t.Fatalf("HeadObject: %v", err)
	}
	for name, want := range metadata {
		if got := head.Metadata[name]; got != want {
			t.Errorf("metadata[%q] = %q, want %q (got map %v)", name, got, want, head.Metadata)
		}
	}
}

// Overwriting must replace the contents and the metadata, and must not leave
// the old blob referenced.
func TestOverwriteObject(t *testing.T) {
	client, pool := newIntegrationServer(t)
	ctx := t.Context()
	makeBucket(t, client, "objects")

	for _, body := range []string{"first version", "second version, longer"} {
		if _, err := client.PutObject(ctx, &s3.PutObjectInput{
			Bucket: aws.String("objects"), Key: aws.String("overwritten.txt"),
			Body: strings.NewReader(body),
		}); err != nil {
			t.Fatalf("PutObject(%q): %v", body, err)
		}
	}

	get, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String("objects"), Key: aws.String("overwritten.txt"),
	})
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	defer get.Body.Close()
	body, _ := io.ReadAll(get.Body)
	if string(body) != "second version, longer" {
		t.Errorf("body = %q, want the second version", body)
	}

	// Exactly one blob should still be referenced; the first version's must
	// have dropped to zero and become collectable.
	var referenced int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM blobs WHERE refcount > 0`).Scan(&referenced); err != nil {
		t.Fatalf("count referenced blobs: %v", err)
	}
	if referenced != 1 {
		t.Errorf("%d blobs still referenced after an overwrite, want 1", referenced)
	}
}

// Two objects with identical bytes share one blob, and deleting one must not
// destroy the other's data.
func TestIdenticalObjectsShareOneBlob(t *testing.T) {
	client, pool := newIntegrationServer(t)
	ctx := t.Context()
	makeBucket(t, client, "objects")

	const shared = "identical contents in two places"
	for _, key := range []string{"copy-a.txt", "copy-b.txt"} {
		if _, err := client.PutObject(ctx, &s3.PutObjectInput{
			Bucket: aws.String("objects"), Key: aws.String(key),
			Body: strings.NewReader(shared),
		}); err != nil {
			t.Fatalf("PutObject(%q): %v", key, err)
		}
	}

	var blobs, refcount int
	if err := pool.QueryRow(ctx, `SELECT count(*), coalesce(max(refcount), 0) FROM blobs`).
		Scan(&blobs, &refcount); err != nil {
		t.Fatalf("count blobs: %v", err)
	}
	if blobs != 1 || refcount != 2 {
		t.Fatalf("got %d blobs with max refcount %d, want 1 blob referenced twice", blobs, refcount)
	}

	if _, err := client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String("objects"), Key: aws.String("copy-a.txt"),
	}); err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}

	get, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String("objects"), Key: aws.String("copy-b.txt"),
	})
	if err != nil {
		t.Fatalf("the surviving object became unreadable after its twin was deleted: %v", err)
	}
	defer get.Body.Close()
	body, _ := io.ReadAll(get.Body)
	if string(body) != shared {
		t.Errorf("surviving object body = %q, want %q", body, shared)
	}
}

func TestDeleteObjectIsIdempotent(t *testing.T) {
	client, _ := newIntegrationServer(t)
	ctx := t.Context()
	makeBucket(t, client, "objects")

	if _, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("objects"), Key: aws.String("doomed.txt"),
		Body: strings.NewReader("x"),
	}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	// Deleting twice, and deleting something that never existed, must both
	// succeed — clients depend on this when cleaning up.
	for _, key := range []string{"doomed.txt", "doomed.txt", "never-existed.txt"} {
		if _, err := client.DeleteObject(ctx, &s3.DeleteObjectInput{
			Bucket: aws.String("objects"), Key: aws.String(key),
		}); err != nil {
			t.Errorf("DeleteObject(%q): %v", key, err)
		}
	}
}

func TestGetMissingObject(t *testing.T) {
	client, _ := newIntegrationServer(t)
	makeBucket(t, client, "objects")

	_, err := client.GetObject(t.Context(), &s3.GetObjectInput{
		Bucket: aws.String("objects"), Key: aws.String("absent.txt"),
	})
	if err == nil {
		t.Fatal("GetObject succeeded for a key that does not exist")
	}
	if code := apiErrorCode(err); code != "NoSuchKey" {
		t.Errorf("error code = %q, want NoSuchKey", code)
	}
}

func TestPutObjectIntoMissingBucket(t *testing.T) {
	client, _ := newIntegrationServer(t)

	_, err := client.PutObject(t.Context(), &s3.PutObjectInput{
		Bucket: aws.String("no-such-bucket"), Key: aws.String("k.txt"),
		Body: strings.NewReader("x"),
	})
	if err == nil {
		t.Fatal("PutObject succeeded into a bucket that does not exist")
	}
	if code := apiErrorCode(err); code != "NoSuchBucket" {
		t.Errorf("error code = %q, want NoSuchBucket", code)
	}
}

// Keys are opaque strings. Anything the client sends must come back exactly,
// which is where percent-encoding and unicode handling get tested for real.
func TestAwkwardKeysRoundTrip(t *testing.T) {
	client, _ := newIntegrationServer(t)
	ctx := t.Context()
	makeBucket(t, client, "objects")

	keys := []string{
		"simple.txt",
		"with space.txt",
		"with+plus.txt",
		"with%20literal-percent.txt",
		"nested/deeply/inside/a/prefix.txt",
		"unicode-ünïcødé-文字-🎉.txt",
		"with$dollar&ampersand=equals,comma.txt",
		"trailing-slash-in-key/",
		"dots/../not-normalised.txt",
	}

	for _, key := range keys {
		t.Run(key, func(t *testing.T) {
			want := []byte("contents for " + key)
			if _, err := client.PutObject(ctx, &s3.PutObjectInput{
				Bucket: aws.String("objects"), Key: aws.String(key),
				Body: bytes.NewReader(want),
			}); err != nil {
				t.Fatalf("PutObject: %v", err)
			}

			get, err := client.GetObject(ctx, &s3.GetObjectInput{
				Bucket: aws.String("objects"), Key: aws.String(key),
			})
			if err != nil {
				t.Fatalf("GetObject: %v", err)
			}
			defer get.Body.Close()
			body, _ := io.ReadAll(get.Body)
			if !bytes.Equal(body, want) {
				t.Errorf("body = %q, want %q", body, want)
			}
		})
	}
}

// Range requests are what make video streaming and resumable downloads work.
// They come free from http.ServeContent, but only because the blob store hands
// back a seekable file rather than a stream.
func TestRangeRequest(t *testing.T) {
	client, _ := newIntegrationServer(t)
	ctx := t.Context()
	makeBucket(t, client, "objects")

	payload := []byte("0123456789abcdefghijklmnopqrstuvwxyz")
	if _, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("objects"), Key: aws.String("ranged.txt"),
		Body: bytes.NewReader(payload),
	}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	get, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String("objects"), Key: aws.String("ranged.txt"),
		Range: aws.String("bytes=10-19"),
	})
	if err != nil {
		t.Fatalf("GetObject with Range: %v", err)
	}
	defer get.Body.Close()

	body, _ := io.ReadAll(get.Body)
	if string(body) != "abcdefghij" {
		t.Errorf("range body = %q, want %q", body, "abcdefghij")
	}
}

// A large object must round-trip byte for byte, which is the property that
// matters most and the one that silently breaks when framing is mishandled.
func TestLargeObjectRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping large object round trip in -short mode")
	}
	client, _ := newIntegrationServer(t)
	ctx := t.Context()
	makeBucket(t, client, "objects")

	payload := make([]byte, 16<<20) // 16 MiB
	if _, err := rand.Read(payload); err != nil {
		t.Fatalf("generate payload: %v", err)
	}
	want := md5.Sum(payload)

	if _, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("objects"), Key: aws.String("large.bin"),
		Body: bytes.NewReader(payload),
	}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	get, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String("objects"), Key: aws.String("large.bin"),
	})
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	defer get.Body.Close()

	body, err := io.ReadAll(get.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if got := md5.Sum(body); got != want {
		t.Errorf("checksum mismatch: got %x, want %x (%d bytes vs %d)",
			got, want, len(body), len(payload))
	}
}

// Concurrent writes to the same key must not leak blob references. Without
// serialisation both writers see no existing row, so neither releases the
// other's blob and the loser's data is stranded on disk forever.
func TestConcurrentPutsToSameKeyDoNotLeakBlobs(t *testing.T) {
	client, pool := newIntegrationServer(t)
	ctx := t.Context()
	makeBucket(t, client, "objects")

	const writers = 8
	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body := strings.Repeat("version-"+string(rune('a'+i)), 100)
			_, _ = client.PutObject(ctx, &s3.PutObjectInput{
				Bucket: aws.String("objects"), Key: aws.String("contended.txt"),
				Body: strings.NewReader(body),
			})
		}()
	}
	wg.Wait()

	var referenced int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM blobs WHERE refcount > 0`).Scan(&referenced); err != nil {
		t.Fatalf("count referenced blobs: %v", err)
	}
	if referenced != 1 {
		t.Errorf("%d blobs still referenced after %d concurrent writes to one key, want 1; "+
			"the surplus are leaked and will never be collected", referenced, writers)
	}
}

// Deleting a bucket that still holds objects must fail with BucketNotEmpty
// rather than silently cascading the objects away.
func TestDeleteNonEmptyBucket(t *testing.T) {
	client, _ := newIntegrationServer(t)
	ctx := t.Context()
	makeBucket(t, client, "occupied")

	if _, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("occupied"), Key: aws.String("resident.txt"),
		Body: strings.NewReader("still here"),
	}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	_, err := client.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String("occupied")})
	if err == nil {
		t.Fatal("DeleteBucket removed a bucket that still had objects in it")
	}
	if code := apiErrorCode(err); code != "BucketNotEmpty" {
		t.Errorf("error code = %q, want BucketNotEmpty", code)
	}
}

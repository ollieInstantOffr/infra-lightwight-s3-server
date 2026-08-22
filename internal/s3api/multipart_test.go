package s3api

import (
	"bytes"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// The SDK's upload manager is what actually drives multipart in the field:
// callers hand it a stream and it decides on part sizes and concurrency. Using
// it here means the tests exercise the same sequence a real client produces.
func TestMultipartUploadViaManager(t *testing.T) {
	client, _ := newIntegrationServer(t)
	ctx := t.Context()
	makeBucket(t, client, "multipart")

	payload := make([]byte, 26<<20) // 26 MiB, comfortably over the 8 MiB threshold
	if _, err := rand.Read(payload); err != nil {
		t.Fatalf("generate payload: %v", err)
	}
	want := md5.Sum(payload)

	uploader := manager.NewUploader(client, func(u *manager.Uploader) {
		u.PartSize = 5 << 20
		u.Concurrency = 3
	})
	if _, err := uploader.Upload(ctx, &s3.PutObjectInput{
		Bucket: aws.String("multipart"), Key: aws.String("big/object.bin"),
		Body: bytes.NewReader(payload),
	}); err != nil {
		t.Fatalf("multipart upload: %v", err)
	}

	get, err := client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String("multipart"), Key: aws.String("big/object.bin"),
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
		t.Fatalf("checksum mismatch after multipart round trip: got %x want %x (%d vs %d bytes)",
			got, want, len(body), len(payload))
	}

	// A multipart object's ETag is the composite form, not a plain MD5.
	etag := unquoteETag(aws.ToString(get.ETag))
	if !strings.Contains(etag, "-") {
		t.Errorf("ETag = %q, want the composite <md5>-<partcount> form", etag)
	}
}

// The composite ETag is the MD5 of the concatenated raw part digests, suffixed
// with the part count. Concatenating the hex text instead produces a plausible
// value no S3 client agrees with.
func TestCompositeETagMatchesS3Rule(t *testing.T) {
	client, _ := newIntegrationServer(t)
	ctx := t.Context()
	makeBucket(t, client, "multipart")

	parts := [][]byte{
		bytes.Repeat([]byte("a"), 5<<20),
		bytes.Repeat([]byte("b"), 5<<20),
		bytes.Repeat([]byte("c"), 1024),
	}

	created, err := client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String("multipart"), Key: aws.String("manual.bin"),
	})
	if err != nil {
		t.Fatalf("CreateMultipartUpload: %v", err)
	}

	var completed []types.CompletedPart
	var rawDigests []byte
	for i, part := range parts {
		out, err := client.UploadPart(ctx, &s3.UploadPartInput{
			Bucket: aws.String("multipart"), Key: aws.String("manual.bin"),
			UploadId: created.UploadId, PartNumber: aws.Int32(int32(i + 1)),
			Body: bytes.NewReader(part),
		})
		if err != nil {
			t.Fatalf("UploadPart %d: %v", i+1, err)
		}
		// Each part's ETag must be the plain MD5 of that part.
		wantPart := md5.Sum(part)
		if got := unquoteETag(aws.ToString(out.ETag)); got != hex.EncodeToString(wantPart[:]) {
			t.Errorf("part %d ETag = %s, want %s", i+1, got, hex.EncodeToString(wantPart[:]))
		}
		rawDigests = append(rawDigests, wantPart[:]...)
		completed = append(completed, types.CompletedPart{
			ETag: out.ETag, PartNumber: aws.Int32(int32(i + 1)),
		})
	}

	done, err := client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: aws.String("multipart"), Key: aws.String("manual.bin"),
		UploadId:        created.UploadId,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: completed},
	})
	if err != nil {
		t.Fatalf("CompleteMultipartUpload: %v", err)
	}

	sum := md5.Sum(rawDigests)
	want := fmt.Sprintf("%s-%d", hex.EncodeToString(sum[:]), len(parts))
	if got := unquoteETag(aws.ToString(done.ETag)); got != want {
		t.Errorf("composite ETag = %s, want %s", got, want)
	}
}

func TestListParts(t *testing.T) {
	client, _ := newIntegrationServer(t)
	ctx := t.Context()
	makeBucket(t, client, "multipart")

	created, err := client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String("multipart"), Key: aws.String("listed.bin"),
	})
	if err != nil {
		t.Fatalf("CreateMultipartUpload: %v", err)
	}
	for i := range 3 {
		if _, err := client.UploadPart(ctx, &s3.UploadPartInput{
			Bucket: aws.String("multipart"), Key: aws.String("listed.bin"),
			UploadId: created.UploadId, PartNumber: aws.Int32(int32(i + 1)),
			Body: bytes.NewReader(bytes.Repeat([]byte("x"), 5<<20)),
		}); err != nil {
			t.Fatalf("UploadPart %d: %v", i+1, err)
		}
	}

	out, err := client.ListParts(ctx, &s3.ListPartsInput{
		Bucket: aws.String("multipart"), Key: aws.String("listed.bin"),
		UploadId: created.UploadId,
	})
	if err != nil {
		t.Fatalf("ListParts: %v", err)
	}
	if len(out.Parts) != 3 {
		t.Fatalf("got %d parts, want 3", len(out.Parts))
	}
	for i, part := range out.Parts {
		if aws.ToInt32(part.PartNumber) != int32(i+1) {
			t.Errorf("part %d has number %d; parts must be listed in ascending order",
				i, aws.ToInt32(part.PartNumber))
		}
	}
}

func TestListMultipartUploads(t *testing.T) {
	client, _ := newIntegrationServer(t)
	ctx := t.Context()
	makeBucket(t, client, "multipart")

	for _, key := range []string{"a.bin", "b.bin"} {
		if _, err := client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
			Bucket: aws.String("multipart"), Key: aws.String(key),
		}); err != nil {
			t.Fatalf("CreateMultipartUpload(%q): %v", key, err)
		}
	}

	out, err := client.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{
		Bucket: aws.String("multipart"),
	})
	if err != nil {
		t.Fatalf("ListMultipartUploads: %v", err)
	}
	if len(out.Uploads) != 2 {
		t.Errorf("got %d in-progress uploads, want 2", len(out.Uploads))
	}
}

// Aborting must release every part's blob reference, or an abandoned upload
// leaves its data on disk forever.
func TestAbortMultipartUploadReleasesParts(t *testing.T) {
	client, pool := newIntegrationServer(t)
	ctx := t.Context()
	makeBucket(t, client, "multipart")

	created, err := client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String("multipart"), Key: aws.String("abandoned.bin"),
	})
	if err != nil {
		t.Fatalf("CreateMultipartUpload: %v", err)
	}
	for i := range 2 {
		if _, err := client.UploadPart(ctx, &s3.UploadPartInput{
			Bucket: aws.String("multipart"), Key: aws.String("abandoned.bin"),
			UploadId: created.UploadId, PartNumber: aws.Int32(int32(i + 1)),
			Body: bytes.NewReader(bytes.Repeat([]byte{byte(i)}, 5<<20)),
		}); err != nil {
			t.Fatalf("UploadPart: %v", err)
		}
	}

	var before int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM blobs WHERE refcount > 0`).Scan(&before); err != nil {
		t.Fatalf("count blobs: %v", err)
	}
	if before != 2 {
		t.Fatalf("%d blobs referenced before abort, want 2", before)
	}

	if _, err := client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
		Bucket: aws.String("multipart"), Key: aws.String("abandoned.bin"),
		UploadId: created.UploadId,
	}); err != nil {
		t.Fatalf("AbortMultipartUpload: %v", err)
	}

	var after int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM blobs WHERE refcount > 0`).Scan(&after); err != nil {
		t.Fatalf("count blobs: %v", err)
	}
	if after != 0 {
		t.Errorf("%d blobs still referenced after abort, want 0; the parts are stranded on disk", after)
	}

	// The upload must be gone, not merely emptied.
	if _, err := client.ListParts(ctx, &s3.ListPartsInput{
		Bucket: aws.String("multipart"), Key: aws.String("abandoned.bin"),
		UploadId: created.UploadId,
	}); err == nil {
		t.Error("ListParts succeeded on an aborted upload")
	}
}

// Completing must also release the part blobs, keeping only the assembled one.
func TestCompleteReleasesPartBlobs(t *testing.T) {
	client, pool := newIntegrationServer(t)
	ctx := t.Context()
	makeBucket(t, client, "multipart")

	created, _ := client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String("multipart"), Key: aws.String("assembled.bin"),
	})
	var completed []types.CompletedPart
	for i := range 2 {
		out, err := client.UploadPart(ctx, &s3.UploadPartInput{
			Bucket: aws.String("multipart"), Key: aws.String("assembled.bin"),
			UploadId: created.UploadId, PartNumber: aws.Int32(int32(i + 1)),
			Body: bytes.NewReader(bytes.Repeat([]byte{byte('a' + i)}, 5<<20)),
		})
		if err != nil {
			t.Fatalf("UploadPart: %v", err)
		}
		completed = append(completed, types.CompletedPart{
			ETag: out.ETag, PartNumber: aws.Int32(int32(i + 1)),
		})
	}
	if _, err := client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: aws.String("multipart"), Key: aws.String("assembled.bin"),
		UploadId:        created.UploadId,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: completed},
	}); err != nil {
		t.Fatalf("CompleteMultipartUpload: %v", err)
	}

	var referenced int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM blobs WHERE refcount > 0`).Scan(&referenced); err != nil {
		t.Fatalf("count blobs: %v", err)
	}
	if referenced != 1 {
		t.Errorf("%d blobs referenced after completion, want 1 (the assembled object)", referenced)
	}
}

func TestCompleteWithMissingPart(t *testing.T) {
	client, _ := newIntegrationServer(t)
	ctx := t.Context()
	makeBucket(t, client, "multipart")

	created, _ := client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String("multipart"), Key: aws.String("incomplete.bin"),
	})
	out, err := client.UploadPart(ctx, &s3.UploadPartInput{
		Bucket: aws.String("multipart"), Key: aws.String("incomplete.bin"),
		UploadId: created.UploadId, PartNumber: aws.Int32(1),
		Body: bytes.NewReader(bytes.Repeat([]byte("x"), 5<<20)),
	})
	if err != nil {
		t.Fatalf("UploadPart: %v", err)
	}

	_, err = client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: aws.String("multipart"), Key: aws.String("incomplete.bin"),
		UploadId: created.UploadId,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: []types.CompletedPart{
			{ETag: out.ETag, PartNumber: aws.Int32(1)},
			{ETag: aws.String(`"00000000000000000000000000000000"`), PartNumber: aws.Int32(2)},
		}},
	})
	if err == nil {
		t.Fatal("completion succeeded while referencing a part that was never uploaded")
	}
	if code := apiErrorCode(err); code != "InvalidPart" {
		t.Errorf("error code = %q, want InvalidPart", code)
	}
}

func TestCompleteWithMismatchedETag(t *testing.T) {
	client, _ := newIntegrationServer(t)
	ctx := t.Context()
	makeBucket(t, client, "multipart")

	created, _ := client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String("multipart"), Key: aws.String("wrong-etag.bin"),
	})
	if _, err := client.UploadPart(ctx, &s3.UploadPartInput{
		Bucket: aws.String("multipart"), Key: aws.String("wrong-etag.bin"),
		UploadId: created.UploadId, PartNumber: aws.Int32(1),
		Body: bytes.NewReader([]byte("small")),
	}); err != nil {
		t.Fatalf("UploadPart: %v", err)
	}

	_, err := client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: aws.String("multipart"), Key: aws.String("wrong-etag.bin"),
		UploadId: created.UploadId,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: []types.CompletedPart{
			{ETag: aws.String(`"ffffffffffffffffffffffffffffffff"`), PartNumber: aws.Int32(1)},
		}},
	})
	if err == nil {
		t.Fatal("completion succeeded with an ETag that does not match the uploaded part")
	}
	if code := apiErrorCode(err); code != "InvalidPart" {
		t.Errorf("error code = %q, want InvalidPart", code)
	}
}

// Every part but the last must meet S3's 5 MiB minimum, so an object assembled
// here has a part layout real S3 would also accept.
func TestCompleteRejectsUndersizedPart(t *testing.T) {
	client, _ := newIntegrationServer(t)
	ctx := t.Context()
	makeBucket(t, client, "multipart")

	created, _ := client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String("multipart"), Key: aws.String("tiny-parts.bin"),
	})
	var completed []types.CompletedPart
	for i := range 2 {
		out, err := client.UploadPart(ctx, &s3.UploadPartInput{
			Bucket: aws.String("multipart"), Key: aws.String("tiny-parts.bin"),
			UploadId: created.UploadId, PartNumber: aws.Int32(int32(i + 1)),
			Body: strings.NewReader(fmt.Sprintf("part %d, far too small", i+1)),
		})
		if err != nil {
			t.Fatalf("UploadPart: %v", err)
		}
		completed = append(completed, types.CompletedPart{
			ETag: out.ETag, PartNumber: aws.Int32(int32(i + 1)),
		})
	}

	_, err := client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: aws.String("multipart"), Key: aws.String("tiny-parts.bin"),
		UploadId:        created.UploadId,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: completed},
	})
	if err == nil {
		t.Fatal("completion accepted a non-final part below the 5 MiB minimum")
	}
	if code := apiErrorCode(err); code != "EntityTooSmall" {
		t.Errorf("error code = %q, want EntityTooSmall", code)
	}
}

func TestOperationsOnUnknownUploadID(t *testing.T) {
	client, _ := newIntegrationServer(t)
	makeBucket(t, client, "multipart")

	_, err := client.ListParts(t.Context(), &s3.ListPartsInput{
		Bucket: aws.String("multipart"), Key: aws.String("k.bin"),
		UploadId: aws.String("deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"),
	})
	if err == nil {
		t.Fatal("ListParts succeeded for an unknown upload id")
	}
	if code := apiErrorCode(err); code != "NoSuchUpload" {
		t.Errorf("error code = %q, want NoSuchUpload", code)
	}
}

// Re-uploading a part replaces it, which is what a client does after a failed
// attempt. The superseded blob must not stay referenced.
func TestReuploadingPartReplacesIt(t *testing.T) {
	client, pool := newIntegrationServer(t)
	ctx := t.Context()
	makeBucket(t, client, "multipart")

	created, _ := client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String("multipart"), Key: aws.String("retried.bin"),
	})
	for _, filler := range []byte{'a', 'b'} {
		if _, err := client.UploadPart(ctx, &s3.UploadPartInput{
			Bucket: aws.String("multipart"), Key: aws.String("retried.bin"),
			UploadId: created.UploadId, PartNumber: aws.Int32(1),
			Body: bytes.NewReader(bytes.Repeat([]byte{filler}, 5<<20)),
		}); err != nil {
			t.Fatalf("UploadPart: %v", err)
		}
	}

	var referenced int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM blobs WHERE refcount > 0`).Scan(&referenced); err != nil {
		t.Fatalf("count blobs: %v", err)
	}
	if referenced != 1 {
		t.Errorf("%d blobs referenced after re-uploading one part, want 1", referenced)
	}

	parts, err := client.ListParts(ctx, &s3.ListPartsInput{
		Bucket: aws.String("multipart"), Key: aws.String("retried.bin"),
		UploadId: created.UploadId,
	})
	if err != nil {
		t.Fatalf("ListParts: %v", err)
	}
	if len(parts.Parts) != 1 {
		t.Errorf("got %d parts, want 1 (re-upload replaces rather than appends)", len(parts.Parts))
	}
}

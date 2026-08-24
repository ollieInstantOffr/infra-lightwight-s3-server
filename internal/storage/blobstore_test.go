package storage

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func TestPutGetRoundTrip(t *testing.T) {
	s := newTestStore(t)
	payload := []byte("the quick brown fox jumps over the lazy dog")

	blob, err := s.Put(context.Background(), bytes.NewReader(payload), "")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	wantDigest := sha256.Sum256(payload)
	if blob.Digest != hex.EncodeToString(wantDigest[:]) {
		t.Errorf("digest = %s, want %s", blob.Digest, hex.EncodeToString(wantDigest[:]))
	}
	wantETag := md5.Sum(payload)
	if blob.ETag != hex.EncodeToString(wantETag[:]) {
		t.Errorf("etag = %s, want %s", blob.ETag, hex.EncodeToString(wantETag[:]))
	}
	if blob.Size != int64(len(payload)) {
		t.Errorf("size = %d, want %d", blob.Size, len(payload))
	}

	f, err := s.Open(blob.Digest)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()
	got, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("round-trip mismatch: got %q, want %q", got, payload)
	}
}

// A caller that already verified the body's SHA-256 elsewhere (a signed PUT,
// checked against x-amz-content-sha256 as it streamed) passes that digest in
// rather than have Put hash the same bytes again. Proving Put actually trusts
// it — rather than quietly recomputing and overriding it — means passing a
// wrong-but-well-formed digest and checking the blob lands there instead of
// at the real content hash; if Put still computed its own hash internally,
// the digest actually used would be the correct one regardless of what was
// passed in, and this is the one observable difference that would catch that.
func TestPutTrustsAKnownDigestRatherThanRecomputingIt(t *testing.T) {
	s := newTestStore(t)
	payload := []byte("trust, but verify never happens on purpose")
	wrongDigest := strings.Repeat("ab", sha256.Size) // well-formed, not the real hash

	blob, err := s.Put(context.Background(), bytes.NewReader(payload), wrongDigest)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if blob.Digest != wrongDigest {
		t.Fatalf("digest = %s, want the passed-in %s (Put must not recompute it)", blob.Digest, wrongDigest)
	}

	f, err := s.Open(wrongDigest)
	if err != nil {
		t.Fatalf("Open(%s): %v", wrongDigest, err)
	}
	defer f.Close()
	got, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("content under the trusted digest = %q, want %q", got, payload)
	}

	// ETag has no other source, so it must still be computed regardless.
	wantETag := md5.Sum(payload)
	if blob.ETag != hex.EncodeToString(wantETag[:]) {
		t.Errorf("etag = %s, want %s (MD5 must still be computed even with a known digest)", blob.ETag, hex.EncodeToString(wantETag[:]))
	}
}

// The ordinary case: an honest known digest is what the content actually
// hashes to, and the round trip works exactly as it would without one.
func TestPutWithACorrectKnownDigestRoundTrips(t *testing.T) {
	s := newTestStore(t)
	payload := []byte("the quick brown fox jumps over the lazy dog")
	realDigest := sha256.Sum256(payload)
	digestHex := hex.EncodeToString(realDigest[:])

	blob, err := s.Put(context.Background(), bytes.NewReader(payload), digestHex)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if blob.Digest != digestHex {
		t.Fatalf("digest = %s, want %s", blob.Digest, digestHex)
	}

	f, err := s.Open(digestHex)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()
	got, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("round-trip mismatch: got %q, want %q", got, payload)
	}
}

// Range requests depend on the returned file being seekable rather than the
// server reading and discarding a prefix.
func TestOpenIsSeekable(t *testing.T) {
	s := newTestStore(t)
	payload := []byte("0123456789abcdef")

	blob, err := s.Put(context.Background(), bytes.NewReader(payload), "")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	f, err := s.Open(blob.Digest)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()

	if _, err := f.Seek(10, io.SeekStart); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	got, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("ReadAll after seek: %v", err)
	}
	if string(got) != "abcdef" {
		t.Errorf("after seek got %q, want %q", got, "abcdef")
	}
}

func TestPutDeduplicatesIdenticalContent(t *testing.T) {
	s := newTestStore(t)
	payload := []byte("identical bytes converge on one file")

	first, err := s.Put(context.Background(), bytes.NewReader(payload), "")
	if err != nil {
		t.Fatalf("first Put: %v", err)
	}
	second, err := s.Put(context.Background(), bytes.NewReader(payload), "")
	if err != nil {
		t.Fatalf("second Put: %v", err)
	}
	if first.Digest != second.Digest {
		t.Fatalf("digests differ: %s vs %s", first.Digest, second.Digest)
	}
	if n := countBlobFiles(t, s); n != 1 {
		t.Errorf("blob files on disk = %d, want 1", n)
	}
}

// Two objects sharing bytes must not delete each other's data. The store has no
// notion of references, so this asserts the property it does guarantee: a single
// file backs both, and removing it once leaves nothing behind.
func TestRemove(t *testing.T) {
	s := newTestStore(t)
	blob, err := s.Put(context.Background(), strings.NewReader("removable"), "")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	exists, err := s.Exists(blob.Digest)
	if err != nil || !exists {
		t.Fatalf("Exists before remove = %v, %v; want true, nil", exists, err)
	}
	if err := s.Remove(blob.Digest); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	exists, err = s.Exists(blob.Digest)
	if err != nil || exists {
		t.Fatalf("Exists after remove = %v, %v; want false, nil", exists, err)
	}
	// Removing again must succeed, so a retried cleanup is safe.
	if err := s.Remove(blob.Digest); err != nil {
		t.Errorf("second Remove: %v", err)
	}
}

func TestOpenMissingBlob(t *testing.T) {
	s := newTestStore(t)
	absent := strings.Repeat("ab", 32)
	if _, err := s.Open(absent); !errors.Is(err, ErrNotFound) {
		t.Errorf("Open(missing) error = %v, want ErrNotFound", err)
	}
	if _, err := s.Stat(absent); !errors.Is(err, ErrNotFound) {
		t.Errorf("Stat(missing) error = %v, want ErrNotFound", err)
	}
}

// A digest reaches the store from the database. A malformed or hostile value
// must never be usable to read or delete outside the blob root.
func TestDigestValidationRejectsTraversal(t *testing.T) {
	s := newTestStore(t)
	for _, bad := range []string{
		"",
		"../../../../etc/passwd",
		strings.Repeat("A", 64),  // uppercase
		strings.Repeat("ab", 31), // too short
		strings.Repeat("zz", 32), // not hex
	} {
		if _, err := s.Open(bad); err == nil || errors.Is(err, ErrNotFound) {
			t.Errorf("Open(%q) = %v, want a validation error", bad, err)
		}
		if err := s.Remove(bad); err == nil {
			t.Errorf("Remove(%q) succeeded, want a validation error", bad)
		}
	}
}

// Partial uploads left by a crash are unreferenced by definition, since a blob
// only becomes reachable once renamed into place.
func TestNewCleansStaleTempFiles(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	stale := filepath.Join(s.Root(), tempDir, "upload-crashed")
	if err := os.WriteFile(stale, []byte("half an object"), 0o600); err != nil {
		t.Fatalf("seed stale temp file: %v", err)
	}

	if _, err := New(dir); err != nil {
		t.Fatalf("re-open store: %v", err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stale temp file still present: %v", err)
	}
}

// A cancelled upload must stop promptly rather than streaming to completion,
// and must leave nothing behind.
func TestPutCancellation(t *testing.T) {
	s := newTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := s.Put(ctx, bytes.NewReader(make([]byte, 4<<20)), ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("Put with cancelled context = %v, want context.Canceled", err)
	}
	entries, err := os.ReadDir(filepath.Join(s.Root(), tempDir))
	if err != nil {
		t.Fatalf("read temp dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("cancelled upload left %d temp files behind", len(entries))
	}
}

func TestConcurrentPutsOfSameContent(t *testing.T) {
	s := newTestStore(t)
	payload := bytes.Repeat([]byte("race"), 1<<16)

	const writers = 16
	var wg sync.WaitGroup
	digests := make([]string, writers)
	errs := make([]error, writers)

	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			blob, err := s.Put(context.Background(), bytes.NewReader(payload), "")
			digests[i], errs[i] = blob.Digest, err
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("writer %d: %v", i, err)
		}
		if digests[i] != digests[0] {
			t.Fatalf("writer %d digest %s differs from %s", i, digests[i], digests[0])
		}
	}
	if n := countBlobFiles(t, s); n != 1 {
		t.Errorf("blob files on disk = %d, want 1", n)
	}
}

// The point of streaming: memory must not scale with object size. A store that
// buffered uploads would fail this outright.
func TestLargeUploadHoldsMemoryFlat(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping large streaming upload in -short mode")
	}
	s := newTestStore(t)

	const size = 512 << 20 // 512 MiB
	// Well above the 1 MiB copy buffer and any incidental allocation, but far
	// below the object size — a buffering implementation would blow past this.
	const maxHeapGrowth = 32 << 20

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	blob, err := s.Put(context.Background(), io.LimitReader(rand.Reader, size), "")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	if blob.Size != size {
		t.Errorf("size = %d, want %d", blob.Size, size)
	}
	if growth := int64(after.HeapAlloc) - int64(before.HeapAlloc); growth > maxHeapGrowth {
		t.Errorf("heap grew by %d bytes writing a %d byte object; want under %d",
			growth, size, maxHeapGrowth)
	}

	// The bytes must also be correct, not merely cheap to write.
	f, err := s.Open(blob.Digest)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if got := hex.EncodeToString(sum.Sum(nil)); got != blob.Digest {
		t.Errorf("re-read digest %s does not match written digest %s", got, blob.Digest)
	}
}

func TestUsageReportsVolumeSpace(t *testing.T) {
	s := newTestStore(t)
	usage, err := s.Usage()
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	if usage.TotalBytes == 0 {
		t.Error("TotalBytes = 0, want the size of the backing volume")
	}
	if usage.FreeBytes > usage.TotalBytes {
		t.Errorf("FreeBytes %d exceeds TotalBytes %d", usage.FreeBytes, usage.TotalBytes)
	}
}

func countBlobFiles(t *testing.T, s *Store) int {
	t.Helper()
	count := 0
	err := filepath.WalkDir(filepath.Join(s.Root(), blobsDir), func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			count++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk blobs: %v", err)
	}
	return count
}

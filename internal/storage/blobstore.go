// Package storage implements the content-addressed blob store that holds object
// bytes on local disk.
//
// Blobs are keyed by the SHA-256 of their contents, so two uploads of identical
// bytes converge on a single file. Reference counting lives in Postgres rather
// than here, because it must be transactional with the object metadata that
// creates and destroys those references; this package only owns the files.
//
// There is exactly one copy of every blob. No replication, no parity, no
// repair. That is a deliberate constraint of this server, not an omission.
package storage

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// ErrNotFound is returned when a digest has no file behind it.
var ErrNotFound = errors.New("blob not found")

const (
	blobsDir = "blobs"
	tempDir  = "tmp"

	// Fan out on the first two byte-pairs of the digest, giving 65,536 leaf
	// directories. Keeps any single directory small enough that listing and
	// lookup stay cheap even with millions of blobs.
	fanoutDepth = 2
	fanoutWidth = 2

	dirPerm  = 0o750
	filePerm = 0o640
)

// copyBufferSize bounds memory per in-flight transfer. Every read and write
// path reuses a buffer of this size, so a 5 GB upload costs the same resident
// memory as a 5 KB one.
const copyBufferSize = 1 << 20 // 1 MiB

// bufferPool recycles copy buffers across concurrent transfers rather than
// allocating a megabyte per request.
var bufferPool = sync.Pool{
	New: func() any {
		buf := make([]byte, copyBufferSize)
		return &buf
	},
}

// Store is a content-addressed blob store rooted at a directory on local disk.
type Store struct {
	root string
}

// Blob describes bytes that have been committed to the store.
type Blob struct {
	// Digest is the lowercase hex SHA-256 of the contents, and the store's key.
	Digest string
	// ETag is the lowercase hex MD5 of the contents. S3 clients compare against
	// this, so it is computed in the same pass rather than by re-reading.
	ETag string
	Size int64
}

// New prepares a store rooted at dir, creating the layout if needed and proving
// it is writable before any request depends on it.
func New(dir string) (*Store, error) {
	root, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve data dir %q: %w", dir, err)
	}
	for _, sub := range []string{blobsDir, tempDir} {
		if err := os.MkdirAll(filepath.Join(root, sub), dirPerm); err != nil {
			return nil, fmt.Errorf("create %s directory: %w", sub, err)
		}
	}
	s := &Store{root: root}
	// Any temp files left by a crash are dead weight; nothing references them.
	if err := s.cleanTemp(); err != nil {
		return nil, err
	}
	return s, nil
}

// Root returns the store's base directory.
func (s *Store) Root() string { return s.root }

// Put streams r into the store, hashing as it goes, and commits the result
// atomically. Nothing is buffered in memory beyond a fixed copy buffer, so
// object size does not affect memory use.
//
// If the contents already exist the temporary file is discarded and the
// existing blob is returned; callers cannot tell the difference, and should
// not need to.
func (s *Store) Put(ctx context.Context, r io.Reader) (Blob, error) {
	temp, err := os.CreateTemp(filepath.Join(s.root, tempDir), "upload-*")
	if err != nil {
		return Blob{}, fmt.Errorf("create temp file: %w", err)
	}
	tempName := temp.Name()

	// Until the rename succeeds, the temp file is ours to clean up. Both
	// cleanup paths are idempotent, so the deferred removal after a successful
	// rename is a harmless no-op.
	committed := false
	defer func() {
		temp.Close()
		if !committed {
			_ = os.Remove(tempName)
		}
	}()

	if err := os.Chmod(tempName, filePerm); err != nil {
		return Blob{}, fmt.Errorf("set temp file permissions: %w", err)
	}

	sha := sha256.New()
	sum := md5.New()
	// Hashing happens as part of the same copy that writes to disk; the bytes
	// are never read twice.
	written, err := copyWithContext(ctx, io.MultiWriter(temp, sha, sum), r)
	if err != nil {
		return Blob{}, err
	}

	// fsync before the rename, so a crash can never leave a correctly named
	// blob whose contents are still in the page cache.
	if err := temp.Sync(); err != nil {
		return Blob{}, fmt.Errorf("sync temp file: %w", err)
	}
	if err := temp.Close(); err != nil {
		return Blob{}, fmt.Errorf("close temp file: %w", err)
	}

	blob := Blob{
		Digest: hex.EncodeToString(sha.Sum(nil)),
		ETag:   hex.EncodeToString(sum.Sum(nil)),
		Size:   written,
	}

	final := s.pathFor(blob.Digest)
	if err := os.MkdirAll(filepath.Dir(final), dirPerm); err != nil {
		return Blob{}, fmt.Errorf("create blob directory: %w", err)
	}

	// A blob already at this digest has identical contents by construction, so
	// deduplicating is safe and the upload becomes a no-op on disk.
	if _, err := os.Stat(final); err == nil {
		return blob, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return Blob{}, fmt.Errorf("stat blob path: %w", err)
	}

	if err := os.Rename(tempName, final); err != nil {
		// A concurrent writer of the same content may have won the race. Same
		// digest means same bytes, so that outcome is a success, not a clash.
		if _, statErr := os.Stat(final); statErr == nil {
			return blob, nil
		}
		return Blob{}, fmt.Errorf("commit blob: %w", err)
	}
	committed = true

	// Fsync the parent directory so the rename itself survives a power loss;
	// syncing the file alone does not persist the directory entry.
	if err := syncDir(filepath.Dir(final)); err != nil {
		return Blob{}, err
	}
	return blob, nil
}

// Open returns the blob's contents. The returned file is an io.ReadSeekCloser,
// which is what lets HTTP Range requests seek directly instead of reading and
// discarding a prefix.
func (s *Store) Open(digest string) (*os.File, error) {
	if err := validateDigest(digest); err != nil {
		return nil, err
	}
	f, err := os.Open(s.pathFor(digest))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, digest)
	}
	if err != nil {
		return nil, fmt.Errorf("open blob %s: %w", digest, err)
	}
	return f, nil
}

// Stat reports a blob's size without opening it for reading.
func (s *Store) Stat(digest string) (int64, error) {
	if err := validateDigest(digest); err != nil {
		return 0, err
	}
	info, err := os.Stat(s.pathFor(digest))
	if errors.Is(err, os.ErrNotExist) {
		return 0, fmt.Errorf("%w: %s", ErrNotFound, digest)
	}
	if err != nil {
		return 0, fmt.Errorf("stat blob %s: %w", digest, err)
	}
	return info.Size(), nil
}

// Remove unlinks a blob. It is the caller's responsibility to have established,
// transactionally, that nothing references it any more — the store has no view
// of reference counts.
//
// Removing an absent blob succeeds, so a retried cleanup is not an error.
func (s *Store) Remove(digest string) error {
	if err := validateDigest(digest); err != nil {
		return err
	}
	if err := os.Remove(s.pathFor(digest)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove blob %s: %w", digest, err)
	}
	return nil
}

// Exists reports whether a blob is present on disk.
func (s *Store) Exists(digest string) (bool, error) {
	if err := validateDigest(digest); err != nil {
		return false, err
	}
	_, err := os.Stat(s.pathFor(digest))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, fmt.Errorf("stat blob %s: %w", digest, err)
}

// pathFor maps a digest to its file: blobs/ab/cd/abcd...
func (s *Store) pathFor(digest string) string {
	parts := make([]string, 0, fanoutDepth+2)
	parts = append(parts, s.root, blobsDir)
	for i := 0; i < fanoutDepth; i++ {
		parts = append(parts, digest[i*fanoutWidth:(i+1)*fanoutWidth])
	}
	return filepath.Join(append(parts, digest)...)
}

// cleanTemp discards partial uploads left behind by a crash. They are
// unreferenced by definition: a blob only becomes reachable once renamed.
func (s *Store) cleanTemp() error {
	dir := filepath.Join(s.root, tempDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read temp directory: %w", err)
	}
	for _, entry := range entries {
		if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove stale temp file %s: %w", entry.Name(), err)
		}
	}
	return nil
}

// copyWithContext is io.Copy with cancellation and a pooled buffer, so an
// aborted upload stops promptly instead of streaming to completion.
func copyWithContext(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	bufp := bufferPool.Get().(*[]byte)
	defer bufferPool.Put(bufp)
	buf := *bufp

	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, fmt.Errorf("upload cancelled after %d bytes: %w", total, err)
		}
		n, readErr := src.Read(buf)
		if n > 0 {
			written, writeErr := dst.Write(buf[:n])
			total += int64(written)
			if writeErr != nil {
				return total, fmt.Errorf("write blob data: %w", writeErr)
			}
			if written != n {
				return total, fmt.Errorf("write blob data: %w", io.ErrShortWrite)
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return total, nil
			}
			return total, fmt.Errorf("read upload body: %w", readErr)
		}
	}
}

// syncDir fsyncs a directory so a rename into it is durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open blob directory for sync: %w", err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("sync blob directory: %w", err)
	}
	return nil
}

// validateDigest rejects anything that is not a well-formed SHA-256 hex string.
// Digests reach this package from the database, but checking here means a
// corrupt or hostile value can never be used to escape the blob root.
func validateDigest(digest string) error {
	if len(digest) != sha256.Size*2 {
		return fmt.Errorf("invalid blob digest %q: expected %d hex characters, got %d",
			digest, sha256.Size*2, len(digest))
	}
	if strings.TrimLeft(digest, "0123456789abcdef") != "" {
		return fmt.Errorf("invalid blob digest %q: must be lowercase hex", digest)
	}
	return nil
}

package db

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// The behaviour these pin is the reason ILS-85 existed: S3's delete semantics
// have no counterpart in a plain delete. Deleting a key on a versioned bucket
// removes nothing, a marker becomes current, and deleting the marker brings the
// object back.

func versionedBucket(t *testing.T, pool *Pool, state VersioningState) string {
	t.Helper()
	ctx := context.Background()
	bucket, err := CreateBucket(ctx, pool, "versioned", nil)
	if err != nil {
		t.Fatalf("CreateBucket: %v", err)
	}
	settings := &BucketSettings{
		BucketID: bucket.ID, Versioning: state,
		CORSRules: []CORSRule{}, Lifecycle: []LifecycleRule{},
	}
	if err := SaveBucketSettings(ctx, pool, settings); err != nil {
		t.Fatalf("SaveBucketSettings: %v", err)
	}
	return bucket.ID
}

// put writes a key and returns the version id it was given.
func put(t *testing.T, pool *Pool, bucketID, key, body string, state VersioningState) string {
	t.Helper()
	ctx := context.Background()
	digest := writeBlob(t, pool, body)
	object := &Object{
		BucketID: bucketID, Key: key, BlobDigest: digest,
		Size: int64(len(body)), ETag: digest, ContentType: "text/plain",
	}
	if err := PutObject(ctx, pool, object, WriteOptions{Versioning: state, Actor: "test"}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	return object.VersionID
}

// writeBlob stores bytes and returns their digest.
func writeBlob(t *testing.T, pool *Pool, body string) string {
	t.Helper()
	// A digest unique per body is all the object layer needs here; the blob
	// store itself is exercised by its own tests.
	sum := sha256.Sum256([]byte(body))
	digest := hex.EncodeToString(sum[:])
	tx, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := RetainBlob(context.Background(), tx, digest, int64(len(body))); err != nil {
		t.Fatalf("RetainBlob: %v", err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return digest
}

func TestVersionIDsAreStableAcrossOverwrites(t *testing.T) {
	// A version id a client saved must keep pointing at the same bytes. If
	// superseding a state minted a new id for it, every id handed out would rot
	// on the next write.
	pool := testPool(t)
	bucketID := versionedBucket(t, pool, VersioningEnabled)

	first := put(t, pool, bucketID, "k", "one", VersioningEnabled)
	second := put(t, pool, bucketID, "k", "two", VersioningEnabled)

	if first == "" || second == "" || first == second {
		t.Fatalf("version ids are not distinct and non-empty: %q, %q", first, second)
	}

	// The first version is now history, and must still answer to its own id.
	object, err := GetObjectVersion(context.Background(), pool, bucketID, "k", first)
	if err != nil {
		t.Fatalf("the superseded version lost its id: %v", err)
	}
	if object.Size != 3 {
		t.Errorf("size = %d, want 3", object.Size)
	}
}

func TestDeleteOnAVersionedBucketWritesAMarkerAndRemovesNothing(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	bucketID := versionedBucket(t, pool, VersioningEnabled)

	versionID := put(t, pool, bucketID, "k", "body", VersioningEnabled)

	deletion, err := DeleteObject(ctx, pool, bucketID, "k", WriteOptions{Versioning: VersioningEnabled})
	if err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}
	if !deletion.DeleteMarker {
		t.Error("no delete marker was reported")
	}
	if deletion.MarkerVersionID == "" {
		t.Error("the delete marker has no version id; a client cannot undo the delete without it")
	}

	// An ordinary read is gone.
	if _, err := GetObject(ctx, pool, bucketID, "k"); err == nil {
		t.Error("the object is still readable after a versioned delete")
	}

	// The bytes are not.
	if _, err := GetObjectVersion(ctx, pool, bucketID, "k", versionID); err != nil {
		t.Errorf("the data was destroyed by a versioned delete: %v", err)
	}
}

func TestDeletingTheMarkerBringsTheObjectBack(t *testing.T) {
	// The behaviour with no counterpart in a plain delete, and the reason this
	// was worth doing at all.
	pool := testPool(t)
	ctx := context.Background()
	bucketID := versionedBucket(t, pool, VersioningEnabled)

	put(t, pool, bucketID, "k", "body", VersioningEnabled)
	deletion, err := DeleteObject(ctx, pool, bucketID, "k", WriteOptions{Versioning: VersioningEnabled})
	if err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}

	if _, err := DeleteObjectVersion(ctx, pool, bucketID, "k", deletion.MarkerVersionID); err != nil {
		t.Fatalf("DeleteObjectVersion: %v", err)
	}

	object, err := GetObject(ctx, pool, bucketID, "k")
	if err != nil {
		t.Fatalf("the object did not come back after its delete marker was removed: %v", err)
	}
	if object.Size != 4 {
		t.Errorf("size = %d, want 4", object.Size)
	}
}

func TestDeletingTheCurrentVersionPromotesTheOneBeneath(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	bucketID := versionedBucket(t, pool, VersioningEnabled)

	first := put(t, pool, bucketID, "k", "one", VersioningEnabled)
	second := put(t, pool, bucketID, "k", "two", VersioningEnabled)

	if _, err := DeleteObjectVersion(ctx, pool, bucketID, "k", second); err != nil {
		t.Fatalf("DeleteObjectVersion: %v", err)
	}

	object, err := GetObject(ctx, pool, bucketID, "k")
	if err != nil {
		t.Fatalf("no current version after removing the current one: %v", err)
	}
	if object.VersionID != first {
		t.Errorf("current version = %q, want the promoted %q", object.VersionID, first)
	}
	if object.Size != 3 {
		t.Errorf("size = %d, want the older version's 3", object.Size)
	}
}

func TestSuspendedKeepsOldVersionsAndReplacesTheNullOne(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	bucketID := versionedBucket(t, pool, VersioningEnabled)

	enabled := put(t, pool, bucketID, "k", "kept", VersioningEnabled)

	// Suspended: the next write takes the null id, and the version written
	// while versioning was on survives.
	put(t, pool, bucketID, "k", "null-one", VersioningSuspended)
	if _, err := GetObjectVersion(ctx, pool, bucketID, "k", enabled); err != nil {
		t.Errorf("suspending versioning destroyed a version written while it was on: %v", err)
	}

	// A second suspended write replaces the null version rather than keeping
	// both — the one genuinely surprising rule in S3 versioning.
	put(t, pool, bucketID, "k", "null-two", VersioningSuspended)

	result, err := ListObjectVersions(ctx, pool, bucketID, VersionListOptions{})
	if err != nil {
		t.Fatalf("ListObjectVersions: %v", err)
	}
	nulls := 0
	for _, entry := range result.Versions {
		if entry.VersionID == NullVersionID {
			nulls++
		}
	}
	if nulls != 1 {
		t.Errorf("found %d null versions, want exactly 1 — a suspended write replaces it", nulls)
	}
}

func TestListObjectVersionsOrdersAndMarksTheLatest(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	bucketID := versionedBucket(t, pool, VersioningEnabled)

	put(t, pool, bucketID, "a", "1", VersioningEnabled)
	put(t, pool, bucketID, "a", "22", VersioningEnabled)
	put(t, pool, bucketID, "b", "333", VersioningEnabled)
	if _, err := DeleteObject(ctx, pool, bucketID, "b", WriteOptions{Versioning: VersioningEnabled}); err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}

	result, err := ListObjectVersions(ctx, pool, bucketID, VersionListOptions{})
	if err != nil {
		t.Fatalf("ListObjectVersions: %v", err)
	}

	// Keys in byte order, newest first within each.
	var keys []string
	for _, entry := range result.Versions {
		keys = append(keys, entry.Key)
	}
	if len(keys) != 4 {
		t.Fatalf("got %d entries (%v), want 4: two versions of a, one of b, and b's delete marker", len(keys), keys)
	}
	if keys[0] != "a" || keys[1] != "a" || keys[2] != "b" || keys[3] != "b" {
		t.Errorf("keys out of order: %v", keys)
	}

	// Exactly one latest per key, and b's is its delete marker.
	latest := map[string]ObjectVersionEntry{}
	for _, entry := range result.Versions {
		if entry.IsLatest {
			if _, seen := latest[entry.Key]; seen {
				t.Errorf("key %q has more than one latest version", entry.Key)
			}
			latest[entry.Key] = entry
		}
	}
	if len(latest) != 2 {
		t.Fatalf("marked %d latest versions, want 2", len(latest))
	}
	if latest["a"].Size != 2 {
		t.Errorf("a's latest is size %d, want the newest write's 2", latest["a"].Size)
	}
	if !latest["b"].IsDeleteMarker {
		t.Error("b's latest should be its delete marker, since b was deleted")
	}
}

func TestListObjectVersionsPagesThroughAKeysHistory(t *testing.T) {
	// A page boundary can fall in the middle of one key's history, which is why
	// S3 pages versions with a key marker and a version marker together.
	pool := testPool(t)
	ctx := context.Background()
	bucketID := versionedBucket(t, pool, VersioningEnabled)

	for i := 0; i < 5; i++ {
		put(t, pool, bucketID, "k", string(rune('a'+i)), VersioningEnabled)
	}

	var seen []string
	opts := VersionListOptions{MaxKeys: 2}
	for page := 0; page < 10; page++ {
		result, err := ListObjectVersions(ctx, pool, bucketID, opts)
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		for _, entry := range result.Versions {
			seen = append(seen, entry.VersionID)
		}
		if !result.IsTruncated {
			break
		}
		opts.KeyMarker, opts.VersionIDMarker = result.NextKeyMarker, result.NextVersionID
	}

	if len(seen) != 5 {
		t.Fatalf("paged through %d versions, want 5: %v", len(seen), seen)
	}
	unique := map[string]bool{}
	for _, id := range seen {
		if unique[id] {
			t.Fatalf("version %q was returned twice across pages", id)
		}
		unique[id] = true
	}
}

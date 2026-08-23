package db

import (
	"context"
	"fmt"
	"time"
)

// ListObjectVersions walks every version of every key in a bucket.
//
// The response interleaves two element types — stored versions and delete
// markers — in one ordering, which is the part that catches people out. They
// are not two lists; they are one history, and a delete marker sits in it at
// the point the delete happened.
//
// Ordering is by key in UTF-8 byte order, then newest version first within each
// key, matching S3 exactly. The current version of a key lives in a different
// table from the rest, so this is a union — with the objects row always sorting
// first for its key, since it is by definition the newest.

// VersionListOptions selects and pages a version listing.
type VersionListOptions struct {
	Prefix    string
	Delimiter string
	MaxKeys   int
	// KeyMarker and VersionIDMarker resume a listing. S3 pages versions with
	// both together rather than one opaque token, because a page boundary can
	// fall in the middle of a key's history.
	KeyMarker       string
	VersionIDMarker string
}

// ObjectVersionEntry is one row of the listing.
type ObjectVersionEntry struct {
	Key            string
	VersionID      string
	IsLatest       bool
	IsDeleteMarker bool
	Size           int64
	ETag           string
	ContentType    string
	LastModified   time.Time
}

// VersionListResult is one page.
type VersionListResult struct {
	Versions       []ObjectVersionEntry
	CommonPrefixes []string
	IsTruncated    bool
	NextKeyMarker  string
	NextVersionID  string
}

// versionBatchSize is how many rows are read per round trip while assembling a
// page. Larger than a typical page so delimiter rollup, which can discard most
// of a batch, usually still fills one.
const versionBatchSize = 1000

func ListObjectVersions(ctx context.Context, q Querier, bucketID string, opts VersionListOptions) (*VersionListResult, error) {
	maxKeys := opts.MaxKeys
	if maxKeys <= 0 || maxKeys > maxMaxKeys {
		maxKeys = defaultMaxKeys
	}

	result := &VersionListResult{}
	seenPrefixes := make(map[string]bool)

	// Paging resumes at a key and a version within it. Rather than expressing
	// "the version after this one" in SQL — which would need the row's ordinal,
	// and so a second query to find it — the scan restarts at the marker key
	// and skips forward in Go. A key's history is short, so this rereads a
	// handful of rows at most.
	cursorKey := opts.KeyMarker
	skipping := opts.VersionIDMarker != ""
	var lastKey, lastVersion string

	for {
		batch, err := fetchVersionBatch(ctx, q, bucketID, opts.Prefix, cursorKey, versionBatchSize)
		if err != nil {
			return nil, err
		}
		if len(batch) == 0 {
			return result, nil
		}

		for _, entry := range batch {
			// Skip forward to just past the marker version.
			if skipping {
				if entry.Key == opts.KeyMarker && entry.VersionID == opts.VersionIDMarker {
					skipping = false
				}
				continue
			}
			cursorKey = entry.Key

			if group, ok := groupOf(entry.Key, opts.Prefix, opts.Delimiter); ok {
				if seenPrefixes[group] {
					continue
				}
				if len(result.Versions)+len(result.CommonPrefixes) >= maxKeys {
					result.IsTruncated = true
					result.NextKeyMarker, result.NextVersionID = lastKey, lastVersion
					return result, nil
				}
				seenPrefixes[group] = true
				result.CommonPrefixes = append(result.CommonPrefixes, group)
				lastKey, lastVersion = entry.Key, entry.VersionID
				continue
			}

			if len(result.Versions)+len(result.CommonPrefixes) >= maxKeys {
				result.IsTruncated = true
				result.NextKeyMarker, result.NextVersionID = lastKey, lastVersion
				return result, nil
			}
			result.Versions = append(result.Versions, entry)
			lastKey, lastVersion = entry.Key, entry.VersionID
		}

		if len(batch) < versionBatchSize {
			return result, nil
		}
		// Continue past the last key seen. Restarting at the same key would
		// loop, so the scan resumes strictly after it and the in-key skip
		// handles any remainder.
		cursorKey = cursorKey + "\x00"
	}
}

// fetchVersionBatch reads the next rows of the union, in listing order.
func fetchVersionBatch(ctx context.Context, q Querier, bucketID, prefix, after string, limit int) ([]ObjectVersionEntry, error) {
	// is_current sorts the live row first within its key: it is the newest
	// version by construction, and comparing timestamps to establish that would
	// be both slower and less certain.
	rows, err := q.Query(ctx, `
		WITH history AS (
		    SELECT key, version_id, size, etag, content_type,
		           updated_at AS at, false AS is_delete_marker, true AS is_current
		    FROM objects
		    WHERE bucket_id = $1
		  UNION ALL
		    SELECT key, version_id, size, etag, content_type,
		           created_at AS at, is_delete_marker, false AS is_current
		    FROM object_versions
		    WHERE bucket_id = $1
		)
		SELECT key, version_id, size, etag, content_type, at, is_delete_marker,
		       row_number() OVER (PARTITION BY key ORDER BY is_current DESC, at DESC, version_id DESC) = 1
		FROM history
		WHERE ($2 = '' OR (key >= $2 AND key < $3))
		  AND key >= $4
		ORDER BY key, is_current DESC, at DESC, version_id DESC
		LIMIT $5`,
		bucketID, prefix, prefixUpperBound(prefix), after, limit)
	if err != nil {
		return nil, fmt.Errorf("list object versions: %w", err)
	}
	defer rows.Close()

	var out []ObjectVersionEntry
	for rows.Next() {
		var entry ObjectVersionEntry
		if err := rows.Scan(&entry.Key, &entry.VersionID, &entry.Size, &entry.ETag,
			&entry.ContentType, &entry.LastModified, &entry.IsDeleteMarker,
			&entry.IsLatest); err != nil {
			return nil, fmt.Errorf("scan object version: %w", err)
		}
		// The null version is stored as an empty id on the live row and as the
		// literal "null" in history; clients see one spelling.
		entry.VersionID = externalVersionID(entry.VersionID)
		out = append(out, entry)
	}
	return out, rows.Err()
}

// HasVersionHistory reports whether a bucket holds any versions at all, which
// is what DeleteBucket needs to know: S3 refuses to delete a bucket that still
// has versions in it, and reporting "not empty" for a bucket whose object
// listing is empty is otherwise baffling.
func HasVersionHistory(ctx context.Context, q Querier, bucketID string) (bool, error) {
	var exists bool
	err := q.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM object_versions WHERE bucket_id = $1)`,
		bucketID).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check version history: %w", err)
	}
	return exists, nil
}

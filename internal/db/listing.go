package db

import (
	"context"
	"fmt"
	"strings"
)

// ListOptions describes a ListObjectsV2 request.
type ListOptions struct {
	Prefix    string
	Delimiter string
	// StartAfter is exclusive: listing resumes at the first key strictly
	// greater than it. Both start-after and a continuation token map onto it.
	StartAfter string
	MaxKeys    int
}

// ListResult is one page of a listing.
type ListResult struct {
	Objects        []Object
	CommonPrefixes []string
	IsTruncated    bool
	// NextStartAfter is the cursor for the following page, set only when the
	// result is truncated.
	NextStartAfter string
}

const (
	// defaultMaxKeys and maxMaxKeys match S3's limits.
	defaultMaxKeys = 1000
	maxMaxKeys     = 1000

	// listBatchSize is how many rows are fetched per round trip while
	// assembling a page. Larger than a typical page because grouping by
	// delimiter can collapse many keys into a single common prefix.
	listBatchSize = 1000
)

// highestRune is the largest valid UTF-8 code point. Appending it to a common
// prefix produces a cursor greater than every key beneath that prefix, which is
// what lets the scan skip an entire folder in one step instead of walking every
// key inside it. Without this, listing the top level of a bucket holding a
// million objects under one prefix would read all million rows to emit a single
// CommonPrefix.
const highestRune = "\U0010FFFF"

// ListObjects returns one page of a bucket listing, applying S3's prefix and
// delimiter semantics.
//
// Grouping is done here rather than in SQL because the skip-ahead above needs
// to drive the next query's cursor: a set-returning query cannot decide to jump
// past a range it has not yet examined.
func ListObjects(ctx context.Context, q Querier, bucketID string, opts ListOptions) (*ListResult, error) {
	maxKeys := opts.MaxKeys
	if maxKeys <= 0 || maxKeys > maxMaxKeys {
		maxKeys = defaultMaxKeys
	}

	result := &ListResult{}
	seenPrefixes := make(map[string]bool)
	cursor := opts.StartAfter
	if cursor == "" {
		// An empty cursor must not exclude the prefix itself, which is a legal
		// key. Comparing greater-than against the empty string includes it.
		cursor = ""
	}

	for {
		batch, err := fetchKeyBatch(ctx, q, bucketID, opts.Prefix, cursor, listBatchSize)
		if err != nil {
			return nil, err
		}
		if len(batch) == 0 {
			return result, nil
		}

		for i := range batch {
			object := batch[i]
			cursor = object.Key

			if group, ok := groupOf(object.Key, opts.Prefix, opts.Delimiter); ok {
				if seenPrefixes[group] {
					continue
				}
				if count(result) >= maxKeys {
					result.IsTruncated = true
					result.NextStartAfter = previousCursor(result, object.Key)
					return result, nil
				}
				seenPrefixes[group] = true
				result.CommonPrefixes = append(result.CommonPrefixes, group)
				// Skip the rest of this folder outright.
				cursor = group + highestRune
				break
			}

			if count(result) >= maxKeys {
				result.IsTruncated = true
				result.NextStartAfter = previousCursor(result, object.Key)
				return result, nil
			}
			result.Objects = append(result.Objects, object)
		}

		if count(result) >= maxKeys {
			// The page is full; whether more remains is settled on the next
			// iteration, which is cheap and avoids reporting a truncated result
			// when the page happened to end exactly at the last key.
			more, err := fetchKeyBatch(ctx, q, bucketID, opts.Prefix, cursor, 1)
			if err != nil {
				return nil, err
			}
			if len(more) > 0 {
				result.IsTruncated = true
				result.NextStartAfter = cursor
			}
			return result, nil
		}

		if len(batch) < listBatchSize && !strings.HasSuffix(cursor, highestRune) {
			return result, nil
		}
	}
}

// count is the number of entries a page holds. S3 counts objects and common
// prefixes together against max-keys.
func count(r *ListResult) int {
	return len(r.Objects) + len(r.CommonPrefixes)
}

// previousCursor returns the cursor to resume from when a page filled up before
// consuming the key currently in hand. Resuming from the last *emitted* key
// means the unconsumed one is the first entry of the next page.
func previousCursor(r *ListResult, unconsumed string) string {
	if n := len(r.Objects); n > 0 {
		last := r.Objects[n-1].Key
		if len(r.CommonPrefixes) == 0 || last > r.CommonPrefixes[len(r.CommonPrefixes)-1] {
			return last
		}
	}
	if n := len(r.CommonPrefixes); n > 0 {
		return r.CommonPrefixes[n-1] + highestRune
	}
	// Nothing was emitted, so the caller asked for a zero-length page.
	return unconsumed
}

// groupOf returns the common prefix a key rolls up into, if the delimiter
// appears in the part of the key after the prefix.
//
// The returned group includes the delimiter, so "photos/2026/a.jpg" under
// prefix "photos/" with delimiter "/" groups as "photos/2026/".
func groupOf(key, prefix, delimiter string) (string, bool) {
	if delimiter == "" {
		return "", false
	}
	rest := strings.TrimPrefix(key, prefix)
	index := strings.Index(rest, delimiter)
	if index < 0 {
		return "", false
	}
	return prefix + rest[:index+len(delimiter)], true
}

// fetchKeyBatch reads the next rows in key order.
//
// The prefix filter uses a half-open range rather than LIKE. LIKE would need
// the prefix escaped for % and _, and a range comparison is a plain index scan
// under the C collation the key column uses.
func fetchKeyBatch(ctx context.Context, q Querier, bucketID, prefix, after string, limit int) ([]Object, error) {
	query := `
		SELECT id::text, key, blob_digest, size, etag, content_type, created_at, updated_at
		FROM objects
		WHERE bucket_id = $1 AND key > $2`
	args := []any{bucketID, after}

	if prefix != "" {
		query += ` AND key >= $3 AND key < $4`
		args = append(args, prefix, prefixUpperBound(prefix))
	}
	query += fmt.Sprintf(` ORDER BY key LIMIT $%d`, len(args)+1)
	args = append(args, limit)

	rows, err := q.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list objects: %w", err)
	}
	defer rows.Close()

	var out []Object
	for rows.Next() {
		var o Object
		o.BucketID = bucketID
		if err := rows.Scan(&o.ID, &o.Key, &o.BlobDigest, &o.Size, &o.ETag,
			&o.ContentType, &o.CreatedAt, &o.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan object: %w", err)
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// prefixUpperBound returns the first string greater than every string starting
// with prefix, by incrementing its last byte. Under the C collation that turns
// a prefix match into a range scan.
func prefixUpperBound(prefix string) string {
	buf := []byte(prefix)
	for i := len(buf) - 1; i >= 0; i-- {
		if buf[i] < 0xFF {
			buf[i]++
			return string(buf[:i+1])
		}
	}
	// Every byte is 0xFF, so no string sorts above it. Appending the highest
	// rune is unreachable in practice: a valid UTF-8 key cannot contain 0xFF.
	return prefix + highestRune
}

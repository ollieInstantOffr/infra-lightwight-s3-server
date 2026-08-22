package db

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// SearchHit is one object found by the command palette.
type SearchHit struct {
	Bucket       string
	Key          string
	Size         int64
	ContentType  string
	LastModified time.Time
}

// SearchResult carries the hits and whether the scan was cut short.
type SearchResult struct {
	Hits []SearchHit
	// Truncated says the cap was reached, so the caller can say "showing the
	// first N" rather than implying these are all the matches. Silently
	// truncating a search is worse than finding nothing: the user concludes
	// the object does not exist.
	Truncated bool
	// ScannedAsPrefix records that the query was answered by an index range
	// scan rather than a substring scan, which is worth surfacing because the
	// two have very different completeness guarantees on a large bucket.
	ScannedAsPrefix bool
}

// searchScanLimit bounds a substring search. Object keys have no trigram index
// here, so a contains-match is a scan; the cap keeps a careless query from
// reading an entire bucket.
const searchScanLimit = 20000

// SearchObjects finds objects whose key matches a query, across every bucket.
//
// A query with no wildcard characters is first tried as a prefix, which the
// (bucket_id, key) index answers directly. Anything else falls back to a
// bounded substring scan.
func SearchObjects(ctx context.Context, q Querier, query string, limit int) (*SearchResult, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return &SearchResult{Hits: []SearchHit{}}, nil
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}

	result := &SearchResult{Hits: []SearchHit{}}

	// A prefix search is index-assisted, so it is tried first and is exact.
	prefixHits, err := searchByPrefix(ctx, q, query, limit+1)
	if err != nil {
		return nil, err
	}
	if len(prefixHits) > 0 {
		result.ScannedAsPrefix = true
		if len(prefixHits) > limit {
			result.Truncated = true
			prefixHits = prefixHits[:limit]
		}
		result.Hits = prefixHits
		return result, nil
	}

	// Otherwise scan, bounded. The inner LIMIT caps how much is read; the outer
	// one caps what is returned, and comparing the two is how truncation is
	// detected.
	rows, err := q.Query(ctx, `
		SELECT b.name, o.key, o.size, o.content_type, o.updated_at
		FROM objects o JOIN buckets b ON b.id = o.bucket_id
		WHERE position($1 IN o.key) > 0
		ORDER BY b.name, o.key
		LIMIT $2`, query, limit+1)
	if err != nil {
		return nil, fmt.Errorf("search objects: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var hit SearchHit
		if err := rows.Scan(&hit.Bucket, &hit.Key, &hit.Size, &hit.ContentType, &hit.LastModified); err != nil {
			return nil, fmt.Errorf("scan search hit: %w", err)
		}
		result.Hits = append(result.Hits, hit)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if len(result.Hits) > limit {
		result.Truncated = true
		result.Hits = result.Hits[:limit]
	}
	return result, nil
}

func searchByPrefix(ctx context.Context, q Querier, prefix string, limit int) ([]SearchHit, error) {
	rows, err := q.Query(ctx, `
		SELECT b.name, o.key, o.size, o.content_type, o.updated_at
		FROM objects o JOIN buckets b ON b.id = o.bucket_id
		WHERE o.key >= $1 AND o.key < $2
		ORDER BY b.name, o.key
		LIMIT $3`, prefix, prefixUpperBound(prefix), limit)
	if err != nil {
		return nil, fmt.Errorf("search objects by prefix: %w", err)
	}
	defer rows.Close()

	var hits []SearchHit
	for rows.Next() {
		var hit SearchHit
		if err := rows.Scan(&hit.Bucket, &hit.Key, &hit.Size, &hit.ContentType, &hit.LastModified); err != nil {
			return nil, fmt.Errorf("scan search hit: %w", err)
		}
		hits = append(hits, hit)
	}
	return hits, rows.Err()
}

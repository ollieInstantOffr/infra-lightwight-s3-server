package db

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// BucketSettings are the per-bucket options the console exposes.
type BucketSettings struct {
	BucketID string
	// PublicRead permits anonymous GET and HEAD on this bucket. Never anonymous
	// writes: a publicly writable bucket is an open file drop, and nobody means
	// to create one.
	PublicRead bool
	Versioning bool
	CORSRules  []CORSRule
	Lifecycle  []LifecycleRule
	UpdatedAt  time.Time
}

// CORSRule permits a browser on another origin to use this bucket.
type CORSRule struct {
	AllowedOrigins []string `json:"allowedOrigins"`
	AllowedMethods []string `json:"allowedMethods"`
	AllowedHeaders []string `json:"allowedHeaders"`
	ExposeHeaders  []string `json:"exposeHeaders,omitempty"`
	MaxAgeSeconds  int      `json:"maxAgeSeconds,omitempty"`
}

// LifecycleRule expires objects under a prefix after a number of days.
type LifecycleRule struct {
	ID         string `json:"id"`
	Prefix     string `json:"prefix"`
	ExpireDays int    `json:"expireDays"`
	Enabled    bool   `json:"enabled"`
}

// GetBucketSettings returns a bucket's settings, defaulting when none are
// stored. A bucket with no settings row is not an error: it simply has never
// been configured, and every option is off.
func GetBucketSettings(ctx context.Context, q Querier, bucketID string) (*BucketSettings, error) {
	settings := &BucketSettings{BucketID: bucketID, CORSRules: []CORSRule{}, Lifecycle: []LifecycleRule{}}
	var cors, lifecycle []byte

	err := q.QueryRow(ctx, `
		SELECT public_read, versioning, cors_rules, lifecycle_rules, updated_at
		FROM bucket_settings WHERE bucket_id = $1`, bucketID,
	).Scan(&settings.PublicRead, &settings.Versioning, &cors, &lifecycle, &settings.UpdatedAt)
	if err != nil {
		// No row means unconfigured, which is the default and not a failure.
		return settings, nil
	}

	if len(cors) > 0 {
		if err := json.Unmarshal(cors, &settings.CORSRules); err != nil {
			return nil, fmt.Errorf("decode cors rules: %w", err)
		}
	}
	if len(lifecycle) > 0 {
		if err := json.Unmarshal(lifecycle, &settings.Lifecycle); err != nil {
			return nil, fmt.Errorf("decode lifecycle rules: %w", err)
		}
	}
	return settings, nil
}

// SaveBucketSettings writes a bucket's settings.
func SaveBucketSettings(ctx context.Context, q Querier, settings *BucketSettings) error {
	cors, err := json.Marshal(settings.CORSRules)
	if err != nil {
		return fmt.Errorf("encode cors rules: %w", err)
	}
	lifecycle, err := json.Marshal(settings.Lifecycle)
	if err != nil {
		return fmt.Errorf("encode lifecycle rules: %w", err)
	}

	_, err = q.Exec(ctx, `
		INSERT INTO bucket_settings (bucket_id, public_read, versioning, cors_rules, lifecycle_rules)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (bucket_id) DO UPDATE SET
			public_read     = EXCLUDED.public_read,
			versioning      = EXCLUDED.versioning,
			cors_rules      = EXCLUDED.cors_rules,
			lifecycle_rules = EXCLUDED.lifecycle_rules,
			updated_at      = now()`,
		settings.BucketID, settings.PublicRead, settings.Versioning, cors, lifecycle)
	if err != nil {
		return fmt.Errorf("save bucket settings: %w", err)
	}
	return nil
}

// PublicBucket reports whether a bucket permits anonymous reads. Looked up by
// name because the S3 API resolves anonymity before it has a bucket record.
func PublicBucket(ctx context.Context, q Querier, name string) (bool, error) {
	var public bool
	err := q.QueryRow(ctx, `
		SELECT coalesce(s.public_read, false)
		FROM buckets b LEFT JOIN bucket_settings s ON s.bucket_id = b.id
		WHERE b.name = $1`, name).Scan(&public)
	if err != nil {
		return false, nil
	}
	return public, nil
}

// LifecycleTarget is one bucket's expiry rules, for the sweeper.
type LifecycleTarget struct {
	BucketID   string
	BucketName string
	Rules      []LifecycleRule
}

// BucketsWithLifecycle returns every bucket that has expiry rules configured.
func BucketsWithLifecycle(ctx context.Context, q Querier) ([]LifecycleTarget, error) {
	rows, err := q.Query(ctx, `
		SELECT b.id::text, b.name, s.lifecycle_rules
		FROM bucket_settings s JOIN buckets b ON b.id = s.bucket_id
		WHERE jsonb_array_length(s.lifecycle_rules) > 0`)
	if err != nil {
		return nil, fmt.Errorf("list lifecycle rules: %w", err)
	}
	defer rows.Close()

	var out []LifecycleTarget
	for rows.Next() {
		var target LifecycleTarget
		var raw []byte
		if err := rows.Scan(&target.BucketID, &target.BucketName, &raw); err != nil {
			return nil, fmt.Errorf("scan lifecycle rules: %w", err)
		}
		if err := json.Unmarshal(raw, &target.Rules); err != nil {
			return nil, fmt.Errorf("decode lifecycle rules for %s: %w", target.BucketName, err)
		}
		out = append(out, target)
	}
	return out, rows.Err()
}

// ExpiredObjectKeys returns keys under a prefix older than the given age.
func ExpiredObjectKeys(ctx context.Context, q Querier, bucketID, prefix string, days, limit int) ([]string, error) {
	rows, err := q.Query(ctx, `
		SELECT key FROM objects
		WHERE bucket_id = $1
		  AND key LIKE $2 || '%'
		  AND updated_at < now() - make_interval(days => $3)
		ORDER BY key
		LIMIT $4`, bucketID, prefix, days, limit)
	if err != nil {
		return nil, fmt.Errorf("find expired objects: %w", err)
	}
	defer rows.Close()

	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, fmt.Errorf("scan expired object: %w", err)
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

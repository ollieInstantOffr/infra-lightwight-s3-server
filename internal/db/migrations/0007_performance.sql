-- What the Performance page needs that the log did not already carry.
--
-- The router already decides which S3 operation a request was — GetObject,
-- ListObjectsV2, CompleteMultipartUpload — for the scrape metrics added in the
-- monitoring epic. That name was discarded rather than persisted, and without
-- it "slowest operations" cannot tell ListObjectsV2 apart from
-- GetBucketVersioning: both are a GET on a bucket, distinguished only by a
-- query string this table never kept.
ALTER TABLE request_logs
    ADD COLUMN operation TEXT NOT NULL DEFAULT '';

-- Grouping by operation over a time window is the slowest-operations query.
-- Partial on a non-empty operation, since rows written before this migration
-- (and the small number that never reach routing at all, like a request
-- rejected on signature) have none and would otherwise bloat an index that
-- exists to serve one specific aggregate.
CREATE INDEX request_logs_operation_idx
    ON request_logs (operation, occurred_at DESC) WHERE operation <> '';

-- The log screen has never been able to ask "just the slow ones" — every
-- filter is about what a request was, not how long it took. This is the
-- design's own words for the gap: "slow=1 is new — the log filter has no
-- duration predicate today."
CREATE INDEX request_logs_duration_idx
    ON request_logs (duration_ms DESC, occurred_at DESC) WHERE duration_ms > 0;

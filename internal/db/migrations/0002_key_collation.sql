-- Object keys must sort in UTF-8 byte order.
--
-- S3 orders ListObjectsV2 results by the raw bytes of the key. Postgres orders
-- text by the database's collation, which for a typical en_US.UTF-8 database
-- ignores case and punctuation differences during comparison. Under that
-- collation "Foo.txt", "foo.txt" and "foo-bar.txt" come back in an order no S3
-- client expects, and pagination can revisit or skip keys, because the
-- continuation cursor compares under one ordering while the client assumes
-- another.
--
-- COLLATE "C" is plain byte comparison, which is exactly S3's rule.
--
-- It also makes the index usable for both the range scan and the ordering. The
-- previous text_pattern_ops index served LIKE 'prefix%' but not ORDER BY key,
-- because that operator class only applies to pattern matching. With a C
-- collation one plain index serves both, so prefix listing stays an index scan
-- instead of degrading to a sort over the whole bucket.

ALTER TABLE objects
    ALTER COLUMN key TYPE TEXT COLLATE "C";

ALTER TABLE multipart_uploads
    ALTER COLUMN key TYPE TEXT COLLATE "C";

DROP INDEX IF EXISTS objects_prefix_scan_idx;

CREATE INDEX objects_prefix_scan_idx ON objects (bucket_id, key);

COMMENT ON COLUMN objects.key IS
    'Collated C so ordering is UTF-8 byte order, matching S3 exactly.';

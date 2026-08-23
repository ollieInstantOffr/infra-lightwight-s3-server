-- Reconciles the internal version history with S3's version semantics.
--
-- The history already existed and the console could browse and restore it. What
-- it did not have was the things S3 clients need: a stable identifier for every
-- version including the current one, and the three-state versioning switch.
--
-- The invariant this establishes, and which the Go code maintains:
--
--   objects          holds the CURRENT version of a key
--   object_versions  holds every version that is not current
--
-- A key whose current version is a delete marker has no objects row at all,
-- which is what makes an ordinary GET return 404 while the data is still there.
-- Listing versions is therefore a union of the two tables, with the objects row
-- always being the latest when it is present.

-- ─── Version identity for the current version ────────────────────────────────

-- Until now only superseded states carried a version id; the live object had
-- none, so there was nothing to put in x-amz-version-id or to address with
-- ?versionId. A version id must also be stable: when a state stops being
-- current it moves into object_versions carrying the id it already had, rather
-- than being assigned a new one, or a client's saved version id would rot.
--
-- The empty string means the null version: an object written while the bucket
-- was unversioned. S3 reports these with the literal id "null", and clients do
-- check for it, so it is represented explicitly rather than faked.
ALTER TABLE objects
    ADD COLUMN version_id TEXT NOT NULL DEFAULT '';

-- ─── Three-state versioning ──────────────────────────────────────────────────

-- S3 has three states, not two, and the difference is visible to clients.
-- Suspended is not the same as never having been enabled: existing versions
-- survive and remain addressable, while new writes stop creating them and take
-- the null version id instead.
--
-- S3 also does not allow returning to the unversioned state once versioning has
-- been enabled; suspending is as far back as it goes. The check constraint does
-- not enforce that transition rule — the Go code does, where a useful error
-- message can be produced.
ALTER TABLE bucket_settings
    ADD COLUMN versioning_state TEXT NOT NULL DEFAULT 'unversioned';

UPDATE bucket_settings
SET versioning_state = 'enabled'
WHERE versioning;

ALTER TABLE bucket_settings
    ADD CONSTRAINT bucket_settings_versioning_state
    CHECK (versioning_state IN ('unversioned', 'enabled', 'suspended'));

-- The boolean is dropped rather than kept in step. Two columns describing the
-- same thing disagree eventually, and the one that gets forgotten is the one
-- the authorizer reads.
ALTER TABLE bucket_settings
    DROP COLUMN versioning;

-- ─── Listing support ─────────────────────────────────────────────────────────

-- ListObjectVersions walks a bucket in key order, newest version first within
-- each key, and pages by (key, version). The existing history index is per key;
-- this one serves the bucket-wide walk.
CREATE INDEX object_versions_listing_idx
    ON object_versions (bucket_id, key, created_at DESC);

DROP INDEX IF EXISTS object_versions_history_idx;

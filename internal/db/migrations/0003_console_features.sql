-- Tables behind the console screens: request metrics, the audit log, per-bucket
-- settings, and object versioning.

-- ─── Request metrics ─────────────────────────────────────────────────────────

-- Rolled up per hour rather than stored per request. A row per request would
-- mean a database write on every object GET, which is both the hottest path in
-- the system and the one that must stay closest to a plain file read.
CREATE TABLE request_metrics (
    hour         TIMESTAMPTZ NOT NULL,
    status_class SMALLINT    NOT NULL CHECK (status_class BETWEEN 1 AND 5),
    requests     BIGINT      NOT NULL DEFAULT 0 CHECK (requests >= 0),
    bytes_in     BIGINT      NOT NULL DEFAULT 0 CHECK (bytes_in >= 0),
    bytes_out    BIGINT      NOT NULL DEFAULT 0 CHECK (bytes_out >= 0),
    PRIMARY KEY (hour, status_class)
);

COMMENT ON TABLE request_metrics IS
    'Hourly S3 API request rollups. Written by a periodic flush, never per request.';

CREATE INDEX request_metrics_hour_idx ON request_metrics (hour DESC);

-- ─── Audit log ───────────────────────────────────────────────────────────────

CREATE TABLE audit_events (
    id           BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    actor_id     UUID        REFERENCES users (id) ON DELETE SET NULL,
    -- Kept alongside the reference so the log still reads correctly after the
    -- account is deleted. An audit trail that forgets who acted once they
    -- leave is not an audit trail.
    actor_email  TEXT        NOT NULL,
    action       TEXT        NOT NULL,
    subject_type TEXT        NOT NULL DEFAULT '',
    subject      TEXT        NOT NULL DEFAULT '',
    detail       JSONB       NOT NULL DEFAULT '{}'::jsonb,
    ip           INET,
    user_agent   TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX audit_events_created_idx ON audit_events (created_at DESC);
CREATE INDEX audit_events_actor_idx ON audit_events (actor_email, created_at DESC);
CREATE INDEX audit_events_action_idx ON audit_events (action, created_at DESC);

-- ─── Bucket settings ─────────────────────────────────────────────────────────

CREATE TABLE bucket_settings (
    bucket_id       UUID        PRIMARY KEY REFERENCES buckets (id) ON DELETE CASCADE,
    -- Anonymous GET and HEAD only. Never anonymous writes: a publicly writable
    -- bucket is an open file drop, and nobody means to create one.
    public_read     BOOLEAN     NOT NULL DEFAULT false,
    versioning      BOOLEAN     NOT NULL DEFAULT false,
    cors_rules      JSONB       NOT NULL DEFAULT '[]'::jsonb,
    lifecycle_rules JSONB       NOT NULL DEFAULT '[]'::jsonb,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ─── Object versions ─────────────────────────────────────────────────────────

-- Version history, not redundancy: each distinct version still has exactly one
-- copy of its bytes. What it changes is that deleting an object no longer frees
-- its space until the versions are purged.
CREATE TABLE object_versions (
    id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    bucket_id    UUID        NOT NULL REFERENCES buckets (id) ON DELETE CASCADE,
    key          TEXT        COLLATE "C" NOT NULL,
    version_id   TEXT        NOT NULL,
    -- Null for a delete marker, which records that the key was deleted at a
    -- point in time without destroying what came before it.
    blob_digest  TEXT        REFERENCES blobs (digest) ON DELETE RESTRICT,
    size         BIGINT      NOT NULL DEFAULT 0 CHECK (size >= 0),
    etag         TEXT        NOT NULL DEFAULT '',
    content_type TEXT        NOT NULL DEFAULT 'application/octet-stream',
    metadata     JSONB       NOT NULL DEFAULT '{}'::jsonb,
    is_delete_marker BOOLEAN NOT NULL DEFAULT false,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by   TEXT        NOT NULL DEFAULT '',
    UNIQUE (bucket_id, key, version_id),
    -- A delete marker has no bytes; anything else must have some.
    CONSTRAINT object_versions_marker_has_no_blob CHECK (
        (is_delete_marker AND blob_digest IS NULL) OR (NOT is_delete_marker AND blob_digest IS NOT NULL)
    )
);

-- Version history is read newest-first for one key, and swept by bucket.
CREATE INDEX object_versions_history_idx ON object_versions (bucket_id, key, created_at DESC);
CREATE INDEX object_versions_blob_idx ON object_versions (blob_digest);

-- Initial schema for the lightweight S3 server.
--
-- Two broad groups live here: console identity (users, invites, sessions,
-- magic_links, credentials) and object storage metadata (buckets, blobs,
-- objects, multipart_uploads, multipart_parts).
--
-- Object bytes never live in Postgres. The blobs table is an index over the
-- content-addressed files under DATA_DIR, and exists so reference counting is
-- transactional rather than a filesystem scan.

-- ─── Console identity ────────────────────────────────────────────────────────

CREATE TABLE users (
    id            UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    email         TEXT        NOT NULL UNIQUE CHECK (email = lower(email) AND position('@' IN email) > 1),
    role          TEXT        NOT NULL DEFAULT 'MEMBER' CHECK (role IN ('ADMIN', 'MEMBER')),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_login_at TIMESTAMPTZ
);

COMMENT ON TABLE users IS 'Console operators. Authentication is passwordless, so no password column exists by design.';

CREATE TABLE invites (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    email       TEXT        NOT NULL CHECK (email = lower(email)),
    token_hash  BYTEA       NOT NULL UNIQUE,
    role        TEXT        NOT NULL DEFAULT 'MEMBER' CHECK (role IN ('ADMIN', 'MEMBER')),
    invited_by  UUID        REFERENCES users (id) ON DELETE SET NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at  TIMESTAMPTZ NOT NULL,
    accepted_at TIMESTAMPTZ,
    revoked_at  TIMESTAMPTZ
);

-- At most one live invite per address, so re-inviting replaces rather than
-- accumulating tokens that all still work.
CREATE UNIQUE INDEX invites_one_pending_per_email
    ON invites (email)
    WHERE accepted_at IS NULL AND revoked_at IS NULL;

CREATE TABLE magic_links (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    email       TEXT        NOT NULL CHECK (email = lower(email)),
    token_hash  BYTEA       NOT NULL UNIQUE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at  TIMESTAMPTZ NOT NULL,
    consumed_at TIMESTAMPTZ,
    request_ip  INET
);

COMMENT ON COLUMN magic_links.token_hash IS 'SHA-256 of the token. The plaintext exists only in the email, so a database leak cannot be replayed into a login.';

-- Drives per-email rate limiting on the login endpoint.
CREATE INDEX magic_links_email_created_idx ON magic_links (email, created_at DESC);

CREATE TABLE sessions (
    id                  UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id             UUID        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    token_hash          BYTEA       NOT NULL UNIQUE,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Idle expiry slides forward on use; absolute expiry never does, so a
    -- stolen cookie cannot be kept alive indefinitely.
    idle_expires_at     TIMESTAMPTZ NOT NULL,
    absolute_expires_at TIMESTAMPTZ NOT NULL,
    user_agent          TEXT,
    ip                  INET,
    revoked_at          TIMESTAMPTZ
);

CREATE INDEX sessions_user_idx ON sessions (user_id) WHERE revoked_at IS NULL;

CREATE TABLE credentials (
    id            UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    access_key_id TEXT        NOT NULL UNIQUE,
    -- SigV4 verification re-derives the signing key from the secret, so unlike
    -- a password this cannot be a one-way hash. It is stored encrypted with
    -- AES-256-GCM instead, keyed from CREDENTIALS_KEY.
    secret_ciphertext BYTEA   NOT NULL,
    secret_nonce      BYTEA   NOT NULL,
    description   TEXT        NOT NULL DEFAULT '',
    owner_user_id UUID        REFERENCES users (id) ON DELETE CASCADE,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at  TIMESTAMPTZ,
    revoked_at    TIMESTAMPTZ
);

COMMENT ON TABLE credentials IS 'S3 access key pairs. Separate from console sessions: these authenticate the S3 API, sessions authenticate the console.';

-- ─── Object storage ──────────────────────────────────────────────────────────

CREATE TABLE buckets (
    id         UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    name       TEXT        NOT NULL UNIQUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by UUID        REFERENCES users (id) ON DELETE SET NULL,
    -- Full S3 naming rules are enforced in Go, where the error can name the
    -- exact rule broken. This is a coarse backstop against nonsense rows.
    CONSTRAINT buckets_name_shape CHECK (
        length(name) BETWEEN 3 AND 63 AND name ~ '^[a-z0-9][a-z0-9.-]*[a-z0-9]$'
    )
);

CREATE TABLE blobs (
    digest     TEXT        PRIMARY KEY,
    size       BIGINT      NOT NULL CHECK (size >= 0),
    -- Incremented per referencing object or part. A blob reaching zero is
    -- unlinked from disk. Kept here so the count is transactional.
    refcount   BIGINT      NOT NULL DEFAULT 0 CHECK (refcount >= 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMENT ON TABLE blobs IS 'Index over content-addressed files in DATA_DIR, keyed by SHA-256. Identical uploads share one blob; only one copy of any byte sequence exists.';

CREATE INDEX blobs_unreferenced_idx ON blobs (created_at) WHERE refcount = 0;

CREATE TABLE objects (
    id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    bucket_id    UUID        NOT NULL REFERENCES buckets (id) ON DELETE CASCADE,
    key          TEXT        NOT NULL CHECK (length(key) BETWEEN 1 AND 1024),
    blob_digest  TEXT        NOT NULL REFERENCES blobs (digest) ON DELETE RESTRICT,
    size         BIGINT      NOT NULL CHECK (size >= 0),
    -- The value S3 clients compare against. For a single PUT this is the MD5
    -- hex digest; for a completed multipart upload it is the composite form
    -- "<md5-of-concatenated-part-md5s>-<partcount>".
    etag         TEXT        NOT NULL,
    content_type TEXT        NOT NULL DEFAULT 'application/octet-stream',
    -- User metadata from x-amz-meta-* headers.
    metadata     JSONB       NOT NULL DEFAULT '{}'::jsonb,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (bucket_id, key)
);

-- ListObjectsV2 pages with `key > continuation AND key LIKE prefix || '%'`
-- ordered by key. text_pattern_ops makes that prefix comparison index-assisted
-- regardless of the database's collation.
CREATE INDEX objects_prefix_scan_idx ON objects (bucket_id, key text_pattern_ops);

CREATE INDEX objects_blob_idx ON objects (blob_digest);

CREATE TABLE multipart_uploads (
    id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    upload_id    TEXT        NOT NULL UNIQUE,
    bucket_id    UUID        NOT NULL REFERENCES buckets (id) ON DELETE CASCADE,
    key          TEXT        NOT NULL CHECK (length(key) BETWEEN 1 AND 1024),
    content_type TEXT        NOT NULL DEFAULT 'application/octet-stream',
    metadata     JSONB       NOT NULL DEFAULT '{}'::jsonb,
    initiated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    initiated_by UUID        REFERENCES credentials (id) ON DELETE SET NULL
);

-- ListMultipartUploads orders by key, and the reaper sweeps by age.
CREATE INDEX multipart_uploads_bucket_key_idx ON multipart_uploads (bucket_id, key);
CREATE INDEX multipart_uploads_initiated_idx ON multipart_uploads (initiated_at);

CREATE TABLE multipart_parts (
    upload_id   UUID        NOT NULL REFERENCES multipart_uploads (id) ON DELETE CASCADE,
    part_number INT         NOT NULL CHECK (part_number BETWEEN 1 AND 10000),
    blob_digest TEXT        NOT NULL REFERENCES blobs (digest) ON DELETE RESTRICT,
    size        BIGINT      NOT NULL CHECK (size >= 0),
    etag        TEXT        NOT NULL,
    uploaded_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (upload_id, part_number)
);

COMMENT ON TABLE multipart_parts IS 'Re-uploading a part number replaces the row, matching S3 semantics where the last write wins.';

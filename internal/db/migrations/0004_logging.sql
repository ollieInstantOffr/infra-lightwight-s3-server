-- Persisted request logs, captured server events, and alerts.
--
-- The server already produced all of this on stdout. What it lacked was
-- anywhere to query it from, which meant the console could report that 0.02% of
-- requests failed while being unable to say which ones or why.

-- ─── Request logs ────────────────────────────────────────────────────────────

-- Not every request lands here. Errors and slow requests always do; successes
-- are sampled. A row per request would put a database write on every object
-- GET, which is the hottest path in the system and the one that most needs to
-- stay close to a plain file read.
CREATE TABLE request_logs (
    id            BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    occurred_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    request_id    TEXT        NOT NULL DEFAULT '',
    -- node is meaningless today and essential once clustered. Recorded now so
    -- the log does not need a migration to become useful later.
    node          TEXT        NOT NULL DEFAULT '',
    -- s3 or console: the two listeners have very different traffic, and mixing
    -- them makes both harder to read.
    surface       TEXT        NOT NULL DEFAULT 's3' CHECK (surface IN ('s3', 'console')),
    method        TEXT        NOT NULL DEFAULT '',
    bucket        TEXT        NOT NULL DEFAULT '',
    object_key    TEXT        NOT NULL DEFAULT '',
    path          TEXT        NOT NULL DEFAULT '',
    status        SMALLINT    NOT NULL,
    -- The S3 error code the client was told, and the reason the server actually
    -- had. They differ deliberately: a prober is told InvalidAccessKeyId while
    -- the operator needs to know the credential was revoked last Tuesday.
    error_code    TEXT        NOT NULL DEFAULT '',
    reason        TEXT        NOT NULL DEFAULT '',
    bytes_in      BIGINT      NOT NULL DEFAULT 0,
    bytes_out     BIGINT      NOT NULL DEFAULT 0,
    duration_ms   INTEGER     NOT NULL DEFAULT 0,
    access_key_id TEXT        NOT NULL DEFAULT '',
    actor         TEXT        NOT NULL DEFAULT '',
    client_ip     INET,
    user_agent    TEXT        NOT NULL DEFAULT '',
    -- False for a request kept because it failed or was slow; true for one kept
    -- as a sample of ordinary traffic. Retention treats them differently.
    sampled       BOOLEAN     NOT NULL DEFAULT false
);

COMMENT ON TABLE request_logs IS
    'Sampled request log. Errors and slow requests always retained; successes sampled. Never contains signatures, secrets or presigned query parameters.';

-- The viewer reads newest-first and filters on status class, so this covers the
-- common query without a scan.
CREATE INDEX request_logs_recent_idx ON request_logs (occurred_at DESC);
CREATE INDEX request_logs_status_idx ON request_logs (status, occurred_at DESC);
CREATE INDEX request_logs_bucket_idx ON request_logs (bucket, occurred_at DESC) WHERE bucket <> '';
CREATE INDEX request_logs_key_idx ON request_logs (access_key_id, occurred_at DESC) WHERE access_key_id <> '';
-- Error analytics groups by code over a window.
CREATE INDEX request_logs_errors_idx ON request_logs (error_code, occurred_at DESC) WHERE status >= 400;

-- ─── Server events ───────────────────────────────────────────────────────────

-- Warnings and errors the server raised about itself: a failed email send, a
-- blob it could not reclaim, settings it could not read. Request logs explain
-- requests; these explain everything else.
--
-- Warn and above only. Capturing info would be volume without value.
CREATE TABLE server_events (
    id          BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    occurred_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    node        TEXT        NOT NULL DEFAULT '',
    level       TEXT        NOT NULL CHECK (level IN ('WARN', 'ERROR')),
    message     TEXT        NOT NULL,
    attributes  JSONB       NOT NULL DEFAULT '{}'::jsonb
);

CREATE INDEX server_events_recent_idx ON server_events (occurred_at DESC);
CREATE INDEX server_events_level_idx ON server_events (level, occurred_at DESC);

-- ─── Alerts ──────────────────────────────────────────────────────────────────

CREATE TABLE alert_rules (
    id          TEXT        PRIMARY KEY,
    name        TEXT        NOT NULL,
    description TEXT        NOT NULL DEFAULT '',
    enabled     BOOLEAN     NOT NULL DEFAULT true,
    severity    TEXT        NOT NULL DEFAULT 'warning' CHECK (severity IN ('info', 'warning', 'critical')),
    -- Thresholds vary by rule kind, so they are held as JSON rather than a
    -- column per rule. The rule implementation validates its own shape.
    settings    JSONB       NOT NULL DEFAULT '{}'::jsonb,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- An alert has state, and resolves itself when the condition clears. The
-- partial unique index is what stops a firing condition creating a new row on
-- every evaluation: one live alert per rule, updated rather than duplicated.
CREATE TABLE alerts (
    id              BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    rule_id         TEXT        NOT NULL REFERENCES alert_rules (id) ON DELETE CASCADE,
    state           TEXT        NOT NULL CHECK (state IN ('firing', 'acknowledged', 'resolved')),
    severity        TEXT        NOT NULL,
    summary         TEXT        NOT NULL,
    -- What to do about it. An alert that says what is wrong without saying what
    -- to do gets muted.
    guidance        TEXT        NOT NULL DEFAULT '',
    detail          JSONB       NOT NULL DEFAULT '{}'::jsonb,
    fired_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    acknowledged_at TIMESTAMPTZ,
    acknowledged_by TEXT,
    resolved_at     TIMESTAMPTZ,
    -- Rate limits notification. A flapping condition should send one message,
    -- not four hundred.
    notified_at     TIMESTAMPTZ
);

CREATE UNIQUE INDEX alerts_one_live_per_rule ON alerts (rule_id) WHERE state <> 'resolved';
CREATE INDEX alerts_recent_idx ON alerts (fired_at DESC);
CREATE INDEX alerts_live_idx ON alerts (state, severity) WHERE state <> 'resolved';

-- Password authentication, replacing sign-in by emailed link.
--
-- The console no longer needs an email provider to let anyone in. Resend
-- remains, but only to deliver alerts.

ALTER TABLE users
    -- Nullable on purpose. An account that has never had a password set must be
    -- unable to sign in at all, rather than signing in with an empty one. Every
    -- account that exists at migration time is in exactly that state, including
    -- the bootstrap administrator, until `s3d user set-password` is run.
    ADD COLUMN password_hash        BYTEA,
    ADD COLUMN password_set_at      TIMESTAMPTZ,
    -- Set when an administrator chooses the password, cleared when the user
    -- replaces it with one only they know.
    ADD COLUMN must_change_password BOOLEAN NOT NULL DEFAULT false;

COMMENT ON TABLE users IS 'Console operators. Authentication is by email and password; password_hash is null until one is set, and such an account cannot sign in.';

-- Failed sign-in attempts, which is what bounds password guessing.
--
-- Recorded per address and per client address separately. Throttling only on
-- the account would let an attacker lock a real user out by guessing at them;
-- throttling only on the IP would let a botnet spread the guessing thin enough
-- to never trip it.
CREATE TABLE login_attempts (
    id         BIGSERIAL   PRIMARY KEY,
    email      TEXT        NOT NULL CHECK (email = lower(email)),
    ip         INET,
    at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    successful BOOLEAN     NOT NULL
);

-- Both lookups ask the same question: how many failures recently. Partial on
-- failure because successes are never counted, and the table is dominated by
-- them on a healthy deployment.
CREATE INDEX login_attempts_email_idx ON login_attempts (email, at DESC) WHERE NOT successful;
CREATE INDEX login_attempts_ip_idx    ON login_attempts (ip, at DESC)    WHERE NOT successful;

-- Console-managed settings that are not per-bucket: currently the alert email
-- channel, which used to be read once from the environment at startup.
--
-- A single row, enforced by the primary key, so there is never a question of
-- which row is live.
CREATE TABLE app_settings (
    id         BOOLEAN     PRIMARY KEY DEFAULT true CHECK (id),
    -- Encrypted with the same cipher as S3 secret keys. A settings table that
    -- holds a provider API key in plain text is a database dump away from
    -- being someone else's mail account.
    resend_api_key   BYTEA,
    resend_from      TEXT NOT NULL DEFAULT '',
    resend_enabled   BOOLEAN NOT NULL DEFAULT false,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by UUID REFERENCES users (id) ON DELETE SET NULL
);

INSERT INTO app_settings (id) VALUES (true) ON CONFLICT DO NOTHING;

-- magic_links and invites are left in place deliberately.
--
-- Both are unreachable from this release onwards, but dropping them here would
-- mean a rollback to the previous image meets a schema that image cannot use.
-- See the compatibility rule in docs/releasing.md; they are dropped a release
-- later, once rolling back past this point is no longer supported.
COMMENT ON TABLE magic_links IS 'Unused from 0008. Sign-in is by password; retained one release for rollback compatibility.';
COMMENT ON TABLE invites IS 'Unused from 0008. Users are created with a password by an administrator; retained one release for rollback compatibility.';

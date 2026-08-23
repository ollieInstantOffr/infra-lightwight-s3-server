-- What an access key is allowed to do.
--
-- Until now a key was all or nothing: every key could read, write and delete
-- every bucket. That is tolerable for one application and indefensible the
-- moment a second consumer exists, because the key handed to it is a key to
-- everything.

-- The scope is stored as a document rather than as a join table. It is read on
-- every single S3 request as part of credential lookup, it is always read whole,
-- and it is never queried across keys — so a row that arrives with the
-- credential costs nothing extra, while a join would add one to the hottest
-- path in the system.
--
-- The default is deliberate and is the reason this migration is safe to apply
-- to a running deployment: every existing key stays unrestricted, so an upgrade
-- changes nobody's access. Narrowing is opt-in, per key.
--
-- Shape:
--   {"unrestricted": true}
--   {"unrestricted": false,
--    "rules": [{"bucket": "assets-prod", "prefix": "", "permissions": ["read"]}]}
--
-- An empty prefix means the whole bucket. A rules array that is empty or absent
-- with unrestricted false is a key that can do nothing, which is a valid thing
-- to want — it is how a key is disabled without destroying the record of it.
ALTER TABLE credentials
    ADD COLUMN access_scope JSONB NOT NULL DEFAULT '{"unrestricted": true}'::jsonb;

-- Cheap insurance against a malformed document reaching the authorizer. The
-- authorizer denies on anything it cannot parse, but a scope that cannot be
-- written in the first place is one fewer way to be surprised.
ALTER TABLE credentials
    ADD CONSTRAINT credentials_access_scope_shape
    CHECK (jsonb_typeof(access_scope) = 'object'
           AND jsonb_typeof(access_scope -> 'unrestricted') = 'boolean');

# Changelog

What changed in each release, in terms of what it means to somebody running
Pail. Versioning is [semantic](docs/releasing.md): MAJOR breaks a deployment,
MINOR adds capability, PATCH fixes without adding.

**Upgrade notes** is the section that matters. It carries anything needing
action, and says when rolling back is not simply redeploying the previous
image.

## Unreleased

### Console authentication is now email and password

Signing in no longer emails a link. The console previously could not let anyone
in at all when the mail provider was unreachable or unconfigured, which is a
poor property for the tool you reach for when something is wrong.

- Sign in with an email address and a password.
- Administrators create users with a starting password shown once, instead of
  sending invitations. The user must replace it on first sign-in, so it stops
  being a password two people know.
- `s3d user list`, `s3d user set-password` and `s3d user promote` run on the
  host and work when nobody can sign in at all.
- Sign-in attempts are throttled per address and per client address.
- Resend is no longer part of signing in. It now carries alert notifications
  only, and is configured in the console under **Settings** rather than by
  environment variable — so a rejected API key can be corrected at the moment
  alerts stop arriving, without editing `.env` and redeploying.

### Separately restartable services

Pail can now run as three containers instead of one, so `docker compose restart
console` leaves the S3 API serving.

- `ROLE` selects what a process runs: `all` (the default), `s3`, `console` or
  `worker`. One binary and one image, so nothing about the build or the release
  changes.
- `docker-compose.split.yml` is an overlay that runs a container per role.
- The alert engine now holds an advisory lock across an evaluation cycle. It
  sent notifications before marking them sent, so two engines — which any
  restart produces briefly — would both send the same email.
- Each role is validated and configured for what it actually does. The worker
  holds no session secret and no administrator address; the S3 API holds
  neither of those nor any console URL.

Nothing is required of anyone running the single container. It is still the
default, and switching is adding or removing an overlay file.

See **docs/services.md** for what was split and what deliberately was not.

### Upgrade notes

**Nobody can sign in until a password is set.** No account has one after the
migration, including the bootstrap administrator, and the sign-in screen will
reject every attempt until you run:

```bash
docker compose exec s3d s3d user set-password you@example.com
```

The server logs a warning naming this command on every start until it is done,
and the sign-in screen shows it rather than an unexplained failure.

Pending invitations stop working. Anyone who had one but never accepted it
needs an account creating for them.

`RESEND_API_KEY` and `RESEND_FROM` are still read, as the initial value for the
alert email setting, so alert delivery survives the upgrade untouched. Once the
setting is saved in the console, the stored value wins and the environment is
ignored.

**Rolling back to 1.0.0 works.** `magic_links` and `invites` are left in place
for one release precisely so the previous image still finds the schema it
expects. They are dropped in a later release, after which rolling back past
this point is no longer supported.

## 1.0.0

The first tagged release. Everything below already existed; this is the point
it acquired a version number.

### Storage

- S3 API: objects, listing (v1 and v2), multipart, server-side copy, range and
  conditional requests, batch delete.
- AWS Signature Version 4, including `aws-chunked` streaming bodies and
  presigned URLs. SigV2 is refused with an error saying how to fix the client.
- Buckets with public read, CORS and lifecycle expiry.
- Versioning through the S3 API: three states, `ListObjectVersions` with delete
  markers, and `?versionId` on GET, HEAD, DELETE, batch delete and copy source.
- Content-addressed blob store with reference counting and a sweeper.

### Access

- Passwordless console sign-in by magic link, users and invitations.
- S3 access keys scoped per key: which buckets, optionally narrowed to a
  prefix, and read / write / delete on them.

### Operating it

- Console: object browser with previews and drag-and-drop upload, version
  history with restore, an audit log, and a system health screen.
- Request logs with the reason each request failed, grouped into named causes.
- Alerts on error rate, disk space, authentication failures, write failures and
  missing credentials, in the console and by email.
- Prometheus metrics at `/metrics`, with a Grafana dashboard and alerting rules
  in [docs/monitoring.md](docs/monitoring.md).

### Upgrade notes

None — this is the first release.

Two things to have in place before running it in earnest:

- **Back up `CREDENTIALS_KEY`.** It is in neither the database dump nor the
  object archive. Without it every stored S3 secret is undecryptable and all
  access keys have to be reissued.
- **Back up both halves.** The data volume holds object bytes and Postgres
  holds every record of what those bytes are. Neither is usable without the
  other. See the Backups section of the README.

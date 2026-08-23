# Changelog

What changed in each release, in terms of what it means to somebody running
Pail. Versioning is [semantic](docs/releasing.md): MAJOR breaks a deployment,
MINOR adds capability, PATCH fixes without adding.

**Upgrade notes** is the section that matters. It carries anything needing
action, and says when rolling back is not simply redeploying the previous
image.

## Unreleased

Nothing yet.

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

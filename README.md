# Pail

S3-compatible object storage that runs on your own hardware. One container, one
Postgres, one disk.

It speaks enough of the S3 API that `aws s3 cp`, boto3 and the AWS SDKs work
against it unmodified, and it comes with a web console for buckets, objects,
access keys and users.

---

## What this is not

**There is one copy of every object.** No replication, no parity, no repair. A
lost disk is lost data.

That is a deliberate design decision, not a gap to be filled later. Pail is for
the cases where you want S3 semantics on hardware you control — a build cache,
an asset store, a staging environment, a backup target that is itself backed up
elsewhere. It is not a durable store of record, and nothing in it pretends
otherwise: the console says so on the dashboard.

If you need durability, put Pail's data volume on storage that provides it, or
use something else.

---

## Quick start

```bash
git clone https://github.com/ollieInstantOffr/infra-lightwight-s3-server.git
cd infra-lightwight-s3-server
./setup.sh
```

The script asks what it cannot work out — who administers it, whether this is
local or behind a reverse proxy, and where it will be reached — then generates
the secrets, writes `.env`, builds, starts, and offers to create your first
access key.

It is safe to re-run. `./setup.sh --configure` changes the configuration
without starting anything, and `--start` starts with the configuration you
already have. Re-running preserves `CREDENTIALS_KEY`, because replacing it
would silently invalidate every access key that already exists.

By default the console is on <http://localhost:8444> and the S3 API on
<http://localhost:8443>.

<details>
<summary>Configuring it by hand instead</summary>

```bash
cp .env.example .env
```

Four values have no sensible default:

| Variable            | What it is                                                     |
| ------------------- | -------------------------------------------------------------- |
| `ADMIN_EMAIL`       | The first administrator. Can always sign in, invites the rest. |
| `POSTGRES_PASSWORD` | Anything; it never leaves the compose network.                 |
| `SESSION_SECRET`    | Signs console session cookies. `openssl rand -base64 32`        |
| `CREDENTIALS_KEY`   | Encrypts S3 secret keys at rest. **Back this up.**              |

Then `docker compose up -d`.

</details>

Sign in with your `ADMIN_EMAIL`. Without a Resend key the sign-in link is
written to the log rather than emailed:

```bash
docker compose logs s3d | grep callback
```

---

## Connecting a client

Every client needs two things: the endpoint overridden, and **path-style
addressing** forced. Creating an access key in the console returns these
snippets already filled in.

**aws-cli**

```ini
# ~/.aws/config
[profile pail]
region = us-east-1
endpoint_url = https://s3.example.com
s3 =
    addressing_style = path
```

```bash
aws --profile pail s3 ls
```

**boto3**

```python
import boto3
from botocore.config import Config

s3 = boto3.client(
    "s3",
    endpoint_url="https://s3.example.com",
    aws_access_key_id="...",
    aws_secret_access_key="...",
    region_name="us-east-1",
    # Required. Against a custom endpoint botocore otherwise falls back to
    # the deprecated SigV2 when presigning, which Pail refuses.
    config=Config(signature_version="s3v4", s3={"addressing_style": "path"}),
)
```

**Go (SDK v2)**

```go
client := s3.New(s3.Options{
    Region:       "us-east-1",
    BaseEndpoint: aws.String("https://s3.example.com"),
    UsePathStyle: true,
    Credentials:  credentials.NewStaticCredentialsProvider(id, secret, ""),
})
```

**Node (SDK v3)**

```javascript
const s3 = new S3Client({
  region: "us-east-1",
  endpoint: "https://s3.example.com",
  forcePathStyle: true,
  credentials: { accessKeyId, secretAccessKey },
});
```

### Graphical clients

Cyberduck, Transmit, S3 Browser and similar mostly assume virtual-host style
addressing, and fail in a confusing way against a path-style deployment —
buckets list, then listing inside one reports a connection error because
`bucket.s3.example.com` does not resolve.

**[clients/README.md](clients/README.md)** covers them, and ships a Cyberduck
profile that fixes it in one double-click.

---

## What is implemented

**Objects** — PUT, GET, HEAD, DELETE, batch delete, server-side copy, range
requests, conditional requests, user metadata.

**Listing** — ListObjectsV2 and the original ListObjects, with prefix,
delimiter, pagination and `encoding-type=url`. Keys sort in UTF-8 byte order,
matching S3 exactly.

**Multipart** — the full lifecycle, with the composite ETag S3 clients expect.
Required for anything over about 8 MB, since the SDKs switch to it on their own.

**Auth** — AWS Signature Version 4, including `aws-chunked` streaming bodies
with signed and unsigned trailers. Presigned URLs for GET and PUT. SigV2 is
refused, with an error saying how to fix the client.

**Buckets** — create, delete, head, list, location, public read, CORS,
lifecycle expiry, versioning.

**Versioning** — the full S3 surface: `PutBucketVersioning` and
`GetBucketVersioning` with all three states, `ListObjectVersions` with delete
markers interleaved in history order, and `?versionId` on GET, HEAD, DELETE,
batch delete and as a copy source. A delete on a versioned bucket writes a
delete marker rather than removing anything, and deleting that marker brings
the object back.

**Access keys** — scoped per key: which buckets, optionally narrowed to a
prefix, and whether the key may read, write or delete. Keys are unrestricted
unless narrowed, so nothing changes until you choose to. A scoped key sees only
the buckets it can use, and listing a bucket shows only its own prefix.

**Console** — passwordless sign-in, an object browser with previews and
drag-and-drop upload, access keys, users and invitations, version history with
restore, an audit log, request metrics, and a system health screen.

### Not implemented

Object locking, replication, storage classes, requester-pays, ACLs (buckets are
either private or public read), server-side encryption, bucket policies, and
event notifications. Some of these would be reasonable to add; none of them are
faked.

Access keys are scoped, but there is no policy language: a key names buckets and
prefixes and holds read, write or delete on them. That covers giving one
application access to one bucket, which is what the absence of scoping actually
cost. It does not cover conditions, deny rules, or anything else IAM does.

---

## Deploying it properly

Pail serves plain HTTP and expects a reverse proxy in front of it, with a
hostname each for the S3 API and the console.

**[docs/reverse-proxy.md](docs/reverse-proxy.md)** covers Nginx Proxy Manager,
including the four settings whose defaults will otherwise leave you with a
working console and an S3 API that rejects every request.

For production, add the overlay:

```bash
docker compose -f docker-compose.yml -f docker-compose.prod.yml up -d
```

It binds the ports to loopback only, bounds the logs, and restarts always.

---

## Running more than one node

Pail is a single-node server today.

**[docs/scale-out.md](docs/scale-out.md)** scopes turning it into a distributed
one: replication and erasure coding chosen per bucket, one node to unbounded,
across multiple datacentres, with no single point of failure. It is roughly the
size of everything built so far, and the document says so plainly — along with
the observation that MinIO already does almost exactly that list.

---

## Backups

**You need both halves, and neither is usable without the other.**

The data volume holds object bytes. Postgres holds every record of what those
bytes *are* — which bucket, which key, which size. A volume without the database
is a directory of files named after their own hashes.

```bash
# Metadata
docker compose exec -T postgres pg_dump -U s3d s3d | gzip > pail-$(date +%F).sql.gz

# Objects
docker run --rm -v pail_objects:/data -v "$PWD":/backup alpine \
  tar czf /backup/pail-objects-$(date +%F).tar.gz -C /data .
```

Take the database dump *after* the object archive. Metadata describing an object
that is missing produces a clear error; an object with no metadata is invisible
and will eventually be swept.

**Also back up `CREDENTIALS_KEY`.** It is not in either dump. Without it, every
stored S3 secret is undecryptable and all access keys have to be reissued.

---

## Operating it

```bash
docker compose logs -f s3d          # follow the log
docker compose exec s3d s3d help    # the CLI
```

The binary is also its own admin tool, which is how you get back in if the
console is unreachable:

```bash
docker compose exec s3d s3d credential create "ci-deploy"
docker compose exec s3d s3d credential list
docker compose exec s3d s3d credential revoke AKIA...
```

`ADMIN_EMAIL` is re-promoted to administrator on every start, so an accidentally
removed admin is fixed by a restart.

The **System & health** screen in the console reports the node, disk, database
and configuration, and warns about the things that break a deployment quietly —
no email provider, no access keys, a filling volume.

---

## Development

```bash
make test-db-up    # Postgres for the tests
make test          # unit and integration tests
make test-compat   # aws-cli and boto3, in containers
make all           # build the console and the binary
```

The compatibility suite is the one that matters. Unit tests prove the
implementation against itself; `make test-compat` drives the real aws-cli and
boto3 against a running server, which is what decides whether clients work.

Run the server locally:

```bash
make test-db-up
ENV=development \
DATABASE_URL='postgres://s3d:test@localhost:55432/s3d?sslmode=disable' \
ADMIN_EMAIL=you@example.com DATA_DIR=./data \
  go run ./cmd/s3d
```

And the console with hot reload, proxying the API to that process:

```bash
cd web && npm run dev
```

### Layout

```
cmd/s3d/            entrypoint, subcommands, background sweeps
internal/config/    typed configuration, validated once at startup
internal/s3api/     the S3 protocol: SigV4, routing, XML, handlers
internal/console/   the console API: sessions, users, storage, audit
internal/db/        Postgres access and migrations
internal/storage/   the content-addressed blob store
internal/metrics/   in-memory request counters
web/                the console interface (React, Vite)
```

---

## How it works

Object bytes live on disk in a content-addressed store, keyed by SHA-256, so two
uploads of identical content share one file. Postgres holds only metadata.
Reference counting is transactional, and a background sweep reclaims blobs
nothing points at.

Everything streams. A 5 GB upload costs the same memory as a 5 KB one, because
nothing is ever held whole — not on upload, not on download, and not when
assembling a multipart object.

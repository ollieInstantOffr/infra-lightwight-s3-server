package s3api

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/db"
	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/httpx"
	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/secrets"
	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/storage"
)

// The Go SDK tests prove the protocol against one implementation. These drive
// the tools people actually use — aws-cli and boto3 — in containers, because
// neither is installed on a typical development machine, and because an
// implementation can satisfy every unit test and still be rejected by a real
// client.
//
// Set S3D_EXTERNAL_TESTS=1 to run them. They need Docker and pull images on
// first use, so they are opt-in rather than part of the default go test ./...

func skipUnlessExternal(t *testing.T) {
	t.Helper()
	if os.Getenv("S3D_EXTERNAL_TESTS") == "" {
		t.Skip("set S3D_EXTERNAL_TESTS=1 to run container-based client tests")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
}

// lockedBuffer collects log output from the server's goroutines.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// externalEndpoint is a fully assembled server reachable from a container.
type externalEndpoint struct {
	port        int
	accessKeyID string
	secretKey   string
}

// url is the endpoint as seen from inside a container.
func (e *externalEndpoint) url() string {
	return fmt.Sprintf("http://host.docker.internal:%d", e.port)
}

// startExternalServer runs the real S3 server — real routing, real storage,
// real database — on a port a container can reach.
func startExternalServer(t *testing.T, schema string) *externalEndpoint {
	t.Helper()

	dsn := testDSN(t, schema)
	ctx := context.Background()

	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	// The server's own log is captured rather than discarded. When a container
	// client reports a bare 403, the reason only exists on this side, and
	// without it the failure is undiagnosable from the test output alone.
	var logs lockedBuffer
	quiet := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("server log:\n%s", logs.String())
		}
	})

	if err := db.Migrate(ctx, pool, quiet); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	for _, stmt := range []string{`DELETE FROM buckets`, `DELETE FROM blobs`, `DELETE FROM credentials`} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("reset (%s): %v", stmt, err)
		}
	}

	cipher, err := secrets.NewCipher("external-test-credentials-key-32-chars-ok")
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	cred, err := db.CreateCredential(ctx, pool, cipher, "external client test", nil, db.UnrestrictedGrant())
	if err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}

	blobs, err := storage.New(t.TempDir())
	if err != nil {
		t.Fatalf("storage.New: %v", err)
	}
	trust, _ := httpx.NewProxyTrust(nil)

	// Bound to all interfaces so the container can reach it through
	// host.docker.internal.
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port

	server := &Server{
		DB: pool, Blobs: blobs, Log: quiet, Region: "us-east-1",
		PublicURL: fmt.Sprintf("http://host.docker.internal:%d", port),
		Verifier: &Verifier{
			Region: "us-east-1", Proxies: trust,
			Lookup: func(ctx context.Context, accessKeyID string) (KeyMaterial, error) {
				c, err := db.LookupCredential(ctx, pool, cipher, accessKeyID)
				if err != nil {
					return KeyMaterial{}, err
				}
				return KeyMaterial{SecretKey: c.SecretKey, Grant: c.Scope}, nil
			},
		},
	}

	httpSrv := &http.Server{Handler: server.Handler()}
	go func() { _ = httpSrv.Serve(listener) }()
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	})

	return &externalEndpoint{port: port, accessKeyID: cred.AccessKeyID, secretKey: cred.SecretKey}
}

// runInContainer executes a shell script inside an image, with the endpoint's
// credentials in the environment.
func runInContainer(t *testing.T, image string, endpoint *externalEndpoint, script string) string {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "docker", "run", "--rm",
		"--add-host=host.docker.internal:host-gateway",
		"-e", "AWS_ACCESS_KEY_ID="+endpoint.accessKeyID,
		"-e", "AWS_SECRET_ACCESS_KEY="+endpoint.secretKey,
		"-e", "AWS_DEFAULT_REGION=us-east-1",
		"-e", "AWS_REGION=us-east-1",
		"-e", "S3_ENDPOINT="+endpoint.url(),
		"--entrypoint", "/bin/sh",
		image, "-c", script)

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s failed: %v\n%s", image, err, out)
	}
	return string(out)
}

// TestAWSCLICompatibility exercises the high-level `aws s3` commands, which are
// what most people actually type. They wrap several API calls each, so a
// failure here catches interactions the per-call tests do not.
func TestAWSCLICompatibility(t *testing.T) {
	skipUnlessExternal(t)
	endpoint := startExternalServer(t, "test_s3api_awscli")

	out := runInContainer(t, "amazon/aws-cli:latest", endpoint, `
set -eu
E="--endpoint-url $S3_ENDPOINT"

# The aws-cli image carries no cmp or diff, so comparisons go through md5sum.
# A missing tool must fail the test rather than skip the check silently, which
# is exactly what "cmp ... && echo ok" did on the first run here.
same() {
  a=$(md5sum "$1" | cut -d" " -f1)
  b=$(md5sum "$2" | cut -d" " -f1)
  if [ "$a" != "$b" ]; then echo "MISMATCH: $1 ($a) vs $2 ($b)"; exit 1; fi
  echo "$3: byte identical"
}

echo "== mb / ls =="
aws $E s3 mb s3://compat
aws $E s3 ls | grep compat

echo "== small round trip =="
head -c 100000 /dev/urandom > /tmp/small.bin
aws $E s3 cp /tmp/small.bin s3://compat/small.bin
aws $E s3 cp s3://compat/small.bin /tmp/small.back
same /tmp/small.bin /tmp/small.back small

echo "== multipart round trip =="
head -c 40000000 /dev/urandom > /tmp/big.bin
aws $E s3 cp /tmp/big.bin s3://compat/big.bin --quiet
aws $E s3 cp s3://compat/big.bin /tmp/big.back --quiet
same /tmp/big.bin /tmp/big.back multipart

echo "== awkward keys =="
head -c 4096 /dev/urandom > /tmp/k.bin
aws $E s3 cp /tmp/k.bin "s3://compat/dir with spaces/ünïcødé & symbols (v2).txt"
aws $E s3 cp "s3://compat/dir with spaces/ünïcødé & symbols (v2).txt" /tmp/k.back
same /tmp/k.bin /tmp/k.back "awkward key"

echo "== sync up and down =="
mkdir -p /tmp/tree/nested
for i in $(seq 1 25); do head -c 2048 /dev/urandom > /tmp/tree/f$i.bin; done
head -c 2048 /dev/urandom > /tmp/tree/nested/deep.bin
aws $E s3 sync /tmp/tree s3://compat/synced --quiet
mkdir -p /tmp/down
aws $E s3 sync s3://compat/synced /tmp/down --quiet

# Compare every file in the tree, and the file count, so a sync that silently
# dropped a file is caught rather than passing on the survivors.
up=$(find /tmp/tree -type f | wc -l)
down=$(find /tmp/down -type f | wc -l)
if [ "$up" != "$down" ]; then echo "SYNC COUNT MISMATCH: $up up, $down down"; exit 1; fi
for f in $(cd /tmp/tree && find . -type f | sort); do
  same "/tmp/tree/$f" "/tmp/down/$f" "sync $f" > /dev/null
done
echo "sync: $up files, all byte identical"

echo "== delimiter listing =="
aws $E s3 ls s3://compat/
echo "== recursive listing =="
aws $E s3 ls s3://compat/ --recursive | wc -l

echo "== presign =="
URL=$(aws $E s3 presign s3://compat/small.bin --expires-in 300)
curl -sf "$URL" -o /tmp/presigned.bin
same /tmp/small.bin /tmp/presigned.bin presigned

echo "== copy =="
aws $E s3 cp s3://compat/small.bin s3://compat/copied.bin
aws $E s3api head-object --bucket compat --key copied.bin > /dev/null
aws $E s3 cp s3://compat/copied.bin /tmp/copied.back
same /tmp/small.bin /tmp/copied.back copy

echo "== rm --recursive / rb =="
aws $E s3 rm s3://compat --recursive --quiet
remaining=$(aws $E s3 ls s3://compat/ --recursive | wc -l)
if [ "$remaining" != "0" ]; then echo "BUCKET NOT EMPTIED: $remaining objects remain"; exit 1; fi
aws $E s3 rb s3://compat
echo "AWS CLI COMPATIBILITY OK"
`)
	t.Log(out)
}

// TestBoto3Compatibility covers the Python client, including a paginator over
// enough keys to cross several pages — the case where a cursor bug shows up.
func TestBoto3Compatibility(t *testing.T) {
	skipUnlessExternal(t)
	endpoint := startExternalServer(t, "test_s3api_boto3")

	out := runInContainer(t, "python:3.12-slim", endpoint, `
set -e
pip install --quiet boto3
python3 - <<'PY'
import hashlib, io, os
import boto3
from botocore.config import Config

s3 = boto3.client(
    "s3",
    endpoint_url=os.environ["S3_ENDPOINT"],
    # signature_version matters: against a custom endpoint botocore otherwise
    # falls back to the deprecated SigV2 when presigning, which this server
    # refuses (with an error saying exactly this).
    config=Config(signature_version="s3v4",
                  s3={"addressing_style": "path"},
                  retries={"max_attempts": 2}),
)

s3.create_bucket(Bucket="boto")
print("== created bucket ==")

# Round trip with checksum.
payload = os.urandom(250_000)
s3.put_object(Bucket="boto", Key="round/trip.bin", Body=payload)
got = s3.get_object(Bucket="boto", Key="round/trip.bin")["Body"].read()
assert got == payload, "round trip corrupted the object"
print("round trip: byte identical")

# ETag must be the MD5 for a single-part upload.
head = s3.head_object(Bucket="boto", Key="round/trip.bin")
assert head["ETag"].strip('"') == hashlib.md5(payload).hexdigest(), "ETag is not the MD5"
print("etag: matches md5")

# Managed multipart upload.
big = os.urandom(30_000_000)
s3.upload_fileobj(io.BytesIO(big), "boto", "big.bin")
back = s3.get_object(Bucket="boto", Key="big.bin")["Body"].read()
assert back == big, "multipart round trip corrupted the object"
print("multipart: byte identical")

# Metadata and content type.
s3.put_object(Bucket="boto", Key="meta.txt", Body=b"x",
              ContentType="text/plain", Metadata={"author": "ollie"})
head = s3.head_object(Bucket="boto", Key="meta.txt")
assert head["ContentType"] == "text/plain", head["ContentType"]
assert head["Metadata"]["author"] == "ollie", head["Metadata"]
print("metadata: round tripped")

# Paginate over enough keys to cross many pages.
TOTAL = 2500
for start in range(0, TOTAL, 500):
    for i in range(start, min(start + 500, TOTAL)):
        s3.put_object(Bucket="boto", Key=f"page/{i:05d}.txt", Body=b"x")

seen = set()
paginator = s3.get_paginator("list_objects_v2")
pages = 0
for page in paginator.paginate(Bucket="boto", Prefix="page/", PaginationConfig={"PageSize": 113}):
    pages += 1
    for obj in page.get("Contents", []):
        assert obj["Key"] not in seen, f"duplicate key {obj['Key']}"
        seen.add(obj["Key"])
assert len(seen) == TOTAL, f"saw {len(seen)} of {TOTAL} keys across {pages} pages"
print(f"paginator: {TOTAL} keys over {pages} pages, no gaps or duplicates")

# Delimiter grouping.
listing = s3.list_objects_v2(Bucket="boto", Delimiter="/")
prefixes = {p["Prefix"] for p in listing.get("CommonPrefixes", [])}
assert prefixes == {"page/", "round/"}, prefixes
print("delimiter: folders grouped correctly")

# Presigned URL, fetched with no credentials.
import urllib.request
url = s3.generate_presigned_url("get_object",
                                Params={"Bucket": "boto", "Key": "meta.txt"}, ExpiresIn=300)
assert urllib.request.urlopen(url).read() == b"x"
print("presigned: fetched without credentials")

# A SigV2 presigned URL must be refused with an actionable message rather than
# a bare "not signed".
v2 = boto3.client("s3", endpoint_url=os.environ["S3_ENDPOINT"],
                  config=Config(signature_version="s3", s3={"addressing_style": "path"}))
v2_url = v2.generate_presigned_url("get_object",
                                   Params={"Bucket": "boto", "Key": "meta.txt"}, ExpiresIn=300)
try:
    urllib.request.urlopen(v2_url)
    raise AssertionError("a SigV2 presigned URL was accepted")
except urllib.error.HTTPError as e:
    detail = e.read().decode()
    assert "Signature Version 4" in detail, detail
    print("sigv2: refused with an actionable message")

# Batch delete.
keys = [{"Key": f"page/{i:05d}.txt"} for i in range(1000)]
resp = s3.delete_objects(Bucket="boto", Delete={"Objects": keys})
assert not resp.get("Errors"), resp.get("Errors")
print("batch delete: 1000 keys in one call")

print("BOTO3 COMPATIBILITY OK")
PY
`)
	t.Log(out)
}

// TestVersioningCompatibility drives the versioning surface with boto3.
//
// Versioning semantics are where a home-grown S3 implementation usually
// diverges quietly: delete markers behave unlike anything in a plain object
// store, and a listing that groups versions rather than interleaving them looks
// right until a client walks it. Nothing but a real client checks this
// convincingly.
func TestVersioningCompatibility(t *testing.T) {
	skipUnlessExternal(t)
	endpoint := startExternalServer(t, "test_s3api_versioning")

	out := runInContainer(t, "python:3.12-slim", endpoint, `
set -e
pip install --quiet boto3
python3 - <<'PY'
import os
import boto3
from botocore.config import Config
from botocore.exceptions import ClientError

s3 = boto3.client(
    "s3",
    endpoint_url=os.environ["S3_ENDPOINT"],
    config=Config(signature_version="s3v4",
                  s3={"addressing_style": "path"},
                  retries={"max_attempts": 2}),
)

B = "versioned"
s3.create_bucket(Bucket=B)

# An object written before versioning was enabled. Real buckets are full of
# these, and S3 gives them the literal version id "null".
s3.put_object(Bucket=B, Key="preexisting", Body=b"old")

status = s3.get_bucket_versioning(Bucket=B)
assert "Status" not in status, f"a never-versioned bucket reported a status: {status}"
print("== unversioned bucket omits Status ==")

s3.put_bucket_versioning(Bucket=B, VersioningConfiguration={"Status": "Enabled"})
assert s3.get_bucket_versioning(Bucket=B)["Status"] == "Enabled"
print("== versioning enabled ==")

# Three writes to one key.
ids = []
for body in (b"one", b"two", b"three"):
    ids.append(s3.put_object(Bucket=B, Key="k", Body=body)["VersionId"])
assert len(set(ids)) == 3, f"version ids were not distinct: {ids}"
print("== three distinct version ids ==")

# Every version is addressable, and still holds its own bytes.
for version, body in zip(ids, (b"one", b"two", b"three")):
    got = s3.get_object(Bucket=B, Key="k", VersionId=version)["Body"].read()
    assert got == body, f"version {version} returned {got!r}, want {body!r}"
print("== each version returns its own bytes ==")

# The pre-existing object is the null version.
listing = s3.list_object_versions(Bucket=B, Prefix="preexisting")
assert listing["Versions"][0]["VersionId"] == "null", listing["Versions"][0]
print("== pre-versioning object reports the null version ==")

# Ordering: newest first within a key, exactly one IsLatest.
listing = s3.list_object_versions(Bucket=B, Prefix="k")
versions = listing["Versions"]
assert [v["VersionId"] for v in versions] == list(reversed(ids)), \
    f"versions not newest-first: {[v['VersionId'] for v in versions]} vs {list(reversed(ids))}"
latest = [v for v in versions if v["IsLatest"]]
assert len(latest) == 1 and latest[0]["VersionId"] == ids[-1], latest
print("== versions ordered newest first, one marked latest ==")

# A delete with no version id writes a marker. Nothing is removed.
deleted = s3.delete_object(Bucket=B, Key="k")
assert deleted.get("DeleteMarker") is True, f"delete did not report a marker: {deleted}"
marker = deleted["VersionId"]

try:
    s3.get_object(Bucket=B, Key="k")
    raise AssertionError("the object is still readable after a versioned delete")
except ClientError as e:
    assert e.response["Error"]["Code"] in ("NoSuchKey", "404"), e.response["Error"]

# ...but the data is still there.
assert s3.get_object(Bucket=B, Key="k", VersionId=ids[0])["Body"].read() == b"one"
print("== delete wrote a marker and removed nothing ==")

# The marker appears in the listing as a DeleteMarker, not a Version.
listing = s3.list_object_versions(Bucket=B, Prefix="k")
markers = listing.get("DeleteMarkers", [])
assert any(m["VersionId"] == marker and m["IsLatest"] for m in markers), listing
print("== the marker is listed as a delete marker and is latest ==")

# Deleting the marker brings the object back. This is the behaviour with no
# counterpart in a plain delete.
s3.delete_object(Bucket=B, Key="k", VersionId=marker)
assert s3.get_object(Bucket=B, Key="k")["Body"].read() == b"three"
print("== deleting the marker restored the object ==")

# Copying an old version over the key promotes it: the API way to restore.
s3.copy_object(Bucket=B, Key="k",
               CopySource={"Bucket": B, "Key": "k", "VersionId": ids[0]})
assert s3.get_object(Bucket=B, Key="k")["Body"].read() == b"one"
print("== copying an old version restored it as current ==")

# Deleting a version by id destroys it for real.
s3.delete_object(Bucket=B, Key="k", VersionId=ids[1])
try:
    s3.get_object(Bucket=B, Key="k", VersionId=ids[1])
    raise AssertionError("a permanently deleted version is still readable")
except ClientError as e:
    assert e.response["Error"]["Code"] in ("NoSuchVersion", "NoSuchKey", "404"), e.response["Error"]
print("== deleting a version by id destroyed it ==")

# Paging, with a boundary that falls inside one key's history.
for i in range(7):
    s3.put_object(Bucket=B, Key="paged", Body=bytes([i]))

seen, token = [], {}
for _ in range(20):
    page = s3.list_object_versions(Bucket=B, Prefix="paged", MaxKeys=2, **token)
    seen += [v["VersionId"] for v in page.get("Versions", [])]
    if not page["IsTruncated"]:
        break
    token = {"KeyMarker": page["NextKeyMarker"],
             "VersionIdMarker": page["NextVersionIdMarker"]}
assert len(seen) == 7, f"paged through {len(seen)} versions, want 7"
assert len(set(seen)) == 7, "a version was returned on more than one page"
print("== paged through a key's history without gaps or repeats ==")

# Suspension keeps what exists and stops making more.
s3.put_bucket_versioning(Bucket=B, VersioningConfiguration={"Status": "Suspended"})
assert s3.get_bucket_versioning(Bucket=B)["Status"] == "Suspended"
before = {v["VersionId"] for v in s3.list_object_versions(Bucket=B, Prefix="paged").get("Versions", [])}

# The first suspended write takes the null id and becomes current. The version
# that was current keeps its real id and is preserved, so the count goes up by
# one — suspending stops new versions being made, it does not discard the one
# it finds.
s3.put_object(Bucket=B, Key="paged", Body=b"suspended-one")
after = s3.list_object_versions(Bucket=B, Prefix="paged").get("Versions", [])
ids = {v["VersionId"] for v in after}
assert before <= ids, f"suspending lost versions: {before - ids}"
assert "null" in ids, f"the suspended write did not take the null version: {ids}"
latest = [v for v in after if v["IsLatest"]]
assert len(latest) == 1 and latest[0]["VersionId"] == "null", latest
print("== suspended write takes the null version and preserves the rest ==")

# The second one replaces the null version rather than adding another. There
# can only be one, which is the rule that makes suspension unsafe as a way to
# pause history: the intermediate state is gone.
s3.put_object(Bucket=B, Key="paged", Body=b"suspended-two")
final = s3.list_object_versions(Bucket=B, Prefix="paged").get("Versions", [])
assert len(final) == len(after), \
    f"a second suspended write added a version instead of replacing: {len(after)} -> {len(final)}"
assert sum(1 for v in final if v["VersionId"] == "null") == 1, final
assert s3.get_object(Bucket=B, Key="paged")["Body"].read() == b"suspended-two"
print("== a second suspended write replaced the null version ==")

print("VERSIONING OK")
PY
`)

	if !strings.Contains(out, "VERSIONING OK") {
		t.Fatalf("versioning compatibility failed:\n%s", out)
	}
	t.Log(out)
}

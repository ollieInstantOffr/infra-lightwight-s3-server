# Putting Pail behind Nginx Proxy Manager

Pail serves plain HTTP on two ports and expects a reverse proxy in front of it.
TLS terminates at the proxy; the app never sees a certificate.

Two ports means two proxy hosts:

| Proxy host              | Forwards to    | What it serves                    |
| ----------------------- | -------------- | --------------------------------- |
| `s3.example.com`        | `s3d:9000`     | The S3 API — SDKs, aws-cli, links |
| `console.example.com`   | `s3d:9001`     | The web console                   |

They are separate because a bucket can be named anything, including `assets` or
`api`. On one hostname, a bucket would eventually collide with a console route.

---

## The settings that matter

Nginx Proxy Manager's defaults will get you a working console and a **broken S3
API**. Four things need changing, and each has a specific failure mode if you
skip it.

### 1. Upload size — otherwise uploads fail at 1 MB

Under **Advanced → Custom Nginx Configuration** for the S3 proxy host:

```nginx
client_max_body_size 0;
```

`0` means no limit. Nginx defaults to 1 MB, and anything larger is rejected
with a `413` before it ever reaches Pail. Multipart parts are typically 8 MB or
more, so this affects essentially every real upload.

### 2. Request buffering — otherwise large uploads fill the proxy's disk

```nginx
proxy_request_buffering off;
proxy_buffering off;
```

By default nginx reads the entire request body to its own disk before
forwarding a byte. A 50 GB upload becomes a 50 GB temporary file on the proxy,
and the client waits through the whole thing before Pail even starts. With
buffering off, the upload streams through, which is what Pail is built for — it
never holds an object in memory either.

### 3. Timeouts — otherwise long transfers are cut off

```nginx
proxy_read_timeout 3600s;
proxy_send_timeout 3600s;
```

The default is 60 seconds of silence. A slow client uploading a large object
can exceed that between chunks, and nginx will close the connection mid-transfer.

### 4. Forwarded headers — otherwise every signed request fails

Nginx Proxy Manager sets these already, but confirm they are present:

```nginx
proxy_set_header Host              $host;
proxy_set_header X-Forwarded-Host  $host;
proxy_set_header X-Forwarded-Proto $scheme;
proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
```

This is the one that produces the most confusing failure. **SigV4 signs the
`Host` header.** The client signs `s3.example.com`; if Pail sees only the
upstream address, it computes a different signature and rejects the request with
`SignatureDoesNotMatch` — which reads like a credentials problem and is not.

Pail only believes these headers from addresses in `TRUSTED_PROXIES`, so that an
outside caller cannot spoof the hostname and get a signature validated against a
host the client never used. Set that variable to your proxy's address range.

---

## Complete custom configuration

For the **S3** proxy host (`s3.example.com`):

```nginx
client_max_body_size 0;
proxy_request_buffering off;
proxy_buffering off;
proxy_read_timeout 3600s;
proxy_send_timeout 3600s;
```

For the **console** proxy host (`console.example.com`):

```nginx
client_max_body_size 512m;
proxy_read_timeout 300s;
```

The console's own upload limit is 512 MB, so matching it here means an oversized
file is refused by Pail with a clear message rather than by nginx with a bare
`413`.

---

## Checking it works

From outside the network:

```bash
aws --endpoint-url https://s3.example.com s3 ls
```

If that returns your buckets, signing and forwarding are both correct.

A common half-working state is that the console loads but the S3 API returns
`SignatureDoesNotMatch`. That is almost always `X-Forwarded-Host`, either
missing or arriving from an address outside `TRUSTED_PROXIES`. The server logs
the reason on every rejected request:

```bash
docker compose logs s3d | grep "authentication failed"
```

---

## Virtual-host style addressing (optional)

Path style — `s3.example.com/mybucket/key` — works everywhere and needs nothing
extra. Virtual-host style — `mybucket.s3.example.com/key` — is what AWS itself
uses, and some tools assume it.

To enable it:

1. Add a wildcard DNS record: `*.s3.example.com` → your proxy.
2. Get a wildcard certificate for `*.s3.example.com`. Nginx Proxy Manager can do
   this with a DNS-01 challenge; HTTP-01 cannot issue wildcards.
3. Add `s3.example.com` as a proxy host with the domain `*.s3.example.com`.
4. Set `S3_DOMAIN=s3.example.com` in your `.env` and restart.

Only a single label counts as a bucket. `a.b.s3.example.com` is not bucket
`a.b`, because each dot is a DNS label and a wildcard certificate covers exactly
one level — such a name could never be reached over TLS anyway.

---

## Public buckets

A bucket set to public read serves anonymous `GET` and `HEAD` through the S3
proxy host. No extra proxy configuration is needed. Anonymous writes are refused
regardless of any setting, so a public bucket cannot become an open file drop.

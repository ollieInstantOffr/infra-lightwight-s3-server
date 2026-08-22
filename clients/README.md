# Connecting other S3 clients

`aws-cli`, boto3 and the AWS SDKs are covered in the main README. This is for
the graphical clients, which mostly need one thing.

## The addressing problem

S3 has two ways of naming a bucket:

| Style | Request | Needs |
| ----- | ------- | ----- |
| Path | `s3.example.com/bucket/key` | nothing |
| Virtual-host | `bucket.s3.example.com/key` | a wildcard DNS record and certificate |

Pail supports both, but virtual-host style is off unless you set `S3_DOMAIN`.
Many graphical clients assume virtual-host style, because that is what AWS
itself uses.

**The failure is misleading.** Listing buckets works, because that request has
no bucket in the hostname. Listing *inside* a bucket then fails with something
like "the connection attempt was rejected" — the client is asking for
`bucket.s3.example.com`, which does not resolve, so nothing reaches the server
at all. Pail's log stays silent, which sends people looking at the proxy.

You can fix this on either side.

---

## Cyberduck and Mountain Duck

Double-click **[Pail.cyberduckprofile](Pail.cyberduckprofile)** to install it,
then create a bookmark using the "Pail (S3, path style)" connection type:

| Field | Value |
| ----- | ----- |
| Server | `s3.example.com` |
| Access Key ID | from the console's Access keys screen |
| Secret Access Key | shown once, at creation |

The profile is the built-in Amazon S3 one with path-style addressing forced. The
hostname is configurable, so the same file works for any deployment.

There is a hidden preference that does the same thing:

```bash
defaults write ch.sudo.cyberduck s3.bucket.virtualhost.disable true
```

It is unreliable across versions and applies to every S3 bookmark you have,
including real AWS ones. Prefer the profile.

## Other clients

**Transmit** — in the S3 server settings, enable "Use path-style addressing".

**Rclone** — path style is the default for a custom endpoint:

```ini
[pail]
type = s3
provider = Other
endpoint = https://s3.example.com
access_key_id = ...
secret_access_key = ...
region = us-east-1
force_path_style = true
```

**S3 Browser (Windows)** — choose "S3 Compatible Storage" when adding the
account, not "Amazon S3", and tick "Use path-style requests".

**WinSCP** — the S3 protocol uses path style against a custom endpoint already.

---

## Or turn on virtual-host style instead

If you would rather not configure each client, make the server match what they
expect:

1. Add a wildcard DNS record: `*.s3.example.com` → your proxy, alongside the
   existing `s3` record.
2. Add `*.s3.example.com` to the proxy host's domain names.
3. Issue the certificate with a **DNS-01** challenge. HTTP-01 cannot produce
   wildcards.
4. Set `S3_DOMAIN=s3.example.com` and restart.

Path style keeps working; this only adds the second form.

One limit worth knowing: only a single label counts as a bucket, so
`my-bucket.s3.example.com` works but a bucket with a dot in its name does not.
Each dot is a DNS label, and a wildcard certificate covers exactly one level —
such a name could never be reached over TLS anyway.

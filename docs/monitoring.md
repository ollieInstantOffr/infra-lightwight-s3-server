# Monitoring Pail

Pail exposes Prometheus metrics at `/metrics` on the console port.

It also has alerts of its own, on the Alerts screen, which email administrators
when something is wrong. The two overlap deliberately and it is worth deciding
which you want to be the source of truth before configuring both — see
[Which alerts where](#which-alerts-where) at the end.

---

## Setting up the scrape

The endpoint is authenticated. Metrics are not neutral: bucket names, object
counts, traffic volume and error patterns together describe who is using the
system and how much, so an open `/metrics` is a quiet information leak.

Set a token and restart:

```bash
# Add to .env
METRICS_TOKEN=$(openssl rand -hex 32)
```

Then point Prometheus at the console hostname:

```yaml
scrape_configs:
  - job_name: pail
    metrics_path: /metrics
    scheme: https
    static_configs:
      - targets: ["console.example.com"]
    authorization:
      type: Bearer
      credentials: "the value of METRICS_TOKEN"
```

With no token configured the endpoint still exists, but only a signed-in
administrator can read it. That is the safe default rather than a broken one:
you can open `/metrics` in a browser to see what is there before deciding to
provision a token, and nothing is exposed in the meantime.

The token authorises a scrape and nothing else. It is not accepted anywhere
else on the console.

### Checking it works

```bash
curl -H "Authorization: Bearer $METRICS_TOKEN" https://console.example.com/metrics
```

A 401 means the token does not match. A 404 means the binary predates this
feature.

---

## What is exported

| Metric | Type | Labels | What it is for |
| --- | --- | --- | --- |
| `pail_requests_total` | counter | `surface`, `operation`, `status` | Request rate and error rate |
| `pail_request_duration_seconds` | histogram | `surface`, `operation` | Latency, including the tail |
| `pail_received_bytes_total` | counter | `surface` | Ingest throughput |
| `pail_sent_bytes_total` | counter | `surface` | Egress throughput |
| `pail_bucket_objects` | gauge | `bucket` | Growth, per bucket |
| `pail_bucket_bytes` | gauge | `bucket` | Growth, per bucket |
| `pail_disk_free_bytes` | gauge | — | Capacity headroom |
| `pail_disk_total_bytes` | gauge | — | Capacity headroom |
| `pail_alerts_firing` | gauge | `rule` | What Pail itself thinks is wrong |
| `pail_log_entries_dropped_total` | counter | — | Whether the request log has holes |
| `pail_up_database` | gauge | — | Whether the metadata store answered |
| `pail_uptime_seconds` | gauge | — | Restarts |
| `pail_build_info` | gauge | `version` | Which build is running |

`surface` is `s3` or `console`. `operation` is the S3 call name — `GetObject`,
`PutObject`, `ListObjectsV2` and so on — or `Unknown` for a request rejected
before routing, which is where a bad signature or a denied access scope lands.
`status` is a class (`2xx`, `4xx`) rather than a code.

**On cardinality.** Every label here is drawn from a fixed set or from something
an operator creates. There is deliberately no label for object key, access key
or client address: those grow with traffic rather than with configuration, and a
label that grows without bound eventually takes the monitoring down rather than
the storage.

### A note on `pail_up_database`

A scrape does not fail when the database is unreachable. It answers with the
request counters it holds in memory and sets `pail_up_database` to 0. The moment
the database is down is the moment someone most wants their monitoring to say
something, and a 500 says nothing.

---

## A starting dashboard

[`grafana-dashboard.json`](grafana-dashboard.json) covers request rate, error
rate, latency percentiles, throughput and capacity. Import it in Grafana and
select your Prometheus data source.

---

## Alerting rules

These are a starting point rather than a recommendation — thresholds depend on
what your deployment is for.

```yaml
groups:
  - name: pail
    rules:
      - alert: PailDown
        expr: up{job="pail"} == 0
        for: 2m
        annotations:
          summary: "Pail is not answering scrapes"

      - alert: PailDatabaseDown
        expr: pail_up_database == 0
        for: 2m
        annotations:
          summary: "Pail cannot reach its metadata store"
          description: "The S3 API rejects every request while this holds."

      - alert: PailErrorRate
        expr: |
          sum(rate(pail_requests_total{surface="s3",status=~"4xx|5xx"}[5m]))
            / sum(rate(pail_requests_total{surface="s3"}[5m])) > 0.05
        for: 10m
        annotations:
          summary: "More than 5% of S3 requests are failing"
          description: "Open the console's Logs screen — it groups failures by cause."

      - alert: PailServerErrors
        # Distinct from the rate above: a 4xx is usually a client, a 5xx is
        # always this server.
        expr: sum(rate(pail_requests_total{status="5xx"}[5m])) > 0
        for: 5m
        annotations:
          summary: "Pail is returning server errors"

      - alert: PailDiskFilling
        expr: pail_disk_free_bytes / pail_disk_total_bytes < 0.15
        for: 15m
        annotations:
          summary: "Less than 15% free on the object volume"
          description: "Objects are stored as a single copy; a full volume fails every write."

      - alert: PailWriteLatency
        expr: |
          histogram_quantile(0.99,
            sum by (le) (rate(pail_request_duration_seconds_bucket{operation="PutObject"}[5m]))
          ) > 5
        for: 10m
        annotations:
          summary: "PutObject p99 is above 5 seconds"

      - alert: PailLogsDropping
        # Not an outage, but it means the console's Logs screen has holes in it
        # and cannot be trusted to be complete while this fires.
        expr: rate(pail_log_entries_dropped_total[10m]) > 0
        for: 10m
        annotations:
          summary: "Pail is discarding log entries"
```

---

## Which alerts where

Pail's built-in alerts and an external alerting stack will otherwise tell you
the same things twice, from two places, with thresholds that drift apart.

**Pail's own alerts** know things Prometheus cannot: which credential is being
rejected, which bucket the failures are against, and what the likely cause is.
They arrive by email and need no infrastructure. They stop when Pail stops,
which is their significant limitation.

**Prometheus** keeps working when Pail does not, holds history far longer, and
puts Pail alongside everything else you run.

A reasonable split:

- Leave `PailDown` and `PailDatabaseDown` to Prometheus. Pail cannot report its
  own absence.
- Leave the diagnostic rules — error rate, authentication failures, write
  failures — to Pail, where the alert can name the cause. Turn the equivalent
  Prometheus rules off, or set them wide enough to be a backstop rather than a
  duplicate.
- Capacity can live in either. Pick one.

Pail's rules are editable on the Alerts screen, and disabling one there resolves
whatever it had firing.

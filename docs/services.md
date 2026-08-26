# Service boundaries

The decision behind splitting Pail into separately restartable pieces, and the
evidence for it. Recorded because the shape is easy to get wrong in a way that
is expensive to undo, and because three of the assumptions this epic started
from turned out to be false.

The goal is narrow and worth stating plainly: **`docker compose restart console`
should leave the S3 API serving.** Everything below is in service of that, and
anything that does not serve it is out of scope.

---

## What was decided

| Question | Decision |
| -------- | -------- |
| How are services separated? | One binary, several **roles**. Not several binaries, not services with APIs between them. |
| Does metadata become a service? | **No.** Every role keeps talking to Postgres directly. |
| Do object bytes become a service? | **No.** Shared volume, mounted by every role that needs it — which is all of them. |
| One worker service or several? | **One.** |
| Do the roles version independently? | **No.** One binary, one tag, one release. |
| What needs single execution? | **Only the alert engine.** Not background work as a whole. |

---

## One binary, several roles

The process learns a `ROLE`: `s3`, `console`, `worker`, or `all`. A role selects
which listeners and which background goroutines start. Nothing else changes —
every role links the same packages and talks to the same Postgres.

`all` stays the default, so an existing deployment upgrading to this version
keeps working without touching its compose file. That is not politeness; it is
what makes the split reversible. If this shape turns out to be wrong, the
all-in-one role is still there and nothing has to be un-built.

Separate binaries were rejected for the ordinary reasons: three build targets,
three images, three things to keep in step, and a standing invitation for the
roles to drift apart in the libraries they carry.

## Metadata does not become a service

Putting an API in front of `internal/db` would buy a place to enforce
invariants and a seam for future distribution. It would cost a network hop on
the hot path — every object read already does a metadata lookup before it
touches a byte — and an interface to keep in step with several callers.

The deciding argument is that **ILS-57 already plans to move the metadata store
to CockroachDB**, which is a different answer to most of the same problem.
Building a metadata service now means building it twice, and the second answer
is the one the scale-out epic actually needs.

## Object bytes stay on a shared volume

The epic assumed only some roles need the object volume. That is false:

| Role | What it does with the blob store |
| ---- | -------------------------------- |
| `s3` | Reads and writes object bytes. |
| `console` | Writes on upload and folder creation, reads for preview and download, and calls `Usage()` for the storage screen. |
| `worker` | Unlinks reclaimed blobs during the sweep, and calls `Usage()` for the disk-headroom alert rule. |

All three, then. A blob service would add a hop to the hottest path in the
system in exchange for nothing at single-node scale, and cross-node transfer is
already scoped separately as ILS-60.

## The workers are one service

The epic proposed splitting the four background workers by failure
characteristic — "a stuck sweep is slow and harmless, a stuck log sink loses
observability". True, but it argues for *monitoring* them separately, not for
*deploying* them separately. Four containers to restart independently, when the
failure mode of each is "it stops running", is machinery without a use.

One `worker` role runs all four. If one of them later needs to fail
independently, splitting it out is a smaller change than merging four back.

## Roles version together

One binary means one tag, one image and one compatibility matrix. Independent
versioning is only worth its cost when services are genuinely released on
different cadences, and nothing here suggests that. Answered here rather than
discovered at the first release after the split.

---

## What is actually unsafe with two of them running

This is the part the epic got most wrong, and it matters because it decides how
much machinery ILS-96 needs. Two of every role will exist during any restart or
rolling upgrade, so "is it safe" is not hypothetical.

**Safe: the blob sweep.** Not because of a sweep-level lock — there isn't one —
but because `claimAndUnlink` takes a per-blob advisory transaction lock,
re-checks the reference count under it, and unlinks before committing. Two
sweepers race on the `DELETE ... WHERE refcount = 0`; one wins, the other sees
zero rows affected and moves on.

**Safe: the purges.** Bounded, idempotent `DELETE`s. Two sweepers duplicate the
work and neither corrupts anything.

**Safe: the metrics collector.** The epic said two collectors "would
double-count". They would not. Each collector accumulates counts for the
requests *its own process served*, and `FlushMetrics` adds them:

```sql
ON CONFLICT (hour, status_class) DO UPDATE SET
    requests = request_metrics.requests + EXCLUDED.requests
```

Two processes flushing disjoint slices of traffic into an additive rollup is
precisely correct. Making it idempotent instead would break it.

**Safe: the log sink.** Same reasoning. Each sink buffers entries for requests
its own process served and inserts those rows. Two sinks write different rows,
not duplicates.

**Unsafe: the alert engine.** `notify()` selects the alerts awaiting
notification, sends each one, and only then marks it notified. The send is an
external side effect that happens *before* the mark, so two engines both select
the same pending alert and both send it. The operator gets two emails, and
there is no way to un-send the second.

`evaluate()` is the lesser half of the same problem: two engines racing to raise
the same alert collide on the one-live-alert-per-rule constraint. The schema
makes that fail rather than corrupt, but it fails noisily and repeatedly.

### So: one lock, not leader election

Only the alert engine needs to be single-execution, and it needs it for one
function. That is a Postgres advisory lock held across an evaluation cycle —
not a leader-election mechanism, not a lease table, not a coordination service.

`pg_try_advisory_lock` is the right primitive: a second engine that cannot take
the lock skips that cycle and tries again on the next tick, rather than queueing
up behind the first and running immediately after it.

ILS-55 in the scale-out epic proposes leader election for maintenance sweeps.
On this evidence the sweeps do not need it. If a genuine need for leader
election appears later, this lock is a small thing to replace.

---

## What each role may talk to

| | Postgres | Object volume | Outbound email | Serves |
| --- | --- | --- | --- | --- |
| `s3` | yes | yes | no | S3 API port |
| `console` | yes | yes | test sends only | console port |
| `worker` | yes | yes | alert notifications | nothing |

The `worker` listens on no port at all, which is the cleanest statement of what
it is: the thing that must not be duplicated and that nobody talks to.

## What each role must be configured with

Configuration is currently one environment for one process, validated as a
whole. A role that does not serve the console should not have to be given a
console URL to start, and a console container holding the credentials
encryption key is a wider blast radius than it needs.

| Setting | `s3` | `console` | `worker` |
| ------- | ---- | --------- | -------- |
| `DATABASE_URL`, `DATA_DIR` | required | required | required |
| `S3_REGION`, `PUBLIC_S3_URL` | required | required (shows endpoint snippets) | — |
| `S3_PORT` | required | — | — |
| `CONSOLE_PORT`, `PUBLIC_CONSOLE_URL` | — | required | — |
| `SESSION_SECRET` | — | required | — |
| `CREDENTIALS_KEY` | required (verifies signatures) | required (shows a key once at creation) | — |
| `ADMIN_EMAIL` | — | required | — |

`CREDENTIALS_KEY` reaching two roles is unavoidable: the S3 API needs the secret
to check a signature, and the console needs it to display a newly created key
once. The worker never needs it, and that is a real reduction.

Startup must fail loudly when a role lacks something it needs, and must not
demand what it does not.

---

## Health checks

`/readyz` currently checks the database and the blob store and answers for the
whole process. That is nearly right already — every role needs both — but it
lives on the console port, so an `s3`-only container has nothing to probe.

Each role needs a probe reflecting what that role actually does. A console-only
container must not report unhealthy because it is not serving S3, and a worker
that listens on nothing still needs to be answerable to an orchestrator.

---

## What this does not do

It does not make Pail distributed, or highly available, or able to survive a
node. There is still one Postgres, one disk and one copy of every object.

It makes the console restartable without dropping S3 traffic, and the worker
restartable without either. That is the whole claim.

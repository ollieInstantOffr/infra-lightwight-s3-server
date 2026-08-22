# Scaling Pail out across hosts

A plan for running Pail as a cluster: containers on several hosts, every node
serving the full API and console, and routing built into each node.

This document exists to be decided from. It ends with the questions that have to
be answered before any code is written, because the answers change the size of
the work by roughly an order of magnitude.

---

## 1. What is already cluster-safe

Most of it, which was not obvious until I went looking.

Every piece of cross-request coordination in the server already runs through
Postgres advisory locks — blob reference counting, per-key write serialisation,
multipart part replacement, and schema migration. Advisory locks are held by the
database, not the process, so they work identically across hosts. Two nodes
sharing a database coordinate correctly today, with no changes.

| Concern | State lives in | Works across nodes today |
| ------- | -------------- | ------------------------ |
| Object and bucket metadata | Postgres | yes |
| Blob reference counting | Postgres, advisory-locked | yes |
| Sessions, users, invitations | Postgres | yes |
| Sign-in rate limiting | Postgres | yes |
| Access keys | Postgres | yes |
| Audit log | Postgres | yes |
| Schema migration | Postgres, advisory-locked | yes — verified with 5 concurrent cold starts |
| Request metrics | in memory, flushed to Postgres | yes — sums correctly across nodes |
| **Object bytes** | **local disk** | **no** |

There is exactly one thing that does not work: the blob store.

## 2. The one real problem

Objects are stored as content-addressed files under `DATA_DIR`. Node A writes a
blob; node B has no way to know it exists and no way to read it. A request that
lands on the wrong node returns a 404 for an object that plainly exists.

The good news is that this is a clean seam. The entire local-disk surface is
five methods — `Put`, `Open`, `Concat`, `Remove`, `Usage` — across ten call
sites. Everything else in the system talks to that interface and would not
change.

That means the architectural choice is narrow and well-contained. It is also
consequential.

---

## 3. Three architectures

### Option A — shared filesystem

Every node mounts the same storage (NFS, CephFS, a SAN) at `DATA_DIR`, and they
all point at one Postgres.

**Work: about 3 points.** Nothing about placement changes. The only real change
is electing one node to run the maintenance sweeps, because today every node
would run them — safe, thanks to the advisory locks, but wasteful.

**What you get:** more request throughput, and any node can die without losing
anything.

**What you do not get:** storage that scales with nodes. Capacity, durability
and performance all belong to the shared filesystem. You have scaled compute,
not storage.

**The catch worth knowing:** the blob store's durability rests on `fsync` of the
file *and* its parent directory before a write is acknowledged. NFS honours that
unevenly depending on version and mount options. On CephFS or a proper SAN it is
fine. On a casual NFS export it is a quiet correctness risk.

### Option B — shared-nothing, sharded

Each node keeps its own disk. Blobs are placed on nodes by hashing their digest.
Every node can serve every request: if it holds the blob it serves it, and if it
does not, it streams it from the node that does.

**Work: about 55–65 points**, phased below.

**What you get:** capacity that grows with nodes, no shared storage dependency,
and — with replication above 1 — survival of node loss.

**What it costs:** this is a real distributed system, with the failure modes of
one. Rebalancing and repair are where storage systems of this kind get genuinely
difficult, and where their bugs lose data rather than merely erroring.

### Option C — full replication

Every node holds every object.

**Work: about 25 points.** Simple to reason about, no proxying, reads always
local.

**What you get:** read throughput and node-loss survival.

**What you do not get:** capacity. The cluster holds what the smallest node
holds, and every write goes to every node.

Sensible for a small cluster of a few nodes holding a modest working set.
Pointless beyond that.

---

## 4. What I would recommend

**Start with Option A unless you specifically need capacity to scale with
nodes.**

Three points of work against sixty is not a close call, and it buys the two
things people usually mean by "scale out": more throughput, and surviving a host
dying. If you already have shared storage in the lab, this is a day.

**Take Option B if any of these are true:** you have no shared storage and do
not want to run one; total capacity will exceed what one filesystem can
sensibly hold; or you want each host's local disks to contribute.

I would not recommend Option C. It occupies an awkward middle: nearly the
complexity of B, with none of the capacity benefit.

---

## 5. Scope for Option B

Phased so each phase is independently useful and independently revertible.

### Phase 0 — make the current design multi-node safe (3 pts)

Leader election for the maintenance sweeps, so one node runs them rather than
all of them. A Postgres advisory lock is enough; no new dependency.

Also required for Option A, so it is not wasted either way.

### Phase 1 — node identity and membership (5 pts)

Nodes register in a `nodes` table with a heartbeat: id, advertised address,
capacity, last seen. Membership in Postgres rather than gossip, because the
database is already a hard dependency and cluster membership has to agree with
metadata anyway. Adding a gossip protocol would mean two sources of truth about
who is in the cluster.

A node absent for longer than a threshold is considered down. The console gains
a cluster view.

No behaviour change yet.

### Phase 2 — placement and read proxying (13 pts)

- Rendezvous hashing on the blob digest decides which nodes should hold it.
  Rendezvous rather than a hash ring: it is simpler, needs no virtual nodes, and
  moves the minimum when membership changes.
- A `blob_locations` table records which nodes actually hold each blob, because
  where a blob *should* be and where it *is* diverge constantly during any
  membership change, and the read path must follow reality rather than the
  formula.
- An internal HTTP API between nodes, on a separate port, authenticated with a
  shared cluster secret. Never exposed publicly.
- `Open` becomes: hold it locally, or stream it from a node that does.

Dedup survives, because placement keys on the content digest — identical content
still converges on one blob, cluster-wide.

### Phase 3 — replication (8 pts)

A configurable replication factor. Writes go to N nodes before being
acknowledged, matching the current promise that an acknowledged write is durable.
Reads fail over to another replica.

**This is where your single-copy decision has to be revisited, and it is the
most important thing in this document.** With one node, one copy means one disk
failure loses data. With eight nodes and still one copy, you have eight times as
many disks that can each lose a distinct eighth of your data. Scaling out with
`RF=1` makes durability worse, not better.

`RF=2` is the smallest number that makes a cluster safer than the single node it
replaced.

### Phase 4 — repair and rebalance (13 pts)

- Detect under-replicated blobs and re-replicate them
- Move blobs when nodes join or leave
- Throttle both, so a repair cannot saturate the network and take down serving
- Report progress, because an unobservable rebalance is one nobody trusts

This phase is the hardest and the most dangerous. Every distributed storage
system's worst bugs live here.

### Phase 5 — multipart across nodes (5 pts)

Parts are already blobs addressed by their own digest, so they scatter across
the cluster naturally. Completion assembles them with `io.MultiReader`, which
can read from remote nodes as easily as from disk — the existing streaming
assembly needs remote readers, not a redesign.

### Phase 6 — request routing and the built-in balancer (8 pts)

Each node routes requests it cannot serve locally. Health-aware, so a request is
never sent to a node that is down. Console screen showing cluster state,
placement, and repair progress.

**One thing to be clear about:** a balancer inside every node solves internal
routing, not client distribution. Something still has to spread clients across
nodes in the first place. The simple answer is DNS round-robin over the node
addresses; the better answer is your existing Nginx Proxy Manager with all nodes
as upstreams and health checks enabled.

### Phase 7 — Postgres high availability (8 pts, or deliberately not)

Once nodes are redundant, Postgres is the only remaining single point of
failure — and it is now a much larger one, because it takes down the whole
cluster rather than one node.

Options are a managed Postgres, or Patroni / CloudNativePG for automatic
failover. This is a decision about operational appetite more than code.

It is legitimate to accept a single Postgres and back it up well. It is not
legitimate to build phases 1–6 and not decide.

**Total: roughly 55–65 points**, against 174 for everything built so far.

---

## 6. Decisions needed before any code

1. **Shared storage, or shared-nothing?** Everything else follows from this. If
   you have CephFS or a SAN available, Option A is a day's work and this
   document mostly stops here.

2. **Replication factor.** `RF=1` on a cluster is worse than the single node you
   have now. If the answer is still "one copy, no protection", scale-out is the
   wrong tool — buy a bigger disk.

3. **How many nodes, and how similar are they?** Rendezvous hashing assumes
   roughly comparable capacity. Wildly uneven disks need weighting, which is
   more work.

4. **Is Postgres allowed to be the single point of failure?** A defensible yes,
   but it should be said out loud.

5. **What is the actual goal — throughput, capacity, or availability?** They
   pull in different directions, and the honest answer changes the design.

---

## 7. What this does to what Pail is

Worth saying plainly: this stops being a lightweight single-node object server.

The current README leads with a promise — one container, one Postgres, one disk,
and a clear statement about what it does not do. A cluster with placement,
replication and repair is a different product with different operational
demands, and the documentation would need to say so rather than growing a
"clustering" section on the end.

That is not an argument against doing it. It is an argument for deciding
deliberately rather than arriving there.

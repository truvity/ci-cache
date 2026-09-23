# Operations

What to watch, what each destructive call actually destroys, and how to
restart a cache that everything in CI is pointed at. The shape this all rests
on is in [architecture](architecture.md).

## Garbage collection

The disk tier's budget is derived from the filesystem with `statfs`, not
configured as a size. That is the single most useful property of this
arrangement: **resizing the PVC is the whole change**, and no value has to be
edited to match it.

| value | meaning | default |
|---|---|---|
| `gc.floor` | percentage of the filesystem to leave free; the budget is what remains | 10 |
| `gc.high` | percentage of the budget at which eviction starts | 95 |
| `gc.low` | percentage of the budget eviction runs down to | 85 |
| `gc.budgetBytes` | overrides the derived budget, for a volume shared with something else | unset |
| `gc.budgets` | caps one front-end as a percentage of the whole | unset |

The gap between `high` and `low` is what keeps the cache out of a GC loop: if
they were equal, every write past the line would evict, and the process would
spend its time walking the index instead of serving. Narrowing the gap to a
point makes a full cache slow in a way that looks like the network.

Eviction is least-recently-used, over an index seeded at start-up from each
file's modification time. `atime` is not used, because `relatime` and
`noatime` are both common and a cache evicting by an access time the
filesystem never updates would throw out the hottest objects first. While that
index is being rebuilt, `Stats` reports `index_cold`; the cache serves
normally throughout and only its counters are incomplete.

`gc.budgets` is for a volume where one front-end can crowd out the others —
module zips against build outputs, say. A front-end over its cap is evicted
before anything else, whatever the global watermarks say.

When the disk cannot make room at all, a `Put` returns `ErrNoSpace`
(`ResourceExhausted` on the wire). That is not an error the client should
fail on: the cache is full, not broken.

## The three wipes

All three are on the **admin port** and nowhere else
([0003](decisions/0003-two-ports.md)). They are three calls rather than one
with a flag, because the cheap one and the expensive one must not be one wrong
argument apart.

| call | removes | costs | reach for it when |
|---|---|---|---|
| `Invalidate` | negative-cache entries under a prefix | one upstream round trip per path, next time | an upstream published something this cache remembers as absent |
| `WipeDisk` | disk entries under a prefix or one exact key | a re-download from the bucket | the volume is full of one front-end's junk, or one object is suspect |
| `WipeBucket` | bucket objects under a prefix, **and the disk entries that went with them** | a cold cache, estate-wide | a layout migration is finished, or the bucket holds something that must not be served again |

Each answers what it actually did — entries and bytes — because a wipe that
matched nothing is not an error, and the count is the only way to tell the
difference between "cleared it" and "spelled the prefix wrong".

`WipeDisk` treats an argument ending in `/` as a prefix and anything else as
one exact key. `WipeBucket` takes a prefix only and **refuses `""` and `"/"`**:
there is no call that empties the whole bucket, and a genuine full reset is
done with the store's own tooling by somebody who has decided to.

`WipeBucket` removing the matching disk entries is not a convenience. A disk
copy of an object the bucket no longer has would answer the next read, so the
cache would go on serving what was just deleted and the operator would
conclude the wipe had failed.

## The negative cache

The proxying front-ends remember absence: Maven's `negativeTTL`, Nix's
`negativeTTL`. Without that, every optional artefact a resolver probes for and
every path Nix asks about costs an upstream round trip on every build, and the
cache makes those builds *slower* than no cache at all.

The cost is a window in which something newly published is reported missing.
`Invalidate` closes it for a prefix without touching a byte of cached content,
which is why it is a separate call from `WipeDisk` — invalidating a stale
negative answer should not throw away the positives beside it.

`Stats` reports `negative_entries` per front-end, and the instrument is
`cicache.negative.entries`.

## Reading the metrics

Telemetry is OTLP: `telemetry.otlpEndpoint` and `telemetry.traceRatio` (1% by
default — traces here are for finding a slow tier, not for accounting). Empty
sends nothing and costs nothing.

The instrument names are constants in the code and are covered by a test, so
an alert that names one keeps working across a rename or fails loudly in CI.
The attribute keys have one spelling each — `frontend`, `tier`, `outcome`,
`direction`, `op` — because a dashboard cannot group by two spellings at once
and will not say which it got.

| instrument | attributes | what it tells you |
|---|---|---|
| `cicache.get` | `frontend`, `tier`, `outcome` | the hit rate, per tier. A disk hit rate that falls while the bucket's rises means the volume is too small |
| `cicache.put` | `frontend`, `tier`, `outcome` | what is being written, and where a write is being refused |
| `cicache.bytes` | `frontend`, `tier`, `direction` | the traffic that is *not* leaving the cluster — the number the bucket bill is compared against |
| `cicache.latency` | `frontend`, `tier`, `op` | where the time goes. A bucket p99 in seconds is an endpoint problem, not a cache problem |
| `cicache.disk.used_bytes` | — | with the next one, how close the volume is to eviction |
| `cicache.disk.budget_bytes` | — | derived from `statfs`, so it moves when the PVC is resized. A budget that did not move after a resize means the pod did not restart |
| `cicache.gc.evicted_bytes` | `frontend` | steady eviction is healthy; eviction that tracks every write is a volume that is too small |
| `cicache.negative.entries` | `frontend` | absence being remembered |
| `cicache.upload.queue_depth` | — | objects waiting for the bucket. **A number that only grows means the bucket is slower than the cache is being filled**, and objects are being dropped from the queue — a miss later, never an error now |
| `cicache.disk.entries` | — | how many objects the volume holds, beside the bytes |
| `cicache.gc.runs`, `cicache.gc.duration` | — | how often the collector runs and for how long. Runs that never stop are the narrow-watermark case above |
| `cicache.agent.degraded` | — | 1 while a runner's agent cannot reach the server, so it is serving from its own disk alone. Only an agent exports it, which is how a query tells the two deployments apart |

The two alerts worth having are the queue depth rising without returning, and
the hit rate falling by a step — the second is almost always a key-layout
change, a new `keyPrefix`, or a front-end whose upstream started answering
differently.

## Drain and restart

`server.drainTimeout` (30s by default) is how long the process keeps serving
in-flight requests after it is told to stop. It should be at or above the
longest single object transfer, and comfortably below the pod's
`terminationGracePeriodSeconds`, or the kubelet kills the process in the
middle of the drain and the client sees a truncated body rather than a clean
end of stream.

A restart is an outage of the cache, briefly, and that is acceptable because
**every client has a fallback**: `GOPROXY` ends in `direct`, the agent fails
open, Nix has `cache.nixos.org` after us, Gradle's remote cache is an
optimisation over its local one. Each client page names its own. What is not
acceptable is a client wired without its fallback — that turns a rollout into
a CI outage, and it is the first thing to check when one happens.

On restart, the disk index is rebuilt from the volume. The cache serves while
that runs (`index_cold` in `Stats`), so a restart is not a cold cache — only
a brief one with incomplete counters.

## Why a singleton

One replica, `Recreate`, one ReadWriteOnce PVC. Two replicas behind one
Service would each hold half the answers: the hit rate halves, bucket traffic
doubles, and both pods fault in the same objects. RWO is what makes the
mistake loud — the second pod stays `Pending` instead of quietly doing that —
and `Recreate` is what stops a rollout from briefly having two.

Scale by making the volume bigger and the bucket closer, not by adding pods.
The runner-side agent is the horizontal part of this design: every runner
already has a disk tier of its own in front of the shared one.

If the pod cannot schedule after a node goes away, the usual cause is the RWO
volume still attached to the old node. That is a storage-layer fix, and it is
one of the reasons the bucket exists: deleting the PVC and letting the cache
refill from the bucket is a legitimate, if slow, repair.

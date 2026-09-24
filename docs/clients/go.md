# Go

Two front-ends, both in the first release: `/go/build` is the build cache and
`/go/mod` is the module proxy with its sumdb mirror. They are independent —
one can be enabled without the other — and they answer different things. The
build cache holds compiled actions, which are produced by the machine that
asks for them; the module proxy holds module zips and metadata, which come
from upstream.

## The build cache

The Go toolchain does not speak HTTP to a cache. It speaks `GOCACHEPROG`: a
program it starts and talks to over a pipe. So the build cache has a client
program, the **agent**, and the agent is a `ci-cache` too — its chain is
`disk(emptyDir) → remote`, where `remote` is this service's
`cache.v1.Cache` over Connect.

```
ci-cache agent --remote http://<service>:8080/go/build
```

and in the job's environment:

```
GOCACHEPROG=ci-cache agent --remote http://<service>:8080/go/build
```

The runner's own disk is in front of the network for a reason: a re-run of the
same job, or a second job on the same runner, hits it without a hop, and the
`emptyDir` costs nothing to throw away. Because that disk really is ephemeral,
the agent sets `persistence.ephemeralIsAcceptable` — the start-up check that
refuses a build cache on `/tmp` exists to catch a *server* deployed that way,
not the agent.

### The fallback: the agent fails open

`GOCACHEPROG` is not optional to the toolchain. When the program answers an
error, `go build` **fails the action** — that is a fatal toolchain error, not
a slow cache. So the agent is written around one rule: **the only error that
ever reaches the toolchain is a local disk failure.** A server that is down, a
bucket that refuses, a network that has gone away — each is a miss on the way
in and a dropped write on the way out, counted and reported, never returned.

Two details follow from that rule.

- **A remote that is unreachable at start-up is dropped**, with a warning, and
  the agent serves from its local disk alone. It reports that it came up
  *degraded* (`cicache.agent.degraded`), which is the number to look at when a
  pipeline is mysteriously slower than last week.
- **A remote that fails repeatedly stops being asked.** After five consecutive
  failures the agent stops calling it for thirty seconds, because a tier that
  is timing out charges every single compile action its full timeout before
  the build can carry on without it. Thirty seconds is short enough that a
  server which comes back mid-build is used again for the rest of it.

The agent can also print a one-line summary of what it did to stderr at exit —
the habit comes from `GOCACHE_METRICS`, and it is worth keeping: a job log that
does not say what the cache did is a job nobody can tell was slowed down by it.

The one thing that does still fail is the agent not starting at all, because
then `go` has no program to talk to. Keep the binary in the image rather than
fetching it in the job.

### What a read fetches

A lookup is two things: an action record of 84 bytes that names an output, and
the output itself. The agent materialises every output into a directory the
compiler opens by path, and within one build it is asked repeatedly for
objects already sitting there.

So the record is resolved first, and the body is fetched **only if the object
is not already materialised**. Previously the body was fetched regardless and
then discarded, because the record was the only way to learn the output's
identity and there was no way to act on it without the bytes.

The summary line reports `reused=N` for lookups answered that way. Usually
what it saves is a full read from local disk; it saves a network fetch when
the disk tier has been evicted under budget pressure while the materialised
file survives — the constrained runner where it matters most.

### Where an object is stored

Once. The agent materialises every output into a directory the compiler opens
by path, and that file is the object as far as the build is concerned.

The local disk tier keeps the **action records** — 84 bytes each, and what a
lookup resolves first — but not output bodies. A second complete copy there
would be written on every fault and every put and read by nothing, because a
repeat lookup is answered from the materialised file.

The summary line reports `tier-io=` per tier, so the question "which tier is
being written to, and does anything read it back" has an answer in the job
log rather than in someone's model of the code.

### What a write waits for

The toolchain calls `put` once per compiled object and waits for the answer
before it goes on, so whatever `put` does is on the critical path of the
build. A `go build` writes thousands of objects, and anything paid per object
is paid thousands of times in series.

So a write is split:

| tier | when | why |
|---|---|---|
| the local disk | before the answer | the build asks for things it has just produced, and a deferred local write turns those into misses and recompiles |
| the server, the object store | after the answer | this is the round trip, and nothing in this build is waiting on it |

The deferred recordings run on a bounded pool — `--upload-workers`, which
also reads `CI_CACHE_AGENT_UPLOAD_WORKERS`. Bounded rather than unbounded
because a build that outruns the network would otherwise accumulate one
in-flight request per object and fail on file descriptors somewhere
unhelpful.

The time is paid back at the end. When `go` closes the stream, the agent
waits for the outstanding recordings and says how long it waited. That wait
is bounded too: a job that appears to finish and then sits there is a job
somebody cancels. If it runs out, the objects are still on local disk and the
summary reports `lost=N` — the count of objects that were **not** recorded
for the next build.

`lost` is the number to check first when a build time improves, because the
cheapest way to make this agent look fast is to stop recording anything.

### Direct mode, for a laptop

Direct mode is what the agent does with **no remote**: the object store goes
straight behind the local disk, so the chain is `disk → bucket`, with no
server in it and the developer's own bucket credentials.

```
GOCACHEPROG=ci-cache agent --bucket <bucket> --region <region>
```

The local directory defaults to one under the user's cache directory, so
running this by hand needs no decision about where to put it. The same shape
is a runner's fallback when the server is down, and it is why the bucket tier
is not tied to the server.

### A bucket already warmed by `go-cache-plugin`

The `go/build` key layout is deliberately the one `tailscale/go-cache-plugin`
wrote ([0004](../decisions/0004-reimplementation-not-fork.md)). Point
`frontends.go.build.legacyPrefix` at where that plugin wrote, and a bucket
miss under the current layout is retried there, so the switch is a deploy
rather than a cold cache. Writes always go to the new layout, so the legacy
prefix decays on its own and can be dropped — and then deleted with
`WipeBucket` — once nothing is finding anything there.

## The module proxy

```
GOPROXY=http://<service>:8080/go/mod|https://proxy.golang.org,direct
```

The separator matters. `|` means "on any error, try the next"; `,` means "on a
404 or 410 only". Written as above, an unreachable or broken cache falls
through to the public proxy, and a module neither of them has falls through to
`direct`, which fetches from the origin. That is the fallback: a cache outage
makes module downloads slower and nothing else.

`/go/mod/sumdb/…` mirrors the checksum database named by `frontends.go.mod.sumdb`,
so `GOSUMDB` verification keeps working when a runner has no route to the
internet. Keep `GOFLAGS`, `GONOSUMDB` and `GOPRIVATE` exactly as they are:
private modules must still bypass the proxy and the sumdb, and pointing
`GOPROXY` at this cache changes nothing about that
([the private-module wiring lives in `ci-workflows`](#what-ci-workflows-sets)).

`frontends.go.mod.listTTL` is how long a `@v/list` and `@latest` answer is
reused. Those are the only module-proxy responses that are allowed to change
for an unchanged input, so they are the only ones with a TTL; a `@v/<version>.zip`
is immutable and is cached until it is evicted.

## What `ci-workflows` sets

The estate's jobs are wired by `truvity/ci-workflows`, in the `setup-devbox`
action, and the wiring is already in the shape this service needs:

| input | what the action writes |
|---|---|
| `goproxy` | `GOPROXY`, verbatim — so the value above goes here |
| `go-cache-bucket` (plus `-region`, `-endpoint`, `-path-style`) | `GOCACHEPROG` and the `GOCACHE_*` variables |

Both are opt-in and empty by default, because a hosted runner has no VPC
endpoint and no pool identity: pointing a build cache at a bucket there would
fail every build.

Today `go-cache-bucket` writes `GOCACHEPROG=go-cache-plugin` and gives the
program the bucket directly. Moving a runner pool to this service replaces
that program with `ci-cache agent --remote …`, and the pool then needs no
bucket credentials at all — the server holds them. That is the migration, and
the legacy prefix above is what makes it free.

**One trap, and it is not this service's.** `devbox run` re-applies the `env`
block of `devbox.json` on top of the job environment, so a `GOPROXY` pinned
there beats the one CI wrote, and every module download goes to the public
proxy while the job looks correctly wired. `setup-devbox` warns about it. If
module traffic is not reaching the cache, look there before looking here.

# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/). A section describes
the state of the repository at that version, not the history of edits that got
there.

## [0.1.4] - 2026-09-24

### Fixed

- **The agent kept two complete copies of every object it served.** One in the
  directory the compiler opens by path -- which is the object, as far as the
  build is concerned -- and one in the local disk tier, written on every fault
  and every put.

  Nothing read the second. A repeat lookup resolves the 84-byte action record
  and finds the materialised file already there, so the tier's copy of an
  output was written once and read never. On one `truvity/gitops` build that
  is 3501 remote hits at around 1.2 MB: roughly 4.3 GB written to disk and
  ignored, plus the page cache that came with it, charged to the container's
  memory limit.

  The disk tier now stores action records and declines output bodies. It is
  still there, and still worth having, because the records are what a lookup
  resolves first and holding them locally makes a repeat lookup a stat rather
  than a round trip.

- **A repeat lookup fetched the body it already had.** A lookup is an action
  record naming an output, then the output. The agent had no way to act on the
  record alone, so it read the whole object back out of a tier and handed it to
  the object directory, which -- finding the file already present -- drained
  the reader into `io.Discard`. A full read of a megabyte, to learn nothing,
  1068 times in one build.

  `GetIf` resolves the record and then asks whether the body is wanted.

### Added

- **`reused=` and `tier-io=` on the agent's summary line.** The first counts
  lookups answered from an already-materialised file; the second reports bytes
  read and written per tier.

  Both exist because the totals could not answer the question that mattered --
  which tier is being written to, and does anything read it back -- and two
  rounds of this work were spent inferring that from wall-clock alone.

## [0.1.3] - 2026-09-24

### Fixed

- **The agent waited for the network before answering the compiler.** The
  toolchain calls `put` once per compiled object and waits for the answer,
  and the agent was recording that object into the whole chain -- a local
  tier write and an HTTP round trip to the server -- before replying. One
  gitops `build` wrote 4.4 GB that way, every byte of it on the critical
  path, in series.

  This is the measured gap against `go-cache-plugin`, which answers as soon
  as the object is on local disk and pushes to S3 from a bounded background
  group. It beat this agent on every recipe -- 139 s against 191 s on build,
  148 against 242 on lint, 231 against 287 on test -- while talking to a
  bucket in another datacentre rather than to a server on the same LAN. Its
  uploads were never on the critical path and ours were.

  The write is now split rather than simply deferred. The local tier is
  written **before** the answer, because a build asks for things it has just
  produced and a deferred local write turns those into misses and recompiles.
  The server and the object store are recorded afterwards, on a bounded pool
  (`--upload-workers`, or `CI_CACHE_AGENT_UPLOAD_WORKERS`), and waited for
  once when the toolchain closes the stream.

  That wait is bounded, because a job that appears to finish and then sits
  there is a job somebody cancels. When it runs out, the objects are still on
  local disk and the summary line reports `lost=N`: objects that were not
  recorded for the next build. Deferring the work introduces exactly one way
  to look faster while doing less, and `lost` is the number that shows it --
  check it first when a build time improves.

### Added

- **A benchmark harness, `ci-cache-bench`.** CI is the wrong instrument for a
  performance question: a job takes minutes, conflates compile time with
  cache time, runs on whichever runner is free, and answers one point on one
  curve. The harness loads a running server and reports throughput, latency
  separated into first-byte and complete, and what failed.

  It refuses rather than reports in the three cases that would otherwise
  produce 0 ms at every percentile and read like the best result it had ever
  produced: a run that completed no requests, a run where every request
  failed, and a scenario that could not reach its starting state.

  `docs/bench/0.1.2.md` records the first baseline. It is `warm-disk` only:
  the admin port has no ingress allowance from any pod, so nothing in the
  cluster can put the server into a cold-disk state.

## [0.1.2] - 2026-09-23

### Fixed

- **The agent sized its local cache from the filesystem, which in a container
  is the node's disk.** Every byte it wrote there was page cache charged to the
  container's memory limit, so an uncapped agent pushed a CI job from 76 % of
  its memory limit with zero reclaim events to 100 % with 59,190 of them, and
  the runner process was starved until the control plane lost contact with it.
  The job did not fail, it vanished mid-step.

  The budget now comes from the cgroup's memory limit when there is one, at a
  quarter of it, and the start-up log says which source was used. `statfs` is
  still right where the cache owns its volume, which is what the server does.

## [0.1.1] - 2026-09-23

### Fixed

- **The chart rendered a pod that could not run.** No `args`, so ko's
  entrypoint ran the bare binary, which is a CLI with several subcommands:
  with none it printed its help, exited 0, and Kubernetes restarted it. The
  first cluster it reached held it in `CrashLoopBackOff` having never opened
  a socket. The chart now passes `serve`.

  Every golden had been reviewed and every refusal passed, because the chart
  harness only asked what the chart must REFUSE. It now also asserts the one
  thing the rendered container must DO, which is the check that would have
  caught this before a cluster did.

## [0.1.0] - 2026-09-23

The first release: the engine, the two Go front-ends, the runner agent, the
admin API and the chart.

### One cache, in three deployments of the same code

A protocol-agnostic engine maps a key to bytes over a chain of tiers read
front to back and written through. A hit at the back is streamed to the
caller and faulted into every tier in front of it, so the next read is
answered closer; concurrent misses on one key collapse into a single read of
the tier behind. That is what makes a server (`disk` over a bucket), a
runner's agent (`disk` over the network) and a laptop (`disk` over a bucket)
the same program rather than three that resemble each other.

### What it serves

`/go/build` is the Go build cache over Connect, and `/go/mod` the module and
sumdb proxy, both on one listener that carries HTTP, Connect and gRPC. The
bucket layout is `tailscale/go-cache-plugin`'s on purpose -- a bucket that
tool already filled stays readable, and `legacyPrefix` serves the older
layout on a miss while writes go to the new one. Maven, Gradle, nix, npm and
Bazel have their paths reserved and are not built yet; the docs say so where
they describe them.

### The disk is bounded by the volume, not by a number

The disk tier keeps an LRU index seeded from `mtime`, because `atime` cannot
be trusted, and persists it so a clean restart is warm before it serves.
Eviction is driven by `statfs`: the budget is the filesystem less a floor,
so resizing the volume needs no other change, and a background pass runs
from a high watermark down to a low one -- a `Put` never waits for it.

### Two ports, because a job that can wipe can empty the cache for everyone

Every CI job is a client of the data port. `Stats`, `List`, the three
separate wipes and `Invalidate` live on a second listener the chart's
NetworkPolicy never admits, and the chart refuses at render time what the
binary would refuse at start-up: a build cache on an ephemeral volume, an
endpoint with no region, two identities at once, one port for both.

### Written down because they were paid for elsewhere

The bucket tier sends the checksum VALUE and never an algorithm: asking the
SDK for an algorithm on an object that also carries a `Content-Encoding`
makes it use the aws-chunked trailer, which Cloudflare R2 answers `403
SignatureDoesNotMatch`. The agent is fail-open, because an unreachable
`GOCACHEPROG` is a fatal toolchain error rather than a slow cache -- proven
by killing the server mid-build and getting a green build with one warning.
And `Stat` reports a miss when no tier has the object even if a tier failed
while being asked: returning the failure made one bucket's 403 degrade every
agent on the estate, although the server's warm disk would have served them.

Inspired by `tailscale/go-cache-plugin` (BSD-3). No upstream code is copied;
what is kept is the tiering and the `go/build` bucket key layout.

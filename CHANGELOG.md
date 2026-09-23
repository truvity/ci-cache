# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/). A section describes
the state of the repository at that version, not the history of edits that got
there.

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

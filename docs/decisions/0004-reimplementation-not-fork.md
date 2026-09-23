# 0004. A reimplementation, not a fork of `go-cache-plugin`

- Status: accepted
- Date: 2026-09-23

## Context

`tailscale/go-cache-plugin` is the origin of this idea and it works. The
obvious cheap route was to fork it and add front-ends. Four things decided
against it.

- **Its server binds to loopback.** `commands.go:89` listens on localhost, and
  its `connect` subcommand dials localhost. It is designed as a sidecar beside
  one toolchain, not as a service several hundred CI jobs reach over a
  cluster network. Changing that is not a flag — it reaches the concurrency
  limits, the shutdown path and everything that assumed one caller.
- **The bucket is mandatory.** There is no shape that is disk-only, which is
  exactly the runner agent's shape and the laptop's.
- **It is one protocol.** The tier and chain abstraction that lets the Go
  build cache, a module proxy and a Maven proxy share a volume, a budget and a
  GC has nowhere to live in it.
- **It has had no feature commit since June 2025.** A fork of a quiet
  repository is not shared maintenance — it is our maintenance, with somebody
  else's structure.

A fork would also have carried the part we specifically do not want: a
TLS-intercepting reverse proxy, with the certificate handling that implies, to
catch module traffic that `GOPROXY` points at us far more simply.

## Decision

This is a reimplementation. **No upstream code is copied**, and the repository
is MIT-licensed on its own terms.

Two things are kept deliberately, and credited in the
[README](../../README.md): the **tiering idea** — a local disk in front of a
bucket, faulted forward — and the **`go/build` bucket key layout**, so that a
bucket already warmed by `go-cache-plugin` keeps answering through the switch.
`frontends.go.build.legacyPrefix` is consulted on a bucket miss for exactly
that reason; writes always go to the new layout.

Also kept is the library upstream itself depends on for the toolchain
conversation, used directly rather than inherited through a fork:
`creachadair/gocache`, whose `Server` the agent is. The disk tier's own file
format is not the library's, for the reason in
[0002](0002-cachedir-kept.md).

## Consequences

- Every line here is ours to fix, and no upstream release has to be tracked
  or merged.
- The behaviour an estate already depends on — the bucket's contents — is
  preserved across the switch, so the migration is a deploy and not a cold
  cache.
- Credit is owed and given explicitly, because the idea is not ours.

## What would reverse it

Upstream becoming active again *and* growing a networked, multi-protocol
shape. At that point the honest move is to contribute the front-ends there
rather than to keep two of them.

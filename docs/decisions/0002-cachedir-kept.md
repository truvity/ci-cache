# 0002. The GOCACHEPROG protocol stays library code; the disk tier's file format does not

- Status: accepted
- Date: 2026-09-23

## Context

Two pieces of this service are the kind of code where a bug is not an error
but a silent wrong answer, and both were candidates for being somebody else's
problem.

The first is the **GOCACHEPROG protocol**: the small JSON conversation the Go
toolchain holds with a cache program over stdin and stdout. Getting a frame
wrong there does not produce a clear failure — it produces a build that fails
with something unrelated, and a stdout that is one stray log line away from
being corrupted entirely.

The second is the **atomic write on the volume**: write to a temporary file,
`fsync`, rename over the final name, under keys that are content hashes, while
other processes read. `creachadair/gocache/cachedir` does exactly that and has
been in front of real Go builds for years. That is where a cache's correctness
bugs live, and the instinct was to keep it.

## Decision

**The protocol is the library's.** The agent is a `gocache.Server`; this
repository writes what happens behind it and not the conversation itself.

**The disk tier's file operations are ours**, because `cachedir` stores bytes
under a key and this tier has to store `tier.Meta` — the size, the *producer's*
modification time, the content type, the immutable flag — atomically with
those bytes. Two files cannot be renamed into place atomically, and a cache
whose metadata can outlive its bytes is a cache that lies. So an object file is
a fixed 1KiB JSON header followed by the body, written to a temp file in the
object's own directory, `fsync`ed and renamed over the final name.

The parent directory is deliberately not `fsync`ed: after a power loss the
rename itself may be lost, which costs a cache miss, and a directory `fsync`
per object would cost far more than those misses ever will.

## Consequences

- The part that talks to the toolchain has far more exposure than this
  repository could give it, and a Go release that changes the protocol is
  upstream's to follow.
- The volume's layout is ours to document, which
  [architecture](../architecture.md) and the package comment both do: a rename
  within a directory is atomic on every filesystem this runs on, so a reader
  sees the old object or the new one and never a partial one.
- A restart can rebuild the whole key space from the volume alone, because
  every file carries its own key and metadata. That is what `cachedir` could
  not have given us and what the header is for.
- The one place where a race would corrupt rather than fail is code we wrote,
  so it is code with tests that write and read the same key concurrently, and
  `just race` runs them as its own CI job.

## What would reverse it

`cachedir` growing a way to store caller metadata atomically with the body —
at which point the argument for our own format disappears and `tier.Tier` is
the seam to replace it behind.

# Security Policy

## Reporting a Vulnerability

If you discover a security vulnerability, please report it privately via
[GitHub Security Advisories](https://github.com/truvity/ci-cache/security/advisories/new).

Do NOT open a public issue for security vulnerabilities.

## Supported Versions

Only the latest release is supported with security updates.

## Design properties that matter for reports

- **A cache is a supply chain.** Everything this server hands back is
  compiled, linked or installed by something downstream. A way to make it
  serve bytes under a key that was not derived from those bytes is the most
  serious class of report here, and the one most worth looking for.
- **Two ports, two audiences.** The data port carries every front-end and is
  reachable by every CI job. The admin port carries Stats, List, the wipes
  and the UI, and is in no consumer's NetworkPolicy: a job that can wipe can
  empty the cache for everyone, quietly, in a request that returns 200. A way
  to reach an admin operation from the data port is a boundary crossing, not
  a missing feature. See `docs/decisions/0003-two-ports.md`.
- **The proxying front-ends are proxies.** The Gradle distribution proxy
  refuses to start without an allow list, because a distribution proxy with
  none is an open relay — anything that can reach the data port could make
  the cache fetch any URL and serve it back from a name the estate trusts.
  A way past an allow list, or a redirect that escapes one, is a report.
- **The Go module mirror verifies.** It checks against the checksum database
  (`frontends.go.mod.sumdb`). A path that serves a module without that check
  is a report even when the bytes happen to be right.
- **Keys are content-addressed where the protocol says so.** Where a
  front-end's protocol makes the client compute the key, the server trusts
  it, and an estate that wants only CI to fill the cache has
  `frontends.gradle.build.readOnly`. A cross-key or cross-tenant write is a
  report; a client that poisons its own key with `readOnly` off is the
  documented shape.
- **Credentials.** The server holds one identity for one bucket, from the
  pod's own identity or from `store.existingSecret`, never both. It writes
  nothing outside `store.keyPrefix`.

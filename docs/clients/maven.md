# Maven and Gradle repositories

> **Planned.** The `/maven/…` front-end is designed and configured for, but it
> is not in the first release — only `/go/build` and `/go/mod` are. This page
> is the agreed shape, not something to point a build at yet.

One path per configured upstream. `frontends.maven.upstreams` is a map of name
to base URL, and each entry `name: https://…` becomes `/maven/<name>`. Naming
them rather than proxying an arbitrary URL is deliberate: a proxy that fetches
whatever a path says is an open relay with a cache in front of it, and the
first thing it will be used for is not a build.

## The client setting

Gradle:

```kotlin
repositories {
    maven {
        url = uri("http://<service>:8080/maven/central")
        isAllowInsecureProtocol = true  // in-cluster plain HTTP, see below
    }
    mavenCentral()  // the fallback
}
```

Maven, in `settings.xml`, as a mirror:

```xml
<mirror>
  <id>ci-cache</id>
  <url>http://<service>:8080/maven/central</url>
  <mirrorOf>central</mirrorOf>
</mirror>
```

`allowInsecureProtocol` is needed because Gradle refuses a plain-HTTP
repository by default, and this service is plain HTTP inside the cluster: the
hop is a pod-to-pod one inside the CNI, the client is CI and not a browser,
and terminating TLS here would mean a certificate and its rotation for no
attacker this threat model has.

## The fallback

Whatever the build already had. A Maven mirror is `mirrorOf`, so removing it
restores the upstream; in Gradle, `mavenCentral()` after the cache means a
repository the cache cannot answer is fetched directly. **List the cache
first and the upstream second**, because a resolver tries repositories in
order.

There is no automatic failover inside a single repository entry: if the cache
answers slowly, Gradle waits. That is an argument for the cache being a
singleton with a fast disk in front of it, not for a second entry.

## TTLs

| value | governs |
|---|---|
| `frontends.maven.metadataTTL` | `maven-metadata.xml`, the only file a repository is allowed to change for an unchanged coordinate |
| `frontends.maven.negativeTTL` | how long a 404 is remembered, so a missing optional artefact is not re-fetched on every module of every build |

A released artefact is immutable, so it is cached until it is evicted, not
until a clock runs out. A snapshot is not, which is what the metadata TTL is
about; `Invalidate` on the admin port is the way to force one out early
([operations](../operations.md)).

# Gradle

> **Planned.** Neither `/gradle/build` nor `/gradle/dist` is in the first
> release — only `/go/build` and `/go/mod` are. This page is the agreed shape.

Two unrelated things share the prefix: the **remote build cache**, which holds
task outputs a build produced, and the **distributions**, which are the
wrapper's own zips. For Maven *repositories*, see [maven](maven.md).

## The build cache

```kotlin
// settings.gradle.kts
buildCache {
    local { isEnabled = true }
    remote<HttpBuildCache> {
        url = uri("http://<service>:8080/gradle/build/")
        isAllowInsecureProtocol = true
        isPush = System.getenv("CI") != null  // only CI fills it
    }
}
```

`push = true` on CI and not on laptops is the usual arrangement, and it is
what `frontends.gradle.build.readOnly` enforces from the server side for an
estate that would rather not rely on every `settings.gradle.kts` getting it
right: with it set, a `PUT` is refused whoever sends it.

`frontends.gradle.build.maxEntry` caps one entry. A build cache entry is a
task's outputs, and one badly configured task — a whole `build/` directory, a
container image — can be gigabytes. The cap turns that into a refused entry
and a log line rather than an evicted cache.

## The distributions

```properties
# gradle/wrapper/gradle-wrapper.properties
distributionUrl=http://<service>:8080/gradle/dist/gradle-8.14-bin.zip
```

`frontends.gradle.dist.allow` is a list of upstream prefixes this front-end
may fetch from, and it has no default that permits everything: the binary
refuses to start with the distribution front-end on and the list empty,
because a distribution proxy with no allow list is an open relay. The shipped
default allows `https://services.gradle.org/distributions/` and nothing else.

## The fallback

`local { isEnabled = true }`, above, is most of it: Gradle's own local cache
answers a re-run on the same machine, and the remote cache is an optimisation
over it. A remote build cache that is unreachable is a **warning** in the
build log and a slower build, not a failure — Gradle is explicit about this,
which is why the build cache is the one front-end where an outage genuinely
costs nothing but time.

The distributions are different: an unreachable `distributionUrl` fails the
wrapper before the build starts. Keep the upstream URL in the file on branches
that must build without the cluster.

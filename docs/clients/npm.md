# npm

> **Planned.** The `/npm` front-end is designed and configured for, but it is
> not in the first release — only `/go/build` and `/go/mod` are.

A registry proxy in front of `frontends.npm.upstream`
(`https://registry.npmjs.org` by default). Tarballs are immutable and cached
until eviction; packuments — the per-package metadata document — change
whenever anything is published, and `frontends.npm.metadataTTL` is how long
one is reused.

## The client setting

```
# .npmrc
registry=http://<service>:8080/npm/
```

Yarn 4 needs three, because it validates the protocol of a registry and of any
host it is told to trust:

```yaml
# .yarnrc.yml
npmRegistryServer: "http://<service>:8080/npm/"
unsafeHttpWhitelist:
  - "<service>"
```

The trailing slash on the registry URL is not decorative: npm concatenates
paths onto it, and without it the first request is for `…/npm<package>`.

## The fallback

There is none inside the client. npm and Yarn have one registry, and if it
does not answer, `npm install` fails. That is why the estate's jobs **probe
before they override**: `ci-workflows` curls the cache's ping endpoint with a
short timeout and only then writes the registry variables, so a cache that is
down means a job that installs from the public registry rather than a job that
fails.

Anything private stays on its own scope and its own registry line. A scoped
`@scope:registry=` beats the default registry, so the cache being the default
does not route private packages through it.

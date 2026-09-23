# Bazel and moon

> **Planned.** The `/bazel` front-end is designed and configured for, but it is
> not in the first release — only `/go/build` and `/go/mod` are.

The front-end implements the **cache half of the Remote Execution API v2** —
`ContentAddressableStorage`, `ActionCache` and `Capabilities` — over gRPC on
the data port. It executes nothing: an action that misses is run by the
client, exactly as it would be without a remote cache. Executing actions is a
different service with a different threat model, and this one is a cache.

REAPI is the reason the data port is h2c rather than plain HTTP/1.1: gRPC
needs HTTP/2, and serving it beside Connect and the HTTP front-ends on one
port is what keeps a runner pointed at one address.

## The client setting

moon:

```yaml
# .moon/workspace.yml
unstable_remote:
  host: 'grpc://<service>:8080'
```

Bazel itself:

```
build --remote_cache=grpc://<service>:8080
build --remote_upload_local_results=true   # CI only
```

`grpc://` and not `grpcs://`: the hop is inside the cluster and there is no
TLS on the data port.

## The fallback

Both clients treat a remote cache as best-effort by default and fall back to
local execution when it is unreachable, with a warning. Bazel's
`--remote_local_fallback` governs the *execution* case and is irrelevant here,
since this front-end never executes anything; what matters is not setting
`--remote_cache` to something mandatory, such as a build without
`--remote_download_outputs` set to a mode that can be served locally.

Keep uploads to CI. A developer's laptop pushing action results into a shared
CAS is how a cache acquires entries nobody can reproduce.

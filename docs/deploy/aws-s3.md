# Deploying on AWS S3

One release per cluster, in a namespace of its own. This is the AWS variant;
[Cloudflare R2](cloudflare-r2.md) is the other, and it differs in three ways
that have to be said at once, which is why it is a separate page.

Everything particular — the bucket, the account, the namespace, the consumer
list — is an input with a neutral default. Nothing in this repository names an
estate, and `just leak-canary` is what keeps it that way.

The example this page describes is `charts/ci-cache/examples/aws-s3.yaml`, and
`just chart` renders it on every run: a documented example that no longer
templates is a red mark rather than somebody's afternoon.

## What to prepare

1. **A bucket**, versioning off, in the cluster's region — or knowingly not,
   since a cross-region cache works and costs a transfer charge per miss.
   Versioning would keep every generation of an overwritten key and bill for
   it, and this is a cache: a lost object is re-derived.
2. **A lifecycle rule** expiring objects after 30 to 90 days. The disk tier
   has watermarks and the bucket has none, so without a rule the bucket grows
   forever — and expiry is the only thing that removes objects for a toolchain
   version nobody builds with any more.
3. **A VPC gateway endpoint for S3**, if the cluster has not got one. Without
   it every fault-in leaves through a NAT gateway and is billed per gigabyte,
   which is most of what this service exists to save.
4. **A role** with four actions on this bucket and nothing on any other:

   ```json
   {
     "Effect": "Allow",
     "Action": ["s3:GetObject", "s3:PutObject", "s3:DeleteObject", "s3:ListBucket"],
     "Resource": ["arn:aws:s3:::<bucket>", "arn:aws:s3:::<bucket>/*"]
   }
   ```

   `DeleteObject` is not optional paranoia: the cache deletes what it evicts
   and what `WipeBucket` is asked to remove. If the bucket is shared, scope
   the role with the same `store.keyPrefix` the cache is given.

## Pod identity, or a Secret

**On AWS, pod identity — always.** EKS Pod Identity or IRSA puts the role on
the chart's ServiceAccount, the SDK's default chain picks it up inside the
pod, and no long-lived key exists to rotate, leak or forget:

```yaml
serviceAccount:
  create: true
  name: ci-cache
  annotations:
    eks.amazonaws.com/role-arn: "arn:aws:iam::<account>:role/<role>"
```

`store.existingSecret` is for a store with no pod identity — R2, or anything
else off-cloud. It names a Secret this chart does not create, holding
`AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY` (and optionally
`AWS_SESSION_TOKEN`), handed to the container with `envFrom`. The chart never
generates one, because a generated credential is one Helm knows about and
nobody else does.

**Naming both is refused at render time.** Two identities for one process
means the one that wins is a property of the SDK's version rather than of this
file, and the failure surfaces as a signature error weeks later.

## The values

The chart's values **are** `config.Config` — every key under `store`,
`frontends`, `persistence`, `gc`, `upload`, `service`, `server`, `telemetry`
and `logLevel` is a field of that struct, spelled the same way, reaching the
binary as `CI_CACHE_<SECTION>_<NAME>`. Everything else in the file is how
Kubernetes is told to run it. One schema is why the chart, the binary and this
page cannot end up describing different servers.

The AWS shape is short, because most of the schema is already right:

```yaml
store:
  bucket: <bucket>
  region: <region>
  # endpoint, pathStyle: left alone on AWS. An endpoint also turns off the
  # SDK's default checksums, which AWS has no reason to skip.

serviceAccount:
  create: true
  annotations:
    eks.amazonaws.com/role-arn: "arn:aws:iam::<account>:role/<role>"

persistence:
  enabled: true
  size: 200Gi
  storageClass: <storage-class>

frontends:
  go:
    build:
      enabled: true
    mod:
      enabled: true

networkPolicy:
  enabled: true
  consumerNamespaces:
    - <namespace>
```

### Every value

| value | what it decides |
|---|---|
| `store.bucket` | the bucket. No default: one named by the chart would be somebody else's, and rendering without it is refused |
| `store.region` | required always — see [R2](cloudflare-r2.md) for why guessing is not an option |
| `store.endpoint` | empty keeps AWS; an absolute URL points at another store and turns the SDK's default checksums off |
| `store.pathStyle` | `endpoint/bucket/key` addressing, a property of the store's certificate rather than of the endpoint |
| `store.keyPrefix` | in front of every key, for a bucket that already holds something else. Changing it on a running installation is a cold cache, not a migration |
| `store.existingSecret` | static credentials for a store with no pod identity. Empty keeps the pod's ambient identity |
| `frontends.go.build.enabled` | the Go build cache over GOCACHEPROG — [clients/go](../clients/go.md) |
| `frontends.go.build.legacyPrefix` | consulted on a bucket miss, for a bucket warmed by `go-cache-plugin`. See [the transition](#a-bucket-already-written-by-go-cache-plugin) |
| `frontends.go.mod.enabled` | the module proxy and sumdb mirror |
| `frontends.go.mod.upstream` | the proxy in front of which this one sits |
| `frontends.go.mod.sumdb` | the checksum database the mirror serves. Emptying it does not make the proxy faster, it makes it unverified |
| `frontends.go.mod.listTTL` | how long `@v/list` and `@latest` are reused. Longer than a release cadence and a freshly published module looks missing |
| `frontends.go.mod.legacyPrefix` | the same transition, for module objects |
| `frontends.maven.enabled` `.upstreams` `.metadataTTL` `.negativeTTL` | [clients/maven](../clients/maven.md) — planned, off |
| `frontends.gradle.build.enabled` `.readOnly` `.maxEntry` | [clients/gradle](../clients/gradle.md) — planned, off |
| `frontends.gradle.dist.enabled` `.allow` | the wrapper distributions and their allow list — planned, off |
| `frontends.nix.enabled` `.upstreams` `.priority` `.negativeTTL` | [clients/nix](../clients/nix.md) — planned, off |
| `frontends.npm.enabled` `.upstream` `.metadataTTL` | [clients/npm](../clients/npm.md) — planned, off |
| `frontends.bazel.enabled` | [clients/bazel](../clients/bazel.md) — planned, off |
| `persistence.enabled` | off gives an `emptyDir`, which empties on every restart and is refused while a build cache front-end is on |
| `persistence.size` | the working set, not the corpus — see [sizing](#sizing) |
| `persistence.storageClass` | the class the claim asks for |
| `persistence.existingClaim` | a claim made elsewhere, including one that already holds a warm cache. Set it and the chart creates none |
| `persistence.dir` | where the volume is mounted in the container. It rarely moves |
| `persistence.ephemeralIsAcceptable` | says a volume that does not survive a restart is intended. The only way past the refusal above, and explicit so nobody reaches it by accident |
| `gc.floor` `gc.high` `gc.low` `gc.budgetBytes` `gc.budgets` | [operations](../operations.md#garbage-collection) |
| `upload.concurrency` `upload.queue` `upload.queueBytes` | the write-behind queue: how many workers, how many objects, how many bytes |
| `upload.minSize` | skip the bucket below this size. Zero uploads everything; a few kilobytes is right for a Go estate, whose build cache is full of hundred-byte objects |
| `service.data` `service.admin` | the two ports ([0003](../decisions/0003-two-ports.md)). Rendering them equal is refused |
| `server.concurrency` | in-flight requests. Unbounded concurrency turns a thundering herd into an OOM |
| `server.drainTimeout` | how long the server keeps serving after SIGTERM; the pod's termination grace is this plus ten seconds — [operations](../operations.md#drain-and-restart) |
| `networkPolicy.enabled` `networkPolicy.consumerNamespaces` | [below](#the-networkpolicy) |
| `serviceAccount.create` `serviceAccount.name` `serviceAccount.annotations` | the identity, above |
| `telemetry.otlpEndpoint` `telemetry.traceRatio` | empty sends nothing; the ratio samples, because a cache serves very many very short requests |
| `logLevel` | how much is said |

### What the chart refuses to render

Each of these is a configuration the binary rejects at start-up, or accepts
and then gets quietly wrong. The chart catches them before a cluster sees
them, and the binary catches them when somebody runs it by hand; two gates for
one rule is not duplication.

- No `store.bucket`, or no `store.region`.
- `service.data` equal to `service.admin`.
- `gc.low` at or above `gc.high`.
- A build cache front-end on an `emptyDir`, without
  `persistence.ephemeralIsAcceptable`.
- `store.existingSecret` together with an identity annotation on the
  ServiceAccount.
- `frontends.maven.enabled` with no upstreams; `frontends.gradle.dist.enabled`
  with an empty allow list; `frontends.nix.enabled` with no upstreams.

## Sizing

**The volume** holds the working set, not the corpus — the bucket holds the
corpus. 50Gi is the chart's default and 200Gi is the shape a Go estate
settles at. The number to watch is `cicache.gc.evicted_bytes` against
`cicache.get`: eviction that tracks every write, while the bucket's share of
hits rises, means the volume cannot hold what recent builds are asking for.
Resizing is the whole change, because the budget comes from `statfs`
([operations](../operations.md#garbage-collection)).

**`gc.floor`** at 10% is right for a volume this service owns alone. Raise it
when the volume is shared with anything that writes without asking: the floor
is what stops the cache filling a filesystem out from under its neighbour. A
full volume is not a slow cache, it is a failing writer, and on many
filesystems the failure arrives well before 100%.

## A bucket already written by `go-cache-plugin`

The `go/build` key layout is deliberately that plugin's
([0004](../decisions/0004-reimplementation-not-fork.md)), so an existing
bucket keeps answering through the switch:

1. Deploy with `frontends.go.build.legacyPrefix` set to the prefix the plugin
   wrote under, and `store.keyPrefix` empty. Writes go to the new layout; a
   miss under it is retried under the legacy prefix, so day one is warm.
2. Watch the hit rate for a few weeks. Traffic drains out of the legacy prefix
   on its own, because nothing writes there any more.
3. Clear `legacyPrefix`, and then `WipeBucket` that prefix from the admin port
   to stop paying for it.

`store.keyPrefix` answers a different question — a bucket that holds something
*else*. Putting the cache under a prefix of its own keeps the lifecycle rule,
the IAM scope and the wipes inside it; the garbage collector's first listing
otherwise walks that something else too. Empty gives the natural layout
(`go/build/…`, `go/mod/…`), which is what a dedicated bucket should have.

## The NetworkPolicy

```yaml
networkPolicy:
  enabled: true
  consumerNamespaces:
    - <namespace>
```

The namespaces whose pods may reach the **data** port, matched on the
`kubernetes.io/metadata.name` label the kubelet sets: the runner namespaces,
and any namespace whose builds use the module proxy.

**An empty list admits nobody**, which is the safe reading and is deliberate:
the classic bug in a chart like this one is the other reading, where an empty
list renders a rule with an empty `from` and admits the whole cluster. Name
the namespaces, or leave the policy off and say so.

**The admin port is not in that list and never is**
([0003](../decisions/0003-two-ports.md)). Operators reach `:8081` with
`kubectl port-forward`, which is already how they reach anything else without
an ingress.

Egress the cache needs: the bucket — a prefix list rather than the internet,
with a gateway endpoint — plus DNS, the OTLP collector, and for `/go/mod` the
upstream proxy and sumdb. Without that last one the module proxy can only
serve what it already has, which looks exactly like a cache that has gone
cold.

## After it is up

- `readyz` on the data port answers `503` with the reason in the body until
  the installation's readiness check passes. A pod that never becomes ready is
  usually a credentials or endpoint problem, and the body and the log say
  which.
- `Stats` on the admin port through a port-forward: `index_cold` should go
  false within a minute or two, and `disk_budget_bytes` should be roughly the
  volume less `gc.floor`.
- Point one job at it ([clients/go](../clients/go.md)) and look for its second
  run reading rather than writing.

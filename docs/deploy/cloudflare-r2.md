# Deploying on Cloudflare R2

R2 speaks the S3 API, and this service runs on it unchanged — but three
settings have to be right **at once**, and getting two of them right produces
a failure that reads like a fourth problem entirely.

Everything else is the same as on [AWS](aws-s3.md), including the values
reference, the sizing, the legacy-prefix transition and the NetworkPolicy.
This page is only what differs. The example it describes is
`charts/ci-cache/examples/cloudflare-r2.yaml`, rendered by `just chart` on
every run.

## The three settings

```yaml
store:
  bucket: <bucket>
  region: auto
  endpoint: https://<account>.r2.cloudflarestorage.com
  pathStyle: true
  existingSecret: ci-cache-r2
```

| setting | why | what it looks like when it is wrong |
|---|---|---|
| `store.region: auto` | R2 refuses a bucket-location lookup, so an SDK left to guess signs with whatever the pod's environment happens to carry | a signature error, which reads as a credentials problem and is not |
| `store.endpoint` | it is another store, and setting it also turns off the SDK's default request and response checksums, which R2 rejects | uploads fail once the first object is written, not at start-up |
| `store.pathStyle: true` | the wildcard certificate does not cover a bucket *below* the account | the TLS handshake fails, before anything HTTP-shaped happens |

### The checksum trap, in full

The bucket tier sends checksum **values** and never checksum **algorithms**.
Naming an algorithm alongside a `Content-Encoding` makes the AWS SDK switch to
the `aws-chunked` trailer encoding, and R2 answers that with
`403 SignatureDoesNotMatch` (measured 2026-09-23).

That is worth knowing even though the code already does the right thing,
because the same `403` is what a wrong access key produces. If uploads fail
with it after a working start-up, the credentials are almost certainly fine
and something has reintroduced an algorithm header.

## Credentials

R2 has **no pod identity**. The credentials are an R2 API token's access key
id and secret, in a Secret the chart does not create:

```
kubectl create secret generic ci-cache-r2 \
  --from-literal=AWS_ACCESS_KEY_ID=<access-key-id> \
  --from-literal=AWS_SECRET_ACCESS_KEY=<secret-access-key>
```

and then `store.existingSecret: ci-cache-r2`. Scope the token to this bucket
with object read and write; the cache deletes what it evicts, so a read-only
token deploys and then fails at the first eviction rather than at start-up.

**Do not also put an identity annotation on the ServiceAccount.** Two
identities for one process is refused at render time, because which one wins
would be a property of the SDK's version rather than of the values file.

Rotation is the estate's, on its own schedule: replace the Secret's contents
and restart the pod, which is a short cache outage that every client has a
fallback for ([operations](../operations.md#drain-and-restart)).

## Sizing, and what is different about the economics

The volume and `gc.floor` are sized exactly as [on AWS](aws-s3.md#sizing).
What differs is what a miss costs.

R2 has no egress charge, so a fault-in from the bucket costs operations
rather than bandwidth. That makes `upload.minSize` worth setting — the
example uses 4096 — because a Class A operation per hundred-byte object is
the cost that actually accumulates here, and such an object is cheaper to
re-derive than to store.

It does **not** make the disk tier less important. A miss is still a build
waiting on a round trip to another network, and latency is what the volume in
front of the bucket is for.

There is no VPC endpoint to arrange and no NAT gateway in the path, so the
egress rule is plain internet egress to the R2 endpoint, plus DNS, the OTLP
collector and — for `/go/mod` — the upstream proxy and sumdb.

## Lifecycle

R2 has object lifecycle rules, and the cache needs one for the same reason it
does on S3: the disk tier has watermarks and the bucket has none. Expire
objects after 30 to 90 days on the prefix the cache writes.

## Everything else

- [The values, in full](aws-s3.md#every-value) — identical on both stores.
- [What the chart refuses to render](aws-s3.md#what-the-chart-refuses-to-render).
- [A bucket already written by `go-cache-plugin`](aws-s3.md#a-bucket-already-written-by-go-cache-plugin)
  — the layout is the same wherever the bucket is.
- [The NetworkPolicy](aws-s3.md#the-networkpolicy) — `consumerNamespaces`, and
  the admin port in nobody's list.

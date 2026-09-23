# Documentation

Start with [architecture](architecture.md): the tier, the chain, the two
ports, and why three quite different deployments are the same binary. Then
pick the road you are on.

| you are | start at |
|---|---|
| pointing a build at the cache | the [client page](#clients) for your tool |
| installing it in a cluster | [AWS S3](deploy/aws-s3.md) or [Cloudflare R2](deploy/cloudflare-r2.md) |
| running one | [operations](operations.md) — watermarks, the three wipes, the metrics, drain and restart |
| asking why it is shaped this way | [the decisions](decisions/README.md) |

## Clients

One page per front-end. Each gives the exact setting and names the fallback
that client has when the cache is down — every one of them has one, and a
client wired without it turns a rollout into a CI outage.

| front-end | path | page | first release |
|---|---|---|---|
| Go build cache, Go modules | `/go/build`, `/go/mod` | [go](clients/go.md) | ships |
| Maven repositories | `/maven/<name>` | [maven](clients/maven.md) | planned |
| Gradle build cache and distributions | `/gradle/build`, `/gradle/dist` | [gradle](clients/gradle.md) | planned |
| Nix binary cache | `/nix` | [nix](clients/nix.md) | planned |
| npm registry | `/npm` | [npm](clients/npm.md) | planned |
| Bazel and moon | `/bazel` | [bazel](clients/bazel.md) | planned |

**Only the Go front-ends ship in the first release.** The others are designed
and configured for, and their pages say so at the top — the shape is agreed,
the code is not written.

## Deploying

| page | for |
|---|---|
| [AWS S3](deploy/aws-s3.md) | the values in full, pod identity, sizing, the legacy prefix, the NetworkPolicy |
| [Cloudflare R2](deploy/cloudflare-r2.md) | the three settings R2 needs at once, its credentials, and what a miss costs there |

Both render an example the chart's own `just chart` recipe templates, so a
page that has drifted from the chart is a failed build rather than a surprise
in a cluster.

## Decisions

| id | title |
|---|---|
| [0001](decisions/0001-net-http-not-fiber.md) | `net/http` everywhere, not fasthttp |
| [0002](decisions/0002-cachedir-kept.md) | The GOCACHEPROG protocol stays library code; the disk tier's file format does not |
| [0003](decisions/0003-two-ports.md) | Two ports: machines on one, people on the other |
| [0004](decisions/0004-reimplementation-not-fork.md) | A reimplementation, not a fork of `go-cache-plugin` |

## The gate

`just docs` runs `hack/check-docs.sh`, which holds these pages to the code:
every front-end path in [architecture](architecture.md)'s table has a client
page, every value in the chart appears somewhere here, and no relative link
between these files is broken. A behaviour nobody wrote down is not a
behaviour, and a value nobody documented is a red mark.

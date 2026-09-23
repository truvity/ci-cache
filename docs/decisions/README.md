# Decisions

Architecture decision records: the choices any deployer of this service would
also face, each with what would reverse it. Deployment-specific choices — which
bucket, which namespace, how large the volume — belong to the deployer and are
recorded in the [deploy pages](../deploy/aws-s3.md).

| id | title | status |
|---|---|---|
| [0001](0001-net-http-not-fiber.md) | `net/http` everywhere, not fasthttp | accepted |
| [0002](0002-cachedir-kept.md) | The GOCACHEPROG protocol stays library code; the disk tier's file format does not | accepted |
| [0003](0003-two-ports.md) | Two ports: machines on one, people on the other | accepted |
| [0004](0004-reimplementation-not-fork.md) | A reimplementation, not a fork of `go-cache-plugin` | accepted |

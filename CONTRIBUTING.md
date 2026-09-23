# Contributing

## Ground rules for a public repository

This repository is public. Nothing in it may name a real organisation,
cluster, account, bucket, hostname, team, person, incident or internal
ticket. The mechanism belongs here; the particulars are caller inputs, and
they live with the deployer.

`just leak-canary` enforces that mechanically rather than by memory, because
this history cannot be unpublished — rewriting it changes the SHAs, not what
was already fetched. Add a pattern to `hack/leak-canary.sh` the first time
something new turns out to be a particular, and never add an exception
without one.

## One schema

`config/config.go` is the configuration, and it is the only one. The same
shape is the YAML file, the flags, the environment variables
(`CI_CACHE_<SECTION>_<NAME>`) and `charts/ci-cache/values.yaml`. Adding a
field means adding it in all of them in the same change: two schemas for one
server is how the chart and the binary come to describe different things,
which they will, the moment there are two.

`config.Validate` and `charts/ci-cache/templates/_checks.tpl` refuse the same
configurations. That is not duplication — the chart catches it before a
cluster sees it, and the binary catches it when somebody runs the server by
hand — but a rule added to one belongs in the other.

## Decisions

Every decision a stranger deploying this component would also face is
recorded under `docs/decisions/` in the MADR format. A decision that applies
to one deployment only is not recorded here. Decisions are never edited after
acceptance; they are superseded by a new one that links back.

## Documentation

- `docs/architecture.md` is the entry point and must stay readable by someone
  who has never seen the code.
- `docs/clients/` has a page per front-end, and `docs/deploy/` a page per
  store. `just docs` fails if a front-end the server mounts has no client
  page, or a chart value appears in no deploy page.
- The CHANGELOG describes the state of the repository, not the journey.

## Tooling

Tools come from `devbox.json` through direnv. Never hand-roll a PATH; add a
missing tool with `devbox add <pkg>@<version>`, and commit `devbox.lock` in
the same change — a lock that lags its `devbox.json` fails every CI job with
"modified the working tree".

`just check` is the gate. It needs nothing but this checkout: no cluster, no
bucket, no network, no C toolchain. What needs more is its own recipe, which
CI runs as its own job, and those matter as much.

- `just race` needs a C toolchain, which nothing else here does. Run it
  before changing anything that hands an object between a request goroutine
  and the background writer: a race there is a corrupted entry served to
  every build afterwards, not a crash somebody notices.
- `just chart` renders the chart four ways into `testdata/golden/` and runs
  `testdata/refuse.sh`. The goldens are committed so that a template change
  that alters a manifest is a diff a reviewer reads rather than a surprise in
  a cluster. The refusals matter more: each is a configuration the binary
  rejects at start-up, or accepts and then gets quietly wrong.
- The store-backed tests skip without their environment variable, and a
  cache is exactly the kind of component whose bugs only appear against a
  real store. A change to the bucket tier is not finished until they have
  run.

## The chart

`charts/ci-cache/examples/` is what the deploy pages show, and `just chart`
renders both examples, so a documented example that no longer renders is a
red mark rather than somebody's afternoon. Change the example and the page
in the same commit.

Two rules in the chart are worth stating because they are easy to undo:

- The data port and the admin port are separate Services. The admin port
  carries the wipes, and a job that can wipe can empty the cache for everyone
  in a request that returns 200. Never put them in one Service.
- An empty `networkPolicy.consumerNamespaces` must render a policy with no
  ingress rules, which admits nobody. The classic bug is the inversion — a
  rule whose `from` came out empty admits the whole cluster and looks
  identical in a diff — and `refuse.sh` asserts against it.

## Commits and pull requests

Small, reviewable pull requests. A pull request that changes a contract
(`proto/`, `config/config.go`, the chart's values) updates the matching
reference page, and, if the change is not additive, a decision record.

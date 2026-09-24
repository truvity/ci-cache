# The setup action

`truvity/ci-cache/setup` is a composite action that looks at a repository,
works out which build systems it uses, and wires each one to this estate's
cache. It is the client half of the bundle, versioned with the chart and the
client tools so that the three cannot disagree.

## It is nested, and that is deliberate

Only the shared CI workflow calls it:

```yaml
# ci-workflows/.github/actions/setup-devbox/action.yaml
- uses: truvity/ci-cache/setup@<sha>   # vX.Y.Z
  with:
    bucket: ${{ inputs.cache-bucket }}
    region: ${{ inputs.cache-region }}
```

A repository keeps **one** pin, to the shared workflow, and never names this
action. The alternative — every repository wiring the cache action beside the
CI workflow — doubles the number of estate-wide pins each repository carries,
and the estate has already learned what that costs: six repositories once
disagreed with themselves about a single shared pin, and nobody noticed until
a sweep went looking.

## What it detects, and why it does not ask

| marker in the tree | wires |
|---|---|
| `go.mod` | the Go build cache, through `go-cache-plugin` to the bucket |
| `devbox.json` or `flake.nix` | Nix |
| `yarn.lock` or `package-lock.json` | the npm registry |
| `.moon/` | moon's task cache |
| `build.gradle` or `build.gradle.kts` | Gradle's remote build cache |

A marker is a file the build system cannot work without, so its presence is
not a guess. The `languages:` input overrides detection for the repository
whose tree genuinely does not say — but a list of languages that lives
outside the repository drifts from it, and the drift is silent, so the
override is the exception and not the interface.

## Where it writes, and where it does not

It writes to `$GITHUB_ENV`, `$HOME` and `$RUNNER_TEMP`. It does **not** edit
the repository.

Some caches are configured by an environment variable (Go), and some by a
file the repository owns (`.yarnrc.yml`, `.moon/workspace.yml`, a Gradle init
script). For the second kind the action *checks* that the file names the
endpoint it expects and warns when it does not. An action that edited those
files would fight the tree and leave a diff behind in every job.

## Failing open

A job whose cache is unavailable must be slower, never broken:

| situation | what happens |
|---|---|
| GitHub-hosted runner | wires nothing, says why, exits 0 — no estate backend is reachable from there |
| no bucket configured | wires nothing, warns, exits 0 |
| client binary not on `PATH` | that cache is off for the job, warns, exits 0 |
| a client binary that fails **checksum** verification | the job **fails** |

The last is the one hard failure, and it is the right one: running an
unverified binary is worse than a slow build.

## The cache directory

```
CI_CACHE_NODE_DIR  →  a node-persistent directory, mounted by the runner
(unset or missing)  →  $RUNNER_TEMP/ci-cache, discarded with the job
```

The directory is a **runner** concern (ci-plane's scale sets provide the
mount) serving a **cache** need (this action uses it), so neither side
hard-codes the path: the chart exports the variable and the action reads it.
A scale set that has lost its mount falls back silently to the job's own
temp, which is correct and merely cold — and the action prints which one it
chose, so a job that quietly ran without the shared directory says so in its
own log.

### Sharing it is safe because nothing prunes

A node-persistent directory is written by every runner on that node at
once, so the question that decides whether the mount is a good idea is:
what deletes from it?

Today, nothing. `go-cache-plugin` prunes by calling
`cachedir.Cleanup(expiry)`, and that returns `nil` for `expiry <= 0` — no
pruner is installed at all. `GOCACHE_EXPIRY` defaults to `0`, so the
default is the safe one. **This action pins it to `0` anyway**, because a
safety property that depends on nobody setting a variable is not a safety
property.

Above zero it is genuinely unsafe to share. Every job close runs a full
mark-and-sweep: mark walks `action/` and collects the object ids those
actions reference; sweep walks `output/` and removes every object the mark
phase did not see. An object written by runner B *after* runner A's mark
phase is not in A's keep set, so A's sweep deletes it while B is still
building against it.

Most of that race is survivable — `cachedir.Get` stats the object and
compares its size, treating a mismatch as a miss — so a swept object
usually costs a refetch. What it cannot cover is the window *after* `Get`
has handed the compiler a path: the file is removed, and the compiler opens
something that is no longer there. A build failure with no explanation in
it, on a node that looks healthy.

So the action pins `GOCACHE_EXPIRY=0` in every job, and **warns** when the
directory is shared and a caller had set it to something else. On a
job-local directory it pins silently: nothing is at risk there, the
directory dies with the job, and a warning printed on every job of every
repository is a warning people learn to scroll past.

The directory's size is therefore a **node's** problem, not a job's, and
that is deliberate: trimming belongs to something that can see the whole
directory and knows no build is reading it. Upstream growing touch-on-use
and a concurrency-safe trim would change this; until then, see INF-961.

## Why every decision is printed

The estate has already had a cache that was configured, believed in, and
doing nothing: an org variable named a module proxy that had been removed two
releases earlier, and because the `GOPROXY` separator was `|` rather than
`,`, Go fell through to the public proxy in four milliseconds and no job ever
failed. It went unnoticed for days.

So this action says what it wired, from which source, and into which
directory — one line per language — and warns whenever it wires nothing. A
cache that cannot be seen working cannot be trusted to be working.

## Testing it

`just setup-action` extracts each step's `run` body **by name** and executes
it against a stubbed environment: the detection table, the fail-open paths,
which variables each shape sets, and the promise that the inspection steps
never modify the tree. A renamed step is a loud failure rather than a
silently skipped case, and a run that checks nothing refuses to pass.

That harness runs on a hosted runner, because it needs no backend. The
end-to-end proof — that this action, wiring this version's chart, produces
hits on the second run — needs the cluster, so it is a separate ARC workflow
and a post-merge gate.

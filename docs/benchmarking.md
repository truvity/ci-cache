# Benchmarking

`ci-cache-bench` loads a running server and reports what it did. It exists
because CI is the wrong instrument for a performance question: a job takes
minutes, conflates compile time with cache time, runs on whichever runner is
free, and answers one point on one curve.

It is a separate binary from `ci-cache` on purpose. The server ships in a
distroless image sized to the byte, and a load generator has no business in
it — nothing in production should be able to point the cache at itself and
fill a bucket.

## The workload's shape

A Go build cache lookup is **two** requests, not one: a small action record
(84 bytes, a fixed format) and then a large output object. They are bound by
different things — the record is latency and the output is throughput — so a
benchmark that generates one uniform object size measures neither. The corpus
is one record and one output per lookup, and that ratio is not a knob: a
bench whose request mix can be tuned away from the real one is a bench that
can be made to say anything.

Output sizes are drawn from a log-normal with a ~300 KiB median, capped at
64 MiB. The cap matters: without it a seed can draw a several-hundred-megabyte
object, one request dominates the run, and two runs become incomparable for a
reason that has nothing to do with the server.

The corpus is deterministic from `--seed`. Two runs against two builds must
ask for the same bytes in the same order, or the difference between them is
the corpus rather than the change.

## Scenarios

The three are not variations on one test. They ask different questions, and a
change can move one without touching another.

| scenario | starting state | what it measures |
|---|---|---|
| `warm-disk` | everything on the server's disk tier | the server and the wire, with the object store out of the picture |
| `cold-bucket` | nothing on disk, everything in the bucket | the fault-in path |
| `mixed` | a `--hit-ratio` share warmed by reading it | the closest to a real job, and the least useful for attributing a change to a cause |

`mixed` warms by **reading**, not by placing entries on disk by hand. A
hand-placed disk tier could hold entries the server would never have written,
and the bench would be measuring a cache that cannot exist.

Everything but `warm-disk` needs `--admin`, because reaching the starting
state means wiping the disk tier.

## Running it

```
just bench --server http://localhost:8080/go/build \
           --admin  http://localhost:8081 \
           --scenario cold-bucket --concurrency 64 --duration 60s
```

The sweep is the useful form — one point tells you a number, a curve tells
you where the limit is:

```
just bench-sweep http://localhost:8080/go/build http://localhost:8081
```

Reports append as one JSON object per line, so an interrupted sweep still
leaves a readable file and two runs diff.

## Against a cache real jobs depend on

Don't, if you can avoid it — but when you must, `--key-prefix` segregates the
run under `go/build/<prefix>/` and `--cleanup` deletes it again afterwards.

`--cleanup` refuses to run without `--key-prefix`, and the refusal is the
point: without a prefix the corpus root **is** `go/build/`, and the cleanup
would delete the entire Go build cache every job in the cluster is using.

Note that wiping the disk tier is not segregated the same way — `cold-bucket`
and `mixed` wipe only the corpus's own prefix, so other entries survive, but
a shared server under benchmark load is still a shared server under
benchmark load.

## Reading the numbers

**First byte and complete are separate, and the gap is the finding.** First
byte is the round trip plus whatever the server does before it starts
writing. While a fault-in buffers, that is the whole object. A change that
streams properly moves first byte a long way and complete hardly at all — a
single "latency" number would show that as noise.

**Percentiles are exact**, computed by sorting the whole sample set rather
than from a streaming estimator. p99 is the number a change has to move, and
an estimator's p99 is that number plus an apology.

**Errors are counted, not folded into the distribution.** A fast failure in
p50 makes a broken server look quick.

**A run that measured nothing is an error, not a fast run.** Zero at every
percentile reads like the best result the bench has ever produced, so both
traps — no completed requests, and every request failing — refuse instead of
reporting.

## Where to run it from

**Not over `kubectl port-forward`.** That measures your laptop's link to the
cluster, and it saturates long before the server does. A port-forwarded run
is good for proving the harness works and for nothing else.

A real baseline runs in the cluster, next to the server, on the same kind of
node a CI job uses. The ones taken that way live in [bench/](bench/README.md). Numbers captured any other way should say so in the
report's notes, or they will be compared against in-cluster numbers a month
from now by someone who did not know.

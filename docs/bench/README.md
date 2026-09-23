# Baselines

One file per measured version. A baseline is only useful against another
baseline taken the same way, so each file records the version, the hardware,
and how the run was driven — a number without those is not comparable to
anything.

See [benchmarking](../benchmarking.md) for how to take one.

| version | date | what it covers |
|---|---|---|
| [0.1.2](0.1.2.md) | 2026-09-24 | `warm-disk` concurrency sweep, h2c and http1, one replica |

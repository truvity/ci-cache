# 0003. Two ports: machines on one, people on the other

- Status: accepted
- Date: 2026-09-23

## Context

The cache serves two audiences with nothing in common. Every CI job in the
estate talks to it — that is the point — and a CI job is arbitrary code
someone pushed to a branch. Operators need `Stats`, `List` and three wipes,
and those calls can empty the cache for everyone or, worse, leave it holding
objects the bucket no longer has.

Separating them by path and guarding the admin paths with a token puts the
boundary inside the process, where it is one routing mistake, one middleware
ordering bug or one leaked token away from being no boundary at all. And a
token shared by every CI job is not a secret.

## Decision

Two listeners. `:8080` carries every front-end and is reachable by every CI
job. `:8081` carries the admin service and the UI, and **never appears in a
consumer NetworkPolicy**. The binary refuses to start when the two port
numbers are equal.

## Consequences

- The boundary is enforced by the cluster's network policy, which is audited
  and reviewed, rather than by this service's routing table.
- Reaching admin from a laptop is a port-forward, which is already how an
  operator reaches anything else that has no ingress.
- There are two Service ports and two probes' worth of chart, and someone
  eventually has to be told why `curl` against the data port cannot call
  `Stats`. That is the cost of the boundary being real.

## What would reverse it

Admin becoming something a CI job legitimately needs — a per-job invalidation,
say. That is an argument for a narrow, authenticated call on the *data* port
with no wipe in it, not for merging the ports.

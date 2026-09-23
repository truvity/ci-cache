# Nix

> **Planned.** The `/nix` front-end is designed and configured for, but it is
> not in the first release — only `/go/build` and `/go/mod` are.

The front-end is a binary cache **origin**: it serves `nix-cache-info`,
`<hash>.narinfo` and `nar/…` in front of the upstreams listed in
`frontends.nix.upstreams` (`https://cache.nixos.org` by default).

## The client setting

```
substituters = http://<service>:8080/nix https://cache.nixos.org
```

on a machine, or `--substituters` for one invocation. The list is the
fallback: Nix asks every substituter it has and takes the first that answers,
so an unreachable cache costs a connection timeout per path and then the
upstream serves the build. Nothing fails.

**Signatures are not proxied away.** Nix verifies each path's signature
against `trusted-public-keys`, and the narinfo this service serves carries the
upstream's signature unchanged. It does not sign, so this cache cannot make a
path trusted that was not already: it moves bytes, not trust.

## Priority

`frontends.nix.priority` is what `nix-cache-info` advertises, and **Nix
prefers the lowest number**. `cache.nixos.org` says 40, so the default here is
10: low enough to be asked first, and far enough from the boundary that an
upstream changing its own number does not silently reverse the order.

Get it backwards and nothing breaks visibly — Nix simply asks the internet
first and this cache never sees a hit, which looks exactly like a cache that
is not working.

`frontends.nix.negativeTTL` is how long a path not being in the upstream is
remembered. Nix asks about a great many paths that nobody has built, and
without a negative cache each one is an upstream round trip on every
evaluation.

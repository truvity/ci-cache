# 0001. `net/http` everywhere, not fasthttp

- Status: accepted
- Date: 2026-09-23

## Context

The estate's Go services are built on Fiber, so the question was asked and had
to be answered rather than assumed. Fiber is fasthttp, and fasthttp is not an
implementation of `net/http`: it has its own request and response types, and
its own server loop.

Every part of this service is somebody else's `net/http` code. The build cache
is a ConnectRPC handler, which is an `http.Handler`. The module proxy is
`golang.org/x/mod/... goproxy`, which is an `http.Handler`. The Maven, npm and
Gradle proxies are `httputil.ReverseProxy`, which is an `http.Handler` over an
`http.RoundTripper`. None of them can be hosted on fasthttp without an adapter
that reconstitutes a `net/http` request per call, and the adapter buffers.

## Decision

Every listener in this repository is `net/http`, served as h2c on the data
port so that one listener carries HTTP, Connect and gRPC together.

## Consequences

- The two properties a byte-serving cache needs most are kept: **streaming**,
  so an object of any size costs bounded memory rather than its own length,
  and **HTTP/2**, which gRPC requires and which lets one connection from a
  runner carry many concurrent cache operations.
- Throughput on tiny requests is lower than fasthttp's. That is the right
  trade here: this service's requests are objects, not JSON documents, and it
  is bounded by disk and network rather than by header parsing.
- Handlers from the standard library and from upstream projects are used as
  they are, which is most of the code this service does not have to write.

## What would reverse it

A front-end that is genuinely request-rate-bound rather than byte-bound, and
that is not somebody else's `http.Handler`. It would be its own process before
it would be a second server type inside this one.

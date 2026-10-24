# Prisma

A reverse-proxy API gateway in Go for HTTP, GraphQL and gRPC traffic. It handles
authentication, authorization, rate limiting, load balancing, redirects and
rewrites, response masking and resilience, all configured from one YAML file
that can be reloaded without dropping connections.

## Quick start

```sh
# 1. Sample upstreams: REST :9001, GraphQL :9002, gRPC :9003
go run ./cmd/demo-upstream

# 2. Gateway on :8080 (admin on 127.0.0.1:9901)
export PRISMA_JWT_SECRET='dev-secret-dev-secret-dev-secret!' PRISMA_DEMO_API_KEY=demo-key
go run ./cmd/prisma -config configs/prisma.yaml

# 3. Try it
curl -i http://127.0.0.1:8080/api/v1/orders/42                          # 401
curl -s -H 'X-API-Key: demo-key' http://127.0.0.1:8080/api/v1/orders/42  # masked JSON
curl -s -H 'X-API-Key: demo-key' -H 'Content-Type: application/json' \
     -d '{"query":"{ me { name email ssn } }"}' http://127.0.0.1:8080/graphql
grpcurl -plaintext -H 'x-api-key: demo-key' 127.0.0.1:8080 grpc.health.v1.Health/Check
```

Validate a config without starting the gateway: `go run ./cmd/prisma -config path.yaml -validate`.

## Features

| Area | Supported |
|------|-----------|
| Protocols | HTTP/1.1, HTTP/2 (TLS + ALPN), h2c, WebSocket upgrades, GraphQL (GET/POST/batch), gRPC unary and streaming with trailers passed through, gRPC-Web translated to native gRPC (binary and text) |
| Routing | Host (exact/wildcard), path (exact/regex/prefix), method, headers, gRPC service/method, per-listener routes, weighted backend splits |
| Authentication | JWT (HS*, or RS/ES/EdDSA via JWKS with caching and key rotation), OAuth2 token introspection (RFC 7662, cached, with concurrent lookups merged), API keys (hashed in memory), mTLS; `any`/`all` modes |
| Authorization | Required scopes per route, per HTTP method and per gRPC method; consumer allow-lists; IP allow/deny CIDRs |
| Rate limiting | GCRA, local or Redis (one atomic Lua script); keys from IP, consumer, route, header, JWT claim, gRPC method or GraphQL operation; GraphQL cost-based limits; IETF `RateLimit-*` headers; falls back to local limits if Redis fails |
| GraphQL guard | Limits on depth, aliases, root fields, cost and batch size; introspection blocking; persisted-query aware |
| Masking | JSONPath-style selectors (`$..ssn`, `$.a[*].b`) and detectors (email, SSN, Luhn-checked cards); redact, partial, HMAC hash or drop; `unlessScope` bypass; fails closed by default |
| Redirects / rewrites | 301/302/303/307/308, HTTPS upgrade, prefix replacement, prefix stripping, regex path rewrite, host rewrite |
| Browser support | CORS preflights answered at the gateway before auth, exact or wildcard-subdomain origins, CORS headers on gateway errors too |
| Compression | Streaming zstd and gzip, negotiated from `Accept-Encoding` q-values; applied after masking |
| Load balancing | round_robin, weighted_round_robin (smooth), least_conn, p2c_ewma, ring_hash (IP, header or cookie) |
| Service discovery | Static endpoints, DNS A/AAAA (e.g. Kubernetes headless services), DNS SRV; refreshed periodically; endpoints that stay keep their health and latency state |
| Resilience | Active health checks (HTTP, gRPC, TCP), outlier ejection with backoff and a max-ejection cap, circuit breaker with half-open probing and concurrency cap, retries with backoff and a retry budget, timeouts |
| Operations | Hot reload (file watch, SIGHUP, `POST /config/reload`); invalid configs are rejected while the current one keeps serving. Prometheus metrics, JSON access logs with trace IDs, request IDs (UUIDv7), graceful drain |
| Tracing | OpenTelemetry: a server span per request and a client span per upstream attempt (so retries are visible), W3C trace context propagation, OTLP/gRPC export, sampling that follows the caller's decision |
| Errors | One error model rendered as RFC 9457 problem+json for HTTP, the GraphQL error envelope for GraphQL, and Trailers-Only `grpc-status` for gRPC |

## Admin API (`admin.address`, optionally `admin.token`)

| Endpoint | Purpose |
|----------|---------|
| `GET /healthz`, `GET /readyz` | Liveness and readiness probes |
| `GET /metrics` | Prometheus metrics |
| `GET /config` | Active config as YAML, with secrets redacted |
| `POST /config/validate` | Dry-run validation of a YAML body |
| `POST /config/reload` | Reload from disk |
| `GET /clusters` | Endpoint health, ejections, in-flight requests, EWMA latency, breaker state |
| `GET /routes?host=&path=&method=&header=K:V` | Route debugger: which route matches and its effective policy |
| `/debug/pprof/*` | Profiling (`admin.pprof: true`) |

## Layout

```
cmd/prisma            gateway entrypoint: reload, signals, graceful shutdown
cmd/demo-upstream     sample REST, GraphQL and gRPC backends
internal/config       schema, ${env:}/${file:} secrets, defaults, validation
internal/router       compiled host and path matching
internal/pipeline     filter chain and per-request context
internal/filters/*    authn, authz, ratelimit, graphqlguard, redirect, headers, masking
internal/upstream     clusters, balancers, health checks, outlier detection, breaker, retries
internal/gateway      snapshot build and swap, request handling, proxying, listeners
internal/admin        admin API
internal/gwerr        protocol-aware error model
```

## Development

```sh
go test ./...                       # unit and end-to-end tests (real HTTP, gRPC and miniredis)
docker run --rm -v "$PWD:/src" -w /src golang:1.27 go test -race ./...
docker build -t prisma .
```

## Branching

`main` holds released code, and every release is tagged (`vX.Y.Z`). Work
happens on `feature/*` branches cut from `develop`, which merge back with
`--no-ff`. Releases go through `release/*` branches into `main` and back
into `develop`. See [CHANGELOG.md](CHANGELOG.md).

## Not yet implemented

Protobuf body masking, GraphQL field-level authorization, ext_authz
callouts, HMAC request signing, response caching, traffic mirroring,
Kubernetes EndpointSlice and Consul discovery, PROXY protocol, WASM plugins,
and applying listener and tracing changes without a restart.

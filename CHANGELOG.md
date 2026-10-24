# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/).

## [0.2.0] - 2026-10-23

### Added
- **gRPC-Web**: `grpcWeb: true` routes translate binary and text gRPC-Web
  into native gRPC; upstream trailers are re-encoded as gRPC-Web trailer frames.
- **OpenTelemetry tracing**: a server span per request and a client span per
  upstream attempt, W3C trace context and baggage propagation, OTLP/gRPC
  export, sampling that follows the caller's decision, and trace IDs in access
  logs.
- **CORS**: preflights are answered at the gateway before authentication, and
  gateway-generated errors carry CORS headers.
- **Compression**: streaming zstd and gzip based on `Accept-Encoding`, applied
  after masking.
- **Service discovery**: DNS A/AAAA and SRV discovery with periodic refresh.
  Endpoints that remain after a refresh keep their health and latency state.
- **OAuth2 token introspection** (RFC 7662) with positive and negative
  caching. Concurrent lookups of the same token are merged into one request,
  and an identity-provider outage returns 503 instead of 401.
- `-ldflags "-X main.version=..."` build version, reported in logs and traces.

### Fixed
- Retries on single-endpoint clusters failed with "no healthy upstream"
  instead of retrying.
- `Vary` headers from upstreams were overwritten by gateway-added headers.

## [0.1.0] - 2026-10-23

### Added
- Phase 1 gateway: HTTP/1.1, HTTP/2, h2c, WebSocket, GraphQL and gRPC
  proxying; JWT, API key and mTLS authentication; authorization; local and
  Redis GCRA rate limiting; GraphQL guard; response masking; redirects and
  rewrites; five load-balancing policies; health checks, outlier ejection,
  circuit breaking and retries; hot reload; Prometheus metrics; admin API.

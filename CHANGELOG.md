# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/).

## [0.5.0] - 2026-10-24

### Added
- **TCP and TLS proxying**: listeners with `type: tcp` (optionally
  terminating TLS) or `type: tls-passthrough` (routed by SNI without
  decrypting) proxy connections to `type: tcp` routes. Clusters keep their
  balancing, health checks, outlier ejection, breakers and discovery; routes
  support weighted backends, IP allow/deny and idle timeouts. Half-close is
  propagated, PROXY protocol works on these listeners, and clusters with
  `tls` re-encrypt to the upstream. New metrics: `prisma_tcp_connections_total`,
  `prisma_tcp_active_connections`, `prisma_tcp_bytes_total`.
- **Gateway API TLSRoute and TCPRoute** (v1alpha2, experimental channel):
  TLS listeners in Passthrough or Terminate mode and TCP listeners.
- **OpenAPI request validation** (`openapi` on routes and policies): requests
  are checked against an OpenAPI 3 document and rejected with a 400 listing
  each problem (`in`, `field`, `message`), or 404/405 for undefined
  operations. `mode: report` only logs and counts; `responses: report`
  validates upstream JSON responses without blocking them. Gateway errors can
  now carry an `errors` member in their problem document.
- **gRPC-JSON transcoding** (`grpcTranscode` on routes): REST/JSON requests
  are mapped to gRPC calls through the `google.api.http` annotations in a
  descriptor set, including nested path variables, `**` and custom verbs,
  query parameters, `body` and `response_body`, and additional bindings.
  Server-streaming methods return JSON arrays as messages arrive; gRPC
  errors become HTTP statuses with a problem document. `autoMapping` calls
  unannotated methods as `POST /package.Service/Method`. Other filters,
  masking included, see the JSON side.
- **Consul service discovery** (`discovery: {type: consul}`): healthy
  instances of a service, followed with blocking queries so health changes
  apply within moments. Supports tags, datacenters, namespaces and
  partitions, ACL tokens (redacted on the admin API), `allowWarning`, and
  instance weights. Consul outages keep the last known endpoints.
- **Proxy-Wasm shared queues**: register, resolve, enqueue and dequeue,
  with `proxy_on_queue_ready` delivered to a root context of the plugin.
- **Tracing changes apply on reload**: the `tracing` block can be added,
  changed or removed without a restart; spans of in-flight requests end on
  the previous exporter, which is flushed afterwards.
- The demo upstream serves an annotated `shop.v1.Orders` gRPC service on
  `:9004` and can write its descriptor set (`-write-descriptors`).
- `examples/`: runnable walkthroughs with expected output, from a
  minimal config to the Gateway API on kind. `go test ./examples/` checks
  that every example config loads and compiles.
- The demo upstream takes `-name` and per-backend address flags (to run
  several instances), and has `/echo`, `/slow`, `/flaky`, `/products`
  (cacheable) and `/admin/health` endpoints for the examples.

### Fixed
- The container image ran as the named user `nonroot`, which Kubernetes
  `runAsNonRoot` cannot verify, so pods failed with CreateContainerConfigError.
  It now runs as UID 65532.
- The Gateway API Deployment relied on `NET_BIND_SERVICE` to bind port 80,
  which a non-root process does not get; it now sets
  `net.ipv4.ip_unprivileged_port_start=0` instead.
- `deploy/kubernetes/gateway-api.yaml` could not be applied in one step: its
  example PrismaPolicy was created before the CRD was established. The
  examples moved to `examples/14-kubernetes`.
- `rewrite: {stripPrefix: /, replacePrefix: /x/}` dropped the path separator
  (`/hello` became `/xhello`).

## [0.4.0] - 2026-10-24

### Added
- **Proxy-Wasm plugins** (ABI 0.2.x) on wazero. Plugins built with the
  official SDKs run unmodified. Supported: headers with pseudo-headers,
  buffered bodies, local responses, HTTP callouts through gateway clusters,
  properties, shared data with CAS, Prometheus metrics, timers and logging.
  Each plugin runs a pool of VM instances, and callbacks have a time limit;
  a crashed or looping instance is replaced and fails only its own request.
- **Gateway API controller**: Gateways, HTTPRoutes, GRPCRoutes and
  ReferenceGrants are translated into listeners, routes and Kubernetes-backed
  clusters, applied without restarts, and get status conditions written back.
- **PrismaPolicy CRD** (`prisma.dev/v1alpha1`) attaches Prisma features to
  Gateways or Routes; route policies override gateway policies, and each
  policy is validated on its own.
- **Kubernetes EndpointSlice discovery**: endpoints follow pod readiness and
  termination through list+watch, without client-go.
- Exact `queryParams` route matching, inline PEM listener certificates, and
  `directResponse` routes.
- Kubernetes manifests: `deploy/kubernetes/prisma.yaml` (ConfigMap mode) and
  `deploy/kubernetes/gateway-api.yaml` (controller mode, CRD, RBAC).

### Fixed
- Rewriting a prefix to `/` produced a double slash (`/foo/bar` became
  `//bar`).

## [0.3.0] - 2026-10-23

### Added
- **External authorization**: Envoy-compatible HTTP ext_authz. The
  authorization service receives the request metadata and the authenticated
  principal; it can inject allow-listed headers upstream or deny with its own
  response. It fails closed (503) unless `failOpen` is set.
- **GraphQL schema validation and field-level authorization**: rules on
  `Type.field` or `Type.*` that reject the operation or strip the field and
  rewrite the query, resolved through aliases, fragments and interfaces.
- **gRPC response masking**: the masking selectors and detectors now apply
  inside Protobuf messages, frame by frame, using a `FileDescriptorSet`. It
  works with streaming and gRPC-Web.
- **Traffic mirroring**: sampled, fire-and-forget copies to a shadow cluster,
  taken after authentication, with a concurrency cap.
- **Response cache**: a shared in-memory LRU that honors Cache-Control, Age
  and Vary, with separate entries per consumer by default. Purged on reload
  when a route's config changes; admin `GET /cache` and `POST /cache/purge`.
- **HMAC request signing**: a new authentication provider with a canonical
  string-to-sign, a timestamp window, replay protection and a reference
  `authn.Sign` function.
- **PROXY protocol v1/v2** on listeners, accepted only from trusted CIDRs and
  parsed without blocking the accept loop.
- **Listener hot reload**: listeners can be added, removed, moved or changed
  without a restart. Changed listeners are swapped on the same socket while
  in-flight requests drain, and a bind failure aborts the reload with nothing
  changed.
- Access logs report requests the gateway answered itself (cache hits,
  redirects, CORS preflights) as `served_by`, plus the cache status.

### Fixed
- gRPC-Web translation ran before the other response filters, so they saw
  gRPC-Web bytes instead of native gRPC.

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

# Examples

Each directory is a self-contained walkthrough: a `prisma.yaml` that runs as
is, and a README with commands to try and the output to expect. Run everything
from the repository root.

Most examples use the demo upstream, which serves a REST API on `:9001`,
GraphQL on `:9002` and gRPC on `:9003`:

```sh
go run ./cmd/demo-upstream          # terminal 1
go run ./cmd/prisma -config examples/01-quickstart/prisma.yaml   # terminal 2
```

The gateway listens on `:8080` and its admin API on `127.0.0.1:9901`.

| Example | Shows |
|---|---|
| [01-quickstart](01-quickstart) | Minimal config, admin API, hot reload |
| [02-authentication](02-authentication) | JWT, API keys, HMAC signing, mTLS, scopes |
| [03-rate-limiting](03-rate-limiting) | Per-IP, per-consumer and per-header limits, shared via Redis |
| [04-load-balancing](04-load-balancing) | Balancing policies, health checks, sticky sessions, canary splits |
| [05-resilience](05-resilience) | Timeouts, retries, outlier ejection, circuit breaking |
| [06-redirects-rewrites](06-redirects-rewrites) | Redirects, path and host rewrites, fixed responses |
| [07-masking](07-masking) | Hiding personal data in responses |
| [08-graphql](08-graphql) | Query limits, schema validation, field authorization, cost limits |
| [09-grpc](09-grpc) | gRPC routing, per-method authorization, gRPC-Web |
| [10-browser-apps](10-browser-apps) | CORS, compression, response caching |
| [11-external-authz](11-external-authz) | Delegating decisions to your own authorization service |
| [12-wasm-plugin](12-wasm-plugin) | A Proxy-Wasm plugin written with the Go SDK |
| [13-observability](13-observability) | Tracing with Jaeger, metrics with Prometheus, in Docker Compose |
| [14-kubernetes](14-kubernetes) | Gateway API controller and PrismaPolicy on a kind cluster |
| [15-tcp-tls](15-tcp-tls) | TCP proxying, TLS passthrough by SNI, TLS termination |
| [16-openapi](16-openapi) | Validating requests (and responses) against an OpenAPI document |

[tools/](tools) has helpers used by the examples: `token` mints test JWTs and
`hmac-sign` sends HMAC-signed requests.

`go test ./examples/` checks that every example config loads and compiles.

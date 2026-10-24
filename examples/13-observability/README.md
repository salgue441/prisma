# 13 · Observability

A complete local stack in Docker: the gateway, two demo upstreams, Jaeger for
traces and Prometheus for metrics.

## Run

From the repository root:

```sh
docker compose -f examples/13-observability/docker-compose.yaml up --build
```

| URL | What |
|---|---|
| http://localhost:8080 | the gateway |
| http://localhost:16686 | Jaeger (traces) |
| http://localhost:9090 | Prometheus (metrics) |
| http://localhost:9901 | admin API |

Send some traffic, including some that fails and gets retried:

```sh
for i in $(seq 50); do
  curl -s -o /dev/null -H 'X-API-Key: demo-key' localhost:8080/api/orders/$i
  curl -s -o /dev/null -H 'X-API-Key: demo-key' "localhost:8080/api/flaky?rate=0.3"
done
curl -s -o /dev/null localhost:8080/api/orders/1          # 401
```

## Traces

Open Jaeger, pick the `prisma-gateway` service and find a `/api/flaky`
request. Each request has a server span, plus one client span per upstream
attempt, so retries show up as separate children with their own status and
the endpoint they went to.

Prisma joins traces started by callers (W3C `traceparent`) and forwards trace
context to upstreams, so the gateway appears in the middle of end-to-end
traces. It follows the caller's sampling decision.

```sh
curl -s -H 'X-API-Key: demo-key' \
  -H 'traceparent: 00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01' \
  localhost:8080/api/orders/1 > /dev/null
# Jaeger: search for trace 4bf92f3577b34da6a3ce929d0e0e4736
```

## Metrics

Useful queries in Prometheus:

```promql
# Requests per second by route and status
sum by (route, code) (rate(prisma_requests_total[1m]))

# p99 latency by route
histogram_quantile(0.99, sum by (route, le) (rate(prisma_request_duration_seconds_bucket[5m])))

# Upstream errors and retries
sum by (cluster, outcome) (rate(prisma_upstream_requests_total[1m]))
sum by (cluster) (rate(prisma_upstream_retries_total[1m]))

# Authentication failures, rate limiting, endpoint health
sum by (route, reason) (rate(prisma_auth_failures_total[5m]))
sum by (route, result) (rate(prisma_ratelimit_decisions_total[1m]))
min by (cluster, endpoint) (prisma_endpoint_healthy)
```

## Access logs

The gateway writes one JSON line per request, including the trace ID, so a
log line leads straight to its trace:

```sh
docker compose -f examples/13-observability/docker-compose.yaml logs --no-log-prefix prisma | grep '"msg":"access"' | tail -1 | jq .
```

## Health probes

`/healthz` (liveness) and `/readyz` (readiness) on the admin port. Readiness
turns false while the gateway drains on shutdown, so load balancers stop
sending traffic before connections close.

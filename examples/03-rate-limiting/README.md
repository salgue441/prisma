# 03 · Rate limiting

Prisma limits with GCRA (a smooth token bucket): `limit` requests per
`period`, with `burst` extra capacity for short spikes. Limits are keyed by any
combination of `client_ip`, `consumer`, `route`, `header:<name>`,
`claim:<jwt claim>`, `grpc_method` and `graphql_operation`.

| Route | Limit |
|---|---|
| `/public` | 5/s per client IP, burst 10 |
| `/api/orders` | 3/s **and** 30/min per API consumer |
| `/tenant` | 2/s per `X-Tenant` header value, shared via Redis |

## Run

```sh
export PRISMA_DEMO_API_KEY=demo-key
docker compose -f examples/03-rate-limiting/docker-compose.yaml up -d
go run ./cmd/demo-upstream
go run ./cmd/prisma -config examples/03-rate-limiting/prisma.yaml
```

## Per IP

```sh
for i in $(seq 15); do curl -s -o /dev/null -w '%{http_code} ' localhost:8080/public; done; echo
# 200 200 200 200 200 200 200 200 200 200 200 200 429 429 429
```

The burst of 10 goes through at once, a few more are allowed as the bucket
refills at 5/s while the loop runs, and the rest are rejected.

Every response carries the IETF rate limit headers, and a 429 says when to
retry:

```sh
curl -si localhost:8080/public | grep -i ratelimit
# Ratelimit-Limit: 10
# Ratelimit-Remaining: 9
# Ratelimit-Reset: 1
```

## Per consumer

Each consumer has its own budget, so one noisy client can't starve another:

```sh
for i in $(seq 5); do curl -s -o /dev/null -w '%{http_code} ' -H 'X-API-Key: demo-key' localhost:8080/api/orders/1; done; echo
# 200 200 200 429 429
curl -s -o /dev/null -w '%{http_code}\n' -H 'X-API-Key: paid-key' localhost:8080/api/orders/1
# 200
```

## Shared across replicas (Redis)

```sh
for i in $(seq 4); do curl -s -o /dev/null -w '%{http_code} ' -H 'X-Tenant: acme' localhost:8080/tenant; done; echo
# 200 200 429 429
```

Start a second gateway with the same config on another port and the two share
the `acme` budget. Stop Redis (`docker compose ... stop`) and requests keep
flowing under a local limit; `prisma_ratelimit_decisions_total{result="backend_error"}`
on `/metrics` counts the fallbacks.

## GraphQL cost limits

For GraphQL routes, `cost: graphql` charges each request its computed query
cost instead of 1. See [08-graphql](../08-graphql).

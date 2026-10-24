# 05 · Resilience

The demo upstream has two misbehaving endpoints: `/flaky?rate=0.5` fails with
503 at the given rate, and `/slow?ms=N` answers after a delay. This example
puts the gateway's failure handling in front of them.

## Run

```sh
go run ./cmd/demo-upstream
go run ./cmd/demo-upstream -name backup -rest :9011 -graphql "" -grpc ""
go run ./cmd/prisma -config examples/05-resilience/prisma.yaml
```

```sh
codes() { for i in $(seq ${2:-20}); do curl -s -o /dev/null -w '%{http_code}\n' "localhost:8080$1"; done | sort | uniq -c; }
```

## Retries

Without retries, about half the requests fail. With up to 3 attempts (and
backoff between them), almost none do:

```sh
codes '/not-retried?rate=0.5' 40     # ~20 x 200, ~20 x 503
codes '/retried?rate=0.5' 40         # ~35 x 200, ~5 x 503
```

Retries only happen for idempotent methods unless `allowNonIdempotent` is set,
and the `budget` caps retries at a share of normal traffic so they can't turn
an outage into a retry storm. `prisma_upstream_retries_total` counts them.

## Timeouts

```sh
curl -s -o /dev/null -w '%{http_code}\n' 'localhost:8080/slow?ms=200'    # 200
curl -s -w '\n%{http_code}\n'            'localhost:8080/slow?ms=3000'   # 504 after 1s
```

`global.requestTimeout` is the default; routes override it with `timeout`.

## Outlier ejection

Stop the backup instance (Ctrl-C in its terminal). Requests sent to it fail to
connect, and after three consecutive failures it is ejected, so the remaining
traffic goes to the primary:

```sh
for i in $(seq 12); do curl -s -o /dev/null -w '%{http_code} ' 'localhost:8080/not-retried?rate=0'; done
# 200 502 200 502 200 502 200 200 200 200 200 200
curl -s localhost:9901/clusters | jq '.[] | select(.name=="flaky").endpoints'
# [{"address": "localhost:9001", "healthy": true, "ejected": false, ...},
#  {"address": "localhost:9011", "healthy": true, "ejected": true, ...}]
```

Passive ejection reacts to real traffic, so it catches failures that active
health checks miss. Ejections double in length each time an endpoint is
ejected again, and `maxEjectionPercent` keeps part of the cluster in service
whatever happens.

## Circuit breaker

Drive the error rate above 50% and the breaker opens: requests fail fast with
503 instead of waiting on a struggling upstream. After `openDuration` a few
probe requests are let through, and the circuit closes once they succeed.

```sh
codes '/breaker?rate=1' 15        # 10 x 503 from upstream, then 5 fast 503s from the gateway
curl -s localhost:9901/clusters | jq '.[] | select(.name=="fragile").circuitOpen'   # true
sleep 6; codes '/breaker?rate=0' 5   # half-open probes succeed, the circuit closes
```

# 04 · Load balancing

Two demo instances, `blue` (`:9001`) and `green` (`:9011`), behind different
balancing policies. Every response says which instance served it, in the
`X-Instance` header and the `instance` field.

| Route | Cluster policy |
|---|---|
| `/weighted/1` | `weighted_round_robin`, 3:1 in favor of blue, with health checks |
| `/fastest/1` | `p2c_ewma`: power of two choices, weighted by latency and load |
| `/sticky/1` | `ring_hash` on the `X-User` header |
| `/shop/1` | 90/10 canary split between two clusters; `X-Canary: always` forces green |

The other policies are `round_robin` (the default) and `least_conn`.

## Run

```sh
go run ./cmd/demo-upstream -name blue
go run ./cmd/demo-upstream -name green -rest :9011 -graphql "" -grpc ""
go run ./cmd/prisma -config examples/04-load-balancing/prisma.yaml
```

A small helper to count which instance answered:

```sh
hits() { for i in $(seq ${2:-20}); do curl -s -D - -o /dev/null "${@:3}" "localhost:8080$1" | grep -i x-instance; done | sort | uniq -c; }
```

## Weighted

```sh
hits /weighted/1 20
#  15 X-Instance: blue
#   5 X-Instance: green
```

## Health checks take instances out of rotation

Make green fail its health check, and within two checks (about 4 seconds)
all traffic goes to blue:

```sh
curl -XPOST 'localhost:9011/admin/health?ok=false'
sleep 5; hits /weighted/1 10
#  10 X-Instance: blue
curl -s localhost:9901/clusters | jq '.[] | select(.name=="weighted")'
curl -XPOST 'localhost:9011/admin/health?ok=true'      # and it comes back
```

## Sticky sessions

```sh
hits /sticky/1 10 -H 'X-User: alice'     # always the same instance
hits /sticky/1 10 -H 'X-User: bob'       # possibly the other one, but always the same
```

`hashOn` can also be `ip` or `cookie:<name>`. When an instance leaves, only the
users that were on it move.

## Canary releases

```sh
hits /shop/1 100
#  ~90 X-Instance: blue
#  ~10 X-Instance: green
hits /shop/1 5 -H 'X-Canary: always'
#   5 X-Instance: green
```

Change the weights in `prisma.yaml` and save: the new split applies
immediately, without dropping connections.

## Service discovery

Instead of listing endpoints, clusters can discover them. Use one of:

```yaml
# DNS A/AAAA records (e.g. a headless Service) with a fixed port
discovery: {type: dns, host: orders.default.svc.cluster.local, port: 8080, interval: 10s}
# DNS SRV records, which carry ports and weights
discovery: {type: srv, host: _http._tcp.orders.service.consul, interval: 10s}
# Kubernetes EndpointSlices, watched live
discovery: {type: kubernetes, kubernetes: {service: orders, namespace: shop, portName: http}}
```

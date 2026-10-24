# 01 · Quickstart

A gateway with one listener, one upstream cluster and two routes. Start here.

## Run

```sh
go run ./cmd/demo-upstream                                    # terminal 1
go run ./cmd/prisma -config examples/01-quickstart/prisma.yaml   # terminal 2
```

## Try it

```sh
curl -s http://localhost:8080/api/orders/42 | jq .
```

The gateway strips `/api` and forwards `/orders/42` to the demo upstream on
`:9001`. The upstream echoes back the `X-Request-Id` the gateway generated:

```json
{ "id": "42", "instance": "demo-1", "requestId": "01a16eb9-3ae8-72c6-…", ... }
```

A path that no route matches gets an RFC 9457 problem document:

```sh
curl -s http://localhost:8080/nope
# {"code":"NOT_FOUND","detail":"no route matches the request","status":404,"title":"Not Found","type":"about:blank"}
```

## Look around the admin API

```sh
curl -s localhost:9901/routes?path=/api/orders/1 | jq .   # which route matches, and its policy
curl -s localhost:9901/clusters | jq .                    # endpoint health and latency
curl -s localhost:9901/metrics | grep prisma_requests     # Prometheus metrics
curl -s localhost:9901/config                             # the active config
```

## Hot reload

Edit `prisma.yaml` (for example, change the prefix to `/v2/orders`) and save.
The gateway reloads it without dropping connections, and keeps the old config
if the new one is invalid. Reloads can also be triggered with `kill -HUP` or
`curl -XPOST localhost:9901/config/reload`.

Validate a config without starting the gateway:

```sh
go run ./cmd/prisma -config examples/01-quickstart/prisma.yaml -validate
```

# 09 · gRPC

gRPC runs over HTTP/2 with trailers. Prisma proxies it natively, streaming
included, and applies the same authentication, authorization and rate limits
as for HTTP. Errors come back as gRPC status codes, not HTTP error pages.

You need [grpcurl](https://github.com/fullstorydev/grpcurl) for the commands
below.

## Run

```sh
go run ./cmd/demo-upstream
go run ./cmd/prisma -config examples/09-grpc/prisma.yaml
```

## Calls

```sh
grpcurl -plaintext localhost:8080 list                     # via server reflection
grpcurl -plaintext -H 'x-api-key: dashboard-key' \
  -d '{"service":"inventory.v1.Inventory"}' localhost:8080 grpc.health.v1.Health/Check
# { "status": "SERVING" }
```

## Errors are gRPC statuses

```sh
grpcurl -plaintext localhost:8080 grpc.health.v1.Health/Check
# ERROR: Code: Unauthenticated

# Watch needs the health:watch scope, which dashboard-key lacks
grpcurl -plaintext -H 'x-api-key: dashboard-key' localhost:8080 grpc.health.v1.Health/Watch
# ERROR: Code: PermissionDenied

grpcurl -plaintext -H 'x-api-key: ops-key' localhost:8080 grpc.health.v1.Health/Watch
# { "status": "SERVING" }   (a server stream; Ctrl-C to stop)
```

Rate limits come back as `ResourceExhausted`, and a missing upstream as
`Unavailable`.

## Routing

`grpcService` matches every method of a service, `grpcMethod` a single one:

```yaml
match: {grpcService: shop.v1.Orders, grpcMethod: CreateOrder}
```

`authz.methods` adds scopes for specific methods, and the `grpc_method` rate
limit key gives each method its own budget.

## gRPC-Web

With `grpcWeb: true`, browsers using grpc-web clients (Connect, improbable-eng,
the official grpc-web) talk to the gateway, and the upstream sees native gRPC.
No Envoy sidecar needed. A raw gRPC-Web call looks like this:

```sh
# A Check request for service "" is the empty message: a 5-byte frame header.
printf '\x00\x00\x00\x00\x00' | curl -s -X POST --data-binary @- \
  -H 'Content-Type: application/grpc-web+proto' -H 'X-API-Key: dashboard-key' \
  localhost:8080/grpc.health.v1.Health/Check | xxd
# 00000000: 0000 0000 0208 0180 0000 0020 6772 7063  ...........  grpc
# 00000010: 2d6d 6573 7361 6765 3a20 0d0a 6772 7063  -message: ..grpc
# 00000020: 2d73 7461 7475 733a 2030 0d0a            -status: 0..
#
# A message frame (flag 0x00, length 2, "08 01" = status SERVING), then a
# trailer frame (flag 0x80) carrying grpc-status.
```

## Masking gRPC responses

Masking rules work on Protobuf messages too. Give the policy a descriptor set
for your services and the same selectors apply to message fields:

```sh
protoc --include_imports --descriptor_set_out=api.pb -I proto proto/shop/v1/*.proto
```

```yaml
masking:
  descriptorSet: api.pb
  rules: [{selector: "$..card_number", strategy: partial, keepLast: 4}]
```

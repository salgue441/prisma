# 17 · gRPC-JSON transcoding

Serve a gRPC service as a REST API without changing it. The gateway reads
the `google.api.http` annotations from the service's descriptor set (the same
annotations Envoy, grpc-gateway and Google Cloud Endpoints use), turns JSON
requests into gRPC calls and converts the answers back to JSON.

[orders.proto](orders.proto) is the service: each method declares its
HTTP mapping.

```proto
rpc GetOrder(GetOrderRequest) returns (Order) {
  option (google.api.http) = {get: "/v1/orders/{id}"};
}
```

## Run

From the repository root:

```sh
go run ./cmd/demo-upstream -write-descriptors examples/17-grpc-transcoding/shop.binpb
go run ./cmd/demo-upstream                 # serves shop.v1.Orders over gRPC on :9004
go run ./cmd/prisma -config examples/17-grpc-transcoding/prisma.yaml
```

For your own services, build the descriptor set with `protoc
--include_imports --descriptor_set_out=api.binpb` (or `buf build -o
api.binpb`).

## Requests

Path variables, query parameters and the body fill the request message:

```sh
curl -s localhost:8080/v1/orders/42 | jq -c .
# {"id":"42","item":"book","qty":1,"status":"SHIPPED","customer":{"name":"Ana","email":"***@example.com"}}

curl -s 'localhost:8080/v1/orders/42?includeItems=true' | jq .qty      # 2
curl -s -d '{"item":"pen","qty":3}' localhost:8080/v1/shops/main/orders | jq -c .
# {"id":"new-main","item":"pen","qty":3}
curl -s -X PATCH -d '{"item":"lamp","status":"PENDING"}' localhost:8080/v1/orders/7 | jq -c .
# {"id":"7","item":"lamp","status":"PENDING"}
curl -s -X POST localhost:8080/v1/orders/9:cancel | jq -c .            # response_body: "customer"
# {"name":"Ana","email":"***@example.com"}
```

Query parameters can address nested fields (`a.b=1`), repeat for repeated
fields (`status=PENDING&status=SHIPPED`), and use either the proto name
(`include_items`) or the JSON name (`includeItems`).

## Streaming

Server-streaming methods return a JSON array, sent as messages arrive:

```sh
curl -s 'localhost:8080/v1/shops/main/orders?pageSize=3&status=PENDING' | jq -c '.[] | {id, status}'
# {"id":"main-0","status":"PENDING"}
# {"id":"main-1","status":"PENDING"}
# {"id":"main-2","status":"PENDING"}
```

If the stream fails midway, the array ends with an `{"error": {...}}`
element, because the HTTP status was already sent:

```sh
curl -s localhost:8080/v1/shops/broken/orders | jq -c '.[-1]'
# {"error":{"code":"UNAVAILABLE","grpcCode":14,"message":"backend down"}}
```

## Errors

gRPC status codes become HTTP statuses (NOT_FOUND is 404, INVALID_ARGUMENT
400, UNAVAILABLE 503, ...), with a problem document:

```sh
curl -s localhost:8080/v1/orders/404 | jq -c .
# {"code":"NOT_FOUND","detail":"order 404 not found","grpcCode":5,"status":404,"title":"Not Found","type":"about:blank"}
```

Requests that can't be mapped are rejected before reaching the service:

```sh
curl -s -d '{"nope":1}' localhost:8080/v1/shops/main/orders | jq -c .errors
# [{"in":"body","message":"proto: (line 1:2): unknown field \"nope\""}]
curl -s 'localhost:8080/v1/orders/1?includeItems=maybe' | jq -c .errors
# [{"in":"query","field":"includeItems","message":"invalid value for include_items: strconv.ParseBool: parsing \"maybe\": invalid syntax"}]
curl -s localhost:8080/v1/customers | jq -c '{status, code}'
# {"status":404,"code":"NOT_FOUND"}
```

## Calling methods by name

With `autoMapping: true`, any unary method can be called as
`POST /package.Service/Method` with the request message as JSON, even
without an annotation:

```sh
curl -s -d '{"id":"p1"}' localhost:8080/shop.v1.Orders/Ping | jq -c .
# {"id":"p1"}
```

## Notes

- Transcoding runs last on the way in and first on the way out, so
  authentication, rate limits, OpenAPI validation, masking (above, on
  `email`), caching and compression all work on the JSON side.
- `emitUnpopulated: true` includes zero values in responses;
  `useProtoNames: true` uses `snake_case` names; `discardUnknown: true`
  ignores unknown fields and query parameters instead of rejecting them.
- Client-streaming and bidirectional methods can't be transcoded; native
  gRPC clients can still use them through a `type: grpc` route.

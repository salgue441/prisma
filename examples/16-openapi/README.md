# 16 · OpenAPI validation

Give a route the OpenAPI 3 document of its API and the gateway checks every
request against it: the path and method must exist, and path, query, header
and cookie parameters and the JSON body must match their schemas. Bad requests
are rejected with a list of what is wrong, and never reach the service.

## Run

From the repository root:

```sh
go run ./cmd/demo-upstream
go run ./cmd/prisma -config examples/16-openapi/prisma.yaml
```

## Valid requests pass

```sh
curl -s localhost:8080/api/orders/42 | jq -c '{id, instance}'
curl -s -H 'Idempotency-Key: 7f3c9a1e' -H 'Content-Type: application/json' \
  -d '{"item":"book","qty":2}' localhost:8080/api/orders | jq -c .
# {"created":{"item":"book","qty":2},"instance":"demo-1"}
```

## Invalid requests are rejected with details

```sh
curl -s -H 'Idempotency-Key: 7f3c9a1e' -H 'Content-Type: application/json' \
  -d '{"qty":0,"admin":true}' localhost:8080/api/orders | jq .
```

```json
{
  "code": "REQUEST_VALIDATION_FAILED",
  "detail": "request does not match the API specification",
  "errors": [
    {"in": "body", "message": "property \"admin\" is unsupported"},
    {"in": "body", "field": "/qty", "message": "number must be at least 1"},
    {"in": "body", "field": "/item", "message": "property \"item\" is missing"}
  ],
  "status": 400,
  "title": "Bad Request",
  "type": "about:blank"
}
```

More examples:

```sh
curl -s localhost:8080/api/orders/abc | jq -c .errors
# [{"in":"path","field":"id","message":"string doesn't match the regular expression \"^[0-9]+$\""}]
curl -s 'localhost:8080/api/orders/1?expand=everything' | jq -c .errors
# [{"in":"query","field":"expand","message":"value is not one of the allowed values [\"items\",\"customer\"]"}]
curl -s 'localhost:8080/api/products?limit=500' | jq -c .errors
# [{"in":"query","field":"limit","message":"number must be at most 50"}]
curl -s -H 'Content-Type: application/json' -d '{"item":"x","qty":1}' localhost:8080/api/orders | jq -c .errors
# [{"in":"header","field":"Idempotency-Key","message":"value is required but missing"}]
curl -s -X DELETE localhost:8080/api/orders/1 | jq -c '{status, code}'
# {"status":405,"code":"METHOD_NOT_ALLOWED"}
curl -s localhost:8080/api/admin | jq -c '{status, code}'
# {"status":404,"code":"NOT_FOUND"}
```

## Rolling it out safely

Start with `mode: report`: nothing is rejected, but mismatches are logged
and counted, so you can see what would break before enforcing.

```sh
curl -s localhost:9901/metrics | grep prisma_openapi
# prisma_openapi_validations_total{phase="request",result="invalid",route="api"} 5
# prisma_openapi_validations_total{phase="request",result="unknown_operation",route="api"} 2
# prisma_openapi_validations_total{phase="request",result="valid",route="api"} 2
# prisma_openapi_validations_total{phase="response",result="valid",route="api"} 2
```

`unknownOperations: allow` forwards requests for paths the document doesn't
describe (useful while the document is incomplete).

## Checking the service too

With `responses: report`, upstream JSON responses are validated against the
document and mismatches are logged. Responses are never changed or blocked,
so this is safe in production and catches services drifting from their
contract.

## Notes

- Validation runs after authentication, authorization and rate limiting,
  so anonymous callers get a 401 and learn nothing about the schema. It
  runs before rewrites: the document describes the paths clients call.
- Matching ignores the scheme and host of `servers`, and uses the path of
  the first server URL as the base path unless `basePath` is set.
- Security schemes in the document are not enforced; configure `authn` on
  the route instead.
- `$ref`s to other local files work with `specFile`; remote references are
  refused. Bodies are buffered up to `maxBodyBytes` (default 1MiB).

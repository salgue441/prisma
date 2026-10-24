# 11 · External authorization

Scopes answer "may this client call this endpoint?". Some rules depend on
data the gateway doesn't have, such as "may this tenant read *this* order?".
`extAuthz` hands that decision to a service you write, in any language,
using the same HTTP contract as Envoy's ext_authz (so OPA, oauth2-proxy and
similar services work too).

For every request, after authentication, Prisma calls
`<url><request path>?<query>` with the original method and these headers:

| Header | Value |
|---|---|
| `X-Prisma-Consumer`, `X-Prisma-Principal`, `X-Prisma-Scopes` | the authenticated identity |
| `X-Forwarded-Method`, `-Uri`, `-Host`, `-Proto`, `-For` | the original request |
| `Authorization`, `Cookie`, `X-Request-Id`, plus `allowedHeaders` | copied from the client |

A 2xx answer allows the request. Any other answer is sent to the client as
the response, with its status and body.

## Run

```sh
go run ./cmd/demo-upstream
go run ./examples/11-external-authz/authz-server      # :9100
go run ./cmd/prisma -config examples/11-external-authz/prisma.yaml
```

The sample policy: orders 1 and 2 belong to `acme`, order 3 to `globex`;
`support` can see everything, and only `orders:admin` may delete.

```sh
curl -s -H 'X-API-Key: acme-key'   localhost:8080/orders/1 | jq .id      # "1"
curl -si -H 'X-API-Key: acme-key'  localhost:8080/orders/3               # 403
# X-Deny-Reason: order belongs to another tenant
# {"error":"order belongs to another tenant"}
curl -s -H 'X-API-Key: globex-key' localhost:8080/orders/3 | jq .id      # "3"
curl -s -H 'X-API-Key: support-key' localhost:8080/orders/3 | jq .id     # "3"
curl -si -X DELETE -H 'X-API-Key: acme-key' localhost:8080/orders/1 | head -1   # 403
```

The allow response sets `X-Tenant`, which `upstreamHeaders` forwards to the
upstream, so services can trust the tenant without re-checking.

## When the authorization service is down

Stop `authz-server` and requests fail closed with 503. Set `failOpen: true`
only when letting requests through is safer than blocking them.
`prisma_ext_authz_decisions_total` counts allows, denies and errors.

## Order of checks

Within a route, Prisma runs: CORS preflight, authentication, scope and IP
checks (`authz`), then `extAuthz`, then plugins, GraphQL checks and rate
limits. Cheap local checks reject most bad requests before the network call.

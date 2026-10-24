# 02 · Authentication and authorization

Three ways for clients to prove who they are, and scope checks on top:

| Route prefix | Credential | Who can call it |
|---|---|---|
| `/jwt/orders` | `Authorization: Bearer <JWT>` | `orders:read` scope; `POST` also needs `orders:write` |
| `/key/orders` | `X-API-Key` header | same scope rules, scopes come from the consumer |
| `/any/orders` | either of the above | `orders:read` |
| `/signed/orders` | HMAC signature headers | only the `partner` consumer, only from localhost |

The gateway forwards the authenticated identity upstream in `X-Consumer` (see
`headers.request.set` with the `${consumer}` template).

## Run

```sh
export PRISMA_JWT_SECRET='dev-secret-dev-secret-dev-secret!'
export PRISMA_DEMO_API_KEY=demo-key PRISMA_HMAC_SECRET=partner-secret-partner-secret-32b
go run ./cmd/demo-upstream
go run ./cmd/prisma -config examples/02-authentication/prisma.yaml
```

## JWT

`examples/tools/token` mints test tokens signed with `$PRISMA_JWT_SECRET`:

```sh
READ=$(go run ./examples/tools/token -sub alice -scope orders:read)
WRITE=$(go run ./examples/tools/token -sub bob -scope "orders:read orders:write")

curl -i localhost:8080/jwt/orders/1                                   # 401, no token
curl -s -H "Authorization: Bearer $READ" localhost:8080/jwt/orders/1 | jq .seenConsumer   # "alice"
curl -i -H "Authorization: Bearer $READ" -d '{}' localhost:8080/jwt/orders           # 403, needs orders:write
curl -i -H "Authorization: Bearer $WRITE" -d '{}' localhost:8080/jwt/orders          # 201

EXPIRED=$(go run ./examples/tools/token -ttl -1h)
curl -i -H "Authorization: Bearer $EXPIRED" localhost:8080/jwt/orders/1              # 401 (the reason is logged, not returned)
```

In production, point the gateway at your identity provider's key set instead
of a shared secret. Keys are cached and refreshed when they rotate:

```yaml
authn:
  jwt:
    jwksUri: https://idp.example.com/.well-known/jwks.json
    issuer: https://idp.example.com/
    audiences: [orders-api]
```

Opaque tokens can be checked against an OAuth2 introspection endpoint with
`authn.introspection` instead.

## API keys

```sh
curl -s -H 'X-API-Key: demo-key'  localhost:8080/key/orders/1 | jq .seenConsumer   # "mobile-app"
curl -i -H 'X-API-Key: demo-key'  -d '{}' localhost:8080/key/orders                # 403, read-only consumer
curl -i -H 'X-API-Key: batch-key' -d '{}' localhost:8080/key/orders                # 201
curl -i -H 'X-API-Key: wrong'     localhost:8080/key/orders/1                      # 401
```

Keys can be written in the config as `sha256:<hex>` digests (see the
`batch-job` consumer), so the file never holds the raw key. Generate one with
`printf %s "$KEY" | sha256sum`.

## Either credential

```sh
curl -s -H "Authorization: Bearer $READ" localhost:8080/any/orders/1 | jq .seenConsumer   # "alice"
curl -s -H 'X-API-Key: demo-key'         localhost:8080/any/orders/1 | jq .seenConsumer   # "mobile-app"
```

Set `authn.mode: all` to require every configured method instead.

## HMAC-signed requests

`examples/tools/hmac-sign` signs requests with the same function the gateway
verifies with (`authn.Sign`). The signature covers the method, path, host,
timestamp and a hash of the body, so nothing can be changed in transit:

```sh
go run ./examples/tools/hmac-sign -key partner-key -secret $PRISMA_HMAC_SECRET \
    -X POST -d '{"item":"book"}' http://localhost:8080/signed/orders          # 201

# The same signature can only be used once:
go run ./examples/tools/hmac-sign -key partner-key -secret $PRISMA_HMAC_SECRET -replay \
    http://localhost:8080/signed/orders/7                                     # 200, then 401

go run ./examples/tools/hmac-sign -key partner-key -secret wrong \
    http://localhost:8080/signed/orders/7                                     # 401
```

The README at the repository root documents the exact string-to-sign for
clients written in other languages.

## Mutual TLS

Client certificates are configured on the listener and checked per route:

```yaml
listeners:
  - name: internal
    address: ":8443"
    tls:
      certificates: [{certFile: server.crt, keyFile: server.key}]
      clientCAFile: clients-ca.crt
      clientAuth: require
routes:
  - name: internal-api
    authn:
      mtls: {allowedSubjects: [billing-service, "spiffe://prod/billing"]}   # CN, URI or DNS SAN
```

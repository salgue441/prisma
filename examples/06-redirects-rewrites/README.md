# 06 · Redirects, rewrites and fixed responses

- **Redirects** are answered by the gateway with a `Location` header; the
  upstream never sees them.
- **Rewrites** change the path or Host header before the request is proxied;
  the client never sees them.
- **Direct responses** answer with a fixed status and body.

## Run

```sh
go run ./cmd/demo-upstream
go run ./cmd/prisma -config examples/06-redirects-rewrites/prisma.yaml
```

## Redirects

```sh
curl -si 'localhost:8080/api/v1/orders/7?expand=items' | grep -iE '^(HTTP|location)'
# HTTP/1.1 308 Permanent Redirect
# Location: http://localhost:8080/api/v2/orders/7?expand=items

curl -si -H 'Host: docs.example.com' localhost:8080/guide | grep -iE '^(HTTP|location)'
# HTTP/1.1 301 Moved Permanently
# Location: https://docs.example.com/guide

curl -si 'localhost:8080/blog/2026/hello?utm=x' | grep -iE '^(HTTP|location)'
# HTTP/1.1 302 Found
# Location: http://blog.example.com/
```

Use 307/308 when the method and body must be preserved (APIs), and 301/302
for browsers.

## Rewrites

The `/echo` endpoint of the demo upstream reports the path and host it
received:

```sh
curl -s localhost:8080/api/v2/orders/7 | jq .id                        # "7", fetched as /orders/7
curl -s localhost:8080/users/42/profile | jq .path                     # "/echo/profiles/42"
curl -s -H 'Host: acme.tenants.local' localhost:8080/settings | jq '{path, host}'
# {"path": "/echo/tenant/settings", "host": "tenants.internal"}
```

Routes are matched in this order: exact `path`, then `pathRegex`, then the
longest `pathPrefix`; `priority` breaks ties. Host matches (exact before
wildcard) narrow the candidates first. Ask the gateway which route a request
would hit:

```sh
curl -s 'localhost:9901/routes?host=acme.tenants.local&path=/settings' | jq .route
```

## Direct responses

```sh
curl -s localhost:8080/robots.txt
curl -si localhost:8080/admin/users | head -1      # HTTP/1.1 503 Service Unavailable
```

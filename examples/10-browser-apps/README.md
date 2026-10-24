# 10 · Browser apps: CORS, compression and caching

## Run

```sh
go run ./cmd/demo-upstream
go run ./cmd/prisma -config examples/10-browser-apps/prisma.yaml
```

## CORS

Browsers send a preflight `OPTIONS` request before cross-origin calls with
custom headers. The gateway answers it itself, before authentication, so
upstreams never need CORS code:

```sh
curl -si -X OPTIONS localhost:8080/orders/1 \
  -H 'Origin: http://localhost:3000' \
  -H 'Access-Control-Request-Method: GET' \
  -H 'Access-Control-Request-Headers: x-api-key' | grep -i '^access-control\|^HTTP'
# HTTP/1.1 204 No Content
# Access-Control-Allow-Origin: http://localhost:3000
# Access-Control-Allow-Credentials: true
# Access-Control-Allow-Headers: content-type, x-api-key
# Access-Control-Allow-Methods: GET, POST
# Access-Control-Max-Age: 600
```

Origins can be exact, `*`, or wildcard subdomains (`https://*.shop.example`).
Other origins get no CORS headers, so the browser blocks the response. Gateway
errors (401, 429, ...) carry CORS headers too, so the app can read them
instead of seeing an opaque network error.

## Compression

Responses larger than `minBytes` are compressed with the best algorithm the
client accepts, while streaming:

```sh
curl -s -o /dev/null -w '%{size_download} bytes\n' localhost:8080/products                              # 8814 bytes
curl -s -o /dev/null -w '%{size_download} bytes\n' -H 'Accept-Encoding: zstd' localhost:8080/products   #  320 bytes
curl -s -o /dev/null -w '%{size_download} bytes\n' -H 'Accept-Encoding: gzip' localhost:8080/products   #  416 bytes
```

Compression runs after masking, and responses that are already compressed or
aren't a compressible type are left alone.

## Caching

The upstream marks `/products` as cacheable for 30 seconds. The first request
is fetched from the upstream; repeats are served from the gateway's memory with
an `X-Cache: HIT` header, and the body is the
same (look at `generatedAt`):

```sh
for i in 1 2 3; do curl -s -D - -o /tmp/body localhost:8080/products | grep -i '^x-cache\|^age'; jq -r .generatedAt /tmp/body; done
# 2026-10-24T10:28:16.91Z      <- first request: fetched from the upstream
# X-Cache: HIT
# Age: 1
# 2026-10-24T10:28:16.91Z      <- same body, served by the gateway
```

The cache honors `Cache-Control` (`no-store`, `private`, `max-age`,
`s-maxage`) and `Vary`. Entries are kept per consumer unless the route sets
`shared: true`, so one user's data is never served to another. `defaultTTL`
applies to responses with no caching headers (the `/orders` route).

```sh
curl -s localhost:9901/cache                                # {"bytes": 9026, "entries": 1}
curl -s -XPOST 'localhost:9901/cache/purge?route=products'  # drop one route's entries
```

Cached entries are purged automatically when a route's configuration changes.

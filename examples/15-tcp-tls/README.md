# 15 · TCP and TLS proxying

Not everything is HTTP. Listeners with `type: tcp` or `type: tls-passthrough`
proxy whole connections (databases, message brokers, TLS services you can't
or shouldn't decrypt) to `type: tcp` routes. Clusters work the same as for
HTTP: load balancing, health checks, outlier ejection, circuit breakers and
service discovery all apply.

| Listener | Behavior |
|---|---|
| `:7000` `type: tcp` | forwards bytes to the `raw` route |
| `:8443` `type: tls-passthrough` | reads the SNI from the TLS ClientHello and forwards the encrypted stream untouched |
| `:9444` `type: tcp` with `tls` | terminates TLS with the gateway's certificate, routes by SNI, forwards plaintext |

## Run

From the repository root. The TLS listener needs a certificate:

```sh
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 30 \
  -subj /CN=db.local -addext subjectAltName=DNS:db.local \
  -keyout examples/15-tcp-tls/tls.key -out examples/15-tcp-tls/tls.crt
go run ./cmd/demo-upstream -rest-tls :9443     # REST on :9001, REST over TLS on :9443
go run ./cmd/prisma -config examples/15-tcp-tls/prisma.yaml
```

## Raw TCP

The gateway doesn't parse the bytes, so any protocol works. Here it happens
to be HTTP:

```sh
curl -s localhost:7000/orders/1 | jq -c .customer.ssn
# "123-45-6789"      <- no masking: L4 routes never look inside the stream
```

## TLS passthrough

The gateway routes on the server name the client asks for, and the TLS
session is between the client and the upstream:

```sh
curl -sk --resolve api.local:8443:127.0.0.1 https://api.local:8443/orders/1 | jq .id
# "1"
openssl s_client -connect 127.0.0.1:8443 -servername v2.api.local </dev/null 2>/dev/null | grep ^subject=
# subject=CN=demo-1.local            <- the upstream's certificate, not the gateway's

curl -sk --resolve other.local:8443:127.0.0.1 https://other.local:8443/ ; echo $?
# 35                                 <- no route for that name: the connection is closed
```

## TLS termination

```sh
curl -sk --resolve db.local:9444:127.0.0.1 https://db.local:9444/orders/2 | jq .id
# "2"
openssl s_client -connect 127.0.0.1:9444 -servername db.local </dev/null 2>/dev/null | grep ^subject=
# subject=CN=db.local                <- the gateway's certificate
```

Clusters with a `tls` block re-encrypt towards the upstream (TLS
origination), so the gateway can terminate with a public certificate and
connect with internal ones.

## Routing rules

- A listener's tcp routes are matched by SNI: an exact name wins over the
  longest matching wildcard (`*.api.local`), which wins over a route without
  `hosts`. `priority` breaks ties.
- Plain `tcp` listeners have no SNI, so their routes can't use `hosts`.
- Routes support `backends` (weighted), `timeout` (idle timeout, default 1h),
  and `authz.allowIPs` / `denyIPs`. HTTP features (auth, rate limits,
  masking...) need an HTTP listener.
- Changing routes applies to new connections; open ones keep their upstream.
  Listener changes drain open connections for `global.drainTimeout`.

Every connection gets an access log line and metrics:

```sh
curl -s localhost:9901/metrics | grep prisma_tcp_
# prisma_tcp_connections_total{listener="passthrough",result="closed",route="api-passthrough"} 2
# prisma_tcp_connections_total{listener="passthrough",result="no_route",route="-"} 1
# prisma_tcp_bytes_total{direction="upstream",listener="tcp",route="raw"} 86
# prisma_tcp_active_connections{listener="tcp"} 0
```

## Kubernetes

With the Gateway API experimental channel installed, `TLSRoute` (passthrough
or terminate listeners) and `TCPRoute` (TCP or terminate listeners) become
these routes:

```yaml
apiVersion: gateway.networking.k8s.io/v1alpha2
kind: TLSRoute
metadata: {name: api, namespace: shop}
spec:
  parentRefs: [{name: edge, namespace: gateway, sectionName: tls}]
  hostnames: [api.example.com]
  rules: [{backendRefs: [{name: api, port: 443}]}]
```

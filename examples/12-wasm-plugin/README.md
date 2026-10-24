# 12 · Proxy-Wasm plugins

Plugins add custom logic to the gateway without forking it. Prisma implements
the [Proxy-Wasm](https://github.com/proxy-wasm/spec) ABI, the same one Envoy
and Istio use, so plugins written with the official Go, Rust or C++ SDKs run
unmodified. Plugins run in a sandbox: a plugin that crashes or loops forever
fails only the request it was handling.

This example's plugin ([plugin/main.go](plugin/main.go)) enforces a tenant
header, adds a header upstream and downstream, and exports a metric.

## Build and run

From the repository root:

```sh
(cd examples/12-wasm-plugin/plugin && GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o ../tenant.wasm .)
go run ./cmd/demo-upstream
go run ./cmd/prisma -config examples/12-wasm-plugin/prisma.yaml
```

With Rust: `cargo build --target wasm32-wasip1 --release` using the
`proxy-wasm` crate; point `file:` at the `.wasm` output.

## Try it

```sh
curl -si localhost:8080/hello | head -1
# HTTP/1.1 403 Forbidden                       (rejected by the plugin)

curl -s -H 'X-Tenant: acme' localhost:8080/hello | jq '.headers["X-Tenant-Plan"]'
# ["enterprise"]                               (added by the plugin)

curl -si -H 'X-Tenant: globex' localhost:8080/hello | grep -i x-served-tenant
# X-Served-Tenant: globex

curl -s localhost:9901/metrics | grep tenant_requests
# prisma_wasm_metric{name="tenant_requests_plan_enterprise",plugin="tenant"} 1
```

Edit the plans in `prisma.yaml` and save: the plugin restarts with the new
configuration on reload. Rebuild the `.wasm` and save the config (or `POST
/config/reload`) to load new plugin code.

## What plugins can do

| Capability | SDK calls |
|---|---|
| Read and change request and response headers, including `:path`, `:method`, `:authority` | `GetHttpRequestHeader`, `ReplaceHttpRequestHeader`, ... |
| Read and change bodies (set `bodies: true`) | `GetHttpRequestBody`, `ReplaceHttpResponseBody`, ... |
| Answer at the gateway | `SendHttpResponse` |
| Call other services through gateway clusters | `DispatchHttpCall("cluster-name", ...)` |
| Read request and identity data | `GetProperty`: `request.*`, `source.address`, `route_name`, `prisma.consumer`, `prisma.scopes` |
| Share state between requests and VMs | `SetSharedData` / `GetSharedData` (with compare-and-swap) |
| Hand work to a background context | `RegisterSharedQueue` in `OnPluginStart`, `EnqueueSharedQueue` from requests, `OnQueueReady` + `DequeueSharedQueue` in the plugin context |
| Export metrics | `DefineCounterMetric`, `DefineGaugeMetric`, `DefineHistogramMetric` |
| Run periodic work | `SetTickPeriodMilliSeconds` / `OnTick` |

Plugins can be attached to routes (`plugins:`), to policies, or to every route
through a policy listed in `global.use`. When several apply, they run in order: global, then
policy, then route.

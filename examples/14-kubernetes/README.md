# 14 · Kubernetes with the Gateway API

Prisma runs as a [Gateway API](https://gateway-api.sigs.k8s.io/) controller:
it watches Gateways, HTTPRoutes and GRPCRoutes, turns them into listeners and
routes without restarts, and writes their status back. Prisma-specific
features (rate limits, masking, JWT, plugins, ...) attach through the
`PrismaPolicy` resource.

This walkthrough uses [kind](https://kind.sigs.k8s.io/) for a local cluster.

## 1. Cluster and images

From the repository root:

```sh
kind create cluster --name prisma
docker build -t prisma:dev .
docker build -f examples/14-kubernetes/Dockerfile.demo -t prisma-demo:dev .
kind load docker-image prisma:dev prisma-demo:dev --name prisma
```

## 2. Gateway API and Prisma

```sh
kubectl apply --server-side -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.2.1/standard-install.yaml
kubectl apply -f deploy/kubernetes/gateway-api.yaml
kubectl -n gateway set image deploy/prisma prisma=prisma:dev
kubectl -n gateway rollout status deploy/prisma
```

`gateway-api.yaml` installs the `PrismaPolicy` CRD, the `prisma`
GatewayClass, RBAC and the controller Deployment.

```sh
kubectl get gatewayclass prisma          # ACCEPTED: True
```

## 3. The application

```sh
kubectl apply -f examples/14-kubernetes/app.yaml
kubectl -n shop rollout status deploy/orders
```

```sh
kubectl -n gateway get gateway public    # PROGRAMMED: True
```

This creates a `public` Gateway on port 80 (in the `gateway` namespace) and a
`shop` namespace with a stable and a canary deployment of the demo upstream,
an HTTPRoute that splits `/shop/orders` 80/20 between them, a GRPCRoute for
the gRPC health service, and a PrismaPolicy that adds rate limiting, masking
and compression to the HTTPRoute.

```sh
kubectl -n shop get httproute orders -o jsonpath='{.status.parents[0].conditions}' | jq .
# [{"reason":"Accepted","status":"True",...}, {"reason":"ResolvedRefs","status":"True",...}]
kubectl -n shop get prismapolicy orders-protection -o jsonpath='{.status}' | jq .
```

## 4. Send traffic

```sh
kubectl -n gateway port-forward svc/prisma 8080:80 &

curl -s localhost:8080/shop/orders/1 | jq .customer.ssn     # "***" (PrismaPolicy masking)
for i in $(seq 20); do curl -s localhost:8080/shop/orders/1 | jq -r .instance; done | sort | uniq -c
#  16 stable
#   4 canary
grpcurl -plaintext localhost:8080 grpc.health.v1.Health/Check
```

Change the weights in `app.yaml` and re-apply: the split changes within a
second, without restarting the gateway. Scale the deployments and the
endpoints follow pod readiness through EndpointSlices.

## 5. Debugging

```sh
kubectl -n gateway port-forward deploy/prisma 9901 &
curl -s localhost:9901/config | less                 # the config generated from Gateway API resources
curl -s 'localhost:9901/routes?path=/shop/orders/1' | jq .
curl -s localhost:9901/clusters | jq .               # endpoints discovered from EndpointSlices
kubectl -n gateway logs deploy/prisma | grep -v '"msg":"access"'
```

Invalid resources don't take the gateway down: a route with an unknown
backend gets `ResolvedRefs: False` (reason `BackendNotFound`) in its status,
and an invalid PrismaPolicy is rejected on its own with the reason in its
status.

## Without the Gateway API

For clusters without the Gateway API, or for a plain config file workflow,
[deploy/kubernetes/prisma.yaml](../../deploy/kubernetes/prisma.yaml) runs
Prisma from a ConfigMap. Clusters can still follow Services:

```yaml
clusters:
  - name: orders
    discovery: {type: kubernetes, kubernetes: {service: orders, namespace: shop, portName: http}}
```

## Clean up

```sh
kind delete cluster --name prisma
```

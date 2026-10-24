package gatewayapi

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"prisma/internal/config"
	"prisma/internal/k8s"
)

const controller = "prisma.dev/gateway-controller"

// objs parses "---"-separated YAML manifests into k8s.Objects by kind.
func objs(t *testing.T, manifests string) map[string]map[string]k8s.Object {
	t.Helper()
	out := map[string]map[string]k8s.Object{}
	for _, doc := range strings.Split(manifests, "\n---\n") {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		var m map[string]any
		if err := yaml.Unmarshal([]byte(doc), &m); err != nil {
			t.Fatalf("fixture: %v\n%s", err, doc)
		}
		raw, _ := json.Marshal(m)
		md := m["metadata"].(map[string]any)
		ns, _ := md["namespace"].(string)
		o := k8s.Object{Namespace: ns, Name: md["name"].(string), Raw: raw}
		kind := m["kind"].(string)
		if out[kind] == nil {
			out[kind] = map[string]k8s.Object{}
		}
		out[kind][o.Key()] = o
	}
	return out
}

func translate(t *testing.T, manifests string, base *config.Config) Output {
	t.Helper()
	o := objs(t, manifests)
	if base == nil {
		base = &config.Config{APIVersion: config.APIVersion}
	}
	out := Translate(Input{
		ControllerName: controller, Base: base,
		GatewayClasses: o["GatewayClass"], Gateways: o["Gateway"], HTTPRoutes: o["HTTPRoute"], GRPCRoutes: o["GRPCRoute"],
		ReferenceGrants: o["ReferenceGrant"], Services: o["Service"], Secrets: o["Secret"], Policies: o["PrismaPolicy"],
		TLSRoutes: o["TLSRoute"], TCPRoutes: o["TCPRoute"],
	})
	// Every translation must produce a configuration the gateway accepts.
	cfg := *base
	cfg.GatewayAPI = &config.GatewayAPI{ControllerName: controller}
	cfg.Listeners = append(slices.Clone(base.Listeners), out.Listeners...)
	cfg.Clusters = append(slices.Clone(base.Clusters), out.Clusters...)
	cfg.Routes = append(slices.Clone(base.Routes), out.Routes...)
	cfg.Policies = map[string]config.Policy{}
	for k, v := range base.Policies {
		cfg.Policies[k] = v
	}
	for k, v := range out.Policies {
		cfg.Policies[k] = v
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("translated config is invalid: %v", err)
	}
	return out
}

const classes = `
apiVersion: gateway.networking.k8s.io/v1
kind: GatewayClass
metadata: {name: prisma}
spec: {controllerName: prisma.dev/gateway-controller}
---
apiVersion: gateway.networking.k8s.io/v1
kind: GatewayClass
metadata: {name: other}
spec: {controllerName: example.com/other}
---
apiVersion: v1
kind: Service
metadata: {name: orders, namespace: default}
spec:
  ports: [{name: http, port: 80, targetPort: 8080}]
---
apiVersion: v1
kind: Service
metadata: {name: grpc-svc, namespace: default}
spec:
  ports: [{port: 9090, targetPort: 9090}]
`

const gatewayHTTP = `
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata: {name: web, namespace: default}
spec:
  gatewayClassName: prisma
  listeners:
    - {name: http, port: 80, protocol: HTTP, hostname: "*.example.com", allowedRoutes: {namespaces: {from: All}}}
    - {name: udp, port: 9000, protocol: UDP}
`

func route(t *testing.T, out Output, kind, ns, name string) RouteStatus {
	t.Helper()
	for _, r := range out.Status.Routes {
		if r.Kind == kind && r.Namespace == ns && r.Name == name {
			return r
		}
	}
	t.Fatalf("no status for %s %s/%s: %+v", kind, ns, name, out.Status.Routes)
	return RouteStatus{}
}

func condition(cs []Condition, typ string) Condition {
	for _, c := range cs {
		if c.Type == typ {
			return c
		}
	}
	return Condition{}
}

func TestHTTPRouteTranslation(t *testing.T) {
	out := translate(t, classes+"\n---\n"+gatewayHTTP+`
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata: {name: orders, namespace: default}
spec:
  parentRefs: [{name: web}]
  hostnames: [api.example.com]
  rules:
    - matches:
        - path: {type: PathPrefix, value: /orders}
          headers: [{name: X-Version, value: "2"}]
          queryParams: [{name: tenant, value: acme}]
          method: GET
      filters:
        - type: RequestHeaderModifier
          requestHeaderModifier: {set: [{name: X-Gw, value: prisma}], remove: [X-Debug]}
        - type: URLRewrite
          urlRewrite: {path: {type: ReplacePrefixMatch, replacePrefixMatch: /}}
      timeouts: {request: 5s}
      backendRefs: [{name: orders, port: 80, weight: 3}]
    - matches: [{path: {type: Exact, value: /old}}]
      filters:
        - type: RequestRedirect
          requestRedirect: {scheme: https, statusCode: 301}
`, nil)

	if len(out.Listeners) != 1 || out.Listeners[0].Name != "k8s/port/80" || out.Listeners[0].Address != ":80" {
		t.Fatalf("listeners = %+v", out.Listeners)
	}
	if len(out.Clusters) != 1 {
		t.Fatalf("clusters = %+v", out.Clusters)
	}
	cl := out.Clusters[0]
	if cl.Name != "k8s/svc/default/orders/80" || cl.Discovery.Kubernetes.Service != "orders" || cl.Discovery.Kubernetes.PortName != "http" {
		t.Errorf("cluster = %+v %+v", cl, cl.Discovery.Kubernetes)
	}
	if len(out.Routes) != 2 {
		t.Fatalf("routes = %+v", out.Routes)
	}
	r := out.Routes[0]
	m := r.Match
	if m.PathPrefix != "/orders" || m.Headers["X-Version"] != "2" || m.QueryParams["tenant"] != "acme" || m.Methods[0] != "GET" ||
		!reflect.DeepEqual(m.Hosts, []string{"api.example.com"}) || !reflect.DeepEqual(r.Listeners, []string{"k8s/port/80"}) {
		t.Errorf("match = %+v listeners %v", m, r.Listeners)
	}
	if r.Headers.Request.Set["X-Gw"] != "prisma" || r.Rewrite.StripPrefix != "/orders" || r.Rewrite.ReplacePrefix != "/" ||
		*r.Timeout != 5*time.Second || r.Backends[0].Weight != 3 {
		t.Errorf("rule 0 = %+v", r)
	}
	if red := out.Routes[1].Redirect; red == nil || red.Scheme != "https" || red.Status != 301 || out.Routes[1].Match.Path != "/old" {
		t.Errorf("redirect rule = %+v", out.Routes[1])
	}

	rs := route(t, out, KindHTTPRoute, "default", "orders")
	if len(rs.Parents) != 1 || condition(rs.Parents[0].Conditions, "Accepted").Status != "True" ||
		condition(rs.Parents[0].Conditions, "ResolvedRefs").Status != "True" {
		t.Errorf("route status = %+v", rs)
	}
	gs := out.Status.Gateways[0]
	if condition(gs.Conditions, "Programmed").Status != "True" || gs.Listeners[0].AttachedRoutes != 1 {
		t.Errorf("gateway status = %+v", gs)
	}
	if c := condition(gs.Listeners[1].Conditions, "Accepted"); c.Status != "False" || c.Reason != "UnsupportedProtocol" {
		t.Errorf("UDP listener = %+v", gs.Listeners[1])
	}
	if len(out.Status.Classes) != 1 || out.Status.Classes[0].Name != "prisma" {
		t.Errorf("classes = %+v", out.Status.Classes)
	}
}

func TestAttachmentRules(t *testing.T) {
	out := translate(t, classes+`
---
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata: {name: web, namespace: default}
spec:
  gatewayClassName: prisma
  listeners: [{name: http, port: 80, protocol: HTTP, hostname: "*.example.com"}]
---
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata: {name: foreign, namespace: default}
spec:
  gatewayClassName: other
  listeners: [{name: http, port: 81, protocol: HTTP}]
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata: {name: wrong-host, namespace: default}
spec:
  parentRefs: [{name: web}]
  hostnames: [shop.other.org]
  rules: [{backendRefs: [{name: orders, port: 80}]}]
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata: {name: other-ns, namespace: team}
spec:
  parentRefs: [{name: web, namespace: default}]
  rules: [{backendRefs: [{name: orders, port: 80}]}]
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata: {name: foreign-parent, namespace: default}
spec:
  parentRefs: [{name: foreign}]
  rules: [{backendRefs: [{name: orders, port: 80}]}]
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata: {name: bad-section, namespace: default}
spec:
  parentRefs: [{name: web, sectionName: https}]
  rules: [{backendRefs: [{name: orders, port: 80}]}]
`, nil)
	if c := condition(route(t, out, KindHTTPRoute, "default", "wrong-host").Parents[0].Conditions, "Accepted"); c.Reason != "NoMatchingListenerHostname" {
		t.Errorf("hostname mismatch: %+v", c)
	}
	if c := condition(route(t, out, KindHTTPRoute, "team", "other-ns").Parents[0].Conditions, "Accepted"); c.Reason != "NotAllowedByListeners" {
		t.Errorf("namespace not allowed: %+v", c)
	}
	if c := condition(route(t, out, KindHTTPRoute, "default", "bad-section").Parents[0].Conditions, "Accepted"); c.Reason != "NoMatchingParent" {
		t.Errorf("unknown section: %+v", c)
	}
	for _, r := range out.Status.Routes {
		if r.Name == "foreign-parent" {
			t.Error("routes of other controllers' Gateways must not get our status")
		}
	}
	if len(out.Routes) != 0 {
		t.Errorf("no route should be programmed: %+v", out.Routes)
	}
}

func TestBackendResolution(t *testing.T) {
	out := translate(t, classes+"\n---\n"+gatewayHTTP+`
---
apiVersion: v1
kind: Service
metadata: {name: payments, namespace: billing}
spec: {ports: [{name: http, port: 80}]}
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata: {name: missing, namespace: default}
spec:
  parentRefs: [{name: web}]
  rules: [{matches: [{path: {value: /missing}}], backendRefs: [{name: ghost, port: 80}]}]
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata: {name: cross, namespace: default}
spec:
  parentRefs: [{name: web}]
  rules: [{matches: [{path: {value: /pay}}], backendRefs: [{name: payments, namespace: billing, port: 80}]}]
`, nil)
	missing := route(t, out, KindHTTPRoute, "default", "missing")
	if c := condition(missing.Parents[0].Conditions, "ResolvedRefs"); c.Status != "False" || c.Reason != "BackendNotFound" {
		t.Errorf("missing service: %+v", c)
	}
	cross := route(t, out, KindHTTPRoute, "default", "cross")
	if c := condition(cross.Parents[0].Conditions, "ResolvedRefs"); c.Reason != "RefNotPermitted" {
		t.Errorf("cross-namespace without grant: %+v", c)
	}
	for _, r := range out.Routes {
		if r.DirectResponse == nil || r.DirectResponse.Status != 500 || len(r.Backends) != 0 {
			t.Errorf("unresolvable route %s must answer 500: %+v", r.Name, r)
		}
	}

	granted := translate(t, classes+"\n---\n"+gatewayHTTP+`
---
apiVersion: v1
kind: Service
metadata: {name: payments, namespace: billing}
spec: {ports: [{name: http, port: 80}]}
---
apiVersion: gateway.networking.k8s.io/v1beta1
kind: ReferenceGrant
metadata: {name: allow-default, namespace: billing}
spec:
  from: [{group: gateway.networking.k8s.io, kind: HTTPRoute, namespace: default}]
  to: [{group: "", kind: Service}]
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata: {name: cross, namespace: default}
spec:
  parentRefs: [{name: web}]
  rules: [{backendRefs: [{name: payments, namespace: billing, port: 80}]}]
`, nil)
	if c := condition(route(t, granted, KindHTTPRoute, "default", "cross").Parents[0].Conditions, "ResolvedRefs"); c.Status != "True" {
		t.Errorf("ReferenceGrant should permit the backend: %+v", c)
	}
}

func selfSigned(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "example.com"},
		DNSNames: []string{"*.example.com"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := x509.MarshalECPrivateKey(key)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})
}

func TestHTTPSListenersAndConflicts(t *testing.T) {
	crt, key := selfSigned(t)
	b64 := base64.StdEncoding.EncodeToString
	out := translate(t, classes+`
---
apiVersion: v1
kind: Secret
metadata: {name: web-tls, namespace: default}
type: kubernetes.io/tls
data: {tls.crt: `+b64(crt)+`, tls.key: `+b64(key)+`}
---
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata: {name: secure, namespace: default, creationTimestamp: "2026-01-01T00:00:00Z"}
spec:
  gatewayClassName: prisma
  listeners:
    - {name: https, port: 443, protocol: HTTPS, tls: {certificateRefs: [{name: web-tls}]}}
    - {name: broken, port: 8443, protocol: HTTPS, tls: {certificateRefs: [{name: nope}]}}
---
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata: {name: clash, namespace: default, creationTimestamp: "2026-02-01T00:00:00Z"}
spec:
  gatewayClassName: prisma
  listeners: [{name: http, port: 443, protocol: HTTP}]
`, nil)
	if len(out.Listeners) != 1 || out.Listeners[0].TLS == nil || out.Listeners[0].TLS.Certificates[0].CertPEM != string(crt) {
		t.Fatalf("https listener = %+v", out.Listeners)
	}
	var secure, clash GatewayStatus
	for _, g := range out.Status.Gateways {
		if g.Name == "secure" {
			secure = g
		} else {
			clash = g
		}
	}
	if c := condition(secure.Listeners[1].Conditions, "ResolvedRefs"); c.Status != "False" || c.Reason != "InvalidCertificateRef" {
		t.Errorf("missing secret: %+v", secure.Listeners[1])
	}
	if c := condition(clash.Listeners[0].Conditions, "Conflicted"); c.Status != "True" || c.Reason != "ProtocolConflict" {
		t.Errorf("protocol conflict: %+v", clash.Listeners[0])
	}
	if c := condition(clash.Conditions, "Programmed"); c.Status != "False" {
		t.Errorf("gateway with no programmable listener: %+v", clash.Conditions)
	}
}

func TestGRPCRouteTranslation(t *testing.T) {
	out := translate(t, classes+"\n---\n"+gatewayHTTP+`
---
apiVersion: gateway.networking.k8s.io/v1
kind: GRPCRoute
metadata: {name: users, namespace: default}
spec:
  parentRefs: [{name: web}]
  hostnames: [grpc.example.com]
  rules:
    - matches:
        - method: {service: users.v1.Users, method: Get}
        - method: {method: Health}
      backendRefs: [{name: grpc-svc, port: 9090}]
`, nil)
	if len(out.Routes) != 2 {
		t.Fatalf("routes = %+v", out.Routes)
	}
	if m := out.Routes[0].Match; m.GRPCService != "users.v1.Users" || m.GRPCMethod != "Get" || out.Routes[0].Type != "grpc" {
		t.Errorf("service+method match = %+v", m)
	}
	if m := out.Routes[1].Match; m.PathRegex != "^/[^/]+/Health$" {
		t.Errorf("method-only match = %+v", m)
	}
	if out.Clusters[0].Protocol != "h2c" || out.Clusters[0].Discovery.Port != 9090 {
		t.Errorf("gRPC cluster = %+v", out.Clusters[0])
	}
}

func TestPrismaPolicyAttachment(t *testing.T) {
	base := &config.Config{APIVersion: config.APIVersion, Redis: nil}
	out := translate(t, classes+"\n---\n"+gatewayHTTP+`
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata: {name: orders, namespace: default}
spec:
  parentRefs: [{name: web}]
  rules: [{backendRefs: [{name: orders, port: 80}]}]
---
apiVersion: prisma.dev/v1alpha1
kind: PrismaPolicy
metadata: {name: gateway-defaults, namespace: default, creationTimestamp: "2026-01-01T00:00:00Z"}
spec:
  targetRefs: [{group: gateway.networking.k8s.io, kind: Gateway, name: web}]
  cors: {allowOrigins: ["https://app.example.com"]}
---
apiVersion: prisma.dev/v1alpha1
kind: PrismaPolicy
metadata: {name: orders-limits, namespace: default, creationTimestamp: "2026-01-02T00:00:00Z"}
spec:
  targetRefs: [{group: gateway.networking.k8s.io, kind: HTTPRoute, name: orders}]
  rateLimit: [{key: [client_ip], limit: 10, period: 1s}]
  masking: {rules: [{selector: "$..ssn", strategy: redact}], maxBufferBytes: 2MiB}
---
apiVersion: prisma.dev/v1alpha1
kind: PrismaPolicy
metadata: {name: broken, namespace: default}
spec:
  targetRefs: [{group: gateway.networking.k8s.io, kind: HTTPRoute, name: orders}]
  rateLimit: [{key: [nope], limit: 0, period: 1s}]
---
apiVersion: prisma.dev/v1alpha1
kind: PrismaPolicy
metadata: {name: dangling, namespace: default}
spec:
  targetRefs: [{group: gateway.networking.k8s.io, kind: HTTPRoute, name: ghost}]
  cors: {allowOrigins: ["*"]}
`, base)

	use := out.Routes[0].Use
	if !reflect.DeepEqual(use, []string{"k8s/policy/default/gateway-defaults", "k8s/policy/default/orders-limits"}) {
		t.Errorf("route policies = %v (gateway policies first, route policies override)", use)
	}
	p := out.Policies["k8s/policy/default/orders-limits"]
	if p.RateLimit[0].Period != time.Second || p.Masking.MaxBufferBytes != 2<<20 {
		t.Errorf("policy decoded with config semantics: %+v", p)
	}
	if _, ok := out.Policies["k8s/policy/default/broken"]; ok {
		t.Error("invalid policy must not be applied")
	}
	statuses := map[string]Condition{}
	for _, ps := range out.Status.Policies {
		statuses[ps.Name] = condition(ps.Ancestors[0].Conditions, "Accepted")
	}
	if statuses["orders-limits"].Status != "True" || statuses["broken"].Reason != "Invalid" || statuses["dangling"].Reason != "TargetNotFound" {
		t.Errorf("policy statuses = %+v", statuses)
	}
}

func TestTranslationIsDeterministic(t *testing.T) {
	manifests := classes + "\n---\n" + gatewayHTTP + `
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata: {name: a, namespace: default}
spec:
  parentRefs: [{name: web}]
  rules: [{backendRefs: [{name: orders, port: 80}]}, {matches: [{path: {value: /x}}], backendRefs: [{name: orders, port: 80}]}]
`
	first := translate(t, manifests, nil)
	for i := 0; i < 20; i++ {
		if !reflect.DeepEqual(first, translate(t, manifests, nil)) {
			t.Fatal("translation output depends on map iteration order")
		}
	}
}

func TestTLSAndTCPRouteTranslation(t *testing.T) {
	crt, key := selfSigned(t)
	b64 := base64.StdEncoding.EncodeToString
	out := translate(t, classes+`
---
apiVersion: v1
kind: Service
metadata: {name: db, namespace: default}
spec:
  ports: [{name: pg, port: 5432, targetPort: 5432}]
---
apiVersion: v1
kind: Secret
metadata: {name: db-tls, namespace: default}
type: kubernetes.io/tls
data: {tls.crt: `+b64(crt)+`, tls.key: `+b64(key)+`}
---
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata: {name: edge, namespace: default}
spec:
  gatewayClassName: prisma
  listeners:
    - {name: passthrough, port: 443, protocol: TLS, tls: {mode: Passthrough}}
    - {name: postgres, port: 5432, protocol: TCP}
    - {name: postgres-tls, port: 6432, protocol: TLS, hostname: db.example.com, tls: {certificateRefs: [{name: db-tls}]}}
    - {name: tls-only-tcp, port: 7000, protocol: TLS, tls: {mode: Passthrough}, allowedRoutes: {kinds: [{kind: TCPRoute}]}}
---
apiVersion: gateway.networking.k8s.io/v1alpha2
kind: TLSRoute
metadata: {name: orders, namespace: default}
spec:
  parentRefs: [{name: edge, sectionName: passthrough}]
  hostnames: [orders.example.com, "*.shop.example.com"]
  rules: [{backendRefs: [{name: orders, port: 80}]}]
---
apiVersion: gateway.networking.k8s.io/v1alpha2
kind: TCPRoute
metadata: {name: pg, namespace: default}
spec:
  parentRefs: [{name: edge, sectionName: postgres}, {name: edge, sectionName: postgres-tls}]
  rules: [{backendRefs: [{name: db, port: 5432, weight: 3}, {name: missing, port: 1}]}]
---
apiVersion: gateway.networking.k8s.io/v1alpha2
kind: TCPRoute
metadata: {name: wrong-listener, namespace: default}
spec:
  parentRefs: [{name: edge, sectionName: passthrough}]
  rules: [{backendRefs: [{name: db, port: 5432}]}]
---
apiVersion: gateway.networking.k8s.io/v1alpha2
kind: TLSRoute
metadata: {name: no-backends, namespace: default}
spec:
  parentRefs: [{name: edge, sectionName: passthrough}]
  hostnames: [nothing.example.com]
  rules: [{backendRefs: [{name: missing, port: 1}]}]
`, nil)

	listeners := map[string]config.Listener{}
	for _, l := range out.Listeners {
		listeners[l.Address] = l
	}
	if l := listeners[":443"]; l.Type != config.ListenerTLSPassthrough || l.TLS != nil {
		t.Errorf("passthrough listener = %+v", l)
	}
	if l := listeners[":5432"]; l.Type != config.ListenerTCP || l.TLS != nil {
		t.Errorf("tcp listener = %+v", l)
	}
	if l := listeners[":6432"]; l.Type != config.ListenerTCP || l.TLS == nil || l.TLS.Certificates[0].CertPEM != string(crt) {
		t.Errorf("tls-terminating listener = %+v", l)
	}

	byName := map[string]config.Route{}
	for _, r := range out.Routes {
		byName[r.Name] = r
		if r.Type != config.RouteTCP || len(r.Use) > 0 {
			t.Errorf("route %s: type %q, use %v", r.Name, r.Type, r.Use)
		}
	}
	tls := byName["k8s/tlsroute/default/orders/r0/m0/a0"]
	if !reflect.DeepEqual(tls.Match.Hosts, []string{"orders.example.com", "*.shop.example.com"}) || tls.Listeners[0] != "k8s/port/443" {
		t.Errorf("TLSRoute = %+v", tls)
	}
	plain, terminated := byName["k8s/tcproute/default/pg/r0/m0/a0"], byName["k8s/tcproute/default/pg/r0/m0/a1"]
	if plain.Match.Hosts != nil || plain.Listeners[0] != "k8s/port/5432" || len(plain.Backends) != 1 || plain.Backends[0].Weight != 3 {
		t.Errorf("TCPRoute on TCP = %+v", plain)
	}
	if !reflect.DeepEqual(terminated.Match.Hosts, []string{"db.example.com"}) || terminated.Listeners[0] != "k8s/port/6432" {
		t.Errorf("TCPRoute on terminating TLS = %+v", terminated)
	}
	if _, ok := byName["k8s/tlsroute/default/no-backends/r0/m0/a0"]; ok {
		t.Error("a TLSRoute without resolvable backends must not produce a route")
	}

	rs := route(t, out, KindTCPRoute, "default", "pg")
	if c := condition(rs.Parents[0].Conditions, "ResolvedRefs"); c.Status != "False" || c.Reason != "BackendNotFound" {
		t.Errorf("pg ResolvedRefs = %+v", c)
	}
	rs = route(t, out, KindTCPRoute, "default", "wrong-listener")
	if c := condition(rs.Parents[0].Conditions, "Accepted"); c.Status != "False" || c.Reason != "NotAllowedByListeners" {
		t.Errorf("TCPRoute on a passthrough listener: %+v", c)
	}
	for _, gs := range out.Status.Gateways {
		for _, ls := range gs.Listeners {
			switch ls.Name {
			case "passthrough":
				if !reflect.DeepEqual(ls.SupportedKinds, []string{KindTLSRoute}) || ls.AttachedRoutes != 2 {
					t.Errorf("passthrough listener status = %+v", ls)
				}
			case "tls-only-tcp":
				// TCPRoute cannot ride a passthrough listener, so nothing is left.
				if len(ls.SupportedKinds) != 0 {
					t.Errorf("kinds narrowed to an unsupported kind = %v", ls.SupportedKinds)
				}
			}
		}
	}
}

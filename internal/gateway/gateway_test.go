package gateway_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/golang-jwt/jwt/v5"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"prisma/internal/config"
	"prisma/internal/gateway"
	"prisma/internal/telemetry"
)

const jwtSecret = "test-secret-at-least-32-bytes-long!!"

// newGateway applies the YAML config (with {{key}} placeholders replaced) and
// returns an HTTP test server for the "public" listener.
func newGateway(t *testing.T, yml string, vars map[string]string) (*gateway.Gateway, *httptest.Server) {
	t.Helper()
	for k, v := range vars {
		yml = strings.ReplaceAll(yml, "{{"+k+"}}", v)
	}
	cfg, version, err := config.Parse([]byte(yml))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	gw := gateway.New(telemetry.NewMetrics(), nil)
	if err := gw.Apply(cfg, version); err != nil {
		t.Fatalf("apply: %v", err)
	}
	t.Cleanup(gw.Close)
	srv := httptest.NewServer(gw.Handler("public"))
	t.Cleanup(srv.Close)
	return gw, srv
}

func addr(s *httptest.Server) string { return strings.TrimPrefix(s.URL, "http://") }

// echo returns an upstream that reports what it received as JSON.
func echo(t *testing.T) *httptest.Server {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"path":      r.URL.Path,
			"host":      r.Host,
			"xff":       r.Header.Get("X-Forwarded-For"),
			"requestId": r.Header.Get("X-Request-Id"),
			"consumer":  r.Header.Get("X-Prisma-Consumer"),
			"apiKey":    r.Header.Get("X-Api-Key"),
		})
	}))
	t.Cleanup(s.Close)
	return s
}

func get(t *testing.T, url string, hdr map[string]string) (*http.Response, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]any
	raw, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(raw, &body)
	return resp, body
}

const basePrefix = `
apiVersion: prisma/v1
listeners:
  - name: public
    address: "127.0.0.1:0"
`

func TestProxyRewriteAndForwardedHeaders(t *testing.T) {
	up := echo(t)
	_, gw := newGateway(t, basePrefix+`
clusters:
  - name: orders
    endpoints: [{address: "{{up}}"}]
routes:
  - name: orders
    match: {pathPrefix: /api/v1/orders}
    rewrite: {stripPrefix: /api/v1}
    backends: [{cluster: orders}]
`, map[string]string{"up": addr(up)})

	resp, body := get(t, gw.URL+"/api/v1//orders/../orders/42", map[string]string{
		"X-Forwarded-For":   "6.6.6.6",
		"X-Prisma-Consumer": "spoofed",
	})
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if body["path"] != "/orders/42" {
		t.Errorf("upstream path = %v, want /orders/42", body["path"])
	}
	if body["xff"] != "127.0.0.1" {
		t.Errorf("untrusted X-Forwarded-For was forwarded: %v", body["xff"])
	}
	if body["consumer"] != "" {
		t.Errorf("spoofed identity header reached upstream: %v", body["consumer"])
	}
	if body["host"] != addr(up) {
		t.Errorf("host = %v, want endpoint address", body["host"])
	}
	if id := resp.Header.Get("X-Request-Id"); id == "" || id != body["requestId"] {
		t.Errorf("request id mismatch: client %q upstream %v", id, body["requestId"])
	}

	resp, _ = get(t, gw.URL+"/api/v1/ordersX", nil)
	if resp.StatusCode != 404 || resp.Header.Get("Content-Type") != "application/problem+json" {
		t.Errorf("segment-boundary prefix: status %d ct %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
}

func TestRedirects(t *testing.T) {
	_, gw := newGateway(t, basePrefix+`
routes:
  - name: legacy
    match: {pathPrefix: /v0/orders}
    redirect: {pathPrefixReplace: /v1/orders, status: 308}
  - name: https
    match: {pathPrefix: /}
    redirect: {scheme: https}
`, nil)
	resp, _ := get(t, gw.URL+"/v0/orders/7?x=1", nil)
	if resp.StatusCode != 308 || !strings.HasSuffix(resp.Header.Get("Location"), "/v1/orders/7?x=1") {
		t.Errorf("legacy redirect: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp, _ = get(t, gw.URL+"/a", nil)
	if resp.StatusCode != 301 || resp.Header.Get("Location") != "https://127.0.0.1/a" {
		t.Errorf("https redirect: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func signJWT(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	s, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(jwtSecret))
	if err != nil {
		t.Fatal(err)
	}
	return "Bearer " + s
}

func TestJWTAndAPIKeyAuth(t *testing.T) {
	up := echo(t)
	_, gw := newGateway(t, basePrefix+`
policies:
  auth:
    authn:
      jwt: {secret: "`+jwtSecret+`", issuer: "https://idp.test/", audiences: [api]}
      apiKey: {header: X-API-Key}
consumers:
  - name: partner-a
    apiKeys: ["key-123"]
    scopes: [orders:read]
clusters:
  - name: up
    endpoints: [{address: "{{up}}"}]
routes:
  - name: orders
    match: {pathPrefix: /orders}
    use: [auth]
    authz: {scopes: [orders:read], methods: {DELETE: [orders:admin]}}
    backends: [{cluster: up}]
`, map[string]string{"up": addr(up)})

	exp := time.Now().Add(time.Hour).Unix()
	cases := []struct {
		name   string
		hdr    map[string]string
		status int
	}{
		{"missing credentials", nil, 401},
		{"garbage token", map[string]string{"Authorization": "Bearer nope"}, 401},
		{"wrong audience", map[string]string{"Authorization": signJWT(t, jwt.MapClaims{
			"sub": "u1", "iss": "https://idp.test/", "aud": "other", "exp": exp, "scope": "orders:read"})}, 401},
		{"expired", map[string]string{"Authorization": signJWT(t, jwt.MapClaims{
			"sub": "u1", "iss": "https://idp.test/", "aud": "api", "exp": time.Now().Add(-time.Hour).Unix(), "scope": "orders:read"})}, 401},
		{"missing scope", map[string]string{"Authorization": signJWT(t, jwt.MapClaims{
			"sub": "u1", "iss": "https://idp.test/", "aud": "api", "exp": exp})}, 403},
		{"valid jwt", map[string]string{"Authorization": signJWT(t, jwt.MapClaims{
			"sub": "u1", "iss": "https://idp.test/", "aud": []string{"api"}, "exp": exp, "scope": "orders:read profile"})}, 200},
		{"unknown api key", map[string]string{"X-API-Key": "bad"}, 401},
		{"valid api key", map[string]string{"X-API-Key": "key-123"}, 200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := get(t, gw.URL+"/orders", tc.hdr)
			if resp.StatusCode != tc.status {
				t.Fatalf("status %d, want %d (%v)", resp.StatusCode, tc.status, body)
			}
			if tc.status == 401 && resp.Header.Get("Www-Authenticate") == "" {
				t.Error("missing WWW-Authenticate")
			}
			if tc.status == 200 {
				if body["consumer"] == "" {
					t.Error("upstream did not receive X-Prisma-Consumer")
				}
				if body["apiKey"] != "" {
					t.Error("api key was forwarded upstream")
				}
			}
		})
	}
}

func TestCORSRunsBeforeAuthAndDecoratesErrors(t *testing.T) {
	var hits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Vary", "Accept-Encoding")
	}))
	defer up.Close()
	_, gw := newGateway(t, basePrefix+`
consumers: [{name: web, apiKeys: [k]}]
clusters:
  - name: up
    endpoints: [{address: "{{up}}"}]
routes:
  - name: api
    match: {pathPrefix: /}
    cors: {allowOrigins: ["https://app.example.com"], allowCredentials: true}
    authn: {apiKey: {}}
    backends: [{cluster: up}]
`, map[string]string{"up": addr(up)})

	req, _ := http.NewRequest(http.MethodOptions, gw.URL+"/x", nil)
	req.Header.Set("Origin", "https://app.example.com")
	req.Header.Set("Access-Control-Request-Method", "GET")
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 204 || resp.Header.Get("Access-Control-Allow-Origin") != "https://app.example.com" || hits.Load() != 0 {
		t.Fatalf("preflight: %d %v, upstream hits %d", resp.StatusCode, resp.Header, hits.Load())
	}

	resp, _ = get(t, gw.URL+"/x", map[string]string{"Origin": "https://app.example.com"})
	if resp.StatusCode != 401 || resp.Header.Get("Access-Control-Allow-Origin") == "" {
		t.Errorf("401 must carry CORS headers so the browser can read it: %d %v", resp.StatusCode, resp.Header)
	}

	resp, _ = get(t, gw.URL+"/x", map[string]string{"Origin": "https://app.example.com", "X-API-Key": "k"})
	vary := strings.Join(resp.Header.Values("Vary"), ",")
	if resp.StatusCode != 200 || !strings.Contains(vary, "Accept-Encoding") || !strings.Contains(vary, "Origin") {
		t.Errorf("upstream and CORS Vary must both survive: %d %q", resp.StatusCode, vary)
	}
}

func TestLocalRateLimit(t *testing.T) {
	up := echo(t)
	_, gw := newGateway(t, basePrefix+`
clusters:
  - name: up
    endpoints: [{address: "{{up}}"}]
routes:
  - name: limited
    match: {pathPrefix: /}
    rateLimit:
      - {key: [client_ip], limit: 3, period: 1m}
    backends: [{cluster: up}]
`, map[string]string{"up": addr(up)})
	for i := 0; i < 3; i++ {
		resp, _ := get(t, gw.URL+"/", nil)
		if resp.StatusCode != 200 {
			t.Fatalf("request %d: status %d", i, resp.StatusCode)
		}
		if got, want := resp.Header.Get("RateLimit-Remaining"), fmt.Sprint(2-i); got != want {
			t.Errorf("request %d: remaining %s, want %s", i, got, want)
		}
	}
	resp, body := get(t, gw.URL+"/", nil)
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") == "" || body["code"] != "RATE_LIMITED" {
		t.Fatalf("expected 429 with Retry-After, got %d %v", resp.StatusCode, body)
	}
}

func TestRedisRateLimitIsSharedAcrossReplicas(t *testing.T) {
	mr := miniredis.RunT(t)
	up := echo(t)
	yml := basePrefix + `
redis: {address: "{{redis}}"}
clusters:
  - name: up
    endpoints: [{address: "{{up}}"}]
routes:
  - name: limited
    match: {pathPrefix: /}
    rateLimit:
      - {key: [route], limit: 4, period: 1m, backend: redis}
    backends: [{cluster: up}]
`
	vars := map[string]string{"up": addr(up), "redis": mr.Addr()}
	_, a := newGateway(t, yml, vars)
	_, b := newGateway(t, yml, vars)
	ok := 0
	for i := 0; i < 6; i++ {
		srv := a
		if i%2 == 1 {
			srv = b
		}
		if resp, _ := get(t, srv.URL+"/", nil); resp.StatusCode == 200 {
			ok++
		}
	}
	if ok != 4 {
		t.Fatalf("allowed %d requests across replicas, want 4", ok)
	}

	// Redis outage falls back to local limiting rather than failing requests.
	mr.Close()
	if resp, _ := get(t, a.URL+"/", nil); resp.StatusCode != 200 {
		t.Fatalf("redis down: status %d, want local fallback 200", resp.StatusCode)
	}
}

func TestMasking(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/gzip" {
			w.Header().Set("Content-Encoding", "gzip")
			gz := gzip.NewWriter(w)
			_, _ = gz.Write([]byte(`{"ssn":"123-45-6789"}`))
			gz.Close()
			return
		}
		_, _ = io.WriteString(w, `{"user":{"name":"Ana","ssn":"123-45-6789","card":"4111111111111111",
			"note":"mail me at ana@example.com"},"items":[{"secret":"a"},{"secret":"b"}]}`)
	}))
	defer up.Close()
	_, gw := newGateway(t, basePrefix+`
clusters:
  - name: up
    endpoints: [{address: "{{up}}"}]
routes:
  - name: users
    match: {pathPrefix: /}
    masking:
      rules:
        - {selector: "$..ssn", strategy: redact}
        - {selector: "$.user.card", strategy: partial, keepLast: 4}
        - {selector: "$.items[*].secret", strategy: drop}
        - {detector: email, strategy: hash}
    backends: [{cluster: up}]
`, map[string]string{"up": addr(up)})

	resp, body := get(t, gw.URL+"/u", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	user := body["user"].(map[string]any)
	if user["ssn"] != "***" || user["card"] != "************1111" || user["name"] != "Ana" {
		t.Errorf("masked user = %v", user)
	}
	if note := user["note"].(string); strings.Contains(note, "ana@example.com") || !strings.Contains(note, "hmac:") {
		t.Errorf("email not hashed: %q", note)
	}
	for _, it := range body["items"].([]any) {
		if _, ok := it.(map[string]any)["secret"]; ok {
			t.Errorf("secret not dropped: %v", it)
		}
	}

	// A body the gateway cannot inspect is blocked, not leaked.
	resp, _ = get(t, gw.URL+"/gzip", nil)
	if resp.StatusCode != 502 {
		t.Errorf("unmaskable response: status %d, want 502", resp.StatusCode)
	}
}

func TestCompressionAppliesAfterMasking(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ssn":"123-45-6789","filler":"`+strings.Repeat("abc", 2000)+`"}`)
	}))
	defer up.Close()
	_, gw := newGateway(t, basePrefix+`
clusters:
  - name: up
    endpoints: [{address: "{{up}}"}]
routes:
  - name: r
    match: {pathPrefix: /}
    masking: {rules: [{selector: "$.ssn", strategy: redact}]}
    compression: {algorithms: [gzip]}
    backends: [{cluster: up}]
`, map[string]string{"up": addr(up)})

	req, _ := http.NewRequest(http.MethodGet, gw.URL+"/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("not compressed: %v", resp.Header)
	}
	zr, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	plain, _ := io.ReadAll(zr)
	if !strings.Contains(string(plain), `"ssn":"***"`) {
		t.Errorf("body was compressed before masking: %.80s", plain)
	}
}

func TestRetriesFailOverToHealthyEndpoint(t *testing.T) {
	var badHits atomic.Int32
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		badHits.Add(1)
		w.WriteHeader(503)
	}))
	defer bad.Close()
	good := echo(t)
	_, gw := newGateway(t, basePrefix+`
clusters:
  - name: up
    endpoints: [{address: "{{bad}}"}, {address: "{{good}}"}]
    loadBalancer: {policy: round_robin}
    outlierDetection: {consecutiveFailures: 2, baseEjection: 1m, maxEjectionPercent: 50}
routes:
  - name: r
    match: {pathPrefix: /}
    retries: {attempts: 1, on: ["503"], backoff: 1ms}
    backends: [{cluster: up}]
`, map[string]string{"bad": addr(bad), "good": addr(good)})
	for i := 0; i < 10; i++ {
		if resp, _ := get(t, gw.URL+"/", nil); resp.StatusCode != 200 {
			t.Fatalf("request %d: status %d", i, resp.StatusCode)
		}
	}
	if n := badHits.Load(); n > 2 {
		t.Errorf("bad endpoint hit %d times; outlier detection should eject it after 2", n)
	}
}

func TestTimeoutAndBodyLimit(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer slow.Close()
	_, gw := newGateway(t, basePrefix+`
clusters:
  - name: slow
    endpoints: [{address: "{{slow}}"}]
routes:
  - name: r
    match: {pathPrefix: /}
    timeout: 100ms
    maxBodyBytes: 16
    backends: [{cluster: slow}]
`, map[string]string{"slow": addr(slow)})
	if resp, _ := get(t, gw.URL+"/", nil); resp.StatusCode != 504 {
		t.Errorf("timeout: status %d, want 504", resp.StatusCode)
	}
	resp, err := http.Post(gw.URL+"/", "text/plain", strings.NewReader(strings.Repeat("x", 64)))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 413 {
		t.Errorf("body limit: status %d, want 413", resp.StatusCode)
	}
}

func TestGraphQLGuard(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"echo": string(body)}})
	}))
	defer up.Close()
	_, gw := newGateway(t, basePrefix+`
clusters:
  - name: graph
    endpoints: [{address: "{{up}}"}]
routes:
  - name: graphql
    type: graphql
    match: {path: /graphql}
    graphql: {maxDepth: 3, maxCost: 50, blockIntrospection: true}
    backends: [{cluster: graph}]
`, map[string]string{"up": addr(up)})

	post := func(q string, vars map[string]any) (int, map[string]any) {
		b, _ := json.Marshal(map[string]any{"query": q, "variables": vars})
		resp, err := http.Post(gw.URL+"/graphql", "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	code, out := post(`query Me { me { name friends(first: 5) { name } } }`, nil)
	if code != 200 || !strings.Contains(fmt.Sprint(out["data"]), "Me") {
		t.Fatalf("valid query: %d %v (body must still reach upstream)", code, out)
	}
	code, out = post(`{ a { b { c { d } } } }`, nil)
	if code != 400 || !strings.Contains(fmt.Sprint(out["errors"]), "QUERY_TOO_COMPLEX") {
		t.Errorf("deep query: %d %v", code, out)
	}
	code, out = post(`query($n: Int) { users(first: $n) { name email } }`, map[string]any{"n": 100})
	if code != 400 || !strings.Contains(fmt.Sprint(out["errors"]), "cost") {
		t.Errorf("costly query: %d %v", code, out)
	}
	if code, _ = post(`{ __schema { types { name } } }`, nil); code != 403 {
		t.Errorf("introspection: status %d, want 403", code)
	}
	if code, _ = post(`{ broken `, nil); code != 400 {
		t.Errorf("parse error: status %d, want 400", code)
	}
}

func TestHotReloadSwapsRoutes(t *testing.T) {
	up := echo(t)
	yml := basePrefix + `
clusters:
  - name: up
    endpoints: [{address: "` + addr(up) + `"}]
routes:
  - name: r
    match: {path: /%s}
    backends: [{cluster: up}]
`
	g, srv := newGateway(t, fmt.Sprintf(yml, "old"), nil)
	before := g.Snapshot().Clusters["up"]
	cfg, v, err := config.Parse([]byte(fmt.Sprintf(yml, "new")))
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Apply(cfg, v); err != nil {
		t.Fatal(err)
	}
	if resp, _ := get(t, srv.URL+"/new", nil); resp.StatusCode != 200 {
		t.Errorf("new route: %d", resp.StatusCode)
	}
	if resp, _ := get(t, srv.URL+"/old", nil); resp.StatusCode != 404 {
		t.Errorf("old route still served: %d", resp.StatusCode)
	}
	if g.Snapshot().Clusters["up"] != before {
		t.Error("unchanged cluster was rebuilt; health and pool state would be lost")
	}
}

func TestTracingPropagatesContextAndRecordsSpans(t *testing.T) {
	var upstreamTraceparent atomic.Value
	attempt := atomic.Int32{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamTraceparent.Store(r.Header.Get("Traceparent"))
		if attempt.Add(1) == 1 {
			w.WriteHeader(503) // force one retry → two client spans
		}
	}))
	defer up.Close()

	yml := strings.ReplaceAll(basePrefix+`
clusters:
  - name: up
    endpoints: [{address: "{{up}}"}]
routes:
  - name: traced
    match: {pathPrefix: /}
    retries: {attempts: 1, on: ["503"], backoff: 1ms}
    backends: [{cluster: up}]
`, "{{up}}", addr(up))
	cfg, v, err := config.Parse([]byte(yml))
	if err != nil {
		t.Fatal(err)
	}
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	gw := gateway.New(telemetry.NewMetrics(), nil)
	gw.SetTracerProvider(tp)
	if err := gw.Apply(cfg, v); err != nil {
		t.Fatal(err)
	}
	defer gw.Close()
	srv := httptest.NewServer(gw.Handler("public"))
	defer srv.Close()

	const parentTrace = "4bf92f3577b34da6a3ce929d0e0e4736"
	resp, _ := get(t, srv.URL+"/x", map[string]string{"Traceparent": "00-" + parentTrace + "-00f067aa0ba902b7-01"})
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}

	spans := rec.Ended()
	var server sdktrace.ReadOnlySpan
	var clients []sdktrace.ReadOnlySpan
	for _, s := range spans {
		switch s.SpanKind() {
		case trace.SpanKindServer:
			server = s
		case trace.SpanKindClient:
			clients = append(clients, s)
		}
	}
	if server == nil || server.Name() != "GET traced" || server.SpanContext().TraceID().String() != parentTrace {
		t.Fatalf("server span missing or not joined to the inbound trace: %v", spans)
	}
	if len(clients) != 2 {
		t.Fatalf("want one client span per attempt (2), got %d", len(clients))
	}
	for _, c := range clients {
		if c.Parent().SpanID() != server.SpanContext().SpanID() {
			t.Error("client span is not a child of the server span")
		}
	}
	gotTP := upstreamTraceparent.Load().(string)
	if !strings.Contains(gotTP, parentTrace) || strings.Contains(gotTP, "00f067aa0ba902b7") {
		t.Errorf("upstream traceparent %q must carry the trace id with the gateway's client span id", gotTP)
	}

	// Tracing can be retargeted and turned off while serving (config reload).
	rec2 := tracetest.NewSpanRecorder()
	gw.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec2)))
	get(t, srv.URL+"/x", nil)
	if len(rec2.Ended()) == 0 || len(rec.Ended()) != len(spans) {
		t.Errorf("after switching providers: new %d spans, old %d (was %d)", len(rec2.Ended()), len(rec.Ended()), len(spans))
	}
	gw.SetTracerProvider(nil)
	before := len(rec2.Ended())
	get(t, srv.URL+"/x", map[string]string{"Traceparent": "00-" + parentTrace + "-00f067aa0ba902b7-01"})
	if len(rec2.Ended()) != before {
		t.Error("spans recorded after tracing was disabled")
	}
	if got := upstreamTraceparent.Load().(string); !strings.Contains(got, "00f067aa0ba902b7") {
		t.Errorf("with tracing off, traceparent must pass through untouched, got %q", got)
	}
}

// --- gRPC ---

func startGRPCUpstream(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := grpc.NewServer()
	hs := health.NewServer()
	hs.SetServingStatus("orders.v1.Orders", healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(s, hs)
	go s.Serve(ln)
	t.Cleanup(s.Stop)
	return ln.Addr().String()
}

func startH2CGateway(t *testing.T, yml string) string {
	t.Helper()
	cfg, v, err := config.Parse([]byte(yml))
	if err != nil {
		t.Fatal(err)
	}
	gw := gateway.New(telemetry.NewMetrics(), nil)
	if err := gw.Apply(cfg, v); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(gw.Close)
	srv, err := gw.NewServer(cfg.Listeners[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	go srv.Serve()
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	return srv.Addr()
}

// grpcWebCall performs a unary gRPC-Web call over HTTP/1.1 and returns the
// decoded data message, trailer block and response headers.
func grpcWebCall(t *testing.T, url string, text bool, msg proto.Message) ([]byte, string, http.Header) {
	t.Helper()
	payload, _ := proto.Marshal(msg)
	frame := append([]byte{0, 0, 0, 0, 0}, payload...)
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(payload)))
	ct := "application/grpc-web+proto"
	body := frame
	if text {
		ct = "application/grpc-web-text"
		body = []byte(base64.StdEncoding.EncodeToString(frame))
	}
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Content-Type", ct)
	req.Header.Set("X-Grpc-Web", "1")
	resp, err := (&http.Transport{}).RoundTrip(req) // forces HTTP/1.1, like a browser fetch
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if text {
		var dec []byte
		for len(raw) >= 4 { // concatenated, independently padded segments
			chunk := make([]byte, 3)
			n, err := base64.StdEncoding.Decode(chunk, raw[:4])
			if err != nil {
				t.Fatalf("bad base64 in response: %v", err)
			}
			dec, raw = append(dec, chunk[:n]...), raw[4:]
		}
		raw = dec
	}
	var data []byte
	var trailers string
	for len(raw) >= 5 {
		flag, n := raw[0], binary.BigEndian.Uint32(raw[1:5])
		chunk := raw[5 : 5+n]
		if flag&0x80 != 0 {
			trailers = string(chunk)
		} else {
			data = chunk
		}
		raw = raw[5+n:]
	}
	return data, trailers, resp.Header
}

func TestGRPCWebTranslation(t *testing.T) {
	upAddr := startGRPCUpstream(t)
	_, gw := newGateway(t, basePrefix+`
clusters:
  - name: health
    protocol: h2c
    endpoints: [{address: "{{up}}"}]
routes:
  - name: health-web
    match: {grpcService: grpc.health.v1.Health}
    grpcWeb: true
    backends: [{cluster: health}]
`, map[string]string{"up": upAddr})
	url := gw.URL + "/grpc.health.v1.Health/Check"

	for _, text := range []bool{false, true} {
		data, trailers, hdr := grpcWebCall(t, url, text, &healthpb.HealthCheckRequest{Service: "orders.v1.Orders"})
		var out healthpb.HealthCheckResponse
		if err := proto.Unmarshal(data, &out); err != nil || out.GetStatus() != healthpb.HealthCheckResponse_SERVING {
			t.Fatalf("text=%v: response %v %v (trailers %q)", text, &out, err, trailers)
		}
		if !strings.Contains(trailers, "grpc-status: 0\r\n") {
			t.Errorf("text=%v: trailer frame = %q", text, trailers)
		}
		wantCT := "application/grpc-web+proto"
		if text {
			wantCT = "application/grpc-web-text+proto"
		}
		if got := hdr.Get("Content-Type"); got != wantCT {
			t.Errorf("text=%v: content-type %q, want %q", text, got, wantCT)
		}
	}

	// Upstream errors arrive as Trailers-Only and keep their status in headers.
	_, _, hdr := grpcWebCall(t, url, false, &healthpb.HealthCheckRequest{Service: "missing"})
	if hdr.Get("Grpc-Status") != "5" {
		t.Errorf("NotFound status not propagated: %v", hdr)
	}
}

func TestGRPCWebRequiresHTTP2Cluster(t *testing.T) {
	_, _, err := config.Parse([]byte(basePrefix + `
clusters: [{name: c, endpoints: [{address: "a:1"}]}]
routes: [{name: r, match: {pathPrefix: /}, grpcWeb: true, backends: [{cluster: c}]}]
`))
	if err == nil || !strings.Contains(err.Error(), "grpcWeb requires an HTTP/2 cluster") {
		t.Fatalf("got %v", err)
	}
}

func TestProxyProtocolListener(t *testing.T) {
	up := echo(t)
	gwAddr := startH2CGateway(t, `
apiVersion: prisma/v1
listeners:
  - name: public
    address: "127.0.0.1:0"
    protocols: [http1]
    proxyProtocol: {trustedCIDRs: [127.0.0.0/8]}
clusters:
  - name: up
    endpoints: [{address: "`+addr(up)+`"}]
routes:
  - name: r
    match: {pathPrefix: /}
    backends: [{cluster: up}]
`)
	send := func(raw string) string {
		c, err := net.Dial("tcp", gwAddr)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(3 * time.Second))
		_, _ = io.WriteString(c, raw)
		b, _ := io.ReadAll(c)
		return string(b)
	}
	resp := send("PROXY TCP4 203.0.113.50 10.0.0.1 40000 80\r\nGET /x HTTP/1.1\r\nHost: a\r\nConnection: close\r\n\r\n")
	if !strings.Contains(resp, `"xff":"203.0.113.50"`) {
		t.Errorf("upstream did not see the PROXY client address:\n%s", resp)
	}
	if resp := send("GET /x HTTP/1.1\r\nHost: a\r\nConnection: close\r\n\r\n"); strings.Contains(resp, "200 OK") {
		t.Error("trusted peer without a PROXY header must be rejected")
	}
}

func TestGRPCProxyUnaryStreamingAndErrors(t *testing.T) {
	upAddr := startGRPCUpstream(t)
	gwAddr := startH2CGateway(t, `
apiVersion: prisma/v1
listeners:
  - name: public
    address: "127.0.0.1:0"
    protocols: [h2c]
consumers:
  - name: svc-a
    apiKeys: [grpc-key]
    scopes: [health:read]
clusters:
  - name: health
    protocol: h2c
    endpoints: [{address: "`+upAddr+`"}]
    healthCheck: {type: grpc, interval: 1s}
routes:
  - name: health
    type: grpc
    match: {grpcService: grpc.health.v1.Health}
    authn: {apiKey: {header: x-api-key}}
    authz: {methods: {"/grpc.health.v1.Health/Check": [health:read]}}
    rateLimit:
      - {key: [consumer, grpc_method], limit: 100, period: 1s}
    backends: [{cluster: health}]
`)
	conn, err := grpc.NewClient(gwAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := healthpb.NewHealthClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err = client.Check(ctx, &healthpb.HealthCheckRequest{Service: "orders.v1.Orders"})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("no key: got %v, want Unauthenticated", err)
	}

	authed := metadata.AppendToOutgoingContext(ctx, "x-api-key", "grpc-key")
	resp, err := client.Check(authed, &healthpb.HealthCheckRequest{Service: "orders.v1.Orders"})
	if err != nil || resp.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		t.Fatalf("unary via gateway: %v %v", resp, err)
	}

	// Upstream errors and their trailers pass through unchanged.
	_, err = client.Check(authed, &healthpb.HealthCheckRequest{Service: "unknown"})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("upstream NotFound via gateway: got %v", err)
	}

	// Server streaming.
	stream, err := client.Watch(authed, &healthpb.HealthCheckRequest{Service: "orders.v1.Orders"})
	if err != nil {
		t.Fatal(err)
	}
	msg, err := stream.Recv()
	if err != nil || msg.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		t.Fatalf("stream via gateway: %v %v", msg, err)
	}
}

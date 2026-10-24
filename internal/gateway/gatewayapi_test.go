package gateway_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"prisma/internal/config"
	"prisma/internal/gateway"
	"prisma/internal/gatewayapi"
	"prisma/internal/telemetry"
)

// fakeAPI is a minimal Kubernetes API server: collections (list+watch) and
// status patches.
type fakeAPI struct {
	*httptest.Server
	mu       sync.Mutex
	items    map[string][]map[string]any // collection path -> objects
	watchers map[string][]chan map[string]any
	patches  []string
}

func newFakeAPI(t *testing.T) *fakeAPI {
	f := &fakeAPI{items: map[string][]map[string]any{}, watchers: map[string][]chan map[string]any{}}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeAPI) add(t *testing.T, path, manifest string) {
	t.Helper()
	var obj map[string]any
	if err := yaml.Unmarshal([]byte(manifest), &obj); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.items[path] = append(f.items[path], obj)
	ws := f.watchers[path]
	f.mu.Unlock()
	for _, w := range ws {
		w <- map[string]any{"type": "ADDED", "object": obj}
	}
}

func (f *fakeAPI) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPatch {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.patches = append(f.patches, r.URL.Path+" "+string(body))
		f.mu.Unlock()
		_, _ = io.WriteString(w, "{}")
		return
	}
	path := r.URL.Path
	if r.URL.Query().Get("watch") == "1" {
		ch := make(chan map[string]any, 8)
		f.mu.Lock()
		f.watchers[path] = append(f.watchers[path], ch)
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		http.NewResponseController(w).Flush()
		for {
			select {
			case ev := <-ch:
				_ = json.NewEncoder(w).Encode(ev)
				http.NewResponseController(w).Flush()
			case <-r.Context().Done():
				return
			}
		}
	}
	f.mu.Lock()
	items := f.items[path]
	f.mu.Unlock()
	if items == nil {
		items = []map[string]any{}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"metadata": map[string]any{"resourceVersion": "1"}, "items": items})
}

func (f *fakeAPI) patched(substr string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.patches {
		if strings.Contains(p, substr) {
			return true
		}
	}
	return false
}

func eventually(t *testing.T, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestGatewayAPIController(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, "echo %s host=%s x-gw=%s", r.URL.Path, r.Host, r.Header.Get("X-Gw"))
	}))
	defer up.Close()
	_, upPort, _ := net.SplitHostPort(addr(up))
	_, gwPort, _ := net.SplitHostPort(freeAddr(t))

	api := newFakeAPI(t)
	const gw = "/apis/gateway.networking.k8s.io/v1"
	api.add(t, gw+"/gatewayclasses", `{kind: GatewayClass, metadata: {name: prisma, generation: 1}, spec: {controllerName: prisma.dev/gateway-controller}}`)
	api.add(t, gw+"/gateways", `{kind: Gateway, metadata: {name: web, namespace: default, generation: 1},
	  spec: {gatewayClassName: prisma, listeners: [{name: http, port: `+gwPort+`, protocol: HTTP}]}}`)
	api.add(t, "/api/v1/services", `{kind: Service, metadata: {name: echo, namespace: default}, spec: {ports: [{name: http, port: 80}]}}`)
	api.add(t, "/apis/discovery.k8s.io/v1/namespaces/default/endpointslices", `{kind: EndpointSlice,
	  metadata: {name: echo-abc, namespace: default, labels: {kubernetes.io/service-name: echo}},
	  endpoints: [{addresses: [127.0.0.1], conditions: {ready: true}}], ports: [{name: http, port: `+upPort+`}]}`)
	api.add(t, gw+"/httproutes", `{kind: HTTPRoute, metadata: {name: hello, namespace: default, generation: 3},
	  spec: {parentRefs: [{name: web}], rules: [{matches: [{path: {type: PathPrefix, value: /hello}}],
	    filters: [{type: RequestHeaderModifier, requestHeaderModifier: {set: [{name: X-Gw, value: prisma}]}}],
	    backendRefs: [{name: echo, port: 80}]}]}}`)

	base, v, err := config.Parse([]byte("apiVersion: prisma/v1\ngatewayAPI: {apiServer: \"" + api.URL + "\"}\n"))
	if err != nil {
		t.Fatal(err)
	}
	g := gateway.New(telemetry.NewMetrics(), nil)
	if err := g.Apply(base, v); err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	ls := g.NewListenerSet(time.Second)
	defer ls.Shutdown(context.Background())
	ctrl, err := gatewayapi.NewController(*base.GatewayAPI, base, func(c *config.Config, v string) error {
		if err := g.Apply(c, v); err != nil {
			return err
		}
		return ls.Apply(c.Listeners)
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ctrl.Run(ctx)

	get := func(path string) (int, string) {
		resp, err := http.Get("http://127.0.0.1:" + gwPort + path)
		if err != nil {
			return 0, err.Error()
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	eventually(t, "HTTPRoute to be served", func() bool {
		code, body := get("/hello/world")
		return code == 200 && strings.HasPrefix(body, "echo /hello/world")
	})
	if _, body := get("/hello"); !strings.Contains(body, "x-gw=prisma") || !strings.Contains(body, "host=echo.default.svc") {
		t.Errorf("filters and service host not applied: %q", body)
	}
	defer func() {
		if t.Failed() {
			api.mu.Lock()
			for _, p := range api.patches {
				t.Log(p)
			}
			api.mu.Unlock()
		}
	}()
	eventually(t, "HTTPRoute status", func() bool {
		return api.patched(`/httproutes/hello/status {"status":{"parents":[{"conditions":[{"type":"Accepted","status":"True","reason":"Accepted"`) &&
			api.patched(`"observedGeneration":3`) && !api.patched(`null`)
	})
	eventually(t, "Gateway and GatewayClass status", func() bool {
		return api.patched("/gateways/web/status") && api.patched("/gatewayclasses/prisma/status")
	})

	// A route added while running is picked up through the watch.
	api.add(t, gw+"/httproutes", `{kind: HTTPRoute, metadata: {name: bye, namespace: default},
	  spec: {parentRefs: [{name: web}], rules: [{matches: [{path: {type: Exact, value: /bye}}], backendRefs: [{name: echo, port: 80}]}]}}`)
	eventually(t, "new HTTPRoute", func() bool {
		code, body := get("/bye")
		return code == 200 && strings.HasPrefix(body, "echo /bye")
	})
	if code, _ := get("/nothing"); code != 404 {
		t.Errorf("unrouted path: %d", code)
	}

	// Status writes settle: no endless patch loop.
	time.Sleep(600 * time.Millisecond)
	api.mu.Lock()
	n := len(api.patches)
	api.mu.Unlock()
	time.Sleep(600 * time.Millisecond)
	api.mu.Lock()
	m := len(api.patches)
	api.mu.Unlock()
	if m != n {
		t.Errorf("status kept being patched (%d -> %d)", n, m)
	}
}

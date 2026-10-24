package upstream

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"prisma/internal/config"
	"prisma/internal/telemetry"
)

func slice(name string, eps ...string) map[string]any {
	var endpoints []any
	for _, e := range eps {
		addr, state, _ := strings.Cut(e, "/")
		cond := map[string]any{}
		switch state {
		case "notready":
			cond["ready"] = false
		case "terminating":
			cond["ready"], cond["terminating"] = true, true
		}
		endpoints = append(endpoints, map[string]any{"addresses": []string{addr}, "conditions": cond})
	}
	return map[string]any{
		"metadata":  map[string]any{"name": name, "resourceVersion": "1"},
		"endpoints": endpoints,
		"ports":     []any{map[string]any{"name": "http", "port": 8080}, map[string]any{"name": "grpc", "port": 9090}},
	}
}

type fakeK8s struct {
	*httptest.Server
	events chan map[string]any
	lists  atomic.Int32
	bad    atomic.Value
}

func newFakeK8s(t *testing.T, listItems func(n int32) []any) *fakeK8s {
	f := &fakeK8s{events: make(chan map[string]any, 16)}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/apis/discovery.k8s.io/v1/namespaces/shop/endpointslices" ||
			r.URL.Query().Get("labelSelector") != "kubernetes.io/service-name=orders" ||
			r.Header.Get("Authorization") != "Bearer tok-1" {
			f.bad.Store(r.URL.String() + " " + r.Header.Get("Authorization"))
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if r.URL.Query().Get("watch") != "1" {
			n := f.lists.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"metadata": map[string]any{"resourceVersion": fmt.Sprint(n * 100)}, "items": listItems(n)})
			return
		}
		w.WriteHeader(http.StatusOK)
		http.NewResponseController(w).Flush()
		for {
			select {
			case ev := <-f.events:
				_ = json.NewEncoder(w).Encode(ev)
				http.NewResponseController(w).Flush()
				if ev["type"] == "ERROR" {
					return
				}
			case <-r.Context().Done():
				return
			}
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func waitFor(t *testing.T, c *Cluster, want ...string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if got := strings.Join(addrs(c), ","); got == strings.Join(want, ",") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("endpoints = %v, want %v", addrs(c), want)
}

func TestKubernetesEndpointSliceDiscovery(t *testing.T) {
	f := newFakeK8s(t, func(n int32) []any {
		if n == 1 {
			return []any{slice("orders-a", "10.1.0.1", "10.1.0.2/notready", "10.1.0.3/terminating")}
		}
		return []any{slice("orders-c", "10.3.0.1")} // state after the watch expired
	})
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("tok-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Cluster{Name: "orders", Protocol: "http1", LoadBalancer: config.LoadBalancer{Policy: "round_robin"},
		Discovery: &config.Discovery{Type: "kubernetes", Kubernetes: &config.KubernetesDiscovery{
			Service: "orders", Namespace: "shop", PortName: "http", APIServer: f.URL, TokenFile: token,
		}}}
	c, err := NewCluster(cfg, telemetry.NewMetrics())
	if err != nil {
		t.Fatal(err)
	}
	if c.DefaultHost != "orders.shop.svc" {
		t.Errorf("DefaultHost = %q", c.DefaultHost)
	}
	c.Start()
	defer c.Close(0)

	waitFor(t, c, "10.1.0.1:8080") // not-ready and terminating excluded; named port
	f.events <- map[string]any{"type": "MODIFIED", "object": slice("orders-a", "10.1.0.1", "10.1.0.2")}
	waitFor(t, c, "10.1.0.1:8080", "10.1.0.2:8080")
	f.events <- map[string]any{"type": "ADDED", "object": slice("orders-b", "10.2.0.1")}
	waitFor(t, c, "10.1.0.1:8080", "10.1.0.2:8080", "10.2.0.1:8080")
	f.events <- map[string]any{"type": "DELETED", "object": slice("orders-a")}
	waitFor(t, c, "10.2.0.1:8080")
	f.events <- map[string]any{"type": "ERROR", "object": map[string]any{"code": 410, "reason": "Expired"}}
	waitFor(t, c, "10.3.0.1:8080") // relisted
	if v := f.bad.Load(); v != nil {
		t.Errorf("unexpected API request: %v", v)
	}
}

func TestKubernetesDiscoveryRequiresNamespaceOutsideCluster(t *testing.T) {
	cfg := config.Cluster{Name: "x", Protocol: "http1", LoadBalancer: config.LoadBalancer{Policy: "round_robin"},
		Discovery: &config.Discovery{Type: "kubernetes", Kubernetes: &config.KubernetesDiscovery{Service: "s", APIServer: "http://127.0.0.1:1"}}}
	if _, err := NewCluster(cfg, telemetry.NewMetrics()); err == nil || !strings.Contains(err.Error(), "namespace") {
		t.Fatalf("want a clear namespace error, got %v", err)
	}
}

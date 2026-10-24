package upstream

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"prisma/internal/config"
)

// fakeConsul serves /v1/health/service/<name> with blocking queries: a
// request whose index equals the current one waits for the next change.
type fakeConsul struct {
	mu      sync.Mutex
	index   uint64
	entries []map[string]any
	fail    bool
	changed chan struct{}
	seen    []string // query strings
	tokens  []string
}

func newFakeConsul(t *testing.T) (*fakeConsul, *httptest.Server) {
	f := &fakeConsul{index: 10, changed: make(chan struct{})}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/health/service/orders" {
			http.NotFound(w, r)
			return
		}
		f.mu.Lock()
		f.seen = append(f.seen, r.URL.RawQuery)
		f.tokens = append(f.tokens, r.Header.Get("X-Consul-Token"))
		idx, _ := strconv.ParseUint(r.URL.Query().Get("index"), 10, 64)
		ch := f.changed
		cur := f.index
		f.mu.Unlock()
		if idx != 0 && idx == cur {
			select {
			case <-ch:
			case <-time.After(2 * time.Second):
			case <-r.Context().Done():
				return
			}
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.fail {
			http.Error(w, "No cluster leader", http.StatusInternalServerError)
			return
		}
		w.Header().Set("X-Consul-Index", strconv.FormatUint(f.index, 10))
		_ = json.NewEncoder(w).Encode(f.entries)
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeConsul) set(fail bool, entries ...map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail, f.entries = fail, entries
	f.index++
	close(f.changed)
	f.changed = make(chan struct{})
}

func instance(addr string, port int, weight int, checks ...string) map[string]any {
	var cs []map[string]string
	for _, c := range checks {
		cs = append(cs, map[string]string{"Status": c})
	}
	return map[string]any{
		"Node":    map[string]string{"Address": "10.0.0.99"},
		"Service": map[string]any{"Address": addr, "Port": port, "Weights": map[string]int{"Passing": weight, "Warning": 1}},
		"Checks":  cs,
	}
}

func TestConsulDiscovery(t *testing.T) {
	f, srv := newFakeConsul(t)
	f.set(false,
		instance("10.0.0.1", 8080, 3, "passing", "passing"),
		instance("", 8080, 1, "passing"), // falls back to the node address
		instance("10.0.0.3", 8080, 1, "passing", "critical"),
		instance("10.0.0.4", 8080, 1, "warning"),
	)
	c := newDiscovered(t, &config.Discovery{Type: "consul", Interval: time.Second, Consul: &config.ConsulDiscovery{
		Address: srv.URL, Service: "orders", Tag: "v2", Datacenter: "eu1", Token: "secret",
	}}, nil)
	c.Start()
	defer c.Close(0)

	// The first query runs before Start returns.
	if got := addrs(c); len(got) != 2 || got[0] != "10.0.0.1:8080" || got[1] != "10.0.0.99:8080" {
		t.Fatalf("initial endpoints = %v", got)
	}
	if c.Endpoints()[0].Weight != 3 {
		t.Errorf("weight = %d, want the service's passing weight", c.Endpoints()[0].Weight)
	}
	if c.DefaultHost != "orders.service.consul" {
		t.Errorf("default host = %q", c.DefaultHost)
	}

	// A change is picked up by the blocking query right away.
	f.set(false, instance("10.0.0.5", 9090, 1, "passing"))
	waitFor(t, c, "10.0.0.5:9090")

	// Consul errors keep the last known endpoints.
	f.set(true)
	time.Sleep(300 * time.Millisecond)
	if got := addrs(c); len(got) != 1 || got[0] != "10.0.0.5:9090" {
		t.Fatalf("endpoints after a Consul error = %v", got)
	}
	f.set(false, instance("10.0.0.6", 9090, 1, "passing"))
	waitFor(t, c, "10.0.0.6:9090")

	f.mu.Lock()
	defer f.mu.Unlock()
	q := f.seen[len(f.seen)-1]
	for _, want := range []string{"passing=true", "tag=v2", "dc=eu1", "wait=5m0s", "index="} {
		if !strings.Contains(q, want) {
			t.Errorf("query %q lacks %s", q, want)
		}
	}
	if f.tokens[0] != "secret" {
		t.Errorf("token header = %q", f.tokens[0])
	}
}

func TestConsulAllowWarning(t *testing.T) {
	f, srv := newFakeConsul(t)
	f.set(false, instance("10.0.0.1", 80, 5, "passing"), instance("10.0.0.2", 80, 5, "warning"), instance("10.0.0.3", 80, 5, "maintenance"))
	c := newDiscovered(t, &config.Discovery{Type: "consul", Interval: time.Second,
		Consul: &config.ConsulDiscovery{Address: srv.URL, Service: "orders", AllowWarning: true}}, nil)
	c.Start()
	defer c.Close(0)
	eps := c.Endpoints()
	if len(eps) != 2 || eps[1].Address != "10.0.0.2:80" || eps[1].Weight != 1 {
		t.Fatalf("endpoints = %v", addrs(c))
	}
	if strings.Contains(f.seen[0], "passing=true") {
		t.Errorf("allowWarning must not filter on passing: %q", f.seen[0])
	}
}

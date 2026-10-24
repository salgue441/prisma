package upstream

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"prisma/internal/config"
	"prisma/internal/telemetry"
)

// testCluster builds a cluster around prepared endpoints without a transport.
func testCluster(cfg config.Cluster, eps []*Endpoint) *Cluster {
	c := &Cluster{Name: "t", Config: cfg, metrics: telemetry.NewMetrics(), checks: map[*Endpoint]context.CancelFunc{}}
	c.set.Store(&endpointSet{endpoints: eps, balancer: newBalancer(cfg.LoadBalancer, eps)})
	return c
}

type fakeResolver struct {
	mu  sync.Mutex
	ips []string
	srv []*net.SRV
	err error
}

func (f *fakeResolver) set(err error, ips ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ips, f.err = ips, err
}

func (f *fakeResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	var out []netip.Addr
	for _, s := range f.ips {
		out = append(out, netip.MustParseAddr(s))
	}
	return out, nil
}

func (f *fakeResolver) LookupSRV(context.Context, string, string, string) (string, []*net.SRV, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return "", f.srv, f.err
}

func newDiscovered(t *testing.T, d *config.Discovery, r Resolver) *Cluster {
	t.Helper()
	cfg := config.Cluster{Name: "svc", Protocol: "http1", Discovery: d, LoadBalancer: config.LoadBalancer{Policy: "round_robin"},
		HealthCheck: &config.HealthCheck{Type: "tcp", Interval: time.Hour, Timeout: time.Second, HealthyThreshold: 1, UnhealthyThreshold: 1}}
	c, err := NewCluster(cfg, telemetry.NewMetrics())
	if err != nil {
		t.Fatal(err)
	}
	c.resolver = r
	return c
}

func addrs(c *Cluster) []string {
	var out []string
	for _, e := range c.Endpoints() {
		out = append(out, e.Address)
	}
	return out
}

func TestDNSDiscoveryUpdatesAndKeepsState(t *testing.T) {
	r := &fakeResolver{}
	r.set(nil, "10.0.0.2", "10.0.0.1", "10.0.0.1")
	c := newDiscovered(t, &config.Discovery{Type: "dns", Host: "orders.svc", Port: 8080, Interval: time.Hour}, r)
	if c.DefaultHost != "orders.svc:8080" {
		t.Errorf("DefaultHost = %q", c.DefaultHost)
	}
	c.Start()
	defer c.Close(0)
	if got := addrs(c); len(got) != 2 || got[0] != "10.0.0.1:8080" || got[1] != "10.0.0.2:8080" {
		t.Fatalf("initial endpoints %v (want sorted and deduplicated)", got)
	}
	kept := c.Endpoints()[1]
	kept.ejections.Store(3)

	r.set(nil, "10.0.0.2", "10.0.0.3")
	c.refresh(context.Background())
	if got := addrs(c); len(got) != 2 || got[0] != "10.0.0.2:8080" || got[1] != "10.0.0.3:8080" {
		t.Fatalf("after scale event: %v", got)
	}
	if c.Endpoints()[0] != kept || kept.ejections.Load() != 3 {
		t.Error("surviving endpoint lost its identity/state")
	}
	c.mu.Lock()
	running := len(c.checks)
	c.mu.Unlock()
	if running != 2 {
		t.Errorf("%d health checks running, want 2 (removed endpoint's check must stop)", running)
	}

	// DNS failures keep the last known endpoints.
	r.set(errors.New("SERVFAIL"))
	c.refresh(context.Background())
	if len(c.Endpoints()) != 2 {
		t.Errorf("lookup error emptied the cluster: %v", addrs(c))
	}
	// A successful empty answer is applied.
	r.set(nil)
	c.refresh(context.Background())
	if len(c.Endpoints()) != 0 || c.pick(nil, nil) != nil {
		t.Errorf("scaled-to-zero answer not applied: %v", addrs(c))
	}
}

func TestSRVDiscovery(t *testing.T) {
	r := &fakeResolver{srv: []*net.SRV{
		{Target: "b.example.", Port: 9000, Weight: 0},
		{Target: "a.example.", Port: 9001, Weight: 5},
	}}
	c := newDiscovered(t, &config.Discovery{Type: "srv", Host: "_grpc._tcp.svc", Interval: time.Hour}, r)
	c.Start()
	defer c.Close(0)
	eps := c.Endpoints()
	if len(eps) != 2 || eps[0].Address != "a.example:9001" || eps[0].Weight != 5 || eps[1].Weight != 1 {
		t.Fatalf("srv endpoints: %+v %+v", eps[0], eps[1])
	}
}

func TestDiscoveryRebuildsRingHash(t *testing.T) {
	r := &fakeResolver{}
	r.set(nil, "10.0.0.1")
	c := newDiscovered(t, &config.Discovery{Type: "dns", Host: "h", Port: 1, Interval: time.Hour}, r)
	c.Config.LoadBalancer = config.LoadBalancer{Policy: "ring_hash", HashOn: "ip"}
	c.Start()
	defer c.Close(0)
	r.set(nil, "10.0.0.1", "10.0.0.2")
	c.refresh(context.Background())
	rh := c.set.Load().balancer.(*ringHash)
	owners := map[string]bool{}
	for _, o := range rh.owners {
		owners[o.Address] = true
	}
	if !owners["10.0.0.2:1"] {
		t.Error("ring was not rebuilt with the new endpoint")
	}
}

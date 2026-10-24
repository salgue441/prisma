package upstream

import (
	"net/http/httptest"
	"testing"
	"time"

	"prisma/internal/config"
)

func endpoints(weights ...int) []*Endpoint {
	var eps []*Endpoint
	for i, w := range weights {
		e := &Endpoint{Address: string(rune('a' + i)), Weight: w}
		e.healthy.Store(true)
		eps = append(eps, e)
	}
	return eps
}

func TestSmoothWeightedRoundRobin(t *testing.T) {
	eps := endpoints(5, 1, 1)
	b := &wrr{}
	var seq string
	for i := 0; i < 7; i++ {
		seq += b.Pick(nil, eps).Address
	}
	// nginx's smooth WRR interleaves instead of sending 5 in a row.
	if seq != "aabacaa" {
		t.Errorf("sequence %q, want aabacaa", seq)
	}
}

func TestLeastConnAndP2CAvoidLoadedEndpoints(t *testing.T) {
	eps := endpoints(1, 1)
	eps[0].inflight.Store(10)
	for i := 0; i < 20; i++ {
		if got := (leastConn{}).Pick(nil, eps); got != eps[1] {
			t.Fatal("least_conn picked the busy endpoint")
		}
	}
	eps[0].ewmaNanos.Store(int64(100 * time.Millisecond))
	eps[1].ewmaNanos.Store(int64(time.Millisecond))
	for i := 0; i < 20; i++ {
		if got := (p2c{}).Pick(nil, eps); got != eps[1] {
			t.Fatal("p2c picked the slow endpoint")
		}
	}
}

func TestRingHashIsStableAndRemapsMinimally(t *testing.T) {
	eps := endpoints(1, 1, 1, 1)
	rh := newRingHash(eps, "header:X-User")
	owner := map[string]*Endpoint{}
	for i := 0; i < 200; i++ {
		r := httptest.NewRequest("GET", "/", nil)
		key := string(rune('A'+i%26)) + string(rune('a'+i/26))
		r.Header.Set("X-User", key)
		owner[key] = rh.Pick(r, eps)
		if rh.Pick(r, eps) != owner[key] {
			t.Fatal("ring hash is not deterministic")
		}
	}
	// Remove one endpoint: only its keys may move.
	avail := eps[1:]
	for key, o := range owner {
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("X-User", key)
		got := rh.Pick(r, avail)
		if o != eps[0] && got != o {
			t.Fatalf("key %s moved from %s to %s although its endpoint stayed", key, o.Address, got.Address)
		}
	}
}

func TestBreakerOpensAndRecovers(t *testing.T) {
	b := newBreaker(config.CircuitBreaker{ErrorRatio: 0.5, Window: time.Minute, MinRequests: 4, OpenDuration: 20 * time.Millisecond}, nil)
	for i := 0; i < 4; i++ {
		done, err := b.Allow()
		if err != nil {
			t.Fatalf("closed breaker rejected request %d", i)
		}
		done(i%2 == 0)
	}
	if _, err := b.Allow(); err == nil {
		t.Fatal("breaker should be open at 50% errors")
	}
	time.Sleep(25 * time.Millisecond)
	probe, err := b.Allow()
	if err != nil {
		t.Fatal("breaker should admit a half-open probe")
	}
	if _, err := b.Allow(); err == nil {
		t.Fatal("only one probe may run while half-open")
	}
	probe(true)
	if done, err := b.Allow(); err != nil {
		t.Fatal("breaker should close after a successful probe")
	} else {
		done(true)
	}
}

func TestPickRelaxation(t *testing.T) {
	c := testCluster(config.Cluster{}, endpoints(1, 1))
	a, b := c.Endpoints()[0], c.Endpoints()[1]
	if got := c.pick(nil, []*Endpoint{a}); got != b {
		t.Error("should prefer an untried endpoint")
	}
	b.ejectedUntil.Store(time.Now().Add(time.Minute).UnixNano())
	if got := c.pick(nil, []*Endpoint{a}); got != b {
		t.Error("should ignore ejection before re-trying an endpoint")
	}
	if got := c.pick(nil, []*Endpoint{a, b}); got == nil {
		t.Error("should retry an already-tried endpoint rather than fail")
	}
	a.healthy.Store(false)
	b.healthy.Store(false)
	if got := c.pick(nil, nil); got != nil {
		t.Error("must never pick endpoints failing active health checks")
	}
}

func TestOutlierEjectionRespectsMaxPercent(t *testing.T) {
	c := testCluster(
		config.Cluster{OutlierDetection: &config.OutlierDetection{ConsecutiveFailures: 1, BaseEjection: time.Minute, MaxEjectionPercent: 50}},
		endpoints(1, 1, 1),
	)
	for _, e := range c.Endpoints() {
		c.outlier(e, true)
	}
	now := time.Now()
	ejected := 0
	for _, e := range c.Endpoints() {
		if e.Ejected(now) {
			ejected++
		}
	}
	if ejected != 1 {
		t.Errorf("ejected %d of 3 endpoints with a 50%% cap, want 1", ejected)
	}
}

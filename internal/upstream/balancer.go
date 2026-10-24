package upstream

import (
	"hash/fnv"
	"math/rand/v2"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"prisma/internal/config"
	"prisma/internal/pipeline"
)

// Endpoint is one upstream address. Its counters are updated concurrently by
// the proxy path, the health checker and the outlier detector.
type Endpoint struct {
	Address string
	Weight  int

	healthy        atomic.Bool
	ejectedUntil   atomic.Int64 // unix nanos
	ejections      atomic.Int32
	consecFailures atomic.Int32
	inflight       atomic.Int64
	ewmaNanos      atomic.Int64

	swrrCurrent int64 // guarded by wrr.mu
}

func (e *Endpoint) Healthy() bool   { return e.healthy.Load() }
func (e *Endpoint) Inflight() int64 { return e.inflight.Load() }
func (e *Endpoint) Ejected(now time.Time) bool {
	return now.UnixNano() < e.ejectedUntil.Load()
}
func (e *Endpoint) EWMA() time.Duration { return time.Duration(e.ewmaNanos.Load()) }

// observeLatency updates an exponentially weighted moving average (alpha 1/8).
func (e *Endpoint) observeLatency(d time.Duration) {
	for {
		old := e.ewmaNanos.Load()
		next := int64(d)
		if old != 0 {
			next = old + (int64(d)-old)/8
		}
		if e.ewmaNanos.CompareAndSwap(old, next) {
			return
		}
	}
}

// Balancer picks one endpoint from the currently available set (never empty).
type Balancer interface {
	Pick(r *http.Request, eps []*Endpoint) *Endpoint
}

func newBalancer(cfg config.LoadBalancer, all []*Endpoint) Balancer {
	switch cfg.Policy {
	case "weighted_round_robin":
		return &wrr{}
	case "least_conn":
		return leastConn{}
	case "p2c_ewma":
		return p2c{}
	case "ring_hash":
		return newRingHash(all, cfg.HashOn)
	default:
		return &roundRobin{}
	}
}

type roundRobin struct{ n atomic.Uint64 }

func (b *roundRobin) Pick(_ *http.Request, eps []*Endpoint) *Endpoint {
	return eps[(b.n.Add(1)-1)%uint64(len(eps))]
}

// wrr is nginx's smooth weighted round-robin: it interleaves picks instead of
// sending bursts to the heaviest endpoint.
type wrr struct{ mu sync.Mutex }

func (b *wrr) Pick(_ *http.Request, eps []*Endpoint) *Endpoint {
	b.mu.Lock()
	defer b.mu.Unlock()
	var best *Endpoint
	var total int64
	for _, e := range eps {
		e.swrrCurrent += int64(e.Weight)
		total += int64(e.Weight)
		if best == nil || e.swrrCurrent > best.swrrCurrent {
			best = e
		}
	}
	best.swrrCurrent -= total
	return best
}

type leastConn struct{}

func (leastConn) Pick(_ *http.Request, eps []*Endpoint) *Endpoint {
	var best *Endpoint
	ties := 0
	for _, e := range eps {
		switch {
		case best == nil || e.Inflight() < best.Inflight():
			best, ties = e, 1
		case e.Inflight() == best.Inflight():
			ties++
			if rand.IntN(ties) == 0 {
				best = e
			}
		}
	}
	return best
}

// p2c samples two endpoints and keeps the one with the lower
// latency × load score. Unmeasured endpoints score 0 and get tried first.
type p2c struct{}

func (p2c) Pick(_ *http.Request, eps []*Endpoint) *Endpoint {
	if len(eps) == 1 {
		return eps[0]
	}
	i := rand.IntN(len(eps))
	j := rand.IntN(len(eps) - 1)
	if j >= i {
		j++
	}
	a, b := eps[i], eps[j]
	if score(b) < score(a) {
		return b
	}
	return a
}

func score(e *Endpoint) float64 {
	return float64(e.ewmaNanos.Load()) * float64(e.inflight.Load()+1)
}

// ringHash is a consistent-hash ring with weighted virtual nodes. The ring is
// built over all endpoints so that availability changes only remap the keys
// owned by the affected endpoint.
type ringHash struct {
	hashOn string
	points []uint64
	owners []*Endpoint
}

const vnodesPerWeight = 100

func newRingHash(all []*Endpoint, hashOn string) *ringHash {
	type point struct {
		h uint64
		e *Endpoint
	}
	var pts []point
	for _, e := range all {
		for i := 0; i < e.Weight*vnodesPerWeight; i++ {
			pts = append(pts, point{hash64(e.Address + "#" + strconv.Itoa(i)), e})
		}
	}
	slices.SortFunc(pts, func(a, b point) int {
		switch {
		case a.h < b.h:
			return -1
		case a.h > b.h:
			return 1
		}
		return 0
	})
	rh := &ringHash{hashOn: hashOn}
	for _, p := range pts {
		rh.points = append(rh.points, p.h)
		rh.owners = append(rh.owners, p.e)
	}
	return rh
}

func (b *ringHash) Pick(r *http.Request, eps []*Endpoint) *Endpoint {
	key := b.key(r)
	if key == "" || len(b.points) == 0 {
		return eps[rand.IntN(len(eps))]
	}
	h := hash64(key)
	start, _ := slices.BinarySearch(b.points, h)
	for i := 0; i < len(b.points); i++ {
		owner := b.owners[(start+i)%len(b.points)]
		if slices.Contains(eps, owner) {
			return owner
		}
	}
	return eps[0]
}

func (b *ringHash) key(r *http.Request) string {
	switch {
	case strings.HasPrefix(b.hashOn, "header:"):
		return r.Header.Get(strings.TrimPrefix(b.hashOn, "header:"))
	case strings.HasPrefix(b.hashOn, "cookie:"):
		if c, err := r.Cookie(strings.TrimPrefix(b.hashOn, "cookie:")); err == nil {
			return c.Value
		}
		return ""
	default:
		if pc := pipeline.From(r.Context()); pc != nil && pc.ClientIP.IsValid() {
			return pc.ClientIP.String()
		}
		return r.RemoteAddr
	}
}

// hash64 must be stable across processes so every gateway replica maps a key
// to the same endpoint.
func hash64(s string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return h.Sum64()
}

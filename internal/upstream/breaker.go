package upstream

import (
	"sync"
	"sync/atomic"
	"time"

	"prisma/internal/config"
	"prisma/internal/gwerr"
)

type breakerState int

const (
	stateClosed breakerState = iota
	stateOpen
	stateHalfOpen
)

// Breaker is a cluster-level circuit breaker. It opens when the error ratio in
// the current window exceeds the threshold, rejects requests while open, then
// lets a single probe through (half-open) to decide whether to close again.
// It also enforces a max-concurrency limit.
type Breaker struct {
	cfg      config.CircuitBreaker
	onChange func(open bool)
	inflight atomic.Int64

	mu          sync.Mutex
	state       breakerState
	windowStart time.Time
	total       int
	failures    int
	openedAt    time.Time
	probing     bool
}

func newBreaker(cfg config.CircuitBreaker, onChange func(open bool)) *Breaker {
	return &Breaker{cfg: cfg, onChange: onChange, windowStart: time.Now()}
}

// Allow admits a request or returns an error. The returned func must be called
// exactly once with the request outcome.
func (b *Breaker) Allow() (func(success bool), error) {
	if max := int64(b.cfg.MaxConcurrent); max > 0 && b.inflight.Load() >= max {
		return nil, gwerr.Unavailable("upstream overloaded").WithReason("max concurrent requests reached")
	}
	now := time.Now()
	b.mu.Lock()
	probe := false
	switch b.state {
	case stateOpen:
		if now.Sub(b.openedAt) < b.cfg.OpenDuration {
			b.mu.Unlock()
			return nil, gwerr.Unavailable("upstream unavailable").WithReason("circuit open")
		}
		b.state = stateHalfOpen
		fallthrough
	case stateHalfOpen:
		if b.probing {
			b.mu.Unlock()
			return nil, gwerr.Unavailable("upstream unavailable").WithReason("circuit half-open")
		}
		b.probing, probe = true, true
	default:
		if now.Sub(b.windowStart) > b.cfg.Window {
			b.windowStart, b.total, b.failures = now, 0, 0
		}
	}
	b.mu.Unlock()
	b.inflight.Add(1)

	return func(success bool) {
		b.inflight.Add(-1)
		b.mu.Lock()
		changed, open := false, false
		if probe {
			b.probing = false
			if success {
				b.state, b.total, b.failures, b.windowStart = stateClosed, 0, 0, time.Now()
				changed = true
			} else {
				b.state, b.openedAt = stateOpen, time.Now()
				open = true
			}
		} else if b.state == stateClosed {
			b.total++
			if !success {
				b.failures++
			}
			if b.total >= b.cfg.MinRequests && float64(b.failures)/float64(b.total) >= b.cfg.ErrorRatio {
				b.state, b.openedAt = stateOpen, time.Now()
				changed, open = true, true
			}
		}
		b.mu.Unlock()
		if changed && b.onChange != nil {
			b.onChange(open)
		}
	}, nil
}

// Open reports whether the breaker is currently rejecting traffic.
func (b *Breaker) Open() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state != stateClosed
}

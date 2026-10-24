package ratelimit

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestGCRABurstThenSteadyRate(t *testing.T) {
	p := Params{Interval: time.Second, Burst: 3}
	now := time.Unix(1_700_000_000, 0)
	var tat time.Time
	for i := 0; i < 3; i++ {
		d, next := gcra(now, tat, p, 1)
		if !d.Allowed || d.Remaining != 2-i {
			t.Fatalf("burst %d: %+v", i, d)
		}
		tat = next
	}
	d, _ := gcra(now, tat, p, 1)
	if d.Allowed || d.RetryAfter != time.Second {
		t.Fatalf("over burst: %+v", d)
	}
	// One interval later exactly one more request fits.
	d, tat = gcra(now.Add(time.Second), tat, p, 1)
	if !d.Allowed {
		t.Fatalf("after refill: %+v", d)
	}
	if d, _ = gcra(now.Add(time.Second), tat, p, 1); d.Allowed {
		t.Fatalf("second after refill should be limited: %+v", d)
	}
}

func TestGCRACostAboveBurstIsRejected(t *testing.T) {
	p := Params{Interval: time.Millisecond, Burst: 10}
	if d, _ := gcra(time.Now(), time.Time{}, p, 11); d.Allowed {
		t.Fatal("cost larger than burst must never be allowed")
	}
}

func TestStoresAgree(t *testing.T) {
	mr := miniredis.RunT(t)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rc.Close()
	stores := map[string]Store{
		"local": NewLocalStore(),
		"redis": NewRedisStore(rc, "t:", time.Second),
	}
	p := Params{Interval: time.Minute / 5, Burst: 5}
	for name, s := range stores {
		allowed := 0
		var last Decision
		for i := 0; i < 8; i++ {
			d, err := s.Allow(context.Background(), "k", p, 1)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if d.Allowed {
				allowed++
			}
			last = d
		}
		if allowed != 5 {
			t.Errorf("%s: allowed %d, want 5", name, allowed)
		}
		if last.RetryAfter <= 0 || last.RetryAfter > p.Interval {
			t.Errorf("%s: retryAfter %v out of range", name, last.RetryAfter)
		}
	}
}

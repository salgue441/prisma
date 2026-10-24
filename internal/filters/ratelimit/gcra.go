package ratelimit

import (
	"context"
	"hash/maphash"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Params describe one limit in GCRA terms. Interval is the time "cost" of one
// request (period / limit); Burst is how many requests may arrive at once.
type Params struct {
	Interval time.Duration
	Burst    int
}

type Decision struct {
	Allowed    bool
	Remaining  int
	RetryAfter time.Duration
	ResetAfter time.Duration
}

type Store interface {
	Allow(ctx context.Context, key string, p Params, cost int) (Decision, error)
}

// gcra applies the Generic Cell Rate Algorithm given the stored theoretical
// arrival time (tat). It returns the decision and the tat to store.
func gcra(now, tat time.Time, p Params, cost int) (Decision, time.Time) {
	tau := p.Interval * time.Duration(p.Burst)
	if tat.Before(now) {
		tat = now
	}
	newTat := tat.Add(p.Interval * time.Duration(cost))
	allowAt := newTat.Add(-tau)
	if now.Before(allowAt) {
		return Decision{Allowed: false, RetryAfter: allowAt.Sub(now), ResetAfter: tat.Sub(now)}, tat
	}
	remaining := int((tau - newTat.Sub(now)) / p.Interval)
	return Decision{Allowed: true, Remaining: remaining, ResetAfter: newTat.Sub(now)}, newTat
}

// LocalStore keeps per-key state in memory, sharded to reduce lock contention.
// It is shared across config reloads so limits survive a reload.
type LocalStore struct {
	seed   maphash.Seed
	shards [64]localShard
}

type localShard struct {
	mu   sync.Mutex
	tats map[string]time.Time
}

func NewLocalStore() *LocalStore {
	s := &LocalStore{seed: maphash.MakeSeed()}
	for i := range s.shards {
		s.shards[i].tats = map[string]time.Time{}
	}
	go s.sweep()
	return s
}

func (s *LocalStore) Allow(_ context.Context, key string, p Params, cost int) (Decision, error) {
	sh := &s.shards[maphash.String(s.seed, key)%uint64(len(s.shards))]
	now := time.Now()
	sh.mu.Lock()
	d, tat := gcra(now, sh.tats[key], p, cost)
	if d.Allowed {
		sh.tats[key] = tat
	}
	sh.mu.Unlock()
	return d, nil
}

// sweep drops keys whose tat is in the past; they are equivalent to absent keys.
func (s *LocalStore) sweep() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for range t.C {
		now := time.Now()
		for i := range s.shards {
			sh := &s.shards[i]
			sh.mu.Lock()
			for k, tat := range sh.tats {
				if tat.Before(now) {
					delete(sh.tats, k)
				}
			}
			sh.mu.Unlock()
		}
	}
}

// gcraScript is the same algorithm executed atomically in Redis. Times are in
// microseconds; values are formatted with %.0f because Lua's default number
// formatting would lose precision on 16-digit integers.
var gcraScript = redis.NewScript(`
local now = tonumber(ARGV[1])
local interval = tonumber(ARGV[2])
local tau = tonumber(ARGV[3])
local cost = tonumber(ARGV[4])
local tat = tonumber(redis.call("GET", KEYS[1]) or now)
if tat < now then tat = now end
local newTat = tat + interval * cost
local allowAt = newTat - tau
if now < allowAt then
  return {0, 0, allowAt - now, tat - now}
end
local ttl = math.ceil((newTat - now) / 1000)
if ttl < 1 then ttl = 1 end
redis.call("SET", KEYS[1], string.format("%.0f", newTat), "PX", ttl)
return {1, math.floor((tau - (newTat - now)) / interval), 0, newTat - now}
`)

// RedisStore enforces limits cluster-wide across gateway replicas.
type RedisStore struct {
	client  redis.UniversalClient
	prefix  string
	timeout time.Duration
}

func NewRedisStore(client redis.UniversalClient, prefix string, timeout time.Duration) *RedisStore {
	return &RedisStore{client: client, prefix: prefix, timeout: timeout}
}

func (s *RedisStore) Allow(ctx context.Context, key string, p Params, cost int) (Decision, error) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	interval := p.Interval.Microseconds()
	if interval < 1 {
		interval = 1
	}
	res, err := gcraScript.Run(ctx, s.client, []string{s.prefix + "rl:" + key},
		time.Now().UnixMicro(), interval, interval*int64(p.Burst), cost).Int64Slice()
	if err != nil {
		return Decision{}, err
	}
	if len(res) != 4 {
		return Decision{}, errUnexpected(len(res))
	}
	return Decision{
		Allowed:    res[0] == 1,
		Remaining:  int(res[1]),
		RetryAfter: time.Duration(res[2]) * time.Microsecond,
		ResetAfter: time.Duration(res[3]) * time.Microsecond,
	}, nil
}

type errUnexpected int

func (e errUnexpected) Error() string {
	return "ratelimit: unexpected redis reply length " + strconv.Itoa(int(e))
}

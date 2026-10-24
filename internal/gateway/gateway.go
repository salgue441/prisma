// Package gateway wires configuration into an immutable, atomically swapped
// snapshot (router, filter chains, clusters) and serves requests from it.
package gateway

import (
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/tetratelabs/wazero"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"prisma/internal/config"
	"prisma/internal/filters/authn"
	"prisma/internal/filters/cache"
	"prisma/internal/filters/ratelimit"
	"prisma/internal/router"
	"prisma/internal/telemetry"
	"prisma/internal/upstream"
)

// drainGrace is how long a replaced cluster keeps its idle connections so
// in-flight requests on the old snapshot can finish cleanly.
const drainGrace = 60 * time.Second

type Snapshot struct {
	Version  string
	Config   *config.Config
	Router   *router.Router
	Clusters map[string]*upstream.Cluster
	Plugins  map[string]*loadedPlugin
	LoadedAt time.Time

	trusted []netip.Prefix
}

func (s *Snapshot) isTrusted(a netip.Addr) bool {
	for _, p := range s.trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

type Gateway struct {
	metrics *telemetry.Metrics
	access  *slog.Logger
	jwks    *authn.Registry
	local   *ratelimit.LocalStore
	cache   *cache.Store
	tracer  trace.Tracer
	tracing bool

	wasmCache wazero.CompilationCache
	plugins   map[string]*loadedPlugin // guarded by mu

	snap  atomic.Pointer[Snapshot]
	ready atomic.Bool

	mu       sync.Mutex // serializes Apply
	redis    redis.UniversalClient
	redisCfg *config.Redis
}

// New creates a gateway. accessLog may be nil to discard access logs.
func New(m *telemetry.Metrics, accessLog *slog.Logger) *Gateway {
	if accessLog == nil {
		accessLog = slog.New(slog.DiscardHandler)
	}
	return &Gateway{
		metrics: m, access: accessLog, jwks: authn.NewRegistry(), local: ratelimit.NewLocalStore(),
		cache:     cache.NewStore(64 << 20),
		tracer:    noop.NewTracerProvider().Tracer(""),
		wasmCache: wazero.NewCompilationCache(),
		plugins:   map[string]*loadedPlugin{},
	}
}

// SetTracerProvider enables tracing. Call it before serving traffic. Inbound
// trace context is only trusted (extracted) once tracing is enabled;
// otherwise trace headers pass through to upstreams untouched.
func (g *Gateway) SetTracerProvider(tp trace.TracerProvider) {
	g.tracer = tp.Tracer("prisma")
	g.tracing = true
}

func (g *Gateway) Metrics() *telemetry.Metrics { return g.metrics }
func (g *Gateway) Cache() *cache.Store         { return g.cache }
func (g *Gateway) Snapshot() *Snapshot         { return g.snap.Load() }
func (g *Gateway) Ready() bool                 { return g.ready.Load() && g.snap.Load() != nil }
func (g *Gateway) SetReady(v bool)             { g.ready.Store(v) }

// Apply compiles cfg into a new snapshot and swaps it in. On error the
// current snapshot keeps serving untouched.
func (g *Gateway) Apply(cfg *config.Config, version string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	prev := g.snap.Load()

	redisClient, redisChanged := g.redis, false
	if !reflect.DeepEqual(cfg.Redis, g.redisCfg) {
		redisChanged = true
		redisClient = nil
		if r := cfg.Redis; r != nil {
			redisClient = redis.NewClient(&redis.Options{
				Addr: r.Address, Password: r.Password, DB: r.DB,
				DialTimeout: time.Second, ReadTimeout: r.Timeout, WriteTimeout: r.Timeout,
			})
		}
	}

	snap, created, err := g.build(cfg, version, prev, redisClient)
	if err != nil {
		for _, c := range created {
			c.Close(0)
		}
		if redisChanged && redisClient != nil {
			redisClient.Close()
		}
		return err
	}
	for _, c := range created {
		c.Start()
	}
	g.cache.SetMax(int64(cfg.Global.CacheMaxBytes))
	g.snap.Store(snap)
	g.ready.Store(true)
	g.retirePlugins(snap.Plugins)

	if redisChanged {
		if g.redis != nil {
			old := g.redis
			time.AfterFunc(drainGrace, func() { old.Close() })
		}
		g.redis, g.redisCfg = redisClient, cfg.Redis
	}
	if prev != nil {
		purgeChangedRoutes(g.cache, prev.Config, cfg)
		g.metrics.ConfigInfo.DeleteLabelValues(prev.Version)
		for name, c := range prev.Clusters {
			if snap.Clusters[name] != c {
				c.Close(drainGrace)
			}
		}
	}
	g.metrics.ConfigInfo.WithLabelValues(version).Set(1)
	slog.Info("configuration applied", "version", version, "routes", len(cfg.Routes), "clusters", len(cfg.Clusters))
	return nil
}

// build compiles a snapshot. Clusters whose config is unchanged are reused so
// health state, outlier ejections and connection pools survive reloads. It
// returns the newly created clusters, which the caller starts or closes.
func (g *Gateway) build(cfg *config.Config, version string, prev *Snapshot, rc redis.UniversalClient) (*Snapshot, []*upstream.Cluster, error) {
	snap := &Snapshot{Version: version, Config: cfg, Clusters: map[string]*upstream.Cluster{}, LoadedAt: time.Now()}
	for _, p := range cfg.Global.TrustedProxies {
		snap.trusted = append(snap.trusted, netip.MustParsePrefix(p))
	}

	var created []*upstream.Cluster
	for _, cc := range cfg.Clusters {
		if prev != nil {
			if old, ok := prev.Clusters[cc.Name]; ok && reflect.DeepEqual(old.Config, cc) {
				snap.Clusters[cc.Name] = old
				continue
			}
		}
		cl, err := upstream.NewCluster(cc, g.metrics)
		if err != nil {
			return nil, created, err
		}
		created = append(created, cl)
		snap.Clusters[cc.Name] = cl
	}

	plugins, newPlugins, err := g.loadPlugins(cfg.Plugins)
	ok := false
	defer func() {
		if !ok {
			for _, p := range newPlugins {
				p.Close()
			}
		}
	}()
	if err != nil {
		return nil, created, err
	}
	snap.Plugins = plugins

	var redisStore ratelimit.Store
	if rc != nil {
		redisStore = ratelimit.NewRedisStore(rc, cfg.Redis.Prefix, cfg.Redis.Timeout)
	}
	deps := routeDeps{
		gw:        g,
		snap:      snap,
		consumers: authn.NewConsumers(cfg.Consumers),
		redis:     redisStore,
	}

	var errs []error
	routes := make([]*router.Route, 0, len(cfg.Routes))
	for i := range cfg.Routes {
		rcfg := &cfg.Routes[i]
		h, err := deps.buildRoute(rcfg)
		if err != nil {
			errs = append(errs, fmt.Errorf("route %s: %w", rcfg.Name, err))
			continue
		}
		r, err := router.NewRoute(rcfg, i, h)
		if err != nil {
			errs = append(errs, fmt.Errorf("route %s: %w", rcfg.Name, err))
			continue
		}
		routes = append(routes, r)
	}
	if err := errors.Join(errs...); err != nil {
		return nil, created, err
	}
	snap.Router = router.New(routes)
	ok = true
	return snap, created, nil
}

// purgeChangedRoutes drops cached responses of routes that were removed or
// whose effective configuration changed: entries produced under old rules
// (masking, rewrites, upstreams) must not outlive them.
func purgeChangedRoutes(store *cache.Store, prev, next *config.Config) {
	nextByName := map[string]*config.Route{}
	for i := range next.Routes {
		nextByName[next.Routes[i].Name] = &next.Routes[i]
	}
	for i := range prev.Routes {
		old := &prev.Routes[i]
		nr, ok := nextByName[old.Name]
		if !ok || !reflect.DeepEqual(old, nr) || !reflect.DeepEqual(prev.EffectivePolicy(old), next.EffectivePolicy(nr)) {
			store.Purge(cache.RoutePrefix(old.Name))
		}
	}
}

// Close stops background work owned by the current snapshot.
func (g *Gateway) Close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if s := g.snap.Load(); s != nil {
		for _, c := range s.Clusters {
			c.Close(0)
		}
	}
	for _, p := range g.plugins {
		p.plugin.Close()
	}
	if g.redis != nil {
		g.redis.Close()
	}
}

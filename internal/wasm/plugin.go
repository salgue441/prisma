package wasm

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

const (
	rootContextID = 1
	// callbackTimeout bounds every guest call so a looping plugin cannot
	// stall the gateway; the instance is replaced afterwards.
	callbackTimeout = 2 * time.Second
	maxInstances    = 8
)

// Config describes one plugin.
type Config struct {
	Name         string
	Module       []byte
	PluginConfig []byte
	VMConfig     []byte
	Instances    int
}

// Callouts performs proxy_http_call requests against a gateway cluster.
type Callouts interface {
	HTTPCall(ctx context.Context, cluster string, req *http.Request) (*http.Response, error)
}

// MetricSink receives plugin-defined metrics.
type MetricSink interface {
	SetWasmMetric(plugin, name string, value float64)
}

type Plugin struct {
	cfg      Config
	runtime  wazero.Runtime
	compiled wazero.CompiledModule
	callouts Callouts
	sink     MetricSink
	log      *slog.Logger

	mu        sync.Mutex
	instances []*instance
	next      atomic.Uint32

	shared  sharedData
	metrics metricStore
	closed  chan struct{}
}

// Load compiles the module, then creates and configures its instances.
// cache may be shared across plugins and reloads to avoid recompilation.
func Load(ctx context.Context, cfg Config, cache wazero.CompilationCache, callouts Callouts, sink MetricSink) (*Plugin, error) {
	rc := wazero.NewRuntimeConfig().WithCloseOnContextDone(true)
	if cache != nil {
		rc = rc.WithCompilationCache(cache)
	}
	r := wazero.NewRuntimeWithConfig(ctx, rc)
	p := &Plugin{
		cfg: cfg, runtime: r, callouts: callouts, sink: sink, closed: make(chan struct{}),
		log:     slog.With("plugin", cfg.Name),
		shared:  sharedData{entries: map[string]sharedEntry{}},
		metrics: metricStore{byName: map[string]uint32{}},
	}
	fail := func(err error) (*Plugin, error) {
		r.Close(ctx)
		return nil, fmt.Errorf("wasm plugin %s: %w", cfg.Name, err)
	}
	if _, err := wasi_snapshot_preview1.Instantiate(ctx, r); err != nil {
		return fail(err)
	}
	if err := p.registerHost(ctx); err != nil {
		return fail(err)
	}
	compiled, err := r.CompileModule(ctx, cfg.Module)
	if err != nil {
		return fail(err)
	}
	p.compiled = compiled
	exports := compiled.ExportedFunctions()
	if _, ok := exports["proxy_abi_version_0_2_0"]; !ok {
		if _, ok := exports["proxy_abi_version_0_2_1"]; !ok {
			return fail(errors.New("module does not export proxy_abi_version_0_2_0 or _0_2_1 (only Proxy-Wasm ABI 0.2.x is supported)"))
		}
	}
	n := cfg.Instances
	if n <= 0 {
		n = min(runtime.GOMAXPROCS(0), maxInstances)
	}
	for i := 0; i < n; i++ {
		inst, err := p.newInstance(ctx)
		if err != nil {
			p.Close()
			return nil, fmt.Errorf("wasm plugin %s: %w", cfg.Name, err)
		}
		p.instances = append(p.instances, inst)
	}
	return p, nil
}

func (p *Plugin) Name() string { return p.cfg.Name }

// Close stops timers and releases every instance.
func (p *Plugin) Close() {
	select {
	case <-p.closed:
		return
	default:
		close(p.closed)
	}
	_ = p.runtime.Close(context.Background())
}

type logWriter struct{ log *slog.Logger }

func (w logWriter) Write(b []byte) (int, error) {
	w.log.Debug("plugin output", "text", strings.TrimRight(string(b), "\n"))
	return len(b), nil
}

// instance is one VM. Its mutex is held only while the guest runs.
type instance struct {
	p      *Plugin
	mod    api.Module
	malloc api.Function

	mu     sync.Mutex
	cur    *Stream // stream whose callback is running, nil for root calls
	nextID uint32
	broken atomic.Bool
	tick   chan time.Duration
}

func (p *Plugin) newInstance(ctx context.Context) (*instance, error) {
	mc := wazero.NewModuleConfig().WithName("").WithStartFunctions().
		WithSysWalltime().WithSysNanotime().WithRandSource(rand.Reader).
		WithStdout(logWriter{p.log}).WithStderr(logWriter{p.log})
	mod, err := p.runtime.InstantiateModule(ctx, p.compiled, mc)
	if err != nil {
		return nil, err
	}
	inst := &instance{p: p, mod: mod, nextID: rootContextID + 1, tick: make(chan time.Duration, 1)}
	if inst.malloc = mod.ExportedFunction("proxy_on_memory_allocate"); inst.malloc == nil {
		if inst.malloc = mod.ExportedFunction("malloc"); inst.malloc == nil {
			mod.Close(ctx)
			return nil, errors.New("module exports neither proxy_on_memory_allocate nor malloc")
		}
	}
	// WASI reactors (Go 1.24 c-shared, Rust cdylib) initialize via _initialize.
	if init := mod.ExportedFunction("_initialize"); init != nil {
		if _, err := init.Call(ctx); err != nil {
			mod.Close(ctx)
			return nil, fmt.Errorf("_initialize: %w", err)
		}
	}
	vmSize, cfgSize := uint64(len(p.cfg.VMConfig)), uint64(len(p.cfg.PluginConfig))
	if _, err := inst.call(ctx, nil, "proxy_on_context_create", rootContextID, 0); err != nil {
		return nil, err
	}
	if ok, err := inst.call(ctx, nil, "proxy_on_vm_start", rootContextID, vmSize); err != nil || ok == 0 {
		return nil, fmt.Errorf("proxy_on_vm_start failed (%v)", err)
	}
	if ok, err := inst.call(ctx, nil, "proxy_on_configure", rootContextID, cfgSize); err != nil || ok == 0 {
		return nil, fmt.Errorf("plugin rejected its configuration (proxy_on_configure returned false; %v)", err)
	}
	go inst.timer()
	return inst, nil
}

type instKey struct{}

// call runs a guest export with s as the current stream. Missing optional
// exports are treated as "continue".
func (inst *instance) call(ctx context.Context, s *Stream, fn string, args ...uint64) (uint64, error) {
	f := inst.mod.ExportedFunction(fn)
	if f == nil {
		return actionContinue, nil
	}
	ctx, cancel := context.WithTimeout(context.WithValue(ctx, instKey{}, inst), callbackTimeout)
	defer cancel()
	inst.mu.Lock()
	inst.cur = s
	res, err := f.Call(ctx, args...)
	inst.cur = nil
	inst.mu.Unlock()
	if err != nil {
		inst.broken.Store(true)
		return 0, fmt.Errorf("%s: %w", fn, err)
	}
	if len(res) == 0 {
		return actionContinue, nil
	}
	return res[0], nil
}

// timer drives proxy_on_tick once the plugin sets a tick period.
func (inst *instance) timer() {
	var t *time.Ticker
	var c <-chan time.Time
	for {
		select {
		case <-inst.p.closed:
			if t != nil {
				t.Stop()
			}
			return
		case d := <-inst.tick:
			if t != nil {
				t.Stop()
				t, c = nil, nil
			}
			if d > 0 {
				t = time.NewTicker(d)
				c = t.C
			}
		case <-c:
			if inst.broken.Load() {
				return
			}
			if _, err := inst.call(context.Background(), nil, "proxy_on_tick", rootContextID); err != nil {
				inst.p.log.Warn("proxy_on_tick failed", "err", err)
			}
		}
	}
}

// pick returns a healthy instance round-robin, replacing broken ones.
func (p *Plugin) pick(ctx context.Context) (*instance, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := len(p.instances)
	i := int(p.next.Add(1)) % n
	inst := p.instances[i]
	if !inst.broken.Load() {
		return inst, nil
	}
	p.log.Warn("replacing crashed plugin instance")
	_ = inst.mod.Close(ctx)
	fresh, err := p.newInstance(ctx)
	if err != nil {
		return nil, err
	}
	p.instances[i] = fresh
	return fresh, nil
}

// --- memory helpers (called from host functions with the instance locked) ---

func read(m api.Module, ptr, size uint32) ([]byte, bool) {
	if size == 0 {
		return nil, true
	}
	b, ok := m.Memory().Read(ptr, size)
	if !ok {
		return nil, false
	}
	return append([]byte(nil), b...), true
}

func readString(m api.Module, ptr, size uint32) (string, bool) {
	b, ok := read(m, ptr, size)
	return string(b), ok
}

// returnBytes allocates guest memory, copies data, and writes the pointer
// and size to the guest's out-parameters. Empty data returns a nil pointer,
// which SDKs report as "not found".
func (inst *instance) returnBytes(ctx context.Context, m api.Module, data []byte, ptrPtr, sizePtr uint32) status {
	var ptr uint32
	if len(data) > 0 {
		res, err := inst.malloc.Call(ctx, uint64(len(data)))
		if err != nil || len(res) == 0 {
			return statusInternalFailure
		}
		ptr = uint32(res[0])
		if !m.Memory().Write(ptr, data) {
			return statusInternalFailure
		}
	}
	if !m.Memory().WriteUint32Le(ptrPtr, ptr) || !m.Memory().WriteUint32Le(sizePtr, uint32(len(data))) {
		return statusInternalFailure
	}
	return statusOK
}

// --- shared data (plugin-wide, across instances) ---

type sharedEntry struct {
	value []byte
	cas   uint32
}

type sharedData struct {
	mu      sync.Mutex
	entries map[string]sharedEntry
}

func (s *sharedData) get(key string) (sharedEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[key]
	return e, ok
}

func (s *sharedData) set(key string, value []byte, cas uint32) status {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.entries[key]
	if cas != 0 && (!ok || cur.cas != cas) {
		return statusCasMismatch
	}
	s.entries[key] = sharedEntry{value: value, cas: cur.cas + 1}
	return statusOK
}

// --- metrics ---

type metricStore struct {
	mu     sync.Mutex
	byName map[string]uint32
	names  []string
	values []float64
}

func (m *metricStore) define(name string) uint32 {
	m.mu.Lock()
	defer m.mu.Unlock()
	if id, ok := m.byName[name]; ok {
		return id
	}
	id := uint32(len(m.names))
	m.byName[name] = id
	m.names = append(m.names, name)
	m.values = append(m.values, 0)
	return id
}

func (m *metricStore) update(id uint32, fn func(float64) float64) (string, float64, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if int(id) >= len(m.values) {
		return "", 0, false
	}
	m.values[id] = fn(m.values[id])
	return m.names[id], m.values[id], true
}

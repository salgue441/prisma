package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"prisma/internal/config"
	"prisma/internal/wasm"
)

type loadedPlugin struct {
	fingerprint [32]byte
	plugin      *wasm.Plugin
}

func pluginBytes(v any) ([]byte, error) {
	switch t := v.(type) {
	case nil:
		return nil, nil
	case string:
		return []byte(t), nil
	default:
		return json.Marshal(t)
	}
}

// loadPlugins returns the plugins for a new snapshot. Plugins whose module
// bytes and configuration are unchanged are reused (keeping their
// instances, shared data and compiled code); others are loaded fresh and
// returned in created so the caller can close them if the build fails.
func (g *Gateway) loadPlugins(cfgs []config.Plugin) (map[string]*loadedPlugin, []*wasm.Plugin, error) {
	out := map[string]*loadedPlugin{}
	var created []*wasm.Plugin
	for _, pc := range cfgs {
		module, err := os.ReadFile(pc.File)
		if err != nil {
			return nil, created, fmt.Errorf("plugin %s: %w", pc.Name, err)
		}
		pluginCfg, err := pluginBytes(pc.Config)
		if err != nil {
			return nil, created, fmt.Errorf("plugin %s: config: %w", pc.Name, err)
		}
		vmCfg, err := pluginBytes(pc.VMConfig)
		if err != nil {
			return nil, created, fmt.Errorf("plugin %s: vmConfig: %w", pc.Name, err)
		}
		h := sha256.New()
		for _, part := range [][]byte{module, pluginCfg, vmCfg, []byte(fmt.Sprint(pc.Instances))} {
			h.Write(part)
			h.Write([]byte{0})
		}
		var fp [32]byte
		copy(fp[:], h.Sum(nil))
		if old, ok := g.plugins[pc.Name]; ok && old.fingerprint == fp {
			out[pc.Name] = old
			continue
		}
		p, err := wasm.Load(context.Background(), wasm.Config{
			Name: pc.Name, Module: module, PluginConfig: pluginCfg, VMConfig: vmCfg, Instances: pc.Instances,
		}, g.wasmCache, g, g.metrics)
		if err != nil {
			return nil, created, err
		}
		created = append(created, p)
		out[pc.Name] = &loadedPlugin{fingerprint: fp, plugin: p}
	}
	return out, created, nil
}

// retirePlugins closes plugins no longer in use once in-flight streams drain.
func (g *Gateway) retirePlugins(next map[string]*loadedPlugin) {
	for name, old := range g.plugins {
		if cur, ok := next[name]; !ok || cur != old {
			p := old.plugin
			time.AfterFunc(drainGrace, p.Close)
		}
	}
	g.plugins = next
}

// HTTPCall implements wasm.Callouts: proxy_http_call targets a cluster by
// name and goes through its load balancing, health and breaker. The call
// runs detached from the request's pipeline context so it cannot overwrite
// the request's upstream attribution.
func (g *Gateway) HTTPCall(ctx context.Context, cluster string, req *http.Request) (*http.Response, error) {
	cl, ok := g.snap.Load().Clusters[cluster]
	if !ok {
		return nil, fmt.Errorf("unknown cluster %q", cluster)
	}
	var cctx context.Context
	var cancel context.CancelFunc
	if dl, ok := ctx.Deadline(); ok {
		cctx, cancel = context.WithDeadline(context.Background(), dl)
	} else {
		cctx, cancel = context.WithCancel(context.Background())
	}
	stop := context.AfterFunc(ctx, cancel)
	req = req.WithContext(cctx)
	req.URL.Scheme, req.URL.Host = cl.Scheme, cl.Name
	if req.Host == "" {
		req.Host = cl.DefaultHost
	}
	resp, err := cl.RoundTrip(req)
	if err != nil {
		stop()
		cancel()
		return nil, err
	}
	resp.Body = &cancelBody{ReadCloser: resp.Body, cancel: func() { stop(); cancel() }}
	return resp, nil
}

type cancelBody struct {
	io.ReadCloser
	cancel func()
}

func (b *cancelBody) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}

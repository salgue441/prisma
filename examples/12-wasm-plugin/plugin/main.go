// Command plugin is a Proxy-Wasm plugin for Prisma, built with the official
// Go SDK. It enforces a tenant header:
//
//   - requests without a known tenant are rejected at the gateway (403);
//   - the tenant's plan is looked up and sent upstream as X-Tenant-Plan;
//   - responses get an X-Served-Tenant header;
//   - a per-plan request counter is exported on the gateway's /metrics.
//
// Build: GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o tenant.wasm .
package main

import (
	"encoding/json"

	"github.com/proxy-wasm/proxy-wasm-go-sdk/proxywasm"
	"github.com/proxy-wasm/proxy-wasm-go-sdk/proxywasm/types"
)

func main() {}

func init() { proxywasm.SetVMContext(&vmContext{}) }

type vmContext struct{ types.DefaultVMContext }

func (*vmContext) NewPluginContext(uint32) types.PluginContext {
	return &pluginContext{counters: map[string]proxywasm.MetricCounter{}}
}

// config is the plugin's `config` block from prisma.yaml, as JSON.
type config struct {
	Header string            `json:"header"`
	Plans  map[string]string `json:"plans"` // tenant -> plan
}

type pluginContext struct {
	types.DefaultPluginContext
	cfg      config
	counters map[string]proxywasm.MetricCounter
}

func (p *pluginContext) OnPluginStart(int) types.OnPluginStartStatus {
	raw, err := proxywasm.GetPluginConfiguration()
	if err != nil {
		proxywasm.LogCriticalf("tenant plugin: no configuration: %v", err)
		return types.OnPluginStartStatusFailed
	}
	if err := json.Unmarshal(raw, &p.cfg); err != nil || len(p.cfg.Plans) == 0 {
		proxywasm.LogCriticalf("tenant plugin: invalid configuration %q", raw)
		return types.OnPluginStartStatusFailed
	}
	if p.cfg.Header == "" {
		p.cfg.Header = "x-tenant"
	}
	for _, plan := range p.cfg.Plans {
		if _, ok := p.counters[plan]; !ok {
			p.counters[plan] = proxywasm.DefineCounterMetric("tenant_requests_plan_" + plan)
		}
	}
	proxywasm.LogInfof("tenant plugin started with %d tenants", len(p.cfg.Plans))
	return types.OnPluginStartStatusOK
}

func (p *pluginContext) NewHttpContext(uint32) types.HttpContext { return &httpContext{p: p} }

type httpContext struct {
	types.DefaultHttpContext
	p      *pluginContext
	tenant string
}

func (h *httpContext) OnHttpRequestHeaders(int, bool) types.Action {
	tenant, err := proxywasm.GetHttpRequestHeader(h.p.cfg.Header)
	plan, known := h.p.cfg.Plans[tenant]
	if err != nil || !known {
		body := `{"error":"missing or unknown tenant"}`
		_ = proxywasm.SendHttpResponse(403, [][2]string{{"content-type", "application/json"}}, []byte(body), -1)
		return types.ActionPause
	}
	h.tenant = tenant
	h.p.counters[plan].Increment(1)
	_ = proxywasm.ReplaceHttpRequestHeader("x-tenant-plan", plan)

	// Properties expose gateway data, such as the authenticated consumer.
	if consumer, err := proxywasm.GetProperty([]string{"prisma", "consumer"}); err == nil && len(consumer) > 0 {
		proxywasm.LogDebugf("tenant %s called by consumer %s", tenant, consumer)
	}
	return types.ActionContinue
}

func (h *httpContext) OnHttpResponseHeaders(int, bool) types.Action {
	_ = proxywasm.AddHttpResponseHeader("x-served-tenant", h.tenant)
	return types.ActionContinue
}

// Command plugin is a Proxy-Wasm test plugin built with the official Go SDK
// (GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared). Its behavior is
// selected by request path so one module exercises the whole host ABI.
package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"strconv"

	"github.com/proxy-wasm/proxy-wasm-go-sdk/proxywasm"
	"github.com/proxy-wasm/proxy-wasm-go-sdk/proxywasm/types"
)

func main() {}

func init() {
	proxywasm.SetVMContext(&vm{})
}

type vm struct{ types.DefaultVMContext }

func (*vm) NewPluginContext(uint32) types.PluginContext { return &plugin{} }

type plugin struct {
	types.DefaultPluginContext
	tag      string
	requests proxywasm.MetricCounter
}

func (p *plugin) OnPluginStart(int) types.OnPluginStartStatus {
	raw, err := proxywasm.GetPluginConfiguration()
	if err != nil {
		proxywasm.LogCriticalf("no configuration: %v", err)
		return types.OnPluginStartStatusFailed
	}
	var cfg struct {
		Tag string `json:"tag"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil || cfg.Tag == "" {
		proxywasm.LogCriticalf("bad configuration %q", raw)
		return types.OnPluginStartStatusFailed
	}
	p.tag = cfg.Tag
	p.requests = proxywasm.DefineCounterMetric("plugin_requests")
	// Seed the counter: a write with CAS 0 is unconditional (Proxy-Wasm ABI),
	// so two streams creating the key at once would lose an increment.
	if _, _, err := proxywasm.GetSharedData("hits"); err != nil {
		_ = proxywasm.SetSharedData("hits", make([]byte, 8), 0)
	}
	if _, err := proxywasm.RegisterSharedQueue("events"); err != nil {
		proxywasm.LogCriticalf("register queue: %v", err)
		return types.OnPluginStartStatusFailed
	}
	return types.OnPluginStartStatusOK
}

// OnQueueReady drains the events queue into the "events" shared data entry
// (comma-separated), so tests can observe delivery.
func (p *plugin) OnQueueReady(queueID uint32) {
	for {
		item, err := proxywasm.DequeueSharedQueue(queueID)
		if err != nil {
			return
		}
		for {
			cur, cas, _ := proxywasm.GetSharedData("events")
			next := string(item)
			if len(cur) > 0 {
				next = string(cur) + "," + next
			}
			if proxywasm.SetSharedData("events", []byte(next), cas) == nil {
				break
			}
		}
	}
}

func (p *plugin) NewHttpContext(uint32) types.HttpContext { return &httpCtx{p: p} }

type httpCtx struct {
	types.DefaultHttpContext
	p    *plugin
	path string
}

func (h *httpCtx) OnHttpRequestHeaders(_ int, _ bool) types.Action {
	h.p.requests.Increment(1)
	h.path, _ = proxywasm.GetHttpRequestHeader(":path")
	_ = proxywasm.AddHttpRequestHeader("x-wasm-plugin", h.p.tag)
	switch h.path {
	case "/enqueue":
		id, err := proxywasm.ResolveSharedQueue("", "events")
		if err == nil {
			ev, _ := proxywasm.GetHttpRequestHeader("x-event")
			err = proxywasm.EnqueueSharedQueue(id, []byte(ev))
		}
		status := uint32(202)
		if err != nil {
			status = 500
		}
		_ = proxywasm.SendHttpResponse(status, nil, nil, -1)
		return types.ActionPause
	case "/deny":
		_ = proxywasm.SendHttpResponse(403, [][2]string{{"x-denied-by", "wasm"}}, []byte("denied by plugin"), -1)
		return types.ActionPause
	case "/rewrite":
		_ = proxywasm.ReplaceHttpRequestHeader(":path", "/rewritten")
	case "/panic":
		panic("plugin crashed")
	case "/loop":
		for {
			h.p.requests.Increment(0)
		}
	case "/callout", "/callout-fail":
		target := "/allow"
		if h.path == "/callout-fail" {
			target = "/forbid"
		}
		_, err := proxywasm.DispatchHttpCall("authsvc",
			[][2]string{{":method", "GET"}, {":path", target}, {":authority", "auth.internal"}},
			nil, nil, 2000, h.onCallout)
		if err != nil {
			_ = proxywasm.SendHttpResponse(500, nil, []byte("dispatch failed: "+err.Error()), -1)
		}
		return types.ActionPause
	}
	return types.ActionContinue
}

func (h *httpCtx) onCallout(_, _, _ int) {
	hdrs, _ := proxywasm.GetHttpCallResponseHeaders()
	status := ""
	for _, kv := range hdrs {
		if kv[0] == ":status" {
			status = kv[1]
		}
	}
	body, _ := proxywasm.GetHttpCallResponseBody(0, 1024)
	if status != "200" {
		_ = proxywasm.SendHttpResponse(401, nil, []byte("callout said "+status), -1)
		return
	}
	_ = proxywasm.AddHttpRequestHeader("x-callout", string(body))
	_ = proxywasm.ResumeHttpRequest()
}

func (h *httpCtx) OnHttpRequestBody(size int, eos bool) types.Action {
	if h.path != "/body" {
		return types.ActionContinue
	}
	if !eos {
		return types.ActionPause // wait for the whole body
	}
	b, _ := proxywasm.GetHttpRequestBody(0, size)
	_ = proxywasm.ReplaceHttpRequestBody(bytes.ToUpper(b))
	return types.ActionContinue
}

func (h *httpCtx) OnHttpResponseHeaders(_ int, _ bool) types.Action {
	_ = proxywasm.AddHttpResponseHeader("x-wasm-response", h.p.tag)
	if v, err := proxywasm.GetProperty([]string{"request", "path"}); err == nil {
		_ = proxywasm.AddHttpResponseHeader("x-property-path", string(v))
	}
	if v, err := proxywasm.GetProperty([]string{"route_name"}); err == nil {
		_ = proxywasm.AddHttpResponseHeader("x-property-route", string(v))
	}
	_ = proxywasm.AddHttpResponseHeader("x-hits", strconv.FormatUint(h.countHit(), 10))
	return types.ActionContinue
}

// countHit increments a counter in shared data with compare-and-swap.
func (h *httpCtx) countHit() uint64 {
	for {
		raw, cas, err := proxywasm.GetSharedData("hits")
		var n uint64
		if err == nil && len(raw) == 8 {
			n = binary.LittleEndian.Uint64(raw)
		}
		n++
		buf := make([]byte, 8)
		binary.LittleEndian.PutUint64(buf, n)
		if err := proxywasm.SetSharedData("hits", buf, cas); err == nil {
			return n
		}
	}
}

func (h *httpCtx) OnHttpResponseBody(size int, eos bool) types.Action {
	if h.path != "/body" {
		return types.ActionContinue
	}
	if !eos {
		return types.ActionPause
	}
	b, _ := proxywasm.GetHttpResponseBody(0, size)
	_ = proxywasm.ReplaceHttpResponseBody(append(b, []byte(" (seen by wasm)")...))
	return types.ActionContinue
}

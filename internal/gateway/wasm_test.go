package gateway_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"prisma/internal/config"
	pt "prisma/internal/testutil"
)

func TestProxyWasmPlugins(t *testing.T) {
	wasmFile := strings.ReplaceAll(pt.WasmPlugin(t), `\`, `/`)
	var upstreamHits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("X-Seen-Plugin", r.Header.Get("X-Wasm-Plugin"))
		w.Header().Set("X-Seen-Callout", r.Header.Get("X-Callout"))
		_, _ = io.WriteString(w, r.URL.Path+":"+string(body))
	}))
	defer up.Close()
	authsvc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/forbid" {
			w.WriteHeader(403)
			return
		}
		_, _ = io.WriteString(w, "ok-from-authsvc host="+r.Host)
	}))
	defer authsvc.Close()

	yml := basePrefix + `
plugins:
  - name: tagger
    file: "` + wasmFile + `"
    config: {tag: gw}
    bodies: true
    instances: 2
clusters:
  - name: up
    endpoints: [{address: "{{up}}"}]
  - name: authsvc
    endpoints: [{address: "{{auth}}"}]
routes:
  - name: plugged
    match: {pathPrefix: /}
    plugins: [tagger]
    backends: [{cluster: up}]
`
	g, gw := newGateway(t, yml, map[string]string{"up": addr(up), "auth": addr(authsvc)})
	do := func(method, path, body string) (*http.Response, string) {
		req, _ := http.NewRequest(method, gw.URL+path, strings.NewReader(body))
		resp, err := http.DefaultTransport.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}

	resp, body := do("GET", "/x", "")
	if resp.Header.Get("X-Seen-Plugin") != "gw" || resp.Header.Get("X-Wasm-Response") != "gw" ||
		resp.Header.Get("X-Property-Route") != "plugged" || body != "/x:" {
		t.Fatalf("pass-through: %d %q %v", resp.StatusCode, body, resp.Header)
	}

	before := upstreamHits.Load()
	resp, body = do("GET", "/deny", "")
	if resp.StatusCode != 403 || body != "denied by plugin" || resp.Header.Get("X-Denied-By") != "wasm" || upstreamHits.Load() != before {
		t.Errorf("local response: %d %q (upstream must not be called)", resp.StatusCode, body)
	}

	resp, _ = do("GET", "/rewrite", "")
	if b, _ := io.ReadAll(resp.Body); resp.StatusCode != 200 {
		t.Errorf("rewrite: %d %s", resp.StatusCode, b)
	}
	if _, body = do("GET", "/rewrite", ""); body != "/rewritten:" {
		t.Errorf(":path rewrite reached upstream as %q", body)
	}

	resp, _ = do("GET", "/callout", "")
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("X-Seen-Callout"), "ok-from-authsvc host=auth.internal") {
		t.Errorf("callout through cluster: %d %v", resp.StatusCode, resp.Header)
	}
	if resp, body = do("GET", "/callout-fail", ""); resp.StatusCode != 401 || body != "callout said 403" {
		t.Errorf("callout deny: %d %q", resp.StatusCode, body)
	}

	if _, body = do("POST", "/body", "hello"); body != "/body:HELLO (seen by wasm)" {
		t.Errorf("body rewrite both ways: %q", body)
	}

	if v := testutil.ToFloat64(g.Metrics().WasmMetric.WithLabelValues("tagger", "plugin_requests")); v < 6 {
		t.Errorf("plugin metric = %v, want >= 6", v)
	}

	// An unchanged plugin survives a reload (same instances and shared data).
	before2 := g.Snapshot().Plugins["tagger"]
	cfg, v, err := config.Parse([]byte(strings.NewReplacer("{{up}}", addr(up), "{{auth}}", addr(authsvc)).Replace(yml) + "    priority: 1\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Apply(cfg, v); err != nil {
		t.Fatal(err)
	}
	if g.Snapshot().Plugins["tagger"] != before2 {
		t.Error("unchanged plugin was reloaded")
	}
}

func TestProxyWasmConfigErrors(t *testing.T) {
	_, _, err := config.Parse([]byte(basePrefix + `
clusters: [{name: c, endpoints: [{address: "a:1"}]}]
routes: [{name: r, match: {pathPrefix: /}, plugins: [ghost], backends: [{cluster: c}]}]
`))
	if err == nil || !strings.Contains(err.Error(), `unknown plugin "ghost"`) {
		t.Fatalf("got %v", err)
	}
}

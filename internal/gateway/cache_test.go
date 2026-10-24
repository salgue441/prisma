package gateway_test

import (
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"prisma/internal/config"
)

func TestResponseCache(t *testing.T) {
	var hits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		switch r.URL.Path {
		case "/nostore":
			w.Header().Set("Cache-Control", "no-store")
		case "/private":
			w.Header().Set("Cache-Control", "private, max-age=60")
		case "/cookie":
			w.Header().Set("Cache-Control", "max-age=60")
			w.Header().Set("Set-Cookie", "s=1")
		case "/nodirective":
		default:
			w.Header().Set("Cache-Control", "public, max-age=60")
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = fmt.Fprintf(w, "response %d for %s %s", n, r.Header.Get("X-Prisma-Consumer"), strings.Repeat(".", 2000))
	}))
	defer up.Close()

	yml := basePrefix + `
consumers:
  - {name: alice, apiKeys: [ka]}
  - {name: bob, apiKeys: [kb]}
clusters:
  - name: up
    endpoints: [{address: "{{up}}"}]
routes:
  - name: cached
    match: {pathPrefix: /}
    authn: {apiKey: {}}
    cache: {}
    compression: {algorithms: [gzip]}
    backends: [{cluster: up}]
`
	g, gw := newGateway(t, yml, map[string]string{"up": addr(up)})

	// The default transport silently adds Accept-Encoding: gzip to GETs (not
	// HEADs), which would make them different Vary variants.
	client := &http.Transport{DisableCompression: true}
	defer client.CloseIdleConnections()
	call := func(method, path, key string, hdr map[string]string) (*http.Response, string) {
		req, _ := http.NewRequest(method, gw.URL+path, nil)
		if key != "" {
			req.Header.Set("X-API-Key", key)
		}
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := client.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var r io.Reader = resp.Body
		if resp.Header.Get("Content-Encoding") == "gzip" {
			r, _ = gzip.NewReader(resp.Body)
		}
		b, _ := io.ReadAll(r)
		return resp, string(b)
	}
	upstreamCalls := func() int32 { return hits.Load() }

	_, first := call("GET", "/a", "ka", nil)
	resp, second := call("GET", "/a", "ka", nil)
	if upstreamCalls() != 1 || resp.Header.Get("X-Cache") != "HIT" || second != first || resp.Header.Get("Age") == "" {
		t.Fatalf("second GET should be a hit: calls=%d x-cache=%q", upstreamCalls(), resp.Header.Get("X-Cache"))
	}
	if resp, body := call("HEAD", "/a", "ka", nil); resp.Header.Get("X-Cache") != "HIT" || body != "" {
		t.Errorf("HEAD should be served from the GET entry without a body")
	}
	if resp, _ := call("GET", "/a", "", nil); resp.StatusCode != 401 {
		t.Errorf("cache hits must still require authentication, got %d", resp.StatusCode)
	}
	if _, body := call("GET", "/a", "kb", nil); !strings.Contains(body, "for bob") || upstreamCalls() != 2 {
		t.Errorf("another consumer must not get alice's entry: %.30q (calls %d)", body, upstreamCalls())
	}

	// Encoding variants are cached separately (Vary: Accept-Encoding).
	resp, gz := call("GET", "/a", "ka", map[string]string{"Accept-Encoding": "gzip"})
	if resp.Header.Get("Content-Encoding") != "gzip" || resp.Header.Get("X-Cache") == "HIT" {
		t.Errorf("gzip client got the identity variant: %v", resp.Header)
	}
	resp, gz2 := call("GET", "/a", "ka", map[string]string{"Accept-Encoding": "gzip"})
	if resp.Header.Get("X-Cache") != "HIT" || resp.Header.Get("Content-Encoding") != "gzip" || gz2 != gz {
		t.Errorf("gzip variant should now be a hit")
	}

	if _, _ = call("GET", "/a", "ka", map[string]string{"Cache-Control": "no-cache"}); upstreamCalls() != 4 {
		t.Errorf("client no-cache must go to the upstream (calls %d)", upstreamCalls())
	}

	for _, p := range []string{"/nostore", "/private", "/cookie", "/nodirective"} {
		before := upstreamCalls()
		call("GET", p, "ka", nil)
		call("GET", p, "ka", nil)
		if upstreamCalls()-before != 2 {
			t.Errorf("%s must not be cached", p)
		}
	}

	// Changing the route's config on reload purges its entries.
	before := upstreamCalls()
	cfg, v, err := config.Parse([]byte(strings.ReplaceAll(strings.Replace(yml, "cache: {}", "cache: {maxEntryBytes: 2MiB}", 1), "{{up}}", addr(up))))
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Apply(cfg, v); err != nil {
		t.Fatal(err)
	}
	if call("GET", "/a", "ka", nil); upstreamCalls() != before+1 {
		t.Error("entries of a changed route must be purged on reload")
	}
}

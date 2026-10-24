package gateway_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type shadowSeen struct {
	method, path, host, body, shadowHdr, consumer string
}

func TestTrafficMirroring(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_, _ = io.WriteString(w, "primary got "+string(b))
	}))
	defer primary.Close()

	var mu sync.Mutex
	var seen []shadowSeen
	var shadowHits atomic.Int32
	shadow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, shadowSeen{r.Method, r.URL.Path, r.Host, string(b), r.Header.Get("X-Prisma-Shadow"), r.Header.Get("X-Prisma-Consumer")})
		mu.Unlock()
		shadowHits.Add(1)
		if r.URL.Path == "/orders/slow" {
			time.Sleep(time.Second) // a slow, failing shadow must not hurt clients
		}
		w.WriteHeader(500)
	}))
	defer shadow.Close()

	_, gw := newGateway(t, basePrefix+`
consumers: [{name: shop, apiKeys: [k]}]
clusters:
  - name: primary
    endpoints: [{address: "{{primary}}"}]
  - name: shadow
    preserveHost: true
    endpoints: [{address: "{{shadow}}"}]
routes:
  - name: orders
    match: {pathPrefix: /api/orders}
    authn: {apiKey: {}}
    rewrite: {stripPrefix: /api}
    mirror: {cluster: shadow}
    backends: [{cluster: primary}]
  - name: sampled
    match: {pathPrefix: /sampled}
    mirror: {cluster: shadow, percent: 25}
    backends: [{cluster: primary}]
`, map[string]string{"primary": addr(primary), "shadow": addr(shadow)})

	post := func(path, body string, key bool) (int, string, time.Duration) {
		req, _ := http.NewRequest(http.MethodPost, gw.URL+path, strings.NewReader(body))
		if key {
			req.Header.Set("X-API-Key", "k")
		}
		start := time.Now()
		resp, err := http.DefaultTransport.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b), time.Since(start)
	}

	code, body, elapsed := post("/api/orders/slow", `{"id":1}`, true)
	if code != 200 || body != `primary got {"id":1}` {
		t.Fatalf("primary response changed by mirroring: %d %q", code, body)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("client waited %v for a slow shadow", elapsed)
	}
	if code, _, _ := post("/api/orders/x", "nope", false); code != 401 {
		t.Fatalf("unauthenticated: %d", code)
	}

	deadline := time.Now().Add(3 * time.Second)
	for shadowHits.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // give a wrongly mirrored 401 time to show up
	mu.Lock()
	got := append([]shadowSeen(nil), seen...)
	mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("shadow saw %d requests, want exactly 1 (rejected requests must not be mirrored): %+v", len(got), got)
	}
	s := got[0]
	if s.method != "POST" || s.path != "/orders/slow" || s.body != `{"id":1}` || s.shadowHdr != "true" || s.consumer != "shop" ||
		!strings.HasPrefix(s.host, "127.0.0.1-shadow:") {
		t.Errorf("shadow request = %+v", s)
	}

	before := shadowHits.Load()
	for i := 0; i < 200; i++ {
		post("/sampled", "", false)
	}
	time.Sleep(200 * time.Millisecond)
	if n := shadowHits.Load() - before; n < 20 || n > 90 {
		t.Errorf("25%% sampling mirrored %d of 200 requests", n)
	}
}

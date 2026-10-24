package wasm

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"prisma/internal/testutil"
)

// testPlugin returns the SDK-built test plugin (see testutil.WasmPlugin).
func testPlugin(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(testutil.WasmPlugin(t))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type fakeCallouts struct{ status int }

func (f fakeCallouts) HTTPCall(_ context.Context, cluster string, req *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	code := f.status
	if req.URL.Path == "/forbid" {
		code = 403
	}
	rec.WriteHeader(code)
	_, _ = io.WriteString(rec, "approved-by-"+cluster)
	return rec.Result(), nil
}

func load(t *testing.T, cfg string) *Plugin {
	t.Helper()
	p, err := Load(context.Background(), Config{Name: "test", Module: testPlugin(t), PluginConfig: []byte(cfg), Instances: 2},
		nil, fakeCallouts{status: 200}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

func TestPluginLifecycleAndHeaders(t *testing.T) {
	if _, err := Load(context.Background(), Config{Name: "bad", Module: testPlugin(t), PluginConfig: []byte(`{}`), Instances: 1}, nil, nil, nil); err == nil ||
		!strings.Contains(err.Error(), "rejected its configuration") {
		t.Fatalf("plugin refusing its config must fail Load, got %v", err)
	}
	p := load(t, `{"tag":"blue"}`)
	ctx := context.Background()

	s, err := p.NewStream(ctx, func(path []string) ([]byte, bool) {
		if strings.Join(path, ".") == "request.path" {
			return []byte("/orders"), true
		}
		return nil, false
	})
	if err != nil {
		t.Fatal(err)
	}
	s.Req = httptest.NewRequest("GET", "http://api.test/orders", nil)
	if err := s.OnRequestHeaders(ctx, true); err != nil {
		t.Fatal(err)
	}
	if s.Req.Header.Get("X-Wasm-Plugin") != "blue" {
		t.Errorf("request header not added: %v", s.Req.Header)
	}
	s.Resp = &http.Response{StatusCode: 200, Header: http.Header{}}
	if err := s.OnResponseHeaders(ctx, true); err != nil {
		t.Fatal(err)
	}
	if s.Resp.Header.Get("X-Wasm-Response") != "blue" || s.Resp.Header.Get("X-Property-Path") != "/orders" || s.Resp.Header.Get("X-Hits") != "1" {
		t.Errorf("response headers: %v", s.Resp.Header)
	}
	s.Done(ctx)
}

func TestPluginLocalResponseRewriteCalloutsAndBodies(t *testing.T) {
	p := load(t, `{"tag":"x"}`)
	ctx := context.Background()
	stream := func(method, path string) *Stream {
		s, err := p.NewStream(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		s.Req = httptest.NewRequest(method, "http://api.test"+path, nil)
		return s
	}

	s := stream("GET", "/deny")
	_ = s.OnRequestHeaders(ctx, true)
	if l := s.Local(); l == nil || l.Status != 403 || string(l.Body) != "denied by plugin" || l.Header.Get("X-Denied-By") != "wasm" {
		t.Errorf("local response = %+v", l)
	}

	s = stream("GET", "/rewrite")
	_ = s.OnRequestHeaders(ctx, true)
	if s.Req.URL.Path != "/rewritten" {
		t.Errorf(":path rewrite: %s", s.Req.URL.Path)
	}

	s = stream("GET", "/callout")
	if err := s.OnRequestHeaders(ctx, true); err != nil {
		t.Fatal(err)
	}
	if s.Local() != nil || s.Req.Header.Get("X-Callout") != "approved-by-authsvc" {
		t.Errorf("callout allow: local=%+v headers=%v", s.Local(), s.Req.Header)
	}
	s = stream("GET", "/callout-fail")
	_ = s.OnRequestHeaders(ctx, true)
	if l := s.Local(); l == nil || l.Status != 401 || string(l.Body) != "callout said 403" {
		t.Errorf("callout deny: %+v", l)
	}

	s = stream("POST", "/body")
	_ = s.OnRequestHeaders(ctx, false)
	s.ReqBody = []byte("hello")
	if err := s.OnRequestBody(ctx); err != nil {
		t.Fatal(err)
	}
	if string(s.ReqBody) != "HELLO" {
		t.Errorf("request body = %q", s.ReqBody)
	}
	s.Resp = &http.Response{StatusCode: 200, Header: http.Header{}}
	_ = s.OnResponseHeaders(ctx, false)
	s.RespBody = []byte("ok")
	_ = s.OnResponseBody(ctx)
	if string(s.RespBody) != "ok (seen by wasm)" {
		t.Errorf("response body = %q", s.RespBody)
	}
}

func TestPluginConcurrentStreams(t *testing.T) {
	p := load(t, `{"tag":"c"}`)
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Go(func() {
			s, err := p.NewStream(ctx, nil)
			if err != nil {
				t.Error(err)
				return
			}
			s.Req = httptest.NewRequest("GET", "http://a/x", nil)
			if err := s.OnRequestHeaders(ctx, true); err != nil || s.Req.Header.Get("X-Wasm-Plugin") != "c" {
				t.Errorf("concurrent stream: %v", err)
			}
			s.Resp = &http.Response{StatusCode: 200, Header: http.Header{}}
			_ = s.OnResponseHeaders(ctx, true)
			s.Done(ctx)
		})
	}
	wg.Wait()
	// Shared data is plugin-wide: 50 CAS increments across both instances.
	e, _ := p.shared.get("hits")
	if len(e.value) != 8 || e.value[0] != 50 {
		t.Errorf("shared counter = %v, want 50", e.value)
	}
}

func TestPluginFaultsAreContained(t *testing.T) {
	p := load(t, `{"tag":"f"}`)
	ctx := context.Background()
	run := func(path string) error {
		s, err := p.NewStream(ctx, nil)
		if err != nil {
			return err
		}
		s.Req = httptest.NewRequest("GET", "http://a"+path, nil)
		return s.OnRequestHeaders(ctx, true)
	}
	for _, path := range []string{"/panic", "/loop"} {
		start := time.Now()
		if err := run(path); err == nil {
			t.Errorf("%s: expected an error", path)
		}
		if d := time.Since(start); d > callbackTimeout+2*time.Second {
			t.Errorf("%s took %v; callbacks must be bounded", path, d)
		}
		// Every instance keeps serving: crashed ones are replaced on use.
		for i := 0; i < 4; i++ {
			if err := run("/ok"); err != nil {
				t.Fatalf("after %s: healthy request failed: %v", path, err)
			}
		}
	}
}

func TestSerdeRoundTrip(t *testing.T) {
	in := [][2]string{{":path", "/a"}, {"x", ""}, {"k", "v\x00w"}}
	out, err := deserializeMap(serializeMap(in))
	if err != nil || len(out) != 3 || out[2][1] != "v\x00w" || out[1][0] != "x" {
		t.Fatalf("round trip: %v %v", out, err)
	}
	if _, err := deserializeMap([]byte{9, 0, 0, 0, 1}); err == nil {
		t.Error("truncated map must fail")
	}
}

func TestPluginSharedQueues(t *testing.T) {
	p := load(t, `{"tag":"q"}`)
	ctx := context.Background()
	for _, ev := range []string{"a", "b", "c"} {
		s, err := p.NewStream(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		s.Req = httptest.NewRequest("GET", "http://x/enqueue", nil)
		s.Req.Header.Set("X-Event", ev)
		if err := s.OnRequestHeaders(ctx, true); err != nil {
			t.Fatal(err)
		}
		if l := s.Local(); l == nil || l.Status != 202 {
			t.Fatalf("enqueue response = %+v", l)
		}
		s.Done(ctx)
	}
	// proxy_on_queue_ready runs asynchronously on a root context.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if e, ok := p.shared.get("events"); ok && strings.Count(string(e.value), ",") == 2 {
			for _, ev := range []string{"a", "b", "c"} {
				if !strings.Contains(string(e.value), ev) {
					t.Fatalf("events = %q", e.value)
				}
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	e, _ := p.shared.get("events")
	t.Fatalf("queued events not delivered: %q", e.value)
}

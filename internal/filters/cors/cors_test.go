package cors

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"prisma/internal/config"
	"prisma/internal/pipeline"
)

func run(f *Filter, method, origin string, hdr map[string]string) (*pipeline.Context, pipeline.Responder) {
	r := httptest.NewRequest(method, "/", nil)
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	c := &pipeline.Context{Request: r}
	return c, f.OnRequest(c)
}

func TestOriginMatching(t *testing.T) {
	f := New(&config.CORS{AllowOrigins: []string{"https://app.example.com", "https://*.partner.io"}})
	for origin, want := range map[string]bool{
		"https://app.example.com":  true,
		"https://APP.example.com":  true,
		"https://a.partner.io":     true,
		"https://a.b.partner.io":   true,
		"https://partner.io":       false,
		"http://a.partner.io":      false,
		"https://evil-partner.io":  false,
		"https://app.example.com.": false,
	} {
		if got := f.allowed(origin); got != want {
			t.Errorf("allowed(%q) = %v, want %v", origin, got, want)
		}
	}
}

func TestPreflight(t *testing.T) {
	f := New(&config.CORS{
		AllowOrigins: []string{"https://app.example.com"}, AllowCredentials: true, MaxAge: 10 * time.Minute,
	})
	c, res := run(f, http.MethodOptions, "https://app.example.com", map[string]string{
		"Access-Control-Request-Method":  "POST",
		"Access-Control-Request-Headers": "authorization, content-type",
	})
	if res == nil {
		t.Fatal("preflight must be answered by the gateway")
	}
	h := c.ResponseHeaders
	for k, want := range map[string]string{
		"Access-Control-Allow-Origin":      "https://app.example.com",
		"Access-Control-Allow-Credentials": "true",
		"Access-Control-Allow-Headers":     "authorization, content-type",
		"Access-Control-Max-Age":           "600",
	} {
		if got := h.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}

	c, res = run(f, http.MethodOptions, "https://evil.com", map[string]string{"Access-Control-Request-Method": "POST"})
	if res == nil || c.ResponseHeaders.Get("Access-Control-Allow-Origin") != "" {
		t.Error("disallowed preflight must be answered without CORS headers")
	}
}

func TestSimpleRequests(t *testing.T) {
	f := New(&config.CORS{AllowOrigins: []string{"*"}, ExposeHeaders: []string{"X-Request-Id"}})
	c, res := run(f, http.MethodGet, "https://anything.dev", nil)
	if res != nil {
		t.Fatal("simple requests must continue to the upstream")
	}
	if c.ResponseHeaders.Get("Access-Control-Allow-Origin") != "*" || c.ResponseHeaders.Get("Vary") != "" {
		t.Errorf("wildcard without credentials: %v", c.ResponseHeaders)
	}
	if c.ResponseHeaders.Get("Access-Control-Expose-Headers") != "X-Request-Id" {
		t.Errorf("expose headers: %v", c.ResponseHeaders)
	}
	// OPTIONS without Access-Control-Request-Method is a normal request.
	if _, res := run(f, http.MethodOptions, "https://a.dev", nil); res != nil {
		t.Error("plain OPTIONS must be proxied")
	}
}

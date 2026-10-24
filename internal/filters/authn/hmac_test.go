package authn

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"prisma/internal/config"
	"prisma/internal/pipeline"
	"prisma/internal/telemetry"
)

const hmacSecret = "0123456789abcdef0123456789abcdef"

func hmacFilter(t *testing.T) *Filter {
	t.Helper()
	consumers := []config.Consumer{{Name: "partner", HMACKeys: map[string]string{"k1": hmacSecret}, Scopes: []string{"orders:write"}}}
	f, err := New("r", &config.AuthN{HMAC: &config.HMAC{SignedHeaders: []string{"X-Tenant"}}}, NewConsumers(consumers), NewRegistry(), telemetry.NewMetrics())
	if err != nil {
		t.Fatal(err)
	}
	return f
}

type signedReq struct {
	method, target, body, tenant string
	ts                           int64
	// mutate tampers with the request after signing.
	mutate func(*http.Request)
}

func (s signedReq) build() *http.Request {
	r := httptest.NewRequest(s.method, s.target, strings.NewReader(s.body))
	r.Header.Set("X-Tenant", s.tenant)
	sig := Sign([]byte(hmacSecret), s.method, r.URL.RequestURI(), r.Host, s.ts, []byte(s.body), r.Header, []string{"X-Tenant"})
	r.Header.Set(HeaderSignatureKey, "k1")
	r.Header.Set(HeaderSignatureTimestamp, strconv.FormatInt(s.ts, 10))
	r.Header.Set(HeaderSignature, sig)
	if s.mutate != nil {
		s.mutate(r)
	}
	return r
}

func TestHMACSignatures(t *testing.T) {
	f := hmacFilter(t)
	now := time.Now().Unix()
	base := signedReq{method: "POST", target: "http://api.test/orders?x=1", body: `{"qty":2}`, tenant: "acme", ts: now}

	r := base.build()
	c := &pipeline.Context{Request: r}
	if res := f.OnRequest(c); res != nil {
		t.Fatalf("valid signature rejected: %v", res)
	}
	if c.Principal.Consumer != "partner" || c.Principal.Method != "hmac" || !c.Principal.HasScope("orders:write") {
		t.Errorf("principal = %+v", c.Principal)
	}
	if b, _ := io.ReadAll(c.Request.Body); string(b) != base.body {
		t.Errorf("body not restored for the upstream: %q", b)
	}
	if res := f.OnRequest(&pipeline.Context{Request: base.build()}); res == nil {
		t.Error("replayed signature accepted")
	}

	cases := map[string]signedReq{
		"tampered body":    {method: "POST", target: base.target, body: base.body, tenant: "acme", ts: now + 1, mutate: func(r *http.Request) { r.Body = io.NopCloser(strings.NewReader(`{"qty":200}`)) }},
		"tampered path":    {method: "POST", target: base.target, body: base.body, tenant: "acme", ts: now + 2, mutate: func(r *http.Request) { r.URL.Path = "/admin" }},
		"tampered query":   {method: "POST", target: base.target, body: base.body, tenant: "acme", ts: now + 3, mutate: func(r *http.Request) { r.URL.RawQuery = "x=2" }},
		"tampered method":  {method: "POST", target: base.target, body: base.body, tenant: "acme", ts: now + 4, mutate: func(r *http.Request) { r.Method = "DELETE" }},
		"tampered host":    {method: "POST", target: base.target, body: base.body, tenant: "acme", ts: now + 5, mutate: func(r *http.Request) { r.Host = "evil.test" }},
		"tampered header":  {method: "POST", target: base.target, body: base.body, tenant: "acme", ts: now + 6, mutate: func(r *http.Request) { r.Header.Set("X-Tenant", "other") }},
		"missing header":   {method: "POST", target: base.target, body: base.body, tenant: "acme", ts: now + 7, mutate: func(r *http.Request) { r.Header.Del("X-Tenant") }},
		"stale timestamp":  {method: "POST", target: base.target, body: base.body, tenant: "acme", ts: now - 3600},
		"future timestamp": {method: "POST", target: base.target, body: base.body, tenant: "acme", ts: now + 3600},
		"unknown key":      {method: "POST", target: base.target, body: base.body, tenant: "acme", ts: now + 8, mutate: func(r *http.Request) { r.Header.Set(HeaderSignatureKey, "nope") }},
	}
	for name, tc := range cases {
		if res := f.OnRequest(&pipeline.Context{Request: tc.build()}); res == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

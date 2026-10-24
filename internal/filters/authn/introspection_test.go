package authn

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"prisma/internal/config"
	"prisma/internal/gwerr"
	"prisma/internal/pipeline"
	"prisma/internal/telemetry"
)

type fakeIdP struct {
	*httptest.Server
	calls atomic.Int32
	down  atomic.Bool
	delay time.Duration
}

func newFakeIdP(t *testing.T) *fakeIdP {
	idp := &fakeIdP{}
	idp.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		idp.calls.Add(1)
		time.Sleep(idp.delay)
		if idp.down.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		if id, secret, _ := r.BasicAuth(); id != "gateway" || secret != "s3cret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = r.ParseForm()
		res := map[string]any{"active": false}
		switch r.PostForm.Get("token") {
		case "good":
			res = map[string]any{"active": true, "sub": "user-1", "scope": "orders:read profile",
				"aud": "api", "iss": "https://idp", "exp": time.Now().Add(time.Hour).Unix()}
		case "wrong-aud":
			res = map[string]any{"active": true, "sub": "user-2", "aud": []string{"other"}, "iss": "https://idp"}
		}
		_ = json.NewEncoder(w).Encode(res)
	}))
	t.Cleanup(idp.Close)
	return idp
}

func newFilter(t *testing.T, idp *fakeIdP, jwtToo bool) *Filter {
	cfg := &config.AuthN{Introspection: &config.Introspection{
		Endpoint: idp.URL, ClientID: "gateway", ClientSecret: "s3cret", Audiences: []string{"api"}, Issuer: "https://idp",
	}}
	if jwtToo {
		cfg.JWT = &config.JWT{Secret: "irrelevant-secret-irrelevant-secret"}
	}
	f, err := New("r", cfg, NewConsumers(nil), NewRegistry(), telemetry.NewMetrics())
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func authenticate(f *Filter, token string) (*pipeline.Context, pipeline.Responder) {
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	c := &pipeline.Context{Request: r}
	return c, f.OnRequest(c)
}

func statusOf(res pipeline.Responder) int {
	if e, ok := res.(*gwerr.Error); ok {
		return e.HTTPStatus
	}
	return 0
}

func TestIntrospection(t *testing.T) {
	idp := newFakeIdP(t)
	f := newFilter(t, idp, true) // JWT first: opaque tokens must fall through to introspection

	c, res := authenticate(f, "good")
	if res != nil || c.Principal.ID != "user-1" || !c.Principal.HasScope("orders:read") || c.Principal.Method != "introspection" {
		t.Fatalf("good token: %v %+v", res, c.Principal)
	}
	authenticate(f, "good")
	if n := idp.calls.Load(); n != 1 {
		t.Errorf("active token introspected %d times, want 1 (cached)", n)
	}

	for _, tok := range []string{"revoked", "revoked", "wrong-aud"} {
		if _, res := authenticate(f, tok); statusOf(res) != 401 {
			t.Errorf("%s: status %d, want 401", tok, statusOf(res))
		}
	}
	if n := idp.calls.Load(); n != 3 {
		t.Errorf("calls = %d, want 3 (inactive token negatively cached)", n)
	}

	idp.down.Store(true)
	if _, res := authenticate(f, "new-token"); statusOf(res) != 503 {
		t.Errorf("IdP outage: status %d, want 503", statusOf(res))
	}
	if _, res := authenticate(f, "good"); res != nil {
		t.Error("cached token must keep working during an IdP outage")
	}
}

func TestIntrospectionCollapsesConcurrentLookups(t *testing.T) {
	idp := newFakeIdP(t)
	idp.delay = 50 * time.Millisecond
	f := newFilter(t, idp, false)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Go(func() {
			if _, res := authenticate(f, "good"); res != nil {
				t.Error("concurrent lookup failed")
			}
		})
	}
	wg.Wait()
	if n := idp.calls.Load(); n != 1 {
		t.Errorf("20 concurrent requests made %d introspection calls, want 1", n)
	}
}

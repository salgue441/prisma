package gateway_test

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"prisma/internal/config"
	"prisma/internal/gateway"
	"prisma/internal/telemetry"
)

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// h2cClient speaks HTTP/2 with prior knowledge over cleartext.
func h2cClient() *http.Client {
	p := new(http.Protocols)
	p.SetUnencryptedHTTP2(true)
	return &http.Client{Transport: &http.Transport{Protocols: p}, Timeout: 3 * time.Second}
}

func TestListenerHotReload(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			time.Sleep(300 * time.Millisecond)
		}
		_, _ = io.WriteString(w, "ok "+r.Proto)
	}))
	defer up.Close()
	addrA, addrB := freeAddr(t), freeAddr(t)
	base := basePrefix + `
clusters:
  - name: up
    endpoints: [{address: "` + addr(up) + `"}]
routes:
  - name: r
    match: {pathPrefix: /}
    backends: [{cluster: up}]
`
	cfg, v, err := config.Parse([]byte(base))
	if err != nil {
		t.Fatal(err)
	}
	gw := gateway.New(telemetry.NewMetrics(), nil)
	if err := gw.Apply(cfg, v); err != nil {
		t.Fatal(err)
	}
	defer gw.Close()
	ls := gw.NewListenerSet(5 * time.Second)
	defer ls.Shutdown(context.Background())

	listener := func(name, address string, protocols ...string) config.Listener {
		c := &config.Config{APIVersion: config.APIVersion, Listeners: []config.Listener{{Name: name, Address: address, Protocols: protocols}}}
		c.ApplyDefaults()
		return c.Listeners[0]
	}
	// get returns the client-side protocol and the body.
	get := func(client *http.Client, address, path string) (string, error) {
		resp, err := client.Get("http://" + address + path)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.Proto + " " + string(b), nil
	}
	h1 := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{TLSNextProto: map[string]func(string, *tls.Conn) http.RoundTripper{}}}

	if err := ls.Apply([]config.Listener{listener("public", addrA, "http1")}); err != nil {
		t.Fatal(err)
	}
	if body, err := get(h1, addrA, "/"); err != nil || !strings.Contains(body, "ok") {
		t.Fatalf("http1 before reload: %q %v", body, err)
	}
	if _, err := get(h2cClient(), addrA, "/"); err == nil {
		t.Fatal("h2c should not be enabled yet")
	}

	// In-flight request across the swap must complete.
	inflight := make(chan error, 1)
	go func() {
		body, err := get(h1, addrA, "/slow")
		if err == nil && !strings.Contains(body, "ok") {
			err = io.ErrUnexpectedEOF
		}
		inflight <- err
	}()
	time.Sleep(50 * time.Millisecond)
	if err := ls.Apply([]config.Listener{listener("public", addrA, "http1", "h2c")}); err != nil {
		t.Fatal(err)
	}
	if err := <-inflight; err != nil {
		t.Fatalf("in-flight request broken by listener reload: %v", err)
	}
	if body, err := get(h2cClient(), addrA, "/"); err != nil || !strings.HasPrefix(body, "HTTP/2.0") {
		t.Fatalf("h2c after reload on the same socket: %q %v", body, err)
	}

	// A bind failure aborts the whole change and keeps the old listeners.
	blocker, _ := net.Listen("tcp", "127.0.0.1:0")
	defer blocker.Close()
	err = ls.Apply([]config.Listener{listener("public", addrA, "http1"), listener("admin2", blocker.Addr().String(), "http1")})
	if err == nil {
		t.Fatal("binding an occupied address must fail")
	}
	if _, err := get(h2cClient(), addrA, "/"); err != nil {
		t.Errorf("failed apply must leave the previous listener (with h2c) untouched: %v", err)
	}

	// Add B, remove A.
	if err := ls.Apply([]config.Listener{listener("internal", addrB, "http1")}); err != nil {
		t.Fatal(err)
	}
	if _, err := get(h1, addrB, "/"); err != nil {
		t.Errorf("new listener: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if c, err := net.DialTimeout("tcp", addrA, 500*time.Millisecond); err == nil {
		c.Close()
		t.Error("removed listener's port is still open")
	}

	ls.Shutdown(context.Background())
	if c, err := net.DialTimeout("tcp", addrB, 500*time.Millisecond); err == nil {
		c.Close()
		t.Error("port still open after shutdown")
	}
}

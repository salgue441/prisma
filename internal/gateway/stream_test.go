package gateway_test

import (
	"bufio"
	"context"
	"crypto/tls"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"prisma/internal/config"
	"prisma/internal/gateway"
	"prisma/internal/telemetry"
	"prisma/internal/testutil"
)

// tcpEcho echoes each connection until the client half-closes, then
// half-closes back, so tests can check EOF propagation in both directions.
func tcpEcho(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_, _ = io.Copy(c, c)
				_ = c.(*net.TCPConn).CloseWrite()
			}()
		}
	}()
	return ln.Addr().String()
}

// tlsNamed is a TLS server that answers every connection with its name.
func tlsNamed(t *testing.T, name string, hosts ...string) string {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{testutil.SelfSigned(t, hosts...)}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_, _ = io.WriteString(c, name+"\n")
			}()
		}
	}()
	return ln.Addr().String()
}

type streamGateway struct {
	gw *gateway.Gateway
	ls *gateway.ListenerSet
}

func startStreamGateway(t *testing.T, yml string) *streamGateway {
	t.Helper()
	cfg, v, err := config.Parse([]byte(yml))
	if err != nil {
		t.Fatal(err)
	}
	gw := gateway.New(telemetry.NewMetrics(), nil)
	if err := gw.Apply(cfg, v); err != nil {
		t.Fatal(err)
	}
	ls := gw.NewListenerSet(time.Second)
	if err := ls.Apply(cfg.Listeners); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ls.Shutdown(context.Background())
		gw.Close()
	})
	return &streamGateway{gw, ls}
}

func (s *streamGateway) reload(t *testing.T, yml string) {
	t.Helper()
	cfg, v, err := config.Parse([]byte(yml))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.gw.Apply(cfg, v); err != nil {
		t.Fatal(err)
	}
}

// echoOnce sends msg, half-closes, and returns everything read back.
func echoOnce(t *testing.T, c net.Conn, msg string) string {
	t.Helper()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.WriteString(c, msg); err != nil {
		t.Fatal(err)
	}
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		if err := cw.CloseWrite(); err != nil {
			t.Fatal(err)
		}
	}
	b, err := io.ReadAll(c)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(b)
}

func TestTCPProxyEchoAndHalfClose(t *testing.T) {
	up := tcpEcho(t)
	sg := startStreamGateway(t, `
apiVersion: prisma/v1
listeners:
  - {name: db, address: "127.0.0.1:0", type: tcp}
clusters:
  - name: echo
    endpoints: [{address: "127.0.0.1:1"}, {address: "`+up+`"}]
routes:
  - name: echo
    type: tcp
    listeners: [db]
    backends: [{cluster: echo}]
`)
	// The first endpoint refuses connections; every connection still works
	// because the dial moves on to the next endpoint.
	for range 4 {
		c, err := net.Dial("tcp", sg.ls.Addr("db"))
		if err != nil {
			t.Fatal(err)
		}
		if got := echoOnce(t, c, "hello over tcp"); got != "hello over tcp" {
			t.Fatalf("echo = %q", got)
		}
		c.Close()
	}
}

func TestTCPProxyTLSTermination(t *testing.T) {
	up := tcpEcho(t)
	dir := t.TempDir()
	cert, key := testutil.SelfSignedPEM(t, "db.example.com")
	_ = os.WriteFile(filepath.Join(dir, "tls.crt"), []byte(cert), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "tls.key"), []byte(key), 0o600)
	sg := startStreamGateway(t, `
apiVersion: prisma/v1
listeners:
  - name: db
    address: "127.0.0.1:0"
    type: tcp
    tls: {certificates: [{certFile: "`+filepath.ToSlash(filepath.Join(dir, "tls.crt"))+`", keyFile: "`+filepath.ToSlash(filepath.Join(dir, "tls.key"))+`"}]}
clusters:
  - {name: echo, endpoints: [{address: "`+up+`"}]}
routes:
  - name: db
    type: tcp
    listeners: [db]
    match: {hosts: [db.example.com]}
    backends: [{cluster: echo}]
`)
	c, err := tls.Dial("tcp", sg.ls.Addr("db"), &tls.Config{ServerName: "db.example.com", InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if got := echoOnce(t, c, "decrypted at the gateway"); got != "decrypted at the gateway" {
		t.Fatalf("echo = %q", got)
	}
	if cn := c.ConnectionState().PeerCertificates[0].Subject.CommonName; cn != "db.example.com" {
		t.Fatalf("certificate presented by the gateway = %q", cn)
	}
}

func TestTLSPassthroughRoutesBySNI(t *testing.T) {
	a := tlsNamed(t, "server-a", "a.example.com")
	b := tlsNamed(t, "server-b", "x.b.example.com")
	def := tlsNamed(t, "default", "default.example.com")
	yml := `
apiVersion: prisma/v1
listeners:
  - {name: tls, address: "127.0.0.1:0", type: tls-passthrough}
clusters:
  - {name: a, endpoints: [{address: "` + a + `"}]}
  - {name: b, endpoints: [{address: "` + b + `"}]}
  - {name: def, endpoints: [{address: "` + def + `"}]}
routes:
  - {name: a, type: tcp, listeners: [tls], match: {hosts: [a.example.com]}, backends: [{cluster: a}]}
  - {name: b, type: tcp, listeners: [tls], match: {hosts: ["*.b.example.com"]}, backends: [{cluster: b}]}
  - {name: fallback, type: tcp, listeners: [tls], backends: [{cluster: def}]}
`
	sg := startStreamGateway(t, yml)
	read := func(sni string) (string, string) {
		c, err := tls.Dial("tcp", sg.ls.Addr("tls"), &tls.Config{ServerName: sni, InsecureSkipVerify: true})
		if err != nil {
			return "", "dial: " + err.Error()
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(3 * time.Second))
		line, _ := bufio.NewReader(c).ReadString('\n')
		// The certificate comes from the upstream: TLS was not terminated.
		return strings.TrimSpace(line), c.ConnectionState().PeerCertificates[0].Subject.CommonName
	}
	cases := []struct{ sni, name, cn string }{
		{"a.example.com", "server-a", "a.example.com"},
		{"A.Example.Com", "server-a", "a.example.com"},
		{"x.b.example.com", "server-b", "x.b.example.com"},
		{"deep.x.b.example.com", "server-b", "x.b.example.com"},
		{"b.example.com", "default", "default.example.com"},
		{"other.org", "default", "default.example.com"},
	}
	for _, tc := range cases {
		if name, cn := read(tc.sni); name != tc.name || cn != tc.cn {
			t.Errorf("SNI %s: served by %q with certificate %q, want %q / %q", tc.sni, name, cn, tc.name, tc.cn)
		}
	}

	// Routes reload without touching the listener: drop the fallback and
	// unknown names are refused.
	sg.reload(t, strings.Replace(yml, "  - {name: fallback, type: tcp, listeners: [tls], backends: [{cluster: def}]}\n", "", 1))
	if name, _ := read("other.org"); name != "" {
		t.Fatalf("other.org served by %q after its route was removed", name)
	}
	if name, _ := read("a.example.com"); name != "server-a" {
		t.Fatalf("a.example.com after reload: %q", name)
	}
}

func TestTCPRouteDeniesClientIP(t *testing.T) {
	up := tcpEcho(t)
	sg := startStreamGateway(t, `
apiVersion: prisma/v1
listeners:
  - {name: db, address: "127.0.0.1:0", type: tcp}
clusters:
  - {name: echo, endpoints: [{address: "`+up+`"}]}
routes:
  - name: db
    type: tcp
    listeners: [db]
    authz: {allowIPs: [10.0.0.0/8]}
    backends: [{cluster: echo}]
`)
	c, err := net.Dial("tcp", sg.ls.Addr("db"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	_, _ = io.WriteString(c, "let me in")
	// The gateway closes without reading, which may surface as a reset.
	if got, _ := io.ReadAll(c); len(got) > 0 {
		t.Fatalf("denied client got %q", got)
	}
}

func TestTCPIdleTimeout(t *testing.T) {
	up := tcpEcho(t)
	sg := startStreamGateway(t, `
apiVersion: prisma/v1
listeners:
  - {name: db, address: "127.0.0.1:0", type: tcp}
clusters:
  - {name: echo, endpoints: [{address: "`+up+`"}]}
routes:
  - {name: db, type: tcp, listeners: [db], timeout: 150ms, backends: [{cluster: echo}]}
`)
	c, err := net.Dial("tcp", sg.ls.Addr("db"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	// Activity keeps the connection open past the idle timeout...
	buf := make([]byte, 4)
	for range 4 {
		_, _ = io.WriteString(c, "ping")
		if _, err := io.ReadFull(c, buf); err != nil {
			t.Fatalf("active connection closed: %v", err)
		}
		time.Sleep(60 * time.Millisecond)
	}
	// ...and silence closes it.
	start := time.Now()
	if _, err := c.Read(buf); err == nil {
		t.Fatal("idle connection was not closed")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("idle connection closed after %v", d)
	}
}

func TestStreamListenerDrainsOnShutdown(t *testing.T) {
	up := tcpEcho(t)
	sg := startStreamGateway(t, `
apiVersion: prisma/v1
listeners:
  - {name: db, address: "127.0.0.1:0", type: tcp}
clusters:
  - {name: echo, endpoints: [{address: "`+up+`"}]}
routes:
  - {name: db, type: tcp, listeners: [db], backends: [{cluster: echo}]}
`)
	c, err := net.Dial("tcp", sg.ls.Addr("db"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = io.WriteString(c, "x")
	if _, err := io.ReadFull(c, make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	// Shutdown waits for the open connection until its deadline, then
	// closes it.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	sg.ls.Shutdown(ctx)
	if d := time.Since(start); d < 150*time.Millisecond {
		t.Fatalf("shutdown returned after %v without draining", d)
	}
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("connection still open after the drain deadline")
	}
}

func TestTCPRouteValidation(t *testing.T) {
	cases := map[string]string{
		"http listener": `
listeners: [{name: web, address: ":1"}]
routes: [{name: r, type: tcp, listeners: [web], backends: [{cluster: c}]}]`,
		"no listeners": `
listeners: [{name: db, address: ":1", type: tcp}]
routes: [{name: r, type: tcp, backends: [{cluster: c}]}]`,
		"hosts without SNI": `
listeners: [{name: db, address: ":1", type: tcp}]
routes: [{name: r, type: tcp, listeners: [db], match: {hosts: [a.example.com]}, backends: [{cluster: c}]}]`,
		"http-only field": `
listeners: [{name: db, address: ":1", type: tcp}]
routes: [{name: r, type: tcp, listeners: [db], rewrite: {stripPrefix: /x}, backends: [{cluster: c}]}]`,
		"http route on tcp listener": `
listeners: [{name: db, address: ":1", type: tcp}]
routes: [{name: r, listeners: [db], match: {pathPrefix: /}, backends: [{cluster: c}]}]`,
		"passthrough with tls": `
listeners: [{name: db, address: ":1", type: tls-passthrough, tls: {certificates: [{certFile: a, keyFile: b}]}}]
routes: [{name: r, type: tcp, listeners: [db], backends: [{cluster: c}]}]`,
		"protocols on tcp": `
listeners: [{name: db, address: ":1", type: tcp, protocols: [http1]}]
routes: [{name: r, type: tcp, listeners: [db], backends: [{cluster: c}]}]`,
	}
	for name, body := range cases {
		yml := "apiVersion: prisma/v1\nclusters: [{name: c, endpoints: [{address: \"127.0.0.1:1\"}]}]\n" + body
		if _, _, err := config.Parse([]byte(yml)); err == nil {
			t.Errorf("%s: config accepted", name)
		}
	}
	ok := "apiVersion: prisma/v1\nclusters: [{name: c, endpoints: [{address: \"127.0.0.1:1\"}]}]\n" + `
listeners: [{name: tls, address: ":1", type: tls-passthrough}]
routes: [{name: r, type: tcp, listeners: [tls], priority: 1, timeout: 1m, match: {hosts: ["*.example.com"]},
          authz: {allowIPs: [10.0.0.0/8]}, backends: [{cluster: c}]}]`
	if _, _, err := config.Parse([]byte(ok)); err != nil {
		t.Errorf("valid tcp route rejected: %v", err)
	}
}

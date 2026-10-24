package testutil

import (
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"prisma/internal/demo"
)

// WriteShopDescriptorSet writes the demo shop.v1.Orders descriptor set
// (with its imports) and returns the path.
func WriteShopDescriptorSet(t *testing.T) string {
	t.Helper()
	b, err := demo.ShopDescriptorSet()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "shop.binpb")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// ServeShop runs the demo shop.v1.Orders gRPC service over h2c.
func ServeShop(t *testing.T) string {
	t.Helper()
	h, err := demo.ShopHandler()
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := new(http.Protocols)
	p.SetUnencryptedHTTP2(true)
	srv := &http.Server{Handler: h, Protocols: p}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return ln.Addr().String()
}

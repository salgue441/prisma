// Command demo-upstream runs sample backends for trying Prisma locally:
// a REST orders API (:9001), a tiny GraphQL endpoint (:9002), a gRPC health
// service (:9003) and a gRPC orders service with HTTP annotations (:9004).
// Run several copies with different -name and addresses to see load
// balancing; an empty address disables that backend.
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	crand "crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"flag"
	"io"
	"log"
	"math/big"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"

	"prisma/internal/demo"
)

var instance string

func main() {
	rest := flag.String("rest", ":9001", "REST API address")
	restTLS := flag.String("rest-tls", "", "REST API over TLS (self-signed certificate), e.g. :9443")
	gql := flag.String("graphql", ":9002", "GraphQL address")
	grpcAddr := flag.String("grpc", ":9003", "gRPC address")
	shop := flag.String("shop", ":9004", "gRPC shop.v1.Orders service (h2c), for transcoding")
	writeDescriptors := flag.String("write-descriptors", "", "write the shop.v1.Orders descriptor set to this file and exit")
	flag.StringVar(&instance, "name", "demo-1", "instance name reported in responses")
	flag.Parse()
	if *writeDescriptors != "" {
		b, err := demo.ShopDescriptorSet()
		if err == nil {
			err = os.WriteFile(*writeDescriptors, b, 0o644)
		}
		if err != nil {
			log.Fatal(err)
		}
		return
	}

	done := make(chan struct{})
	for _, s := range []struct {
		addr  string
		serve func(string)
	}{{*rest, serveREST}, {*restTLS, serveRESTTLS}, {*gql, serveGraphQL}, {*grpcAddr, serveGRPC}, {*shop, serveShop}} {
		if s.addr != "" {
			go func() { s.serve(s.addr); done <- struct{}{} }()
		}
	}
	<-done
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Server", "demo-upstream")
	w.Header().Set("X-Instance", instance)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

var restAPI = sync.OnceValue(restMux)

func serveREST(addr string) {
	log.Printf("%s: REST upstream on %s", instance, addr)
	log.Fatal(http.ListenAndServe(addr, restAPI()))
}

// serveRESTTLS serves the REST API over TLS with a fresh self-signed
// certificate for localhost and *.local, to try TLS passthrough.
func serveRESTTLS(addr string) {
	cert, err := selfSigned(instance+".local", "localhost", "*.local")
	if err != nil {
		log.Fatal(err)
	}
	srv := &http.Server{Addr: addr, Handler: restAPI(), TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}}}
	log.Printf("%s: REST upstream over TLS on %s", instance, addr)
	log.Fatal(srv.ListenAndServeTLS("", ""))
}

func restMux() http.Handler {
	var unhealthy atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		if unhealthy.Load() {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "failing"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	// POST /admin/health?ok=false makes the health check fail, to watch the
	// gateway take this instance out of rotation.
	mux.HandleFunc("POST /admin/health", func(w http.ResponseWriter, r *http.Request) {
		unhealthy.Store(r.URL.Query().Get("ok") == "false")
		writeJSON(w, http.StatusOK, map[string]bool{"healthy": !unhealthy.Load()})
	})
	mux.HandleFunc("GET /orders/{id}", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"id":       r.PathValue("id"),
			"instance": instance,
			"customer": map[string]any{
				"name": "Ana Example", "email": "ana@example.com", "ssn": "123-45-6789",
				"payment": map[string]any{"cardNumber": "4111111111111111"},
			},
			"seenConsumer": r.Header.Get("X-Consumer"),
			"requestId":    r.Header.Get("X-Request-Id"),
		})
	})
	mux.HandleFunc("POST /orders", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !json.Valid(body) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"instance": instance, "created": json.RawMessage(body)})
	})
	// GET /products returns a cacheable list.
	mux.HandleFunc("GET /products", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=30")
		items := make([]map[string]any, 50)
		for i := range items {
			items[i] = map[string]any{"id": i + 1, "name": "Product " + strconv.Itoa(i+1), "description": strings.Repeat("A fine product. ", 8)}
		}
		writeJSON(w, http.StatusOK, map[string]any{"instance": instance, "generatedAt": time.Now().Format(time.RFC3339Nano), "items": items})
	})
	// GET /echo reflects the request the upstream received.
	mux.HandleFunc("/echo/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"instance": instance, "method": r.Method, "host": r.Host, "path": r.URL.Path,
			"query": r.URL.RawQuery, "headers": r.Header,
		})
	})
	// GET /slow?ms=500 answers after a delay.
	mux.HandleFunc("GET /slow", func(w http.ResponseWriter, r *http.Request) {
		ms, _ := strconv.Atoi(r.URL.Query().Get("ms"))
		time.Sleep(time.Duration(ms) * time.Millisecond)
		writeJSON(w, http.StatusOK, map[string]any{"instance": instance, "sleptMs": ms})
	})
	// GET /flaky?rate=0.5 fails with 503 at the given rate.
	mux.HandleFunc("GET /flaky", func(w http.ResponseWriter, r *http.Request) {
		rate, err := strconv.ParseFloat(r.URL.Query().Get("rate"), 64)
		if err != nil {
			rate = 0.5
		}
		if rand.Float64() < rate {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"instance": instance, "error": "flaked"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"instance": instance, "status": "ok"})
	})
	return mux
}

// serveGraphQL answers any query with canned data; it exists to show the
// gateway's guard and masking, not to be a GraphQL server.
func serveGraphQL(addr string) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query string `json:"query"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		// Return only the requested fields (by name), so it is visible when the
		// gateway strips a field from the query.
		me := map[string]any{}
		for k, v := range map[string]string{"name": "Ana", "email": "ana@example.com", "ssn": "123-45-6789"} {
			if strings.Contains(req.Query, k) {
				me[k] = v
			}
		}
		data := map[string]any{"me": me}
		if strings.Contains(req.Query, "__schema") {
			data = map[string]any{"__schema": map[string]any{"types": []any{}}}
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": data, "extensions": map[string]string{"receivedQuery": req.Query}})
	})
	log.Printf("%s: GraphQL upstream on %s", instance, addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

func serveGRPC(addr string) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatal(err)
	}
	s := grpc.NewServer()
	hs := health.NewServer()
	hs.SetServingStatus("inventory.v1.Inventory", healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(s, hs)
	reflection.Register(s)
	log.Printf("%s: gRPC upstream on %s", instance, addr)
	log.Fatal(s.Serve(ln))
}

func selfSigned(hosts ...string) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), crand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: hosts[0]},
		DNSNames:     hosts,
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(crand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}

// serveShop serves shop.v1.Orders (see internal/demo) over h2c, for the
// gRPC-JSON transcoding example.
func serveShop(addr string) {
	h, err := demo.ShopHandler()
	if err != nil {
		log.Fatal(err)
	}
	p := new(http.Protocols)
	p.SetUnencryptedHTTP2(true)
	srv := &http.Server{Addr: addr, Handler: h, Protocols: p}
	log.Printf("%s: gRPC shop.v1.Orders on %s", instance, addr)
	log.Fatal(srv.ListenAndServe())
}

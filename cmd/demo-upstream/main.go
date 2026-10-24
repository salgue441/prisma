// Command demo-upstream runs sample backends for trying Prisma locally:
// a REST orders API (:9001), a tiny GraphQL endpoint (:9002) and a gRPC
// health service (:9003). Run several copies with different -name and
// addresses to see load balancing; an empty address disables that backend.
package main

import (
	"encoding/json"
	"flag"
	"io"
	"log"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"
)

var instance string

func main() {
	rest := flag.String("rest", ":9001", "REST API address")
	gql := flag.String("graphql", ":9002", "GraphQL address")
	grpcAddr := flag.String("grpc", ":9003", "gRPC address")
	flag.StringVar(&instance, "name", "demo-1", "instance name reported in responses")
	flag.Parse()

	done := make(chan struct{})
	for _, s := range []struct {
		addr  string
		serve func(string)
	}{{*rest, serveREST}, {*gql, serveGraphQL}, {*grpcAddr, serveGRPC}} {
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

func serveREST(addr string) {
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
	log.Printf("%s: REST upstream on %s", instance, addr)
	log.Fatal(http.ListenAndServe(addr, mux))
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

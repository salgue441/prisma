// Command demo-upstream runs sample backends for trying Prisma locally:
// a REST orders API (:9001), a tiny GraphQL endpoint (:9002) and a gRPC
// health service (:9003).
package main

import (
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

func main() {
	go serveREST(":9001")
	go serveGraphQL(":9002")
	serveGRPC(":9003")
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Server", "demo-upstream")
	_ = json.NewEncoder(w).Encode(v)
}

func serveREST(addr string) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /orders/{id}", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"id": r.PathValue("id"),
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
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, map[string]any{"created": json.RawMessage(body)})
	})
	log.Printf("REST upstream on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

// serveGraphQL answers any query with canned data; it exists to show the
// gateway's guard and masking, not to be a GraphQL server.
func serveGraphQL(addr string) {
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query string `json:"query"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		data := map[string]any{"me": map[string]any{"name": "Ana", "email": "ana@example.com", "ssn": "123-45-6789"}}
		if strings.Contains(req.Query, "__schema") {
			data = map[string]any{"__schema": map[string]any{"types": []any{}}}
		}
		writeJSON(w, map[string]any{"data": data})
	})
	log.Printf("GraphQL upstream on %s", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
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
	log.Printf("gRPC upstream on %s", addr)
	log.Fatal(s.Serve(ln))
}

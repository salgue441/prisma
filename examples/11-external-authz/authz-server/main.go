// Command authz-server is a sample external authorization service. Prisma
// sends it each request's method, path and headers (never the body), plus
// the authenticated identity in X-Prisma-Consumer and X-Prisma-Scopes.
// A 2xx answer allows the request; anything else is returned to the client.
//
// The policy here is data-dependent, which scopes alone can't express:
// tenants may only read their own orders, and only admins may delete.
package main

import (
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"strings"
)

// orderOwner maps order IDs to the tenant that owns them.
var orderOwner = map[string]string{"1": "acme", "2": "acme", "3": "globex"}

// tenantOf maps gateway consumers to tenants.
var tenantOf = map[string]string{"acme-app": "acme", "globex-app": "globex", "support": "*"}

func main() {
	addr := flag.String("addr", ":9100", "listen address")
	flag.Parse()
	http.HandleFunc("/", check)
	log.Printf("authz-server on %s", *addr)
	log.Fatal(http.ListenAndServe(*addr, nil))
}

func check(w http.ResponseWriter, r *http.Request) {
	consumer := r.Header.Get("X-Prisma-Consumer")
	tenant, known := tenantOf[consumer]
	log.Printf("check %s %s consumer=%q", r.Method, r.URL.Path, consumer)

	if !known {
		deny(w, http.StatusForbidden, "unknown consumer")
		return
	}
	if r.Method == http.MethodDelete && !strings.Contains(" "+r.Header.Get("X-Prisma-Scopes")+" ", " orders:admin ") {
		deny(w, http.StatusForbidden, "deleting orders requires orders:admin")
		return
	}
	// The gateway appends the client path to this service's URL.
	if id, ok := strings.CutPrefix(r.URL.Path, "/orders/"); ok && tenant != "*" {
		owner, exists := orderOwner[id]
		if !exists {
			deny(w, http.StatusNotFound, "no such order")
			return
		}
		if owner != tenant {
			deny(w, http.StatusForbidden, "order belongs to another tenant")
			return
		}
	}
	// Allowed: tell the upstream which tenant is calling (the gateway copies
	// only the headers listed in extAuthz.upstreamHeaders).
	w.Header().Set("X-Tenant", tenant)
	w.WriteHeader(http.StatusOK)
}

func deny(w http.ResponseWriter, status int, reason string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Deny-Reason", reason)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": reason})
}

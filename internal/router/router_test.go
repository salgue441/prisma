package router

import (
	"net/http/httptest"
	"testing"

	"prisma/internal/config"
)

func build(t *testing.T, routes []config.Route) *Router {
	t.Helper()
	var rs []*Route
	for i := range routes {
		r, err := NewRoute(&routes[i], i, nil)
		if err != nil {
			t.Fatal(err)
		}
		rs = append(rs, r)
	}
	return New(rs)
}

func TestMatchPrecedence(t *testing.T) {
	rt := build(t, []config.Route{
		{Name: "catch-all", Match: config.Match{PathPrefix: "/"}},
		{Name: "api", Match: config.Match{PathPrefix: "/api"}},
		{Name: "api-users", Match: config.Match{PathPrefix: "/api/users"}},
		{Name: "exact", Match: config.Match{Path: "/api/users/me"}},
		{Name: "regex", Match: config.Match{PathRegex: `^/api/items/\d+$`}},
		{Name: "post-only", Match: config.Match{PathPrefix: "/api/users", Methods: []string{"POST"}}},
		{Name: "host", Match: config.Match{Hosts: []string{"admin.example.com"}, PathPrefix: "/"}},
		{Name: "wild", Match: config.Match{Hosts: []string{"*.example.com"}, PathPrefix: "/"}},
		{Name: "header", Match: config.Match{PathPrefix: "/api", Headers: map[string]string{"X-Canary": "1"}}},
		{Name: "grpc", Match: config.Match{GRPCService: "orders.v1.Orders"}},
		{Name: "grpc-method", Match: config.Match{GRPCService: "orders.v1.Orders", GRPCMethod: "Delete"}},
		{Name: "internal-only", Listeners: []string{"internal"}, Match: config.Match{PathPrefix: "/internal"}},
		{Name: "query", Match: config.Match{PathPrefix: "/api", QueryParams: map[string]string{"v": "2"}}},
	})
	cases := []struct {
		listener, method, host, path, header, want string
	}{
		{"public", "GET", "x.com", "/", "", "catch-all"},
		{"public", "GET", "x.com", "/api/x", "", "api"},
		{"public", "GET", "x.com", "/apix", "", "catch-all"},
		{"public", "GET", "x.com", "/api/users/42", "", "api-users"},
		{"public", "POST", "x.com", "/api/users/42", "", "post-only"},
		{"public", "GET", "x.com", "/api/users/me", "", "exact"},
		{"public", "GET", "x.com", "/api/items/7", "", "regex"},
		{"public", "GET", "x.com", "/api/items/x", "", "api"},
		{"public", "GET", "admin.example.com:8443", "/api", "", "host"},
		{"public", "GET", "shop.example.com", "/", "", "wild"},
		{"public", "GET", "x.com", "/api/x", "1", "header"},
		{"public", "POST", "x.com", "/orders.v1.Orders/Get", "", "grpc"},
		{"public", "POST", "x.com", "/orders.v1.Orders/Delete", "", "grpc-method"},
		{"public", "GET", "x.com", "/internal/x", "", "catch-all"},
		{"internal", "GET", "x.com", "/internal/x", "", "internal-only"},
		{"public", "GET", "x.com", "/api/x?v=2", "", "query"},
		{"public", "GET", "x.com", "/api/x?v=3", "", "api"},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(tc.method, "http://"+tc.host+tc.path, nil)
		req.Host = tc.host
		if tc.header != "" {
			req.Header.Set("X-Canary", tc.header)
		}
		got := rt.Match(tc.listener, req)
		if got == nil || got.Name != tc.want {
			name := "<nil>"
			if got != nil {
				name = got.Name
			}
			t.Errorf("%s %s%s on %s: got %s, want %s", tc.method, tc.host, tc.path, tc.listener, name, tc.want)
		}
	}
}

func TestCleanPath(t *testing.T) {
	for in, want := range map[string]string{
		"":               "/",
		"/a/b":           "/a/b",
		"/a//b":          "/a/b",
		"/a/./b/":        "/a/b/",
		"/a/../../etc":   "/etc",
		"/a/b/..":        "/a/",
		"/.well-known/x": "/.well-known/x",
		"/svc.v1.S/M":    "/svc.v1.S/M",
	} {
		if got := CleanPath(in); got != want {
			t.Errorf("CleanPath(%q) = %q, want %q", in, got, want)
		}
	}
}

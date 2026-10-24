// Package router compiles route matchers into a host-indexed table.
//
// Lookup order: exact host routes, then wildcard host routes (longest suffix
// first), then host-agnostic routes. Within a host bucket routes are ordered
// like nginx locations: exact path > regex (declaration order) > longest
// prefix > catch-all, then by the number of extra predicates, then by
// priority, then by declaration order.
package router

import (
	"cmp"
	"net"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"prisma/internal/config"
)

type Route struct {
	Name    string
	Config  *config.Route
	Handler http.Handler

	listeners map[string]bool
	exact     string
	prefix    string
	re        *regexp.Regexp
	methods   []string
	headers   map[string]string
	query     map[string]string
	index     int
}

// NewRoute compiles cfg. index is the declaration order, used as a tie-breaker.
func NewRoute(cfg *config.Route, index int, h http.Handler) (*Route, error) {
	m := cfg.Match
	r := &Route{Name: cfg.Name, Config: cfg, Handler: h, index: index, headers: m.Headers, query: m.QueryParams}
	if len(cfg.Listeners) > 0 {
		r.listeners = map[string]bool{}
		for _, l := range cfg.Listeners {
			r.listeners[l] = true
		}
	}
	for _, mt := range m.Methods {
		r.methods = append(r.methods, strings.ToUpper(mt))
	}
	switch {
	case m.GRPCMethod != "":
		r.exact = "/" + m.GRPCService + "/" + m.GRPCMethod
	case m.GRPCService != "":
		r.prefix = "/" + m.GRPCService + "/"
	case m.Path != "":
		r.exact = m.Path
	case m.PathPrefix != "":
		r.prefix = m.PathPrefix
	case m.PathRegex != "":
		re, err := regexp.Compile(m.PathRegex)
		if err != nil {
			return nil, err
		}
		r.re = re
	}
	return r, nil
}

func (r *Route) rank() int {
	switch {
	case r.exact != "":
		return 3
	case r.re != nil:
		return 2
	case r.prefix != "":
		return 1
	}
	return 0
}

func (r *Route) matches(listener string, req *http.Request) bool {
	if r.listeners != nil && !r.listeners[listener] {
		return false
	}
	p := req.URL.Path
	switch {
	case r.exact != "" && p != r.exact:
		return false
	case r.prefix != "" && !hasPathPrefix(p, r.prefix):
		return false
	case r.re != nil && !r.re.MatchString(p):
		return false
	}
	if len(r.methods) > 0 && !slices.Contains(r.methods, req.Method) {
		return false
	}
	for k, v := range r.headers {
		got := req.Header.Values(k)
		if len(got) == 0 || (v != "*" && !slices.Contains(got, v)) {
			return false
		}
	}
	if len(r.query) > 0 {
		q := req.URL.Query()
		for k, v := range r.query {
			if got, ok := q[k]; !ok || !slices.Contains(got, v) {
				return false
			}
		}
	}
	return true
}

// hasPathPrefix matches on segment boundaries: "/api" matches "/api" and
// "/api/x" but not "/apix". A prefix ending in "/" matches as-is.
func hasPathPrefix(p, prefix string) bool {
	if !strings.HasPrefix(p, prefix) {
		return false
	}
	return len(p) == len(prefix) || strings.HasSuffix(prefix, "/") || p[len(prefix)] == '/'
}

type wildcardGroup struct {
	suffix string // ".example.com"
	routes []*Route
}

type Router struct {
	routes   []*Route
	exact    map[string][]*Route
	wildcard []wildcardGroup
	any      []*Route
}

func New(routes []*Route) *Router {
	rt := &Router{routes: routes, exact: map[string][]*Route{}}
	wild := map[string][]*Route{}
	for _, r := range routes {
		hosts := r.Config.Match.Hosts
		if len(hosts) == 0 {
			rt.any = append(rt.any, r)
			continue
		}
		for _, h := range hosts {
			h = strings.ToLower(h)
			if s, ok := strings.CutPrefix(h, "*"); ok {
				wild[s] = append(wild[s], r)
			} else {
				rt.exact[h] = append(rt.exact[h], r)
			}
		}
	}
	for s, rs := range wild {
		rt.wildcard = append(rt.wildcard, wildcardGroup{suffix: s, routes: rs})
	}
	slices.SortFunc(rt.wildcard, func(a, b wildcardGroup) int { return cmp.Compare(len(b.suffix), len(a.suffix)) })
	for h := range rt.exact {
		sortRoutes(rt.exact[h])
	}
	for i := range rt.wildcard {
		sortRoutes(rt.wildcard[i].routes)
	}
	sortRoutes(rt.any)
	return rt
}

func sortRoutes(rs []*Route) {
	slices.SortStableFunc(rs, func(a, b *Route) int {
		if c := cmp.Compare(b.rank(), a.rank()); c != 0 {
			return c
		}
		if c := cmp.Compare(len(b.prefix)+len(b.exact), len(a.prefix)+len(a.exact)); c != 0 {
			return c
		}
		if c := cmp.Compare(len(b.methods)+len(b.headers)+len(b.query), len(a.methods)+len(a.headers)+len(a.query)); c != 0 {
			return c
		}
		if c := cmp.Compare(b.Config.Priority, a.Config.Priority); c != 0 {
			return c
		}
		return cmp.Compare(a.index, b.index)
	})
}

// Match returns the best route for req on the given listener, or nil.
func (rt *Router) Match(listener string, req *http.Request) *Route {
	host := HostOnly(req.Host)
	if r := first(rt.exact[host], listener, req); r != nil {
		return r
	}
	for _, g := range rt.wildcard {
		if strings.HasSuffix(host, g.suffix) {
			if r := first(g.routes, listener, req); r != nil {
				return r
			}
		}
	}
	return first(rt.any, listener, req)
}

// Routes returns all compiled routes in declaration order.
func (rt *Router) Routes() []*Route { return rt.routes }

func first(rs []*Route, listener string, req *http.Request) *Route {
	for _, r := range rs {
		if r.matches(listener, req) {
			return r
		}
	}
	return nil
}

// HostOnly lowercases a Host header value and strips any port.
func HostOnly(h string) string {
	if host, _, err := net.SplitHostPort(h); err == nil {
		return strings.ToLower(host)
	}
	return strings.ToLower(strings.Trim(h, "[]"))
}

// CleanPath normalizes dot segments and duplicate slashes so that routing and
// upstreams see the same path. A trailing slash is preserved.
func CleanPath(p string) string {
	if p == "" {
		return "/"
	}
	if !strings.Contains(p, "//") && !strings.Contains(p, "/.") {
		return p
	}
	parts := strings.Split(p, "/")
	out := make([]string, 0, len(parts))
	for _, s := range parts {
		switch s {
		case "", ".":
		case "..":
			if len(out) > 0 {
				out = out[:len(out)-1]
			}
		default:
			out = append(out, s)
		}
	}
	res := "/" + strings.Join(out, "/")
	last := parts[len(parts)-1]
	if (last == "" || last == "." || last == "..") && res != "/" {
		res += "/"
	}
	return res
}

package config

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Validate performs semantic validation. It reports every problem it finds,
// not just the first.
func (c *Config) Validate() error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if c.APIVersion != APIVersion {
		add("apiVersion: unsupported %q (want %q)", c.APIVersion, APIVersion)
	}
	for _, p := range c.Global.TrustedProxies {
		if _, err := netip.ParsePrefix(p); err != nil {
			add("global.trustedProxies: invalid CIDR %q", p)
		}
	}
	for _, name := range c.Global.Use {
		if _, ok := c.Policies[name]; !ok {
			add("global.use: unknown policy %q", name)
		}
	}

	if t := c.Tracing; t != nil {
		if _, _, err := net.SplitHostPort(t.Endpoint); err != nil {
			add("tracing.endpoint must be host:port, got %q", t.Endpoint)
		}
		if t.SampleRatio < 0 || t.SampleRatio > 1 {
			add("tracing.sampleRatio must be between 0 and 1")
		}
	}

	listeners := map[string]bool{}
	addresses := map[string]string{}
	if len(c.Listeners) == 0 {
		add("listeners: at least one listener is required")
	}
	for i, l := range c.Listeners {
		where := fmt.Sprintf("listeners[%d] (%s)", i, l.Name)
		if l.Name == "" {
			add("listeners[%d]: name is required", i)
		} else if listeners[l.Name] {
			add("%s: duplicate listener name", where)
		}
		listeners[l.Name] = true
		if _, _, err := net.SplitHostPort(l.Address); err != nil {
			add("%s: invalid address %q", where, l.Address)
		}
		if other, dup := addresses[l.Address]; dup {
			add("%s: address %s is already used by listener %s", where, l.Address, other)
		}
		addresses[l.Address] = l.Name
		for _, p := range l.Protocols {
			if !slices.Contains([]string{"http1", "http2", "h2c"}, p) {
				add("%s: unknown protocol %q", where, p)
			}
			if p == "http2" && l.TLS == nil {
				add("%s: http2 requires tls (use h2c for cleartext HTTP/2)", where)
			}
		}
		if pp := l.ProxyProtocol; pp != nil {
			for _, cidr := range pp.TrustedCIDRs {
				if _, err := netip.ParsePrefix(cidr); err != nil {
					add("%s: proxyProtocol: invalid CIDR %q", where, cidr)
				}
			}
		}
		if t := l.TLS; t != nil {
			if len(t.Certificates) == 0 {
				add("%s: tls requires at least one certificate", where)
			}
			if !slices.Contains([]string{"1.2", "1.3"}, t.MinVersion) {
				add("%s: tls.minVersion must be 1.2 or 1.3", where)
			}
			if !slices.Contains([]string{"", "none", "request", "require"}, t.ClientAuth) {
				add("%s: tls.clientAuth must be none, request or require", where)
			}
			if t.ClientAuth != "" && t.ClientAuth != "none" && t.ClientCAFile == "" {
				add("%s: tls.clientAuth requires clientCAFile", where)
			}
		}
	}

	clusters := map[string]bool{}
	clusterProto := map[string]string{}
	for i, cl := range c.Clusters {
		clusterProto[cl.Name] = cl.Protocol
		where := fmt.Sprintf("clusters[%d] (%s)", i, cl.Name)
		if cl.Name == "" {
			add("clusters[%d]: name is required", i)
		} else if clusters[cl.Name] {
			add("%s: duplicate cluster name", where)
		}
		clusters[cl.Name] = true
		if !slices.Contains([]string{"http1", "http2", "h2c"}, cl.Protocol) {
			add("%s: unknown protocol %q", where, cl.Protocol)
		}
		if cl.Protocol == "h2c" && cl.TLS != nil {
			add("%s: h2c cannot be combined with tls", where)
		}
		switch d := cl.Discovery; {
		case d == nil && len(cl.Endpoints) == 0:
			add("%s: endpoints or discovery is required", where)
		case d != nil && len(cl.Endpoints) > 0:
			add("%s: endpoints and discovery are mutually exclusive", where)
		case d != nil:
			if d.Host == "" {
				add("%s: discovery.host is required", where)
			}
			switch d.Type {
			case "dns":
				if d.Port <= 0 || d.Port > 65535 {
					add("%s: discovery type dns requires a port", where)
				}
			case "srv":
			default:
				add("%s: discovery.type must be dns or srv", where)
			}
			if d.Interval < time.Second {
				add("%s: discovery.interval must be at least 1s", where)
			}
		}
		for _, ep := range cl.Endpoints {
			if _, _, err := net.SplitHostPort(ep.Address); err != nil {
				add("%s: endpoint address %q must be host:port", where, ep.Address)
			}
			if ep.Weight < 0 {
				add("%s: endpoint weight must be positive", where)
			}
		}
		if !slices.Contains([]string{"round_robin", "weighted_round_robin", "least_conn", "p2c_ewma", "ring_hash"}, cl.LoadBalancer.Policy) {
			add("%s: unknown load balancer policy %q", where, cl.LoadBalancer.Policy)
		}
		if h := cl.LoadBalancer.HashOn; h != "" && h != "ip" && !strings.HasPrefix(h, "header:") && !strings.HasPrefix(h, "cookie:") {
			add("%s: loadBalancer.hashOn must be ip, header:<name> or cookie:<name>", where)
		}
		if hc := cl.HealthCheck; hc != nil && !slices.Contains([]string{"http", "grpc", "tcp"}, hc.Type) {
			add("%s: unknown health check type %q", where, hc.Type)
		}
		if cb := cl.CircuitBreaker; cb != nil && (cb.ErrorRatio <= 0 || cb.ErrorRatio > 1) {
			add("%s: circuitBreaker.errorRatio must be in (0, 1]", where)
		}
	}

	for name, p := range c.Policies {
		c.validatePolicy("policies."+name, &p, add)
	}

	consumers := map[string]bool{}
	hmacKeyOwner := map[string]string{}
	for i, cs := range c.Consumers {
		if cs.Name == "" {
			add("consumers[%d]: name is required", i)
		} else if consumers[cs.Name] {
			add("consumers[%d]: duplicate consumer %q", i, cs.Name)
		}
		consumers[cs.Name] = true
		for id, secret := range cs.HMACKeys {
			if hmacKeyOwner[id] != "" {
				add("consumers[%d]: hmac key id %q is also used by %q", i, id, hmacKeyOwner[id])
			}
			hmacKeyOwner[id] = cs.Name
			if len(secret) < 32 {
				add("consumers[%d]: hmac key %q must be at least 32 bytes", i, id)
			}
		}
	}

	routes := map[string]bool{}
	if len(c.Routes) == 0 {
		add("routes: at least one route is required")
	}
	for i := range c.Routes {
		r := &c.Routes[i]
		where := fmt.Sprintf("routes[%d] (%s)", i, r.Name)
		if r.Name == "" {
			add("routes[%d]: name is required", i)
		} else if routes[r.Name] {
			add("%s: duplicate route name", where)
		}
		routes[r.Name] = true
		if !slices.Contains([]string{"", "http", "graphql", "grpc"}, r.Type) {
			add("%s: unknown type %q", where, r.Type)
		}
		for _, l := range r.Listeners {
			if !listeners[l] {
				add("%s: unknown listener %q", where, l)
			}
		}
		for _, u := range r.Use {
			if _, ok := c.Policies[u]; !ok {
				add("%s: unknown policy %q", where, u)
			}
		}
		m := r.Match
		set := 0
		for _, s := range []string{m.Path, m.PathPrefix, m.PathRegex} {
			if s != "" {
				set++
			}
		}
		if set > 1 {
			add("%s: match accepts only one of path, pathPrefix, pathRegex", where)
		}
		if m.PathRegex != "" {
			if _, err := regexp.Compile(m.PathRegex); err != nil {
				add("%s: invalid pathRegex: %v", where, err)
			}
		}
		if (m.GRPCService != "" || m.GRPCMethod != "") && set > 0 {
			add("%s: grpcService/grpcMethod cannot be combined with path matchers", where)
		}
		if m.GRPCMethod != "" && m.GRPCService == "" {
			add("%s: grpcMethod requires grpcService", where)
		}
		for _, h := range m.Hosts {
			if strings.Contains(h, "*") && !strings.HasPrefix(h, "*.") {
				add("%s: wildcard hosts must look like *.example.com", where)
			}
		}

		if r.Redirect != nil {
			if len(r.Backends) > 0 {
				add("%s: redirect routes cannot have backends", where)
			}
			if !slices.Contains([]int{301, 302, 303, 307, 308}, r.Redirect.Status) {
				add("%s: redirect.status must be one of 301, 302, 303, 307, 308", where)
			}
			if r.Redirect.PathPrefixReplace != "" && m.PathPrefix == "" {
				add("%s: redirect.pathPrefixReplace requires match.pathPrefix", where)
			}
		} else if len(r.Backends) == 0 {
			add("%s: backends are required unless the route redirects", where)
		}
		for _, b := range r.Backends {
			if !clusters[b.Cluster] {
				add("%s: unknown cluster %q", where, b.Cluster)
			}
			if b.Weight < 0 {
				add("%s: backend weight must be positive", where)
			}
			if r.GRPCWeb && clusters[b.Cluster] && clusterProto[b.Cluster] == "http1" {
				add("%s: grpcWeb requires an HTTP/2 cluster (protocol h2c or http2), %q is http1", where, b.Cluster)
			}
		}
		if mi := r.Mirror; mi != nil {
			if !clusters[mi.Cluster] {
				add("%s: mirror: unknown cluster %q", where, mi.Cluster)
			}
			if mi.Percent <= 0 || mi.Percent > 100 {
				add("%s: mirror.percent must be in (0, 100]", where)
			}
		}
		if rw := r.Rewrite; rw != nil {
			if rw.PathRegex != "" {
				if _, err := regexp.Compile(rw.PathRegex); err != nil {
					add("%s: invalid rewrite.pathRegex: %v", where, err)
				}
			}
			if rw.ReplacePrefix != "" && rw.StripPrefix == "" {
				add("%s: rewrite.replacePrefix requires stripPrefix", where)
			}
		}
		if rt := r.Retries; rt != nil {
			if rt.Attempts < 0 || rt.Attempts > 10 {
				add("%s: retries.attempts must be between 0 and 10", where)
			}
			for _, on := range rt.On {
				if !slices.Contains([]string{"connect-failure", "5xx", "502", "503", "504", "unavailable"}, on) {
					add("%s: unknown retry condition %q", where, on)
				}
			}
		}
		c.validatePolicy(where, &r.Policy, add)
	}

	c.checkRedirectLoops(add)
	return errors.Join(errs...)
}

func (c *Config) validatePolicy(where string, p *Policy, add func(string, ...any)) {
	if a := p.AuthN; a != nil {
		if !slices.Contains([]string{"", "any", "all"}, a.Mode) {
			add("%s: authn.mode must be any or all", where)
		}
		if a.JWT == nil && a.Introspection == nil && a.APIKey == nil && a.MTLS == nil && a.HMAC == nil {
			add("%s: authn requires at least one provider (jwt, introspection, apiKey, mtls, hmac)", where)
		}
		if in := a.Introspection; in != nil {
			if u, err := url.Parse(in.Endpoint); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
				add("%s: authn.introspection.endpoint must be an http(s) URL", where)
			}
			if in.ClientID == "" {
				add("%s: authn.introspection.clientId is required", where)
			}
		}
		if j := a.JWT; j != nil {
			if (j.JWKSURI == "") == (j.Secret == "") {
				add("%s: authn.jwt requires exactly one of jwksUri or secret", where)
			}
			for _, alg := range j.Algorithms {
				if strings.EqualFold(alg, "none") {
					add("%s: authn.jwt algorithm none is not allowed", where)
				}
			}
		}
	}
	if z := p.AuthZ; z != nil {
		for _, cidr := range append(slices.Clone(z.AllowIPs), z.DenyIPs...) {
			if _, err := netip.ParsePrefix(cidr); err != nil {
				add("%s: authz: invalid CIDR %q", where, cidr)
			}
		}
	}
	for i, rl := range p.RateLimit {
		w := fmt.Sprintf("%s: rateLimit[%d]", where, i)
		if rl.Limit <= 0 || rl.Period <= 0 {
			add("%s: limit and period must be positive", w)
		}
		if len(rl.Key) == 0 {
			add("%s: key is required", w)
		}
		for _, k := range rl.Key {
			switch {
			case slices.Contains([]string{"client_ip", "consumer", "route", "grpc_method", "graphql_operation"}, k),
				strings.HasPrefix(k, "header:"), strings.HasPrefix(k, "claim:"):
			default:
				add("%s: unknown key descriptor %q", w, k)
			}
		}
		if !slices.Contains([]string{"", "local", "redis"}, rl.Backend) {
			add("%s: backend must be local or redis", w)
		}
		if rl.Backend == "redis" && c.Redis == nil {
			add("%s: redis backend requires a top-level redis section", w)
		}
		if !slices.Contains([]string{"", "local", "allow", "deny"}, rl.OnBackendFailure) {
			add("%s: onBackendFailure must be local, allow or deny", w)
		}
		if !slices.Contains([]string{"", "graphql"}, rl.Cost) {
			add("%s: cost must be empty or graphql", w)
		}
	}
	if g := p.GraphQL; g != nil {
		if g.Schema != "" && g.SchemaFile != "" {
			add("%s: graphql: schema and schemaFile are mutually exclusive", where)
		}
		if len(g.FieldAuthz) > 0 && g.Schema == "" && g.SchemaFile == "" {
			add("%s: graphql.fieldAuthz requires schema or schemaFile", where)
		}
		for i, fa := range g.FieldAuthz {
			typ, field, ok := strings.Cut(fa.Coordinate, ".")
			if !ok || typ == "" || field == "" {
				add("%s: graphql.fieldAuthz[%d]: coordinate must look like Type.field or Type.*", where, i)
			}
			if !slices.Contains([]string{"", "reject", "strip"}, fa.OnDeny) {
				add("%s: graphql.fieldAuthz[%d]: onDeny must be reject or strip", where, i)
			}
			if len(fa.Scopes) == 0 {
				add("%s: graphql.fieldAuthz[%d]: scopes are required", where, i)
			}
		}
	}
	if ea := p.ExtAuthz; ea != nil {
		if u, err := url.Parse(ea.URL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			add("%s: extAuthz.url must be an http(s) URL", where)
		}
	}
	if cors := p.CORS; cors != nil {
		if len(cors.AllowOrigins) == 0 {
			add("%s: cors.allowOrigins is required", where)
		}
		for _, o := range cors.AllowOrigins {
			switch {
			case o == "*":
				if cors.AllowCredentials {
					add("%s: cors: allowCredentials cannot be combined with origin *", where)
				}
			case !strings.HasPrefix(o, "http://") && !strings.HasPrefix(o, "https://"):
				add("%s: cors origin %q must start with http:// or https://", where, o)
			case strings.Count(o, "*") > 1 || (strings.Contains(o, "*") && !strings.Contains(o, "://*.")):
				add("%s: cors origin %q: wildcards must look like https://*.example.com", where, o)
			}
		}
	}
	if cp := p.Compression; cp != nil {
		for _, a := range cp.Algorithms {
			if a != "gzip" && a != "zstd" {
				add("%s: compression: unknown algorithm %q (gzip, zstd)", where, a)
			}
		}
	}
	if m := p.Masking; m != nil {
		if !slices.Contains([]string{"", "closed", "open"}, m.OnFailure) {
			add("%s: masking.onFailure must be closed or open", where)
		}
		for i, rule := range m.Rules {
			if (rule.Selector == "") == (rule.Detector == "") {
				add("%s: masking.rules[%d] requires exactly one of selector or detector", where, i)
			}
			if rule.Detector != "" && !slices.Contains([]string{"email", "credit_card", "ssn"}, rule.Detector) {
				add("%s: masking.rules[%d]: unknown detector %q", where, i, rule.Detector)
			}
			if !slices.Contains([]string{"redact", "partial", "hash", "drop"}, rule.Strategy) {
				add("%s: masking.rules[%d]: unknown strategy %q", where, i, rule.Strategy)
			}
			if rule.Strategy == "drop" && rule.Detector != "" {
				add("%s: masking.rules[%d]: drop only works with selectors", where, i)
			}
			if rule.Selector != "" && !strings.HasPrefix(rule.Selector, "$") {
				add("%s: masking.rules[%d]: selector must start with $", where, i)
			}
		}
	}
}

// checkRedirectLoops detects static redirect rules that redirect to a path
// matched by another redirect rule which eventually points back.
func (c *Config) checkRedirectLoops(add func(string, ...any)) {
	target := map[string]string{}
	for _, r := range c.Routes {
		if r.Redirect == nil || r.Redirect.Path == "" {
			continue
		}
		src := r.Match.Path
		if src == "" {
			continue
		}
		target[src] = r.Redirect.Path
	}
	for start := range target {
		seen := map[string]bool{start: true}
		for cur := target[start]; cur != ""; cur = target[cur] {
			if seen[cur] {
				add("routes: redirect loop detected starting at %q", start)
				break
			}
			seen[cur] = true
		}
	}
}

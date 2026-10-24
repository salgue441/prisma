// Package authz enforces scope, consumer and IP rules on authenticated requests.
package authz

import (
	"net/netip"
	"slices"

	"prisma/internal/config"
	"prisma/internal/gwerr"
	"prisma/internal/pipeline"
	"prisma/internal/telemetry"
)

type Filter struct {
	route     string
	scopes    []string
	methods   map[string][]string
	consumers []string
	allow     []netip.Prefix
	deny      []netip.Prefix
	metrics   *telemetry.Metrics
}

func New(route string, cfg *config.AuthZ, m *telemetry.Metrics) *Filter {
	f := &Filter{route: route, scopes: cfg.Scopes, methods: cfg.Methods, consumers: cfg.Consumers, metrics: m}
	for _, s := range cfg.AllowIPs {
		f.allow = append(f.allow, netip.MustParsePrefix(s))
	}
	for _, s := range cfg.DenyIPs {
		f.deny = append(f.deny, netip.MustParsePrefix(s))
	}
	return f
}

func (f *Filter) Name() string { return "authz" }

func (f *Filter) OnRequest(c *pipeline.Context) pipeline.Responder {
	ip := c.ClientIP
	if slices.ContainsFunc(f.deny, func(p netip.Prefix) bool { return p.Contains(ip) }) {
		return f.forbid("ip_denied", "client ip %s denied", ip)
	}
	if len(f.allow) > 0 && !slices.ContainsFunc(f.allow, func(p netip.Prefix) bool { return p.Contains(ip) }) {
		return f.forbid("ip_not_allowed", "client ip %s not in allow list", ip)
	}

	required := f.scopes
	// Methods is keyed by HTTP method or by full gRPC method path.
	if extra := f.methods[c.Request.Method]; len(extra) > 0 {
		required = append(slices.Clone(required), extra...)
	}
	if extra := f.methods[c.Request.URL.Path]; len(extra) > 0 {
		required = append(slices.Clone(required), extra...)
	}
	if len(required) == 0 && len(f.consumers) == 0 {
		return nil
	}
	p := c.Principal
	if p == nil {
		f.metrics.AuthFailures.WithLabelValues(f.route, "unauthenticated").Inc()
		return gwerr.Unauthenticated("authentication required")
	}
	if len(f.consumers) > 0 && !slices.Contains(f.consumers, p.Consumer) {
		return f.forbid("consumer_not_allowed", "consumer %q not allowed", p.Consumer)
	}
	for _, s := range required {
		if !p.HasScope(s) {
			return f.forbid("missing_scope", "missing scope %q", s)
		}
	}
	return nil
}

func (f *Filter) forbid(reason, format string, args ...any) pipeline.Responder {
	f.metrics.AuthFailures.WithLabelValues(f.route, reason).Inc()
	return gwerr.Forbidden("access denied").WithReason(format, args...)
}

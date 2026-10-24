// Package ratelimit enforces request rate limits using GCRA, either per
// gateway instance (local) or cluster-wide (Redis).
package ratelimit

import (
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"prisma/internal/config"
	"prisma/internal/gwerr"
	"prisma/internal/pipeline"
	"prisma/internal/telemetry"
)

type limit struct {
	name      string
	keys      []string
	params    Params
	store     Store
	fallback  string // local | allow | deny
	graphCost bool
}

type Filter struct {
	route   string
	limits  []limit
	local   Store
	metrics *telemetry.Metrics
}

// New builds the filter. redisStore may be nil when no limit uses Redis.
func New(route string, cfgs []config.RateLimit, local, redisStore Store, m *telemetry.Metrics) *Filter {
	f := &Filter{route: route, local: local, metrics: m}
	for i, c := range cfgs {
		burst := c.Burst
		if burst <= 0 {
			burst = c.Limit
		}
		name := c.Name
		if name == "" {
			name = strconv.Itoa(i)
		}
		l := limit{
			name:      name,
			keys:      c.Key,
			params:    Params{Interval: c.Period / time.Duration(c.Limit), Burst: burst},
			store:     local,
			fallback:  c.OnBackendFailure,
			graphCost: c.Cost == "graphql",
		}
		if l.params.Interval <= 0 {
			l.params.Interval = time.Nanosecond
		}
		if c.Backend == "redis" && redisStore != nil {
			l.store = redisStore
		}
		f.limits = append(f.limits, l)
	}
	return f
}

func (f *Filter) Name() string { return "ratelimit" }

func (f *Filter) OnRequest(c *pipeline.Context) pipeline.Responder {
	var tightest *Decision
	var tightestLimit int
	for i := range f.limits {
		l := &f.limits[i]
		cost := 1
		if l.graphCost && c.GraphQL != nil && c.GraphQL.Cost > 1 {
			cost = c.GraphQL.Cost
		}
		key := f.route + "|" + l.name + "|" + buildKey(c, l.keys)
		d, err := l.store.Allow(c.Request.Context(), key, l.params, cost)
		if err != nil {
			d = f.onStoreError(c, l, key, cost, err)
		}
		result := "allowed"
		if !d.Allowed {
			result = "limited"
		}
		f.metrics.RateLimit.WithLabelValues(f.route, l.name, result).Inc()
		if tightest == nil || !d.Allowed || (tightest.Allowed && d.Remaining < tightest.Remaining) {
			tightest, tightestLimit = &d, l.params.Burst
		}
		if !d.Allowed {
			break
		}
	}
	if tightest == nil {
		return nil
	}
	setHeaders(c, tightest, tightestLimit)
	if !tightest.Allowed {
		return gwerr.RateLimited(tightest.RetryAfter)
	}
	return nil
}

func (f *Filter) onStoreError(c *pipeline.Context, l *limit, key string, cost int, err error) Decision {
	slog.Warn("rate limit backend error", "route", f.route, "limit", l.name, "fallback", l.fallback, "err", err)
	f.metrics.RateLimit.WithLabelValues(f.route, l.name, "backend_error").Inc()
	switch l.fallback {
	case "allow":
		return Decision{Allowed: true, Remaining: l.params.Burst}
	case "deny":
		return Decision{Allowed: false, RetryAfter: time.Second}
	default:
		d, _ := f.local.Allow(c.Request.Context(), key, l.params, cost)
		return d
	}
}

// setHeaders emits the IETF RateLimit header fields for the tightest limit.
// The advertised limit is the burst capacity, so Remaining never exceeds it.
func setHeaders(c *pipeline.Context, d *Decision, lim int) {
	if c.ResponseHeaders == nil {
		c.ResponseHeaders = http.Header{}
	}
	h := c.ResponseHeaders
	h.Set("RateLimit-Limit", strconv.Itoa(lim))
	h.Set("RateLimit-Remaining", strconv.Itoa(max(d.Remaining, 0)))
	h.Set("RateLimit-Reset", strconv.Itoa(int((d.ResetAfter+time.Second-1)/time.Second)))
}

// buildKey joins descriptor values. An anonymous caller is keyed by client IP
// so "consumer" limits still apply before authentication.
func buildKey(c *pipeline.Context, descriptors []string) string {
	var b strings.Builder
	for i, d := range descriptors {
		if i > 0 {
			b.WriteByte('|')
		}
		switch {
		case d == "client_ip":
			b.WriteString(c.Var("client_ip"))
		case d == "consumer":
			if c.Principal != nil && c.Principal.Consumer != "" {
				b.WriteString(c.Principal.Consumer)
			} else {
				b.WriteString("anon:" + c.Var("client_ip"))
			}
		case d == "route":
			b.WriteString(c.RouteName)
		case d == "grpc_method":
			b.WriteString(c.Request.URL.Path)
		case d == "graphql_operation":
			if c.GraphQL != nil {
				b.WriteString(c.GraphQL.OperationName)
			}
		case strings.HasPrefix(d, "header:"):
			b.WriteString(c.Var("header." + strings.TrimPrefix(d, "header:")))
		case strings.HasPrefix(d, "claim:"):
			b.WriteString(c.Var("claim." + strings.TrimPrefix(d, "claim:")))
		}
	}
	return b.String()
}

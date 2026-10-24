// Package extauthz asks an external HTTP service whether a request may
// proceed (Envoy HTTP ext_authz semantics), so policy engines such as OPA or
// bespoke services can make decisions the built-in filters cannot.
package extauthz

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"prisma/internal/config"
	"prisma/internal/gwerr"
	"prisma/internal/pipeline"
	"prisma/internal/protocol"
	"prisma/internal/telemetry"
)

const (
	defaultTimeout = 500 * time.Millisecond
	maxDenyBody    = 64 << 10
)

var alwaysForwarded = []string{"Authorization", "Cookie", "X-Request-Id"}

type Filter struct {
	route    string
	base     *url.URL
	client   *http.Client
	failOpen bool
	forward  []string
	upstream []string
	toClient []string
	metrics  *telemetry.Metrics
}

func New(route string, cfg *config.ExtAuthz, m *telemetry.Metrics) *Filter {
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = defaultTimeout
	}
	base, _ := url.Parse(strings.TrimSuffix(cfg.URL, "/"))
	return &Filter{
		route:    route,
		base:     base,
		client:   &http.Client{Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		failOpen: cfg.FailOpen,
		forward:  append(append([]string{}, alwaysForwarded...), cfg.AllowedHeaders...),
		upstream: cfg.UpstreamHeaders,
		toClient: cfg.ClientHeaders,
		metrics:  m,
	}
}

func (f *Filter) Name() string { return "ext_authz" }

func (f *Filter) OnRequest(c *pipeline.Context) pipeline.Responder {
	r := c.Request
	u := *f.base
	u.Path = f.base.Path + r.URL.Path
	u.RawQuery = r.URL.RawQuery
	check, err := http.NewRequestWithContext(r.Context(), r.Method, u.String(), nil)
	if err != nil {
		return gwerr.Internal().WithReason("ext_authz: %v", err)
	}
	for _, h := range f.forward {
		for _, v := range r.Header.Values(h) {
			check.Header.Add(h, v)
		}
	}
	check.Header.Set("X-Forwarded-For", c.Var("client_ip"))
	check.Header.Set("X-Forwarded-Host", r.Host)
	check.Header.Set("X-Forwarded-Uri", r.URL.RequestURI())
	proto := "http"
	if r.TLS != nil {
		proto = "https"
	}
	check.Header.Set("X-Forwarded-Proto", proto)
	check.Header.Set("X-Forwarded-Method", r.Method)
	if p := c.Principal; p != nil {
		check.Header.Set("X-Prisma-Consumer", p.Consumer)
		check.Header.Set("X-Prisma-Principal", p.ID)
		check.Header.Set("X-Prisma-Scopes", strings.Join(p.Scopes, " "))
	}

	resp, err := f.client.Do(check)
	if err != nil {
		if r.Context().Err() != nil {
			return nil // client gone or deadline hit; the proxy reports it
		}
		return f.unavailable(err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		f.metrics.ExtAuthz.WithLabelValues(f.route, "allowed").Inc()
		for _, h := range f.upstream {
			if vs := resp.Header.Values(h); len(vs) > 0 {
				r.Header[http.CanonicalHeaderKey(h)] = vs
			}
		}
		return nil
	case resp.StatusCode >= 500:
		return f.unavailable(errStatus(resp.StatusCode))
	}
	f.metrics.ExtAuthz.WithLabelValues(f.route, "denied").Inc()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxDenyBody))
	d := &denial{status: resp.StatusCode, header: http.Header{}, body: body, contentType: resp.Header.Get("Content-Type")}
	for _, h := range f.toClient {
		if vs := resp.Header.Values(h); len(vs) > 0 {
			d.header[http.CanonicalHeaderKey(h)] = vs
		}
	}
	c.Error = "ext_authz denied with " + resp.Status
	return d
}

func (f *Filter) unavailable(err error) pipeline.Responder {
	if f.failOpen {
		f.metrics.ExtAuthz.WithLabelValues(f.route, "failed_open").Inc()
		slog.Warn("ext_authz unavailable, failing open", "route", f.route, "err", err)
		return nil
	}
	f.metrics.ExtAuthz.WithLabelValues(f.route, "error").Inc()
	return gwerr.Unavailable("authorization service unavailable").WithReason("ext_authz: %v", err)
}

type errStatus int

func (e errStatus) Error() string { return "authorization service returned " + http.StatusText(int(e)) }

// denial relays the authorization service's answer. HTTP clients get its
// status and body verbatim; gRPC and GraphQL clients get the equivalent
// error in their own wire format.
type denial struct {
	status      int
	header      http.Header
	body        []byte
	contentType string
}

func (d *denial) Respond(w http.ResponseWriter, kind protocol.Kind) {
	for k, vs := range d.header {
		w.Header()[k] = vs
	}
	if (kind != protocol.HTTP && kind != protocol.WebSocket) || len(d.body) == 0 {
		e := gwerr.Forbidden("access denied")
		if d.status == http.StatusUnauthorized {
			e = gwerr.Unauthenticated("authentication required")
		}
		e.HTTPStatus = d.status
		e.Respond(w, kind)
		return
	}
	if d.contentType != "" {
		w.Header().Set("Content-Type", d.contentType)
	}
	w.WriteHeader(d.status)
	_, _ = io.Copy(w, bytes.NewReader(d.body))
}

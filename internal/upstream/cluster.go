// Package upstream manages clusters of backend endpoints: connection pools,
// load balancing, active health checks, passive outlier ejection, circuit
// breaking and retries.
package upstream

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"prisma/internal/config"
	"prisma/internal/gwerr"
	"prisma/internal/pipeline"
	"prisma/internal/telemetry"
)

// maxEjection caps outlier ejection backoff.
const maxEjection = 5 * time.Minute

type Cluster struct {
	Name   string
	Config config.Cluster
	Scheme string
	// DefaultHost is the Host header sent upstream when neither the route
	// nor preserveHost sets one. Empty means "use the endpoint address".
	DefaultHost string

	// set is swapped atomically when discovery changes the endpoints, so
	// requests always see a consistent endpoints+balancer pair.
	set atomic.Pointer[endpointSet]

	transport *http.Transport
	breaker   *Breaker
	metrics   *telemetry.Metrics
	resolver  Resolver

	active   atomic.Int64
	retrying atomic.Int64

	mu     sync.Mutex // guards the lifecycle fields below
	ctx    context.Context
	cancel context.CancelFunc
	checks map[*Endpoint]context.CancelFunc
}

type endpointSet struct {
	endpoints []*Endpoint
	balancer  Balancer
}

// Resolver is the subset of *net.Resolver used for service discovery.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
	LookupSRV(ctx context.Context, service, proto, name string) (string, []*net.SRV, error)
}

func NewCluster(cfg config.Cluster, m *telemetry.Metrics) (*Cluster, error) {
	c := &Cluster{Name: cfg.Name, Config: cfg, Scheme: "http", metrics: m, resolver: net.DefaultResolver,
		checks: map[*Endpoint]context.CancelFunc{}}
	if cfg.TLS != nil || cfg.Protocol == "http2" {
		c.Scheme = "https"
	}
	t, err := newTransport(cfg)
	if err != nil {
		return nil, fmt.Errorf("cluster %s: %w", cfg.Name, err)
	}
	c.transport = t
	if d := cfg.Discovery; d != nil {
		c.DefaultHost = d.Host
		if d.Type == "dns" {
			c.DefaultHost = net.JoinHostPort(d.Host, strconv.Itoa(d.Port))
		}
	}
	c.setEndpoints(cfg.Endpoints)
	if cfg.CircuitBreaker != nil {
		c.breaker = newBreaker(*cfg.CircuitBreaker, func(open bool) {
			v := 0.0
			if open {
				v = 1
			}
			m.CircuitOpen.WithLabelValues(cfg.Name).Set(v)
		})
	}
	return c, nil
}

func newTransport(cfg config.Cluster) (*http.Transport, error) {
	dialer := &net.Dialer{Timeout: cfg.ConnectTimeout, KeepAlive: 30 * time.Second}
	t := &http.Transport{
		DialContext:           dialer.DialContext,
		MaxIdleConnsPerHost:   cfg.MaxIdleConnsPerHost,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
		// The gateway forwards bytes as-is; never decompress transparently.
		DisableCompression: true,
		HTTP2: &http.HTTP2Config{
			SendPingTimeout: 30 * time.Second,
			PingTimeout:     15 * time.Second,
		},
	}
	p := new(http.Protocols)
	switch cfg.Protocol {
	case "http2":
		p.SetHTTP2(true)
	case "h2c":
		p.SetUnencryptedHTTP2(true)
	default:
		p.SetHTTP1(true)
	}
	t.Protocols = p

	tc := &tls.Config{MinVersion: tls.VersionTLS12}
	if cfg.Discovery != nil {
		// Discovered endpoints are IPs; verify certificates against the
		// service name instead.
		tc.ServerName = cfg.Discovery.Host
	}
	if u := cfg.TLS; u != nil && u.ServerName != "" {
		tc.ServerName = u.ServerName
	}
	if u := cfg.TLS; u != nil {
		tc.InsecureSkipVerify = u.InsecureSkipVerify
		if u.CAFile != "" {
			pem, err := os.ReadFile(u.CAFile)
			if err != nil {
				return nil, err
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pem) {
				return nil, fmt.Errorf("no certificates in %s", u.CAFile)
			}
			tc.RootCAs = pool
		}
		if u.CertFile != "" {
			cert, err := tls.LoadX509KeyPair(u.CertFile, u.KeyFile)
			if err != nil {
				return nil, err
			}
			tc.Certificates = []tls.Certificate{cert}
		}
	}
	t.TLSClientConfig = tc
	return t, nil
}

// Endpoints returns the current endpoint list. Callers must not modify it.
func (c *Cluster) Endpoints() []*Endpoint { return c.set.Load().endpoints }

// setEndpoints installs a new endpoint list. Endpoints whose address and
// weight are unchanged keep their identity, so health, ejection and latency
// state survive discovery refreshes. When the cluster is running, health
// checks are started for new endpoints and stopped for removed ones.
func (c *Cluster) setEndpoints(list []config.Endpoint) {
	c.mu.Lock()
	defer c.mu.Unlock()
	prev := map[string]*Endpoint{}
	if s := c.set.Load(); s != nil {
		for _, e := range s.endpoints {
			prev[e.Address] = e
		}
	}
	next := make([]*Endpoint, 0, len(list))
	for _, e := range list {
		old, ok := prev[e.Address]
		switch {
		case ok && old.Weight == e.Weight:
			next = append(next, old)
			delete(prev, e.Address)
			continue
		case ok:
			// Weight changed: new identity (weights are read without locks),
			// but keep the observed health.
			ep := &Endpoint{Address: e.Address, Weight: e.Weight}
			ep.healthy.Store(old.Healthy())
			next = append(next, ep)
		default:
			ep := &Endpoint{Address: e.Address, Weight: e.Weight}
			ep.healthy.Store(true)
			next = append(next, ep)
		}
	}
	c.set.Store(&endpointSet{endpoints: next, balancer: newBalancer(c.Config.LoadBalancer, next)})
	if c.ctx == nil {
		return // not started; Start reports and checks everything
	}
	for _, old := range prev {
		if !slices.ContainsFunc(next, func(e *Endpoint) bool { return e.Address == old.Address }) {
			c.metrics.EndpointHealthy.DeleteLabelValues(c.Name, old.Address)
		}
		c.stopCheckLocked(old)
	}
	for _, ep := range next {
		if _, running := c.checks[ep]; !running {
			c.startCheckLocked(ep)
		}
	}
}

// Start launches health checks and, for discovered clusters, resolves the
// endpoints once synchronously and then keeps refreshing them.
func (c *Cluster) Start() {
	c.mu.Lock()
	c.ctx, c.cancel = context.WithCancel(context.Background())
	for _, ep := range c.Endpoints() {
		c.startCheckLocked(ep)
	}
	ctx := c.ctx
	c.mu.Unlock()
	if c.Config.Discovery != nil {
		c.refresh(ctx)
		go c.discoveryLoop(ctx)
	}
}

func (c *Cluster) startCheckLocked(ep *Endpoint) {
	c.reportHealth(ep)
	if c.Config.HealthCheck == nil {
		c.checks[ep] = func() {}
		return
	}
	ctx, cancel := context.WithCancel(c.ctx)
	c.checks[ep] = cancel
	go c.checkLoop(ctx, ep)
}

func (c *Cluster) stopCheckLocked(ep *Endpoint) {
	if cancel, ok := c.checks[ep]; ok {
		cancel()
		delete(c.checks, ep)
	}
}

// Close stops health checks and discovery and, after grace, closes idle
// connections. In-flight requests keep their connections until they finish.
func (c *Cluster) Close(grace time.Duration) {
	c.mu.Lock()
	if c.cancel != nil {
		c.cancel()
	}
	c.checks = map[*Endpoint]context.CancelFunc{}
	c.mu.Unlock()
	time.AfterFunc(grace, c.transport.CloseIdleConnections)
	for _, ep := range c.Endpoints() {
		c.metrics.EndpointHealthy.DeleteLabelValues(c.Name, ep.Address)
	}
}

func (c *Cluster) BreakerOpen() bool { return c.breaker != nil && c.breaker.Open() }

// pick selects a healthy endpoint, relaxing constraints in order until one
// qualifies:
//  1. not ejected and not yet tried by this request
//  2. not yet tried, ignoring ejections (panic routing: outlier detection
//     must never take down the whole cluster)
//  3. any healthy endpoint, so retries still work when every endpoint was
//     already tried (e.g. single-endpoint clusters)
//
// Endpoints failing active health checks are never picked.
func (c *Cluster) pick(r *http.Request, tried []*Endpoint) *Endpoint {
	now := time.Now()
	set := c.set.Load()
	filters := []func(*Endpoint) bool{
		func(e *Endpoint) bool { return !e.Ejected(now) && !slices.Contains(tried, e) },
		func(e *Endpoint) bool { return !slices.Contains(tried, e) },
		func(*Endpoint) bool { return true },
	}
	avail := make([]*Endpoint, 0, len(set.endpoints))
	for _, ok := range filters {
		for _, e := range set.endpoints {
			if e.Healthy() && ok(e) {
				avail = append(avail, e)
			}
		}
		if len(avail) > 0 {
			return set.balancer.Pick(r, avail)
		}
	}
	return nil
}

// RetryPolicy is the compiled form of a route's retries config.
type RetryPolicy struct {
	Attempts           int
	Budget             float64
	Backoff            time.Duration
	AllowNonIdempotent bool
	on                 map[string]bool
}

func NewRetryPolicy(cfg *config.Retries) *RetryPolicy {
	if cfg == nil || cfg.Attempts == 0 {
		return nil
	}
	p := &RetryPolicy{Attempts: cfg.Attempts, Budget: cfg.Budget, Backoff: cfg.Backoff,
		AllowNonIdempotent: cfg.AllowNonIdempotent, on: map[string]bool{}}
	for _, o := range cfg.On {
		p.on[o] = true
	}
	return p
}

func (p *RetryPolicy) eligible(r *http.Request) bool {
	if p == nil {
		return false
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodPut, http.MethodDelete, http.MethodTrace:
	default:
		if !p.AllowNonIdempotent {
			return false
		}
	}
	return r.Body == nil || r.Body == http.NoBody || r.GetBody != nil
}

type retryKey struct{}

func WithRetryPolicy(ctx context.Context, p *RetryPolicy) context.Context {
	if p == nil {
		return ctx
	}
	return context.WithValue(ctx, retryKey{}, p)
}

// RoundTrip implements http.RoundTripper. The ReverseProxy hands it a request
// without a concrete host; each attempt fills in the picked endpoint.
func (c *Cluster) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	pc := pipeline.From(ctx)
	policy, _ := ctx.Value(retryKey{}).(*RetryPolicy)
	maxAttempts := 1
	if policy.eligible(req) {
		maxAttempts += policy.Attempts
	}
	c.active.Add(1)
	defer c.active.Add(-1)
	retries := int64(0)
	defer func() { c.retrying.Add(-retries) }()

	var tried []*Endpoint
	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			retries++
			c.retrying.Add(1)
			c.metrics.Retries.WithLabelValues(c.Name).Inc()
			if err := sleepBackoff(ctx, policy.Backoff, attempt); err != nil {
				return nil, err
			}
			if req.GetBody != nil {
				body, err := req.GetBody()
				if err != nil {
					return nil, err
				}
				req.Body = body
			}
		}
		ep := c.pick(req, tried)
		if ep == nil {
			return nil, gwerr.Unavailable("no healthy upstream").WithReason("cluster %s has no available endpoints", c.Name)
		}
		tried = append(tried, ep)

		var done func(bool)
		if c.breaker != nil {
			d, err := c.breaker.Allow()
			if err != nil {
				return nil, err
			}
			done = d
		}

		// The tracer comes from the server span, so tracing costs nothing
		// when it is disabled (noop provider).
		actx, span := trace.SpanFromContext(ctx).TracerProvider().Tracer("prisma").Start(ctx, "upstream "+c.Name,
			trace.WithSpanKind(trace.SpanKindClient),
			trace.WithAttributes(
				attribute.String("server.address", ep.Address),
				attribute.String("prisma.cluster", c.Name),
				attribute.Int("prisma.attempt", attempt+1),
			))
		out := req.WithContext(actx)
		u := *req.URL
		u.Scheme, u.Host = c.Scheme, ep.Address
		out.URL = &u
		if span.SpanContext().IsValid() {
			telemetry.Propagator.Inject(actx, propagation.HeaderCarrier(out.Header))
		}

		ep.inflight.Add(1)
		start := time.Now()
		resp, err := c.transport.RoundTrip(out)
		latency := time.Since(start)

		failure, retryable := classify(resp, err, policy)
		recordAttempt(span, resp, err, failure)
		if err == nil {
			ep.observeLatency(latency)
		}
		c.outlier(ep, failure)
		if done != nil {
			done(!failure)
		}
		outcome := "success"
		if failure {
			outcome = "failure"
		}
		c.metrics.UpstreamRequests.WithLabelValues(c.Name, ep.Address, outcome).Inc()
		c.metrics.UpstreamDuration.WithLabelValues(c.Name).Observe(latency.Seconds())
		if pc != nil {
			pc.Cluster, pc.Endpoint, pc.Attempts, pc.UpstreamLatency = c.Name, ep.Address, attempt+1, latency
		}

		canRetry := retryable && attempt+1 < maxAttempts && c.budgetAllows(policy)
		if !canRetry {
			if err != nil {
				ep.inflight.Add(-1)
				span.End()
				return nil, err
			}
			// The span covers the whole response stream, not just headers.
			resp.Body = &trackedBody{ReadCloser: resp.Body, done: func() {
				ep.inflight.Add(-1)
				span.End()
			}}
			return resp, nil
		}
		ep.inflight.Add(-1)
		span.End()
		if resp != nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
			resp.Body.Close()
		}
	}
}

func recordAttempt(span trace.Span, resp *http.Response, err error, failure bool) {
	if !span.IsRecording() {
		return
	}
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return
	}
	span.SetAttributes(attribute.Int("http.response.status_code", resp.StatusCode))
	if failure {
		span.SetStatus(codes.Error, resp.Status)
	}
}

// classify reports whether the attempt counts as an upstream failure (for
// breakers and outlier detection) and whether the policy allows a retry.
func classify(resp *http.Response, err error, p *RetryPolicy) (failure, retryable bool) {
	on := func(k string) bool { return p != nil && p.on[k] }
	if err != nil {
		switch {
		case errors.Is(err, context.Canceled):
			return false, false
		case errors.Is(err, context.DeadlineExceeded):
			return true, false
		}
		return true, on("connect-failure")
	}
	code := resp.StatusCode
	grpcUnavailable := resp.Header.Get("Grpc-Status") == "14"
	failure = code == 502 || code == 503 || code == 504 || grpcUnavailable
	retryable = (on("5xx") && code >= 500) || on(strconv.Itoa(code)) || (on("unavailable") && grpcUnavailable)
	return failure, retryable
}

// budgetAllows caps concurrent retries to a fraction of active requests so a
// struggling cluster is not hit by a retry storm.
func (c *Cluster) budgetAllows(p *RetryPolicy) bool {
	limit := max(int64(float64(c.active.Load())*p.Budget), 3)
	return c.retrying.Load() < limit
}

func (c *Cluster) outlier(ep *Endpoint, failure bool) {
	od := c.Config.OutlierDetection
	if od == nil {
		return
	}
	if !failure {
		ep.consecFailures.Store(0)
		return
	}
	if int(ep.consecFailures.Add(1)) < od.ConsecutiveFailures {
		return
	}
	ep.consecFailures.Store(0)
	now := time.Now()
	eps := c.Endpoints()
	ejected := 0
	for _, e := range eps {
		if e.Ejected(now) {
			ejected++
		}
	}
	if (ejected+1)*100 > od.MaxEjectionPercent*len(eps) {
		return
	}
	n := ep.ejections.Add(1)
	d := od.BaseEjection << min(n-1, 10)
	if d > maxEjection || d <= 0 {
		d = maxEjection
	}
	ep.ejectedUntil.Store(now.Add(d).UnixNano())
}

func sleepBackoff(ctx context.Context, base time.Duration, attempt int) error {
	d := base << min(attempt-1, 4)
	d = d/2 + time.Duration(rand.Int64N(int64(d/2)+1))
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// trackedBody runs done exactly once when the body is closed, so in-flight
// counts cover the full duration of streaming responses.
type trackedBody struct {
	io.ReadCloser
	once sync.Once
	done func()
}

func (b *trackedBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.done)
	return err
}

// EndpointStatus is a point-in-time view for the Admin API.
type EndpointStatus struct {
	Address  string  `json:"address"`
	Weight   int     `json:"weight"`
	Healthy  bool    `json:"healthy"`
	Ejected  bool    `json:"ejected"`
	Inflight int64   `json:"inflight"`
	EWMAms   float64 `json:"ewmaMs"`
}

func (c *Cluster) Status() []EndpointStatus {
	now := time.Now()
	eps := c.Endpoints()
	out := make([]EndpointStatus, 0, len(eps))
	for _, e := range eps {
		out = append(out, EndpointStatus{
			Address: e.Address, Weight: e.Weight, Healthy: e.Healthy(), Ejected: e.Ejected(now),
			Inflight: e.Inflight(), EWMAms: float64(e.EWMA()) / float64(time.Millisecond),
		})
	}
	return out
}

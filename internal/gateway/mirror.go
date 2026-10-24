package gateway

import (
	"bytes"
	"context"
	"io"
	"math/rand/v2"
	"net/http"

	"prisma/internal/config"
	"prisma/internal/pipeline"
	"prisma/internal/telemetry"
	"prisma/internal/upstream"
)

// hopHeaders are connection-scoped and must not be copied to the shadow.
var hopHeaders = []string{"Connection", "Keep-Alive", "Proxy-Connection", "Te", "Trailer", "Transfer-Encoding", "Upgrade"}

type mirror struct {
	route   string
	cfg     *config.Mirror
	cluster *upstream.Cluster
	slots   chan struct{}
	metrics *telemetry.Metrics
}

func newMirror(route string, cfg *config.Mirror, cl *upstream.Cluster, m *telemetry.Metrics) *mirror {
	return &mirror{route: route, cfg: cfg, cluster: cl, slots: make(chan struct{}, cfg.MaxConcurrent), metrics: m}
}

// maybeSend shadows the request when it is sampled. It runs after the filter
// chain, so only authenticated, authorized and rewritten requests are
// mirrored. The request body is buffered (up to maxBodyBytes) and restored
// for the primary upstream; larger bodies are not mirrored.
func (m *mirror) maybeSend(c *pipeline.Context) {
	if m.cfg.Percent < 100 && rand.Float64()*100 >= m.cfg.Percent {
		return
	}
	if c.Protocol.IsStreaming() {
		return // long-lived streams cannot be replayed
	}
	r := c.Request
	var body []byte
	if r.Body != nil && r.Body != http.NoBody {
		limit := int64(m.cfg.MaxBodyBytes)
		buf, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
		if err != nil {
			r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(buf), r.Body))
			m.metrics.Mirror.WithLabelValues(m.route, "skipped").Inc()
			return
		}
		if int64(len(buf)) > limit {
			r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(buf), r.Body))
			m.metrics.Mirror.WithLabelValues(m.route, "skipped").Inc()
			return
		}
		body = buf
		r.Body = io.NopCloser(bytes.NewReader(buf))
		r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(buf)), nil }
	}
	select {
	case m.slots <- struct{}{}:
	default:
		m.metrics.Mirror.WithLabelValues(m.route, "dropped").Inc()
		return
	}
	shadow := m.build(c, body)
	go func() {
		defer func() { <-m.slots }()
		ctx, cancel := context.WithTimeout(context.Background(), m.cfg.Timeout)
		defer cancel()
		resp, err := m.cluster.RoundTrip(shadow.WithContext(ctx))
		if err != nil {
			m.metrics.Mirror.WithLabelValues(m.route, "error").Inc()
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		m.metrics.Mirror.WithLabelValues(m.route, "sent").Inc()
	}()
}

// build snapshots the request before the primary proxy mutates it. The
// shadow context deliberately carries no pipeline.Context, so shadow results
// never leak into the primary request's logs, metrics or retries.
func (m *mirror) build(c *pipeline.Context, body []byte) *http.Request {
	r := c.Request
	u := *r.URL
	u.Scheme, u.Host = m.cluster.Scheme, m.cluster.Name
	out := &http.Request{
		Method:        r.Method,
		URL:           &u,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        r.Header.Clone(),
		Host:          shadowHost(r.Host),
		ContentLength: int64(len(body)),
	}
	if body != nil {
		out.Body = io.NopCloser(bytes.NewReader(body))
		out.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	}
	for _, h := range hopHeaders {
		out.Header.Del(h)
	}
	out.Header.Set("X-Request-Id", c.RequestID)
	out.Header.Set("X-Forwarded-For", c.Var("client_ip"))
	out.Header.Set("X-Prisma-Shadow", "true")
	if p := c.Principal; p != nil {
		out.Header.Set("X-Prisma-Consumer", p.Consumer)
		out.Header.Set("X-Prisma-Principal", p.ID)
		out.Header.Set("X-Prisma-Auth-Method", p.Method)
	}
	return out
}

func shadowHost(host string) string {
	h, port := host, ""
	if i := lastColon(host); i >= 0 {
		h, port = host[:i], host[i:]
	}
	return h + "-shadow" + port
}

// lastColon finds a port separator, ignoring colons inside IPv6 brackets.
func lastColon(host string) int {
	for i := len(host) - 1; i >= 0; i-- {
		switch host[i] {
		case ':':
			return i
		case ']':
			return -1
		}
	}
	return -1
}

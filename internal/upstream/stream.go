package upstream

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"time"

	"prisma/internal/gwerr"
)

// StreamConn is an upstream connection for connection-level (L4) proxying.
// Close it to release the endpoint.
type StreamConn struct {
	net.Conn
	Endpoint string
	Attempts int
	release  func()
}

func (s *StreamConn) Close() error {
	err := s.Conn.Close()
	if s.release != nil {
		s.release()
		s.release = nil
	}
	return err
}

// CloseWrite half-closes the upstream side when the client finished sending.
func (s *StreamConn) CloseWrite() error {
	if cw, ok := s.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return errors.ErrUnsupported
}

// DialStream connects to an endpoint for L4 proxying, using the cluster's
// balancer, health state, outlier ejection and circuit breaker. Connection
// failures move on to another endpoint, up to attempts tries. client keys
// ring_hash balancing with hashOn: ip. With cluster tls set, the connection
// is TLS to the upstream (TLS origination).
func (c *Cluster) DialStream(ctx context.Context, client netip.Addr, attempts int) (*StreamConn, error) {
	attempts = max(attempts, 1)
	// The balancers pick by request; L4 only has the client address.
	req := (&http.Request{Header: http.Header{}, RemoteAddr: client.String()}).WithContext(ctx)
	dialer := &net.Dialer{Timeout: c.Config.ConnectTimeout, KeepAlive: 30 * time.Second}
	var tried []*Endpoint
	var lastErr error
	for attempt := range attempts {
		ep := c.pick(req, tried)
		if ep == nil {
			break
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
		start := time.Now()
		conn, err := dialer.DialContext(ctx, "tcp", ep.Address)
		if err == nil && c.Config.TLS != nil {
			conn, err = c.tlsClient(ctx, conn, ep.Address)
		}
		c.outlier(ep, err != nil)
		if done != nil {
			done(err == nil)
		}
		outcome := "success"
		if err != nil {
			outcome = "failure"
		}
		c.metrics.UpstreamRequests.WithLabelValues(c.Name, ep.Address, outcome).Inc()
		if err != nil {
			lastErr = err
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			continue
		}
		ep.observeLatency(time.Since(start))
		ep.inflight.Add(1)
		return &StreamConn{Conn: conn, Endpoint: ep.Address, Attempts: attempt + 1, release: func() {
			ep.inflight.Add(-1)
		}}, nil
	}
	if lastErr == nil {
		return nil, gwerr.Unavailable("no healthy upstream").WithReason("cluster %s has no available endpoints", c.Name)
	}
	return nil, errors.Join(gwerr.BadGateway("upstream connection failed"), lastErr)
}

func (c *Cluster) tlsClient(ctx context.Context, conn net.Conn, addr string) (net.Conn, error) {
	tc := c.transport.TLSClientConfig.Clone()
	if tc.ServerName == "" {
		tc.ServerName, _, _ = net.SplitHostPort(addr)
	}
	t := tls.Client(conn, tc)
	if err := t.HandshakeContext(ctx); err != nil {
		conn.Close()
		return nil, err
	}
	return t, nil
}

package upstream

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

// checkLoop probes one endpoint until ctx is canceled. Endpoints start
// healthy and flip only after the configured number of consecutive results.
func (c *Cluster) checkLoop(ctx context.Context, ep *Endpoint) {
	hc := c.Config.HealthCheck
	var conn *grpc.ClientConn
	defer func() {
		if conn != nil {
			conn.Close()
		}
	}()
	// Jitter the first probe so replicas don't probe in lockstep.
	timer := time.NewTimer(time.Duration(rand.Int64N(int64(hc.Interval))))
	defer timer.Stop()
	okRun, failRun := 0, 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		var ok bool
		ok, conn = c.probe(ctx, ep, conn)
		if ctx.Err() != nil {
			return // stopped mid-probe; don't report a stale endpoint
		}
		if ok {
			okRun, failRun = okRun+1, 0
			if !ep.Healthy() && okRun >= hc.HealthyThreshold {
				ep.healthy.Store(true)
				slog.Info("endpoint healthy", "cluster", c.Name, "endpoint", ep.Address)
			}
		} else {
			okRun, failRun = 0, failRun+1
			if ep.Healthy() && failRun >= hc.UnhealthyThreshold {
				ep.healthy.Store(false)
				slog.Warn("endpoint unhealthy", "cluster", c.Name, "endpoint", ep.Address)
			}
		}
		c.reportHealth(ep)
		timer.Reset(hc.Interval)
	}
}

func (c *Cluster) reportHealth(ep *Endpoint) {
	v := 0.0
	if ep.Healthy() {
		v = 1
	}
	c.metrics.EndpointHealthy.WithLabelValues(c.Name, ep.Address).Set(v)
}

func (c *Cluster) probe(ctx context.Context, ep *Endpoint, conn *grpc.ClientConn) (bool, *grpc.ClientConn) {
	hc := c.Config.HealthCheck
	ctx, cancel := context.WithTimeout(ctx, hc.Timeout)
	defer cancel()
	switch hc.Type {
	case "tcp":
		d := net.Dialer{}
		nc, err := d.DialContext(ctx, "tcp", ep.Address)
		if err != nil {
			return false, nil
		}
		nc.Close()
		return true, nil
	case "grpc":
		if conn == nil {
			creds := insecure.NewCredentials()
			if c.Scheme == "https" {
				creds = credentials.NewTLS(c.transport.TLSClientConfig.Clone())
			}
			var err error
			conn, err = grpc.NewClient(ep.Address, grpc.WithTransportCredentials(creds))
			if err != nil {
				return false, nil
			}
		}
		resp, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{Service: hc.Service})
		return err == nil && resp.GetStatus() == healthpb.HealthCheckResponse_SERVING, conn
	default:
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Scheme+"://"+ep.Address+hc.Path, nil)
		if err != nil {
			return false, nil
		}
		req.Header.Set("User-Agent", "prisma-healthcheck")
		resp, err := c.transport.RoundTrip(req)
		if err != nil {
			return false, nil
		}
		resp.Body.Close()
		return resp.StatusCode >= 200 && resp.StatusCode < 400, nil
	}
}

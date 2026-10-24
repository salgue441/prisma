package upstream

import (
	"cmp"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"time"

	"prisma/internal/config"
)

const (
	consulWait       = 5 * time.Minute
	consulMaxBackoff = 30 * time.Second
)

// consulWatcher keeps a cluster's endpoints in sync with a Consul service
// using blocking queries: each request returns when the service's health
// changes (or after consulWait), so updates arrive within moments.
type consulWatcher struct {
	c      *Cluster
	cfg    config.ConsulDiscovery
	base   *url.URL
	client *http.Client
}

type consulEntry struct {
	Node struct {
		Address string `json:"Address"`
	} `json:"Node"`
	Service struct {
		Address string   `json:"Address"`
		Port    int      `json:"Port"`
		Tags    []string `json:"Tags"`
		Weights struct {
			Passing int `json:"Passing"`
			Warning int `json:"Warning"`
		} `json:"Weights"`
	} `json:"Service"`
	Checks []struct {
		Status string `json:"Status"` // passing | warning | critical | maintenance
	} `json:"Checks"`
}

func newConsulWatcher(c *Cluster) (*consulWatcher, error) {
	cfg := *c.Config.Discovery.Consul
	if cfg.Address == "" {
		cfg.Address = "http://127.0.0.1:8500"
	}
	base, err := url.Parse(cfg.Address)
	if err != nil {
		return nil, fmt.Errorf("consul discovery: %w", err)
	}
	tc := &tls.Config{MinVersion: tls.VersionTLS12}
	if cfg.CAFile != "" {
		pem, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("consul discovery: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("consul discovery: no certificates in %s", cfg.CAFile)
		}
		tc.RootCAs = pool
	}
	// No client timeout: blocking queries are bounded by the wait parameter
	// and the request context.
	return &consulWatcher{c: c, cfg: cfg, base: base, client: &http.Client{Transport: &http.Transport{TLSClientConfig: tc, Proxy: http.ProxyFromEnvironment}}}, nil
}

// run polls until ctx ends. Errors keep the last known endpoints, so a
// Consul outage never empties a working cluster.
// sync performs one non-blocking query and applies it, returning the index
// to continue from (0 after an error).
func (w *consulWatcher) sync(ctx context.Context) uint64 {
	eps, index, err := w.query(ctx, 0)
	if err != nil {
		slog.Warn("consul discovery failed", "cluster", w.c.Name, "service", w.cfg.Service, "err", err)
		return 0
	}
	w.apply(eps)
	return index
}

func (w *consulWatcher) apply(eps []config.Endpoint) {
	if sameEndpoints(w.c.Endpoints(), eps) {
		return
	}
	if len(eps) == 0 {
		slog.Warn("consul discovery returned no healthy instances", "cluster", w.c.Name, "service", w.cfg.Service)
	}
	w.c.setEndpoints(eps)
	slog.Info("endpoints updated", "cluster", w.c.Name, "endpoints", len(eps), "source", "consul")
}

func (w *consulWatcher) run(ctx context.Context, index uint64) {
	backoff := time.Second
	for ctx.Err() == nil {
		eps, next, err := w.query(ctx, index)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Warn("consul discovery failed; keeping last known endpoints",
				"cluster", w.c.Name, "service", w.cfg.Service, "endpoints", len(w.c.Endpoints()), "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, consulMaxBackoff)
			continue
		}
		backoff = time.Second
		// An index that goes backwards (Consul restored from a snapshot)
		// restarts the blocking sequence; 0 is never a valid wait index.
		if next < index || next == 0 {
			next = 0
		}
		index = next
		w.apply(eps)
	}
}

// query performs one (blocking, when index > 0) health query.
func (w *consulWatcher) query(ctx context.Context, index uint64) ([]config.Endpoint, uint64, error) {
	u := w.base.JoinPath("/v1/health/service/", w.cfg.Service)
	q := url.Values{}
	if !w.cfg.AllowWarning {
		q.Set("passing", "true")
	}
	if w.cfg.Tag != "" {
		q.Set("tag", w.cfg.Tag)
	}
	for k, v := range map[string]string{"dc": w.cfg.Datacenter, "ns": w.cfg.Namespace, "partition": w.cfg.Partition} {
		if v != "" {
			q.Set(k, v)
		}
	}
	timeout := resolveTimeout
	if index > 0 {
		q.Set("index", strconv.FormatUint(index, 10))
		q.Set("wait", consulWait.String())
		// Consul adds up to wait/16 of jitter to blocking queries.
		timeout = consulWait + consulWait/16 + resolveTimeout
	}
	u.RawQuery = q.Encode()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, 0, err
	}
	if w.cfg.Token != "" {
		req.Header.Set("X-Consul-Token", w.cfg.Token)
	}
	resp, err := w.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, 0, fmt.Errorf("consul: %s: %s", resp.Status, b)
	}
	var entries []consulEntry
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		return nil, 0, fmt.Errorf("consul: %w", err)
	}
	next, _ := strconv.ParseUint(resp.Header.Get("X-Consul-Index"), 10, 64)
	return w.endpoints(entries), next, nil
}

func (w *consulWatcher) endpoints(entries []consulEntry) []config.Endpoint {
	var out []config.Endpoint
	for _, e := range entries {
		state := "passing"
		for _, c := range e.Checks {
			switch {
			case c.Status == "critical" || c.Status == "maintenance":
				state = "critical"
			case c.Status == "warning" && state == "passing":
				state = "warning"
			}
		}
		weight := e.Service.Weights.Passing
		switch state {
		case "critical":
			continue
		case "warning":
			if !w.cfg.AllowWarning {
				continue
			}
			weight = e.Service.Weights.Warning
		}
		host := e.Service.Address
		if host == "" {
			host = e.Node.Address
		}
		if host == "" || e.Service.Port == 0 {
			continue
		}
		out = append(out, config.Endpoint{Address: net.JoinHostPort(host, strconv.Itoa(e.Service.Port)), Weight: max(weight, 1)})
	}
	slices.SortFunc(out, func(a, b config.Endpoint) int { return cmp.Compare(a.Address, b.Address) })
	return slices.CompactFunc(out, func(a, b config.Endpoint) bool { return a.Address == b.Address })
}

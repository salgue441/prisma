package upstream

import (
	"cmp"
	"context"
	"log/slog"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	"prisma/internal/config"
)

const resolveTimeout = 5 * time.Second

func (c *Cluster) discoveryLoop(ctx context.Context) {
	t := time.NewTicker(c.Config.Discovery.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.refresh(ctx)
		}
	}
}

// refresh re-resolves the cluster and installs changed results. A failed
// lookup keeps the last known endpoints: a DNS blip must not empty a
// healthy cluster. A successful empty answer is applied (scaled to zero).
func (c *Cluster) refresh(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, resolveTimeout)
	defer cancel()
	eps, err := c.resolve(ctx)
	if err != nil {
		slog.Warn("service discovery failed; keeping last known endpoints",
			"cluster", c.Name, "host", c.Config.Discovery.Host, "endpoints", len(c.Endpoints()), "err", err)
		return
	}
	if sameEndpoints(c.Endpoints(), eps) {
		return
	}
	if len(eps) == 0 {
		slog.Warn("service discovery returned no endpoints", "cluster", c.Name, "host", c.Config.Discovery.Host)
	}
	c.setEndpoints(eps)
	slog.Info("endpoints updated", "cluster", c.Name, "endpoints", len(eps))
}

func (c *Cluster) resolve(ctx context.Context) ([]config.Endpoint, error) {
	d := c.Config.Discovery
	var out []config.Endpoint
	switch d.Type {
	case "srv":
		_, records, err := c.resolver.LookupSRV(ctx, "", "", d.Host)
		if err != nil {
			return nil, err
		}
		for _, r := range records {
			target := strings.TrimSuffix(r.Target, ".")
			// SRV weight 0 means "rarely"; it still deserves some traffic.
			out = append(out, config.Endpoint{Address: net.JoinHostPort(target, strconv.Itoa(int(r.Port))), Weight: max(int(r.Weight), 1)})
		}
	default:
		ips, err := c.resolver.LookupNetIP(ctx, "ip", d.Host)
		if err != nil {
			return nil, err
		}
		for _, ip := range ips {
			out = append(out, config.Endpoint{Address: net.JoinHostPort(ip.Unmap().String(), strconv.Itoa(d.Port)), Weight: 1})
		}
	}
	slices.SortFunc(out, func(a, b config.Endpoint) int { return cmp.Compare(a.Address, b.Address) })
	out = slices.CompactFunc(out, func(a, b config.Endpoint) bool { return a.Address == b.Address })
	return out, nil
}

func sameEndpoints(cur []*Endpoint, next []config.Endpoint) bool {
	if len(cur) != len(next) {
		return false
	}
	have := make(map[string]int, len(cur))
	for _, e := range cur {
		have[e.Address] = e.Weight
	}
	for _, e := range next {
		if w, ok := have[e.Address]; !ok || w != e.Weight {
			return false
		}
	}
	return true
}

package upstream

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"slices"
	"strconv"

	"prisma/internal/config"
	"prisma/internal/k8s"
)

const serviceNameLabel = "kubernetes.io/service-name"

// k8sWatcher keeps a cluster's endpoints in sync with the EndpointSlices of
// a Kubernetes Service.
type k8sWatcher struct {
	c        *Cluster
	cfg      config.KubernetesDiscovery
	port     int
	informer *k8s.Informer
}

type endpointSlice struct {
	Endpoints []struct {
		Addresses  []string `json:"addresses"`
		Conditions struct {
			Ready       *bool `json:"ready"`
			Terminating *bool `json:"terminating"`
		} `json:"conditions"`
	} `json:"endpoints"`
	Ports []struct {
		Name *string `json:"name"`
		Port *int    `json:"port"`
	} `json:"ports"`
}

func newK8sWatcher(c *Cluster) (*k8sWatcher, error) {
	d := c.Config.Discovery
	w := &k8sWatcher{c: c, cfg: *d.Kubernetes, port: d.Port}
	if w.cfg.Namespace == "" {
		ns, err := k8s.InClusterNamespace()
		if err != nil {
			return nil, fmt.Errorf("kubernetes discovery: namespace not set and not running in a pod: %w", err)
		}
		w.cfg.Namespace = ns
	}
	client, err := k8s.NewClient(k8s.Options{APIServer: w.cfg.APIServer, TokenFile: w.cfg.TokenFile, CAFile: w.cfg.CAFile})
	if err != nil {
		return nil, fmt.Errorf("kubernetes discovery: %w", err)
	}
	w.informer = &k8s.Informer{
		Client:   client,
		Path:     "/apis/discovery.k8s.io/v1/namespaces/" + url.PathEscape(w.cfg.Namespace) + "/endpointslices",
		Query:    url.Values{"labelSelector": {serviceNameLabel + "=" + w.cfg.Service}},
		OnChange: w.apply,
	}
	return w, nil
}

func (w *k8sWatcher) run(ctx context.Context) { w.informer.Run(ctx) }

// apply converts the slices into endpoints: ready (or unspecified), not
// terminating, on the selected port.
func (w *k8sWatcher) apply(objs map[string]k8s.Object) {
	var eps []config.Endpoint
	for _, o := range objs {
		var s endpointSlice
		if err := json.Unmarshal(o.Raw, &s); err != nil {
			slog.Warn("skipping malformed EndpointSlice", "slice", o.Key(), "err", err)
			continue
		}
		port := w.pickPort(s)
		if port == 0 {
			continue
		}
		for _, e := range s.Endpoints {
			if e.Conditions.Ready != nil && !*e.Conditions.Ready {
				continue
			}
			if e.Conditions.Terminating != nil && *e.Conditions.Terminating {
				continue
			}
			for _, a := range e.Addresses {
				eps = append(eps, config.Endpoint{Address: net.JoinHostPort(a, strconv.Itoa(port)), Weight: 1})
			}
		}
	}
	slices.SortFunc(eps, func(a, b config.Endpoint) int { return cmp.Compare(a.Address, b.Address) })
	eps = slices.CompactFunc(eps, func(a, b config.Endpoint) bool { return a.Address == b.Address })
	if !sameEndpoints(w.c.Endpoints(), eps) {
		w.c.setEndpoints(eps)
		slog.Info("endpoints updated", "cluster", w.c.Name, "endpoints", len(eps), "source", "kubernetes")
	}
}

func (w *k8sWatcher) pickPort(s endpointSlice) int {
	for _, p := range s.Ports {
		if p.Port == nil {
			continue
		}
		name := ""
		if p.Name != nil {
			name = *p.Name
		}
		if w.cfg.PortName != "" && name == w.cfg.PortName {
			return *p.Port
		}
		if w.cfg.PortName == "" && len(s.Ports) == 1 {
			return *p.Port
		}
	}
	if w.cfg.PortName == "" {
		return w.port
	}
	return 0
}

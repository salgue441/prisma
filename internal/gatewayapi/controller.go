package gatewayapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"net/url"
	"slices"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"prisma/internal/config"
	"prisma/internal/k8s"
)

const debounce = 200 * time.Millisecond

// ApplyFunc installs a merged configuration (routes and listeners).
type ApplyFunc func(cfg *config.Config, version string) error

// Controller watches Gateway API resources and PrismaPolicies, merges the
// translation with the static base configuration, applies it, and writes
// status back. Replicas need no leader election: they compute identical
// status and skip writes that would not change anything.
type Controller struct {
	cfg    config.GatewayAPI
	client *k8s.Client
	apply  ApplyFunc

	informers map[string]*k8s.Informer

	mu      sync.Mutex
	base    *config.Config
	applied string
	kick    chan struct{}
}

var resources = map[string]struct {
	path     string
	query    url.Values
	optional bool
}{
	"GatewayClass":   {path: "/apis/gateway.networking.k8s.io/v1/gatewayclasses", optional: true},
	"Gateway":        {path: "/apis/gateway.networking.k8s.io/v1/gateways", optional: true},
	"HTTPRoute":      {path: "/apis/gateway.networking.k8s.io/v1/httproutes", optional: true},
	"GRPCRoute":      {path: "/apis/gateway.networking.k8s.io/v1/grpcroutes", optional: true},
	"ReferenceGrant": {path: "/apis/gateway.networking.k8s.io/v1beta1/referencegrants", optional: true},
	"PrismaPolicy":   {path: "/apis/prisma.dev/v1alpha1/prismapolicies", optional: true},
	"Service":        {path: "/api/v1/services"},
	// Only TLS secrets: the gateway never needs (or watches) other secrets.
	"Secret": {path: "/api/v1/secrets", query: url.Values{"fieldSelector": {"type=kubernetes.io/tls"}}},
}

func NewController(cfg config.GatewayAPI, base *config.Config, apply ApplyFunc) (*Controller, error) {
	client, err := k8s.NewClient(k8s.Options{APIServer: cfg.APIServer, TokenFile: cfg.TokenFile, CAFile: cfg.CAFile})
	if err != nil {
		return nil, err
	}
	c := &Controller{cfg: cfg, client: client, apply: apply, base: base, kick: make(chan struct{}, 1), informers: map[string]*k8s.Informer{}}
	for kind, r := range resources {
		c.informers[kind] = &k8s.Informer{Client: client, Path: r.path, Query: r.query, Optional: r.optional,
			OnChange: func(map[string]k8s.Object) { c.trigger() }}
	}
	return c, nil
}

// SetBase replaces the static configuration (after a file reload).
func (c *Controller) SetBase(base *config.Config) {
	c.mu.Lock()
	c.base = base
	c.mu.Unlock()
	c.trigger()
}

func (c *Controller) trigger() {
	select {
	case c.kick <- struct{}{}:
	default:
	}
}

// Run blocks until ctx ends. It waits for every informer's first list before
// the first apply, so a restart never briefly serves a partial route table.
func (c *Controller) Run(ctx context.Context) {
	for _, inf := range c.informers {
		go inf.Run(ctx)
	}
	for _, inf := range c.informers {
		select {
		case <-ctx.Done():
			return
		case <-inf.Synced():
		}
	}
	slog.Info("gateway API controller synced", "controllerName", c.cfg.ControllerName)
	c.trigger()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.kick:
		}
		// Coalesce bursts (e.g. kubectl apply of many objects).
		timer := time.NewTimer(debounce)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		c.reconcile(ctx)
	}
}

func (c *Controller) snapshot(kind string) map[string]k8s.Object { return c.informers[kind].Snapshot() }

func (c *Controller) reconcile(ctx context.Context) {
	c.mu.Lock()
	base := c.base
	c.mu.Unlock()
	out := Translate(Input{
		ControllerName: c.cfg.ControllerName,
		Base:           base,
		Discovery:      config.KubernetesDiscovery{APIServer: c.cfg.APIServer, TokenFile: c.cfg.TokenFile, CAFile: c.cfg.CAFile},
		GatewayClasses: c.snapshot("GatewayClass"), Gateways: c.snapshot("Gateway"),
		HTTPRoutes: c.snapshot("HTTPRoute"), GRPCRoutes: c.snapshot("GRPCRoute"),
		ReferenceGrants: c.snapshot("ReferenceGrant"), Services: c.snapshot("Service"),
		Secrets: c.snapshot("Secret"), Policies: c.snapshot("PrismaPolicy"),
	})
	merged, version, err := Merge(base, out)
	if err != nil {
		slog.Error("gateway API configuration rejected; keeping the current one", "err", err)
	} else if version != c.applied {
		if err := c.apply(merged, version); err != nil {
			slog.Error("gateway API configuration failed to apply", "err", err)
		} else {
			c.applied = version
			slog.Info("gateway API configuration applied", "version", version,
				"listeners", len(out.Listeners), "routes", len(out.Routes), "clusters", len(out.Clusters))
		}
	}
	c.writeStatus(ctx, out.Status)
}

// Merge combines the static base with translated resources and validates
// the result. The version is a hash of the merged configuration.
func Merge(base *config.Config, out Output) (*config.Config, string, error) {
	raw, err := yaml.Marshal(base)
	if err != nil {
		return nil, "", err
	}
	var cfg config.Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, "", err
	}
	cfg.Listeners = append(cfg.Listeners, out.Listeners...)
	cfg.Clusters = append(cfg.Clusters, out.Clusters...)
	cfg.Routes = append(cfg.Routes, out.Routes...)
	if cfg.Policies == nil {
		cfg.Policies = map[string]config.Policy{}
	}
	maps.Copy(cfg.Policies, out.Policies)
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, "", err
	}
	final, err := yaml.Marshal(&cfg)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(final)
	return &cfg, "k8s-" + hex.EncodeToString(sum[:6]), nil
}

// --- status ---

type metaCondition struct {
	Type               string `json:"type"`
	Status             string `json:"status"`
	Reason             string `json:"reason"`
	Message            string `json:"message"`
	LastTransitionTime string `json:"lastTransitionTime"`
	ObservedGeneration int64  `json:"observedGeneration"`
}

// conditions converts translator conditions, keeping lastTransitionTime
// from the existing status when a condition's status did not change.
func conditions(want []Condition, have []metaCondition, gen int64) []metaCondition {
	now := time.Now().UTC().Format(time.RFC3339)
	out := make([]metaCondition, 0, len(want))
	for _, w := range want {
		mc := metaCondition{Type: w.Type, Status: w.Status, Reason: w.Reason, Message: w.Message, ObservedGeneration: gen, LastTransitionTime: now}
		for _, h := range have {
			if h.Type == w.Type && h.Status == w.Status {
				mc.LastTransitionTime = h.LastTransitionTime
			}
		}
		out = append(out, mc)
	}
	return out
}

func (c *Controller) writeStatus(ctx context.Context, st Status) {
	classes := c.snapshot("GatewayClass")
	for _, cs := range st.Classes {
		o, ok := classes[cs.Name]
		if !ok {
			continue
		}
		var cur struct {
			Status struct {
				Conditions []metaCondition `json:"conditions"`
			} `json:"status"`
		}
		_ = json.Unmarshal(o.Raw, &cur)
		want := map[string]any{"conditions": conditions(cs.Conditions, cur.Status.Conditions, cs.Generation)}
		c.patch(ctx, "/apis/gateway.networking.k8s.io/v1/gatewayclasses/"+url.PathEscape(cs.Name), o.Raw, want)
	}

	gateways := c.snapshot("Gateway")
	for _, gs := range st.Gateways {
		o, ok := gateways[gs.Namespace+"/"+gs.Name]
		if !ok {
			continue
		}
		var cur struct {
			Status struct {
				Conditions []metaCondition `json:"conditions"`
				Listeners  []struct {
					Name       string          `json:"name"`
					Conditions []metaCondition `json:"conditions"`
				} `json:"listeners"`
			} `json:"status"`
		}
		_ = json.Unmarshal(o.Raw, &cur)
		var listeners []any
		for _, ls := range gs.Listeners {
			var have []metaCondition
			for _, cl := range cur.Status.Listeners {
				if cl.Name == ls.Name {
					have = cl.Conditions
				}
			}
			kinds := []map[string]string{}
			for _, k := range ls.SupportedKinds {
				kinds = append(kinds, map[string]string{"group": Group, "kind": k})
			}
			listeners = append(listeners, map[string]any{
				"name": ls.Name, "supportedKinds": kinds, "attachedRoutes": ls.AttachedRoutes,
				"conditions": conditions(ls.Conditions, have, gs.Generation),
			})
		}
		want := map[string]any{"conditions": conditions(gs.Conditions, cur.Status.Conditions, gs.Generation), "listeners": listeners}
		c.patch(ctx, fmt.Sprintf("/apis/gateway.networking.k8s.io/v1/namespaces/%s/gateways/%s", url.PathEscape(gs.Namespace), url.PathEscape(gs.Name)), o.Raw, want)
	}

	routes := map[string]map[string]k8s.Object{KindHTTPRoute: c.snapshot("HTTPRoute"), KindGRPCRoute: c.snapshot("GRPCRoute")}
	for _, rs := range st.Routes {
		o, ok := routes[rs.Kind][rs.Namespace+"/"+rs.Name]
		if !ok {
			continue
		}
		var cur struct {
			Status struct {
				Parents []json.RawMessage `json:"parents"`
			} `json:"status"`
		}
		_ = json.Unmarshal(o.Raw, &cur)
		// Keep other controllers' parent entries; replace ours.
		var parents []any
		ours := map[string][]metaCondition{}
		for _, raw := range cur.Status.Parents {
			var p struct {
				ParentRef      parentRef       `json:"parentRef"`
				ControllerName string          `json:"controllerName"`
				Conditions     []metaCondition `json:"conditions"`
			}
			if json.Unmarshal(raw, &p) != nil {
				continue
			}
			if p.ControllerName != c.cfg.ControllerName {
				parents = append(parents, raw)
			} else {
				ours[parentKey(p.ParentRef)] = p.Conditions
			}
		}
		for _, ps := range rs.Parents {
			parents = append(parents, map[string]any{
				"parentRef": ps.Ref, "controllerName": c.cfg.ControllerName,
				"conditions": conditions(ps.Conditions, ours[parentKey(ps.Ref)], rs.Generation),
			})
		}
		plural := map[string]string{KindHTTPRoute: "httproutes", KindGRPCRoute: "grpcroutes"}[rs.Kind]
		c.patch(ctx, fmt.Sprintf("/apis/gateway.networking.k8s.io/v1/namespaces/%s/%s/%s", url.PathEscape(rs.Namespace), plural, url.PathEscape(rs.Name)),
			o.Raw, map[string]any{"parents": parents})
	}

	policies := c.snapshot("PrismaPolicy")
	for _, ps := range st.Policies {
		o, ok := policies[ps.Namespace+"/"+ps.Name]
		if !ok {
			continue
		}
		var cur struct {
			Status struct {
				Ancestors []struct {
					AncestorRef targetRef       `json:"ancestorRef"`
					Conditions  []metaCondition `json:"conditions"`
				} `json:"ancestors"`
			} `json:"status"`
		}
		_ = json.Unmarshal(o.Raw, &cur)
		var ancestors []any
		for _, a := range ps.Ancestors {
			var have []metaCondition
			for _, ca := range cur.Status.Ancestors {
				if ca.AncestorRef.Kind == a.Ref.Kind && ca.AncestorRef.Name == a.Ref.Name {
					have = ca.Conditions
				}
			}
			ref := map[string]any{"group": a.Ref.Group, "kind": a.Ref.Kind, "name": a.Ref.Name, "namespace": ps.Namespace}
			ancestors = append(ancestors, map[string]any{
				"ancestorRef": ref, "controllerName": c.cfg.ControllerName,
				"conditions": conditions(a.Conditions, have, ps.Generation),
			})
		}
		c.patch(ctx, fmt.Sprintf("/apis/prisma.dev/v1alpha1/namespaces/%s/prismapolicies/%s", url.PathEscape(ps.Namespace), url.PathEscape(ps.Name)),
			o.Raw, map[string]any{"ancestors": ancestors})
	}
}

func parentKey(p parentRef) string {
	return deref(p.Namespace, "") + "/" + p.Name + "/" + deref(p.SectionName, "") + "/" + fmt.Sprint(deref(p.Port, 0))
}

// patch writes status only when it differs from the object's current status.
func (c *Controller) patch(ctx context.Context, path string, current json.RawMessage, want map[string]any) {
	var cur struct {
		Status map[string]any `json:"status"`
	}
	_ = json.Unmarshal(current, &cur)
	wantJSON, _ := json.Marshal(want)
	var wantNorm map[string]any
	_ = json.Unmarshal(wantJSON, &wantNorm)
	if equalJSON(cur.Status, wantNorm, slices.Collect(maps.Keys(wantNorm))) {
		return
	}
	body, _ := json.Marshal(map[string]any{"status": want})
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := c.client.MergePatchStatus(ctx, path, body); err != nil {
		slog.Warn("status update failed", "object", path, "err", err)
	}
}

// equalJSON compares the given top-level keys of two decoded JSON objects.
func equalJSON(a, b map[string]any, keys []string) bool {
	for _, k := range keys {
		x, _ := json.Marshal(a[k])
		y, _ := json.Marshal(b[k])
		if !bytes.Equal(x, y) {
			return false
		}
	}
	return true
}

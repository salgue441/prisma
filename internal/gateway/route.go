package gateway

import (
	"context"
	"errors"
	"math/rand/v2"
	"net/http"
	"net/http/httputil"
	"strings"
	"sync"

	"prisma/internal/config"
	"prisma/internal/filters/authn"
	"prisma/internal/filters/authz"
	"prisma/internal/filters/compression"
	"prisma/internal/filters/cors"
	"prisma/internal/filters/graphqlguard"
	"prisma/internal/filters/grpcweb"
	"prisma/internal/filters/headers"
	"prisma/internal/filters/masking"
	"prisma/internal/filters/ratelimit"
	"prisma/internal/filters/redirect"
	"prisma/internal/gwerr"
	"prisma/internal/pipeline"
	"prisma/internal/upstream"
)

type routeDeps struct {
	gw        *Gateway
	snap      *Snapshot
	consumers *authn.Consumers
	redis     ratelimit.Store
}

type backend struct {
	cluster *upstream.Cluster
	proxy   *httputil.ReverseProxy
	weight  int
}

// routeHandler runs a route's filter chain, then proxies to a backend.
type routeHandler struct {
	chain       pipeline.Chain
	backends    []backend
	totalWeight int
	retry       *upstream.RetryPolicy
}

// buildRoute assembles the filter chain in phase order:
// redirect → cors → authn → authz → graphql guard → rate limit →
// rewrite/headers/masking. CORS runs before authn because browsers send
// preflights without credentials.
func (d routeDeps) buildRoute(rc *config.Route) (*routeHandler, error) {
	cfg := d.snap.Config
	m := d.gw.metrics
	pol := cfg.EffectivePolicy(rc)
	h := &routeHandler{retry: upstream.NewRetryPolicy(rc.Retries)}
	ch := &h.chain

	if rc.Redirect != nil {
		ch.Request = append(ch.Request, redirect.New(rc.Redirect, rc.Match))
		return h, nil
	}
	if pol.CORS != nil {
		ch.Request = append(ch.Request, cors.New(pol.CORS))
	}
	if pol.AuthN != nil {
		f, err := authn.New(rc.Name, pol.AuthN, d.consumers, d.gw.jwks, m)
		if err != nil {
			return nil, err
		}
		ch.Request = append(ch.Request, f)
	}
	if pol.AuthZ != nil {
		ch.Request = append(ch.Request, authz.New(rc.Name, pol.AuthZ, m))
	}
	if pol.GraphQL != nil {
		maxBody := int64(rc.MaxBodyBytes)
		if maxBody == 0 {
			maxBody = int64(cfg.Global.MaxBodyBytes)
		}
		ch.Request = append(ch.Request, graphqlguard.New(pol.GraphQL, maxBody))
	}
	if len(pol.RateLimit) > 0 {
		ch.Request = append(ch.Request, ratelimit.New(rc.Name, pol.RateLimit, d.gw.local, d.redis, m))
	}
	if rc.Rewrite != nil {
		ch.Request = append(ch.Request, redirect.NewRewrite(rc.Rewrite))
	}
	if pol.Headers != nil {
		f := headers.New(pol.Headers)
		ch.Request = append(ch.Request, f)
		ch.Response = append(ch.Response, f)
	}
	if pol.Masking != nil && len(pol.Masking.Rules) > 0 {
		f, err := masking.New(rc.Name, pol.Masking, m)
		if err != nil {
			return nil, err
		}
		ch.Request = append(ch.Request, f)
		// Masking runs last on the way out (first registered = outermost).
		ch.Response = append([]pipeline.ResponseFilter{f}, ch.Response...)
	}
	if rc.GRPCWeb {
		// Last before the proxy on the way in, first to see the upstream
		// response on the way out, so other filters see native gRPC.
		f := grpcweb.New()
		ch.Request = append(ch.Request, f)
		ch.Response = append(ch.Response, f)
	}
	if pol.Compression != nil {
		// Outermost on the way out: compress the final (masked) body.
		ch.Response = append([]pipeline.ResponseFilter{compression.New(pol.Compression)}, ch.Response...)
	}

	for _, b := range rc.Backends {
		cl := d.snap.Clusters[b.Cluster]
		h.backends = append(h.backends, backend{cluster: cl, proxy: d.gw.newProxy(cl, &h.chain, d.snap), weight: b.Weight})
		h.totalWeight += b.Weight
	}
	return h, nil
}

func (h *routeHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c := pipeline.From(r.Context())
	if res := h.chain.RunRequest(c); res != nil {
		respond(w, c, res)
		return
	}
	if len(h.backends) == 0 {
		respond(w, c, gwerr.Internal().WithReason("route has no backends"))
		return
	}
	b := h.pick()
	ctx := upstream.WithRetryPolicy(c.Request.Context(), h.retry)
	b.proxy.ServeHTTP(w, c.Request.WithContext(ctx))
}

// pick does a weighted random choice for traffic splitting (canaries).
func (h *routeHandler) pick() *backend {
	if len(h.backends) == 1 {
		return &h.backends[0]
	}
	n := rand.IntN(h.totalWeight)
	for i := range h.backends {
		if n < h.backends[i].weight {
			return &h.backends[i]
		}
		n -= h.backends[i].weight
	}
	return &h.backends[len(h.backends)-1]
}

// respond writes a gateway-generated response, including headers that
// filters asked to add to every response (e.g. RateLimit-*).
func respond(w http.ResponseWriter, c *pipeline.Context, res pipeline.Responder) {
	pipeline.MergeHeaders(w.Header(), c.ResponseHeaders)
	res.Respond(w, c.Protocol)
}

var bufferPool = &bytesPool{p: sync.Pool{New: func() any { b := make([]byte, 32<<10); return &b }}}

type bytesPool struct{ p sync.Pool }

func (b *bytesPool) Get() []byte  { return *(b.p.Get().(*[]byte)) }
func (b *bytesPool) Put(v []byte) { b.p.Put(&v) }

func (g *Gateway) newProxy(cl *upstream.Cluster, chain *pipeline.Chain, snap *Snapshot) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Transport:  cl,
		BufferPool: bufferPool,
		ErrorLog:   proxyErrorLog,
		Rewrite: func(pr *httputil.ProxyRequest) {
			c := pipeline.From(pr.In.Context())
			// The cluster transport replaces the host with the picked endpoint.
			pr.Out.URL.Scheme, pr.Out.URL.Host = cl.Scheme, cl.Name
			switch {
			case c.UpstreamHost != "":
				pr.Out.Host = c.UpstreamHost
			case cl.Config.PreserveHost:
				pr.Out.Host = pr.In.Host
			default:
				// Static clusters send the endpoint address; discovered
				// clusters send the service name, not a pod IP.
				pr.Out.Host = cl.DefaultHost
			}
			setForwarded(pr, c, snap)
			pr.Out.Header.Set("X-Request-Id", c.RequestID)
			if p := c.Principal; p != nil {
				pr.Out.Header.Set("X-Prisma-Consumer", p.Consumer)
				pr.Out.Header.Set("X-Prisma-Principal", p.ID)
				pr.Out.Header.Set("X-Prisma-Auth-Method", p.Method)
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			c := pipeline.From(resp.Request.Context())
			if err := chain.RunResponse(c, resp); err != nil {
				return err
			}
			pipeline.MergeHeaders(resp.Header, c.ResponseHeaders)
			return nil
		},
		ErrorHandler: proxyError,
	}
}

// setForwarded appends the peer to X-Forwarded-For only when the peer is a
// trusted proxy; otherwise inbound forwarding headers are discarded.
func setForwarded(pr *httputil.ProxyRequest, c *pipeline.Context, snap *Snapshot) {
	peer := peerAddr(pr.In.RemoteAddr)
	peerStr := peer.String()
	proto := "http"
	if pr.In.TLS != nil {
		proto = "https"
	}
	if snap.isTrusted(peer) {
		if prior := pr.In.Header.Values("X-Forwarded-For"); len(prior) > 0 {
			peerStr = strings.Join(prior, ", ") + ", " + peerStr
		}
		if p := pr.In.Header.Get("X-Forwarded-Proto"); p != "" {
			proto = p
		}
	}
	pr.Out.Header.Set("X-Forwarded-For", peerStr)
	pr.Out.Header.Set("X-Forwarded-Proto", proto)
	pr.Out.Header.Set("X-Forwarded-Host", pr.In.Host)
}

func proxyError(w http.ResponseWriter, r *http.Request, err error) {
	c := pipeline.From(r.Context())
	var ge *gwerr.Error
	var mbe *http.MaxBytesError
	switch {
	case errors.As(err, &ge):
	case errors.As(err, &mbe):
		ge = gwerr.PayloadTooLarge()
	case errors.Is(err, context.DeadlineExceeded):
		ge = gwerr.Timeout()
	case errors.Is(err, context.Canceled):
		// The client went away; nobody will read this response.
		c.Error = "client canceled"
		w.WriteHeader(499)
		return
	default:
		ge = gwerr.BadGateway("upstream request failed")
	}
	c.Error = err.Error()
	respond(w, c, ge)
}

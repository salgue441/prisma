// Package cors answers CORS preflights at the gateway and decorates responses
// with CORS headers, so browsers can call any route (including gRPC-Web) and
// read gateway-generated errors such as 401 and 429.
package cors

import (
	"net/http"
	"slices"
	"strconv"
	"strings"

	"prisma/internal/config"
	"prisma/internal/pipeline"
	"prisma/internal/protocol"
)

var defaultMethods = []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE"}

type Filter struct {
	cfg      *config.CORS
	any      bool
	exact    []string
	suffixes []wildcard
	methods  string
	headers  string
	expose   string
	maxAge   string
}

type wildcard struct{ scheme, suffix string } // "https://", ".example.com"

func New(cfg *config.CORS) *Filter {
	f := &Filter{cfg: cfg}
	for _, o := range cfg.AllowOrigins {
		switch {
		case o == "*":
			f.any = true
		case strings.Contains(o, "://*."):
			scheme, rest, _ := strings.Cut(o, "*")
			f.suffixes = append(f.suffixes, wildcard{scheme: scheme, suffix: strings.ToLower(rest)})
		default:
			f.exact = append(f.exact, strings.ToLower(strings.TrimSuffix(o, "/")))
		}
	}
	methods := cfg.AllowMethods
	if len(methods) == 0 {
		methods = defaultMethods
	}
	f.methods = strings.Join(methods, ", ")
	f.headers = strings.Join(cfg.AllowHeaders, ", ")
	f.expose = strings.Join(cfg.ExposeHeaders, ", ")
	if cfg.MaxAge > 0 {
		f.maxAge = strconv.Itoa(int(cfg.MaxAge.Seconds()))
	}
	return f
}

func (f *Filter) Name() string { return "cors" }

func (f *Filter) allowed(origin string) bool {
	if f.any {
		return true
	}
	o := strings.ToLower(origin)
	if slices.Contains(f.exact, o) {
		return true
	}
	for _, w := range f.suffixes {
		if host, ok := strings.CutPrefix(o, w.scheme); ok && strings.HasSuffix(host, w.suffix) && len(host) > len(w.suffix) {
			return true
		}
	}
	return false
}

func (f *Filter) OnRequest(c *pipeline.Context) pipeline.Responder {
	r := c.Request
	origin := r.Header.Get("Origin")
	preflight := r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != ""
	if c.ResponseHeaders == nil {
		c.ResponseHeaders = http.Header{}
	}
	h := c.ResponseHeaders
	// Responses differ per Origin unless every origin gets "*".
	if !f.any || f.cfg.AllowCredentials {
		h.Add("Vary", "Origin")
	}
	if origin == "" || !f.allowed(origin) {
		if preflight {
			// Answer without CORS headers: the browser blocks the call, and
			// the upstream never sees an unauthenticated OPTIONS request.
			return preflightResponse{}
		}
		return nil
	}
	if f.any && !f.cfg.AllowCredentials {
		h.Set("Access-Control-Allow-Origin", "*")
	} else {
		h.Set("Access-Control-Allow-Origin", origin)
	}
	if f.cfg.AllowCredentials {
		h.Set("Access-Control-Allow-Credentials", "true")
	}
	if !preflight {
		if f.expose != "" {
			h.Set("Access-Control-Expose-Headers", f.expose)
		}
		return nil
	}
	h.Add("Vary", "Access-Control-Request-Method")
	h.Add("Vary", "Access-Control-Request-Headers")
	h.Set("Access-Control-Allow-Methods", f.methods)
	switch {
	case f.headers != "":
		h.Set("Access-Control-Allow-Headers", f.headers)
	case r.Header.Get("Access-Control-Request-Headers") != "":
		// No explicit list: reflect what the browser asked for.
		h.Set("Access-Control-Allow-Headers", r.Header.Get("Access-Control-Request-Headers"))
	}
	if f.maxAge != "" {
		h.Set("Access-Control-Max-Age", f.maxAge)
	}
	return preflightResponse{}
}

type preflightResponse struct{}

func (preflightResponse) Respond(w http.ResponseWriter, _ protocol.Kind) {
	w.WriteHeader(http.StatusNoContent)
}

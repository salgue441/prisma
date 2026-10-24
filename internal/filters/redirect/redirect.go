// Package redirect implements redirect routes and path/host rewriting.
package redirect

import (
	"net/http"
	"regexp"
	"strings"

	"prisma/internal/config"
	"prisma/internal/pipeline"
	"prisma/internal/protocol"
)

type Filter struct {
	cfg         *config.Redirect
	matchPrefix string
}

func New(cfg *config.Redirect, match config.Match) *Filter {
	return &Filter{cfg: cfg, matchPrefix: match.PathPrefix}
}

func (f *Filter) Name() string { return "redirect" }

func (f *Filter) OnRequest(c *pipeline.Context) pipeline.Responder {
	r := c.Request
	scheme := f.cfg.Scheme
	if scheme == "" {
		scheme = "http"
		if r.TLS != nil {
			scheme = "https"
		}
	}
	host := f.cfg.Host
	if host == "" {
		host = r.Host
		// Moving to https: drop an explicit plaintext port.
		if f.cfg.Scheme == "https" && r.TLS == nil {
			if i := strings.LastIndex(host, ":"); i > strings.LastIndex(host, "]") {
				host = host[:i]
			}
		}
	}
	path := r.URL.EscapedPath()
	switch {
	case f.cfg.Path != "":
		path = f.cfg.Path
	case f.cfg.PathPrefixReplace != "":
		path = f.cfg.PathPrefixReplace + strings.TrimPrefix(path, f.matchPrefix)
	}
	loc := scheme + "://" + host + path
	if !f.cfg.StripQuery && r.URL.RawQuery != "" {
		loc += "?" + r.URL.RawQuery
	}
	return response{location: loc, status: f.cfg.Status}
}

type response struct {
	location string
	status   int
}

func (r response) Respond(w http.ResponseWriter, _ protocol.Kind) {
	w.Header().Set("Location", r.location)
	w.WriteHeader(r.status)
}

// Rewrite changes the upstream path and host without a client round-trip.
type Rewrite struct {
	cfg *config.Rewrite
	re  *regexp.Regexp
}

func NewRewrite(cfg *config.Rewrite) *Rewrite {
	rw := &Rewrite{cfg: cfg}
	if cfg.PathRegex != "" {
		rw.re = regexp.MustCompile(cfg.PathRegex)
	}
	return rw
}

func (f *Rewrite) Name() string { return "rewrite" }

func (f *Rewrite) OnRequest(c *pipeline.Context) pipeline.Responder {
	u := c.Request.URL
	p := u.Path
	if f.cfg.StripPrefix != "" {
		if rest, ok := strings.CutPrefix(p, f.cfg.StripPrefix); ok {
			p = f.cfg.ReplacePrefix + rest
			if !strings.HasPrefix(p, "/") {
				p = "/" + p
			}
		}
	}
	if f.re != nil {
		p = f.re.ReplaceAllString(p, f.cfg.Replacement)
	}
	if p != u.Path {
		u.Path, u.RawPath = p, ""
	}
	if f.cfg.Host != "" {
		c.UpstreamHost = f.cfg.Host
	}
	return nil
}

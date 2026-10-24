// Package headers adds, sets and removes request and response headers.
// Values may use ${...} templates (see pipeline.Context.Expand).
package headers

import (
	"net/http"

	"prisma/internal/config"
	"prisma/internal/pipeline"
)

type Filter struct{ cfg *config.Headers }

func New(cfg *config.Headers) *Filter { return &Filter{cfg: cfg} }

func (f *Filter) Name() string { return "headers" }

func (f *Filter) OnRequest(c *pipeline.Context) pipeline.Responder {
	apply(c, c.Request.Header, f.cfg.Request)
	return nil
}

func (f *Filter) OnResponse(c *pipeline.Context, resp *http.Response) error {
	apply(c, resp.Header, f.cfg.Response)
	return nil
}

func apply(c *pipeline.Context, h http.Header, ops config.HeaderOps) {
	for _, k := range ops.Remove {
		h.Del(k)
	}
	for k, v := range ops.Set {
		h.Set(k, c.Expand(v))
	}
	for k, v := range ops.Add {
		h.Add(k, c.Expand(v))
	}
}

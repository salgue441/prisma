// Package wasmplugin runs Proxy-Wasm plugins as pipeline filters.
package wasmplugin

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"prisma/internal/config"
	"prisma/internal/gwerr"
	"prisma/internal/pipeline"
	"prisma/internal/protocol"
	"prisma/internal/wasm"
)

const defaultMaxBody = 1 << 20

type Filter struct {
	name     string
	plugin   *wasm.Plugin
	bodies   bool
	maxBody  int64
	failOpen bool
}

func New(cfg *config.Plugin, p *wasm.Plugin) *Filter {
	maxBody := int64(cfg.MaxBodyBytes)
	if maxBody == 0 {
		maxBody = defaultMaxBody
	}
	return &Filter{name: cfg.Name, plugin: p, bodies: cfg.Bodies, maxBody: maxBody, failOpen: cfg.FailOpen}
}

func (f *Filter) Name() string { return "wasm:" + f.name }

type streamKey struct{ name string }

func (f *Filter) OnRequest(c *pipeline.Context) pipeline.Responder {
	ctx := c.Request.Context()
	s, err := f.plugin.NewStream(ctx, properties(c))
	if err != nil {
		return f.failure(c, err)
	}
	c.SetValue(streamKey{f.name}, s)
	c.OnDone(func() { s.Done(context.WithoutCancel(ctx)) })

	r := c.Request
	s.Req = r
	hasBody := r.Body != nil && r.Body != http.NoBody && r.ContentLength != 0
	if err := s.OnRequestHeaders(ctx, !hasBody); err != nil {
		return f.failure(c, err)
	}
	if l := s.Local(); l != nil {
		return localResponse{l}
	}
	if !f.bodies || !hasBody || c.Protocol.IsStreaming() {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, f.maxBody+1))
	r.Body.Close()
	if err != nil {
		return f.failure(c, err)
	}
	if int64(len(body)) > f.maxBody {
		return gwerr.PayloadTooLarge().WithReason("plugin %s buffers at most %d bytes", f.name, f.maxBody)
	}
	s.ReqBody = body
	if err := s.OnRequestBody(ctx); err != nil {
		return f.failure(c, err)
	}
	if l := s.Local(); l != nil {
		return localResponse{l}
	}
	setRequestBody(r, s.ReqBody)
	return nil
}

func (f *Filter) OnResponse(c *pipeline.Context, resp *http.Response) error {
	s, ok := c.Value(streamKey{f.name}).(*wasm.Stream)
	if !ok {
		return nil
	}
	ctx := resp.Request.Context()
	s.Resp = resp
	hasBody := resp.Body != nil && resp.Body != http.NoBody && resp.ContentLength != 0
	if err := s.OnResponseHeaders(ctx, !hasBody); err != nil {
		return f.responseFailure(err)
	}
	if l := s.Local(); l != nil {
		replaceResponse(resp, l)
		return nil
	}
	if !f.bodies || !hasBody || c.Protocol.IsStreaming() {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, f.maxBody+1))
	if err != nil {
		return f.responseFailure(err)
	}
	if int64(len(body)) > f.maxBody {
		if !f.failOpen {
			return gwerr.BadGateway("upstream response could not be processed").WithReason("plugin %s: response exceeds %d bytes", f.name, f.maxBody)
		}
		resp.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(body), resp.Body), resp.Body}
		return nil
	}
	resp.Body.Close()
	s.RespBody = body
	if err := s.OnResponseBody(ctx); err != nil {
		return f.responseFailure(err)
	}
	if l := s.Local(); l != nil {
		replaceResponse(resp, l)
		return nil
	}
	setResponseBody(resp, s.RespBody)
	return nil
}

func (f *Filter) failure(c *pipeline.Context, err error) pipeline.Responder {
	if f.failOpen {
		c.Error = fmt.Sprintf("plugin %s failed open: %v", f.name, err)
		return nil
	}
	return gwerr.Internal().WithReason("plugin %s: %v", f.name, err)
}

func (f *Filter) responseFailure(err error) error {
	if f.failOpen {
		return nil
	}
	return gwerr.Internal().WithReason("plugin %s: %v", f.name, err)
}

func setRequestBody(r *http.Request, body []byte) {
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	r.ContentLength = int64(len(body))
	r.Header.Set("Content-Length", strconv.Itoa(len(body)))
}

func setResponseBody(resp *http.Response, body []byte) {
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
}

func replaceResponse(resp *http.Response, l *wasm.LocalResponse) {
	resp.Body.Close()
	resp.StatusCode, resp.Status = l.Status, fmt.Sprintf("%d %s", l.Status, http.StatusText(l.Status))
	resp.Header = l.Header.Clone()
	setResponseBody(resp, l.Body)
}

// localResponse renders proxy_send_local_response. gRPC clients get the
// plugin's grpc status (or one derived from the HTTP status).
type localResponse struct{ l *wasm.LocalResponse }

func (r localResponse) Respond(w http.ResponseWriter, kind protocol.Kind) {
	for k, vs := range r.l.Header {
		w.Header()[k] = vs
	}
	if kind.IsGRPC() {
		code := int(r.l.GRPCStatus)
		if code < 0 {
			code = grpcFromHTTP(r.l.Status)
		}
		w.Header().Set("Content-Type", "application/grpc")
		w.Header().Set("Grpc-Status", strconv.Itoa(code))
		w.Header().Set("Grpc-Message", string(r.l.Body))
		w.WriteHeader(http.StatusOK)
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(r.l.Body)))
	w.WriteHeader(r.l.Status)
	_, _ = w.Write(r.l.Body)
}

func grpcFromHTTP(status int) int {
	switch status {
	case 400:
		return 3
	case 401:
		return 16
	case 403:
		return 7
	case 404:
		return 12
	case 429:
		return 8
	case 502, 503, 504:
		return 14
	}
	return 2
}

// properties exposes request attributes under Envoy's property names.
func properties(c *pipeline.Context) wasm.Properties {
	return func(path []string) ([]byte, bool) {
		r := c.Request
		switch strings.Join(path, ".") {
		case "request.path":
			return wasm.StringProperty(r.URL.RequestURI())
		case "request.url_path":
			return wasm.StringProperty(r.URL.Path)
		case "request.query":
			return wasm.StringProperty(r.URL.RawQuery)
		case "request.host":
			return wasm.StringProperty(r.Host)
		case "request.method":
			return wasm.StringProperty(r.Method)
		case "request.scheme":
			if r.TLS != nil {
				return wasm.StringProperty("https")
			}
			return wasm.StringProperty("http")
		case "request.protocol":
			return wasm.StringProperty(r.Proto)
		case "request.id":
			return wasm.StringProperty(c.RequestID)
		case "request.useragent":
			return wasm.StringProperty(r.UserAgent())
		case "request.referer":
			return wasm.StringProperty(r.Referer())
		case "source.address":
			return wasm.StringProperty(r.RemoteAddr)
		case "source.ip", "prisma.client_ip":
			return wasm.StringProperty(c.Var("client_ip"))
		case "connection.mtls":
			mtls := r.TLS != nil && len(r.TLS.PeerCertificates) > 0
			if mtls {
				return []byte{1}, true
			}
			return []byte{0}, true
		case "route_name", "xds.route_name":
			return wasm.StringProperty(c.RouteName)
		case "prisma.protocol":
			return wasm.StringProperty(c.Protocol.String())
		case "prisma.consumer":
			if c.Principal != nil {
				return wasm.StringProperty(c.Principal.Consumer)
			}
		case "prisma.principal":
			if c.Principal != nil {
				return wasm.StringProperty(c.Principal.ID)
			}
		case "prisma.auth_method":
			if c.Principal != nil {
				return wasm.StringProperty(c.Principal.Method)
			}
		case "prisma.scopes":
			if c.Principal != nil {
				return wasm.StringProperty(strings.Join(c.Principal.Scopes, " "))
			}
		case "upstream.address":
			if c.Endpoint != "" {
				return wasm.StringProperty(c.Endpoint)
			}
		case "cluster_name":
			if c.Cluster != "" {
				return wasm.StringProperty(c.Cluster)
			}
		}
		return nil, false
	}
}

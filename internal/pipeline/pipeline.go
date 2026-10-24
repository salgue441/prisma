// Package pipeline defines the protocol-neutral filter chain and the
// per-request context that filters share.
package pipeline

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"runtime/debug"
	"slices"
	"strings"
	"time"

	"prisma/internal/gwerr"
	"prisma/internal/protocol"
)

// Principal is the normalized identity produced by authentication.
type Principal struct {
	ID       string
	Consumer string
	Method   string // jwt | apikey | mtls
	Scopes   []string
	Claims   map[string]any
}

func (p *Principal) HasScope(s string) bool {
	return p != nil && slices.Contains(p.Scopes, s)
}

// GraphQLInfo is filled in by the GraphQL guard for later filters and logs.
type GraphQLInfo struct {
	OperationType string
	OperationName string
	Depth         int
	Aliases       int
	Cost          int
}

// Context carries per-request state through filters, the proxy and logging.
// A request is handled by a single goroutine, so Context needs no locking.
type Context struct {
	Request      *http.Request
	Listener     string
	RouteName    string
	Protocol     protocol.Kind
	ClientIP     netip.Addr
	RequestID    string
	Start        time.Time
	OriginalPath string
	// AcceptEncoding is the client's original Accept-Encoding; filters may
	// strip the header before proxying (e.g. masking needs identity bodies).
	AcceptEncoding string

	Principal *Principal
	GraphQL   *GraphQLInfo

	// UpstreamHost overrides the Host header sent upstream when non-empty.
	UpstreamHost string
	// ResponseHeaders are added to every response, proxied or generated.
	ResponseHeaders http.Header

	Cluster         string
	Endpoint        string
	Attempts        int
	UpstreamLatency time.Duration

	// Rejection names the filter that rejected the request, for logs.
	Rejection string
	// ServedBy names the filter that answered the request itself.
	ServedBy string
	// CacheStatus is hit, miss or bypass on cached routes.
	CacheStatus string
	// Error is the internal error behind a gateway-generated response, for logs.
	Error string

	// values holds filter-private state shared between a filter's request
	// and response phases. Keys should be unexported types of that filter.
	values map[any]any
	done   []func()
}

func (c *Context) SetValue(k, v any) {
	if c.values == nil {
		c.values = map[any]any{}
	}
	c.values[k] = v
}

func (c *Context) Value(k any) any { return c.values[k] }

// OnDone registers fn to run once the request has completed, whatever path
// it took (proxied, rejected, or failed).
func (c *Context) OnDone(fn func()) { c.done = append(c.done, fn) }

// Finish runs the OnDone callbacks in reverse registration order.
func (c *Context) Finish() {
	for i := len(c.done) - 1; i >= 0; i-- {
		c.done[i]()
	}
	c.done = nil
}

type ctxKey struct{}

func WithContext(ctx context.Context, c *Context) context.Context {
	return context.WithValue(ctx, ctxKey{}, c)
}

// From returns the pipeline Context stored in ctx, or nil.
func From(ctx context.Context) *Context {
	c, _ := ctx.Value(ctxKey{}).(*Context)
	return c
}

// Responder short-circuits a request with a response.
type Responder interface {
	Respond(w http.ResponseWriter, kind protocol.Kind)
}

// Served marks Responders that answer a request successfully at the gateway
// (cache hits, redirects, CORS preflights) rather than rejecting it.
type Served interface {
	ServedByGateway()
}

type RequestFilter interface {
	Name() string
	// OnRequest returns nil to continue, or a Responder to stop the chain.
	OnRequest(c *Context) Responder
}

type ResponseFilter interface {
	Name() string
	OnResponse(c *Context, resp *http.Response) error
}

type Chain struct {
	Request  []RequestFilter
	Response []ResponseFilter
}

// RunRequest executes request filters in order. A panicking filter is
// converted into an internal error rather than crashing the connection.
func (ch *Chain) RunRequest(c *Context) (res Responder) {
	var current string
	defer func() {
		if p := recover(); p != nil {
			slog.Error("filter panic", "filter", current, "panic", p, "stack", string(debug.Stack()))
			c.Rejection = current
			c.Error = fmt.Sprint("panic: ", p)
			res = gwerr.Internal()
		}
	}()
	for _, f := range ch.Request {
		current = f.Name()
		if r := f.OnRequest(c); r != nil {
			if _, ok := r.(Served); ok {
				c.ServedBy = current
				return r
			}
			c.Rejection = current
			if e, ok := r.(*gwerr.Error); ok && e.Reason != "" {
				c.Error = e.Reason
			}
			return r
		}
	}
	return nil
}

// RunResponse executes response filters in reverse order (onion model).
func (ch *Chain) RunResponse(c *Context, resp *http.Response) (err error) {
	var current string
	defer func() {
		if p := recover(); p != nil {
			slog.Error("response filter panic", "filter", current, "panic", p, "stack", string(debug.Stack()))
			err = gwerr.Internal().WithReason("panic in %s: %v", current, p)
		}
	}()
	for i := len(ch.Response) - 1; i >= 0; i-- {
		f := ch.Response[i]
		current = f.Name()
		if err := f.OnResponse(c, resp); err != nil {
			return err
		}
	}
	return nil
}

// MergeHeaders copies src into dst. Vary accumulates tokens from every source
// (upstream, CORS, compression); other headers replace dst's values.
func MergeHeaders(dst, src http.Header) {
	for k, vs := range src {
		if k != "Vary" {
			dst[k] = vs
			continue
		}
		for _, v := range vs {
			if !headerHasToken(dst, "Vary", v) {
				dst.Add("Vary", v)
			}
		}
	}
}

func headerHasToken(h http.Header, key, token string) bool {
	for _, v := range h.Values(key) {
		for _, t := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(t), token) {
				return true
			}
		}
	}
	return false
}

// Expand replaces ${...} variables in tpl with request values:
// consumer, principal, auth_method, request_id, client_ip, route,
// claim.<name> and header.<name>. Unknown variables expand to "".
func (c *Context) Expand(tpl string) string {
	if !strings.Contains(tpl, "${") {
		return tpl
	}
	var b strings.Builder
	for {
		i := strings.Index(tpl, "${")
		if i < 0 {
			b.WriteString(tpl)
			break
		}
		j := strings.IndexByte(tpl[i:], '}')
		if j < 0 {
			b.WriteString(tpl)
			break
		}
		b.WriteString(tpl[:i])
		b.WriteString(c.Var(tpl[i+2 : i+j]))
		tpl = tpl[i+j+1:]
	}
	return b.String()
}

// Var resolves a single template or key variable.
func (c *Context) Var(name string) string {
	switch {
	case name == "consumer":
		if c.Principal != nil {
			return c.Principal.Consumer
		}
	case name == "principal":
		if c.Principal != nil {
			return c.Principal.ID
		}
	case name == "auth_method":
		if c.Principal != nil {
			return c.Principal.Method
		}
	case name == "request_id":
		return c.RequestID
	case name == "client_ip":
		if c.ClientIP.IsValid() {
			return c.ClientIP.String()
		}
	case name == "route":
		return c.RouteName
	case strings.HasPrefix(name, "claim."):
		if c.Principal != nil {
			if v, ok := c.Principal.Claims[strings.TrimPrefix(name, "claim.")]; ok {
				return fmt.Sprint(v)
			}
		}
	case strings.HasPrefix(name, "header."):
		return c.Request.Header.Get(strings.TrimPrefix(name, "header."))
	}
	return ""
}

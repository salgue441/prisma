// Package openapi validates requests, and optionally responses, against an
// OpenAPI 3 document: the path and method must exist, and parameters,
// headers, cookies and bodies must match their schemas.
package openapi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/gorillamux"

	"prisma/internal/config"
	"prisma/internal/gwerr"
	"prisma/internal/pipeline"
	"prisma/internal/telemetry"
)

const defaultMaxBody = 1 << 20

// Filter validates requests in OnRequest and, with responses: report,
// upstream responses in OnResponse.
type Filter struct {
	route     string
	router    routers.Router
	opts      *openapi3filter.Options
	report    bool
	allowUnk  bool
	responses bool
	maxBody   int64
	metrics   *telemetry.Metrics
}

type stateKey struct{}

// state carries the matched operation to the response phase.
type state struct {
	input *openapi3filter.RequestValidationInput
}

// Load parses and validates an OpenAPI document. External references may
// point at local files only (relative to specFile), never at URLs.
func Load(cfg *config.OpenAPI) (*openapi3.T, error) {
	loader := openapi3.NewLoader()
	loader.IsExternalRefsAllowed = true
	loader.ReadFromURIFunc = func(l *openapi3.Loader, u *url.URL) ([]byte, error) {
		if u.Scheme != "" && u.Scheme != "file" {
			return nil, fmt.Errorf("remote reference %s is not allowed", u)
		}
		return openapi3.ReadFromFile(l, u)
	}
	var doc *openapi3.T
	var err error
	if cfg.SpecFile != "" {
		doc, err = loader.LoadFromFile(cfg.SpecFile)
	} else {
		loader.IsExternalRefsAllowed = false
		doc, err = loader.LoadFromData([]byte(cfg.Spec))
	}
	if err != nil {
		return nil, fmt.Errorf("openapi: %w", err)
	}
	if err := doc.Validate(context.Background()); err != nil {
		return nil, fmt.Errorf("openapi: invalid document: %w", err)
	}
	return doc, nil
}

// New compiles a validator. Servers in the document are reduced to their
// paths (or replaced by basePath), so matching ignores scheme and host:
// the gateway, not the document, decides which hosts reach this route.
func New(route string, cfg *config.OpenAPI, m *telemetry.Metrics) (*Filter, error) {
	doc, err := Load(cfg)
	if err != nil {
		return nil, err
	}
	base := cfg.BasePath
	if base == "" && len(doc.Servers) > 0 {
		if u, err := url.Parse(doc.Servers[0].URL); err == nil {
			base = u.Path
		}
	}
	doc.Servers = openapi3.Servers{{URL: strings.TrimSuffix(base, "/")}}
	if base == "" || base == "/" {
		doc.Servers = nil
	}
	router, err := gorillamux.NewRouter(doc)
	if err != nil {
		return nil, fmt.Errorf("openapi: %w", err)
	}
	f := &Filter{
		route: route, router: router, metrics: m,
		report:    cfg.Mode == "report",
		allowUnk:  cfg.UnknownOperations == "allow",
		responses: cfg.Responses == "report",
		maxBody:   int64(cfg.MaxBodyBytes),
		opts: &openapi3filter.Options{
			MultiError: true,
			// Authentication is the gateway's job (authn filters); the
			// document's security schemes are not re-checked here.
			AuthenticationFunc: openapi3filter.NoopAuthenticationFunc,
			// Validation must not change what is forwarded.
			SkipSettingDefaults: true,
		},
	}
	if f.maxBody <= 0 {
		f.maxBody = defaultMaxBody
	}
	return f, nil
}

func (f *Filter) Name() string { return "openapi" }

func (f *Filter) OnRequest(c *pipeline.Context) pipeline.Responder {
	r := c.Request
	// Match on a copy without the host so servers' hosts never matter.
	probe := r.Clone(r.Context())
	probe.Host, probe.URL.Host = "", ""
	route, params, err := f.router.FindRoute(probe)
	if err != nil {
		f.metrics.OpenAPI.WithLabelValues(f.route, "request", "unknown_operation").Inc()
		if f.allowUnk {
			return nil
		}
		if errors.Is(err, routers.ErrMethodNotAllowed) {
			return f.verdict(c, gwerr.MethodNotAllowed(r.Method+" is not defined for this path"))
		}
		return f.verdict(c, gwerr.NotFound("operation not defined in the API specification"))
	}

	var body []byte
	if r.Body != nil && r.Body != http.NoBody {
		body, err = io.ReadAll(io.LimitReader(r.Body, f.maxBody+1))
		r.Body.Close()
		if err != nil {
			return gwerr.BadRequest("BAD_REQUEST", "could not read request body")
		}
		if int64(len(body)) > f.maxBody {
			return gwerr.PayloadTooLarge()
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
	}
	probe.Body = io.NopCloser(bytes.NewReader(body))
	probe.ContentLength = int64(len(body))
	input := &openapi3filter.RequestValidationInput{Request: probe, PathParams: params, Route: route, Options: f.opts}
	err = openapi3filter.ValidateRequest(r.Context(), input)
	if f.responses {
		c.SetValue(stateKey{}, &state{input: input})
	}
	if err == nil {
		f.metrics.OpenAPI.WithLabelValues(f.route, "request", "valid").Inc()
		return nil
	}
	f.metrics.OpenAPI.WithLabelValues(f.route, "request", "invalid").Inc()
	return f.verdict(c, gwerr.ValidationFailed(details(err)))
}

// verdict enforces or, in report mode, only logs a rejection.
func (f *Filter) verdict(c *pipeline.Context, e *gwerr.Error) pipeline.Responder {
	if !f.report {
		return e
	}
	slog.Warn("openapi: request does not match the specification", "route", f.route,
		"request_id", c.RequestID, "method", c.Request.Method, "path", c.Request.URL.Path,
		"code", e.Code, "errors", e.Details)
	return nil
}

// OnResponse validates JSON responses in report mode. It never changes or
// blocks the response.
func (f *Filter) OnResponse(c *pipeline.Context, resp *http.Response) error {
	st, _ := c.Value(stateKey{}).(*state)
	if st == nil {
		return nil
	}
	mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if mt != "application/json" && !strings.HasSuffix(mt, "+json") {
		return nil
	}
	if resp.Header.Get("Content-Encoding") != "" || resp.ContentLength > f.maxBody {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, f.maxBody+1))
	resp.Body.Close()
	resp.Body = io.NopCloser(io.MultiReader(bytes.NewReader(body), eofOnError{err}))
	if err != nil || int64(len(body)) > f.maxBody {
		return nil // too large to check; already streamed back unchanged
	}
	in := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: st.input, Status: resp.StatusCode, Header: resp.Header, Options: f.opts,
	}
	in.SetBodyBytes(body)
	if err := openapi3filter.ValidateResponse(c.Request.Context(), in); err != nil {
		f.metrics.OpenAPI.WithLabelValues(f.route, "response", "invalid").Inc()
		slog.Warn("openapi: response does not match the specification", "route", f.route,
			"request_id", c.RequestID, "status", resp.StatusCode, "errors", details(err))
		return nil
	}
	f.metrics.OpenAPI.WithLabelValues(f.route, "response", "valid").Inc()
	return nil
}

// eofOnError surfaces a read error after the buffered bytes.
type eofOnError struct{ err error }

func (e eofOnError) Read([]byte) (int, error) {
	if e.err != nil {
		return 0, e.err
	}
	return 0, io.EOF
}

// details flattens validation errors into client-facing details. Messages
// come from the schema rules and parameter parsing.
func details(err error) []gwerr.Detail {
	var out []gwerr.Detail
	// Direct type checks, not errors.As: MultiError.As matches through its
	// elements and wrapped errors, which would lose the parameter location.
	var walk func(error, gwerr.Detail)
	walk = func(err error, d gwerr.Detail) {
		switch e := err.(type) {
		case openapi3.MultiError:
			for _, inner := range e {
				walk(inner, d)
			}
		case *openapi3filter.RequestError:
			if p := e.Parameter; p != nil {
				d.In, d.Field = p.In, p.Name
			}
			d.Message = e.Reason
			walk(e.Err, d)
		case *openapi3filter.ResponseError:
			d.Message = e.Reason
			walk(e.Err, d)
		default:
			out = append(out, describe(err, d))
		}
	}
	walk(err, gwerr.Detail{In: "body", Message: "invalid value"})
	return out
}

// describe refines a detail with the schema error behind it, if any.
func describe(err error, d gwerr.Detail) gwerr.Detail {
	var se *openapi3.SchemaError
	if errors.As(err, &se) {
		if ptr := se.JSONPointer(); len(ptr) > 0 && d.In == "body" {
			d.Field = "/" + strings.Join(ptr, "/")
		}
		if se.Reason != "" {
			d.Message = se.Reason
		}
		return d
	}
	if d.Message == "" && err != nil {
		d.Message = err.Error()
	}
	return d
}

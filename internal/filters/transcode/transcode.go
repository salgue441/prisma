// Package transcode serves gRPC methods as a JSON/HTTP API. Requests are
// matched against the google.api.http annotations in a descriptor set,
// turned into protobuf messages (path variables, query parameters and the
// JSON body), and sent upstream as gRPC; responses are converted back to
// JSON, with gRPC errors mapped to HTTP statuses.
package transcode

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"

	"google.golang.org/genproto/googleapis/api/annotations"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	"prisma/internal/config"
	"prisma/internal/gwerr"
	"prisma/internal/pipeline"
)

const (
	defaultMaxBody = 4 << 20
	// maxMessage bounds a single response message.
	maxMessage = 32 << 20
)

// binding is one HTTP rule bound to a method.
type binding struct {
	method       protoreflect.MethodDescriptor
	verb         string // GET, POST, ... or a custom verb
	tmpl         *template
	body         string // "", "*" or a top-level field name
	responseBody string
}

type Filter struct {
	bindings []*binding
	byPath   map[string]protoreflect.MethodDescriptor // "/pkg.Svc/Method" for autoMapping
	auto     bool
	marshal  protojson.MarshalOptions
	unmarsh  protojson.UnmarshalOptions
	discard  bool
	maxBody  int64
	types    *protoregistry.Types
}

type stateKey struct{}

type state struct {
	b *binding
}

// New loads the descriptor set and collects the HTTP bindings of its
// methods.
func New(cfg *config.GRPCTranscode) (*Filter, error) {
	raw, err := os.ReadFile(cfg.DescriptorSet)
	if err != nil {
		return nil, fmt.Errorf("grpcTranscode: %w", err)
	}
	var set descriptorpb.FileDescriptorSet
	// Unmarshal with the global registry so google.api.http options are
	// decoded as extensions.
	if err := proto.Unmarshal(raw, &set); err != nil {
		return nil, fmt.Errorf("grpcTranscode: %s: %w", cfg.DescriptorSet, err)
	}
	files, err := protodesc.NewFiles(&set)
	if err != nil {
		return nil, fmt.Errorf("grpcTranscode: %s: %w (build it with --include_imports)", cfg.DescriptorSet, err)
	}
	f := &Filter{
		byPath:  map[string]protoreflect.MethodDescriptor{},
		auto:    cfg.AutoMapping,
		discard: cfg.DiscardUnknown,
		maxBody: int64(cfg.MaxBodyBytes),
		types:   dynamicTypes(files),
		marshal: protojson.MarshalOptions{EmitUnpopulated: cfg.EmitUnpopulated, UseProtoNames: cfg.UseProtoNames},
	}
	f.marshal.Resolver = f.types
	f.unmarsh = protojson.UnmarshalOptions{DiscardUnknown: cfg.DiscardUnknown, Resolver: f.types}
	if f.maxBody <= 0 {
		f.maxBody = defaultMaxBody
	}
	found := map[string]bool{}
	var errs []error
	files.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		for i := range fd.Services().Len() {
			svc := fd.Services().Get(i)
			if len(cfg.Services) > 0 && !slices.Contains(cfg.Services, string(svc.FullName())) {
				continue
			}
			found[string(svc.FullName())] = true
			for j := range svc.Methods().Len() {
				m := svc.Methods().Get(j)
				if m.IsStreamingClient() {
					continue // a JSON request is a single message
				}
				f.byPath["/"+string(svc.FullName())+"/"+string(m.Name())] = m
				opts, ok := m.Options().(*descriptorpb.MethodOptions)
				if !ok || opts == nil || !proto.HasExtension(opts, annotations.E_Http) {
					continue
				}
				rule := proto.GetExtension(opts, annotations.E_Http).(*annotations.HttpRule)
				for _, r := range append([]*annotations.HttpRule{rule}, rule.GetAdditionalBindings()...) {
					b, err := newBinding(m, r)
					if err != nil {
						errs = append(errs, fmt.Errorf("%s: %w", m.FullName(), err))
						continue
					}
					f.bindings = append(f.bindings, b)
				}
			}
		}
		return true
	})
	for _, s := range cfg.Services {
		if !found[s] {
			errs = append(errs, fmt.Errorf("service %s is not in %s", s, cfg.DescriptorSet))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("grpcTranscode: %w", err)
	}
	if len(f.bindings) == 0 && !f.auto {
		return nil, fmt.Errorf("grpcTranscode: no method in %s has a google.api.http annotation (set autoMapping to call methods by name)", cfg.DescriptorSet)
	}
	// Most specific first: more literal segments, then fewer variables.
	slices.SortStableFunc(f.bindings, func(a, b *binding) int {
		if d := b.tmpl.literals() - a.tmpl.literals(); d != 0 {
			return d
		}
		return len(a.tmpl.vars) - len(b.tmpl.vars)
	})
	return f, nil
}

// dynamicTypes registers the set's messages so Any fields resolve.
func dynamicTypes(files *protoregistry.Files) *protoregistry.Types {
	t := new(protoregistry.Types)
	var reg func(protoreflect.MessageDescriptors)
	reg = func(ms protoreflect.MessageDescriptors) {
		for i := range ms.Len() {
			_ = t.RegisterMessage(dynamicpb.NewMessageType(ms.Get(i)))
			reg(ms.Get(i).Messages())
		}
	}
	files.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		reg(fd.Messages())
		return true
	})
	return t
}

func newBinding(m protoreflect.MethodDescriptor, r *annotations.HttpRule) (*binding, error) {
	b := &binding{method: m, body: r.GetBody(), responseBody: r.GetResponseBody()}
	var path string
	switch p := r.GetPattern().(type) {
	case *annotations.HttpRule_Get:
		b.verb, path = http.MethodGet, p.Get
	case *annotations.HttpRule_Put:
		b.verb, path = http.MethodPut, p.Put
	case *annotations.HttpRule_Post:
		b.verb, path = http.MethodPost, p.Post
	case *annotations.HttpRule_Delete:
		b.verb, path = http.MethodDelete, p.Delete
	case *annotations.HttpRule_Patch:
		b.verb, path = http.MethodPatch, p.Patch
	case *annotations.HttpRule_Custom:
		b.verb, path = strings.ToUpper(p.Custom.GetKind()), p.Custom.GetPath()
	default:
		return nil, fmt.Errorf("http rule without a pattern")
	}
	t, err := parseTemplate(path)
	if err != nil {
		return nil, err
	}
	b.tmpl = t
	in := m.Input()
	for _, v := range t.vars {
		if _, err := fieldPath(in, v.field); err != nil {
			return nil, fmt.Errorf("path variable %s: %w", strings.Join(v.field, "."), err)
		}
	}
	if b.body != "" && b.body != "*" {
		if fd := in.Fields().ByName(protoreflect.Name(b.body)); fd == nil {
			return nil, fmt.Errorf("body field %q not in %s", b.body, in.FullName())
		}
	}
	if b.responseBody != "" {
		if fd := m.Output().Fields().ByName(protoreflect.Name(b.responseBody)); fd == nil {
			return nil, fmt.Errorf("response_body field %q not in %s", b.responseBody, m.Output().FullName())
		}
	}
	return b, nil
}

func (f *Filter) Name() string { return "grpc_transcode" }

func (f *Filter) OnRequest(c *pipeline.Context) pipeline.Responder {
	r := c.Request
	b, vars := f.match(r)
	if b == nil {
		return gwerr.NotFound("no gRPC method is mapped to " + r.Method + " " + r.URL.Path)
	}
	msg := dynamicpb.NewMessage(b.method.Input())
	if b.body != "" {
		raw, err := io.ReadAll(io.LimitReader(r.Body, f.maxBody+1))
		r.Body.Close()
		if err != nil {
			return gwerr.BadRequest("BAD_REQUEST", "could not read request body")
		}
		if int64(len(raw)) > f.maxBody {
			return gwerr.PayloadTooLarge()
		}
		if len(bytes.TrimSpace(raw)) > 0 {
			target := msg.ProtoReflect()
			if b.body != "*" {
				fd := b.method.Input().Fields().ByName(protoreflect.Name(b.body))
				if fd.Message() == nil || fd.IsList() || fd.IsMap() {
					return gwerr.BadRequest("INVALID_BODY", "body field "+b.body+" must be a message")
				}
				target = msg.Mutable(fd).Message()
			}
			if err := f.unmarsh.Unmarshal(raw, target.Interface()); err != nil {
				return invalid("body", "", err.Error())
			}
		}
	}
	for path, val := range vars {
		if err := setField(msg, strings.Split(path, "."), []string{val}); err != nil {
			return invalid("path", path, err.Error())
		}
	}
	if b.body != "*" {
		for key, vals := range r.URL.Query() {
			if _, bound := vars[key]; bound {
				continue
			}
			fp := strings.Split(key, ".")
			if b.body != "" && fp[0] == b.body {
				continue
			}
			if err := setField(msg, fp, vals); err != nil {
				if f.discard && errors.Is(err, errNoField) {
					continue
				}
				return invalid("query", key, err.Error())
			}
		}
	}
	payload, err := proto.Marshal(msg)
	if err != nil {
		return gwerr.Internal().WithReason("transcode: %v", err)
	}
	frame := make([]byte, 5+len(payload))
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(payload)))
	copy(frame[5:], payload)

	svc := b.method.Parent().(protoreflect.ServiceDescriptor)
	r.Method = http.MethodPost
	r.URL.Path, r.URL.RawPath, r.URL.RawQuery = "/"+string(svc.FullName())+"/"+string(b.method.Name()), "", ""
	r.Body = io.NopCloser(bytes.NewReader(frame))
	r.ContentLength = int64(len(frame))
	h := r.Header
	h.Del("Content-Length")
	h.Del("Accept-Encoding") // gRPC compression is negotiated separately
	h.Set("Content-Type", "application/grpc")
	h.Set("Te", "trailers")
	c.SetValue(stateKey{}, &state{b: b})
	return nil
}

// match finds the binding for a request: annotated rules first, then the
// /package.Service/Method form when autoMapping is on.
func (f *Filter) match(r *http.Request) (*binding, map[string]string) {
	path := r.URL.EscapedPath()
	for _, b := range f.bindings {
		if b.verb != r.Method {
			continue
		}
		if vars, ok := b.tmpl.match(path); ok {
			return b, vars
		}
	}
	if f.auto && r.Method == http.MethodPost {
		if m, ok := f.byPath[r.URL.Path]; ok {
			return &binding{method: m, verb: http.MethodPost, body: "*"}, nil
		}
	}
	return nil, nil
}

func invalid(in, field, msg string) *gwerr.Error {
	return gwerr.ValidationFailed([]gwerr.Detail{{In: in, Field: field, Message: msg}})
}

// OnResponse converts the gRPC response to JSON. Unary methods return the
// message (or a problem document for a gRPC error); server-streaming
// methods return a JSON array of messages, streamed as they arrive.
func (f *Filter) OnResponse(c *pipeline.Context, resp *http.Response) error {
	st, _ := c.Value(stateKey{}).(*state)
	if st == nil || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/grpc") {
		return nil
	}
	h := resp.Header
	if h.Get("Grpc-Encoding") != "" && h.Get("Grpc-Encoding") != "identity" {
		return gwerr.BadGateway("upstream sent a compressed gRPC response").WithReason("grpc-encoding %s", h.Get("Grpc-Encoding"))
	}
	// Trailers-Only: the status is in the headers and there is no body.
	if s := h.Get("Grpc-Status"); s != "" && s != "0" {
		resp.Body.Close()
		f.writeError(resp, s, h.Get("Grpc-Message"))
		return nil
	}
	if st.b.method.IsStreamingServer() {
		src := resp.Body
		f.prepareJSON(resp, http.StatusOK)
		resp.ContentLength = -1
		resp.Body = &streamBody{f: f, b: st.b, src: src, resp: resp}
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxMessage+5))
	resp.Body.Close()
	if err != nil {
		return gwerr.BadGateway("reading the gRPC response failed").WithReason("%v", err)
	}
	if s := resp.Trailer.Get("Grpc-Status"); s != "" && s != "0" {
		f.writeError(resp, s, resp.Trailer.Get("Grpc-Message"))
		return nil
	}
	msgs, err := frames(data)
	if err != nil || len(msgs) != 1 {
		return gwerr.BadGateway("invalid gRPC response").WithReason("frames: %v (%d messages)", err, len(msgs))
	}
	out, err := f.toJSON(st.b, msgs[0])
	if err != nil {
		return gwerr.BadGateway("invalid gRPC response").WithReason("%v", err)
	}
	f.prepareJSON(resp, http.StatusOK)
	resp.Body = io.NopCloser(bytes.NewReader(out))
	resp.ContentLength = int64(len(out))
	h.Set("Content-Length", strconv.Itoa(len(out)))
	return nil
}

func (f *Filter) prepareJSON(resp *http.Response, status int) {
	h := resp.Header
	for k := range h {
		if strings.HasPrefix(k, "Grpc-") {
			h.Del(k)
		}
	}
	h.Del("Trailer")
	h.Del("Content-Length")
	h.Set("Content-Type", "application/json")
	resp.Trailer = nil
	resp.StatusCode = status
	resp.Status = fmt.Sprintf("%d %s", status, http.StatusText(status))
}

func (f *Filter) toJSON(b *binding, payload []byte) ([]byte, error) {
	msg := dynamicpb.NewMessage(b.method.Output())
	if err := proto.Unmarshal(payload, msg); err != nil {
		return nil, err
	}
	var out proto.Message = msg
	if b.responseBody != "" {
		fd := b.method.Output().Fields().ByName(protoreflect.Name(b.responseBody))
		if fd.Message() == nil || fd.IsList() || fd.IsMap() {
			return marshalValue(f.marshal, msg, fd)
		}
		out = msg.Get(fd).Message().Interface()
	}
	return f.marshal.Marshal(out)
}

// marshalValue renders a single non-message field (response_body pointing
// at a scalar, list or map) by marshaling the message and extracting it.
func marshalValue(o protojson.MarshalOptions, msg *dynamicpb.Message, fd protoreflect.FieldDescriptor) ([]byte, error) {
	o.EmitUnpopulated = true
	full, err := o.Marshal(msg)
	if err != nil {
		return nil, err
	}
	name := fd.JSONName()
	if o.UseProtoNames {
		name = string(fd.Name())
	}
	var m map[string]jsonRaw
	if err := jsonUnmarshal(full, &m); err != nil {
		return nil, err
	}
	return m[name], nil
}

func (f *Filter) writeError(resp *http.Response, status, message string) {
	n, _ := strconv.Atoi(status)
	code := codes.Code(n)
	msg, _ := url.PathUnescape(message)
	httpStatus := httpStatus(code)
	f.prepareJSON(resp, httpStatus)
	resp.Header.Set("Content-Type", "application/problem+json")
	body, _ := jsonMarshal(map[string]any{
		"type": "about:blank", "title": http.StatusText(httpStatus), "status": httpStatus,
		"detail": msg, "code": grpcCodeName(code), "grpcCode": n,
	})
	body = append(body, '\n')
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
}

// httpStatus maps gRPC codes to HTTP statuses as google.rpc.Code documents.
func httpStatus(c codes.Code) int {
	switch c {
	case codes.OK:
		return http.StatusOK
	case codes.Canceled:
		return 499
	case codes.InvalidArgument, codes.FailedPrecondition, codes.OutOfRange:
		return http.StatusBadRequest
	case codes.DeadlineExceeded:
		return http.StatusGatewayTimeout
	case codes.NotFound:
		return http.StatusNotFound
	case codes.AlreadyExists, codes.Aborted:
		return http.StatusConflict
	case codes.PermissionDenied:
		return http.StatusForbidden
	case codes.Unauthenticated:
		return http.StatusUnauthorized
	case codes.ResourceExhausted:
		return http.StatusTooManyRequests
	case codes.Unimplemented:
		return http.StatusNotImplemented
	case codes.Unavailable:
		return http.StatusServiceUnavailable
	}
	return http.StatusInternalServerError
}

// grpcCodeName is the SCREAMING_SNAKE name, e.g. NOT_FOUND.
func grpcCodeName(c codes.Code) string {
	var b strings.Builder
	for i, r := range c.String() {
		if i > 0 && r >= 'A' && r <= 'Z' {
			b.WriteByte('_')
		}
		b.WriteRune(r)
	}
	return strings.ToUpper(b.String())
}

// frames splits length-prefixed gRPC messages.
func frames(data []byte) ([][]byte, error) {
	var out [][]byte
	for len(data) > 0 {
		if len(data) < 5 {
			return nil, io.ErrUnexpectedEOF
		}
		if data[0] != 0 {
			return nil, errors.New("compressed message")
		}
		n := binary.BigEndian.Uint32(data[1:5])
		if uint64(len(data)-5) < uint64(n) {
			return nil, io.ErrUnexpectedEOF
		}
		out = append(out, data[5:5+n])
		data = data[5+n:]
	}
	return out, nil
}

// streamBody turns a stream of gRPC messages into a JSON array as messages
// arrive. A gRPC error at the end of the stream becomes a final
// {"error": {...}} element, since the HTTP status was already sent.
type streamBody struct {
	f   *Filter
	b   *binding
	src io.ReadCloser
	// resp.Trailer is set by the transport when the stream ends; it is read
	// and cleared then, so gRPC trailers never reach JSON clients.
	resp    *http.Response
	hdr     [5]byte
	out     bytes.Buffer
	n       int
	started bool
	done    bool
	err     error
}

func (s *streamBody) Read(p []byte) (int, error) {
	for s.out.Len() == 0 && !s.done {
		s.next()
	}
	if s.out.Len() > 0 {
		return s.out.Read(p)
	}
	return 0, s.err
}

func (s *streamBody) next() {
	if !s.started {
		s.out.WriteByte('[')
		s.started = true
	}
	if _, err := io.ReadFull(s.src, s.hdr[:]); err != nil {
		s.finish(err)
		return
	}
	size := binary.BigEndian.Uint32(s.hdr[1:])
	if s.hdr[0] != 0 || size > maxMessage {
		s.finish(errors.New("invalid gRPC frame"))
		return
	}
	payload := make([]byte, size)
	if _, err := io.ReadFull(s.src, payload); err != nil {
		s.finish(err)
		return
	}
	js, err := s.f.toJSON(s.b, payload)
	if err != nil {
		s.finish(err)
		return
	}
	if s.n > 0 {
		s.out.WriteByte(',')
	}
	s.out.Write(js)
	s.n++
}

// finish closes the array, appending the stream's error, if any.
func (s *streamBody) finish(err error) {
	s.done, s.err = true, io.EOF
	defer func() { s.resp.Trailer = nil }()
	status, message := "", ""
	if errors.Is(err, io.EOF) {
		if t := s.resp.Trailer; t != nil {
			status, message = t.Get("Grpc-Status"), t.Get("Grpc-Message")
		}
	} else {
		status, message = strconv.Itoa(int(codes.Internal)), "transcoding failed: "+err.Error()
	}
	if status != "" && status != "0" {
		n, _ := strconv.Atoi(status)
		msg, _ := url.PathUnescape(message)
		e, _ := jsonMarshal(map[string]any{"error": map[string]any{"code": grpcCodeName(codes.Code(n)), "grpcCode": n, "message": msg}})
		if s.n > 0 {
			s.out.WriteByte(',')
		}
		s.out.Write(e)
	}
	s.out.WriteByte(']')
}

func (s *streamBody) Close() error { return s.src.Close() }

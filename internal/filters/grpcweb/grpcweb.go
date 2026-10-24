// Package grpcweb translates gRPC-Web (binary and base64 "-text" modes) into
// native gRPC for upstreams, so browsers can call gRPC services through the
// gateway without an extra proxy.
//
// Requests: Content-Type becomes application/grpc[+subtype], TE: trailers is
// added and -text bodies are base64-decoded. Responses: the Content-Type is
// mapped back and the upstream's HTTP/2 trailers are appended to the body as
// a gRPC-Web trailer frame (flag 0x80), base64-encoded in -text mode.
package grpcweb

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"io"
	"net/http"
	"sort"
	"strings"

	"prisma/internal/pipeline"
	"prisma/internal/protocol"
)

const trailerFlag = 0x80

type Filter struct{}

func New() *Filter { return &Filter{} }

func (f *Filter) Name() string { return "grpc_web" }

type stateKey struct{}

type state struct {
	clientCT string
	text     bool
}

func (f *Filter) OnRequest(c *pipeline.Context) pipeline.Responder {
	if c.Protocol != protocol.GRPCWeb {
		return nil
	}
	r := c.Request
	ct := strings.ToLower(r.Header.Get("Content-Type"))
	st := &state{clientCT: ct, text: strings.HasPrefix(ct, "application/grpc-web-text")}
	sub := strings.TrimPrefix(strings.TrimPrefix(ct, "application/grpc-web-text"), "application/grpc-web")
	r.Header.Set("Content-Type", "application/grpc"+sub)
	r.Header.Set("Te", "trailers")
	r.Header.Del("Content-Length")
	if st.text {
		r.Body = struct {
			io.Reader
			io.Closer
		}{&textDecoder{src: r.Body}, r.Body}
		r.ContentLength = -1
	}
	c.SetValue(stateKey{}, st)
	return nil
}

func (f *Filter) OnResponse(c *pipeline.Context, resp *http.Response) error {
	st, ok := c.Value(stateKey{}).(*state)
	if !ok {
		return nil
	}
	ct := "application/grpc-web+proto"
	if up := resp.Header.Get("Content-Type"); strings.HasPrefix(up, "application/grpc") {
		sub := strings.TrimPrefix(up, "application/grpc")
		ct = "application/grpc-web" + sub
		if st.text {
			ct = "application/grpc-web-text" + sub
		}
		if sub == "" {
			ct += "+proto"
		}
	}
	resp.Header.Set("Content-Type", ct)
	resp.Header.Del("Content-Length")
	resp.Header.Del("Trailer")
	resp.ContentLength = -1
	// Trailers go into the body; stop the proxy from announcing them as
	// HTTP trailers (which HTTP/1.1 browsers cannot read anyway).
	resp.Trailer = nil
	if resp.Header.Get("Grpc-Status") != "" {
		// Trailers-Only response: status already lives in the headers,
		// which is valid gRPC-Web.
		return nil
	}
	var body io.Reader = &trailerAppender{src: resp.Body, resp: resp}
	if st.text {
		body = &textEncoder{src: body}
	}
	resp.Body = struct {
		io.Reader
		io.Closer
	}{body, resp.Body}
	return nil
}

// trailerAppender streams the upstream body and, at EOF, emits the HTTP/2
// trailers (populated by the transport once the body is drained) as a
// gRPC-Web trailer frame.
type trailerAppender struct {
	src  io.Reader
	resp *http.Response
	tail *bytes.Reader
}

func (t *trailerAppender) Read(p []byte) (int, error) {
	if t.tail != nil {
		return t.tail.Read(p)
	}
	n, err := t.src.Read(p)
	if err != io.EOF {
		return n, err
	}
	t.tail = bytes.NewReader(trailerFrame(t.resp.Trailer))
	t.resp.Trailer = nil
	if n > 0 {
		return n, nil
	}
	return t.tail.Read(p)
}

func trailerFrame(tr http.Header) []byte {
	if tr.Get("Grpc-Status") == "" {
		tr = tr.Clone()
		if tr == nil {
			tr = http.Header{}
		}
		tr.Set("Grpc-Status", "13")
		tr.Set("Grpc-Message", "upstream closed the stream without trailers")
	}
	keys := make([]string, 0, len(tr))
	for k := range tr {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var block bytes.Buffer
	for _, k := range keys {
		for _, v := range tr[k] {
			block.WriteString(strings.ToLower(k))
			block.WriteString(": ")
			block.WriteString(v)
			block.WriteString("\r\n")
		}
	}
	frame := make([]byte, 5, 5+block.Len())
	frame[0] = trailerFlag
	binary.BigEndian.PutUint32(frame[1:], uint32(block.Len()))
	return append(frame, block.Bytes()...)
}

// textDecoder decodes grpc-web-text request bodies. Clients may concatenate
// independently padded base64 segments, so input is decoded in 4-byte quanta
// rather than as one base64 stream.
type textDecoder struct {
	src     io.Reader
	quantum []byte
	out     []byte
	err     error
}

func (d *textDecoder) Read(p []byte) (int, error) {
	for len(d.out) == 0 && d.err == nil {
		buf := make([]byte, 4096)
		n, err := d.src.Read(buf)
		for _, b := range buf[:n] {
			if b == '\r' || b == '\n' || b == ' ' {
				continue
			}
			d.quantum = append(d.quantum, b)
			if len(d.quantum) == 4 {
				dec := make([]byte, 3)
				m, derr := base64.StdEncoding.Decode(dec, d.quantum)
				if derr != nil {
					d.err = derr
					break
				}
				d.out = append(d.out, dec[:m]...)
				d.quantum = d.quantum[:0]
			}
		}
		if err != nil && d.err == nil {
			d.err = err
			if err == io.EOF && len(d.quantum) != 0 {
				d.err = io.ErrUnexpectedEOF
			}
		}
	}
	n := copy(p, d.out)
	d.out = d.out[n:]
	if len(d.out) == 0 && d.err != nil {
		return n, d.err
	}
	return n, nil
}

// textEncoder base64-encodes each chunk independently (padded), which
// gRPC-Web text clients accept and which keeps streaming responses flowing.
type textEncoder struct {
	src     io.Reader
	pending []byte
	err     error
}

func (e *textEncoder) Read(p []byte) (int, error) {
	for len(e.pending) == 0 && e.err == nil {
		raw := make([]byte, max(len(p)*3/4, 3))
		n, err := e.src.Read(raw)
		if n > 0 {
			e.pending = base64.StdEncoding.AppendEncode(nil, raw[:n])
		}
		e.err = err
	}
	n := copy(p, e.pending)
	e.pending = e.pending[n:]
	if len(e.pending) == 0 && e.err != nil {
		return n, e.err
	}
	return n, nil
}

// Package compression compresses upstream responses (zstd or gzip) based on
// the client's Accept-Encoding. It streams: bodies are never buffered.
package compression

import (
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/klauspost/compress/gzip"
	"github.com/klauspost/compress/zstd"

	"prisma/internal/config"
	"prisma/internal/pipeline"
	"prisma/internal/protocol"
)

const defaultMinBytes = 1024

var defaultTypes = []string{
	"text/*", "application/json", "application/*+json", "application/javascript",
	"application/xml", "application/*+xml", "application/graphql-response+json", "image/svg+xml",
}

type Filter struct {
	algorithms []string
	minBytes   int64
	exact      map[string]bool
	families   []string // "text/"
	suffixes   []string // "+json"
}

func New(cfg *config.Compression) *Filter {
	f := &Filter{algorithms: cfg.Algorithms, minBytes: int64(cfg.MinBytes), exact: map[string]bool{}}
	if len(f.algorithms) == 0 {
		f.algorithms = []string{"zstd", "gzip"}
	}
	if f.minBytes == 0 {
		f.minBytes = defaultMinBytes
	}
	types := cfg.Types
	if len(types) == 0 {
		types = defaultTypes
	}
	for _, t := range types {
		switch {
		case strings.HasSuffix(t, "/*"):
			f.families = append(f.families, strings.TrimSuffix(t, "*"))
		case strings.Contains(t, "/*+"):
			f.suffixes = append(f.suffixes, t[strings.Index(t, "+"):])
		default:
			f.exact[t] = true
		}
	}
	return f
}

func (f *Filter) Name() string { return "compression" }

func (f *Filter) OnResponse(c *pipeline.Context, resp *http.Response) error {
	if c.Protocol.IsGRPC() || c.Protocol == protocol.WebSocket || resp.Request.Method == http.MethodHead ||
		resp.StatusCode < 200 || resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotModified ||
		resp.Body == nil || resp.Body == http.NoBody ||
		resp.Header.Get("Content-Encoding") != "" || resp.Header.Get("Content-Range") != "" ||
		strings.Contains(strings.ToLower(resp.Header.Get("Cache-Control")), "no-transform") ||
		(resp.ContentLength >= 0 && resp.ContentLength < f.minBytes) ||
		!f.compressible(resp.Header.Get("Content-Type")) {
		return nil
	}
	// The representation now depends on Accept-Encoding even when we decide
	// not to compress for this particular client.
	pipeline.MergeHeaders(resp.Header, http.Header{"Vary": {"Accept-Encoding"}})
	alg := Negotiate(c.AcceptEncoding, f.algorithms)
	if alg == "" {
		return nil
	}
	resp.Header.Set("Content-Encoding", alg)
	resp.Header.Del("Content-Length")
	resp.ContentLength = -1
	if et := resp.Header.Get("Etag"); et != "" && !strings.HasPrefix(et, "W/") {
		resp.Header.Set("Etag", "W/"+et)
	}
	resp.Body = compress(resp.Body, alg)
	return nil
}

func (f *Filter) compressible(ct string) bool {
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return false
	}
	if f.exact[mt] {
		return true
	}
	for _, fam := range f.families {
		if strings.HasPrefix(mt, fam) {
			return true
		}
	}
	for _, s := range f.suffixes {
		if strings.HasSuffix(mt, s) {
			return true
		}
	}
	return false
}

// Negotiate picks the server-preferred algorithm with the highest client
// q-value. "*" matches any algorithm; q=0 forbids one.
func Negotiate(acceptEncoding string, preference []string) string {
	q := map[string]float64{}
	wildcard := -1.0
	for _, part := range strings.Split(acceptEncoding, ",") {
		name, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" {
			continue
		}
		v := 1.0
		if qs, ok := strings.CutPrefix(strings.TrimSpace(params), "q="); ok {
			if parsed, err := strconv.ParseFloat(qs, 64); err == nil {
				v = parsed
			}
		}
		if name == "*" {
			wildcard = v
		} else {
			q[name] = v
		}
	}
	best, bestQ := "", 0.0
	for _, alg := range preference {
		v, ok := q[alg]
		if !ok {
			v = max(wildcard, 0)
		}
		if v > bestQ {
			best, bestQ = alg, v
		}
	}
	return best
}

var (
	gzipPool = sync.Pool{New: func() any { w, _ := gzip.NewWriterLevel(nil, gzip.DefaultCompression); return w }}
	zstdPool = sync.Pool{New: func() any {
		w, _ := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithEncoderConcurrency(1))
		return w
	}}
)

type encoder interface {
	io.WriteCloser
	Reset(io.Writer)
}

// compress returns a body that yields src compressed. A goroutine pumps src
// through a pooled encoder into a pipe; closing the body closes both ends so
// the goroutine always terminates, even if the client goes away mid-stream.
func compress(src io.ReadCloser, alg string) io.ReadCloser {
	pool := &gzipPool
	if alg == "zstd" {
		pool = &zstdPool
	}
	pr, pw := io.Pipe()
	go func() {
		enc := pool.Get().(encoder)
		enc.Reset(pw)
		_, err := io.Copy(enc, src)
		if cerr := enc.Close(); err == nil {
			err = cerr
		}
		enc.Reset(nil)
		pool.Put(enc)
		src.Close()
		pw.CloseWithError(err)
	}()
	return &body{PipeReader: pr, src: src}
}

type body struct {
	*io.PipeReader
	src io.Closer
}

func (b *body) Close() error {
	b.PipeReader.Close()
	return b.src.Close()
}

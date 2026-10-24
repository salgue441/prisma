package cache

import (
	"bytes"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"prisma/internal/config"
	"prisma/internal/pipeline"
	"prisma/internal/protocol"
	"prisma/internal/telemetry"
)

const defaultMaxEntry = 1 << 20

// unstored headers are hop-by-hop or recomputed on every hit.
var unstored = []string{"Connection", "Keep-Alive", "Transfer-Encoding", "Trailer", "Upgrade", "Age", "Date", "X-Request-Id"}

type Filter struct {
	route    string
	cfg      *config.Cache
	store    *Store
	maxEntry int64
	metrics  *telemetry.Metrics
}

func New(route string, cfg *config.Cache, store *Store, m *telemetry.Metrics) *Filter {
	maxEntry := int64(cfg.MaxEntryBytes)
	if maxEntry == 0 {
		maxEntry = defaultMaxEntry
	}
	return &Filter{route: route, cfg: cfg, store: store, maxEntry: maxEntry, metrics: m}
}

func (f *Filter) Name() string { return "cache" }

type stateKey struct{}

type state struct {
	key    string
	header http.Header // client request headers, for Vary
}

// RoutePrefix is the key prefix of a route's entries (for purging).
func RoutePrefix(route string) string { return route + "|" }

func (f *Filter) key(c *pipeline.Context) string {
	r := c.Request
	var b strings.Builder
	b.WriteString(RoutePrefix(f.route))
	b.WriteString(strings.ToLower(r.Host))
	b.WriteByte('|')
	b.WriteString(r.URL.EscapedPath())
	b.WriteByte('?')
	b.WriteString(r.URL.RawQuery)
	if !f.cfg.Shared {
		b.WriteString("|c=")
		if c.Principal != nil {
			b.WriteString(c.Principal.Method + ":" + c.Principal.ID)
		}
	}
	for _, h := range f.cfg.KeyHeaders {
		b.WriteString("|" + h + "=" + strings.Join(r.Header.Values(h), ","))
	}
	return b.String()
}

func (f *Filter) OnRequest(c *pipeline.Context) pipeline.Responder {
	r := c.Request
	if (r.Method != http.MethodGet && r.Method != http.MethodHead) || (c.Protocol != protocol.HTTP && c.Protocol != protocol.GraphQL) {
		return nil
	}
	req := parseCacheControl(r.Header.Get("Cache-Control"))
	if strings.Contains(strings.ToLower(r.Header.Get("Pragma")), "no-cache") {
		req.noCache = true
	}
	key := f.key(c)
	hdr := r.Header.Clone()
	hdr.Set("Accept-Encoding", c.AcceptEncoding) // filters may strip it later
	if !req.noCache && !req.noStore {
		if e := f.store.Get(key, time.Now()); e != nil && varyMatches(e, hdr) {
			c.CacheStatus = "hit"
			f.metrics.Cache.WithLabelValues(f.route, "hit").Inc()
			return &hit{entry: e, head: r.Method == http.MethodHead}
		}
	}
	if req.noStore {
		c.CacheStatus = "bypass"
		f.metrics.Cache.WithLabelValues(f.route, "bypass").Inc()
		return nil
	}
	c.CacheStatus = "miss"
	f.metrics.Cache.WithLabelValues(f.route, "miss").Inc()
	c.SetValue(stateKey{}, &state{key: key, header: hdr})
	return nil
}

// OnResponse stores cacheable responses while streaming them to the client;
// the entry is committed only when the full body has been read.
func (f *Filter) OnResponse(c *pipeline.Context, resp *http.Response) error {
	st, ok := c.Value(stateKey{}).(*state)
	if !ok || resp.Request.Method != http.MethodGet || resp.StatusCode != http.StatusOK || resp.Header.Get("Set-Cookie") != "" {
		return nil
	}
	cc := parseCacheControl(resp.Header.Get("Cache-Control"))
	if cc.noStore || cc.noCache || cc.private {
		return nil
	}
	ttl := f.cfg.DefaultTTL
	switch {
	case cc.sMaxAge >= 0:
		ttl = time.Duration(cc.sMaxAge) * time.Second
	case cc.maxAge >= 0:
		ttl = time.Duration(cc.maxAge) * time.Second
	}
	if age, err := strconv.Atoi(resp.Header.Get("Age")); err == nil {
		ttl -= time.Duration(age) * time.Second
	}
	if ttl <= 0 || resp.ContentLength > f.maxEntry {
		return nil
	}
	vary := map[string]string{}
	for _, v := range resp.Header.Values("Vary") {
		for _, name := range strings.Split(v, ",") {
			name = http.CanonicalHeaderKey(strings.TrimSpace(name))
			if name == "*" {
				return nil
			}
			if name != "" {
				vary[name] = st.header.Get(name)
			}
		}
	}
	header := resp.Header.Clone()
	for _, h := range unstored {
		header.Del(h)
	}
	status := resp.StatusCode
	resp.Body = &teeBody{src: resp.Body, limit: f.maxEntry, done: func(body []byte) {
		now := time.Now()
		f.store.Put(st.key, &Entry{Status: status, Header: header, Body: body, StoredAt: now, Expires: now.Add(ttl), Vary: vary})
		f.metrics.Cache.WithLabelValues(f.route, "stored").Inc()
	}}
	return nil
}

func varyMatches(e *Entry, h http.Header) bool {
	for name, want := range e.Vary {
		if h.Get(name) != want {
			return false
		}
	}
	return true
}

type hit struct {
	entry *Entry
	head  bool
}

func (h *hit) ServedByGateway() {}

func (h *hit) Respond(w http.ResponseWriter, _ protocol.Kind) {
	// Stored headers exclude per-request gateway headers (CORS, RateLimit-*),
	// which respond() has already set for this request; merge, don't replace.
	dst := w.Header()
	pipeline.MergeHeaders(dst, h.entry.Header)
	dst.Set("Age", strconv.Itoa(int(time.Since(h.entry.StoredAt).Seconds())))
	dst.Set("X-Cache", "HIT")
	dst.Set("Content-Length", strconv.Itoa(len(h.entry.Body)))
	w.WriteHeader(h.entry.Status)
	if !h.head {
		_, _ = w.Write(h.entry.Body)
	}
}

// teeBody copies the stream into memory and calls done with the complete
// body at EOF. Bodies over limit, read errors and early closes are not stored.
type teeBody struct {
	src   io.ReadCloser
	buf   bytes.Buffer
	limit int64
	done  func([]byte)
	over  bool
}

func (t *teeBody) Read(p []byte) (int, error) {
	n, err := t.src.Read(p)
	if n > 0 && !t.over {
		if int64(t.buf.Len()+n) > t.limit {
			t.over = true
			t.buf = bytes.Buffer{}
		} else {
			t.buf.Write(p[:n])
		}
	}
	if err == io.EOF && !t.over && t.done != nil {
		t.done(bytes.Clone(t.buf.Bytes()))
		t.done = nil
	}
	return n, err
}

func (t *teeBody) Close() error { return t.src.Close() }

type cacheControl struct {
	noStore, noCache, private bool
	maxAge, sMaxAge           int
}

func parseCacheControl(v string) cacheControl {
	cc := cacheControl{maxAge: -1, sMaxAge: -1}
	for _, d := range strings.Split(v, ",") {
		name, val, _ := strings.Cut(strings.TrimSpace(d), "=")
		val = strings.Trim(val, `"`)
		switch strings.ToLower(name) {
		case "no-store":
			cc.noStore = true
		case "no-cache":
			cc.noCache = true
		case "private":
			cc.private = true
		case "max-age":
			if n, err := strconv.Atoi(val); err == nil {
				cc.maxAge = n
			}
		case "s-maxage":
			if n, err := strconv.Atoi(val); err == nil {
				cc.sMaxAge = n
			}
		}
	}
	return cc
}

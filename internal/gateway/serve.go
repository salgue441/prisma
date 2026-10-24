package gateway

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"prisma/internal/gwerr"
	"prisma/internal/pipeline"
	"prisma/internal/protocol"
	"prisma/internal/router"
)

var proxyErrorLog = log.New(slogWriter{}, "", 0)

type slogWriter struct{}

func (slogWriter) Write(p []byte) (int, error) {
	slog.Debug("proxy", "msg", strings.TrimSpace(string(p)))
	return len(p), nil
}

// Handler returns the request handler for the named listener.
func (g *Gateway) Handler(listener string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.serve(listener, w, r)
	})
}

func (g *Gateway) serve(listener string, w http.ResponseWriter, r *http.Request) {
	snap := g.snap.Load()
	if snap == nil {
		gwerr.Unavailable("gateway is starting").Respond(w, protocol.Detect(r, ""))
		return
	}
	rec := &recorder{ResponseWriter: w}
	peer := peerAddr(r.RemoteAddr)
	trustedPeer := snap.isTrusted(peer)

	c := &pipeline.Context{
		Listener:     listener,
		Start:        time.Now(),
		OriginalPath: r.URL.Path,
		ClientIP:     clientIP(r, peer, trustedPeer, snap),
		RequestID:    requestID(r, trustedPeer),
		RouteName:    "-",
	}
	defer g.finish(c, rec, snap)

	// Identity headers are only ever set by the gateway itself.
	for k := range r.Header {
		if strings.HasPrefix(k, "X-Prisma-") {
			delete(r.Header, k)
		}
	}
	if p := router.CleanPath(r.URL.Path); p != r.URL.Path {
		r.URL.Path, r.URL.RawPath = p, ""
	}
	rec.Header().Set("X-Request-Id", c.RequestID)

	route := snap.Router.Match(listener, r)
	if route == nil {
		c.Protocol = protocol.Detect(r, "")
		c.Request = r
		c.Rejection = "router"
		respond(rec, c, gwerr.NotFound("no route matches the request"))
		return
	}
	rc := route.Config
	c.RouteName = rc.Name
	c.Protocol = protocol.Detect(r, rc.Type)

	global := snap.Config.Global
	if !c.Protocol.IsStreaming() {
		maxBody := int64(rc.MaxBodyBytes)
		if maxBody == 0 {
			maxBody = int64(global.MaxBodyBytes)
		}
		if r.ContentLength > maxBody {
			c.Request = r
			c.Rejection = "body_limit"
			respond(rec, c, gwerr.PayloadTooLarge())
			return
		}
		r.Body = http.MaxBytesReader(rec, r.Body, maxBody)
	}

	ctx := r.Context()
	timeout := global.RequestTimeout
	switch {
	case rc.Timeout != nil:
		timeout = *rc.Timeout
	case c.Protocol.IsStreaming():
		// Streams rely on grpc-timeout or idle timeouts instead.
		timeout = 0
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	c.Request = r.WithContext(pipeline.WithContext(ctx, c))
	route.Handler.ServeHTTP(rec, c.Request)
}

func (g *Gateway) finish(c *pipeline.Context, rec *recorder, snap *Snapshot) {
	status := rec.status
	if status == 0 {
		status = http.StatusOK
	}
	grpcStatus := ""
	if c.Protocol.IsGRPC() {
		h := rec.Header()
		if grpcStatus = h.Get("Grpc-Status"); grpcStatus == "" {
			grpcStatus = h.Get(http.TrailerPrefix + "Grpc-Status")
		}
	}
	elapsed := time.Since(c.Start)
	proto := c.Protocol.String()
	g.metrics.Requests.WithLabelValues(c.Listener, c.RouteName, proto, strconv.Itoa(status), grpcStatus).Inc()
	g.metrics.Duration.WithLabelValues(c.RouteName, proto).Observe(elapsed.Seconds())

	if snap.Config.Global.AccessLog.Disabled {
		return
	}
	attrs := []slog.Attr{
		slog.String("request_id", c.RequestID),
		slog.String("listener", c.Listener),
		slog.String("route", c.RouteName),
		slog.String("protocol", proto),
		slog.String("client_ip", c.Var("client_ip")),
		slog.String("method", methodOf(c)),
		slog.String("host", hostOf(c)),
		// Query strings are omitted: they often carry credentials.
		slog.String("path", c.OriginalPath),
		slog.Int("status", status),
		slog.Int64("bytes", rec.bytes),
		slog.Float64("duration_ms", ms(elapsed)),
	}
	if grpcStatus != "" {
		attrs = append(attrs, slog.String("grpc_status", grpcStatus))
	}
	if c.GraphQL != nil {
		attrs = append(attrs, slog.String("graphql_operation", c.GraphQL.OperationType+" "+c.GraphQL.OperationName),
			slog.Int("graphql_cost", c.GraphQL.Cost))
	}
	if c.Principal != nil {
		attrs = append(attrs, slog.String("consumer", c.Principal.Consumer), slog.String("auth_method", c.Principal.Method))
	}
	if c.Cluster != "" {
		attrs = append(attrs, slog.String("cluster", c.Cluster), slog.String("upstream", c.Endpoint),
			slog.Int("attempts", c.Attempts), slog.Float64("upstream_ms", ms(c.UpstreamLatency)))
	}
	if c.Rejection != "" {
		attrs = append(attrs, slog.String("rejected_by", c.Rejection))
	}
	if c.Error != "" {
		attrs = append(attrs, slog.String("error", c.Error))
	}
	g.access.LogAttrs(context.Background(), slog.LevelInfo, "access", attrs...)
}

func methodOf(c *pipeline.Context) string {
	if c.Request == nil {
		return ""
	}
	return c.Request.Method
}

func hostOf(c *pipeline.Context) string {
	if c.Request == nil {
		return ""
	}
	return c.Request.Host
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func peerAddr(remote string) netip.Addr {
	if ap, err := netip.ParseAddrPort(remote); err == nil {
		return ap.Addr().Unmap()
	}
	a, _ := netip.ParseAddr(remote)
	return a.Unmap()
}

// clientIP walks X-Forwarded-For right to left, skipping trusted proxies; the
// first untrusted hop is the client. Headers from untrusted peers are ignored.
func clientIP(r *http.Request, peer netip.Addr, trustedPeer bool, snap *Snapshot) netip.Addr {
	if !trustedPeer {
		return peer
	}
	hops := r.Header.Values("X-Forwarded-For")
	for i := len(hops) - 1; i >= 0; i-- {
		parts := strings.Split(hops[i], ",")
		for j := len(parts) - 1; j >= 0; j-- {
			a, err := netip.ParseAddr(strings.TrimSpace(parts[j]))
			if err != nil {
				return peer
			}
			a = a.Unmap()
			if !snap.isTrusted(a) {
				return a
			}
		}
	}
	return peer
}

// requestID honors an inbound X-Request-Id from trusted proxies, otherwise
// generates a time-ordered UUIDv7.
func requestID(r *http.Request, trustedPeer bool) string {
	if trustedPeer {
		if id := r.Header.Get("X-Request-Id"); id != "" && len(id) <= 128 && printable(id) {
			return id
		}
	}
	var b [16]byte
	_, _ = rand.Read(b[:])
	binary.BigEndian.PutUint64(b[:8], uint64(time.Now().UnixMilli())<<16|uint64(b[6])<<8|uint64(b[7]))
	b[6] = 0x70 | b[6]&0x0f
	b[8] = 0x80 | b[8]&0x3f
	var s [36]byte
	hex.Encode(s[0:8], b[0:4])
	s[8] = '-'
	hex.Encode(s[9:13], b[4:6])
	s[13] = '-'
	hex.Encode(s[14:18], b[6:8])
	s[18] = '-'
	hex.Encode(s[19:23], b[8:10])
	s[23] = '-'
	hex.Encode(s[24:], b[10:])
	return string(s[:])
}

func printable(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// recorder captures status and size while staying transparent to
// http.ResponseController (flush, hijack, deadlines) via Unwrap.
type recorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (r *recorder) WriteHeader(code int) {
	if code >= 200 && r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(p)
	r.bytes += int64(n)
	return n, err
}

func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (r *recorder) Flush() {
	_ = http.NewResponseController(r.ResponseWriter).Flush()
}

func (r *recorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return http.NewResponseController(r.ResponseWriter).Hijack()
}

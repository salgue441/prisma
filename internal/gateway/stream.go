package gateway

import (
	"bytes"
	"cmp"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"prisma/internal/config"
	"prisma/internal/upstream"
)

const (
	// defaultStreamIdle closes connections with no traffic either way.
	defaultStreamIdle = time.Hour
	// handshakeTimeout bounds reading the ClientHello (passthrough) or the
	// whole handshake (termination).
	handshakeTimeout = 10 * time.Second
	// streamDialAttempts is how many endpoints a connection may try.
	streamDialAttempts = 3
)

// streamRoute is a compiled tcp route.
type streamRoute struct {
	name     string
	index    int
	priority int
	exact    []string // lowercase SNI names
	wildcard []string // suffixes, e.g. ".example.com"
	allow    []netip.Prefix
	deny     []netip.Prefix
	idle     time.Duration
	backends []streamBackend
	total    int
}

type streamBackend struct {
	cluster *upstream.Cluster
	weight  int
}

// streamTable holds the tcp routes of one listener.
type streamTable struct{ routes []*streamRoute }

func buildStreamRoute(rc *config.Route, index int, clusters map[string]*upstream.Cluster) (*streamRoute, error) {
	sr := &streamRoute{name: rc.Name, index: index, priority: rc.Priority, idle: defaultStreamIdle}
	if rc.Timeout != nil {
		sr.idle = *rc.Timeout
	}
	for _, h := range rc.Match.Hosts {
		h = strings.ToLower(h)
		if s, ok := strings.CutPrefix(h, "*"); ok {
			sr.wildcard = append(sr.wildcard, s)
		} else {
			sr.exact = append(sr.exact, h)
		}
	}
	if z := rc.AuthZ; z != nil {
		for _, c := range z.AllowIPs {
			sr.allow = append(sr.allow, netip.MustParsePrefix(c))
		}
		for _, c := range z.DenyIPs {
			sr.deny = append(sr.deny, netip.MustParsePrefix(c))
		}
	}
	for _, b := range rc.Backends {
		cl, ok := clusters[b.Cluster]
		if !ok {
			return nil, fmt.Errorf("unknown cluster %q", b.Cluster)
		}
		sr.backends = append(sr.backends, streamBackend{cl, b.Weight})
		sr.total += b.Weight
	}
	return sr, nil
}

// match picks the route for an SNI name: an exact name beats the longest
// matching wildcard, which beats routes without hosts. Ties go to the higher
// priority, then to the route listed first.
func (t *streamTable) match(sni string) *streamRoute {
	sni = strings.ToLower(strings.TrimSuffix(sni, "."))
	var best *streamRoute
	bestRank := -1
	for _, r := range t.routes {
		rank := -1
		switch {
		case len(r.exact) == 0 && len(r.wildcard) == 0:
			rank = 0
		case sni != "" && slices.Contains(r.exact, sni):
			rank = 1 << 20
		default:
			for _, w := range r.wildcard {
				if strings.HasSuffix(sni, w) && len(sni) > len(w) {
					rank = max(rank, len(w))
				}
			}
		}
		if rank < 0 {
			continue
		}
		if best == nil || rank > bestRank || (rank == bestRank && (r.priority > best.priority ||
			(r.priority == best.priority && r.index < best.index))) {
			best, bestRank = r, rank
		}
	}
	return best
}

func (r *streamRoute) permits(ip netip.Addr) bool {
	ip = ip.Unmap()
	for _, p := range r.deny {
		if p.Contains(ip) {
			return false
		}
	}
	if len(r.allow) == 0 {
		return true
	}
	for _, p := range r.allow {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

func (r *streamRoute) pickBackend() *upstream.Cluster {
	if len(r.backends) == 1 || r.total <= 0 {
		return r.backends[0].cluster
	}
	n := rand.IntN(r.total)
	for _, b := range r.backends {
		if n < b.weight {
			return b.cluster
		}
		n -= b.weight
	}
	return r.backends[len(r.backends)-1].cluster
}

// buildStreams compiles tcp routes into per-listener tables.
func buildStreams(cfg *config.Config, clusters map[string]*upstream.Cluster) (map[string]*streamTable, error) {
	tables := map[string]*streamTable{}
	var errs []error
	for i := range cfg.Routes {
		rc := &cfg.Routes[i]
		if rc.Type != config.RouteTCP {
			continue
		}
		sr, err := buildStreamRoute(rc, i, clusters)
		if err != nil {
			errs = append(errs, fmt.Errorf("route %s: %w", rc.Name, err))
			continue
		}
		for _, l := range rc.Listeners {
			t := tables[l]
			if t == nil {
				t = &streamTable{}
				tables[l] = t
			}
			t.routes = append(t.routes, sr)
		}
	}
	for _, t := range tables {
		slices.SortStableFunc(t.routes, func(a, b *streamRoute) int { return cmp.Compare(a.index, b.index) })
	}
	return tables, errors.Join(errs...)
}

// streamServer proxies the connections of a tcp or tls-passthrough listener.
type streamServer struct {
	gw        *Gateway
	listener  string
	tlsConfig *tls.Config // set when terminating TLS
	passthru  bool

	ln     net.Listener
	mu     sync.Mutex
	conns  map[net.Conn]struct{}
	wg     sync.WaitGroup
	closed atomic.Bool
}

func (s *streamServer) serve(ln net.Listener) error {
	s.mu.Lock()
	s.ln = ln
	s.conns = map[net.Conn]struct{}{}
	closed := s.closed.Load()
	s.mu.Unlock()
	if closed { // shut down before serving started
		ln.Close()
		return nil
	}
	for {
		c, err := ln.Accept()
		if err != nil {
			if s.closed.Load() {
				return nil
			}
			return err
		}
		if !s.track(c, true) {
			c.Close()
			return nil
		}
		s.wg.Go(func() {
			defer s.track(c, false)
			s.handle(c)
		})
	}
}

// track registers or forgets a connection so Shutdown can force-close it.
func (s *streamServer) track(c net.Conn, add bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !add {
		delete(s.conns, c)
		return true
	}
	if s.closed.Load() {
		return false
	}
	s.conns[c] = struct{}{}
	return true
}

// shutdown stops accepting and waits for open connections to finish until
// ctx expires, then closes the rest.
func (s *streamServer) shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.closed.Store(true)
	if s.ln != nil {
		s.ln.Close()
	}
	s.mu.Unlock()
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		s.mu.Lock()
		for c := range s.conns {
			c.Close()
		}
		s.mu.Unlock()
		<-done
		return ctx.Err()
	}
}

// streamLog accumulates one connection's access log fields.
type streamLog struct {
	start      time.Time
	clientIP   netip.Addr
	sni        string
	route      string
	cluster    string
	upstream   string
	attempts   int
	bytesUp    int64
	bytesDown  int64
	result     string
	reason     string
	terminated bool
}

func (s *streamServer) handle(c net.Conn) {
	l := &streamLog{start: time.Now(), result: "closed"}
	if ap, err := netip.ParseAddrPort(c.RemoteAddr().String()); err == nil {
		l.clientIP = ap.Addr().Unmap()
	}
	m := s.gw.metrics
	m.StreamActive.WithLabelValues(s.listener).Inc()
	defer func() {
		m.StreamActive.WithLabelValues(s.listener).Dec()
		route := l.route
		if route == "" {
			route = "-"
		}
		m.StreamConnections.WithLabelValues(s.listener, route, l.result).Inc()
		s.logAccess(l)
	}()

	client := c
	switch {
	case s.passthru:
		sni, replay, err := peekSNI(c)
		if err != nil {
			c.Close()
			l.result, l.reason = "bad_handshake", err.Error()
			return
		}
		l.sni, client = sni, replay
	case s.tlsConfig != nil:
		tc := tls.Server(c, s.tlsConfig)
		_ = c.SetDeadline(time.Now().Add(handshakeTimeout))
		if err := tc.Handshake(); err != nil {
			c.Close()
			l.result, l.reason = "bad_handshake", err.Error()
			return
		}
		_ = c.SetDeadline(time.Time{})
		l.sni, l.terminated, client = tc.ConnectionState().ServerName, true, tc
	}
	defer client.Close()

	snap := s.gw.snap.Load()
	t := snap.Streams[s.listener]
	var r *streamRoute
	if t != nil {
		r = t.match(l.sni)
	}
	if r == nil {
		l.result, l.reason = "no_route", "no tcp route matches"
		return
	}
	l.route = r.name
	if !r.permits(l.clientIP) {
		l.result, l.reason = "denied", "client IP not allowed"
		return
	}
	cl := r.pickBackend()
	l.cluster = cl.Name
	ctx, cancel := context.WithTimeout(context.Background(), cl.Config.ConnectTimeout*streamDialAttempts)
	up, err := cl.DialStream(ctx, l.clientIP, streamDialAttempts)
	cancel()
	if err != nil {
		l.result, l.reason = "upstream_error", err.Error()
		return
	}
	defer up.Close()
	l.upstream, l.attempts = up.Endpoint, up.Attempts

	l.bytesUp, l.bytesDown, l.result = pipe(client, up, r.idle)
	m.StreamBytes.WithLabelValues(s.listener, r.name, "upstream").Add(float64(l.bytesUp))
	m.StreamBytes.WithLabelValues(s.listener, r.name, "downstream").Add(float64(l.bytesDown))
}

func (s *streamServer) logAccess(l *streamLog) {
	attrs := []slog.Attr{
		slog.String("listener", s.listener), slog.String("protocol", "tcp"),
		slog.String("client_ip", l.clientIP.String()), slog.String("result", l.result),
		slog.Int64("bytes_up", l.bytesUp), slog.Int64("bytes_down", l.bytesDown),
		slog.Int64("duration_ms", time.Since(l.start).Milliseconds()),
	}
	for _, kv := range [][2]string{{"route", l.route}, {"sni", l.sni}, {"cluster", l.cluster}, {"upstream", l.upstream}, {"reason", l.reason}} {
		if kv[1] != "" {
			attrs = append(attrs, slog.String(kv[0], kv[1]))
		}
	}
	if l.attempts > 0 {
		attrs = append(attrs, slog.Int("attempts", l.attempts))
	}
	if l.terminated {
		attrs = append(attrs, slog.Bool("tls_terminated", true))
	}
	s.gw.access.LogAttrs(context.Background(), slog.LevelInfo, "access", attrs...)
}

// pipe copies both directions until both are done or the connection is idle
// for longer than idle. An EOF from one side half-closes the other, so
// protocols that rely on half-close keep working. It returns the bytes sent
// upstream and downstream, and how the connection ended.
func pipe(client net.Conn, up *upstream.StreamConn, idle time.Duration) (upBytes, downBytes int64, result string) {
	var last atomic.Int64
	last.Store(time.Now().UnixNano())
	var timedOut atomic.Bool
	stop := make(chan struct{})
	go func() {
		tick := time.NewTicker(min(idle/4, 5*time.Second) + time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				if time.Since(time.Unix(0, last.Load())) > idle {
					timedOut.Store(true)
					client.Close()
					up.Close()
					return
				}
			}
		}
	}()

	var wg sync.WaitGroup
	wg.Go(func() {
		upBytes = copyActive(up, client, &last)
		closeWrite(up)
	})
	wg.Go(func() {
		downBytes = copyActive(client, up, &last)
		closeWrite(client)
	})
	wg.Wait()
	close(stop)
	if timedOut.Load() {
		return upBytes, downBytes, "idle_timeout"
	}
	return upBytes, downBytes, "closed"
}

// closeWrite half-closes c, or closes it when half-close is unsupported so
// the peer still sees the end of the stream.
func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok && cw.CloseWrite() == nil {
		return
	}
	c.Close()
}

// copyActive copies src to dst, recording the time of each transfer.
func copyActive(dst io.Writer, src io.Reader, last *atomic.Int64) int64 {
	buf := make([]byte, 32<<10)
	var n int64
	for {
		nr, rerr := src.Read(buf)
		if nr > 0 {
			last.Store(time.Now().UnixNano())
			nw, werr := dst.Write(buf[:nr])
			n += int64(nw)
			if werr != nil {
				return n
			}
		}
		if rerr != nil {
			return n
		}
	}
}

// peekSNI reads the TLS ClientHello without terminating TLS and returns the
// server name plus a connection that replays the bytes read so far.
func peekSNI(c net.Conn) (string, net.Conn, error) {
	_ = c.SetReadDeadline(time.Now().Add(handshakeTimeout))
	defer c.SetReadDeadline(time.Time{})
	var buf bytes.Buffer
	var sni string
	seen := false
	errStop := errors.New("hello read")
	// crypto/tls parses the hello; GetConfigForClient stops the handshake
	// before anything is written to the client.
	err := tls.Server(readOnlyConn{r: io.TeeReader(c, &buf), Conn: c}, &tls.Config{
		GetConfigForClient: func(h *tls.ClientHelloInfo) (*tls.Config, error) {
			sni, seen = h.ServerName, true
			return nil, errStop
		},
	}).Handshake()
	if !seen {
		return "", nil, fmt.Errorf("reading TLS client hello: %w", err)
	}
	return sni, &replayConn{Conn: c, r: io.MultiReader(bytes.NewReader(buf.Bytes()), c)}, nil
}

// readOnlyConn feeds reads from r and drops writes, so the TLS parser can
// never send anything to the client.
type readOnlyConn struct {
	net.Conn
	r io.Reader
}

func (c readOnlyConn) Read(p []byte) (int, error)  { return c.r.Read(p) }
func (c readOnlyConn) Write(p []byte) (int, error) { return len(p), nil }
func (c readOnlyConn) Close() error                { return nil }

// replayConn returns the peeked bytes before reading on from the socket.
type replayConn struct {
	net.Conn
	r io.Reader
}

func (c *replayConn) Read(p []byte) (int, error) { return c.r.Read(p) }

func (c *replayConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return errors.ErrUnsupported
}

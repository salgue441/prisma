package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"prisma/internal/config"
)

// ListenerSet owns the gateway's sockets and servers and applies listener
// changes without a restart. Each bound address has one acceptor goroutine
// that hands connections to whichever server is current for it, so a
// changed listener (protocols, TLS, timeouts, PROXY protocol) is swapped on
// the same socket: no connection is refused and in-flight requests on the
// old server drain gracefully.
type ListenerSet struct {
	gw    *Gateway
	drain time.Duration

	mu      sync.Mutex
	sockets map[string]*socket // by address
	servers map[string]*Server // by listener name
	errs    chan error
}

func (g *Gateway) NewListenerSet(drain time.Duration) *ListenerSet {
	return &ListenerSet{gw: g, drain: drain, sockets: map[string]*socket{}, servers: map[string]*Server{}, errs: make(chan error, 16)}
}

// Errors reports fatal serve errors.
func (ls *ListenerSet) Errors() <-chan error { return ls.errs }

// Addr returns the bound address of a listener (useful with port 0).
func (ls *ListenerSet) Addr(name string) string {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if s, ok := ls.servers[name]; ok {
		return s.sock.ln.Addr().String()
	}
	return ""
}

// Apply reconciles running listeners with cfgs. It binds new addresses and
// builds new servers first; any error aborts with nothing changed.
func (ls *ListenerSet) Apply(cfgs []config.Listener) error {
	ls.mu.Lock()
	defer ls.mu.Unlock()

	var opened []*socket
	abort := func(err error) error {
		for _, s := range opened {
			s.close()
		}
		return err
	}
	want := map[string]bool{}
	type swap struct {
		srv  *Server
		sock *socket
	}
	var swaps []swap
	for _, lc := range cfgs {
		want[lc.Name] = true
		cur, exists := ls.servers[lc.Name]
		if exists && reflect.DeepEqual(cur.cfg, lc) {
			if err := cur.ReloadCertificates(); err != nil {
				slog.Error("certificate reload failed", "listener", lc.Name, "err", err)
			}
			continue
		}
		sock := ls.sockets[lc.Address]
		if sock == nil {
			for _, s := range opened {
				if s.addr == lc.Address {
					sock = s
				}
			}
		}
		if sock == nil {
			ln, err := net.Listen("tcp", lc.Address)
			if err != nil {
				return abort(fmt.Errorf("listener %s: %w", lc.Name, err))
			}
			sock = newSocket(lc.Address, ln)
			opened = append(opened, sock)
		}
		srv, err := ls.gw.NewServer(lc)
		if err != nil {
			return abort(err)
		}
		swaps = append(swaps, swap{srv, sock})
	}

	// Commit: nothing below can fail.
	for _, s := range opened {
		ls.sockets[s.addr] = s
		go s.acceptLoop()
	}
	for _, sw := range swaps {
		old := ls.servers[sw.srv.Name]
		sw.srv.attach(sw.sock)
		ls.servers[sw.srv.Name] = sw.srv
		go ls.serve(sw.srv)
		if old != nil {
			action := "updated"
			if old.sock != sw.sock {
				action = "moved"
			}
			slog.Info("listener "+action, "listener", old.Name, "address", sw.sock.ln.Addr().String())
			ls.retire(old)
		}
	}
	for name, old := range ls.servers {
		if !want[name] {
			delete(ls.servers, name)
			slog.Info("listener removed", "listener", name)
			ls.retire(old)
		}
	}
	// Close sockets no server uses any more.
	for addr, sock := range ls.sockets {
		inUse := false
		for _, s := range ls.servers {
			if s.sock == sock {
				inUse = true
			}
		}
		if !inUse {
			sock.close()
			delete(ls.sockets, addr)
		}
	}
	return nil
}

func (ls *ListenerSet) serve(s *Server) {
	if err := s.Serve(); err != nil && !errors.Is(err, net.ErrClosed) {
		select {
		case ls.errs <- fmt.Errorf("listener %s: %w", s.Name, err):
		default:
		}
	}
}

// retire drains a replaced server in the background: it stops receiving new
// connections immediately and in-flight requests get the drain timeout.
func (ls *ListenerSet) retire(s *Server) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), ls.drain)
		defer cancel()
		if err := s.Shutdown(ctx); err != nil {
			slog.Warn("listener did not drain in time", "listener", s.Name, "err", err)
		}
	}()
}

// Shutdown drains every server and closes every socket.
func (ls *ListenerSet) Shutdown(ctx context.Context) {
	ls.mu.Lock()
	servers := make([]*Server, 0, len(ls.servers))
	for _, s := range ls.servers {
		servers = append(servers, s)
	}
	ls.mu.Unlock()
	var wg sync.WaitGroup
	for _, s := range servers {
		wg.Go(func() {
			if err := s.Shutdown(ctx); err != nil {
				slog.Warn("listener did not drain in time", "listener", s.Name, "err", err)
			}
		})
	}
	wg.Wait()
	ls.mu.Lock()
	for addr, sock := range ls.sockets {
		sock.close()
		delete(ls.sockets, addr)
	}
	ls.mu.Unlock()
}

// socket is a bound address whose connections go to the current target.
type socket struct {
	addr   string
	ln     net.Listener
	target atomic.Pointer[chanListener]
}

func newSocket(addr string, ln net.Listener) *socket { return &socket{addr: addr, ln: ln} }

func (s *socket) acceptLoop() {
	backoff := 5 * time.Millisecond
	for {
		c, err := s.ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			slog.Warn("accept failed", "address", s.addr, "err", err)
			time.Sleep(backoff)
			backoff = min(backoff*2, time.Second)
			continue
		}
		backoff = 5 * time.Millisecond
		s.deliver(c)
	}
}

// deliver hands c to the current server, following swaps that happen while
// waiting so a connection is never handed to a server that is shutting down.
func (s *socket) deliver(c net.Conn) {
	for {
		t := s.target.Load()
		if t == nil {
			c.Close()
			return
		}
		select {
		case t.conns <- c:
			return
		case <-t.done:
			if s.target.Load() == t {
				c.Close()
				return
			}
		}
	}
}

func (s *socket) close() { s.ln.Close() }

// chanListener is the net.Listener a Server sees: connections arrive from the
// socket's acceptor; closing it detaches the server without closing the socket.
type chanListener struct {
	addr  net.Addr
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
}

func (l *chanListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *chanListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *chanListener) Addr() net.Addr { return l.addr }

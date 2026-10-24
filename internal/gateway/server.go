package gateway

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"sync/atomic"

	"prisma/internal/config"
	"prisma/internal/proxyproto"
)

// Server is one configured listener.
type Server struct {
	Name  string
	cfg   config.Listener
	srv   *http.Server
	certs atomic.Pointer[[]tls.Certificate]
	ln    net.Listener
	sock  *socket // set when managed by a ListenerSet
}

func (g *Gateway) NewServer(l config.Listener) (*Server, error) {
	s := &Server{Name: l.Name, cfg: l}
	p := new(http.Protocols)
	for _, proto := range l.Protocols {
		switch proto {
		case "http1":
			p.SetHTTP1(true)
		case "http2":
			p.SetHTTP2(true)
		case "h2c":
			p.SetUnencryptedHTTP2(true)
		}
	}
	s.srv = &http.Server{
		Handler:           g.Handler(l.Name),
		Protocols:         p,
		ReadHeaderTimeout: l.ReadHeaderTimeout,
		IdleTimeout:       l.IdleTimeout,
		MaxHeaderBytes:    int(l.MaxHeaderBytes),
		HTTP2:             &http.HTTP2Config{MaxConcurrentStreams: l.MaxConcurrentStreams},
		// TLS handshake failures and similar are noisy; keep them at debug.
		ErrorLog: log.New(slogWriter{}, "", 0),
	}
	if l.TLS != nil {
		if err := s.ReloadCertificates(); err != nil {
			return nil, err
		}
		tc, err := s.tlsConfig()
		if err != nil {
			return nil, err
		}
		s.srv.TLSConfig = tc
	}
	return s, nil
}

func (s *Server) tlsConfig() (*tls.Config, error) {
	t := s.cfg.TLS
	tc := &tls.Config{MinVersion: tls.VersionTLS12}
	if t.MinVersion == "1.3" {
		tc.MinVersion = tls.VersionTLS13
	}
	tc.GetCertificate = func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
		certs := *s.certs.Load()
		for i := range certs {
			if hello.SupportsCertificate(&certs[i]) == nil {
				return &certs[i], nil
			}
		}
		return &certs[0], nil
	}
	if t.ClientCAFile != "" {
		pem, err := os.ReadFile(t.ClientCAFile)
		if err != nil {
			return nil, err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("listener %s: no certificates in %s", s.Name, t.ClientCAFile)
		}
		tc.ClientCAs = pool
		switch t.ClientAuth {
		case "require":
			tc.ClientAuth = tls.RequireAndVerifyClientCert
		case "request":
			tc.ClientAuth = tls.VerifyClientCertIfGiven
		}
	}
	return tc, nil
}

// ReloadCertificates rereads certificate files; new handshakes use them
// immediately, existing connections are unaffected.
func (s *Server) ReloadCertificates() error {
	if s.cfg.TLS == nil {
		return nil
	}
	var certs []tls.Certificate
	for _, c := range s.cfg.TLS.Certificates {
		var cert tls.Certificate
		var err error
		if c.CertPEM != "" {
			cert, err = tls.X509KeyPair([]byte(c.CertPEM), []byte(c.KeyPEM))
		} else {
			cert, err = tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
		}
		if err != nil {
			return fmt.Errorf("listener %s: %w", s.Name, err)
		}
		certs = append(certs, cert)
	}
	s.certs.Store(&certs)
	return nil
}

// Listen binds the socket so bind errors surface before serving starts.
func (s *Server) Listen() error {
	ln, err := net.Listen("tcp", s.cfg.Address)
	if err != nil {
		return fmt.Errorf("listener %s: %w", s.Name, err)
	}
	s.ln = s.wrap(ln)
	return nil
}

// attach makes s the current server for a ListenerSet socket.
func (s *Server) attach(sock *socket) {
	cl := &chanListener{addr: sock.ln.Addr(), conns: make(chan net.Conn), done: make(chan struct{})}
	s.sock = sock
	s.ln = s.wrap(cl)
	sock.target.Store(cl)
}

// wrap adds PROXY protocol parsing below TLS (the header precedes the
// handshake).
func (s *Server) wrap(ln net.Listener) net.Listener {
	pp := s.cfg.ProxyProtocol
	if pp == nil {
		return ln
	}
	pl := &proxyproto.Listener{Listener: ln, Optional: pp.Optional, HeaderTimeout: pp.HeaderTimeout}
	for _, c := range pp.TrustedCIDRs {
		pl.Trusted = append(pl.Trusted, netip.MustParsePrefix(c))
	}
	return pl
}

// Addr returns the bound address (useful with port 0).
func (s *Server) Addr() string { return s.ln.Addr().String() }

func (s *Server) Serve() error {
	slog.Info("listening", "listener", s.Name, "address", s.Addr(), "tls", s.cfg.TLS != nil,
		"protocols", strings.Join(s.cfg.Protocols, ","))
	var err error
	if s.cfg.TLS != nil {
		err = s.srv.ServeTLS(s.ln, "", "")
	} else {
		err = s.srv.Serve(s.ln)
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Shutdown stops accepting, sends GOAWAY to HTTP/2 clients and waits for
// in-flight requests until ctx expires.
func (s *Server) Shutdown(ctx context.Context) error { return s.srv.Shutdown(ctx) }

// Config returns the listener config this server was built from.
func (s *Server) Config() config.Listener { return s.cfg }

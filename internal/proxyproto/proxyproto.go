// Package proxyproto implements the receiving side of the HAProxy PROXY
// protocol (v1 text and v2 binary), recovering the original client address
// when the gateway sits behind an L4 load balancer.
package proxyproto

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"
)

var v2Signature = []byte("\r\n\r\n\x00\r\nQUIT\n")

// Listener wraps a net.Listener. Connections from Trusted peers must start
// with a PROXY header (or may, when Optional); connections from any other
// peer are passed through untouched so they cannot spoof addresses.
type Listener struct {
	net.Listener
	Trusted       []netip.Prefix // empty trusts every peer
	Optional      bool
	HeaderTimeout time.Duration
}

func (l *Listener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	if !l.trusted(c.RemoteAddr()) {
		return c, nil
	}
	timeout := l.HeaderTimeout
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	// Parsing is deferred to the first Read/RemoteAddr call, which happens on
	// the connection's own goroutine: a slow client cannot block Accept.
	return &Conn{Conn: c, br: bufio.NewReaderSize(c, 256), optional: l.Optional, timeout: timeout}, nil
}

func (l *Listener) trusted(a net.Addr) bool {
	if len(l.Trusted) == 0 {
		return true
	}
	ap, err := netip.ParseAddrPort(a.String())
	if err != nil {
		return false
	}
	for _, p := range l.Trusted {
		if p.Contains(ap.Addr().Unmap()) {
			return true
		}
	}
	return false
}

// Conn reports the address carried by the PROXY header as RemoteAddr.
type Conn struct {
	net.Conn
	br       *bufio.Reader
	optional bool
	timeout  time.Duration

	once   sync.Once
	remote net.Addr
	local  net.Addr
	err    error
}

func (c *Conn) init() {
	c.once.Do(func() {
		_ = c.Conn.SetReadDeadline(time.Now().Add(c.timeout))
		c.remote, c.local, c.err = parse(c.br, c.optional)
		_ = c.Conn.SetReadDeadline(time.Time{})
		if c.err != nil {
			c.Conn.Close()
		}
	})
}

func (c *Conn) Read(p []byte) (int, error) {
	c.init()
	if c.err != nil {
		return 0, c.err
	}
	return c.br.Read(p)
}

func (c *Conn) RemoteAddr() net.Addr {
	c.init()
	if c.remote != nil {
		return c.remote
	}
	return c.Conn.RemoteAddr()
}

func (c *Conn) LocalAddr() net.Addr {
	c.init()
	if c.local != nil {
		return c.local
	}
	return c.Conn.LocalAddr()
}

// parse consumes a PROXY header. It returns nil addresses for LOCAL/UNKNOWN
// headers (health checks from the load balancer itself).
func parse(br *bufio.Reader, optional bool) (remote, local net.Addr, err error) {
	peek, err := br.Peek(len(v2Signature))
	switch {
	case err == nil && bytes.Equal(peek, v2Signature):
		return parseV2(br)
	case len(peek) >= 6 && string(peek[:6]) == "PROXY ":
		return parseV1(br)
	case optional:
		return nil, nil, nil
	case err != nil && len(peek) < 6:
		return nil, nil, fmt.Errorf("proxyproto: reading header: %w", err)
	}
	return nil, nil, errors.New("proxyproto: connection did not start with a PROXY header")
}

func parseV1(br *bufio.Reader) (net.Addr, net.Addr, error) {
	// A v1 header is at most 107 bytes including CRLF.
	var line []byte
	for len(line) < 107 {
		b, err := br.ReadByte()
		if err != nil {
			return nil, nil, fmt.Errorf("proxyproto: v1: %w", err)
		}
		line = append(line, b)
		if b == '\n' {
			break
		}
	}
	s, ok := strings.CutSuffix(string(line), "\r\n")
	if !ok {
		return nil, nil, errors.New("proxyproto: v1 header too long or not CRLF-terminated")
	}
	f := strings.Fields(s)
	if len(f) >= 2 && f[1] == "UNKNOWN" {
		return nil, nil, nil
	}
	if len(f) != 6 || (f[1] != "TCP4" && f[1] != "TCP6") {
		return nil, nil, fmt.Errorf("proxyproto: malformed v1 header %q", s)
	}
	src, err1 := addrPort(f[2], f[4])
	dst, err2 := addrPort(f[3], f[5])
	if err := errors.Join(err1, err2); err != nil {
		return nil, nil, fmt.Errorf("proxyproto: v1: %w", err)
	}
	if (f[1] == "TCP4") != src.Addr().Is4() {
		return nil, nil, errors.New("proxyproto: v1 address family mismatch")
	}
	return net.TCPAddrFromAddrPort(src), net.TCPAddrFromAddrPort(dst), nil
}

func addrPort(ip, port string) (netip.AddrPort, error) {
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return netip.AddrPort{}, err
	}
	p, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("invalid port %q", port)
	}
	return netip.AddrPortFrom(a, uint16(p)), nil
}

func parseV2(br *bufio.Reader) (net.Addr, net.Addr, error) {
	hdr := make([]byte, 16)
	if _, err := io.ReadFull(br, hdr); err != nil {
		return nil, nil, fmt.Errorf("proxyproto: v2: %w", err)
	}
	verCmd, fam := hdr[12], hdr[13]
	n := int(binary.BigEndian.Uint16(hdr[14:16]))
	if verCmd>>4 != 2 {
		return nil, nil, errors.New("proxyproto: unsupported v2 version")
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(br, body); err != nil {
		return nil, nil, fmt.Errorf("proxyproto: v2: %w", err)
	}
	switch verCmd & 0x0f {
	case 0x0: // LOCAL: the load balancer's own connection
		return nil, nil, nil
	case 0x1: // PROXY
	default:
		return nil, nil, errors.New("proxyproto: unsupported v2 command")
	}
	var size int
	switch fam >> 4 {
	case 0x1:
		size = 4
	case 0x2:
		size = 16
	default:
		return nil, nil, nil // AF_UNSPEC / AF_UNIX: keep the socket address
	}
	if n < 2*size+4 {
		return nil, nil, errors.New("proxyproto: v2 address block too short")
	}
	src, _ := netip.AddrFromSlice(body[:size])
	dst, _ := netip.AddrFromSlice(body[size : 2*size])
	sport := binary.BigEndian.Uint16(body[2*size:])
	dport := binary.BigEndian.Uint16(body[2*size+2:])
	// Remaining bytes are TLVs, which the gateway does not need.
	return net.TCPAddrFromAddrPort(netip.AddrPortFrom(src, sport)),
		net.TCPAddrFromAddrPort(netip.AddrPortFrom(dst, dport)), nil
}

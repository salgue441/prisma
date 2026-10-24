package proxyproto

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func v2Header(cmd byte, src, dst netip.AddrPort, tlv []byte) []byte {
	var b bytes.Buffer
	b.Write(v2Signature)
	b.WriteByte(0x20 | cmd)
	addrs := append(src.Addr().AsSlice(), dst.Addr().AsSlice()...)
	fam := byte(0x11)
	if src.Addr().Is6() {
		fam = 0x21
	}
	b.WriteByte(fam)
	ports := make([]byte, 4)
	binary.BigEndian.PutUint16(ports, src.Port())
	binary.BigEndian.PutUint16(ports[2:], dst.Port())
	body := append(append(addrs, ports...), tlv...)
	_ = binary.Write(&b, binary.BigEndian, uint16(len(body)))
	b.Write(body)
	return b.Bytes()
}

func TestParse(t *testing.T) {
	src4, dst4 := netip.MustParseAddrPort("203.0.113.7:51000"), netip.MustParseAddrPort("10.0.0.1:443")
	src6 := netip.MustParseAddrPort("[2001:db8::1]:4000")
	cases := []struct {
		name     string
		in       []byte
		optional bool
		remote   string
		wantErr  bool
		rest     string
	}{
		{name: "v1 tcp4", in: []byte("PROXY TCP4 203.0.113.7 10.0.0.1 51000 443\r\nGET /"), remote: "203.0.113.7:51000", rest: "GET /"},
		{name: "v1 tcp6", in: []byte("PROXY TCP6 2001:db8::1 ::1 4000 443\r\nX"), remote: "[2001:db8::1]:4000", rest: "X"},
		{name: "v1 unknown", in: []byte("PROXY UNKNOWN\r\nX"), rest: "X"},
		{name: "v2 tcp4 with tlv", in: append(v2Header(1, src4, dst4, []byte{0x01, 0, 2, 'h', '2'}), "GET"...), remote: "203.0.113.7:51000", rest: "GET"},
		{name: "v2 tcp6", in: append(v2Header(1, src6, netip.MustParseAddrPort("[::1]:443"), nil), "Z"...), remote: "[2001:db8::1]:4000", rest: "Z"},
		{name: "v2 local", in: append(v2Header(0, src4, dst4, nil), "Z"...), rest: "Z"},
		{name: "missing header", in: []byte("GET / HTTP/1.1\r\n\r\n"), wantErr: true},
		{name: "missing header optional", in: []byte("GET / HTTP/1.1\r\n\r\n"), optional: true, rest: "GET / HTTP/1.1\r\n\r\n"},
		{name: "v1 bad family", in: []byte("PROXY TCP4 2001:db8::1 ::1 1 2\r\n"), wantErr: true},
		{name: "v1 garbage", in: []byte("PROXY TCP4 nope\r\n"), wantErr: true},
		{name: "v1 no crlf", in: []byte("PROXY TCP4 " + strings.Repeat("1", 200)), wantErr: true},
		{name: "v2 truncated", in: v2Header(1, src4, dst4, nil)[:20], wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			br := bufio.NewReaderSize(bytes.NewReader(tc.in), 256)
			remote, _, err := parse(br, tc.optional)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			got := ""
			if remote != nil {
				got = remote.String()
			}
			if got != tc.remote {
				t.Errorf("remote = %q, want %q", got, tc.remote)
			}
			if rest, _ := io.ReadAll(br); string(rest) != tc.rest {
				t.Errorf("remaining stream = %q, want %q", rest, tc.rest)
			}
		})
	}
}

func TestListenerDoesNotBlockAccept(t *testing.T) {
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l := &Listener{Listener: inner, HeaderTimeout: 200 * time.Millisecond}
	defer l.Close()

	silent, _ := net.Dial("tcp", inner.Addr().String()) // never sends a header
	defer silent.Close()
	good, _ := net.Dial("tcp", inner.Addr().String())
	defer good.Close()
	_, _ = good.Write([]byte("PROXY TCP4 198.51.100.9 10.0.0.1 1234 80\r\nhello"))

	c1, _ := l.Accept()
	c2, _ := l.Accept() // must not wait for the silent client's header
	start := time.Now()
	if c2.RemoteAddr().String() != "198.51.100.9:1234" {
		t.Errorf("remote = %s", c2.RemoteAddr())
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(c2, buf); err != nil || string(buf) != "hello" {
		t.Errorf("payload %q %v", buf, err)
	}
	if _, err := c1.Read(buf); err == nil {
		t.Error("silent connection should fail after the header timeout")
	}
	if time.Since(start) > 2*time.Second {
		t.Error("header timeout not enforced")
	}
}

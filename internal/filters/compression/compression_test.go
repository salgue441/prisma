package compression

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/gzip"
	"github.com/klauspost/compress/zstd"

	"prisma/internal/config"
	"prisma/internal/pipeline"
	"prisma/internal/protocol"
)

func TestNegotiate(t *testing.T) {
	pref := []string{"zstd", "gzip"}
	for ae, want := range map[string]string{
		"":                     "",
		"gzip":                 "gzip",
		"gzip, zstd":           "zstd",
		"gzip;q=1, zstd;q=0.5": "gzip",
		"zstd;q=0, gzip":       "gzip",
		"*":                    "zstd",
		"*;q=0.1, gzip;q=0":    "zstd",
		"br":                   "",
		"identity":             "",
		"GZIP ; q=0.8":         "gzip",
	} {
		if got := Negotiate(ae, pref); got != want {
			t.Errorf("Negotiate(%q) = %q, want %q", ae, got, want)
		}
	}
}

func response(ct, body string) *http.Response {
	return &http.Response{
		StatusCode:    200,
		Header:        http.Header{"Content-Type": {ct}, "Etag": {`"v1"`}},
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       httptest.NewRequest("GET", "/", nil),
	}
}

func TestCompressesAndRoundTrips(t *testing.T) {
	f := New(&config.Compression{})
	payload := strings.Repeat(`{"hello":"world"},`, 500)
	for _, alg := range []string{"gzip", "zstd"} {
		resp := response("application/json; charset=utf-8", payload)
		if err := f.OnResponse(&pipeline.Context{AcceptEncoding: alg}, resp); err != nil {
			t.Fatal(err)
		}
		if resp.Header.Get("Content-Encoding") != alg || resp.ContentLength != -1 || resp.Header.Get("Etag") != `W/"v1"` {
			t.Fatalf("%s: headers %v len %d", alg, resp.Header, resp.ContentLength)
		}
		compressed, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		var r io.Reader
		if alg == "gzip" {
			r, _ = gzip.NewReader(bytes.NewReader(compressed))
		} else {
			d, _ := zstd.NewReader(bytes.NewReader(compressed))
			defer d.Close()
			r = d
		}
		got, err := io.ReadAll(r)
		if err != nil || string(got) != payload {
			t.Fatalf("%s: round trip failed: %v", alg, err)
		}
		if len(compressed) >= len(payload)/4 {
			t.Errorf("%s: compressed %d -> %d bytes, expected a much smaller body", alg, len(payload), len(compressed))
		}
	}
}

func TestSkips(t *testing.T) {
	f := New(&config.Compression{MinBytes: 100})
	big := strings.Repeat("x", 500)
	cases := map[string]func() (*pipeline.Context, *http.Response){
		"small body": func() (*pipeline.Context, *http.Response) {
			return &pipeline.Context{AcceptEncoding: "gzip"}, response("text/plain", "tiny")
		},
		"binary type": func() (*pipeline.Context, *http.Response) {
			return &pipeline.Context{AcceptEncoding: "gzip"}, response("image/png", big)
		},
		"already encoded": func() (*pipeline.Context, *http.Response) {
			r := response("text/plain", big)
			r.Header.Set("Content-Encoding", "br")
			return &pipeline.Context{AcceptEncoding: "gzip"}, r
		},
		"no-transform": func() (*pipeline.Context, *http.Response) {
			r := response("text/plain", big)
			r.Header.Set("Cache-Control", "public, no-transform")
			return &pipeline.Context{AcceptEncoding: "gzip"}, r
		},
		"grpc": func() (*pipeline.Context, *http.Response) {
			return &pipeline.Context{AcceptEncoding: "gzip", Protocol: protocol.GRPC}, response("text/plain", big)
		},
		"client does not accept": func() (*pipeline.Context, *http.Response) {
			return &pipeline.Context{AcceptEncoding: "br"}, response("text/plain", big)
		},
	}
	for name, mk := range cases {
		c, resp := mk()
		_ = f.OnResponse(c, resp)
		if ce := resp.Header.Get("Content-Encoding"); ce == "gzip" {
			t.Errorf("%s: should not compress", name)
		}
	}
}

func TestEarlyCloseDoesNotLeakGoroutines(t *testing.T) {
	before := runtime.NumGoroutine()
	for i := 0; i < 50; i++ {
		pr, pw := io.Pipe() // an upstream that never finishes
		b := compress(pr, "gzip")
		_, _ = pw.Write([]byte("partial"))
		b.Close()
		pw.Close()
	}
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before+2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > before+2 {
		t.Errorf("goroutines grew from %d to %d", before, n)
	}
}

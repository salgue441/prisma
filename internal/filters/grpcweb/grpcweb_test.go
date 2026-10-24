package grpcweb

import (
	"encoding/base64"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/iotest"
)

func TestTextDecoderHandlesPaddedSegments(t *testing.T) {
	// Two independently padded segments, split across tiny reads.
	in := base64.StdEncoding.EncodeToString([]byte("hello")) + base64.StdEncoding.EncodeToString([]byte("grpc-web!")) + "\r\n"
	got, err := io.ReadAll(&textDecoder{src: iotest.OneByteReader(strings.NewReader(in))})
	if err != nil || string(got) != "hellogrpc-web!" {
		t.Fatalf("got %q, %v", got, err)
	}
	if _, err := io.ReadAll(&textDecoder{src: strings.NewReader("aGVsbG8")}); err == nil {
		t.Error("truncated base64 must fail")
	}
}

func TestTextEncoderRoundTrip(t *testing.T) {
	payload := strings.Repeat("0123456789", 100)
	enc, err := io.ReadAll(&textEncoder{src: iotest.HalfReader(strings.NewReader(payload))})
	if err != nil {
		t.Fatal(err)
	}
	dec, err := io.ReadAll(&textDecoder{src: strings.NewReader(string(enc))})
	if err != nil || string(dec) != payload {
		t.Fatalf("round trip failed: %v", err)
	}
}

func TestTrailerFrame(t *testing.T) {
	f := trailerFrame(http.Header{"Grpc-Status": {"0"}, "Grpc-Message": {"ok"}})
	if f[0] != trailerFlag || string(f[5:]) != "grpc-message: ok\r\ngrpc-status: 0\r\n" {
		t.Errorf("frame = %q", f)
	}
	if missing := string(trailerFrame(nil)[5:]); !strings.Contains(missing, "grpc-status: 13") {
		t.Errorf("missing trailers must become INTERNAL, got %q", missing)
	}
}

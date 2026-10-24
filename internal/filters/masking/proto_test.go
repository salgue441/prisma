package masking

import (
	"bytes"
	"encoding/binary"
	"io"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"prisma/internal/config"
	"prisma/internal/telemetry"
	"prisma/internal/testutil"
)

func protoFilter(t *testing.T, rules ...config.MaskRule) *Filter {
	t.Helper()
	f, err := New("r", &config.Masking{Rules: rules, HashKey: "k", DescriptorSet: testutil.WriteDescriptorSet(t)}, telemetry.NewMetrics())
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestMaskProto(t *testing.T) {
	f := protoFilter(t,
		config.MaskRule{Selector: "$..ssn", Strategy: "redact"},
		config.MaskRule{Selector: "$.cardNumber", Strategy: "partial", KeepLast: 4}, // JSON name
		config.MaskRule{Selector: "$.balance", Strategy: "redact"},                  // non-string: cleared
		config.MaskRule{Selector: "$.addresses[*].street", Strategy: "hash"},
		config.MaskRule{Detector: "email", Strategy: "redact"},
	)
	if len(f.grpcOutputs) != 2 || f.grpcOutputs["/test.Users/List"] == nil {
		t.Fatalf("descriptor methods: %v", f.grpcOutputs)
	}
	md := testutil.Message(t, "User")
	in, _ := proto.Marshal(testutil.SampleUser(t))
	out, err := f.MaskProto(md, in)
	if err != nil {
		t.Fatal(err)
	}
	m := dynamicpb.NewMessage(md)
	if err := proto.Unmarshal(out, m); err != nil {
		t.Fatal(err)
	}
	get := func(name string) string { return m.Get(md.Fields().ByName(protoreflect.Name(name))).String() }
	if get("ssn") != "***" || get("card_number") != "************1111" || get("name") != "Ana" {
		t.Errorf("strings: ssn=%q card=%q name=%q", get("ssn"), get("card_number"), get("name"))
	}
	if m.Has(md.Fields().ByName("balance")) {
		t.Error("numeric field targeted by redact must be cleared")
	}
	street := m.Get(md.Fields().ByName("addresses")).List().Get(0).Message()
	if s := street.Get(street.Descriptor().Fields().ByName("street")).String(); !strings.HasPrefix(s, "hmac:") {
		t.Errorf("nested repeated street = %q", s)
	}
	tags := m.Get(md.Fields().ByName("tags")).List()
	if tags.Get(0).String() != "vip" || strings.Contains(tags.Get(1).String(), "ana@example.com") {
		t.Errorf("detector on repeated strings: %v, %v", tags.Get(0), tags.Get(1))
	}
}

func frame(flag byte, b []byte) []byte {
	out := make([]byte, 5, 5+len(b))
	out[0] = flag
	binary.BigEndian.PutUint32(out[1:], uint32(len(b)))
	return append(out, b...)
}

func TestGRPCMaskerStreamsFrames(t *testing.T) {
	f := protoFilter(t, config.MaskRule{Selector: "$.ssn", Strategy: "redact"})
	md := testutil.Message(t, "User")
	msg, _ := proto.Marshal(testutil.SampleUser(t))
	trailer := []byte("grpc-status: 0\r\n")
	src := bytes.Join([][]byte{frame(0, msg), frame(0, msg), frame(0x80, trailer)}, nil)

	out, err := io.ReadAll(&grpcMasker{f: f, md: md, src: bytes.NewReader(src), maxMsg: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out, []byte("123-45-6789")) {
		t.Error("ssn leaked through the stream")
	}
	if !bytes.HasSuffix(out, frame(0x80, trailer)) {
		t.Error("gRPC-Web trailer frame must pass through unchanged")
	}

	// Compressed messages cannot be inspected: the stream aborts.
	_, err = io.ReadAll(&grpcMasker{f: f, md: md, src: bytes.NewReader(frame(1, msg)), maxMsg: 1 << 20})
	if err == nil {
		t.Error("compressed frame must fail closed")
	}
	// Truncated frames are errors, not a clean end of stream.
	_, err = io.ReadAll(&grpcMasker{f: f, md: md, src: bytes.NewReader(frame(0, msg)[:20]), maxMsg: 1 << 20})
	if err == nil {
		t.Error("truncated frame must be an error")
	}
}

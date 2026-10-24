package gateway_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"prisma/internal/testutil"
)

func TestGRPCResponseMasking(t *testing.T) {
	upAddr := testutil.ServeUsers(t)
	desc := testutil.WriteDescriptorSet(t)
	gwAddr := startH2CGateway(t, `
apiVersion: prisma/v1
listeners:
  - name: public
    address: "127.0.0.1:0"
    protocols: [http1, h2c]
clusters:
  - name: users
    protocol: h2c
    endpoints: [{address: "`+upAddr+`"}]
routes:
  - name: users
    type: grpc
    match: {grpcService: test.Users}
    grpcWeb: true
    masking:
      descriptorSet: "`+strings.ReplaceAll(desc, `\`, `/`)+`"
      rules:
        - {selector: "$.ssn", strategy: redact}
        - {selector: "$.cardNumber", strategy: partial}
    backends: [{cluster: users}]
`)
	conn, err := grpc.NewClient(gwAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	userMD := testutil.Message(t, "User")
	req := dynamicpb.NewMessage(testutil.Message(t, "GetRequest"))

	check := func(label string, m *dynamicpb.Message) {
		t.Helper()
		f := userMD.Fields()
		ssn := m.Get(f.ByName("ssn")).String()
		card := m.Get(f.ByName(protoreflect.Name("card_number"))).String()
		if ssn != "***" || card != "************1111" || m.Get(f.ByName("name")).String() != "Ana" {
			t.Errorf("%s: ssn=%q card=%q", label, ssn, card)
		}
	}

	resp := dynamicpb.NewMessage(userMD)
	if err := conn.Invoke(ctx, "/test.Users/Get", req, resp); err != nil {
		t.Fatalf("unary: %v", err)
	}
	check("unary", resp)

	stream, err := conn.NewStream(ctx, &grpc.StreamDesc{ServerStreams: true}, "/test.Users/List")
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.SendMsg(req); err != nil {
		t.Fatal(err)
	}
	_ = stream.CloseSend()
	n := 0
	for {
		m := dynamicpb.NewMessage(userMD)
		if err := stream.RecvMsg(m); err != nil {
			break
		}
		check("stream", m)
		n++
	}
	if n != 3 {
		t.Errorf("received %d streamed messages, want 3", n)
	}

	// gRPC-Web: masking must run on native frames before translation.
	data, trailers, _ := grpcWebCall(t, "http://"+gwAddr+"/test.Users/Get", false, req)
	web := dynamicpb.NewMessage(userMD)
	if err := proto.Unmarshal(data, web); err != nil {
		t.Fatalf("grpc-web body: %v (trailers %q)", err, trailers)
	}
	check("grpc-web", web)
	if !strings.Contains(trailers, "grpc-status: 0") {
		t.Errorf("grpc-web trailers: %q", trailers)
	}
}

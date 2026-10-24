// Package testutil holds helpers shared by tests across packages.
package testutil

import (
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

func field(name string, num int32, typ descriptorpb.FieldDescriptorProto_Type, repeated bool, typeName string) *descriptorpb.FieldDescriptorProto {
	label := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL
	if repeated {
		label = descriptorpb.FieldDescriptorProto_LABEL_REPEATED
	}
	f := &descriptorpb.FieldDescriptorProto{Name: proto.String(name), Number: proto.Int32(num), Type: typ.Enum(), Label: label.Enum()}
	if typeName != "" {
		f.TypeName = proto.String(typeName)
	}
	return f
}

// UsersFile describes:
//
//	package test;
//	message Address { string street = 1; }
//	message User { string name = 1; string ssn = 2; int64 balance = 3;
//	  repeated string tags = 4; repeated Address addresses = 5; string card_number = 6; }
//	message GetRequest { string id = 1; }
//	service Users { rpc Get(GetRequest) returns (User); rpc List(GetRequest) returns (stream User); }
func UsersFile() *descriptorpb.FileDescriptorProto {
	str := descriptorpb.FieldDescriptorProto_TYPE_STRING
	msg := descriptorpb.FieldDescriptorProto_TYPE_MESSAGE
	return &descriptorpb.FileDescriptorProto{
		Name: proto.String("test/users.proto"), Package: proto.String("test"), Syntax: proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{
			{Name: proto.String("Address"), Field: []*descriptorpb.FieldDescriptorProto{field("street", 1, str, false, "")}},
			{Name: proto.String("User"), Field: []*descriptorpb.FieldDescriptorProto{
				field("name", 1, str, false, ""),
				field("ssn", 2, str, false, ""),
				field("balance", 3, descriptorpb.FieldDescriptorProto_TYPE_INT64, false, ""),
				field("tags", 4, str, true, ""),
				field("addresses", 5, msg, true, ".test.Address"),
				field("card_number", 6, str, false, ""),
			}},
			{Name: proto.String("GetRequest"), Field: []*descriptorpb.FieldDescriptorProto{field("id", 1, str, false, "")}},
		},
		Service: []*descriptorpb.ServiceDescriptorProto{{
			Name: proto.String("Users"),
			Method: []*descriptorpb.MethodDescriptorProto{
				{Name: proto.String("Get"), InputType: proto.String(".test.GetRequest"), OutputType: proto.String(".test.User")},
				{Name: proto.String("List"), InputType: proto.String(".test.GetRequest"), OutputType: proto.String(".test.User"), ServerStreaming: proto.Bool(true)},
			},
		}},
	}
}

// WriteDescriptorSet writes UsersFile as a FileDescriptorSet and returns its path.
func WriteDescriptorSet(t *testing.T) string {
	t.Helper()
	b, err := proto.Marshal(&descriptorpb.FileDescriptorSet{File: []*descriptorpb.FileDescriptorProto{UsersFile()}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "users.binpb")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// Message returns the descriptor of a message in UsersFile.
func Message(t *testing.T, name string) protoreflect.MessageDescriptor {
	t.Helper()
	fd, err := protodesc.NewFile(UsersFile(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return fd.Messages().ByName(protoreflect.Name(name))
}

// SampleUser is a User with every sensitive field populated.
func SampleUser(t *testing.T) *dynamicpb.Message {
	md := Message(t, "User")
	m := dynamicpb.NewMessage(md)
	f := md.Fields()
	m.Set(f.ByName("name"), protoreflect.ValueOfString("Ana"))
	m.Set(f.ByName("ssn"), protoreflect.ValueOfString("123-45-6789"))
	m.Set(f.ByName("balance"), protoreflect.ValueOfInt64(1000))
	tags := m.Mutable(f.ByName("tags")).List()
	tags.Append(protoreflect.ValueOfString("vip"))
	tags.Append(protoreflect.ValueOfString("mail ana@example.com"))
	addrs := m.Mutable(f.ByName("addresses")).List()
	a := addrs.NewElement()
	a.Message().Set(a.Message().Descriptor().Fields().ByName("street"), protoreflect.ValueOfString("1 Main St"))
	addrs.Append(a)
	m.Set(f.ByName("card_number"), protoreflect.ValueOfString("4111111111111111"))
	return m
}

// ServeUsers runs an h2c gRPC server for test.Users without generated code:
// Get returns one SampleUser, List streams three.
func ServeUsers(t *testing.T) string {
	t.Helper()
	user, err := proto.Marshal(SampleUser(t))
	if err != nil {
		t.Fatal(err)
	}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/grpc")
		w.Header().Set("Trailer", "Grpc-Status")
		w.WriteHeader(http.StatusOK)
		n := 1
		if r.URL.Path == "/test.Users/List" {
			n = 3
		}
		for i := 0; i < n; i++ {
			var hdr [5]byte
			binary.BigEndian.PutUint32(hdr[1:], uint32(len(user)))
			_, _ = w.Write(hdr[:])
			_, _ = w.Write(user)
			http.NewResponseController(w).Flush()
		}
		w.Header().Set("Grpc-Status", "0")
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := new(http.Protocols)
	p.SetUnencryptedHTTP2(true)
	srv := &http.Server{Handler: h, Protocols: p}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return ln.Addr().String()
}

// Package demo holds sample services for examples and tests: a
// shop.v1.Orders gRPC service with google.api.http annotations, served
// without generated code.
package demo

import (
	"encoding/binary"
	"io"
	"net/http"
	"path"
	"strconv"

	"google.golang.org/genproto/googleapis/api/annotations"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
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

func httpRule(r *annotations.HttpRule) *descriptorpb.MethodOptions {
	o := &descriptorpb.MethodOptions{}
	proto.SetExtension(o, annotations.E_Http, r)
	return o
}

// ShopFile describes shop.v1.Orders with google.api.http annotations:
//
//	GetOrder    GET   /v1/orders/{id}
//	CreateOrder POST  /v1/shops/{shop}/orders           body: "order"
//	ListOrders  GET   /v1/shops/{shop}/orders           (server streaming)
//	UpdateOrder PATCH /v1/orders/{order.id}             body: "order"
//	            PUT   /v1/orders/{order.id}             body: "*"  (additional binding)
//	CancelOrder POST  /v1/orders/{id}:cancel            response_body: "customer"
//	Ping        (no annotation)
func ShopFile() *descriptorpb.FileDescriptorProto {
	str := descriptorpb.FieldDescriptorProto_TYPE_STRING
	msg := descriptorpb.FieldDescriptorProto_TYPE_MESSAGE
	enum := descriptorpb.FieldDescriptorProto_TYPE_ENUM
	i32 := descriptorpb.FieldDescriptorProto_TYPE_INT32
	boolean := descriptorpb.FieldDescriptorProto_TYPE_BOOL
	m := func(name, in, out string, rule *annotations.HttpRule, stream bool) *descriptorpb.MethodDescriptorProto {
		md := &descriptorpb.MethodDescriptorProto{Name: proto.String(name), InputType: proto.String(in), OutputType: proto.String(out)}
		if stream {
			md.ServerStreaming = proto.Bool(true)
		}
		if rule != nil {
			md.Options = httpRule(rule)
		}
		return md
	}
	return &descriptorpb.FileDescriptorProto{
		Name: proto.String("shop/v1/orders.proto"), Package: proto.String("shop.v1"), Syntax: proto.String("proto3"),
		Dependency: []string{"google/api/annotations.proto"},
		EnumType: []*descriptorpb.EnumDescriptorProto{{
			Name: proto.String("Status"),
			Value: []*descriptorpb.EnumValueDescriptorProto{
				{Name: proto.String("STATUS_UNSPECIFIED"), Number: proto.Int32(0)},
				{Name: proto.String("PENDING"), Number: proto.Int32(1)},
				{Name: proto.String("SHIPPED"), Number: proto.Int32(2)},
			},
		}},
		MessageType: []*descriptorpb.DescriptorProto{
			{Name: proto.String("Customer"), Field: []*descriptorpb.FieldDescriptorProto{
				field("name", 1, str, false, ""), field("email", 2, str, false, ""),
			}},
			{Name: proto.String("Order"), Field: []*descriptorpb.FieldDescriptorProto{
				field("id", 1, str, false, ""),
				field("item", 2, str, false, ""),
				field("qty", 3, i32, false, ""),
				field("status", 4, enum, false, ".shop.v1.Status"),
				field("tags", 5, str, true, ""),
				field("customer", 6, msg, false, ".shop.v1.Customer"),
			}},
			{Name: proto.String("GetOrderRequest"), Field: []*descriptorpb.FieldDescriptorProto{
				field("id", 1, str, false, ""), field("include_items", 2, boolean, false, ""),
			}},
			{Name: proto.String("CreateOrderRequest"), Field: []*descriptorpb.FieldDescriptorProto{
				field("shop", 1, str, false, ""), field("order", 2, msg, false, ".shop.v1.Order"),
			}},
			{Name: proto.String("ListOrdersRequest"), Field: []*descriptorpb.FieldDescriptorProto{
				field("shop", 1, str, false, ""),
				field("page_size", 2, i32, false, ""),
				field("status", 3, enum, true, ".shop.v1.Status"),
			}},
			{Name: proto.String("UpdateOrderRequest"), Field: []*descriptorpb.FieldDescriptorProto{
				field("order", 1, msg, false, ".shop.v1.Order"),
			}},
		},
		Service: []*descriptorpb.ServiceDescriptorProto{{
			Name: proto.String("Orders"),
			Method: []*descriptorpb.MethodDescriptorProto{
				m("GetOrder", ".shop.v1.GetOrderRequest", ".shop.v1.Order",
					&annotations.HttpRule{Pattern: &annotations.HttpRule_Get{Get: "/v1/orders/{id}"}}, false),
				m("CreateOrder", ".shop.v1.CreateOrderRequest", ".shop.v1.Order",
					&annotations.HttpRule{Pattern: &annotations.HttpRule_Post{Post: "/v1/shops/{shop}/orders"}, Body: "order"}, false),
				m("ListOrders", ".shop.v1.ListOrdersRequest", ".shop.v1.Order",
					&annotations.HttpRule{Pattern: &annotations.HttpRule_Get{Get: "/v1/shops/{shop}/orders"}}, true),
				m("UpdateOrder", ".shop.v1.UpdateOrderRequest", ".shop.v1.Order",
					&annotations.HttpRule{Pattern: &annotations.HttpRule_Patch{Patch: "/v1/orders/{order.id}"}, Body: "order",
						AdditionalBindings: []*annotations.HttpRule{{Pattern: &annotations.HttpRule_Put{Put: "/v1/orders/{order.id}"}, Body: "*"}}}, false),
				m("CancelOrder", ".shop.v1.GetOrderRequest", ".shop.v1.Order",
					&annotations.HttpRule{Pattern: &annotations.HttpRule_Post{Post: "/v1/orders/{id}:cancel"}, ResponseBody: "customer"}, false),
				m("Ping", ".shop.v1.GetOrderRequest", ".shop.v1.GetOrderRequest", nil, false),
			},
		}},
	}
}

// WriteShopDescriptorSet writes ShopFile and its imports (as protoc
// --include_imports would) and returns the path.
// ShopDescriptorSet is ShopFile and its imports as a FileDescriptorSet, as
// protoc --include_imports --descriptor_set_out would write it.
func ShopDescriptorSet() ([]byte, error) {
	set := &descriptorpb.FileDescriptorSet{}
	for _, fd := range []protoreflect.FileDescriptor{
		descriptorpb.File_google_protobuf_descriptor_proto,
		annotations.File_google_api_http_proto,
		annotations.File_google_api_annotations_proto,
	} {
		set.File = append(set.File, protodesc.ToFileDescriptorProto(fd))
	}
	set.File = append(set.File, ShopFile())
	return proto.Marshal(set)
}

// ShopHandler serves shop.v1.Orders over h2c (wrap it in an HTTP/2
// server). It reports what it received: GetOrder builds an order from the
// request (id "404" is NOT_FOUND), CreateOrder and UpdateOrder echo the
// order, ListOrders streams page_size orders (shop "broken" fails after
// one), and Ping echoes its input.
func ShopHandler() (http.Handler, error) {
	fd, err := protodesc.NewFile(ShopFile(), protoregistry.GlobalFiles)
	if err != nil {
		return nil, err
	}
	svc := fd.Services().Get(0)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		md := svc.Methods().ByName(protoreflect.Name(path.Base(r.URL.Path)))
		w.Header().Set("Content-Type", "application/grpc")
		if md == nil || len(raw) < 5 {
			w.Header().Set("Grpc-Status", "12")
			w.WriteHeader(http.StatusOK)
			return
		}
		in := dynamicpb.NewMessage(md.Input())
		if err := proto.Unmarshal(raw[5:], in); err != nil {
			w.Header().Set("Grpc-Status", "3")
			w.WriteHeader(http.StatusOK)
			return
		}
		get := func(m protoreflect.Message, name string) protoreflect.Value {
			return m.Get(m.Descriptor().Fields().ByName(protoreflect.Name(name)))
		}
		order := func(id string) *dynamicpb.Message {
			o := dynamicpb.NewMessage(fd.Messages().ByName("Order"))
			f := o.Descriptor().Fields()
			o.Set(f.ByName("id"), protoreflect.ValueOfString(id))
			o.Set(f.ByName("item"), protoreflect.ValueOfString("book"))
			o.Set(f.ByName("qty"), protoreflect.ValueOfInt32(1))
			o.Set(f.ByName("status"), protoreflect.ValueOfEnum(2))
			c := o.Mutable(f.ByName("customer")).Message()
			c.Set(c.Descriptor().Fields().ByName("name"), protoreflect.ValueOfString("Ana"))
			c.Set(c.Descriptor().Fields().ByName("email"), protoreflect.ValueOfString("ana@example.com"))
			return o
		}
		var out []proto.Message
		status, message := "0", ""
		switch md.Name() {
		case "GetOrder", "CancelOrder":
			id := get(in, "id").String()
			if id == "404" {
				w.Header().Set("Grpc-Status", "5")
				w.Header().Set("Grpc-Message", "order%20404%20not%20found")
				w.WriteHeader(http.StatusOK)
				return
			}
			o := order(id)
			if get(in, "include_items").Bool() {
				o.Set(o.Descriptor().Fields().ByName("qty"), protoreflect.ValueOfInt32(2))
			}
			out = append(out, o)
		case "CreateOrder":
			o := get(in, "order").Message()
			o.Set(o.Descriptor().Fields().ByName("id"), protoreflect.ValueOfString("new-"+get(in, "shop").String()))
			out = append(out, o.Interface())
		case "UpdateOrder":
			out = append(out, get(in, "order").Message().Interface())
		case "ListOrders":
			n := int(get(in, "page_size").Int())
			if n == 0 {
				n = 2
			}
			statuses := get(in, "status").List()
			for i := range n {
				o := order(get(in, "shop").String() + "-" + strconv.Itoa(i))
				if statuses.Len() > 0 {
					o.Set(o.Descriptor().Fields().ByName("status"), statuses.Get(0))
				}
				out = append(out, o)
			}
			if get(in, "shop").String() == "broken" {
				out, status, message = out[:1], "14", "backend%20down"
			}
		default:
			out = append(out, in)
		}
		w.Header().Set("Trailer", "Grpc-Status, Grpc-Message")
		w.WriteHeader(http.StatusOK)
		for _, m := range out {
			b, _ := proto.Marshal(m)
			var hdr [5]byte
			binary.BigEndian.PutUint32(hdr[1:], uint32(len(b)))
			_, _ = w.Write(hdr[:])
			_, _ = w.Write(b)
			http.NewResponseController(w).Flush()
		}
		w.Header().Set("Grpc-Status", status)
		if message != "" {
			w.Header().Set("Grpc-Message", message)
		}
	}), nil
}

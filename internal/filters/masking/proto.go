package masking

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// loadDescriptors maps every "/pkg.Service/Method" in the descriptor set to
// its response message type.
func loadDescriptors(path string) (map[string]protoreflect.MessageDescriptor, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("masking descriptorSet: %w", err)
	}
	var set descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(raw, &set); err != nil {
		return nil, fmt.Errorf("masking descriptorSet %s: %w", path, err)
	}
	files, err := protodesc.NewFiles(&set)
	if err != nil {
		return nil, fmt.Errorf("masking descriptorSet %s: %w (was it built with --include_imports?)", path, err)
	}
	out := map[string]protoreflect.MessageDescriptor{}
	files.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		svcs := fd.Services()
		for i := 0; i < svcs.Len(); i++ {
			svc := svcs.Get(i)
			methods := svc.Methods()
			for j := 0; j < methods.Len(); j++ {
				m := methods.Get(j)
				out["/"+string(svc.FullName())+"/"+string(m.Name())] = m.Output()
			}
		}
		return true
	})
	return out, nil
}

// MaskProto masks one serialized message of type md.
func (f *Filter) MaskProto(md protoreflect.MessageDescriptor, b []byte) ([]byte, error) {
	m := dynamicpb.NewMessage(md)
	if err := proto.Unmarshal(b, m); err != nil {
		return nil, fmt.Errorf("decode %s: %w", md.FullName(), err)
	}
	for _, r := range f.rules {
		f.walkProto(m, r.segs, r)
	}
	if len(f.detectors) > 0 {
		f.scanProto(m)
	}
	return proto.Marshal(m)
}

func fieldByName(md protoreflect.MessageDescriptor, name string) protoreflect.FieldDescriptor {
	fields := md.Fields()
	if fd := fields.ByName(protoreflect.Name(name)); fd != nil {
		return fd
	}
	return fields.ByJSONName(name)
}

// setFields snapshots populated fields so the message can be mutated after.
func setFields(m protoreflect.Message) []protoreflect.FieldDescriptor {
	var fds []protoreflect.FieldDescriptor
	m.Range(func(fd protoreflect.FieldDescriptor, _ protoreflect.Value) bool {
		fds = append(fds, fd)
		return true
	})
	return fds
}

// walkProto applies the selector to a message with JSONPath-like semantics
// over field names (proto or JSON names) and repeated fields.
func (f *Filter) walkProto(m protoreflect.Message, segs []seg, r rule) {
	s, rest := segs[0], segs[1:]
	switch s.kind {
	case segChild:
		if fd := fieldByName(m.Descriptor(), s.name); fd != nil && m.Has(fd) {
			f.applyField(m, fd, rest, r)
		}
	case segWildcard:
		for _, fd := range setFields(m) {
			f.applyField(m, fd, rest, r)
		}
	case segRecursive:
		for _, fd := range setFields(m) {
			if string(fd.Name()) == s.name || fd.JSONName() == s.name {
				f.applyField(m, fd, rest, r)
				if len(rest) == 0 || !m.Has(fd) {
					continue
				}
			}
			forEachChild(m, fd, func(child protoreflect.Message) { f.walkProto(child, segs, r) })
		}
	}
}

func forEachChild(m protoreflect.Message, fd protoreflect.FieldDescriptor, fn func(protoreflect.Message)) {
	switch {
	case fd.IsList() && fd.Message() != nil:
		list := m.Mutable(fd).List()
		for i := 0; i < list.Len(); i++ {
			fn(list.Get(i).Message())
		}
	case fd.IsMap() && fd.MapValue().Message() != nil:
		m.Mutable(fd).Map().Range(func(_ protoreflect.MapKey, v protoreflect.Value) bool {
			fn(v.Message())
			return true
		})
	case !fd.IsList() && !fd.IsMap() && fd.Message() != nil:
		fn(m.Mutable(fd).Message())
	}
}

func (f *Filter) applyField(m protoreflect.Message, fd protoreflect.FieldDescriptor, rest []seg, r rule) {
	if len(rest) == 0 {
		f.mutate(m, fd, r)
		return
	}
	if fd.IsList() && (rest[0].kind == segWildcard || rest[0].kind == segIndex) {
		list := m.Mutable(fd).List()
		for i := 0; i < list.Len(); i++ {
			if rest[0].kind == segIndex && rest[0].idx != i {
				continue
			}
			switch {
			case len(rest) > 1 && fd.Message() != nil:
				f.walkProto(list.Get(i).Message(), rest[1:], r)
			case len(rest) == 1 && fd.Kind() == protoreflect.StringKind && r.strategy != "drop":
				list.Set(i, protoreflect.ValueOfString(f.apply(r.strategy, r.keepLast, list.Get(i).String())))
			case len(rest) == 1 && fd.Message() != nil:
				clearMessage(list.Get(i).Message())
			}
		}
		return
	}
	forEachChild(m, fd, func(child protoreflect.Message) { f.walkProto(child, rest, r) })
}

func clearMessage(m protoreflect.Message) {
	for _, fd := range setFields(m) {
		m.Clear(fd)
	}
}

// mutate masks one field. Strings carry the masked value; numbers, bools,
// enums, messages and maps cannot, so they are cleared.
func (f *Filter) mutate(m protoreflect.Message, fd protoreflect.FieldDescriptor, r rule) {
	switch {
	case r.strategy == "drop":
		m.Clear(fd)
	case fd.IsList() && fd.Kind() == protoreflect.StringKind:
		list := m.Mutable(fd).List()
		for i := 0; i < list.Len(); i++ {
			list.Set(i, protoreflect.ValueOfString(f.apply(r.strategy, r.keepLast, list.Get(i).String())))
		}
	case fd.IsList() || fd.IsMap() || fd.Message() != nil:
		m.Clear(fd)
	case fd.Kind() == protoreflect.StringKind:
		m.Set(fd, protoreflect.ValueOfString(f.apply(r.strategy, r.keepLast, m.Get(fd).String())))
	case fd.Kind() == protoreflect.BytesKind:
		m.Set(fd, protoreflect.ValueOfBytes([]byte(f.apply(r.strategy, r.keepLast, string(m.Get(fd).Bytes())))))
	default:
		m.Clear(fd)
	}
}

// scanProto runs detectors over every string in the message tree.
func (f *Filter) scanProto(m protoreflect.Message) {
	for _, fd := range setFields(m) {
		switch {
		case fd.IsList() && fd.Kind() == protoreflect.StringKind:
			list := m.Mutable(fd).List()
			for i := 0; i < list.Len(); i++ {
				list.Set(i, protoreflect.ValueOfString(f.scan(list.Get(i).String()).(string)))
			}
		case fd.IsMap() && fd.MapValue().Kind() == protoreflect.StringKind:
			mp := m.Mutable(fd).Map()
			mp.Range(func(k protoreflect.MapKey, v protoreflect.Value) bool {
				mp.Set(k, protoreflect.ValueOfString(f.scan(v.String()).(string)))
				return true
			})
		case !fd.IsList() && !fd.IsMap() && fd.Kind() == protoreflect.StringKind:
			m.Set(fd, protoreflect.ValueOfString(f.scan(m.Get(fd).String()).(string)))
		default:
			forEachChild(m, fd, f.scanProto)
		}
	}
}

// grpcMasker re-frames a gRPC response stream, masking each message.
// gRPC-Web trailer frames (flag 0x80) pass through untouched.
type grpcMasker struct {
	f      *Filter
	md     protoreflect.MessageDescriptor
	src    io.Reader
	maxMsg int64
	out    bytes.Buffer
	err    error
}

var errCompressed = errors.New("compressed gRPC message cannot be masked")

func (g *grpcMasker) Read(p []byte) (int, error) {
	for g.out.Len() == 0 && g.err == nil {
		g.err = g.next()
	}
	if g.out.Len() > 0 {
		return g.out.Read(p)
	}
	return 0, g.err
}

func (g *grpcMasker) next() error {
	var hdr [5]byte
	if _, err := io.ReadFull(g.src, hdr[:]); err != nil {
		if err == io.ErrUnexpectedEOF {
			return fmt.Errorf("truncated gRPC frame: %w", err)
		}
		return err // io.EOF ends the stream cleanly, letting trailers flow
	}
	n := int64(binary.BigEndian.Uint32(hdr[1:]))
	if n > g.maxMsg {
		return g.failure(fmt.Errorf("gRPC message of %d bytes exceeds maxBufferBytes", n), hdr, nil)
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(g.src, payload); err != nil {
		return fmt.Errorf("truncated gRPC message: %w", err)
	}
	switch {
	case hdr[0]&0x80 != 0:
		g.write(hdr[0], payload)
		return nil
	case hdr[0]&0x01 != 0:
		return g.failure(errCompressed, hdr, payload)
	}
	masked, err := g.f.MaskProto(g.md, payload)
	if err != nil {
		return g.failure(err, hdr, payload)
	}
	g.write(0, masked)
	return nil
}

// failure aborts the stream (fail closed: the client sees a reset stream and
// no data leaks) or passes the frame through unmasked (fail open).
func (g *grpcMasker) failure(err error, hdr [5]byte, payload []byte) error {
	if !g.f.failOpen || payload == nil {
		g.f.metrics.MaskingFailures.WithLabelValues(g.f.route, "blocked").Inc()
		slog.Warn("gRPC masking failed; aborting stream", "route", g.f.route, "err", err)
		return err
	}
	g.f.metrics.MaskingFailures.WithLabelValues(g.f.route, "passed").Inc()
	g.write(hdr[0], payload)
	return nil
}

func (g *grpcMasker) write(flag byte, payload []byte) {
	var hdr [5]byte
	hdr[0] = flag
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)))
	g.out.Write(hdr[:])
	g.out.Write(payload)
}

package transcode

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

type jsonRaw = json.RawMessage

var (
	jsonMarshal   = json.Marshal
	jsonUnmarshal = json.Unmarshal
	errNoField    = errors.New("unknown field")
)

// lookup finds a field by its proto name or its JSON name.
func lookup(md protoreflect.MessageDescriptor, name string) protoreflect.FieldDescriptor {
	fs := md.Fields()
	if fd := fs.ByName(protoreflect.Name(name)); fd != nil {
		return fd
	}
	return fs.ByJSONName(name)
}

// fieldPath resolves a dotted path through singular message fields.
func fieldPath(md protoreflect.MessageDescriptor, path []string) ([]protoreflect.FieldDescriptor, error) {
	var out []protoreflect.FieldDescriptor
	for i, name := range path {
		fd := lookup(md, name)
		if fd == nil {
			return nil, fmt.Errorf("%w %q in %s", errNoField, name, md.FullName())
		}
		out = append(out, fd)
		if i < len(path)-1 {
			if fd.Message() == nil || fd.IsList() || fd.IsMap() {
				return nil, fmt.Errorf("%s is not a message field", name)
			}
			md = fd.Message()
		}
	}
	return out, nil
}

// setField assigns string values (from the path or query) to the field at
// path, creating intermediate messages. Repeated fields take every value.
func setField(msg *dynamicpb.Message, path []string, vals []string) error {
	fds, err := fieldPath(msg.Descriptor(), path)
	if err != nil {
		return err
	}
	m := protoreflect.Message(msg)
	for _, fd := range fds[:len(fds)-1] {
		m = m.Mutable(fd).Message()
	}
	fd := fds[len(fds)-1]
	switch {
	case fd.IsMap():
		return fmt.Errorf("map field %s cannot be set from a parameter", fd.Name())
	case fd.IsList():
		list := m.Mutable(fd).List()
		for _, v := range vals {
			pv, err := parseValue(fd, v, list.NewElement)
			if err != nil {
				return err
			}
			list.Append(pv)
		}
	default:
		if len(vals) == 0 {
			return nil
		}
		pv, err := parseValue(fd, vals[len(vals)-1], func() protoreflect.Value { return m.NewField(fd) })
		if err != nil {
			return err
		}
		m.Set(fd, pv)
	}
	return nil
}

// parseValue converts one parameter value for a field. Message fields
// accept the JSON form of well-known types (wrappers, Timestamp,
// Duration, FieldMask) given as a plain string.
func parseValue(fd protoreflect.FieldDescriptor, s string, newMsg func() protoreflect.Value) (protoreflect.Value, error) {
	bad := func(err error) (protoreflect.Value, error) {
		return protoreflect.Value{}, fmt.Errorf("invalid value for %s: %v", fd.Name(), err)
	}
	switch fd.Kind() {
	case protoreflect.StringKind:
		return protoreflect.ValueOfString(s), nil
	case protoreflect.BytesKind:
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			if b, err = base64.URLEncoding.DecodeString(s); err != nil {
				return bad(err)
			}
		}
		return protoreflect.ValueOfBytes(b), nil
	case protoreflect.BoolKind:
		v, err := strconv.ParseBool(s)
		if err != nil {
			return bad(err)
		}
		return protoreflect.ValueOfBool(v), nil
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		v, err := strconv.ParseInt(s, 10, 32)
		if err != nil {
			return bad(err)
		}
		return protoreflect.ValueOfInt32(int32(v)), nil
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		v, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return bad(err)
		}
		return protoreflect.ValueOfInt64(v), nil
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		v, err := strconv.ParseUint(s, 10, 32)
		if err != nil {
			return bad(err)
		}
		return protoreflect.ValueOfUint32(uint32(v)), nil
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		v, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			return bad(err)
		}
		return protoreflect.ValueOfUint64(v), nil
	case protoreflect.FloatKind:
		v, err := strconv.ParseFloat(s, 32)
		if err != nil {
			return bad(err)
		}
		return protoreflect.ValueOfFloat32(float32(v)), nil
	case protoreflect.DoubleKind:
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return bad(err)
		}
		return protoreflect.ValueOfFloat64(v), nil
	case protoreflect.EnumKind:
		ed := fd.Enum()
		if ev := ed.Values().ByName(protoreflect.Name(s)); ev != nil {
			return protoreflect.ValueOfEnum(ev.Number()), nil
		}
		if n, err := strconv.ParseInt(s, 10, 32); err == nil && ed.Values().ByNumber(protoreflect.EnumNumber(n)) != nil {
			return protoreflect.ValueOfEnum(protoreflect.EnumNumber(n)), nil
		}
		return bad(fmt.Errorf("%q is not a value of %s", s, ed.FullName()))
	case protoreflect.MessageKind, protoreflect.GroupKind:
		if !strings.HasPrefix(string(fd.Message().FullName()), "google.protobuf.") {
			return bad(errors.New("message fields cannot be set from a parameter"))
		}
		v := newMsg()
		quoted, _ := json.Marshal(s)
		err := protojson.Unmarshal(quoted, v.Message().Interface())
		if err != nil {
			// Numeric and boolean wrappers take unquoted JSON.
			if err2 := protojson.Unmarshal([]byte(s), v.Message().Interface()); err2 != nil {
				return bad(err)
			}
		}
		return v, nil
	}
	return bad(fmt.Errorf("unsupported kind %s", fd.Kind()))
}

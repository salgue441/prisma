// Package wasm hosts Proxy-Wasm (ABI 0.2.x) plugins on wazero, the same ABI
// Envoy and Istio implement, so plugins built with the official SDKs (Go,
// Rust, C++, AssemblyScript) run unmodified. It implements the HTTP-filter
// subset: headers, bodies, local responses, HTTP callouts, properties,
// shared data, metrics, logging and timers.
package wasm

import (
	"encoding/binary"
	"errors"
)

type status = uint32

const (
	statusOK              status = 0
	statusNotFound        status = 1
	statusBadArgument     status = 2
	statusEmpty           status = 7
	statusCasMismatch     status = 8
	statusInternalFailure status = 10
	statusUnimplemented   status = 12
)

const (
	bufferRequestBody      = 0
	bufferResponseBody     = 1
	bufferCallResponseBody = 4
	bufferVMConfiguration  = 6
	bufferPluginConfig     = 7
)

const (
	mapRequestHeaders      = 0
	mapRequestTrailers     = 1
	mapResponseHeaders     = 2
	mapResponseTrailers    = 3
	mapCallResponseHeaders = 6
	mapCallResponseTrailer = 7
)

const (
	actionContinue = 0
	actionPause    = 1
)

// serializeMap encodes pairs in the Proxy-Wasm layout: count, then
// (keyLen, valueLen) for each pair, then NUL-terminated keys and values.
func serializeMap(pairs [][2]string) []byte {
	size := 4
	for _, p := range pairs {
		size += 8 + len(p[0]) + len(p[1]) + 2
	}
	out := make([]byte, size)
	binary.LittleEndian.PutUint32(out, uint32(len(pairs)))
	off := 4
	for _, p := range pairs {
		binary.LittleEndian.PutUint32(out[off:], uint32(len(p[0])))
		binary.LittleEndian.PutUint32(out[off+4:], uint32(len(p[1])))
		off += 8
	}
	for _, p := range pairs {
		off += copy(out[off:], p[0]) + 1
		off += copy(out[off:], p[1]) + 1
	}
	return out
}

var errBadMap = errors.New("malformed header map")

func deserializeMap(b []byte) ([][2]string, error) {
	if len(b) < 4 {
		if len(b) == 0 {
			return nil, nil
		}
		return nil, errBadMap
	}
	n := int(binary.LittleEndian.Uint32(b))
	if n > len(b)/8 {
		return nil, errBadMap
	}
	sizes := 4
	data := 4 + 8*n
	out := make([][2]string, 0, n)
	for i := 0; i < n; i++ {
		kl := int(binary.LittleEndian.Uint32(b[sizes:]))
		vl := int(binary.LittleEndian.Uint32(b[sizes+4:]))
		sizes += 8
		if data+kl+1+vl+1 > len(b) {
			return nil, errBadMap
		}
		k := string(b[data : data+kl])
		data += kl + 1
		v := string(b[data : data+vl])
		data += vl + 1
		out = append(out, [2]string{k, v})
	}
	return out, nil
}

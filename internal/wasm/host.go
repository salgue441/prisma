package wasm

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/tetratelabs/wazero/api"
)

func instFrom(ctx context.Context) *instance {
	inst, _ := ctx.Value(instKey{}).(*instance)
	return inst
}

// registerHost exports the Proxy-Wasm host functions under module "env".
func (p *Plugin) registerHost(ctx context.Context) error {
	b := p.runtime.NewHostModuleBuilder("env")
	fn := func(name string, f any) { b.NewFunctionBuilder().WithFunc(f).Export(name) }

	fn("proxy_log", func(ctx context.Context, m api.Module, level, ptr, size uint32) uint32 {
		msg, ok := readString(m, ptr, size)
		if !ok {
			return statusBadArgument
		}
		lvl := slog.LevelInfo
		switch {
		case level <= 1:
			lvl = slog.LevelDebug
		case level == 3:
			lvl = slog.LevelWarn
		case level >= 4:
			lvl = slog.LevelError
		}
		p.log.Log(ctx, lvl, msg)
		return statusOK
	})
	fn("proxy_get_log_level", func(_ context.Context, m api.Module, retPtr uint32) uint32 {
		m.Memory().WriteUint32Le(retPtr, 1) // debug: let the host's logger filter
		return statusOK
	})
	fn("proxy_get_current_time_nanoseconds", func(_ context.Context, m api.Module, retPtr uint32) uint32 {
		m.Memory().WriteUint64Le(retPtr, uint64(time.Now().UnixNano()))
		return statusOK
	})
	fn("proxy_set_tick_period_milliseconds", func(ctx context.Context, period uint32) uint32 {
		inst := instFrom(ctx)
		select {
		case inst.tick <- time.Duration(period) * time.Millisecond:
		default:
		}
		return statusOK
	})

	// Buffers.
	fn("proxy_get_buffer_bytes", func(ctx context.Context, m api.Module, bt, start, maxSize, ptrPtr, sizePtr uint32) uint32 {
		inst := instFrom(ctx)
		var data []byte
		switch bt {
		case bufferVMConfiguration:
			data = p.cfg.VMConfig
		case bufferPluginConfig:
			data = p.cfg.PluginConfig
		default:
			if inst.cur == nil {
				return statusNotFound
			}
			buf, ok := inst.cur.buffer(bt)
			if !ok {
				return statusNotFound
			}
			data = *buf
		}
		if int(start) > len(data) {
			return statusBadArgument
		}
		end := min(len(data), int(start)+int(maxSize))
		if int(start)+int(maxSize) < 0 { // overflow from MaxInt32 sizes
			end = len(data)
		}
		return inst.returnBytes(ctx, m, data[start:end], ptrPtr, sizePtr)
	})
	fn("proxy_set_buffer_bytes", func(ctx context.Context, m api.Module, bt, start, size, dataPtr, dataSize uint32) uint32 {
		inst := instFrom(ctx)
		if inst.cur == nil {
			return statusNotFound
		}
		buf, ok := inst.cur.buffer(bt)
		if !ok {
			return statusNotFound
		}
		data, ok := read(m, dataPtr, dataSize)
		if !ok {
			return statusBadArgument
		}
		return setBuffer(buf, start, size, data)
	})

	// Header maps.
	fn("proxy_get_header_map_pairs", func(ctx context.Context, m api.Module, mt, ptrPtr, sizePtr uint32) uint32 {
		inst := instFrom(ctx)
		if inst.cur == nil {
			return statusNotFound
		}
		pairs, ok := inst.cur.pairs(mt)
		if !ok {
			return statusNotFound
		}
		return inst.returnBytes(ctx, m, serializeMap(pairs), ptrPtr, sizePtr)
	})
	fn("proxy_set_header_map_pairs", func(ctx context.Context, m api.Module, mt, ptr, size uint32) uint32 {
		inst := instFrom(ctx)
		raw, ok := read(m, ptr, size)
		if !ok || inst.cur == nil {
			return statusBadArgument
		}
		pairs, err := deserializeMap(raw)
		if err != nil {
			return statusBadArgument
		}
		return inst.cur.replaceAll(mt, pairs)
	})
	fn("proxy_get_header_map_value", func(ctx context.Context, m api.Module, mt, keyPtr, keySize, ptrPtr, sizePtr uint32) uint32 {
		inst := instFrom(ctx)
		key, ok := readString(m, keyPtr, keySize)
		if !ok || inst.cur == nil {
			return statusBadArgument
		}
		pairs, ok := inst.cur.pairs(mt)
		if !ok {
			return statusNotFound
		}
		key = strings.ToLower(key)
		var vals []string
		for _, kv := range pairs {
			if kv[0] == key {
				vals = append(vals, kv[1])
			}
		}
		if len(vals) == 0 {
			return statusNotFound
		}
		return inst.returnBytes(ctx, m, []byte(strings.Join(vals, ",")), ptrPtr, sizePtr)
	})
	headerOp := func(op string) any {
		return func(ctx context.Context, m api.Module, mt, keyPtr, keySize, valPtr, valSize uint32) uint32 {
			inst := instFrom(ctx)
			key, ok1 := readString(m, keyPtr, keySize)
			val, ok2 := readString(m, valPtr, valSize)
			if !ok1 || !ok2 || inst.cur == nil {
				return statusBadArgument
			}
			return inst.cur.setHeader(mt, op, key, val)
		}
	}
	fn("proxy_add_header_map_value", headerOp("add"))
	fn("proxy_replace_header_map_value", headerOp("replace"))
	fn("proxy_remove_header_map_value", func(ctx context.Context, m api.Module, mt, keyPtr, keySize uint32) uint32 {
		inst := instFrom(ctx)
		key, ok := readString(m, keyPtr, keySize)
		if !ok || inst.cur == nil {
			return statusBadArgument
		}
		return inst.cur.setHeader(mt, "remove", key, "")
	})

	// Properties.
	fn("proxy_get_property", func(ctx context.Context, m api.Module, pathPtr, pathSize, ptrPtr, sizePtr uint32) uint32 {
		inst := instFrom(ctx)
		raw, ok := read(m, pathPtr, pathSize)
		if !ok {
			return statusBadArgument
		}
		path := strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
		var val []byte
		switch strings.Join(path, ".") {
		case "plugin_name":
			val, ok = []byte(p.cfg.Name), true
		case "plugin_root_id", "plugin_vm_id":
			val, ok = nil, true
		default:
			if inst.cur == nil || inst.cur.props == nil {
				return statusNotFound
			}
			val, ok = inst.cur.props(path)
		}
		if !ok {
			return statusNotFound
		}
		return inst.returnBytes(ctx, m, val, ptrPtr, sizePtr)
	})
	fn("proxy_set_property", func(context.Context, uint32, uint32, uint32, uint32) uint32 { return statusUnimplemented })

	// Stream control.
	fn("proxy_send_local_response", func(ctx context.Context, m api.Module, code, _, _, bodyPtr, bodySize, hdrPtr, hdrSize uint32, grpcStatus int32) uint32 {
		inst := instFrom(ctx)
		if inst.cur == nil {
			return statusNotFound
		}
		body, ok1 := read(m, bodyPtr, bodySize)
		raw, ok2 := read(m, hdrPtr, hdrSize)
		pairs, err := deserializeMap(raw)
		if !ok1 || !ok2 || err != nil {
			return statusBadArgument
		}
		h := http.Header{}
		for _, kv := range pairs {
			h.Add(kv[0], kv[1])
		}
		inst.cur.local = &LocalResponse{Status: int(code), Header: h, Body: body, GRPCStatus: grpcStatus}
		return statusOK
	})
	resume := func(ctx context.Context, _ uint32) uint32 {
		if inst := instFrom(ctx); inst.cur != nil {
			inst.cur.resumed = true
		}
		return statusOK
	}
	fn("proxy_continue_stream", resume)
	fn("proxy_continue_request", func(ctx context.Context) uint32 { return resume(ctx, 0) })
	fn("proxy_continue_response", func(ctx context.Context) uint32 { return resume(ctx, 1) })
	fn("proxy_close_stream", func(ctx context.Context, _ uint32) uint32 {
		if inst := instFrom(ctx); inst.cur != nil && inst.cur.local == nil {
			inst.cur.local = &LocalResponse{Status: http.StatusBadGateway, Header: http.Header{}}
		}
		return statusOK
	})
	fn("proxy_set_effective_context", func(context.Context, uint32) uint32 { return statusOK })
	fn("proxy_done", func(context.Context) uint32 { return statusOK })

	// HTTP callouts.
	fn("proxy_http_call", func(ctx context.Context, m api.Module, upPtr, upSize, hdrPtr, hdrSize, bodyPtr, bodySize, _, _, timeoutMs, tokenPtr uint32) uint32 {
		inst := instFrom(ctx)
		if inst.cur == nil {
			return statusUnimplemented // callouts from root contexts (on_tick) are not supported
		}
		cluster, ok1 := readString(m, upPtr, upSize)
		raw, ok2 := read(m, hdrPtr, hdrSize)
		body, ok3 := read(m, bodyPtr, bodySize)
		pairs, err := deserializeMap(raw)
		if !ok1 || !ok2 || !ok3 || err != nil {
			return statusBadArgument
		}
		req, err := http.NewRequest(http.MethodGet, "http://"+cluster+"/", bytes.NewReader(body))
		if err != nil {
			return statusBadArgument
		}
		req.ContentLength = int64(len(body))
		for _, kv := range pairs {
			switch kv[0] {
			case ":method":
				req.Method = kv[1]
			case ":path":
				u, err := req.URL.Parse(kv[1])
				if err != nil {
					return statusBadArgument
				}
				req.URL.Path, req.URL.RawQuery = u.Path, u.RawQuery
			case ":authority":
				req.Host = kv[1]
			case ":scheme":
			default:
				req.Header.Add(kv[0], kv[1])
			}
		}
		timeout := time.Duration(timeoutMs) * time.Millisecond
		if timeout <= 0 {
			timeout = 5 * time.Second
		}
		token := inst.nextID
		inst.nextID++
		inst.cur.pending = &callout{token: token, cluster: cluster, req: req, timeout: timeout}
		m.Memory().WriteUint32Le(tokenPtr, token)
		return statusOK
	})

	// Shared data.
	fn("proxy_get_shared_data", func(ctx context.Context, m api.Module, keyPtr, keySize, ptrPtr, sizePtr, casPtr uint32) uint32 {
		key, ok := readString(m, keyPtr, keySize)
		if !ok {
			return statusBadArgument
		}
		e, found := p.shared.get(key)
		if !found {
			return statusNotFound
		}
		if st := instFrom(ctx).returnBytes(ctx, m, e.value, ptrPtr, sizePtr); st != statusOK {
			return st
		}
		m.Memory().WriteUint32Le(casPtr, e.cas)
		return statusOK
	})
	fn("proxy_set_shared_data", func(_ context.Context, m api.Module, keyPtr, keySize, valPtr, valSize, cas uint32) uint32 {
		key, ok1 := readString(m, keyPtr, keySize)
		val, ok2 := read(m, valPtr, valSize)
		if !ok1 || !ok2 {
			return statusBadArgument
		}
		return p.shared.set(key, val, cas)
	})
	unimplemented := func(names ...string) {
		for _, n := range names {
			switch n {
			case "proxy_register_shared_queue":
				fn(n, func(context.Context, uint32, uint32, uint32) uint32 { return statusUnimplemented })
			case "proxy_resolve_shared_queue":
				fn(n, func(context.Context, uint32, uint32, uint32, uint32, uint32) uint32 { return statusUnimplemented })
			case "proxy_dequeue_shared_queue":
				fn(n, func(context.Context, uint32, uint32, uint32) uint32 { return statusUnimplemented })
			case "proxy_enqueue_shared_queue":
				fn(n, func(context.Context, uint32, uint32, uint32) uint32 { return statusUnimplemented })
			case "proxy_call_foreign_function":
				fn(n, func(context.Context, uint32, uint32, uint32, uint32, uint32, uint32) uint32 {
					return statusUnimplemented
				})
			}
		}
	}
	unimplemented("proxy_call_foreign_function")

	// Shared queues. Queues are plugin-wide; vm_id may be empty or the
	// plugin's own name (queues of other plugins are not visible).
	fn("proxy_register_shared_queue", func(ctx context.Context, m api.Module, namePtr, nameSize, idPtr uint32) uint32 {
		name, ok := readString(m, namePtr, nameSize)
		if !ok {
			return statusBadArgument
		}
		m.Memory().WriteUint32Le(idPtr, p.queues.register(name, instFrom(ctx)))
		return statusOK
	})
	fn("proxy_resolve_shared_queue", func(_ context.Context, m api.Module, vmPtr, vmSize, namePtr, nameSize, idPtr uint32) uint32 {
		vm, ok1 := readString(m, vmPtr, vmSize)
		name, ok2 := readString(m, namePtr, nameSize)
		if !ok1 || !ok2 {
			return statusBadArgument
		}
		if vm != "" && vm != p.cfg.Name {
			return statusNotFound
		}
		id, found := p.queues.resolve(name)
		if !found {
			return statusNotFound
		}
		m.Memory().WriteUint32Le(idPtr, id)
		return statusOK
	})
	fn("proxy_enqueue_shared_queue", func(_ context.Context, m api.Module, id, valPtr, valSize uint32) uint32 {
		val, ok := read(m, valPtr, valSize)
		if !ok {
			return statusBadArgument
		}
		st, owner := p.queues.enqueue(id, val)
		if owner != nil {
			// Asynchronous: the owner may be the instance running right now.
			select {
			case owner.ready <- id:
			default: // notifications pending; the item is picked up with them
			}
		}
		return st
	})
	fn("proxy_dequeue_shared_queue", func(ctx context.Context, m api.Module, id, ptrPtr, sizePtr uint32) uint32 {
		v, st := p.queues.dequeue(id)
		if st != statusOK {
			return st
		}
		return instFrom(ctx).returnBytes(ctx, m, v, ptrPtr, sizePtr)
	})

	// Metrics.
	fn("proxy_define_metric", func(_ context.Context, m api.Module, _, namePtr, nameSize, idPtr uint32) uint32 {
		name, ok := readString(m, namePtr, nameSize)
		if !ok {
			return statusBadArgument
		}
		m.Memory().WriteUint32Le(idPtr, p.metrics.define(name))
		return statusOK
	})
	report := func(name string, v float64) {
		if p.sink != nil {
			p.sink.SetWasmMetric(p.cfg.Name, name, v)
		}
	}
	fn("proxy_increment_metric", func(_ context.Context, id uint32, offset int64) uint32 {
		name, v, ok := p.metrics.update(id, func(cur float64) float64 { return cur + float64(offset) })
		if !ok {
			return statusNotFound
		}
		report(name, v)
		return statusOK
	})
	fn("proxy_record_metric", func(_ context.Context, id uint32, value uint64) uint32 {
		name, v, ok := p.metrics.update(id, func(float64) float64 { return float64(value) })
		if !ok {
			return statusNotFound
		}
		report(name, v)
		return statusOK
	})
	fn("proxy_get_metric", func(_ context.Context, m api.Module, id, retPtr uint32) uint32 {
		_, v, ok := p.metrics.update(id, func(cur float64) float64 { return cur })
		if !ok {
			return statusNotFound
		}
		m.Memory().WriteUint64Le(retPtr, uint64(v))
		return statusOK
	})

	_, err := b.Instantiate(ctx)
	return err
}

// Int64Property encodes integer properties the way Envoy does (8-byte LE).
func Int64Property(v int64) []byte {
	b := make([]byte, 8)
	for i := 0; i < 8; i++ {
		b[i] = byte(v >> (8 * i))
	}
	return b
}

// StringProperty is a convenience for Properties implementations.
func StringProperty(s string) ([]byte, bool) { return []byte(s), true }

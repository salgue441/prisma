// Package admin serves the operational API: health, metrics, config
// inspection and reload, cluster state and a route debugger.
package admin

import (
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/pprof"
	"sort"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"gopkg.in/yaml.v3"

	"prisma/internal/config"
	"prisma/internal/gateway"
)

func New(g *gateway.Gateway, cfg config.Admin, reload func() error) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !g.Ready() {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not ready"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})
	mux.Handle("GET /metrics", promhttp.HandlerFor(g.Metrics().Registry, promhttp.HandlerOpts{}))

	mux.HandleFunc("GET /config", func(w http.ResponseWriter, _ *http.Request) {
		// YAML keeps durations and the layout identical to the config file.
		s := g.Snapshot()
		out, err := yaml.Marshal(map[string]any{
			"version":  s.Version,
			"loadedAt": s.LoadedAt.Format(time.RFC3339),
			"config":   s.Config.Redacted(),
		})
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		w.Header().Set("Content-Type", "application/yaml")
		_, _ = w.Write(out)
	})
	mux.HandleFunc("POST /config/validate", func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"valid": false, "errors": []string{err.Error()}})
			return
		}
		_, version, err := config.Parse(raw)
		if err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"valid": false, "errors": strings.Split(err.Error(), "\n")})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"valid": true, "version": version})
	})
	mux.HandleFunc("POST /config/reload", func(w http.ResponseWriter, _ *http.Request) {
		if err := reload(); err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"reloaded": false, "errors": strings.Split(err.Error(), "\n")})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"reloaded": true, "version": g.Snapshot().Version})
	})

	mux.HandleFunc("GET /clusters", func(w http.ResponseWriter, _ *http.Request) {
		s := g.Snapshot()
		names := make([]string, 0, len(s.Clusters))
		for n := range s.Clusters {
			names = append(names, n)
		}
		sort.Strings(names)
		out := make([]any, 0, len(names))
		for _, n := range names {
			c := s.Clusters[n]
			out = append(out, map[string]any{
				"name":         n,
				"protocol":     c.Config.Protocol,
				"loadBalancer": c.Config.LoadBalancer.Policy,
				"circuitOpen":  c.BreakerOpen(),
				"endpoints":    c.Status(),
			})
		}
		writeJSON(w, http.StatusOK, out)
	})

	// Route debugger: GET /routes?listener=public&host=api.example.com&path=/x&method=GET&header=K:V
	mux.HandleFunc("GET /routes", func(w http.ResponseWriter, r *http.Request) {
		s := g.Snapshot()
		q := r.URL.Query()
		if q.Get("path") == "" {
			names := []string{}
			for _, rt := range s.Router.Routes() {
				names = append(names, rt.Name)
			}
			writeJSON(w, http.StatusOK, map[string]any{"routes": names})
			return
		}
		method := q.Get("method")
		if method == "" {
			method = http.MethodGet
		}
		probe := httptest.NewRequest(method, "http://"+q.Get("host")+q.Get("path"), nil)
		probe.Host = q.Get("host")
		for _, h := range q["header"] {
			if k, v, ok := strings.Cut(h, ":"); ok {
				probe.Header.Add(strings.TrimSpace(k), strings.TrimSpace(v))
			}
		}
		listener := q.Get("listener")
		if listener == "" && len(s.Config.Listeners) > 0 {
			listener = s.Config.Listeners[0].Name
		}
		rt := s.Router.Match(listener, probe)
		if rt == nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"matched": false})
			return
		}
		red := s.Config.Redacted()
		var policy config.Policy
		for i := range red.Routes {
			if red.Routes[i].Name == rt.Name {
				policy = red.EffectivePolicy(&red.Routes[i])
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"matched": true, "route": rt.Name, "listener": listener,
			"policy": policy, "backends": rt.Config.Backends,
		})
	})

	if cfg.Pprof {
		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	}
	return withToken(mux, cfg.Token)
}

// withToken requires "Authorization: Bearer <token>" except on probe endpoints.
func withToken(next http.Handler, token string) http.Handler {
	if token == "" {
		return next
	}
	want := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" && r.URL.Path != "/readyz" &&
			subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

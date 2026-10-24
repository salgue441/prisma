// Command prisma runs the Prisma API gateway.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"sync"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"prisma/internal/admin"
	"prisma/internal/config"
	"prisma/internal/gateway"
	"prisma/internal/gatewayapi"
	"prisma/internal/telemetry"
)

// version is set at build time: -ldflags "-X main.version=v0.2.0".
var version = "dev"

func main() {
	configPath := flag.String("config", "configs/prisma.yaml", "path to the configuration file")
	validate := flag.Bool("validate", false, "validate the configuration and exit")
	logLevel := flag.String("log-level", "info", "log level: debug, info, warn, error")
	watch := flag.Bool("watch", true, "reload automatically when the config file changes")
	flag.Parse()

	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		fmt.Fprintln(os.Stderr, "invalid -log-level:", err)
		os.Exit(2)
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	cfg, cfgVersion, err := config.LoadFile(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid configuration %s:\n%v\n", *configPath, err)
		os.Exit(1)
	}
	if *validate {
		fmt.Printf("configuration %s is valid (version %s)\n", *configPath, cfgVersion)
		return
	}
	slog.Info("starting prisma", "version", version)
	if err := run(*configPath, cfg, cfgVersion, *watch); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(path string, cfg *config.Config, cfgVersion string, watch bool) error {
	metrics := telemetry.NewMetrics()
	access := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	gw := gateway.New(metrics, access)
	defer gw.Close()
	tr := &tracing{gw: gw}
	if err := tr.apply(cfg.Tracing); err != nil {
		return fmt.Errorf("tracing: %w", err)
	}
	defer tr.close()
	if err := gw.Apply(cfg, cfgVersion); err != nil {
		return err
	}

	listeners := gw.NewListenerSet(cfg.Global.DrainTimeout)
	if err := listeners.Apply(cfg.Listeners); err != nil {
		return err
	}

	// applyAll installs routes first, then listeners: a new listener only
	// starts once the routes that reference it exist.
	applyAll := func(c *config.Config, v string) error {
		if err := gw.Apply(c, v); err != nil {
			return err
		}
		return listeners.Apply(c.Listeners)
	}
	var controller *gatewayapi.Controller
	if cfg.GatewayAPI != nil {
		var err error
		if controller, err = gatewayapi.NewController(*cfg.GatewayAPI, cfg, applyAll); err != nil {
			return fmt.Errorf("gateway API controller: %w", err)
		}
	}

	var mu sync.Mutex
	current := cfgVersion
	reload := func() error {
		mu.Lock()
		defer mu.Unlock()
		next, v, err := config.LoadFile(path)
		if err != nil {
			metrics.ConfigReloads.WithLabelValues("invalid").Inc()
			slog.Error("config reload rejected; keeping current config", "version", current, "err", err)
			return err
		}
		if v == current {
			return nil
		}
		if err := tr.apply(next.Tracing); err != nil {
			// Routes still reload; tracing keeps its previous settings.
			slog.Error("tracing reload failed; keeping current tracing", "err", err)
		}
		if controller != nil {
			// The controller merges the new base with cluster resources.
			controller.SetBase(next)
			metrics.ConfigReloads.WithLabelValues("success").Inc()
			current = v
			return nil
		}
		if err := applyAll(next, v); err != nil {
			metrics.ConfigReloads.WithLabelValues("failed").Inc()
			slog.Error("config reload failed; keeping current config", "version", current, "err", err)
			return err
		}
		metrics.ConfigReloads.WithLabelValues("success").Inc()
		current = v
		return nil
	}

	adminSrv := &http.Server{
		Addr:              cfg.Admin.Address,
		Handler:           admin.New(gw, cfg.Admin, reload),
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("admin listening", "address", cfg.Admin.Address)
		if err := adminSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("admin: %w", err)
		}
	}()

	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	if watch {
		go watchFile(ctx, path, func() { _ = reload() })
	}
	if controller != nil {
		go controller.Run(ctx)
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	for {
		select {
		case err := <-errCh:
			return err
		case err := <-listeners.Errors():
			return err
		case sig := <-sigs:
			if sig == syscall.SIGHUP {
				slog.Info("SIGHUP received, reloading configuration")
				_ = reload()
				continue
			}
			slog.Info("shutting down", "signal", sig.String())
			return shutdown(gw, listeners, adminSrv)
		}
	}
}

// shutdown fails readiness first so load balancers stop sending traffic, then
// drains listeners.
func shutdown(gw *gateway.Gateway, listeners *gateway.ListenerSet, adminSrv *http.Server) error {
	gw.SetReady(false)
	g := gw.Snapshot().Config.Global
	if g.ShutdownDelay > 0 {
		time.Sleep(g.ShutdownDelay)
	}
	ctx, cancel := context.WithTimeout(context.Background(), g.DrainTimeout)
	defer cancel()
	listeners.Shutdown(ctx)
	return adminSrv.Shutdown(ctx)
}

// watchFile watches the config's directory (editors and Kubernetes ConfigMaps
// replace files rather than writing in place) and debounces bursts of events.
func watchFile(ctx context.Context, path string, onChange func()) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		slog.Warn("config watch disabled", "err", err)
		return
	}
	defer w.Close()
	abs, _ := filepath.Abs(path)
	if err := w.Add(filepath.Dir(abs)); err != nil {
		slog.Warn("config watch disabled", "err", err)
		return
	}
	var timer *time.Timer
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-w.Events:
			if !ok {
				return
			}
			if p, _ := filepath.Abs(ev.Name); p != abs && filepath.Base(ev.Name) != "..data" {
				continue
			}
			if timer != nil {
				timer.Stop()
			}
			timer = time.AfterFunc(300*time.Millisecond, onChange)
		case err, ok := <-w.Errors:
			if !ok {
				return
			}
			slog.Warn("config watch error", "err", err)
		}
	}
}

// tracing owns the OpenTelemetry provider and replaces it when the tracing
// configuration changes, so reloads can enable, retarget or disable
// tracing without a restart.
type tracing struct {
	gw  *gateway.Gateway
	cfg *config.Tracing
	tp  *sdktrace.TracerProvider
}

func (t *tracing) apply(cfg *config.Tracing) error {
	if reflect.DeepEqual(cfg, t.cfg) {
		return nil
	}
	var tp *sdktrace.TracerProvider
	if cfg != nil {
		var err error
		if tp, err = telemetry.NewTracerProvider(context.Background(), cfg, version); err != nil {
			return err
		}
		slog.Info("tracing enabled", "endpoint", cfg.Endpoint, "sampleRatio", cfg.SampleRatio)
	} else if t.cfg != nil {
		slog.Info("tracing disabled")
	}
	if tp != nil {
		t.gw.SetTracerProvider(tp)
	} else {
		t.gw.SetTracerProvider(nil) // untyped nil: a nil *TracerProvider is not a nil interface
	}
	old := t.tp
	t.tp, t.cfg = tp, cfg
	if old != nil {
		// Spans of requests still in flight end on the old provider; give
		// them time, then flush it.
		go func() {
			time.Sleep(drainGrace)
			flush(old)
		}()
	}
	return nil
}

// close flushes buffered spans before exiting.
func (t *tracing) close() {
	if t.tp != nil {
		flush(t.tp)
	}
}

const drainGrace = 30 * time.Second

func flush(tp *sdktrace.TracerProvider) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = tp.Shutdown(ctx)
}

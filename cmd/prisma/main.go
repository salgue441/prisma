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

	"prisma/internal/admin"
	"prisma/internal/config"
	"prisma/internal/gateway"
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
	if cfg.Tracing != nil {
		tp, err := telemetry.NewTracerProvider(context.Background(), cfg.Tracing, version)
		if err != nil {
			return fmt.Errorf("tracing: %w", err)
		}
		gw.SetTracerProvider(tp)
		defer func() {
			// Flush buffered spans before exiting.
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = tp.Shutdown(ctx)
		}()
		slog.Info("tracing enabled", "endpoint", cfg.Tracing.Endpoint, "sampleRatio", cfg.Tracing.SampleRatio)
	}
	if err := gw.Apply(cfg, cfgVersion); err != nil {
		return err
	}

	servers := make([]*gateway.Server, 0, len(cfg.Listeners))
	for _, l := range cfg.Listeners {
		s, err := gw.NewServer(l)
		if err != nil {
			return err
		}
		if err := s.Listen(); err != nil {
			return err
		}
		servers = append(servers, s)
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
		if !reflect.DeepEqual(next.Listeners, gw.Snapshot().Config.Listeners) {
			slog.Warn("listener changes require a restart; only routes, clusters and policies were reloaded")
		}
		if !reflect.DeepEqual(next.Tracing, gw.Snapshot().Config.Tracing) {
			slog.Warn("tracing changes require a restart")
		}
		if err := gw.Apply(next, v); err != nil {
			metrics.ConfigReloads.WithLabelValues("failed").Inc()
			slog.Error("config reload failed; keeping current config", "version", current, "err", err)
			return err
		}
		for _, s := range servers {
			if err := s.ReloadCertificates(); err != nil {
				slog.Error("certificate reload failed", "listener", s.Name, "err", err)
			}
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

	errCh := make(chan error, len(servers)+1)
	for _, s := range servers {
		go func() { errCh <- s.Serve() }()
	}
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

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	for {
		select {
		case err := <-errCh:
			if err != nil {
				return err
			}
		case sig := <-sigs:
			if sig == syscall.SIGHUP {
				slog.Info("SIGHUP received, reloading configuration")
				_ = reload()
				continue
			}
			slog.Info("shutting down", "signal", sig.String())
			return shutdown(gw, servers, adminSrv)
		}
	}
}

// shutdown fails readiness first so load balancers stop sending traffic, then
// drains listeners.
func shutdown(gw *gateway.Gateway, servers []*gateway.Server, adminSrv *http.Server) error {
	gw.SetReady(false)
	g := gw.Snapshot().Config.Global
	if g.ShutdownDelay > 0 {
		time.Sleep(g.ShutdownDelay)
	}
	ctx, cancel := context.WithTimeout(context.Background(), g.DrainTimeout)
	defer cancel()
	var wg sync.WaitGroup
	for _, s := range servers {
		wg.Go(func() {
			if err := s.Shutdown(ctx); err != nil {
				slog.Warn("listener did not drain in time", "listener", s.Name, "err", err)
			}
		})
	}
	wg.Wait()
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

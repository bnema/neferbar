// Command neferbar is a one-cell-high, terminal-style status bar for Wayland.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"git.bnema.dev/bnema/neferbar/internal/bar"
	"git.bnema.dev/bnema/neferbar/internal/config"
)

func main() {
	cfgPath := flag.String("config", "", "config file (default: $XDG_CONFIG_HOME/neferbar/config.toml)")
	display := flag.String("display", os.Getenv("NEFERBAR_DISPLAY"), "Wayland socket name or absolute path (default: $NEFERBAR_DISPLAY, then $WAYLAND_DISPLAY)")
	pprofAddr := flag.String("pprof", "", "serve pprof on this loopback address, e.g. localhost:6060")
	memStats := flag.Duration("memstats", 0, "log allocation counters at this interval (0: off)")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, log, *cfgPath, *display, *pprofAddr, *memStats); err != nil {
		fmt.Fprintf(os.Stderr, "neferbar: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, log *slog.Logger, cfgPath, display, pprofAddr string, memStats time.Duration) error {
	explicit := cfgPath != ""
	if !explicit {
		cfgPath = config.DefaultPath()
	}
	cfg, err := config.Load(cfgPath, explicit)
	if err != nil {
		return err
	}
	if pprofAddr != "" {
		if err = servePprof(ctx, log, pprofAddr); err != nil {
			return err
		}
	}
	b, err := bar.New(cfg, log, display)
	if err != nil {
		return err
	}
	if memStats > 0 {
		go logMemStats(ctx, log, memStats)
	}
	return b.Run(ctx)
}

// servePprof serves the pprof handlers on a loopback address only.
func servePprof(ctx context.Context, log *slog.Logger, addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("pprof address: %w", err)
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("pprof address %q must be loopback", addr)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("pprof listen: %w", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	// Count every allocation so the heap profile shows all of them.
	runtime.MemProfileRate = 1
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Error("pprof", "err", err)
		}
	}()
	log.Info("pprof listening", "addr", ln.Addr().String())
	return nil
}

// logMemStats prints cumulative allocation counters so a steady-state run can
// be checked for allocations per frame.
func logMemStats(ctx context.Context, log *slog.Logger, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	var ms runtime.MemStats
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			runtime.ReadMemStats(&ms)
			log.Info("memstats", "mallocs", ms.Mallocs, "frees", ms.Frees, "heap_alloc", ms.HeapAlloc,
				"total_alloc", ms.TotalAlloc, "num_gc", ms.NumGC, "goroutines", runtime.NumGoroutine())
		}
	}
}

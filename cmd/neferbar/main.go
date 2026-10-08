// Command neferbar is a one-cell-high, terminal-style status bar for Wayland.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/bnema/neferbar/internal/bar"
	"github.com/bnema/neferbar/internal/config"
)

// version is set at build time: -ldflags "-X main.version=1.2.3".
var version = "dev"

// printVersion implements "neferbar version". A binary built without the
// ldflag reports its module version instead, as with go install pkg@v1.2.3.
func printVersion(args []string, stdout io.Writer) error {
	if len(args) > 0 {
		return fmt.Errorf("unexpected argument %q", args[0])
	}
	v := version
	if info, ok := debug.ReadBuildInfo(); v == "dev" && ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		v = strings.TrimPrefix(info.Main.Version, "v")
	}
	_, err := fmt.Fprintln(stdout, v)
	return err
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		if err := printVersion(os.Args[2:], os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "neferbar version: %v\n", err)
			os.Exit(2)
		}
		return
	}
	if len(os.Args) > 1 && (os.Args[1] == "app" || os.Args[1] == "tray") {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		var err error
		if os.Args[1] == "app" {
			err = runApp(ctx, os.Args[2:], os.Getenv("NEFERBAR_DISPLAY"))
		} else {
			err = runTray(ctx, os.Args[2:])
		}
		stop()
		if err != nil && !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintf(os.Stderr, "neferbar %s: %v\n", os.Args[1], err)
			os.Exit(1)
		}
		return
	}
	cfgPath := flag.String("config", "", "config file (default: $XDG_CONFIG_HOME/neferbar/config.toml)")
	display := flag.String("display", os.Getenv("NEFERBAR_DISPLAY"), "Wayland socket name or absolute path (default: $NEFERBAR_DISPLAY, then $WAYLAND_DISPLAY)")
	pprofAddr := flag.String("pprof", os.Getenv("NEFERBAR_PPROF"), "serve pprof on this loopback address, e.g. localhost:6060 (default: $NEFERBAR_PPROF)")
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
	b, err := bar.New(cfg, cfgPath, log, display)
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
	// "localhost" is resolved by the system: trust the address actually bound.
	if tcp, ok := ln.Addr().(*net.TCPAddr); !ok || !tcp.IP.IsLoopback() {
		_ = ln.Close()
		return fmt.Errorf("pprof address %q is not loopback; refusing to serve profiles on it", addr)
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

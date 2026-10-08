package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/bnema/zerobus"

	"github.com/bnema/neferbar/internal/config"
	"github.com/bnema/neferbar/internal/glyph"
	"github.com/bnema/neferbar/internal/theme"
	"github.com/bnema/neferbar/internal/tray"
)

const trayUsage = `usage: neferbar tray [-config file]

Prints the system tray as one line of Nerd Font icons: one line now, then a
new one each time an item appears, changes or goes away. Each icon takes the
color of the application's own icon. Hidden (passive) items are left out, and
items that need attention use the accent color.

Run it as a module:

  [[module]]
  name = "tray"
  zone = "right"
  exec = '"$NEFERBAR_BIN" tray'

flags:
`

// runTray is "neferbar tray": the StatusNotifierItem tray, for a module.
func runTray(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("neferbar tray", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "config file (default: $XDG_CONFIG_HOME/neferbar/config.toml)")
	fs.Usage = func() { fmt.Fprint(os.Stderr, trayUsage); fs.PrintDefaults() }
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	explicit := *cfgPath != ""
	if !explicit {
		*cfgPath = config.DefaultPath()
	}
	cfg, err := config.Load(*cfgPath, explicit)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	// The icons are glyph names of the bar's font.
	paths, err := glyph.FindFonts(cfg.Bar.Font)
	if err != nil {
		return err
	}
	names, err := glyph.LoadNames(paths[0])
	if err != nil {
		return err
	}
	defer names.Close()
	if names.Len() == 0 {
		log.Warn("the font has no icon names: is bar.font a Nerd Font?", "font", cfg.Bar.Font)
	}

	opt := tray.Options{
		Resolver: tray.NewResolver(names.Lookup, cfg.Tray.Icons, nil),
		Out:      os.Stdout,
		Log:      log,
	}
	opt.Foreground, opt.Background, opt.Accent = trayColors(log)

	c, err := zerobus.SessionBus()
	if err != nil {
		return err
	}
	err = tray.Run(ctx, c, opt)
	if errors.Is(err, zerobus.ErrClosed) {
		return fmt.Errorf("the session bus closed the connection")
	}
	return err
}

// trayColors are the colors the bar gives its scripts, so the tray matches
// the bar. Run outside the bar, in a terminal, it uses the default theme.
func trayColors(log *slog.Logger) (fg, bg, accent tray.RGB) {
	def := theme.Default()
	return envColor(log, "NEFERBAR_FOREGROUND", def.Foreground),
		envColor(log, "NEFERBAR_BACKGROUND", def.Background),
		envColor(log, "NEFERBAR_ACCENT", def.Palette[4])
}

func envColor(log *slog.Logger, name string, def tray.RGB) tray.RGB {
	v, ok := os.LookupEnv(name)
	if !ok {
		return def
	}
	c, err := config.ParseColor(v)
	if err != nil {
		log.Warn("tray: using the default color", "var", name, "err", err)
		return def
	}
	return c
}

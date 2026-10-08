// Package config loads the bar's TOML configuration.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/BurntSushi/toml"
)

// Bar holds the global bar settings.
type Bar struct {
	Font string  `toml:"font"`
	Size float64 `toml:"size"` // font size in logical pixels
	// Scale multiplies the monitor scale (1.0 follows the monitor).
	Scale float64 `toml:"scale"`
	// Output is the wl_output name; empty lets the compositor choose.
	Output string `toml:"output"`
	// Position is the screen edge the bar sits on: "top" or "bottom".
	Position string `toml:"position"`
	// Theme is "auto" (the colors of the terminal in use) or the path of a
	// terminal theme or config file: kitty, foot, ghostty or alacritty.
	Theme string `toml:"theme"`
	// Accent is the number (0-15) of the theme's ANSI color the bundled
	// scripts use for highlights. Scripts read it as $NEFERBAR_ACCENT.
	Accent int `toml:"accent"`
	// Background and Foreground, as #rrggbb, override the theme's.
	Background string `toml:"background"`
	Foreground string `toml:"foreground"`
}

// Module is one script.
type Module struct {
	Name string `toml:"name"`
	Zone string `toml:"zone"` // left | center | right
	Exec string `toml:"exec"`
}

// Tray configures "neferbar tray".
type Tray struct {
	// Icons replaces the icon chosen for an item. The key is the item id or
	// application name, in any case; the value is a Nerd Font glyph name such
	// as "fa-steam", or the text to show; "" hides the item.
	Icons map[string]string `toml:"icons"`
}

// Config is the whole file.
type Config struct {
	Bar    Bar      `toml:"bar"`
	Tray   Tray     `toml:"tray"`
	Module []Module `toml:"module"`
}

// Default returns the built-in settings.
func Default() Config {
	return Config{Bar: Bar{
		Font: "JetBrainsMono Nerd Font Mono", Size: 14, Scale: 1,
		Theme: "auto", Accent: 4, Position: "top",
	}}
}

// DefaultPath is $XDG_CONFIG_HOME/neferbar/config.toml.
func DefaultPath() string {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "neferbar", "config.toml")
}

// Load reads path over the defaults. A missing file at the default path is
// not an error.
func Load(path string, explicit bool) (Config, error) {
	c := Default()
	md, err := toml.DecodeFile(path, &c)
	if err != nil {
		if !explicit && os.IsNotExist(err) {
			return c, nil
		}
		return c, fmt.Errorf("config: %w", err)
	}
	if u := md.Undecoded(); len(u) > 0 {
		return c, fmt.Errorf("config: unknown key %s", u[0])
	}
	return c, c.Validate()
}

// Validate checks every field.
func (c *Config) Validate() error {
	b := &c.Bar
	if b.Font == "" {
		return fmt.Errorf("config: bar.font is empty")
	}
	if b.Size < 6 || b.Size > 128 {
		return fmt.Errorf("config: bar.size %v out of range 6..128", b.Size)
	}
	if b.Scale < 0.5 || b.Scale > 8 {
		return fmt.Errorf("config: bar.scale %v out of range 0.5..8", b.Scale)
	}
	for _, col := range []string{b.Background, b.Foreground} {
		if col == "" {
			continue // taken from the theme
		}
		if _, err := ParseColor(col); err != nil {
			return err
		}
	}
	if b.Position != "top" && b.Position != "bottom" {
		return fmt.Errorf(`config: bar.position %q must be "top" or "bottom"`, b.Position)
	}
	if b.Accent < 0 || b.Accent > 15 {
		return fmt.Errorf("config: bar.accent %d must be a color number from 0 to 15", b.Accent)
	}
	if b.Theme == "" {
		return fmt.Errorf("config: bar.theme is empty; use \"auto\" or the path of a theme file")
	}
	seen := map[string]bool{}
	for i := range c.Module {
		m := &c.Module[i]
		if m.Exec == "" {
			return fmt.Errorf("config: module %d has no exec", i+1)
		}
		if m.Name == "" {
			m.Name = fmt.Sprintf("module%d", i+1)
		}
		if seen[m.Name] {
			return fmt.Errorf("config: two modules are named %q", m.Name)
		}
		seen[m.Name] = true
		switch m.Zone {
		case "left", "center", "right":
		default:
			return fmt.Errorf("config: module %q: zone must be left, center or right", m.Name)
		}
	}
	for k, v := range c.Tray.Icons {
		// The tray prints the value on its one output line.
		if strings.ContainsFunc(v, unicode.IsControl) {
			return fmt.Errorf("config: tray.icons.%s contains a control character", k)
		}
	}
	return nil
}

// ParseColor parses #rrggbb.
func ParseColor(s string) ([3]uint8, error) {
	var r, g, b uint8
	if len(s) != 7 || s[0] != '#' {
		return [3]uint8{}, fmt.Errorf("config: color %q must be #rrggbb", s)
	}
	if _, err := fmt.Sscanf(s[1:], "%02x%02x%02x", &r, &g, &b); err != nil {
		return [3]uint8{}, fmt.Errorf("config: color %q must be #rrggbb", s)
	}
	return [3]uint8{r, g, b}, nil
}

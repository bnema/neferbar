// Package theme reads a color theme: the 16 ANSI colors plus a background and
// a foreground. It understands the config files of kitty, foot, ghostty and
// alacritty, so the bar can look like the terminal you use.
package theme

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// RGB is a color.
type RGB = [3]uint8

// Theme is a set of colors. A color a source does not set keeps the value of
// the theme it was filled from (see Default and Fill).
type Theme struct {
	Background, Foreground RGB
	Palette                [16]RGB
	// Source says where the colors came from, for logs.
	Source string
	// Files lists every file that was read, includes and named themes
	// included, so a caller can watch them for changes.
	Files []string

	hasBG, hasFG bool
	hasPal       [16]bool
}

// Default is the built-in dark theme, used for any color a source leaves out.
func Default() Theme {
	return Theme{
		Background: RGB{0x1e, 0x1e, 0x2e}, Foreground: RGB{0xcd, 0xd6, 0xf4},
		Palette: [16]RGB{
			{0x45, 0x47, 0x5a}, {0xf3, 0x8b, 0xa8}, {0xa6, 0xe3, 0xa1}, {0xf9, 0xe2, 0xaf},
			{0x89, 0xb4, 0xfa}, {0xf5, 0xc2, 0xe7}, {0x94, 0xe2, 0xd5}, {0xba, 0xc2, 0xde},
			{0x58, 0x5b, 0x70}, {0xf3, 0x8b, 0xa8}, {0xa6, 0xe3, 0xa1}, {0xf9, 0xe2, 0xaf},
			{0x89, 0xb4, 0xfa}, {0xf5, 0xc2, 0xe7}, {0x94, 0xe2, 0xd5}, {0xa6, 0xad, 0xc8},
		},
		Source: "built-in",
	}
}

// Fill returns t with every color it does not define taken from def.
func (t Theme) Fill(def Theme) Theme {
	if !t.hasBG {
		t.Background = def.Background
	}
	if !t.hasFG {
		t.Foreground = def.Foreground
	}
	for i := range t.Palette {
		if !t.hasPal[i] {
			t.Palette[i] = def.Palette[i]
		}
	}
	return t
}

// dedupFiles removes repeated entries, keeping the first of each.
func dedupFiles(files []string) []string {
	seen := make(map[string]bool, len(files))
	out := files[:0:0]
	for _, f := range files {
		if !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	return out
}

// Defines reports how many of the 18 colors the source set.
func (t Theme) Defines() int {
	n := 0
	if t.hasBG {
		n++
	}
	if t.hasFG {
		n++
	}
	for _, h := range t.hasPal {
		if h {
			n++
		}
	}
	return n
}

func (t *Theme) setBG(c RGB)         { t.Background, t.hasBG = c, true }
func (t *Theme) setFG(c RGB)         { t.Foreground, t.hasFG = c, true }
func (t *Theme) setPal(i int, c RGB) { t.Palette[i], t.hasPal[i] = c, true }

// overlay applies every color src defines on top of t.
func (t *Theme) overlay(src Theme) {
	t.Files = append(t.Files, src.Files...)
	if src.hasBG {
		t.setBG(src.Background)
	}
	if src.hasFG {
		t.setFG(src.Foreground)
	}
	for i, h := range src.hasPal {
		if h {
			t.setPal(i, src.Palette[i])
		}
	}
}

var hexRe = regexp.MustCompile(`^(?:#|0x|0X)?([0-9a-fA-F]{6})$`)

// parseColor accepts #rrggbb, rrggbb and 0xrrggbb, with optional quotes.
func parseColor(s string) (RGB, bool) {
	s = strings.Trim(strings.TrimSpace(s), `"'`)
	m := hexRe.FindStringSubmatch(s)
	if m == nil {
		return RGB{}, false
	}
	v, _ := strconv.ParseUint(m[1], 16, 32)
	return RGB{uint8(v >> 16), uint8(v >> 8), uint8(v)}, true
}

// expand resolves a leading ~ and, for a relative path, joins it to dir.
func expand(p, dir string) string {
	p = strings.Trim(strings.TrimSpace(p), `"'`)
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		p = filepath.Join(home, strings.TrimPrefix(p, "~"))
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(dir, p)
	}
	return p
}

// maxDepth bounds include chains, so a loop cannot hang the bar.
const maxDepth = 8

// Load reads the theme file at path. The format is recognized from the file:
// a .toml file is alacritty, a file with "palette = N=#rrggbb" or "theme =" lines is
// ghostty, one with a [colors] section is foot, and anything else is kitty.
func Load(path string) (Theme, error) {
	f, err := detectFormat(path)
	if err != nil {
		return Theme{}, err
	}
	t, err := f.load(path)
	if err != nil {
		return Theme{}, err
	}
	if t.Defines() == 0 {
		return Theme{}, fmt.Errorf("theme: %s defines no colors (read as %s)", path, f.name)
	}
	t.Source = fmt.Sprintf("%s (%s)", path, f.name)
	t.Files = dedupFiles(t.Files)
	return t, nil
}

var (
	ghosttyHint = regexp.MustCompile(`(?m)^\s*(palette\s*=\s*\d+\s*=|theme\s*=)`)
	footHint    = regexp.MustCompile(`(?m)^\s*\[colors`)
)

func detectFormat(path string) (format, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return format{}, fmt.Errorf("theme: %w", err)
	}
	switch {
	case strings.HasSuffix(path, ".toml"):
		return formats["alacritty"], nil
	case ghosttyHint.Match(data):
		return formats["ghostty"], nil
	case footHint.Match(data):
		return formats["foot"], nil
	}
	return formats["kitty"], nil
}

type format struct {
	name string
	load func(path string) (Theme, error)
}

var formats = map[string]format{}

func init() {
	formats["kitty"] = format{"kitty", func(p string) (Theme, error) { return loadKitty(p, 0) }}
	formats["foot"] = format{"foot", func(p string) (Theme, error) { return loadFoot(p, 0) }}
	formats["ghostty"] = format{"ghostty", func(p string) (Theme, error) { return loadGhostty(p, 0) }}
	formats["alacritty"] = format{"alacritty", func(p string) (Theme, error) { return loadAlacritty(p, 0) }}
}

// Resolve turns the bar.theme setting into a complete theme. spec is "auto"
// (the terminal in use) or the path of a theme or config file; a relative
// path is taken from baseDir. Colors the source leaves out come from Default.
func Resolve(spec, baseDir string, env Env) (Theme, error) {
	def := Default()
	var t Theme
	var err error
	if spec == "auto" {
		t, err = Auto(env)
	} else {
		t, err = Load(expand(spec, baseDir))
	}
	if err != nil {
		return def, err
	}
	t = t.Fill(def)
	t.Files = dedupFiles(t.Files)
	return t, nil
}

// Mix returns a blended with b: t=0 is a, t=1 is b.
func Mix(a, b RGB, t float64) RGB {
	var o RGB
	for i := range o {
		o[i] = uint8(float64(a[i]) + (float64(b[i])-float64(a[i]))*t + 0.5)
	}
	return o
}

// Hex formats a color as #rrggbb.
func Hex(c RGB) string { return fmt.Sprintf("#%02x%02x%02x", c[0], c[1], c[2]) }

// EnvVars returns the colors as environment entries for scripts:
// NEFERBAR_BACKGROUND, NEFERBAR_FOREGROUND, NEFERBAR_ACCENT and
// NEFERBAR_COLOR0..15, each #rrggbb. bg and fg are the colors the bar really
// uses; accent is the number of the palette color chosen for highlights.
func (t Theme) EnvVars(bg, fg RGB, accent int) []string {
	out := []string{"NEFERBAR_BACKGROUND=" + Hex(bg), "NEFERBAR_FOREGROUND=" + Hex(fg),
		"NEFERBAR_ACCENT=" + Hex(t.Palette[accent&15])}
	for i, c := range t.Palette {
		out = append(out, fmt.Sprintf("NEFERBAR_COLOR%d=%s", i, Hex(c)))
	}
	return out
}

// Equal reports whether two themes have the same colors.
func (t Theme) Equal(o Theme) bool {
	return t.Background == o.Background && t.Foreground == o.Foreground && t.Palette == o.Palette
}

package theme

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// lines returns the non-empty lines of a file, without trailing whitespace.
func lines(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if l := strings.TrimSpace(sc.Text()); l != "" {
			out = append(out, l)
		}
	}
	return out, sc.Err()
}

// ---- kitty: "key value" lines, "include file" ------------------------------

func loadKitty(path string, depth int) (Theme, error) {
	var t Theme
	ls, err := lines(path)
	if err != nil {
		return t, err
	}
	t.Files = append(t.Files, path)
	for _, l := range ls {
		if strings.HasPrefix(l, "#") {
			continue
		}
		key, val, _ := strings.Cut(l, " ")
		val = strings.TrimSpace(val)
		switch {
		case key == "include" && depth < maxDepth:
			inc, err := loadKitty(expand(val, filepath.Dir(path)), depth+1)
			if err == nil { // a missing include is skipped, as kitty would warn
				t.overlay(inc)
			}
		case key == "background":
			if c, ok := parseColor(val); ok {
				t.setBG(c)
			}
		case key == "foreground":
			if c, ok := parseColor(val); ok {
				t.setFG(c)
			}
		case strings.HasPrefix(key, "color"):
			if n, err := strconv.Atoi(strings.TrimPrefix(key, "color")); err == nil && n >= 0 && n < 16 {
				if c, ok := parseColor(val); ok {
					t.setPal(n, c)
				}
			}
		}
	}
	return t, nil
}

// ---- foot: ini, [colors] and [colors-dark], "include=file" -----------------

// footLayers reads one foot file. [colors-dark] wins over the legacy [colors],
// as in foot, and an include is merged layer by layer, as if its text were
// pasted where the include line is.
func footLayers(path string, depth int) (colors, dark Theme, err error) {
	ls, err := lines(path)
	if err != nil {
		return colors, dark, err
	}
	colors.Files = append(colors.Files, path)
	section := ""
	for _, l := range ls {
		if strings.HasPrefix(l, "#") || strings.HasPrefix(l, ";") {
			continue
		}
		if strings.HasPrefix(l, "[") {
			section = strings.Trim(l, "[] ")
			continue
		}
		key, val, ok := strings.Cut(l, "=")
		if !ok {
			continue
		}
		key, val = strings.TrimSpace(key), strings.TrimSpace(val)
		if key == "include" {
			if depth < maxDepth {
				if c, d, err := footLayers(expand(val, filepath.Dir(path)), depth+1); err == nil {
					colors.overlay(c)
					dark.overlay(d)
				}
			}
			continue
		}
		var dst *Theme
		switch section {
		case "colors":
			dst = &colors
		case "colors-dark":
			dst = &dark
		default:
			continue
		}
		val, _, _ = strings.Cut(val, " ") // a trailing "# comment"
		c, ok := parseColor(val)
		if !ok {
			continue
		}
		switch {
		case key == "background":
			dst.setBG(c)
		case key == "foreground":
			dst.setFG(c)
		case strings.HasPrefix(key, "regular"):
			if n, err := strconv.Atoi(key[len("regular"):]); err == nil && n >= 0 && n < 8 {
				dst.setPal(n, c)
			}
		case strings.HasPrefix(key, "bright"):
			if n, err := strconv.Atoi(key[len("bright"):]); err == nil && n >= 0 && n < 8 {
				dst.setPal(8+n, c)
			}
		}
	}
	return colors, dark, nil
}

func loadFoot(path string, depth int) (Theme, error) {
	colors, dark, err := footLayers(path, depth)
	if err != nil {
		return Theme{}, err
	}
	var t Theme
	t.overlay(colors)
	t.overlay(dark)
	return t, nil
}

// ---- ghostty: "key = value", "palette = N=#rrggbb", "theme = name" ---------

func ghosttyDirs(config string) []string {
	home, _ := os.UserHomeDir()
	return []string{
		filepath.Join(filepath.Dir(config), "themes"),
		filepath.Join(home, ".local", "share", "ghostty", "themes"),
		"/usr/share/ghostty/themes",
		"/usr/local/share/ghostty/themes",
	}
}

func loadGhostty(path string) (Theme, error) {
	var own Theme
	themeName := ""
	ls, err := lines(path)
	if err != nil {
		return own, err
	}
	own.Files = append(own.Files, path)
	for _, l := range ls {
		if strings.HasPrefix(l, "#") {
			continue
		}
		key, val, ok := strings.Cut(l, "=")
		if !ok {
			continue
		}
		key, val = strings.TrimSpace(key), strings.TrimSpace(val)
		switch key {
		case "theme":
			themeName = strings.Trim(val, `"'`)
		case "background":
			if c, ok := parseColor(val); ok {
				own.setBG(c)
			}
		case "foreground":
			if c, ok := parseColor(val); ok {
				own.setFG(c)
			}
		case "palette":
			idx, hex, ok := strings.Cut(val, "=")
			if !ok {
				continue
			}
			if n, err := strconv.Atoi(strings.TrimSpace(idx)); err == nil && n >= 0 && n < 16 {
				if c, ok := parseColor(hex); ok {
					own.setPal(n, c)
				}
			}
		}
	}
	// The named theme is applied first; the config's own colors win over it.
	var t Theme
	if themeName != "" {
		if file := findGhosttyTheme(path, themeName); file != "" {
			if th, err := loadGhostty(file); err == nil {
				t.overlay(th)
			}
		}
	}
	t.overlay(own)
	return t, nil
}

// findGhosttyTheme resolves "dark:x,light:y" to x and looks the name up.
func findGhosttyTheme(config, name string) string {
	for _, part := range strings.Split(name, ",") {
		if k, v, ok := strings.Cut(part, ":"); ok {
			if strings.TrimSpace(k) == "dark" {
				name = strings.TrimSpace(v)
			}
		}
	}
	if k, v, ok := strings.Cut(name, ":"); ok && (k == "dark" || k == "light") {
		name = v
	}
	if filepath.IsAbs(name) {
		return name
	}
	for _, dir := range ghosttyDirs(config) {
		if p := filepath.Join(dir, name); fileExists(p) {
			return p
		}
	}
	return ""
}

// ---- alacritty: toml, import = [files], [colors.*] -------------------------

type alacrittyFile struct {
	Import  []string `toml:"import"` // older configs
	General struct {
		Import []string `toml:"import"`
	} `toml:"general"`
	Colors struct {
		Primary struct {
			Background string `toml:"background"`
			Foreground string `toml:"foreground"`
		} `toml:"primary"`
		Normal map[string]string `toml:"normal"`
		Bright map[string]string `toml:"bright"`
	} `toml:"colors"`
}

var alacrittyNames = [8]string{"black", "red", "green", "yellow", "blue", "magenta", "cyan", "white"}

func loadAlacritty(path string, depth int) (Theme, error) {
	var t Theme
	var f alacrittyFile
	if _, err := toml.DecodeFile(path, &f); err != nil {
		return t, err
	}
	t.Files = append(t.Files, path)
	// Imports come first, so the file's own colors win over them.
	if depth < maxDepth {
		for _, imp := range append(f.General.Import, f.Import...) {
			if inc, err := loadAlacritty(expand(imp, filepath.Dir(path)), depth+1); err == nil {
				t.overlay(inc)
			}
		}
	}
	if c, ok := parseColor(f.Colors.Primary.Background); ok {
		t.setBG(c)
	}
	if c, ok := parseColor(f.Colors.Primary.Foreground); ok {
		t.setFG(c)
	}
	for i, name := range alacrittyNames {
		if c, ok := parseColor(f.Colors.Normal[name]); ok {
			t.setPal(i, c)
		}
		if c, ok := parseColor(f.Colors.Bright[name]); ok {
			t.setPal(8+i, c)
		}
	}
	return t, nil
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

var errNoTerminal = errors.New("theme: no terminal config found")

func noTheme(tried []string) error {
	return fmt.Errorf("%w (looked for %s)", errNoTerminal, strings.Join(tried, ", "))
}

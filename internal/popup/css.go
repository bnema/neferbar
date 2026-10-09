package popup

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Inset is the space the popup stylesheet puts between the surface edge and
// the content: 4px of padding and a 1px border.
const Inset = 5

// Style is what the popup stylesheet takes from the bar.
type Style struct {
	Font           string
	Size           float64 // font size in logical pixels
	FG, BG, Accent [3]uint8
}

func hex(c [3]uint8) string { return fmt.Sprintf("#%02x%02x%02x", c[0], c[1], c[2]) }

// cssString makes a font name safe inside a quoted CSS string.
func cssString(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '"' || r == '\\' || r == '{' || r == '}' || r == ';' || r < ' ' || r == 0x7f:
			return -1
		}
		return r
	}, s)
}

// CSS is the stylesheet of every popup.
func CSS(s Style) string {
	fg, bg, ac := hex(s.FG), hex(s.BG), hex(s.Accent)
	size := strconv.FormatFloat(s.Size, 'f', -1, 64)
	var b strings.Builder
	fmt.Fprintf(&b, "app { background-color: %s; color: %s; font-family: %q; font-size: %spx; padding: 4px; border: 1px solid %s; }\n",
		bg, fg, cssString(s.Font), size, ac)
	b.WriteString(".title { font-weight: bold; }\n")
	b.WriteString(".body { opacity: 0.85; }\n")
	b.WriteString("button.item, checkbox.item { text-align: left; background-color: transparent; padding: 2px 12px; }\n")
	fmt.Fprintf(&b, "button.item:hover, checkbox.item:hover, button.item:focus-visible, checkbox.item:focus-visible { background-color: %s; color: %s; }\n", ac, bg)
	b.WriteString("button.item:disabled, checkbox.item:disabled { opacity: 0.5; }\n")
	fmt.Fprintf(&b, "button.item:disabled:hover, checkbox.item:disabled:hover { background-color: transparent; color: %s; }\n", fg)
	fmt.Fprintf(&b, "separator { background-color: %s; opacity: 0.3; margin: 4px 0; }\n", fg)
	return b.String()
}

// WriteCSS stores css as popup.css in $XDG_RUNTIME_DIR/neferbar (directory
// 0700, file 0600, replaced atomically) and returns the path. nefergui reads
// its stylesheet from a path.
func WriteCSS(css string) (string, error) {
	rt := os.Getenv("XDG_RUNTIME_DIR")
	if rt == "" {
		return "", fmt.Errorf("popup: XDG_RUNTIME_DIR is not set")
	}
	dir := filepath.Join(rt, "neferbar")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("popup: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", fmt.Errorf("popup: %w", err)
	}
	f, err := os.CreateTemp(dir, "popup-*.css")
	if err != nil {
		return "", fmt.Errorf("popup: %w", err)
	}
	tmp := f.Name()
	_, err = f.WriteString(css)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp, 0o600)
	}
	path := filepath.Join(dir, "popup.css")
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("popup: %w", err)
	}
	return path, nil
}

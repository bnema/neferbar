package tray

import (
	"os"
	"path/filepath"
	"testing"
)

// fakeFont knows a few glyph names; each maps to a letter so results read
// easily.
var fakeFont = map[string]rune{
	"fa-steam":           'S',
	"custom-obsidian":    'O',
	"md-volume_off":      'M',
	"md-gamepad_variant": 'G',
	"md-web":             'W',
	"md-lan":             'N',
	"md-alpha_z_circle":  'Z',
	"md-google_chrome":   'C',
}

func lookup(name string) (rune, bool) {
	r, ok := fakeFont[name]
	return r, ok
}

func writeDesktop(t *testing.T, dir, name, body string) {
	t.Helper()
	apps := filepath.Join(dir, "applications")
	if err := os.MkdirAll(apps, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(apps, name+".desktop"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestResolve(t *testing.T) {
	dir := t.TempDir()
	writeDesktop(t, dir, "com.example.Racer", "[Desktop Entry]\nName=Racer\nIcon=racer-game\nCategories=Game;\n")
	writeDesktop(t, dir, "chromium", "[Desktop Entry]\nName=Chromium\nIcon=chromium\nCategories=Network;WebBrowser;\n")
	writeDesktop(t, dir, "zed", "[Desktop Entry]\nName=Zed\nStartupWMClass=dev.zed.Zed\n\n[Desktop Action new]\nCategories=Game;\n")
	writeDesktop(t, dir, "dev.lizardbyte.app.Sunrise", "[Desktop Entry]\nName=Sunrise\nCategories=RemoteAccess;Network;\n")
	r := NewResolver(lookup, map[string]string{"Sunshine": "☀", "Mail": "md-lan"}, []string{dir})

	for _, c := range []struct {
		name, id, icon, title, want string
	}{
		{"brand from the id", "steam", "", "", "S"},
		{"brand from a word of the id", "obsidian_status_icon_1", "", "", "O"},
		{"brand from the whole name", "google-chrome", "", "", "C"},
		{"status icon name", "nm-applet", "audio-volume-muted", "", "M"},
		{"category from the desktop file", "racer", "racer-game", "", "G"},
		{"most specific category wins", "chromium", "", "", "W"},
		{"desktop file found by a word of its name", "Sunrise", "", "Sunrise", "N"},
		{"literal override, any case", "sunshine", "", "", "☀"},
		{"glyph-name override", "", "", "mail", "N"},
		{"letter of the desktop name; actions are ignored", "dev.zed.Zed", "", "", "Z"},
		{"letter without the font glyph", "Quux", "", "", "Q"},
		{"a file path is not a name", "", "/run/user/1/steam.png", "", "•"},
		{"nothing to go on", "", "", "", "•"},
	} {
		if got := r.Resolve(c.id, c.icon, c.title); got != c.want {
			t.Errorf("%s: Resolve(%q, %q, %q) = %q, want %q", c.name, c.id, c.icon, c.title, got, c.want)
		}
	}
}

func TestWords(t *testing.T) {
	got := words("org.telegram.desktop_2-Tray 42")
	if len(got) != 1 || got[0] != "telegram" {
		t.Fatalf("words = %q", got)
	}
}

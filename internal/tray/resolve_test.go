package tray

import (
	"os"
	"path/filepath"
	"testing"
	"time"
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
	"md-wifi":            'F',
	"md-sync":            'Y',
	"md-chat":            'H',
	"md-cog":             'K',
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
	writeDesktop(t, dir, "org.gnome.Settings", "[Desktop Entry]\nName=Settings\nCategories=Settings;\n")
	writeDesktop(t, dir, "chatter", "[Desktop Entry]\nName=Chatter\nCategories=Chat;\n")
	writeDesktop(t, dir, "ghost", "[Desktop Entry]\nName=Ghost\nHidden=true\n")
	later := t.TempDir()
	writeDesktop(t, later, "chatter", "[Desktop Entry]\nName=Chatter\nCategories=Game;\n")
	writeDesktop(t, later, "ghost", "[Desktop Entry]\nName=Ghost\nCategories=Game;\n")
	r := NewResolver(lookup, map[string]string{"Sunshine": "☀", "Mail": "md-lan"}, []string{dir, later})

	for _, c := range []struct {
		name, id, icon, title, want string
	}{
		{"brand from the id", "steam", "", "", "S"},
		{"brand from a word of the id", "obsidian_status_icon_1", "", "", "O"},
		{"brand from the whole name", "google-chrome", "", "", "C"},
		{"status icon name", "nm-applet", "audio-volume-muted", "", "M"},
		{"status icon name before a brand", "steam", "audio-volume-muted", "", "M"},
		{"status word inside an app name is not a status", "signal-desktop", "signal-desktop", "", "S"},
		{"wireless status", "nm-applet", "nm-signal-75", "", "F"},
		{"title words are not brands", "", "", "Sync complete", "S"},
		{"whole title as a brand", "", "", "Google Chrome", "C"},
		{"title words find no desktop file", "", "", "Open settings", "O"},
		{"last part of a reverse-DNS name", "settings", "", "", "K"},
		{"earlier data dir wins", "chatter", "", "", "H"},
		{"hidden desktop file counts as deleted", "ghost", "", "", "G"},
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

func TestResolveFindsAppsInstalledLater(t *testing.T) {
	dir := t.TempDir()
	r := NewResolver(lookup, nil, []string{dir})
	if got := r.Resolve("racer", "", ""); got != "R" {
		t.Fatalf("before install: %q", got)
	}
	writeDesktop(t, dir, "racer", "[Desktop Entry]\nName=Racer\nCategories=Game;\n")
	// The directory's mtime must move past the load time.
	future := time.Now().Add(time.Second)
	if err := os.Chtimes(filepath.Join(dir, "applications"), future, future); err != nil {
		t.Fatal(err)
	}
	if got := r.Resolve("racer", "", ""); got != "G" {
		t.Fatalf("after install: %q, want G", got)
	}
}

func TestEmptyOverrideHides(t *testing.T) {
	r := NewResolver(lookup, map[string]string{"steam": ""}, []string{t.TempDir()})
	if got := r.Resolve("steam", "", ""); got != "" {
		t.Fatalf("Resolve = %q, want empty", got)
	}
}

func TestWords(t *testing.T) {
	got := words("org.telegram.desktop_2-Tray 42")
	if len(got) != 1 || got[0] != "telegram" {
		t.Fatalf("words = %q", got)
	}
}

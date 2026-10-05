package theme

import (
	"os"
	"path/filepath"
	"testing"
)

func put(t *testing.T, dir, rel, body string) string {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func want(t *testing.T, got Theme, bg, fg RGB, c1, c12 RGB) {
	t.Helper()
	if got.Background != bg || got.Foreground != fg || got.Palette[1] != c1 || got.Palette[12] != c12 {
		t.Errorf("got bg=%v fg=%v c1=%v c12=%v, want bg=%v fg=%v c1=%v c12=%v",
			got.Background, got.Foreground, got.Palette[1], got.Palette[12], bg, fg, c1, c12)
	}
}

var (
	bg  = RGB{0x07, 0x07, 0x22}
	fg  = RGB{0xf3, 0xed, 0xf7}
	red = RGB{0xfd, 0x46, 0x63}
	blu = RGB{0xa9, 0xae, 0xfe}
)

func TestKittyWithInclude(t *testing.T) {
	d := t.TempDir()
	put(t, d, "themes/x.conf", "background #070722\nforeground #f3edf7\ncolor1 #fd4663\ncolor12 #a9aefe\n")
	main := put(t, d, "kitty.conf", "font_size 12\ninclude themes/x.conf\n")
	got, err := Load(main)
	if err != nil {
		t.Fatal(err)
	}
	want(t, got, bg, fg, red, blu)
	if len(got.Files) != 2 {
		t.Errorf("Files = %v, want the config and the include", got.Files)
	}
}

func TestKittyOwnColorsWinOverInclude(t *testing.T) {
	d := t.TempDir()
	put(t, d, "t.conf", "color1 #000001\n")
	main := put(t, d, "kitty.conf", "include t.conf\ncolor1 #fd4663\n")
	got, _ := Load(main)
	if got.Palette[1] != red {
		t.Errorf("c1 = %v, want the file's own %v", got.Palette[1], red)
	}
}

func TestFootWithInclude(t *testing.T) {
	d := t.TempDir()
	put(t, d, "themes/n", "[colors-dark]\nbackground = 070722\nforeground = f3edf7\nregular1 = fd4663\nbright4 = a9aefe\n")
	main := put(t, d, "foot.ini", "include=~/nowhere\ninclude=themes/n\n[colors]\nforeground = 000000\n")
	got, err := Load(main)
	if err != nil {
		t.Fatal(err)
	}
	// [colors-dark] wins over the legacy [colors], and bright4 is color 12.
	want(t, got, bg, fg, red, blu)
}

func TestGhosttyNamedThemeAndOverrides(t *testing.T) {
	d := t.TempDir()
	put(t, d, "themes/n", "palette = 1=#fd4663\npalette = 12=#a9aefe\nbackground = #070722\nforeground = #f3edf7\n")
	main := put(t, d, "config", "theme = n\nforeground = #ffffff\n")
	got, err := Load(main)
	if err != nil {
		t.Fatal(err)
	}
	want(t, got, bg, RGB{255, 255, 255}, red, blu) // the config's own foreground wins
}

func TestGhosttyDarkLightPair(t *testing.T) {
	d := t.TempDir()
	put(t, d, "themes/night", "palette = 1=#fd4663\n")
	put(t, d, "themes/day", "palette = 1=#000000\n")
	main := put(t, d, "config", "theme = dark:night,light:day\n")
	got, _ := Load(main)
	if got.Palette[1] != red {
		t.Errorf("dark theme not chosen: c1 = %v", got.Palette[1])
	}
}

func TestAlacrittyImport(t *testing.T) {
	d := t.TempDir()
	put(t, d, "themes/n.toml", "[colors.primary]\nbackground = '#070722'\nforeground = '#f3edf7'\n[colors.normal]\nred = '#fd4663'\n[colors.bright]\nblue = '#a9aefe'\n")
	main := put(t, d, "alacritty.toml", "[general]\nimport = [\"themes/n.toml\"]\n")
	got, err := Load(main)
	if err != nil {
		t.Fatal(err)
	}
	want(t, got, bg, fg, red, blu)
}

func TestAlacrittyOwnColorsWinOverImport(t *testing.T) {
	d := t.TempDir()
	put(t, d, "n.toml", "[colors.primary]\nbackground = '#070722'\n")
	main := put(t, d, "a.toml", "import = [\"n.toml\"]\n[colors.primary]\nbackground = '#000000'\n")
	got, _ := Load(main)
	if got.Background != (RGB{}) {
		t.Errorf("bg = %v, want the file's own black", got.Background)
	}
}

func TestIncludeLoopTerminates(t *testing.T) {
	d := t.TempDir()
	put(t, d, "a.conf", "include b.conf\ncolor1 #fd4663\n")
	put(t, d, "b.conf", "include a.conf\n")
	got, err := Load(filepath.Join(d, "a.conf"))
	if err != nil || got.Palette[1] != red {
		t.Fatalf("loop: err=%v c1=%v", err, got.Palette[1])
	}
}

func TestLoadRejectsAFileWithNoColors(t *testing.T) {
	p := put(t, t.TempDir(), "x.conf", "font_size 12\n")
	if _, err := Load(p); err == nil {
		t.Fatal("a file without colors must be an error, not a silent default")
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("a missing file must be an error")
	}
}

func fakeEnv(dir string, vars map[string]string) Env {
	return Env{ConfigDir: dir, Getenv: func(k string) string { return vars[k] }}
}

func TestAutoPrefersTheTerminalInUse(t *testing.T) {
	d := t.TempDir()
	put(t, d, "kitty/kitty.conf", "color1 #000001\n")
	put(t, d, "foot/foot.ini", "[colors]\nregular1 = fd4663\n")
	got, err := Auto(fakeEnv(d, map[string]string{"TERMINAL": "/usr/bin/foot"}))
	if err != nil || got.Palette[1] != red {
		t.Fatalf("$TERMINAL=foot: err=%v c1=%v", err, got.Palette[1])
	}
	put(t, d, "xdg-terminals.list", "# comment\nkitty.desktop\n")
	got, _ = Auto(fakeEnv(d, nil))
	if got.Palette[1] != (RGB{0, 0, 1}) {
		t.Fatalf("xdg-terminals.list=kitty: c1=%v", got.Palette[1])
	}
}

func TestAutoFallsBackToTheFirstConfigWithColors(t *testing.T) {
	d := t.TempDir()
	put(t, d, "kitty/kitty.conf", "font_size 12\n") // exists but has no colors
	put(t, d, "alacritty/alacritty.toml", "[colors.normal]\nred = '#fd4663'\n")
	got, err := Auto(fakeEnv(d, nil))
	if err != nil || got.Palette[1] != red {
		t.Fatalf("err=%v c1=%v", err, got.Palette[1])
	}
}

func TestAutoWithNoTerminalIsAnError(t *testing.T) {
	if _, err := Auto(fakeEnv(t.TempDir(), nil)); err == nil {
		t.Fatal("no terminal config must be an error the caller can report")
	}
}

func TestResolveFillsGapsFromTheDefault(t *testing.T) {
	d := t.TempDir()
	p := put(t, d, "t.conf", "color1 #fd4663\n")
	got, err := Resolve(p, "", fakeEnv(d, nil))
	if err != nil {
		t.Fatal(err)
	}
	if got.Palette[1] != red || got.Palette[2] != Default().Palette[2] || got.Background != Default().Background {
		t.Errorf("gaps not filled: %+v", got.Palette[:3])
	}
	// A relative path is taken from the config directory.
	rel, err := Resolve("t.conf", d, fakeEnv(d, nil))
	if err != nil || rel.Palette[1] != red {
		t.Errorf("relative path: err=%v", err)
	}
}

func TestParseColorForms(t *testing.T) {
	for _, s := range []string{"#fd4663", "fd4663", "0xfd4663", `"#fd4663"`, "'fd4663'"} {
		if c, ok := parseColor(s); !ok || c != red {
			t.Errorf("parseColor(%q) = %v, %v", s, c, ok)
		}
	}
	for _, s := range []string{"", "#fff", "red", "#gg0000", "#fd466300"} {
		if _, ok := parseColor(s); ok {
			t.Errorf("parseColor(%q) accepted", s)
		}
	}
}

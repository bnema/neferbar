package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "c.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadDefaultsAndModules(t *testing.T) {
	c, err := Load(write(t, "[bar]\nsize = 16\n[[module]]\nzone = \"left\"\nexec = \"date\"\n"), true)
	if err != nil {
		t.Fatal(err)
	}
	if c.Bar.Size != 16 || c.Bar.Scale != 1 || c.Bar.Font == "" {
		t.Fatalf("defaults not kept: %+v", c.Bar)
	}
	if c.Module[0].Name != "module1" {
		t.Fatalf("module name = %q", c.Module[0].Name)
	}
}

func TestLoadErrors(t *testing.T) {
	for name, body := range map[string]string{
		"unknown key":  "[bar]\nfnot = 1\n",
		"bad zone":     "[[module]]\nzone = \"top\"\nexec = \"x\"\n",
		"no exec":      "[[module]]\nzone = \"left\"\n",
		"bad size":     "[bar]\nsize = 1\n",
		"bad scale":    "[bar]\nscale = 20\n",
		"bad color":    "[bar]\nbackground = \"red\"\n",
		"position":     "[bar]\nposition = \"middle\"\n",
		"accent low":   "[bar]\naccent = -1\n",
		"accent high":  "[bar]\naccent = 16\n",
		"syntax error": "[bar\n",
	} {
		if _, err := Load(write(t, body), true); err == nil {
			t.Errorf("%s: want error", name)
		} else if !strings.Contains(err.Error(), "config") && !strings.Contains(err.Error(), "toml") {
			t.Errorf("%s: unexpected error %v", name, err)
		}
	}
}

func TestMissingFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "none.toml")
	if _, err := Load(p, false); err != nil {
		t.Fatalf("default path missing must be fine: %v", err)
	}
	if _, err := Load(p, true); err == nil {
		t.Fatal("explicit path missing must fail")
	}
}

func TestAccentDefaultsToBlue(t *testing.T) {
	c, err := Load(write(t, "[bar]\nsize = 14\n"), true)
	if err != nil || c.Bar.Accent != 4 {
		t.Fatalf("accent = %d, err %v; want the default 4", c.Bar.Accent, err)
	}
	c, err = Load(write(t, "[bar]\naccent = 2\n"), true)
	if err != nil || c.Bar.Accent != 2 {
		t.Fatalf("accent = %d, err %v; want 2", c.Bar.Accent, err)
	}
}

func TestPositionDefaultsToTop(t *testing.T) {
	for config, want := range map[string]string{
		"[bar]\nsize = 14\n":             "top",
		"[bar]\nposition = \"top\"\n":    "top",
		"[bar]\nposition = \"bottom\"\n": "bottom",
	} {
		c, err := Load(write(t, config), true)
		if err != nil || c.Bar.Position != want {
			t.Errorf("%q: position = %q, err %v; want %q", config, c.Bar.Position, err, want)
		}
	}
}

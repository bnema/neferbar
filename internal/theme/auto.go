package theme

import (
	"os"
	"path/filepath"
	"strings"
)

// terminal describes where one terminal keeps its config.
type terminal struct {
	name   string
	format string
	rel    string   // path under the config directory
	match  []string // substrings of $TERMINAL or a desktop-file id
}

// terminals is the order used when nothing says which one is in use.
var terminals = []terminal{
	{"kitty", "kitty", "kitty/kitty.conf", []string{"kitty"}},
	{"ghostty", "ghostty", "ghostty/config", []string{"ghostty"}},
	{"foot", "foot", "foot/foot.ini", []string{"foot"}},
	{"alacritty", "alacritty", "alacritty/alacritty.toml", []string{"alacritty"}},
}

// Env is how Auto reads its surroundings, so tests can fake them.
type Env struct {
	Getenv    func(string) string
	ConfigDir string // $XDG_CONFIG_HOME or ~/.config
}

// SystemEnv is the real environment.
func SystemEnv() Env {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".config")
	}
	return Env{Getenv: os.Getenv, ConfigDir: dir}
}

// preferred names the terminal the user says they use, or "". It looks at
// $TERMINAL, then at the first entry of xdg-terminals.list (the file read by
// xdg-terminal-exec).
func (e Env) preferred() (string, string) {
	if v := e.Getenv("TERMINAL"); v != "" {
		return filepath.Base(strings.Fields(v)[0]), "$TERMINAL"
	}
	data, err := os.ReadFile(filepath.Join(e.ConfigDir, "xdg-terminals.list"))
	if err == nil {
		for _, l := range strings.Split(string(data), "\n") {
			if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
				return l, "xdg-terminals.list"
			}
		}
	}
	return "", ""
}

// Auto reads the theme of the terminal in use: the one named by $TERMINAL or
// xdg-terminals.list when its config exists, otherwise the first of kitty,
// ghostty, foot and alacritty that has a config with colors in it.
func Auto(e Env) (Theme, error) {
	var order []terminal
	why := "first terminal config found"
	if want, src := e.preferred(); want != "" {
		for _, t := range terminals {
			for _, m := range t.match {
				if strings.Contains(strings.ToLower(want), m) {
					order = append(order, t)
				}
			}
		}
		if len(order) > 0 {
			why = src + " says " + want
		}
	}
	for _, t := range terminals {
		order = append(order, t)
	}
	var tried []string
	for _, t := range order {
		path := filepath.Join(e.ConfigDir, t.rel)
		tried = append(tried, path)
		if !fileExists(path) {
			continue
		}
		th, err := formats[t.format].load(path)
		if err != nil || th.Defines() == 0 {
			continue
		}
		th.Source = t.name + " (" + path + "): " + why
		return th, nil
	}
	return Theme{}, noTheme(tried)
}

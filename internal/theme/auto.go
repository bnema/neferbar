package theme

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// terminal describes where one terminal keeps its config.
type terminal struct {
	name   string
	format string
	rel    string // path under the config directory
}

// terminals are the terminals auto knows how to read, by the name $TERMINAL
// gives them.
var terminals = map[string]terminal{
	"kitty":     {"kitty", "kitty", "kitty/kitty.conf"},
	"ghostty":   {"ghostty", "ghostty", "ghostty/config"},
	"foot":      {"foot", "foot", "foot/foot.ini"},
	"alacritty": {"alacritty", "alacritty", "alacritty/alacritty.toml"},
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

// Auto reads the theme of the terminal named by $TERMINAL: a bare name such as
// "kitty" or a path such as "/usr/bin/foot". It is deliberately the only rule,
// so there is one place to look when the colors are not the ones you expect.
func Auto(e Env) (Theme, error) {
	value := strings.TrimSpace(e.Getenv("TERMINAL"))
	if value == "" {
		return Theme{}, fmt.Errorf(`theme = "auto" needs $TERMINAL (for example TERMINAL=kitty). ` +
			`Put it in ~/.config/environment.d/10-terminal.conf and start a new session`)
	}
	// Only the program name counts: "/usr/bin/foot --server" is foot.
	name := filepath.Base(strings.Fields(value)[0])
	term, ok := terminals[name]
	if !ok {
		return Theme{}, fmt.Errorf("$TERMINAL is %q, but neferbar reads the themes of kitty, ghostty, foot and alacritty only; "+
			`set bar.theme to the path of a theme file instead`, name)
	}
	path := filepath.Join(e.ConfigDir, term.rel)
	if !fileExists(path) {
		return Theme{}, fmt.Errorf("$TERMINAL is %s, but %s does not exist", name, path)
	}
	th, err := formats[term.format].load(path)
	if err != nil {
		return Theme{}, fmt.Errorf("cannot read the %s config %s: %w", name, path, err)
	}
	if th.Defines() == 0 {
		return Theme{}, fmt.Errorf("the %s config %s sets no colors", name, path)
	}
	th.Source = fmt.Sprintf("%s (%s), from $TERMINAL", name, path)
	return th, nil
}

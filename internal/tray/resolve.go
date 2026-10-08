package tray

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

// Resolver picks the icon of a tray item from what the system already knows,
// so a new application needs no table entry:
//
//  1. the user's [tray.icons] override;
//  2. a glyph for a freedesktop status icon name, such as "audio-volume-muted";
//  3. a Nerd Font glyph named after the application, such as "fa-steam";
//  4. a glyph for the category of its .desktop file, such as Game;
//  5. a letter in a circle.
type Resolver struct {
	lookup   func(name string) (rune, bool)
	icons    map[string]string // lowercased keys
	dataDirs []string
	apps     map[string]*desktopApp // nil until the first lookup
	loaded   time.Time              // when apps was read
}

// desktopApp is what the resolver uses from a .desktop file.
type desktopApp struct {
	name       string
	words      []string // file name, Icon and StartupWMClass, lowercased
	categories []string
	hidden     bool // Hidden=true: the application counts as deleted
}

// NewResolver returns a resolver that finds glyphs with lookup (a font's
// glyph names) and .desktop files under dataDirs/applications. Nil dataDirs
// uses the XDG data directories.
func NewResolver(lookup func(string) (rune, bool), icons map[string]string, dataDirs []string) *Resolver {
	r := &Resolver{lookup: lookup, icons: make(map[string]string, len(icons)), dataDirs: dataDirs}
	for k, v := range icons {
		r.icons[strings.ToLower(k)] = v
	}
	if r.dataDirs == nil {
		r.dataDirs = xdgDataDirs()
	}
	return r
}

// prefixes are the Nerd Font sets, brand-rich ones first.
var prefixes = [...]string{"linux", "dev", "fa", "md", "seti", "fae", "cod", "oct", "custom"}

// stopwords are parts of item ids and names that say nothing about the
// application.
var stopwords = map[string]bool{
	"org": true, "com": true, "net": true, "io": true, "dev": true, "github": true, "gitlab": true,
	"desktop": true, "app": true, "application": true, "tray": true, "icon": true, "status": true,
	"notifier": true, "item": true, "indicator": true, "applet": true, "client": true, "linux": true,
	"gnome": true, "kde": true, "freedesktop": true, "symbolic": true, "panel": true, "the": true,
	"ayatana": true, "notification": true, "electron": true, "bin": true,
}

// statusIcons maps freedesktop status icon names, by prefix, to glyphs, most
// specific first. A prefix matches whole dash-separated parts, so
// "signal-desktop" or "evolution-mail" are not status icons.
var statusIcons = [...][2]string{
	{"audio-volume-muted", "md-volume_off"}, {"audio-volume", "md-volume_high"},
	{"microphone-sensitivity-muted", "md-microphone_off"}, {"microphone-sensitivity", "md-microphone"},
	{"audio-input-microphone-muted", "md-microphone_off"}, {"audio-input-microphone", "md-microphone"},
	{"bluetooth", "md-bluetooth"}, {"battery", "md-battery"},
	{"network-vpn", "md-vpn"}, {"nm-vpn", "md-vpn"}, {"network-wired", "md-lan"}, {"nm-device-wired", "md-lan"},
	{"network-wireless", "md-wifi"}, {"nm-signal", "md-wifi"}, {"nm-device-wireless", "md-wifi"},
	{"network", "md-lan"},
	{"software-update", "md-update"}, {"system-software-update", "md-update"},
	{"mail-unread", "md-email"}, {"mail-read", "md-email"}, {"mail-message-new", "md-email"},
}

// statusIcon returns the glyph name for a status icon name, or "".
func statusIcon(iconName string) string {
	n := strings.ToLower(iconName)
	for _, s := range statusIcons {
		if rest, ok := strings.CutPrefix(n, s[0]); ok && (rest == "" || rest[0] == '-') {
			return s[1]
		}
	}
	return ""
}

// categories maps .desktop categories to glyphs, most specific first: an
// application lists broad categories too, such as "Network;WebBrowser".
var categories = [...][2]string{
	{"TerminalEmulator", "md-console"}, {"WebBrowser", "md-web"}, {"Email", "md-email"},
	{"InstantMessaging", "md-chat"}, {"Chat", "md-chat"}, {"Game", "md-gamepad_variant"},
	{"Music", "md-music"}, {"Audio", "md-music"}, {"Player", "md-play_circle"},
	{"Video", "md-play_circle"}, {"AudioVideo", "md-play_circle"}, {"Graphics", "md-palette"},
	{"IDE", "md-code_braces"}, {"Development", "md-code_braces"}, {"Office", "md-file_document"},
	{"FileManager", "md-folder"}, {"FileTransfer", "md-download"}, {"P2P", "md-download"},
	{"Security", "md-shield"}, {"Network", "md-lan"}, {"Settings", "md-cog"},
	{"System", "md-cog"}, {"Utility", "md-tools"},
}

// Resolve returns the icon text of an item: one glyph, or the user's
// override. id, iconName and title are the item's properties; any may be
// empty. A title can say anything ("Sync complete"), so it only counts as a
// whole name, after the stronger signals.
func (r *Resolver) Resolve(id, iconName, title string) string {
	if strings.HasPrefix(iconName, "/") {
		iconName = "" // a file path names no application
	}
	keys := [...]string{id, iconName, title}
	for _, k := range keys {
		if v, ok := r.icons[strings.ToLower(k)]; ok && k != "" {
			return r.glyphOr(v, v)
		}
	}
	app := r.app(id, iconName, title)
	if app != nil {
		if v, ok := r.icons[strings.ToLower(app.name)]; ok {
			return r.glyphOr(v, v)
		}
	}
	// Status icon names first: "audio-volume-muted" is a state, not a brand.
	if s := statusIcon(iconName); s != "" {
		if g, ok := r.glyph(s); ok {
			return g
		}
	}
	names := []string{id, iconName}
	if app != nil {
		names = append(names, app.words...)
		names = append(names, app.name)
	}
	for _, n := range names {
		if g, ok := r.brand(n); ok {
			return g
		}
	}
	if g, ok := r.prefixed(underscored(title)); ok {
		return g
	}
	if app != nil {
		for _, c := range categories {
			for _, ac := range app.categories {
				if ac == c[0] {
					if g, ok := r.glyph(c[1]); ok {
						return g
					}
				}
			}
		}
	}
	for _, n := range [...]string{nameOf(app), title, id} {
		if l := firstLetter(n); l != 0 {
			return r.glyphOr("md-alpha_"+string(l)+"_circle", string(unicode.ToUpper(l)))
		}
	}
	return "•"
}

func nameOf(app *desktopApp) string {
	if app == nil {
		return ""
	}
	return app.name
}

// glyph returns the glyph called name as a string.
func (r *Resolver) glyph(name string) (string, bool) {
	if rn, ok := r.lookup(name); ok {
		return string(rn), true
	}
	return "", false
}

// glyphOr returns the glyph called name, or fallback when the font has none.
func (r *Resolver) glyphOr(name, fallback string) string {
	if g, ok := r.glyph(name); ok {
		return g
	}
	return fallback
}

// underscored lowercases s and joins its parts with '_', the way glyph names
// are spelled: "Google Chrome" gives "google_chrome".
func underscored(s string) string {
	return strings.Map(func(c rune) rune {
		if c == '-' || c == '.' || c == ' ' {
			return '_'
		}
		return unicode.ToLower(c)
	}, s)
}

// brand finds a glyph named after s: its whole name, then each word.
func (r *Resolver) brand(s string) (string, bool) {
	if whole := underscored(s); !stopwords[whole] {
		if g, ok := r.prefixed(whole); ok {
			return g, true
		}
	}
	for _, w := range words(s) {
		if g, ok := r.prefixed(w); ok {
			return g, true
		}
	}
	return "", false
}

func (r *Resolver) prefixed(w string) (string, bool) {
	if len(w) < 3 {
		return "", false
	}
	for _, p := range prefixes {
		if g, ok := r.glyph(p + "-" + w); ok {
			return g, true
		}
	}
	return "", false
}

// words splits s into lowercase alphanumeric words of 3 or more letters,
// without stopwords or numbers.
func words(s string) []string {
	var out []string
	for f := range strings.FieldsFuncSeq(strings.ToLower(s), func(c rune) bool {
		return !unicode.IsLetter(c) && !unicode.IsDigit(c)
	}) {
		if len(f) >= 3 && !stopwords[f] && strings.ContainsFunc(f, unicode.IsLetter) {
			out = append(out, f)
		}
	}
	return out
}

func firstLetter(s string) rune {
	for _, c := range strings.ToLower(s) {
		if c >= 'a' && c <= 'z' {
			return c
		}
	}
	return 0
}

// app finds the .desktop file of an item. The index is read on the first
// call, and again on a miss when an applications directory changed since,
// as when an application is installed during the session.
func (r *Resolver) app(id, iconName, title string) *desktopApp {
	if r.apps == nil {
		r.reload()
	}
	if a := r.findApp(id, iconName, title); a != nil {
		return a
	}
	if r.changed() {
		r.reload()
		return r.findApp(id, iconName, title)
	}
	return nil
}

func (r *Resolver) reload() {
	r.loaded = time.Now()
	r.apps = loadApps(r.dataDirs)
}

// findApp looks up the whole id, icon name and title, then the words of the
// id and icon name. Titles are free text: their words are not used.
func (r *Resolver) findApp(id, iconName, title string) *desktopApp {
	for _, k := range [...]string{id, iconName, title} {
		if a := r.apps[strings.ToLower(k)]; a != nil && k != "" {
			return a
		}
	}
	for _, k := range [...]string{id, iconName} {
		for _, w := range words(k) {
			if a := r.apps[w]; a != nil {
				return a
			}
		}
	}
	return nil
}

// changed reports whether an applications directory was modified after the
// index was read.
func (r *Resolver) changed() bool {
	for _, d := range r.dataDirs {
		if d == "" {
			continue
		}
		if st, err := os.Stat(filepath.Join(d, "applications")); err == nil && !st.ModTime().Before(r.loaded) {
			return true
		}
	}
	return false
}

func xdgDataDirs() []string {
	home := os.Getenv("XDG_DATA_HOME")
	if home == "" {
		if h, err := os.UserHomeDir(); err == nil {
			home = filepath.Join(h, ".local", "share")
		}
	}
	dirs := os.Getenv("XDG_DATA_DIRS")
	if dirs == "" {
		dirs = "/usr/local/share:/usr/share"
	}
	return append([]string{home}, filepath.SplitList(dirs)...)
}

// loadApps indexes the .desktop files of dataDirs by lowercased file name,
// Icon and StartupWMClass, and by the last part of reverse-DNS ones
// ("org.gnome.Settings" gives "settings" too). Whole values win over last
// parts, and earlier directories over later ones, as in the XDG spec. A
// hidden application keeps its keys, with no application behind them.
func loadApps(dataDirs []string) map[string]*desktopApp {
	apps := map[string]*desktopApp{}
	var all []*desktopApp
	for _, d := range dataDirs {
		if d == "" {
			continue
		}
		files, _ := filepath.Glob(filepath.Join(d, "applications", "*.desktop"))
		for _, f := range files {
			if a := readDesktop(f); a != nil {
				all = append(all, a)
			}
		}
	}
	add := func(k string, a *desktopApp) {
		if _, taken := apps[k]; !taken && k != "" {
			if a.hidden {
				a = nil
			}
			apps[k] = a
		}
	}
	for _, a := range all {
		for _, w := range a.words {
			add(w, a)
		}
	}
	for _, a := range all {
		for _, v := range a.words {
			if i := strings.LastIndexByte(v, '.'); i >= 0 {
				add(v[i+1:], a)
			}
		}
	}
	return apps
}

// readDesktop reads the keys the resolver needs from the [Desktop Entry]
// group of a .desktop file.
func readDesktop(path string) *desktopApp {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	a := &desktopApp{words: []string{strings.ToLower(strings.TrimSuffix(filepath.Base(path), ".desktop"))}}
	in := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "[") {
			if in {
				break // only the first group is the entry
			}
			in = line == "[Desktop Entry]"
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !in || !ok {
			continue
		}
		switch strings.TrimSpace(k) {
		case "Name":
			a.name = strings.TrimSpace(v)
		case "Icon":
			if v = strings.TrimSpace(v); v != "" && !strings.HasPrefix(v, "/") {
				a.words = append(a.words, strings.ToLower(v))
			}
		case "StartupWMClass":
			if v = strings.TrimSpace(v); v != "" {
				a.words = append(a.words, strings.ToLower(v))
			}
		case "Categories":
			a.categories = strings.FieldsFunc(v, func(c rune) bool { return c == ';' })
		case "Hidden":
			a.hidden = strings.TrimSpace(v) == "true"
		case "Type":
			if strings.TrimSpace(v) != "Application" {
				return nil
			}
		}
	}
	if !in {
		return nil
	}
	return a
}

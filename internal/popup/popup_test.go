package popup

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bnema/neferclient"

	"github.com/bnema/neferbar/internal/module"
)

func TestCellRectRoundsOutward(t *testing.T) {
	// Scale 1.6, 13 px cells: a cell is 8.125 logical px wide.
	got := CellRect(2, 1, 13, 1.6, 20)
	if want := (neferclient.Rect{X: 16, Y: 0, Width: 9, Height: 20}); got != want { // [16.25, 24.375) -> [16, 25)
		t.Fatalf("CellRect = %+v, want %+v", got, want)
	}
	got = CellRect(0, 4, 10, 1, 24)
	if want := (neferclient.Rect{X: 0, Y: 0, Width: 40, Height: 24}); got != want {
		t.Fatalf("CellRect = %+v, want %+v", got, want)
	}
}

func TestAnchorIsClampedToTheShownCells(t *testing.T) {
	// The module shows cells 10..13 (4 cells) of the row; 10 px cells at scale 1.
	r, ok := Anchor(10, 4, 1, 2, 10, 1, 20)
	if !ok || r != (neferclient.Rect{X: 110, Width: 20, Height: 20}) {
		t.Fatalf("Anchor = %+v %v", r, ok)
	}
	// A width that runs past the shown cells is cut at the module's edge.
	r, ok = Anchor(10, 4, 3, 5, 10, 1, 20)
	if !ok || r.X != 130 || r.Width != 10 {
		t.Fatalf("clamped Anchor = %+v %v", r, ok)
	}
	for _, c := range [][2]int{{-1, 1}, {4, 1}, {0, 0}, {0, -2}} {
		if _, ok = Anchor(10, 4, c[0], c[1], 10, 1, 20); ok {
			t.Errorf("Anchor(col %d, width %d) must fail", c[0], c[1])
		}
	}
	if _, ok = Anchor(0, 4, 0, 1, 0, 1, 20); ok {
		t.Error("zero cell width must fail")
	}
	if _, ok = Anchor(0, 4, 0, 1, 10, 0, 20); ok {
		t.Error("zero scale must fail")
	}
}

func TestEdgeFollowsTheBarPosition(t *testing.T) {
	if Edge(false) != neferclient.EdgeBottom || Edge(true) != neferclient.EdgeTop {
		t.Fatal("a top bar opens below, a bottom bar above")
	}
	p := Placement(neferclient.Rect{Width: 1, Height: 1}, true, 100, 50)
	if p.Edge != neferclient.EdgeTop || p.Gravity != neferclient.EdgeTop || p.Width != 100 || p.Height != 50 {
		t.Fatalf("placement = %+v", p)
	}
	want := neferclient.AdjustSlideX | neferclient.AdjustFlipY | neferclient.AdjustResizeY
	if p.Adjust != want {
		t.Fatalf("adjust = %v, want %v", p.Adjust, want)
	}
}

func TestClamp(t *testing.T) {
	for _, c := range []struct {
		w, h       float64
		cw, ch     int32
		wantScroll bool
	}{
		{100.2, 50.1, 101, 51, false},
		{0, 0, 1, 1, false},
		{200, 600, 200, 600, false},
		{200, 600.5, 200, MaxHeight, true},
		{200, 5000, 200, MaxHeight, true},
	} {
		cw, ch, scroll := Clamp(c.w, c.h)
		if cw != c.cw || ch != c.ch || scroll != c.wantScroll {
			t.Errorf("Clamp(%v,%v) = %d,%d,%v; want %d,%d,%v", c.w, c.h, cw, ch, scroll, c.cw, c.ch, c.wantScroll)
		}
	}
}

func TestCSS(t *testing.T) {
	got := CSS(Style{Font: `My "Nerd" Font`, Size: 14.5, FG: [3]uint8{0xcc, 0xcc, 0xcc}, BG: [3]uint8{0x10, 0x20, 0x30}, Accent: [3]uint8{0x35, 0x84, 0xe4}})
	want := `app { background-color: #102030; color: #cccccc; font-family: "My Nerd Font"; font-size: 14.5px; padding: 4px; border: 1px solid #3584e4; }
.title { font-weight: bold; }
.body { opacity: 0.85; }
.table { column-gap: 1.5em; }
.key { opacity: 0.7; }
button.item, checkbox.item { text-align: left; background-color: transparent; padding: 2px 12px; }
checkbox.item { padding-left: 2em; }
button.item:hover, checkbox.item:hover, button.item:focus-visible, checkbox.item:focus-visible { background-color: #3584e4; color: #102030; }
button.item:disabled, checkbox.item:disabled { opacity: 0.5; }
button.item:disabled:hover, checkbox.item:disabled:hover { background-color: transparent; color: #cccccc; }
separator { background-color: #cccccc; opacity: 0.3; margin: 4px 0; }
`
	if got != want {
		t.Fatalf("CSS =\n%s\nwant\n%s", got, want)
	}
	// Non-ASCII names stay as they are: %q would have turned them into escapes.
	if got := CSS(Style{Font: "JetBrainsMono Nerd Font Mono é", Size: 10}); !strings.Contains(got, `font-family: "JetBrainsMono Nerd Font Mono é";`) {
		t.Fatalf("font-family changed:\n%s", got)
	}
	// Runes that do not print are dropped: controls, bidi overrides, DEL, BOM.
	if got := cssString("a\u202eb\u0000c\x7fd\ufeffe\u200bf\n"); got != "abcdef" {
		t.Fatalf("cssString = %q", got)
	}
	// A font name cannot break out of the declaration.
	evil := CSS(Style{Font: `x"; } body { `, Size: 10})
	if strings.Count(evil, "{") != strings.Count(want, "{") {
		t.Fatalf("a font name injected rules:\n%s", evil)
	}
}

func TestWriteCSSPermissions(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", dir)
	path, err := WriteCSS("app { color: red; }\n")
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(dir, "neferbar", "popup-"+strconv.Itoa(os.Getpid())+".css") {
		t.Fatalf("path = %s", path)
	}
	for p, want := range map[string]os.FileMode{filepath.Dir(path): 0o700, path: 0o600} {
		st, err := os.Stat(p)
		if err != nil || st.Mode().Perm() != want {
			t.Errorf("%s: mode %v err %v, want %v", p, st.Mode().Perm(), err, want)
		}
	}
	// A rewrite replaces the file and leaves no temporary behind.
	if _, err = WriteCSS("app { color: blue; }\n"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != "app { color: blue; }\n" {
		t.Fatalf("content = %q", b)
	}
	if ents, _ := os.ReadDir(filepath.Dir(path)); len(ents) != 1 {
		t.Fatalf("directory holds %d entries, want 1", len(ents))
	}
	// Another bar's file is left alone; ours is removed.
	other := filepath.Join(filepath.Dir(path), "popup-1.css")
	if err = os.WriteFile(other, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	RemoveCSS()
	if _, err = os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("RemoveCSS left %s (%v)", path, err)
	}
	if _, err = os.Stat(other); err != nil {
		t.Fatalf("RemoveCSS touched another bar's file: %v", err)
	}
	RemoveCSS() // twice is fine
	t.Setenv("XDG_RUNTIME_DIR", "")
	if _, err = WriteCSS("x"); err == nil {
		t.Fatal("no runtime dir must fail")
	}
}

func TestTooltipLines(t *testing.T) {
	// A blank line in the body is kept; blank lines around it are not.
	m := newTooltip("Title", "\na\n\nb\n", nil)
	if m.title != "Title" || !slices.Equal(m.lines, []string{"a", "", "b"}) {
		t.Fatalf("tooltip = %+v", m)
	}
	if m = newTooltip("", "", nil); len(m.lines) != 0 || m.cols != 0 {
		t.Fatalf("empty tooltip = %+v", m)
	}
}

func TestTooltipTable(t *testing.T) {
	m := newTooltip("", "", [][]string{{"Signal", "62%"}, {"Band", "5 GHz", "ch 100"}, {"x"}})
	if m.cols != 3 || len(m.rows) != 3 {
		t.Fatalf("table = %+v", m)
	}
}

func TestTooltipKeyIncludesRows(t *testing.T) {
	a := module.Control{Type: module.ControlTooltip, Width: 1, Rows: [][]string{{"a", "b"}}}
	b := module.Control{Type: module.ControlTooltip, Width: 1, Rows: [][]string{{"ab"}}}
	if keyOf(a) == keyOf(b) {
		t.Fatal("different tables must not share a key")
	}
	if keyOf(a) != keyOf(module.Control{Type: module.ControlTooltip, Width: 1, Rows: [][]string{{"a", "b"}}}) {
		t.Fatal("same table, same key")
	}
}

func sampleMenu() *menuModel {
	return newMenu([]module.MenuItem{
		{ID: 1, Label: "Open", Kind: module.KindNormal, Enabled: true},
		{ID: 4, Label: "Settings", Kind: module.KindNormal, Enabled: true, Items: []module.MenuItem{
			{ID: 5, Label: "Advanced", Kind: module.KindNormal, Enabled: true, Items: []module.MenuItem{
				{ID: 8, Label: "Deep", Kind: module.KindNormal, Enabled: true},
			}},
			{ID: 6, Label: "Quiet", Kind: module.KindCheck, Enabled: true},
		}},
	}, 9)
}

func TestMenuTransitions(t *testing.T) {
	m := sampleMenu()
	if m.chosen != -1 || m.inSubmenu() || len(m.level()) != 2 {
		t.Fatalf("start = %+v", m)
	}
	if m.pop() {
		t.Fatal("pop at the top level must do nothing")
	}
	if m.push(0) {
		t.Fatal("a leaf is not a submenu")
	}
	if m.push(7) || m.push(-1) {
		t.Fatal("out of range push must fail")
	}
	if m.changed {
		t.Fatal("failed moves must not flag a change")
	}
	if !m.push(1) || !m.inSubmenu() || !m.changed || m.level()[0].ID != 5 {
		t.Fatalf("after push = %+v", m)
	}
	m.changed = false
	if !m.push(0) || m.level()[0].ID != 8 || len(m.path) != 2 {
		t.Fatalf("after second push = %+v", m)
	}
	if !m.pop() {
		t.Fatalf("first pop = %+v", m)
	}
	if !m.pop() || m.inSubmenu() || len(m.level()) != 2 || !m.changed {
		t.Fatalf("after pops = %+v", m)
	}
	m.choose(6)
	if m.chosen != 6 {
		t.Fatalf("chosen = %d", m.chosen)
	}
}

func TestMenuLevelKeysDiffer(t *testing.T) {
	m := sampleMenu()
	top := m.levelKey()
	m.push(1)
	if m.levelKey() == top {
		t.Fatal("levels must not share control keys")
	}
}

func TestMapKey(t *testing.T) {
	for _, c := range []struct {
		sym     uint32
		sub     bool
		want    keyAction
		comment string
	}{
		{keysymDown, false, keyNext, "Down walks forward"},
		{keysymUp, true, keyPrevious, "Up walks back"},
		{keysymEscape, false, keyClose, "Escape closes"},
		{keysymEscape, true, keyClose, "Escape closes from a submenu too"},
		{keysymLeft, true, keyBack, "Left leaves a submenu"},
		{keysymBackSpace, true, keyBack, "BackSpace leaves a submenu"},
		{keysymLeft, false, keyForward, "Left does nothing at the top"},
		{keysymBackSpace, false, keyForward, "BackSpace does nothing at the top"},
		{keysymTab, false, keyForward, "Tab goes to nefergui"},
		{0x20, false, keyForward, "Space goes to nefergui"},
		{0xff0d, true, keyForward, "Return goes to nefergui"},
	} {
		if got := mapKey(c.sym, c.sub); got != c.want {
			t.Errorf("%s: mapKey(%#x, %v) = %d, want %d", c.comment, c.sym, c.sub, got, c.want)
		}
	}
}

func TestTooltipDoesNotWaitForTheWarmUpButMenusDo(t *testing.T) {
	h := &Host{}
	if !h.waitWarm(KindTooltip) || !h.waitWarm(KindMenu) {
		t.Fatal("without a warm-up nothing waits")
	}
	h.warm = make(chan struct{})
	if h.waitWarm(KindTooltip) {
		t.Fatal("a tooltip must be skipped while the warm-up runs")
	}
	if h.WaitTimeout(10 * time.Millisecond) {
		t.Fatal("WaitTimeout reported a running warm-up as done")
	}
	got := make(chan bool)
	go func() { got <- h.waitWarm(KindMenu) }()
	select {
	case <-got:
		t.Fatal("a menu must wait for the warm-up")
	case <-time.After(30 * time.Millisecond):
	}
	close(h.warm)
	if !<-got {
		t.Fatal("a menu goes on once the warm-up ended")
	}
	if !h.waitWarm(KindTooltip) || !h.WaitTimeout(time.Second) {
		t.Fatal("after the warm-up a tooltip goes on")
	}
}

func TestShowsTooltip(t *testing.T) {
	m := &module.Module{}
	other := &module.Module{}
	c := module.Control{Type: module.ControlTooltip, Col: 1, Width: 2, Title: "T", Body: "B"}
	h := &Host{kind: KindTooltip, owner: m, tipKey: keyOf(c)}
	if !h.showsTooltip(m, c) {
		t.Fatal("the same tooltip of the same owner is shown")
	}
	for name, d := range map[string]module.Control{
		"title": {Col: 1, Width: 2, Title: "T2", Body: "B"}, "body": {Col: 1, Width: 2, Title: "T", Body: "B2"},
		"col": {Col: 2, Width: 2, Title: "T", Body: "B"}, "width": {Col: 1, Width: 3, Title: "T", Body: "B"},
	} {
		if h.showsTooltip(m, d) {
			t.Errorf("a tooltip with another %s counts as shown", name)
		}
	}
	if h.showsTooltip(other, c) {
		t.Error("another owner's tooltip counts as shown")
	}
	h.kind = KindMenu
	if h.showsTooltip(m, c) {
		t.Error("a menu counts as a tooltip")
	}
	if (*Host)(nil).ShowsTooltip(m, c) || (&Host{}).ShowsTooltip(m, c) {
		t.Error("no popup open must not match")
	}
}

func TestFitToScrollsAMenuThatCannotBeResized(t *testing.T) {
	m := newMenu(nil, 1)
	m.fitTo(200, 300) // the new level fits: no scrolling
	if m.scroll {
		t.Fatal("a level that fits must not scroll")
	}
	m.fitTo(400, 300)
	if !m.scroll || m.scrollH != 300-2*Inset {
		t.Fatalf("scroll %v height %d", m.scroll, m.scrollH)
	}
	m.fitTo(400, 1)
	if m.scrollH < 1 {
		t.Fatalf("scroll height %d", m.scrollH)
	}
}

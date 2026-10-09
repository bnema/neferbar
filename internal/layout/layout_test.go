package layout

import (
	"fmt"
	"log/slog"
	"testing"

	"github.com/bnema/neferbar/internal/glyph"
	"github.com/bnema/neferbar/internal/module"
	"github.com/bnema/neferbar/internal/racecheck"
)

var testPalette = [16][3]uint8{{1, 1, 1}, {255, 0, 0}, {0, 255, 0}, {255, 255, 0}, {0, 0, 255}}

func newTestLayout(cols int) (*Layout, []*module.Module) {
	wake := make(chan struct{}, 1)
	log := slog.New(slog.DiscardHandler)
	mods := []*module.Module{
		module.New("l", module.Left, "true", wake, log),
		module.New("c", module.Center, "true", wake, log),
		module.New("r", module.Right, "true", wake, log),
	}
	return New(cols, [3]uint8{200, 200, 200}, [3]uint8{10, 10, 10}, testPalette, mods), mods
}

func publish(m *module.Module, text string) { module.PublishForTest(m, []byte(text)) }

func rowText(l *Layout) string {
	row := l.Compose()
	rs := make([]rune, len(row))
	for i, c := range row {
		rs[i] = c.Rune
	}
	return string(rs)
}

func TestZones(t *testing.T) {
	l, m := newTestLayout(20)
	publish(m[0], "LEFT")
	publish(m[1], "mid")
	publish(m[2], "RIGHT")
	if !l.Update() {
		t.Fatal("Update reported no change")
	}
	got := rowText(l)
	want := "LEFT    mid  RIGHT"
	if len(want) < 20 {
		want += "   "[:20-len(want)]
	}
	if got[:4] != "LEFT" || got[15:] != "RIGHT" {
		t.Fatalf("row = %q", got)
	}
	if i := indexOf(got, "mid"); i < 4 || i > 15 {
		t.Fatalf("center misplaced in %q", got)
	}
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestOverlapCutsCenterFirst(t *testing.T) {
	l, m := newTestLayout(12)
	publish(m[0], "AAAAA")
	publish(m[1], "CCCCCCCC")
	publish(m[2], "ZZZZZ")
	l.Update()
	got := rowText(l)
	if got[:5] != "AAAAA" || got[7:] != "ZZZZZ" {
		t.Fatalf("sides must survive: %q", got)
	}
}

func TestColorsAndClip(t *testing.T) {
	l, m := newTestLayout(6)
	publish(m[0], "\x1b[31mR\x1b[0mx\x1b[1;38;2;1;2;3mverylongtext")
	l.Update()
	row := l.Compose()
	if row[0].Rune != 'R' || row[0].FG == l.fg {
		t.Fatalf("red cell: %+v", row[0])
	}
	if row[1].FG != l.fg {
		t.Fatalf("reset should restore default fg: %+v", row[1])
	}
	if row[2].Style&glyph.Bold == 0 || row[2].FG != [3]uint8{1, 2, 3} {
		t.Fatalf("bold truecolor cell: %+v", row[2])
	}
}

func TestNewFrameReplacesOld(t *testing.T) {
	l, m := newTestLayout(10)
	publish(m[0], "long text")
	l.Update()
	publish(m[0], "ab")
	l.Update()
	if got := rowText(l); got[:2] != "ab" || got[2] != ' ' {
		t.Fatalf("old text lingers: %q", got)
	}
}

// TestUpdateComposeAllocs: parsing a frame and composing the row must not
// allocate once the buffers are warm.
func TestUpdateComposeAllocs(t *testing.T) {
	if racecheck.Enabled {
		t.Skip("the race detector allocates")
	}
	l, m := newTestLayout(100)
	frames := make([]string, 8)
	for i := range frames {
		frames[i] = fmt.Sprintf("\x1b[48;2;%d;10;10m \x1b[0m clock %02d \x1b[1;32mok\x1b[0m \uf017", i*30, i)
	}
	n := 0
	step := func() {
		publish(m[2], frames[n%len(frames)])
		n++
		l.Update()
		l.Compose()
	}
	for i := 0; i < 20; i++ {
		step()
	}
	if got := testing.AllocsPerRun(500, step); got > 0 {
		t.Errorf("Update+Compose allocates %.1f objects per frame; want 0", got)
	}
}

func TestTextStyles(t *testing.T) {
	l, m := newTestLayout(10)
	publish(m[0], "\x1b[3mi\x1b[0m\x1b[4mu\x1b[0m\x1b[9ms\x1b[0m\x1b[1;3;4;9mA\x1b[0mn")
	l.Update()
	row := l.Compose()
	want := []glyph.Style{glyph.Italic, glyph.Underline, glyph.Strike, glyph.Bold | glyph.Italic | glyph.Underline | glyph.Strike, 0}
	for i, w := range want {
		if row[i].Style != w {
			t.Errorf("cell %d (%q): style %04b, want %04b", i, row[i].Rune, row[i].Style, w)
		}
	}
}

func TestAnsiColorsComeFromThePalette(t *testing.T) {
	l, m := newTestLayout(6)
	publish(m[0], "\x1b[31mr\x1b[44mb\x1b[0m\x1b[38;5;2mg")
	l.Update()
	row := l.Compose()
	if row[0].FG != testPalette[1] {
		t.Errorf("\\e[31m = %v, want palette[1] %v", row[0].FG, testPalette[1])
	}
	if row[1].BG != testPalette[4] {
		t.Errorf("\\e[44m background = %v, want palette[4] %v", row[1].BG, testPalette[4])
	}
	if row[2].FG != testPalette[2] {
		t.Errorf("\\e[38;5;2m = %v, want palette[2] %v", row[2].FG, testPalette[2])
	}
	pal := testPalette
	pal[1] = [3]uint8{9, 9, 9}
	l.SetColors(l.fg, l.bg, pal)
	if got := l.Compose()[0].FG; got != pal[1] {
		t.Errorf("after SetColors, \\e[31m = %v, want %v", got, pal[1])
	}
}

// A frame that stops inside an escape sequence must not blank the frames after it.
func TestBrokenEscapeDoesNotSwallowNextFrames(t *testing.T) {
	for name, broken := range map[string]string{
		"osc":  "\x1b]0;title",
		"csi":  "\x1b[38;2;1",
		"dcs":  "\x1bP1;2",
		"apc":  "\x1b_Gf=24",
		"esc":  "\x1b",
		"utf8": "ab\xe2\x82",
	} {
		l, m := newTestLayout(10)
		publish(m[0], broken)
		l.Update()
		publish(m[0], "hello")
		l.Update()
		if got := rowText(l); got[:5] != "hello" {
			t.Errorf("%s: row after the next frame = %q, want hello", name, got)
		}
	}
}

// The center zone sits on the middle of the bar, not on the middle of the
// space left between a wide left zone and a narrow right one.
func TestCenterIsOnTheMiddleOfTheBar(t *testing.T) {
	l, m := newTestLayout(40)
	publish(m[0], "LLLLLLLLLL") // 10 cells on the left
	publish(m[1], "cccc")       // 4 in the middle
	publish(m[2], "RRRR")       // 4 on the right
	l.Update()
	got := rowText(l)
	if i := indexOf(got, "cccc"); i != 18 {
		t.Fatalf("center starts at cell %d, want 18 (the bar is 40 cells wide): %q", i, got)
	}
}

// When the middle of the bar is taken, the center moves just enough to fit.
func TestCenterMovesOnlyAsFarAsNeeded(t *testing.T) {
	l, m := newTestLayout(40)
	publish(m[0], "LLLLLLLLLLLLLLLLLLLL") // 20 cells: reaches past the ideal start of 18
	publish(m[1], "cccc")
	publish(m[2], "RRRR")
	l.Update()
	got := rowText(l)
	if i := indexOf(got, "cccc"); i != 20 {
		t.Fatalf("center starts at cell %d, want 20 (right after the left zone): %q", i, got)
	}
	// And the other way round: a wide right zone pushes it left.
	l, m = newTestLayout(40)
	publish(m[0], "LL")
	publish(m[1], "cccc")
	publish(m[2], "RRRRRRRRRRRRRRRRRRRR") // 20 cells: the center must end by cell 20
	l.Update()
	got = rowText(l)
	if i := indexOf(got, "cccc"); i != 16 {
		t.Fatalf("center starts at cell %d, want 16 (ending where the right zone starts): %q", i, got)
	}
}

func spanOf(spans []Span, m *module.Module) (Span, bool) {
	for _, s := range spans {
		if s.M == m {
			return s, true
		}
	}
	return Span{}, false
}

func TestSpansPerZone(t *testing.T) {
	l, m := newTestLayout(20)
	publish(m[0], "LEFT")
	publish(m[1], "mid")
	publish(m[2], "RIGHT")
	l.Update()
	row := rowText(l)
	spans := l.Spans(nil)
	if len(spans) != 3 {
		t.Fatalf("spans = %+v, want 3", spans)
	}
	for i, want := range []string{"LEFT", "mid", "RIGHT"} {
		s, ok := spanOf(spans, m[i])
		if !ok || s.Width != len(want) || row[s.Start:s.Start+s.Width] != want {
			t.Errorf("span %d = %+v (ok %v) in %q, want %q", i, s, ok, row, want)
		}
	}
	// Spans come in module order.
	if spans[0].M != m[0] || spans[1].M != m[1] || spans[2].M != m[2] {
		t.Errorf("spans out of order: %+v", spans)
	}
}

func TestSpansTwoModulesInOneZone(t *testing.T) {
	wake := make(chan struct{}, 1)
	log := slog.New(slog.DiscardHandler)
	a := module.New("a", module.Left, "true", wake, log)
	b := module.New("b", module.Left, "true", wake, log)
	l := New(10, [3]uint8{1, 1, 1}, [3]uint8{2, 2, 2}, testPalette, []*module.Module{a, b})
	publish(a, "AA")
	publish(b, "BBB")
	l.Update()
	l.Compose()
	sa, _ := spanOf(l.Spans(nil), a)
	sb, _ := spanOf(l.Spans(nil), b)
	if sa != (Span{a, 0, 2}) || sb != (Span{b, 2, 3}) {
		t.Fatalf("spans = %+v %+v", sa, sb)
	}
	for col, want := range map[int]struct {
		m   *module.Module
		off int
		ok  bool
	}{0: {a, 0, true}, 1: {a, 1, true}, 2: {b, 0, true}, 4: {b, 2, true}, 5: {nil, 0, false}, -1: {nil, 0, false}, 99: {nil, 0, false}} {
		gm, off, ok := l.At(col)
		if gm != want.m || off != want.off || ok != want.ok {
			t.Errorf("At(%d) = %v, %d, %v; want %v, %d, %v", col, gm, off, ok, want.m, want.off, want.ok)
		}
	}
}

func TestSpansAreClipped(t *testing.T) {
	// The right zone wins over the left one: left is cut to the room left.
	l, m := newTestLayout(8)
	publish(m[0], "AAAAA")
	publish(m[2], "ZZZZZ")
	l.Update()
	l.Compose()
	spans := l.Spans(nil)
	if s, ok := spanOf(spans, m[0]); !ok || s != (Span{m[0], 0, 3}) {
		t.Errorf("left span = %+v, want {0,3}", s)
	}
	if s, ok := spanOf(spans, m[2]); !ok || s != (Span{m[2], 3, 5}) {
		t.Errorf("right span = %+v, want {3,5}", s)
	}
	if _, _, ok := l.At(3); !ok {
		t.Error("column 3 belongs to the right module")
	}
	if gm, off, _ := l.At(3); gm != m[2] || off != 0 {
		t.Errorf("At(3) = %v, %d", gm, off)
	}

	// The center is cut first, and may vanish entirely.
	l, m = newTestLayout(12)
	publish(m[0], "AAAAA")
	publish(m[1], "CCCCCCCC")
	publish(m[2], "ZZZZZ")
	l.Update()
	l.Compose()
	if s, ok := spanOf(l.Spans(nil), m[1]); !ok || s != (Span{m[1], 5, 2}) {
		t.Errorf("center span = %+v (ok %v), want {5,2}", s, ok)
	}
	l, m = newTestLayout(10)
	publish(m[0], "AAAAA")
	publish(m[1], "CC")
	publish(m[2], "ZZZZZ")
	l.Update()
	l.Compose()
	if _, ok := spanOf(l.Spans(nil), m[1]); ok {
		t.Error("a fully clipped module must have no span")
	}
	if _, _, ok := l.At(5); !ok {
		// column 5 is the first of the right zone
		t.Error("At(5) must find the right module")
	}
}

func TestSpansEmptyModuleAndRecompose(t *testing.T) {
	l, m := newTestLayout(10)
	publish(m[0], "")
	publish(m[2], "R")
	l.Update()
	l.Compose()
	if _, ok := spanOf(l.Spans(nil), m[0]); ok {
		t.Error("an empty module has no span")
	}
	if len(l.Spans(nil)) != 1 {
		t.Errorf("spans = %+v", l.Spans(nil))
	}
	// A module that empties out loses its span at the next Compose.
	publish(m[2], "")
	l.Update()
	l.Compose()
	if got := l.Spans(nil); len(got) != 0 {
		t.Errorf("spans after emptying = %+v", got)
	}
	if _, _, ok := l.At(9); ok {
		t.Error("At must not find an emptied module")
	}
}

func TestComposeAndSpansAllocs(t *testing.T) {
	if racecheck.Enabled {
		t.Skip("the race detector allocates")
	}
	l, m := newTestLayout(40)
	publish(m[0], "left")
	publish(m[1], "center")
	publish(m[2], "right")
	l.Update()
	if got := testing.AllocsPerRun(100, func() { l.Compose() }); got > 0 {
		t.Errorf("Compose allocates %.1f objects; want 0", got)
	}
	buf := make([]Span, 0, 8)
	if got := testing.AllocsPerRun(100, func() { buf = l.Spans(buf[:0]) }); got > 0 {
		t.Errorf("Spans allocates %.1f objects; want 0", got)
	}
	if got := testing.AllocsPerRun(100, func() { l.At(3) }); got > 0 {
		t.Errorf("At allocates %.1f objects; want 0", got)
	}
}

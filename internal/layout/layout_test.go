package layout

import (
	"fmt"
	"log/slog"
	"testing"

	"git.bnema.dev/bnema/neferbar/internal/glyph"
	"git.bnema.dev/bnema/neferbar/internal/module"
	"git.bnema.dev/bnema/neferbar/internal/racecheck"
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

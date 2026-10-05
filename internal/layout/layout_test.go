package layout

import (
	"fmt"
	"log/slog"
	"testing"

	"git.bnema.dev/bnema/neferbar/internal/module"
	"git.bnema.dev/bnema/neferbar/internal/racecheck"
)

func newTestLayout(cols int) (*Layout, []*module.Module) {
	wake := make(chan struct{}, 1)
	log := slog.New(slog.DiscardHandler)
	mods := []*module.Module{
		module.New("l", module.Left, "true", wake, log),
		module.New("c", module.Center, "true", wake, log),
		module.New("r", module.Right, "true", wake, log),
	}
	return New(cols, [3]uint8{200, 200, 200}, [3]uint8{10, 10, 10}, mods), mods
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
	if !row[2].Bold || row[2].FG != [3]uint8{1, 2, 3} {
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

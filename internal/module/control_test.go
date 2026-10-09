package module

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// newInteractive is newTest for a module that may send control lines.
func newInteractive() (*Module, chan struct{}) {
	m, wake := newTest()
	m.Interactive = true
	return m, wake
}

func TestParseControlValid(t *testing.T) {
	c, err := ParseControl([]byte(`{"type":"tooltip","col":2,"width":1,"title":"Steam","body":"line1\nline2"}`))
	if err != nil || c.Type != ControlTooltip || c.Col != 2 || c.Width != 1 || c.Title != "Steam" || c.Body != "line1\nline2" {
		t.Fatalf("tooltip: %+v, %v", c, err)
	}
	c, err = ParseControl([]byte(`{"type":"menu","col":0,"width":2,"click":9,"items":[
		{"id":1,"label":"Open"},{"id":2,"kind":"separator"},
		{"id":3,"label":"Mute","kind":"check","checked":true},
		{"id":4,"label":"Settings","items":[{"id":5,"label":"Advanced"}]},
		{"id":6,"label":"Quit","enabled":false}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Click != 9 || len(c.Items) != 5 {
		t.Fatalf("menu: %+v", c)
	}
	if !c.Items[0].Enabled || c.Items[0].Kind != KindNormal {
		t.Fatalf("defaults: %+v", c.Items[0])
	}
	if c.Items[1].Kind != KindSeparator || c.Items[2].Kind != KindCheck || !c.Items[2].Checked {
		t.Fatalf("kinds: %+v", c.Items)
	}
	if len(c.Items[3].Items) != 1 || c.Items[3].Items[0].ID != 5 || c.Items[4].Enabled {
		t.Fatalf("submenu/disabled: %+v", c.Items)
	}
	if c, err = ParseControl([]byte(`{"type":"close"}`)); err != nil || c.Type != ControlClose {
		t.Fatalf("close: %+v, %v", c, err)
	}
}

// nested builds a menu with depth levels of submenus.
func nested(depth int) string {
	s := `{"id":1,"label":"x"}`
	for i := 1; i < depth; i++ {
		s = fmt.Sprintf(`{"id":1,"label":"x","items":[%s]}`, s)
	}
	return `{"type":"menu","col":0,"width":1,"click":1,"items":[` + s + `]}`
}

func TestParseControlLimits(t *testing.T) {
	if _, err := ParseControl([]byte(nested(MaxMenuDepth))); err != nil {
		t.Fatalf("depth %d must pass: %v", MaxMenuDepth, err)
	}
	many := func(n int) string {
		return `{"type":"menu","col":0,"width":1,"click":1,"items":[` + strings.TrimSuffix(strings.Repeat(`{"id":1,"label":"a"},`, n), ",") + `]}`
	}
	if _, err := ParseControl([]byte(many(MaxMenuItems))); err != nil {
		t.Fatalf("%d items must pass: %v", MaxMenuItems, err)
	}
	for name, in := range map[string]string{
		"too deep":      nested(MaxMenuDepth + 1),
		"too many":      many(MaxMenuItems + 1),
		"oversize":      `{"type":"tooltip","col":0,"width":1,"title":"` + strings.Repeat("a", MaxControl) + `"}`,
		"unknown type":  `{"type":"explode"}`,
		"no type":       `{}`,
		"bad json":      `{"type":`,
		"not an object": `[1]`,
		"neg col":       `{"type":"tooltip","col":-1,"width":1,"title":"a"}`,
		"huge width":    `{"type":"tooltip","col":0,"width":99999999,"title":"a"}`,
		"zero width":    `{"type":"tooltip","col":0,"width":0,"title":"a"}`,
		"empty tip":     `{"type":"tooltip","col":0,"width":1}`,
		"bad kind":      `{"type":"menu","col":0,"width":1,"click":1,"items":[{"id":1,"label":"a","kind":"weird"}]}`,
		"neg id":        `{"type":"menu","col":0,"width":1,"click":1,"items":[{"id":-4,"label":"a"}]}`,
		"id overflow":   `{"type":"menu","col":0,"width":1,"click":1,"items":[{"id":4294967296,"label":"a"}]}`,
		"no items":      `{"type":"menu","col":0,"width":1,"click":1,"items":[]}`,
		"neg click":     `{"type":"menu","col":0,"width":1,"click":-1,"items":[{"id":1}]}`,
	} {
		if _, err := ParseControl([]byte(in)); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestParseControlCleansText(t *testing.T) {
	long := strings.Repeat("é", MaxLabel) // 2 bytes each
	c, err := ParseControl([]byte(`{"type":"menu","col":0,"width":1,"click":1,"items":[{"id":1,"label":"a\u001b[31mb\u0007\n\u202ec` + long + `"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	got := c.Items[0].Label
	if len(got) > MaxLabel || !strings.HasPrefix(got, "a[31mbc") || strings.ContainsAny(got, "\x1b\x07\n\u202e") {
		t.Fatalf("label = %q (%d bytes)", got, len(got))
	}
	if strings.ContainsRune(got, '\uFFFD') {
		t.Fatalf("cut inside a rune: %q", got)
	}
	c, err = ParseControl([]byte(`{"type":"tooltip","col":0,"width":1,"title":"T\u001b","body":"a\nb\u0000c"}`))
	if err != nil || c.Title != "T" || c.Body != "a\nbc" {
		t.Fatalf("tooltip text: %+v, %v", c, err)
	}
}

func TestControlLineBetweenFramesLeavesBothIntact(t *testing.T) {
	m, _ := newInteractive()
	m.readFrames(strings.NewReader("one\n\x1b]777;neferbar;{\"type\":\"close\"}\x07\ntwo\n"))
	got, changed, _ := m.Take(nil)
	if !changed || string(got) != "two" {
		t.Fatalf("frame = %q changed=%v, want two", got, changed)
	}
	b, ok := m.TakeControl()
	if !ok || string(b) != `{"type":"close"}` {
		t.Fatalf("control = %q %v", b, ok)
	}
	if _, ok = m.TakeControl(); ok {
		t.Fatal("one control line expected")
	}

	// The control line alone never becomes a frame.
	m, _ = newInteractive()
	m.readFrames(strings.NewReader("one\n"))
	m.Take(nil)
	m.readFrames(strings.NewReader("\x1b]777;neferbar;{\"type\":\"close\"}\x07\n"))
	if _, changed, _ = m.Take(nil); changed {
		t.Fatal("a control line must not be published as a frame")
	}
}

func TestUnterminatedControlIsAFrame(t *testing.T) {
	m, _ := newInteractive()
	m.readFrames(strings.NewReader("\x1b]777;neferbar;{bad\n"))
	if _, ok := m.TakeControl(); ok {
		t.Fatal("a line without BEL is not a control line")
	}
	got, changed, _ := m.Take(nil)
	if !changed || string(got) != "\x1b]777;neferbar;{bad" {
		t.Fatalf("frame = %q changed=%v", got, changed)
	}
}

func TestOversizeControlIsDropped(t *testing.T) {
	m, _ := newInteractive()
	m.readFrames(strings.NewReader("a\n\x1b]777;neferbar;" + strings.Repeat("x", MaxControl) + "\x07\nb\n"))
	if _, ok := m.TakeControl(); ok {
		t.Fatal("an oversize control line must be dropped")
	}
	if got, _, _ := m.Take(nil); string(got) != "b" {
		t.Fatalf("frame = %q, want b", got)
	}
}

func TestControlQueueDropsWhenFull(t *testing.T) {
	m, wake := newInteractive()
	var in strings.Builder
	for range 20 {
		in.WriteString("\x1b]777;neferbar;{\"type\":\"close\"}\x07\n")
	}
	m.readFrames(strings.NewReader(in.String()))
	n := 0
	for {
		if _, ok := m.TakeControl(); !ok {
			break
		}
		n++
	}
	if n != 8 || len(wake) != 1 {
		t.Fatalf("queued %d (want 8), wake %d", n, len(wake))
	}
}

func TestControlLinesOfANonInteractiveModuleAreDropped(t *testing.T) {
	m, wake := newTest()
	m.readFrames(strings.NewReader("one\n\x1b]777;neferbar;{\"type\":\"close\"}\x07\ntwo\n"))
	if _, ok := m.TakeControl(); ok {
		t.Fatal("a non-interactive module queued a control line")
	}
	if got, _, _ := m.Take(nil); string(got) != "two" {
		t.Fatalf("frame = %q, want two (the control line is still not a frame)", got)
	}
	if len(wake) != 1 { // from the frames only
		t.Fatalf("wake = %d", len(wake))
	}
}

func TestOversizeControlWarnsOncePerRun(t *testing.T) {
	var logs bytes.Buffer
	m, _ := newInteractive()
	m.log = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	big := "\x1b]777;neferbar;" + strings.Repeat("x", MaxControl) + "\x07\n"
	m.readFrames(strings.NewReader(big + big + big))
	warns := func() int { return strings.Count(logs.String(), "level=WARN") }
	if warns() != 1 || strings.Count(logs.String(), "level=DEBUG") != 2 {
		t.Fatalf("want 1 warning and 2 debug lines:\n%s", logs.String())
	}
	// A new run of the script warns again; runOnce resets the flag.
	m.overLogged = false
	m.readFrames(strings.NewReader(big))
	if warns() != 2 {
		t.Fatalf("a new run must warn again:\n%s", logs.String())
	}
}

func FuzzParseControl(f *testing.F) {
	for _, s := range []string{
		`{"type":"close"}`,
		`{"type":"tooltip","col":2,"width":1,"title":"Steam","body":"a\nb"}`,
		`{"type":"menu","col":0,"width":2,"click":9,"items":[{"id":1,"label":"Open"},{"id":2,"kind":"separator"},{"id":3,"label":"Mute","kind":"check","checked":true},{"id":4,"label":"S","items":[{"id":5,"label":"A"}]},{"id":6,"enabled":false}]}`,
		nested(MaxMenuDepth), nested(MaxMenuDepth + 1),
		`{"type":"menu","col":0,"width":1,"click":1,"items":[{"id":1,"label":"a\u001b[31m\u202e\ud800"}]}`,
		`{"type":"tooltip","col":-1,"width":99999999,"title":"x"}`,
		`{"type":`, `[]`, `null`, ``, `{"type":"menu","items":null}`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		c, err := ParseControl(b)
		if err != nil {
			return
		}
		// What is accepted respects every limit and carries no control rune.
		if c.Col < 0 || c.Col > maxColumn || c.Width < 0 || c.Width > maxColumn {
			t.Fatalf("column %d width %d", c.Col, c.Width)
		}
		bad := func(s string, max int) {
			if len(s) > max || !utf8.ValidString(s) || strings.ContainsFunc(s, func(r rune) bool { return unicode.IsControl(r) && r != '\n' }) {
				t.Fatalf("text %q", s)
			}
		}
		bad(c.Title, MaxLabel)
		bad(c.Body, MaxText)
		count := 0
		var walk func(items []MenuItem, depth int)
		walk = func(items []MenuItem, depth int) {
			if len(items) > 0 && depth > MaxMenuDepth {
				t.Fatalf("depth %d", depth)
			}
			for _, it := range items {
				count++
				bad(it.Label, MaxLabel)
				if strings.ContainsRune(it.Label, '\n') || it.ID < 0 {
					t.Fatalf("item %+v", it)
				}
				switch it.Kind {
				case KindNormal, KindSeparator, KindCheck, KindRadio:
				default:
					t.Fatalf("kind %q", it.Kind)
				}
				walk(it.Items, depth+1)
			}
		}
		walk(c.Items, 1)
		if count > MaxMenuItems {
			t.Fatalf("%d items", count)
		}
		if c.Type == ControlMenu && len(c.Items) == 0 {
			t.Fatal("an empty menu was accepted")
		}
	})
}

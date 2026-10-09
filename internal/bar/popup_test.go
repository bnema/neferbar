package bar

import (
	"bytes"
	"log/slog"
	"testing"
	"time"
)

func TestMenuAcceptsOnlyTheLatestFreshPressOfTheSameModule(t *testing.T) {
	r := newInputRig(t)
	var p pointerState
	p.button(r.lay, 1, btnRight, 77) // token 1, serial 77, module a
	now := p.press.at

	if _, ok := p.takeMenuPress(r.a, 2, now); ok {
		t.Error("a token that is not the latest press was accepted")
	}
	if _, ok := p.takeMenuPress(r.a, 0, now); ok {
		t.Error("token 0 was accepted")
	}
	if _, ok := p.takeMenuPress(r.b, 1, now); ok {
		t.Error("another module answered the press")
	}
	if _, ok := p.takeMenuPress(nil, 1, now); ok {
		t.Error("no module answered the press")
	}
	if _, ok := p.takeMenuPress(r.a, 1, now.Add(menuPressTTL+time.Millisecond)); ok {
		t.Error("a press older than the limit was accepted")
	}
	serial, ok := p.takeMenuPress(r.a, 1, now.Add(menuPressTTL))
	if !ok || serial != 77 {
		t.Fatalf("takeMenuPress = %d, %v; want 77, true", serial, ok)
	}
	if _, ok = p.takeMenuPress(r.a, 1, now); ok {
		t.Error("one press opened two menus")
	}

	// A newer press supersedes the old token.
	p.button(r.lay, 1, btnRight, 78)
	now = p.press.at
	if _, ok = p.takeMenuPress(r.a, 1, now); ok {
		t.Error("the superseded token was accepted")
	}
	if serial, ok = p.takeMenuPress(r.a, 2, now); !ok || serial != 78 {
		t.Errorf("latest press: %d, %v", serial, ok)
	}

	// A press without a serial cannot grab.
	p.button(r.lay, 1, btnRight, 0)
	if _, ok = p.takeMenuPress(r.a, 3, p.press.at); ok {
		t.Error("a press without a serial was accepted")
	}
}

func TestControlLinesAreCheckedBeforeAnythingOpens(t *testing.T) {
	r := newInputRig(t)
	var logs bytes.Buffer
	b := &Bar{lay: r.lay, log: slog.New(slog.NewTextHandler(&logs, nil))}

	// An invalid line is logged once per module and otherwise ignored.
	b.control(r.a, []byte(`{"type":"explode"}`))
	b.control(r.a, []byte(`{nope`))
	if n := bytes.Count(logs.Bytes(), []byte("invalid control line")); n != 1 {
		t.Fatalf("logged %d times, want once:\n%s", n, logs.String())
	}

	tooltip := []byte(`{"type":"tooltip","col":0,"width":1,"title":"t"}`)
	menu := []byte(`{"type":"menu","col":0,"width":1,"click":5,"items":[{"id":1,"label":"x"}]}`)
	// None of these may reach openPopup: the bar has no surface, so reaching
	// it would return quietly, hence the checks go through the pointer state
	// the rules read.
	b.control(r.b, tooltip) // not interactive
	b.control(r.a, tooltip) // not hovered
	b.control(r.a, menu)    // no press at all
	b.ptr.button(r.lay, 1, btnRight, 9)
	b.control(r.a, menu) // wrong token
	if b.ptr.press.used {
		t.Fatal("a refused menu consumed the press")
	}
	b.control(r.a, []byte(`{"type":"menu","col":0,"width":1,"click":1,"items":[{"id":1,"label":"x"}]}`))
	if !b.ptr.press.used {
		t.Fatal("the right token should have been accepted")
	}
}

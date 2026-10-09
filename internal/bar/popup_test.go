package bar

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bnema/neferbar/internal/module"
	"github.com/bnema/neferbar/internal/popup"
	"github.com/bnema/neferbar/internal/racecheck"
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

// ctl parses the JSON of a control line, as the bar does.
func ctl(t testing.TB, js string) module.Control {
	t.Helper()
	c, err := module.ParseControl([]byte(js))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// feed prints control lines on module m's stdout, as its script would.
func feed(m *module.Module, lines ...string) {
	var sb strings.Builder
	for _, l := range lines {
		sb.WriteString("\x1b]777;neferbar;" + l + "\x07\n")
	}
	m.ReadFramesForTest(strings.NewReader(sb.String()))
}

func TestControlLinesAreCheckedBeforeAnythingOpens(t *testing.T) {
	r := newInputRig(t)
	var logs bytes.Buffer
	b := &Bar{lay: r.lay, log: slog.New(slog.NewTextHandler(&logs, nil))}

	// An invalid line is logged once per module and otherwise ignored.
	feed(r.a, `{"type":"explode"}`, `{nope`)
	b.drainControl(r.a)
	if n := bytes.Count(logs.Bytes(), []byte("invalid control line")); n != 1 {
		t.Fatalf("logged %d times, want once:\n%s", n, logs.String())
	}

	tooltip := ctl(t, `{"type":"tooltip","col":0,"width":1,"title":"t"}`)
	menu := ctl(t, `{"type":"menu","col":0,"width":1,"click":5,"items":[{"id":1,"label":"x"}]}`)
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
	b.control(r.a, ctl(t, `{"type":"menu","col":0,"width":1,"click":1,"items":[{"id":1,"label":"x"}]}`))
	if !b.ptr.press.used {
		t.Fatal("the right token should have been accepted")
	}
}

func TestCoalesceKeepsMenusAndTheLastTooltipOrClose(t *testing.T) {
	tip := func(title string) module.Control {
		return ctl(t, `{"type":"tooltip","col":0,"width":1,"title":"`+title+`"}`)
	}
	closeC := ctl(t, `{"type":"close"}`)
	menu := ctl(t, `{"type":"menu","col":0,"width":1,"click":1,"items":[{"id":1,"label":"x"}]}`)
	types := func(cs []module.Control) string {
		var sb strings.Builder
		for _, c := range cs {
			sb.WriteString(c.Type[:1] + c.Title + " ")
		}
		return strings.TrimSpace(sb.String())
	}
	for name, c := range map[string]struct {
		in   []module.Control
		want string
	}{
		"none":               {nil, ""},
		"one":                {[]module.Control{tip("a")}, "ta"},
		"storm":              {[]module.Control{tip("a"), tip("b"), tip("c")}, "tc"},
		"tooltip then close": {[]module.Control{tip("a"), closeC}, "c"},
		"close then tooltip": {[]module.Control{closeC, tip("z")}, "tz"},
		"menus are kept":     {[]module.Control{tip("a"), menu, tip("b"), menu, closeC}, "m m c"},
		"only menus":         {[]module.Control{menu, menu}, "m m"},
	} {
		if got := types(coalesceControl(slices.Clone(c.in))); got != c.want {
			t.Errorf("%s: got %q, want %q", name, got, c.want)
		}
	}
}

func TestDrainAppliesOnlyTheLastTooltipOfAWakeUp(t *testing.T) {
	r := newInputRig(t)
	b := &Bar{lay: r.lay, log: slog.New(slog.DiscardHandler)}
	var seen []string
	b.ctlBuf = nil
	for i := range 50 {
		feed(r.a, fmt.Sprintf(`{"type":"tooltip","col":0,"width":1,"title":"t%d"}`, i))
	}
	// Count what survives by running the same steps drainControl does.
	raw, ok := r.a.TakeControl()
	for ; ok; raw, ok = r.a.TakeControl() {
		c, err := module.ParseControl(raw)
		if err != nil {
			t.Fatal(err)
		}
		b.ctlBuf = append(b.ctlBuf, c)
	}
	for _, c := range coalesceControl(b.ctlBuf) {
		seen = append(seen, c.Title)
	}
	// The queue holds 8 lines; of them one remains.
	if len(seen) != 1 || seen[0] != "t7" {
		t.Fatalf("applied %q, want just the last queued one", seen)
	}
}

func TestTooltipRateLimit(t *testing.T) {
	r := newInputRig(t)
	var l tooltipLimiter
	t0 := time.Now()
	if !l.allow(r.a, t0) {
		t.Fatal("the first tooltip must pass")
	}
	if l.allow(r.a, t0.Add(tooltipInterval-time.Millisecond)) {
		t.Fatal("a second tooltip within the interval passed")
	}
	if !l.allow(r.b, t0.Add(time.Millisecond)) {
		t.Fatal("another module shares the limit")
	}
	if !l.allow(r.a, t0.Add(tooltipInterval)) {
		t.Fatal("the limit must lift after the interval")
	}
	// A refused tooltip does not push the window.
	l.allow(r.a, t0.Add(tooltipInterval+10*time.Millisecond))
	if !l.allow(r.a, t0.Add(2*tooltipInterval)) {
		t.Fatal("refusals extended the window")
	}
	l.forget([]*module.Module{r.b})
	if _, ok := l.last[r.a]; ok || len(l.last) != 1 {
		t.Fatalf("forget kept %v", l.last)
	}
}

func TestStoppedModulesAreForgotten(t *testing.T) {
	r := newInputRig(t)
	b := &Bar{lay: r.lay, log: slog.New(slog.DiscardHandler)}
	b.badControl(r.a, errors.New("x"))
	b.badControl(r.b, errors.New("x"))
	b.tips.allow(r.a, time.Now())
	b.forgetModules([]*module.Module{r.b})
	if len(b.badCtl) != 1 || !b.badCtl[r.b] || len(b.tips.last) != 0 {
		t.Fatalf("badCtl %v, tips %v", b.badCtl, b.tips.last)
	}
}

func TestStepPopupAllocsWithNoPopup(t *testing.T) {
	if racecheck.Enabled {
		t.Skip("the race detector allocates")
	}
	r := newInputRig(t)
	b := &Bar{lay: r.lay, log: slog.New(slog.DiscardHandler), pop: &popup.Host{}, mods: []*module.Module{r.a, r.b}}
	b.stepPopup() // warm any lazily grown state
	if got := testing.AllocsPerRun(200, b.stepPopup); got > 0 {
		t.Errorf("stepPopup with no popup open allocates %.1f objects; want 0", got)
	}
}

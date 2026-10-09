package bar

import (
	"log/slog"
	"slices"
	"testing"

	"github.com/bnema/neferclient"

	"github.com/bnema/neferbar/internal/layout"
	"github.com/bnema/neferbar/internal/module"
	"github.com/bnema/neferbar/internal/racecheck"
)

// inputRig is a 30-column layout: "a" (interactive) at columns 0-3, "b" (not)
// at 4-7, both in the left zone.
type inputRig struct {
	lay  *layout.Layout
	a, b *module.Module
}

func newInputRig(t testing.TB) *inputRig {
	t.Helper()
	wake := make(chan struct{}, 1)
	log := slog.New(slog.DiscardHandler)
	a := module.New("a", module.Left, "true", wake, log)
	b := module.New("b", module.Left, "true", wake, log)
	a.Interactive = true
	lay := layout.New(30, [3]uint8{1, 1, 1}, [3]uint8{2, 2, 2}, [16][3]uint8{}, []*module.Module{a, b})
	module.PublishForTest(a, []byte("AAAA"))
	module.PublishForTest(b, []byte("BBBB"))
	lay.Update()
	lay.Compose()
	return &inputRig{lay: lay, a: a, b: b}
}

// drain returns the lines queued on m and empties its queue.
func drain(m *module.Module) []string {
	return module.DrainForTest(m)
}

func TestHoverAndLeave(t *testing.T) {
	r := newInputRig(t)
	var p pointerState
	p.motion(r.lay, 2)
	p.motion(r.lay, 2) // same column: nothing
	p.motion(r.lay, 3)
	p.motion(r.lay, 5) // over the non-interactive module: leaves a
	p.motion(r.lay, 6) // still nothing
	p.motion(r.lay, 1)
	p.leave()
	p.leave()
	want := []string{"hover 2", "hover 3", "leave", "hover 1", "leave"}
	if got := drain(r.a); !slices.Equal(got, want) {
		t.Fatalf("lines = %q, want %q", got, want)
	}
	if got := drain(r.b); len(got) != 0 {
		t.Fatalf("a non-interactive module got %q", got)
	}
}

func TestClickTokensAndButtons(t *testing.T) {
	r := newInputRig(t)
	var p pointerState
	p.button(r.lay, 1, btnLeft, 7)
	p.button(r.lay, 2, btnMiddle, 8)
	p.button(r.lay, 3, btnRight, 9)
	p.button(r.lay, 3, 0x113, 10) // another button: ignored
	p.button(r.lay, 5, btnLeft, 11)
	p.button(r.lay, 20, btnLeft, 12) // empty bar space
	want := []string{"click left 1 1", "click middle 2 2", "click right 3 3"}
	if got := drain(r.a); !slices.Equal(got, want) {
		t.Fatalf("lines = %q, want %q", got, want)
	}
	if p.press.token != 3 || p.press.serial != 9 || p.press.at.IsZero() {
		t.Fatalf("press = %+v", p.press)
	}
}

func TestScrollLines(t *testing.T) {
	r := newInputRig(t)
	var p pointerState
	p.axis(r.lay, 1, neferclient.AxisVertical, 120, 0)
	p.axis(r.lay, 1, neferclient.AxisVertical, -240, 0)
	p.axis(r.lay, 2, neferclient.AxisHorizontal, 120, 0)
	p.axis(r.lay, 2, neferclient.AxisHorizontal, -120, 0)
	p.axis(r.lay, 2, neferclient.AxisVertical, 0, 100) // continuous after a wheel: ignored
	p.axis(r.lay, 6, neferclient.AxisVertical, 120, 0) // not interactive
	want := []string{"scroll down 1 1", "scroll up 2 1", "scroll right 1 2", "scroll left 1 2"}
	if got := drain(r.a); !slices.Equal(got, want) {
		t.Fatalf("lines = %q, want %q", got, want)
	}
}

func TestWheelAccumulator(t *testing.T) {
	var a wheelAcc
	for _, c := range []struct {
		axis uint32
		v120 int32
		d    float64
		want int32
	}{
		{neferclient.AxisVertical, 120, 0, 1},
		{neferclient.AxisVertical, 240, 0, 2},
		{neferclient.AxisVertical, -120, 0, -1},
		{neferclient.AxisVertical, 60, 0, 0},
		{neferclient.AxisVertical, 60, 0, 1}, // two half notches
		{neferclient.AxisHorizontal, 120, 0, 1},
		{neferclient.AxisVertical, 0, 50, 0}, // continuous after a wheel is ignored
	} {
		if got := a.add(c.axis, c.v120, c.d); got != c.want {
			t.Errorf("add(%d, %d, %v) = %d, want %d", c.axis, c.v120, c.d, got, c.want)
		}
	}
	a.reset()
	for _, c := range []struct {
		d    float64
		want int32
	}{{15, 1}, {10, 0}, {5, 1}, {-30, -2}, {7, 0}} {
		if got := a.add(neferclient.AxisVertical, 0, c.d); got != c.want {
			t.Errorf("continuous %v = %d, want %d", c.d, got, c.want)
		}
	}
	if a.add(2, 120, 0) != 0 {
		t.Error("an unknown axis must be ignored")
	}
}

func TestInputRect(t *testing.T) {
	// Scale 1.6, 13 px cells: columns 2..4 are physical px 26..65, logical
	// 16.25..40.625, rounded outward.
	got := inputRect(2, 3, 13, 1.6, 24)
	want := neferclient.Rect{X: 16, Y: 0, Width: 25, Height: 24}
	if got != want {
		t.Fatalf("rect = %+v, want %+v", got, want)
	}
	if got = inputRect(0, 4, 10, 1, 20); got != (neferclient.Rect{X: 0, Y: 0, Width: 40, Height: 20}) {
		t.Fatalf("exact rect = %+v", got)
	}
	if got = inputRect(3, 2, 10, 2, 20); got != (neferclient.Rect{X: 15, Y: 0, Width: 10, Height: 20}) {
		t.Fatalf("scale 2 rect = %+v", got)
	}
}

func TestInputRegionUpdates(t *testing.T) {
	r := newInputRig(t)
	var in inputRegion
	key := inputKey{cellW: 10, scale: 1, height: 20}
	rects, changed := in.update(r.lay, key)
	want := []neferclient.Rect{{X: 0, Y: 0, Width: 40, Height: 20}}
	if !changed || !slices.Equal(rects, want) {
		t.Fatalf("first update = %+v, %v; want %+v", rects, changed, want)
	}
	if _, changed = in.update(r.lay, key); changed {
		t.Fatal("same spans must not change the region")
	}
	if _, changed = in.update(r.lay, inputKey{cellW: 10, scale: 2, height: 20}); !changed {
		t.Fatal("a scale change must change the region")
	}
	in.reset()
	if _, changed = in.update(r.lay, key); !changed {
		t.Fatal("a new surface needs the region again")
	}
	// No interactive module left: back to an empty, non-nil region.
	r.a.Interactive = false
	rects, changed = in.update(r.lay, key)
	if !changed || rects == nil || len(rects) != 0 {
		t.Fatalf("empty update = %#v, %v; want non-nil empty", rects, changed)
	}
	if _, changed = in.update(r.lay, key); changed {
		t.Fatal("staying empty must not change the region")
	}
}

func TestPointerAllocs(t *testing.T) {
	if racecheck.Enabled {
		t.Skip("the race detector allocates")
	}
	r := newInputRig(t)
	var p pointerState
	cols := [...]int{1, 2, 3, 5, 9}
	i := 0
	if got := testing.AllocsPerRun(200, func() {
		p.motion(r.lay, cols[i%len(cols)])
		i++
	}); got > 0 {
		t.Errorf("motion allocates %.1f objects; want 0", got)
	}
	if got := testing.AllocsPerRun(200, func() {
		p.button(r.lay, 1, btnLeft, 1)
		p.axis(r.lay, 1, neferclient.AxisVertical, 120, 0)
		p.leave()
	}); got > 0 {
		t.Errorf("click/scroll/leave allocate %.1f objects; want 0", got)
	}
	var in inputRegion
	key := inputKey{cellW: 10, scale: 1, height: 20}
	in.update(r.lay, key)
	if got := testing.AllocsPerRun(200, func() { in.update(r.lay, key) }); got > 0 {
		t.Errorf("an unchanged input region allocates %.1f objects; want 0", got)
	}
}

func TestRecreatingTheSurfaceResetsPointerState(t *testing.T) {
	r := newInputRig(t)
	b := &Bar{lay: r.lay}
	b.ptr.motion(r.lay, 2)
	b.ptr.wheel.add(neferclient.AxisVertical, 120, 0)
	b.input.update(r.lay, inputKey{cellW: 10, scale: 1, height: 20})
	drain(r.a)

	b.resetInput()

	if got := drain(r.a); !slices.Equal(got, []string{"leave"}) {
		t.Fatalf("lines = %q, want one leave", got)
	}
	if b.ptr.hover != nil || b.ptr.wheel != (wheelAcc{}) {
		t.Fatalf("pointer state kept: hover %v, wheel %+v", b.ptr.hover, b.ptr.wheel)
	}
	if _, changed := b.input.update(r.lay, inputKey{cellW: 10, scale: 1, height: 20}); !changed {
		t.Fatal("the new surface needs its input region again")
	}
}

// A failed SetInputRegion resets the tracking, so the next draw tries again.
func TestFailedInputRegionIsRetried(t *testing.T) {
	r := newInputRig(t)
	var in inputRegion
	key := inputKey{cellW: 10, scale: 1, height: 20}
	if _, changed := in.update(r.lay, key); !changed {
		t.Fatal("first update must change")
	}
	in.reset() // what syncInputRegion does when the request fails
	if _, changed := in.update(r.lay, key); !changed {
		t.Fatal("after a failure the same region must be sent again")
	}
}

func TestAxisStopClearsTheTouchpadRemainder(t *testing.T) {
	r := newInputRig(t)
	b := &Bar{lay: r.lay, sid: 1}
	axis := func(kind neferclient.PointerKind, ax uint32, dy float64) {
		b.handlePointer(&neferclient.PointerEvent{Surface: 1, Kind: kind, X: 1, Axis: ax, DY: dy}, 1, 10)
	}
	axis(neferclient.PointerAxis, neferclient.AxisVertical, 10) // 10 of 15 px: no step yet
	axis(neferclient.PointerAxisStop, neferclient.AxisVertical, 0)
	axis(neferclient.PointerAxis, neferclient.AxisVertical, 10) // would complete a step if 10 were kept
	if got := drain(r.a); len(got) != 0 {
		t.Fatalf("lines = %q; the remainder crossed a gesture", got)
	}
	axis(neferclient.PointerAxis, neferclient.AxisVertical, 10)
	if got := drain(r.a); !slices.Equal(got, []string{"scroll down 1 0"}) {
		t.Fatalf("lines = %q, want one step", got)
	}
	// Stopping the other axis leaves this one alone.
	axis(neferclient.PointerAxis, neferclient.AxisVertical, 10)
	axis(neferclient.PointerAxisStop, neferclient.AxisHorizontal, 0)
	axis(neferclient.PointerAxis, neferclient.AxisVertical, 5)
	if got := drain(r.a); !slices.Equal(got, []string{"scroll down 1 0"}) {
		t.Fatalf("lines = %q; the other axis's stop cleared this one", got)
	}
	var a wheelAcc
	a.stop(7) // unknown axis: no panic
}

func TestRestartedScriptGetsNoLeaveAndAHoverAgain(t *testing.T) {
	r := newInputRig(t)
	var p pointerState
	p.motion(r.lay, 1)
	if got := drain(r.a); !slices.Equal(got, []string{"hover 1"}) {
		t.Fatalf("lines = %q", got)
	}
	// The script restarts: the new process knows nothing of the hover.
	module.SetGenForTest(r.a, 7)
	p.motion(r.lay, 5) // moves off the module: the new process gets no leave
	if got := drain(r.a); len(got) != 0 {
		t.Fatalf("lines = %q, want no leave for the new process", got)
	}
	// The same move onto the module again sends it a hover.
	p.motion(r.lay, 1)
	module.SetGenForTest(r.a, 8)
	p.motion(r.lay, 1) // still over it, but the script restarted: hover again
	if got := drain(r.a); !slices.Equal(got, []string{"hover 1", "hover 1"}) {
		t.Fatalf("lines = %q, want a fresh hover after the restart", got)
	}

	// leave after a restart sends nothing to the new process.
	p.motion(r.lay, 2)
	drain(r.a)
	module.SetGenForTest(r.a, 9)
	p.leave()
	if got := drain(r.a); len(got) != 0 {
		t.Fatalf("lines = %q, want none", got)
	}
	if p.hover != nil {
		t.Fatal("hover not cleared")
	}

	// Without a restart nothing changes.
	p.motion(r.lay, 2)
	p.motion(r.lay, 6)
	if got := drain(r.a); !slices.Equal(got, []string{"hover 2", "leave"}) {
		t.Fatalf("lines = %q", got)
	}
	if got := testing.AllocsPerRun(100, func() { p.motion(r.lay, 1); p.motion(r.lay, 6) }); got > 0 && !racecheck.Enabled {
		t.Errorf("motion allocates %.1f", got)
	}
}

func TestCellColumnAtFractionalScale(t *testing.T) {
	// Scale 1.6, 13 px cells: a cell is 8.125 logical px wide.
	for _, c := range []struct {
		x    float64
		want int
	}{{0, 0}, {8.1, 0}, {8.125, 1}, {16.24, 1}, {16.25, 2}, {40.6, 4}, {40.625, 5}, {-0.1, -1}} {
		if got := cellColumn(c.x, 1.6, 13); got != c.want {
			t.Errorf("cellColumn(%v) = %d, want %d", c.x, got, c.want)
		}
	}
}

func TestPointerFiltersSurfaceAndUsesFractionalScale(t *testing.T) {
	r := newInputRig(t) // a: columns 0-3, b: 4-7
	b := &Bar{lay: r.lay, sid: 5}
	// Another surface (a popup): ignored before the bar's size is even read.
	b.Pointer(&neferclient.PointerEvent{Surface: 9, Kind: neferclient.PointerMotion, X: 1})
	if got := drain(r.a); len(got) != 0 || b.ptr.hover != nil {
		t.Fatalf("a foreign surface moved the hover: %q", got)
	}
	// Logical x 30 at scale 1.6 and 13 px cells is column 3 (physical px 48).
	b.handlePointer(&neferclient.PointerEvent{Surface: 5, Kind: neferclient.PointerMotion, X: 30}, 1.6, 13)
	// Logical x 33 is column 4: the non-interactive module, so a leave.
	b.handlePointer(&neferclient.PointerEvent{Surface: 5, Kind: neferclient.PointerMotion, X: 33}, 1.6, 13)
	b.handlePointer(&neferclient.PointerEvent{Surface: 5, Kind: neferclient.PointerMotion, X: 8.2}, 1.6, 13)
	b.handlePointer(&neferclient.PointerEvent{Surface: 5, Kind: neferclient.PointerButton, Button: btnRight, Pressed: true, X: 8.2}, 1.6, 13)
	b.handlePointer(&neferclient.PointerEvent{Surface: 5, Kind: neferclient.PointerButton, Button: btnRight, Pressed: false, X: 8.2}, 1.6, 13)
	want := []string{"hover 3", "leave", "hover 1", "click right 1 1"}
	if got := drain(r.a); !slices.Equal(got, want) {
		t.Fatalf("lines = %q, want %q", got, want)
	}
}

package bar

import (
	"math"
	"slices"
	"strconv"
	"time"

	"github.com/bnema/neferclient"

	"github.com/bnema/neferbar/internal/layout"
	"github.com/bnema/neferbar/internal/module"
	"github.com/bnema/neferbar/internal/popup"
)

// Evdev button codes.
const (
	btnLeft   = 0x110
	btnRight  = 0x111
	btnMiddle = 0x112
)

// pxPerStep is how far a continuous scroll (touchpad) moves for one step.
const pxPerStep = 15

// press is the latest button press sent to a module.
type press struct {
	m      *module.Module // the module the press was sent to
	token  uint32         // the number the module echoes to refer to this press
	serial uint32         // the pointer serial of the press, for popup grabs
	at     time.Time
	used   bool // a menu already answered this press
}

// menuPressTTL is how long after a press a module may still answer it with a
// menu.
const menuPressTTL = 5 * time.Second

// takeMenuPress accepts a menu that module m opens in answer to the press
// numbered token: it must be the latest press, sent to m, not older than
// menuPressTTL, and not answered yet. The serial of the press is what the
// popup grabs with. A press answers one menu.
func (p *pointerState) takeMenuPress(m *module.Module, token uint32, now time.Time) (serial uint32, ok bool) {
	pr := &p.press
	if m == nil || pr.m != m || token == 0 || pr.token != token || pr.used || pr.serial == 0 || now.Sub(pr.at) > menuPressTTL {
		return 0, false
	}
	pr.used = true
	return pr.serial, true
}

// pointerState turns pointer events over the bar into the lines of the module
// protocol. It owns no Wayland object: the bar loop feeds it columns.
type pointerState struct {
	hover    *module.Module // the interactive module under the pointer
	hoverOff int            // its cell column
	hoverGen uint64         // hover.Gen() when the hover line was sent
	token    uint32         // last click token
	press    press
	wheel    wheelAcc
	buf      [64]byte // the line being built
}

// send hands a line built in p.buf to m. A full queue drops the line: the
// script is not reading and pointer events are not worth waiting for.
func (p *pointerState) send(m *module.Module, line []byte) { m.Send(line) }

// line starts a line in p.buf.
func (p *pointerState) line(verb string) []byte { return append(p.buf[:0], verb...) }

func appendInt(b []byte, v int) []byte {
	b = append(b, ' ')
	return strconv.AppendInt(b, int64(v), 10)
}

// forgetRestarted drops the hover of a module whose script restarted: the new
// process was never told about it, so it gets no leave, and the next motion
// sends it a hover.
func (p *pointerState) forgetRestarted() {
	if p.hover != nil && p.hover.Gen() != p.hoverGen {
		p.hover, p.hoverOff = nil, 0
	}
}

// motion handles Enter and Motion at cell column col of the bar row.
func (p *pointerState) motion(lay *layout.Layout, col int) {
	p.forgetRestarted()
	m, off := interactiveAt(lay, col)
	if m == p.hover && (m == nil || off == p.hoverOff) {
		return
	}
	if p.hover != nil && m != p.hover {
		p.send(p.hover, p.line("leave"))
	}
	p.hover, p.hoverOff = m, off
	if m != nil {
		p.hoverGen = m.Gen()
		p.send(m, appendInt(p.line("hover"), off))
	}
}

// leave handles the pointer leaving the bar.
func (p *pointerState) leave() {
	p.forgetRestarted()
	if p.hover != nil {
		p.send(p.hover, p.line("leave"))
	}
	p.hover, p.hoverOff = nil, 0
	p.wheel.reset()
}

// button handles a button press at column col. serial is the press serial.
func (p *pointerState) button(lay *layout.Layout, col int, code uint32, serial uint32) {
	var name string
	switch code {
	case btnLeft:
		name = "left"
	case btnMiddle:
		name = "middle"
	case btnRight:
		name = "right"
	default:
		return
	}
	m, off := interactiveAt(lay, col)
	if m == nil {
		return
	}
	p.token++
	p.press = press{m: m, token: p.token, serial: serial, at: time.Now()}
	b := append(p.line("click "), name...)
	b = appendInt(b, off)
	b = append(b, ' ')
	b = strconv.AppendUint(b, uint64(p.token), 10)
	p.send(m, b)
}

// axis handles one scroll event at column col.
func (p *pointerState) axis(lay *layout.Layout, col int, axis uint32, value120 int32, delta float64) {
	steps := p.wheel.add(axis, value120, delta)
	if steps == 0 {
		return
	}
	m, off := interactiveAt(lay, col)
	if m == nil {
		return
	}
	var dir string
	switch {
	case axis == neferclient.AxisVertical && steps > 0:
		dir = "down"
	case axis == neferclient.AxisVertical:
		dir = "up"
	case steps > 0:
		dir = "right"
	default:
		dir = "left"
	}
	if steps < 0 {
		steps = -steps
	}
	b := append(p.line("scroll "), dir...)
	b = appendInt(b, int(steps))
	b = appendInt(b, off)
	p.send(m, b)
}

// interactiveAt is the interactive module at column col, and the column inside it.
func interactiveAt(lay *layout.Layout, col int) (*module.Module, int) {
	m, off, ok := lay.At(col)
	if !ok || !m.Interactive {
		return nil, 0
	}
	return m, off
}

// wheelAcc counts scroll steps. A wheel reports notches in 1/120 units; a
// touchpad reports pixels. A compositor may send both for a wheel, so once a
// 1/120 value was seen the pixels are ignored until the pointer leaves.
type wheelAcc struct {
	wheel bool
	v120  [2]int32
	px    [2]float64
}

func (a *wheelAcc) reset() { *a = wheelAcc{} }

// stop ends a touchpad scroll on axis: the pixels left over do not carry into
// the next gesture.
func (a *wheelAcc) stop(axis uint32) {
	if axis <= 1 {
		a.px[axis] = 0
	}
}

// cellColumn is the cell column under logical x on a surface of the given
// scale, for cells cellW physical pixels wide.
func cellColumn(x, scale float64, cellW int) int {
	return int(math.Floor(x * scale / float64(cellW)))
}

// add accumulates one event and returns the whole steps it completed:
// positive for down or right, negative for up or left.
func (a *wheelAcc) add(axis uint32, value120 int32, delta float64) int32 {
	if axis > 1 {
		return 0
	}
	if value120 != 0 {
		a.wheel = true
		a.v120[axis] += value120
		steps := a.v120[axis] / 120
		a.v120[axis] -= steps * 120
		return steps
	}
	if a.wheel {
		return 0
	}
	a.px[axis] += delta
	steps := int32(a.px[axis] / pxPerStep)
	a.px[axis] -= float64(steps) * pxPerStep
	return steps
}

// inputRect is the logical-pixel rectangle of a span of cells: rounded
// outward, full surface height.
func inputRect(start, width, cellW int, scale float64, height int32) neferclient.Rect {
	return popup.CellRect(start, width, cellW, scale, height)
}

// inputKey is everything but the spans that the input rectangles depend on.
type inputKey struct {
	cellW  int
	scale  float64
	height int32
}

// inputRegion tracks the spans the compositor was last told about.
type inputRegion struct {
	spans, last []layout.Span
	rects       []neferclient.Rect
	key         inputKey
}

// reset forgets what the compositor knows: a new surface starts click-through.
func (r *inputRegion) reset() {
	r.last = r.last[:0]
	r.key = inputKey{}
}

// update collects the spans of the interactive modules and reports whether the
// input rectangles differ from the last ones; if so rects holds the new ones
// (non-nil, possibly empty). It allocates only while the slices grow.
func (r *inputRegion) update(lay *layout.Layout, key inputKey) (rects []neferclient.Rect, changed bool) {
	r.spans = lay.Spans(r.spans[:0])
	r.spans = slices.DeleteFunc(r.spans, func(s layout.Span) bool { return !s.M.Interactive })
	if key == r.key && slices.Equal(r.spans, r.last) {
		return nil, false
	}
	if len(r.spans) == 0 && len(r.last) == 0 {
		r.key = key
		return nil, false
	}
	r.rects = r.rects[:0]
	for _, s := range r.spans {
		r.rects = append(r.rects, inputRect(s.Start, s.Width, key.cellW, key.scale, key.height))
	}
	if r.rects == nil {
		r.rects = []neferclient.Rect{}
	}
	r.last = append(r.last[:0], r.spans...)
	r.key = key
	return r.rects, true
}

package popup

import (
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"time"

	"github.com/bnema/neferclient"
	"github.com/bnema/nefergui"

	"github.com/bnema/neferbar/internal/module"
)

// FDBase is added to the id of a popup renderer's buffers when their release
// descriptors are watched: the bar's own slots use small ids, and
// Conn.WatchFD reports the same id for both through Handler.FDReady.
const FDBase = 1 << 48

// Kind is what a popup shows.
type Kind uint8

const (
	KindTooltip Kind = iota + 1
	KindMenu
)

// guiMods are the modifier bits nefergui knows; neferclient uses the same order.
const guiMods = neferclient.ModShift | neferclient.ModCtrl | neferclient.ModAlt | neferclient.ModSuper |
	neferclient.ModCapsLock | neferclient.ModNumLock

// Request asks the host to open a popup.
type Request struct {
	Kind  Kind
	Owner *module.Module
	// Ctrl is the control line: its title and body for a tooltip, its items
	// and token for a menu.
	Ctrl module.Control
	// Anchor is the part of Parent the popup attaches to, in logical pixels.
	Anchor    neferclient.Rect
	Parent    *neferclient.Surface
	BottomBar bool
	// Feedback gives the GPU and the formats of the renderer.
	Feedback *neferclient.Feedback
	// Serial is the pointer serial of the press that opened a menu; the menu
	// grabs the keyboard and the pointer with it.
	Serial uint32
}

// watch is a release descriptor being watched.
type watch struct {
	fd     int
	buffer uint64
}

// Host owns at most one open popup. Everything runs on the bar's loop
// goroutine, like the Handler methods that feed it.
type Host struct {
	conn *neferclient.Conn
	seat *neferclient.Seat
	log  *slog.Logger

	styles string // path of the stylesheet; "" disables popups

	warm chan struct{} // closed when the warm-up renderer is gone; nil before Warm

	surf *neferclient.Surface
	sid  neferclient.SurfaceID
	r    *nefergui.Renderer
	out  nefergui.Output

	render func(*nefergui.Output) (bool, error) // Render with the model's view
	tip    *tooltipModel
	menu   *menuModel

	kind      Kind
	tipCtl    module.Control // what the open tooltip shows
	owner     *module.Module
	ownerGen  uint64
	anchor    neferclient.Rect
	bottomBar bool

	watched     []watch
	damage      []neferclient.Rect
	configured  bool
	canPresent  bool
	haveAcquire bool
	inside      bool // the pointer is over the popup
	cursor      neferclient.CursorShape
}

// NewHost makes a host for conn.
func NewHost(conn *neferclient.Conn, log *slog.Logger) *Host {
	return &Host{conn: conn, seat: conn.Seat(), log: log, cursor: neferclient.CursorDefault}
}

// SetStyles sets the stylesheet path (see WriteCSS). An empty path disables
// popups. An open popup keeps the sheet it was built with.
func (h *Host) SetStyles(path string) { h.styles = path }

func formats(fb *neferclient.Feedback) []nefergui.Format {
	out := make([]nefergui.Format, len(fb.Formats))
	for i, f := range fb.Formats {
		out[i] = nefergui.Format{FourCC: f.FourCC, Modifier: f.Modifier}
	}
	return out
}

// Warm loads the GPU libraries on another goroutine so the first popup opens
// quickly: the first renderer of a process takes about half a second, later
// ones a fraction of that. It does nothing when called again.
func (h *Host) Warm(fb *neferclient.Feedback) {
	if h.warm != nil || fb == nil {
		return
	}
	h.warm = make(chan struct{})
	cfg := nefergui.RendererConfig{MainDevice: fb.MainDevice, Formats: formats(fb)}
	done := h.warm
	go func() {
		defer close(done)
		r, err := nefergui.NewRenderer(cfg)
		if err != nil {
			h.log.Debug("popup warm-up failed", "err", err)
			return
		}
		_ = r.Close()
	}()
}

// WaitTimeout waits up to d for the warm-up renderer to be gone and reports
// whether it is. The bar calls it before it tears down its own GPU state, and
// goes on when the GPU libraries are stuck.
func (h *Host) WaitTimeout(d time.Duration) bool {
	if h == nil || h.warm == nil {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-h.warm:
		return true
	case <-t.C:
		return false
	}
}

// waitWarm decides whether Open of a popup of the given kind goes on while the
// warm-up renderer may still be loading the GPU libraries. A tooltip does not
// wait: it is skipped, since a tooltip that shows up half a second late is
// worse than none (false). A menu was asked for with a click and waits (true).
func (h *Host) waitWarm(kind Kind) (proceed bool) {
	if h.warm == nil {
		return true
	}
	select {
	case <-h.warm:
		return true
	default:
	}
	if kind == KindTooltip {
		return false
	}
	<-h.warm // two renderers must not start at once
	return true
}

// sameTooltip reports whether two tooltip controls show the same thing:
// showing the same one again is pointless.
func sameTooltip(a, b module.Control) bool {
	return a.Title == b.Title && a.Body == b.Body && a.Col == b.Col && a.Width == b.Width &&
		slices.EqualFunc(a.Rows, b.Rows, slices.Equal[[]string])
}

// ShowsTooltip reports whether the open popup is a tooltip of owner that shows
// exactly what c says.
func (h *Host) ShowsTooltip(owner *module.Module, c module.Control) bool {
	return h != nil && h.r != nil && h.showsTooltip(owner, c)
}

func (h *Host) showsTooltip(owner *module.Module, c module.Control) bool {
	return h.kind == KindTooltip && h.owner == owner && sameTooltip(h.tipCtl, c)
}

// Active reports whether a popup is open.
func (h *Host) Active() bool { return h != nil && h.r != nil }

// Kind is the kind of the open popup, 0 when none.
func (h *Host) Kind() Kind {
	if h == nil || h.r == nil {
		return 0
	}
	return h.kind
}

// Owner is the module the open popup belongs to, nil when none.
func (h *Host) Owner() *module.Module {
	if h == nil || h.r == nil {
		return nil
	}
	return h.owner
}

// OwnerRestarted reports whether the script behind the popup started again
// since the popup opened.
func (h *Host) OwnerRestarted() bool {
	return h.owner != nil && h.owner.Gen() != h.ownerGen
}

// Is reports whether id is the open popup's surface.
func (h *Host) Is(id neferclient.SurfaceID) bool { return h != nil && h.r != nil && id == h.sid }

// Wake is signaled when the popup renderer needs a Render from another
// goroutine; nil when no popup is open.
func (h *Host) Wake() <-chan struct{} {
	if h == nil || h.r == nil {
		return nil
	}
	return h.r.Wake()
}

// Pending reports a built frame waiting for the GPU: call Draw again soon.
func (h *Host) Pending() bool { return h != nil && h.r != nil && h.r.Pending() }

// measure sizes the model of the open popup.
func (h *Host) measure(r *nefergui.Renderer) (w, ht int32, scroll bool, err error) {
	var fw, fh float64
	switch h.kind {
	case KindTooltip:
		fw, fh, err = r.Measure(h.tip, tooltipView, MaxTooltipWidth)
	default:
		h.menu.scroll = false
		fw, fh, err = r.Measure(h.menu, menuView, MaxMenuWidth)
		if err == nil && fh > MaxHeight {
			h.menu.scroll, h.menu.scrollH = true, menuContentHeight
			fw, fh, err = r.Measure(h.menu, menuView, MaxMenuWidth)
		}
	}
	if err != nil {
		return 0, 0, false, err
	}
	w, ht, scroll = Clamp(fw, fh)
	return w, ht, scroll, nil
}

// Open shows a popup, replacing any open one. It can take a while the first
// time (the GPU libraries load) and allocates freely: popups are rare.
func (h *Host) Open(req Request) error {
	h.Close()
	switch {
	case h.styles == "":
		return errors.New("popup: no stylesheet")
	case req.Parent == nil || req.Feedback == nil || req.Owner == nil:
		return errors.New("popup: incomplete request")
	case req.Kind != KindTooltip && req.Kind != KindMenu:
		return fmt.Errorf("popup: unknown kind %d", req.Kind)
	}
	if !h.waitWarm(req.Kind) {
		return nil // the GPU libraries are still loading: skip this tooltip
	}
	r, err := nefergui.NewRenderer(nefergui.RendererConfig{MainDevice: req.Feedback.MainDevice, Formats: formats(req.Feedback), Styles: h.styles})
	if err != nil {
		return fmt.Errorf("popup: %w", err)
	}
	h.r, h.kind = r, req.Kind
	h.tipCtl = req.Ctrl
	switch req.Kind {
	case KindTooltip:
		h.tip = newTooltip(req.Ctrl.Title, req.Ctrl.Body, req.Ctrl.Rows)
		h.render = func(out *nefergui.Output) (bool, error) { return r.Render(out, h.tip, tooltipView) }
	default:
		h.menu = newMenu(req.Ctrl.Items, req.Ctrl.Click)
		h.render = func(out *nefergui.Output) (bool, error) { return r.Render(out, h.menu, menuView) }
	}
	w, ht, _, err := h.measure(r)
	if err != nil {
		h.reset()
		_ = r.Close()
		return fmt.Errorf("popup: %w", err)
	}
	h.anchor, h.bottomBar = req.Anchor, req.BottomBar
	h.owner, h.ownerGen = req.Owner, req.Owner.Gen()
	grab := req.Kind == KindMenu
	surf, err := h.conn.NewPopup(neferclient.PopupConfig{
		Parent: req.Parent, Place: Placement(req.Anchor, req.BottomBar, w, ht), Grab: grab, Serial: req.Serial,
	})
	if err != nil {
		h.reset()
		_ = r.Close()
		return fmt.Errorf("popup: %w", err)
	}
	h.surf, h.sid = surf, surf.ID()
	if req.Kind == KindTooltip {
		// A tooltip is never clicked: the pointer goes through it.
		if err = surf.SetInputRegion([]neferclient.Rect{}); err != nil {
			h.Close()
			return fmt.Errorf("popup: %w", err)
		}
	}
	return nil
}

// reset forgets everything about the open popup; the caller has closed its
// renderer and surface.
func (h *Host) reset() {
	*h = Host{conn: h.conn, seat: h.seat, log: h.log, styles: h.styles, warm: h.warm, cursor: neferclient.CursorDefault}
}

// Close dismisses the open popup. A menu that was not used tells its script
// ("menu-closed"). It does nothing when no popup is open.
func (h *Host) Close() { h.end(true) }

func (h *Host) end(notify bool) {
	if h == nil || h.r == nil {
		return
	}
	for _, w := range h.watched {
		_ = h.conn.UnwatchFD(w.fd)
	}
	if h.surf != nil {
		if err := h.surf.Close(); err != nil {
			h.log.Warn("popup: cannot close the surface", "err", err)
		}
	}
	_ = h.r.Close()
	if notify && h.menu != nil && h.owner != nil && !h.OwnerRestarted() {
		h.owner.Send(strconv.AppendUint([]byte("menu-closed "), uint64(h.menu.token), 10))
	}
	h.reset()
}

// fail logs err and closes the popup: a broken popup never takes the bar down.
func (h *Host) fail(what string, err error) {
	h.log.Warn("popup: "+what, "err", err)
	h.Close()
}

// PopupDone is the compositor dismissing the popup (outside click, denied
// grab, gone parent).
func (h *Host) PopupDone() { h.Close() }

// Configure follows the size the compositor gave the popup.
func (h *Host) Configure() {
	if h.r == nil {
		return
	}
	if !h.configured { // later configures must not lift the frame-callback gate
		h.canPresent = true
	}
	h.configured = true
	h.resize()
}

// Scale follows the popup's preferred scale.
func (h *Host) Scale() { h.resize() }

func (h *Host) resize() {
	if h.r == nil {
		return
	}
	w, ht, scale := h.surf.Size()
	h.r.Resize(int(w), int(ht), scale)
}

// Frame allows the next Present.
func (h *Host) Frame() {
	if h.r != nil {
		h.canPresent = true
	}
}

// FDReady hands a buffer back once the compositor released it; id is the
// watched id, FDBase plus the renderer's buffer.
func (h *Host) FDReady(id uint64) {
	if h.r == nil || id < FDBase {
		return
	}
	if err := h.r.Released(id - FDBase); err != nil {
		h.fail("release buffer", err)
	}
}

// Pointer feeds a pointer event over the popup to its renderer.
func (h *Host) Pointer(ev *neferclient.PointerEvent) {
	if h.r == nil {
		return
	}
	in := nefergui.Input{X: ev.X, Y: ev.Y}
	switch ev.Kind {
	case neferclient.PointerEnter:
		h.inside = true
		in.Kind = nefergui.InputPointerMotion
		h.applyCursor() // the shape is only valid after the enter
	case neferclient.PointerMotion:
		in.Kind = nefergui.InputPointerMotion
	case neferclient.PointerLeave:
		h.inside = false
		in.Kind = nefergui.InputPointerLeave
	case neferclient.PointerButton:
		in.Kind, in.Button = nefergui.InputPointerRelease, ev.Button
		if ev.Pressed {
			in.Kind = nefergui.InputPointerPress
		}
	case neferclient.PointerAxis:
		in.Kind, in.DX, in.DY = nefergui.InputPointerAxis, ev.DX, ev.DY
	default:
		return
	}
	h.r.Input(&in)
}

// Key feeds a key event to the popup. A menu maps its navigation keys first.
func (h *Host) Key(ev *neferclient.KeyEvent) {
	if h.r == nil {
		return
	}
	if h.menu != nil {
		switch act := mapKey(ev.Keysym, h.menu.inSubmenu()); act {
		case keyNext, keyPrevious:
			if ev.Pressed { // the repeats of a held arrow walk on
				mods := nefergui.Modifiers(0)
				if act == keyPrevious {
					mods = nefergui.ModShift
				}
				h.r.Input(&nefergui.Input{Kind: nefergui.InputKey, Keysym: keysymTab, Pressed: true, Modifiers: mods})
			}
			return
		case keyBack:
			if ev.Pressed && !ev.Repeat && h.menu.pop() {
				h.r.Invalidate()
			}
			return
		case keyClose:
			if ev.Pressed {
				h.Close()
			}
			return
		}
	}
	h.r.Input(&nefergui.Input{
		Kind:      nefergui.InputKey,
		Keysym:    ev.Keysym,
		Text:      ev.Text, // valid only during the call, as for Input
		Pressed:   ev.Pressed,
		Repeat:    ev.Repeat,
		Modifiers: nefergui.Modifiers(ev.Modifiers & guiMods),
	})
}

// KeyboardFocus tells the renderer whether the popup has the keyboard.
func (h *Host) KeyboardFocus(focused bool) {
	if h.r == nil {
		return
	}
	kind := nefergui.InputFocusOut
	if focused {
		kind = nefergui.InputFocusIn
	}
	h.r.Input(&nefergui.Input{Kind: kind})
}

// Draw renders and presents the popup when something changed, and carries out
// what the user chose in a menu.
func (h *Host) Draw() {
	if h.r == nil {
		return
	}
	if h.menu != nil && h.menu.changed {
		h.levelChanged()
	}
	if h.r == nil || !h.configured || !h.canPresent || h.surf.Feedback() == nil {
		return
	}
	ok, err := h.render(&h.out)
	if err != nil {
		h.fail("render", err)
		return
	}
	if h.menu != nil && h.menu.chosen >= 0 {
		// Chosen during the build: tell the script and go, without
		// "menu-closed" (the choice ends the menu).
		line := append([]byte("menu-activate "), strconv.FormatUint(uint64(h.menu.token), 10)...)
		line = append(line, ' ')
		line = strconv.AppendInt(line, int64(h.menu.chosen), 10)
		if !h.owner.Send(line) {
			h.log.Debug("popup: the choice was not delivered: the script's input queue is full or closed", "module", h.owner.Name)
		}
		h.end(false)
		return
	}
	if ok {
		if err = h.present(); err != nil {
			h.fail("present", err)
			return
		}
		h.applyCursor()
	}
	if h.menu != nil && h.menu.changed {
		h.levelChanged()
	}
}

// levelChanged resizes the popup for the level of the menu now shown.
func (h *Host) levelChanged() {
	h.menu.changed = false
	w, ht, _, err := h.measure(h.r)
	if err != nil {
		h.fail("measure", err)
		return
	}
	if err = h.surf.Reposition(Placement(h.anchor, h.bottomBar, w, ht)); err != nil {
		// Without xdg_wm_base 3 the popup keeps its size: a level taller than
		// it scrolls inside it.
		h.log.Debug("popup: cannot resize the menu", "err", err)
		_, cur, _ := h.surf.Size()
		h.menu.fitTo(ht, cur)
	}
	h.r.Invalidate()
}

// present hands the frame in h.out to the compositor, as nefergui's demo does.
func (h *Host) present() error {
	out := &h.out
	// A resize retired buffers: stop watching and destroy them first.
	for _, rt := range out.Retired {
		if err := h.conn.UnwatchFD(rt.ReleaseFD); err != nil {
			return fmt.Errorf("unwatch retired buffer %d: %w", rt.Buffer, err)
		}
		for i, w := range h.watched {
			if w.fd == rt.ReleaseFD {
				h.watched = append(h.watched[:i], h.watched[i+1:]...)
				break
			}
		}
		if err := h.surf.DestroyBuffer(rt.Buffer); err != nil {
			return fmt.Errorf("destroy retired buffer %d: %w", rt.Buffer, err)
		}
		// The release timeline of a buffer is imported under the buffer's id.
		if err := h.surf.DestroyTimeline(rt.Buffer); err != nil {
			return fmt.Errorf("destroy retired timeline %d: %w", rt.Buffer, err)
		}
	}
	if out.NewBuffer {
		if out.Buffer >= FDBase {
			return fmt.Errorf("buffer id %d is out of range", out.Buffer)
		}
		buf := neferclient.Buffer{
			Width: out.Width, Height: out.Height, FourCC: out.FourCC, Modifier: out.Modifier,
			PlaneCount: out.PlaneCount,
		}
		for i, p := range out.Planes {
			buf.Planes[i] = neferclient.Plane{FD: p.FD, Offset: p.Offset, Stride: p.Stride}
		}
		if err := h.surf.ImportBuffer(out.Buffer, &buf); err != nil {
			return fmt.Errorf("import buffer: %w", err)
		}
		// The release eventfd is stable per buffer: watch it once.
		if err := h.conn.WatchFD(out.ReleaseFD, FDBase|out.Buffer); err != nil {
			return fmt.Errorf("watch release fd: %w", err)
		}
		h.watched = append(h.watched, watch{fd: out.ReleaseFD, buffer: out.Buffer})
	}
	if out.NewTimelines {
		if !h.haveAcquire { // the acquire timeline is shared by every buffer
			if err := h.surf.ImportTimeline(out.Acquire.ID, out.Acquire.FD); err != nil {
				return fmt.Errorf("import acquire timeline: %w", err)
			}
			h.haveAcquire = true
		}
		if err := h.surf.ImportTimeline(out.Release.ID, out.Release.FD); err != nil {
			return fmt.Errorf("import release timeline: %w", err)
		}
	}
	h.damage = h.damage[:0]
	for _, r := range out.Damage {
		h.damage = append(h.damage, neferclient.Rect{X: r.X, Y: r.Y, Width: r.Width, Height: r.Height})
	}
	err := h.surf.Present(&neferclient.Present{
		Buffer:          out.Buffer,
		AcquireTimeline: out.Acquire.ID,
		ReleaseTimeline: out.Release.ID,
		AcquirePoint:    out.AcquirePoint,
		ReleasePoint:    out.ReleasePoint,
		Damage:          h.damage,
		Opaque:          true, // XRGB8888: RendererConfig.Transparent is false
	})
	if err != nil {
		return fmt.Errorf("present: %w", err)
	}
	h.canPresent = false // until Frame
	return nil
}

// applyCursor shows the shape the hovered element asked for, while the
// pointer is over the popup.
func (h *Host) applyCursor() {
	switch h.out.Cursor {
	case nefergui.CursorPointer:
		h.cursor = neferclient.CursorPointer
	case nefergui.CursorText:
		h.cursor = neferclient.CursorText
	case nefergui.CursorNotAllowed:
		h.cursor = neferclient.CursorNotAllowed
	default:
		h.cursor = neferclient.CursorDefault
	}
	if !h.inside {
		return
	}
	if err := h.seat.SetCursor(h.cursor); err != nil {
		h.log.Debug("popup: cannot set the cursor", "err", err)
	}
}

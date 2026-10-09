package bar

import (
	"slices"
	"time"

	"github.com/bnema/neferbar/internal/layout"
	"github.com/bnema/neferbar/internal/module"
	"github.com/bnema/neferbar/internal/popup"
)

// popupPoll is how long the loop waits before it asks again for a popup frame
// that is waiting for the GPU.
const popupPoll = 2 * time.Millisecond

// updatePopupStyle writes the stylesheet of the popups when the bar's colors,
// font or size changed. Without a stylesheet no popup opens.
func (b *Bar) updatePopupStyle() {
	if b.pop == nil {
		return
	}
	css := popup.CSS(popup.Style{
		Font: b.cfg.Bar.Font, Size: b.cfg.Bar.Size * b.cfg.Bar.Scale,
		FG: b.fg, BG: b.bg, Accent: b.pal[b.cfg.Bar.Accent&15],
	})
	if css == b.popCSS {
		return
	}
	path, err := popup.WriteCSS(css)
	if err != nil {
		b.log.Warn("popups are off: cannot write their stylesheet", "err", err)
		b.pop.SetStyles("")
		b.popCSS = ""
		return
	}
	b.popCSS = css
	b.pop.SetStyles(path)
}

// closePopup dismisses the tooltip or menu being shown.
func (b *Bar) closePopup() { b.pop.Close() }

// closeTooltip dismisses the popup when it is a tooltip; a menu stays.
func (b *Bar) closeTooltip() {
	if b.pop.Kind() == popup.KindTooltip {
		b.pop.Close()
	}
}

// stepPopup runs after every wake-up of the loop: it starts the GPU warm-up,
// carries out what the modules asked for, and draws the popup.
func (b *Bar) stepPopup() {
	if b.pop == nil {
		return
	}
	b.popC = nil
	if !b.warmed && b.fb != nil && b.popCSS != "" && b.anyInteractive() {
		b.warmed = true
		b.pop.Warm(b.fb)
	}
	for _, m := range b.mods {
		b.drainControl(m)
	}
	if b.pop.OwnerRestarted() {
		b.closePopup()
	}
	if !b.pop.Active() {
		return
	}
	b.pop.Draw()
	if b.pop.Pending() {
		b.popC = time.After(popupPoll)
	}
}

func (b *Bar) anyInteractive() bool {
	for _, m := range b.mods {
		if m.Interactive {
			return true
		}
	}
	return false
}

// drainControl takes the control lines module m printed since the last
// wake-up and carries them out. A script that prints a tooltip on every
// mouse move would otherwise open and close a popup (a renderer, a surface)
// for each one: of the tooltip and close lines of one wake-up only the last
// counts.
func (b *Bar) drainControl(m *module.Module) {
	raw, ok := m.TakeControl()
	if !ok {
		return
	}
	b.ctlBuf = b.ctlBuf[:0]
	for ; ok; raw, ok = m.TakeControl() {
		ctrl, err := module.ParseControl(raw)
		if err != nil {
			b.badControl(m, err)
			continue
		}
		b.ctlBuf = append(b.ctlBuf, ctrl)
	}
	b.ctlBuf = coalesceControl(b.ctlBuf)
	for i := range b.ctlBuf {
		b.control(m, b.ctlBuf[i])
	}
	clear(b.ctlBuf) // keeps no parsed menu or table alive until the next wake-up
}

// badControl logs an invalid control line, once per module.
func (b *Bar) badControl(m *module.Module, err error) {
	if b.badCtl[m] {
		return
	}
	if b.badCtl == nil {
		b.badCtl = map[*module.Module]bool{}
	}
	b.badCtl[m] = true
	b.log.Warn("module sent an invalid control line; ignored (shown once per module)", "module", m.Name, "err", err)
}

// coalesceControl keeps, in order, every menu line and only the last of the
// tooltip and close lines. It works in place.
func coalesceControl(cs []module.Control) []module.Control {
	last := -1
	for i, c := range cs {
		if c.Type != module.ControlMenu {
			last = i
		}
	}
	i := 0
	return slices.DeleteFunc(cs, func(c module.Control) bool {
		drop := c.Type != module.ControlMenu && i != last
		i++
		return drop
	})
}

// tooltipInterval is the shortest time between two tooltips opened for one
// module.
const tooltipInterval = 250 * time.Millisecond

// tooltipLimiter rate-limits the tooltips of each module.
type tooltipLimiter struct {
	last map[*module.Module]time.Time
}

// allow reports whether m may open a tooltip at now, and counts it if so.
func (l *tooltipLimiter) allow(m *module.Module, now time.Time) bool {
	if t, ok := l.last[m]; ok && now.Sub(t) < tooltipInterval {
		return false
	}
	if l.last == nil {
		l.last = map[*module.Module]time.Time{}
	}
	l.last[m] = now
	return true
}

// forget drops what is kept for modules that are not in keep.
func (l *tooltipLimiter) forget(keep []*module.Module) {
	for m := range l.last {
		if !slices.Contains(keep, m) {
			delete(l.last, m)
		}
	}
}

// forgetModules drops the per-module state of modules that were stopped.
func (b *Bar) forgetModules(keep []*module.Module) {
	for m := range b.badCtl {
		if !slices.Contains(keep, m) {
			delete(b.badCtl, m)
		}
	}
	b.tips.forget(keep)
}

// control handles one control line of module m.
func (b *Bar) control(m *module.Module, ctrl module.Control) {
	if !m.Interactive {
		return // only modules that asked for the pointer may open popups
	}
	switch ctrl.Type {
	case module.ControlClose:
		if b.pop.Kind() == popup.KindTooltip && b.pop.Owner() == m {
			b.pop.Close()
		}
	case module.ControlTooltip:
		// Only for the module under the pointer, and never over a menu.
		if m != b.ptr.hover || b.pop.Kind() == popup.KindMenu {
			return
		}
		if b.pop.ShowsTooltip(m, ctrl) || !b.tips.allow(m, time.Now()) {
			return // already shown, or the script asks too often
		}
		b.openPopup(m, popup.KindTooltip, ctrl, 0)
	case module.ControlMenu:
		serial, ok := b.ptr.takeMenuPress(m, ctrl.Click, time.Now())
		if !ok {
			b.log.Debug("menu dropped: it does not answer the latest press", "module", m.Name, "click", ctrl.Click)
			return
		}
		b.openPopup(m, popup.KindMenu, ctrl, serial)
	}
}

// openPopup shows ctrl as a popup of module m, anchored to the cells it names.
func (b *Bar) openPopup(m *module.Module, kind popup.Kind, ctrl module.Control, serial uint32) {
	if !b.configured || b.fb == nil || b.face == nil || b.lay == nil {
		return
	}
	b.spanBuf = b.lay.Spans(b.spanBuf[:0])
	var span layout.Span
	for _, s := range b.spanBuf {
		if s.M == m {
			span = s
		}
	}
	if span.M == nil {
		return // the module shows nothing now
	}
	_, h, scale := b.surf.Size()
	anchor, ok := popup.Anchor(span.Start, span.Width, ctrl.Col, ctrl.Width, b.face.CellW, scale, h)
	if !ok {
		b.log.Debug("popup dropped: its column is outside the module", "module", m.Name, "col", ctrl.Col)
		return
	}
	err := b.pop.Open(popup.Request{
		Kind: kind, Owner: m, Ctrl: ctrl, Anchor: anchor, Parent: b.surf,
		BottomBar: b.cfg.Bar.Position == "bottom", Feedback: b.fb, Serial: serial,
	})
	if err != nil {
		b.log.Warn("cannot open a popup", "module", m.Name, "err", err)
	}
}
